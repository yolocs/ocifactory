package oci

import (
	"context"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/testutil"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

func TestNewRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		baseURL          *url.URL
		opts             []RegistryOption
		wantErr          bool
		wantLandingDir   string
		wantArtifactType string
	}{
		{
			name:             "default options",
			baseURL:          &url.URL{Scheme: "https", Host: "example.com"},
			opts:             nil,
			wantErr:          false,
			wantLandingDir:   os.TempDir(),
			wantArtifactType: DefaultArtifactType,
		},
		{
			name:             "with landing dir",
			baseURL:          &url.URL{Scheme: "https", Host: "example.com"},
			opts:             []RegistryOption{WithLandingDir("/tmp")},
			wantErr:          false,
			wantLandingDir:   "/tmp",
			wantArtifactType: DefaultArtifactType,
		},
		{
			name:             "with artifact type",
			baseURL:          &url.URL{Scheme: "https", Host: "example.com"},
			opts:             []RegistryOption{WithArtifactType("application/custom")},
			wantErr:          false,
			wantLandingDir:   os.TempDir(),
			wantArtifactType: "application/custom",
		},
		{
			name:             "with both options",
			baseURL:          &url.URL{Scheme: "https", Host: "example.com"},
			opts:             []RegistryOption{WithLandingDir("/tmp"), WithArtifactType("application/custom")},
			wantErr:          false,
			wantLandingDir:   "/tmp",
			wantArtifactType: "application/custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewRegistry(tt.baseURL, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewRegistry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if got.landingDir != tt.wantLandingDir {
				t.Errorf("NewRegistry() landingDir = %v, want %v", got.landingDir, tt.wantLandingDir)
			}

			if got.artifactType != tt.wantArtifactType {
				t.Errorf("NewRegistry() artifactType = %v, want %v", got.artifactType, tt.wantArtifactType)
			}

			if diff := cmp.Diff(tt.baseURL, got.baseURL); diff != "" {
				t.Errorf("NewRegistry() baseURL mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDetectFileMediaType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		file     *RepoFile
		expected string
	}{
		{
			name: "with explicit media type",
			file: &RepoFile{
				Name:      "test.bin",
				MediaType: "application/custom",
			},
			expected: "application/custom",
		},
		{
			name: "txt file",
			file: &RepoFile{
				Name: "test.txt",
			},
			expected: "text/plain",
		},
		{
			name: "html file",
			file: &RepoFile{
				Name: "test.html",
			},
			expected: "text/html",
		},
		{
			name: "xml file",
			file: &RepoFile{
				Name: "test.xml",
			},
			expected: "application/xml",
		},
		{
			name: "json file",
			file: &RepoFile{
				Name: "test.json",
			},
			expected: "application/json",
		},
		{
			name: "tar file",
			file: &RepoFile{
				Name: "test.tar",
			},
			expected: "application/x-tar",
		},
		{
			name: "gz file",
			file: &RepoFile{
				Name: "test.gz",
			},
			expected: "application/x-gzip",
		},
		{
			name: "tgz file",
			file: &RepoFile{
				Name: "test.tgz",
			},
			expected: "application/x-gzip",
		},
		{
			name: "zip file",
			file: &RepoFile{
				Name: "test.zip",
			},
			expected: "application/zip",
		},
		{
			name: "unknown extension",
			file: &RepoFile{
				Name: "test.unknown",
			},
			expected: "application/octet-stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := detectFileMediaType(tt.file)
			if got != tt.expected {
				t.Errorf("detectFileMediaType() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestUpsertFileLayer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		existingLayers []ocispec.Descriptor
		newFileDesc    ocispec.Descriptor
		wantUpdated    bool
		wantLayers     []ocispec.Descriptor
	}{
		{
			name:           "add new file",
			existingLayers: []ocispec.Descriptor{},
			newFileDesc: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    "sha256:123",
				Size:      100,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
			wantUpdated: true,
			wantLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
		},
		{
			name: "update existing file with different digest",
			existingLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
			newFileDesc: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    "sha256:456",
				Size:      200,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
			wantUpdated: true,
			wantLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:456",
					Size:      200,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
		},
		{
			name: "no update for same digest",
			existingLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
			newFileDesc: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    "sha256:123",
				Size:      100,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
			wantUpdated: false,
			wantLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotUpdated, gotLayers := upsertFileLayer(tt.existingLayers, tt.newFileDesc)
			if gotUpdated != tt.wantUpdated {
				t.Errorf("upsertFileLayer() updated = %v, want %v", gotUpdated, tt.wantUpdated)
			}

			if diff := cmp.Diff(tt.wantLayers, gotLayers); diff != "" {
				t.Errorf("upsertFileLayer() layers mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

type inMemoryRepo struct {
	*memory.Store
	allTags map[string]string
}

func (r *inMemoryRepo) Tags(_ context.Context, _ string, fn func(tags []string) error) error {
	return fn(slices.Collect(maps.Keys(r.allTags)))
}

func (r *inMemoryRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	r.allTags[reference] = desc.Digest.String()
	return r.Store.Tag(ctx, desc, reference)
}

func (r *inMemoryRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	for tag, digest := range r.allTags {
		if digest == target.Digest.String() {
			delete(r.allTags, tag)
		}
	}
	return nil
}

// Intercept the Resolve call to return ErrNotFound if the target has been deleted.
func (r *inMemoryRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	target, err := r.Store.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	if _, ok := r.allTags[reference]; !ok {
		return ocispec.Descriptor{}, errdef.ErrNotFound
	}

	return target, nil
}

// TestAddReadRoundtrip exercises the full add → read → ref → list → delete
// lifecycle in sequence. It is intentionally a single non-subtest function:
// each phase depends on state established by the previous one (the in-memory
// backend, the recorded wantDesc), so subtests with t.Parallel would race.
func TestAddReadRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	r, err := NewRegistry(
		&url.URL{Scheme: "https", Host: "example.com"},
		WithLandingDir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	// Override the newBackendFunc to use the memory backend.
	memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{"v0": "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"}}
	r.newBackendFunc = func(ctx context.Context, f *RepoFile) (destRepo, error) {
		return memRepo, nil
	}

	f0 := &RepoFile{
		OwningRepo: "foobar",
		OwningTag:  "v0",
		Name:       "test.txt",
		Digest:     "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
	}

	// Read missing file -> ErrNotFound.
	if _, _, err := r.ReadFile(ctx, f0); err == nil {
		t.Errorf("ReadFile() before AddFile: got nil error, want not-found")
	} else if diff := testutil.DiffErrString(err, "not found"); diff != "" {
		t.Errorf("ReadFile() error diff: %s", diff)
	}

	// Add file.
	const content = "hello world"
	wantDesc, err := r.AddFile(ctx, f0, strings.NewReader(content))
	if err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}

	// Read by owning tag.
	if gotDesc, body, err := r.ReadFile(ctx, f0); err != nil {
		t.Errorf("ReadFile() by owning tag: %v", err)
	} else {
		defer body.Close()
		if diff := cmp.Diff(wantDesc, gotDesc); diff != "" {
			t.Errorf("ReadFile() desc mismatch (-want +got):\n%s", diff)
		}
		gotContent, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}
		if string(gotContent) != content {
			t.Errorf("ReadAll() content = %q, want %q", string(gotContent), content)
		}
	}

	// Append ref tags.
	if err := r.AppendRefs(ctx, "foobar", "v0", "tag1", "tag2"); err != nil {
		t.Fatalf("AppendRefs() error = %v", err)
	}

	// Read by ref tag.
	if gotDesc, body, err := r.ReadFile(ctx, &RepoFile{
		OwningRepo: "foobar",
		RefTag:     "tag1",
		Name:       "test.txt",
		Digest:     "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
	}); err != nil {
		t.Errorf("ReadFile() by ref tag: %v", err)
	} else {
		defer body.Close()
		if diff := cmp.Diff(wantDesc, gotDesc); diff != "" {
			t.Errorf("ReadFile() by ref tag desc mismatch (-want +got):\n%s", diff)
		}
		gotContent, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}
		if string(gotContent) != content {
			t.Errorf("ReadAll() content = %q, want %q", string(gotContent), content)
		}
	}

	// List tags.
	gotTags, err := r.ListTags(ctx, "foobar")
	if err != nil {
		t.Errorf("ListTags() error = %v", err)
	}
	if diff := cmp.Diff([]string{"v0"}, gotTags); diff != "" {
		t.Errorf("ListTags() mismatch (-want +got):\n%s", diff)
	}

	// List files.
	gotFiles, err := r.ListFiles(ctx, "foobar")
	if err != nil {
		t.Errorf("ListFiles() error = %v", err)
	}
	if diff := cmp.Diff([]*RepoFile{f0}, gotFiles); diff != "" {
		t.Errorf("ListFiles() mismatch (-want +got):\n%s", diff)
	}

	// Delete tag files, then re-list.
	if err := r.DeleteTagFiles(ctx, "foobar", "v0"); err != nil {
		t.Errorf("DeleteTagFiles() error = %v", err)
	}
	gotFiles, err = r.ListFiles(ctx, "foobar")
	if err != nil {
		t.Errorf("ListFiles() after delete: %v", err)
	}
	if len(gotFiles) != 0 {
		t.Errorf("ListFiles() after delete = %v, want empty", gotFiles)
	}
}
