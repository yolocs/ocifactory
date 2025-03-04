package oci

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

func TestNewFakeRegistry(t *testing.T) {
	t.Parallel()

	registry := NewFakeRegistry()
	if registry == nil {
		t.Fatal("NewFakeRegistry() returned nil")
	}

	if registry.Files == nil {
		t.Error("Files map is nil")
	}

	if registry.Tags == nil {
		t.Error("Tags map is nil")
	}
}

func TestFakeRegistry_AddTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		repo     string
		tag      string
		existing map[string][]string
		want     map[string][]string
	}{
		{
			name: "add tag to new repo",
			repo: "example/repo",
			tag:  "v1.0.0",
			existing: map[string][]string{
				"other/repo": {"v1.0.0"},
			},
			want: map[string][]string{
				"other/repo":   {"v1.0.0"},
				"example/repo": {"v1.0.0"},
			},
		},
		{
			name: "add tag to existing repo",
			repo: "example/repo",
			tag:  "v1.1.0",
			existing: map[string][]string{
				"example/repo": {"v1.0.0"},
			},
			want: map[string][]string{
				"example/repo": {"v1.0.0", "v1.1.0"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := NewFakeRegistry()
			registry.Tags = tt.existing

			registry.AddTag(tt.repo, tt.tag)

			if diff := cmp.Diff(tt.want, registry.Tags); diff != "" {
				t.Errorf("Tags mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFakeRegistry_AddFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		file       *RepoFile
		content    string
		wantErr    bool
		wantDigest string
	}{
		{
			name: "add simple file",
			file: &RepoFile{
				OwningRepo: "example/repo",
				OwningTag:  "v1.0.0",
				Name:       "test.txt",
				MediaType:  "text/plain",
			},
			content:    "test content",
			wantErr:    false,
			wantDigest: "sha256:6ae8a75555209fd6c44157c0aed8016e763ff435a19cf186f76863140143ff72",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := NewFakeRegistry()
			ctx := context.Background()

			desc, err := registry.AddFile(ctx, tt.file, strings.NewReader(tt.content))
			if (err != nil) != tt.wantErr {
				t.Errorf("AddFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			// Check if file was stored correctly
			key := tt.file.OwningRepo + "/" + tt.file.OwningTag + "/" + tt.file.Name
			content, ok := registry.Files[key]
			if !ok {
				t.Errorf("File not found in registry: %s", key)
				return
			}

			if string(content) != tt.content {
				t.Errorf("File content = %q, want %q", string(content), tt.content)
			}

			// Check if tag was added
			tags, ok := registry.Tags[tt.file.OwningRepo]
			if !ok {
				t.Errorf("Repo not found in tags: %s", tt.file.OwningRepo)
				return
			}

			found := false
			for _, tag := range tags {
				if tag == tt.file.OwningTag {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Tag %q not found for repo %q", tt.file.OwningTag, tt.file.OwningRepo)
			}

			// Check descriptor
			if desc.File.Digest.String() != tt.wantDigest {
				t.Errorf("Descriptor digest = %q, want %q", desc.File.Digest.String(), tt.wantDigest)
			}

			if desc.File.MediaType != tt.file.MediaType {
				t.Errorf("Descriptor media type = %q, want %q", desc.File.MediaType, tt.file.MediaType)
			}

			if desc.File.Annotations[FileNameAnnotation] != tt.file.Name {
				t.Errorf("Descriptor filename annotation = %q, want %q",
					desc.File.Annotations[FileNameAnnotation], tt.file.Name)
			}
		})
	}
}

func TestFakeRegistry_ReadFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupFile  *RepoFile
		setupData  string
		readFile   *RepoFile
		wantErr    bool
		wantErrIs  error
		wantDigest string
		wantData   string
	}{
		{
			name: "read existing file",
			setupFile: &RepoFile{
				OwningRepo: "example/repo",
				OwningTag:  "v1.0.0",
				Name:       "test.txt",
				MediaType:  "text/plain",
			},
			setupData: "test content",
			readFile: &RepoFile{
				OwningRepo: "example/repo",
				OwningTag:  "v1.0.0",
				Name:       "test.txt",
				MediaType:  "text/plain",
			},
			wantErr:    false,
			wantDigest: "sha256:6ae8a75555209fd6c44157c0aed8016e763ff435a19cf186f76863140143ff72",
			wantData:   "test content",
		},
		{
			name: "file not found",
			readFile: &RepoFile{
				OwningRepo: "example/repo",
				OwningTag:  "v1.0.0",
				Name:       "nonexistent.txt",
				MediaType:  "text/plain",
			},
			wantErr:   true,
			wantErrIs: errdef.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := NewFakeRegistry()
			ctx := context.Background()

			// Setup file if needed
			if tt.setupFile != nil {
				_, err := registry.AddFile(ctx, tt.setupFile, strings.NewReader(tt.setupData))
				if err != nil {
					t.Fatalf("Failed to set up file: %v", err)
				}
			}

			// Read file
			desc, rc, err := registry.ReadFile(ctx, tt.readFile)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReadFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				if tt.wantErrIs != nil && !strings.Contains(err.Error(), tt.wantErrIs.Error()) {
					t.Errorf("ReadFile() error = %v, want error containing %v", err, tt.wantErrIs)
				}
				return
			}

			// Check descriptor
			if desc.File.Digest.String() != tt.wantDigest {
				t.Errorf("Descriptor digest = %q, want %q", desc.File.Digest.String(), tt.wantDigest)
			}

			// Check content
			data, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("Failed to read content: %v", err)
			}
			defer rc.Close()

			if string(data) != tt.wantData {
				t.Errorf("File content = %q, want %q", string(data), tt.wantData)
			}
		})
	}
}

func TestFakeRegistry_ListTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setupTags map[string][]string
		repo      string
		want      []string
	}{
		{
			name: "list existing tags",
			setupTags: map[string][]string{
				"example/repo": {"v1.0.0", "v1.1.0"},
				"other/repo":   {"latest"},
			},
			repo: "example/repo",
			want: []string{"v1.0.0", "v1.1.0"},
		},
		{
			name: "repo not found",
			setupTags: map[string][]string{
				"example/repo": {"v1.0.0"},
			},
			repo: "nonexistent/repo",
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := NewFakeRegistry()
			registry.Tags = tt.setupTags

			ctx := context.Background()
			got, err := registry.ListTags(ctx, tt.repo)
			if err != nil {
				t.Errorf("ListTags() error = %v", err)
				return
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ListTags() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func Test_generateDescriptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
		file    *RepoFile
		want    ocispec.Descriptor
	}{
		{
			name:    "generate descriptor for text file",
			content: []byte("test content"),
			file: &RepoFile{
				Name:      "test.txt",
				MediaType: "text/plain",
			},
			want: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    digest.Digest("sha256:6ae8a75555209fd6c44157c0aed8016e763ff435a19cf186f76863140143ff72"),
				Size:      12,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := generateDescriptor(tt.content, tt.file)

			if got.MediaType != tt.want.MediaType {
				t.Errorf("MediaType = %q, want %q", got.MediaType, tt.want.MediaType)
			}

			if got.Digest != tt.want.Digest {
				t.Errorf("Digest = %q, want %q", got.Digest, tt.want.Digest)
			}

			if got.Size != tt.want.Size {
				t.Errorf("Size = %d, want %d", got.Size, tt.want.Size)
			}

			if got.Annotations[FileNameAnnotation] != tt.want.Annotations[FileNameAnnotation] {
				t.Errorf("Filename annotation = %q, want %q",
					got.Annotations[FileNameAnnotation], tt.want.Annotations[FileNameAnnotation])
			}
		})
	}
}
