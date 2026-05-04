package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestLoadConfigFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		yaml        string
		wantSchema  *FileSchema
		wantErrPart string
	}{
		{
			name: "valid",
			yaml: `authenticators:
  - kind: oidc
    issuer: https://accounts.google.com
    audience: https://ocifactory.example
  - kind: basictoken
    file: /etc/tokens.yaml
`,
			wantSchema: &FileSchema{
				Authenticators: []AuthenticatorSpec{
					{Kind: "oidc", Issuer: "https://accounts.google.com", Audience: "https://ocifactory.example"},
					{Kind: "basictoken", File: "/etc/tokens.yaml"},
				},
			},
		},
		{
			name:        "empty list",
			yaml:        `authenticators: []`,
			wantErrPart: "no authenticators configured",
		},
		{
			name: "missing kind",
			yaml: `authenticators:
  - issuer: https://x
`,
			wantErrPart: "kind is required",
		},
		{
			name: "unknown kind",
			yaml: `authenticators:
  - kind: magic
`,
			wantErrPart: `unknown kind "magic"`,
		},
		{
			name: "oidc missing issuer",
			yaml: `authenticators:
  - kind: oidc
    audience: x
`,
			wantErrPart: "oidc requires issuer",
		},
		{
			name: "oidc missing audience",
			yaml: `authenticators:
  - kind: oidc
    issuer: https://x
`,
			wantErrPart: "oidc requires audience",
		},
		{
			name: "basictoken missing file",
			yaml: `authenticators:
  - kind: basictoken
`,
			wantErrPart: "basictoken requires file",
		},
		{
			name:        "malformed yaml",
			yaml:        "authenticators:\n  - kind: oidc\n    issuer: [unclosed",
			wantErrPart: "parse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "auth.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got, err := LoadConfigFile(path)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("LoadConfigFile() error = nil, want %q", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("LoadConfigFile() error = %q, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfigFile() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantSchema, got); diff != "" {
				t.Errorf("schema mismatch (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := LoadConfigFile(filepath.Join(t.TempDir(), "does-not-exist"))
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}
