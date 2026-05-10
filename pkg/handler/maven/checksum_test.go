package maven

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestChecksumExt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filename string
		wantExt  string
		wantAlgo string
	}{
		{name: "sha1", filename: "foo-1.0.jar.sha1", wantExt: ".sha1", wantAlgo: "sha1"},
		{name: "md5", filename: "foo-1.0.jar.md5", wantExt: ".md5", wantAlgo: "md5"},
		{name: "sha256", filename: "foo-1.0.jar.sha256", wantExt: ".sha256", wantAlgo: "sha256"},
		{name: "sha512", filename: "foo-1.0.jar.sha512", wantExt: ".sha512", wantAlgo: "sha512"},
		{name: "snapshot timestamped sha1", filename: "foo-1.0-20260101.123456-1.jar.sha1", wantExt: ".sha1", wantAlgo: "sha1"},
		{name: "pom sha1", filename: "foo-1.0.pom.sha1", wantExt: ".sha1", wantAlgo: "sha1"},
		{name: "non checksum jar", filename: "foo-1.0.jar", wantExt: "", wantAlgo: ""},
		{name: "non checksum pom", filename: "foo-1.0.pom", wantExt: "", wantAlgo: ""},
		{name: "non checksum xml", filename: "maven-metadata.xml", wantExt: "", wantAlgo: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ext, algo, _ := checksumExt(tc.filename)
			if ext != tc.wantExt {
				t.Errorf("checksumExt(%q) ext = %q, want %q", tc.filename, ext, tc.wantExt)
			}
			if algo != tc.wantAlgo {
				t.Errorf("checksumExt(%q) algo = %q, want %q", tc.filename, algo, tc.wantAlgo)
			}
		})
	}
}

func TestParseChecksumBody(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "bare hex", body: "356a192b7913b04c54574d18c28d46e6395428ab", want: "356a192b7913b04c54574d18c28d46e6395428ab"},
		{name: "trailing newline", body: "356a192b7913b04c54574d18c28d46e6395428ab\n", want: "356a192b7913b04c54574d18c28d46e6395428ab"},
		{name: "two-space filename suffix", body: "356a192b7913b04c54574d18c28d46e6395428ab  foo.jar", want: "356a192b7913b04c54574d18c28d46e6395428ab"},
		{name: "asterisk filename suffix", body: "356a192b7913b04c54574d18c28d46e6395428ab *foo.jar", want: "356a192b7913b04c54574d18c28d46e6395428ab"},
		{name: "uppercase hex preserved", body: "356A192B7913B04C54574D18C28D46E6395428AB", want: "356A192B7913B04C54574D18C28D46E6395428AB"},
		{name: "empty body", body: "", wantErr: true},
		{name: "whitespace only", body: "   \n\t", wantErr: true},
		{name: "non hex", body: "not-a-hex-string", wantErr: true},
		{name: "odd length hex", body: "abc", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseChecksumBody([]byte(tc.body))
			if got, want := err != nil, tc.wantErr; got != want {
				t.Errorf("parseChecksumBody(%q) err = %v, wantErr = %v", tc.body, err, want)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("parseChecksumBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func hashHex(t *testing.T, h hash.Hash, data string) string {
	t.Helper()
	if _, err := h.Write([]byte(data)); err != nil {
		t.Fatalf("hash write: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestVerifyChecksumUpload(t *testing.T) {
	t.Parallel()

	const repo = "com/example/project"
	const tag = "1.0.0"
	const artifactName = "project-1.0.0.jar"
	const artifactBody = "jar content"

	cases := []struct {
		name           string
		filename       string
		body           func(t *testing.T) []byte
		setupArtifact  bool
		wantErr        bool
		wantStatusCode int
	}{
		{
			name:           "non checksum filename is a no-op",
			filename:       artifactName,
			body:           func(*testing.T) []byte { return []byte(artifactBody) },
			setupArtifact:  false,
			wantErr:        false,
			wantStatusCode: 0,
		},
		{
			name:     "valid sha1",
			filename: artifactName + ".sha1",
			body: func(t *testing.T) []byte {
				return []byte(hashHex(t, sha1.New(), artifactBody))
			},
			setupArtifact: true,
			wantErr:       false,
		},
		{
			name:     "valid md5",
			filename: artifactName + ".md5",
			body: func(t *testing.T) []byte {
				return []byte(hashHex(t, md5.New(), artifactBody))
			},
			setupArtifact: true,
			wantErr:       false,
		},
		{
			name:     "valid sha256",
			filename: artifactName + ".sha256",
			body: func(t *testing.T) []byte {
				return []byte(hashHex(t, sha256.New(), artifactBody))
			},
			setupArtifact: true,
			wantErr:       false,
		},
		{
			name:     "valid sha512",
			filename: artifactName + ".sha512",
			body: func(t *testing.T) []byte {
				return []byte(hashHex(t, sha512.New(), artifactBody))
			},
			setupArtifact: true,
			wantErr:       false,
		},
		{
			name:     "valid sha1 with filename suffix",
			filename: artifactName + ".sha1",
			body: func(t *testing.T) []byte {
				return []byte(fmt.Sprintf("%s  %s", hashHex(t, sha1.New(), artifactBody), artifactName))
			},
			setupArtifact: true,
			wantErr:       false,
		},
		{
			name:           "mismatched sha1 rejected",
			filename:       artifactName + ".sha1",
			body:           func(*testing.T) []byte { return []byte(strings.Repeat("a", 40)) },
			setupArtifact:  true,
			wantErr:        true,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "mismatched sha256 rejected",
			filename:       artifactName + ".sha256",
			body:           func(*testing.T) []byte { return []byte(strings.Repeat("a", 64)) },
			setupArtifact:  true,
			wantErr:        true,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "missing companion artifact rejected",
			filename:       artifactName + ".sha1",
			body:           func(t *testing.T) []byte { return []byte(hashHex(t, sha1.New(), artifactBody)) },
			setupArtifact:  false,
			wantErr:        true,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "malformed body rejected",
			filename:       artifactName + ".sha1",
			body:           func(*testing.T) []byte { return []byte("not-hex") },
			setupArtifact:  true,
			wantErr:        true,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "checksum filename without companion stem",
			filename:       ".sha1",
			body:           func(*testing.T) []byte { return []byte(strings.Repeat("a", 40)) },
			setupArtifact:  false,
			wantErr:        true,
			wantStatusCode: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			if tc.setupArtifact {
				_, err := reg.AddFile(t.Context(), &oci.RepoFile{
					OwningRepo: repo,
					OwningTag:  tag,
					Name:       artifactName,
					MediaType:  "application/java-archive",
				}, strings.NewReader(artifactBody))
				if err != nil {
					t.Fatalf("setup AddFile: %v", err)
				}
			}

			f := &oci.RepoFile{
				OwningRepo: repo,
				OwningTag:  tag,
				Name:       tc.filename,
			}
			err := verifyChecksumUpload(t.Context(), reg, f, tc.body(t))
			if got, want := err != nil, tc.wantErr; got != want {
				t.Errorf("verifyChecksumUpload err = %v, wantErr = %v", err, want)
			}
			if tc.wantStatusCode != 0 {
				if got := httpStatus(err); got != tc.wantStatusCode {
					t.Errorf("httpStatus(err) = %d, want %d", got, tc.wantStatusCode)
				}
			}
		})
	}
}

// TestVerifyChecksumUpload_SnapshotTimestamps proves that checksum
// verification handles Maven's snapshot timestamped artifact filenames
// without misparsing the timestamp as part of the extension.
func TestVerifyChecksumUpload_SnapshotTimestamps(t *testing.T) {
	t.Parallel()

	const repo = "com/example/project"
	const tag = "1.0-SNAPSHOT"
	const artifactName = "project-1.0-20260101.123456-1.jar"
	const artifactBody = "snapshot jar content"

	reg := oci.NewFakeRegistry()
	if _, err := reg.AddFile(t.Context(), &oci.RepoFile{
		OwningRepo: repo,
		OwningTag:  tag,
		Name:       artifactName,
		MediaType:  "application/java-archive",
	}, strings.NewReader(artifactBody)); err != nil {
		t.Fatalf("setup AddFile: %v", err)
	}

	f := &oci.RepoFile{
		OwningRepo: repo,
		OwningTag:  tag,
		Name:       artifactName + ".sha1",
	}
	body := []byte(hashHex(t, sha1.New(), artifactBody))
	if err := verifyChecksumUpload(t.Context(), reg, f, body); err != nil {
		t.Errorf("verifyChecksumUpload on snapshot timestamped filename: %v", err)
	}
}

// TestHandlePut_ChecksumIntegration drives the full Mux path for a
// jar + sha1 happy path, a sha1-without-prior-jar rejection, and a
// mismatched sha1 rejection. The fake registry doubles as the
// companion-artifact store.
func TestHandlePut_ChecksumIntegration(t *testing.T) {
	t.Parallel()

	const artifactPath = "/default/maven2/com/example/project/1.0.0/project-1.0.0.jar"
	const sha1Path = artifactPath + ".sha1"
	const md5Path = artifactPath + ".md5"
	const artifactBody = "jar content"

	cases := []struct {
		name         string
		setup        func(t *testing.T, h http.Handler)
		uploadPath   string
		uploadBody   string
		wantStatus   int
		wantStored   bool
		storedKey    string
		storedExpect string
	}{
		{
			name: "valid sha1 after artifact",
			setup: func(t *testing.T, h http.Handler) {
				putOK(t, h, artifactPath, artifactBody)
			},
			uploadPath:   sha1Path,
			uploadBody:   hashHex(t, sha1.New(), artifactBody),
			wantStatus:   http.StatusCreated,
			wantStored:   true,
			storedKey:    "com/example/project/1.0.0/project-1.0.0.jar.sha1",
			storedExpect: hashHex(t, sha1.New(), artifactBody),
		},
		{
			name: "valid md5 after artifact",
			setup: func(t *testing.T, h http.Handler) {
				putOK(t, h, artifactPath, artifactBody)
			},
			uploadPath:   md5Path,
			uploadBody:   hashHex(t, md5.New(), artifactBody),
			wantStatus:   http.StatusCreated,
			wantStored:   true,
			storedKey:    "com/example/project/1.0.0/project-1.0.0.jar.md5",
			storedExpect: hashHex(t, md5.New(), artifactBody),
		},
		{
			name:       "sha1 before artifact rejected",
			setup:      func(t *testing.T, h http.Handler) {},
			uploadPath: sha1Path,
			uploadBody: hashHex(t, sha1.New(), artifactBody),
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "mismatched sha1 rejected",
			setup: func(t *testing.T, h http.Handler) {
				putOK(t, h, artifactPath, artifactBody)
			},
			uploadPath: sha1Path,
			uploadBody: strings.Repeat("a", 40),
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, err := newHandlerWithRegistry(reg)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			mux := h.Mux()

			tc.setup(t, mux)

			req := httptest.NewRequest(http.MethodPut, tc.uploadPath, strings.NewReader(tc.uploadBody))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if got, want := w.Code, tc.wantStatus; got != want {
				t.Errorf("status = %d, want %d (body=%q)", got, want, w.Body.String())
			}

			if tc.wantStored {
				got, ok := reg.Files[tc.storedKey]
				if !ok {
					t.Errorf("checksum file not stored at %q", tc.storedKey)
				} else if string(got) != tc.storedExpect {
					t.Errorf("stored checksum = %q, want %q", got, tc.storedExpect)
				}
			} else if !tc.wantStored && tc.uploadPath == sha1Path {
				if _, exists := reg.Files["com/example/project/1.0.0/project-1.0.0.jar.sha1"]; exists {
					t.Errorf("checksum unexpectedly stored after rejection")
				}
			}
		})
	}
}

// TestHandlePut_PathTraversal proves the handler rejects URL components
// containing ".." or other unsafe characters with 400, and that nothing
// reaches the OCI backend.
func TestHandlePut_PathTraversal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{name: "traversal in repoParts", path: "/default/maven2/com/../etc/1.0/project-1.0.jar"},
		{name: "traversal segment in version", path: "/default/maven2/com/example/project/../project-1.0.jar"},
		// `..` as filename suffix would be stripped by gorilla/mux's
		// route matching, but a literal `..` filename component lands
		// in the filename mux var and must be rejected.
		{name: "double-dot filename", path: "/default/maven2/com/example/project/1.0/.."},
		{name: "doubled slash in repoParts", path: "/default/maven2/com//example/1.0/project-1.0.jar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := oci.NewFakeRegistry()
			h, err := newHandlerWithRegistry(reg)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}

			req := httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader("x"))
			w := httptest.NewRecorder()
			h.Mux().ServeHTTP(w, req)

			// Either the route matches and validatePath returns 400,
			// or gorilla/mux's normalisation rejects the URL with
			// 404 / 301 before our handler runs. Either way no file
			// must be stored — a successful 201 would be the bug.
			if w.Code == http.StatusCreated {
				t.Errorf("path %q produced 201; want a rejection (400/404/301)", tc.path)
			}
			if len(reg.Files) != 0 {
				t.Errorf("path %q stored %d file(s); want 0", tc.path, len(reg.Files))
			}
		})
	}
}

func putOK(t *testing.T, h http.Handler, path, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup PUT %q failed: status=%d body=%q", path, w.Code, w.Body.String())
	}
}
