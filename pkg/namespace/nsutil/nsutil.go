// Package nsutil contains small helpers for tests and integration harnesses
// that need to seed namespace metadata through the production Store API.
package nsutil

import (
	"context"
	"testing"

	"github.com/yolocs/ocifactory/pkg/namespace"
)

// Seed writes ns to store and fails t on error.
func Seed(t testing.TB, ctx context.Context, store *namespace.Store, ns *namespace.Namespace) {
	t.Helper()
	if err := store.Put(ctx, ns); err != nil {
		t.Fatalf("seed namespace %q: %v", ns.Name, err)
	}
}

// AllowIssuerSpec returns a namespace spec that grants both read and write to
// OIDC subjects from issuer.
func AllowIssuerSpec(issuer string) namespace.Spec {
	matcher := namespace.SubjectMatcher{Issuer: issuer}
	return namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{matcher},
		Writers: []namespace.SubjectMatcher{matcher},
	}}
}
