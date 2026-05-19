package npm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/artifact"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestNamespace_UnknownNamespace404 confirms a request to an
// unregistered namespace surfaces as 404 — the wrapper turns
// ErrNotFound into a sentinel WriteNamespaceError maps to the right
// status.
func TestNamespace_UnknownNamespace404(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := artifact.NewStore(fake, store, artifact.WithPolicyCacheTTL(0))
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
		{name: "packument", method: http.MethodGet, path: "/ghost/foo"},
		{name: "tarball", method: http.MethodGet, path: "/ghost/foo/-/foo-1.0.0.tgz"},
		{name: "dist-tag list", method: http.MethodGet, path: "/ghost/-/package/foo/dist-tags"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)
			if got, want := w.Code, http.StatusNotFound; got != want {
				t.Errorf("status=%d, want %d (body=%s)", got, want, w.Body.String())
			}
		})
	}
}

// TestNamespace_InvalidNamespaceName400 confirms a malformed namespace
// segment surfaces as 400, not 500.
func TestNamespace_InvalidNamespaceName400(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := artifact.NewStore(fake, store, artifact.WithPolicyCacheTTL(0))
	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/UPPER/foo", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Errorf("status=%d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_NotInReadersForbidden confirms a subject that is not
// in the namespace's readers list gets 403 on packument GET.
func TestNamespace_NotInReadersForbidden(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := artifact.NewStore(fake, store, artifact.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/foo", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status=%d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_NotInWritersForbiddenOnPublish confirms a subject not
// in the writers list gets 403 on publish.
func TestNamespace_NotInWritersForbiddenOnPublish(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := artifact.NewStore(fake, store, artifact.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	rec := doPublish(t, h, "example", "1.0.0", []byte("payload"), nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("publish status=%d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestNamespace_CrossNamespaceIsolation verifies a package published
// to ns1 is not visible from ns2.
func TestNamespace_CrossNamespaceIsolation(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := artifact.NewStore(fake, store, artifact.WithPolicyCacheTTL(0))
	putNamespace(t, store, "alpha", namespace.Spec{Policy: allowAllPolicy()})
	putNamespace(t, store, "beta", namespace.Spec{Policy: allowAllPolicy()})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	rec := doPublishInNS(t, h, "alpha", "example", "1.0.0", []byte("payload"), map[string]string{"latest": "1.0.0"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish to alpha status=%d (body=%s)", rec.Code, rec.Body.String())
	}

	// alpha sees the package.
	r := httptest.NewRequest(http.MethodGet, "/alpha/example", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("alpha packument status=%d (body=%s)", w.Code, w.Body.String())
	}
	var pak packument
	if err := json.Unmarshal(w.Body.Bytes(), &pak); err != nil {
		t.Fatalf("unmarshal alpha packument: %v", err)
	}
	if _, ok := pak.Versions["1.0.0"]; !ok {
		t.Errorf("alpha packument missing version 1.0.0: %v", pak)
	}

	// beta does not.
	r = httptest.NewRequest(http.MethodGet, "/beta/example", nil)
	w = httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("beta packument status=%d, want 404", w.Code)
	}

	// Direct tarball fetch under beta misses too.
	r = httptest.NewRequest(http.MethodGet, "/beta/example/-/example-1.0.0.tgz", nil)
	w = httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("beta tarball status=%d, want 404", w.Code)
	}
}
