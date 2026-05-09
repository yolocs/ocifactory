package python

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestAuthz_PerRoute proves every routed handler calls the
// authorizer with the right (Repo, Format, Op) Action and that
// auth.ErrUnauthorized comes back as 403 (not 401). The
// authentication middleware always installs an AuthContext so
// the authorizer is the only thing deciding the verdict.
func TestAuthz_PerRoute(t *testing.T) {
	t.Parallel()

	const subjectIssuer = "test"
	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: subjectIssuer, ID: "alice"}, nil
	}))

	tests := []struct {
		name       string
		method     string
		path       string
		body       func() (io.Reader, string) // returns body + content-type
		wantAct    auth.Action
		wantStatus int // when the authorizer denies
	}{
		{
			name:       "simple index list",
			method:     http.MethodGet,
			path:       "/simple/",
			wantAct:    auth.Action{Repo: "index", Format: RepoType, Op: auth.OpRead},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "package index read",
			method:     http.MethodGet,
			path:       "/simple/foo/",
			wantAct:    auth.Action{Repo: "packages/foo", Format: RepoType, Op: auth.OpRead},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "file get",
			method:     http.MethodGet,
			path:       "/packages/foo/1.0/foo-1.0.tar.gz",
			wantAct:    auth.Action{Repo: "packages/foo", Format: RepoType, Op: auth.OpRead},
			wantStatus: http.StatusForbidden,
		},
		{
			name:   "upload write",
			method: http.MethodPost,
			path:   "/",
			body: func() (io.Reader, string) {
				var b bytes.Buffer
				mw := multipart.NewWriter(&b)
				_ = mw.WriteField("name", "Foo")
				_ = mw.WriteField("version", "1.0")
				fw, _ := mw.CreateFormFile("content", "foo-1.0.tar.gz")
				_, _ = fw.Write([]byte("payload"))
				_ = mw.Close()
				return &b, mw.FormDataContentType()
			},
			wantAct:    auth.Action{Repo: "packages/foo", Format: RepoType, Op: auth.OpWrite},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got auth.Action
			authz := auth.AuthorizerFunc(func(_ context.Context, ac *auth.AuthContext, act auth.Action) error {
				got = act
				if ac.Issuer != subjectIssuer {
					t.Errorf("subject issuer = %q, want %q", ac.Issuer, subjectIssuer)
				}
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

			var body io.Reader
			contentType := ""
			if tc.body != nil {
				body, contentType = tc.body()
			}
			r := httptest.NewRequest(tc.method, tc.path, body)
			if contentType != "" {
				r.Header.Set("Content-Type", contentType)
			}
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if got != tc.wantAct {
				t.Errorf("authorizer Action = %+v, want %+v", got, tc.wantAct)
			}
		})
	}
}

// TestAuthz_AllowsRequest confirms that returning nil from the
// Authorizer lets the request through to the route handler.
func TestAuthz_AllowsRequest(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	h, err := NewHandler(oci.NewFakeRegistry(),
		WithAuthMiddleware(installCtx),
		WithAuthorizer(auth.AllowAll),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Errorf("status = %d; AllowAll authorizer should let the request through (body: %s)", w.Code, w.Body.String())
	}
}

// TestAuthz_NoAuthorizerOpen confirms that omitting WithAuthorizer
// leaves routes open. The serve command always supplies one in
// production, so this case applies to tests and to operators
// running behind another authz layer.
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

	r := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code == http.StatusForbidden {
		t.Errorf("status = 403 with no authorizer configured; default should be open (body: %s)", w.Body.String())
	}
}

// TestAuthz_InternalError maps a non-ErrUnauthorized authorizer
// error to 500 rather than 403 so transient authorization-system
// failures don't look like a legitimate policy denial.
func TestAuthz_InternalError(t *testing.T) {
	t.Parallel()

	installCtx := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "test", ID: "alice"}, nil
	}))

	authz := auth.AuthorizerFunc(func(context.Context, *auth.AuthContext, auth.Action) error {
		return errInternal
	})
	h, err := NewHandler(oci.NewFakeRegistry(),
		WithAuthMiddleware(installCtx),
		WithAuthorizer(authz),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	r := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
}

// errInternal is a deliberate non-sentinel error returned by the
// internal-error authz fake — confirms Check passes through
// arbitrary errors as 500.
var errInternal = errors.New("authz backend down")
