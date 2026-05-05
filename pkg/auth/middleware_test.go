package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestMiddleware(t *testing.T) {
	t.Parallel()

	successSubject := &AuthContext{Issuer: "test", ID: "alice"}

	tests := []struct {
		name           string
		auth           Authenticator
		wantStatus     int
		wantWWWHeaders []string
		wantSubject    *AuthContext
		wantBodyPart   string
	}{
		{
			name:        "success",
			auth:        AuthenticatorFunc(func(*http.Request) (*AuthContext, error) { return successSubject, nil }),
			wantStatus:  http.StatusOK,
			wantSubject: successSubject,
		},
		{
			name:           "no credential",
			auth:           AuthenticatorFunc(func(*http.Request) (*AuthContext, error) { return nil, ErrNoCredential }),
			wantStatus:     http.StatusUnauthorized,
			wantWWWHeaders: []string{`Bearer realm="ocifactory"`, `Basic realm="ocifactory"`},
		},
		{
			name:           "invalid token",
			auth:           AuthenticatorFunc(func(*http.Request) (*AuthContext, error) { return nil, ErrInvalidToken }),
			wantStatus:     http.StatusUnauthorized,
			wantWWWHeaders: []string{`Bearer realm="ocifactory"`, `Basic realm="ocifactory"`},
		},
		{
			name:       "issuer unavailable",
			auth:       AuthenticatorFunc(func(*http.Request) (*AuthContext, error) { return nil, ErrIssuerUnavailable }),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "wrapped issuer unavailable",
			auth: AuthenticatorFunc(func(*http.Request) (*AuthContext, error) {
				return nil, errors.Join(ErrIssuerUnavailable, errors.New("DNS"))
			}),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "unexpected error",
			auth:       AuthenticatorFunc(func(*http.Request) (*AuthContext, error) { return nil, errors.New("boom") }),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:           "nil subject and nil error treated as no credential",
			auth:           AuthenticatorFunc(func(*http.Request) (*AuthContext, error) { return nil, nil }),
			wantStatus:     http.StatusUnauthorized,
			wantWWWHeaders: []string{`Bearer realm="ocifactory"`, `Basic realm="ocifactory"`},
		},
		{
			name:           "nil authenticator denies",
			auth:           nil,
			wantStatus:     http.StatusUnauthorized,
			wantWWWHeaders: []string{`Bearer realm="ocifactory"`, `Basic realm="ocifactory"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seenSubject *AuthContext
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if s, ok := FromContext(r.Context()); ok {
					seenSubject = s
				}
				w.WriteHeader(http.StatusOK)
			})
			h := Middleware(tc.auth)(next)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(w, r)

			if got := w.Code; got != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", got, tc.wantStatus, strings.TrimSpace(w.Body.String()))
			}
			if tc.wantWWWHeaders != nil {
				got := w.Header().Values("WWW-Authenticate")
				if diff := cmp.Diff(tc.wantWWWHeaders, got); diff != "" {
					t.Errorf("WWW-Authenticate mismatch (-want +got):\n%s", diff)
				}
			}
			if diff := cmp.Diff(tc.wantSubject, seenSubject); diff != "" {
				t.Errorf("subject seen by next handler mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
