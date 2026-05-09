package maven

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestAuthz_PerRoute proves every Maven route translates the
// inbound HTTP request into the expected (Repo, Format, Op)
// Action and that auth.ErrUnauthorized maps to 403.
func TestAuthz_PerRoute(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	tests := []struct {
		name    string
		method  string
		path    string
		wantAct auth.Action
	}{
		{
			name:    "archetype catalog read",
			method:  http.MethodGet,
			path:    "/archetype-catalog.xml",
			wantAct: auth.Action{Repo: "archetype", Format: RepoType, Op: auth.OpRead},
		},
		{
			name:    "archetype catalog write",
			method:  http.MethodPut,
			path:    "/archetype-catalog.xml",
			wantAct: auth.Action{Repo: "archetype", Format: RepoType, Op: auth.OpWrite},
		},
		{
			name:    "snapshot metadata read",
			method:  http.MethodGet,
			path:    "/com/example/foo/1.0-SNAPSHOT/maven-metadata.xml",
			wantAct: auth.Action{Repo: "com/example/foo", Format: RepoType, Op: auth.OpRead},
		},
		{
			name:    "snapshot metadata write",
			method:  http.MethodPut,
			path:    "/com/example/foo/1.0-SNAPSHOT/maven-metadata.xml",
			wantAct: auth.Action{Repo: "com/example/foo", Format: RepoType, Op: auth.OpWrite},
		},
		{
			name:    "release metadata read",
			method:  http.MethodGet,
			path:    "/com/example/foo/maven-metadata.xml",
			wantAct: auth.Action{Repo: "com/example/foo", Format: RepoType, Op: auth.OpRead},
		},
		{
			name:    "regular artifact read",
			method:  http.MethodGet,
			path:    "/com/example/foo/1.0/foo-1.0.jar",
			wantAct: auth.Action{Repo: "com/example/foo", Format: RepoType, Op: auth.OpRead},
		},
		{
			name:    "regular artifact write",
			method:  http.MethodPut,
			path:    "/com/example/foo/1.0/foo-1.0.jar",
			wantAct: auth.Action{Repo: "com/example/foo", Format: RepoType, Op: auth.OpWrite},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got auth.Action
			authz := auth.AuthorizerFunc(func(_ context.Context, _ *auth.AuthContext, act auth.Action) error {
				got = act
				return auth.ErrUnauthorized
			})
			h, err := NewHandler(oci.NewFakeRegistry(),
				WithAuthMiddleware(installCtx),
				WithAuthorizer(authz),
			)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			srv := h.Mux()

			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
			}
			if got != tc.wantAct {
				t.Errorf("authorizer Action = %+v, want %+v", got, tc.wantAct)
			}
		})
	}
}

// TestAuthz_NoAuthorizerOpen confirms omitting WithAuthorizer
// leaves routes open. Production wiring always supplies one.
func TestAuthz_NoAuthorizerOpen(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	h, err := NewHandler(oci.NewFakeRegistry(), WithAuthMiddleware(installCtx))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/archetype-catalog.xml", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code == http.StatusForbidden {
		t.Errorf("status = 403 with no authorizer; default should be open (body: %s)", w.Body.String())
	}
}

// TestAuthz_InternalError maps a non-ErrUnauthorized authorizer
// error to 500 rather than 403.
func TestAuthz_InternalError(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	authz := auth.AuthorizerFunc(func(context.Context, *auth.AuthContext, auth.Action) error {
		return errors.New("authz backend down")
	})
	h, err := NewHandler(oci.NewFakeRegistry(),
		WithAuthMiddleware(installCtx),
		WithAuthorizer(authz),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/archetype-catalog.xml", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
}
