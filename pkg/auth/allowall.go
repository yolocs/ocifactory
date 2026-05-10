package auth

import "context"

// AllowAll is an [Authorizer] that permits every request, regardless
// of the [AuthContext] or [Op]. It is intended for tests and for the
// `--default-namespace-allow-all` operator escape hatch in the data
// plane. Production deployments must not wire this in directly.
var AllowAll Authorizer = allowAllAuthorizer{}

type allowAllAuthorizer struct{}

func (allowAllAuthorizer) Authorize(_ context.Context, _ *AuthContext, _ Op) error {
	return nil
}
