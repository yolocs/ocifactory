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

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	googleIss   = "https://accounts.google.com"
	aliceID     = "alice"
	aliceEmail  = "alice@example.com"
	otherEmail  = "bob@example.com"
	repoFoo     = "packages/foo"
	repoBar     = "packages/bar"
	defaultBody = "hello world"
)

// allowAllSpec is a Spec whose policy admits any oidc subject for both
// read and write. Tests that aren't exercising authz mechanics use it
// to keep noise out of the assertions.
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

func TestRegistry_AddRead_HappyPath(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	body := strings.NewReader(defaultBody)
	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "foo.txt"), body); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	if _, ok := fake.Files["alpha/"+repoFoo+"/1.0.0/foo.txt"]; !ok {
		t.Errorf("expected backend file under alpha/%s/1.0.0/foo.txt; got %v",
			repoFoo, slicesSortedKeys(fake.Files))
	}

	_, rc, err := reg.ReadFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "foo.txt"))
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

// TestRegistry_NamespaceIsolation pins the load-bearing security
// guarantee: namespace beta cannot read a file written through
// namespace alpha, even when both have permissive policies. The
// prefix is the only thing isolating them — if it ever regresses,
// every authz test below is moot.
func TestRegistry_NamespaceIsolation(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	putNamespace(t, store, "beta", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "foo.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile alpha: %v", err)
	}

	_, _, err := reg.ReadFile(ctx, "beta", newRepoFile(repoFoo, "1.0.0", "foo.txt"))
	if err == nil {
		t.Fatal("ReadFile beta = nil, want not-found error (namespaces must isolate)")
	}
}

func TestRegistry_NamespaceNotFound(t *testing.T) {
	t.Parallel()

	_, reg, _ := setup(t)
	ctx := aliceCtx(t)

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "AddFile",
			call: func() error {
				_, err := reg.AddFile(ctx, "ghost", newRepoFile(repoFoo, "1.0.0", "foo.txt"), strings.NewReader(defaultBody))
				return err
			},
		},
		{
			name: "ReadFile",
			call: func() error {
				_, _, err := reg.ReadFile(ctx, "ghost", newRepoFile(repoFoo, "1.0.0", "foo.txt"))
				return err
			},
		},
		{
			name: "BlobRedirectURL",
			call: func() error {
				_, err := reg.BlobRedirectURL(ctx, "ghost", newRepoFile(repoFoo, "1.0.0", "foo.txt"))
				return err
			},
		},
		{
			name: "ListTags",
			call: func() error {
				_, err := reg.ListTags(ctx, "ghost", repoFoo)
				return err
			},
		},
		{
			name: "ListFiles",
			call: func() error {
				_, err := reg.ListFiles(ctx, "ghost", repoFoo)
				return err
			},
		},
		{
			name: "ListPackages",
			call: func() error {
				_, err := reg.ListPackages(ctx, "ghost")
				return err
			},
		},
		{
			name: "AppendRefs",
			call: func() error {
				return reg.AppendRefs(ctx, "ghost", repoFoo, "1.0.0", "latest")
			},
		},
		{
			name: "DeleteRepoFiles",
			call: func() error {
				return reg.DeleteRepoFiles(ctx, "ghost", repoFoo)
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

func TestRegistry_Authz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		spec      namespace.Spec
		ac        *auth.AuthContext
		op        string // "read" or "write"
		wantAllow bool
	}{
		{
			name:      "read-allowed",
			spec:      namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Email: aliceEmail}}}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        "read",
			wantAllow: true,
		},
		{
			name: "read-denied-when-only-writers-set",
			spec: namespace.Spec{Policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        "read",
			wantAllow: false,
		},
		{
			name: "write-allowed",
			spec: namespace.Spec{Policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        "write",
			wantAllow: true,
		},
		{
			name: "write-denied-when-only-readers-set",
			spec: namespace.Spec{Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        "write",
			wantAllow: false,
		},
		{
			name:      "empty-policy-denies-read",
			spec:      namespace.Spec{},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        "read",
			wantAllow: false,
		},
		{
			name:      "empty-policy-denies-write",
			spec:      namespace.Spec{},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: aliceID, Email: aliceEmail},
			op:        "write",
			wantAllow: false,
		},
		{
			name: "subject-mismatch-denied",
			spec: namespace.Spec{Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: aliceEmail}},
			}},
			ac:        &auth.AuthContext{Issuer: googleIss, ID: "stranger", Email: otherEmail},
			op:        "read",
			wantAllow: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake, reg, store := setup(t)
			putNamespace(t, store, "alpha", tc.spec)

			// Seed the file directly through the fake so the read
			// path always has something to find: any error must come
			// from authz, not file-not-found.
			if tc.op == "read" {
				seedDirect(t, fake, "alpha")
			}

			ctx := auth.WithAuthContext(t.Context(), tc.ac)

			var got error
			switch tc.op {
			case "write":
				_, got = reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "foo.txt"), strings.NewReader(defaultBody))
			case "read":
				_, _, got = reg.ReadFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "foo.txt"))
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

// seedDirect plants a file under namespace ns via the fake directly,
// bypassing the wrapper's authz so read tests can isolate the deny
// paths from "file not found" noise.
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

func TestRegistry_NoAuthContextDenied(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())

	// No AuthContext on ctx — policyAuthorizer denies a nil subject.
	_, _, err := reg.ReadFile(t.Context(), "alpha", newRepoFile(repoFoo, "1.0.0", "foo.txt"))
	if err == nil {
		t.Fatal("ReadFile with no AuthContext = nil, want error wrapping auth.ErrUnauthorized")
	}
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("ReadFile err = %v, want errors.Is(auth.ErrUnauthorized)", err)
	}
}

// TestRegistry_PackageIndex_PopulatedOnce verifies the index is
// populated by AddFile and that re-writing the same owning-repo does
// not stack duplicate entries.
func TestRegistry_PackageIndex_PopulatedOnce(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	for i := 0; i < 3; i++ {
		tag := fmt.Sprintf("1.0.%d", i)
		if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, tag, "foo.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", tag, err)
		}
	}

	got, err := reg.ListPackages(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if diff := cmp.Diff([]string{repoFoo}, got); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

func TestRegistry_PackageIndex_MultipleRepos(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	for _, repo := range []string{repoFoo, repoBar, "tools/cli"} {
		if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", repo, err)
		}
	}

	got, err := reg.ListPackages(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	slices.Sort(got)
	want := []string{repoBar, repoFoo, "tools/cli"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

// TestRegistry_PackageIndex_ConcurrentSameRepo races N goroutines
// publishing to the same (ns, owning-repo) and asserts the index ends
// up with exactly one entry and no errors leak out.
func TestRegistry_PackageIndex_ConcurrentSameRepo(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := fmt.Sprintf("v%d", i)
			if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, tag, "f.txt"), strings.NewReader(defaultBody)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent AddFile: %v", err)
	}

	pkgs, err := reg.ListPackages(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if diff := cmp.Diff([]string{repoFoo}, pkgs); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

// TestRegistry_PackageIndex_Isolated pins that the package index is
// per-namespace: a write into alpha doesn't show up in beta's index.
func TestRegistry_PackageIndex_Isolated(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	putNamespace(t, store, "beta", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile alpha: %v", err)
	}

	betaPkgs, err := reg.ListPackages(ctx, "beta")
	if err != nil {
		t.Fatalf("ListPackages beta: %v", err)
	}
	if len(betaPkgs) != 0 {
		t.Errorf("ListPackages beta = %v, want empty", betaPkgs)
	}
}

// TestRegistry_PackageIndex_TagEncoding pins that owning-repo names
// containing '/' round-trip through the index correctly.
func TestRegistry_PackageIndex_TagEncoding(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	repos := []string{
		"packages/requests",
		"com/example/foo-bar",
		"toplevel",
	}
	for _, r := range repos {
		if _, err := reg.AddFile(ctx, "alpha", newRepoFile(r, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", r, err)
		}
	}

	// Verify the on-backend tag form is escaped (for inspectability
	// the test pins the encoding the wrapper uses).
	indexTags, err := fake.ListTags(ctx, "alpha/_packages")
	if err != nil {
		t.Fatalf("ListTags index: %v", err)
	}
	wantBackend := []string{
		"packages__requests",
		"com__example__foo-bar",
		"toplevel",
	}
	slices.Sort(indexTags)
	slices.Sort(wantBackend)
	if diff := cmp.Diff(wantBackend, indexTags); diff != "" {
		t.Errorf("backend index tags mismatch (-want +got):\n%s", diff)
	}

	got, err := reg.ListPackages(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	slices.Sort(got)
	wantDecoded := []string{
		"com/example/foo-bar",
		"packages/requests",
		"toplevel",
	}
	if diff := cmp.Diff(wantDecoded, got); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

func TestRegistry_ListPackages_EmptyOnUntouchedNamespace(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	got, err := reg.ListPackages(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListPackages = %v, want empty", got)
	}
}

func TestRegistry_ListTags_AndListFiles(t *testing.T) {
	t.Parallel()

	_, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	for _, tag := range []string{"1.0.0", "2.0.0"} {
		if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, tag, "f.txt"), strings.NewReader(defaultBody)); err != nil {
			t.Fatalf("AddFile %s: %v", tag, err)
		}
	}

	tags, err := reg.ListTags(ctx, "alpha", repoFoo)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	slices.Sort(tags)
	if diff := cmp.Diff([]string{"1.0.0", "2.0.0"}, tags); diff != "" {
		t.Errorf("ListTags mismatch (-want +got):\n%s", diff)
	}

	files, err := reg.ListFiles(ctx, "alpha", repoFoo)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	// OwningRepo must come back without the namespace prefix —
	// callers should not see the namespace leak through.
	for _, f := range files {
		if f.OwningRepo != repoFoo {
			t.Errorf("ListFiles OwningRepo = %q, want %q (namespace prefix must not leak)", f.OwningRepo, repoFoo)
		}
	}
	if len(files) != 2 {
		t.Errorf("ListFiles len = %d, want 2", len(files))
	}
}

func TestRegistry_AppendRefs_Forwards(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := reg.AppendRefs(ctx, "alpha", repoFoo, "1.0.0", "latest"); err != nil {
		t.Fatalf("AppendRefs: %v", err)
	}
	// Assert the alias landed under the namespace-scoped repo.
	if got, ok := fake.Aliases["alpha/"+repoFoo+"/latest"]; !ok || got != "1.0.0" {
		t.Errorf("alias under alpha/%s/latest = %q (ok=%v), want 1.0.0", repoFoo, got, ok)
	}
}

func TestRegistry_DeleteRepoFiles_ClearsBackendAndIndex(t *testing.T) {
	t.Parallel()

	fake, reg, store := setup(t)
	putNamespace(t, store, "alpha", allowAllSpec())
	ctx := aliceCtx(t)

	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := reg.DeleteRepoFiles(ctx, "alpha", repoFoo); err != nil {
		t.Fatalf("DeleteRepoFiles: %v", err)
	}
	for k := range fake.Files {
		if strings.HasPrefix(k, "alpha/"+repoFoo+"/") {
			t.Errorf("file %q remained after DeleteRepoFiles", k)
		}
	}
	// Re-AddFile must repopulate the package index — DeleteRepoFiles
	// drops the in-process indexed marker.
	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "2.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile after delete: %v", err)
	}
	pkgs, err := reg.ListPackages(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if diff := cmp.Diff([]string{repoFoo}, pkgs); diff != "" {
		t.Errorf("ListPackages mismatch (-want +got):\n%s", diff)
	}
}

// TestRegistry_PolicyCache_HotPathSkipsStore counts the spec.json
// reads the metadata Store performs and asserts the cache absorbs all
// but the first lookup across N data-plane requests. The
// countingBackend below double-duties as the data plane's backend AND
// the metadata store's backend, so we can isolate spec-file reads by
// path matching.
func TestRegistry_PolicyCache_HotPathSkipsStore(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))

	if err := store.Put(t.Context(), &namespace.Namespace{Name: "alpha", Spec: allowAllSpec()}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ctx := aliceCtx(t)

	// Seed one write so subsequent reads have something to fetch.
	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	before := counted.specReads("alpha")
	const iters = 50
	for i := 0; i < iters; i++ {
		_, _, err := reg.ReadFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"))
		if err != nil {
			t.Fatalf("ReadFile %d: %v", i, err)
		}
	}
	after := counted.specReads("alpha")
	if after-before != 0 {
		t.Errorf("spec re-fetched %d times across %d requests, want 0 (cache must absorb)",
			after-before, iters)
	}
}

// TestRegistry_PolicyCache_TTLZero_DisablesCache is the inverse of the
// above: with caching disabled, every request must re-fetch the spec.
func TestRegistry_PolicyCache_TTLZero_DisablesCache(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(0))

	if err := store.Put(t.Context(), &namespace.Namespace{Name: "alpha", Spec: allowAllSpec()}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ctx := aliceCtx(t)

	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	before := counted.specReads("alpha")
	const iters = 5
	for i := 0; i < iters; i++ {
		_, _, err := reg.ReadFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"))
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

// TestRegistry_PolicyCache_NegativeCaching pins that a 404 from
// Store.Get is cached too — a hot loop hammering an unknown
// namespace must not turn into N store reads.
func TestRegistry_PolicyCache_NegativeCaching(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))
	ctx := aliceCtx(t)

	const iters = 10
	for i := 0; i < iters; i++ {
		_, _, err := reg.ReadFile(ctx, "ghost", newRepoFile(repoFoo, "1.0.0", "f.txt"))
		if !errors.Is(err, namespace.ErrNotFound) {
			t.Fatalf("ReadFile(ghost) %d err = %v, want errors.Is(ErrNotFound)", i, err)
		}
	}
	// The first miss should pay one spec read; the rest are absorbed
	// by the negative cache. >= 2 here would be a regression.
	if got := counted.specReads("ghost"); got > 1 {
		t.Errorf("spec reads for missing namespace = %d, want <= 1 (negative cache must absorb)", got)
	}
}

// TestRegistry_InvalidatePolicy_PicksUpNewSpec exercises the
// admin-side invalidation path: a Put followed by Invalidate must
// take effect on the very next call.
func TestRegistry_InvalidatePolicy_PicksUpNewSpec(t *testing.T) {
	t.Parallel()

	counted := newCountingBackend()
	store := namespace.NewStore(counted)
	reg := namespace.NewRegistry(counted, store, namespace.WithPolicyCacheTTL(time.Hour))
	ctx := aliceCtx(t)

	// Initial spec denies alice.
	putNamespace(t, store, "alpha", namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Email: otherEmail}},
	}})
	if _, _, err := reg.ReadFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt")); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("ReadFile pre-update err = %v, want errors.Is(auth.ErrUnauthorized)", err)
	}

	// Update the spec to allow alice — without invalidation the cache
	// would still deny.
	putNamespace(t, store, "alpha", allowAllSpec())
	reg.InvalidatePolicy("alpha")

	// Seed via AddFile (now permitted) so the subsequent ReadFile
	// tests authz, not file presence.
	if _, err := reg.AddFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt"), strings.NewReader(defaultBody)); err != nil {
		t.Fatalf("AddFile post-invalidate: %v", err)
	}
	if _, _, err := reg.ReadFile(ctx, "alpha", newRepoFile(repoFoo, "1.0.0", "f.txt")); err != nil {
		t.Errorf("ReadFile post-update err = %v, want nil", err)
	}
}

// countingBackend wraps an [oci.FakeRegistry] and counts how many
// times the namespace metadata spec.json file has been read for each
// namespace. We use the counter to prove the policy cache absorbs
// repeated lookups: every wrapper call goes through ReadFile, but
// only spec-file reads — those whose path matches "<ns>/_metadata/spec.json"
// — represent uncached metadata-store traffic.
type countingBackend struct {
	*oci.FakeRegistry
	mu         sync.Mutex
	specReads_ map[string]int
}

func newCountingBackend() *countingBackend {
	return &countingBackend{
		FakeRegistry: oci.NewFakeRegistry(),
		specReads_:   map[string]int{},
	}
}

func (c *countingBackend) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	if isSpecRead(f) {
		c.mu.Lock()
		c.specReads_[f.OwningRepo]++
		c.mu.Unlock()
	}
	return c.FakeRegistry.ReadFile(ctx, f)
}

func (c *countingBackend) specReads(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.specReads_[name]
}

func isSpecRead(f *oci.RepoFile) bool {
	// The Store stores the spec at "<name>/_metadata/spec.json" with
	// the default empty prefix. Anything matching that pattern is
	// metadata traffic.
	return f.OwningTag == "_metadata" && f.Name == "spec.json"
}

// slicesSortedKeys returns the keys of m sorted; lifted out so
// failure messages are stable across runs.
func slicesSortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
