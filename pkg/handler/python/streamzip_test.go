package python

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestExtractWheelMetadataStream(t *testing.T) {
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
			name: "metadata after other entries",
			files: map[string]string{
				"requests/__init__.py":              "import x\nimport y\n",
				"requests/util.py":                  "def f(): pass\n",
				"requests-2.0.0.dist-info/METADATA": "Name: requests\nRequires-Python: >=3.10\n",
			},
			want: "Name: requests\nRequires-Python: >=3.10\n",
		},
		{
			name: "no dist-info METADATA",
			files: map[string]string{
				"requests/__init__.py": "",
			},
			wantErr: errMetadataNotFound,
		},
		{
			name: "nested distinfo only matches top-level",
			files: map[string]string{
				"sub/something.dist-info/METADATA":  "wrong",
				"requests-2.0.0.dist-info/METADATA": "Name: requests\n",
			},
			want: "Name: requests\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := buildTestWheel(t, tc.files)
			got, err := extractWheelMetadataStream(bytes.NewReader(data))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("extractWheelMetadataStream error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractWheelMetadataStream unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("extractWheelMetadataStream = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractWheelMetadataStream_OversizedDeclared verifies the cap on
// uncompressed METADATA size kicks in even when archive/zip has emitted
// the entry with bit-3 streaming framing — the decoder buffer stops at
// maxMetadataSize+1 and the walker errors before allocating more.
func TestExtractWheelMetadataStream_OversizedDeclared(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", maxMetadataSize+1)
	data := buildTestWheel(t, map[string]string{
		"requests-1.0.0.dist-info/METADATA": big,
	})
	_, err := extractWheelMetadataStream(bytes.NewReader(data))
	if err == nil || errors.Is(err, errMetadataNotFound) || errors.Is(err, errWheelTooManyEntries) {
		t.Errorf("expected oversized-METADATA rejection, got %v", err)
	}
}

// TestExtractWheelMetadataStream_TooManyEntries triggers the local-
// file-header count cap by writing entries in a fixed order (METADATA
// last) so the walker has to walk past the cap before finding it.
func TestExtractWheelMetadataStream_TooManyEntries(t *testing.T) {
	t.Parallel()

	entries := make([]orderedZipEntry, 0, maxWheelEntries+2)
	for i := 0; i < maxWheelEntries+1; i++ {
		entries = append(entries, orderedZipEntry{Name: "pkg/file-" + strconv.Itoa(i) + ".py"})
	}
	entries = append(entries, orderedZipEntry{
		Name: "requests-1.0.0.dist-info/METADATA",
		Body: "Name: requests\n",
	})
	data := buildOrderedZip(t, entries)
	_, err := extractWheelMetadataStream(bytes.NewReader(data))
	if !errors.Is(err, errWheelTooManyEntries) {
		t.Errorf("error = %v, want errWheelTooManyEntries", err)
	}
}

// orderedZipEntry is the ordered companion to buildTestWheel for tests
// that need a deterministic local-file-header sequence.
type orderedZipEntry struct {
	Name string
	Body string
}

func buildOrderedZip(t *testing.T, entries []orderedZipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.Name)
		if err != nil {
			t.Fatalf("zip create %q: %v", e.Name, err)
		}
		if _, err := w.Write([]byte(e.Body)); err != nil {
			t.Fatalf("zip write %q: %v", e.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractWheelMetadataStream_NotZip(t *testing.T) {
	t.Parallel()

	data := []byte("plain text, no PK signature")
	_, err := extractWheelMetadataStream(bytes.NewReader(data))
	if err == nil {
		t.Fatalf("expected error for non-zip input, got nil")
	}
	if errors.Is(err, errMetadataNotFound) {
		t.Errorf("non-zip should not surface as errMetadataNotFound; got %v", err)
	}
}

// TestExtractWheelMetadataStream_Encrypted verifies that an entry with
// the encryption flag set is reported as unreachable rather than parsed.
// archive/zip can't produce encrypted entries, so the test patches the
// general-purpose flag in a real zip's local file header.
func TestExtractWheelMetadataStream_Encrypted(t *testing.T) {
	t.Parallel()

	data := buildTestWheel(t, map[string]string{
		"requests-1.0.0.dist-info/METADATA": "Name: requests\n",
	})
	// First local file header starts at byte 0. Flags live at offset
	// 6 (uint16 little-endian). Set bit 0 (encrypted).
	flags := binary.LittleEndian.Uint16(data[6:8])
	flags |= zipFlagEncrypted
	binary.LittleEndian.PutUint16(data[6:8], flags)

	_, err := extractWheelMetadataStream(bytes.NewReader(data))
	if !errors.Is(err, errStreamMETADATAUnreachable) {
		t.Errorf("error = %v, want errStreamMETADATAUnreachable", err)
	}
}

// TestExtractWheelMetadataStream_NonWheelZip doesn't include any
// dist-info member, mirroring an sdist or arbitrary zip handed to the
// walker. The walker should report errMetadataNotFound, not error.
func TestExtractWheelMetadataStream_NonWheelZip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"requests/__init__.py", "requests/api.py", "README.md"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte("contents of " + name)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	_, err := extractWheelMetadataStream(bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, errMetadataNotFound) {
		t.Errorf("error = %v, want errMetadataNotFound", err)
	}
}

// TestExtractWheelMetadataStream_TruncatedHeader confirms that the
// walker reports an error when the input ends mid-header rather than
// silently treating it as end-of-archive.
func TestExtractWheelMetadataStream_TruncatedHeader(t *testing.T) {
	t.Parallel()

	data := buildTestWheel(t, map[string]string{
		"requests-1.0.0.dist-info/METADATA": "Name: requests\n",
	})
	// Truncate inside the first local file header.
	_, err := extractWheelMetadataStream(bytes.NewReader(data[:20]))
	if err == nil {
		t.Fatalf("expected error on truncated header, got nil")
	}
	if errors.Is(err, errMetadataNotFound) {
		t.Errorf("truncated header should not surface as errMetadataNotFound; got %v", err)
	}
}

// TestExtractWheelMetadataStream_StopsAfterFound is a regression
// check: the walker returns immediately after the METADATA entry is
// decoded, leaving the rest of the reader untouched. Callers
// downstream rely on that — they drain the remainder themselves so
// the writer side of an upload pipe doesn't deadlock.
//
// The test gates "doesn't read past entry N" by appending bytes that
// the walker would choke on if it kept reading: METADATA is the first
// entry, the rest of a normal zip follows, then garbage. A walker
// that stopped at METADATA never sees the garbage.
func TestExtractWheelMetadataStream_StopsAfterFound(t *testing.T) {
	t.Parallel()

	entries := []orderedZipEntry{
		{Name: "requests-1.0.0.dist-info/METADATA", Body: "Name: requests\n"},
		{Name: "requests/__init__.py", Body: strings.Repeat("a", 8*1024)},
	}
	data := buildOrderedZip(t, entries)
	// Append a buffer of arbitrary bytes the walker would treat as a
	// malformed-signature error if it reached them.
	data = append(data, bytes.Repeat([]byte{0xFF}, 4096)...)

	got, err := extractWheelMetadataStream(io.MultiReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatalf("extractWheelMetadataStream error: %v", err)
	}
	if string(got) != "Name: requests\n" {
		t.Errorf("got %q, want %q", got, "Name: requests\n")
	}
}
