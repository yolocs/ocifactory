package oci

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestEncodeTagDecodeTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain", input: "packages/foo", want: "packages_2Ffoo"},
		{name: "underscore", input: "foo_bar", want: "foo_5Fbar"},
		{name: "npm scoped", input: "@scope/foo", want: "_40scope_2Ffoo"},
		{name: "keeps safe chars", input: "a.Z-0", want: "a.Z-0"},
		{name: "empty", input: "", wantErr: true},
		{name: "encoded too long", input: strings.Repeat("_", 43), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := EncodeTag(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EncodeTag(%q) err = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("EncodeTag(%q) mismatch (-want +got):\n%s", tc.input, diff)
			}
			back, err := DecodeTag(got)
			if err != nil {
				t.Fatalf("DecodeTag(%q): %v", got, err)
			}
			if diff := cmp.Diff(tc.input, back); diff != "" {
				t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDecodeTagRejectsMalformedEscapes(t *testing.T) {
	t.Parallel()

	for _, tag := range []string{"foo_", "foo_zz"} {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeTag(tag); err == nil {
				t.Fatalf("DecodeTag(%q) = nil, want error", tag)
			}
		})
	}
}
