package backend

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/oauth2"
)

func TestAnonymous(t *testing.T) {
	t.Parallel()

	got, err := Anonymous().Credential(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("Anonymous() error = %v", err)
	}
	if diff := cmp.Diff(Credential{}, got); diff != "" {
		t.Errorf("Anonymous() mismatch (-want +got):\n%s", diff)
	}
}

func TestNew_DefaultIsAnonymous(t *testing.T) {
	t.Parallel()

	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New(zero) error = %v", err)
	}
	got, err := p.Credential(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("Credential() error = %v", err)
	}
	if diff := cmp.Diff(Credential{}, got); diff != "" {
		t.Errorf("zero-config Credential mismatch (-want +got):\n%s", diff)
	}
}

func TestNew_UnknownKindReturnsError(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Kind: "definitely-not-a-kind"})
	if err == nil {
		t.Fatal("New() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error = %q, want substring 'unknown kind'", err)
	}
}

func TestNew_StaticEnvRequiresBothEnvVars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, userEnv, passEnv, wantSubstr string
	}{
		{"missing user", "", "PASS", "user_env is required"},
		{"missing password", "USER", "", "password_env is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Config{
				Kind:                 KindStaticEnv,
				StaticEnvUserEnv:     tc.userEnv,
				StaticEnvPasswordEnv: tc.passEnv,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("New() error = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestStaticEnvReadsOnEveryCall verifies the rotate-without-restart
// contract: the provider re-reads the env on every call, so an
// operator updating Vault / SOPS / k8s secretKeyRef sees the new
// password on the next backend hit.
//
// Mutates process-global env, so cannot t.Parallel.
func TestStaticEnvReadsOnEveryCall(t *testing.T) {
	const userVar = "OCIFACTORY_TEST_STATICENV_USER"
	const passVar = "OCIFACTORY_TEST_STATICENV_PASS"

	t.Setenv(userVar, "alice")
	t.Setenv(passVar, "p1")

	p, err := New(Config{
		Kind:                 KindStaticEnv,
		StaticEnvUserEnv:     userVar,
		StaticEnvPasswordEnv: passVar,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := p.Credential(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("Credential() error = %v", err)
	}
	want := Credential{Username: "alice", Password: "p1"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Credential() mismatch (-want +got):\n%s", diff)
	}

	t.Setenv(passVar, "p2")
	got, err = p.Credential(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("Credential() after rotate = %v", err)
	}
	want = Credential{Username: "alice", Password: "p2"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rotated Credential mismatch (-want +got):\n%s", diff)
	}
}

// TestStaticEnvEmptyVarsYieldEmpty confirms a missing env var
// surfaces as the empty Credential rather than an error — the
// backend's WWW-Authenticate response is what tells the operator
// the credential was missing, and we want a uniform code path for
// every "no creds available" case.
func TestStaticEnvEmptyVarsYieldEmpty(t *testing.T) {
	const userVar = "OCIFACTORY_TEST_STATICENV_USER_EMPTY"
	const passVar = "OCIFACTORY_TEST_STATICENV_PASS_EMPTY"

	tests := []struct {
		name, user, pass string
	}{
		{"both empty", "", ""},
		{"user empty", "", "p"},
		{"password empty", "u", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(userVar, tc.user)
			t.Setenv(passVar, tc.pass)
			p, err := New(Config{
				Kind:                 KindStaticEnv,
				StaticEnvUserEnv:     userVar,
				StaticEnvPasswordEnv: passVar,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			got, err := p.Credential(t.Context(), "example.com")
			if err != nil {
				t.Fatalf("Credential() error = %v", err)
			}
			if diff := cmp.Diff(Credential{}, got); diff != "" {
				t.Errorf("Credential() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDockerConfig_RoundTrip verifies the dockerconfig provider
// reads a plaintext entry. The wrapper around oras-go's credential
// store is intentionally thin — upstream covers helper-process and
// auths-block edge cases.
func TestDockerConfig_RoundTrip(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	const cfg = `{
  "auths": {
    "us-docker.pkg.dev": {
      "username": "_json_key",
      "password": "secret"
    }
  }
}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p, err := New(Config{Kind: KindDockerConfig, DockerConfigPath: path})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name string
		host string
		want Credential
	}{
		{
			name: "configured host",
			host: "us-docker.pkg.dev",
			want: Credential{Username: "_json_key", Password: "secret"},
		},
		{
			name: "unknown host yields empty",
			host: "no-creds.example.com",
			want: Credential{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := p.Credential(t.Context(), tc.host)
			if err != nil {
				t.Fatalf("Credential(%q) error = %v", tc.host, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Credential(%q) mismatch (-want +got):\n%s", tc.host, diff)
			}
		})
	}
}

// fakeTokenSource is a stub oauth2.TokenSource so the gcpadc tests
// don't reach for real Google metadata.
type fakeTokenSource struct {
	token *oauth2.Token
	err   error
}

func (f *fakeTokenSource) Token() (*oauth2.Token, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.token, nil
}

func TestGCPADC_TokenReturned(t *testing.T) {
	t.Parallel()

	ts := &fakeTokenSource{token: &oauth2.Token{
		AccessToken: "ya29.test-token",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(1 * time.Hour),
	}}
	p, err := newGCPADC(gcpadcOptions{tokenSource: ts})
	if err != nil {
		t.Fatalf("newGCPADC() error = %v", err)
	}

	got, err := p.Credential(t.Context(), "us-docker.pkg.dev")
	if err != nil {
		t.Fatalf("Credential() error = %v", err)
	}
	want := Credential{AccessToken: "ya29.test-token"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Credential() mismatch (-want +got):\n%s", diff)
	}
}

func TestGCPADC_TokenSourceError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("token unavailable")
	p, err := newGCPADC(gcpadcOptions{tokenSource: &fakeTokenSource{err: wantErr}})
	if err != nil {
		t.Fatalf("newGCPADC() error = %v", err)
	}

	_, err = p.Credential(t.Context(), "us-docker.pkg.dev")
	if err == nil {
		t.Fatal("Credential() error = nil, want error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Credential() error = %v, want errors.Is(%v)", err, wantErr)
	}
}

// TestGCPADC_HostIgnored locks in the contract that the same token
// is issued regardless of host — ADC is a single-identity model
// and operators picking gcpadc accept that.
func TestGCPADC_HostIgnored(t *testing.T) {
	t.Parallel()

	ts := &fakeTokenSource{token: &oauth2.Token{AccessToken: "tok-1"}}
	p, err := newGCPADC(gcpadcOptions{tokenSource: ts})
	if err != nil {
		t.Fatalf("newGCPADC() error = %v", err)
	}

	c1, err := p.Credential(t.Context(), "us-docker.pkg.dev")
	if err != nil {
		t.Fatalf("Credential(host1) error = %v", err)
	}
	c2, err := p.Credential(t.Context(), "europe-docker.pkg.dev")
	if err != nil {
		t.Fatalf("Credential(host2) error = %v", err)
	}
	if diff := cmp.Diff(c1, c2); diff != "" {
		t.Errorf("per-host creds differ (-host1 +host2):\n%s", diff)
	}
}
