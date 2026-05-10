package maven

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/namespace/nsutil"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestNamespaceRoutes(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	nsutil.Seed(t, t.Context(), store, &namespace.Namespace{Name: "alpha", Spec: nsutil.AllowIssuerSpec("issuer")})
	nsutil.Seed(t, t.Context(), store, &namespace.Namespace{Name: "beta", Spec: nsutil.AllowIssuerSpec("issuer")})
	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := h.Mux()
	ctx := auth.WithAuthContext(t.Context(), &auth.AuthContext{Issuer: "issuer", ID: "alice"})

	seededPaths := []struct {
		path string
		body string
	}{
		{path: "/alpha/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", body: "jar"},
		{path: "/alpha/maven2/archetype-catalog.xml", body: "<archetype-catalog/>"},
		{path: "/alpha/maven2/com/example/demo/1.0-SNAPSHOT/maven-metadata.xml", body: "<snapshot/>"},
		{path: "/alpha/maven2/com/example/demo/maven-metadata.xml", body: "<metadata/>"},
	}
	for _, seeded := range seededPaths {
		req := httptest.NewRequest(http.MethodPut, seeded.path, strings.NewReader(seeded.body))
		req = req.WithContext(ctx)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("put %s status=%d body=%s", seeded.path, resp.Code, resp.Body.String())
		}
	}

	for _, tc := range []struct {
		name string
		url  string
		want int
	}{
		{name: "same namespace artifact", url: "/alpha/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusOK},
		{name: "same namespace archetype", url: "/alpha/maven2/archetype-catalog.xml", want: http.StatusOK},
		{name: "same namespace snapshot metadata", url: "/alpha/maven2/com/example/demo/1.0-SNAPSHOT/maven-metadata.xml", want: http.StatusOK},
		{name: "same namespace artifact metadata", url: "/alpha/maven2/com/example/demo/maven-metadata.xml", want: http.StatusOK},
		{name: "other namespace", url: "/beta/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusNotFound},
		{name: "unknown namespace", url: "/missing/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusNotFound},
		{name: "invalid namespace", url: "/_meta/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", want: http.StatusBadRequest},
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
	nsutil.Seed(t, t.Context(), store, &namespace.Namespace{Name: "alpha", Spec: nsutil.AllowIssuerSpec("other")})
	h, err := NewHandler(reg)
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

func TestWriteRegistryErrorBadRequest(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		fmt.Errorf("%w: _meta", namespace.ErrInvalidName),
		fmt.Errorf("%w: ../escape", namespace.ErrInvalidOwningRepo),
	} {
		rec := httptest.NewRecorder()
		writeRegistryError(t.Context(), rec, err, "internal")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%d want=%d for %v", rec.Code, http.StatusBadRequest, err)
		}
	}
}
