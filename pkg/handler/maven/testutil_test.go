package maven

import (
	"testing"

	"github.com/yolocs/ocifactory/pkg/artifact"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
)

// testNS is the namespace every maven handler test publishes / reads
// under. Tests address requests at /test-ns/maven2/... and inspect
// backend keys prefixed with test-ns/. Cross-namespace isolation
// tests pick different names to make the boundary explicit.
const testNS = "test-ns"

// allowAllPolicy admits the anonymous-issuer AuthContext produced by
// [auth.AlwaysAnonymous]. Used by the default test wiring; tests
// exercising specific policy behaviours pass a custom spec.
func allowAllPolicy() namespace.Policy {
	return namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
	}
}

// newTestHandler builds a maven [*Handler] wired to a
// [*artifact.Store] backed by inner. The "test-ns" namespace
// receives an allow-all policy and the auth middleware installs the
// [auth.AlwaysAnonymous] AuthContext on every request so the wrapper
// has a subject to authorize.
func newTestHandler(t *testing.T, inner artifact.Backend, opts ...Option) (*Handler, *namespace.Store) {
	t.Helper()
	store := namespace.NewStore(inner)
	reg := artifact.NewStore(inner, store, artifact.WithPolicyCacheTTL(0))
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
func putNamespace(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("Put namespace %q: %v", name, err)
	}
}

// nsPath returns a request path under the test-ns/maven2 subtree for
// the maven coordinate components. Used to keep the /<ns>/maven2/...
// shape uniform across tests.
func nsPath(suffix string) string {
	return "/" + testNS + "/maven2" + suffix
}
