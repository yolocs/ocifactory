package maven

import (
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/namespace/nsutil"
)

func newHandlerWithRegistry(t *testing.T, backend namespace.RegistryBackend, opts ...Option) (*Handler, error) {
	t.Helper()
	store := namespace.NewStore(backend)
	nsutil.Seed(t, t.Context(), store, &namespace.Namespace{Name: "default", Spec: allowTestIssuersSpec()})
	reg := namespace.NewRegistry(backend, store, namespace.WithPolicyCacheTTL(0))
	allOpts := append([]Option{WithAuthMiddleware(auth.Middleware(auth.AlwaysAnonymous))}, opts...)
	return NewHandler(reg, allOpts...)
}

func allowTestIssuersSpec() namespace.Spec {
	return namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}, {Issuer: "test-issuer"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}, {Issuer: "test-issuer"}},
	}}
}
