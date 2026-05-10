package python

import (
	"bytes"
	"fmt"
	"mime/multipart"
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
		name         string
		url          string
		want         int
		wantContains []string
	}{
		{
			name: "same namespace index",
			url:  "/alpha/simple/demo/",
			want: http.StatusOK,
			wantContains: []string{
				"/alpha/packages/demo/1.0.0/demo-1.0.0.whl",
			},
		},
		{
			name: "same namespace root",
			url:  "/alpha/simple/",
			want: http.StatusOK,
			wantContains: []string{
				"/alpha/simple/demo/",
			},
		},
		{name: "other namespace", url: "/beta/packages/demo/1.0.0/demo-1.0.0.whl", want: http.StatusNotFound},
		{name: "unknown namespace", url: "/missing/simple/", want: http.StatusNotFound},
		{name: "invalid namespace", url: "/_meta/simple/", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.url, nil).WithContext(ctx)
			resp := httptest.NewRecorder()
			mux.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Errorf("status=%d want=%d body=%s", resp.Code, tc.want, resp.Body.String())
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(resp.Body.String(), want) {
					t.Errorf("body missing %q; got:\n%s", want, resp.Body.String())
				}
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
