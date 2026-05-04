package python

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/oci"
)

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

			h, err := NewHandler(registry)
			if err != nil {
				t.Fatalf("NewHandler() unexpected error: %v", err)
			}

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
			req := httptest.NewRequest(http.MethodPut, "/", &b)
			req.Header.Set("Content-Type", w.FormDataContentType())

			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)

			if got, want := resp.Code, tc.wantStatus; got != want {
				t.Errorf("Status code = %d, want %d", got, want)
			}

			if tc.wantFile {
				// Verify package file was created
				key := "packages/" + tc.pkgName + "/" + tc.version + "/" + tc.filename
				content, ok := registry.Files[key]
				if !ok {
					t.Errorf("Package file not found in registry: %s", key)
				} else if string(content) != tc.content {
					t.Errorf("Package file content = %q, want %q", string(content), tc.content)
				}
			}

			if tc.wantIndex {
				// Verify the per-package sentinel was written under index/<pkgName>.
				// The body is a constant placeholder, not the version, so the
				// OCI backend deduplicates the blob across uploads.
				indexKey := "index/" + tc.pkgName + "/" + indexSentinelName
				indexContent, ok := registry.Files[indexKey]
				if !ok {
					t.Errorf("Index sentinel not found in registry: %s", indexKey)
				} else if string(indexContent) != indexSentinelContent {
					t.Errorf("Index sentinel content = %q, want %q", string(indexContent), indexSentinelContent)
				}

				// The version string should never appear as a layer name in
				// the index repo — that was the per-version write the new
				// sentinel approach replaces.
				perVersionKey := "index/" + tc.pkgName + "/" + tc.version
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
				OwningRepo: "packages/example-pkg",
				OwningTag:  "1.0.0",
				Name:       "example-pkg-1.0.0.whl",
				MediaType:  "application/x-wheel+zip",
			},
			setupData:  "wheel content",
			path:       "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "wheel content",
		},
		{
			name: "head existing wheel",
			setupFile: &oci.RepoFile{
				OwningRepo: "packages/example-pkg",
				OwningTag:  "1.0.0",
				Name:       "example-pkg-1.0.0.whl",
				MediaType:  "application/x-wheel+zip",
			},
			setupData:  "wheel content",
			path:       "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl",
			method:     http.MethodHead,
			wantStatus: http.StatusOK,
			wantBody:   "",
		},
		{
			name:       "file not found",
			path:       "/packages/example-pkg/1.0.0/example-pkg-1.0.0.whl",
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid path",
			path:       "/packages/example-pkg",
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

			h, err := NewHandler(registry)
			if err != nil {
				t.Fatalf("NewHandler() unexpected error: %v", err)
			}

			req := httptest.NewRequest(tc.method, tc.path, nil)

			// Set path values manually since we're not using a real router
			if strings.HasPrefix(tc.path, "/packages/") && strings.Count(tc.path, "/") >= 4 {
				parts := strings.Split(strings.TrimPrefix(tc.path, "/packages/"), "/")
				if len(parts) >= 3 {
					req.SetPathValue("package", parts[0])
					req.SetPathValue("version", parts[1])
					req.SetPathValue("filename", parts[2])
				}
			}

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

// TestIndexSentinel covers the per-package sentinel write path: the
// index repo grows by exactly one tag per package, regardless of how
// many versions of that package have been uploaded, and handleSimpleIndex
// surfaces every uploaded package in /simple/.
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
				"index/requests/" + indexSentinelName: indexSentinelContent,
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
				"index/requests/" + indexSentinelName: indexSentinelContent,
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
				"index/requests/" + indexSentinelName: indexSentinelContent,
				"index/flask/" + indexSentinelName:    indexSentinelContent,
				"index/django/" + indexSentinelName:   indexSentinelContent,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			h, err := NewHandler(registry)
			if err != nil {
				t.Fatalf("NewHandler() unexpected error: %v", err)
			}

			for _, up := range tc.uploads {
				if code := uploadPackage(t, h, up.pkg, up.version); code != http.StatusCreated {
					t.Fatalf("upload %q@%q: status = %d, want %d", up.pkg, up.version, code, http.StatusCreated)
				}
			}

			gotIndexTags := append([]string{}, registry.Tags["index"]...)
			sort.Strings(gotIndexTags)
			if diff := cmp.Diff(tc.wantIndexTags, gotIndexTags); diff != "" {
				t.Errorf("index repo tags mismatch (-want +got):\n%s", diff)
			}

			gotIndexFiles := map[string]string{}
			for k, v := range registry.Files {
				if strings.HasPrefix(k, "index/") {
					gotIndexFiles[k] = string(v)
				}
			}
			if diff := cmp.Diff(tc.wantIndexFiles, gotIndexFiles); diff != "" {
				t.Errorf("index repo files mismatch (-want +got):\n%s", diff)
			}

			// handleSimpleIndex must list every uploaded package, regardless
			// of how many versions each has.
			req := httptest.NewRequest(http.MethodGet, "/simple/", nil)
			resp := httptest.NewRecorder()
			h.Mux().ServeHTTP(resp, req)
			if got, want := resp.Code, http.StatusOK; got != want {
				t.Fatalf("/simple/ status = %d, want %d", got, want)
			}
			body := resp.Body.String()
			for _, pkg := range tc.wantIndexTags {
				if !strings.Contains(body, "/simple/"+pkg+"/") {
					t.Errorf("/simple/ body missing link for %q, got:\n%s", pkg, body)
				}
			}
		})
	}
}

// uploadPackage performs a multipart twine-style upload of a package
// version through the python handler and returns the response status.
func uploadPackage(t *testing.T, h *Handler, pkgName, version string) int {
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

	req := httptest.NewRequest(http.MethodPut, "/", &b)
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
			path:             "/simple/",
			wantStatus:       http.StatusOK,
			wantBodyContains: []string{"Simple Index"},
		},
		{
			name:       "index with packages",
			setupTags:  []string{"package1", "package2", "example-pkg"},
			path:       "/simple/",
			wantStatus: http.StatusOK,
			wantBodyContains: []string{
				"Simple Index",
				"package1",
				"package2",
				"example-pkg",
				"/simple/package1/",
				"/simple/package2/",
				"/simple/example-pkg/",
			},
		},
		{
			name:       "index without trailing slash",
			setupTags:  []string{"package1", "package2"},
			path:       "/simple",
			wantStatus: http.StatusOK,
			wantBodyContains: []string{
				"Simple Index",
				"package1",
				"package2",
				"/simple/package1/",
				"/simple/package2/",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			registry.Tags["index"] = append(registry.Tags["index"], tc.setupTags...)

			h, err := NewHandler(registry)
			if err != nil {
				t.Fatalf("NewHandler() unexpected error: %v", err)
			}

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
