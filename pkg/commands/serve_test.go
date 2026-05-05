package commands

import (
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/yolocs/ocifactory/pkg/testutil"
)

func TestServeFlagsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		flags           serveFlags
		wantErr         string
		wantRegistryURL *url.URL
	}{
		{
			name: "all fields set",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
				authNone:       true,
			},
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
				authNone:       true,
			},
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
				authNone:       true,
			},
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
				authNone:       true,
			},
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
				authNone: true,
			},
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
				authNone:       true,
			},
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
				authNone:       true,
			},
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "https",
				Host:   "gar.example.com",
				Path:   "/project",
			},
		},
		{
			name: "auth-config and disable-auth mutually exclusive",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
				authConfigPath: "/etc/auth.yaml",
				authNone:       true,
			},
			wantErr: "mutually exclusive",
			wantRegistryURL: &url.URL{
				Scheme: "http",
				Host:   "example.com",
			},
		},
		{
			name: "missing auth config and disable-auth not set",
			flags: serveFlags{
				port:           "8080",
				repoType:       "maven",
				registryURLStr: "http://example.com",
			},
			wantErr: "either --auth-config or --disable-auth must be set",
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
				authNone: true,
			},
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
				authNone:       true,
			},
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
			err := tc.flags.Validate()
			if diff := testutil.DiffErrString(err, tc.wantErr); diff != "" {
				t.Errorf("Validate() returned unexpected error (-got, +want): %s", diff)
			}
			if diff := cmp.Diff(tc.wantRegistryURL, tc.flags.registryURL, cmpopts.EquateEmpty()); diff != "" {
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
		{name: "auth-config", flagName: "auth-config"},
		{name: "disable-auth", flagName: "disable-auth"},
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
