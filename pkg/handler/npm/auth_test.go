package npm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestMux_AuthGating wires an npm handler with an auth middleware
// that always 401s and confirms every namespaced route hits the gate
// — i.e. the handler itself is NOT reached, and the namespace wrapper
// has no chance to deny first. Mirrors the python and maven
// equivalents.
func TestMux_AuthGating(t *testing.T) {
	t.Parallel()

	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	h, err := NewHandler(reg, WithAuthMiddleware(denyAll))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "ping root", method: http.MethodGet, path: "/" + testNS + "/"},
		{name: "ping endpoint", method: http.MethodGet, path: "/" + testNS + "/-/ping"},
		{name: "packument get", method: http.MethodGet, path: "/" + testNS + "/foo"},
		{name: "packument head", method: http.MethodHead, path: "/" + testNS + "/foo"},
		{name: "scoped packument", method: http.MethodGet, path: "/" + testNS + "/@scope/foo"},
		{name: "tarball get", method: http.MethodGet, path: "/" + testNS + "/foo/-/foo-1.0.0.tgz"},
		{name: "scoped tarball get", method: http.MethodGet, path: "/" + testNS + "/@scope/foo/-/foo-1.0.0.tgz"},
		{name: "publish", method: http.MethodPut, path: "/" + testNS + "/foo", body: "{}"},
		{name: "dist-tag list", method: http.MethodGet, path: "/" + testNS + "/-/package/foo/dist-tags"},
		{name: "dist-tag put", method: http.MethodPut, path: "/" + testNS + "/-/package/foo/dist-tags/latest", body: `"1.0.0"`},
		{name: "dist-tag delete", method: http.MethodDelete, path: "/" + testNS + "/-/package/foo/dist-tags/latest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			r := httptest.NewRequest(tc.method, tc.path, body)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if got, want := w.Code, http.StatusUnauthorized; got != want {
				t.Errorf("status=%d, want %d (auth middleware not chained?) (body=%s)", got, want, w.Body.String())
			}
		})
	}
}

// TestMux_NoAuthMiddleware confirms that omitting WithAuthMiddleware
// leaves the chain ungated at the middleware layer — production
// wiring (cmd/ocifactory serve) always supplies one. With no
// AuthContext on the request, the namespace wrapper denies at 403.
func TestMux_NoAuthMiddleware(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: allowAllPolicy()})

	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/foo", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got := w.Code; got == http.StatusUnauthorized {
		t.Errorf("status=401 with no middleware configured; the namespace wrapper, not the middleware, should deny")
	}
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status=%d, want %d (no AuthContext → wrapper denies)", got, want)
	}
}

// TestMux_AuthChainsBeforeHandler proves the order: the auth
// middleware sees the request before any npm route handler runs.
func TestMux_AuthChainsBeforeHandler(t *testing.T) {
	t.Parallel()

	installer := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "anonymous", ID: "u"}, nil
	}))

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putNamespace(t, store, testNS, namespace.Spec{Policy: allowAllPolicy()})

	h, err := NewHandler(reg, WithAuthMiddleware(installer))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/-/ping", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable {
		t.Errorf("status=%d; auth middleware unexpectedly rejected the request", w.Code)
	}
}
