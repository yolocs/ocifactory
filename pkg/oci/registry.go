package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/auth/backend"
	"github.com/yolocs/ocifactory/pkg/metrics"
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

	// FileNameAnnotation is set on file manifests (and copied onto the file
	// blob's layer descriptor) so the referrers index surfaces the file
	// name without callers having to fetch each file manifest's body.
	FileNameAnnotation = "ocifactory.file.title"

	// FileDigestAnnotation carries the file blob's digest on the file
	// manifest. ListFiles reads it from the referrers index entry directly,
	// avoiding an extra fetch per file just to learn the blob digest.
	FileDigestAnnotation = "ocifactory.file.digest"

	// AliasTargetAnnotation is set on alias manifests to record the
	// canonical version they point at. The subject field carries the same
	// information as a digest; the annotation carries it as a human-readable
	// tag name so an operator inspecting the registry can read it directly.
	AliasTargetAnnotation = "ocifactory.alias.target"

	// Subtype suffixes appended to the configured base artifactType to
	// distinguish the three manifest kinds in the OCI 1.1 referrers layout.
	// Operators inspecting the registry see e.g.
	// "application/vnd.ocifactory.python.version" and can tell at a glance
	// which manifests are version anchors, file payloads, or aliases.
	versionArtifactSuffix = ".version"
	fileArtifactSuffix    = ".file"
	aliasArtifactSuffix   = ".alias"

	// uploadMemThreshold is the largest upload body that AddFile will buffer
	// fully in memory. Anything larger streams via chunked PATCH unless
	// streaming is disabled, in which case it spills to a single temp
	// file. Sized to keep typical package metadata and small wheels fully
	// in memory while still allowing multi-tenant Cloud Run instances
	// comfortable headroom.
	uploadMemThreshold = 4 * 1024 * 1024
)

// ErrDigestMismatch is returned by AddFile when the caller-supplied
// RepoFile.Digest does not match the digest computed from the uploaded body.
// It is returned before any data is pushed to the backend.
var ErrDigestMismatch = errors.New("file digest mismatch")

// ErrAliasCollision is returned by AddFile or AppendRefs when a write would
// clobber an existing tag whose manifest has an incompatible artifactType
// (e.g. AppendRefs trying to overwrite a canonical version, or AddFile
// trying to overwrite an alias). The HEAD-and-artifactType probe in the
// write path turns silent overwrite into a typed error.
var ErrAliasCollision = errors.New("tag collides with incompatible artifactType")

// ErrAlreadyExists is returned by AddFile when a file with the same
// (OwningRepo, OwningTag, Name) already exists and the registry was
// constructed without WithAllowOverwrite(true). Handlers translate it
// to 409 Conflict — re-uploading an existing file in an existing
// version is rejected by default to match PyPI's auditability story
// and keep an immutable view of every published version.
var ErrAlreadyExists = errors.New("file already exists in version")

// destRepo is the subset of oras-go's remote.Repository (and our in-memory
// fake) that pkg/oci needs. Pinned as an interface so tests can substitute
// memory-backed implementations.
type destRepo interface {
	oras.Target
	registry.TagLister
	content.Tagger
	content.Deleter
	content.PredecessorFinder

	// DeleteTag removes a single tag reference without otherwise
	// touching the manifest it points to. Used by the deterministic
	// file-tag cleanup paths so a deleted file manifest doesn't leave
	// behind a dangling _f_* tag entry that ListTags would have to
	// filter on every call. oras-go's public surface exposes manifest
	// deletion by digest only, so the destRepo abstraction grows this
	// one method and remoteRepo implements it via a direct HTTP
	// DELETE against /v2/<repo>/manifests/<tag>.
	DeleteTag(ctx context.Context, tag string) error
}

type Registry struct {
	baseURL *url.URL

	// artifactType is the operator-configured base type. We derive three
	// suffixed subtypes (versionArtifactType / fileArtifactType /
	// aliasArtifactType) so each manifest kind in the OCI 1.1 referrers
	// layout has a distinct, inspectable artifactType.
	artifactType        string
	versionArtifactType string
	fileArtifactType    string
	aliasArtifactType   string

	// uploadMemThreshold is the in-memory staging cap for AddFile bodies.
	// Initialised to the package default; overridable from tests.
	uploadMemThreshold int64

	// disableStreamingPush, when true, forces every AddFile call to use the
	// buffered + monolithic upload path. When false (the default), bodies
	// above uploadMemThreshold take the chunked PATCH streaming path, which
	// uses O(streamChunkSize) memory and zero disk regardless of body size.
	disableStreamingPush bool

	// disableBlobRedirect, when true, makes BlobRedirectURL return
	// ("", nil) without contacting the backend. Operators set this in
	// environments where exposing backend (CDN / object-store)
	// presigned URLs to clients is unacceptable — egress restrictions,
	// DLP, audit requirements. Handlers fall through to the
	// stream-through ReadFile path automatically.
	disableBlobRedirect bool

	// allowOverwrite, when true, lets AddFile re-upload a file whose
	// (OwningRepo, OwningTag, Name) already exists; the old file
	// manifest is unlinked from the version's referrer set after the
	// new one is pushed so readers always see exactly one match per
	// filename. The default (false) returns ErrAlreadyExists instead,
	// which handlers translate to 409 Conflict — safe-by-default for
	// auditability and immutable-version semantics. Operators flip
	// this on via --allow-overwrite when they need lax behaviour
	// (e.g. snapshot workflows that re-publish under the same tag).
	allowOverwrite bool

	// rec collects per-backend-call observations. Defaults to
	// metrics.NoOp() so existing tests and library callers that don't
	// opt into instrumentation see no behavioural change. Wire a real
	// recorder via WithMetrics.
	rec metrics.Recorder

	// backendProvider supplies credentials for every call to the
	// backend OCI registry. nil means "anonymous" (the empty
	// Credential for every host) — fine for public read-only
	// backends, fails closed against any private backend.
	backendProvider backend.Provider

	// authClient is the single authenticated HTTP client every
	// backend call shares. Constructed once in NewRegistry around
	// backendProvider (or anonymous when nil). The wrapped
	// auth.Cache amortises bearer-token fetches across every PATCH
	// chunk and every manifest write, so a 200 MB upload pays for
	// one token round-trip rather than ~50.
	authClient *auth.Client

	// probeClient is the auth.Client BlobRedirectURL uses to inspect
	// the backend's /v2/<repo>/blobs/<digest> response. It shares the
	// same auth.Cache and credential function as authClient so token
	// fetches are amortised across both paths, but its inner
	// http.Client refuses to follow redirects — we want to read the
	// 3xx Location directly rather than transparently fetch the
	// presigned URL.
	probeClient *auth.Client

	// Used in unit tests to stub with in-memory backend.
	newBackendFunc func(ctx context.Context, f *RepoFile) (destRepo, error)

	// Used in unit tests to stub the streaming-push side independently of
	// the in-memory backend.
	newStreamPusherFunc func(ctx context.Context, f *RepoFile) (streamingPusher, error)
}

type RegistryOption func(*Registry) error

func WithArtifactType(artifactType string) RegistryOption {
	return func(r *Registry) error {
		r.artifactType = artifactType
		return nil
	}
}

// WithMetrics wires a metrics.Recorder into the registry so every OCI
// backend call (push_blob, push_manifest, fetch_*, list_tags, …) and
// every streaming-push session is observed. Defaults to metrics.NoOp()
// when not set, so tests and library callers that don't opt into
// instrumentation keep their existing behaviour.
func WithMetrics(rec metrics.Recorder) RegistryOption {
	return func(r *Registry) error {
		if rec == nil {
			rec = metrics.NoOp()
		}
		r.rec = rec
		return nil
	}
}

// WithBackendAuth sets the credential provider the Registry presents
// to the backend OCI registry on every call. Construction wraps the
// provider in an auth.Client with a shared auth.Cache so bearer-token
// fetches are amortised across every PATCH chunk and every manifest
// write.
//
// Default when no option is supplied: backend.Anonymous() — the empty
// Credential for every host. Fine for public read-only backends (a
// public zot, docker-style anonymous mirrors); fails closed against
// any private backend, which is the safe shape for a library default.
//
// The interface is intentionally ours (backend.Provider), not oras-go's
// auth.CredentialFunc, so out-of-tree credential providers depend only
// on ocifactory's typed interface and the upstream type doesn't show up
// in our public API.
func WithBackendAuth(p backend.Provider) RegistryOption {
	return func(r *Registry) error {
		r.backendProvider = p
		return nil
	}
}

// WithStreamingPushDisabled forces AddFile to use the buffered + monolithic
// upload path for every body, no matter how large — large bodies will spill
// to a temp file before being pushed instead of streaming via chunked PATCH.
// The escape hatch exists for registries with broken or absent chunked-PATCH
// support; the distribution-spec requires chunked PATCH from v1.1.1 onward
// but occasional self-hosted registries are still in the wild that don't
// implement it correctly.
func WithStreamingPushDisabled(disabled bool) RegistryOption {
	return func(r *Registry) error {
		r.disableStreamingPush = disabled
		return nil
	}
}

// WithAllowOverwrite controls re-upload semantics. When true, AddFile
// permits replacing a file whose (OwningRepo, OwningTag, Name) already
// exists in the version: the new file manifest is pushed and the old
// one is unlinked from the version's referrer set so subsequent reads
// see only the new content. When false (the default), AddFile returns
// ErrAlreadyExists on a re-upload attempt; handlers translate that to
// 409 Conflict.
//
// The safe-by-default shape mirrors PyPI's behaviour and keeps every
// published version auditably immutable. Operators that intentionally
// re-publish under the same tag (Maven snapshots, fix-the-CI-job
// workflows, ephemeral staging) flip this on via --allow-overwrite.
func WithAllowOverwrite(allow bool) RegistryOption {
	return func(r *Registry) error {
		r.allowOverwrite = allow
		return nil
	}
}

// WithBlobRedirectDisabled, when true, makes BlobRedirectURL return
// ("", nil) without contacting the backend. Set this in environments
// where exposing the backend's presigned object-store URLs to clients
// is unacceptable (egress restrictions, DLP, audit requirements);
// handlers fall through to the stream-through ReadFile path
// automatically. The default (false) lets BlobRedirectURL probe the
// backend and short-circuit blob downloads to the backend's CDN.
func WithBlobRedirectDisabled(disabled bool) RegistryOption {
	return func(r *Registry) error {
		r.disableBlobRedirect = disabled
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

	// Size is the body length in bytes when the caller knows it ahead of
	// time — typically forwarded from an HTTP Content-Length. It lets
	// AddFile skip the peek-and-decide buffer entirely and dispatch
	// straight to the buffered or streaming path, which keeps a 200 MB
	// jar from pinning even one chunk of memory before the first byte
	// hits the network. Any value <= 0 (matching http.Request.ContentLength's
	// "unknown" sentinel of -1) means unknown and AddFile falls back to
	// peeking up to uploadMemThreshold+1 bytes.
	Size int64
}

// FileDescriptor identifies a file within the OCI-backed registry. Manifest
// is the descriptor of the per-file OCI manifest that owns the file blob;
// File is the descriptor of the blob layer itself. Callers typically only
// inspect File.Digest / File.Size (e.g. for HTTP Content-Length / checksum
// headers).
type FileDescriptor struct {
	Manifest ocispec.Descriptor
	File     ocispec.Descriptor
}

func NewRegistry(baseURL *url.URL, opt ...RegistryOption) (*Registry, error) {
	r := &Registry{
		baseURL:            baseURL,
		artifactType:       DefaultArtifactType,
		uploadMemThreshold: uploadMemThreshold,
		rec:                metrics.NoOp(),
	}
	r.newBackendFunc = r.newBackend
	r.newStreamPusherFunc = r.newStreamPusher

	for _, o := range opt {
		if err := o(r); err != nil {
			return nil, err
		}
	}

	r.versionArtifactType = r.artifactType + versionArtifactSuffix
	r.fileArtifactType = r.artifactType + fileArtifactSuffix
	r.aliasArtifactType = r.artifactType + aliasArtifactSuffix

	provider := r.backendProvider
	if provider == nil {
		provider = backend.Anonymous()
	}
	credFn := func(ctx context.Context, host string) (auth.Credential, error) {
		c, err := provider.Credential(ctx, host)
		if err != nil {
			return auth.EmptyCredential, err
		}
		return auth.Credential{
			Username:    c.Username,
			Password:    c.Password,
			AccessToken: c.AccessToken,
		}, nil
	}
	cache := auth.NewCache()
	r.authClient = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      cache,
		Credential: credFn,
	}

	// probeClient shares the credential function and token cache with
	// authClient — keeping bearer-token fetches amortised — but its
	// inner http.Client refuses to follow redirects so the redirect
	// probe sees the raw 3xx Location.
	probeHTTP := retry.NewClient()
	probeHTTP.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	r.probeClient = &auth.Client{
		Client:     probeHTTP,
		Cache:      cache,
		Credential: credFn,
	}

	return r, nil
}

// AddFile adds a file to the registry under the OwningRepo / OwningTag pair
// in RepoFile. The implementation uses the OCI 1.1 referrers layout:
//
//  1. Push (or skip, if already present) the file blob.
//  2. Idempotently ensure a "version" manifest exists tagged with
//     RepoFile.OwningTag. The version manifest carries no layers — it is
//     a constant-size anchor that file manifests reference via their
//     subject field.
//  3. Pack and push a "file" manifest whose single layer is the blob and
//     whose subject is the version manifest. The file manifest is not
//     tagged; it is addressed via the referrers API.
//
// This eliminates the read-modify-write cycle the legacy aggregated-manifest
// flow performed on every file, and lets concurrent uploads to the same
// version proceed independently — there is no shared mutable state between
// them.
//
// AddFile picks one of two upload paths per call:
//
//   - **Buffered + monolithic.** For bodies that fit within
//     uploadMemThreshold, or always when streaming is disabled (the
//     WithStreamingPushDisabled option / --disable-streaming-push flag),
//     the body is staged in memory (or, on the disabled path, into a
//     single temp file) and pushed in a single POST/PUT round-trip via
//     oras-go.
//   - **Streaming chunked PATCH.** For bodies that exceed
//     uploadMemThreshold and when streaming is enabled, the body is
//     streamed straight to the backend using POST/PATCH/PUT chunked
//     uploads, with the SHA-256 digest computed on-the-fly. Memory is
//     O(chunk size); disk is zero, regardless of body length.
//
// Path selection short-circuits whenever the caller knows the body size
// up front (RepoFile.Size > 0): no peek-and-decide buffer is allocated,
// and dispatch falls straight onto the threshold comparison. When the
// size is unknown (Size <= 0, e.g. an HTTP request with chunked
// Transfer-Encoding), AddFile peeks up to uploadMemThreshold+1 bytes to
// decide.
//
// Behaviour shared between paths:
//
//   - If RepoFile.Digest is set and disagrees with the digest computed
//     from the body, AddFile returns ErrDigestMismatch. On the buffered
//     path this happens before any byte is sent. On the streaming path
//     the bytes have already been transmitted, but the upload session is
//     deleted so the blob never lands in the registry.
//   - If a layer with the same digest already exists in the backend, the
//     blob upload is skipped (buffered path) or the registry deduplicates
//     on commit (streaming path).
//   - The file manifest digest is deterministic for a given (blob,
//     filename, version) tuple, so re-adding identical content does not
//     re-push the manifest either — the Exists check inside
//     packAndPushManifest short-circuits.
//   - If RepoFile.OwningTag currently resolves to an alias manifest
//     (artifactType ≠ versionArtifactType), AddFile returns
//     ErrAliasCollision rather than silently overwriting the alias.
//   - If a file with the same (OwningRepo, OwningTag, Name) already
//     exists and the registry was constructed without
//     WithAllowOverwrite(true), AddFile returns ErrAlreadyExists
//     before the body is read so a rejected re-upload doesn't pay for
//     a wasted upload. With WithAllowOverwrite(true), the previous
//     file manifest is unlinked from the version's referrer set after
//     the new one is pushed; readers only ever see one match per
//     filename, even across overwrites.
func (r *Registry) AddFile(ctx context.Context, f *RepoFile, ro io.Reader) (*FileDescriptor, error) {
	if f.OwningTag == "" {
		return nil, fmt.Errorf("OwningTag must be set")
	}

	// Existence probe runs before uploadBlob so a rejected re-upload
	// doesn't read a byte of the request body. The probe also returns
	// the pre-existing file manifest descriptor when overwrite is
	// allowed, so we can unlink it after the new manifest lands
	// without a second referrer scan.
	oldFileManifest, err := r.probeExistingFile(ctx, f)
	if err != nil {
		return nil, err
	}

	blobDesc, backend, err := r.uploadBlob(ctx, f, ro)
	if err != nil {
		return nil, err
	}

	versionDesc, err := r.ensureVersionManifest(ctx, backend, f.OwningTag)
	if err != nil {
		return nil, err
	}

	fileManifestDesc, err := r.pushFileManifest(ctx, backend, blobDesc, f.OwningTag, f.Name, versionDesc)
	if err != nil {
		return nil, err
	}

	// Best-effort cleanup of the previous file manifest when this is
	// an overwrite. Skipped when the new manifest is byte-identical
	// (same digest) to the old — re-uploading identical content is
	// idempotent and there's nothing to unlink. A delete failure is
	// not fatal: the orphan remains discoverable via the version's
	// referrers list until backend GC reaps it, but the live referrer
	// scan in ReadFile may then return either manifest. That
	// uncertainty is exactly what overwrite-mode opts into.
	//
	// The deterministic file tag was already retagged onto the new
	// manifest by pushFileManifest (Tag is an overwrite, not a
	// create), so there's no separate tag to untag here — the old
	// manifest is reachable only via Referrers until the Delete below
	// detaches it.
	if oldFileManifest != nil && oldFileManifest.Digest != fileManifestDesc.Digest {
		_ = backend.Delete(ctx, *oldFileManifest)
	}

	return &FileDescriptor{Manifest: fileManifestDesc, File: blobDesc}, nil
}

// probeExistingFile resolves the version manifest (if any) and scans
// its file referrers for one whose FileNameAnnotation matches f.Name.
// Behaviour:
//
//   - No version tag yet, or no matching file referrer → returns
//     (nil, nil); caller proceeds with the normal write path.
//   - Match found and overwrite is disabled → returns
//     (nil, ErrAlreadyExists).
//   - Match found and overwrite is enabled → returns the existing
//     descriptor so AddFile can unlink it after the new manifest is
//     pushed.
//
// The tag-points-at-an-alias case is left to ensureVersionManifest to
// translate into ErrAliasCollision: this probe only cares about file
// referrers under a canonical version manifest.
func (r *Registry) probeExistingFile(ctx context.Context, f *RepoFile) (*ocispec.Descriptor, error) {
	backend, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return nil, err
	}

	versionDesc, err := backend.Resolve(ctx, f.OwningTag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to resolve version tag %q: %w", f.OwningTag, err)
	}

	aType, err := manifestArtifactType(ctx, backend, versionDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect tag %q: %w", f.OwningTag, err)
	}
	if aType != r.versionArtifactType {
		// Alias or foreign manifest — ensureVersionManifest will
		// surface ErrAliasCollision when the write reaches it.
		return nil, nil
	}

	refs, err := registry.Referrers(ctx, backend, versionDesc, r.fileArtifactType)
	if err != nil {
		return nil, fmt.Errorf("failed to list file referrers for %q: %w", f.OwningTag, err)
	}
	for i := range refs {
		if refs[i].Annotations[FileNameAnnotation] != f.Name {
			continue
		}
		if !r.allowOverwrite {
			return nil, fmt.Errorf("%w: %s/%s/%s", ErrAlreadyExists, f.OwningRepo, f.OwningTag, f.Name)
		}
		return &refs[i], nil
	}
	return nil, nil
}

// uploadBlob picks between the buffered and streaming push paths and
// returns the resulting blob descriptor along with the backend handle the
// caller should reuse for subsequent manifest writes.
func (r *Registry) uploadBlob(ctx context.Context, f *RepoFile, ro io.Reader) (ocispec.Descriptor, destRepo, error) {
	if r.disableStreamingPush {
		return r.uploadBlobBuffered(ctx, f, ro)
	}

	// Fast path: when the caller knows the body length up front (typically
	// from an HTTP Content-Length), dispatch directly without peeking. The
	// peek itself only costs O(threshold) memory but it pre-buffers up to
	// uploadMemThreshold bytes for a body we already know is going to take
	// the streaming path — a meaningful waste at high concurrency.
	if f.Size > 0 {
		if f.Size > r.uploadMemThreshold {
			return r.uploadBlobStreaming(ctx, f, ro)
		}
		return r.uploadBlobBuffered(ctx, f, ro)
	}

	// Unknown size: peek up to memThreshold+1 bytes so we can decide
	// which path to take without losing the byte that pushed us over the
	// threshold. If the body fully fits in the head buffer the monolithic
	// path takes over and the buffer is replayed; otherwise the streaming
	// path consumes the head and the rest of `ro` in order.
	head, full, err := bufferUploadHead(ro, r.uploadMemThreshold)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	if full {
		return r.uploadBlobBuffered(ctx, f, bytes.NewReader(head))
	}
	return r.uploadBlobStreaming(ctx, f, io.MultiReader(bytes.NewReader(head), ro))
}

func (r *Registry) uploadBlobBuffered(ctx context.Context, f *RepoFile, ro io.Reader) (ocispec.Descriptor, destRepo, error) {
	staged, err := stageUploadWithThreshold(ro, r.uploadMemThreshold)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	defer staged.cleanup()

	if f.Digest != "" {
		expected, err := digest.Parse(f.Digest)
		if err != nil {
			return ocispec.Descriptor{}, nil, fmt.Errorf("invalid expected digest %q: %w", f.Digest, err)
		}
		if expected != staged.digest {
			return ocispec.Descriptor{}, nil, fmt.Errorf("%w: %q != %q", ErrDigestMismatch, staged.digest, expected)
		}
	}

	blobDesc := newBlobDescriptor(f, staged.digest, staged.size)

	backend, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	exists, err := backend.Exists(ctx, blobDesc)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("failed to check if file blob exists: %w", err)
	}
	if !exists {
		blobReader, err := staged.reader()
		if err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		if err := backend.Push(ctx, blobDesc, blobReader); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return ocispec.Descriptor{}, nil, fmt.Errorf("failed to push file blob: %w", err)
		}
	}

	return blobDesc, backend, nil
}

func (r *Registry) uploadBlobStreaming(ctx context.Context, f *RepoFile, body io.Reader) (ocispec.Descriptor, destRepo, error) {
	pusher, err := r.newStreamPusherFunc(ctx, f)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("failed to construct streaming pusher: %w", err)
	}

	mediaType := detectFileMediaType(f)
	pushedDesc, err := pusher.Push(ctx, mediaType, f.Digest, body)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("failed to stream file blob: %w", err)
	}

	blobDesc := newBlobDescriptor(f, pushedDesc.Digest, pushedDesc.Size)

	backend, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	return blobDesc, backend, nil
}

// newBlobDescriptor builds the layer descriptor we store in the file
// manifest. Annotations carry both the spec-defined image-title and the
// ocifactory-private FileNameAnnotation; downstream callers that already
// look up by FileNameAnnotation continue to work unchanged.
func newBlobDescriptor(f *RepoFile, dgst digest.Digest, size int64) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: detectFileMediaType(f),
		Digest:    dgst,
		Size:      size,
		Annotations: map[string]string{
			FileNameAnnotation:      f.Name,
			ocispec.AnnotationTitle: f.Name,
		},
	}
}

// ensureVersionManifest resolves owningTag to a version manifest, creating
// one if it doesn't yet exist. If the tag already resolves to a manifest
// with an incompatible artifactType (typically an alias), it returns
// ErrAliasCollision. Idempotent: concurrent callers all converge on the
// same content-addressed manifest digest.
func (r *Registry) ensureVersionManifest(ctx context.Context, backend destRepo, owningTag string) (ocispec.Descriptor, error) {
	if existing, err := backend.Resolve(ctx, owningTag); err == nil {
		aType, err := manifestArtifactType(ctx, backend, existing)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("failed to inspect tag %q: %w", owningTag, err)
		}
		switch aType {
		case r.versionArtifactType:
			return existing, nil
		case r.aliasArtifactType:
			return ocispec.Descriptor{}, fmt.Errorf("%w: tag %q is an alias", ErrAliasCollision, owningTag)
		default:
			return ocispec.Descriptor{}, fmt.Errorf("%w: tag %q has artifactType %q (expected %q)", ErrAliasCollision, owningTag, aType, r.versionArtifactType)
		}
	} else if !errors.Is(err, errdef.ErrNotFound) {
		return ocispec.Descriptor{}, fmt.Errorf("failed to resolve version tag %q: %w", owningTag, err)
	}

	desc, err := packAndPushManifest(ctx, backend, r.versionArtifactType, nil, nil, nil)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to push version manifest for %q: %w", owningTag, err)
	}
	if err := backend.Tag(ctx, desc, owningTag); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to tag version manifest %q: %w", owningTag, err)
	}
	return desc, nil
}

// pushFileManifest packs a file manifest with subject = versionDesc, single
// layer = blobDesc, and the file's name + blob digest mirrored into manifest
// annotations so the referrers index reflects them without an extra fetch.
//
// After the manifest is pushed, the deterministic file tag derived from
// (owningTag, fileName) is set on it so ReadFile and BlobRedirectURL can
// resolve the file manifest in one round-trip instead of paying for the
// version-manifest → Referrers → file-manifest descriptor walk on every
// download. Tag is an overwrite (PUT /v2/<repo>/manifests/<tag>), so an
// AddFile that replaces an existing file simply moves the tag onto the
// new manifest atomically.
func (r *Registry) pushFileManifest(ctx context.Context, backend destRepo, blobDesc ocispec.Descriptor, owningTag, fileName string, versionDesc ocispec.Descriptor) (ocispec.Descriptor, error) {
	annotations := map[string]string{
		FileNameAnnotation:      fileName,
		ocispec.AnnotationTitle: fileName,
		FileDigestAnnotation:    blobDesc.Digest.String(),
	}
	desc, err := packAndPushManifest(ctx, backend, r.fileArtifactType, []ocispec.Descriptor{blobDesc}, &versionDesc, annotations)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to push file manifest for %q: %w", fileName, err)
	}
	if err := backend.Tag(ctx, desc, fileTagFor(owningTag, fileName)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to tag file manifest for %q: %w", fileName, err)
	}
	return desc, nil
}

// ReadFile reads a file by either OwningTag or RefTag.
//
// With OwningTag, the file manifest is resolved directly via the
// deterministic _f_<sha256(OwningTag\0Name)> tag that AddFile attaches.
// That's two backend round-trips on the hot path (Resolve + Fetch
// manifest) before the blob fetch, instead of the four serial RTTs the
// old Resolve-version → Referrers → fetchBlobDescriptor → Fetch path
// paid. At a 30ms backend RTT that's ~60ms saved per file, which
// compounds into seconds across a cold `mvn dependency:resolve` or
// `npm install`.
//
// With RefTag, the alias manifest is resolved first, the canonical
// version tag is read out of its AliasTargetAnnotation, and the
// deterministic file tag is computed against the canonical name. The
// alias path keeps the extra round-trip to fetch the alias manifest
// body but still skips the Referrers walk.
func (r *Registry) ReadFile(ctx context.Context, f *RepoFile) (*FileDescriptor, io.ReadCloser, error) {
	if f.OwningTag == "" && f.RefTag == "" {
		return nil, nil, fmt.Errorf("either OwningTag or RefTag must be set")
	}

	backend, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return nil, nil, err
	}

	canonicalTag, err := r.resolveCanonicalTag(ctx, backend, f)
	if err != nil {
		return nil, nil, err
	}

	fileManifestDesc, err := backend.Resolve(ctx, fileTagFor(canonicalTag, f.Name))
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil, fmt.Errorf("file %q not found in version %q: %w", f.Name, canonicalTag, errdef.ErrNotFound)
		}
		return nil, nil, fmt.Errorf("failed to resolve file manifest tag: %w", err)
	}
	blobDesc, err := fetchBlobDescriptor(ctx, backend, fileManifestDesc)
	if err != nil {
		return nil, nil, err
	}
	if f.Digest != "" && string(blobDesc.Digest) != f.Digest {
		return nil, nil, fmt.Errorf("file digest mismatch: %q != %q", blobDesc.Digest, f.Digest)
	}
	rc, err := backend.Fetch(ctx, blobDesc)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch file blob: %w", err)
	}
	return &FileDescriptor{Manifest: fileManifestDesc, File: blobDesc}, rc, nil
}

// resolveCanonicalTag turns a RepoFile that identifies a version either
// directly (OwningTag) or via an alias (RefTag) into the canonical
// version tag string. The string is what fileTagFor hashes; we don't
// need the version manifest descriptor on the per-file read path because
// the deterministic file tag short-circuits the Referrers walk.
//
// The alias path still pays one Resolve + one Fetch (to inspect the
// alias manifest's AliasTargetAnnotation), but skips the Referrers list
// that used to walk every file under the canonical version.
func (r *Registry) resolveCanonicalTag(ctx context.Context, backend destRepo, f *RepoFile) (string, error) {
	if f.OwningTag != "" {
		return f.OwningTag, nil
	}

	aliasDesc, err := backend.Resolve(ctx, f.RefTag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve alias tag %q: %w", f.RefTag, err)
	}
	var aliasManifest ocispec.Manifest
	if err := fetchManifestJSON(ctx, backend, aliasDesc, &aliasManifest); err != nil {
		return "", fmt.Errorf("failed to fetch alias manifest %q: %w", f.RefTag, err)
	}
	if aliasManifest.ArtifactType != r.aliasArtifactType {
		return "", fmt.Errorf("%w: tag %q has artifactType %q (expected %q)", ErrAliasCollision, f.RefTag, aliasManifest.ArtifactType, r.aliasArtifactType)
	}
	canonical := aliasManifest.Annotations[AliasTargetAnnotation]
	if canonical == "" {
		return "", fmt.Errorf("alias manifest %q is missing %s annotation", f.RefTag, AliasTargetAnnotation)
	}
	return canonical, nil
}

// ListTags lists the tags for a repository. Canonical version tags and
// alias tags are returned in the same flat list; deterministic file
// tags (the _f_<sha256> entries AddFile attaches to each file manifest
// so ReadFile can resolve a file in one round-trip) are filtered out
// so callers see the same "versions + aliases" view they did before
// the deterministic-tag fast path landed.
//
// 404 NAME_UNKNOWN responses (zot returns this for repositories that
// have never been pushed to) are normalized to errdef.ErrNotFound so
// callers can rely on a single sentinel check regardless of whether
// they're talking to a fake or a real backend.
func (r *Registry) ListTags(ctx context.Context, repo string) ([]string, error) {
	backend, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return nil, err
	}
	tags, err := registry.Tags(ctx, backend)
	if err != nil {
		return nil, normalizeNotFound(err)
	}
	out := tags[:0]
	for _, t := range tags {
		if isFileTag(t) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// ListFiles enumerates all files across all canonical versions in repo.
// Aliases are skipped by checking each tag's manifest artifactType so the
// same file isn't reported twice. The blob digest comes from the
// FileDigestAnnotation we mirror on the file manifest, avoiding a fetch
// per file.
func (r *Registry) ListFiles(ctx context.Context, repo string) ([]*RepoFile, error) {
	backend, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return nil, err
	}

	tags, err := registry.Tags(ctx, backend)
	if err != nil {
		return nil, fmt.Errorf("failed to list tags: %w", err)
	}

	var files []*RepoFile
	for _, tag := range tags {
		// Deterministic file tags (_f_<sha256>) point at file manifests
		// directly; iterating over them would re-enter the file
		// manifest scan and double-count every file. Skip them up
		// front rather than relying on the artifactType filter below.
		if isFileTag(tag) {
			continue
		}
		versionDesc, err := backend.Resolve(ctx, tag)
		if err != nil {
			if errors.Is(err, errdef.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("failed to resolve tag %q: %w", tag, err)
		}
		aType, err := manifestArtifactType(ctx, backend, versionDesc)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect tag %q: %w", tag, err)
		}
		if aType != r.versionArtifactType {
			// Alias or unrelated manifest — skip.
			continue
		}
		refs, err := registry.Referrers(ctx, backend, versionDesc, r.fileArtifactType)
		if err != nil {
			return nil, fmt.Errorf("failed to list referrers for %q: %w", tag, err)
		}
		for _, ref := range refs {
			name := ref.Annotations[FileNameAnnotation]
			if name == "" {
				continue
			}
			files = append(files, &RepoFile{
				Name:       name,
				OwningRepo: repo,
				OwningTag:  tag,
				Digest:     ref.Annotations[FileDigestAnnotation],
			})
		}
	}

	return files, nil
}

// AppendRefs creates one alias manifest per ref name, each with subject =
// the canonical version manifest and tagged with the ref name. Concurrent
// AppendRefs calls for the same ref are resolved by the registry's tag
// namespace (last writer wins on the Tag write); orphaned previous alias
// manifests are best-effort cleaned up.
//
// If a ref name already exists as a non-alias tag (e.g. there's a
// canonical version literally named "latest"), AppendRefs returns
// ErrAliasCollision rather than overwriting it.
func (r *Registry) AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error {
	backend, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return err
	}

	versionDesc, err := backend.Resolve(ctx, canonicalTag)
	if err != nil {
		return fmt.Errorf("failed to resolve canonical tag %q: %w", canonicalTag, err)
	}
	// Defence in depth: refuse to alias something that isn't a version
	// manifest. Stops a stray AppendRefs("packageRoot", "anAlias") from
	// pointing aliases at random manifests we didn't author.
	if aType, err := manifestArtifactType(ctx, backend, versionDesc); err != nil {
		return fmt.Errorf("failed to inspect canonical tag %q: %w", canonicalTag, err)
	} else if aType != r.versionArtifactType {
		return fmt.Errorf("%w: canonical tag %q has artifactType %q (expected %q)", ErrAliasCollision, canonicalTag, aType, r.versionArtifactType)
	}

	for _, ref := range refs {
		var oldAlias ocispec.Descriptor
		hasOld := false
		if existing, err := backend.Resolve(ctx, ref); err == nil {
			aType, err := manifestArtifactType(ctx, backend, existing)
			if err != nil {
				return fmt.Errorf("failed to inspect tag %q: %w", ref, err)
			}
			if aType != r.aliasArtifactType {
				return fmt.Errorf("%w: tag %q has artifactType %q (expected alias)", ErrAliasCollision, ref, aType)
			}
			oldAlias = existing
			hasOld = true
		} else if !errors.Is(err, errdef.ErrNotFound) {
			return fmt.Errorf("failed to resolve tag %q: %w", ref, err)
		}

		annotations := map[string]string{
			AliasTargetAnnotation: canonicalTag,
		}
		aliasDesc, err := packAndPushManifest(ctx, backend, r.aliasArtifactType, nil, &versionDesc, annotations)
		if err != nil {
			return fmt.Errorf("failed to push alias manifest %q: %w", ref, err)
		}
		if err := backend.Tag(ctx, aliasDesc, ref); err != nil {
			return fmt.Errorf("failed to tag alias %q: %w", ref, err)
		}
		// Best-effort cleanup of the old alias manifest if it had a
		// different digest. Failure here is non-fatal — the orphan stays
		// discoverable via the version's referrers list until backend GC
		// reaps it, but the live tag points at the new alias regardless.
		if hasOld && oldAlias.Digest != aliasDesc.Digest {
			_ = backend.Delete(ctx, oldAlias)
		}
	}

	return nil
}

// DeleteTagFiles deletes every file under a canonical version tag, then
// the version manifest itself. Alias manifests pointing at the version
// are left alone — callers that want them gone should AppendRefs to a new
// target first or call DeleteRepoFiles.
//
// If tag resolves to a non-version manifest, only that manifest is
// deleted (treated as deleting the alias). The caller can chain a second
// call to drop the underlying version.
func (r *Registry) DeleteTagFiles(ctx context.Context, repo string, tag string) error {
	backend, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return err
	}
	return r.deleteTagFiles(ctx, backend, tag)
}

func (r *Registry) deleteTagFiles(ctx context.Context, backend destRepo, tag string) error {
	desc, err := backend.Resolve(ctx, tag)
	if err != nil {
		return fmt.Errorf("failed to resolve manifest for tag %q: %w", tag, err)
	}

	aType, err := manifestArtifactType(ctx, backend, desc)
	if err != nil {
		return fmt.Errorf("failed to inspect tag %q: %w", tag, err)
	}

	if aType == r.versionArtifactType {
		// Best-effort: enumerate all referrers (file manifests + any alias
		// manifests still pointing here) and delete them before the
		// version manifest itself, so the on-backend graph is left clean.
		refs, err := registry.Referrers(ctx, backend, desc, "")
		if err != nil {
			return fmt.Errorf("failed to list referrers for tag %q: %w", tag, err)
		}
		for _, ref := range refs {
			// Untag the deterministic file tag first so an in-flight
			// reader that already resolved the tag is the only race
			// window — without this, ListTags would keep returning
			// _f_* entries that point at a soon-deleted digest. Only
			// file manifests carry one; alias manifests don't, and
			// the call is a no-op against a missing tag.
			if name := ref.Annotations[FileNameAnnotation]; name != "" {
				if err := backend.DeleteTag(ctx, fileTagFor(tag, name)); err != nil && !errors.Is(err, errdef.ErrNotFound) {
					return fmt.Errorf("failed to untag file %q: %w", name, err)
				}
			}
			if err := backend.Delete(ctx, ref); err != nil && !errors.Is(err, errdef.ErrNotFound) {
				return fmt.Errorf("failed to delete referrer %s: %w", ref.Digest, err)
			}
		}
	}

	if err := backend.Delete(ctx, desc); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("failed to delete manifest for tag %q: %w", tag, err)
	}
	return nil
}

// DeleteRepoFiles deletes every canonical version (and its files) in a
// repository. Aliases are dropped as a side effect of DeleteTagFiles
// reaping referrers.
func (r *Registry) DeleteRepoFiles(ctx context.Context, repo string) error {
	backend, err := r.newBackendFunc(ctx, &RepoFile{OwningRepo: repo})
	if err != nil {
		return err
	}

	tags, err := registry.Tags(ctx, backend)
	if err != nil {
		return fmt.Errorf("failed to list tags: %w", err)
	}

	for _, tag := range tags {
		// Resolve and skip anything that isn't a version manifest — alias
		// manifests are already deleted as referrers when we delete their
		// version (above), so iterating over them again would either
		// double-delete (harmless) or, if some are still live because
		// their target version is gone, blow up the loop ordering. Just
		// scope deletion to versions and let backend GC tidy the rest.
		desc, err := backend.Resolve(ctx, tag)
		if err != nil {
			if errors.Is(err, errdef.ErrNotFound) {
				continue
			}
			return fmt.Errorf("failed to resolve tag %q: %w", tag, err)
		}
		aType, err := manifestArtifactType(ctx, backend, desc)
		if err != nil {
			return fmt.Errorf("failed to inspect tag %q: %w", tag, err)
		}
		if aType != r.versionArtifactType {
			continue
		}
		if err := r.deleteTagFiles(ctx, backend, tag); err != nil {
			return err
		}
	}

	return nil
}

// packAndPushManifest builds a deterministic OCI image manifest, checks
// existence in the backend, and pushes it only when missing. Determinism
// matters: identical (artifactType, layers, subject, annotations) inputs
// must produce identical manifest digests so re-runs of AddFile /
// AppendRefs short-circuit at the Exists check rather than re-pushing
// the same bytes.
//
// We avoid oras.PackManifest on this path because it auto-injects the
// org.opencontainers.image.created annotation with a fresh timestamp,
// which would defeat content-addressed dedup. Instead we marshal the
// manifest ourselves and let json.Marshal's stable map-key ordering
// produce reproducible bytes.
func packAndPushManifest(
	ctx context.Context,
	target destRepo,
	artifactType string,
	layers []ocispec.Descriptor,
	subject *ocispec.Descriptor,
	annotations map[string]string,
) (ocispec.Descriptor, error) {
	emptyDesc := ocispec.DescriptorEmptyJSON

	if err := pushIfMissing(ctx, target, emptyDesc, emptyDesc.Data); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to push empty config blob: %w", err)
	}

	if len(layers) == 0 {
		// image-spec v1.1 requires at least one layer; reuse the empty
		// blob as a constant-size sentinel for version and alias
		// manifests that semantically have none.
		layers = []ocispec.Descriptor{emptyDesc}
	}

	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       emptyDesc,
		Layers:       layers,
		Subject:      subject,
		Annotations:  annotations,
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to marshal manifest: %w", err)
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, body)
	desc.ArtifactType = artifactType
	desc.Annotations = annotations

	if err := pushIfMissing(ctx, target, desc, body); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to push manifest: %w", err)
	}
	return desc, nil
}

// pushIfMissing pushes data only if the backend doesn't already have the
// described content. Wraps the Exists/Push pair so the manifest dedup path
// reads cleanly. ErrAlreadyExists from a concurrent push is swallowed —
// we only care that the content is there at the end.
func pushIfMissing(ctx context.Context, target destRepo, desc ocispec.Descriptor, data []byte) error {
	exists, err := target.Exists(ctx, desc)
	if err != nil {
		return fmt.Errorf("exists check: %w", err)
	}
	if exists {
		return nil
	}
	if err := target.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}

// manifestArtifactType fetches a manifest's body and returns its
// artifactType field. Used by ensureVersionManifest, AppendRefs, and
// ListFiles to discriminate version manifests from alias manifests
// without relying on tag-name conventions. Exists at the cost of one
// extra GET per discriminated tag — acceptable because no current code
// path discriminates inside a tight loop on a hot read path.
func manifestArtifactType(ctx context.Context, backend destRepo, desc ocispec.Descriptor) (string, error) {
	if desc.ArtifactType != "" {
		return desc.ArtifactType, nil
	}
	var m ocispec.Manifest
	if err := fetchManifestJSON(ctx, backend, desc, &m); err != nil {
		return "", err
	}
	return m.ArtifactType, nil
}

// fetchManifestJSON fetches and decodes a manifest body. The descriptor
// must point at a manifest, not a generic blob — there is no media-type
// guard here because every caller already knows what kind of manifest it
// is asking for.
func fetchManifestJSON(ctx context.Context, backend destRepo, desc ocispec.Descriptor, out *ocispec.Manifest) error {
	rc, err := backend.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed to fetch manifest %s: %w", desc.Digest, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", desc.Digest, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("failed to parse manifest %s: %w", desc.Digest, err)
	}
	return nil
}

// fetchBlobDescriptor returns the single layer descriptor of a file
// manifest. ReadFile uses it to translate "I want file X under version
// Y" into "fetch this blob digest".
func fetchBlobDescriptor(ctx context.Context, backend destRepo, fileManifestDesc ocispec.Descriptor) (ocispec.Descriptor, error) {
	var m ocispec.Manifest
	if err := fetchManifestJSON(ctx, backend, fileManifestDesc, &m); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to fetch file manifest: %w", err)
	}
	if len(m.Layers) != 1 {
		return ocispec.Descriptor{}, fmt.Errorf("file manifest %s has %d layers, expected 1", fileManifestDesc.Digest, len(m.Layers))
	}
	return m.Layers[0], nil
}

// bufferUploadHead reads up to threshold+1 bytes from r into memory. It
// returns (head, full, nil) where full is true iff the body ended at or
// before threshold — in which case head holds the entire body and r is
// drained. When full is false, head holds threshold+1 bytes and the caller
// must read more bytes from r to get the rest of the body.
//
// Memory: bytes.Buffer grows on demand, so small bodies don't pin a full
// threshold-sized slice; large bodies cap out at threshold+1.
func bufferUploadHead(r io.Reader, threshold int64) ([]byte, bool, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, threshold+1))
	if err != nil {
		return nil, false, fmt.Errorf("failed to buffer upload head: %w", err)
	}
	return buf.Bytes(), n <= threshold, nil
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
	// Unlink-while-open (Linux idiom): the directory entry disappears
	// immediately while the data stays accessible via tmp's fd until
	// Close. Survives panic and SIGKILL — a Cloud Run instance that
	// crashes mid-upload doesn't leak a /tmp inode. Best-effort: if the
	// platform refuses early unlink, the cleanup func still runs.
	_ = os.Remove(tmp.Name())
	cleanup := func() {
		tmp.Close()
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
	// remote.Repository defaults to HTTPS; honour an http:// baseURL the
	// way the streaming pusher does so a local zot / dev registry round
	// trips end-to-end.
	if r.baseURL.Scheme == "http" {
		repo.PlainHTTP = true
	}
	repo.Client = r.authClient

	return newInstrumentedRepo(remoteRepo{Repository: repo}, r.rec), nil
}

// remoteRepo adapts oras-go's *remote.Repository to our destRepo
// interface. The wrapper exists so we can attach DeleteTag — oras-go's
// public surface only exposes manifest deletion by digest, but the OCI
// distribution spec allows DELETE /v2/<repo>/manifests/<tag> to remove
// the tag reference (leaving the underlying manifest alone if other
// tags or referrers still pin it).
type remoteRepo struct {
	*remote.Repository
}

// DeleteTag issues a direct HTTP DELETE against
// /v2/<repo>/manifests/<tag>. Used by the deterministic file-tag
// cleanup paths so removed file manifests don't leave dangling _f_*
// tags behind. A 404 from the backend is translated to
// errdef.ErrNotFound so callers can use errors.Is to swallow
// already-gone tags.
func (r remoteRepo) DeleteTag(ctx context.Context, tag string) error {
	ref := r.Repository.Reference
	ref.Reference = tag
	scheme := "https"
	if r.Repository.PlainHTTP {
		scheme = "http"
	}
	deleteURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, ref.Host(), ref.Repository, tag)

	delCtx := auth.AppendRepositoryScope(ctx, ref, auth.ActionDelete)
	req, err := http.NewRequestWithContext(delCtx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("build delete-tag request: %w", err)
	}
	resp, err := r.Repository.Client.Do(req)
	if err != nil {
		return fmt.Errorf("delete tag %q: %w", tag, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return errdef.ErrNotFound
	default:
		return fmt.Errorf("delete tag %q: backend returned %s", tag, resp.Status)
	}
}

// newStreamPusher constructs a streamPusher that talks directly to the
// configured OCI registry, reusing the same authenticated http.Client setup
// as newBackend. The returned pusher is good for a single AddFile call.
func (r *Registry) newStreamPusher(ctx context.Context, f *RepoFile) (streamingPusher, error) {
	repoRef := r.baseURL.Host + r.baseURL.Path + "/" + f.OwningRepo
	ref, err := registry.ParseReference(repoRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse repository reference %q: %w", repoRef, err)
	}
	if err := ref.ValidateRepository(); err != nil {
		return nil, fmt.Errorf("invalid repository name %q: %w", ref.Repository, err)
	}

	plainHTTP := r.baseURL.Scheme == "http"
	return newInstrumentedStreamPusher(newStreamPusher(r.authClient, ref, plainHTTP), r.rec), nil
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
