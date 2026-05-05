package echo

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// TestMux_AuthGating: a deny-all middleware reaches every route.
// Mirrors pkg/handler/python/auth_test.go.
func TestMux_AuthGating(t *testing.T) {
	t.Parallel()

	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}

	h := NewHandler(WithAuthMiddleware(denyAll))
	srv := h.Mux()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "echo", method: http.MethodGet, path: "/echo/hello"},
		{name: "whoami", method: http.MethodGet, path: "/whoami"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if got, want := w.Code, http.StatusUnauthorized; got != want {
				t.Errorf("status = %d, want %d (auth middleware not chained?)", got, want)
			}
		})
	}
}

// TestMux_NoAuthMiddleware: omitting WithAuthMiddleware leaves
// routes ungated (this is what tests want; serve.go always passes
// the option in production).
func TestMux_NoAuthMiddleware(t *testing.T) {
	t.Parallel()

	h := NewHandler()
	srv := h.Mux()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/echo/hello", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if got := w.Code; got == http.StatusUnauthorized {
		t.Errorf("status = 401 with no middleware configured; default should be ungated")
	}
}

// TestMux_AuthChainsBeforeHandler proves the AuthContext put on the
// request by the middleware is visible to the handler.
func TestMux_AuthChainsBeforeHandler(t *testing.T) {
	t.Parallel()

	const wantIssuer = "test-issuer"
	const wantID = "test-id"
	installer := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: wantIssuer, ID: wantID}, nil
	}))

	h := NewHandler(WithAuthMiddleware(installer))
	srv := h.Mux()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/whoami", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), wantIssuer) || !contains(w.Body.String(), wantID) {
		t.Errorf("body = %q; want issuer %q and id %q present", w.Body.String(), wantIssuer, wantID)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
