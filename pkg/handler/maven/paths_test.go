package maven

import (
	"testing"
)

func TestValidatePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		repoParts string
		version   string
		filename  string
		wantErr   bool
	}{
		{
			name:      "typical maven coordinates",
			repoParts: "com/example/project",
			version:   "1.0.0",
			filename:  "project-1.0.0.jar",
			wantErr:   false,
		},
		{
			name:      "snapshot version",
			repoParts: "com/example/project",
			version:   "1.0-SNAPSHOT",
			filename:  "project-1.0-SNAPSHOT.jar",
			wantErr:   false,
		},
		{
			name:      "snapshot timestamped filename",
			repoParts: "com/example/project",
			version:   "1.0-SNAPSHOT",
			filename:  "project-1.0-20260101.123456-1.jar",
			wantErr:   false,
		},
		{
			name:      "checksum filename",
			repoParts: "com/example/project",
			version:   "1.0.0",
			filename:  "project-1.0.0.jar.sha1",
			wantErr:   false,
		},
		{
			name:      "single-segment groupId",
			repoParts: "com/foo",
			version:   "1.0",
			filename:  "foo-1.0.jar",
			wantErr:   false,
		},
		{
			name:      "empty optional fields",
			repoParts: "com/example/project",
			version:   "",
			filename:  "",
			wantErr:   false,
		},
		{
			name:      "traversal segment in repoParts",
			repoParts: "com/../etc",
			version:   "1.0.0",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "traversal segment in version",
			repoParts: "com/example/project",
			version:   "..",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "traversal segment in filename",
			repoParts: "com/example/project",
			version:   "1.0.0",
			filename:  "..",
			wantErr:   true,
		},
		{
			name:      "single-dot segment in repoParts",
			repoParts: "com/./foo",
			version:   "1.0.0",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "leading slash in repoParts",
			repoParts: "/com/foo",
			version:   "1.0.0",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "trailing slash in repoParts",
			repoParts: "com/foo/",
			version:   "1.0.0",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "doubled slash in repoParts",
			repoParts: "com//foo",
			version:   "1.0.0",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "slash inside filename segment",
			repoParts: "com/foo",
			version:   "1.0",
			filename:  "etc/passwd",
			wantErr:   true,
		},
		{
			name:      "space in filename",
			repoParts: "com/foo",
			version:   "1.0",
			filename:  "bad name.jar",
			wantErr:   true,
		},
		{
			name:      "null byte in filename",
			repoParts: "com/foo",
			version:   "1.0",
			filename:  "bad\x00.jar",
			wantErr:   true,
		},
		{
			name:      "unicode in repoParts",
			repoParts: "com/föö",
			version:   "1.0",
			filename:  "x.jar",
			wantErr:   true,
		},
		{
			name:      "tilde in version",
			repoParts: "com/foo",
			version:   "1.0~beta",
			filename:  "x.jar",
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validatePath(tc.repoParts, tc.version, tc.filename)
			if got, want := err != nil, tc.wantErr; got != want {
				t.Errorf("validatePath(%q, %q, %q) err = %v, wantErr = %v", tc.repoParts, tc.version, tc.filename, err, want)
			}
		})
	}
}

func TestIsSnapshotVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "canonical snapshot", version: "1.0-SNAPSHOT", want: true},
		{name: "lowercase snapshot", version: "1.0-snapshot", want: true},
		{name: "mixed case snapshot", version: "1.0-SnApShOt", want: true},
		{name: "multi segment snapshot", version: "1.0.0-RC1-SNAPSHOT", want: true},
		{name: "release version", version: "1.0.0", want: false},
		{name: "release with -RC1 qualifier", version: "1.0.0-RC1", want: false},
		{name: "release with embedded SNAPSHOT not at end", version: "1.0-SNAPSHOT-final", want: false},
		{name: "empty string", version: "", want: false},
		{name: "shorter than suffix", version: "1.0", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isSnapshotVersion(tc.version); got != tc.want {
				t.Errorf("isSnapshotVersion(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}
