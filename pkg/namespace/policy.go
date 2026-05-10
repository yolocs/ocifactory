package namespace

import (
	"errors"
	"fmt"
	"regexp"
)

// KindOIDC is the default value for [SubjectMatcher.Kind] — an
// OIDC-authenticated caller. It is the only kind supported in v1;
// see [KindBasicToken].
const KindOIDC = "oidc"

// KindBasicToken is reserved for future static-token authenticators
// but is rejected by [SubjectMatcher.Validate] in v1, since no
// in-tree authenticator produces a basictoken [auth.AuthContext]
// today. Operators writing rules that target it would silently never
// match; rejecting at validation time avoids that footgun.
const KindBasicToken = "basictoken"

// ErrInvalidPolicy is the sentinel for [Policy.Validate] failures.
// Specific causes (empty matcher, regex syntax, unsupported kind)
// wrap it via fmt.Errorf("...: %w", ErrInvalidPolicy).
var ErrInvalidPolicy = errors.New("invalid policy")

// Policy is the authz block of a [Spec]. An empty Policy is
// deny-all: a caller is allowed to perform an op only if at least
// one [SubjectMatcher] in the corresponding list matches them. The
// Readers and Writers lists are independent; granting write does
// not implicitly grant read.
type Policy struct {
	Readers []SubjectMatcher `json:"readers,omitempty"`
	Writers []SubjectMatcher `json:"writers,omitempty"`
}

// SubjectMatcher matches an authenticated subject. A subject
// matches iff every populated field on the matcher matches the
// corresponding field on the subject (fields are ANDed). An empty
// matcher (no fields populated) is invalid and rejected by
// [Policy.Validate].
type SubjectMatcher struct {
	// Issuer matches [auth.AuthContext.Issuer] exactly. Required
	// for "oidc" matchers in practice — an OIDC sub is only
	// meaningful in the context of its issuer.
	Issuer string `json:"issuer,omitempty"`

	// SubMatch is an RE2 regex matched against
	// [auth.AuthContext.ID]. The pattern is anchored at both ends
	// (^pattern$) so callers don't accidentally allow
	// "^repo:org/repo$" matching "evil-repo:org/repo:suffix".
	SubMatch string `json:"sub_match,omitempty"`

	// Email matches [auth.AuthContext.Email] exactly.
	Email string `json:"email,omitempty"`

	// ClaimsMatch maps a claim name to an RE2 regex that the
	// claim's value must match. Each claim's value is JSON-encoded
	// (with sorted keys for objects) before matching, so non-string
	// values match against their JSON representation. Patterns are
	// anchored at both ends.
	ClaimsMatch map[string]string `json:"claims_match,omitempty"`

	// Kind selects the credential family. "" defaults to "oidc".
	// "basictoken" is reserved but rejected at validation in v1
	// (see [KindBasicToken]).
	Kind string `json:"kind,omitempty"`
}

// Validate returns nil iff every [SubjectMatcher] in p is valid:
// non-empty, regex fields compile under RE2, and Kind is a
// supported value. Errors wrap [ErrInvalidPolicy] and identify the
// offending list ("readers"/"writers") and index.
//
// An entirely empty Policy is valid and means deny-all.
func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	for i := range p.Readers {
		if err := p.Readers[i].Validate(); err != nil {
			return fmt.Errorf("readers[%d]: %w", i, err)
		}
	}
	for i := range p.Writers {
		if err := p.Writers[i].Validate(); err != nil {
			return fmt.Errorf("writers[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate returns nil iff m has at least one populated field, all
// regex fields compile under RE2, and Kind is supported. Errors
// wrap [ErrInvalidPolicy].
func (m *SubjectMatcher) Validate() error {
	if m.Issuer == "" && m.SubMatch == "" && m.Email == "" && len(m.ClaimsMatch) == 0 && m.Kind == "" {
		return fmt.Errorf("%w: matcher must populate at least one field", ErrInvalidPolicy)
	}

	switch m.Kind {
	case "", KindOIDC:
		// supported.
	case KindBasicToken:
		return fmt.Errorf("%w: kind %q is reserved but not yet supported", ErrInvalidPolicy, KindBasicToken)
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidPolicy, m.Kind)
	}

	if m.SubMatch != "" {
		if _, err := regexp.Compile(anchorRegex(m.SubMatch)); err != nil {
			return fmt.Errorf("%w: sub_match %q: %v", ErrInvalidPolicy, m.SubMatch, err)
		}
	}

	for claim, pat := range m.ClaimsMatch {
		if claim == "" {
			return fmt.Errorf("%w: claims_match key must not be empty", ErrInvalidPolicy)
		}
		if _, err := regexp.Compile(anchorRegex(pat)); err != nil {
			return fmt.Errorf("%w: claims_match[%q] %q: %v", ErrInvalidPolicy, claim, pat, err)
		}
	}

	return nil
}

// anchorRegex wraps pat in ^...$ so matches are full-string. If pat
// already begins with ^ or ends with $ the corresponding anchor is
// not duplicated; this keeps operator-written rules predictable
// regardless of whether they remembered to anchor.
func anchorRegex(pat string) string {
	prefix := "^"
	if len(pat) > 0 && pat[0] == '^' {
		prefix = ""
	}
	suffix := "$"
	if len(pat) > 0 && pat[len(pat)-1] == '$' {
		suffix = ""
	}
	return prefix + pat + suffix
}
