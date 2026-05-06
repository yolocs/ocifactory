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

// authnDisabled is a convenience preset for the validation cases
// that aren't exercising the authn-config branches; it sidesteps
// the "either --authn-kind or --disable-authn" guard.
var validAuthnDisabled = func(f *serveFlags) { f.authnDisabled = true }

// validAuthnOIDC populates flags so the oidc branch passes
// validation, used by the cases that aren't exercising authn.
var validAuthnOIDC = func(f *serveFlags) {
	f.authnKind = "oidc"
	f.authnOIDCIssuers = []string{"https://accounts.google.com"}
	f.authnOIDCAudience = "https://ocifactory.example"
}

func TestServeFlagsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		flags           serveFlags
		mut             func(*serveFlags) // applied before Validate to centralise authn presets
		wantErr         string
		wantRegistryURL *url.URL
	}{
		{
			name: "all fields set",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
			},
			mut:     validAuthnDisabled,
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "missing port",
			flags: serveFlags{
				repoType:       "maven",
				registryURLStr: "http://example.com",
			},
			mut:     validAuthnDisabled,
			wantErr: "port is required",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "missing repo type",
			flags: serveFlags{
				port:           "8080",
				registryURLStr: "http://example.com",
			},
			mut:     validAuthnDisabled,
			wantErr: `repo-type "" is not supported`,
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "invalid repo type",
			flags: serveFlags{
				port:           "8080",
				registryURLStr: "http://example.com",
				repoType:       "invalid",
			},
			mut:     validAuthnDisabled,
			wantErr: `repo-type "invalid" is not supported`,
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "missing registry URL",
			flags: serveFlags{
				port:     "8080",
				repoType: "maven",
			},
			mut:     validAuthnDisabled,
			wantErr: "backend-registry is required",
			// registryURL stays nil — Validate short-circuits before
			// touching it when the user didn't pass --backend-registry.
		},
		{
			name: "registry URL without protocol prefix",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "example.com",
			},
			mut:     validAuthnDisabled,
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "https",
				Host:   "example.com",
			},
		},
		{
			name: "https registry URL passed directly",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "https://gar.example.com/project",
			},
			mut:     validAuthnDisabled,
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "https",
				Host:   "gar.example.com",
				Path:   "/project",
			},
		},
		{
			name: "missing authn config rejects",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
			},
			wantErr: "either --authn-kind",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "unsupported authn kind",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
				authnKind:      "saml",
			},
			wantErr: `authn-kind "saml" is not supported`,
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "oidc kind missing issuers",
			flags: serveFlags{
				port:              "8080",
				repoType:          "maven",
				registryURLStr:    "http://example.com",
				authnKind:         "oidc",
				authnOIDCAudience: "https://ocifactory.example",
			},
			wantErr: "authn-oidc-issuers",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "oidc kind missing audience",
			flags: serveFlags{
				port:             "8080",
				repoType:         "maven",
				registryURLStr:   "http://example.com",
				authnKind:        "oidc",
				authnOIDCIssuers: []string{"https://accounts.google.com"},
			},
			wantErr: "authn-oidc-audience",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "oidc kind fully configured",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
			},
			mut:     validAuthnOIDC,
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			// echo is the no-op CI auth target — it never talks to
			// an OCI backend, so --backend-registry is optional.
			name: "echo without backend-registry is allowed",
			flags: serveFlags{
				port:     "8080",
				repoType: "echo",
			},
			mut:     validAuthnDisabled,
			wantErr: "",
		},
		{
			// echo still accepts --backend-registry if the operator
			// supplies one (it just won't be used). Make sure the URL
			// parsing still happens so any malformed value is caught.
			name: "echo with backend-registry parses URL",
			flags: serveFlags{
				port:           "8080",
				repoType:       "echo",
				registryURLStr: "https://example.com",
			},
			mut:     validAuthnDisabled,
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "https",
				Host:   "example.com",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.flags
			if tc.mut != nil {
				tc.mut(&f)
			}
			err := f.Validate()
			if diff := testutil.DiffErrString(err, tc.wantErr); diff != "" {
				t.Errorf("Validate() returned unexpected error (-got, +want): %s", diff)
			}
			if diff := cmp.Diff(tc.wantRegistryURL, f.registryURL, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Validate() registryURL mismatch (-want +got):\n%s", diff)
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
		{name: "port", flagName: "port"},
		{name: "repo-type", flagName: "repo-type", shorthand: "t"},
		{name: "backend-registry", flagName: "backend-registry"},
		{name: "disable-streaming-push", flagName: "disable-streaming-push"},
		{name: "disable-authn", flagName: "disable-authn"},
		{name: "authn-kind", flagName: "authn-kind"},
		{name: "authn-oidc-issuers", flagName: "authn-oidc-issuers"},
		{name: "authn-oidc-audience", flagName: "authn-oidc-audience"},
		{name: "backend-auth-kind", flagName: "backend-auth-kind"},
		{name: "backend-auth-gcpadc-scopes", flagName: "backend-auth-gcpadc-scopes"},
		{name: "backend-auth-staticenv-user-env", flagName: "backend-auth-staticenv-user-env"},
		{name: "backend-auth-staticenv-password-env", flagName: "backend-auth-staticenv-password-env"},
		{name: "backend-auth-dockerconfig-path", flagName: "backend-auth-dockerconfig-path"},
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

// TestServeCmd_EnvVarBindings exercises the viper precedence chain
// — env var > flag default — for representative scalar, bool,
// duration, and string-slice flags. CLI flag override is not
// covered here because cobra parsing only fires under cmd.Execute,
// which would actually start the server; we trust the standard
// viper.BindPFlags / pflag.Changed contract for that side.
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

	cmd := newServeCmd()
	// Replace the production RunE with one that loads flags through
	// the standard viper plumbing and captures the resolved struct,
	// stopping short of starting the server.
	captured := &serveFlags{}
	cmd.RunE = makeCapturingRunE(captured)

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
		{"port", captured.port, "9090"},
		{"repoType", captured.repoType, "python"},
		{"registryURLStr", captured.registryURLStr, "zot.example.com:5000/ocifactory"},
		{"authnKind", captured.authnKind, "oidc"},
		{"authnOIDCIssuers", captured.authnOIDCIssuers, wantIssuers},
		{"authnOIDCAudience", captured.authnOIDCAudience, "https://ocifactory.example"},
		{"backendAuthKind", captured.backendAuthKind, "staticenv"},
		{"backendAuthStaticEnvUserEnv", captured.backendAuthStaticEnvUserEnv, "REG_USER"},
		{"backendAuthStaticEnvPasswordEnv", captured.backendAuthStaticEnvPasswordEnv, "REG_PASS"},
		{"backendAuthGCPADCScopes", captured.backendAuthGCPADCScopes, wantScopes},
		{"authnDisabled", captured.authnDisabled, true},
		{"simpleIndexCacheTTL", captured.simpleIndexCacheTTL, 13 * time.Second},
	}
	for _, c := range checks {
		if diff := cmp.Diff(c.want, c.got); diff != "" {
			t.Errorf("%s mismatch (-want +got):\n%s", c.name, diff)
		}
	}
}

// makeCapturingRunE returns a RunE that loads flags through the
// standard viper plumbing, captures the resolved struct, and
// stops short of starting the server. Built as a helper so the
// test stays readable and the production RunE wiring isn't
// duplicated.
func makeCapturingRunE(out *serveFlags) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		// Reach back to the same viper instance newServeCmd built
		// by re-binding. Building a fresh viper here gets the same
		// AutomaticEnv mapping and proves the env-var → flag
		// resolution end-to-end — including the comma-split path
		// for string slices.
		v := viper.New()
		v.SetEnvPrefix(envPrefix)
		v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
		v.AutomaticEnv()
		_ = v.BindEnv("port", "PORT")
		if err := v.BindPFlags(cmd.Flags()); err != nil {
			return err
		}
		return loadServeFlags(v, cmd, out)
	}
}
