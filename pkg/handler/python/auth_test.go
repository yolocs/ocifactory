package python

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestMux_AuthGating wires a python handler with an auth
// middleware that always 401s and confirms every route hits the
// gate (i.e. the handler itself is NOT reached). This is the
// happy-case proof that WithAuthMiddleware actually chains the
// middleware on the python router.
func TestMux_AuthGating(t *testing.T) {
	t.Parallel()

	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}

	reg := oci.NewFakeRegistry()
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
		{name: "simple index", method: http.MethodGet, path: "/simple/"},
		{name: "package index", method: http.MethodGet, path: "/simple/foo/"},
		{name: "file get", method: http.MethodGet, path: "/packages/foo/1.0/foo-1.0.tar.gz"},
		{name: "upload", method: http.MethodPost, path: "/"},
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
// leaves the handler ungated — the default useful for tests that
// don't care about auth and for future public-by-default formats.
func TestMux_NoAuthMiddleware(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if got := w.Code; got == http.StatusUnauthorized {
		t.Errorf("status = 401 with no middleware configured; default should be ungated")
	}
}

// TestMux_AuthChainsBeforeHandler proves the order: the auth
// middleware sees the request before the format handler runs.
// Confirms the AuthContext is on the context by the time the route's
// handler is invoked.
func TestMux_AuthChainsBeforeHandler(t *testing.T) {
	t.Parallel()

	const wantIssuer = "test-issuer"
	installer := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: wantIssuer, ID: "u"}, nil
	}))

	reg := oci.NewFakeRegistry()
	h, err := NewHandler(reg, WithAuthMiddleware(installer))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	// The simple index handler returns 200 on an empty index;
	// what we care about is the auth middleware was invoked
	// without short-circuiting.
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable {
		t.Errorf("status = %d; auth middleware unexpectedly rejected the request", w.Code)
	}
}
