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

	"github.com/abcxyz/pkg/testutil"
	"github.com/google/go-cmp/cmp"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

			if !cmp.Equal(got.baseURL, tt.baseURL) {
				t.Errorf("NewRegistry() baseURL diff = %v", cmp.Diff(tt.baseURL, got.baseURL))
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

func TestAddReadRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

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

	t.Run("read file not found", func(t *testing.T) {
		_, _, err := r.ReadFile(ctx, f0)
		if diff := testutil.DiffErrString(err, "not found"); diff != "" {
			t.Errorf("ReadFile() error diff: %s", diff)
		}
	})

	var wantDesc *FileDescriptor
	content := "hello world"

	t.Run("add file", func(t *testing.T) {
		desc, err := r.AddFile(ctx, f0, strings.NewReader(content))
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("AddFile() error diff: %s", diff)
		}
		wantDesc = desc
	})

	t.Run("read file", func(t *testing.T) {
		gotDesc, r, err := r.ReadFile(ctx, f0)
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("ReadFile() error diff: %s", diff)
		}
		defer r.Close()

		if diff := cmp.Diff(wantDesc, gotDesc); diff != "" {
			t.Errorf("ReadFile() desc diff: %s", diff)
		}

		gotContent, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}

		if got, want := string(gotContent), content; got != want {
			t.Errorf("ReadAll() content = %v, want %v", got, want)
		}
	})

	t.Run("append tags", func(t *testing.T) {
		err := r.AppendRefs(ctx, "foobar", "v0", "tag1", "tag2")
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("AppendTags() error diff: %s", diff)
		}
	})

	t.Run("read file by ref", func(t *testing.T) {
		gotDesc, r, err := r.ReadFile(ctx, &RepoFile{
			OwningRepo: "foobar",
			RefTag:     "tag1",
			Name:       "test.txt",
			Digest:     "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		})
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("ReadFile() error diff: %s", diff)
		}
		defer r.Close()

		if diff := cmp.Diff(wantDesc, gotDesc); diff != "" {
			t.Errorf("ReadFile() desc diff: %s", diff)
		}

		gotContent, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}

		if got, want := string(gotContent), content; got != want {
			t.Errorf("ReadAll() content = %v, want %v", got, want)
		}
	})

	t.Run("list tags", func(t *testing.T) {
		wantTags := []string{"v0"}
		gotTags, err := r.ListTags(ctx, "foobar")
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("ListTags() error diff: %s", diff)
		}
		if diff := cmp.Diff(wantTags, gotTags); diff != "" {
			t.Errorf("ListTags() tags diff: %s", diff)
		}
	})

	t.Run("list files", func(t *testing.T) {
		wantFiles := []*RepoFile{f0}
		gotFiles, err := r.ListFiles(ctx, "foobar")
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("ListFiles() error diff: %s", diff)
		}
		if diff := cmp.Diff(wantFiles, gotFiles); diff != "" {
			t.Errorf("ListFiles() files diff: %s", diff)
		}
	})

	t.Run("delete tag files", func(t *testing.T) {
		err := r.DeleteTagFiles(ctx, "foobar", "v0")
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("DeleteTagFiles() error diff: %s", diff)
		}

		gotFiles, err := r.ListFiles(ctx, "foobar")
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("ListFiles() error diff: %s", diff)
		}
		if len(gotFiles) != 0 {
			t.Errorf("ListFiles() files = %v, want %v", gotFiles, []*RepoFile{})
		}
	})
}
