package maven

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestMux_AuthGating wires a maven handler with an auth
// middleware that always 401s and confirms every route hits the
// gate. Mirrors the python equivalent — symmetric plumbing must
// have symmetric tests.
func TestMux_AuthGating(t *testing.T) {
	t.Parallel()

	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}

	reg := oci.NewFakeRegistry()
	h, err := newHandlerWithRegistry(t, reg, WithAuthMiddleware(denyAll))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "archetype catalog", method: http.MethodGet, path: "/default/maven2/archetype-catalog.xml"},
		{name: "snapshot metadata", method: http.MethodGet, path: "/default/maven2/com/example/foo/1.0-SNAPSHOT/maven-metadata.xml"},
		{name: "release metadata", method: http.MethodGet, path: "/default/maven2/com/example/foo/maven-metadata.xml"},
		{name: "regular artifact", method: http.MethodPut, path: "/default/maven2/com/example/foo/1.0/foo-1.0.jar"},
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
// leaves the handler ungated. Default useful for tests; production
// wiring (cmd/ocifactory serve) always supplies a middleware. The
// AGENTS.md rule for adding new format handlers reminds authors
// to plumb WithAuthMiddleware on the serve.go side.
func TestMux_NoAuthMiddleware(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, err := newHandlerWithRegistry(t, reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/default/maven2/archetype-catalog.xml", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if got := w.Code; got == http.StatusUnauthorized {
		t.Errorf("status = 401 with no middleware configured; default should be ungated")
	}
}

// TestMux_AuthChainsBeforeHandler proves the middleware sees the
// request before any maven route handler runs.
func TestMux_AuthChainsBeforeHandler(t *testing.T) {
	t.Parallel()

	installer := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test-issuer", ID: "u"}, nil
	}))

	reg := oci.NewFakeRegistry()
	h, err := newHandlerWithRegistry(t, reg, WithAuthMiddleware(installer))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/default/maven2/archetype-catalog.xml", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable {
		t.Errorf("status = %d; auth middleware unexpectedly rejected the request", w.Code)
	}
}
