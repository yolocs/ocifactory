package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"crypto/sha1"
	"crypto/sha512"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestVersionTagSafe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "semver", input: "1.0.0", want: true},
		{name: "pre-release", input: "1.0.0-beta.1", want: true},
		{name: "build metadata", input: "1.0.0+build.1", want: false}, // OCI tags do not allow "+"
		{name: "v prefix", input: "v1.0.0", want: true},
		{name: "underscore", input: "1_0_0", want: true},
		{name: "empty", input: "", want: false},
		{name: "leading dot", input: ".0.0", want: false},
		{name: "leading dash", input: "-1.0.0", want: false},
		{name: "with slash", input: "1.0/0", want: false},
		{name: "too long", input: strings.Repeat("a", 129), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := versionTagSafe(tc.input)
			if got != tc.want {
				t.Errorf("versionTagSafe(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestPing(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	for _, path := range []string{"/" + testNS + "/-/ping", "/" + testNS + "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status=%d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestPublish_HappyPath_Unscoped exercises a single-version publish of
// an unscoped package and confirms the tarball + version metadata
// landed in the OCI backend under the namespace-prefixed path.
func TestPublish_HappyPath_Unscoped(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	const pkg = "example-pkg"
	const version = "1.2.3"
	tarball := []byte("synthetic-tarball-bytes")

	rec := doPublish(t, h, pkg, version, tarball, map[string]string{"latest": version})
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	tarballKey := testNS + "/packages/u/example-pkg/" + version + "/example-pkg-1.2.3.tgz"
	if got, ok := reg.Files[tarballKey]; !ok {
		t.Fatalf("tarball not stored at %q (have: %v)", tarballKey, fileKeys(reg))
	} else if !bytes.Equal(got, tarball) {
		t.Errorf("tarball bytes mismatch")
	}

	metaKey := testNS + "/packages/u/example-pkg/" + version + "/package.json"
	if _, ok := reg.Files[metaKey]; !ok {
		t.Errorf("version metadata not stored at %q", metaKey)
	}

	// dist-tag latest was bumped → AppendRefs created an alias.
	aliasTarget := reg.Aliases[testNS+"/packages/u/example-pkg/latest"]
	if aliasTarget != version {
		t.Errorf("dist-tag latest target=%q, want %q (aliases=%v)", aliasTarget, version, reg.Aliases)
	}

	// Index sentinel for the package landed in the index repo.
	if _, ok := reg.Files[testNS+"/index/example-pkg/"+indexSentinelName]; !ok {
		t.Errorf("index sentinel not stored")
	}
}

func TestPublish_HappyPath_Scoped(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	const pkg = "@my-scope/foo"
	const version = "0.0.1"
	tarball := []byte("scoped-tarball")

	rec := doPublish(t, h, pkg, version, tarball, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	tarballKey := testNS + "/packages/s/my-scope/foo/" + version + "/foo-0.0.1.tgz"
	if _, ok := reg.Files[tarballKey]; !ok {
		t.Fatalf("scoped tarball not stored at %q (have: %v)", tarballKey, fileKeys(reg))
	}
	metaKey := testNS + "/packages/s/my-scope/foo/" + version + "/package.json"
	if _, ok := reg.Files[metaKey]; !ok {
		t.Errorf("scoped version metadata not stored")
	}
}

// TestPublish_MultipleVersions confirms that publishing two versions
// of the same package writes both, leaves a single index sentinel, and
// updates the `latest` alias to the most recent version.
func TestPublish_MultipleVersions(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	const pkg = "example-pkg"
	tarball := []byte("payload")

	if rec := doPublish(t, h, pkg, "1.0.0", tarball, map[string]string{"latest": "1.0.0"}); rec.Code != http.StatusCreated {
		t.Fatalf("first publish status=%d", rec.Code)
	}
	if rec := doPublish(t, h, pkg, "1.1.0", tarball, map[string]string{"latest": "1.1.0"}); rec.Code != http.StatusCreated {
		t.Fatalf("second publish status=%d (body=%s)", rec.Code, rec.Body.String())
	}

	if got, want := reg.Aliases[testNS+"/packages/u/example-pkg/latest"], "1.1.0"; got != want {
		t.Errorf("dist-tag latest=%q, want %q", got, want)
	}
	if got := len(reg.Tags[testNS+"/index"]); got != 1 {
		t.Errorf("index tags=%v, want exactly one entry", reg.Tags[testNS+"/index"])
	}
}

// TestPublish_BadBody covers the assorted publish-rejection branches
// in one table.
func TestPublish_BadBody(t *testing.T) {
	t.Parallel()

	validTarball := []byte("payload")

	cases := []struct {
		name        string
		urlPkg      string
		body        []byte
		mangle      func([]byte) []byte
		wantStatus  int
		wantContain string
	}{
		{
			name:       "name in URL does not match body",
			urlPkg:     "example-pkg",
			body:       publishBodyForTest("other", "1.0.0", validTarball, nil),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid package name in URL",
			urlPkg:     "Foo-Bar",
			body:       publishBodyForTest("foo-bar", "1.0.0", validTarball, nil),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty versions map",
			urlPkg:     "example-pkg",
			body:       []byte(`{"name":"example-pkg","versions":{},"_attachments":{}}`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing attachment",
			urlPkg:     "example-pkg",
			body:       []byte(`{"name":"example-pkg","versions":{"1.0.0":{"name":"example-pkg","version":"1.0.0"}}}`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:        "invalid base64 attachment",
			urlPkg:      "example-pkg",
			body:        publishBodyForTest("example-pkg", "1.0.0", validTarball, nil),
			mangle:      mangleAttachmentData,
			wantStatus:  http.StatusBadRequest,
			wantContain: "base64",
		},
		{
			name:        "sha1 mismatch",
			urlPkg:      "example-pkg",
			body:        publishBodyForTest("example-pkg", "1.0.0", validTarball, nil),
			mangle:      mangleSha1,
			wantStatus:  http.StatusBadRequest,
			wantContain: "sha1",
		},
		{
			name:        "sha512 mismatch",
			urlPkg:      "example-pkg",
			body:        publishBodyForTest("example-pkg", "1.0.0", validTarball, nil),
			mangle:      mangleIntegrity,
			wantStatus:  http.StatusBadRequest,
			wantContain: "sha512",
		},
		{
			name:        "not JSON",
			urlPkg:      "example-pkg",
			body:        []byte("not-json"),
			wantStatus:  http.StatusBadRequest,
			wantContain: "valid npm publish document",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := tc.body
			if tc.mangle != nil {
				body = tc.mangle(body)
			}

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg)
			req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/"+tc.urlPkg, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantContain != "" && !strings.Contains(rec.Body.String(), tc.wantContain) {
				t.Errorf("body=%q, want substring %q", rec.Body.String(), tc.wantContain)
			}
		})
	}
}

// TestPublish_ReuploadConflict verifies that re-publishing the same
// version returns 409 by default and 201 when allow-overwrite=true.
func TestPublish_ReuploadConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		allowOverwrite bool
		wantSecond     int
	}{
		{name: "default rejects re-publish", allowOverwrite: false, wantSecond: http.StatusConflict},
		{name: "allow-overwrite returns 201", allowOverwrite: true, wantSecond: http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			reg.AllowOverwrite = tc.allowOverwrite
			h, _ := newTestHandler(t, reg)

			if rec := doPublish(t, h, "example", "1.0.0", []byte("first"), nil); rec.Code != http.StatusCreated {
				t.Fatalf("first publish status=%d (body=%s)", rec.Code, rec.Body.String())
			}
			rec := doPublish(t, h, "example", "1.0.0", []byte("second"), nil)
			if rec.Code != tc.wantSecond {
				t.Fatalf("second publish status=%d, want %d (body=%s)", rec.Code, tc.wantSecond, rec.Body.String())
			}
		})
	}
}

// TestPublish_MaxUploadBytes confirms WithMaxUploadBytes returns 413
// for an oversized body without contacting the backend.
func TestPublish_MaxUploadBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		cap            int64
		bodyExtra      int
		wantStatus     int
		wantBackendHit bool
	}{
		{name: "under cap", cap: 1 << 20, bodyExtra: 1 << 10, wantStatus: http.StatusCreated, wantBackendHit: true},
		{name: "over cap", cap: 1 << 12, bodyExtra: 1 << 16, wantStatus: http.StatusRequestEntityTooLarge, wantBackendHit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg, WithMaxUploadBytes(tc.cap))

			rec := doPublish(t, h, "example", "1.0.0", bytes.Repeat([]byte("a"), tc.bodyExtra), nil)
			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			_, hit := reg.Files[testNS+"/packages/u/example/1.0.0/example-1.0.0.tgz"]
			if hit != tc.wantBackendHit {
				t.Errorf("backend hit=%v, want %v", hit, tc.wantBackendHit)
			}
		})
	}
}

// TestPublish_DefaultMaxUploadBytes confirms NewHandler defaults the
// cap to DefaultMaxUploadBytes when no WithMaxUploadBytes option is
// supplied.
func TestPublish_DefaultMaxUploadBytes(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)
	if got, want := h.maxUploadBytes, DefaultMaxUploadBytes; got != want {
		t.Errorf("default maxUploadBytes = %d, want %d", got, want)
	}
}

// TestPackument_RoundTrip verifies that a publish followed by a GET
// returns a packument with the expected versions, dist-tags, and a
// dist.tarball URL that points back at this server.
func TestPackument_RoundTrip(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	const pkg = "example-pkg"
	tarball := []byte("payload")

	if rec := doPublish(t, h, pkg, "1.0.0", tarball, map[string]string{"latest": "1.0.0"}); rec.Code != http.StatusCreated {
		t.Fatalf("publish v1 status=%d", rec.Code)
	}
	if rec := doPublish(t, h, pkg, "2.0.0", tarball, map[string]string{"latest": "2.0.0", "beta": "2.0.0"}); rec.Code != http.StatusCreated {
		t.Fatalf("publish v2 status=%d (body=%s)", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/"+pkg, nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("packument status=%d (body=%s)", rec.Code, rec.Body.String())
	}

	var pak packument
	if err := json.Unmarshal(rec.Body.Bytes(), &pak); err != nil {
		t.Fatalf("unmarshal packument: %v\nbody=%s", err, rec.Body.String())
	}
	if pak.Name != pkg {
		t.Errorf("name=%q, want %q", pak.Name, pkg)
	}
	if got, want := []string{"1.0.0", "2.0.0"}, versionKeys(pak); !cmp.Equal(got, want) {
		t.Errorf("versions mismatch:\n got=%v\nwant=%v", want, got)
	}
	wantDistTags := map[string]string{"latest": "2.0.0", "beta": "2.0.0"}
	if diff := cmp.Diff(wantDistTags, pak.DistTags); diff != "" {
		t.Errorf("dist-tags mismatch (-want +got):\n%s", diff)
	}

	// dist.tarball rewritten to point at us.
	var v map[string]any
	if err := json.Unmarshal(pak.Versions["1.0.0"], &v); err != nil {
		t.Fatalf("unmarshal v1 metadata: %v", err)
	}
	dist, _ := v["dist"].(map[string]any)
	tarballURL, _ := dist["tarball"].(string)
	if !strings.Contains(tarballURL, "/"+testNS+"/"+pkg+"/-/example-pkg-1.0.0.tgz") {
		t.Errorf("tarball URL not rewritten: %q", tarballURL)
	}
}

func TestPackument_NotFound(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/missing-pkg", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestTarballGet verifies the tarball download path: bytes round-trip
// through publish + GET, HEAD returns headers only, redirect probe is
// skipped on HEAD.
func TestTarballGet(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	tarball := []byte("synthetic-payload")
	if rec := doPublish(t, h, "example", "1.0.0", tarball, nil); rec.Code != http.StatusCreated {
		t.Fatalf("publish status=%d", rec.Code)
	}

	t.Run("GET returns bytes", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/example/-/example-1.0.0.tgz", nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d (body=%s)", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), tarball) {
			t.Errorf("body bytes mismatch")
		}
		if got := rec.Header().Get("Content-Length"); got == "" {
			t.Errorf("missing Content-Length header")
		}
	})

	t.Run("HEAD returns headers only", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodHead, "/"+testNS+"/example/-/example-1.0.0.tgz", nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD body=%q, want empty", rec.Body.String())
		}
	})

	t.Run("missing tarball returns 404", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/example/-/example-9.9.9.tgz", nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status=%d, want 404", rec.Code)
		}
	})
}

// TestTarballGet_BlobRedirect verifies the redirect-vs-stream
// branching matches python and maven: probe on GET only, never on
// HEAD.
type redirectingRegistry struct {
	*oci.FakeRegistry
	redirectURL string
	redirectErr error
	calls       atomic.Int64
}

func (r *redirectingRegistry) BlobRedirectURL(_ context.Context, _ *oci.RepoFile) (string, error) {
	r.calls.Add(1)
	return r.redirectURL, r.redirectErr
}

func TestTarballGet_BlobRedirect(t *testing.T) {
	t.Parallel()

	const presigned = "https://cdn.example.com/blob?signature=xyz"
	tarball := []byte("payload")

	cases := []struct {
		name           string
		method         string
		redirectURL    string
		redirectErr    error
		wantStatus     int
		wantLocation   string
		wantBody       string
		wantProbeCalls int64
	}{
		{name: "GET redirects when backend returns presigned URL", method: http.MethodGet, redirectURL: presigned, wantStatus: http.StatusTemporaryRedirect, wantLocation: presigned, wantProbeCalls: 1},
		{name: "GET falls through to streaming when empty", method: http.MethodGet, redirectURL: "", wantStatus: http.StatusOK, wantBody: string(tarball), wantProbeCalls: 1},
		{name: "GET falls through on probe error", method: http.MethodGet, redirectErr: errors.New("transient"), wantStatus: http.StatusOK, wantBody: string(tarball), wantProbeCalls: 1},
		{name: "HEAD never probes", method: http.MethodHead, redirectURL: presigned, wantStatus: http.StatusOK, wantProbeCalls: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := oci.NewFakeRegistry()
			reg := &redirectingRegistry{
				FakeRegistry: fake,
				redirectURL:  tc.redirectURL,
				redirectErr:  tc.redirectErr,
			}
			h, _ := newTestHandler(t, reg)

			if rec := doPublish(t, h, "example", "1.0.0", tarball, nil); rec.Code != http.StatusCreated {
				t.Fatalf("publish status=%d", rec.Code)
			}
			reg.calls.Store(0)

			req := httptest.NewRequest(tc.method, "/"+testNS+"/example/-/example-1.0.0.tgz", nil)
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tc.wantLocation {
				t.Errorf("Location=%q, want %q", got, tc.wantLocation)
			}
			if tc.method == http.MethodGet && tc.wantBody != "" {
				if got := rec.Body.String(); got != tc.wantBody {
					t.Errorf("body=%q, want %q", got, tc.wantBody)
				}
			}
			if got := reg.calls.Load(); got != tc.wantProbeCalls {
				t.Errorf("probe calls=%d, want %d", got, tc.wantProbeCalls)
			}
		})
	}
}

// TestDistTagList ensures the resolved alias map matches what publish
// wrote.
func TestDistTagList(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	tarball := []byte("payload")
	if rec := doPublish(t, h, "example", "1.0.0", tarball, map[string]string{"latest": "1.0.0", "stable": "1.0.0"}); rec.Code != http.StatusCreated {
		t.Fatalf("publish status=%d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/-/package/example/dist-tags", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d (body=%s)", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]string{"latest": "1.0.0", "stable": "1.0.0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dist-tags mismatch (-want +got):\n%s", diff)
	}
}

// TestDistTagPut adds and updates dist-tags via the direct endpoint,
// covering both the JSON-quoted body form (what npm sends) and a bare
// version.
func TestDistTagPut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "json-quoted version", body: `"1.0.0"`, wantStatus: http.StatusOK},
		{name: "bare version", body: "1.0.0", wantStatus: http.StatusOK},
		{name: "missing version", body: ``, wantStatus: http.StatusBadRequest},
		{name: "invalid version characters", body: `"bad version"`, wantStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg)
			if rec := doPublish(t, h, "example", "1.0.0", []byte("payload"), nil); rec.Code != http.StatusCreated {
				t.Fatalf("publish status=%d", rec.Code)
			}

			req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/-/package/example/dist-tags/next", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestDistTagPut_UnknownVersion returns 404 when the target version
// does not exist in the canonical tags.
func TestDistTagPut_UnknownVersion(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/-/package/example/dist-tags/next", strings.NewReader(`"9.9.9"`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestDistTagDelete returns 501 — single-tag removal is not in v1.
func TestDistTagDelete(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	req := httptest.NewRequest(http.MethodDelete, "/"+testNS+"/-/package/example/dist-tags/latest", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status=%d, want 501 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestUnsupportedEndpoints returns 404 for out-of-scope routes (yank,
// login, whoami, search). Probing them must not produce panics.
func TestUnsupportedEndpoints(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "yank version", method: http.MethodDelete, path: "/" + testNS + "/example/-/example-1.0.0.tgz/-rev/1"},
		{name: "yank package", method: http.MethodDelete, path: "/" + testNS + "/example/-rev/1"},
		{name: "login", method: http.MethodPut, path: "/" + testNS + "/-/user/org.couchdb.user:foo"},
		{name: "whoami", method: http.MethodGet, path: "/" + testNS + "/-/whoami"},
		{name: "v1 user", method: http.MethodGet, path: "/" + testNS + "/-/npm/v1/user"},
		{name: "search", method: http.MethodGet, path: "/" + testNS + "/-/v1/search"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- helpers ---

// publishBodyForTest is a non-helper twin of publishBody — used in
// tests where we need to mangle the JSON afterwards.
func publishBodyForTest(pkg, version string, tarball []byte, distTags map[string]string) []byte {
	short := pkg
	if len(pkg) > 0 && pkg[0] == '@' {
		for i := 0; i < len(pkg); i++ {
			if pkg[i] == '/' {
				short = pkg[i+1:]
				break
			}
		}
	}
	tarballName := fmt.Sprintf("%s-%s.tgz", short, version)
	sha1Sum := sha1.Sum(tarball)
	sha512Sum := sha512.Sum512(tarball)
	dist := map[string]any{
		"shasum":    hex.EncodeToString(sha1Sum[:]),
		"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:]),
	}
	doc := map[string]any{
		"name": pkg,
		"versions": map[string]any{
			version: map[string]any{
				"name":    pkg,
				"version": version,
				"dist":    dist,
			},
		},
		"dist-tags": map[string]string{},
		"_attachments": map[string]any{
			tarballName: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	}
	if distTags != nil {
		doc["dist-tags"] = distTags
	}
	b, _ := json.Marshal(doc)
	return b
}

// mangleAttachmentData replaces the base64 attachment data with an
// invalid base64 string.
func mangleAttachmentData(body []byte) []byte {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	atts := m["_attachments"].(map[string]any)
	for k := range atts {
		atts[k].(map[string]any)["data"] = "!!!not-base64!!!"
	}
	out, _ := json.Marshal(m)
	return out
}

// mangleSha1 flips the dist.shasum to a wrong-but-syntactically-valid
// sha1 so the publisher's claim disagrees with the recomputed value.
func mangleSha1(body []byte) []byte {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	versions := m["versions"].(map[string]any)
	for _, v := range versions {
		dist := v.(map[string]any)["dist"].(map[string]any)
		dist["shasum"] = strings.Repeat("0", 40)
	}
	out, _ := json.Marshal(m)
	return out
}

// mangleIntegrity flips the dist.integrity to a wrong-but-valid SRI
// so the recomputed sha512 disagrees.
func mangleIntegrity(body []byte) []byte {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	versions := m["versions"].(map[string]any)
	bogus := make([]byte, 64)
	for i := range bogus {
		bogus[i] = byte(i)
	}
	for _, v := range versions {
		dist := v.(map[string]any)["dist"].(map[string]any)
		dist["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(bogus)
	}
	out, _ := json.Marshal(m)
	return out
}

// fileKeys returns a sorted-ish list of fake registry keys for
// diagnostic output on test failures.
func fileKeys(reg *oci.FakeRegistry) []string {
	keys := make([]string, 0, len(reg.Files))
	for k := range reg.Files {
		keys = append(keys, k)
	}
	return keys
}

// versionKeys extracts the version keys from a packument in sorted
// order so we can cmp.Diff against an expected slice.
func versionKeys(p packument) []string {
	out := make([]string, 0, len(p.Versions))
	for k := range p.Versions {
		out = append(out, k)
	}
	// Simple insertion sort to avoid pulling in sort just for tests.
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

// Make sure unused imports are kept in check across builds.
var (
	_ = io.Discard
	_ = errors.New
)
