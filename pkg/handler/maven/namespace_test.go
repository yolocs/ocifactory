package maven

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestNamespaceRoutes(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putMavenNamespace(t, store, "alpha", allowMavenSubjectSpec())
	putMavenNamespace(t, store, "beta", allowMavenSubjectSpec())
	h, err := NewHandler(fake, WithNamespaceRegistry(func(ns string) handler.Registry { return reg.For(ns) }))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := h.Mux()
	ctx := auth.WithAuthContext(t.Context(), &auth.AuthContext{Issuer: "issuer", ID: "alice"})

	req := httptest.NewRequest(http.MethodPut, "/alpha/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", strings.NewReader("jar"))
	req = req.WithContext(ctx)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("put status=%d body=%s", resp.Code, resp.Body.String())
	}

	for _, tc := range []struct {
		name string
		url  string
		want int
	}{
		{name: "same namespace", url: "/alpha/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusOK},
		{name: "other namespace", url: "/beta/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusNotFound},
		{name: "unknown namespace", url: "/missing/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.url, nil).WithContext(ctx)
			resp := httptest.NewRecorder()
			mux.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Errorf("status=%d want=%d body=%s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

func TestNamespaceRoutesForbidden(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	putMavenNamespace(t, store, "alpha", namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Issuer: "other"}}, Writers: []namespace.SubjectMatcher{{Issuer: "other"}}}})
	h, err := NewHandler(fake, WithNamespaceRegistry(func(ns string) handler.Registry { return reg.For(ns) }))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/alpha/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", nil).WithContext(auth.WithAuthContext(t.Context(), &auth.AuthContext{Issuer: "issuer", ID: "alice"}))
	resp := httptest.NewRecorder()
	h.Mux().ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=%d body=%s", resp.Code, http.StatusForbidden, resp.Body.String())
	}
}

func putMavenNamespace(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("put namespace %s: %v", name, err)
	}
}

func allowMavenSubjectSpec() namespace.Spec {
	return namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Issuer: "issuer"}}, Writers: []namespace.SubjectMatcher{{Issuer: "issuer"}}}}
}
