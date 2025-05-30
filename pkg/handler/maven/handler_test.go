package maven

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
			path:       "/com/example/project/1.0.0/project-1.0.0.jar",
			body:       "jar content",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "valid pom",
			path:       "/com/example/project/1.0.0/project-1.0.0.pom",
			body:       "<project></project>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "invalid path",
			path:       "/com",
			body:       "content",
			wantStatus: http.StatusNotFound,
			wantFile:   false,
		},
		{
			name:       "archetype catalog",
			path:       "/archetype-catalog.xml",
			body:       "<archetype-catalog></archetype-catalog>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "snapshot metadata",
			path:       "/com/example/project/1.0-SNAPSHOT/maven-metadata.xml",
			body:       "<metadata></metadata>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
		},
		{
			name:       "release metadata",
			path:       "/com/example/project/maven-metadata.xml",
			body:       "<metadata></metadata>",
			wantStatus: http.StatusCreated,
			wantFile:   true,
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

			req := httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()

			h.Mux().ServeHTTP(w, req)

			if got, want := w.Code, tc.wantStatus; got != want {
				t.Errorf("Status code = %d, want %d", got, want)
			}

			if tc.wantFile {
				f := pathToRepoFile(t, strings.Trim(tc.path, "/"))
				key := f.OwningRepo + "/" + f.OwningTag + "/" + f.Name
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
				OwningRepo: "com/example/project",
				OwningTag:  "1.0.0",
				Name:       "project-1.0.0.jar",
				MediaType:  "application/java-archive",
			},
			setupData:  "jar content",
			path:       "/com/example/project/1.0.0/project-1.0.0.jar",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "jar content",
		},
		{
			name: "head existing jar",
			setupFile: &oci.RepoFile{
				OwningRepo: "com/example/project",
				OwningTag:  "1.0.0",
				Name:       "project-1.0.0.jar",
				MediaType:  "application/java-archive",
			},
			setupData:  "jar content",
			path:       "/com/example/project/1.0.0/project-1.0.0.jar",
			method:     http.MethodHead,
			wantStatus: http.StatusOK,
			wantBody:   "",
		},
		{
			name:       "file not found",
			path:       "/com/example/project/1.0.0/project-1.0.0.jar",
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid path",
			path:       "/com",
			method:     http.MethodGet,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "get archetype catalog",
			setupFile: &oci.RepoFile{
				OwningRepo: "archetype",
				OwningTag:  "latest",
				Name:       "archetype-catalog.xml",
				MediaType:  "text/xml",
			},
			setupData:  "<archetype-catalog></archetype-catalog>",
			path:       "/archetype-catalog.xml",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "<archetype-catalog></archetype-catalog>",
		},
		{
			name: "get snapshot metadata",
			setupFile: &oci.RepoFile{
				OwningRepo: "com/example/project",
				OwningTag:  "1.0-SNAPSHOT-metadata",
				Name:       "maven-metadata.xml",
				MediaType:  "text/xml",
			},
			setupData:  "<metadata></metadata>",
			path:       "/com/example/project/1.0-SNAPSHOT/maven-metadata.xml",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "<metadata></metadata>",
		},
		{
			name: "get release metadata",
			setupFile: &oci.RepoFile{
				OwningRepo: "com/example/project",
				OwningTag:  "metadata",
				Name:       "maven-metadata.xml",
				MediaType:  "text/xml",
			},
			setupData:  "<metadata></metadata>",
			path:       "/com/example/project/maven-metadata.xml",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "<metadata></metadata>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := oci.NewFakeRegistry()
			if tc.setupFile != nil {
				_, err := registry.AddFile(context.Background(), tc.setupFile, strings.NewReader(tc.setupData))
				if err != nil {
					t.Fatalf("Failed to set up file: %v", err)
				}
			}

			h, err := NewHandler(registry)
			if err != nil {
				t.Fatalf("NewHandler() unexpected error: %v", err)
			}

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

				mediaType := detectMediaType(strings.Trim(tc.path, "/"))
				if got, want := w.Header().Get("Content-Type"), mediaType; got != want {
					t.Errorf("Content-Type = %q, want %q", got, want)
				}
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
				OwningRepo: strings.Join(parts[:len(parts)-2], "/"), // groupId/artifactId
				OwningTag:  parts[len(parts)-2] + "-metadata",       // versionId-metadata
				Name:       fn,
				MediaType:  "text/xml",
			}
		} else {
			// This is a group/artifact level maven-metadata.xml for releases.
			return &oci.RepoFile{
				OwningRepo: strings.Join(parts[:len(parts)-1], "/"), // groupId/artifactId
				OwningTag:  "metadata",                              // metadata
				Name:       fn,
				MediaType:  "text/xml",
			}
		}
	}

	if len(parts) < 4 {
		t.Fatalf("invalid path: %s", p)
	}

	return &oci.RepoFile{
		OwningRepo: strings.Join(parts[:len(parts)-2], "/"), // groupId/artifactId
		OwningTag:  parts[len(parts)-2],                     // versionId
		Name:       fn,
		MediaType:  detectMediaType(fn),
	}
}
