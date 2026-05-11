package admin_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler/admin"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestHandler_NamespaceCRUD(t *testing.T) {
	t.Parallel()

	allowAlice := namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Email: "alice@example.com"}}}}
	allowBob := namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Email: "bob@example.com"}}}}

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		rawBody    string
		seed       func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store)
		wantStatus int
		wantBody   any
	}{
		{
			name:       "put new namespace",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/alpha",
			body:       allowAlice,
			wantStatus: http.StatusCreated,
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowAlice},
		},
		{
			name:   "put existing namespace",
			method: http.MethodPut,
			path:   "/admin/v1/namespaces/alpha",
			body:   allowBob,
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", allowAlice)
			},
			wantStatus: http.StatusOK,
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowBob},
		},
		{
			name:       "put invalid name",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/Bad_Name",
			body:       namespace.Spec{},
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid namespace name: \"Bad_Name\" contains invalid character 'B'"},
		},
		{
			name:       "put invalid spec",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/badspec",
			body:       namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{}}}},
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "readers[0]: invalid policy: matcher must populate at least one field"},
		},
		{
			name:       "put malformed json",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/badjson",
			rawBody:    `}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid JSON body: invalid character '}' looking for beginning of value"},
		},
		{
			name:       "put unknown json field",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/unknownfield",
			rawBody:    `{"bogus":true}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid JSON body: json: unknown field \"bogus\""},
		},
		{
			name:   "get existing namespace",
			method: http.MethodGet,
			path:   "/admin/v1/namespaces/alpha",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", allowBob)
			},
			wantStatus: http.StatusOK,
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowBob},
		},
		{
			name:       "get unknown namespace",
			method:     http.MethodGet,
			path:       "/admin/v1/namespaces/missing",
			wantStatus: http.StatusNotFound,
			wantBody:   map[string]string{"error": "namespace not found: missing"},
		},
		{
			name:       "list namespaces empty",
			method:     http.MethodGet,
			path:       "/admin/v1/namespaces",
			wantStatus: http.StatusOK,
			wantBody:   map[string][]string{"namespaces": {}},
		},
		{
			name:   "list namespaces",
			method: http.MethodGet,
			path:   "/admin/v1/namespaces",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", namespace.Spec{})
				putNamespaceNow(t, store, "beta", namespace.Spec{})
			},
			wantStatus: http.StatusOK,
			wantBody:   map[string][]string{"namespaces": {"alpha", "beta"}},
		},
		{
			name:   "delete non-empty namespace without cascade",
			method: http.MethodDelete,
			path:   "/admin/v1/namespaces/alpha",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", namespace.Spec{})
				reg.AddTag(path.Join("alpha", "ocifactory-packages"), hex.EncodeToString([]byte("pkg")))
			},
			wantStatus: http.StatusConflict,
			wantBody:   map[string]string{"error": "namespace is not empty; pass ?cascade=true to delete all packages"},
		},
		{
			name:   "delete empty namespace",
			method: http.MethodDelete,
			path:   "/admin/v1/namespaces/beta",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "beta", namespace.Spec{})
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name:   "delete empty namespace with cascade is a no-op cascade",
			method: http.MethodDelete,
			path:   "/admin/v1/namespaces/beta?cascade=true",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "beta", namespace.Spec{})
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "delete unknown namespace",
			method:     http.MethodDelete,
			path:       "/admin/v1/namespaces/beta",
			wantStatus: http.StatusNotFound,
			wantBody:   map[string]string{"error": "namespace not found: beta"},
		},
		{
			name:       "delete unknown namespace with cascade is still 404",
			method:     http.MethodDelete,
			path:       "/admin/v1/namespaces/default?cascade=true",
			wantStatus: http.StatusNotFound,
			wantBody:   map[string]string{"error": "namespace not found: default"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := oci.NewFakeRegistry()
			store := namespace.NewStore(reg)
			nsReg := namespace.NewRegistry(reg, store)
			h, err := admin.NewHandler(store, nsReg)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			srv := httptest.NewServer(h.Mux())
			t.Cleanup(srv.Close)

			if tc.seed != nil {
				tc.seed(t, reg, store)
			}
			var body []byte
			if tc.rawBody != "" {
				body = []byte(tc.rawBody)
			} else if tc.body != nil {
				var err error
				body, err = json.Marshal(tc.body)
				if err != nil {
					t.Fatalf("Marshal body: %v", err)
				}
			}
			req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+tc.path, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if diff := cmp.Diff(tc.wantStatus, resp.StatusCode); diff != "" {
				t.Fatalf("status mismatch (-want +got):\n%s", diff)
			}
			if tc.wantBody == nil {
				return
			}
			assertJSONBody(t, resp, tc.wantBody)
		})
	}
}

func TestNewHandler_RequiresDependencies(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	store := namespace.NewStore(reg)
	nsReg := namespace.NewRegistry(reg, store)

	tests := []struct {
		name      string
		store     admin.Store
		packages  admin.PackageRegistry
		wantError string
	}{
		{name: "missing store", packages: nsReg, wantError: "store is required"},
		{name: "missing package registry", store: store, wantError: "package registry is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := admin.NewHandler(tc.store, tc.packages)
			if diff := cmp.Diff(tc.wantError, errString(err)); diff != "" {
				t.Errorf("NewHandler() error mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func putNamespaceNow(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("Store.Put(%q): %v", name, err)
	}
}

// TestHandler_DeleteCascade exercises the full cascade flow against a
// fully-populated namespace: writes land in the OCI fake via the
// namespace.ScopedRegistry (the same code path data-plane handlers
// use), then DELETE ?cascade=true must wipe every sub-repo plus the
// package index plus the namespace metadata.
func TestHandler_DeleteCascade(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	nsReg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespaceNow(t, store, "alpha", allowAllAdminSpec())

	scoped := nsReg.For("alpha")
	ctx := allowAllCtx(t)
	for _, repo := range []string{"packages/foo", "packages/bar", "tools/cli"} {
		writeAdminFile(t, ctx, scoped, repo, "1.0.0", "f.txt", "body")
	}

	h, err := admin.NewHandler(store, nsReg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)

	doAdminRequest(t, srv.URL+"/admin/v1/namespaces/alpha?cascade=true", http.MethodDelete, http.StatusNoContent)

	// Every sub-repo gone.
	for _, repo := range []string{"alpha/packages/foo", "alpha/packages/bar", "alpha/tools/cli"} {
		for k := range fake.Files {
			if strings.HasPrefix(k, repo+"/") {
				t.Errorf("file %q remained after cascade", k)
			}
		}
	}
	// Package index repo gone.
	if got, ok := fake.Tags["alpha/ocifactory-packages"]; ok {
		t.Errorf("package index tags = %v, want absent", got)
	}
	// Namespace metadata + global index entry gone.
	if got, ok := fake.Tags["alpha"]; ok {
		t.Errorf("namespace metadata tags = %v, want absent", got)
	}
	if got, ok := fake.Tags["ocifactory-namespaces"]; ok && slices.Contains(got, "alpha") {
		t.Errorf("global namespace index = %v, still contains alpha", got)
	}

	// Re-create the same-name namespace and write a package: the
	// in-process indexed-LRU sweep must let recordPackage land a
	// fresh tag in the new index repo.
	putNamespaceNow(t, store, "alpha", allowAllAdminSpec())
	writeAdminFile(t, ctx, nsReg.For("alpha"), "packages/foo", "2.0.0", "f.txt", "body")
	pkgs, err := nsReg.ListPackages(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("ListPackages after re-create: %v", err)
	}
	if diff := cmp.Diff([]string{"packages/foo"}, pkgs); diff != "" {
		t.Errorf("ListPackages after re-create mismatch (-want +got):\n%s", diff)
	}
}

// TestHandler_DeleteCascade_Resumable simulates a partial failure
// mid-cascade and verifies a retried DELETE converges. The
// flakyBackend fails the third DeleteRepoFiles call; the first
// DELETE returns 500, the second succeeds.
func TestHandler_DeleteCascade_Resumable(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	flaky := &flakyBackend{FakeRegistry: fake}
	store := namespace.NewStore(flaky)
	nsReg := namespace.NewRegistry(flaky, store, namespace.WithPolicyCacheTTL(0))
	putNamespaceNow(t, store, "alpha", allowAllAdminSpec())

	scoped := nsReg.For("alpha")
	ctx := allowAllCtx(t)
	repos := []string{"packages/aaa", "packages/bbb", "packages/ccc", "packages/ddd"}
	for _, repo := range repos {
		writeAdminFile(t, ctx, scoped, repo, "1.0.0", "f.txt", "body")
	}

	h, err := admin.NewHandler(store, nsReg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)

	flaky.failAfter.Store(2) // succeed twice, fail the third call
	doAdminRequest(t, srv.URL+"/admin/v1/namespaces/alpha?cascade=true", http.MethodDelete, http.StatusInternalServerError)

	// At least one sub-repo is still around (the one whose delete
	// was rejected). The namespace metadata is intact.
	if _, ok := fake.Tags["alpha"]; !ok {
		t.Fatal("namespace metadata gone after partial cascade; resumability is impossible")
	}
	survivors := 0
	for _, repo := range repos {
		if _, ok := fake.Tags["alpha/"+repo]; ok {
			survivors++
		}
	}
	if survivors == 0 {
		t.Fatal("expected at least one sub-repo to survive the partial cascade")
	}

	// Stop the failure injection and retry. The cascade must
	// converge: every sub-repo gone, namespace metadata gone.
	flaky.failAfter.Store(-1)
	doAdminRequest(t, srv.URL+"/admin/v1/namespaces/alpha?cascade=true", http.MethodDelete, http.StatusNoContent)

	for _, repo := range repos {
		if _, ok := fake.Tags["alpha/"+repo]; ok {
			t.Errorf("sub-repo alpha/%s remained after retry", repo)
		}
	}
	if _, ok := fake.Tags["alpha"]; ok {
		t.Error("namespace metadata still present after successful retry")
	}
}

func allowAllAdminSpec() namespace.Spec {
	return namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}}
}

func allowAllCtx(t *testing.T) context.Context {
	t.Helper()
	return auth.WithAuthContext(t.Context(), &auth.AuthContext{
		Issuer: "https://accounts.google.com",
		ID:     "alice",
		Email:  "alice@example.com",
	})
}

func writeAdminFile(t *testing.T, ctx context.Context, scoped *namespace.ScopedRegistry, repo, tag, name, body string) {
	t.Helper()
	rf := &oci.RepoFile{
		OwningRepo: repo,
		OwningTag:  tag,
		Name:       name,
		MediaType:  "application/octet-stream",
		Size:       int64(len(body)),
	}
	if _, err := scoped.AddFile(ctx, rf, strings.NewReader(body)); err != nil {
		t.Fatalf("AddFile %s/%s/%s: %v", repo, tag, name, err)
	}
}

func doAdminRequest(t *testing.T, url, method string, wantStatus int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status = %d, want %d (body: %s)", method, url, resp.StatusCode, wantStatus, body)
	}
}

// flakyBackend wraps an oci.FakeRegistry and returns an injected
// error from DeleteRepoFiles when failAfter reaches zero. Used to
// simulate a partial cascade failure for resumability tests.
// failAfter is decremented on each call; negative values disable the
// failure injection.
type flakyBackend struct {
	*oci.FakeRegistry
	failAfter atomic.Int32
}

func (f *flakyBackend) DeleteRepoFiles(ctx context.Context, repo string) error {
	for {
		remaining := f.failAfter.Load()
		if remaining == 0 {
			return errors.New("simulated backend failure")
		}
		if remaining < 0 {
			return f.FakeRegistry.DeleteRepoFiles(ctx, repo)
		}
		if f.failAfter.CompareAndSwap(remaining, remaining-1) {
			return f.FakeRegistry.DeleteRepoFiles(ctx, repo)
		}
	}
}

func assertJSONBody(t *testing.T, resp *http.Response, want any) {
	t.Helper()
	if got, want := resp.Header.Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	switch want := want.(type) {
	case *namespace.Namespace:
		var got namespace.Namespace
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("Decode namespace: %v", err)
		}
		if diff := cmp.Diff(*want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	case map[string]string:
		var got map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("Decode error body: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	case map[string][]string:
		var got map[string][]string
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("Decode list body: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	default:
		t.Fatalf("unsupported want body type %T", want)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
