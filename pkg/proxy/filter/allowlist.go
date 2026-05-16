package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
)

// KindAllowlist is the JSON discriminator for [Allowlist].
const KindAllowlist = "allowlist"

// Allowlist denies any package whose name does not match at least
// one of Patterns. Patterns are matched against [Ref.Package] in
// order; the first match allows. Each pattern is either an exact
// name or a shell-style glob (`*`, `?`, `[...]`) interpreted by
// [path.Match] — `*` does not cross `/` boundaries, which gives
// scoped npm packages (`@myorg/*`) intuitive single-segment matching.
//
// An Allowlist with zero patterns denies everything; that is a
// useful "off switch" for a proxy namespace but also an easy
// footgun, so [Allowlist.validate] rejects it.
type Allowlist struct {
	// Patterns is the set of allowed package names / globs. Order
	// is preserved for log readability but does not affect the
	// decision — matching is OR across patterns.
	Patterns []string `json:"patterns"`
}

// Kind returns [KindAllowlist].
func (a *Allowlist) Kind() string { return KindAllowlist }

// Allow returns [DecisionAllow] iff [Ref.Package] matches at least
// one pattern; otherwise [DecisionDeny].
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
	return DecisionDeny, nil
}

// MarshalJSON emits the kind discriminator alongside the pattern
// list so [Filters] can roundtrip a heterogeneous chain.
func (a *Allowlist) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind     string   `json:"kind"`
		Patterns []string `json:"patterns"`
	}{KindAllowlist, a.Patterns})
}

// validate runs at construction time (in [UnmarshalFilter]) so an
// invalid pattern is rejected on PUT, not at first request.
func (a *Allowlist) validate() error {
	if len(a.Patterns) == 0 {
		return fmt.Errorf("%w: allowlist must have at least one pattern", ErrInvalidFilter)
	}
	for _, p := range a.Patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%w: allowlist pattern %q: %v", ErrInvalidFilter, p, err)
		}
	}
	return nil
}
