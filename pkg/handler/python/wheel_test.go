package python

import (
	"archive/zip"
	"bytes"
	"testing"
)

// buildTestWheel writes a minimal in-memory zip archive that mimics a
// wheel: at least one `<distinfo>/METADATA` file, optionally other
// members. The returned bytes are usable as a wheel body.
func buildTestWheel(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestParseRequiresPython(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "header present",
			in:   "Metadata-Version: 2.1\nName: requests\nRequires-Python: >=3.7\nSummary: A library\n\nbody",
			want: ">=3.7",
		},
		{
			name: "header absent",
			in:   "Metadata-Version: 2.1\nName: requests\nSummary: A library\n",
			want: "",
		},
		{
			name: "header has surrounding whitespace",
			in:   "Name: foo\nRequires-Python:    !=3.0.*  \nSummary: x\n",
			want: "!=3.0.*",
		},
		{
			name: "header after blank line is ignored (body)",
			in:   "Name: foo\n\nRequires-Python: >=3.5\n",
			want: "",
		},
		{
			name: "first occurrence wins",
			in:   "Requires-Python: >=3.7\nName: foo\nRequires-Python: >=3.10\n",
			want: ">=3.7",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseRequiresPython([]byte(tc.in)); got != tc.want {
				t.Errorf("parseRequiresPython(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsWheelFilename(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "wheel lowercase", in: "requests-2.0.0-py3-none-any.whl", want: true},
		{name: "wheel uppercase", in: "REQUESTS-2.0.0.WHL", want: true},
		{name: "tarball", in: "requests-2.0.0.tar.gz", want: false},
		{name: "no extension", in: "requests", want: false},
		{name: "looks-like extension elsewhere", in: "whl-tools-1.0.tar.gz", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isWheelFilename(tc.in); got != tc.want {
				t.Errorf("isWheelFilename(%q) = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}
