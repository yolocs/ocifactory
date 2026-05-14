package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestFileTagFor pins the deterministic mapping from
// (owningTag, filename) onto the on-wire tag string. The output is part
// of the registry's persistence format — any change here is a
// migration. The cases also lock in the 0x00-separator collision guard
// the issue called out (("1.0","0foo") must not equal ("1.00","foo")).
func TestFileTagFor(t *testing.T) {
	t.Parallel()

	pair := func(owningTag, name string) string {
		h := sha256.New()
		h.Write([]byte(owningTag))
		h.Write([]byte{0x00})
		h.Write([]byte(name))
		return fileTagPrefix + hex.EncodeToString(h.Sum(nil))
	}

	tests := []struct {
		name       string
		owningTag  string
		filename   string
		wantPrefix string
		wantSame   *struct {
			owningTag string
			filename  string
		}
		wantDiffer *struct {
			owningTag string
			filename  string
		}
	}{
		{
			name:       "simple version + filename",
			owningTag:  "1.0.0",
			filename:   "react-1.0.0.tgz",
			wantPrefix: fileTagPrefix,
		},
		{
			name:       "different owning tag differs",
			owningTag:  "1.0.1",
			filename:   "react-1.0.0.tgz",
			wantPrefix: fileTagPrefix,
			wantDiffer: &struct {
				owningTag string
				filename  string
			}{owningTag: "1.0.0", filename: "react-1.0.0.tgz"},
		},
		{
			name:       "collision-resistant separator",
			owningTag:  "1.0",
			filename:   "0foo",
			wantPrefix: fileTagPrefix,
			wantDiffer: &struct {
				owningTag string
				filename  string
			}{owningTag: "1.00", filename: "foo"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := fileTagFor(tc.owningTag, tc.filename)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("fileTagFor(%q, %q) = %q, want prefix %q", tc.owningTag, tc.filename, got, tc.wantPrefix)
			}
			// Length: prefix (3) + sha256 hex (64) = 67. OCI tags
			// allow up to 128; we want some headroom but the size
			// should be stable.
			if want := len(fileTagPrefix) + 64; len(got) != want {
				t.Errorf("len(fileTagFor) = %d, want %d", len(got), want)
			}
			// Reference computation must match (locks in the hash
			// scheme — change here is a wire-format break).
			if want := pair(tc.owningTag, tc.filename); got != want {
				t.Errorf("fileTagFor mismatch:\ngot:  %q\nwant: %q", got, want)
			}
			if tc.wantDiffer != nil {
				other := fileTagFor(tc.wantDiffer.owningTag, tc.wantDiffer.filename)
				if got == other {
					t.Errorf("fileTagFor produced same tag for (%q,%q) and (%q,%q): %q",
						tc.owningTag, tc.filename, tc.wantDiffer.owningTag, tc.wantDiffer.filename, got)
				}
			}
		})
	}
}

func TestIsFileTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tag  string
		want bool
	}{
		{tag: fileTagFor("v1", "a.txt"), want: true},
		{tag: "_f_anything", want: true},
		{tag: "v1.0.0", want: false},
		{tag: "latest", want: false},
		{tag: "", want: false},
		{tag: "_factory", want: false}, // missing trailing underscore
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			t.Parallel()
			if got := isFileTag(tc.tag); got != tc.want {
				t.Errorf("isFileTag(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}
