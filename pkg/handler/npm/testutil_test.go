package npm

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
)

// testNS is the namespace every npm handler test publishes / reads
// under. Tests address requests at /test-ns/... and inspect backend
// keys prefixed with test-ns/.
const testNS = "test-ns"

// allowAllPolicy admits the anonymous-issuer AuthContext produced by
// [auth.AlwaysAnonymous] (the authenticator newTestHandler wires up
// by default so the namespace wrapper has a verified subject to
// authorize). Tests exercising specific policy behaviours pass a
// custom spec to [putNamespace].
func allowAllPolicy() namespace.Policy {
	return namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
	}
}

// newTestHandler builds an npm [*Handler] wired to a
// [*namespace.Registry] backed by inner. A "test-ns" namespace with
// an allow-all policy is registered; the auth middleware installs
// the [auth.AlwaysAnonymous] AuthContext on every request so the
// wrapper has a subject to authorize.
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
func putNamespace(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("Put namespace %q: %v", name, err)
	}
}

// publishBody marshals the npm publish JSON shape for a single
// version of pkgName. tarballBytes is the raw .tgz payload; the
// helper base64-encodes it and computes the dist.shasum / integrity
// the way a real npm client does. distTags is optional.
//
// The `_attachments` map is keyed by the full package name plus
// version (e.g. "@scope/foo-1.0.0.tgz" for scoped) to mirror what real
// npm clients send on `npm publish`.
func publishBody(t *testing.T, pkgName, version string, tarballBytes []byte, distTags map[string]string) []byte {
	t.Helper()
	tarballName := fmt.Sprintf("%s-%s.tgz", pkgName, version)

	sha1Sum := sha1.Sum(tarballBytes)
	sha512Sum := sha512.Sum512(tarballBytes)
	dist := map[string]any{
		"shasum":    hex.EncodeToString(sha1Sum[:]),
		"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:]),
		"tarball":   "http://registry.npmjs.org/" + pkgName + "/-/" + tarballName,
	}
	versionDoc := map[string]any{
		"name":        pkgName,
		"version":     version,
		"description": "test fixture",
		"dist":        dist,
	}

	doc := map[string]any{
		"name":      pkgName,
		"versions":  map[string]any{version: versionDoc},
		"dist-tags": map[string]string{},
		"_attachments": map[string]any{
			tarballName: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarballBytes),
				"length":       len(tarballBytes),
			},
		},
	}
	if distTags != nil {
		doc["dist-tags"] = distTags
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal publish body: %v", err)
	}
	return b
}

// doPublish posts a single-version publish for (pkg, version) under
// the test-ns namespace and returns the response recorder.
func doPublish(t *testing.T, h *Handler, pkg, version string, tarball []byte, distTags map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return doPublishInNS(t, h, testNS, pkg, version, tarball, distTags)
}

// doPublishInNS targets a specific namespace; used by isolation tests.
func doPublishInNS(t *testing.T, h *Handler, ns, pkg, version string, tarball []byte, distTags map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := publishBody(t, pkg, version, tarball, distTags)
	req := httptest.NewRequest(http.MethodPut, "/"+ns+"/"+pkg, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	return rec
}
