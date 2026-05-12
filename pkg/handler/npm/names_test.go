package npm

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestEncodePackageName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "unscoped", in: "left-pad", want: "left-pad"},
		{name: "scoped", in: "@scope/foo", want: "_40scope_2Ffoo"},
		{name: "underscore escaped", in: "foo_bar", want: "foo_5Fbar"},
		{name: "empty", in: "", wantErr: true},
		{name: "dot", in: ".", wantErr: true},
		{name: "parent", in: "../foo", wantErr: true},
		{name: "uppercase", in: "Foo", wantErr: true},
		{name: "bad scoped", in: "@scope", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := encodePackageName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("encodePackageName() error = %v, wantErr %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("encodePackageName() mismatch (-want +got):\n%s", diff)
			}
			if err != nil {
				return
			}
			roundTrip, err := decodePackageName(got)
			if err != nil {
				t.Fatalf("decodePackageName() error = %v", err)
			}
			if diff := cmp.Diff(tc.in, roundTrip); diff != "" {
				t.Errorf("decodePackageName() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
