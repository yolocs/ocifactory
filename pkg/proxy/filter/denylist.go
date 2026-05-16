package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
)

// KindDenylist is the JSON discriminator for [Denylist].
const KindDenylist = "denylist"

// Denylist denies any package whose name matches one of Patterns.
// Patterns are matched against [Ref.Package] in order; the first
// match denies. Pattern semantics are identical to [Allowlist] (exact
// or shell-style glob via [path.Match]).
//
// An empty Denylist allows everything; it is valid but typically
// indicates a misconfiguration, so the spec layer surfaces it
// visually rather than rejecting it.
type Denylist struct {
	// Patterns is the set of denied package names / globs.
	Patterns []string `json:"patterns"`
}

// Kind returns [KindDenylist].
func (d *Denylist) Kind() string { return KindDenylist }

// Allow returns [DecisionDeny] iff [Ref.Package] matches at least
// one pattern; otherwise [DecisionAllow].
func (d *Denylist) Allow(ctx context.Context, ref Ref) (Decision, error) {
	for _, p := range d.Patterns {
		ok, err := matchPattern(p, ref.Package)
		if err != nil {
			return DecisionDeny, fmt.Errorf("denylist: pattern %q: %w", p, err)
		}
		if ok {
			return DecisionDeny, nil
		}
	}
	return DecisionAllow, nil
}

// MarshalJSON emits the kind discriminator alongside the pattern
// list so [Filters] can roundtrip a heterogeneous chain.
func (d *Denylist) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind     string   `json:"kind"`
		Patterns []string `json:"patterns"`
	}{KindDenylist, d.Patterns})
}

// validate runs at construction time (in [UnmarshalFilter]).
func (d *Denylist) validate() error {
	if len(d.Patterns) == 0 {
		return fmt.Errorf("%w: denylist must have at least one pattern", ErrInvalidFilter)
	}
	for _, p := range d.Patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%w: denylist pattern %q: %v", ErrInvalidFilter, p, err)
		}
	}
	return nil
}

// matchPattern is the shared exact-or-glob match used by Allowlist
// and Denylist. Exact match wins as a fast path so a literal
// pattern doesn't depend on [path.Match]'s glob interpretation
// (which would surprise nobody for normal names but means a literal
// "*" in a name is hard to express — operators who actually need a
// literal glob char can escape it via [path.Match]'s `\`).
func matchPattern(pattern, name string) (bool, error) {
	if pattern == name {
		return true, nil
	}
	return path.Match(pattern, name)
}
