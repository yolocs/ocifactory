package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/cred"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	DefaultArtifactType = "application/vnd.ocifactory.generic"
	FileNameAnnotation  = "ocifactory.file.title"

	// uploadMemThreshold is the largest upload body that AddFile will buffer
	// fully in memory. Anything larger spills to a single temp file before
	// being pushed to the backend. Sized to keep typical package metadata and
	// small wheels fully in memory while still allowing multi-tenant Cloud
	// Run instances comfortable headroom.
	uploadMemThreshold = 4 * 1024 * 1024
)

// ErrDigestMismatch is returned by AddFile when the caller-supplied
// RepoFile.Digest does not match the digest computed from the uploaded body.
// It is returned before any data is pushed to the backend.
var ErrDigestMismatch = errors.New("file digest mismatch")

type destRepo interface {
	oras.Target
	registry.TagLister
	content.Tagger
	content.Deleter
}

type Registry struct {
	baseURL      *url.URL
	artifactType string

	// uploadMemThreshold is the in-memory staging cap for AddFile bodies.
	// Initialised to the package default; overridable from tests.
	uploadMemThreshold int64

	// Used in unit test to stub with in memory backend.
	newBackendFunc func(ctx context.Context, f *RepoFile) (destRepo, error)
}

type RegistryOption func(*Registry) error

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
		baseURL:            baseURL,
		artifactType:       DefaultArtifactType,
		uploadMemThreshold: uploadMemThreshold,
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
//
// The body is streamed exactly once: bytes are simultaneously hashed and
// staged into either an in-memory buffer (for bodies up to uploadMemThreshold)
// or a single temp file (for larger bodies). The staged content is then
// pushed directly to the backend repository — no intermediate OCI file store
// or multi-hop disk copy.
//
// If RepoFile.Digest is set and disagrees with the digest computed from the
// body, AddFile returns ErrDigestMismatch before any data is sent to the
// backend.
//
// If the layer with the same digest already exists in the backend, the blob
// upload is skipped. If the file is unchanged in the manifest for
// RepoFile.OwningTag, the manifest is left untouched. Otherwise the manifest
// is repacked and re-tagged.
func (r *Registry) AddFile(ctx context.Context, f *RepoFile, ro io.Reader) (*FileDescriptor, error) {
	if strings.HasPrefix(f.OwningTag, "ref_") {
		return nil, fmt.Errorf("canonical tag cannot be prefixed with ref_; got %q", f.OwningTag)
	}

	staged, err := stageUploadWithThreshold(ro, r.uploadMemThreshold)
	if err != nil {
		return nil, err
	}
	defer staged.cleanup()

	if f.Digest != "" && string(staged.digest) != f.Digest {
		return nil, fmt.Errorf("%w: %q != %q", ErrDigestMismatch, staged.digest, f.Digest)
	}

	fileDesc := ocispec.Descriptor{
		MediaType: detectFileMediaType(f),
		Digest:    staged.digest,
		Size:      staged.size,
		Annotations: map[string]string{
			FileNameAnnotation:      f.Name,
			ocispec.AnnotationTitle: f.Name,
		},
	}

	backendRepo, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return nil, err
	}

	exists, err := backendRepo.Exists(ctx, fileDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to check if file blob exists: %w", err)
	}
	if !exists {
		blobReader, err := staged.reader()
		if err != nil {
			return nil, err
		}
		if err := backendRepo.Push(ctx, fileDesc, blobReader); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return nil, fmt.Errorf("failed to push file blob: %w", err)
		}
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

	packOpts := oras.PackManifestOptions{Layers: layers}
	newManifestDesc, err := oras.PackManifest(ctx, backendRepo, oras.PackManifestVersion1_1, r.artifactType, packOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to pack new manifest: %w", err)
	}
	if err := backendRepo.Tag(ctx, newManifestDesc, f.OwningTag); err != nil {
		return nil, fmt.Errorf("failed to tag new manifest: %w", err)
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

// stagedUpload is the result of staging an upload body for a single push.
// reader() returns a fresh io.Reader positioned at the start of the staged
// content; it may be called at most once. cleanup releases any temp file
// and is safe to call multiple times.
type stagedUpload struct {
	digest  digest.Digest
	size    int64
	reader  func() (io.Reader, error)
	cleanup func()
}

// stageUploadWithThreshold reads ro to completion, computing its sha256
// digest as it goes. Bodies up to memThreshold are buffered in memory;
// larger bodies spill to a single temp file. Either way, the returned
// stagedUpload exposes a rewindable reader and a cleanup callback for any
// spilled state. Production callers thread the per-Registry threshold so
// tests can dial it down to small sizes.
func stageUploadWithThreshold(ro io.Reader, memThreshold int64) (*stagedUpload, error) {
	digester := digest.SHA256.Digester()

	buf := &bytes.Buffer{}
	// Read at most memThreshold+1 bytes into memory so we can decide whether
	// to spill without losing the byte that pushed us over.
	headLimit := memThreshold + 1
	headTee := io.TeeReader(io.LimitReader(ro, headLimit), digester.Hash())
	headN, err := io.Copy(buf, headTee)
	if err != nil {
		return nil, fmt.Errorf("failed to stage upload: %w", err)
	}

	if headN <= memThreshold {
		// Whole body fit in memory.
		data := buf.Bytes()
		return &stagedUpload{
			digest:  digester.Digest(),
			size:    int64(len(data)),
			reader:  func() (io.Reader, error) { return bytes.NewReader(data), nil },
			cleanup: func() {},
		}, nil
	}

	// Spill: dump what we already buffered into a temp file, then drain the
	// rest of the body into the same file (and the digester) in one pass.
	tmp, err := os.CreateTemp("", "ocifactory-upload-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file for upload: %w", err)
	}
	cleanup := func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write head buffer to temp file: %w", err)
	}
	tailN, err := io.Copy(io.MultiWriter(tmp, digester.Hash()), ro)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to stage upload tail to temp file: %w", err)
	}

	return &stagedUpload{
		digest: digester.Digest(),
		size:   headN + tailN,
		reader: func() (io.Reader, error) {
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("failed to rewind staged upload: %w", err)
			}
			return tmp, nil
		},
		cleanup: cleanup,
	}, nil
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
