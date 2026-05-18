// Package indexcache is an OCI-backed pull-through cache for upstream
// index responses (PyPI simple index, npm packument, maven-metadata.xml).
//
// Each cache entry is one OCI manifest under a per-namespace repo:
//
//	<namespace>/_proxy_cache/index/<pkg>
//
// The manifest carries a single layer (the cached body) and two
// annotations: [FetchedAtAnnotation] (wall-clock fetch time) and
// [ContentTypeAnnotation] (the upstream Content-Type). The mutable
// [CacheTag] is overwritten on every refresh — intentional, this is a
// cache, not an immutable artifact. The previous blob is unreferenced
// when the tag moves and is reclaimed by the OCI backend's own GC story
// (GAR / ECR / zot); there is no in-package GC.
//
// The cache reaches ORAS directly instead of layering on
// [pkg/oci.Registry]. That registry enforces the immutable
// version-manifest model used for package storage, which is wrong for a
// mutable cache. Owning the on-OCI shape here keeps the cache loose
// without weakening package-storage invariants.
//
// TTL is not stored. [Cache.Get] returns the recorded [FetchedAtAnnotation]
// as time.Time; the caller (per-format integration) compares against
// its own freshness threshold and decides whether to call [Cache.Put]
// again with a fresh upstream response.
package indexcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/yolocs/ocifactory/pkg/auth/backend"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	// ArtifactType labels every cache manifest so operators
	// inspecting the OCI registry can distinguish cache manifests
	// from real package manifests at a glance.
	ArtifactType = "application/vnd.ocifactory.proxy.indexcache"

	// CacheTag is the mutable tag every cache manifest publishes
	// under. Overwriting on refresh is intentional.
	CacheTag = "current"

	// FetchedAtAnnotation records, on the cache manifest, the
	// wall-clock time the body was fetched from upstream. Encoded
	// with [time.RFC3339Nano] so sub-second resolution survives
	// the roundtrip — useful when operators dial freshness windows
	// down for index files that change minute-to-minute upstream.
	FetchedAtAnnotation = "proxy.cache.fetched-at"

	// ContentTypeAnnotation carries the upstream Content-Type so
	// [Cache.Get] returns it alongside the body. The annotation is
	// used instead of the layer descriptor's MediaType field
	// because real Content-Type values frequently carry parameters
	// (e.g. "application/json; charset=utf-8") that don't match
	// the OCI media-type regex.
	ContentTypeAnnotation = "proxy.cache.content-type"
)

// cacheRepoSegment is the path segment between the namespace name and
// the package name. The `ocifactory-` prefix matches the in-tree
// convention for registry-internal repos (compare
// [pkg/namespace.indexRepoSegment] = "ocifactory-namespaces" and
// [pkg/namespace.Registry] / "ocifactory-packages"), keeping cache
// repos visually distinct from real `<namespace>/packages/...`
// storage when an operator lists the OCI registry. The literal here
// is a single OCI distribution name component followed by
// "/index" — each segment satisfies the `[a-z0-9]+...` regex
// oras-go's `remote.NewRepository` validates.
const cacheRepoSegment = "ocifactory-proxy-cache/index"

// layerMediaType is the MediaType applied to the layer descriptor in
// the cache manifest. The original upstream Content-Type is kept in
// [ContentTypeAnnotation] because it may carry parameters; this field
// only has to be a syntactically-valid OCI media type.
const layerMediaType = "application/octet-stream"

// Cache is the OCI-backed pull-through index cache.
//
// One Cache value is constructed per ocifactory process and shared
// across every namespace and per-format proxy fetcher. Concurrent
// [Cache.Get] / [Cache.Put] calls are safe; the underlying ORAS
// target is constructed per call so there is no shared mutable
// state on the Cache itself.
type Cache struct {
	baseURL    *url.URL
	repoPrefix string
	authClient *auth.Client
	plainHTTP  bool

	// newTargetFunc opens an [oras.Target] for the given repo path
	// (already namespace- and prefix-qualified). Replaced in tests
	// with an in-memory target factory so unit tests don't need a
	// real registry.
	newTargetFunc func(ctx context.Context, repoPath string) (oras.Target, error)

	// now is the wall-clock the cache stamps onto
	// [FetchedAtAnnotation]. Tests override it for deterministic
	// roundtrip assertions; production uses [time.Now].
	now func() time.Time
}

// Option configures a [Cache] at construction time.
type Option func(*Cache) error

// WithBackendAuth wires a credential provider for the backend OCI
// registry. Default is [backend.Anonymous].
func WithBackendAuth(p backend.Provider) Option {
	return func(c *Cache) error {
		if p == nil {
			p = backend.Anonymous()
		}
		credFn := func(ctx context.Context, host string) (auth.Credential, error) {
			cred, err := p.Credential(ctx, host)
			if err != nil {
				return auth.EmptyCredential, err
			}
			return auth.Credential{
				Username:    cred.Username,
				Password:    cred.Password,
				AccessToken: cred.AccessToken,
			}, nil
		}
		c.authClient = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credFn,
		}
		return nil
	}
}

// WithRepoPrefix scopes cache repositories under the same
// single-segment prefix used by pkg/oci.Registry.
func WithRepoPrefix(prefix string) Option {
	return func(c *Cache) error {
		if err := oci.ValidateRepoPrefix(prefix); err != nil {
			return err
		}
		c.repoPrefix = prefix
		return nil
	}
}

// TargetFactory opens an [oras.Target] for the given repo path. The
// path is already namespace- and prefix-qualified by [Cache.repoPath].
// Implementations may construct a real remote target or an in-memory
// one — used by tests in other packages that need a working [Cache]
// without standing up a real OCI backend.
type TargetFactory func(ctx context.Context, repoPath string) (oras.Target, error)

// NewCacheWithTargets constructs a [Cache] that opens targets via fn
// instead of the package's default remote-target factory. This is the
// extension point tests in other packages use to wire an in-memory
// backend; production code calls [NewCache] and gets the remote
// factory automatically.
//
// The supplied factory must return a target safe for concurrent
// [Get] / [Put] calls; the package-supplied in-memory factory
// satisfies this. The clock used to stamp
// [FetchedAtAnnotation] defaults to [time.Now] in UTC and can be
// overridden via [WithClock] when callers need deterministic values.
func NewCacheWithTargets(fn TargetFactory, opts ...Option) (*Cache, error) {
	if fn == nil {
		return nil, errors.New("indexcache: target factory is required")
	}
	c := &Cache{
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	c.newTargetFunc = fn
	return c, nil
}

// WithClock overrides the wall clock the cache stamps onto
// [FetchedAtAnnotation]. Tests use it for deterministic timestamps;
// production passes nothing and the default ([time.Now] UTC)
// applies.
func WithClock(now func() time.Time) Option {
	return func(c *Cache) error {
		if now == nil {
			return errors.New("indexcache: clock function must not be nil")
		}
		c.now = now
		return nil
	}
}

// NewCache constructs a [Cache] that reads and writes against
// baseURL. baseURL is the OCI registry's HTTPS endpoint plus any
// shared path prefix (matching [pkg/oci.NewRegistry]); an
// `http://` scheme switches the underlying ORAS client to plain
// HTTP for local zot / dev workflows.
func NewCache(baseURL *url.URL, opts ...Option) (*Cache, error) {
	if baseURL == nil {
		return nil, errors.New("indexcache: baseURL is required")
	}
	c := &Cache{
		baseURL:   baseURL,
		plainHTTP: baseURL.Scheme == "http",
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if c.authClient == nil {
		// Default to anonymous so a Cache constructed without
		// WithBackendAuth can still target a public read-only
		// backend. Mirrors pkg/oci.NewRegistry's defaulting.
		if err := WithBackendAuth(backend.Anonymous())(c); err != nil {
			return nil, err
		}
	}
	c.newTargetFunc = c.newRemoteTarget
	return c, nil
}

// Get returns the cached body for (ns, pkg) along with its
// upstream Content-Type and the wall-clock time it was fetched.
// A cache miss returns (nil, "", zero-time, false, nil) — no
// error, and intentionally distinct from [proxy.ErrNotFound], which
// callers reserve for "upstream told us this package doesn't
// exist". Any other failure (manifest fetch, malformed annotation,
// dangling tag) is returned wrapped.
//
// pkg may contain '/' (npm scoped packages, Maven groupId paths);
// the caller is responsible for normalising the value (rejecting
// "..", absolute paths, etc.) so a maliciously-crafted pkg cannot
// traverse out of the namespace's cache prefix.
func (c *Cache) Get(ctx context.Context, ns, pkg string) ([]byte, string, time.Time, bool, error) {
	target, err := c.newTargetFunc(ctx, c.repoPath(ns, pkg))
	if err != nil {
		return nil, "", time.Time{}, false, fmt.Errorf("indexcache: open target: %w", err)
	}

	manifestDesc, err := target.Resolve(ctx, CacheTag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, "", time.Time{}, false, nil
		}
		return nil, "", time.Time{}, false, fmt.Errorf("indexcache: resolve %s: %w", CacheTag, err)
	}

	manifest, err := fetchManifest(ctx, target, manifestDesc)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}
	if manifest.ArtifactType != ArtifactType {
		return nil, "", time.Time{}, false, fmt.Errorf("indexcache: unexpected artifactType %q (want %q)", manifest.ArtifactType, ArtifactType)
	}
	if len(manifest.Layers) != 1 {
		return nil, "", time.Time{}, false, fmt.Errorf("indexcache: manifest has %d layers, want 1", len(manifest.Layers))
	}

	fetchedAtStr := manifest.Annotations[FetchedAtAnnotation]
	if fetchedAtStr == "" {
		return nil, "", time.Time{}, false, fmt.Errorf("indexcache: manifest missing %s annotation", FetchedAtAnnotation)
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, fetchedAtStr)
	if err != nil {
		return nil, "", time.Time{}, false, fmt.Errorf("indexcache: parse %s %q: %w", FetchedAtAnnotation, fetchedAtStr, err)
	}

	contentType := manifest.Annotations[ContentTypeAnnotation]

	body, err := fetchBlob(ctx, target, manifest.Layers[0])
	if err != nil {
		return nil, "", time.Time{}, false, err
	}

	return body, contentType, fetchedAt, true, nil
}

// Put writes body to the cache under (ns, pkg), overwriting any
// previous entry. contentType is the upstream response's
// Content-Type and is preserved verbatim for the next [Cache.Get].
//
// Concurrent Put calls for the same (ns, pkg) race on the
// underlying tag write; the last writer wins, which is the expected
// behaviour for a refresh-on-read cache.
func (c *Cache) Put(ctx context.Context, ns, pkg string, body []byte, contentType string) error {
	target, err := c.newTargetFunc(ctx, c.repoPath(ns, pkg))
	if err != nil {
		return fmt.Errorf("indexcache: open target: %w", err)
	}

	layerDesc := content.NewDescriptorFromBytes(layerMediaType, body)
	if err := pushIfMissing(ctx, target, layerDesc, body); err != nil {
		return fmt.Errorf("indexcache: push cache body: %w", err)
	}

	emptyDesc := ocispec.DescriptorEmptyJSON
	if err := pushIfMissing(ctx, target, emptyDesc, emptyDesc.Data); err != nil {
		return fmt.Errorf("indexcache: push config blob: %w", err)
	}

	annotations := map[string]string{
		FetchedAtAnnotation:   c.now().Format(time.RFC3339Nano),
		ContentTypeAnnotation: contentType,
	}
	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       emptyDesc,
		Layers:       []ocispec.Descriptor{layerDesc},
		Annotations:  annotations,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("indexcache: marshal manifest: %w", err)
	}
	manifestDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBytes)
	manifestDesc.ArtifactType = ArtifactType
	manifestDesc.Annotations = annotations

	if err := pushIfMissing(ctx, target, manifestDesc, manifestBytes); err != nil {
		return fmt.Errorf("indexcache: push manifest: %w", err)
	}
	if err := target.Tag(ctx, manifestDesc, CacheTag); err != nil {
		return fmt.Errorf("indexcache: tag %s: %w", CacheTag, err)
	}
	return nil
}

func (c *Cache) repoPath(ns, pkg string) string {
	return path.Join(c.repoPrefix, ns, cacheRepoSegment, pkg)
}

func (c *Cache) newRemoteTarget(_ context.Context, repoPath string) (oras.Target, error) {
	repoRef := c.baseURL.Host + "/" + path.Join(strings.Trim(c.baseURL.Path, "/"), repoPath)
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("create remote repository %q: %w", repoRef, err)
	}
	if c.plainHTTP {
		repo.PlainHTTP = true
	}
	repo.Client = c.authClient
	return repo, nil
}

func pushIfMissing(ctx context.Context, target oras.Target, desc ocispec.Descriptor, data []byte) error {
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

func fetchManifest(ctx context.Context, target oras.Target, desc ocispec.Descriptor) (*ocispec.Manifest, error) {
	rc, err := target.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("indexcache: fetch manifest: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("indexcache: read manifest body: %w", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("indexcache: decode manifest: %w", err)
	}
	return &m, nil
}

func fetchBlob(ctx context.Context, target oras.Target, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := target.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("indexcache: fetch blob: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("indexcache: read blob body: %w", err)
	}
	return body, nil
}
