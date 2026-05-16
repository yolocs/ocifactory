package filter_test

import (
	"errors"
	"testing"

	"github.com/yolocs/ocifactory/pkg/proxy/filter"
)

func TestDenylist_Allow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		patterns []string
		pkg      string
		want     filter.Decision
	}{
		{
			name:     "exact-match-denies",
			patterns: []string{"evil"},
			pkg:      "evil",
			want:     filter.DecisionDeny,
		},
		{
			name:     "no-match-allows",
			patterns: []string{"evil"},
			pkg:      "safe",
			want:     filter.DecisionAllow,
		},
		{
			name:     "glob-match",
			patterns: []string{"evil-*"},
			pkg:      "evil-package",
			want:     filter.DecisionDeny,
		},
		{
			name:     "first-of-many-allows",
			patterns: []string{"a", "b", "c"},
			pkg:      "d",
			want:     filter.DecisionAllow,
		},
		{
			name:     "later-of-many-denies",
			patterns: []string{"a", "b", "c"},
			pkg:      "c",
			want:     filter.DecisionDeny,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &filter.Denylist{Patterns: tc.patterns}
			got, err := d.Allow(t.Context(), filter.Ref{Package: tc.pkg})
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tc.want {
				t.Errorf("Allow(%q) = %v, want %v", tc.pkg, got, tc.want)
			}
		})
	}
}

func TestDenylist_Kind(t *testing.T) {
	t.Parallel()
	d := &filter.Denylist{Patterns: []string{"foo"}}
	if got := d.Kind(); got != filter.KindDenylist {
		t.Errorf("Kind() = %q, want %q", got, filter.KindDenylist)
	}
}

func TestDenylist_Construction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "valid",
			body: `{"kind":"denylist","patterns":["evil","evil-*"]}`,
		},
		{
			name:    "empty-patterns",
			body:    `{"kind":"denylist","patterns":[]}`,
			wantErr: true,
		},
		{
			name:    "malformed-glob",
			body:    `{"kind":"denylist","patterns":["[bad"]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := filter.UnmarshalFilter([]byte(tc.body))
			if tc.wantErr {
				if err == nil || !errors.Is(err, filter.ErrInvalidFilter) {
					t.Errorf("UnmarshalFilter(%q) error = %v, want errors.Is ErrInvalidFilter", tc.body, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalFilter(%q) error: %v", tc.body, err)
			}
		})
	}
}
