// Package chain composes multiple Authenticators into one. Each
// child is tried in order; the first non-ErrNoCredential result
// wins.
//
// Lets operators stack [oidc(google), oidc(github), basictoken] —
// the OIDC verifiers see Bearer / sentinel-Basic, the basictoken
// implementation sees regular Basic.
package chain

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// Chain is an Authenticator that delegates to children in order.
// The zero value is a no-credential authenticator (always returns
// ErrNoCredential).
type Chain struct {
	children []auth.Authenticator
}

// New returns a Chain that tries each child in order. nil children
// are filtered out so callers can build the slice with conditionals
// without special-casing the absent case.
func New(children ...auth.Authenticator) *Chain {
	out := &Chain{}
	for _, c := range children {
		if c != nil {
			out.children = append(out.children, c)
		}
	}
	return out
}

// Authenticate runs each child's Authenticate in sequence:
//
//   - first child to return (subject, nil) wins.
//   - children returning ErrNoCredential are skipped — the next
//     child gets a chance.
//   - any other error short-circuits and is returned to the
//     caller. ErrInvalidToken from one child is NOT silently
//     overridden by ErrNoCredential from a later child — once a
//     child has rejected a credential, that's the answer.
//
// When every child returns ErrNoCredential the chain returns
// ErrNoCredential too. An empty chain behaves the same.
func (c *Chain) Authenticate(r *http.Request) (*auth.Subject, error) {
	for i, child := range c.children {
		subj, err := child.Authenticate(r)
		if err == nil {
			return subj, nil
		}
		if errors.Is(err, auth.ErrNoCredential) {
			continue
		}
		return nil, fmt.Errorf("authenticator[%d]: %w", i, err)
	}
	return nil, auth.ErrNoCredential
}
