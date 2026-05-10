package auth

import (
	"context"
	"errors"
	"fmt"
)

// Op is the coarse operation an authenticated subject is asking to
// perform on a namespace. v1 ships only OpRead and OpWrite; finer
// granularity is left to operators who plug in their own
// [Authorizer].
type Op string

const (
	// OpRead covers all artifact-fetch traffic on a namespace
	// (pip download, mvn dependency resolution, npm install, etc.).
	OpRead Op = "read"

	// OpWrite covers all artifact-publish traffic on a namespace
	// (twine upload, mvn deploy, npm publish, etc.).
	OpWrite Op = "write"
)

// Authorizer decides whether the authenticated caller in ac may
// perform op. It returns nil when the operation is allowed, or an
// error wrapping [ErrUnauthorized] when it is denied. Other errors
// indicate an operational failure (regex compile, backend lookup)
// and should be treated as 5xx by callers.
//
// Implementations MUST treat a nil ac as deny: an unauthenticated
// request never satisfies a non-empty policy in v1. The auth
// middleware already returns 401 for missing credentials, so a nil
// ac reaching an Authorizer is a defensive case rather than a hot
// path.
type Authorizer interface {
	Authorize(ctx context.Context, ac *AuthContext, op Op) error
}

// AuthorizerFunc adapts a plain function to the [Authorizer]
// interface. Useful in tests and for one-off authorizers that
// don't carry state.
type AuthorizerFunc func(ctx context.Context, ac *AuthContext, op Op) error

// Authorize calls f(ctx, ac, op).
func (f AuthorizerFunc) Authorize(ctx context.Context, ac *AuthContext, op Op) error {
	return f(ctx, ac, op)
}

// ErrUnauthorized is the sentinel for authorization failures. The
// HTTP layer maps an error wrapping ErrUnauthorized to 403 (the
// caller is authenticated but not permitted). Authorizer
// implementations should wrap with %w when signalling deny so the
// caller can use [errors.Is].
var ErrUnauthorized = errors.New("unauthorized")

// String returns a stable per-caller identity suitable for audit
// logs, of the form "<issuer>#<id>". Empty fields are preserved so
// the output is unambiguous when one side is missing
// (e.g. "anonymous#anonymous", "https://accounts.google.com#"). A
// nil receiver renders as the empty string so log lines about
// unauthenticated callers still print cleanly.
func (a *AuthContext) String() string {
	if a == nil {
		return ""
	}
	return fmt.Sprintf("%s#%s", a.Issuer, a.ID)
}
