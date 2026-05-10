package maven

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestMux_AuthGating wires a maven handler with an auth middleware
// that always 401s and confirms every namespaced route hits the gate.
// Mirrors the python equivalent — symmetric plumbing must have
// symmetric tests.
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
	}{
		{name: "archetype catalog", method: http.MethodGet, path: nsPath("/archetype-catalog.xml")},
		{name: "snapshot metadata", method: http.MethodGet, path: nsPath("/com/example/foo/1.0-SNAPSHOT/maven-metadata.xml")},
		{name: "release metadata", method: http.MethodGet, path: nsPath("/com/example/foo/maven-metadata.xml")},
		{name: "regular artifact", method: http.MethodPut, path: nsPath("/com/example/foo/1.0/foo-1.0.jar")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if got, want := w.Code, http.StatusUnauthorized; got != want {
				t.Errorf("status = %d, want %d (auth middleware not chained?)", got, want)
			}
		})
	}
}

// TestMux_NoAuthMiddleware confirms that omitting WithAuthMiddleware
// leaves the chain ungated at the middleware layer — production
// wiring (cmd/ocifactory serve) always supplies one. With no
// AuthContext on the request the namespace wrapper denies at 403.
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

	r := httptest.NewRequest(http.MethodGet, nsPath("/archetype-catalog.xml"), nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got := w.Code; got == http.StatusUnauthorized {
		t.Errorf("status = 401 with no middleware configured; the namespace wrapper, not the middleware, should deny")
	}
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (no AuthContext → wrapper denies)", got, want)
	}
}

// TestMux_AuthChainsBeforeHandler proves the middleware sees the
// request before any maven route handler runs.
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

	r := httptest.NewRequest(http.MethodGet, nsPath("/archetype-catalog.xml"), nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable {
		t.Errorf("status = %d; auth middleware unexpectedly rejected the request", w.Code)
	}
}
