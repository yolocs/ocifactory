package echo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// TestAuthz_PerRoute proves both echo routes call the authorizer
// with (Repo: "echo", Format: RepoType, Op: read).
func TestAuthz_PerRoute(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	tests := []struct {
		name string
		path string
	}{
		{name: "echo", path: "/echo/hello"},
		{name: "whoami", path: "/whoami"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got auth.Action
			authz := auth.AuthorizerFunc(func(_ context.Context, _ *auth.AuthContext, act auth.Action) error {
				got = act
				return auth.ErrUnauthorized
			})
			h := NewHandler(WithAuthMiddleware(installCtx), WithAuthorizer(authz))
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
			}
			want := auth.Action{Repo: "echo", Format: RepoType, Op: auth.OpRead}
			if got != want {
				t.Errorf("authorizer Action = %+v, want %+v", got, want)
			}
		})
	}
}

// TestAuthz_InternalError confirms a non-sentinel authz error
// becomes 500.
func TestAuthz_InternalError(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	authz := auth.AuthorizerFunc(func(context.Context, *auth.AuthContext, auth.Action) error {
		return errors.New("authz down")
	})
	h := NewHandler(WithAuthMiddleware(installCtx), WithAuthorizer(authz))
	r := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
}
