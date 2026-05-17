package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
)

// KindAllowlist is the JSON discriminator for [Allowlist].
const KindAllowlist = "allow"

// Allowlist returns [DecisionAllow] for any [Ref] that matches at
// least one entry, and [DecisionAbstain] otherwise — it is an
// explicit allow override that lets a request bypass any later filter
// in the chain (notably [Delay]). It never denies on its own.
//
// Two parallel input shapes are supported and ORed together:
//
//   - Patterns: terse list of package-name globs (the common case).
//     Each pattern is the equivalent of a [Rule] with only Package set.
//   - Rules: full (package, version) entries.
//
// An empty Allowlist would abstain on every ref, which is useless,
// so [Allowlist.validate] rejects it at construction.
type Allowlist struct {
	Patterns []string `json:"patterns,omitempty"`
	Rules    []Rule   `json:"rules,omitempty"`
}

// Kind returns [KindAllowlist].
func (a *Allowlist) Kind() string { return KindAllowlist }

// Decide returns [DecisionAllow] when ref matches an entry, otherwise
// [DecisionAbstain].
func (a *Allowlist) Decide(ctx context.Context, ref Ref) (Decision, error) {
	for _, p := range a.Patterns {
		ok, err := matchPattern(p, ref.Package)
		if err != nil {
			return DecisionDeny, fmt.Errorf("allowlist: pattern %q: %w", p, err)
		}
		if ok {
			return DecisionAllow, nil
		}
	}
	for i, r := range a.Rules {
		ok, err := r.matches(ref)
		if err != nil {
			return DecisionDeny, fmt.Errorf("allowlist: rules[%d]: %w", i, err)
		}
		if ok {
			return DecisionAllow, nil
		}
	}
	return DecisionAbstain, nil
}

// MarshalJSON emits the kind discriminator alongside the entries so
// [Filters] can roundtrip a heterogeneous chain.
func (a *Allowlist) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind     string   `json:"kind"`
		Patterns []string `json:"patterns,omitempty"`
		Rules    []Rule   `json:"rules,omitempty"`
	}{KindAllowlist, a.Patterns, a.Rules})
}

// validate runs at construction time (in [UnmarshalFilter]) so an
// invalid pattern is rejected on PUT, not at first request.
func (a *Allowlist) validate() error {
	if len(a.Patterns) == 0 && len(a.Rules) == 0 {
		return fmt.Errorf("%w: allowlist must have at least one pattern or rule", ErrInvalidFilter)
	}
	for _, p := range a.Patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%w: allowlist pattern %q: %v", ErrInvalidFilter, p, err)
		}
	}
	for i, r := range a.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("allowlist rules[%d]: %w", i, err)
		}
	}
	return nil
}
