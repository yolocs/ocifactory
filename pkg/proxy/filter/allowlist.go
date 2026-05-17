package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
)

// KindAllowlist is the JSON discriminator for [Allowlist].
const KindAllowlist = "allowlist"

// Allowlist denies any [Ref] that doesn't match at least one entry.
// Two parallel input shapes are supported and ORed together:
//
//   - Patterns: terse list of package-name globs (the common case).
//     Each pattern is the equivalent of a [Rule] with only Package set.
//   - Rules: full (package, version) entries. Each rule's version
//     constraint is ignored when [Ref.Version] is empty, so an
//     allowlist of `{package:requests, version:2.31.*}` allows the
//     requests index through and only restricts per-version file
//     requests.
//
// An Allowlist with no entries denies everything; that's a useful
// "off switch" for a proxy namespace but also an easy footgun, so
// [Allowlist.validate] rejects it.
type Allowlist struct {
	Patterns []string `json:"patterns,omitempty"`
	Rules    []Rule   `json:"rules,omitempty"`
}

// Kind returns [KindAllowlist].
func (a *Allowlist) Kind() string { return KindAllowlist }

// Allow returns [DecisionAllow] iff ref matches at least one entry;
// otherwise [DecisionDeny].
func (a *Allowlist) Allow(ctx context.Context, ref Ref) (Decision, error) {
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
	return DecisionDeny, nil
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
