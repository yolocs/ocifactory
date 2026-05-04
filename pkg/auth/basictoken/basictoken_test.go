package basictoken

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/auth"
	"golang.org/x/crypto/bcrypt"
)

// hashFor produces a low-cost bcrypt hash for tests. We use MinCost
// across the test suite to keep wall-clock low — basictoken's API is
// agnostic to cost.
func hashFor(t *testing.T, pwd string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hashFor: %v", err)
	}
	return string(h)
}

func basicHeader(user, pwd string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pwd))
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		users       map[string]string
		wantErrPart string
	}{
		{name: "empty allowed", users: nil},
		{name: "valid", users: map[string]string{"alice": hashFor(t, "p")}},
		{
			name:        "empty user",
			users:       map[string]string{"": hashFor(t, "p")},
			wantErrPart: "user name is empty",
		},
		{
			name:        "sentinel user rejected",
			users:       map[string]string{"_oidc": hashFor(t, "p")},
			wantErrPart: "reserved",
		},
		{
			name:        "empty hash",
			users:       map[string]string{"alice": ""},
			wantErrPart: "empty hash",
		},
		{
			name:        "plaintext password",
			users:       map[string]string{"alice": "plaintext"},
			wantErrPart: "not a bcrypt hash",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := New(tc.users)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("New: error = nil, want %q", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("New: error = %q, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: unexpected error: %v", err)
			}
			if a == nil {
				t.Fatalf("New: got nil authenticator")
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()

	a, err := New(map[string]string{
		"alice": hashFor(t, "alice-pwd"),
		"bob":   hashFor(t, "bob-pwd"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name        string
		header      string
		wantSubject *auth.Subject
		wantErrIs   error
	}{
		{
			name:        "alice ok",
			header:      basicHeader("alice", "alice-pwd"),
			wantSubject: &auth.Subject{Issuer: "basictoken", ID: "alice"},
		},
		{
			name:      "alice wrong password",
			header:    basicHeader("alice", "wrong"),
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			name:      "unknown user",
			header:    basicHeader("eve", "anything"),
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			name:      "no header",
			header:    "",
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name:      "bearer header (not our concern)",
			header:    "Bearer xyz",
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name:      "sentinel _oidc falls through",
			header:    basicHeader("_oidc", "tok"),
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name:      "sentinel oauth2accesstoken falls through",
			header:    basicHeader("oauth2accesstoken", "tok"),
			wantErrIs: auth.ErrNoCredential,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			subj, err := a.Authenticate(r)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Errorf("error = %v, want errors.Is(%v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantSubject, subj); diff != "" {
				t.Errorf("subject mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	t.Parallel()

	hash := hashFor(t, "secret")

	tests := []struct {
		name        string
		yaml        string
		wantErrPart string
	}{
		{
			name: "valid",
			yaml: "users:\n  alice: " + hash + "\n",
		},
		{
			name:        "malformed yaml",
			yaml:        ":::not yaml",
			wantErrPart: "parse",
		},
		{
			name:        "plaintext rejected at load",
			yaml:        "users:\n  alice: plaintext\n",
			wantErrPart: "not a bcrypt hash",
		},
		{
			name: "empty users ok",
			yaml: "users: {}\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "tokens.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := LoadFile(path)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("error = nil, want %q", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("error = %q, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := LoadFile(filepath.Join(t.TempDir(), "nope"))
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}
