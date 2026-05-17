package python

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
	"oras.land/oras-go/v2/errdef"

	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/filter"
	"github.com/yolocs/ocifactory/pkg/proxy/indexcache"
	pypython "github.com/yolocs/ocifactory/pkg/proxy/python"
)

// ProxyFetcher is the subset of [*pkg/proxy/python.Fetcher] methods
// the handler depends on. The concrete *pypython.Fetcher satisfies
// this interface implicitly; the type exists so tests can drop in a
// fake without spinning up a real upstream server. It is not a
// stability surface — out-of-tree implementations are not a supported
// extension point.
type ProxyFetcher interface {
	GetSimpleIndex(ctx context.Context, pkg string) (*pypython.IndexResponse, error)
	GetTopLevelIndex(ctx context.Context) (*pypython.IndexResponse, error)
	GetVersionMetadata(ctx context.Context, pkg, version string) (*pypython.VersionMetadata, error)
	FetchFile(ctx context.Context, url string) (*pypython.FileResponse, error)
}

// FetcherFactory builds a [ProxyFetcher] for a given upstream URL. The
// handler memoises results keyed by the upstream string so a hot
// namespace pays for one factory call across its lifetime. The
// default factory wraps [pypython.New].
type FetcherFactory func(upstream *url.URL) (ProxyFetcher, error)

// DefaultFetcherFactory is the production factory: builds a
// [*pypython.Fetcher] with package defaults.
func DefaultFetcherFactory(upstream *url.URL) (ProxyFetcher, error) {
	return pypython.New(upstream)
}

// Proxy-related constants. Operator-facing knobs live behind Options;
// these are tuning numbers most operators never touch.
const (
	// DefaultIndexCacheTTL is how long an [indexcache.Cache] entry
	// stays "fresh" before the handler re-fetches upstream. 60 s
	// matches the design issue's starting recommendation for python
	// and npm; tunable via [WithProxyIndexCacheTTL].
	DefaultIndexCacheTTL = 60 * time.Second

	// DefaultL1IndexCacheTTL is how long the in-process L1 cache
	// keeps a (namespace, package) index body warm. Picked short so
	// an admin invalidation propagates quickly, long enough that
	// pip's burst of /simple/<pkg>/ requests during a single
	// resolution skips both the indexcache OCI round-trip and the
	// upstream fetch. Tunable via [WithProxyL1IndexCacheTTL].
	DefaultL1IndexCacheTTL = 10 * time.Second

	// l1IndexCacheSize bounds the number of distinct (namespace,
	// package) entries the L1 cache holds. Sized so even busy
	// multi-tenant deployments don't churn.
	l1IndexCacheSize = 4096
)

// L1 index cache entry. We hold the response body and content-type
// alongside the time it was written so the handler can decide
// freshness without re-touching the OCI backend.
type l1IndexEntry struct {
	body        []byte
	contentType string
	fetchedAt   time.Time
}

type l1IndexCache struct {
	lru *expirable.LRU[string, l1IndexEntry]
}

func newL1IndexCache(ttl time.Duration) *l1IndexCache {
	if ttl <= 0 {
		return &l1IndexCache{}
	}
	return &l1IndexCache{
		lru: expirable.NewLRU[string, l1IndexEntry](l1IndexCacheSize, nil, ttl),
	}
}

func (c *l1IndexCache) get(key string) (l1IndexEntry, bool) {
	if c.lru == nil {
		return l1IndexEntry{}, false
	}
	return c.lru.Get(key)
}

func (c *l1IndexCache) put(key string, e l1IndexEntry) {
	if c.lru == nil {
		return
	}
	c.lru.Add(key, e)
}

func (c *l1IndexCache) invalidate(key string) {
	if c.lru == nil {
		return
	}
	c.lru.Remove(key)
}

// proxyState holds everything the handler needs to serve a proxy
// namespace. It hangs off *Handler and is populated when the handler
// is constructed with the appropriate options.
type proxyState struct {
	factory       FetcherFactory
	indexCache    *indexcache.Cache
	negCache      *indexcache.NegativeCache
	l1            *l1IndexCache
	indexCacheTTL time.Duration
	fetchers      sync.Map // upstream string -> ProxyFetcher
	indexFlight   singleflight.Group
	fileFlight    singleflight.Group
	now           func() time.Time
}

// newProxyState builds the proxy plumbing from handler config. Always
// constructs an L1 cache; the indexcache + negCache + factory are
// only populated when the operator wired them via Options.
func newProxyState(cfg handlerConfig) *proxyState {
	factory := cfg.fetcherFactory
	if factory == nil {
		factory = DefaultFetcherFactory
	}
	ttl := cfg.proxyIndexCacheTTL
	if ttl <= 0 {
		ttl = DefaultIndexCacheTTL
	}
	l1TTL := cfg.proxyL1IndexCacheTTL
	if l1TTL <= 0 {
		l1TTL = DefaultL1IndexCacheTTL
	}
	return &proxyState{
		factory:       factory,
		indexCache:    cfg.indexCache,
		negCache:      cfg.negCache,
		l1:            newL1IndexCache(l1TTL),
		indexCacheTTL: ttl,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// fetcherFor returns the cached [ProxyFetcher] for upstream, building
// one via the configured factory on first use. Concurrent first-misses
// for the same upstream race; the loser's value is dropped (cheap —
// fetchers carry only in-memory caches).
func (p *proxyState) fetcherFor(upstream string) (ProxyFetcher, error) {
	if v, ok := p.fetchers.Load(upstream); ok {
		return v.(ProxyFetcher), nil
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream %q: %w", upstream, err)
	}
	f, err := p.factory(u)
	if err != nil {
		return nil, fmt.Errorf("build fetcher for %q: %w", upstream, err)
	}
	actual, _ := p.fetchers.LoadOrStore(upstream, f)
	return actual.(ProxyFetcher), nil
}

// dispatchProxy returns the [namespace.Spec] for the request's
// namespace and whether the request should be served via the proxy
// path. When the namespace is unknown or malformed, it writes the
// matching error response and returns ok=false so the caller stops.
func (h *Handler) dispatchProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry) (*namespace.Spec, bool, bool) {
	spec, err := scoped.Spec(req.Context())
	if err != nil {
		if handler.WriteNamespaceError(w, err) {
			return nil, false, false
		}
		handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "namespace lookup failed")
		return nil, false, false
	}
	isProxy := spec != nil && spec.Mode == namespace.ModeProxy
	return spec, isProxy, true
}

// handleFileGetProxy is the proxy-mode file path. Order of operations
// mirrors the design issue:
//
//  1. registry hit → serve.
//  2. cheap filters (name-only). Deny → 404.
//  3. upstream version metadata; on metadata error, return matching
//     status.
//  4. metadata-dependent filters (publish-time delay). Deny → 404.
//  5. upstream file fetch.
//  6. tee through AddFile while streaming to client. AddFile error
//     mid-stream → 502; next request retries.
//
// Concurrent first-misses for the same (ns, pkg, version, filename)
// collapse onto one upstream fetch via the file-singleflight group.
func (h *Handler) handleFileGetProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, f *oci.RepoFile) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	// Cache-hit fast path. ReadFile authorizes for read and serves
	// from OCI just like the hosted path; on miss we bubble through
	// to the upstream resolver.
	if h.tryServeFromRegistry(w, req, scoped, f) {
		return
	}

	pkg := pathPackage(f)
	version := f.OwningTag
	filename := f.Name

	if h.proxy.negCache != nil {
		negKey := negativeKey(scoped.Namespace(), pkg, version, filename)
		if h.proxy.negCache.IsKnownMissing(negKey) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	upstream := spec.Proxy.Upstream
	fetcher, err := h.proxy.fetcherFor(upstream)
	if err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		return
	}

	// Cheap filters first — if a name-only allow/denylist would
	// reject this package, we don't pay for upstream metadata.
	ref := filter.Ref{Package: pkg, Version: version}
	if decision, denyFilter, err := spec.Proxy.Filters.Decide(ctx, ref); err != nil {
		logger.DebugContext(ctx, "filter chain error", "error", err, "filter", filterKind(denyFilter))
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	} else if decision == filter.DecisionDeny {
		logger.InfoContext(ctx, "proxy file denied by filter", "package", pkg, "version", version, "filter", filterKind(denyFilter))
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	}
	// DecisionNeedsMoreData: we'll re-run after metadata. Allow /
	// abstain both fall through.

	limit := h.maxUploadBytes
	if limit <= 0 {
		limit = DefaultMaxUploadBytes
	}

	// Singleflight so concurrent cold-miss requests for the same file
	// pay one upstream fetch + one AddFile across the process. The
	// leader streams upstream → AddFile (which writes the blob to OCI)
	// and simultaneously tees the bytes onto its own ResponseWriter.
	// Followers observe a "streamed" outcome and re-read the now-cached
	// file from OCI to serve their own clients.
	key := scoped.Namespace() + "|" + f.OwningRepo + "|" + version + "|" + filename
	isHead := req.Method == http.MethodHead
	amLeader := false
	resultV, err, _ := h.proxy.fileFlight.Do(key, func() (any, error) {
		amLeader = true

		// Re-check the registry inside the singleflight — a peer
		// may have populated the cache while we were waiting.
		if hit, hitErr := h.peekRegistry(ctx, scoped, f); hitErr == nil && hit {
			return fileFlightResult{cached: true}, nil
		}

		meta, ferr := fetcher.GetVersionMetadata(ctx, pkg, version)
		if ferr != nil {
			return fileFlightResult{}, ferr
		}
		fileMeta, ok := meta.FindFile(filename)
		if !ok && strings.HasSuffix(filename, ".metadata") {
			// PEP 658 sidecar. PyPI's JSON `urls` list doesn't enumerate
			// `.metadata` files separately, but advertises them via the
			// per-file `core-metadata` field and serves them at
			// `<wheel-url>.metadata`. The rewriter passes the
			// `data-core-metadata` / `data-dist-info-metadata` attributes
			// through to pip, so pip then requests the sidecar by appending
			// `.metadata` to the wheel filename. Resolve by stripping
			// `.metadata`, finding the underlying wheel, and synthesizing
			// the upstream URL.
			base := strings.TrimSuffix(filename, ".metadata")
			if baseMeta, baseOK := meta.FindFile(base); baseOK {
				fileMeta = pypython.FileMetadata{
					Filename:   filename,
					URL:        baseMeta.URL + ".metadata",
					UploadTime: baseMeta.UploadTime,
				}
				ok = true
			}
		}
		if !ok {
			return fileFlightResult{}, fmt.Errorf("python proxy: file %q not in metadata for %s %s: %w",
				filename, pkg, version, proxy.ErrNotFound)
		}

		// Metadata-dependent filter pass. UploadTime is from the
		// matched file; version-level earliest would also work but
		// per-file is the most precise signal for a delay filter.
		metaRef := filter.Ref{Package: pkg, Version: version, UploadTime: fileMeta.UploadTime}
		if decision, denyFilter, derr := spec.Proxy.Filters.Decide(ctx, metaRef); derr != nil {
			return fileFlightResult{}, fmt.Errorf("filter error: %w (kind=%s)", derr, filterKind(denyFilter))
		} else if decision == filter.DecisionDeny {
			return fileFlightResult{filterDeny: true, filterName: filterKind(denyFilter)}, nil
		}

		// Refuse upfront when upstream metadata claims a body bigger
		// than the cap so we don't pay for the upstream fetch.
		if fileMeta.Size > 0 && fileMeta.Size > limit {
			return fileFlightResult{}, fmt.Errorf("upstream metadata size %d exceeds %d-byte cap", fileMeta.Size, limit)
		}

		fileResp, ferr := fetcher.FetchFile(ctx, fileMeta.URL)
		if ferr != nil {
			return fileFlightResult{}, ferr
		}
		defer fileResp.Body.Close()

		populated := &oci.RepoFile{
			OwningRepo: f.OwningRepo,
			OwningTag:  f.OwningTag,
			Name:       f.Name,
			MediaType:  pickMediaType(filename, fileResp.ContentType),
			Size:       fileMeta.Size,
		}

		// Defense in depth even when fileMeta.Size is 0 / lying:
		// LimitReader truncates the body at limit+1 so an AddFile that
		// somehow accepted an unbounded body still can't burn unbounded
		// memory or disk. AddFile will surface the truncation as a
		// digest / size mismatch.
		var body io.Reader = io.LimitReader(fileResp.Body, limit+1)

		// Leader-only tee: set client response headers eagerly and
		// pipe AddFile's read through a tolerant writer onto our own
		// ResponseWriter. If our client disconnects mid-stream the
		// writer swallows the error so AddFile keeps draining upstream
		// — that's the whole point of the cache fill, and finishing it
		// makes the byte we paid upstream serve every follower for
		// free. For HEAD we don't tee; AddFile still drains upstream
		// to populate the cache, and tryServeFromRegistry serves the
		// HEAD response after the flight returns.
		if !isHead {
			w.Header().Set("Content-Type", populated.MediaType)
			if fileMeta.Size > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(fileMeta.Size, 10))
			}
			body = io.TeeReader(body, &tolerantWriter{w: w})
		}

		// AddFile writes blob + file manifest + version anchor;
		// authorizes for write on the bound namespace policy. An
		// already-exists error means a concurrent cold-miss raced us
		// before singleflight could; probeExistingFile reports it
		// before reading the body, so no client bytes were written and
		// the follower path can serve from the cache the peer wrote.
		if _, addErr := scoped.AddFile(ctx, populated, body); addErr != nil {
			if errors.Is(addErr, oci.ErrAlreadyExists) {
				return fileFlightResult{cached: true}, nil
			}
			return fileFlightResult{}, addErr
		}
		return fileFlightResult{streamed: true}, nil
	})

	if err != nil {
		// Classify and surface upstream errors. If we're the leader
		// and the tee already wrote a 200 + partial body, http.Error
		// will fail silently — that's fine, the client sees a truncated
		// response and the next request will retry. Followers haven't
		// written anything to their own ResponseWriter yet.
		switch {
		case errors.Is(err, proxy.ErrNotFound):
			if h.proxy.negCache != nil {
				h.proxy.negCache.RecordMiss(negativeKey(scoped.Namespace(), pkg, version, filename))
			}
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, proxy.ErrUpstreamMalformed):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
		case errors.Is(err, proxy.ErrUpstreamUnavailable):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream unavailable")
		default:
			if handler.WriteNamespaceError(w, err) {
				return
			}
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "proxy file fetch failed")
		}
		return
	}

	result := resultV.(fileFlightResult)
	if result.filterDeny {
		logger.InfoContext(ctx, "proxy file denied by filter", "package", pkg, "version", version, "filter", result.filterName)
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	}
	if result.streamed && amLeader && !isHead {
		// Bytes were written via the tee during AddFile. Nothing left
		// to do on the leader's GET response.
		return
	}
	// Either we're a follower waiting on the leader, the leader on a
	// HEAD that needs the headers written, or we hit ErrAlreadyExists
	// mid-flight. All three serve the canonical headers + body from
	// the cache the leader just populated.
	if h.tryServeFromRegistry(w, req, scoped, f) {
		return
	}
	handler.WriteError(ctx, w, http.StatusBadGateway, nil, "proxy fill reported success but cache miss on re-read")
}

// tolerantWriter wraps an io.Writer (the proxy leader's
// http.ResponseWriter) so a client-side write failure stops further
// writes but doesn't surface as an error to the upstream-side reader.
// This is what lets AddFile finish populating the OCI cache after the
// client disconnects mid-stream: subsequent tee Reads keep flowing to
// AddFile without io.TeeReader propagating the dead client back to the
// upstream copy.
type tolerantWriter struct {
	w    io.Writer
	dead bool
}

func (t *tolerantWriter) Write(p []byte) (int, error) {
	if t.dead {
		return len(p), nil
	}
	if _, err := t.w.Write(p); err != nil {
		t.dead = true
	}
	return len(p), nil
}

// fileFlightResult is the singleflight return value for proxy file
// fetches. Exactly one of streamed / cached / filterDeny is true on
// success; an error result returns the zero value.
//
//   - streamed: the leader called AddFile, which drained upstream into
//     the OCI cache (and, for GET, simultaneously teed bytes onto its
//     own client response). Followers serve from the cache.
//   - cached: a peer populated the cache while this caller was waiting
//     (either the re-probe inside the flight hit, or AddFile returned
//     ErrAlreadyExists before reading any body). Everyone serves from
//     cache.
//   - filterDeny: the metadata-dependent filter chain rejected this
//     version after the upstream metadata call. Everyone returns 404.
type fileFlightResult struct {
	streamed   bool
	cached     bool
	filterDeny bool
	filterName string
}

// peekRegistry probes the registry for a file without consuming the
// body. Used by the singleflight follower path to decide between
// "peer wrote the file, re-serve from cache" and "peer failed, do
// our own fetch". Returns (true, nil) on hit, (false, nil) on miss,
// non-nil error on backend trouble.
func (h *Handler) peekRegistry(ctx context.Context, scoped *namespace.ScopedRegistry, f *oci.RepoFile) (bool, error) {
	desc, rc, err := scoped.ReadFile(ctx, f)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	rc.Close()
	_ = desc
	return true, nil
}

// tryServeFromRegistry attempts the hosted-style ReadFile + redirect
// flow. Returns true on success (response written), false on miss so
// the caller continues with the proxy fetch. Errors other than
// not-found are written as the appropriate HTTP status and return true.
func (h *Handler) tryServeFromRegistry(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, f *oci.RepoFile) bool {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	if req.Method != http.MethodHead {
		if redirectURL, err := scoped.BlobRedirectURL(ctx, f); err == nil && redirectURL != "" {
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return true
		} else if err != nil {
			logger.DebugContext(ctx, "blob redirect probe failed", "error", err)
		}
	}

	desc, r, err := scoped.ReadFile(ctx, f)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false
		}
		if handler.WriteNamespaceError(w, err) {
			return true
		}
		if oci.HasCode(err, http.StatusUnauthorized) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return true
		}
		if oci.HasCode(err, http.StatusForbidden) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return true
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return true
	}
	defer r.Close()

	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(desc.File.Size, 10))
	w.Header().Set("X-Checksum-Sha256", desc.File.Digest.String())
	if req.Method == http.MethodHead {
		return true
	}
	if _, err := io.Copy(w, r); err != nil {
		logger.DebugContext(ctx, "copy proxy hit body", "error", err)
	}
	return true
}

// handlePackageIndexProxy serves /{ns}/simple/{pkg}/ in proxy mode.
//
//  1. L1 in-memory cache hit → serve.
//  2. indexcache.Get → fresh (within DefaultIndexCacheTTL) → serve +
//     warm L1.
//  3. Upstream fetch:
//     - success → rewrite URLs → indexcache.Put → serve + warm L1.
//     - upstream error + stale cache → serve stale; log.
//     - upstream error + no cache → synthesize from
//     ListFiles("packages/"+pkg) (existing hosted code path).
//     - synthesis empty → 503.
//
// Concurrent refreshes for the same (namespace, pkg) collapse onto
// one upstream fetch via the index-singleflight group.
func (h *Handler) handlePackageIndexProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, pkg string) {
	h.serveProxyIndex(w, req, scoped, spec, pkg)
}

// handleSimpleIndexProxy serves /{ns}/simple/ in proxy mode. Same
// flow as the per-package path with cache key pkg="" and synthesis
// fallback `ListTags("index")`.
func (h *Handler) handleSimpleIndexProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec) {
	h.serveProxyIndex(w, req, scoped, spec, "")
}

func (h *Handler) serveProxyIndex(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, pkg string) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)
	ns := scoped.Namespace()
	cacheKey := ns + "|" + pkg

	// L1 in-memory cache fast path.
	if e, ok := h.proxy.l1.get(cacheKey); ok {
		writeIndexBody(w, req, e.body, e.contentType)
		return
	}

	// indexcache OCI freshness check. A fresh entry means we don't
	// fetch upstream; a stale entry remains a fallback if upstream
	// later goes down inside this same request.
	var (
		cachedBody        []byte
		cachedContentType string
		cachedFetchedAt   time.Time
		cachedFound       bool
	)
	if h.proxy.indexCache != nil {
		b, ct, fa, found, err := h.proxy.indexCache.Get(ctx, ns, indexCacheKey(pkg))
		if err != nil {
			logger.DebugContext(ctx, "indexcache get failed", "error", err)
		} else if found {
			cachedBody, cachedContentType, cachedFetchedAt, cachedFound = b, ct, fa, true
			if h.proxy.now().Sub(fa) < h.proxy.indexCacheTTL {
				h.proxy.l1.put(cacheKey, l1IndexEntry{body: b, contentType: ct, fetchedAt: fa})
				writeIndexBody(w, req, b, ct)
				return
			}
		}
	}

	upstream := spec.Proxy.Upstream
	fetcher, err := h.proxy.fetcherFor(upstream)
	if err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		return
	}

	resultV, err, _ := h.proxy.indexFlight.Do(cacheKey, func() (any, error) {
		// Re-check L1 inside the singleflight in case a peer
		// populated it while we were queueing.
		if e, ok := h.proxy.l1.get(cacheKey); ok {
			return indexFlightResult{body: e.body, contentType: e.contentType}, nil
		}
		var (
			indexResp *pypython.IndexResponse
			fetchErr  error
		)
		if pkg == "" {
			indexResp, fetchErr = fetcher.GetTopLevelIndex(ctx)
		} else {
			indexResp, fetchErr = fetcher.GetSimpleIndex(ctx, pkg)
		}
		if fetchErr != nil {
			return indexFlightResult{}, fetchErr
		}

		body := indexResp.Body
		if pkg != "" {
			// Per-package index: rewrite file URLs back through
			// ocifactory. The top-level index doesn't need
			// rewriting (its hrefs are package-relative).
			rewritten, rerr := pypython.RewriteSimpleIndex(indexResp.Body, ns, pkg)
			if rerr != nil {
				return indexFlightResult{}, rerr
			}
			body = rewritten
		}

		if h.proxy.indexCache != nil {
			if perr := h.proxy.indexCache.Put(ctx, ns, indexCacheKey(pkg), body, indexResp.ContentType); perr != nil {
				logger.DebugContext(ctx, "indexcache put failed", "error", perr)
			}
		}
		h.proxy.l1.put(cacheKey, l1IndexEntry{body: body, contentType: indexResp.ContentType, fetchedAt: h.proxy.now()})
		return indexFlightResult{body: body, contentType: indexResp.ContentType}, nil
	})

	if err == nil {
		r := resultV.(indexFlightResult)
		writeIndexBody(w, req, r.body, r.contentType)
		return
	}

	// Upstream error path. We try, in order:
	//   - the stale indexcache entry (still better than the
	//     synthesized one, since it's the last known canonical
	//     upstream view).
	//   - synthesis from local listings — minimal but accurate.
	//   - 503 when synthesis is empty.
	if errors.Is(err, proxy.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if cachedFound {
		logger.WarnContext(ctx, "serving stale proxy index after upstream error",
			"namespace", ns, "package", pkg, "age", h.proxy.now().Sub(cachedFetchedAt), "error", err)
		writeIndexBody(w, req, cachedBody, cachedContentType)
		return
	}

	h.synthesizeIndex(w, req, scoped, pkg, err)
}

type indexFlightResult struct {
	body        []byte
	contentType string
}

// synthesizeIndex serves a minimal but accurate index built from local
// listings when both the upstream and the cache are unavailable. For
// per-package: enumerate files in `packages/<pkg>` and render the same
// HTML/JSON the hosted path would. For top-level: enumerate tags on
// `index` (hosted handler's existing dedupe list).
//
// An empty synthesis result means we have no cached files for this
// (namespace, package) either — 503, since serving a literal empty
// index would have pip conclude the package was unyanked, which is
// worse than a clear "we're degraded" signal.
func (h *Handler) synthesizeIndex(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, pkg string, upstreamErr error) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)
	ns := scoped.Namespace()

	if pkg == "" {
		tags, err := scoped.ListTags(ctx, "index")
		if err != nil && !errors.Is(err, errdef.ErrNotFound) {
			handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr,
				fmt.Sprintf("upstream unavailable and synthesis failed: %v", err))
			return
		}
		if len(tags) == 0 {
			handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr, "upstream unavailable, no cached index")
			return
		}
		logger.WarnContext(ctx, "synthesizing top-level index from local state", "namespace", ns, "count", len(tags), "error", upstreamErr)
		if pickContentType(req.Header.Get("Accept")) == contentTypeJSONv1 {
			writeJSONIndexList(w, tags)
			return
		}
		page := indexPage{Title: "Simple Index"}
		for _, tag := range tags {
			page.Files = append(page.Files, indexFile{
				Filename: tag,
				URL: (&url.URL{
					Scheme: req.URL.Scheme,
					Host:   req.URL.Host,
					Path:   "/" + ns + "/simple/" + tag + "/",
				}).String(),
			})
		}
		h.renderer.RenderHTML(w, "simple.html", page)
		return
	}

	files, err := h.resolvePackageFiles(ctx, scoped, pkg)
	if err != nil && !errors.Is(err, errdef.ErrNotFound) {
		handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr,
			fmt.Sprintf("upstream unavailable and synthesis failed: %v", err))
		return
	}
	if len(files) == 0 {
		handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr, "upstream unavailable, no cached files")
		return
	}
	logger.WarnContext(ctx, "synthesizing package index from local state", "namespace", ns, "package", pkg, "count", len(files), "error", upstreamErr)
	rendered := make([]indexFile, 0, len(files))
	for _, f := range files {
		rendered = append(rendered, indexFile{
			Filename: f.Filename,
			URL:      renderedFileURL(req, ns, pkg, f),
			Sha256:   f.Sha256,
		})
	}
	if pickContentType(req.Header.Get("Accept")) == contentTypeJSONv1 {
		writeJSONPackageIndex(w, pkg, rendered)
		return
	}
	h.renderer.RenderHTML(w, "simple.html", indexPage{Title: pkg, Files: rendered})
}

func writeIndexBody(w http.ResponseWriter, req *http.Request, body []byte, contentType string) {
	if contentType == "" {
		contentType = "text/html"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// pathPackage extracts the (normalized) package name from a RepoFile's
// OwningRepo. The python handler stores files under
// "packages/<pkg>", so we trim the prefix; any other shape is a bug
// in this package and we surface "" so downstream filter / metadata
// calls fail loudly.
func pathPackage(f *oci.RepoFile) string {
	const prefix = "packages/"
	if f == nil {
		return ""
	}
	if len(f.OwningRepo) <= len(prefix) {
		return ""
	}
	if f.OwningRepo[:len(prefix)] != prefix {
		return ""
	}
	return f.OwningRepo[len(prefix):]
}

// negativeKey is the cache key for negative-cache entries on proxy
// file fetches. Includes namespace so a 404 in one namespace doesn't
// suppress a probe in another.
func negativeKey(ns, pkg, version, filename string) string {
	return "py|" + ns + "|" + pkg + "|" + version + "|" + filename
}

// indexCacheKey is the indexcache pkg argument for the python proxy.
// The top-level index uses an underscore prefix that cannot occur in
// a PEP 503 package name so it can't collide with a per-package
// entry. (PEP 503 names lowercase ASCII letters / digits / `-` / `_` /
// `.`, but never start with an underscore in any realistic project —
// and even if they did, the underscore prefix segment is reserved
// internal here.)
func indexCacheKey(pkg string) string {
	if pkg == "" {
		return "_toplevel"
	}
	return pkg
}

func filterKind(f filter.Filter) string {
	if f == nil {
		return ""
	}
	if k, ok := f.(filter.Kinded); ok {
		return k.Kind()
	}
	return fmt.Sprintf("%T", f)
}

// pickMediaType prefers the handler's filename-derived mime type
// (detectMediaType) over a noisy upstream value, since the OCI layer
// descriptor is queried with the filename's media type when pip
// re-reads the cached file. When detection fails we fall back to the
// upstream Content-Type, then to application/octet-stream.
func pickMediaType(filename, upstream string) string {
	if mt := detectMediaType(filename); mt != "" && mt != "application/octet-stream" {
		return mt
	}
	if upstream != "" {
		return upstream
	}
	return "application/octet-stream"
}
