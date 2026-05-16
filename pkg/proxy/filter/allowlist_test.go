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
		pkg      string
		want     filter.Decision
	}{
		{
			name:     "exact-match",
			patterns: []string{"requests"},
			pkg:      "requests",
			want:     filter.DecisionAllow,
		},
		{
			name:     "exact-miss",
			patterns: []string{"requests"},
			pkg:      "urllib3",
			want:     filter.DecisionDeny,
		},
		{
			name:     "glob-suffix",
			patterns: []string{"@myorg/*"},
			pkg:      "@myorg/sdk",
			want:     filter.DecisionAllow,
		},
		{
			name: "glob-does-not-cross-slash",
			// path.Match's `*` does not span `/`, so `@myorg/*` matches
			// `@myorg/sdk` but not `@myorg/sub/sdk`. That's the intuitive
			// single-segment match for npm scoped packages.
			patterns: []string{"@myorg/*"},
			pkg:      "@myorg/sub/sdk",
			want:     filter.DecisionDeny,
		},
		{
			name:     "any-of-multiple",
			patterns: []string{"foo", "bar", "baz*"},
			pkg:      "baz-extras",
			want:     filter.DecisionAllow,
		},
		{
			name:     "no-pattern-matches-empty-name",
			patterns: []string{"requests"},
			pkg:      "",
			want:     filter.DecisionDeny,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &filter.Allowlist{Patterns: tc.patterns}
			got, err := a.Allow(t.Context(), filter.Ref{Package: tc.pkg})
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tc.want {
				t.Errorf("Allow(%q) = %v, want %v", tc.pkg, got, tc.want)
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
			body: `{"kind":"allowlist","patterns":["foo"]}`,
		},
		{
			name: "valid-glob",
			body: `{"kind":"allowlist","patterns":["foo*","@scope/*"]}`,
		},
		{
			name:    "empty-patterns",
			body:    `{"kind":"allowlist","patterns":[]}`,
			wantErr: true,
		},
		{
			name:    "missing-patterns",
			body:    `{"kind":"allowlist"}`,
			wantErr: true,
		},
		{
			name:    "malformed-glob",
			body:    `{"kind":"allowlist","patterns":["[unbalanced"]}`,
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
