package artifact

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
	"golang.org/x/sync/singleflight"
	"oras.land/oras-go/v2/errdef"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/logging"
	nsmeta "github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	// defaultPackageIndexSuffix names the per-namespace OCI repo whose
	// tags enumerate every owning-repo the wrapper has ever recorded a
	// write for. The repo lives at "<namespace>/ocifactory-packages"
	// by default — operators who already park a real artifact under
	// that name can relocate it via [WithPackageIndexSuffix]. The
	// literal must satisfy the OCI distribution name regex (path
	// segments start with [a-z0-9]); a leading "_" is rejected
	// client-side by oras-go.
	defaultPackageIndexSuffix = "ocifactory-packages"

	// packageIndexSentinelFile is the file name written under the
	// package index repo. The body is constant — the tag's existence
	// is the record — so a re-add of an existing tag is a backend
	// no-op.
	packageIndexSentinelFile = "present"

	// packageIndexedReposCacheSize bounds the in-process set of
	// (namespace, owning-repo) pairs the wrapper has already recorded
	// in the index. The set is a hot-path optimisation: a hit skips
	// the redundant AddFile call entirely. Misses are safe and just
	// pay one idempotent backend write.
	packageIndexedReposCacheSize = 4096

	// defaultPolicyCacheSize bounds the number of distinct namespaces
	// the policy cache holds. Sized generously so even multi-tenant
	// deployments with hundreds of namespaces never evict in
	// practice; the cap exists to put a hard ceiling on memory rather
	// than as a tuning knob.
	defaultPolicyCacheSize = 1024
)

// DefaultPolicyCacheTTL is the default lifetime of a single entry in
// the data-plane authz policy cache. Picked short enough that an
// operator's `PUT namespace` lands in front of in-flight clients
// within a one-minute window even without explicit invalidation,
// long enough that a hot namespace pulling thousands of artifacts a
// minute pays for one Store.Get + Policy compile rather than one per
// request. Admin-side mutations (Store.Put / Store.Delete) skip the
// TTL via the mutation-hook invalidation path.
const DefaultPolicyCacheTTL = 60 * time.Second

var packageIndexSentinelBody = []byte("present\n")

// AuthzFactory builds an [auth.Authorizer] from a namespace [Policy].
// Operators wire in alternative implementations (OPA, Casbin, Cedar,
// ...) by passing one to [WithAuthzFactory]; the default is
// [namespace.NewPolicyAuthorizer].
type AuthzFactory func(nsmeta.Policy) (auth.Authorizer, error)

// Backend is the subset of *pkg/oci.Registry the wrapper
// needs. Pinned as an interface so tests can substitute the in-memory
// [oci.FakeRegistry]; production passes *oci.Registry directly.
//
// DeleteTagFiles is included even though no in-tree handler reaches
// for it today — keeping the interface a strict superset of the
// metadata-store [Backend] avoids two parallel interfaces and lets
// future yank semantics (single-version delete) plug in without
// reshaping the surface.
type Backend interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
	ResolveTag(ctx context.Context, repo string, tag string) (string, error)
	ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error)
	AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error
	DeleteRepoFiles(ctx context.Context, repo string) error
	DeleteTagFiles(ctx context.Context, repo string, tag string) error
}

// NamespaceStore is the control-plane namespace metadata surface the
// artifact data-plane needs for existence checks, policy compilation, and
// cache invalidation. The concrete OCI implementation is
// [namespace.Store], but artifact storage depends only on this interface.
type NamespaceStore interface {
	Get(ctx context.Context, name string) (*nsmeta.Namespace, error)
	SetMutationHook(func(name string))
}

// Store is the data-plane wrapper that holds the cross-namespace
// state — the authorizer cache, the package-index dedupe set, the
// pluggable authz factory — and hands out per-namespace
// [NamespaceView] views via [Store.View].
//
// Handlers don't use *Store directly; they hold a
// *NamespaceView obtained per request:
//
//	ns := mux.Vars(req)["namespace"]
//	view := r.View(ns)
//	desc, err := view.AddFile(ctx, f, body)
//
// *NamespaceView implements the existing [pkg/handler.Registry]
// interface, so handlers can swap a *pkg/oci.Registry for a
// *NamespaceView without changing any call site — namespace-aware
// authz and OwningRepo prefixing happen transparently.
type Store struct {
	inner       Backend
	namespaces  NamespaceStore
	cache       *policyCache
	indexed     *expirable.LRU[string, struct{}]
	indexSuffix string
	factory     AuthzFactory
	cacheTTL    time.Duration

	// flight collapses concurrent first-misses for the same namespace
	// so a cold-start burst pays one Store.Get + factory(...) rather
	// than N. Keyed by namespace name.
	flight singleflight.Group

	// indexLocks serialises index writes per (ns, owning-repo) pair so
	// a burst of concurrent first-writers pays for one OCI round-trip
	// instead of N. Entries are NOT deleted after use: the LRU
	// re-check inside the critical section is what carries
	// correctness, and deleting the mutex while another goroutine is
	// still holding a reference would leave two unrelated mutexes
	// claiming to guard the same key. The lock map is bounded by the
	// indexed-LRU size in steady state.
	indexLocks sync.Map
}

// StoreOption customises a [Store].
type StoreOption func(*Store)

// WithPackageIndexSuffix overrides the default
// ("ocifactory-packages"). Operators set this when their backend
// already has a per-namespace "ocifactory-packages" repo they want to
// keep — extremely unlikely, but cheap to make configurable.
func WithPackageIndexSuffix(s string) StoreOption {
	return func(r *Store) { r.indexSuffix = s }
}

// WithAuthzFactory overrides the authorizer constructor.
// Defaults to [namespace.NewPolicyAuthorizer].
func WithAuthzFactory(f AuthzFactory) StoreOption {
	return func(r *Store) { r.factory = f }
}

// WithPolicyCacheTTL overrides [DefaultPolicyCacheTTL]. A value of
// zero or negative disables caching — every request pays for a
// Store.Get + authorizer compile. Operators set ttl=0 in tests that
// want every Put to take effect immediately without sleeping.
func WithPolicyCacheTTL(ttl time.Duration) StoreOption {
	return func(r *Store) { r.cacheTTL = ttl }
}

// NewStore wraps inner in a namespace [Store] backed by store
// for namespace metadata. The wrapper installs a mutation hook on
// store so admin-side Put / Delete calls invalidate the cached
// authorizer — failing to use NewStore (e.g. constructing the
// fields by hand in a test) means admin mutations only take effect
// after the cache TTL expires.
func NewStore(inner Backend, namespaces NamespaceStore, opts ...StoreOption) *Store {
	r := &Store{
		inner:       inner,
		namespaces:  namespaces,
		indexSuffix: defaultPackageIndexSuffix,
		factory:     nsmeta.NewPolicyAuthorizer,
		cacheTTL:    DefaultPolicyCacheTTL,
	}
	for _, o := range opts {
		o(r)
	}
	r.cache = newPolicyCache(defaultPolicyCacheSize, r.cacheTTL)
	// Indexed-repos cache exists purely to short-circuit duplicate
	// AddFile calls into the package index. Its lifetime is the
	// process — entries should not age out under normal operation
	// because the underlying OCI tag is also persistent. ttl=0 inside
	// expirable.NewLRU means "10-year TTL" (effectively no expiry),
	// which is what we want; bounded size is the only ceiling.
	r.indexed = expirable.NewLRU[string, struct{}](packageIndexedReposCacheSize, nil, 0)
	namespaces.SetMutationHook(r.InvalidatePolicy)
	return r
}

// Namespace returns a handler-facing namespace after validating that the
// namespace metadata exists.
func (r *Store) Namespace(ctx context.Context, name string) (Namespace, error) {
	view := r.View(name)
	if _, err := view.Spec(ctx); err != nil {
		return nil, err
	}
	return artifactNamespace{view: view}, nil
}

// View returns a [NamespaceView] bound to namespace. The returned
// view is cheap to construct (a small struct, no I/O) so handlers
// should call it per request rather than caching it. The bound
// namespace is enforced on every method invocation — there is no
// way to issue a cross-namespace operation through a single
// NamespaceView.
func (r *Store) View(namespace string) *NamespaceView {
	return &NamespaceView{parent: r, namespace: namespace}
}

// InvalidatePolicy drops the cached authorizer for name so the next
// request re-fetches the namespace spec. The Store calls this
// automatically after a successful Put or Delete; admin-side code
// that bypasses the Store can call it directly. A name that wasn't
// cached is a no-op.
func (r *Store) InvalidatePolicy(name string) { r.cache.invalidate(name) }

// authorize loads (cached or freshly compiled) the namespace's
// authorizer and runs op against the [auth.AuthContext] in ctx.
// Returns:
//
//   - nil on allow.
//   - an error wrapping [namespace.ErrNotFound] when the namespace is unknown
//     (and caches the negative result).
//   - an error wrapping [auth.ErrUnauthorized] on policy deny or on
//     a missing AuthContext.
//   - any other error from the store / authorizer compile, unwrapped.
//
// The nil-AuthContext check lives here, not in the plugin
// authorizer, because the wrapper is the trust boundary: a
// third-party [AuthzFactory] that forgets to deny nil would
// otherwise turn into an auth bypass.
func (r *Store) authorize(ctx context.Context, namespace string, op auth.Op) error {
	az, err := r.authorizerFor(ctx, namespace)
	if err != nil {
		return err
	}
	ac, ok := auth.FromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("no authenticated subject for %s on namespace %q: %w", op, namespace, auth.ErrUnauthorized)
	}
	return az.Authorize(ctx, ac, op)
}

// specFor is the cache-aware single-namespace lookup that
// [Store.authorizerFor] and [NamespaceView.Spec] funnel through.
// On a hit it returns the cached entry without any I/O; on a miss it
// resolves through the shared singleflight so concurrent callers
// observe one [Store.Get] + authorizer compile.
//
// A namespace that doesn't exist returns an error wrapping
// [namespace.ErrNotFound] with notFound=true on the cached entry — callers that
// only need the spec must inspect the notFound flag rather than the
// returned error so they can produce the expected 404 mapping without
// re-running the load.
func (r *Store) specFor(ctx context.Context, namespace string) (cachedPolicy, error) {
	if v, ok := r.cache.get(namespace); ok {
		if v.notFound {
			return v, fmt.Errorf("%w: %s", nsmeta.ErrNotFound, namespace)
		}
		return v, nil
	}
	v, err, _ := r.flight.Do(namespace, func() (any, error) {
		if v, ok := r.cache.get(namespace); ok {
			return v, nil
		}
		ns, err := r.namespaces.Get(ctx, namespace)
		if err != nil {
			if errors.Is(err, nsmeta.ErrNotFound) {
				neg := cachedPolicy{notFound: true}
				r.cache.put(namespace, neg)
				return neg, err
			}
			return cachedPolicy{}, err
		}
		az, ferr := r.factory(ns.Spec.Policy)
		if ferr != nil {
			return cachedPolicy{}, fmt.Errorf("compile authorizer for %q: %w", namespace, ferr)
		}
		specCopy := ns.Spec
		entry := cachedPolicy{authorizer: az, spec: &specCopy}
		r.cache.put(namespace, entry)
		return entry, nil
	})
	if err != nil {
		return cachedPolicy{}, err
	}
	cp := v.(cachedPolicy)
	if cp.notFound {
		return cp, fmt.Errorf("%w: %s", nsmeta.ErrNotFound, namespace)
	}
	return cp, nil
}

func (r *Store) authorizerFor(ctx context.Context, namespace string) (auth.Authorizer, error) {
	cp, err := r.specFor(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return cp.authorizer, nil
}

// resolveRepo validates the (namespace, owningRepo) pair against the
// namespace-escape attack and returns the joined backend repo path.
// Any owningRepo that doesn't path.Clean to itself, contains a "..",
// or is absolute is rejected as malformed — without this check
// path.Join("alpha", "../beta/foo") collapses to "beta/foo" and
// silently lands traffic in another namespace.
func (r *Store) resolveRepo(namespace, owningRepo string) (string, error) {
	if owningRepo == "" {
		// An empty owning repo is a meaningful root for some
		// operations (e.g. an admin "list everything in this
		// namespace"); we accept it and resolve to just the
		// namespace.
		return namespace, nil
	}
	if path.IsAbs(owningRepo) {
		return "", fmt.Errorf("%w: owning repo %q is absolute", nsmeta.ErrInvalidOwningRepo, owningRepo)
	}
	cleaned := path.Clean(owningRepo)
	if cleaned != owningRepo {
		return "", fmt.Errorf("%w: owning repo %q is not in canonical form (clean: %q)", nsmeta.ErrInvalidOwningRepo, owningRepo, cleaned)
	}
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") || strings.HasSuffix(cleaned, "/..") {
		return "", fmt.Errorf("%w: owning repo %q escapes namespace", nsmeta.ErrInvalidOwningRepo, owningRepo)
	}
	// The first path segment of the namespace package index repo is
	// reserved: a write that landed there would mix user artifacts in
	// with the wrapper's own sentinel tags. Maven's regex-y URL routes
	// can otherwise reach it, so the check lives at the resolver — the
	// chokepoint every per-format handler funnels through.
	if cleaned == r.indexSuffix || strings.HasPrefix(cleaned, r.indexSuffix+"/") {
		return "", fmt.Errorf("%w: owning repo %q uses reserved prefix %q", nsmeta.ErrInvalidOwningRepo, owningRepo, r.indexSuffix)
	}
	return path.Join(namespace, cleaned), nil
}

func (r *Store) packageIndexRepo(namespace string) string {
	return path.Join(namespace, r.indexSuffix)
}

// NamespaceIndex is a tag-backed sibling repository inside one
// namespace. Tags are encoded keys; callers see only decoded keys.
type NamespaceIndex struct {
	view *NamespaceView
	name string
}

// NamespaceView is a per-namespace view of a [Store]. Every
// method authorizes the bound namespace, prefixes OwningRepo (or the
// raw repo string for ListTags/ListFiles/etc.) with it, and forwards
// to the underlying OCI backend.
//
// The struct is intentionally cheap to construct — it carries a
// pointer to the parent and a string. Handlers obtain one per
// request via [Store.View] and discard it when the request
// finishes.
//
// *NamespaceView implements the existing [pkg/handler.Registry]
// interface so handler code that previously held a *pkg/oci.Registry
// can swap to a *NamespaceView with no signature changes.
type NamespaceView struct {
	parent    *Store
	namespace string
}

// Namespace returns the bound namespace name.
func (s *NamespaceView) Namespace() string { return s.namespace }

// Index returns a handle to a named namespace-local sentinel index.
// The index name is a single repo segment such as "python-packages";
// the backing repo lives beside package repos at "<namespace>/<name>".
func (s *NamespaceView) Index(name string) *NamespaceIndex {
	return &NamespaceIndex{view: s, name: name}
}

// Authorize checks whether the request context's authenticated
// subject may perform op in the bound namespace. Format handlers use
// this for protocol paths that don't naturally map to a concrete OCI
// read/write operation before returning data.
func (s *NamespaceView) Authorize(ctx context.Context, op auth.Op) error {
	return s.parent.authorize(ctx, s.namespace, op)
}

// Spec returns the namespace's current [namespace.Spec], used by per-format
// handlers to dispatch on [Spec.Mode] / read [Spec.Proxy] before
// committing to a code path.
//
// The returned pointer references the cached entry; callers must not
// mutate the value. An unknown namespace returns an error wrapping
// [namespace.ErrNotFound] so [handler.WriteNamespaceError] maps it to 404.
//
// Spec does not authorize: the namespace's compiled authorizer still
// runs on downstream [AddFile] / [ReadFile] / [ListFiles] / etc., so
// the dispatch primitive does not become an auth bypass.
func (s *NamespaceView) Spec(ctx context.Context) (*nsmeta.Spec, error) {
	cp, err := s.parent.specFor(ctx, s.namespace)
	if err != nil {
		return nil, err
	}
	return cp.spec, nil
}

// AddFile authorizes the bound namespace for write, prefixes
// f.OwningRepo with the namespace, forwards to the inner registry,
// and (best-effort) records the owning-repo in the namespace's
// package index.
func (s *NamespaceView) AddFile(ctx context.Context, f *oci.RepoFile, body io.Reader) (*oci.FileDescriptor, error) {
	if f == nil {
		return nil, errors.New("RepoFile must not be nil")
	}
	if err := s.parent.authorize(ctx, s.namespace, auth.OpWrite); err != nil {
		return nil, err
	}
	backendFile, err := s.parent.backendFile(s.namespace, f)
	if err != nil {
		return nil, err
	}
	desc, err := s.parent.inner.AddFile(ctx, backendFile, body)
	if err != nil {
		return nil, err
	}
	s.parent.recordPackage(ctx, s.namespace, f.OwningRepo)
	return desc, nil
}

// AddCachedFile writes a proxy cache fill after authorizing the caller
// for read. It is intentionally narrower than AddFile: user-facing
// publish APIs must continue to call AddFile so namespace write policy
// gates real artifact writes, while pull-through proxy cache misses can
// populate OCI storage for readers without granting them publish rights.
func (s *NamespaceView) AddCachedFile(ctx context.Context, f *oci.RepoFile, body io.Reader) (*oci.FileDescriptor, error) {
	if f == nil {
		return nil, errors.New("RepoFile must not be nil")
	}
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return nil, err
	}
	backendFile, err := s.parent.backendFile(s.namespace, f)
	if err != nil {
		return nil, err
	}
	desc, err := s.parent.inner.AddFile(ctx, backendFile, body)
	if err != nil {
		return nil, err
	}
	s.parent.recordPackage(ctx, s.namespace, f.OwningRepo)
	return desc, nil
}

// ReadFile authorizes the bound namespace for read and forwards to
// the inner registry with OwningRepo prefixed.
func (s *NamespaceView) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	if f == nil {
		return nil, nil, errors.New("RepoFile must not be nil")
	}
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return nil, nil, err
	}
	backendFile, err := s.parent.backendFile(s.namespace, f)
	if err != nil {
		return nil, nil, err
	}
	return s.parent.inner.ReadFile(ctx, backendFile)
}

// BlobRedirectURL authorizes the bound namespace for read and
// forwards to the inner registry with OwningRepo prefixed. Returns
// ("", nil) when the backend serves blobs inline, mirroring
// [oci.Registry].
func (s *NamespaceView) BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error) {
	if f == nil {
		return "", errors.New("RepoFile must not be nil")
	}
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return "", err
	}
	backendFile, err := s.parent.backendFile(s.namespace, f)
	if err != nil {
		return "", err
	}
	return s.parent.inner.BlobRedirectURL(ctx, backendFile)
}

// ListTags authorizes the bound namespace for read and returns the
// canonical tags for repo within the namespace.
func (s *NamespaceView) ListTags(ctx context.Context, repo string) ([]string, error) {
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return nil, err
	}
	backendRepo, err := s.parent.resolveRepo(s.namespace, repo)
	if err != nil {
		return nil, err
	}
	return s.parent.inner.ListTags(ctx, backendRepo)
}

// ResolveTag authorizes the bound namespace for read and returns the
// canonical version tag identified by tag within repo.
func (s *NamespaceView) ResolveTag(ctx context.Context, repo, tag string) (string, error) {
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return "", err
	}
	backendRepo, err := s.parent.resolveRepo(s.namespace, repo)
	if err != nil {
		return "", err
	}
	return s.parent.inner.ResolveTag(ctx, backendRepo, tag)
}

// ListFiles authorizes the bound namespace for read and returns
// every file across every canonical version of repo within the
// namespace. The OwningRepo on each returned [oci.RepoFile] is
// rewritten back to the namespace-relative form so callers don't see
// the raw backend prefix leak through. Each returned *RepoFile is
// freshly allocated by the wrapper so a future inner-side cache of
// descriptors stays safe.
func (s *NamespaceView) ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error) {
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return nil, err
	}
	backendRepo, err := s.parent.resolveRepo(s.namespace, repo)
	if err != nil {
		return nil, err
	}
	files, err := s.parent.inner.ListFiles(ctx, backendRepo)
	if err != nil {
		return nil, err
	}
	out := make([]*oci.RepoFile, len(files))
	for i, src := range files {
		copyF := *src
		copyF.OwningRepo = stripPrefix(copyF.OwningRepo, s.namespace)
		out[i] = &copyF
	}
	return out, nil
}

// ListPackages returns every owning-repo the wrapper has recorded a
// write for in namespace's package index without applying data-plane
// authorization. INTENDED FOR CONTROL-PLANE USE ONLY: callers must
// already be on the admin trust boundary because this method can
// enumerate any namespace's package index. Tags are decoded back to
// their original "/"-form.
//
// An absent index repo is reported as an empty list — a namespace
// that has never been written to legitimately has no packages.
func (r *Store) ListPackages(ctx context.Context, namespace string) ([]string, error) {
	if err := nsmeta.ValidateName(namespace); err != nil {
		return nil, err
	}
	return r.listPackages(ctx, namespace)
}

// ListPackages authorizes the bound namespace for read and returns
// package-index entries for that namespace.
func (s *NamespaceView) ListPackages(ctx context.Context) ([]string, error) {
	if err := s.parent.authorize(ctx, s.namespace, auth.OpRead); err != nil {
		return nil, err
	}
	return s.parent.listPackages(ctx, s.namespace)
}

func (r *Store) listPackages(ctx context.Context, namespace string) ([]string, error) {
	return r.listIndex(ctx, namespace, r.indexSuffix)
}

// AppendRefs authorizes the bound namespace for write and forwards
// to the inner registry with repo prefixed.
func (s *NamespaceView) AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error {
	if err := s.parent.authorize(ctx, s.namespace, auth.OpWrite); err != nil {
		return err
	}
	if canonicalTag == "" {
		return errors.New("canonicalTag must not be empty")
	}
	backendRepo, err := s.parent.resolveRepo(s.namespace, repo)
	if err != nil {
		return err
	}
	return s.parent.inner.AppendRefs(ctx, backendRepo, canonicalTag, refs...)
}

// DeleteRepoFiles authorizes the bound namespace for write and
// forwards to the inner registry with repo prefixed. The package
// index entry for repo is cleared after the inner delete succeeds —
// both the in-process indexed marker AND the backend index tag — so
// [ListPackages] no longer reports the now-empty repo. Index cleanup
// failures are logged at WARN and do not fail the call.
func (s *NamespaceView) DeleteRepoFiles(ctx context.Context, repo string) error {
	if err := s.parent.authorize(ctx, s.namespace, auth.OpWrite); err != nil {
		return err
	}
	backendRepo, err := s.parent.resolveRepo(s.namespace, repo)
	if err != nil {
		return err
	}
	if err := s.parent.inner.DeleteRepoFiles(ctx, backendRepo); err != nil {
		return err
	}
	s.parent.indexed.Remove(indexedKey(s.namespace, repo))
	encoded, encErr := oci.EncodeTag(repo)
	if encErr != nil {
		// The repo passed authorize() and resolveRepo() so this
		// should never fail; treat as bug-not-vuln and log.
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index: encode failed on delete",
			"namespace", s.namespace, "owning_repo", repo, "error", encErr,
		)
		return nil
	}
	if err := s.parent.inner.DeleteTagFiles(ctx, s.parent.packageIndexRepo(s.namespace), encoded); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index sweep failed",
			"namespace", s.namespace, "owning_repo", repo, "error", err,
		)
	}
	return nil
}

// backendFile returns a copy of f with OwningRepo rewritten to the
// namespace-prefixed backend form. Returns [namespace.ErrInvalidOwningRepo]
// when the input would escape the namespace.
func (r *Store) backendFile(namespace string, f *oci.RepoFile) (*oci.RepoFile, error) {
	copyF := *f
	resolved, err := r.resolveRepo(namespace, f.OwningRepo)
	if err != nil {
		return nil, err
	}
	copyF.OwningRepo = resolved
	return &copyF, nil
}

// recordPackage best-effort marks owningRepo as present in the
// namespace's package index. Index updates run after the inner write
// already succeeded so the visible state never includes "indexed but
// no data". A failed update is logged at WARN — the index is a hint
// for cascade delete and listing, not a transactional ledger; a
// stale miss is rectified by the next write.
//
// Concurrency: a per-key sync.Mutex serialises concurrent first-
// writers. Mutex entries are NOT deleted from indexLocks after use:
// deleting while another goroutine still references the same
// *sync.Mutex would let an arriving third goroutine LoadOrStore a
// fresh mutex for the same key, "guarding" with two unrelated
// mutexes. The map is bounded by the indexed-LRU's eviction.
func (r *Store) recordPackage(ctx context.Context, namespace, owningRepo string) {
	if owningRepo == "" {
		return
	}
	key := indexedKey(namespace, owningRepo)
	if _, ok := r.indexed.Get(key); ok {
		return
	}

	muAny, _ := r.indexLocks.LoadOrStore(key, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Re-check after acquiring the lock: an earlier holder may have
	// just populated the indexed-set, in which case our backend write
	// is wasted work the LRU re-check catches here. This re-check is
	// the load-bearing dedupe.
	if _, ok := r.indexed.Get(key); ok {
		return
	}

	if err := r.markIndex(ctx, namespace, r.indexSuffix, owningRepo); err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index update failed",
			"namespace", namespace, "owning_repo", owningRepo, "error", err,
		)
		return
	}
	r.indexed.Add(key, struct{}{})
}

// Mark records key in the index.
func (i *NamespaceIndex) Mark(ctx context.Context, key string) error {
	if err := i.view.parent.authorize(ctx, i.view.namespace, auth.OpWrite); err != nil {
		return err
	}
	if err := i.view.parent.validatePublicIndexName(i.name); err != nil {
		return err
	}
	return i.view.parent.markIndex(ctx, i.view.namespace, i.name, key)
}

// Unmark removes key from the index. Missing keys are a no-op.
func (i *NamespaceIndex) Unmark(ctx context.Context, key string) error {
	if err := i.view.parent.authorize(ctx, i.view.namespace, auth.OpWrite); err != nil {
		return err
	}
	if err := i.view.parent.validatePublicIndexName(i.name); err != nil {
		return err
	}
	encoded, err := oci.EncodeTag(key)
	if err != nil {
		return err
	}
	if err := i.view.parent.inner.DeleteTagFiles(ctx, path.Join(i.view.namespace, i.name), encoded); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return err
	}
	return nil
}

// Has reports whether key is present in the index.
func (i *NamespaceIndex) Has(ctx context.Context, key string) (bool, error) {
	if err := i.view.parent.authorize(ctx, i.view.namespace, auth.OpRead); err != nil {
		return false, err
	}
	if err := i.view.parent.validatePublicIndexName(i.name); err != nil {
		return false, err
	}
	encoded, err := oci.EncodeTag(key)
	if err != nil {
		return false, err
	}
	tags, err := i.view.parent.inner.ListTags(ctx, path.Join(i.view.namespace, i.name))
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, tag := range tags {
		if tag == encoded {
			return true, nil
		}
	}
	return false, nil
}

// List returns all decoded keys in the index.
func (i *NamespaceIndex) List(ctx context.Context) ([]string, error) {
	if err := i.view.parent.authorize(ctx, i.view.namespace, auth.OpRead); err != nil {
		return nil, err
	}
	if err := i.view.parent.validatePublicIndexName(i.name); err != nil {
		return nil, err
	}
	return i.view.parent.listIndex(ctx, i.view.namespace, i.name)
}

func (r *Store) markIndex(ctx context.Context, namespace, indexName, key string) error {
	encoded, err := oci.EncodeTag(key)
	if err != nil {
		return err
	}
	rf := &oci.RepoFile{
		OwningRepo: path.Join(namespace, indexName),
		OwningTag:  encoded,
		Name:       packageIndexSentinelFile,
		MediaType:  "text/plain",
		Size:       int64(len(packageIndexSentinelBody)),
	}
	if _, err := r.inner.AddFile(ctx, rf, bytes.NewReader(packageIndexSentinelBody)); err != nil && !errors.Is(err, oci.ErrAlreadyExists) {
		return err
	}
	return nil
}

func (r *Store) listIndex(ctx context.Context, namespace, indexName string) ([]string, error) {
	tags, err := r.inner.ListTags(ctx, path.Join(namespace, indexName))
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list index %q for %q: %w", indexName, namespace, err)
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		decoded, derr := oci.DecodeTag(t)
		if derr != nil {
			logging.FromContext(ctx).WarnContext(ctx, "namespace index: skipping undecodable tag",
				"namespace", namespace, "index", indexName, "tag", t, "error", derr,
			)
			continue
		}
		out = append(out, decoded)
	}
	return out, nil
}

func (r *Store) validatePublicIndexName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: index name must not be empty", nsmeta.ErrInvalidOwningRepo)
	}
	if name == r.indexSuffix {
		return fmt.Errorf("%w: index name %q is reserved", nsmeta.ErrInvalidOwningRepo, name)
	}
	if path.Clean(name) != name || strings.ContainsRune(name, '/') || name == "." || name == ".." {
		return fmt.Errorf("%w: index name %q is not a single canonical segment", nsmeta.ErrInvalidOwningRepo, name)
	}
	if err := oci.ValidateRepoPrefix(name); err != nil {
		return fmt.Errorf("%w: index name %q is invalid: %v", nsmeta.ErrInvalidOwningRepo, name, err)
	}
	return nil
}

// stripPrefix removes a leading "<namespace>/" from owningRepo. The
// trailing slash is required so namespace "alpha" doesn't strip a
// repo from sibling namespace "alpha-foo". A namespace-equal
// owningRepo (i.e. the namespace's "root") is collapsed to "" so
// callers see a stable representation independent of whether they
// originally passed "" or the explicit "<namespace>".
func stripPrefix(owningRepo, namespace string) string {
	if owningRepo == namespace {
		return ""
	}
	prefix := namespace + "/"
	if strings.HasPrefix(owningRepo, prefix) {
		return owningRepo[len(prefix):]
	}
	return owningRepo
}

func indexedKey(namespace, owningRepo string) string {
	return namespace + "\x00" + owningRepo
}
