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
		rules    []filter.Rule
		ref      filter.Ref
		want     filter.Decision
	}{
		{
			name:     "patterns-exact-match-denies",
			patterns: []string{"evil"},
			ref:      filter.Ref{Package: "evil"},
			want:     filter.DecisionDeny,
		},
		{
			name:     "patterns-no-match-allows",
			patterns: []string{"evil"},
			ref:      filter.Ref{Package: "safe"},
			want:     filter.DecisionAllow,
		},
		{
			name:     "patterns-glob-match",
			patterns: []string{"evil-*"},
			ref:      filter.Ref{Package: "evil-package"},
			want:     filter.DecisionDeny,
		},
		{
			name:  "rule-package-and-version-both-match-denies",
			rules: []filter.Rule{{Package: "log4j-core", Version: "2.14.*"}},
			ref:   filter.Ref{Package: "log4j-core", Version: "2.14.1"},
			want:  filter.DecisionDeny,
		},
		{
			name:  "rule-version-mismatch-allows",
			rules: []filter.Rule{{Package: "log4j-core", Version: "2.14.*"}},
			ref:   filter.Ref{Package: "log4j-core", Version: "2.17.1"},
			want:  filter.DecisionAllow,
		},
		{
			// Index-time (no version on the ref): the rule's version
			// constraint is ignored and we fall back to package match,
			// so the denylist denies the log4j-core index entirely.
			// That's the symmetric "missing version = wildcard"
			// semantic the operator chose; per-version requests are
			// still individually checked.
			name:  "rule-empty-ref-version-treats-version-as-wildcard",
			rules: []filter.Rule{{Package: "log4j-core", Version: "2.14.*"}},
			ref:   filter.Ref{Package: "log4j-core"},
			want:  filter.DecisionDeny,
		},
		{
			name:     "patterns-and-rules-both-checked",
			patterns: []string{"banned"},
			rules:    []filter.Rule{{Package: "log4j-core", Version: "2.14.*"}},
			ref:      filter.Ref{Package: "log4j-core", Version: "2.14.1"},
			want:     filter.DecisionDeny,
		},
		{
			name:     "first-of-many-allows",
			patterns: []string{"a", "b", "c"},
			ref:      filter.Ref{Package: "d"},
			want:     filter.DecisionAllow,
		},
		{
			name:     "later-of-many-denies",
			patterns: []string{"a", "b", "c"},
			ref:      filter.Ref{Package: "c"},
			want:     filter.DecisionDeny,
		},
		{
			name:  "rule-package-only-denies-any-version",
			rules: []filter.Rule{{Package: "evil"}},
			ref:   filter.Ref{Package: "evil", Version: "9.9.9"},
			want:  filter.DecisionDeny,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &filter.Denylist{Patterns: tc.patterns, Rules: tc.rules}
			got, err := d.Allow(t.Context(), tc.ref)
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tc.want {
				t.Errorf("Allow(%+v) = %v, want %v", tc.ref, got, tc.want)
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
			name: "valid-patterns",
			body: `{"kind":"deny","patterns":["evil","evil-*"]}`,
		},
		{
			name: "valid-rules",
			body: `{"kind":"deny","rules":[{"package":"log4j-core","version":"2.14.*"}]}`,
		},
		{
			name: "valid-mixed",
			body: `{"kind":"deny","patterns":["evil"],"rules":[{"package":"log4j-core","version":"2.14.*"}]}`,
		},
		{
			name:    "empty-everything",
			body:    `{"kind":"deny"}`,
			wantErr: true,
		},
		{
			name:    "malformed-pattern",
			body:    `{"kind":"deny","patterns":["[bad"]}`,
			wantErr: true,
		},
		{
			name:    "rule-empty",
			body:    `{"kind":"deny","rules":[{}]}`,
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
