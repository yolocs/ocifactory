package npm

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const testNS = "test-ns"

func allowAllPolicy() namespace.Policy {
	return namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
	}
}

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

func newUngatedHandler(t *testing.T, inner namespace.RegistryBackend) (*Handler, *namespace.Store) {
	t.Helper()
	store := namespace.NewStore(inner)
	reg := namespace.NewRegistry(inner, store, namespace.WithPolicyCacheTTL(0))
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: namespace.Spec{Policy: allowAllPolicy()}}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}
	h, err := NewHandler(reg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, store
}

func publishBody(t *testing.T, name, version string, data []byte, distTags map[string]string) []byte {
	t.Helper()
	sha1Sum := sha1.Sum(data)
	sha512Sum := sha512.Sum512(data)
	pm := PackageMetadata{
		Name:     name,
		DistTags: distTags,
		Versions: map[string]VersionInfo{
			version: {
				Name:    name,
				Version: version,
				Dist: Dist{
					Shasum:    hex.EncodeToString(sha1Sum[:]),
					Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:]),
				},
			},
		},
		Attachments: map[string]AttachmentStub{
			tarballFileName(name, version): {
				ContentType: "application/octet-stream",
				Data:        base64.StdEncoding.EncodeToString(data),
				Length:      len(data),
			},
		},
	}
	b, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("Marshal publish body: %v", err)
	}
	return b
}

func serve(t *testing.T, h *Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	return rec
}

func publishOK(t *testing.T, h *Handler, name, version string, data []byte, tags map[string]string) {
	t.Helper()
	rec := serve(t, h, http.MethodPut, "/"+testNS+"/"+name, publishBody(t, name, version, data, tags))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, body %q", rec.Code, rec.Body.String())
	}
}

func packageRepoForTest(t *testing.T, name string) string {
	t.Helper()
	encoded, err := encodePackageName(name)
	if err != nil {
		t.Fatalf("encodePackageName: %v", err)
	}
	return testNS + "/" + packageRepo(encoded)
}

func fakeFile(t *testing.T, fake *oci.FakeRegistry, key string) []byte {
	t.Helper()
	got, ok := fake.Files[key]
	if !ok {
		t.Fatalf("fake file %q missing", key)
	}
	return got
}
