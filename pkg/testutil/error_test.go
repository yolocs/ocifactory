package testutil

import (
	"errors"
	"strings"
	"testing"
)

func TestDiffErrString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		got         error
		want        string
		wantDiffHas string // empty means caller expects ""
	}{
		{
			name: "both nil/empty",
		},
		{
			name:        "want empty but got error",
			got:         errors.New("boom"),
			wantDiffHas: `got error "boom"`,
		},
		{
			name:        "want set but got nil",
			want:        "boom",
			wantDiffHas: `<nil> error`,
		},
		{
			name: "got contains want",
			got:  errors.New("failed to read file: not found"),
			want: "not found",
		},
		{
			name:        "got does not contain want",
			got:         errors.New("connection reset"),
			want:        "not found",
			wantDiffHas: `got error "connection reset"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := DiffErrString(tc.got, tc.want)
			if tc.wantDiffHas == "" {
				if got != "" {
					t.Errorf("DiffErrString(%v, %q) = %q, want empty", tc.got, tc.want, got)
				}
				return
			}
			if !strings.Contains(got, tc.wantDiffHas) {
				t.Errorf("DiffErrString(%v, %q) = %q, want substring %q", tc.got, tc.want, got, tc.wantDiffHas)
			}
		})
	}
}
