package commands

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/yolocs/ocifactory/pkg/testutil"
)

// minimalServeViper builds a viper.Viper preloaded with the minimum
// to pass every Validate guard except the one each case is
// exercising. Tests override specific keys with v.Set; everything
// else stays at the safe default.
func minimalServeViper(t *testing.T) *viper.Viper {
	t.Helper()
	v := viper.New()
	v.Set(flagPort, "8080")
	v.Set(flagRepoType, "maven")
	v.Set(flagBackendRegistry, "http://example.com")
	v.Set(flagDisableAuthn, true)
	return v
}

func TestValidateServeConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		setup   func(v *viper.Viper)
		wantErr string
		wantURL *url.URL
	}{
		{
			name:  "all fields set",
			setup: func(v *viper.Viper) {},
			wantURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name:    "missing port",
			setup:   func(v *viper.Viper) { v.Set(flagPort, "") },
			wantErr: "port is required",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name:    "missing repo type",
			setup:   func(v *viper.Viper) { v.Set(flagRepoType, "") },
			wantErr: `repo-type "" is not supported`,
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name:    "invalid repo type",
			setup:   func(v *viper.Viper) { v.Set(flagRepoType, "invalid") },
			wantErr: `repo-type "invalid" is not supported`,
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "missing registry URL",
			setup: func(v *viper.Viper) {
				v.Set(flagBackendRegistry, "")
			},
			wantErr: "backend-registry is required",
			// validateServeConfig short-circuits before parsing the URL.
		},
		{
			name: "registry URL without protocol prefix",
			setup: func(v *viper.Viper) {
				v.Set(flagBackendRegistry, "example.com")
			},
			wantURL: &url.URL{Scheme: "https", Host: "example.com"},
		},
		{
			name: "https registry URL passed directly",
			setup: func(v *viper.Viper) {
				v.Set(flagBackendRegistry, "https://gar.example.com/project")
			},
			wantURL: &url.URL{
				Scheme: "https",
				Host:   "gar.example.com",
				Path:   "/project",
			},
		},
		{
			name: "missing authn config rejects",
			setup: func(v *viper.Viper) {
				v.Set(flagDisableAuthn, false)
				v.Set(flagAuthnKind, "")
			},
			wantErr: "either --authn-kind",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "unsupported authn kind",
			setup: func(v *viper.Viper) {
				v.Set(flagDisableAuthn, false)
				v.Set(flagAuthnKind, "saml")
			},
			wantErr: `authn-kind "saml" is not supported`,
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "oidc kind missing issuers",
			setup: func(v *viper.Viper) {
				v.Set(flagDisableAuthn, false)
				v.Set(flagAuthnKind, "oidc")
				v.Set(flagAuthnOIDCAudience, "https://ocifactory.example")
			},
			wantErr: "authn-oidc-issuers",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "oidc kind missing audience",
			setup: func(v *viper.Viper) {
				v.Set(flagDisableAuthn, false)
				v.Set(flagAuthnKind, "oidc")
				v.Set(flagAuthnOIDCIssuers, []string{"https://accounts.google.com"})
			},
			wantErr: "authn-oidc-audience",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "oidc kind fully configured",
			setup: func(v *viper.Viper) {
				v.Set(flagDisableAuthn, false)
				v.Set(flagAuthnKind, "oidc")
				v.Set(flagAuthnOIDCIssuers, []string{"https://accounts.google.com"})
				v.Set(flagAuthnOIDCAudience, "https://ocifactory.example")
			},
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			// echo never talks to an OCI backend, so --backend-registry
			// is optional.
			name: "echo without backend-registry is allowed",
			setup: func(v *viper.Viper) {
				v.Set(flagRepoType, "echo")
				v.Set(flagBackendRegistry, "")
			},
		},
		{
			// echo accepts --backend-registry too — make sure URL parsing
			// still happens so a malformed value would be caught.
			name: "echo with backend-registry parses URL",
			setup: func(v *viper.Viper) {
				v.Set(flagRepoType, "echo")
				v.Set(flagBackendRegistry, "https://example.com")
			},
			wantURL: &url.URL{Scheme: "https", Host: "example.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := minimalServeViper(t)
			if tc.setup != nil {
				tc.setup(v)
			}
			gotURL, err := validateServeConfig(v)
			if diff := testutil.DiffErrString(err, tc.wantErr); diff != "" {
				t.Errorf("validateServeConfig() error: %s", diff)
			}
			if diff := cmp.Diff(tc.wantURL, gotURL, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("validateServeConfig() URL mismatch (-want +got):\n%s", diff)
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
		{name: flagDisableAuthn, flagName: flagDisableAuthn},
		{name: flagAuthnKind, flagName: flagAuthnKind},
		{name: flagAuthnOIDCIssuers, flagName: flagAuthnOIDCIssuers},
		{name: flagAuthnOIDCAudience, flagName: flagAuthnOIDCAudience},
		{name: flagBackendAuthKind, flagName: flagBackendAuthKind},
		{name: flagBackendAuthGCPADCScopes, flagName: flagBackendAuthGCPADCScopes},
		{name: flagBackendAuthStaticEnvUserEnv, flagName: flagBackendAuthStaticEnvUserEnv},
		{name: flagBackendAuthStaticEnvPasswordEnv, flagName: flagBackendAuthStaticEnvPasswordEnv},
		{name: flagBackendAuthDockerConfigPath, flagName: flagBackendAuthDockerConfigPath},
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

// TestServeCmd_EnvVarBindings exercises viper's resolution chain end
// to end: with no CLI args and only OCIFACTORY_* / PORT in env,
// every flag must surface the env value via v.GetString / GetBool /
// GetDuration / stringSliceCSV. This guards the env-only deployment
// path that Cloud Run / k8s rely on.
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
	// Stop short of starting the server: replace RunE with a no-op
	// so we only exercise flag parsing + viper binding.
	cmd.RunE = func(c *cobra.Command, _ []string) error { return nil }

	cmd.SetArgs(nil) // no CLI args — env vars must be the only source
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute() error = %v", err)
	}

	wantIssuers := []string{
		"https://accounts.google.com",
		"https://token.actions.githubusercontent.com",
	}
	wantScopes := []string{
		"https://www.googleapis.com/auth/cloud-platform.read-only",
		"https://www.googleapis.com/auth/userinfo.email",
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{flagPort, v.GetString(flagPort), "9090"},
		{flagRepoType, v.GetString(flagRepoType), "python"},
		{flagBackendRegistry, v.GetString(flagBackendRegistry), "zot.example.com:5000/ocifactory"},
		{flagAuthnKind, v.GetString(flagAuthnKind), "oidc"},
		{flagAuthnOIDCIssuers, stringSliceCSV(v, flagAuthnOIDCIssuers), wantIssuers},
		{flagAuthnOIDCAudience, v.GetString(flagAuthnOIDCAudience), "https://ocifactory.example"},
		{flagBackendAuthKind, v.GetString(flagBackendAuthKind), "staticenv"},
		{flagBackendAuthStaticEnvUserEnv, v.GetString(flagBackendAuthStaticEnvUserEnv), "REG_USER"},
		{flagBackendAuthStaticEnvPasswordEnv, v.GetString(flagBackendAuthStaticEnvPasswordEnv), "REG_PASS"},
		{flagBackendAuthGCPADCScopes, stringSliceCSV(v, flagBackendAuthGCPADCScopes), wantScopes},
		{flagDisableAuthn, v.GetBool(flagDisableAuthn), true},
		{flagSimpleIndexCacheTTL, v.GetDuration(flagSimpleIndexCacheTTL), 13 * time.Second},
	}
	for _, c := range checks {
		if diff := cmp.Diff(c.want, c.got); diff != "" {
			t.Errorf("%s mismatch (-want +got):\n%s", c.name, diff)
		}
	}
}
