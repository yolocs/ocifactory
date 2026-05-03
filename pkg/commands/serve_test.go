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
			},
			wantErr: "",
			wantRegistryURL: &url.URL{
				Scheme: "https",
				Host:   "gar.example.com",
				Path:   "/project",
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
