package echo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/auth"
)

// installContext is a test middleware that puts ac on the request
// context, mimicking what auth.Middleware does after a successful
// authentication.
func installContext(ac *auth.AuthContext) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(auth.WithAuthContext(r.Context(), ac))
			next.ServeHTTP(w, r)
		})
	}
}

func TestEcho(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		ac         *auth.AuthContext
		wantMsg    string
		wantCaller *callerSummary
	}{
		{
			name:       "echo with caller",
			path:       "/echo/hello",
			ac:         &auth.AuthContext{Issuer: "https://example.test", ID: "user-1"},
			wantMsg:    "hello",
			wantCaller: &callerSummary{Issuer: "https://example.test", ID: "user-1"},
		},
		{
			name:       "echo with anonymous",
			path:       "/echo/world",
			ac:         &auth.AuthContext{Issuer: "anonymous", ID: "anonymous"},
			wantMsg:    "world",
			wantCaller: &callerSummary{Issuer: "anonymous", ID: "anonymous"},
		},
		{
			name:       "echo with no auth context",
			path:       "/echo/silent",
			ac:         nil,
			wantMsg:    "silent",
			wantCaller: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var opts []Option
			if tc.ac != nil {
				opts = append(opts, WithAuthMiddleware(installContext(tc.ac)))
			}
			h := NewHandler(opts...)

			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)

			if got, want := w.Code, http.StatusOK; got != want {
				t.Fatalf("status = %d, want %d (body=%q)", got, want, w.Body.String())
			}
			var got echoResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body: %v (body=%q)", err, w.Body.String())
			}
			want := echoResponse{Message: tc.wantMsg, Caller: tc.wantCaller}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("response mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWhoami(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ac   *auth.AuthContext
		want whoamiResponse
	}{
		{
			name: "with caller",
			ac:   &auth.AuthContext{Issuer: "https://token.actions.githubusercontent.com", ID: "repo:foo/bar:ref:refs/heads/main"},
			want: whoamiResponse{Caller: &callerSummary{
				Issuer: "https://token.actions.githubusercontent.com",
				ID:     "repo:foo/bar:ref:refs/heads/main",
			}},
		},
		{
			name: "no auth context",
			ac:   nil,
			want: whoamiResponse{Caller: nil},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var opts []Option
			if tc.ac != nil {
				opts = append(opts, WithAuthMiddleware(installContext(tc.ac)))
			}
			h := NewHandler(opts...)

			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/whoami", nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)

			if got, want := w.Code, http.StatusOK; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
			var got whoamiResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("response mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := NewHandler()
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/echo/x"},
		{http.MethodPut, "/echo/x"},
		{http.MethodDelete, "/whoami"},
	}
	for _, tc := range tests {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)
			if w.Code == http.StatusOK {
				t.Errorf("status = 200 for %s %s; want non-200", tc.method, tc.path)
			}
		})
	}
}
