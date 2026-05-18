package python

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/filter"
	"github.com/yolocs/ocifactory/pkg/proxy/indexcache"
	pypython "github.com/yolocs/ocifactory/pkg/proxy/python"
)

// fakeFetcher is the in-test stand-in for pkg/proxy/python.Fetcher. It
// records call counts and returns operator-supplied responses keyed by
// (package, version, filename). It is intentionally simple — tests
// that need richer behaviour set the response field as a function.
type fakeFetcher struct {
	mu sync.Mutex

	simpleIndex     map[string]*pypython.IndexResponse
	simpleIndexErr  map[string]error
	topLevelIndex   *pypython.IndexResponse
	topLevelErr     error
	versionMeta     map[string]*pypython.VersionMetadata // key: pkg + "@" + version
	versionMetaErr  map[string]error
	files           map[string][]byte // key: url
	filesErr        map[string]error
	fileContentType string

	getSimpleIndexCalls int
	getTopLevelCalls    int
	getVersionMetaCalls int
	fetchFileCalls      int
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		simpleIndex:     map[string]*pypython.IndexResponse{},
		simpleIndexErr:  map[string]error{},
		versionMeta:     map[string]*pypython.VersionMetadata{},
		versionMetaErr:  map[string]error{},
		files:           map[string][]byte{},
		filesErr:        map[string]error{},
		fileContentType: "application/zip",
	}
}

func (f *fakeFetcher) GetSimpleIndex(_ context.Context, pkg string) (*pypython.IndexResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSimpleIndexCalls++
	if err, ok := f.simpleIndexErr[pkg]; ok {
		return nil, err
	}
	if r, ok := f.simpleIndex[pkg]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("fake: no index for %q: %w", pkg, proxy.ErrNotFound)
}

func (f *fakeFetcher) GetTopLevelIndex(_ context.Context) (*pypython.IndexResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getTopLevelCalls++
	if f.topLevelErr != nil {
		return nil, f.topLevelErr
	}
	return f.topLevelIndex, nil
}

func (f *fakeFetcher) GetVersionMetadata(_ context.Context, pkg, version string) (*pypython.VersionMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getVersionMetaCalls++
	key := pkg + "@" + version
	if err, ok := f.versionMetaErr[key]; ok {
		return nil, err
	}
	if m, ok := f.versionMeta[key]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("fake: no metadata for %s: %w", key, proxy.ErrNotFound)
}

func (f *fakeFetcher) FetchFile(_ context.Context, rawURL string) (*pypython.FileResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchFileCalls++
	if err, ok := f.filesErr[rawURL]; ok {
		return nil, err
	}
	if body, ok := f.files[rawURL]; ok {
		return &pypython.FileResponse{
			Body:          io.NopCloser(strings.NewReader(string(body))),
			ContentType:   f.fileContentType,
			ContentLength: int64(len(body)),
		}, nil
	}
	return nil, fmt.Errorf("fake: no file at %q: %w", rawURL, proxy.ErrNotFound)
}

func (f *fakeFetcher) calls() (idx, top, meta, file int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getSimpleIndexCalls, f.getTopLevelCalls, f.getVersionMetaCalls, f.fetchFileCalls
}

// newProxyTestHandler builds a python handler in proxy mode against
// the testNS namespace with the supplied fetcher and any extra
// options. Returns the handler + the underlying fake registry so
// tests can poke at backend state.
func newProxyTestHandler(t *testing.T, fetcher ProxyFetcher, extraSpec namespace.Spec, opts ...Option) (*Handler, *namespace.Store, *oci.FakeRegistry) {
	t.Helper()
	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))

	if extraSpec.Mode == "" {
		extraSpec.Mode = namespace.ModeProxy
	}
	if extraSpec.Proxy.Upstream == "" {
		extraSpec.Proxy.Upstream = "https://pypi.org"
	}
	if extraSpec.Policy.Readers == nil && extraSpec.Policy.Writers == nil {
		extraSpec.Policy = allowAllPolicy()
	}
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: extraSpec}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}

	authMW := auth.Middleware(auth.AlwaysAnonymous)
	allOpts := []Option{
		WithAuthMiddleware(authMW),
		WithFetcherFactory(func(*url.URL) (ProxyFetcher, error) { return fetcher, nil }),
		// Disable L1 by default so tests that count fetcher calls
		// don't get fooled by L1 hits. Tests that want L1 set their
		// own TTL via opts.
		WithProxyL1IndexCacheTTL(-1),
	}
	allOpts = append(allOpts, opts...)
	h, err := NewHandler(reg, allOpts...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, store, fake
}

// TestProxy_AuthGating proves the proxy-mode routes go through the
// same WithAuthMiddleware chain as the hosted routes. The wider
// TestMux_AuthGating doesn't exercise proxy mode because it doesn't
// register a namespace; here we register a proxy namespace and
// confirm the deny-all middleware still short-circuits.
func TestProxy_AuthGating(t *testing.T) {
	t.Parallel()

	denyAll := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}

	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))
	if err := store.Put(t.Context(), &namespace.Namespace{
		Name: testNS,
		Spec: namespace.Spec{
			Mode:   namespace.ModeProxy,
			Proxy:  namespace.Proxy{Upstream: "https://pypi.org"},
			Policy: allowAllPolicy(),
		},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h, err := NewHandler(reg, WithAuthMiddleware(denyAll))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := h.Mux()

	tests := []struct {
		name string
		path string
	}{
		{name: "proxy package index", path: "/" + testNS + "/simple/requests/"},
		{name: "proxy top-level index", path: "/" + testNS + "/simple/"},
		{name: "proxy file", path: "/" + testNS + "/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if got, want := w.Code, http.StatusUnauthorized; got != want {
				t.Errorf("status = %d, want %d (proxy routes must chain auth middleware)", got, want)
			}
		})
	}
}

func TestProxy_UploadReturns405(t *testing.T) {
	t.Parallel()
	h, _, _ := newProxyTestHandler(t, newFakeFetcher(), namespace.Spec{})

	r := httptest.NewRequest(http.MethodPost, "/"+testNS+"/", strings.NewReader(""))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusMethodNotAllowed; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got == "" {
		t.Errorf("Allow header missing on 405 response")
	}
}

func TestProxy_FileCacheHit_NoUpstream(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	h, _, backing := newProxyTestHandler(t, fake, namespace.Spec{})

	// Seed the registry with a file directly so the proxy path
	// observes a hit on the first read.
	rf := &oci.RepoFile{
		OwningRepo: "packages/requests",
		OwningTag:  "2.31.0",
		Name:       "requests-2.31.0-py3-none-any.whl",
		MediaType:  "application/x-wheel+zip",
	}
	scoped := namespace.NewRegistry(backing, namespace.NewStore(backing), namespace.WithPolicyCacheTTL(0))
	_ = scoped
	// Use a direct AddFile against the fake; bypasses authz which
	// is fine because the fake registry doesn't authorize on its
	// own — namespace.Registry does. We address the fake at the
	// post-prefix path the namespace wrapper would have produced.
	if _, err := backing.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: testNS + "/packages/requests",
		OwningTag:  rf.OwningTag,
		Name:       rf.Name,
		MediaType:  rf.MediaType,
	}, strings.NewReader("wheel-bytes")); err != nil {
		t.Fatalf("seed AddFile: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/packages/requests/2.31.0/"+rf.Name, nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if body := w.Body.String(); body != "wheel-bytes" {
		t.Errorf("body = %q, want %q", body, "wheel-bytes")
	}
	if _, _, _, fileCalls := fake.calls(); fileCalls != 0 {
		t.Errorf("fetcher.FetchFile called %d times on a registry hit", fileCalls)
	}
}

func TestProxy_FileMissFetchAndCache(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/abc/requests-2.31.0-py3-none-any.whl"
	fake.versionMeta["requests@2.31.0"] = &pypython.VersionMetadata{
		Package: "requests",
		Version: "2.31.0",
		Files: []pypython.FileMetadata{{
			Filename:   "requests-2.31.0-py3-none-any.whl",
			URL:        fileURL,
			SHA256:     "abc",
			Size:       11,
			UploadTime: time.Date(2023, 5, 22, 15, 12, 42, 0, time.UTC),
		}},
		UploadTime: time.Date(2023, 5, 22, 15, 12, 42, 0, time.UTC),
	}
	fake.files[fileURL] = []byte("wheel-bytes")

	h, _, backing := newProxyTestHandler(t, fake, namespace.Spec{})

	path := "/" + testNS + "/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if body := w.Body.String(); body != "wheel-bytes" {
		t.Errorf("body = %q", body)
	}
	if _, _, metaCalls, fileCalls := fake.calls(); metaCalls != 1 || fileCalls != 1 {
		t.Errorf("calls (meta=%d file=%d), want (1 1)", metaCalls, fileCalls)
	}

	// Second request hits the registry cache — no fetcher calls.
	w2 := httptest.NewRecorder()
	h.Mux().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, path, nil))
	if got := w2.Code; got != http.StatusOK {
		t.Fatalf("second request status = %d", got)
	}
	if _, _, metaCalls, fileCalls := fake.calls(); metaCalls != 1 || fileCalls != 1 {
		t.Errorf("after second request: calls (meta=%d file=%d), want (1 1)", metaCalls, fileCalls)
	}

	// And confirm the backing OCI store actually holds the file at
	// the namespace-prefixed path.
	files, err := backing.ListFiles(t.Context(), testNS+"/packages/requests")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("backing has %d files, want 1", len(files))
	}
}

func TestProxy_FileMissCacheFillRequiresOnlyReadPolicy(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/abc/requests-2.31.0-py3-none-any.whl"
	fake.versionMeta["requests@2.31.0"] = &pypython.VersionMetadata{
		Package: "requests",
		Version: "2.31.0",
		Files: []pypython.FileMetadata{{
			Filename:   "requests-2.31.0-py3-none-any.whl",
			URL:        fileURL,
			Size:       11,
			UploadTime: time.Date(2023, 5, 22, 15, 12, 42, 0, time.UTC),
		}},
		UploadTime: time.Date(2023, 5, 22, 15, 12, 42, 0, time.UTC),
	}
	fake.files[fileURL] = []byte("wheel-bytes")

	spec := namespace.Spec{
		Policy: namespace.Policy{
			Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
			Writers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
		},
	}
	h, _, backing := newProxyTestHandler(t, fake, spec)

	path := "/" + testNS + "/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl"
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if body := w.Body.String(); body != "wheel-bytes" {
		t.Errorf("body = %q, want %q", body, "wheel-bytes")
	}

	files, err := backing.ListFiles(t.Context(), testNS+"/packages/requests")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("backing has %d files, want 1", len(files))
	}
}

// TestProxy_FileMetadataSidecar exercises the PEP 658 path: when pip
// asks for `<wheel>.metadata` (because the upstream simple index
// advertised `data-core-metadata`), the proxy synthesizes the upstream
// URL from the matching wheel entry instead of 404'ing because PyPI's
// JSON `urls` list never enumerates `.metadata` files.
func TestProxy_FileMetadataSidecar(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const wheelURL = "https://files.pythonhosted.org/packages/abc/six-1.16.0-py2.py3-none-any.whl"
	fake.versionMeta["six@1.16.0"] = &pypython.VersionMetadata{
		Package: "six",
		Version: "1.16.0",
		Files: []pypython.FileMetadata{{
			Filename:   "six-1.16.0-py2.py3-none-any.whl",
			URL:        wheelURL,
			SHA256:     "wheelhash",
			Size:       11,
			UploadTime: time.Date(2021, 5, 5, 0, 0, 0, 0, time.UTC),
		}},
		UploadTime: time.Date(2021, 5, 5, 0, 0, 0, 0, time.UTC),
	}
	fake.files[wheelURL+".metadata"] = []byte("Metadata-Version: 2.1\nName: six\n")

	h, _, backing := newProxyTestHandler(t, fake, namespace.Spec{})

	path := "/" + testNS + "/packages/six/1.16.0/six-1.16.0-py2.py3-none-any.whl.metadata"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if body := w.Body.String(); body != "Metadata-Version: 2.1\nName: six\n" {
		t.Errorf("body = %q", body)
	}

	files, err := backing.ListFiles(t.Context(), testNS+"/packages/six")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := []string{"six-1.16.0-py2.py3-none-any.whl.metadata"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("cached files (-want +got):\n%s", diff)
	}
}

func TestProxy_FilterDeny(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/.../requests-2.31.0-py3-none-any.whl"
	fake.versionMeta["requests@2.31.0"] = &pypython.VersionMetadata{
		Files: []pypython.FileMetadata{{
			Filename: "requests-2.31.0-py3-none-any.whl",
			URL:      fileURL,
		}},
	}
	fake.files[fileURL] = []byte("wheel-bytes")

	// A denylist matching the package name short-circuits before
	// upstream metadata is fetched.
	deny := &filter.Denylist{Rules: []filter.Rule{{Package: "requests"}}}

	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{
		Proxy: namespace.Proxy{Upstream: "https://pypi.org", Filters: filter.Filters{deny}},
	})

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if _, _, metaCalls, fileCalls := fake.calls(); metaCalls != 0 || fileCalls != 0 {
		t.Errorf("denied request hit upstream: meta=%d file=%d", metaCalls, fileCalls)
	}
}

func TestProxy_FilterDelayDeniesAfterMetadata(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/.../recent-1.0.0-py3-none-any.whl"
	fake.versionMeta["recent@1.0.0"] = &pypython.VersionMetadata{
		Files: []pypython.FileMetadata{{
			Filename:   "recent-1.0.0-py3-none-any.whl",
			URL:        fileURL,
			UploadTime: time.Now().Add(-1 * time.Minute), // fresh
		}},
	}
	fake.files[fileURL] = []byte("wheel-bytes")

	// 1-hour delay → 1-minute-old version denies after metadata.
	delay := &filter.Delay{MinAge: time.Hour}

	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{
		Proxy: namespace.Proxy{Upstream: "https://pypi.org", Filters: filter.Filters{delay}},
	})

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/packages/recent/1.0.0/recent-1.0.0-py3-none-any.whl", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if _, _, metaCalls, fileCalls := fake.calls(); metaCalls != 1 || fileCalls != 0 {
		t.Errorf("delay filter must fetch metadata but not file: meta=%d file=%d", metaCalls, fileCalls)
	}
}

func TestProxy_FileUpstream404Negative(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	negCache := indexcache.NewNegativeCache()
	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{}, WithProxyNegativeCache(negCache))

	path := "/" + testNS + "/packages/missing/1.0.0/missing-1.0.0-py3-none-any.whl"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	// Second request hits the negative cache — no fetcher call.
	w2 := httptest.NewRecorder()
	h.Mux().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, path, nil))
	if got, want := w2.Code, http.StatusNotFound; got != want {
		t.Errorf("second status = %d, want %d", got, want)
	}
	if _, _, metaCalls, fileCalls := fake.calls(); metaCalls != 1 || fileCalls != 0 {
		t.Errorf("negative cache should suppress second metadata call: meta=%d file=%d", metaCalls, fileCalls)
	}
}

func TestProxy_FileUpstreamUnavailable(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	fake.versionMetaErr["x@1.0.0"] = fmt.Errorf("upstream boom: %w", proxy.ErrUpstreamUnavailable)

	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{})
	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/packages/x/1.0.0/x-1.0.0.tar.gz", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusBadGateway; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

func TestProxy_IndexPassthroughAndCache(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const upstreamHTML = `<html><body><a href="https://files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl#sha256=abc">requests-2.31.0-py3-none-any.whl</a></body></html>`
	fake.simpleIndex["requests"] = &pypython.IndexResponse{
		Body:        []byte(upstreamHTML),
		ContentType: "application/vnd.pypi.simple.v1+html",
	}

	// Wire a real OCI-backed index cache against a separate backing
	// fake so we observe Put/Get behaviour end-to-end.
	cache, backingURL, cleanup := newOCIIndexCache(t)
	defer cleanup()
	_ = backingURL

	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{}, WithProxyIndexCache(cache))

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/`+testNS+`/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl#sha256=abc"`) {
		t.Errorf("expected rewritten href, got:\n%s", body)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/vnd.pypi.simple.v1+html") {
		t.Errorf("Content-Type = %q, want PEP 691 html", got)
	}
	idxCalls, _, _, _ := fake.calls()
	if idxCalls != 1 {
		t.Errorf("expected 1 upstream index call, got %d", idxCalls)
	}

	// Second request hits the indexcache (OCI) — no upstream call.
	// We disabled L1 in the test harness so the freshness check
	// against indexcache is what saves us.
	w2 := httptest.NewRecorder()
	h.Mux().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil))
	if got := w2.Code; got != http.StatusOK {
		t.Fatalf("second status = %d", got)
	}
	idxCalls, _, _, _ = fake.calls()
	if idxCalls != 1 {
		t.Errorf("expected indexcache to suppress second upstream call, got %d", idxCalls)
	}
}

func TestProxy_IndexUpstreamErrorWithStaleCache(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const upstreamHTML = `<html><body><a href="https://files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl#sha256=abc">requests-2.31.0-py3-none-any.whl</a></body></html>`

	cache, _, cleanup := newOCIIndexCache(t)
	defer cleanup()

	// Pre-warm the cache directly — simulates "fetched at some
	// point in the past" without playing wall-clock games.
	if err := cache.Put(t.Context(), testNS, "requests", []byte(upstreamHTML), "application/vnd.pypi.simple.v1+html"); err != nil {
		t.Fatalf("cache.Put: %v", err)
	}

	// Now configure the fetcher to fail upstream.
	fake.simpleIndexErr["requests"] = fmt.Errorf("upstream: %w", proxy.ErrUpstreamUnavailable)

	// Use a TTL of 0 so the cached entry is "stale" the instant we
	// look at it; the handler must still fall back to it on
	// upstream failure.
	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{},
		WithProxyIndexCache(cache),
		WithProxyIndexCacheTTL(time.Nanosecond),
	)

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<a href="https://files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl#sha256=abc">`) &&
		!strings.Contains(w.Body.String(), upstreamHTML) {
		t.Errorf("stale cache body not served:\n%s", w.Body.String())
	}
}

func TestProxy_IndexUpstreamErrorSynthesizesFromListFiles(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	fake.simpleIndexErr["requests"] = fmt.Errorf("upstream: %w", proxy.ErrUpstreamUnavailable)

	h, _, backing := newProxyTestHandler(t, fake, namespace.Spec{})

	// Seed one file in the namespace's packages/requests repo
	// directly. The hosted code path uses ListFiles for synthesis;
	// adding a file gives it something to synthesize.
	if _, err := backing.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: testNS + "/packages/requests",
		OwningTag:  "2.31.0",
		Name:       "requests-2.31.0-py3-none-any.whl",
		MediaType:  "application/x-wheel+zip",
	}, strings.NewReader("wheel-bytes")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "requests-2.31.0-py3-none-any.whl") {
		t.Errorf("synthesized index missing seeded file:\n%s", w.Body.String())
	}
}

func TestProxy_IndexUpstreamErrorNoSourceReturns503(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	fake.simpleIndexErr["nothing"] = fmt.Errorf("upstream: %w", proxy.ErrUpstreamUnavailable)
	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{})

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/nothing/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusServiceUnavailable; got != want {
		t.Errorf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
}

func TestProxy_IndexUpstream404Returns404(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	// No explicit simpleIndex entry → fake returns ErrNotFound.
	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{})

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/missing/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

func TestProxy_TopLevelIndexPassthrough(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const body = `<html><body><a href="/simple/requests/">requests</a></body></html>`
	fake.topLevelIndex = &pypython.IndexResponse{Body: []byte(body), ContentType: "text/html"}

	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{})
	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if w.Body.String() != body {
		t.Errorf("body mismatch: got %q want %q", w.Body.String(), body)
	}
	_, top, _, _ := fake.calls()
	if top != 1 {
		t.Errorf("top-level index calls = %d, want 1", top)
	}
}

func TestProxy_TopLevelUpstreamErrorSynthesizes(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	fake.topLevelErr = fmt.Errorf("upstream: %w", proxy.ErrUpstreamUnavailable)

	h, _, backing := newProxyTestHandler(t, fake, namespace.Spec{})

	// Seed the synthesis source — the per-format index repo's tag list.
	if _, err := backing.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: testNS + "/" + packageIndexName,
		OwningTag:  "requests",
		Name:       "present",
		MediaType:  "text/plain",
	}, strings.NewReader(indexSentinelContent)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body=%s)", got, want, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "requests") {
		t.Errorf("synthesis missing seeded tag:\n%s", w.Body.String())
	}
}

func TestProxy_FileFlight_ConcurrentMissDeduped(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/abc/x-1.0.0.tar.gz"
	fake.versionMeta["x@1.0.0"] = &pypython.VersionMetadata{
		Package: "x", Version: "1.0.0",
		Files: []pypython.FileMetadata{{Filename: "x-1.0.0.tar.gz", URL: fileURL, Size: 4}},
	}
	fake.files[fileURL] = []byte("body")

	h, _, _ := newProxyTestHandler(t, fake, namespace.Spec{})

	const goroutines = 8
	var wg sync.WaitGroup
	var ok atomic.Int64
	wg.Add(goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			r := httptest.NewRequest(http.MethodGet, "/"+testNS+"/packages/x/1.0.0/x-1.0.0.tar.gz", nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, r)
			if w.Code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := ok.Load(); got != goroutines {
		t.Errorf("ok responses = %d, want %d", got, goroutines)
	}
	_, _, metaCalls, fileCalls := fake.calls()
	if metaCalls > 1 || fileCalls > 1 {
		// Some racy interleaving CAN produce 2 calls (the
		// re-check inside the singleflight only succeeds when
		// AddFile finished before the next caller arrives), but
		// we should never see goroutines worth of upstream calls.
		if metaCalls >= goroutines || fileCalls >= goroutines {
			t.Errorf("singleflight ineffective: meta=%d file=%d (goroutines=%d)",
				metaCalls, fileCalls, goroutines)
		}
	}
}

// TestTolerantWriter verifies that a client write failure latches the
// writer into "dead" mode without surfacing the error back to the
// reader. This is the contract io.TeeReader relies on inside the
// proxy file path: AddFile keeps draining upstream after the client
// goes away.
func TestTolerantWriter(t *testing.T) {
	t.Parallel()

	failing := &failOnNthWriter{failAt: 1}
	tw := &tolerantWriter{w: failing}

	for i, chunk := range [][]byte{[]byte("aa"), []byte("bb"), []byte("cc")} {
		n, err := tw.Write(chunk)
		if err != nil {
			t.Errorf("Write[%d] err = %v, want nil", i, err)
		}
		if n != len(chunk) {
			t.Errorf("Write[%d] n = %d, want %d", i, n, len(chunk))
		}
	}
	if !tw.dead {
		t.Errorf("tolerantWriter not latched dead after underlying failure")
	}
	if !tw.used {
		t.Errorf("tolerantWriter.used = false after Writes attempted, want true")
	}
	if got, want := failing.writes, 1; got != want {
		t.Errorf("underlying Write calls = %d, want %d (subsequent writes should be swallowed)", got, want)
	}
}

// TestProxy_FileClientDisconnectStillFillsCache simulates a client that
// gives up partway through the response (httptest.NewRecorder wrapped
// in a writer that errors immediately). The OCI cache fill must still
// complete so the next request hits the cache.
func TestProxy_FileClientDisconnectStillFillsCache(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/abc/x-1.0.0.whl"
	fake.versionMeta["x@1.0.0"] = &pypython.VersionMetadata{
		Package: "x", Version: "1.0.0",
		Files: []pypython.FileMetadata{{
			Filename: "x-1.0.0.whl",
			URL:      fileURL,
			Size:     int64(len("body-bytes")),
		}},
	}
	fake.files[fileURL] = []byte("body-bytes")

	h, _, backing := newProxyTestHandler(t, fake, namespace.Spec{})

	// First request with a writer that fails every Write, simulating
	// an immediate client disconnect.
	dead := &deadClientRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodGet,
		"/"+testNS+"/packages/x/1.0.0/x-1.0.0.whl", nil)
	h.Mux().ServeHTTP(dead, r)

	// The cache fill must have completed regardless of the client
	// write status.
	files, err := backing.ListFiles(t.Context(), testNS+"/packages/x")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("backing has %d files after disconnect, want 1", len(files))
	}

	// Second request with a normal writer reads the cached file and
	// must not hit upstream again.
	w2 := httptest.NewRecorder()
	h.Mux().ServeHTTP(w2, httptest.NewRequest(http.MethodGet,
		"/"+testNS+"/packages/x/1.0.0/x-1.0.0.whl", nil))
	if got, want := w2.Code, http.StatusOK; got != want {
		t.Fatalf("second request status = %d, want %d (body=%s)", got, want, w2.Body.String())
	}
	if got, want := w2.Body.String(), "body-bytes"; got != want {
		t.Errorf("second request body = %q, want %q", got, want)
	}
	if _, _, _, fileCalls := fake.calls(); fileCalls != 1 {
		t.Errorf("fetchFileCalls = %d after second request, want 1 (cache should serve)", fileCalls)
	}
}

// TestProxy_FileAddFileFailureAfterTeeDoesNotCorruptBody locks in the
// fix for the bug where AddFile failures (e.g. zot rejecting a manifest
// with invalid mediaType) caused us to call http.Error AFTER the tee
// already wrote 200 + headers + body bytes. http.Error appended its
// public-message string to the body the client was about to hash,
// turning a clean truncation into a silent hash mismatch — exactly what
// pip observed on PEP 658 sidecar fetches when our `.metadata` blobs
// had a parameterized mediaType.
func TestProxy_FileAddFileFailureAfterTeeDoesNotCorruptBody(t *testing.T) {
	t.Parallel()

	fake := newFakeFetcher()
	const fileURL = "https://files.pythonhosted.org/packages/abc/x-1.0.0.whl"
	const bodyBytes = "body-bytes-exactly"
	fake.versionMeta["x@1.0.0"] = &pypython.VersionMetadata{
		Package: "x", Version: "1.0.0",
		Files: []pypython.FileMetadata{{
			Filename: "x-1.0.0.whl",
			URL:      fileURL,
			Size:     int64(len(bodyBytes)),
		}},
	}
	fake.files[fileURL] = []byte(bodyBytes)

	// Build a namespace.Registry wrapping a backend whose AddFile
	// reads the full body (so the tee writes through) and then errors.
	backing := oci.NewFakeRegistry()
	wrapped := &addFileFailingBackend{
		inner:          backing,
		err:            errors.New("simulated manifest invalid"),
		failRepoSuffix: "/packages/x",
	}
	store := namespace.NewStore(wrapped)
	reg := namespace.NewRegistry(wrapped, store, namespace.WithPolicyCacheTTL(0))
	if err := store.Put(t.Context(), &namespace.Namespace{Name: testNS, Spec: namespace.Spec{
		Mode:   namespace.ModeProxy,
		Proxy:  namespace.Proxy{Upstream: "https://pypi.org"},
		Policy: allowAllPolicy(),
	}}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}
	authMW := auth.Middleware(auth.AlwaysAnonymous)
	h, err := NewHandler(reg,
		WithAuthMiddleware(authMW),
		WithFetcherFactory(func(*url.URL) (ProxyFetcher, error) { return fake, nil }),
		WithProxyL1IndexCacheTTL(-1),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/"+testNS+"/packages/x/1.0.0/x-1.0.0.whl", nil)
	h.Mux().ServeHTTP(w, r)

	// The body must be EXACTLY the upstream bytes — no error text
	// appended. The status was already committed to 200 by the tee's
	// first Write; we can't change it, but we MUST NOT corrupt the body.
	if got, want := w.Body.String(), bodyBytes; got != want {
		t.Errorf("body = %q, want %q (extra bytes from http.Error would break content-hash verification)",
			got, want)
	}
}

// addFileFailingBackend wraps a real backend so AddFile reads the full
// body (so any tee gets exercised end-to-end) and then errors — but only
// for OwningRepos whose suffix matches failRepoSuffix, so namespace-store
// writes and other infrastructure AddFile calls still succeed.
type addFileFailingBackend struct {
	inner          *oci.FakeRegistry
	err            error
	failRepoSuffix string
}

func (b *addFileFailingBackend) AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error) {
	if b.failRepoSuffix != "" && strings.HasSuffix(f.OwningRepo, b.failRepoSuffix) {
		if _, err := io.Copy(io.Discard, ro); err != nil {
			return nil, err
		}
		return nil, b.err
	}
	return b.inner.AddFile(ctx, f, ro)
}

func (b *addFileFailingBackend) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	return b.inner.ReadFile(ctx, f)
}

func (b *addFileFailingBackend) BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error) {
	return b.inner.BlobRedirectURL(ctx, f)
}
func (b *addFileFailingBackend) ListTags(ctx context.Context, repo string) ([]string, error) {
	return b.inner.ListTags(ctx, repo)
}
func (b *addFileFailingBackend) ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error) {
	return b.inner.ListFiles(ctx, repo)
}
func (b *addFileFailingBackend) AppendRefs(ctx context.Context, repo, canonicalTag string, refs ...string) error {
	return b.inner.AppendRefs(ctx, repo, canonicalTag, refs...)
}
func (b *addFileFailingBackend) DeleteRepoFiles(ctx context.Context, repo string) error {
	return b.inner.DeleteRepoFiles(ctx, repo)
}
func (b *addFileFailingBackend) DeleteTagFiles(ctx context.Context, repo, tag string) error {
	return b.inner.DeleteTagFiles(ctx, repo, tag)
}

// failOnNthWriter errors on the failAt-th Write (1-indexed) and on
// every Write thereafter. It records the total number of Write calls
// it received so callers can assert "no further calls after failure".
type failOnNthWriter struct {
	failAt int
	writes int
}

func (f *failOnNthWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes >= f.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// deadClientRecorder is an http.ResponseWriter whose Write always
// fails, simulating a TCP-level client disconnect after headers but
// before body. It still records headers / status via the embedded
// httptest.ResponseRecorder so tests can inspect them.
type deadClientRecorder struct {
	*httptest.ResponseRecorder
}

func (d *deadClientRecorder) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// newOCIIndexCache builds an indexcache.Cache backed by in-memory
// per-repo stores so tests exercise the real Put/Get code path
// without standing up a registry server.
func newOCIIndexCache(t *testing.T) (*indexcache.Cache, *url.URL, func()) {
	t.Helper()
	c, err := indexcache.NewInMemoryCache()
	if err != nil {
		t.Fatalf("NewInMemoryCache: %v", err)
	}
	return c, &url.URL{Scheme: "http", Host: "test.local"}, func() {}
}
