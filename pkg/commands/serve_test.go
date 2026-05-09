package commands

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler/python"
	"github.com/yolocs/ocifactory/pkg/testutil"
)

// minimalServeConfig returns a serveConfig pre-populated with the
// minimum fields needed to pass every Validate guard except the one
// each case is exercising. Tests override specific fields.
func minimalServeConfig() serveConfig {
	return serveConfig{
		Port:            "8080",
		RepoType:        "maven",
		BackendRegistry: "http://example.com",
		DisableAuthn:    true,
	}
}

func TestServeConfig_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mut     func(c *serveConfig)
		wantErr string
		wantURL *url.URL
	}{
		{
			name:    "all fields set",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name:    "missing port",
			mut:     func(c *serveConfig) { c.Port = "" },
			wantErr: "port is required",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name:    "missing repo type",
			mut:     func(c *serveConfig) { c.RepoType = "" },
			wantErr: `repo-type "" is not supported`,
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name:    "invalid repo type",
			mut:     func(c *serveConfig) { c.RepoType = "invalid" },
			wantErr: `repo-type "invalid" is not supported`,
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name:    "missing registry URL",
			mut:     func(c *serveConfig) { c.BackendRegistry = "" },
			wantErr: "backend-registry is required",
			// Validate short-circuits before parsing the URL.
		},
		{
			name:    "registry URL without protocol prefix",
			mut:     func(c *serveConfig) { c.BackendRegistry = "example.com" },
			wantURL: &url.URL{Scheme: "https", Host: "example.com"},
		},
		{
			name:    "https registry URL passed directly",
			mut:     func(c *serveConfig) { c.BackendRegistry = "https://gar.example.com/project" },
			wantURL: &url.URL{Scheme: "https", Host: "gar.example.com", Path: "/project"},
		},
		{
			name: "missing authn config rejects",
			mut: func(c *serveConfig) {
				c.DisableAuthn = false
				c.AuthnKind = ""
			},
			wantErr: "either --authn-kind",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "unsupported authn kind",
			mut: func(c *serveConfig) {
				c.DisableAuthn = false
				c.AuthnKind = "saml"
			},
			wantErr: `authn-kind "saml" is not supported`,
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "oidc kind missing issuers",
			mut: func(c *serveConfig) {
				c.DisableAuthn = false
				c.AuthnKind = "oidc"
				c.AuthnOIDCAudience = "https://ocifactory.example"
			},
			wantErr: "authn-oidc-issuers",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "oidc kind missing audience",
			mut: func(c *serveConfig) {
				c.DisableAuthn = false
				c.AuthnKind = "oidc"
				c.AuthnOIDCIssuers = []string{"https://accounts.google.com"}
			},
			wantErr: "authn-oidc-audience",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "oidc kind fully configured",
			mut: func(c *serveConfig) {
				c.DisableAuthn = false
				c.AuthnKind = "oidc"
				c.AuthnOIDCIssuers = []string{"https://accounts.google.com"}
				c.AuthnOIDCAudience = "https://ocifactory.example"
			},
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			// echo never talks to an OCI backend, so --backend-registry
			// is optional.
			name: "echo without backend-registry is allowed",
			mut: func(c *serveConfig) {
				c.RepoType = "echo"
				c.BackendRegistry = ""
			},
		},
		{
			// echo accepts --backend-registry too — make sure URL parsing
			// still happens so a malformed value would be caught.
			name: "echo with backend-registry parses URL",
			mut: func(c *serveConfig) {
				c.RepoType = "echo"
				c.BackendRegistry = "https://example.com"
			},
			wantURL: &url.URL{Scheme: "https", Host: "example.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := minimalServeConfig()
			if tc.mut != nil {
				tc.mut(&c)
			}
			err := c.Validate()
			if diff := testutil.DiffErrString(err, tc.wantErr); diff != "" {
				t.Errorf("Validate() error: %s", diff)
			}
			if diff := cmp.Diff(tc.wantURL, c.RegistryURL, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Validate() RegistryURL mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestServeCmd_Flags(t *testing.T) {
	t.Parallel()

	cmd := newServeCmd()

	tests := []struct {
		name      string
		flagName  string
		shorthand string
	}{
		{name: flagPort, flagName: flagPort},
		{name: flagRepoType, flagName: flagRepoType, shorthand: "t"},
		{name: flagBackendRegistry, flagName: flagBackendRegistry},
		{name: flagDisableStreamingPush, flagName: flagDisableStreamingPush},
		{name: flagAllowOverwrite, flagName: flagAllowOverwrite},
		{name: flagDisableAuthn, flagName: flagDisableAuthn},
		{name: flagAuthnKind, flagName: flagAuthnKind},
		{name: flagAuthnOIDCIssuers, flagName: flagAuthnOIDCIssuers},
		{name: flagAuthnOIDCAudience, flagName: flagAuthnOIDCAudience},
		{name: flagBackendAuthKind, flagName: flagBackendAuthKind},
		{name: flagBackendAuthGCPADCScopes, flagName: flagBackendAuthGCPADCScopes},
		{name: flagBackendAuthStaticEnvUserEnv, flagName: flagBackendAuthStaticEnvUserEnv},
		{name: flagBackendAuthStaticEnvPasswordEnv, flagName: flagBackendAuthStaticEnvPasswordEnv},
		{name: flagBackendAuthDockerConfigPath, flagName: flagBackendAuthDockerConfigPath},
		{name: flagAuthzConfig, flagName: flagAuthzConfig},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := cmd.Flags().Lookup(tc.flagName)
			if f == nil {
				t.Fatalf("flag %q not registered", tc.flagName)
			}
			if got, want := f.Shorthand, tc.shorthand; got != want {
				t.Errorf("flag %q shorthand: got %q, want %q", tc.flagName, got, want)
			}
		})
	}
}

func TestBuildAuthz(t *testing.T) {
	t.Parallel()

	t.Run("empty config returns nil authorizer", func(t *testing.T) {
		t.Parallel()
		got, err := buildAuthz(t.Context(), &serveConfig{AuthzConfig: ""})
		if err != nil {
			t.Fatalf("buildAuthz: %v", err)
		}
		if got != nil {
			t.Errorf("buildAuthz returned %T, want nil", got)
		}
	})

	t.Run("loads config file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "authz.yaml")
		body := `default: deny
rules:
  - subject:
      issuer: https://example.com
    allow:
      - { repo: "*", format: "*", op: read }
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := buildAuthz(t.Context(), &serveConfig{AuthzConfig: path})
		if err != nil {
			t.Fatalf("buildAuthz: %v", err)
		}
		if got == nil {
			t.Fatal("buildAuthz returned nil; want a real Authorizer")
		}
		// Sanity-check the rule actually loaded.
		ac := &auth.AuthContext{Issuer: "https://example.com", ID: "u"}
		if err := got.Authorize(t.Context(), ac, auth.Action{Repo: "anything", Format: "python", Op: auth.OpRead}); err != nil {
			t.Errorf("Authorize: %v", err)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		t.Parallel()
		_, err := buildAuthz(t.Context(), &serveConfig{AuthzConfig: filepath.Join(t.TempDir(), "nope.yaml")})
		if err == nil {
			t.Errorf("buildAuthz with missing file succeeded; want error")
		}
	})
}

// TestServeCmd_EnvVarBindings exercises viper's resolution chain end
// to end: with no CLI args and only OCIFACTORY_* / PORT in env,
// every flag must surface the env value in the unmarshaled
// serveConfig — including comma-split slices and parsed durations,
// which viper.Unmarshal handles via its default DecodeHook.
//
// Mutates process-global env, so cannot t.Parallel.
func TestServeCmd_EnvVarBindings(t *testing.T) {
	t.Setenv("OCIFACTORY_REPO_TYPE", "python")
	t.Setenv("OCIFACTORY_BACKEND_REGISTRY", "zot.example.com:5000/ocifactory")
	t.Setenv("OCIFACTORY_AUTHN_KIND", "oidc")
	t.Setenv("OCIFACTORY_AUTHN_OIDC_ISSUERS",
		"https://accounts.google.com,https://token.actions.githubusercontent.com")
	t.Setenv("OCIFACTORY_AUTHN_OIDC_AUDIENCE", "https://ocifactory.example")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_KIND", "staticenv")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_STATICENV_USER_ENV", "REG_USER")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_STATICENV_PASSWORD_ENV", "REG_PASS")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_GCPADC_SCOPES",
		"https://www.googleapis.com/auth/cloud-platform.read-only,https://www.googleapis.com/auth/userinfo.email")
	t.Setenv("OCIFACTORY_DISABLE_AUTHN", "true")
	t.Setenv("OCIFACTORY_SIMPLE_INDEX_CACHE_TTL", "13s")
	t.Setenv("PORT", "9090")

	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv(flagPort, "PORT")

	cmd := buildServeCmd(v)
	// Stop short of starting the server: capture the unmarshaled
	// config and return.
	var got serveConfig
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return v.Unmarshal(&got)
	}

	cmd.SetArgs(nil) // no CLI args — env vars must be the only source
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute() error = %v", err)
	}

	want := serveConfig{
		Port:            "9090",
		RepoType:        "python",
		BackendRegistry: "zot.example.com:5000/ocifactory",
		// Defaults the production binary advertises in --help.
		EnableMetrics:        true,
		MetricsPath:          "/metrics",
		SimpleIndexCacheTTL:  13 * time.Second,
		PythonMaxUploadBytes: python.DefaultMaxUploadBytes,
		DisableAuthn:         true,
		AuthnKind:            "oidc",
		AuthnOIDCIssuers: []string{
			"https://accounts.google.com",
			"https://token.actions.githubusercontent.com",
		},
		AuthnOIDCAudience: "https://ocifactory.example",
		BackendAuthKind:   "staticenv",
		BackendAuthGCPADCScopes: []string{
			"https://www.googleapis.com/auth/cloud-platform.read-only",
			"https://www.googleapis.com/auth/userinfo.email",
		},
		BackendAuthStaticEnvUserEnv:     "REG_USER",
		BackendAuthStaticEnvPasswordEnv: "REG_PASS",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Unmarshal mismatch (-want +got):\n%s", diff)
	}
}
