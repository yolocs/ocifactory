package filter_test

import (
	"errors"
	"testing"

	"github.com/yolocs/ocifactory/pkg/proxy/filter"
)

func TestAllowlist_Allow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		patterns []string
		rules    []filter.Rule
		ref      filter.Ref
		want     filter.Decision
	}{
		{
			name:     "patterns-exact-match",
			patterns: []string{"requests"},
			ref:      filter.Ref{Package: "requests"},
			want:     filter.DecisionAllow,
		},
		{
			name:     "patterns-exact-miss",
			patterns: []string{"requests"},
			ref:      filter.Ref{Package: "urllib3"},
			want:     filter.DecisionDeny,
		},
		{
			name:     "patterns-glob-suffix",
			patterns: []string{"@myorg/*"},
			ref:      filter.Ref{Package: "@myorg/sdk"},
			want:     filter.DecisionAllow,
		},
		{
			// path.Match's `*` does not span `/`, so `@myorg/*` matches
			// `@myorg/sdk` but not `@myorg/sub/sdk`. That's the intuitive
			// single-segment match for npm scoped packages.
			name:     "patterns-glob-does-not-cross-slash",
			patterns: []string{"@myorg/*"},
			ref:      filter.Ref{Package: "@myorg/sub/sdk"},
			want:     filter.DecisionDeny,
		},
		{
			name:     "patterns-any-of-multiple",
			patterns: []string{"foo", "bar", "baz*"},
			ref:      filter.Ref{Package: "baz-extras"},
			want:     filter.DecisionAllow,
		},
		{
			name:  "rule-package-only-matches",
			rules: []filter.Rule{{Package: "requests"}},
			ref:   filter.Ref{Package: "requests", Version: "2.31.0"},
			want:  filter.DecisionAllow,
		},
		{
			name:  "rule-package-and-version-both-match",
			rules: []filter.Rule{{Package: "requests", Version: "2.31.*"}},
			ref:   filter.Ref{Package: "requests", Version: "2.31.0"},
			want:  filter.DecisionAllow,
		},
		{
			name:  "rule-version-mismatch-denies",
			rules: []filter.Rule{{Package: "requests", Version: "2.31.*"}},
			ref:   filter.Ref{Package: "requests", Version: "2.30.5"},
			want:  filter.DecisionDeny,
		},
		{
			// Index-time (no version on the ref): the rule's version
			// constraint is ignored and we fall back to package match,
			// so the allowlist lets the index through. File-level
			// requests are still subject to the version constraint
			// (see "rule-version-mismatch-denies" above).
			name:  "rule-empty-ref-version-treats-version-as-wildcard",
			rules: []filter.Rule{{Package: "requests", Version: "2.31.*"}},
			ref:   filter.Ref{Package: "requests"},
			want:  filter.DecisionAllow,
		},
		{
			name:  "rule-package-glob-and-version",
			rules: []filter.Rule{{Package: "@myorg/*", Version: "1.*"}},
			ref:   filter.Ref{Package: "@myorg/sdk", Version: "1.4.0"},
			want:  filter.DecisionAllow,
		},
		{
			name:     "patterns-and-rules-or",
			patterns: []string{"requests"},
			rules:    []filter.Rule{{Package: "log4j-core", Version: "2.17.*"}},
			ref:      filter.Ref{Package: "log4j-core", Version: "2.17.1"},
			want:     filter.DecisionAllow,
		},
		{
			name: "rule-version-only-matches-any-package-at-version",
			// Edge case: a rule with only Version set matches any
			// package at that version. Rarely useful but a defined
			// semantic — the operator can express "only allow 1.0.0
			// across the board" with one rule.
			rules: []filter.Rule{{Version: "1.0.0"}},
			ref:   filter.Ref{Package: "anything", Version: "1.0.0"},
			want:  filter.DecisionAllow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &filter.Allowlist{Patterns: tc.patterns, Rules: tc.rules}
			got, err := a.Allow(t.Context(), tc.ref)
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tc.want {
				t.Errorf("Allow(%+v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func TestAllowlist_Kind(t *testing.T) {
	t.Parallel()
	a := &filter.Allowlist{Patterns: []string{"foo"}}
	if got := a.Kind(); got != filter.KindAllowlist {
		t.Errorf("Kind() = %q, want %q", got, filter.KindAllowlist)
	}
}

// TestAllowlist_Construction pins the validation rules surfaced when
// an Allowlist is parsed from JSON. Constructors don't exist by
// design — the struct is exported so tests can build values
// directly; validation happens at UnmarshalFilter time.
func TestAllowlist_Construction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "valid-exact",
			body: `{"kind":"allow","patterns":["foo"]}`,
		},
		{
			name: "valid-glob",
			body: `{"kind":"allow","patterns":["foo*","@scope/*"]}`,
		},
		{
			name: "valid-rules",
			body: `{"kind":"allow","rules":[{"package":"requests","version":"2.31.*"}]}`,
		},
		{
			name: "valid-mixed",
			body: `{"kind":"allow","patterns":["foo"],"rules":[{"package":"bar","version":"1.*"}]}`,
		},
		{
			name:    "empty-everything",
			body:    `{"kind":"allow"}`,
			wantErr: true,
		},
		{
			name:    "empty-patterns-and-rules",
			body:    `{"kind":"allow","patterns":[],"rules":[]}`,
			wantErr: true,
		},
		{
			name:    "malformed-pattern-glob",
			body:    `{"kind":"allow","patterns":["[unbalanced"]}`,
			wantErr: true,
		},
		{
			name:    "rule-with-no-fields",
			body:    `{"kind":"allow","rules":[{}]}`,
			wantErr: true,
		},
		{
			name:    "rule-with-malformed-version-glob",
			body:    `{"kind":"allow","rules":[{"package":"foo","version":"[bad"}]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := filter.UnmarshalFilter([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalFilter(%q) = nil, want error", tc.body)
				}
				if !errors.Is(err, filter.ErrInvalidFilter) {
					t.Errorf("UnmarshalFilter error = %v, want errors.Is ErrInvalidFilter", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalFilter(%q) error: %v", tc.body, err)
			}
		})
	}
}
