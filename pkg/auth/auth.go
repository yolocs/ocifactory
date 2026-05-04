// Package auth defines the pluggable authentication surface for
// ocifactory. Authenticators inspect an inbound *http.Request and
// either return a *Subject (the verified caller identity) or one of
// the sentinel errors so callers can map the failure mode to an HTTP
// status. Concrete implementations live in sub-packages: chain, oidc,
// basictoken.
package auth

import (
	"context"
	"errors"
	"net/http"
)

// Authenticator inspects a request and either resolves the caller
// to a Subject or returns one of the sentinel errors so the
// middleware can map the failure to a status code.
//
// Implementations MUST return ErrNoCredential when the request
// carries no credential the implementation knows how to handle —
// this is what lets Chain fall through to the next authenticator.
// Hard errors (invalid token, unreachable JWKS) MUST be returned as
// distinct error values so the middleware can return 401 vs 503.
type Authenticator interface {
	Authenticate(r *http.Request) (*Subject, error)
}

// Subject is the verified caller identity. It is what the
// authorizer (separate issue) consumes. Authenticators populate as
// many fields as they can verify; consumers should treat absent
// fields as "not asserted" rather than "asserted empty".
type Subject struct {
	// Issuer identifies which authenticator vouched for the
	// caller. For OIDC this is the iss claim
	// ("https://accounts.google.com",
	// "https://token.actions.githubusercontent.com"). For
	// basictoken it is the literal "basictoken" so authorization
	// policy can distinguish.
	Issuer string

	// ID is the stable subject identifier inside Issuer. For OIDC
	// this is the sub claim. For basictoken it is the username.
	ID string

	// Email is the verified email address, when the issuer
	// asserts one. Empty when no email claim is present or
	// email_verified is false.
	Email string

	// Claims is the raw verified claim set. For OIDC tokens it is
	// the decoded JWT payload after signature, issuer, audience
	// and lifetime checks have all passed. For non-JWT
	// authenticators (basictoken) Claims may be nil or carry only
	// authenticator-specific metadata.
	//
	// Authorization policy reads Claims for fine-grained checks
	// (GitHub Actions repo, Google email_verified, etc.).
	Claims map[string]any
}

// Sentinel errors. Authenticator implementations MUST return one of
// these (or wrap one with %w) so middleware can choose the right
// HTTP status.
var (
	// ErrNoCredential signals "this authenticator did not find a
	// credential it knows how to handle in this request". Chain
	// uses this to fall through to the next authenticator. The
	// middleware maps a top-level ErrNoCredential to 401.
	ErrNoCredential = errors.New("no credential present")

	// ErrInvalidToken signals "this authenticator found a
	// credential but it was not valid" (bad signature, wrong
	// issuer, expired, wrong audience, unknown user, hash
	// mismatch). Maps to 401.
	ErrInvalidToken = errors.New("invalid token")

	// ErrIssuerUnavailable signals a transient backend failure
	// the operator can fix (JWKS unreachable, discovery
	// document unfetched). Maps to 503 so callers know to
	// retry rather than re-issue credentials.
	ErrIssuerUnavailable = errors.New("issuer unavailable")
)

// AuthenticatorFunc adapts a plain function to the Authenticator
// interface. Useful in tests and for one-off authenticators that
// don't carry state.
type AuthenticatorFunc func(r *http.Request) (*Subject, error)

// Authenticate calls f(r).
func (f AuthenticatorFunc) Authenticate(r *http.Request) (*Subject, error) {
	return f(r)
}

// AlwaysAnonymous is the dev-only authenticator wired up when the
// operator runs with --auth=none or omits --auth-config. It returns
// a Subject with Issuer="anonymous" and a fixed ID. Production
// deployments must NOT use this — the serve command logs a loud
// warning when it's in effect.
var AlwaysAnonymous Authenticator = AuthenticatorFunc(func(_ *http.Request) (*Subject, error) {
	return &Subject{Issuer: "anonymous", ID: "anonymous"}, nil
})

// contextKey is unexported to prevent collisions with other
// packages' context values.
type contextKey string

const subjectKey contextKey = "subject"

// WithSubject stores s on ctx so downstream handlers can read the
// authenticated identity via SubjectFromContext.
func WithSubject(ctx context.Context, s *Subject) context.Context {
	return context.WithValue(ctx, subjectKey, s)
}

// SubjectFromContext returns the Subject installed by the
// middleware, if any. The boolean is false when no subject is
// present.
func SubjectFromContext(ctx context.Context) (*Subject, bool) {
	s, ok := ctx.Value(subjectKey).(*Subject)
	return s, ok
}
