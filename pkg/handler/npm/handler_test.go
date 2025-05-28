package npm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"fmt"

	"github.com/gorilla/mux"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/oci"
	npmdata "github.com/yolocs/ocifactory/pkg/handler/npm/data"
)

// MockRegistry is a mock implementation of the handler.Registry interface.
type MockRegistry struct {
	// AddFileFunc holds the custom logic for AddFile.
	AddFileFunc func(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	// ReadFileFunc holds the custom logic for ReadFile.
	ReadFileFunc func(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	// ListTagsFunc holds the custom logic for ListTags.
	ListTagsFunc func(ctx context.Context, repo string) ([]string, error)
	// ListFilesFunc holds the custom logic for ListFiles.
	ListFilesFunc func(ctx context.Context, repo string) ([]*oci.RepoFile, error)
	// DeleteTagFilesFunc holds the custom logic for DeleteTagFiles.
	DeleteTagFilesFunc func(ctx context.Context, repo string, tag string) error
	// TagManifestFunc holds the custom logic for TagManifest.
	TagManifestFunc func(ctx context.Context, repo string, existingTagOrDigest string, newTag string) error
	// DeleteTagFunc holds the custom logic for DeleteTag.
	DeleteTagFunc func(ctx context.Context, repo string, tag string) error
	// ResolveFunc holds the custom logic for Resolve (not directly on Registry, but often needed for mocks).
	ResolveFunc func(ctx context.Context, repo string, tagOrDigest string) (ocispec.Descriptor, error)
	// GetManifestFunc holds the custom logic for GetManifest.
	GetManifestFunc func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error)
	// GetBlobFunc holds the custom logic for GetBlob.
	GetBlobFunc func(ctx context.Context, repo string, digest string) (io.ReadCloser, error)


	// Call verification fields
	AddFileCalledWith     []*oci.RepoFile
	ReadFileCalledWith    []*oci.RepoFile
	ListTagsCalledWith    []string
	DeleteTagFilesCalledWith []map[string]string // map of "repo" and "tag"
	TagManifestCalledWith  []map[string]string // map of "repo", "existing", "new"
	DeleteTagCalledWith    []map[string]string // map of "repo" and "tag"
	ResolveCalledWith      []map[string]string
	GetManifestCalledWith  []map[string]string
	GetBlobCalledWith      []map[string]string
}

// ResetCalls resets call verification fields.
func (m *MockRegistry) ResetCalls() {
	m.AddFileCalledWith = nil
	m.ReadFileCalledWith = nil
	m.ListTagsCalledWith = nil
	m.DeleteTagFilesCalledWith = nil
	m.TagManifestCalledWith = nil
	m.DeleteTagCalledWith = nil
	m.ResolveCalledWith = nil
	m.GetManifestCalledWith = nil
	m.GetBlobCalledWith = nil
}


// --- Interface Implementations ---

func (m *MockRegistry) AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error) {
	m.AddFileCalledWith = append(m.AddFileCalledWith, f)
	if m.AddFileFunc != nil {
		return m.AddFileFunc(ctx, f, ro)
	}
	// Provide a default success response if no custom func is set
	return &oci.FileDescriptor{
		Manifest: ocispec.Descriptor{Digest: "sha256:mockmanifestdigest"},
		File:     ocispec.Descriptor{Digest: "sha256:mockfiledigest", Size: 123, MediaType: f.MediaType},
	}, nil
}

func (m *MockRegistry) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	m.ReadFileCalledWith = append(m.ReadFileCalledWith, f)
	if m.ReadFileFunc != nil {
		return m.ReadFileFunc(ctx, f)
	}
	// Default: return not found or an error
	return nil, nil, fmt.Errorf("ReadFile mock not implemented")
}

func (m *MockRegistry) ListTags(ctx context.Context, repo string) ([]string, error) {
	m.ListTagsCalledWith = append(m.ListTagsCalledWith, repo)
	if m.ListTagsFunc != nil {
		return m.ListTagsFunc(ctx, repo)
	}
	return []string{}, nil // Default: empty list
}

func (m *MockRegistry) ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error) {
	// m.ListFilesCalledWith... (if needed)
	if m.ListFilesFunc != nil {
		return m.ListFilesFunc(ctx, repo)
	}
	return []*oci.RepoFile{}, nil
}

func (m *MockRegistry) DeleteTagFiles(ctx context.Context, repo string, tag string) error {
	m.DeleteTagFilesCalledWith = append(m.DeleteTagFilesCalledWith, map[string]string{"repo": repo, "tag": tag})
	if m.DeleteTagFilesFunc != nil {
		return m.DeleteTagFilesFunc(ctx, repo, tag)
	}
	return nil // Default: success
}

func (m *MockRegistry) TagManifest(ctx context.Context, repo string, existingTagOrDigest string, newTag string) error {
	m.TagManifestCalledWith = append(m.TagManifestCalledWith, map[string]string{"repo": repo, "existing": existingTagOrDigest, "new": newTag})
	if m.TagManifestFunc != nil {
		return m.TagManifestFunc(ctx, repo, existingTagOrDigest, newTag)
	}
	return nil // Default: success
}

func (m *MockRegistry) DeleteTag(ctx context.Context, repo string, tag string) error {
	m.DeleteTagCalledWith = append(m.DeleteTagCalledWith, map[string]string{"repo": repo, "tag": tag})
	if m.DeleteTagFunc != nil {
		return m.DeleteTagFunc(ctx, repo, tag)
	}
	return nil // Default: success
}

// Helper methods for mocking OCI interactions (not directly on Registry interface but used by handlers)
func (m *MockRegistry) Resolve(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
	m.ResolveCalledWith = append(m.ResolveCalledWith, map[string]string{"repo": repo, "ref": ref})
	if m.ResolveFunc != nil {
		return m.ResolveFunc(ctx, repo, ref)
	}
	return ocispec.Descriptor{Digest: "sha256:mockresolveddigest", MediaType: ocispec.MediaTypeImageManifest}, nil
}

func (m *MockRegistry) GetManifest(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
	m.GetManifestCalledWith = append(m.GetManifestCalledWith, map[string]string{"repo": repo, "digest": digest})
	if m.GetManifestFunc != nil {
		return m.GetManifestFunc(ctx, repo, digest)
	}
	// Return a minimal valid manifest
	return &ocispec.Manifest{
		Versioned: ocispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ArtifactType, Digest: "sha256:defaultconfigdigest", Size: 100},
		Layers:    []ocispec.Descriptor{},
	}, nil
}

func (m *MockRegistry) GetBlob(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
	m.GetBlobCalledWith = append(m.GetBlobCalledWith, map[string]string{"repo": repo, "digest": digest})
	if m.GetBlobFunc != nil {
		return m.GetBlobFunc(ctx, repo, digest)
	}
	// Return an empty reader by default
	return io.NopCloser(strings.NewReader("")), nil
}


// --- Test Functions ---

func TestPingHandler(t *testing.T) {
	req, err := http.NewRequest("GET", "/-/ping", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	mockRegistry := &MockRegistry{} // Ping handler doesn't use registry.
	
	npmHandler, err := NewHandler(mockRegistry)
	if err != nil {
		t.Fatalf("Failed to create NewHandler: %v", err)
	}
	
	// Create a router and serve the request through the Mux
	router := npmHandler.Mux().(*mux.Router) 
	router.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	expected := `{"ok":true}` + "\n" // json.Encoder adds a newline
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %q want %q", rr.Body.String(), expected)
	}

	expectedContentType := "application/json"
	if contentType := rr.Header().Get("Content-Type"); contentType != expectedContentType {
		t.Errorf("handler returned wrong content type: got %q want %q", contentType, expectedContentType)
	}
}

// TODO: Add tests for other handlers:

func TestDistTagAddHandler(t *testing.T) {
	packageName := "my-disttag-pkg"
	ociRepoName := RepoType + "/" + packageName
	distTagName := "latest"
	versionStr := "1.0.0"

	t.Run("success", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResetCalls()
		resolveCalled := false
		tagManifestCalled := false

		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			if repo == ociRepoName && ref == versionStr {
				resolveCalled = true
				return ocispec.Descriptor{Digest: "sha256:targetmanifestdigest"}, nil
			}
			return ocispec.Descriptor{}, fmt.Errorf("Resolve mock: unexpected call for %s@%s", repo, ref)
		}
		mockRegistry.TagManifestFunc = func(ctx context.Context, repo string, existingTagOrDigest string, newTag string) error {
			if repo == ociRepoName && existingTagOrDigest == versionStr && newTag == distTagName {
				tagManifestCalled = true
				return nil
			}
			return fmt.Errorf("TagManifest mock: unexpected call for %s, %s -> %s", repo, existingTagOrDigest, newTag)
		}

		handler, _ := newTestHandler(mockRegistry)
		bodyBytes, _ := json.Marshal(versionStr) // Body is just the version string, JSON encoded
		req, _ := http.NewRequest("PUT", "/-/package/"+packageName+"/dist-tags/"+distTagName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		var resp map[string]any
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("Could not decode response: %v", err)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Errorf("expected response.ok to be true, got %v", resp["ok"])
		}
		if !resolveCalled {
			t.Error("expected Resolve to be called")
		}
		if !tagManifestCalled {
			t.Error("expected TagManifest to be called")
		}
	})

	t.Run("target version not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{}, errors.NewOCINotFoundError(fmt.Errorf("version not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		bodyBytes, _ := json.Marshal(versionStr)
		req, _ := http.NewRequest("PUT", "/-/package/"+packageName+"/dist-tags/"+distTagName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 if target version not found, got %d. Body: %s", status, rr.Body.String())
		}
	})

	t.Run("invalid request body - not json string", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("PUT", "/-/package/"+packageName+"/dist-tags/"+distTagName, strings.NewReader(`{"version": "1.0.0"}`)) // Not a simple string
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid body, got %d. Body: %s", status, rr.Body.String())
		}
	})
	
	t.Run("empty version string in body", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		handler, _ := newTestHandler(mockRegistry)
		bodyBytes, _ := json.Marshal("") // Empty string
		req, _ := http.NewRequest("PUT", "/-/package/"+packageName+"/dist-tags/"+distTagName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for empty version string, got %d. Body: %s", status, rr.Body.String())
		}
	})


	t.Run("tagmanifest fails", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: "sha256:targetmanifestdigest"}, nil
		}
		mockRegistry.TagManifestFunc = func(ctx context.Context, repo string, existingTagOrDigest string, newTag string) error {
			return fmt.Errorf("simulated TagManifest error")
		}
		handler, _ := newTestHandler(mockRegistry)
		bodyBytes, _ := json.Marshal(versionStr)
		req, _ := http.NewRequest("PUT", "/-/package/"+packageName+"/dist-tags/"+distTagName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusInternalServerError {
			t.Errorf("expected 500 if TagManifest fails, got %d. Body: %s", status, rr.Body.String())
		}
	})
}

func TestDistTagRmHandler(t *testing.T) {
	packageName := "my-disttag-pkg"
	ociRepoName := RepoType + "/" + packageName
	distTagName := "latest"

	t.Run("success", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResetCalls()
		deleteTagCalled := false
		mockRegistry.DeleteTagFunc = func(ctx context.Context, repo string, tag string) error {
			if repo == ociRepoName && tag == distTagName {
				deleteTagCalled = true
				return nil
			}
			return fmt.Errorf("DeleteTag mock: unexpected call for %s@%s", repo, tag)
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("DELETE", "/-/package/"+packageName+"/dist-tags/"+distTagName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		var resp map[string]any
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("Could not decode response: %v", err)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Errorf("expected response.ok to be true, got %v", resp["ok"])
		}
		if !deleteTagCalled {
			t.Error("expected DeleteTag to be called")
		}
	})

	t.Run("tag not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.DeleteTagFunc = func(ctx context.Context, repo string, tag string) error {
			return errors.NewOCINotFoundError(fmt.Errorf("tag not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("DELETE", "/-/package/"+packageName+"/dist-tags/"+distTagName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 if tag not found, got %d. Body: %s", status, rr.Body.String())
		}
	})
	
	t.Run("deletetag fails", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.DeleteTagFunc = func(ctx context.Context, repo string, tag string) error {
			return fmt.Errorf("simulated DeleteTag error")
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("DELETE", "/-/package/"+packageName+"/dist-tags/"+distTagName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusInternalServerError {
			t.Errorf("expected 500 if DeleteTag fails, got %d. Body: %s", status, rr.Body.String())
		}
	})
}

func TestDistTagLsHandler(t *testing.T) {
	packageName := "my-disttag-pkg"
	ociRepoName := RepoType + "/" + packageName

	descV100 := ocispec.Descriptor{Digest: "sha256:v100manifest"}
	descV110 := ocispec.Descriptor{Digest: "sha256:v110manifest"}

	t.Run("success with multiple dist-tags", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return []string{"1.0.0", "1.1.0", "latest", "beta", "next"}, nil
		}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			switch ref {
			case "1.0.0": return descV100, nil
			case "1.1.0": return descV110, nil
			case "latest": return descV110, nil // latest -> 1.1.0
			case "beta": return descV100, nil  // beta -> 1.0.0
			case "next": return ocispec.Descriptor{Digest: "sha256:nonversionmanifest"}, nil // next points to something not a version
			}
			return ocispec.Descriptor{}, fmt.Errorf("Resolve mock: unexpected ref %s", ref)
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/-/package/"+packageName+"/dist-tags", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Fatalf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		var result map[string]string
		if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
			t.Fatalf("Could not decode response: %v. Body: %s", err, rr.Body.String())
		}
		if len(result) != 2 {
			t.Errorf("expected 2 dist-tags, got %d: %+v", len(result), result)
		}
		if result["latest"] != "1.1.0" {
			t.Errorf("expected latest to be 1.1.0, got %s", result["latest"])
		}
		if result["beta"] != "1.0.0" {
			t.Errorf("expected beta to be 1.0.0, got %s", result["beta"])
		}
	})

	t.Run("no tags found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return []string{}, nil
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/-/package/"+packageName+"/dist-tags", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusOK {
			t.Errorf("expected 200, got %d", status)
		}
		var result map[string]string
		if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {t.Fatalf("decode err: %v", err)}
		if len(result) != 0 {t.Errorf("expected empty map, got %+v", result)}
	})

	t.Run("listtags returns OCI not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return nil, errors.NewOCINotFoundError(fmt.Errorf("repo not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/-/package/"+packageName+"/dist-tags", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404, got %d", status)
		}
	})
}


func TestUnpublishPackageHandler(t *testing.T) {
	packageName := "my-unpublish-pkg"
	versionStr := "1.0.0"
	filename := fmt.Sprintf("%s-%s.tgz", packageName, versionStr)
	ociRepoName := RepoType + "/" + packageName
	revision := "some-rev" // Revision is part of URL but not strictly used by OCI logic

	t.Run("success specific version", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResetCalls()
		deleteCalled := false
		mockRegistry.DeleteTagFilesFunc = func(ctx context.Context, repo string, tag string) error {
			if repo == ociRepoName && tag == versionStr {
				deleteCalled = true
				return nil
			}
			return fmt.Errorf("DeleteTagFiles mock: unexpected call for %s@%s", repo, tag)
		}

		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("DELETE", "/"+packageName+"/-/"+filename+"/-rev/"+revision, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		var resp npmdata.ModifyResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("Could not decode response: %v", err)
		}
		if !resp.Ok {
			t.Errorf("expected response.Ok to be true, got false")
		}
		if !deleteCalled {
			t.Error("expected DeleteTagFiles to be called")
		}
		if len(mockRegistry.DeleteTagFilesCalledWith) != 1 {
			t.Errorf("DeleteTagFilesCalledWith not recorded correctly")
		} else {
			call := mockRegistry.DeleteTagFilesCalledWith[0]
			if call["repo"] != ociRepoName || call["tag"] != versionStr {
				t.Errorf("DeleteTagFiles called with wrong args: got %+v", call)
			}
		}
	})

	t.Run("version not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.DeleteTagFilesFunc = func(ctx context.Context, repo string, tag string) error {
			return errors.NewOCINotFoundError(fmt.Errorf("tag not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("DELETE", "/"+packageName+"/-/"+filename+"/-rev/"+revision, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 for version not found, got %d. Body: %s", status, rr.Body.String())
		}
	})

	t.Run("filename parsing fails", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("DELETE", "/"+packageName+"/-/badfilename/-rev/"+revision, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for bad filename, got %d. Body: %s", status, rr.Body.String())
		}
	})

	t.Run("entire package unpublish not implemented", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		handler, _ := newTestHandler(mockRegistry)
		// Path for entire package unpublish (no filename)
		req, _ := http.NewRequest("DELETE", "/"+packageName+"/-rev/"+revision, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotImplemented {
			t.Errorf("expected 501 for entire package unpublish, got %d. Body: %s", status, rr.Body.String())
		}
	})
}


func TestPublishPackageHandler(t *testing.T) {
	packageName := "my-publish-pkg"
	ociRepoName := RepoType + "/" + packageName
	versionStr := "1.0.0"
	tarballFilename := fmt.Sprintf("%s-%s.tgz", packageName, versionStr)
	tarballData := "test-tarball-data"
	encodedTarballData := base64.StdEncoding.EncodeToString([]byte(tarballData))
	
	// Calculate shasum for test data
	hasher := sha256.New()
	hasher.Write([]byte(tarballData))
	shasum := fmt.Sprintf("%x", hasher.Sum(nil))

	t.Run("success single version no dist-tags", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResetCalls() // Ensure clean slate for call verification

		pkgMeta := npmdata.PackageMetadata{
			Name: packageName,
			ID:   packageName, // Often same as name
			Versions: map[string]npmdata.VersionInfo{
				versionStr: {
					Name:    packageName,
					Version: versionStr,
					Dist:    npmdata.Dist{Shasum: shasum, Tarball: "http://example.com/" + tarballFilename}, // Tarball URL is for info, not used by handler directly
				},
			},
			Attachments: map[string]npmdata.AttachmentStub{
				tarballFilename: {ContentType: "application/octet-stream", Data: encodedTarballData, Length: len(tarballData)},
			},
		}
		bodyBytes, _ := json.Marshal(pkgMeta)

		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("PUT", "/"+packageName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusCreated {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusCreated, rr.Body.String())
		}

		var resp npmdata.ModifyResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("Could not decode response: %v", err)
		}
		if !resp.Ok || resp.ID != packageName {
			t.Errorf("unexpected response body: got %+v", resp)
		}

		if len(mockRegistry.AddFileCalledWith) != 2 {
			t.Errorf("expected AddFile to be called 2 times, got %d", len(mockRegistry.AddFileCalledWith))
		} else {
			// Tarball
			call0 := mockRegistry.AddFileCalledWith[0]
			if call0.OwningRepo != ociRepoName || call0.OwningTag != versionStr || call0.Name != tarballFilename || call0.MediaType != TarballArtifactType {
				t.Errorf("AddFile call 0 (tarball) mismatch: got %+v", call0)
			}
			// VersionInfo (package.json)
			call1 := mockRegistry.AddFileCalledWith[1]
			if call1.OwningRepo != ociRepoName || call1.OwningTag != versionStr || call1.Name != VersionInfoFilename || call1.MediaType != ArtifactType {
				t.Errorf("AddFile call 1 (versioninfo) mismatch: got %+v", call1)
			}
		}
	})

	t.Run("success with dist-tag 'latest'", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResetCalls()
	
		pkgMeta := npmdata.PackageMetadata{
			Name: packageName,
			ID:   packageName,
			DistTags: map[string]string{"latest": versionStr},
			Versions: map[string]npmdata.VersionInfo{
				versionStr: {Name: packageName, Version: versionStr, Dist: npmdata.Dist{Shasum: shasum}},
			},
			Attachments: map[string]npmdata.AttachmentStub{
				tarballFilename: {Data: encodedTarballData, Length: len(tarballData)},
			},
		}
		bodyBytes, _ := json.Marshal(pkgMeta)
	
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("PUT", "/"+packageName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	
		if status := rr.Code; status != http.StatusCreated {
			t.Fatalf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusCreated, rr.Body.String())
		}
		
		// Expected: 2 calls for versionStr (tarball, versionInfo) + 2 calls for "latest" tag (tarball, versionInfo)
		if len(mockRegistry.AddFileCalledWith) != 4 {
			t.Errorf("expected AddFile to be called 4 times, got %d", len(mockRegistry.AddFileCalledWith))
		} else {
			// Check "latest" tag calls (assuming they happen after version calls)
			latestTarballCall := mockRegistry.AddFileCalledWith[2]
			if latestTarballCall.OwningTag != "latest" || latestTarballCall.Name != tarballFilename {
				t.Errorf("AddFile call for 'latest' tarball incorrect: %+v", latestTarballCall)
			}
			latestVICall := mockRegistry.AddFileCalledWith[3]
			if latestVICall.OwningTag != "latest" || latestVICall.Name != VersionInfoFilename {
				t.Errorf("AddFile call for 'latest' versioninfo incorrect: %+v", latestVICall)
			}
		}
	})

	t.Run("invalid JSON body", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("PUT", "/"+packageName, strings.NewReader("this is not json"))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid JSON, got %d", status)
		}
	})

	t.Run("shasum mismatch", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		pkgMeta := npmdata.PackageMetadata{
			Name: packageName,
			Versions: map[string]npmdata.VersionInfo{
				versionStr: {Name: packageName, Version: versionStr, Dist: npmdata.Dist{Shasum: "incorrectshasum"}},
			},
			Attachments: map[string]npmdata.AttachmentStub{
				tarballFilename: {Data: encodedTarballData, Length: len(tarballData)},
			},
		}
		bodyBytes, _ := json.Marshal(pkgMeta)
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("PUT", "/"+packageName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for shasum mismatch, got %d. Body: %s", status, rr.Body.String())
		}
		if !contains(rr.Body.String(), "Shasum mismatch") {
			t.Errorf("expected 'Shasum mismatch' in error, got: %s", rr.Body.String())
		}
	})
	
	t.Run("addfile tarball fails", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.AddFileFunc = func(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error) {
			if f.Name == tarballFilename { // Fail only for tarball
				return nil, fmt.Errorf("simulated AddFile error for tarball")
			}
			return &oci.FileDescriptor{}, nil // Success for other files (like package.json)
		}
		pkgMeta := npmdata.PackageMetadata{
			Name: packageName, Versions: map[string]npmdata.VersionInfo{versionStr: {Name:packageName, Version:versionStr, Dist: npmdata.Dist{Shasum:shasum}}},
			Attachments: map[string]npmdata.AttachmentStub{tarballFilename: {Data: encodedTarballData}},
		}
		bodyBytes, _ := json.Marshal(pkgMeta)
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("PUT", "/"+packageName, bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusInternalServerError {
			t.Errorf("expected 500 for AddFile error, got %d. Body: %s", status, rr.Body.String())
		}
		if !contains(rr.Body.String(), "failed to push tarball") {
			t.Errorf("unexpected error message for AddFile tarball failure: %s", rr.Body.String())
		}
	})
}

func TestDownloadTarballHandler(t *testing.T) {
	packageName := "my-dl-pkg"
	versionStr := "0.9.1"
	filename := fmt.Sprintf("%s-%s.tgz", packageName, versionStr)
	ociRepoName := RepoType + "/" + packageName

	tarballContent := "this is tarball data"
	tarballDigest := "sha256:tarballdigest"
	tarballLayerSize := int64(len(tarballContent))
	manifestDigest := "sha256:manifestdigestfordl"

	t.Run("success", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			if repo == ociRepoName && ref == versionStr {
				return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: ocispec.Digest(manifestDigest)}, nil
			}
			return ocispec.Descriptor{}, fmt.Errorf("Resolve mock: unexpected call for %s@%s", repo, ref)
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			if repo == ociRepoName && digest == manifestDigest {
				return &ocispec.Manifest{
					Versioned: ocispec.Versioned{SchemaVersion: 2},
					MediaType: ocispec.MediaTypeImageManifest,
					Layers: []ocispec.Descriptor{
						// A VersionInfo layer might also be present
						{MediaType: ArtifactType, Digest: "sha256:anotherconfidigest", Size: 120, Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}},
						{MediaType: TarballArtifactType, Digest: ocispec.Digest(tarballDigest), Size: tarballLayerSize, Annotations: map[string]string{ocispec.AnnotationTitle: filename}},
					},
				}, nil
			}
			return nil, fmt.Errorf("GetManifest mock: unexpected call")
		}
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if repo == ociRepoName && digest == tarballDigest {
				return io.NopCloser(strings.NewReader(tarballContent)), nil
			}
			return nil, fmt.Errorf("GetBlob mock: unexpected call for blob %s", digest)
		}

		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/-/"+filename, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		if body := rr.Body.String(); body != tarballContent {
			t.Errorf("handler returned unexpected body: got %q want %q", body, tarballContent)
		}
		if ct := rr.Header().Get("Content-Type"); ct != DefaultTarballContentType {
			t.Errorf("wrong content type: got %q want %q", ct, DefaultTarballContentType)
		}
		if cd := rr.Header().Get("Content-Disposition"); cd != fmt.Sprintf(`attachment; filename="%s"`, filename) {
			t.Errorf("wrong content disposition: got %q want %q", cd, fmt.Sprintf(`attachment; filename="%s"`, filename))
		}
		if cl := rr.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", tarballLayerSize) {
			t.Errorf("wrong content length: got %q want %d", cl, tarballLayerSize)
		}
	})

	t.Run("version parsing fails", func(t *testing.T) {
		mockRegistry := &MockRegistry{} // Not used for this path
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/-/nodashesortgz.tgz", nil) // .tgz still present
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for bad filename (no version), got %d. Body: %s", status, rr.Body.String())
		}
	})
	
	t.Run("version parsing fails with only .tgz", func(t *testing.T) {
		mockRegistry := &MockRegistry{} 
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/-/.tgz", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("expected 400 for bad filename (.tgz only), got %d. Body: %s", status, rr.Body.String())
		}
	})


	t.Run("resolve fails with OCI not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{}, errors.NewOCINotFoundError(fmt.Errorf("resolve not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/-/"+filename, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 for Resolve OCI not found, got %d", status)
		}
	})

	t.Run("tarball layer not found in manifest", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: ocispec.Digest(manifestDigest)}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return &ocispec.Manifest{ // Manifest without the required tarball layer
				Versioned: ocispec.Versioned{SchemaVersion: 2},
				MediaType: ocispec.MediaTypeImageManifest,
				Layers:    []ocispec.Descriptor{{MediaType: ArtifactType, Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}}},
			}, nil
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/-/"+filename, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusInternalServerError { 
			t.Errorf("expected 500 when tarball layer is missing, got %d. Body: %s", status, rr.Body.String())
		}
		if !contains(rr.Body.String(), "tarball layer not found") {
			t.Errorf("unexpected error message for missing tarball layer: %s", rr.Body.String())
		}
	})

	t.Run("getblob for tarball fails with OCI not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: ocispec.Digest(manifestDigest)}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return &ocispec.Manifest{
				Versioned: ocispec.Versioned{SchemaVersion: 2},
				MediaType: ocispec.MediaTypeImageManifest,
				Layers:    []ocispec.Descriptor{{MediaType: TarballArtifactType, Digest: ocispec.Digest(tarballDigest), Annotations: map[string]string{ocispec.AnnotationTitle: filename}}},
			}, nil
		}
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if digest == ocispec.Digest(tarballDigest) {
				return nil, errors.NewOCINotFoundError(fmt.Errorf("blob not found"))
			}
			return nil, fmt.Errorf("GetBlob mock: unexpected call")
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/-/"+filename, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 for GetBlob OCI not found, got %d", status)
		}
	})
}

func TestGetPackageVersionMetadataHandler(t *testing.T) {
	packageName := "my-pkg"
	versionStr := "1.0.0"
	ociRepoName := RepoType + "/" + packageName
	
	versionInfo := npmdata.VersionInfo{Name: packageName, Version: versionStr, Description: "Specific version"}
	versionInfoJSON, _ := json.Marshal(versionInfo)
	versionInfoDigest := "sha256:versioninfodigest"
	manifestDigest := "sha256:manifestdigest"

	t.Run("success for version tag", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			if repo == ociRepoName && ref == versionStr {
				return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: ocispec.Digest(manifestDigest)}, nil
			}
			return ocispec.Descriptor{}, fmt.Errorf("Resolve mock: unexpected call for %s@%s", repo, ref)
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			if repo == ociRepoName && digest == manifestDigest {
				return &ocispec.Manifest{
					Versioned: ocispec.Versioned{SchemaVersion: 2},
					MediaType: ocispec.MediaTypeImageManifest,
					Layers: []ocispec.Descriptor{
						{MediaType: ArtifactType, Digest: ocispec.Digest(versionInfoDigest), Size: int64(len(versionInfoJSON)), Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}},
						// other layers like tarball could be here
					},
				}, nil
			}
			return nil, fmt.Errorf("GetManifest mock: unexpected call for %s@%s", repo, digest)
		}
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if repo == ociRepoName && digest == versionInfoDigest {
				return io.NopCloser(bytes.NewReader(versionInfoJSON)), nil
			}
			return nil, fmt.Errorf("GetBlob mock: unexpected call for %s@%s", repo, digest)
		}

		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/"+versionStr, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		var result npmdata.VersionInfo
		if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
			t.Fatalf("Could not decode response: %v", err)
		}
		if result.Version != versionStr || result.Description != "Specific version" {
			t.Errorf("handler returned unexpected body: got %+v want %+v", result, versionInfo)
		}
	})
	
	t.Run("success for dist tag 'latest'", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		latestVersionInfo := npmdata.VersionInfo{Name: packageName, Version: "1.1.0", Description: "Latest version"}
		latestVersionInfoJSON, _ := json.Marshal(latestVersionInfo)
		latestVersionInfoDigest := "sha256:latestversioninfodigest"
		latestManifestDigest := "sha256:latestmanifestdigest"

		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			if repo == ociRepoName && ref == "latest" { // Resolving 'latest' tag
				return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: ocispec.Digest(latestManifestDigest)}, nil
			}
			return ocispec.Descriptor{}, fmt.Errorf("Resolve mock: unexpected call for %s@%s", repo, ref)
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			if repo == ociRepoName && digest == latestManifestDigest {
				return &ocispec.Manifest{
					Versioned: ocispec.Versioned{SchemaVersion: 2},
					MediaType: ocispec.MediaTypeImageManifest,
					Layers: []ocispec.Descriptor{
						{MediaType: ArtifactType, Digest: ocispec.Digest(latestVersionInfoDigest), Size: int64(len(latestVersionInfoJSON)), Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}},
					},
				}, nil
			}
			return nil, fmt.Errorf("GetManifest mock: unexpected call for %s@%s", repo, digest)
		}
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if repo == ociRepoName && digest == latestVersionInfoDigest {
				return io.NopCloser(bytes.NewReader(latestVersionInfoJSON)), nil
			}
			return nil, fmt.Errorf("GetBlob mock: unexpected call for %s@%s", repo, digest)
		}

		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/latest", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}
		var result npmdata.VersionInfo
		if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
			t.Fatalf("Could not decode response: %v", err)
		}
		if result.Version != "1.1.0" || result.Description != "Latest version" {
			t.Errorf("handler returned unexpected body for 'latest' tag: got %+v want %+v", result, latestVersionInfo)
		}
	})


	t.Run("resolve fails with OCI not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{}, errors.NewOCINotFoundError(fmt.Errorf("not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/"+versionStr, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 for Resolve OCI not found, got %d", status)
		}
	})

	t.Run("getmanifest fails with OCI not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: ocispec.Digest(manifestDigest)}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return nil, errors.NewOCINotFoundError(fmt.Errorf("manifest not found"))
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/"+versionStr, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 for GetManifest OCI not found, got %d", status)
		}
	})
	
	t.Run("versioninfo layer not found in manifest", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: ocispec.Digest(manifestDigest)}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return &ocispec.Manifest{ // Manifest without the required VersionInfo layer
				Versioned: ocispec.Versioned{SchemaVersion: 2},
				MediaType: ocispec.MediaTypeImageManifest,
				Layers:    []ocispec.Descriptor{{MediaType: "application/octet-stream"}},
			}, nil
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/"+versionStr, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusInternalServerError {
			t.Errorf("expected 500 when versioninfo layer is missing, got %d", status)
		}
		if !contains(rr.Body.String(), "VersionInfo JSON layer not found") {
			t.Errorf("unexpected error message for missing versioninfo layer: %s", rr.Body.String())
		}
	})

	t.Run("getblob for versioninfo fails with OCI not found", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: ocispec.Digest(manifestDigest)}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return &ocispec.Manifest{
				Versioned: ocispec.Versioned{SchemaVersion: 2},
				MediaType: ocispec.MediaTypeImageManifest,
				Layers:    []ocispec.Descriptor{{MediaType: ArtifactType, Digest: ocispec.Digest(versionInfoDigest), Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}}},
			}, nil
		}
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if digest == versionInfoDigest {
				return nil, errors.NewOCINotFoundError(fmt.Errorf("blob not found"))
			}
			return nil, fmt.Errorf("GetBlob mock: unexpected call for %s@%s", repo, digest)
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/"+versionStr, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("expected 404 for GetBlob OCI not found, got %d", status)
		}
	})

	t.Run("corrupted versioninfo JSON", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: ocispec.Digest(manifestDigest)}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return &ocispec.Manifest{
				Versioned: ocispec.Versioned{SchemaVersion: 2},
				MediaType: ocispec.MediaTypeImageManifest,
				Layers:    []ocispec.Descriptor{{MediaType: ArtifactType, Digest: ocispec.Digest(versionInfoDigest), Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}}},
			}, nil
		}
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if digest == versionInfoDigest {
				return io.NopCloser(strings.NewReader("this is not json")), nil
			}
			return nil, fmt.Errorf("GetBlob mock: unexpected call")
		}
		handler, _ := newTestHandler(mockRegistry)
		req, _ := http.NewRequest("GET", "/"+packageName+"/"+versionStr, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if status := rr.Code; status != http.StatusInternalServerError {
			t.Errorf("expected 500 for corrupted JSON, got %d", status)
		}
		if !contains(rr.Body.String(), "failed to unmarshal npm version info") {
			t.Errorf("unexpected error message for corrupted JSON: %s", rr.Body.String())
		}
	})
}

func TestGetPackageMetadataHandler(t *testing.T) {
	packageName := "my-test-package"
	ociRepoName := RepoType + "/" + packageName

	// Success Case
	t.Run("success", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			if repo != ociRepoName {
				return nil, fmt.Errorf("ListTags called with wrong repo: got %s, want %s", repo, ociRepoName)
			}
			return []string{"1.0.0", "1.1.0"}, nil
		}

		versionInfo100 := npmdata.VersionInfo{Name: packageName, Version: "1.0.0", Description: "Version 1.0.0"}
		versionInfo110 := npmdata.VersionInfo{Name: packageName, Version: "1.1.0", Description: "Version 1.1.0"}
		
		versionInfo100JSON, _ := json.Marshal(versionInfo100)
		versionInfo110JSON, _ := json.Marshal(versionInfo110)

		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			if repo != ociRepoName {
				return ocispec.Descriptor{}, fmt.Errorf("Resolve called with wrong repo: got %s, want %s", repo, ociRepoName)
			}
			return ocispec.Descriptor{Digest: "sha256:" + ref + "manifest", MediaType: ocispec.MediaTypeImageManifest}, nil
		}

		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			if repo != ociRepoName {
				return nil, fmt.Errorf("GetManifest called with wrong repo: got %s, want %s", repo, ociRepoName)
			}
			var versionInfoDigest string
			if strings.Contains(digest, "1.0.0") {
				versionInfoDigest = "sha256:1.0.0config"
			} else if strings.Contains(digest, "1.1.0") {
				versionInfoDigest = "sha256:1.1.0config"
			}
			return &ocispec.Manifest{
				Versioned:   ocispec.Versioned{SchemaVersion: 2},
				MediaType:   ocispec.MediaTypeImageManifest, // This is the type of the manifest itself
				Layers: []ocispec.Descriptor{ // VersionInfo is stored as a layer
					{MediaType: ArtifactType, Digest: ocispec.Digest(versionInfoDigest), Size: 100, Annotations: map[string]string{ocispec.AnnotationTitle: VersionInfoFilename}},
					{MediaType: TarballArtifactType, Digest: "sha256:tarballdummy", Size: 1000, Annotations: map[string]string{ocispec.AnnotationTitle: packageName+"-"+ strings.ReplaceAll(strings.Split(versionInfoDigest, ":")[1], "config", "") +".tgz"}},
				},
			}, nil
		}
		
		mockRegistry.GetBlobFunc = func(ctx context.Context, repo string, digest string) (io.ReadCloser, error) {
			if repo != ociRepoName {
				return nil, fmt.Errorf("GetBlob called with wrong repo: got %s, want %s", repo, ociRepoName)
			}
			if digest == "sha256:1.0.0config" {
				return io.NopCloser(bytes.NewReader(versionInfo100JSON)), nil
			}
			if digest == "sha256:1.1.0config" {
				return io.NopCloser(bytes.NewReader(versionInfo110JSON)), nil
			}
			return nil, fmt.Errorf("unexpected blob digest: %s", digest)
		}

		handler, err := newTestHandler(mockRegistry)
		if err != nil {
			t.Fatalf("Failed to create test handler: %v", err)
		}

		req, _ := http.NewRequest("GET", "/"+packageName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusOK, rr.Body.String())
		}

		var result npmdata.PackageMetadata
		if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
			t.Fatalf("Could not decode response JSON: %v", err)
		}

		if result.Name != packageName {
			t.Errorf("expected package name %s, got %s", packageName, result.Name)
		}
		if len(result.Versions) != 2 {
			t.Errorf("expected 2 versions, got %d", len(result.Versions))
		}
		if _, ok := result.Versions["1.0.0"]; !ok {
			t.Error("expected version 1.0.0 to be present")
		}
		if _, ok := result.Versions["1.1.0"]; !ok {
			t.Error("expected version 1.1.0 to be present")
		}
		if result.DistTags["latest"] != "1.1.0" {
			t.Errorf("expected dist-tags.latest to be 1.1.0, got %s", result.DistTags["latest"])
		}
		if result.Versions["1.1.0"].Description != "Version 1.1.0" {
			t.Errorf("description for 1.1.0 is incorrect")
		}
		if result.Description != "Version 1.1.0" { // Top-level description from latest
			t.Errorf("top-level description is incorrect, expected from latest version")
		}
		if result.Time == nil || result.Time["created"] == "" || result.Time["modified"] == "" || result.Time["1.1.0"] == "" {
			 t.Errorf("Expected Time fields to be populated, got %v", result.Time)
		}
	})

	t.Run("package not found - no tags", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return []string{}, nil
		}

		handler, err := newTestHandler(mockRegistry)
		if err != nil {
			t.Fatalf("Failed to create test handler: %v", err)
		}

		req, _ := http.NewRequest("GET", "/"+packageName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusNotFound)
		}
		if !contains(rr.Body.String(), "package "+packageName+" not found (no versions)") {
			t.Errorf("unexpected error message: %s", rr.Body.String())
		}
	})
	
	t.Run("package not found - registry error", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		// Simulate an OCI "not found" style error. The actual error type might differ.
		// For this test, a generic error that our handler interprets as "not found" or passes up.
		// The current handler logic for ListTags error is:
		// http.Error(w, fmt.Sprintf("failed to list tags for %s: %v", ociRepoName, err), http.StatusInternalServerError)
		// So we expect 500 here. If we wanted 404, ListTagsFunc would need to return a specific error type
		// that errors.IsOCINotFound() would catch, or the handler logic changed.
		// Let's assume a generic server error for now.
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return nil, fmt.Errorf("simulated registry communication error")
		}

		handler, err := newTestHandler(mockRegistry)
		if err != nil {
			t.Fatalf("Failed to create test handler: %v", err)
		}

		req, _ := http.NewRequest("GET", "/"+packageName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusInternalServerError { // Based on current handler code
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusInternalServerError)
		}
		// We can check for part of the error message if needed.
	})

	t.Run("error fetching manifest for a version", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return []string{"1.0.0"}, nil
		}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: "sha256:1.0.0manifest"}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return nil, fmt.Errorf("simulated error getting manifest")
		}
		// No GetBlobFunc needed as GetManifest fails.

		handler, err := newTestHandler(mockRegistry)
		if err != nil {
			t.Fatalf("Failed to create test handler: %v", err)
		}
		
		req, _ := http.NewRequest("GET", "/"+packageName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		// Since GetPackageMetadataHandler logs and continues if a manifest fetch fails,
		// and this is the only version, it should result in "no processable versions found".
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusNotFound, rr.Body.String())
		}
		if !contains(rr.Body.String(), "no processable versions found") {
			t.Errorf("expected 'no processable versions found', got: %s", rr.Body.String())
		}
	})

	t.Run("versioninfo layer not found in manifest", func(t *testing.T) {
		mockRegistry := &MockRegistry{}
		mockRegistry.ListTagsFunc = func(ctx context.Context, repo string) ([]string, error) {
			return []string{"1.0.0"}, nil
		}
		mockRegistry.ResolveFunc = func(ctx context.Context, repo string, ref string) (ocispec.Descriptor, error) {
			return ocispec.Descriptor{Digest: "sha256:1.0.0manifest"}, nil
		}
		mockRegistry.GetManifestFunc = func(ctx context.Context, repo string, digest string) (*ocispec.Manifest, error) {
			return &ocispec.Manifest{ // Manifest with no suitable VersionInfo layer
				Versioned: ocispec.Versioned{SchemaVersion: 2},
				MediaType: ocispec.MediaTypeImageManifest,
				Layers:    []ocispec.Descriptor{{MediaType: "application/octet-stream"}},
			}, nil
		}

		handler, err := newTestHandler(mockRegistry)
		if err != nil {
			t.Fatalf("Failed to create test handler: %v", err)
		}

		req, _ := http.NewRequest("GET", "/"+packageName, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		
		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("handler returned wrong status code: got %v want %v. Body: %s", status, http.StatusNotFound, rr.Body.String())
		}
		if !contains(rr.Body.String(), "no processable versions found") {
			t.Errorf("expected 'no processable versions found', got: %s", rr.Body.String())
		}
	})

}


func newTestHandler(registry *MockRegistry) (http.Handler, error) {
	h, err := NewHandler(registry)
	if err != nil {
		return nil, fmt.Errorf("failed to create npm.Handler: %w", err)
	}
	return h.Mux(), nil
}

// Helper function to check for a substring in a string (useful for error messages)
func contains(s, substr string) bool {
    return strings.Contains(s, substr)
}
