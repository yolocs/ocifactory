package maven

import (
	"context"
	"crypto/sha1"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestNamespace_UnknownNamespace404 verifies that a request whose
// namespace prefix doesn't match any registered namespace gets a
// 404 (the wrapper's ErrNotFound mapped by the handler).
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
		{name: "archetype catalog GET", method: http.MethodGet, path: "/ghost/maven2/archetype-catalog.xml"},
		{name: "regular artifact GET", method: http.MethodGet, path: "/ghost/maven2/com/example/foo/1.0.0/foo-1.0.0.jar"},
		{name: "regular artifact PUT", method: http.MethodPut, path: "/ghost/maven2/com/example/foo/1.0.0/foo-1.0.0.jar"},
		{name: "release metadata GET", method: http.MethodGet, path: "/ghost/maven2/com/example/foo/maven-metadata.xml"},
		{name: "snapshot metadata GET", method: http.MethodGet, path: "/ghost/maven2/com/example/foo/1.0-SNAPSHOT/maven-metadata.xml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)
			if got, want := w.Code, http.StatusNotFound; got != want {
				t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
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
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "uppercase", path: "/UPPER/maven2/com/example/foo/1.0.0/foo-1.0.0.jar"},
		{name: "leading underscore", path: "/_index/maven2/com/example/foo/1.0.0/foo-1.0.0.jar"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)
			if got, want := w.Code, http.StatusBadRequest; got != want {
				t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
			}
		})
	}
}

// TestNamespace_AuthzErrorFailsClosed pins fail-closed behaviour when
// the Authorizer returns a non-sentinel error mid-request.
func TestNamespace_AuthzErrorFailsClosed(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store,
		namespace.WithAuthzFactory(func(_ namespace.Policy) (auth.Authorizer, error) {
			return auth.AuthorizerFunc(func(_ context.Context, _ *auth.AuthContext, _ auth.Op) error {
				return errors.New("transient backend lookup failure")
			}), nil
		}),
		namespace.WithPolicyCacheTTL(0),
	)
	putNamespace(t, store, testNS, namespace.Spec{Policy: allowAllPolicy()})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, nsPath("/archetype-catalog.xml"), nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code >= 200 && w.Code < 300 {
		t.Errorf("status = %d, want non-2xx (Authorizer error must fail closed)", w.Code)
	}
}

// TestNamespace_NotInReadersForbidden confirms a GET by a subject
// outside the readers list returns 403.
func TestNamespace_NotInReadersForbidden(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	// Seed a file directly so a successful authz would have something
	// to find.
	if _, err := fake.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: testNS + "/com/example/foo",
		OwningTag:  "1.0.0",
		Name:       "foo-1.0.0.jar",
	}, strings.NewReader("jar")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, nsPath("/com/example/foo/1.0.0/foo-1.0.0.jar"), nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_NotInWritersForbidden confirms a PUT by a subject
// outside the writers list returns 403.
func TestNamespace_NotInWritersForbidden(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodPut, nsPath("/com/example/foo/1.0.0/foo-1.0.0.jar"), strings.NewReader("jar"))
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_CrossNamespaceIsolation_Artifact verifies an artifact
// uploaded to alpha is not visible from beta with the same coordinate.
func TestNamespace_CrossNamespaceIsolation_Artifact(t *testing.T) {
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
	mux := h.Mux()

	// Upload to alpha.
	r := httptest.NewRequest(http.MethodPut, "/alpha/maven2/com/example/foo/1.0.0/foo-1.0.0.jar", strings.NewReader("jar content"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload to alpha status=%d (body=%s)", w.Code, w.Body.String())
	}

	// Read from alpha — succeeds.
	r = httptest.NewRequest(http.MethodGet, "/alpha/maven2/com/example/foo/1.0.0/foo-1.0.0.jar", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("read from alpha status=%d", w.Code)
	}

	// Same coordinate under beta — 404.
	r = httptest.NewRequest(http.MethodGet, "/beta/maven2/com/example/foo/1.0.0/foo-1.0.0.jar", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("read from beta status=%d, want %d", got, want)
	}
}

// TestNamespace_CrossNamespaceIsolation_Checksum verifies a checksum
// from ns1 cannot satisfy an artifact in ns2 — uploading a sha1 to
// beta whose contents match an artifact in alpha is rejected because
// beta has no companion artifact.
func TestNamespace_CrossNamespaceIsolation_Checksum(t *testing.T) {
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
	mux := h.Mux()

	const jarBody = "jar content"
	// Publish the artifact in alpha.
	r := httptest.NewRequest(http.MethodPut, "/alpha/maven2/com/example/foo/1.0.0/foo-1.0.0.jar", strings.NewReader(jarBody))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("alpha jar upload status=%d", w.Code)
	}

	// Attempt to publish a checksum companion in beta — the artifact
	// is not present in beta, so verifyChecksumUpload must reject with
	// 400 even though the checksum value itself is valid for alpha's
	// jar.
	r = httptest.NewRequest(
		http.MethodPut,
		"/beta/maven2/com/example/foo/1.0.0/foo-1.0.0.jar.sha1",
		strings.NewReader(hashHex(t, sha1.New(), jarBody)),
	)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Errorf("beta sha1 upload status=%d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_ReservedIndexRepoRejected confirms a writer cannot
// reach the wrapper's own per-namespace package-index repo via a
// maven URL that names "ocifactory-packages" as the first repo
// segment.
func TestNamespace_ReservedIndexRepoRejected(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: allowAllPolicy()})

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg, WithAuthMiddleware(authMW))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodPut, nsPath("/ocifactory-packages/1.0.0/x.jar"), strings.NewReader("jar"))
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
}

// TestNamespace_NoAuthForbiddenAtWrapper proves the maven handler
// honours the wrapper's defense-in-depth nil-AuthContext check: even
// with an AllowAll authorizer plugged in, a request that arrives with
// no AuthContext on the request context gets 403.
func TestNamespace_NoAuthForbiddenAtWrapper(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store,
		namespace.WithAuthzFactory(func(_ namespace.Policy) (auth.Authorizer, error) { return auth.AllowAll, nil }),
		namespace.WithPolicyCacheTTL(0),
	)
	putNamespace(t, store, testNS, namespace.Spec{Policy: allowAllPolicy()})

	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, nsPath("/archetype-catalog.xml"), nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (wrapper must deny nil AuthContext even with AllowAll factory)", got, want)
	}
}
