package namespace

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// NewPolicyAuthorizer compiles p into an [auth.Authorizer] bound to
// a single namespace's [Policy]. Regex compilation happens once
// here so [auth.Authorizer.Authorize] is allocation-light on the
// hot path. The wrapper in #70 will hold one of these per cached
// namespace.
//
// An error is returned iff p does not pass [Policy.Validate].
func NewPolicyAuthorizer(p Policy) (auth.Authorizer, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	pa := &policyAuthorizer{}
	for _, m := range p.Readers {
		cm, err := compileMatcher(m)
		if err != nil {
			return nil, err
		}
		pa.readers = append(pa.readers, cm)
	}
	for _, m := range p.Writers {
		cm, err := compileMatcher(m)
		if err != nil {
			return nil, err
		}
		pa.writers = append(pa.writers, cm)
	}
	return pa, nil
}

// policyAuthorizer is the v1 matcher-based [auth.Authorizer]
// implementation. The compiled regex set lives here, not on the
// [Policy] struct, so the JSON shape stays a plain DTO.
type policyAuthorizer struct {
	readers []compiledMatcher
	writers []compiledMatcher
}

// Authorize iterates the matcher list for op and returns nil on the
// first match. An empty list, or no match across all matchers,
// returns an error wrapping [auth.ErrUnauthorized]. A nil ac (no
// authenticated caller) is always denied.
func (a *policyAuthorizer) Authorize(_ context.Context, ac *auth.AuthContext, op auth.Op) error {
	if ac == nil {
		return fmt.Errorf("no authenticated subject for %s: %w", op, auth.ErrUnauthorized)
	}
	var matchers []compiledMatcher
	switch op {
	case auth.OpRead:
		matchers = a.readers
	case auth.OpWrite:
		matchers = a.writers
	default:
		return fmt.Errorf("unknown op %q for %s: %w", op, ac, auth.ErrUnauthorized)
	}
	for _, m := range matchers {
		if m.matches(ac) {
			return nil
		}
	}
	return fmt.Errorf("subject %s denied for %s: %w", ac, op, auth.ErrUnauthorized)
}

type compiledMatcher struct {
	issuer   string
	subRE    *regexp.Regexp
	email    string
	claimsRE map[string]*regexp.Regexp
	kind     string
}

func compileMatcher(m SubjectMatcher) (compiledMatcher, error) {
	out := compiledMatcher{
		issuer: m.Issuer,
		email:  m.Email,
		kind:   m.Kind,
	}
	if out.kind == "" {
		out.kind = KindOIDC
	}
	if m.SubMatch != "" {
		re, err := regexp.Compile(anchorRegex(m.SubMatch))
		if err != nil {
			return compiledMatcher{}, fmt.Errorf("%w: sub_match %q: %v", ErrInvalidPolicy, m.SubMatch, err)
		}
		out.subRE = re
	}
	if len(m.ClaimsMatch) > 0 {
		out.claimsRE = make(map[string]*regexp.Regexp, len(m.ClaimsMatch))
		for claim, pat := range m.ClaimsMatch {
			re, err := regexp.Compile(anchorRegex(pat))
			if err != nil {
				return compiledMatcher{}, fmt.Errorf("%w: claims_match[%q] %q: %v", ErrInvalidPolicy, claim, pat, err)
			}
			out.claimsRE[claim] = re
		}
	}
	return out, nil
}

func (m compiledMatcher) matches(ac *auth.AuthContext) bool {
	// kind: every in-tree authenticator today produces an OIDC-shaped
	// AuthContext, so we check kind only against the matcher's
	// declared family. Other kinds are rejected at Validate time.
	if m.kind != KindOIDC {
		return false
	}
	if m.issuer != "" && m.issuer != ac.Issuer {
		return false
	}
	if m.email != "" && m.email != ac.Email {
		return false
	}
	if m.subRE != nil && !m.subRE.MatchString(ac.ID) {
		return false
	}
	for claim, re := range m.claimsRE {
		v, ok := ac.Claims[claim]
		if !ok {
			return false
		}
		s, err := claimValueString(v)
		if err != nil {
			return false
		}
		if !re.MatchString(s) {
			return false
		}
	}
	return true
}

// claimValueString renders a claim value as the canonical string
// used for regex matching. Strings pass through verbatim; everything
// else is JSON-encoded with sorted object keys (Go's encoding/json
// already sorts map keys when marshalling). Operators writing rules
// against array or object claims see the JSON form; this is
// documented as a v1 surprise on the [SubjectMatcher.ClaimsMatch]
// field.
func claimValueString(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
