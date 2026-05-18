package maven

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPackageOwningRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		repoParts string
		want      string
	}{
		{name: "group artifact", repoParts: "com/google/guava/guava", want: "packages/com/google/guava/guava"},
		{name: "versioned metadata path", repoParts: "com/example/project/1.0.0", want: "packages/com/example/project/1.0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(tc.want, packageOwningRepo(tc.repoParts)); diff != "" {
				t.Errorf("packageOwningRepo(%q) mismatch (-want +got):\n%s", tc.repoParts, diff)
			}
		})
	}
}
