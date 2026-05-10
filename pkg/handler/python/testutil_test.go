package python

import (
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
)

// testNS is the namespace every handler test publishes / reads under.
// Tests address requests at /test-ns/... and inspect backend keys
// prefixed with test-ns/. New tests exercising cross-namespace
// isolation pick a different name to make the boundary explicit.
const testNS = "test-ns"

// allowAllPolicy is a [namespace.Policy] that admits the
// anonymous-issuer AuthContext produced by [auth.AlwaysAnonymous]
// (the authenticator every python handler test wires up so the
// data-plane wrapper has a verified subject to authorize). Tests
// exercising specific policy behaviours pass a custom spec to
// [putNamespace].
func allowAllPolicy() namespace.Policy {
	return namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
	}
}

// newTestHandler builds a python [*Handler] wired to a
// [*namespace.Registry] backed by inner. A "test-ns" namespace with an
// allow-all policy is registered; the auth middleware installs the
// [auth.AlwaysAnonymous] AuthContext on every request so the wrapper
// has a subject to authorize. Tests that need a different namespace /
// policy / authenticator construct the plumbing inline.
func newTestHandler(t *testing.T, inner namespace.RegistryBackend, opts ...Option) (*Handler, *namespace.Store) {
	t.Helper()
	store := namespace.NewStore(inner)
	reg := namespace.NewRegistry(inner, store, namespace.WithPolicyCacheTTL(0))
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: namespace.Spec{Policy: allowAllPolicy()}}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}
	authMW := auth.Middleware(auth.AlwaysAnonymous)
	allOpts := append([]Option{WithAuthMiddleware(authMW)}, opts...)
	h, err := NewHandler(reg, allOpts...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, store
}

// putNamespace upserts a namespace via store, t.Fatal-ing on failure.
// Tests that want a custom policy build the [namespace.Spec] and call
// this directly.
func putNamespace(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("Put namespace %q: %v", name, err)
	}
}
