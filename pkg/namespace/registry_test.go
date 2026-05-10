package namespace_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"oras.land/oras-go/v2/errdef"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// Compile-time assertion: *ScopedRegistry satisfies handler.Registry
// so existing python/maven handlers can swap a *pkg/oci.Registry for
// a *ScopedRegistry without any signature changes. Lives in the
// _test package to avoid pkg/namespace importing pkg/handler.
var _ handler.Registry = (*namespace.ScopedRegistry)(nil)

const (
	googleIss   = "https://accounts.google.com"
	aliceID     = "alice"
	aliceEmail  = "alice@example.com"
	otherEmail  = "bob@example.com"
	repoFoo     = "packages/foo"
	repoBar     = "packages/bar"
	defaultBody = "hello world"
)

// allowAllSpec is a Spec whose policy admits any oidc subject for
// both read and write. Tests that aren't exercising authz mechanics
// use it to keep noise out of the assertions.
func allowAllSpec() namespace.Spec {
	return namespace.Spec{
		Policy: namespace.Policy{
			Readers: []namespace.SubjectMatcher{{Issuer: googleIss}},
			Writers: []namespace.SubjectMatcher{{Issuer: googleIss}},
		},
	}
}

func aliceCtx(t *testing.T) context.Context {
	t.Helper()
	return auth.WithAuthContext(t.Context(), &auth.AuthContext{
		Issuer: googleIss,
		ID:     aliceID,
		Email:  aliceEmail,
	})
}

func setup(t *testing.T, opts ...namespace.RegistryOption) (*oci.FakeRegistry, *namespace.Registry, *namespace.Store) {
	t.Helper()
	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	// Disable the policy cache by default in tests so a Put then call
	// in the same test sees the new spec without sleeping. The cache
	// itself is exercised explicitly in TestRegistry_PolicyCache_*.
	allOpts := append([]namespace.RegistryOption{namespace.WithPolicyCacheTTL(0)}, opts...)
	reg := namespace.NewRegistry(fake, store, allOpts...)
	return fake, reg, store
}

func putNamespace(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("Put namespace %q: %v", name, err)
	}
}

func newRepoFile(repo, tag, name string) *oci.RepoFile {
	body := []byte(defaultBody)
	return &oci.RepoFile{
		OwningRepo: repo,
		OwningTag:  tag,
		Name:       name,
		MediaType:  "application/octet-stream",
		Size:       int64(len(body)),
	}
}

func TestScopedRegistry_AddRead_HappyPath(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	if scoped.Namespace() != "alpha" {
		t.Errorf("Namespace() = %q, want %q", scoped.Namespace(), "alpha")
	}

	body := strings.NewReader(defaultBody)
	if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"), body); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	if _, ok := fake.Files["alpha/"+repoFoo+"/1.0.0/foo.txt"]; !ok {
		t.Errorf("expected backend file under alpha/%s/1.0.0/foo.txt; got %v",
			repoFoo, slicesSortedKeys(fake.Files))
	}

	_, rc, err := scoped.ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	defer rc.Close()
	gotBody, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(gotBody) != defaultBody {
		t.Errorf("ReadFile body = %q, want %q", gotBody, defaultBody)
	}
}

// TestScopedRegistry_NamespaceIsolation pins the load-bearing
// security guarantee: the beta scoped view cannot read a file
// written through the alpha scoped view, AND alpha can still read
// its own file (so a regression that 5xx'd on every read wouldn't
// pass for the wrong reason).
func TestScopedRegistry_NamespaceIsolation(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	putNamespace(t, store, "beta", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := reg.For("alpha").AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile alpha: %v", err)
	}

	// Alpha must still see its own file (rules out a generic-error
	// regression).
	if _, rc, err := reg.For("alpha").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt")); err != nil {
		t.Errorf("ReadFile alpha (own file): %v, want success", err)
	} else {
		rc.Close()
	}

	// Beta must not — and the failure mode must be ErrNotFound.
	_, _, err := reg.For("beta").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"))
	if err == nil {
		t.Fatal("ReadFile beta = nil, want not-found error (namespaces must isolate)")
	}
	if !errors.Is(err, errdef.ErrNotFound) {
		t.Errorf("ReadFile beta err = %v, want errors.Is(errdef.ErrNotFound)", err)
	}
}

// TestScopedRegistry_NamespaceEscape is the explicit anti-vuln
// test: the alpha scoped view cannot reach beta by passing
// OwningRepo="../beta/foo" etc.
func TestScopedRegistry_NamespaceEscape(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	putNamespace(t, store, "beta", allowAllSpec())
	ctx := aliceCtx(t)

	// Plant a beta file directly so a successful escape would have
	// something to find.
	if _, err := fake.AddFile(ctx, &oci.RepoFile{
		OwningRepo: "beta/" + repoFoo,
		OwningTag:  "1.0.0",
		Name:       "secret.txt",
		Size:       int64(len("secret")),
	}, strings.NewReader("secret")); err != nil {
		t.Fatalf("seed beta: %v", err)
	}

	escapes := []string{
		"../beta/" + repoFoo,
		"..",
		"foo/../../beta",
		"/beta/" + repoFoo, // absolute
		"./" + repoFoo,     // non-canonical
		repoFoo + "/",      // trailing slash, non-canonical
	}
	for _, esc := range escapes {
		t.Run("escape="+esc, func(t *testing.T) {
			t.Parallel()
			_, _, err := reg.For("alpha").ReadFile(ctx, newRepoFile(esc, "1.0.0", "secret.txt"))
			if err == nil {
				t.Fatalf("ReadFile owningRepo=%q = nil, want ErrInvalidOwningRepo (namespace escape!)", esc)
			}
			if !errors.Is(err, namespace.ErrInvalidOwningRepo) {
				t.Errorf("ReadFile owningRepo=%q err = %v, want errors.Is(ErrInvalidOwningRepo)", esc, err)
			}
		})
	}
}

func TestScopedRegistry_NamespaceNotFound(t *testing.T) {
	t.Parallel()

	_, reg, _ := setup(t)
	ctx := aliceCtx(t)
	scoped := reg.For("ghost")

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "AddFile",
			call: func() error {
				_, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"), strings.NewReader(defaultBody))
				return err
			},
		},
		{
			name: "ReadFile",
			call: func() error {
				_, _, err := scoped.ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"))
				return err
			},
		},
		{
			name: "BlobRedirectURL",
			call: func() error {
				_, err := scoped.BlobRedirectURL(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"))
				return err
			},
		},
		{
			name: "ListTags",
			call: func() error {
				_, err := scoped.ListTags(ctx, repoFoo)
				return err
			},
		},
		{
			name: "ListFiles",
			call: func() error {
				_, err := scoped.ListFiles(ctx, repoFoo)
				return err
			},
		},
		{
			name: "ListPackages",
			call: func() error {
				_, err := scoped.ListPackages(ctx)
				return err
			},
		},
		{
			name: "AppendRefs",
			call: func() error {
				return scoped.AppendRefs(ctx, repoFoo, "1.0.0", "latest")
			},
		},
		{
			name: "DeleteRepoFiles",
			call: func() error {
				return scoped.DeleteRepoFiles(ctx, repoFoo)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call()
			if err == nil {
				t.Fatalf("%s = nil, want error wrapping ErrNotFound", tc.name)
			}
			if !errors.Is(err, namespace.ErrNotFound) {
				t.Errorf("%s err = %v, want errors.Is(ErrNotFound)", tc.name, err)
			}
		})
	}
}

func TestScopedRegistry_NilFile(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	if _, err := scoped.AddFile(ctx, nil, strings.NewReader("")); err == nil {
		t.Error("AddFile(nil) = nil, want error")
	}
	if _, _, err := scoped.ReadFile(ctx, nil); err == nil {
		t.Error("ReadFile(nil) = nil, want error")
	}
	if _, err := scoped.BlobRedirectURL(ctx, nil); err == nil {
		t.Error("BlobRedirectURL(nil) = nil, want error")
	}
}

func TestScopedRegistry_Authz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		spec      namespace.Spec
		ac        *auth.AuthContext
		op        auth.Op
		wantAllow bool
	}{
		{
			name:      "read-allowed",
			spec:      namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Email: aliceEmail}}}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        auth.OpRead,
			wantAllow: true,
		},
		{
			name: "read-denied-empty-readers",
			spec: namespace.Spec{Policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        auth.OpRead,
			wantAllow: false,
		},
		{
			name: "write-allowed",
			spec: namespace.Spec{Policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "write-denied-empty-writers",
			spec: namespace.Spec{Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        auth.OpWrite,
			wantAllow: false,
		},
		{
			name:      "empty-policy-denies-read",
			spec:      namespace.Spec{},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        auth.OpRead,
			wantAllow: false,
		},
		{
			name:      "empty-policy-denies-write",
			spec:      namespace.Spec{},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        auth.OpWrite,
			wantAllow: false,
		},
		{
			name: "subject-mismatch-denied",
			spec: namespace.Spec{Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: "stranger", Email: otherEmail},
			op:        auth.OpRead,
			wantAllow: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake, reg, store := setup(t)
			putNamespace(t, store, "alpha", tc.spec)

			if tc.op == auth.OpRead {
				seedDirect(t, fake, "alpha")
			}

			ctx := auth.WithAuthContext(t.Context(), tc.ac)
			scoped := reg.For("alpha")

			var got error
			switch tc.op {
			case auth.OpWrite:
				_, got = scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"), strings.NewReader(defaultBody))
			case auth.OpRead:
				_, _, got = scoped.ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"))
			}

			if tc.wantAllow {
				if got != nil {
					t.Errorf("op=%s allowed but err=%v", tc.op, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("op=%s = nil, want error wrapping auth.ErrUnauthorized", tc.op)
			}
			if !errors.Is(got, auth.ErrUnauthorized) {
				t.Errorf("op=%s err = %v, want errors.Is(auth.ErrUnauthorized)", tc.op, got)
			}
		})
	}
}

func seedDirect(t *testing.T, fake *oci.FakeRegistry, ns string) {
	t.Helper()
	rf := &oci.RepoFile{
		OwningRepo: ns + "/" + repoFoo,
		OwningTag:  "1.0.0",
		Name:       "foo.txt",
		MediaType:  "application/octet-stream",
		Size:       int64(len(defaultBody)),
	}
	if _, err := fake.AddFile(t.Context(), rf, strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("seedDirect AddFile: %v", err)
	}
}

func TestScopedRegistry_NoAuthContextDenied(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())

	_, _, err := reg.For("alpha").ReadFile(t.Context(), newRepoFile(repoFoo, "1.0.0", "foo.txt"))
	if err == nil {
		t.Fatal("ReadFile with no AuthContext = nil, want error wrapping auth.ErrUnauthorized")
	}
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("ReadFile err = %v, want errors.Is(auth.ErrUnauthorized)", err)
	}
}

// TestScopedRegistry_NoAuthContextDeniedAtWrapper proves the wrapper
// itself denies nil even when the AuthzFactory would have allowed.
func TestScopedRegistry_NoAuthContextDeniedAtWrapper(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store,
		namespace.WithAuthzFactory(func(_ namespace.Policy) (auth.Authorizer, error) { return auth.AllowAll, nil }),
		namespace.WithPolicyCacheTTL(0),
	)
	putNamespace(t, store, "alpha", allowAllSpec())

	_, _, err := reg.For("alpha").ReadFile(t.Context(), newRepoFile(repoFoo, "1.0.0", "foo.txt"))
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("ReadFile (AllowAll factory + nil ctx) err = %v, want errors.Is(auth.ErrUnauthorized)", err)
	}
}

// TestScopedRegistry_PackageIndex_PopulatedExactlyOnce uses a
// counting backend to prove the wrapper makes exactly one
// index-repo AddFile call across N data writes to the same
// (ns, owning-repo).
func TestScopedRegistry_PackageIndex_PopulatedExactlyOnce(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	for i := 0; i < 5; i++ {
		tag := fmt.Sprintf("1.0.%d", i)
		if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, tag, "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", tag, err)
		}
	}

	if got := counted.indexAddCount("alpha"); got != 1 {
		t.Errorf("index AddFile count = %d, want 1 (LRU must dedupe)", got)
	}
}

func TestScopedRegistry_PackageIndex_MultipleRepos(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	for _, repo := range []string{repoFoo, repoBar, "tools/cli"} {
		if _, err := scoped.AddFile(ctx, newRepoFile(repo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", repo, err)
		}
	}

	got, err := scoped.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	slices.Sort(got)
	want := []string{repoBar, repoFoo, "tools/cli"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

// TestScopedRegistry_PackageIndex_ConcurrentExactlyOneAddFile is
// the concurrency stress: N goroutines hammering the same
// (ns, owning-repo) must result in exactly ONE index-repo AddFile.
func TestScopedRegistry_PackageIndex_ConcurrentExactlyOneAddFile(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := fmt.Sprintf("v%d", i)
			if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, tag, "f.txt"), strings.NewReader(defaultBody)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent AddFile: %v", err)
	}

	if got := counted.indexAddCount("alpha"); got != 1 {
		t.Errorf("index AddFile count = %d across %d concurrent writes, want 1", got, n)
	}
	pkgs, err := scoped.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if diff := cmp.Diff([]string{repoFoo}, pkgs); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

func TestScopedRegistry_PackageIndex_Isolated(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	putNamespace(t, store, "beta", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := reg.For("alpha").AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile alpha: %v", err)
	}

	betaPkgs, err := reg.For("beta").ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages beta: %v", err)
	}
	if len(betaPkgs) != 0 {
		t.Errorf("ListPackages beta = %v, want empty", betaPkgs)
	}
}

// TestScopedRegistry_TagEncoding_RoundTrip pins the encoding
// properties: collision-free between names that differ only in '/'
// vs '_'.
func TestScopedRegistry_TagEncoding_RoundTrip(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	repos := []string{
		"packages/requests",
		"com/example/foo-bar",
		"toplevel",
		"a__b",
		"a_b_c",
		"x.y.z",
	}
	for _, r := range repos {
		if _, err := scoped.AddFile(ctx, newRepoFile(r, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", r, err)
		}
	}

	got, err := scoped.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	slices.Sort(got)
	want := append([]string(nil), repos...)
	slices.Sort(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

func TestScopedRegistry_ListPackages_EmptyOnUntouchedNamespace(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	got, err := reg.For("alpha").ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListPackages = %v, want empty", got)
	}
}

func TestScopedRegistry_ListTags_AndListFiles(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	for _, tag := range []string{"1.0.0", "2.0.0"} {
		if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, tag, "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", tag, err)
		}
	}

	tags, err := scoped.ListTags(ctx, repoFoo)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	slices.Sort(tags)
	if diff := cmp.Diff([]string{"1.0.0", "2.0.0"}, tags); diff != "" {
		t.Errorf("ListTags mismatch (-want +got):\n%s", diff)
	}

	files, err := scoped.ListFiles(ctx, repoFoo)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, f := range files {
		if f.OwningRepo != repoFoo {
			t.Errorf("ListFiles OwningRepo = %q, want %q (namespace prefix must not leak)", f.OwningRepo, repoFoo)
		}
	}
	if len(files) != 2 {
		t.Errorf("ListFiles len = %d, want 2", len(files))
	}
}

func TestScopedRegistry_ListFiles_MultiSegmentRepo(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	repo := "com/example/foo-bar"
	if _, err := scoped.AddFile(ctx, newRepoFile(repo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	files, err := scoped.ListFiles(ctx, repo)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 || files[0].OwningRepo != repo {
		t.Errorf("ListFiles[0].OwningRepo = %v, want %q (full multi-segment path)", files, repo)
	}
}

// TestScopedRegistry_ListFiles_PrefixOfAnotherNamespace pins that
// "alpha" doesn't accidentally strip from "alpha-foo" repos.
func TestScopedRegistry_ListFiles_PrefixOfAnotherNamespace(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := fake.AddFile(ctx, &oci.RepoFile{
		OwningRepo: "alpha/" + repoFoo,
		OwningTag:  "1.0.0",
		Name:       "f.txt",
		Size:       int64(len(defaultBody)),
	}, strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	files, err := reg.For("alpha").ListFiles(ctx, repoFoo)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 || files[0].OwningRepo != repoFoo {
		t.Errorf("ListFiles = %v, want one entry with OwningRepo=%q", files, repoFoo)
	}
}

func TestScopedRegistry_AppendRefs_Forwards(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := scoped.AppendRefs(ctx, repoFoo, "1.0.0", "latest"); err != nil {
		t.Fatalf("AppendRefs: %v", err)
	}
	if got, ok := fake.Aliases["alpha/"+repoFoo+"/latest"]; !ok || got != "1.0.0" {
		t.Errorf("alias under alpha/%s/latest = %q (ok=%v), want 1.0.0", repoFoo, got, ok)
	}
}

// TestScopedRegistry_DeleteRepoFiles_SweepsBackendIndex verifies
// BOTH the in-process indexed marker AND the backend
// ocifactory-packages tag are cleared.
func TestScopedRegistry_DeleteRepoFiles_SweepsBackendIndex(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	pkgs, err := scoped.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages pre: %v", err)
	}
	if !slices.Contains(pkgs, repoFoo) {
		t.Fatalf("ListPackages pre = %v, want to contain %q", pkgs, repoFoo)
	}

	if err := scoped.DeleteRepoFiles(ctx, repoFoo); err != nil {
		t.Fatalf("DeleteRepoFiles: %v", err)
	}
	for k := range fake.Files {
		if strings.HasPrefix(k, "alpha/"+repoFoo+"/") {
			t.Errorf("file %q remained after DeleteRepoFiles", k)
		}
	}
	pkgs, err = scoped.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages post: %v", err)
	}
	if slices.Contains(pkgs, repoFoo) {
		t.Errorf("ListPackages post = %v, want NOT to contain %q (backend index must be swept)", pkgs, repoFoo)
	}

	if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "2.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile after delete: %v", err)
	}
	pkgs, err = scoped.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages re-add: %v", err)
	}
	if !slices.Contains(pkgs, repoFoo) {
		t.Errorf("ListPackages re-add = %v, want to contain %q", pkgs, repoFoo)
	}
}

// TestRegistry_PolicyCache_HotPathSkipsStore counts spec.json reads
// and asserts the cache absorbs all but the first lookup. The
// before-snapshot is taken AFTER the seed so the cache is known to
// be populated.
func TestRegistry_PolicyCache_HotPathSkipsStore(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))

	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	priming := counted.specReads("alpha")
	if priming == 0 {
		t.Fatal("priming spec reads = 0, test is vacuous (the wrapper should have polled the store at least once)")
	}

	const iters = 50
	for i := 0; i < iters; i++ {
		_, _, err := scoped.ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"))
		if err != nil {
			t.Fatalf("ReadFile %d: %v", i, err)
		}
	}
	after := counted.specReads("alpha")
	if after != priming {
		t.Errorf("spec re-fetched %d times across %d cached requests, want 0", after-priming, iters)
	}
}

func TestRegistry_PolicyCache_TTLZero_DisablesCache(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(0))

	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)
	scoped := reg.For("alpha")

	if _, err := scoped.AddFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	before := counted.specReads("alpha")
	const iters = 5
	for i := 0; i < iters; i++ {
		_, _, err := scoped.ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
	}
	after := counted.specReads("alpha")
	if after-before < iters {
		t.Errorf("spec re-fetches = %d across %d disabled-cache requests, want >= %d",
			after-before, iters, iters)
	}
}

func TestRegistry_PolicyCache_NegativeCaching(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))
	ctx := aliceCtx(t)

	const iters = 10
	for i := 0; i < iters; i++ {
		_, _, err := reg.For("ghost").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt"))
		if !errors.Is(err, namespace.ErrNotFound) {
			t.Fatalf("ReadFile(ghost) %d err = %v, want errors.Is(ErrNotFound)", i, err)
		}
	}
	if got := counted.specReads("ghost"); got > 1 {
		t.Errorf("spec reads for missing namespace = %d after %d requests, want 1 (negative cache must absorb)", got, iters)
	}

	reg.InvalidatePolicy("ghost")
	if _, _, err := reg.For("ghost").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "f.txt")); !errors.Is(err, namespace.ErrNotFound) {
		t.Fatalf("post-invalidate ReadFile = %v, want errors.Is(ErrNotFound)", err)
	}
	if got := counted.specReads("ghost"); got != 2 {
		t.Errorf("spec reads after invalidate = %d, want 2 (one before, one after)", got)
	}
}

// TestRegistry_StorePut_InvalidatesCache verifies the mutation hook
// wired by NewRegistry: an admin Put takes effect on the very next
// data-plane call without waiting for the cache TTL.
func TestRegistry_StorePut_InvalidatesCache(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))
	ctx := aliceCtx(t)

	putNamespace(t, store, "alpha", namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Email: otherEmail}},
	}})
	seedDirect(t, counted.FakeRegistry, "alpha")
	if _, _, err := reg.For("alpha").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt")); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("pre-update ReadFile = %v, want errors.Is(auth.ErrUnauthorized)", err)
	}

	putNamespace(t, store, "alpha", allowAllSpec())
	if _, _, err := reg.For("alpha").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt")); err != nil {
		t.Errorf("post-update ReadFile = %v, want success (Store.Put must invalidate cache)", err)
	}
}

// TestRegistry_PolicyCache_Singleflight: N concurrent first-misses
// for the same namespace must collapse into a single Store.Get.
func TestRegistry_PolicyCache_Singleflight(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))
	putNamespace(t, store, "alpha", allowAllSpec())
	seedDirect(t, counted.FakeRegistry, "alpha")
	counted.resetSpecReads("alpha")
	ctx := aliceCtx(t)

	// Block the spec read so concurrent goroutines all queue up
	// behind a slow first miss — this is what makes singleflight
	// observable.
	counted.blockSpecRead = make(chan struct{})
	defer close(counted.blockSpecRead)

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = reg.For("alpha").ReadFile(ctx, newRepoFile(repoFoo, "1.0.0", "foo.txt"))
		}()
	}
	time.Sleep(50 * time.Millisecond)
	counted.blockSpecRead <- struct{}{}
	wg.Wait()

	if got := counted.specReads("alpha"); got != 1 {
		t.Errorf("spec reads under singleflight = %d across %d concurrent misses, want 1", got, n)
	}
}

// countingBackend wraps an [oci.FakeRegistry] and counts how many
// times the namespace metadata spec.json is read for each namespace,
// and how many times the per-namespace ocifactory-packages index
// repo receives an AddFile.
type countingBackend struct {
	*oci.FakeRegistry
	mu            sync.Mutex
	specReads_    map[string]int
	indexAdds_    map[string]int
	blockSpecRead chan struct{}
}

func newCountingBackend() *countingBackend {
	return &countingBackend{
		FakeRegistry: oci.NewFakeRegistry(),
		specReads_:   map[string]int{},
		indexAdds_:   map[string]int{},
	}
}

func (c *countingBackend) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	if isSpecRead(f) {
		c.mu.Lock()
		c.specReads_[f.OwningRepo]++
		blocker := c.blockSpecRead
		c.mu.Unlock()
		if blocker != nil {
			<-blocker
		}
	}
	return c.FakeRegistry.ReadFile(ctx, f)
}

func (c *countingBackend) AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error) {
	if ns, ok := indexRepoNamespace(f.OwningRepo); ok {
		c.mu.Lock()
		c.indexAdds_[ns]++
		c.mu.Unlock()
	}
	return c.FakeRegistry.AddFile(ctx, f, ro)
}

func (c *countingBackend) specReads(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.specReads_[name]
}

func (c *countingBackend) resetSpecReads(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.specReads_, name)
}

func (c *countingBackend) indexAddCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.indexAdds_[name]
}

func isSpecRead(f *oci.RepoFile) bool {
	return f.OwningTag == "_metadata" && f.Name == "spec.json"
}

func indexRepoNamespace(owningRepo string) (string, bool) {
	const suffix = "/ocifactory-packages"
	if strings.HasSuffix(owningRepo, suffix) {
		return owningRepo[:len(owningRepo)-len(suffix)], true
	}
	return "", false
}

func slicesSortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
