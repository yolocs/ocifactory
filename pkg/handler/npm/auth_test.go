package npm

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestMux_AuthGating(t *testing.T) {
	t.Parallel()

	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	h, err := NewHandler(reg, WithAuthMiddleware(denyAll))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "root", method: http.MethodGet, path: "/test-ns/"},
		{name: "ping", method: http.MethodGet, path: "/test-ns/-/ping"},
		{name: "packument", method: http.MethodGet, path: "/test-ns/left-pad"},
		{name: "version", method: http.MethodGet, path: "/test-ns/left-pad/latest"},
		{name: "tarball", method: http.MethodGet, path: "/test-ns/left-pad/-/left-pad-1.0.0.tgz"},
		{name: "publish", method: http.MethodPut, path: "/test-ns/left-pad"},
		{name: "dist-tag add", method: http.MethodPut, path: "/test-ns/-/package/left-pad/dist-tags/latest"},
		{name: "dist-tag list", method: http.MethodGet, path: "/test-ns/-/package/left-pad/dist-tags"},
		{name: "dist-tag rm", method: http.MethodDelete, path: "/test-ns/-/package/left-pad/dist-tags/latest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if got, want := w.Code, http.StatusUnauthorized; got != want {
				t.Errorf("status = %d, want %d (auth middleware not chained?)", got, want)
			}
		})
	}
}

func TestMux_NoAuthMiddleware(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: namespace.Spec{Policy: allowAllPolicy()}}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}

	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got := w.Code; got == http.StatusUnauthorized {
		t.Errorf("status = 401 with no middleware configured; the namespace wrapper, not the middleware, should deny")
	}
	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d (no AuthContext → wrapper denies)", got, want)
	}
}

func TestMux_AuthChainsBeforeHandler(t *testing.T) {
	t.Parallel()

	installer := auth.Middleware(auth.AuthenticatorFunc(func(*http.Request) (*auth.AuthContext, error) {
		return &auth.AuthContext{Issuer: "anonymous", ID: "u"}, nil
	}))

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: namespace.Spec{Policy: allowAllPolicy()}}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}

	h, err := NewHandler(reg, WithAuthMiddleware(installer))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable {
		t.Errorf("status = %d; auth middleware unexpectedly rejected the request", w.Code)
	}
}
