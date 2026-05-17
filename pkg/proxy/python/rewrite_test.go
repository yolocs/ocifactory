package python

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseFilenameVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		filename string
		pkg      string
		want     string
		wantErr  bool
	}{
		{
			name:     "wheel basic",
			filename: "requests-2.31.0-py3-none-any.whl",
			pkg:      "requests",
			want:     "2.31.0",
		},
		{
			name:     "wheel with hyphenated version",
			filename: "requests-2.31.0a1-py3-none-any.whl",
			pkg:      "requests",
			want:     "2.31.0a1",
		},
		{
			name:     "sdist tar.gz",
			filename: "requests-2.31.0.tar.gz",
			pkg:      "requests",
			want:     "2.31.0",
		},
		{
			name:     "sdist zip",
			filename: "requests-2.31.0.zip",
			pkg:      "requests",
			want:     "2.31.0",
		},
		{
			name:     "sdist tar.bz2",
			filename: "requests-2.31.0.tar.bz2",
			pkg:      "requests",
			want:     "2.31.0",
		},
		{
			name:     "wheel metadata sidecar",
			filename: "requests-2.31.0-py3-none-any.whl.metadata",
			pkg:      "requests",
			want:     "2.31.0",
		},
		{
			name:     "hyphen-to-underscore in distribution",
			filename: "python_dateutil-2.8.2-py2.py3-none-any.whl",
			pkg:      "python-dateutil",
			want:     "2.8.2",
		},
		{
			name:     "egg",
			filename: "foo-1.0.0-py3.10.egg",
			pkg:      "foo",
			want:     "1.0.0-py3.10",
		},
		{
			name:     "case-insensitive distribution match",
			filename: "Pillow-10.0.0-cp311-cp311-manylinux_2_28_x86_64.whl",
			pkg:      "pillow",
			want:     "10.0.0",
		},
		{
			name:     "unrelated filename",
			filename: "other-1.0.0-py3-none-any.whl",
			pkg:      "requests",
			wantErr:  true,
		},
		{
			name:     "wheel too few segments",
			filename: "requests-2.31.0.whl",
			pkg:      "requests",
			wantErr:  true,
		},
		{
			name:     "empty filename",
			filename: "",
			pkg:      "requests",
			wantErr:  true,
		},
		{
			name:     "empty pkg",
			filename: "requests-2.31.0.tar.gz",
			pkg:      "",
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFilenameVersion(tc.filename, tc.pkg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseFilenameVersion(%q, %q) err=%v, wantErr=%v",
					tc.filename, tc.pkg, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("parseFilenameVersion(%q, %q) = %q, want %q",
					tc.filename, tc.pkg, got, tc.want)
			}
		})
	}
}

func TestRewriteSimpleIndex(t *testing.T) {
	t.Parallel()

	const upstreamHTML = `<!DOCTYPE html>
<html>
<head><title>Links for requests</title></head>
<body>
<h1>Links for requests</h1>
<a href="https://files.pythonhosted.org/packages/aa/47/cf/requests-2.31.0-py3-none-any.whl#sha256=abc123" data-requires-python="&gt;=3.7">requests-2.31.0-py3-none-any.whl</a><br>
<a href="https://files.pythonhosted.org/packages/aa/47/cf/requests-2.31.0.tar.gz#sha256=def456">requests-2.31.0.tar.gz</a><br>
<a href="https://files.pythonhosted.org/packages/aa/47/cf/requests-2.31.0-py3-none-any.whl.metadata#sha256=meta789" data-dist-info-metadata="sha256=meta789">requests-2.31.0-py3-none-any.whl.metadata</a><br>
</body>
</html>
`
	out, err := RewriteSimpleIndex([]byte(upstreamHTML), "myns", "requests")
	if err != nil {
		t.Fatalf("RewriteSimpleIndex: %v", err)
	}
	s := string(out)
	wantHrefs := []string{
		`href="/myns/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl#sha256=abc123"`,
		`href="/myns/packages/requests/2.31.0/requests-2.31.0.tar.gz#sha256=def456"`,
		`href="/myns/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl.metadata#sha256=meta789"`,
	}
	for _, want := range wantHrefs {
		if !strings.Contains(s, want) {
			t.Errorf("rewritten body missing %q\ngot:\n%s", want, s)
		}
	}
	// Pre-existing data-* attributes survive the rewrite.
	if !strings.Contains(s, `data-requires-python="&gt;=3.7"`) {
		t.Errorf("rewritten body lost data-requires-python attribute:\n%s", s)
	}
	if !strings.Contains(s, `data-dist-info-metadata="sha256=meta789"`) {
		t.Errorf("rewritten body lost data-dist-info-metadata attribute:\n%s", s)
	}
}

func TestRewriteSimpleIndex_LeavesUnparseableAnchors(t *testing.T) {
	t.Parallel()

	in := `<html><body>` +
		`<a href="https://files.pythonhosted.org/somewhere/unknown-archive.bin">unknown</a>` +
		`<a href="https://files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl#sha256=abc">requests-2.31.0-py3-none-any.whl</a>` +
		`</body></html>`
	out, err := RewriteSimpleIndex([]byte(in), "myns", "requests")
	if err != nil {
		t.Fatalf("RewriteSimpleIndex: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `href="https://files.pythonhosted.org/somewhere/unknown-archive.bin"`) {
		t.Errorf("unparseable anchor should pass through unchanged\ngot: %s", s)
	}
	if !strings.Contains(s, `href="/myns/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl#sha256=abc"`) {
		t.Errorf("parseable anchor should be rewritten\ngot: %s", s)
	}
}

func TestRewriteSimpleIndex_Errors(t *testing.T) {
	t.Parallel()
	if _, err := RewriteSimpleIndex([]byte("<html></html>"), "", "requests"); err == nil {
		t.Errorf("expected error for empty namespace")
	}
	if _, err := RewriteSimpleIndex([]byte("<html></html>"), "myns", ""); err == nil {
		t.Errorf("expected error for empty pkg")
	}
}

func TestVersionMetadata_FindFile(t *testing.T) {
	t.Parallel()
	meta := &VersionMetadata{
		Files: []FileMetadata{
			{Filename: "requests-2.31.0-py3-none-any.whl", URL: "u1"},
			{Filename: "requests-2.31.0.tar.gz", URL: "u2"},
		},
	}
	tests := []struct {
		name     string
		filename string
		want     FileMetadata
		wantOK   bool
	}{
		{
			name:     "wheel hit",
			filename: "requests-2.31.0-py3-none-any.whl",
			want:     meta.Files[0],
			wantOK:   true,
		},
		{
			name:     "sdist hit",
			filename: "requests-2.31.0.tar.gz",
			want:     meta.Files[1],
			wantOK:   true,
		},
		{
			name:     "miss",
			filename: "requests-2.31.0.zip",
			wantOK:   false,
		},
		{
			name:     "case-sensitive miss",
			filename: "Requests-2.31.0-py3-none-any.whl",
			wantOK:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := meta.FindFile(tc.filename)
			if ok != tc.wantOK {
				t.Fatalf("FindFile(%q) ok=%v, want %v", tc.filename, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("FindFile(%q) mismatch (-want +got):\n%s", tc.filename, diff)
			}
		})
	}
}
