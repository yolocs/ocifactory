package maven

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/filter"
	proxymaven "github.com/yolocs/ocifactory/pkg/proxy/maven"
)

type fakeProxyFetcher struct {
	mu sync.Mutex

	metadata    map[string]*proxymaven.MetadataResponse
	metadataErr map[string]error
	files       map[string]*proxymaven.FileResponse
	fileErr     map[string]error

	getMetadataCalls int
	fetchFileCalls   int
}

func newFakeProxyFetcher() *fakeProxyFetcher {
	return &fakeProxyFetcher{
		metadata:    map[string]*proxymaven.MetadataResponse{},
		metadataErr: map[string]error{},
		files:       map[string]*proxymaven.FileResponse{},
		fileErr:     map[string]error{},
	}
}

func (f *fakeProxyFetcher) GetMetadata(_ context.Context, repoPath string) (*proxymaven.MetadataResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getMetadataCalls++
	if err, ok := f.metadataErr[repoPath]; ok {
		return nil, err
	}
	if r, ok := f.metadata[repoPath]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("fake: no metadata for %q: %w", repoPath, proxy.ErrNotFound)
}

func (f *fakeProxyFetcher) FetchFile(_ context.Context, repoPath, version, filename string) (*proxymaven.FileResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchFileCalls++
	key := repoPath + "/" + version + "/" + filename
	if err, ok := f.fileErr[key]; ok {
		return nil, err
	}
	if r, ok := f.files[key]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, fmt.Errorf("fake: no file for %q: %w", key, proxy.ErrNotFound)
}

func (f *fakeProxyFetcher) FetchPath(_ context.Context, repoPath, filename string) (*proxymaven.FileResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchFileCalls++
	key := repoPath + "/" + filename
	if err, ok := f.fileErr[key]; ok {
		return nil, err
	}
	if r, ok := f.files[key]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, fmt.Errorf("fake: no file for %q: %w", key, proxy.ErrNotFound)
}

func (f *fakeProxyFetcher) calls() (metadata, files int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getMetadataCalls, f.fetchFileCalls
}

func newProxyTestHandler(t *testing.T, fetcher ProxyFetcher, spec namespace.Spec, opts ...Option) (*Handler, *namespace.Store, *oci.FakeRegistry) {
	t.Helper()
	fake := oci.NewFakeRegistry()
	store := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, store, namespace.WithPolicyCacheTTL(0))

	if spec.Mode == "" {
		spec.Mode = namespace.ModeProxy
	}
	if spec.Proxy.Upstream == "" {
		spec.Proxy.Upstream = "https://repo.maven.apache.org/maven2"
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
	}
	allOpts = append(allOpts, opts...)
	h, err := NewHandler(reg, allOpts...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, store, fake
}

func TestProxy_FileMissFetchesAndCaches(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	body := "jar-bytes"
	fetcher.metadata["com/example/demo"] = &proxymaven.MetadataResponse{
		Body:        []byte(`<metadata><versioning><versions><version>1.2.0</version></versions><lastUpdated>20260516040506</lastUpdated></versioning></metadata>`),
		ContentType: "text/xml",
		Versions:    []string{"1.2.0"},
		LastUpdated: time.Date(2026, 5, 16, 4, 5, 6, 0, time.UTC),
	}
	fetcher.files["com/example/demo/1.2.0/demo-1.2.0.jar"] = &proxymaven.FileResponse{
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentType:   "application/java-archive",
		ContentLength: int64(len(body)),
	}
	h, _, backing := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body=%q, want %q", got, body)
	}
	files, err := backing.ListFiles(t.Context(), testNS+"/packages/com/example/demo")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("cached files = %d, want 1", len(files))
	}
	if got, want := files[0].Name, "demo-1.2.0.jar"; got != want {
		t.Errorf("cached file name = %q, want %q", got, want)
	}

	req = httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"), nil)
	rec = httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("cached body=%q, want %q", got, body)
	}
	_, gotFileCalls := fetcher.calls()
	if gotFileCalls != 1 {
		t.Errorf("FetchFile calls=%d, want 1 cache fill", gotFileCalls)
	}
}

func TestProxy_FileCacheHitSkipsUpstream(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	h, _, backing := newProxyTestHandler(t, fetcher, namespace.Spec{})
	if _, err := backing.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: testNS + "/packages/com/example/demo",
		OwningTag:  "1.2.0",
		Name:       "demo-1.2.0.jar",
		MediaType:  "application/java-archive",
	}, strings.NewReader("cached-jar")); err != nil {
		t.Fatalf("seed AddFile: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "cached-jar" {
		t.Errorf("body=%q, want cached-jar", got)
	}
	if gotMetadata, gotFiles := fetcher.calls(); gotMetadata != 0 || gotFiles != 0 {
		t.Errorf("upstream calls metadata=%d files=%d, want 0/0", gotMetadata, gotFiles)
	}
}

func TestProxy_MetadataPassthrough(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	const releaseMeta = `<metadata><groupId>com.example</groupId><artifactId>demo</artifactId></metadata>`
	const snapshotMeta = `<metadata><version>1.0-SNAPSHOT</version><versioning><snapshotVersions></snapshotVersions></versioning></metadata>`
	fetcher.metadata["com/example/demo"] = &proxymaven.MetadataResponse{
		Body:        []byte(releaseMeta),
		ContentType: "text/xml",
	}
	fetcher.metadata["com/example/demo/1.0-SNAPSHOT"] = &proxymaven.MetadataResponse{
		Body:        []byte(snapshotMeta),
		ContentType: "text/xml",
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	tests := []struct {
		name     string
		path     string
		wantBody string
	}{
		{name: "release metadata", path: nsPath("/com/example/demo/maven-metadata.xml"), wantBody: releaseMeta},
		{name: "snapshot metadata", path: nsPath("/com/example/demo/1.0-SNAPSHOT/maven-metadata.xml"), wantBody: snapshotMeta},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body=%q, want %q", got, tc.wantBody)
			}
		})
	}
}

func TestProxy_MetadataChecksumSidecarFetchesAndCaches(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	const checksum = "0123456789abcdef"
	fetcher.files["com/example/demo/maven-metadata.xml.sha1"] = &proxymaven.FileResponse{
		Body:          io.NopCloser(strings.NewReader(checksum)),
		ContentType:   "text/plain",
		ContentLength: int64(len(checksum)),
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/maven-metadata.xml.sha1"), nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d, want 200 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != checksum {
			t.Errorf("request %d: body=%q, want %q", i+1, got, checksum)
		}
	}

	_, gotFileCalls := fetcher.calls()
	if gotFileCalls != 1 {
		t.Errorf("FetchPath calls=%d, want 1 cache fill", gotFileCalls)
	}
}

func TestProxy_SnapshotMetadataChecksumSidecarFetchesFromSnapshotPath(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	const checksum = "fedcba9876543210"
	fetcher.files["com/example/demo/1.0-SNAPSHOT/maven-metadata.xml.sha1"] = &proxymaven.FileResponse{
		Body:          io.NopCloser(strings.NewReader(checksum)),
		ContentType:   "text/plain",
		ContentLength: int64(len(checksum)),
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

	req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.0-SNAPSHOT/maven-metadata.xml.sha1"), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != checksum {
		t.Errorf("body=%q, want %q", got, checksum)
	}
}

func TestProxy_UploadsReturn405(t *testing.T) {
	t.Parallel()

	h, _, _ := newProxyTestHandler(t, newFakeProxyFetcher(), namespace.Spec{})

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "artifact put", method: http.MethodPut, path: nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar")},
		{name: "metadata post", method: http.MethodPost, path: nsPath("/com/example/demo/maven-metadata.xml")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("body"))
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status=%d, want 405 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProxy_FilterDenySkipsUpstream(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{Proxy: namespace.Proxy{
		Filters: filter.Filters{&filter.Denylist{Patterns: []string{"com.example:demo"}}},
	}})

	req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if gotMetadata, gotFiles := fetcher.calls(); gotMetadata != 0 || gotFiles != 0 {
		t.Errorf("upstream calls metadata=%d files=%d, want 0/0", gotMetadata, gotFiles)
	}
}

func TestProxy_DelayFilterFailsClosedWithoutLastUpdated(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.metadata["com/example/demo"] = &proxymaven.MetadataResponse{Versions: []string{"1.2.0"}}
	fetcher.files["com/example/demo/1.2.0/demo-1.2.0.jar"] = &proxymaven.FileResponse{
		Body:          io.NopCloser(strings.NewReader("jar")),
		ContentType:   "application/java-archive",
		ContentLength: 3,
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{Proxy: namespace.Proxy{
		Filters: filter.Filters{&filter.Delay{MinAge: time.Hour}},
	}})

	req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if gotMetadata, gotFiles := fetcher.calls(); gotMetadata != 1 || gotFiles != 0 {
		t.Errorf("upstream calls metadata=%d files=%d, want 1/0", gotMetadata, gotFiles)
	}
}

func TestProxy_FileExceedingSizeCapIsRejectedBeforeFetchBody(t *testing.T) {
	t.Parallel()

	fetcher := newFakeProxyFetcher()
	fetcher.metadata["com/example/demo"] = &proxymaven.MetadataResponse{Versions: []string{"1.2.0"}}
	fetcher.files["com/example/demo/1.2.0/demo-1.2.0.jar"] = &proxymaven.FileResponse{
		Body:          io.NopCloser(strings.NewReader("too-large")),
		ContentType:   "application/java-archive",
		ContentLength: 9,
	}
	h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{}, WithMaxUploadBytes(8))

	req := httptest.NewRequest(http.MethodGet, nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestProxy_UpstreamErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(*fakeProxyFetcher)
		path       string
		wantStatus int
	}{
		{
			name: "file not found",
			setup: func(f *fakeProxyFetcher) {
				f.metadata["com/example/demo"] = &proxymaven.MetadataResponse{Versions: []string{"1.2.0"}}
				f.fileErr["com/example/demo/1.2.0/demo-1.2.0.jar"] = fmt.Errorf("missing: %w", proxy.ErrNotFound)
			},
			path:       nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"),
			wantStatus: http.StatusNotFound,
		},
		{
			name: "file upstream unavailable",
			setup: func(f *fakeProxyFetcher) {
				f.metadata["com/example/demo"] = &proxymaven.MetadataResponse{Versions: []string{"1.2.0"}}
				f.fileErr["com/example/demo/1.2.0/demo-1.2.0.jar"] = fmt.Errorf("down: %w", proxy.ErrUpstreamUnavailable)
			},
			path:       nsPath("/com/example/demo/1.2.0/demo-1.2.0.jar"),
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "metadata upstream unavailable",
			setup: func(f *fakeProxyFetcher) {
				f.metadataErr["com/example/demo"] = fmt.Errorf("down: %w", proxy.ErrUpstreamUnavailable)
			},
			path:       nsPath("/com/example/demo/maven-metadata.xml"),
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeProxyFetcher()
			tc.setup(fetcher)
			h, _, _ := newProxyTestHandler(t, fetcher, namespace.Spec{})

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
