package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
)

// KindDenylist is the JSON discriminator for [Denylist].
const KindDenylist = "denylist"

// Denylist denies any [Ref] that matches at least one entry. Two
// parallel input shapes are supported and ORed together:
//
//   - Patterns: terse list of package-name globs (the common case).
//     Each pattern is the equivalent of a [Rule] with only Package set.
//   - Rules: full (package, version) entries. Each rule's version
//     constraint is ignored when [Ref.Version] is empty, so a
//     denylist of `{package:log4j-core, version:2.14.*}` denies the
//     log4j-core index AND the specific 2.14.x files; 2.17.x file
//     requests are still allowed.
//
// An empty Denylist allows everything; it is valid but typically
// indicates a misconfiguration, so the spec layer surfaces it
// visually rather than rejecting it. (A no-entries Denylist is
// caught by validate as a misconfig — operators who actually want
// "no denies" simply omit the filter.)
type Denylist struct {
	Patterns []string `json:"patterns,omitempty"`
	Rules    []Rule   `json:"rules,omitempty"`
}

// Kind returns [KindDenylist].
func (d *Denylist) Kind() string { return KindDenylist }

// Allow returns [DecisionDeny] iff ref matches at least one entry;
// otherwise [DecisionAllow].
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
	for i, r := range d.Rules {
		ok, err := r.matches(ref)
		if err != nil {
			return DecisionDeny, fmt.Errorf("denylist: rules[%d]: %w", i, err)
		}
		if ok {
			return DecisionDeny, nil
		}
	}
	return DecisionAllow, nil
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
