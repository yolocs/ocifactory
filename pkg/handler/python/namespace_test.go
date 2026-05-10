package python

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestNamespace_UnknownNamespace404 confirms a request to an
// unregistered namespace surfaces as 404 — the wrapper turns
// ErrNotFound into a sentinel the handler maps to the right status.
func TestNamespace_UnknownNamespace404(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "simple index", method: http.MethodGet, path: "/ghost/simple/"},
		{name: "package index", method: http.MethodGet, path: "/ghost/simple/foo/"},
		{name: "file get", method: http.MethodGet, path: "/ghost/packages/foo/1.0.0/foo-1.0.0.whl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)
			if got, want := w.Code, http.StatusNotFound; got != want {
				t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
			}
		})
	}
}

// TestNamespace_UnknownNamespaceUpload404 confirms POST to an
// unknown namespace also returns 404 (writes follow the same path).
func TestNamespace_UnknownNamespaceUpload404(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	if code := uploadPackageInNS(t, h, "ghost", "example", "1.0.0"); code != http.StatusNotFound {
		t.Errorf("upload to ghost ns status=%d, want 404", code)
	}
}

// TestNamespace_NotInReadersForbiddenOnGet confirms a subject that is
// not in the namespace's readers list gets 403 on GET /simple/.
func TestNamespace_NotInReadersForbiddenOnGet(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	// Subject is "anonymous"; the policy only admits a different issuer.
	putNamespace(t, store, testNS, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_NotInWritersForbiddenOnPost confirms a subject not in
// the writers list gets 403 on twine upload (POST /).
func TestNamespace_NotInWritersForbiddenOnPost(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: namespace.Policy{
		// Readers allow our anonymous subject, but writers do not.
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// Read succeeds, write fails — proves the policy split.
	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got := w.Code; got != http.StatusOK {
		t.Errorf("read status = %d, want 200 (readers should allow)", got)
	}

	if code := uploadPackage(t, h, "example", "1.0.0"); code != http.StatusForbidden {
		t.Errorf("upload status=%d, want 403 (writers should deny)", code)
	}
}

// TestNamespace_CrossNamespaceIsolation verifies a package uploaded
// to ns1 is not visible from ns2's /simple/ index — the load-bearing
// security guarantee of the wrapper.
func TestNamespace_CrossNamespaceIsolation(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, "alpha", namespace.Spec{Policy: allowAllPolicy()})
	putNamespace(t, store, "beta", namespace.Spec{Policy: allowAllPolicy()})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	if code := uploadPackageInNS(t, h, "alpha", "requests", "1.0.0"); code != http.StatusCreated {
		t.Fatalf("upload to alpha: status=%d", code)
	}

	// alpha sees the package.
	r := httptest.NewRequest(http.MethodGet, "/alpha/simple/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("alpha simple status = %d", got)
	}
	if !strings.Contains(w.Body.String(), "/alpha/simple/requests/") {
		t.Errorf("alpha simple index missing requests link, got:\n%s", w.Body.String())
	}

	// beta does not.
	r = httptest.NewRequest(http.MethodGet, "/beta/simple/", nil)
	w = httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("beta simple status = %d", got)
	}
	if strings.Contains(w.Body.String(), "requests") {
		t.Errorf("beta simple index leaked alpha's 'requests' package:\n%s", w.Body.String())
	}

	// Direct file fetch under beta misses too.
	r = httptest.NewRequest(http.MethodGet, "/beta/packages/requests/1.0.0/requests-1.0.0.whl", nil)
	w = httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("beta file fetch status = %d, want %d", got, want)
	}
}

// TestNamespace_PackageIndexCacheIsolation verifies the per-package
// simple-index cache is keyed by (namespace, package) so ns1's file
// list cannot leak into ns2's render of an identically-named package.
func TestNamespace_PackageIndexCacheIsolation(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, "alpha", namespace.Spec{Policy: allowAllPolicy()})
	putNamespace(t, store, "beta", namespace.Spec{Policy: allowAllPolicy()})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	if code := uploadPackageInNS(t, h, "alpha", "requests", "1.0.0"); code != http.StatusCreated {
		t.Fatalf("upload to alpha: status=%d", code)
	}

	// Prime alpha's cache.
	r := httptest.NewRequest(http.MethodGet, "/alpha/simple/requests/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("alpha requests status = %d", w.Code)
	}
	// beta's identically-named requests must still resolve via its
	// own (empty) listing, not borrow alpha's cache entry.
	r = httptest.NewRequest(http.MethodGet, "/beta/simple/requests/", nil)
	w = httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "requests-1.0.0.whl") {
		t.Errorf("beta simple/requests/ leaked alpha's wheel via cache:\n%s", w.Body.String())
	}
}

// TestNamespace_NoAuthForbiddenAtWrapper proves the wrapper is the
// trust boundary: with no AuthContext on the request, the namespace
// wrapper denies before ever consulting the policy.
func TestNamespace_NoAuthForbiddenAtWrapper(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store,
		namespace.WithAuthzFactory(func(_ namespace.Policy) (auth.Authorizer, error) { return auth.AllowAll, nil }),
		namespace.WithPolicyCacheTTL(0),
	)
	putNamespace(t, store, testNS, namespace.Spec{Policy: allowAllPolicy()})

	// No auth middleware installed — request reaches the wrapper
	// with no AuthContext on ctx.
	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (wrapper must deny nil AuthContext even with AllowAll factory)", got, want)
	}
}
