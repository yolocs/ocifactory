package namespace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"oras.land/oras-go/v2/errdef"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	// DefaultPackageIndexSuffix names the per-namespace OCI repo whose
	// tags enumerate every owning-repo the wrapper has ever recorded a
	// write for. The repo lives at "<namespace>/_packages" by default
	// — operators who already park a real artifact under that name can
	// relocate it via [WithPackageIndexSuffix].
	DefaultPackageIndexSuffix = "_packages"

	// indexSentinelFile is the file name written under the package
	// index repo. The body is constant — the tag's existence is the
	// record — so a re-add of an existing tag is a backend no-op.
	packageIndexSentinelFile = "present"

	// packageIndexedReposCacheSize bounds the in-process set of
	// (namespace, owning-repo) pairs the wrapper has already recorded
	// in the index. The set is a hot-path optimisation: a hit skips
	// the redundant AddFile call entirely. Misses are safe and just
	// pay one idempotent backend write.
	packageIndexedReposCacheSize = 4096

	// tagSlashEscape is the OCI-tag-safe substitution for the '/'
	// character. OCI tag names allow [A-Za-z0-9_.-] only, so we encode
	// every '/' in an owning-repo name as "__" and decode on read.
	// Documented next to the constant so a future maintainer doesn't
	// reinvent the escape and break round-tripping.
	tagSlashEscape = "__"
)

var packageIndexSentinelBody = []byte("present\n")

// AuthzFactory builds an [auth.Authorizer] from a namespace [Policy].
// Operators wire in alternative implementations (OPA, Casbin, Cedar,
// ...) by passing one to [WithAuthzFactory]; the default is
// [NewPolicyAuthorizer].
type AuthzFactory func(Policy) (auth.Authorizer, error)

// RegistryBackend is the subset of *pkg/oci.Registry the wrapper
// needs. Pinned as an interface so tests can substitute the in-memory
// [oci.FakeRegistry]; production passes *oci.Registry directly.
type RegistryBackend interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
	ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error)
	AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error
	DeleteRepoFiles(ctx context.Context, repo string) error
}

// Registry is the data-plane wrapper that namespace-scopes every OCI
// operation. It:
//
//   - Prefixes every OwningRepo with the namespace name so writes from
//     namespace "alpha" land under "alpha/<owning-repo>" and cannot be
//     read from namespace "beta".
//   - Enforces the namespace's authz policy on every call. The
//     compiled authorizer is cached behind a TTL+LRU so the hot path
//     is one map lookup per request.
//   - Maintains a per-namespace package index at
//     "<namespace>/_packages" (one tag per owning-repo) so cascade
//     delete and namespace listing don't depend on the OCI _catalog
//     endpoint, which is optional and inconsistently implemented.
//
// Handlers consume *Registry instead of *oci.Registry so authz
// enforcement is a structural guarantee — a future format author
// cannot forget to call the authorizer.
type Registry struct {
	inner       RegistryBackend
	store       *Store
	cache       *policyCache
	indexed     *expirable.LRU[string, struct{}]
	indexSuffix string
	factory     AuthzFactory
	cacheTTL    time.Duration

	// indexLocks serialises index writes per (ns, owning-repo) pair so
	// a burst of concurrent first-writes pays for one OCI round-trip
	// instead of N. The locks are dropped after the write succeeds —
	// repeat writers short-circuit on the indexed LRU above.
	indexLocks sync.Map
}

// RegistryOption customises a [Registry].
type RegistryOption func(*Registry)

// WithPackageIndexSuffix overrides [DefaultPackageIndexSuffix].
// Operators set this when their backend already has a top-level
// "_packages" repo they want to keep — extremely unlikely, but cheap
// to make configurable.
func WithPackageIndexSuffix(s string) RegistryOption {
	return func(r *Registry) { r.indexSuffix = s }
}

// WithAuthzFactory overrides the authorizer constructor.
// Defaults to [NewPolicyAuthorizer].
func WithAuthzFactory(f AuthzFactory) RegistryOption {
	return func(r *Registry) { r.factory = f }
}

// WithPolicyCacheTTL overrides [DefaultPolicyCacheTTL]. A value of
// zero or negative disables caching — every request pays for a
// Store.Get + authorizer compile. Operators set ttl=0 in tests that
// want every Put to take effect immediately without sleeping.
func WithPolicyCacheTTL(ttl time.Duration) RegistryOption {
	return func(r *Registry) { r.cacheTTL = ttl }
}

// NewRegistry wraps inner in a namespace [Registry] backed by store
// for namespace metadata.
func NewRegistry(inner RegistryBackend, store *Store, opts ...RegistryOption) *Registry {
	r := &Registry{
		inner:       inner,
		store:       store,
		indexSuffix: DefaultPackageIndexSuffix,
		factory:     NewPolicyAuthorizer,
		cacheTTL:    DefaultPolicyCacheTTL,
	}
	for _, o := range opts {
		o(r)
	}
	r.cache = newPolicyCache(DefaultPolicyCacheSize, r.cacheTTL)
	// Indexed-repos cache exists purely to short-circuit duplicate
	// AddFile calls into the package index. Its lifetime is the
	// process — entries should not age out under normal operation
	// because the underlying OCI tag is also persistent. ttl=0 inside
	// expirable.NewLRU means "10-year TTL" (effectively no expiry),
	// which is what we want; bounded size is the only ceiling.
	r.indexed = expirable.NewLRU[string, struct{}](packageIndexedReposCacheSize, nil, 0)
	return r
}

// InvalidatePolicy drops the cached authorizer for name so the next
// request re-fetches the namespace spec. Admin-side handlers that
// mutate a namespace call this to make their write visible without
// waiting out the cache TTL. A name that wasn't cached is a no-op.
func (r *Registry) InvalidatePolicy(name string) { r.cache.invalidate(name) }

// authorize loads (cached or freshly compiled) the namespace's
// authorizer and runs op against the [auth.AuthContext] in ctx.
// Returns:
//
//   - nil on allow.
//   - an error wrapping [ErrNotFound] when the namespace is unknown
//     (and caches the negative result).
//   - an error wrapping [auth.ErrUnauthorized] on policy deny.
//   - any other error from the store / authorizer compile, unwrapped.
func (r *Registry) authorize(ctx context.Context, namespace string, op auth.Op) error {
	az, err := r.authorizerFor(ctx, namespace)
	if err != nil {
		return err
	}
	ac, _ := auth.FromContext(ctx)
	return az.Authorize(ctx, ac, op)
}

func (r *Registry) authorizerFor(ctx context.Context, namespace string) (auth.Authorizer, error) {
	if v, ok := r.cache.get(namespace); ok {
		if v.notFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, namespace)
		}
		return v.authorizer, nil
	}

	ns, err := r.store.Get(ctx, namespace)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			r.cache.put(namespace, cachedPolicy{notFound: true})
		}
		return nil, err
	}

	az, err := r.factory(ns.Spec.Policy)
	if err != nil {
		// Compile errors signal misconfiguration the operator must fix
		// (e.g. an admin Put that bypassed validation). Don't cache —
		// repeat requests should keep surfacing the error until the
		// spec is corrected.
		return nil, fmt.Errorf("compile authorizer for %q: %w", namespace, err)
	}
	r.cache.put(namespace, cachedPolicy{authorizer: az})
	return az, nil
}

// repoIn returns the namespace-prefixed OCI repo path for owningRepo.
// "" stays "" so callers passing an empty owning repo (e.g. a probe)
// still see a deterministic path under the namespace.
func (r *Registry) repoIn(namespace, owningRepo string) string {
	return path.Join(namespace, owningRepo)
}

func (r *Registry) packageIndexRepo(namespace string) string {
	return path.Join(namespace, r.indexSuffix)
}

// AddFile authorizes namespace for write, prefixes f.OwningRepo with
// the namespace, forwards to the inner registry, and (best-effort)
// records the owning-repo in the namespace's package index so
// [ListPackages] can enumerate it later.
func (r *Registry) AddFile(ctx context.Context, namespace string, f *oci.RepoFile, body io.Reader) (*oci.FileDescriptor, error) {
	if err := r.authorize(ctx, namespace, auth.OpWrite); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("RepoFile must not be nil")
	}
	owningRepo := f.OwningRepo
	scoped := *f
	scoped.OwningRepo = r.repoIn(namespace, owningRepo)
	desc, err := r.inner.AddFile(ctx, &scoped, body)
	if err != nil {
		return nil, err
	}
	r.recordPackage(ctx, namespace, owningRepo)
	return desc, nil
}

// ReadFile authorizes namespace for read and forwards to the inner
// registry with the owning repo prefixed.
func (r *Registry) ReadFile(ctx context.Context, namespace string, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	if err := r.authorize(ctx, namespace, auth.OpRead); err != nil {
		return nil, nil, err
	}
	if f == nil {
		return nil, nil, errors.New("RepoFile must not be nil")
	}
	scoped := *f
	scoped.OwningRepo = r.repoIn(namespace, f.OwningRepo)
	return r.inner.ReadFile(ctx, &scoped)
}

// BlobRedirectURL authorizes namespace for read and forwards to the
// inner registry with the owning repo prefixed. Returns ("", nil) when
// the backend serves blobs inline, mirroring [oci.Registry].
func (r *Registry) BlobRedirectURL(ctx context.Context, namespace string, f *oci.RepoFile) (string, error) {
	if err := r.authorize(ctx, namespace, auth.OpRead); err != nil {
		return "", err
	}
	if f == nil {
		return "", errors.New("RepoFile must not be nil")
	}
	scoped := *f
	scoped.OwningRepo = r.repoIn(namespace, f.OwningRepo)
	return r.inner.BlobRedirectURL(ctx, &scoped)
}

// ListTags authorizes namespace for read and returns the canonical
// tags for owningRepo within the namespace.
func (r *Registry) ListTags(ctx context.Context, namespace, owningRepo string) ([]string, error) {
	if err := r.authorize(ctx, namespace, auth.OpRead); err != nil {
		return nil, err
	}
	return r.inner.ListTags(ctx, r.repoIn(namespace, owningRepo))
}

// ListFiles authorizes namespace for read and returns every file
// across every canonical version of owningRepo within the namespace.
// The OwningRepo on each returned [oci.RepoFile] is rewritten back to
// the namespace-relative form so callers don't see the namespace
// prefix leak through.
func (r *Registry) ListFiles(ctx context.Context, namespace, owningRepo string) ([]*oci.RepoFile, error) {
	if err := r.authorize(ctx, namespace, auth.OpRead); err != nil {
		return nil, err
	}
	files, err := r.inner.ListFiles(ctx, r.repoIn(namespace, owningRepo))
	if err != nil {
		return nil, err
	}
	for _, fl := range files {
		fl.OwningRepo = stripPrefix(fl.OwningRepo, namespace)
	}
	return files, nil
}

// ListPackages authorizes namespace for read and returns every
// owning-repo the wrapper has recorded a write for in the namespace's
// package index. Tags are decoded back to their original "/"-form.
//
// An absent index repo is reported as an empty list — a namespace
// that has never been written to legitimately has no packages.
func (r *Registry) ListPackages(ctx context.Context, namespace string) ([]string, error) {
	if err := r.authorize(ctx, namespace, auth.OpRead); err != nil {
		return nil, err
	}
	tags, err := r.inner.ListTags(ctx, r.packageIndexRepo(namespace))
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list package index for %q: %w", namespace, err)
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, decodeTag(t))
	}
	return out, nil
}

// AppendRefs authorizes namespace for write and forwards to the inner
// registry with owningRepo prefixed.
func (r *Registry) AppendRefs(ctx context.Context, namespace, owningRepo, canonicalTag string, refs ...string) error {
	if err := r.authorize(ctx, namespace, auth.OpWrite); err != nil {
		return err
	}
	return r.inner.AppendRefs(ctx, r.repoIn(namespace, owningRepo), canonicalTag, refs...)
}

// DeleteRepoFiles authorizes namespace for write and forwards to the
// inner registry with owningRepo prefixed. The package index entry
// for owningRepo is best-effort cleared after the inner delete
// succeeds; failure is logged at WARN and does not fail the call.
func (r *Registry) DeleteRepoFiles(ctx context.Context, namespace, owningRepo string) error {
	if err := r.authorize(ctx, namespace, auth.OpWrite); err != nil {
		return err
	}
	if err := r.inner.DeleteRepoFiles(ctx, r.repoIn(namespace, owningRepo)); err != nil {
		return err
	}
	r.indexed.Remove(indexedKey(namespace, owningRepo))
	return nil
}

// recordPackage best-effort marks owningRepo as present in the
// namespace's package index. Index updates run after the inner write
// already succeeded so the visible state never includes "indexed but
// no data". A failed update is logged at WARN — the index is a hint
// for cascade delete and listing, not a transactional ledger; a stale
// miss is rectified by the next write or a manual reindex.
func (r *Registry) recordPackage(ctx context.Context, namespace, owningRepo string) {
	if owningRepo == "" {
		return
	}
	key := indexedKey(namespace, owningRepo)
	if _, ok := r.indexed.Get(key); ok {
		return
	}

	// Per-key lock so concurrent first-writers pay for one backend
	// AddFile, not N. Stored value is unused; we only need the
	// uniqueness of *Mutex per key. LoadOrStore guarantees at most one
	// goroutine creates a fresh mutex; the rest contend on it.
	muAny, _ := r.indexLocks.LoadOrStore(key, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	defer r.indexLocks.Delete(key)

	// Re-check after acquiring the lock: an earlier holder may have
	// just populated the indexed-set, in which case our backend write
	// is wasted work the LRU would otherwise catch on the next pass.
	if _, ok := r.indexed.Get(key); ok {
		return
	}

	rf := &oci.RepoFile{
		OwningRepo: r.packageIndexRepo(namespace),
		OwningTag:  encodeTag(owningRepo),
		Name:       packageIndexSentinelFile,
		MediaType:  "text/plain",
		Size:       int64(len(packageIndexSentinelBody)),
	}
	if _, err := r.inner.AddFile(ctx, rf, bytes.NewReader(packageIndexSentinelBody)); err != nil && !errors.Is(err, oci.ErrAlreadyExists) {
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index update failed",
			"namespace", namespace,
			"owning_repo", owningRepo,
			"error", err,
		)
		return
	}
	r.indexed.Add(key, struct{}{})
}

// stripPrefix removes a leading "<namespace>/" from owningRepo. Used
// by [ListFiles] so callers don't see the namespace prefix leak
// through in the returned [oci.RepoFile.OwningRepo].
func stripPrefix(owningRepo, namespace string) string {
	prefix := namespace + "/"
	if strings.HasPrefix(owningRepo, prefix) {
		return owningRepo[len(prefix):]
	}
	return owningRepo
}

// encodeTag escapes '/' characters in an owning-repo name so it can
// serve as an OCI tag name (which allows [A-Za-z0-9_.-] only). See
// [tagSlashEscape] for the exact substitution.
func encodeTag(owningRepo string) string {
	return strings.ReplaceAll(owningRepo, "/", tagSlashEscape)
}

// decodeTag reverses [encodeTag]. Owning-repo names produced by
// callers must not themselves contain the escape sequence; this is a
// constraint we accept rather than escape recursively because it
// would push the encoding into territory that operators inspecting
// the OCI registry can no longer eyeball.
func decodeTag(tag string) string {
	return strings.ReplaceAll(tag, tagSlashEscape, "/")
}

func indexedKey(namespace, owningRepo string) string {
	return namespace + "\x00" + owningRepo
}
