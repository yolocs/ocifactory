package namespace

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	// defaultPackageIndexSuffix names the per-namespace OCI repo whose
	// tags enumerate every owning-repo the wrapper has ever recorded a
	// write for. The repo lives at "<namespace>/_packages" by default
	// — operators who already park a real artifact under that name can
	// relocate it via [WithPackageIndexSuffix].
	defaultPackageIndexSuffix = "_packages"

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
// [NewPolicyAuthorizer].
type AuthzFactory func(Policy) (auth.Authorizer, error)

// RegistryBackend is the subset of *pkg/oci.Registry the wrapper
// needs. Pinned as an interface so tests can substitute the in-memory
// [oci.FakeRegistry]; production passes *oci.Registry directly.
//
// DeleteTagFiles is included even though no in-tree handler reaches
// for it today — keeping the interface a strict superset of the
// metadata-store [Backend] avoids two parallel interfaces and lets
// future yank semantics (single-version delete) plug in without
// reshaping the surface.
type RegistryBackend interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
	ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error)
	AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error
	DeleteRepoFiles(ctx context.Context, repo string) error
	DeleteTagFiles(ctx context.Context, repo string, tag string) error
}

// Registry is the data-plane wrapper that namespace-scopes every OCI
// operation. It:
//
//   - Reads [oci.RepoFile.Namespace] from the caller-supplied file
//     and prefixes OwningRepo with it so writes from namespace
//     "alpha" land under "alpha/<owning-repo>" and cannot be read
//     from namespace "beta".
//   - Enforces the namespace's authz policy on every call. The
//     compiled authorizer is cached behind a TTL+LRU and a
//     singleflight collapse so the hot path is one map lookup per
//     request and a cold-start burst pays for one compile, not N.
//   - Maintains a per-namespace package index at
//     "<namespace>/_packages" (one tag per owning-repo) so cascade
//     delete and namespace listing don't depend on the OCI _catalog
//     endpoint.
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

// RegistryOption customises a [Registry].
type RegistryOption func(*Registry)

// WithPackageIndexSuffix overrides the default ("_packages").
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
// for namespace metadata. The wrapper installs a mutation hook on
// store so admin-side Put / Delete calls invalidate the cached
// authorizer — failing to use NewRegistry (e.g. constructing the
// fields by hand in a test) means admin mutations only take effect
// after the cache TTL expires.
func NewRegistry(inner RegistryBackend, store *Store, opts ...RegistryOption) *Registry {
	r := &Registry{
		inner:       inner,
		store:       store,
		indexSuffix: defaultPackageIndexSuffix,
		factory:     NewPolicyAuthorizer,
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
	store.SetMutationHook(r.InvalidatePolicy)
	return r
}

// InvalidatePolicy drops the cached authorizer for name so the next
// request re-fetches the namespace spec. The Store calls this
// automatically after a successful Put or Delete; admin-side code
// that bypasses the Store can call it directly. A name that wasn't
// cached is a no-op.
func (r *Registry) InvalidatePolicy(name string) { r.cache.invalidate(name) }

// authorize loads (cached or freshly compiled) the namespace's
// authorizer and runs op against the [auth.AuthContext] in ctx.
// Returns:
//
//   - nil on allow.
//   - an error wrapping [ErrNotFound] when the namespace is unknown
//     (and caches the negative result).
//   - an error wrapping [auth.ErrUnauthorized] on policy deny or on
//     a missing AuthContext.
//   - any other error from the store / authorizer compile, unwrapped.
//
// The nil-AuthContext check lives here, not in the plugin
// authorizer, because the wrapper is the trust boundary: a
// third-party [AuthzFactory] that forgets to deny nil would
// otherwise turn into an auth bypass.
func (r *Registry) authorize(ctx context.Context, namespace string, op auth.Op) error {
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

func (r *Registry) authorizerFor(ctx context.Context, namespace string) (auth.Authorizer, error) {
	if v, ok := r.cache.get(namespace); ok {
		if v.notFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, namespace)
		}
		return v.authorizer, nil
	}

	// singleflight collapses concurrent misses for the same namespace
	// onto one Store.Get + factory(...) compile. The closure return
	// value is the *cachedPolicy we then cache locally; the
	// follower goroutines all observe the same value via singleflight.
	v, err, _ := r.flight.Do(namespace, func() (any, error) {
		// Re-check the cache inside the singleflight: another
		// goroutine may have populated it between our first miss
		// and our turn at the leader spot.
		if v, ok := r.cache.get(namespace); ok {
			return v, nil
		}
		ns, err := r.store.Get(ctx, namespace)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				neg := cachedPolicy{notFound: true}
				r.cache.put(namespace, neg)
				return neg, err
			}
			return cachedPolicy{}, err
		}
		az, ferr := r.factory(ns.Spec.Policy)
		if ferr != nil {
			// Compile errors signal misconfiguration the operator
			// must fix (e.g. an admin Put that bypassed validation).
			// Don't cache — repeat requests should keep surfacing
			// the error until the spec is corrected.
			return cachedPolicy{}, fmt.Errorf("compile authorizer for %q: %w", namespace, ferr)
		}
		entry := cachedPolicy{authorizer: az}
		r.cache.put(namespace, entry)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	cp := v.(cachedPolicy)
	if cp.notFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, namespace)
	}
	return cp.authorizer, nil
}

// resolveRepo validates the (namespace, owningRepo) pair against the
// namespace-escape attack and returns the joined backend repo path.
// Any owningRepo that doesn't path.Clean to itself, contains a "..",
// or is absolute is rejected as malformed — without this check
// path.Join("alpha", "../beta/foo") collapses to "beta/foo" and
// silently lands traffic in another namespace.
func (r *Registry) resolveRepo(namespace, owningRepo string) (string, error) {
	if owningRepo == "" {
		// An empty owning repo is a meaningful root for some
		// operations (e.g. an admin "list everything in this
		// namespace"); we accept it and resolve to just the
		// namespace.
		return namespace, nil
	}
	if path.IsAbs(owningRepo) {
		return "", fmt.Errorf("%w: owning repo %q is absolute", ErrInvalidOwningRepo, owningRepo)
	}
	cleaned := path.Clean(owningRepo)
	if cleaned != owningRepo {
		return "", fmt.Errorf("%w: owning repo %q is not in canonical form (clean: %q)", ErrInvalidOwningRepo, owningRepo, cleaned)
	}
	// path.Clean turns "" → "." but we already returned for empty;
	// any "." or ".." here is a real escape attempt.
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") || strings.HasSuffix(cleaned, "/..") {
		return "", fmt.Errorf("%w: owning repo %q escapes namespace", ErrInvalidOwningRepo, owningRepo)
	}
	return path.Join(namespace, cleaned), nil
}

// ErrInvalidOwningRepo is returned when an owning-repo string is
// malformed or attempts to escape the namespace it was scoped to.
// Handlers should map this to 400.
var ErrInvalidOwningRepo = errors.New("invalid owning repo")

func (r *Registry) packageIndexRepo(namespace string) string {
	return path.Join(namespace, r.indexSuffix)
}

// requireFile centralises the nil-pointer + namespace-required
// guards so each public method is one line shorter.
func requireFile(f *oci.RepoFile) error {
	if f == nil {
		return errors.New("RepoFile must not be nil")
	}
	if f.Namespace == "" {
		return errors.New("RepoFile.Namespace must not be empty")
	}
	return nil
}

// AddFile authorizes f.Namespace for write, prefixes f.OwningRepo
// with the namespace, forwards to the inner registry, and (best-
// effort) records the owning-repo in the namespace's package index
// so [ListPackages] can enumerate it later.
func (r *Registry) AddFile(ctx context.Context, f *oci.RepoFile, body io.Reader) (*oci.FileDescriptor, error) {
	if err := requireFile(f); err != nil {
		return nil, err
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpWrite); err != nil {
		return nil, err
	}
	scoped, err := r.scopedFile(f)
	if err != nil {
		return nil, err
	}
	desc, err := r.inner.AddFile(ctx, scoped, body)
	if err != nil {
		return nil, err
	}
	r.recordPackage(ctx, f.Namespace, f.OwningRepo)
	return desc, nil
}

// ReadFile authorizes f.Namespace for read and forwards to the inner
// registry with OwningRepo prefixed.
func (r *Registry) ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error) {
	if err := requireFile(f); err != nil {
		return nil, nil, err
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpRead); err != nil {
		return nil, nil, err
	}
	scoped, err := r.scopedFile(f)
	if err != nil {
		return nil, nil, err
	}
	return r.inner.ReadFile(ctx, scoped)
}

// BlobRedirectURL authorizes f.Namespace for read and forwards to
// the inner registry with OwningRepo prefixed. Returns ("", nil)
// when the backend serves blobs inline, mirroring [oci.Registry].
func (r *Registry) BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error) {
	if err := requireFile(f); err != nil {
		return "", err
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpRead); err != nil {
		return "", err
	}
	scoped, err := r.scopedFile(f)
	if err != nil {
		return "", err
	}
	return r.inner.BlobRedirectURL(ctx, scoped)
}

// ListTags authorizes f.Namespace for read and returns the canonical
// tags for f.OwningRepo within the namespace. f need only carry
// Namespace and OwningRepo; other fields are ignored.
func (r *Registry) ListTags(ctx context.Context, f *oci.RepoFile) ([]string, error) {
	if err := requireFile(f); err != nil {
		return nil, err
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpRead); err != nil {
		return nil, err
	}
	scoped, err := r.resolveRepo(f.Namespace, f.OwningRepo)
	if err != nil {
		return nil, err
	}
	return r.inner.ListTags(ctx, scoped)
}

// ListFiles authorizes f.Namespace for read and returns every file
// across every canonical version of f.OwningRepo within the
// namespace. The OwningRepo on each returned [oci.RepoFile] is
// rewritten back to the namespace-relative form and the Namespace
// field is set, so callers don't see the raw backend prefix leak
// through. Returned slices and pointers are freshly allocated by the
// inner registry; the wrapper makes a defensive copy of each
// element before mutating to keep any future inner-side caching
// safe.
func (r *Registry) ListFiles(ctx context.Context, f *oci.RepoFile) ([]*oci.RepoFile, error) {
	if err := requireFile(f); err != nil {
		return nil, err
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpRead); err != nil {
		return nil, err
	}
	scoped, err := r.resolveRepo(f.Namespace, f.OwningRepo)
	if err != nil {
		return nil, err
	}
	files, err := r.inner.ListFiles(ctx, scoped)
	if err != nil {
		return nil, err
	}
	out := make([]*oci.RepoFile, len(files))
	for i, src := range files {
		copyF := *src
		copyF.Namespace = f.Namespace
		copyF.OwningRepo = stripPrefix(copyF.OwningRepo, f.Namespace)
		out[i] = &copyF
	}
	return out, nil
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
		decoded, derr := decodeTag(t)
		if derr != nil {
			// A tag that doesn't decode means an admin or a previous
			// ocifactory version wrote a non-encoded tag. Skip it
			// rather than failing the whole listing — the contract
			// is "best-effort enumeration" and a stray tag is no
			// reason to deny callers their other packages.
			logging.FromContext(ctx).WarnContext(ctx, "namespace package index: skipping undecodable tag",
				"namespace", namespace, "tag", t, "error", derr,
			)
			continue
		}
		out = append(out, decoded)
	}
	return out, nil
}

// AppendRefs authorizes f.Namespace for write and forwards to the
// inner registry with OwningRepo prefixed. f need only carry
// Namespace, OwningRepo, and OwningTag (treated as the canonical
// tag); other fields are ignored.
func (r *Registry) AppendRefs(ctx context.Context, f *oci.RepoFile, refs ...string) error {
	if err := requireFile(f); err != nil {
		return err
	}
	if f.OwningTag == "" {
		return errors.New("RepoFile.OwningTag must be set for AppendRefs")
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpWrite); err != nil {
		return err
	}
	scoped, err := r.resolveRepo(f.Namespace, f.OwningRepo)
	if err != nil {
		return err
	}
	return r.inner.AppendRefs(ctx, scoped, f.OwningTag, refs...)
}

// DeleteRepoFiles authorizes f.Namespace for write and forwards to
// the inner registry with OwningRepo prefixed. The package index
// entry for f.OwningRepo is cleared after the inner delete succeeds
// — both the in-process indexed marker AND the backend index tag —
// so [ListPackages] no longer reports the now-empty repo. Index
// cleanup failures are logged at WARN and do not fail the call.
func (r *Registry) DeleteRepoFiles(ctx context.Context, f *oci.RepoFile) error {
	if err := requireFile(f); err != nil {
		return err
	}
	if err := r.authorize(ctx, f.Namespace, auth.OpWrite); err != nil {
		return err
	}
	scoped, err := r.resolveRepo(f.Namespace, f.OwningRepo)
	if err != nil {
		return err
	}
	if err := r.inner.DeleteRepoFiles(ctx, scoped); err != nil {
		return err
	}
	r.indexed.Remove(indexedKey(f.Namespace, f.OwningRepo))
	encoded, encErr := encodeTag(f.OwningRepo)
	if encErr != nil {
		// The owning-repo passed authorize() and resolveRepo() so
		// this should never fail; treat as bug-not-vuln and log.
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index: encode failed on delete",
			"namespace", f.Namespace, "owning_repo", f.OwningRepo, "error", encErr,
		)
		return nil
	}
	if err := r.inner.DeleteTagFiles(ctx, r.packageIndexRepo(f.Namespace), encoded); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index sweep failed",
			"namespace", f.Namespace, "owning_repo", f.OwningRepo, "error", err,
		)
	}
	return nil
}

// scopedFile returns a copy of f with OwningRepo rewritten to the
// namespace-prefixed backend form and Namespace cleared (the inner
// OCI registry doesn't know about namespaces). Returns
// [ErrInvalidOwningRepo] when the input would escape the namespace.
func (r *Registry) scopedFile(f *oci.RepoFile) (*oci.RepoFile, error) {
	scoped := *f
	scoped.Namespace = ""
	resolved, err := r.resolveRepo(f.Namespace, f.OwningRepo)
	if err != nil {
		return nil, err
	}
	scoped.OwningRepo = resolved
	return &scoped, nil
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
// mutexes. The map is bounded by the indexed-LRU's eviction (entries
// never used again age out as the LRU evicts).
func (r *Registry) recordPackage(ctx context.Context, namespace, owningRepo string) {
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
	// the load-bearing dedupe — without it, a later concurrent
	// arriver under a different mutex (impossible today thanks to
	// the no-delete invariant on indexLocks, but enforced by belt-
	// and-braces here) would race a redundant AddFile to the
	// backend.
	if _, ok := r.indexed.Get(key); ok {
		return
	}

	encoded, err := encodeTag(owningRepo)
	if err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "namespace package index: encode failed",
			"namespace", namespace, "owning_repo", owningRepo, "error", err,
		)
		return
	}
	rf := &oci.RepoFile{
		OwningRepo: r.packageIndexRepo(namespace),
		OwningTag:  encoded,
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

// encodeTag escapes owningRepo into a string usable as an OCI tag
// name. OCI tags allow [A-Za-z0-9_.-] only, so '/' must be escaped.
// We use a percent-style encoding (every '_' becomes "_5F" and every
// '/' becomes "_2F") rather than a substring like "__" because the
// substring approach silently collides: encodeTag("a/b") and
// encodeTag("a__b") would otherwise both produce "a__b". The
// encoding is lossless for any input over the OCI repo charset and
// round-trips exactly.
func encodeTag(owningRepo string) (string, error) {
	var b strings.Builder
	b.Grow(len(owningRepo))
	for _, r := range owningRepo {
		switch {
		case r == '_':
			b.WriteString("_5F")
		case r == '/':
			b.WriteString("_2F")
		case r == '.' || r == '-':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// OCI repo paths are lowercase alnum + ".-_/" per the
			// distribution spec; anything else means the caller is
			// constructing a malformed owning-repo. Surface it
			// rather than silently mangling.
			return "", fmt.Errorf("%w: owning repo %q contains invalid character %q", ErrInvalidOwningRepo, owningRepo, r)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w: owning repo must not be empty", ErrInvalidOwningRepo)
	}
	if b.Len() > 128 {
		// OCI tag spec: 1-128 chars. Most realistic repo names fit
		// after escaping; extremely long ones don't.
		return "", fmt.Errorf("%w: encoded owning repo exceeds 128-char OCI tag limit", ErrInvalidOwningRepo)
	}
	return b.String(), nil
}

// decodeTag reverses [encodeTag]. Any "_HH" sequence (where HH is
// a two-char hex pair) is replaced by the corresponding byte; a
// stray '_' not followed by valid hex is a malformed encoding and
// surfaces as an error.
func decodeTag(tag string) (string, error) {
	var b strings.Builder
	b.Grow(len(tag))
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(tag) {
			return "", fmt.Errorf("malformed escape at %d: trailing underscore", i)
		}
		decoded, err := hex.DecodeString(tag[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("malformed escape at %d: %w", i, err)
		}
		b.WriteByte(decoded[0])
		i += 2
	}
	return b.String(), nil
}

func indexedKey(namespace, owningRepo string) string {
	return namespace + "\x00" + owningRepo
}
