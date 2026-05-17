package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
)

// KindDenylist is the JSON discriminator for [Denylist].
const KindDenylist = "deny"

// Denylist returns [DecisionDeny] for any [Ref] that matches at least
// one entry, and [DecisionAbstain] otherwise — it is an explicit deny
// override that short-circuits the chain. It never allows on its own.
//
// Two parallel input shapes are supported and ORed together:
//
//   - Patterns: terse list of package-name globs (the common case).
//     Each pattern is the equivalent of a [Rule] with only Package set.
//   - Rules: full (package, version) entries.
//
// An empty Denylist would abstain on every ref, which is useless, so
// [Denylist.validate] rejects it at construction.
type Denylist struct {
	Patterns []string `json:"patterns,omitempty"`
	Rules    []Rule   `json:"rules,omitempty"`
}

// Kind returns [KindDenylist].
func (d *Denylist) Kind() string { return KindDenylist }

// Decide returns [DecisionDeny] when ref matches an entry, otherwise
// [DecisionAbstain].
func (d *Denylist) Decide(ctx context.Context, ref Ref) (Decision, error) {
	for _, p := range d.Patterns {
		ok, err := matchPattern(p, ref.Package)
		if err != nil {
			return DecisionDeny, fmt.Errorf("denylist: pattern %q: %w", p, err)
		}
		if ok {
			return DecisionDeny, nil
		}
	}
	for i, r := range d.Rules {
		ok, err := r.matches(ref)
		if err != nil {
			return DecisionDeny, fmt.Errorf("denylist: rules[%d]: %w", i, err)
		}
		if ok {
			return DecisionDeny, nil
		}
	}
	return DecisionAbstain, nil
}

// MarshalJSON emits the kind discriminator alongside the entries so
// [Filters] can roundtrip a heterogeneous chain.
func (d *Denylist) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind     string   `json:"kind"`
		Patterns []string `json:"patterns,omitempty"`
		Rules    []Rule   `json:"rules,omitempty"`
	}{KindDenylist, d.Patterns, d.Rules})
}

// validate runs at construction time (in [UnmarshalFilter]).
func (d *Denylist) validate() error {
	if len(d.Patterns) == 0 && len(d.Rules) == 0 {
		return fmt.Errorf("%w: denylist must have at least one pattern or rule", ErrInvalidFilter)
	}
	for _, p := range d.Patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%w: denylist pattern %q: %v", ErrInvalidFilter, p, err)
		}
	}
	for i, r := range d.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("denylist rules[%d]: %w", i, err)
		}
	}
	return nil
}
