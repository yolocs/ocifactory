package python

import (
	"bytes"
	"mime/multipart"
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
	putPythonNamespace(t, store, "alpha", allowPythonSubjectSpec())
	putPythonNamespace(t, store, "beta", allowPythonSubjectSpec())
	h, err := NewHandler(fake, WithNamespaceRegistry(func(ns string) handler.Registry { return reg.For(ns) }))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := h.Mux()
	ctx := auth.WithAuthContext(t.Context(), &auth.AuthContext{Issuer: "issuer", ID: "alice"})

	body, contentType := pythonUploadBody(t, "Demo", "1.0.0", "demo-1.0.0.whl", "wheel")
	req := httptest.NewRequest(http.MethodPost, "/alpha/", body)
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", resp.Code, resp.Body.String())
	}

	for _, tc := range []struct {
		name string
		url  string
		want int
	}{
		{name: "same namespace", url: "/alpha/simple/demo/", want: http.StatusOK},
		{name: "other namespace", url: "/beta/packages/demo/1.0.0/demo-1.0.0.whl", want: http.StatusNotFound},
		{name: "unknown namespace", url: "/missing/simple/", want: http.StatusNotFound},
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
	putPythonNamespace(t, store, "alpha", namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Issuer: "other"}}, Writers: []namespace.SubjectMatcher{{Issuer: "other"}}}})
	h, err := NewHandler(fake, WithNamespaceRegistry(func(ns string) handler.Registry { return reg.For(ns) }))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/alpha/simple/", nil).WithContext(auth.WithAuthContext(t.Context(), &auth.AuthContext{Issuer: "issuer", ID: "alice"}))
	resp := httptest.NewRecorder()
	h.Mux().ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=%d body=%s", resp.Code, http.StatusForbidden, resp.Body.String())
	}
}

func pythonUploadBody(t *testing.T, pkg, version, filename, content string) (*strings.Reader, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("name", pkg); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("version", version); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("content", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(b.String()), w.FormDataContentType()
}

func putPythonNamespace(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("put namespace %s: %v", name, err)
	}
}

func allowPythonSubjectSpec() namespace.Spec {
	return namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Issuer: "issuer"}}, Writers: []namespace.SubjectMatcher{{Issuer: "issuer"}}}}
}
