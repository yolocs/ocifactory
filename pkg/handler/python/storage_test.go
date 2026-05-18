package python

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPackageOwningRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pkg  string
		want string
	}{
		{name: "normalizes", pkg: "Foo._-Bar", want: "packages/foo-bar"},
		{name: "already normalized", pkg: "requests", want: "packages/requests"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(tc.want, packageOwningRepo(tc.pkg)); diff != "" {
				t.Errorf("packageOwningRepo(%q) mismatch (-want +got):\n%s", tc.pkg, diff)
			}
		})
	}
}
