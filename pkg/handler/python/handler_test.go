package python

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// countingRegistry wraps an *oci.FakeRegistry and tracks the number of
// times each method on the handler.Registry interface is invoked. It
// exists so the simple-index cache tests can assert that a
// /simple/<pkg>/ request served from cache does not fall through to the
// backend, without reaching for mock libraries (per AGENTS.md: fakes,
// not mocks).
type countingRegistry struct {
	*oci.FakeRegistry
	addFile   atomic.Int64
	readFile  atomic.Int64
	listTags  atomic.Int64
	listFiles atomic.Int64
}

func newCountingRegistry() *countingRegistry {
	return &countingRegistry{FakeRegistry: oci.NewFakeRegistry()}
}

func (r *countingRegistry) AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error) {
	r.addFile.Add(1)
	return r.FakeRegistry.AddFile(ctx, f, ro)
}

func (r *countingRegistry) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	r.readFile.Add(1)
	return r.FakeRegistry.ReadFile(ctx, f)
}

func (r *countingRegistry) ListTags(ctx context.Context, repo string) ([]string, error) {
	r.listTags.Add(1)
	return r.FakeRegistry.ListTags(ctx, repo)
}

func (r *countingRegistry) ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error) {
	r.listFiles.Add(1)
	return r.FakeRegistry.ListFiles(ctx, repo)
}

func TestDetectMediaType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "wheel",
			filename: "example-pkg-1.0.0.whl",
			want:     "application/x-wheel+zip",
		},
		{
			name:     "tar.gz",
			filename: "example-pkg-1.0.0.tar.gz",
			want:     "application/x-gzip",
		},
		{
			name:     "python file",
			filename: "setup.py",
			want:     "text/x-python",
		},
		{
			name:     "unknown",
			filename: "example-pkg-1.0.0.unknown",
			want:     "application/octet-stream",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := detectMediaType(tc.filename)
			if got != tc.want {
				t.Errorf("detectMediaType(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

func TestHandlePut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		pkgName    string
		version    string
		filename   string
		content    string
		wantStatus int
		wantFile   bool
		wantIndex  bool
	}{
		{
			name:       "valid wheel",
			pkgName:    "example-pkg",
			version:    "1.0.0",
			filename:   "example-pkg-1.0.0.whl",
			content:    "wheel content",
			wantStatus: http.StatusCreated,
			wantFile:   true,
			wantIndex:  true,
		},
		{
			name:       "valid tar.gz",
			pkgName:    "example-pkg",
			version:    "1.0.0",
			filename:   "example-pkg-1.0.0.tar.gz",
			content:    "tarball content",
			wantStatus: http.StatusCreated,
			wantFile:   true,
			wantIndex:  true,
		},
		{
			name:       "invalid package name",
			pkgName:    "invalid/pkg",
			version:    "1.0.0",
			filename:   "example-pkg-1.0.0.whl",
			content:    "content",
			wantStatus: http.StatusBadRequest,
			wantFile:   false,
			wantIndex:  false,
		},
		{
			name:       "missing package name",
			pkgName:    "",
			version:    "1.0.0",
			filename:   "example-pkg-1.0.0.whl",
			content:    "content",
			wantStatus: http.StatusBadRequest,
			wantFile:   false,
			wantIndex:  false,
		},
		{
			name:       "missing version",
			pkgName:    "example-pkg",
			version:    "",
			filename:   "example-pkg-1.0.0.whl",
			content:    "content",
			wantStatus: http.StatusBadRequest,
			wantFile:   false,
			wantIndex:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, registry)

			// Create a multipart form request
			var b bytes.Buffer
			w := multipart.NewWriter(&b)

			// Add package name field
			if tc.pkgName != "" {
				if err := w.WriteField("name", tc.pkgName); err != nil {
					t.Fatalf("Failed to write package name field: %v", err)
				}
			}

			// Add version field
			if tc.version != "" {
				if err := w.WriteField("version", tc.version); err != nil {
					t.Fatalf("Failed to write version field: %v", err)
				}
			}

			// Add content file
			if tc.filename != "" && tc.content != "" {
				fw, err := w.CreateFormFile("content", tc.filename)
				if err != nil {
					t.Fatalf("Failed to create form file: %v", err)
				}
				if _, err := fw.Write([]byte(tc.content)); err != nil {
					t.Fatalf("Failed to write content: %v", err)
				}
			}

			// Close the writer
			if err := w.Close(); err != nil {
				t.Fatalf("Failed to close multipart writer: %v", err)
			}

			// Create the request
			req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
			req.Header.Set("Content-Type", w.FormDataContentType())

			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)

			if got, want := resp.Code, tc.wantStatus; got != want {
				t.Errorf("Status code = %d, want %d (body=%s)", got, want, resp.Body.String())
			}

			if tc.wantFile {
				// Verify package file was created under the namespaced prefix.
				key := testNS + "/packages/" + tc.pkgName + "/" + tc.version + "/" + tc.filename
				content, ok := registry.Files[key]
				if !ok {
					t.Errorf("Package file not found in registry: %s", key)
				} else if string(content) != tc.content {
					t.Errorf("Package file content = %q, want %q", string(content), tc.content)
				}
			}

			if tc.wantIndex {
				// Verify the per-package sentinel was written under
				// <ns>/<format-index>/<pkgName>. The body is a constant
				// placeholder, not the version, so the OCI backend
				// deduplicates the blob across uploads.
				indexKey := testNS + "/" + packageIndexName + "/" + tc.pkgName + "/" + indexSentinelName
				indexContent, ok := registry.Files[indexKey]
				if !ok {
					t.Errorf("Index sentinel not found in registry: %s", indexKey)
				} else if string(indexContent) != indexSentinelContent {
					t.Errorf("Index sentinel content = %q, want %q", string(indexContent), indexSentinelContent)
				}

				// The version string should never appear as a layer name in
				// the index repo — that was the per-version write the new
				// sentinel approach replaces.
				perVersionKey := testNS + "/" + packageIndexName + "/" + tc.pkgName + "/" + tc.version
				if _, ok := registry.Files[perVersionKey]; ok {
					t.Errorf("Per-version index layer must not exist: %s", perVersionKey)
				}
			}
		})
	}
}

func TestHandleGet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		setupFile  *oci.RepoFile
		setupData  string
		path       string
		method     string
		wantStatus int
		wantBody   string
	}{
		{
			name: "get existing wheel",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/example-pkg",
				OwningTag:  "1.0.0",
				Name:       "example-pkg-1.0.0.whl",
				MediaType:  "application/x-wheel+zip",
			},
			setupData:  "wheel content",
			path:       "/" + testNS + "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "wheel content",
		},
		{
			name: "head existing wheel",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/example-pkg",
				OwningTag:  "1.0.0",
				Name:       "example-pkg-1.0.0.whl",
				MediaType:  "application/x-wheel+zip",
			},
			setupData:  "wheel content",
			path:       "/" + testNS + "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl",
			method:     http.MethodHead,
			wantStatus: http.StatusOK,
			wantBody:   "",
		},
		{
			name:       "file not found",
			path:       "/" + testNS + "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl",
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid path",
			path:       "/" + testNS + "/packages/example-pkg",
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			if tc.setupFile != nil {
				_, err := registry.AddFile(t.Context(), tc.setupFile, strings.NewReader(tc.setupData))
				if err != nil {
					t.Fatalf("Failed to set up file: %v", err)
				}
			}

			h, _ := newTestHandler(t, registry)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()

			h.Mux().ServeHTTP(w, req)

			if got, want := w.Code, tc.wantStatus; got != want {
				t.Errorf("Status code = %d, want %d", got, want)
			}

			if tc.wantStatus == http.StatusOK {
				if tc.method == http.MethodGet && w.Body.String() != tc.wantBody {
					t.Errorf("Body = %q, want %q", w.Body.String(), tc.wantBody)
				}

				if tc.setupFile != nil {
					if got, want := w.Header().Get("Content-Type"), tc.setupFile.MediaType; got != want {
						t.Errorf("Content-Type = %q, want %q", got, want)
					}
				}
			}
		})
	}
}

// redirectingRegistry wraps an *oci.FakeRegistry and lets a test
// program BlobRedirectURL's return values, exercising the handler's
// redirect-vs-stream branching without needing an HTTP-level fake of
// the OCI backend.
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

func TestHandleGet_BlobRedirect(t *testing.T) {
	t.Parallel()

	setupFile := &oci.RepoFile{
		OwningRepo: testNS + "/packages/example-pkg",
		OwningTag:  "1.0.0",
		Name:       "example-pkg-1.0.0.whl",
		MediaType:  "application/x-wheel+zip",
	}
	const setupData = "wheel content"
	reqPath := "/" + testNS + "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl"
	const presigned = "https://cdn.example.com/blob?signature=xyz"

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
		{
			name:           "GET redirects when backend returns presigned URL",
			method:         http.MethodGet,
			redirectURL:    presigned,
			wantStatus:     http.StatusTemporaryRedirect,
			wantLocation:   presigned,
			wantProbeCalls: 1,
		},
		{
			name:           "GET falls through to streaming when redirect URL is empty",
			method:         http.MethodGet,
			redirectURL:    "",
			wantStatus:     http.StatusOK,
			wantBody:       setupData,
			wantProbeCalls: 1,
		},
		{
			name:           "GET falls through to streaming on probe error",
			method:         http.MethodGet,
			redirectErr:    errors.New("transient backend failure"),
			wantStatus:     http.StatusOK,
			wantBody:       setupData,
			wantProbeCalls: 1,
		},
		{
			name:           "HEAD never probes for redirect",
			method:         http.MethodHead,
			redirectURL:    presigned,
			wantStatus:     http.StatusOK,
			wantProbeCalls: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := oci.NewFakeRegistry()
			if _, err := fake.AddFile(t.Context(), setupFile, strings.NewReader(setupData)); err != nil {
				t.Fatalf("setup AddFile: %v", err)
			}
			reg := &redirectingRegistry{
				FakeRegistry: fake,
				redirectURL:  tc.redirectURL,
				redirectErr:  tc.redirectErr,
			}

			h, _ := newTestHandler(t, reg)

			req := httptest.NewRequest(tc.method, reqPath, nil)
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, req)

			if got, want := w.Code, tc.wantStatus; got != want {
				t.Errorf("status = %d, want %d", got, want)
			}
			if got, want := w.Header().Get("Location"), tc.wantLocation; got != want {
				t.Errorf("Location = %q, want %q", got, want)
			}
			if tc.method == http.MethodGet && tc.wantBody != "" {
				if got := w.Body.String(); got != tc.wantBody {
					t.Errorf("body = %q, want %q", got, tc.wantBody)
				}
			}
			if got, want := reg.calls.Load(), tc.wantProbeCalls; got != want {
				t.Errorf("BlobRedirectURL calls = %d, want %d", got, want)
			}
		})
	}
}

// TestPackageIndexCache exercises the per-package simple-index cache
// end-to-end through the HTTP mux: repeated /simple/<pkg>/ requests
// within the TTL must not fall through to Registry.ListFiles, the entry
// must expire after the TTL, and a successful upload must invalidate
// the entry for that package only.
func TestPackageIndexCache(t *testing.T) {
	t.Parallel()

	t.Run("repeated reads within TTL hit the cache", func(t *testing.T) {
		t.Parallel()

		reg := newCountingRegistry()
		h, _ := newTestHandler(t, reg, WithSimpleIndexCacheTTL(time.Minute))
		// Seed one published file so the first /simple/<pkg>/ render is
		// non-empty.
		if code := uploadPackage(t, h, "requests", "1.0.0"); code != http.StatusCreated {
			t.Fatalf("seed upload: status=%d", code)
		}
		// Reset the counters after seeding so the assertion measures
		// the simple-index path only — the upload + namespace wrapper
		// each touch ListFiles/ListTags for their own bookkeeping.
		listFilesAfterSeed := reg.listFiles.Load()

		simple := func() {
			req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("/%s/simple/requests/ status=%d body=%q", testNS, resp.Code, resp.Body.String())
			}
		}

		simple()
		afterFirstRender := reg.listFiles.Load()
		if got, want := afterFirstRender-listFilesAfterSeed, int64(1); got != want {
			t.Fatalf("ListFiles after first render = %d, want %d", got, want)
		}
		for i := 0; i < 5; i++ {
			simple()
		}
		if got, want := reg.listFiles.Load(), afterFirstRender; got != want {
			t.Errorf("ListFiles after 6 renders = %d, want %d (subsequent renders should hit the cache)", got, want)
		}
	})

	t.Run("entry expires after TTL", func(t *testing.T) {
		t.Parallel()

		const ttl = 50 * time.Millisecond
		reg := newCountingRegistry()
		h, _ := newTestHandler(t, reg, WithSimpleIndexCacheTTL(ttl))

		if code := uploadPackage(t, h, "requests", "1.0.0"); code != http.StatusCreated {
			t.Fatalf("seed upload: status=%d", code)
		}
		listFilesAfterSeed := reg.listFiles.Load()

		render := func() {
			req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d", resp.Code)
			}
		}

		render() // miss → ListFiles+=1
		render() // hit  → still ListFiles+=1
		time.Sleep(3 * ttl)
		render() // expired → ListFiles+=2

		if got, want := reg.listFiles.Load()-listFilesAfterSeed, int64(2); got != want {
			t.Errorf("ListFiles delta = %d, want %d (one initial miss + one post-expiry miss)", got, want)
		}
	})

	t.Run("successful upload invalidates only the affected package", func(t *testing.T) {
		t.Parallel()

		reg := newCountingRegistry()
		h, _ := newTestHandler(t, reg, WithSimpleIndexCacheTTL(time.Minute))

		if code := uploadPackage(t, h, "requests", "1.0.0"); code != http.StatusCreated {
			t.Fatalf("upload requests: %d", code)
		}
		if code := uploadPackage(t, h, "flask", "2.3.0"); code != http.StatusCreated {
			t.Fatalf("upload flask: %d", code)
		}
		listFilesAfterSeed := reg.listFiles.Load()

		render := func(pkg string) {
			req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/"+pkg+"/", nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("/%s/simple/%s/ status=%d", testNS, pkg, resp.Code)
			}
		}

		// Prime the cache for both packages.
		render("requests")
		render("flask")
		afterPrime := reg.listFiles.Load() - listFilesAfterSeed
		if afterPrime != 2 {
			t.Fatalf("ListFiles delta after priming both = %d, want 2", afterPrime)
		}

		// A new version of requests must drop only that entry.
		if code := uploadPackage(t, h, "requests", "1.0.1"); code != http.StatusCreated {
			t.Fatalf("upload requests 1.0.1: %d", code)
		}
		listFilesAfterReup := reg.listFiles.Load()

		render("requests") // miss → +1
		render("flask")    // still cached → unchanged

		if got, want := reg.listFiles.Load()-listFilesAfterReup, int64(1); got != want {
			t.Errorf("ListFiles delta after invalidation = %d, want %d", got, want)
		}
	})

	t.Run("ttl=0 disables caching", func(t *testing.T) {
		t.Parallel()

		reg := newCountingRegistry()
		h, _ := newTestHandler(t, reg, WithSimpleIndexCacheTTL(0))
		if code := uploadPackage(t, h, "requests", "1.0.0"); code != http.StatusCreated {
			t.Fatalf("upload: %d", code)
		}
		listFilesAfterSeed := reg.listFiles.Load()

		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d", resp.Code)
			}
		}

		if got, want := reg.listFiles.Load()-listFilesAfterSeed, int64(3); got != want {
			t.Errorf("ListFiles delta with cache disabled = %d, want %d (one per render)", got, want)
		}
	})
}

// TestIndexSentinel covers the per-package sentinel write path: the
// index repo grows by exactly one tag per package, regardless of how
// many versions of that package have been uploaded, and
// handleSimpleIndex surfaces every uploaded package in
// /<ns>/simple/.
func TestIndexSentinel(t *testing.T) {
	t.Parallel()

	type upload struct {
		pkg     string
		version string
	}

	cases := []struct {
		name           string
		uploads        []upload
		wantIndexTags  []string
		wantIndexFiles map[string]string
	}{
		{
			name:          "first upload writes sentinel",
			uploads:       []upload{{pkg: "requests", version: "1.0.0"}},
			wantIndexTags: []string{"requests"},
			wantIndexFiles: map[string]string{
				testNS + "/" + packageIndexName + "/requests/" + indexSentinelName: indexSentinelContent,
			},
		},
		{
			name: "second version of same package does not add a new index layer",
			uploads: []upload{
				{pkg: "requests", version: "1.0.0"},
				{pkg: "requests", version: "1.0.1"},
				{pkg: "requests", version: "2.0.0"},
			},
			wantIndexTags: []string{"requests"},
			wantIndexFiles: map[string]string{
				testNS + "/" + packageIndexName + "/requests/" + indexSentinelName: indexSentinelContent,
			},
		},
		{
			name: "different packages each get their own sentinel tag",
			uploads: []upload{
				{pkg: "requests", version: "1.0.0"},
				{pkg: "flask", version: "2.3.0"},
				{pkg: "requests", version: "1.0.1"},
				{pkg: "django", version: "5.0.0"},
			},
			wantIndexTags: []string{"django", "flask", "requests"},
			wantIndexFiles: map[string]string{
				testNS + "/" + packageIndexName + "/requests/" + indexSentinelName: indexSentinelContent,
				testNS + "/" + packageIndexName + "/flask/" + indexSentinelName:    indexSentinelContent,
				testNS + "/" + packageIndexName + "/django/" + indexSentinelName:   indexSentinelContent,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, registry)

			for _, up := range tc.uploads {
				if code := uploadPackage(t, h, up.pkg, up.version); code != http.StatusCreated {
					t.Fatalf("upload %q@%q: status = %d, want %d", up.pkg, up.version, code, http.StatusCreated)
				}
			}

			gotIndexTags := append([]string{}, registry.Tags[testNS+"/"+packageIndexName]...)
			sort.Strings(gotIndexTags)
			if diff := cmp.Diff(tc.wantIndexTags, gotIndexTags); diff != "" {
				t.Errorf("index repo tags mismatch (-want +got):\n%s", diff)
			}

			gotIndexFiles := map[string]string{}
			for k, v := range registry.Files {
				if strings.HasPrefix(k, testNS+"/"+packageIndexName+"/") {
					gotIndexFiles[k] = string(v)
				}
			}
			if diff := cmp.Diff(tc.wantIndexFiles, gotIndexFiles); diff != "" {
				t.Errorf("index repo files mismatch (-want +got):\n%s", diff)
			}

			// handleSimpleIndex must list every uploaded package, regardless
			// of how many versions each has.
			req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)
			if got, want := resp.Code, http.StatusOK; got != want {
				t.Fatalf("/%s/simple/ status = %d, want %d", testNS, got, want)
			}
			body := resp.Body.String()
			for _, pkg := range tc.wantIndexTags {
				if !strings.Contains(body, "/"+testNS+"/simple/"+pkg+"/") {
					t.Errorf("/%s/simple/ body missing link for %q, got:\n%s", testNS, pkg, body)
				}
			}
		})
	}
}

// uploadPackage performs a multipart twine-style upload of a package
// version through the python handler under the test-ns namespace and
// returns the response status.
func uploadPackage(t *testing.T, h *Handler, pkgName, version string) int {
	t.Helper()
	return uploadPackageInNS(t, h, testNS, pkgName, version)
}

// uploadPackageInNS uploads to a specific namespace; used by the
// cross-namespace isolation tests in namespace_test.go.
func uploadPackageInNS(t *testing.T, h *Handler, ns, pkgName, version string) int {
	t.Helper()

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("name", pkgName); err != nil {
		t.Fatalf("write name field: %v", err)
	}
	if err := w.WriteField("version", version); err != nil {
		t.Fatalf("write version field: %v", err)
	}
	fw, err := w.CreateFormFile("content", pkgName+"-"+version+".whl")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte("payload-" + pkgName + "-" + version)); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/"+ns+"/", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp := httptest.NewRecorder()
	h.Mux().ServeHTTP(resp, req)
	return resp.Code
}

func TestHandleSimpleIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		setupTags        []string
		path             string
		wantStatus       int
		wantBodyContains []string
	}{
		{
			name:             "empty index",
			setupTags:        []string{},
			path:             "/" + testNS + "/simple/",
			wantStatus:       http.StatusOK,
			wantBodyContains: []string{"Simple Index"},
		},
		{
			name:       "index with packages",
			setupTags:  []string{"package1", "package2", "example-pkg"},
			path:       "/" + testNS + "/simple/",
			wantStatus: http.StatusOK,
			wantBodyContains: []string{
				"Simple Index",
				"package1",
				"package2",
				"example-pkg",
				"/" + testNS + "/simple/package1/",
				"/" + testNS + "/simple/package2/",
				"/" + testNS + "/simple/example-pkg/",
			},
		},
		{
			name:       "index without trailing slash",
			setupTags:  []string{"package1", "package2"},
			path:       "/" + testNS + "/simple",
			wantStatus: http.StatusOK,
			wantBodyContains: []string{
				"Simple Index",
				"package1",
				"package2",
				"/" + testNS + "/simple/package1/",
				"/" + testNS + "/simple/package2/",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			registry.Tags[testNS+"/"+packageIndexName] = append(registry.Tags[testNS+"/"+packageIndexName], tc.setupTags...)

			h, _ := newTestHandler(t, registry)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)

			if got, want := resp.Code, tc.wantStatus; got != want {
				t.Errorf("Status code = %d, want %d", got, want)
			}

			body := resp.Body.String()
			for _, wantContent := range tc.wantBodyContains {
				if !strings.Contains(body, wantContent) {
					t.Errorf("Response body does not contain %q, got: %s", wantContent, body)
				}
			}
		})
	}
}

// TestPEP503Normalization round-trips upload + simple-index lookup
// across every separator variant. Per PEP 503, `Foo_Bar`, `foo-bar`,
// and `foo.bar` are the same package; uploading any one and querying
// any other must resolve.
func TestPEP503Normalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		uploadAs   string
		queryAs    string
		wantStored string // canonical OwningRepo path under packages/ (namespace prefix added by test)
	}{
		{name: "underscore upload, dash query", uploadAs: "Foo_Bar", queryAs: "foo-bar", wantStored: "packages/foo-bar"},
		{name: "dot upload, dash query", uploadAs: "Foo.Bar", queryAs: "foo-bar", wantStored: "packages/foo-bar"},
		{name: "mixed upload, dash query", uploadAs: "Foo._-_Bar", queryAs: "foo-bar", wantStored: "packages/foo-bar"},
		{name: "uppercase upload, lowercase query", uploadAs: "REQUESTS", queryAs: "requests", wantStored: "packages/requests"},
		{name: "dash upload, dot query", uploadAs: "foo-bar", queryAs: "foo.bar", wantStored: "packages/foo-bar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg)

			if code := uploadPackage(t, h, tc.uploadAs, "1.0.0"); code != http.StatusCreated {
				t.Fatalf("upload: status=%d", code)
			}

			// Stored under the normalized repo, regardless of upload casing.
			storedKey := testNS + "/" + tc.wantStored + "/1.0.0/" + tc.uploadAs + "-1.0.0.whl"
			if _, ok := reg.Files[storedKey]; !ok {
				t.Errorf("missing stored file at normalized path %q; got files: %v", storedKey, registryFileKeys(reg))
			}
			// And the index sentinel is at the normalized name.
			normalized := normalize(tc.uploadAs)
			if _, ok := reg.Files[testNS+"/"+packageIndexName+"/"+normalized+"/"+indexSentinelName]; !ok {
				t.Errorf("missing index sentinel under normalized name %q", normalized)
			}

			// /<ns>/simple/<queryAs>/ resolves to the same files as /<ns>/simple/<normalized>/.
			req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/"+tc.queryAs+"/", nil)
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("/%s/simple/%s/ status=%d body=%s", testNS, tc.queryAs, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.uploadAs+"-1.0.0.whl") {
				t.Errorf("body missing uploaded filename: %s", body)
			}
		})
	}
}

func registryFileKeys(reg *oci.FakeRegistry) []string {
	keys := make([]string, 0, len(reg.Files))
	for k := range reg.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestMultipartFieldOrder pins down the ordering contract of the
// streaming upload path: metadata fields must precede the content
// part.
func TestMultipartFieldOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		write      func(t *testing.T, mw *multipart.Writer)
		wantStatus int
		wantStored bool
	}{
		{
			name: "fields before content succeeds",
			write: func(t *testing.T, mw *multipart.Writer) {
				if err := mw.WriteField("version", "1.0.0"); err != nil {
					t.Fatalf("write version: %v", err)
				}
				if err := mw.WriteField("name", "example-pkg"); err != nil {
					t.Fatalf("write name: %v", err)
				}
				fw, err := mw.CreateFormFile("content", "example-pkg-1.0.0.whl")
				if err != nil {
					t.Fatalf("create form file: %v", err)
				}
				if _, err := fw.Write([]byte("payload")); err != nil {
					t.Fatalf("write content: %v", err)
				}
			},
			wantStatus: http.StatusCreated,
			wantStored: true,
		},
		{
			name: "content before fields rejected",
			write: func(t *testing.T, mw *multipart.Writer) {
				fw, err := mw.CreateFormFile("content", "example-pkg-1.0.0.whl")
				if err != nil {
					t.Fatalf("create form file: %v", err)
				}
				if _, err := fw.Write([]byte("payload")); err != nil {
					t.Fatalf("write content: %v", err)
				}
				if err := mw.WriteField("name", "example-pkg"); err != nil {
					t.Fatalf("write name: %v", err)
				}
				if err := mw.WriteField("version", "1.0.0"); err != nil {
					t.Fatalf("write version: %v", err)
				}
			},
			wantStatus: http.StatusBadRequest,
			wantStored: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg)

			var b bytes.Buffer
			mw := multipart.NewWriter(&b)
			tc.write(t, mw)
			if err := mw.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)

			if got, want := rec.Code, tc.wantStatus; got != want {
				t.Errorf("status = %d, want %d (body=%s)", got, want, rec.Body.String())
			}
			_, stored := reg.Files[testNS+"/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl"]
			if stored != tc.wantStored {
				t.Errorf("stored = %t, want %t (keys=%v)", stored, tc.wantStored, registryFileKeys(reg))
			}
		})
	}
}

// TestSimpleIndexJSON verifies PEP 691 content negotiation: the same
// /<ns>/simple/<pkg>/ URL renders JSON when the client asks for it.
func TestSimpleIndexJSON(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg, WithSimpleIndexCacheTTL(0))

	const filename = "requests-1.0.0-py3-none-any.whl"
	if code := uploadFile(t, h, "requests", "1.0.0", filename); code != http.StatusCreated {
		t.Fatalf("upload status=%d", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/requests/", nil)
	req.Header.Set("Accept", contentTypeJSONv1)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), contentTypeJSONv1; got != want {
		t.Errorf("Content-Type=%q, want %q", got, want)
	}

	var got simpleIndexJSONPackage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if got.Meta.APIVersion != pypiAPIVersion {
		t.Errorf("api-version=%q, want %q", got.Meta.APIVersion, pypiAPIVersion)
	}
	if got.Name != "requests" {
		t.Errorf("name=%q, want %q", got.Name, "requests")
	}
	if len(got.Files) != 1 {
		t.Fatalf("files=%d, want 1; payload=%+v", len(got.Files), got)
	}
	f := got.Files[0]
	if f.Filename != filename {
		t.Errorf("filename=%q, want %q", f.Filename, filename)
	}
	if f.Hashes["sha256"] == "" {
		t.Errorf("missing sha256 hash; got hashes=%v", f.Hashes)
	}
	if !strings.Contains(f.URL, "/"+testNS+"/packages/requests/1.0.0/"+filename) {
		t.Errorf("file URL = %q, want to contain /%s/packages/requests/1.0.0/%s", f.URL, testNS, filename)
	}
}

// uploadFile is a minimal twine-style multipart upload helper that
// targets the test-ns namespace.
func uploadFile(t *testing.T, h *Handler, pkgName, version, filename string) int {
	t.Helper()

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if err := mw.WriteField("name", pkgName); err != nil {
		t.Fatalf("write name: %v", err)
	}
	if err := mw.WriteField("version", version); err != nil {
		t.Fatalf("write version: %v", err)
	}
	fw, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte("payload-" + filename)); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	return rec.Code
}

// TestSimpleIndexJSONList confirms the root /<ns>/simple/ endpoint
// also honours Accept negotiation and emits the PEP 691 project list
// shape.
func TestSimpleIndexJSONList(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	reg.Tags[testNS+"/"+packageIndexName] = []string{"flask", "requests"}
	h, _ := newTestHandler(t, reg)

	req := httptest.NewRequest(http.MethodGet, "/"+testNS+"/simple/", nil)
	req.Header.Set("Accept", contentTypeJSONv1)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), contentTypeJSONv1; got != want {
		t.Errorf("Content-Type=%q, want %q", got, want)
	}

	var got simpleIndexJSONList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if got.Meta.APIVersion != pypiAPIVersion {
		t.Errorf("api-version=%q, want %q", got.Meta.APIVersion, pypiAPIVersion)
	}
	wantNames := []string{"flask", "requests"}
	gotNames := make([]string, len(got.Projects))
	for i, p := range got.Projects {
		gotNames[i] = p.Name
	}
	sort.Strings(gotNames)
	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Errorf("projects mismatch (-want +got):\n%s", diff)
	}
}

// TestHandleFilePut_BadInputs covers the assorted reject paths that
// were previously untested: oversized name/version, malformed
// multipart, missing filename, traversal-style filenames.
func TestHandleFilePut_BadInputs(t *testing.T) {
	t.Parallel()

	type tweak func(t *testing.T, mw *multipart.Writer) (filename string)

	defaultBody := func(t *testing.T, mw *multipart.Writer) string {
		t.Helper()
		if err := mw.WriteField("name", "example"); err != nil {
			t.Fatalf("write name: %v", err)
		}
		if err := mw.WriteField("version", "1.0.0"); err != nil {
			t.Fatalf("write version: %v", err)
		}
		fw, err := mw.CreateFormFile("content", "example-1.0.0.tar.gz")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write([]byte("payload")); err != nil {
			t.Fatalf("write content: %v", err)
		}
		return "example-1.0.0.tar.gz"
	}

	cases := []struct {
		name       string
		ctype      string // override Content-Type to break multipart
		body       []byte // override the entire body (raw)
		write      tweak
		wantStatus int
	}{
		{
			name: "oversized package name",
			write: func(t *testing.T, mw *multipart.Writer) string {
				_ = mw.WriteField("name", strings.Repeat("a", maxPackageLength+1))
				_ = mw.WriteField("version", "1.0.0")
				fw, _ := mw.CreateFormFile("content", "x-1.0.0.tar.gz")
				_, _ = fw.Write([]byte("p"))
				return "x-1.0.0.tar.gz"
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "oversized version",
			write: func(t *testing.T, mw *multipart.Writer) string {
				_ = mw.WriteField("name", "example")
				_ = mw.WriteField("version", strings.Repeat("9", maxVersionLength+1))
				fw, _ := mw.CreateFormFile("content", "example-x.tar.gz")
				_, _ = fw.Write([]byte("p"))
				return ""
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "filename is dotdot",
			write: func(t *testing.T, mw *multipart.Writer) string {
				_ = mw.WriteField("name", "example")
				_ = mw.WriteField("version", "1.0.0")
				fw, _ := mw.CreateFormFile("content", "..")
				_, _ = fw.Write([]byte("p"))
				return ""
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "filename with backslash",
			write: func(t *testing.T, mw *multipart.Writer) string {
				_ = mw.WriteField("name", "example")
				_ = mw.WriteField("version", "1.0.0")
				fw, _ := mw.CreateFormFile("content", `evil\name.whl`)
				_, _ = fw.Write([]byte("p"))
				return ""
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "filename with control char",
			write: func(t *testing.T, mw *multipart.Writer) string {
				_ = mw.WriteField("name", "example")
				_ = mw.WriteField("version", "1.0.0")
				fw, _ := mw.CreateFormFile("content", "evil\x00.whl")
				_, _ = fw.Write([]byte("p"))
				return ""
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-multipart body",
			ctype:      "application/json",
			body:       []byte(`{"name":"example"}`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing boundary",
			ctype:      "multipart/form-data",
			body:       []byte("not actually multipart"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "happy path establishes the test harness produces 201",
			write: func(t *testing.T, mw *multipart.Writer) string {
				return defaultBody(t, mw)
			},
			wantStatus: http.StatusCreated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg)

			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(http.MethodPut, "/"+testNS+"/", bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", tc.ctype)
			} else {
				var b bytes.Buffer
				mw := multipart.NewWriter(&b)
				tc.write(t, mw)
				if err := mw.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
				req = httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
				req.Header.Set("Content-Type", mw.FormDataContentType())
			}

			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestHandleFilePut_MaxUploadBytes confirms WithMaxUploadBytes returns
// 413 for an oversized body and 201 just under the cap.
func TestHandleFilePut_MaxUploadBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cap        int64
		bodyExtra  int // bytes of payload above ~512 of multipart framing
		wantStatus int
	}{
		{name: "under cap", cap: 1 << 20, bodyExtra: 1 << 10, wantStatus: http.StatusCreated},
		{name: "over cap", cap: 1 << 14, bodyExtra: 1 << 16, wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg, WithMaxUploadBytes(tc.cap))

			var b bytes.Buffer
			mw := multipart.NewWriter(&b)
			_ = mw.WriteField("name", "example")
			_ = mw.WriteField("version", "1.0.0")
			fw, _ := mw.CreateFormFile("content", "example-1.0.0.tar.gz")
			_, _ = fw.Write(bytes.Repeat([]byte("a"), tc.bodyExtra))
			_ = mw.Close()

			req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestNoAcceptHeader_DefaultsToHTML confirms that pip 21 (and earlier)
// clients without Accept get the legacy HTML rendering, not JSON.
func TestNoAcceptHeader_DefaultsToHTML(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	reg.Tags[testNS+"/"+packageIndexName] = []string{"flask"}
	h, _ := newTestHandler(t, reg)

	for _, path := range []string{"/" + testNS + "/simple/", "/" + testNS + "/simple/flask/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("path=%q Content-Type=%q, want text/html prefix", path, got)
		}
	}
}

// TestHandleFilePut_OversizedTextField confirms the per-field byte cap
// rejects an oversized non-file form part with 413.
func TestHandleFilePut_OversizedTextField(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if err := mw.WriteField("name", strings.Repeat("a", maxTextFieldBytes+1)); err != nil {
		t.Fatalf("write name: %v", err)
	}
	if err := mw.WriteField("version", "1.0.0"); err != nil {
		t.Fatalf("write version: %v", err)
	}
	fw, _ := mw.CreateFormFile("content", "x-1.0.0.tar.gz")
	_, _ = fw.Write([]byte("payload"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleFilePut_TotalTextFieldsTooLarge confirms that piling up
// many sub-cap text parts past the cumulative cap is rejected.
func TestHandleFilePut_TotalTextFieldsTooLarge(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	chunk := strings.Repeat("x", maxTextFieldBytes/2)
	count := (maxTotalTextFieldBytes / len(chunk)) + 4

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for i := 0; i < count; i++ {
		_ = mw.WriteField(fmt.Sprintf("filler-%d", i), chunk)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleFilePut_TrailingPartsDrained verifies the streaming walker
// keeps reading after the file part — trailing text parts don't break
// the upload.
func TestHandleFilePut_TrailingPartsDrained(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("name", "example")
	_ = mw.WriteField("version", "1.0.0")
	fw, _ := mw.CreateFormFile("content", "example-1.0.0.tar.gz")
	_, _ = fw.Write([]byte("payload"))
	_ = mw.WriteField("comment", "uploaded by twine")
	_ = mw.WriteField("md5_digest", "deadbeef")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := reg.Files[testNS+"/packages/example/1.0.0/example-1.0.0.tar.gz"]; !ok {
		t.Errorf("file missing: %v", registryFileKeys(reg))
	}
}

// TestHandleFilePut_NoTmpSpill confirms that an upload large enough to
// have spilled past ParseMultipartForm's old 32 MiB threshold leaves
// the temp directory empty when handled by the streaming walker.
//
// Sequential because t.Setenv mutates process-global state.
func TestHandleFilePut_NoTmpSpill(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	const payloadSize = 40 << 20

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("name", "example")
	_ = mw.WriteField("version", "1.0.0")
	fw, _ := mw.CreateFormFile("content", "example-1.0.0.tar.gz")
	if _, err := io.CopyN(fw, dummyByteReader{}, payloadSize); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read tmp: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("TMPDIR not empty after upload: %v", names)
	}
}

type dummyByteReader struct{}

func (dummyByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i)
	}
	return len(p), nil
}

// TestHandleFilePut_ReuploadConflict locks the wire-level shape of the
// re-upload contract.
func TestHandleFilePut_ReuploadConflict(t *testing.T) {
	t.Parallel()

	build := func(content string) (*bytes.Buffer, string) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		_ = mw.WriteField("name", "example-pkg")
		_ = mw.WriteField("version", "1.0.0")
		fw, _ := mw.CreateFormFile("content", "example_pkg-1.0.0.whl")
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		return &b, mw.FormDataContentType()
	}

	send := func(t *testing.T, h http.Handler, body *bytes.Buffer, ctype string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", body)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	tests := []struct {
		name           string
		allowOverwrite bool
		wantSecond     int
	}{
		{name: "default rejects re-upload", allowOverwrite: false, wantSecond: http.StatusConflict},
		{name: "allow-overwrite returns 201", allowOverwrite: true, wantSecond: http.StatusCreated},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			reg.AllowOverwrite = tc.allowOverwrite
			h, _ := newTestHandler(t, reg)

			body, ctype := build("first upload")
			if rec := send(t, h.Mux(), body, ctype); rec.Code != http.StatusCreated {
				t.Fatalf("first upload status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}

			body, ctype = build("second upload")
			rec := send(t, h.Mux(), body, ctype)
			if got, want := rec.Code, tc.wantSecond; got != want {
				t.Fatalf("second upload status=%d, want %d (body=%s)", got, want, rec.Body.String())
			}
			if rec.Code == http.StatusConflict {
				if got := rec.Body.String(); !strings.Contains(got, "file already exists") {
					t.Errorf("409 body = %q, want contains %q", got, "file already exists")
				}
			}
		})
	}
}

// TestHandleFilePut_ReuploadOfDifferentVersionSucceeds proves the
// 409 default scopes by version.
func TestHandleFilePut_ReuploadOfDifferentVersionSucceeds(t *testing.T) {
	t.Parallel()

	build := func(version string) (*bytes.Buffer, string) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		_ = mw.WriteField("name", "example-pkg")
		_ = mw.WriteField("version", version)
		fw, _ := mw.CreateFormFile("content", "example_pkg-"+version+".whl")
		_, _ = fw.Write([]byte("content for " + version))
		_ = mw.Close()
		return &b, mw.FormDataContentType()
	}

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	for _, version := range []string{"1.0.0", "1.0.1", "2.0.0"} {
		body, ctype := build(version)
		req := httptest.NewRequest(http.MethodPut, "/"+testNS+"/", body)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s status=%d, want 201 (body=%s)", version, rec.Code, rec.Body.String())
		}
	}

	// Exactly one sentinel under the index repo, regardless of how
	// many versions were uploaded.
	indexTags := reg.Tags[testNS+"/"+packageIndexName]
	if got, want := len(indexTags), 1; got != want {
		t.Errorf("index tags = %v, want exactly one entry for the package", indexTags)
	}
}
