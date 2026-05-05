package auth

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// stubFactory builds an Authenticator that always returns
// (sentinelSubject, nil). The factory captures the spec for
// inspection so the test can assert decoding worked.
type stubFactory struct {
	called   int
	lastSpec map[string]any
}

func (s *stubFactory) build(spec AuthenticatorSpec) (Authenticator, error) {
	s.called++
	var raw map[string]any
	_ = spec.Decode(&raw)
	s.lastSpec = raw
	return AuthenticatorFunc(func(*http.Request) (*AuthContext, error) {
		return &AuthContext{Issuer: "stub", ID: "stub"}, nil
	}), nil
}

// withTestKind installs a kind for the duration of the test and
// restores the prior state on cleanup. Tests that mutate the
// package-level registry mark themselves not parallel against
// each other via a shared mutex.
//
// The registry is process-global; we serialise mutations through
// registryMu so concurrent tests don't see each other's
// registrations.
func withTestKind(t *testing.T, name string, f Factory) {
	t.Helper()
	registryMu.Lock()
	if _, exists := registry[name]; exists {
		registryMu.Unlock()
		t.Fatalf("withTestKind: %q already registered", name)
	}
	registry[name] = f
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	})
}

func TestLoadConfigFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		yaml        string
		wantKinds   []string
		wantErrPart string
	}{
		{
			name: "valid single kind",
			yaml: `authenticators:
  - kind: oidc
    issuer: https://accounts.google.com
    audience: https://ocifactory.example
`,
			wantKinds: []string{"oidc"},
		},
		{
			name: "valid multi kind",
			yaml: `authenticators:
  - kind: oidc
    issuer: https://accounts.google.com
    audience: https://ocifactory.example
  - kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: https://ocifactory.example
`,
			wantKinds: []string{"oidc", "oidc"},
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
			gotKinds := make([]string, 0, len(got.Authenticators))
			for i := range got.Authenticators {
				gotKinds = append(gotKinds, got.Authenticators[i].Kind)
			}
			if diff := cmp.Diff(tc.wantKinds, gotKinds); diff != "" {
				t.Errorf("kinds mismatch (-want +got):\n%s", diff)
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

func TestRegisterKind_DuplicatePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate registration")
		}
	}()
	stub := &stubFactory{}
	withTestKind(t, "dup-kind-test", stub.build)
	// Second registration with the same name must panic.
	RegisterKind("dup-kind-test", stub.build)
}

func TestRegisterKind_EmptyNamePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty name")
		}
	}()
	RegisterKind("", func(AuthenticatorSpec) (Authenticator, error) { return nil, nil })
}

func TestRegisterKind_NilFactoryPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil factory")
		}
	}()
	RegisterKind("nil-factory-test", nil)
}

func TestBuild(t *testing.T) {
	t.Parallel()

	stub := &stubFactory{}
	withTestKind(t, "test-stub", stub.build)

	cfg, err := LoadConfigFile(writeTemp(t, `authenticators:
  - kind: test-stub
    foo: bar
    nested:
      a: 1
`))
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}

	auths, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := len(auths), 1; got != want {
		t.Errorf("len(auths) = %d, want %d", got, want)
	}
	if stub.called != 1 {
		t.Errorf("stub.called = %d, want 1", stub.called)
	}
	if got, want := stub.lastSpec["foo"], "bar"; got != want {
		t.Errorf("spec.foo = %v, want %v", got, want)
	}
}

func TestBuild_UnknownKind(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfigFile(writeTemp(t, `authenticators:
  - kind: definitely-not-registered
`))
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	_, err = Build(cfg)
	if err == nil {
		t.Fatal("Build: error = nil, want unknown-kind error")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error = %q, want substring 'unknown kind'", err)
	}
}

func TestBuild_FactoryError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("nope")
	withTestKind(t, "errors-on-build", func(AuthenticatorSpec) (Authenticator, error) {
		return nil, wantErr
	})
	cfg, err := LoadConfigFile(writeTemp(t, `authenticators:
  - kind: errors-on-build
`))
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	_, err = Build(cfg)
	if !errors.Is(err, wantErr) {
		t.Errorf("Build error = %v, want errors.Is(%v)", err, wantErr)
	}
}

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
