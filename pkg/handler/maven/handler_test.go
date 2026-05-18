package maven

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
			name:     "jar",
			filename: "project-1.0.0.jar",
			want:     "application/java-archive",
		},
		{
			name:     "pom",
			filename: "project-1.0.0.pom",
			want:     "text/xml",
		},
		{
			name:     "sha1",
			filename: "project-1.0.0.jar.sha1",
			want:     "text/plain",
		},
		{
			name:     "unknown",
			filename: "project-1.0.0.unknown",
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
		path       string
		body       string
		wantStatus int
		wantFile   bool
	}{
		{
			name:       "valid jar",
			path:       nsPath("/com/example/project/1.0.0/project-1.0.0.jar"),
			body:       "jar content",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "valid pom",
			path:       nsPath("/com/example/project/1.0.0/project-1.0.0.pom"),
			body:       "<project></project>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "invalid path",
			path:       nsPath("/com"),
			body:       "content",
			wantStatus: http.StatusNotFound,
			wantFile:   false,
		},
		{
			name:       "archetype catalog",
			path:       nsPath("/archetype-catalog.xml"),
			body:       "<archetype-catalog></archetype-catalog>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "snapshot metadata",
			path:       nsPath("/com/example/project/1.0-SNAPSHOT/maven-metadata.xml"),
			body:       "<metadata></metadata>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "release metadata",
			path:       nsPath("/com/example/project/maven-metadata.xml"),
			body:       "<metadata></metadata>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, registry)

			req := httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()

			h.Mux().ServeHTTP(w, req)

			if got, want := w.Code, tc.wantStatus; got != want {
				t.Errorf("Status code = %d, want %d (body=%s)", got, want, w.Body.String())
			}

			if tc.wantFile {
				// Strip the /<ns>/maven2 prefix when computing the
				// expected backend key shape — pathToRepoFile expects
				// the maven-relative tail.
				rel := strings.TrimPrefix(tc.path, "/"+testNS+"/maven2/")
				f := pathToRepoFile(t, rel)
				key := testNS + "/" + f.OwningRepo + "/" + f.OwningTag + "/" + f.Name
				content, ok := registry.Files[key]
				if !ok {
					t.Errorf("File not found in registry: %s", key)
				} else if string(content) != tc.body {
					t.Errorf("File content = %q, want %q", string(content), tc.body)
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
			name: "get existing jar",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/com/example/project",
				OwningTag:  "1.0.0",
				Name:       "project-1.0.0.jar",
				MediaType:  "application/java-archive",
			},
			setupData:  "jar content",
			path:       nsPath("/com/example/project/1.0.0/project-1.0.0.jar"),
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "jar content",
		},
		{
			name: "head existing jar",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/com/example/project",
				OwningTag:  "1.0.0",
				Name:       "project-1.0.0.jar",
				MediaType:  "application/java-archive",
			},
			setupData:  "jar content",
			path:       nsPath("/com/example/project/1.0.0/project-1.0.0.jar"),
			method:     http.MethodHead,
			wantStatus: http.StatusOK,
			wantBody:   "",
		},
		{
			name:       "file not found",
			path:       nsPath("/com/example/project/1.0.0/project-1.0.0.jar"),
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid path",
			path:       nsPath("/com"),
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "get archetype catalog",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/archetype",
				OwningTag:  "latest",
				Name:       "archetype-catalog.xml",
				MediaType:  "text/xml",
			},
			setupData:  "<archetype-catalog></archetype-catalog>",
			path:       nsPath("/archetype-catalog.xml"),
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "<archetype-catalog></archetype-catalog>",
		},
		{
			name: "get snapshot metadata",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/com/example/project",
				OwningTag:  "1.0-SNAPSHOT-metadata",
				Name:       "maven-metadata.xml",
				MediaType:  "text/xml",
			},
			setupData:  "<metadata></metadata>",
			path:       nsPath("/com/example/project/1.0-SNAPSHOT/maven-metadata.xml"),
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "<metadata></metadata>",
		},
		{
			name: "get snapshot metadata checksum",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/com/example/project",
				OwningTag:  "1.0-SNAPSHOT-metadata",
				Name:       "maven-metadata.xml.sha1",
				MediaType:  "text/plain",
			},
			setupData:  "snapshot-sha1",
			path:       nsPath("/com/example/project/1.0-SNAPSHOT/maven-metadata.xml.sha1"),
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "snapshot-sha1",
		},
		{
			name: "get release metadata",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/com/example/project",
				OwningTag:  "metadata",
				Name:       "maven-metadata.xml",
				MediaType:  "text/xml",
			},
			setupData:  "<metadata></metadata>",
			path:       nsPath("/com/example/project/maven-metadata.xml"),
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "<metadata></metadata>",
		},
		{
			name: "get release metadata checksum",
			setupFile: &oci.RepoFile{
				OwningRepo: testNS + "/packages/com/example/project",
				OwningTag:  "metadata",
				Name:       "maven-metadata.xml.sha1",
				MediaType:  "text/plain",
			},
			setupData:  "release-sha1",
			path:       nsPath("/com/example/project/maven-metadata.xml.sha1"),
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "release-sha1",
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

				// The path tail (without the namespace) determines the
				// expected MIME — detectMediaType keys off the file name.
				rel := strings.TrimPrefix(tc.path, "/"+testNS+"/maven2/")
				mediaType := detectMediaType(strings.Trim(rel, "/"))
				if got, want := w.Header().Get("Content-Type"), mediaType; got != want {
					t.Errorf("Content-Type = %q, want %q", got, want)
				}
			}
		})
	}
}

// redirectingRegistry wraps an *oci.FakeRegistry and lets a test
// program BlobRedirectURL's return values, exercising the handler's
// redirect-vs-stream branching.
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
		OwningRepo: testNS + "/packages/com/example/project",
		OwningTag:  "1.0.0",
		Name:       "project-1.0.0.jar",
		MediaType:  "application/java-archive",
	}
	const setupData = "jar content"
	reqPath := nsPath("/com/example/project/1.0.0/project-1.0.0.jar")
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

func pathToRepoFile(t *testing.T, p string) *oci.RepoFile {
	if strings.HasPrefix(p, "archetype-catalog.xml") {
		return &oci.RepoFile{
			OwningRepo: "archetype",
			OwningTag:  "latest",
			Name:       p,
			MediaType:  "text/xml",
		}
	}

	parts := strings.Split(p, "/")
	if len(parts) < 3 {
		t.Fatalf("invalid path: %s", p)
	}

	fn := parts[len(parts)-1]
	if strings.HasPrefix(fn, "maven-metadata.xml") {
		if strings.Contains(parts[len(parts)-2], "-SNAPSHOT") {
			// This is a version level maven-metadata.xml for snapshots.
			return &oci.RepoFile{
				OwningRepo: packageOwningRepo(strings.Join(parts[:len(parts)-2], "/")), // groupId/artifactId
				OwningTag:  parts[len(parts)-2] + "-metadata",                          // versionId-metadata
				Name:       fn,
				MediaType:  "text/xml",
			}
		} else {
			// This is a group/artifact level maven-metadata.xml for releases.
			return &oci.RepoFile{
				OwningRepo: packageOwningRepo(strings.Join(parts[:len(parts)-1], "/")), // groupId/artifactId
				OwningTag:  "metadata",                                                 // metadata
				Name:       fn,
				MediaType:  "text/xml",
			}
		}
	}

	if len(parts) < 4 {
		t.Fatalf("invalid path: %s", p)
	}

	return &oci.RepoFile{
		OwningRepo: packageOwningRepo(strings.Join(parts[:len(parts)-2], "/")), // groupId/artifactId
		OwningTag:  parts[len(parts)-2],                                        // versionId
		Name:       fn,
		MediaType:  detectMediaType(fn),
	}
}

// TestHandlePut_MaxUploadBytes confirms WithMaxUploadBytes returns 413
// for an oversize body, 201 at-or-under the cap, and that the cap can
// be disabled by passing a non-positive value.
func TestHandlePut_MaxUploadBytes(t *testing.T) {
	t.Parallel()

	const cap = 1 << 14 // 16 KiB

	cases := []struct {
		name           string
		maxUploadBytes int64
		bodySize       int
		wantStatus     int
		wantBackendHit bool
	}{
		{
			name:           "under cap",
			maxUploadBytes: cap,
			bodySize:       cap - 1,
			wantStatus:     http.StatusCreated,
			wantBackendHit: true,
		},
		{
			name:           "exactly at cap",
			maxUploadBytes: cap,
			bodySize:       cap,
			wantStatus:     http.StatusCreated,
			wantBackendHit: true,
		},
		{
			name:           "just over cap",
			maxUploadBytes: cap,
			bodySize:       cap + 1,
			wantStatus:     http.StatusRequestEntityTooLarge,
			wantBackendHit: false,
		},
		{
			name:           "way over cap",
			maxUploadBytes: cap,
			bodySize:       1 << 20, // 1 MiB
			wantStatus:     http.StatusRequestEntityTooLarge,
			wantBackendHit: false,
		},
		{
			name:           "cap disabled accepts 10 MiB",
			maxUploadBytes: 0,
			bodySize:       10 << 20,
			wantStatus:     http.StatusCreated,
			wantBackendHit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, reg, WithMaxUploadBytes(tc.maxUploadBytes))

			body := bytes.Repeat([]byte("a"), tc.bodySize)
			req := httptest.NewRequest(
				http.MethodPut,
				nsPath("/com/example/project/1.0.0/project-1.0.0.jar"),
				bytes.NewReader(body),
			)
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if rec.Code == http.StatusRequestEntityTooLarge && rec.Body.Len() == 0 {
				t.Errorf("413 response had empty body, want non-empty error message")
			}
			// Backend was contacted iff the artifact key was written.
			// The test handler setup writes namespace-metadata blobs
			// unconditionally, so check for the artifact key directly.
			artifactKey := testNS + "/packages/com/example/project/1.0.0/project-1.0.0.jar"
			_, hit := reg.Files[artifactKey]
			if hit != tc.wantBackendHit {
				t.Errorf("backend hit = %v, want %v", hit, tc.wantBackendHit)
			}
		})
	}
}

// TestHandlePut_DefaultMaxUploadBytes confirms NewHandler defaults the
// cap to DefaultMaxUploadBytes when no WithMaxUploadBytes option is
// supplied.
func TestHandlePut_DefaultMaxUploadBytes(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, reg)

	if got, want := h.maxUploadBytes, DefaultMaxUploadBytes; got != want {
		t.Errorf("default maxUploadBytes = %d, want %d", got, want)
	}
}

// TestHandlePut_SnapshotOverwrite locks the snapshot-vs-release
// overwrite contract: snapshot versions and per-version snapshot
// metadata are always overwritable regardless of the registry's
// --allow-overwrite default, while release versions still 409 by
// default. This is the load-bearing contract for `mvn deploy` of a
// snapshot version twice in a row against ocifactory's default
// strict-overwrite config.
func TestHandlePut_SnapshotOverwrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		registryOverwrite bool
		path              string
		wantSecondStatus  int
	}{
		{
			name:              "snapshot jar overwrites under strict registry",
			registryOverwrite: false,
			path:              nsPath("/com/example/project/1.0-SNAPSHOT/project-1.0-SNAPSHOT.jar"),
			wantSecondStatus:  http.StatusCreated,
		},
		{
			name:              "snapshot jar with lowercase suffix overwrites under strict registry",
			registryOverwrite: false,
			path:              nsPath("/com/example/project/1.0-snapshot/project-1.0-snapshot.jar"),
			wantSecondStatus:  http.StatusCreated,
		},
		{
			name:              "release jar 409s under strict registry",
			registryOverwrite: false,
			path:              nsPath("/com/example/project/1.0/project-1.0.jar"),
			wantSecondStatus:  http.StatusConflict,
		},
		{
			name:              "release jar overwrites under permissive registry",
			registryOverwrite: true,
			path:              nsPath("/com/example/project/1.0/project-1.0.jar"),
			wantSecondStatus:  http.StatusCreated,
		},
		{
			name:              "per-version snapshot metadata overwrites under strict registry",
			registryOverwrite: false,
			path:              nsPath("/com/example/project/1.0-SNAPSHOT/maven-metadata.xml"),
			wantSecondStatus:  http.StatusCreated,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			reg.AllowOverwrite = tc.registryOverwrite
			h, _ := newTestHandler(t, reg)
			m := h.Mux()

			send := func(body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader(body))
				rec := httptest.NewRecorder()
				m.ServeHTTP(rec, req)
				return rec
			}

			if rec := send("first"); rec.Code != http.StatusCreated {
				t.Fatalf("first PUT status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}

			rec := send("second")
			if got, want := rec.Code, tc.wantSecondStatus; got != want {
				t.Fatalf("second PUT status=%d, want %d (body=%s)", got, want, rec.Body.String())
			}
		})
	}
}

// TestHandlePut_ReuploadConflict locks the wire-level shape of the
// re-upload contract for Maven uploads.
func TestHandlePut_ReuploadConflict(t *testing.T) {
	t.Parallel()

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
			mux := h.Mux()

			send := func(body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPut, nsPath("/com/example/project/1.0.0/project-1.0.0.jar"), strings.NewReader(body))
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				return rec
			}

			if rec := send("first jar"); rec.Code != http.StatusCreated {
				t.Fatalf("first PUT status=%d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}

			rec := send("second jar")
			if got, want := rec.Code, tc.wantSecond; got != want {
				t.Fatalf("second PUT status=%d, want %d (body=%s)", got, want, rec.Body.String())
			}
			if rec.Code == http.StatusConflict {
				if got := rec.Body.String(); !strings.Contains(got, "file already exists") {
					t.Errorf("409 body = %q, want contains %q", got, "file already exists")
				}
			}
		})
	}
}
