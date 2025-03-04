package commands

import (
	"testing"

	"github.com/abcxyz/pkg/testutil"
)

func TestServeFlagsValidate(t *testing.T) {

	cases := []struct {
		name    string
		flags   serveFlags
		wantErr string
	}{
		{
			name: "all fields set",
			flags: serveFlags{
				port:           "8080",
				repoType:       "type",
				registryURLStr: "http://example.com",
			},
			wantErr: "",
		},
		{
			name: "missing port",
			flags: serveFlags{
				repoType:       "type",
				registryURLStr: "http://example.com",
			},
			wantErr: "port is required",
		},
		{
			name: "missing repo type",
			flags: serveFlags{
				port:           "8080",
				registryURLStr: "http://example.com",
			},
			wantErr: "repo-type is required",
		},
		{
			name: "missing registry URL",
			flags: serveFlags{
				port:     "8080",
				repoType: "type",
			},
			wantErr: "backend-registry is required",
		},
		{
			name: "registry URL without protocol prefix",
			flags: serveFlags{
				port:           "8080",
				repoType:       "type",
				registryURLStr: "example.com",
			},
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.flags.Validate()
			if diff := testutil.DiffErrString(err, tc.wantErr); diff != "" {
				t.Errorf("Validate() returned unexpected error (-got, +want): %s", diff)
			}
		})
	}
}
