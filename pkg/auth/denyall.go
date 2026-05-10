package auth

import (
	"context"
	"fmt"
)

// DenyAll is an [Authorizer] that rejects every request with an
// error wrapping [ErrUnauthorized]. It exists for tests and for
// fallback paths where the operator wants an unmistakable deny
// rather than a missing-policy ambiguity.
var DenyAll Authorizer = denyAllAuthorizer{}

type denyAllAuthorizer struct{}

func (denyAllAuthorizer) Authorize(_ context.Context, ac *AuthContext, op Op) error {
	return fmt.Errorf("deny-all authorizer rejected %s for %s: %w", op, ac, ErrUnauthorized)
}
