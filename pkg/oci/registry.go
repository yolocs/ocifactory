package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/cred"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	DefaultArtifactType = "application/vnd.ocifactory.generic"
	FileNameAnnotation  = "ocifactory.file.title"
)

type destRepo interface {
	oras.Target
	registry.TagLister
	content.Tagger
	content.Deleter
}

type Registry struct {
	baseURL      *url.URL
	landingDir   string
	artifactType string

	// Used in unit test to stub with in memory backend.
	newBackendFunc func(ctx context.Context, f *RepoFile) (destRepo, error)
}

type RegistryOption func(*Registry) error

// WithLandingDir sets the directory where files are stored before being uploaded.
// The provided dir must already exist. The default is the current directory.
func WithLandingDir(dir string) RegistryOption {
	return func(r *Registry) error {
		r.landingDir = dir
		return nil
	}
}

func WithArtifactType(artifactType string) RegistryOption {
	return func(r *Registry) error {
		r.artifactType = artifactType
		return nil
	}
}

// RepoFile represents a file in an OCI repository.
type RepoFile struct {
	OwningRepo string // Repository the owns the file. Usually what's right after the registy host.
	OwningTag  string // Usually the package version that owns the file.
	RefTag     string // Tag that points to the file. Could be empty.
	Name       string // File name.
	MediaType  string // Media type of the file. If not provided, it will be inferred from the file name.
	Digest     string // Digest of the file. If provided, it will be used to cross check retrieved or calculated digest.
}

type FileDescriptor struct {
	Manifest ocispec.Descriptor // The owning manifest descriptor.
	File     ocispec.Descriptor
}

func NewRegistry(baseURL *url.URL, opt ...RegistryOption) (*Registry, error) {
	r := &Registry{
		baseURL:      baseURL,
		landingDir:   os.TempDir(), // Default to the system tmp directory.
		artifactType: DefaultArtifactType,
	}
	r.newBackendFunc = r.newBackend

	for _, o := range opt {
		if err := o(r); err != nil {
			return nil, err
		}
	}

	return r, nil
}

// DeleteTagFiles deletes all files in a tag.
// It's used to delete a tag and all its files.
func (r *Registry) DeleteTagFiles(ctx context.Context, repo string, tag string) error {
	backendRepo, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return err
	}

	return r.deleteTagFiles(ctx, backendRepo, tag)
}

// DeleteRepoFiles deletes all files in a repository.
// It's used to delete a repository and all its tags.
func (r *Registry) DeleteRepoFiles(ctx context.Context, repo string) error {
	backendRepo, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return err
	}

	tags, err := r.listTags(ctx, backendRepo)
	if err != nil {
		return err
	}

	for _, tag := range tags {
		if strings.HasPrefix(tag, "ref_") {
			continue // Ignore refs otherwise we'll get duplicated files.
		}
		if err := r.deleteTagFiles(ctx, backendRepo, tag); err != nil {
			return err
		}
	}

	return nil
}

func (r *Registry) deleteTagFiles(ctx context.Context, backendRepo destRepo, tag string) error {
	manifestDesc, err := backendRepo.Resolve(ctx, tag)
	if err != nil {
		return fmt.Errorf("failed to resolve manifest for tag %q: %w", tag, err)
	}

	if err := backendRepo.Delete(ctx, manifestDesc); err != nil {
		return fmt.Errorf("failed to delete manifest for tag %q: %w", tag, err)
	}
	return nil
}

// AppendRefs appends tags to a manifest.
// The canonical tag is the tag that points to the manifest.
// The tags are the tags to append to the manifest.
// The tags are appended in the order they are provided.
// The canonical tag is not included in the tags list.
func (r *Registry) AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error {
	backendRepo, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return err
	}

	manifestDesc, err := backendRepo.Resolve(ctx, canonicalTag)
	if err != nil {
		return fmt.Errorf("failed to resolve manifest for canonical tag %q: %w", canonicalTag, err)
	}

	for _, ref := range refs {
		if err := backendRepo.Tag(ctx, manifestDesc, "ref_"+ref); err != nil {
			return fmt.Errorf("failed to tag manifest for ref %q: %w", ref, err)
		}
	}

	return nil
}

// AddFile adds a file to the registry.
// The file is first uploaded to the landing zone, then to the OCI store, and finally to the backend repository.
// If the file already exists in the backend repository, it will be updated if and only if the digest has changed.
// Returns the updated manifest descriptor and the file descriptor.
func (r *Registry) AddFile(ctx context.Context, f *RepoFile, ro io.Reader) (*FileDescriptor, error) {
	if strings.HasPrefix(f.OwningTag, "ref_") {
		return nil, fmt.Errorf("canonical tag cannot be prefixed with ref_; got %q", f.OwningTag)
	}

	// Load the file in the landing zone.
	tmpFile, err := r.landFile(ro)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile)

	// Load the file in the OCI store.
	fs, fileDesc, err := r.loadFile(ctx, tmpFile, f)
	if err != nil {
		return nil, err
	}
	defer fs.Close()

	// Create the backend repository for the file.
	backendRepo, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return nil, err
	}

	manifestDesc, err := backendRepo.Resolve(ctx, f.OwningTag)
	if err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return nil, fmt.Errorf("failed to resolve manifest for tag %q: %w", f.OwningTag, err)
	}

	layers, err := manifestLayers(ctx, backendRepo, manifestDesc)
	if err != nil {
		return nil, err
	}
	updated, layers := upsertFileLayer(layers, fileDesc)
	if !updated { // No need to update the manifest if the file hasn't changed.
		return &FileDescriptor{Manifest: manifestDesc, File: fileDesc}, nil
	}

	// Pack the updated manifest
	packOpts := oras.PackManifestOptions{Layers: layers}
	newManifestDesc, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, r.artifactType, packOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to pack new manifest: %w", err)
	}
	if err := fs.Tag(ctx, newManifestDesc, f.OwningTag); err != nil {
		return nil, fmt.Errorf("failed to tag new manifest: %w", err)
	}

	// Push the manifest and tag it
	if _, err := oras.Copy(ctx, fs, f.OwningTag, backendRepo, f.OwningTag, oras.DefaultCopyOptions); err != nil {
		return nil, fmt.Errorf("failed to copy manifest to backend repo: %w", err)
	}

	return &FileDescriptor{Manifest: newManifestDesc, File: fileDesc}, nil
}

// ReadFile reads a file from the registry.
// Returns the file descriptor and a reader for the file.
// It's allowed to use a ref tag to read a file. Set it in the RepoFile.RefTag field.
func (r *Registry) ReadFile(ctx context.Context, f *RepoFile) (*FileDescriptor, io.ReadCloser, error) {
	if f.OwningTag == "" && f.RefTag == "" {
		return nil, nil, fmt.Errorf("either OwningTag or RefTag must be set")
	}

	t := f.OwningTag
	if t == "" {
		t = "ref_" + f.RefTag
	}

	backendRepo, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return nil, nil, err
	}

	manifestDesc, err := backendRepo.Resolve(ctx, t)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve manifest for tag %q: %w", t, err)
	}

	layers, err := manifestLayers(ctx, backendRepo, manifestDesc)
	if err != nil {
		return nil, nil, err
	}

	for _, l := range layers {
		if l.Annotations[FileNameAnnotation] == f.Name {
			if f.Digest != "" && string(l.Digest) != f.Digest {
				return nil, nil, fmt.Errorf("file digest mismatch: %q != %q", l.Digest, f.Digest)
			}
			rc, err := backendRepo.Fetch(ctx, l)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to fetch file: %w", err)
			}
			return &FileDescriptor{Manifest: manifestDesc, File: l}, rc, nil
		}
	}

	return nil, nil, fmt.Errorf("file %q not found in manifest: %w", f.Name, errdef.ErrNotFound)
}

// ListTags lists the tags for a repository.
func (r *Registry) ListTags(ctx context.Context, repo string) ([]string, error) {
	backendRepo, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return nil, err
	}

	return r.listTags(ctx, backendRepo)
}

func (r *Registry) listTags(ctx context.Context, backendRepo destRepo) ([]string, error) {
	tags, err := registry.Tags(ctx, backendRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to list tags: %w", err)
	}

	var excludeRefs []string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "ref_") {
			excludeRefs = append(excludeRefs, tag)
		}
	}

	return excludeRefs, nil
}

// ListFiles lists the files in a repository.
func (r *Registry) ListFiles(ctx context.Context, repo string) ([]*RepoFile, error) {
	backendRepo, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return nil, err
	}

	tags, err := r.listTags(ctx, backendRepo)
	if err != nil {
		return nil, err
	}

	var files []*RepoFile

	for _, tag := range tags {
		manifestDesc, err := backendRepo.Resolve(ctx, tag)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve manifest for tag %q: %w", tag, err)
		}

		layers, err := manifestLayers(ctx, backendRepo, manifestDesc)
		if err != nil {
			return nil, fmt.Errorf("failed to get manifest layers: %w", err)
		}

		for _, l := range layers {
			if l.Annotations != nil && l.Annotations[FileNameAnnotation] != "" {
				files = append(files, &RepoFile{
					Name:       l.Annotations[FileNameAnnotation],
					OwningRepo: repo,
					OwningTag:  tag,
					Digest:     string(l.Digest),
				})
			}
		}
	}

	return files, nil
}

func (r *Registry) landFile(ro io.Reader) (string, error) {
	tmpFile, err := os.CreateTemp(r.landingDir, "oci-upload-")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file in the landing zone: %w", err)
	}

	if _, err := io.Copy(tmpFile, ro); err != nil {
		return "", fmt.Errorf("failed to copy reader to the landing zone: %w", err)
	}

	return tmpFile.Name(), nil
}

func (r *Registry) loadFile(ctx context.Context, fileLanded string, f *RepoFile) (*file.Store, ocispec.Descriptor, error) {
	fs, err := file.New(r.landingDir) // The OCI file store is not used for writing files.
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("failed to create local OCI store: %w", err)
	}

	fileDesc, err := fs.Add(ctx, fileLanded, detectFileMediaType(f), "")
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("failed to add file to local OCI store: %w", err)
	}
	if f.Digest != "" && string(fileDesc.Digest) != f.Digest {
		return nil, ocispec.Descriptor{}, fmt.Errorf("file digest mismatch: %q != %q", fileDesc.Digest, f.Digest)
	}
	fileDesc.Annotations[FileNameAnnotation] = f.Name
	fileDesc.Annotations[ocispec.AnnotationTitle] = f.Name // The 'Add' method by default sets the title to the full path.

	return fs, fileDesc, nil
}

func (r *Registry) newBackend(ctx context.Context, f *RepoFile) (destRepo, error) {
	repoRef := r.baseURL.Host + r.baseURL.Path + "/" + f.OwningRepo
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("failed to create remote OCI repo: %w", err)
	}

	c, ok := cred.FromContext(ctx)
	if ok && c.Basic != nil {
		repo.Client = &auth.Client{
			Client: retry.DefaultClient,
			Credential: auth.StaticCredential(r.baseURL.Host, auth.Credential{
				Username: c.Basic.User,
				Password: c.Basic.Password,
			}),
		}
	}

	return repo, nil
}

// upsertFileLayer updates the layers list with the provided file descriptor.
// If the file already exists in the layers list, it will be updated if the digest has changed.
// Returns true if the file was added or updated, and the updated layers list.
func upsertFileLayer(layers []ocispec.Descriptor, fileDesc ocispec.Descriptor) (bool, []ocispec.Descriptor) {
	existingFileIdx := -1
	for i, l := range layers {
		if l.Annotations != nil && l.Annotations[FileNameAnnotation] == fileDesc.Annotations[FileNameAnnotation] {
			existingFileIdx = i
			break
		}
	}
	if existingFileIdx != -1 {
		// Update the layer if the digest has changed.
		if layers[existingFileIdx].Digest != fileDesc.Digest {
			layers[existingFileIdx] = fileDesc
		} else {
			return false, layers
		}
	} else {
		// Add the layer if it doesn't exist.
		layers = append(layers, fileDesc)
	}
	return true, layers
}

func manifestLayers(ctx context.Context, repo oras.Target, manifestDesc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	var layers []ocispec.Descriptor
	if manifestDesc.Digest != "" {
		// Fetch the existing manifest
		manifestReader, err := repo.Fetch(ctx, manifestDesc)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch manifest: %w", err)
		}
		defer manifestReader.Close()

		manifestBytes, err := io.ReadAll(manifestReader)
		if err != nil {
			return nil, fmt.Errorf("failed to read manifest: %w", err)
		}

		var manifest ocispec.Manifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
		}
		layers = manifest.Layers
	}
	return layers, nil
}

func detectFileMediaType(f *RepoFile) string {
	if f.MediaType != "" {
		return f.MediaType
	}

	ext := filepath.Ext(f.Name)
	switch ext {
	case ".txt":
		return "text/plain"
	case ".html":
		return "text/html"
	case ".xml":
		return "application/xml"
	case ".json":
		return "application/json"
	case ".tar":
		return "application/x-tar"
	case ".gz", ".tgz":
		return "application/x-gzip"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}
