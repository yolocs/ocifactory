package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/ocifactory/pkg/artifact"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/filter"
	"github.com/yolocs/ocifactory/pkg/proxy/indexcache"
	proxynpm "github.com/yolocs/ocifactory/pkg/proxy/npm"
)

type fakeProxyFetcher struct {
	mu sync.Mutex

	packument    map[string]*proxynpm.PackumentResponse
	packumentErr map[string]error
	tarballs     map[string]*proxynpm.TarballResponse
	tarballErr   map[string]error

	getPackumentCalls int
	fetchTarballCalls int
}

func newFakeProxyFetcher() *fakeProxyFetcher {
	return &fakeProxyFetcher{
		packument:    map[string]*proxynpm.PackumentResponse{},
		packumentErr: map[string]error{},
		tarballs:     map[string]*proxynpm.TarballResponse{},
		tarballErr:   map[string]error{},
	}
}

func (f *fakeProxyFetcher) GetPackument(_ context.Context, pkg string) (*proxynpm.PackumentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPackumentCalls++
	if err, ok := f.packumentErr[pkg]; ok {
		return nil, err
	}
	if p, ok := f.packument[pkg]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("fake: no packument for %q: %w", pkg, proxy.ErrNotFound)
}

func (f *fakeProxyFetcher) FetchTarball(_ context.Context, pkg, version, filename string) (*proxynpm.TarballResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchTarballCalls++
	key := pkg + "@" + version + "/" + filename
	if err, ok := f.tarballErr[key]; ok {
		return nil, err
	}
	if r, ok := f.tarballs[key]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, fmt.Errorf("fake: no tarball for %q: %w", key, proxy.ErrNotFound)
}

func (f *fakeProxyFetcher) calls() (packument, tarball int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getPackumentCalls, f.fetchTarballCalls
}

func newProxyTestHandler(t *testing.T, fetcher ProxyFetcher, spec namespace.Spec, opts ...Option) (*Handler, *namespace.Store, *oci.FakeRegistry) {
	t.Helper()
	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := artifact.NewStore(fake, store, artifact.WithPolicyCacheTTL(0))

	if spec.Mode == "" {
		spec.Mode = namespace.ModeProxy
	}
	if spec.Proxy.Upstream == "" {
		spec.Proxy.Upstream = "https://registry.npmjs.org"
	}
	if spec.Policy.Readers == nil && spec.Policy.Writers == nil {
		spec.Policy = allowAllPolicy()
	}
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: spec}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	allOpts := []Option{
		WithAuthMiddleware(authMW),
		WithFetcherFactory(func(*url.URL) (ProxyFetcher, error) { return fetcher, nil }),
		WithProxyL1IndexCacheTTL(-1),
	}
	allOpts = append(allOpts, opts...)
	h, err := NewHandler(reg, allOpts...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, store, fake
}

func TestProxy_TarballMissFetchesAndCaches(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	body := "tgz-bytes"
	packument := npmPackumentBody("left-pad", "1.0.0", "https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz")
	fetcher.tarballs["left-pad@1.0.0/left-pad-1.0.0.tgz"] = &proxynpm.TarballResponse{
		Body:                 io.NopCloser(strings.NewReader(body)),
		ContentType:          "application/octet-stream",
		ContentLength:        int64(len(body)),
		Packument:            packument,
		PackumentContentType: "application/json",
		Version:              versionRaw(t, packument, "1.0.0"),
		UploadTime:           time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad/-/left-pad-1.0.0.tgz", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != body {
		t.Errorf("body=%q, want %q", rec.Body.String(), body)
	}

	req = httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad/-/left-pad-1.0.0.tgz", nil)
	rec = httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != body {
		t.Errorf("cached body=%q, want %q", rec.Body.String(), body)
	}
	_, gotTarballCalls := fetcher.calls()
	if gotTarballCalls != 1 {
		t.Errorf("FetchTarball calls=%d, want 1 cache fill", gotTarballCalls)
	}
}

func TestProxy_PackumentFetchRewritesAndCaches(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.packument["@scope/pkg"] = &proxynpm.PackumentResponse{
		Body:        npmPackumentBody("@scope/pkg", "1.0.0", "https://registry.npmjs.org/@scope/pkg/-/pkg-1.0.0.tgz"),
		ContentType: "application/json",
	}
	cache, err := indexcache.NewInMemoryCache()
	if err != nil {
		t.Fatalf("NewInMemoryCache: %v", err)
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{}, WithProxyIndexCache(cache))

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/@scope/pkg", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	gotTarball := packumentTarball(t, rec.Body.Bytes(), "1.0.0")
	wantTarball := "http://example.com/" + testNS + "/@scope/pkg/-/pkg-1.0.0.tgz"
	if gotTarball != wantTarball {
		t.Errorf("dist.tarball=%q, want %q", gotTarball, wantTarball)
	}

	fetcher.packumentErr["@scope/pkg"] = fmt.Errorf("upstream down: %w", proxy.ErrUpstreamUnavailable)
	req = httptest.NewRequest(http.MethodGet, "/"+testNS+"/@scope/pkg", nil)
	rec = httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stale status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if diff := cmp.Diff(wantTarball, packumentTarball(t, rec.Body.Bytes(), "1.0.0")); diff != "" {
		t.Errorf("stale tarball mismatch (-want +got):\n%s", diff)
	}
}

func TestProxy_PackumentRequiresReadAuthorization(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.packument["left-pad"] = &proxynpm.PackumentResponse{
		Body:        npmPackumentBody("left-pad", "1.0.0", "https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz"),
		ContentType: "application/json",
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
	}})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if got, _ := fetcher.calls(); got != 0 {
		t.Errorf("GetPackument calls=%d, want 0 when read authz denies", got)
	}
}

func TestProxy_ReaderOnlyNamespaceCanFillTarballCache(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	body := "tgz-bytes"
	packument := npmPackumentBody("left-pad", "1.0.0", "https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz")
	fetcher.tarballs["left-pad@1.0.0/left-pad-1.0.0.tgz"] = &proxynpm.TarballResponse{
		Body:                 io.NopCloser(strings.NewReader(body)),
		ContentType:          "application/octet-stream",
		ContentLength:        int64(len(body)),
		Packument:            packument,
		PackumentContentType: "application/json",
		Version:              versionRaw(t, packument, "1.0.0"),
	}
	h, _, backing := newProxyTestHandler(t, fetcher, namespace.Spec{Policy: namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
	}})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad/-/left-pad-1.0.0.tgz", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body=%q, want %q", got, body)
	}

	key := testNS + "/" + packageOwningRepo("left-pad") + "/1.0.0/left-pad-1.0.0.tgz"
	if got := string(backing.Files[key]); got != body {
		t.Errorf("cached tarball at %q = %q, want %q", key, got, body)
	}
}

func TestProxy_MalformedPackumentReturnsBadGateway(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.packument["left-pad"] = &proxynpm.PackumentResponse{
		Body:        npmPackumentBody("other", "1.0.0", "https://registry.npmjs.org/other/-/other-1.0.0.tgz"),
		ContentType: "application/json",
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestProxy_DistTagListReadsUpstreamPackument(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.packument["left-pad"] = &proxynpm.PackumentResponse{
		Body:        npmPackumentBody("left-pad", "1.0.0", "https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz"),
		ContentType: "application/json",
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/-/package/left-pad/dist-tags", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal dist-tags: %v", err)
	}
	want := map[string]string{"latest": "1.0.0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dist-tags mismatch (-want +got):\n%s", diff)
	}
}

func TestProxy_PackumentUpstreamUnavailableSynthesizesFromStoredVersions(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.packumentErr["left-pad"] = fmt.Errorf("upstream down: %w", proxy.ErrUpstreamUnavailable)
	h, _, backing := newProxyTestHandler(t, fetcher, namespace.Spec{})

	seedNPMVersion(t, backing, "left-pad", "1.0.0", "left-pad-1.0.0.tgz", []byte("tgz"))
	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got := packumentTarball(t, rec.Body.Bytes(), "1.0.0")
	want := "http://example.com/" + testNS + "/left-pad/-/left-pad-1.0.0.tgz"
	if got != want {
		t.Errorf("synthesized dist.tarball=%q, want %q", got, want)
	}
}

func TestProxy_PackumentSynthesisEmptyReturns503(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.packumentErr["missing"] = fmt.Errorf("upstream down: %w", proxy.ErrUpstreamUnavailable)
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/missing", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestProxy_TarballFilterDenyReturns404(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{
		Proxy: namespace.Proxy{
			Filters: filter.Filters{&filter.Denylist{Patterns: []string{"left-pad"}}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad/-/left-pad-1.0.0.tgz", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	_, tarballCalls := fetcher.calls()
	if tarballCalls != 0 {
		t.Errorf("FetchTarball calls=%d, want 0 when cheap filter denies", tarballCalls)
	}
}

func TestProxy_TarballUpstream404Returns404(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.tarballErr["left-pad@1.0.0/left-pad-1.0.0.tgz"] = fmt.Errorf("missing: %w", proxy.ErrNotFound)
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/left-pad/-/left-pad-1.0.0.tgz", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestProxy_WriteEndpointsReturn405(t *testing.T) {
	t.Parallel()

	h, _, _ := newProxyTestHandler(t, newFakeProxyFetcher(), namespace.Spec{})
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "publish", method: http.MethodPut, path: "/" + testNS + "/left-pad", body: `{}`},
		{name: "dist tag put", method: http.MethodPut, path: "/" + testNS + "/-/package/left-pad/dist-tags/latest", body: `"1.0.0"`},
		{name: "dist tag delete", method: http.MethodDelete, path: "/" + testNS + "/-/package/left-pad/dist-tags/latest"},
		{name: "unpublish package", method: http.MethodDelete, path: "/" + testNS + "/left-pad/-rev/1"},
		{name: "unpublish tarball", method: http.MethodDelete, path: "/" + testNS + "/left-pad/-/left-pad-1.0.0.tgz/-rev/1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status=%d, want 405 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func npmPackumentBody(pkg, version, tarball string) []byte {
	return []byte(fmt.Sprintf(`{
		"name": %q,
		"dist-tags": {"latest": %q},
		"time": {%q: "2026-05-01T00:00:00.000Z"},
		"versions": {
			%q: {
				"name": %q,
				"version": %q,
				"dist": {
					"tarball": %q,
					"shasum": "abc"
				}
			}
		}
	}`, pkg, version, version, version, pkg, version, tarball))
}

func versionRaw(t *testing.T, body []byte, version string) json.RawMessage {
	t.Helper()
	var p packument
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal packument: %v", err)
	}
	return p.Versions[version]
}

func packumentTarball(t *testing.T, body []byte, version string) string {
	t.Helper()
	var p packument
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal packument: %v", err)
	}
	var v versionMetadata
	if err := json.Unmarshal(p.Versions[version], &v); err != nil {
		t.Fatalf("unmarshal version: %v", err)
	}
	return v.Dist.Tarball
}

func seedNPMVersion(t *testing.T, backing *oci.FakeRegistry, pkg, version, filename string, tarball []byte) {
	t.Helper()
	repo := testNS + "/" + packageOwningRepo(pkg)
	if _, err := backing.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: repo,
		OwningTag:  version,
		Name:       filename,
		MediaType:  "application/octet-stream",
		Size:       int64(len(tarball)),
	}, strings.NewReader(string(tarball))); err != nil {
		t.Fatalf("seed tarball: %v", err)
	}
	meta := versionRaw(t, npmPackumentBody(pkg, version, "https://registry.npmjs.org/"+pkg+"/-/"+filename), version)
	if _, err := backing.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: repo,
		OwningTag:  version,
		Name:       versionMetaName,
		MediaType:  "application/json",
		Size:       int64(len(meta)),
	}, strings.NewReader(string(meta))); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
}
