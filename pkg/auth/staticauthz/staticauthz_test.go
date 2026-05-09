package staticauthz

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
)

func TestGlobMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		input   string
		want    bool
	}{
		{name: "empty matches anything", pattern: "", input: "anything/here", want: true},
		{name: "exact match", pattern: "packages/foo", input: "packages/foo", want: true},
		{name: "exact mismatch", pattern: "packages/foo", input: "packages/bar", want: false},

		{name: "* matches segment", pattern: "packages/*", input: "packages/foo", want: true},
		{name: "* does not cross slash", pattern: "packages/*", input: "packages/foo/bar", want: false},
		{name: "* empty segment", pattern: "packages/*", input: "packages/", want: true},
		{name: "* mid-pattern", pattern: "*/foo", input: "bar/foo", want: true},

		{name: "** matches one segment", pattern: "**", input: "foo", want: true},
		{name: "** matches across slashes", pattern: "**", input: "a/b/c", want: true},
		{name: "** prefix", pattern: "packages/**", input: "packages/foo/bar", want: true},
		{name: "** prefix mismatch", pattern: "packages/**", input: "other/foo", want: false},

		{name: "regex special characters are literal (dot)", pattern: "foo.bar", input: "fooXbar", want: false},
		{name: "regex special characters are literal (parens)", pattern: "(foo)", input: "(foo)", want: true},
		{name: "single star vs literal star is glob", pattern: "*", input: "anything", want: true},
		{name: "single star does not match slash", pattern: "*", input: "a/b", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, err := compileGlob(tc.pattern)
			if err != nil {
				t.Fatalf("compileGlob(%q) error = %v", tc.pattern, err)
			}
			if got := g.match(tc.input); got != tc.want {
				t.Errorf("compileGlob(%q).match(%q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	t.Parallel()

	const ghaIssuer = "https://token.actions.githubusercontent.com"
	const googleIssuer = "https://accounts.google.com"

	cfg := Config{
		Default: DefaultDeny,
		Rules: []Rule{
			{
				Subject: SubjectMatcher{
					Issuer:   ghaIssuer,
					SubMatch: `^repo:my-org/build-bot:.*`,
				},
				Allow: []ActionMatcher{
					{Repo: "packages/*", Format: "python", Op: "write"},
					{Repo: "packages/*", Format: "python", Op: "read"},
				},
			},
			{
				Subject: SubjectMatcher{
					Issuer:   ghaIssuer,
					SubMatch: `^repo:my-org/.*`,
				},
				Allow: []ActionMatcher{
					{Repo: "**", Format: "*", Op: "read"},
				},
			},
			{
				Subject: SubjectMatcher{
					Issuer: googleIssuer,
					Email:  "build-sa@project.iam.gserviceaccount.com",
				},
				Allow: []ActionMatcher{
					{Repo: "**", Format: "*", Op: "*"},
				},
			},
			{
				Subject: SubjectMatcher{
					Issuer: googleIssuer,
					ClaimsMatch: map[string]string{
						"hd": `^example\.com$`,
					},
				},
				Allow: []ActionMatcher{
					{Repo: "**", Op: "read"},
				},
			},
		},
	}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name    string
		ac      *auth.AuthContext
		act     auth.Action
		wantErr error
	}{
		{
			name: "build-bot writes its own packages",
			ac:   &auth.AuthContext{Issuer: ghaIssuer, ID: "repo:my-org/build-bot:ref:refs/heads/main"},
			act:  auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpWrite},
		},
		{
			name:    "build-bot cannot write maven",
			ac:      &auth.AuthContext{Issuer: ghaIssuer, ID: "repo:my-org/build-bot:ref:refs/heads/main"},
			act:     auth.Action{Repo: "com/example/foo", Format: "maven", Op: auth.OpWrite},
			wantErr: auth.ErrUnauthorized,
		},
		{
			name: "any my-org repo can read across formats",
			ac:   &auth.AuthContext{Issuer: ghaIssuer, ID: "repo:my-org/some-other-repo:ref:refs/heads/main"},
			act:  auth.Action{Repo: "com/example/whatever", Format: "maven", Op: auth.OpRead},
		},
		{
			name:    "outside-org repo denied entirely",
			ac:      &auth.AuthContext{Issuer: ghaIssuer, ID: "repo:not-my-org/whatever:ref:refs/heads/main"},
			act:     auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpRead},
			wantErr: auth.ErrUnauthorized,
		},
		{
			name: "google service account allowed everywhere",
			ac:   &auth.AuthContext{Issuer: googleIssuer, ID: "111", Email: "build-sa@project.iam.gserviceaccount.com"},
			act:  auth.Action{Repo: "anything/here/at/all", Format: "maven", Op: auth.OpWrite},
		},
		{
			name:    "google account without email denied write",
			ac:      &auth.AuthContext{Issuer: googleIssuer, ID: "222", Email: "alice@example.com", Claims: map[string]any{"hd": "example.com"}},
			act:     auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpWrite},
			wantErr: auth.ErrUnauthorized,
		},
		{
			name: "claims match works",
			ac:   &auth.AuthContext{Issuer: googleIssuer, ID: "222", Email: "alice@example.com", Claims: map[string]any{"hd": "example.com"}},
			act:  auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpRead},
		},
		{
			name:    "missing claim denies",
			ac:      &auth.AuthContext{Issuer: googleIssuer, ID: "222", Email: "bob@elsewhere.com"},
			act:     auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpRead},
			wantErr: auth.ErrUnauthorized,
		},
		{
			name:    "wrong issuer denies even with right email",
			ac:      &auth.AuthContext{Issuer: "https://other.example.com", Email: "build-sa@project.iam.gserviceaccount.com"},
			act:     auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpRead},
			wantErr: auth.ErrUnauthorized,
		},
		{
			name:    "nil ac denies",
			ac:      nil,
			act:     auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpRead},
			wantErr: auth.ErrUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := a.Authorize(t.Context(), tc.ac, tc.act)
			if tc.wantErr == nil && err != nil {
				t.Errorf("Authorize() = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Authorize() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultAllow(t *testing.T) {
	t.Parallel()

	a, err := New(Config{Default: DefaultAllow})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Authorize(t.Context(), &auth.AuthContext{Issuer: "i", ID: "u"}, auth.Action{Repo: "x", Format: "y", Op: auth.OpWrite}); err != nil {
		t.Errorf("default allow denied: %v", err)
	}
}

func TestDefaultDenyEmptyRules(t *testing.T) {
	t.Parallel()

	a, err := New(Config{}) // default = deny, no rules
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = a.Authorize(t.Context(), &auth.AuthContext{Issuer: "i", ID: "u"}, auth.Action{Repo: "x", Format: "y", Op: auth.OpRead})
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Authorize() = %v, want ErrUnauthorized", err)
	}
}

func TestNewRejectsBadDefault(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{Default: Default("bogus")}); err == nil {
		t.Errorf("New() with bogus default succeeded; want error")
	}
}

func TestNewRejectsBadRegex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "bad sub_match",
			cfg: Config{Rules: []Rule{
				{Subject: SubjectMatcher{SubMatch: "[invalid"}},
			}},
		},
		{
			name: "bad claims_match",
			cfg: Config{Rules: []Rule{
				{Subject: SubjectMatcher{ClaimsMatch: map[string]string{"x": "[invalid"}}},
			}},
		},
		{
			name: "empty claim name",
			cfg: Config{Rules: []Rule{
				{Subject: SubjectMatcher{ClaimsMatch: map[string]string{"": "x"}}},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("New() succeeded; want error")
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		wantErr bool
		assert  func(t *testing.T, a *Authorizer)
	}{
		{
			name: "valid config",
			content: `
default: allow
rules:
  - subject:
      issuer: https://example.com
    allow:
      - { repo: "*", format: "*", op: "*" }
`,
			assert: func(t *testing.T, a *Authorizer) {
				err := a.Authorize(t.Context(), &auth.AuthContext{Issuer: "https://example.com", ID: "u"}, auth.Action{Repo: "x", Format: "python", Op: auth.OpRead})
				if err != nil {
					t.Errorf("Authorize: %v", err)
				}
			},
		},
		{
			name:    "invalid yaml",
			content: "default: [oops",
			wantErr: true,
		},
		{
			name:    "bad default",
			content: "default: maybe\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, tc.name+".yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			a, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Load() succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.assert != nil {
				tc.assert(t, a)
			}
		})
	}

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		if _, err := Load(""); err == nil {
			t.Errorf("Load(\"\") succeeded, want error")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		if _, err := Load(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
			t.Errorf("Load(missing) succeeded, want error")
		}
	})
}
