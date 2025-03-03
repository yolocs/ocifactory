package oci

import (
	"context"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/abcxyz/pkg/testutil"
	"github.com/google/go-cmp/cmp"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
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
	memRepo := memory.New()
	r.newBackendFunc = func(ctx context.Context, f *RepoFile) (oras.Target, error) {
		return memRepo, nil
	}

	f := &RepoFile{
		OwningRepo: "foobar",
		OwningTag:  "v0",
		Name:       "test.txt",
	}

	t.Run("read file not found", func(t *testing.T) {
		_, _, err := r.ReadFile(ctx, f)
		if diff := testutil.DiffErrString(err, "not found"); diff != "" {
			t.Errorf("ReadFile() error diff: %s", diff)
		}
	})

	var wantDesc *FileDescriptor
	content := "hello world"

	t.Run("add file", func(t *testing.T) {
		desc, err := r.AddFile(ctx, f, strings.NewReader(content))
		if diff := testutil.DiffErrString(err, ""); diff != "" {
			t.Errorf("AddFile() error diff: %s", diff)
		}
		wantDesc = desc
	})

	t.Run("read file", func(t *testing.T) {
		gotDesc, r, err := r.ReadFile(ctx, f)
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
}
