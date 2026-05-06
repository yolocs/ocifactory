// Package auth defines the pluggable authentication surface for
// ocifactory. Authenticators inspect an inbound *http.Request and
// either return an *AuthContext (the verified caller identity) or one
// of the sentinel errors so callers can map the failure mode to an
// HTTP status. The built-in OIDC implementation lives in the oidc
// sub-package; the chain composer lives in chain. Out-of-tree
// authenticators (static passwords, GitHub PAT, mTLS, ...) plug in
// by implementing Authenticator and being wired up in a custom main.
package auth

import (
	"context"
	"errors"
	"net/http"
)

// Authenticator inspects a request and either resolves the caller
// to a AuthContext or returns one of the sentinel errors so the
// middleware can map the failure to a status code.
//
// Implementations MUST return ErrNoCredential when the request
// carries no credential the implementation knows how to handle —
// this is what lets Chain fall through to the next authenticator.
// Hard errors (invalid token, unreachable JWKS) MUST be returned as
// distinct error values so the middleware can return 401 vs 503.
type Authenticator interface {
	Authenticate(r *http.Request) (*AuthContext, error)
}

// AuthContext is the verified caller identity. It is what the
// authorizer (separate issue) consumes. Authenticators populate as
// many fields as they can verify; consumers should treat absent
// fields as "not asserted" rather than "asserted empty".
type AuthContext struct {
	// Issuer identifies which authenticator vouched for the
	// caller. For OIDC this is the iss claim
	// ("https://accounts.google.com",
	// "https://token.actions.githubusercontent.com"). Other
	// authenticators set their own stable string so authorization
	// policy can distinguish callers from different sources.
	Issuer string

	// ID is the stable subject identifier inside Issuer. For OIDC
	// this is the sub claim. Other authenticators set whatever
	// stable per-caller identifier they have.
	ID string

	// Email is the verified email address, when the issuer
	// asserts one. Empty when no email claim is present or
	// email_verified is false.
	Email string

	// Claims is the raw verified claim set. For OIDC tokens it is
	// the decoded JWT payload after signature, issuer, audience
	// and lifetime checks have all passed. Non-JWT authenticators
	// may leave it nil or fill it with implementation-specific
	// metadata.
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
type AuthenticatorFunc func(r *http.Request) (*AuthContext, error)

// Authenticate calls f(r).
func (f AuthenticatorFunc) Authenticate(r *http.Request) (*AuthContext, error) {
	return f(r)
}

// AlwaysAnonymous is the dev-only authenticator wired up when the
// operator runs with --disable-authn (OCIFACTORY_AUTHN_DISABLED=true).
// It returns an AuthContext with Issuer="anonymous" and a fixed ID.
// Production deployments must NOT use this — the serve command logs
// a loud warning when it's in effect.
var AlwaysAnonymous Authenticator = AuthenticatorFunc(func(_ *http.Request) (*AuthContext, error) {
	return &AuthContext{Issuer: "anonymous", ID: "anonymous"}, nil
})

// contextKey is unexported to prevent collisions with other
// packages' context values.
type contextKey string

const authContextKey contextKey = "authcontext"

// WithAuthContext stores ac on ctx so downstream handlers can read the
// authenticated identity via FromContext.
func WithAuthContext(ctx context.Context, ac *AuthContext) context.Context {
	return context.WithValue(ctx, authContextKey, ac)
}

// FromContext returns the AuthContext installed by the
// middleware, if any. The boolean is false when no AuthContext is
// present.
func FromContext(ctx context.Context) (*AuthContext, bool) {
	ac, ok := ctx.Value(authContextKey).(*AuthContext)
	return ac, ok
}
