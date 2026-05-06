package python

import (
	"archive/zip"
	"bytes"
	"errors"
	"strconv"
	"testing"
)

// buildTestWheel writes a minimal in-memory zip archive that mimics a
// wheel: at least one `<distinfo>/METADATA` file, optionally other
// members. The returned bytes can be handed to extractWheelMetadata
// via bytes.NewReader.
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

func TestExtractWheelMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		files   map[string]string
		want    string
		wantErr error
	}{
		{
			name: "metadata present",
			files: map[string]string{
				"requests-2.0.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: requests\nRequires-Python: >=3.7\n",
				"requests-2.0.0.dist-info/RECORD":   "ignored",
				"requests/__init__.py":              "",
			},
			want: "Metadata-Version: 2.1\nName: requests\nRequires-Python: >=3.7\n",
		},
		{
			name: "no dist-info METADATA",
			files: map[string]string{
				"requests/__init__.py": "",
			},
			wantErr: errMetadataNotFound,
		},
		{
			name: "metadata in nested distinfo only matches top-level",
			files: map[string]string{
				"sub/something.dist-info/METADATA":  "wrong",
				"requests-2.0.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: requests\n",
			},
			want: "Metadata-Version: 2.1\nName: requests\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := buildTestWheel(t, tc.files)
			got, err := extractWheelMetadata(bytes.NewReader(data), int64(len(data)))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("extractWheelMetadata error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractWheelMetadata unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("extractWheelMetadata = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractWheelMetadata_NotZip(t *testing.T) {
	t.Parallel()

	data := []byte("not a zip file")
	_, err := extractWheelMetadata(bytes.NewReader(data), int64(len(data)))
	if err == nil {
		t.Fatalf("expected error for non-zip input, got nil")
	}
	if errors.Is(err, errMetadataNotFound) {
		t.Errorf("non-zip should not surface as errMetadataNotFound; got %v", err)
	}
}

// TestExtractWheelMetadata_TooManyEntries exercises the zip-bomb cap:
// a wheel whose central directory exceeds maxWheelEntries is rejected
// before its members are walked.
func TestExtractWheelMetadata_TooManyEntries(t *testing.T) {
	t.Parallel()

	files := make(map[string]string, maxWheelEntries+1)
	for i := 0; i <= maxWheelEntries; i++ {
		files["pkg/file-"+strconv.Itoa(i)+".py"] = ""
	}
	data := buildTestWheel(t, files)
	_, err := extractWheelMetadata(bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, errWheelTooManyEntries) {
		t.Errorf("error = %v, want errWheelTooManyEntries", err)
	}
}

// TestExtractWheelMetadata_OversizedDeclaredSize confirms that a wheel
// whose METADATA member declares an UncompressedSize64 above the cap
// is rejected before we read the body — the cap on read alone isn't
// enough if a malformed ZIP64 header lies about the size.
func TestExtractWheelMetadata_OversizedDeclaredSize(t *testing.T) {
	t.Parallel()

	// Build a real wheel zip, then mutate the central-directory
	// header so METADATA claims a size larger than maxMetadataSize.
	// We can't easily mutate post-encoding here without reaching
	// into archive/zip internals, so instead we write an actual
	// METADATA entry of size > maxMetadataSize and assert it's
	// rejected.
	big := bytes.Repeat([]byte("x"), maxMetadataSize+1)
	data := buildTestWheel(t, map[string]string{
		"requests-1.0.0.dist-info/METADATA": string(big),
	})
	_, err := extractWheelMetadata(bytes.NewReader(data), int64(len(data)))
	if err == nil || errors.Is(err, errMetadataNotFound) || errors.Is(err, errWheelTooManyEntries) {
		t.Errorf("expected oversized-METADATA rejection, got %v", err)
	}
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
