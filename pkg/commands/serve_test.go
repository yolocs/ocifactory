package commands

import (
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
