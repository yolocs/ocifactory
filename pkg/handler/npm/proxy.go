package npm

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/filter"
	"github.com/yolocs/ocifactory/pkg/proxy/indexcache"
	proxynpm "github.com/yolocs/ocifactory/pkg/proxy/npm"
)

// ProxyFetcher is the subset of [*pkg/proxy/npm.Fetcher] methods the
// handler depends on. It exists so tests can inject a fake upstream
// boundary; it is not an out-of-tree extension surface.
type ProxyFetcher interface {
	GetPackument(ctx context.Context, pkg string) (*proxynpm.PackumentResponse, error)
	FetchTarball(ctx context.Context, pkg, version, filename string) (*proxynpm.TarballResponse, error)
}

// FetcherFactory builds a [ProxyFetcher] for a namespace upstream URL.
type FetcherFactory func(upstream *url.URL) (ProxyFetcher, error)

// DefaultFetcherFactory builds the production npm proxy fetcher.
func DefaultFetcherFactory(upstream *url.URL) (ProxyFetcher, error) {
	return proxynpm.New(upstream)
}

const (
	DefaultIndexCacheTTL   = 60 * time.Second
	DefaultL1IndexCacheTTL = 10 * time.Second
	l1IndexCacheSize       = 4096
)

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

type proxyState struct {
	factory       FetcherFactory
	indexCache    *indexcache.Cache
	negCache      *indexcache.NegativeCache
	l1            *l1IndexCache
	indexCacheTTL time.Duration
	fetchers      sync.Map
	indexFlight   singleflight.Group
	fileFlight    singleflight.Group
	now           func() time.Time
}

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
	if l1TTL == 0 {
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

// WithFetcherFactory installs a custom npm proxy fetcher factory.
func WithFetcherFactory(f FetcherFactory) Option {
	return func(c *handlerConfig) { c.fetcherFactory = f }
}

// WithProxyIndexCache wires the OCI-backed pull-through packument cache.
func WithProxyIndexCache(c *indexcache.Cache) Option {
	return func(cfg *handlerConfig) { cfg.indexCache = c }
}

// WithProxyNegativeCache wires the in-memory negative cache for tarball
// misses.
func WithProxyNegativeCache(c *indexcache.NegativeCache) Option {
	return func(cfg *handlerConfig) { cfg.negCache = c }
}

// WithProxyIndexCacheTTL overrides [DefaultIndexCacheTTL].
func WithProxyIndexCacheTTL(d time.Duration) Option {
	return func(c *handlerConfig) { c.proxyIndexCacheTTL = d }
}

// WithProxyL1IndexCacheTTL overrides [DefaultL1IndexCacheTTL].
func WithProxyL1IndexCacheTTL(d time.Duration) Option {
	return func(c *handlerConfig) { c.proxyL1IndexCacheTTL = d }
}

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

func (h *Handler) handlePackumentProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, pkg string) {
	ctx := req.Context()
	if err := scoped.Authorize(ctx, auth.OpRead); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "namespace authorization failed")
		return
	}

	result, err := h.loadPackumentProxy(ctx, scoped, spec, pkg)
	if err == nil {
		writeProxyPackumentBody(w, req, result.body, result.contentType)
		return
	}

	if errors.Is(err, proxy.ErrNotFound) {
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, proxy.ErrUpstreamMalformed) {
		handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
		return
	}
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		return
	}
	h.synthesizePackument(w, req, scoped, pkg, err)
}

func (h *Handler) handleDistTagListProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, pkg string) {
	ctx := req.Context()
	if err := scoped.Authorize(ctx, auth.OpRead); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "namespace authorization failed")
		return
	}

	result, err := h.loadPackumentProxy(ctx, scoped, spec, pkg)
	if err != nil {
		switch {
		case errors.Is(err, proxy.ErrNotFound):
			http.Error(w, "package not found", http.StatusNotFound)
		case errors.Is(err, proxy.ErrUpstreamMalformed):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
		case errors.Is(err, proxy.ErrUpstreamUnavailable):
			handler.WriteError(ctx, w, http.StatusServiceUnavailable, err, "upstream unavailable")
		default:
			handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		}
		return
	}

	var p packument
	if err := json.Unmarshal(result.body, &p); err != nil {
		handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
		return
	}
	if p.DistTags == nil {
		p.DistTags = map[string]string{}
	}
	w.Header().Set("Content-Type", distTagsMediaType)
	if req.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(p.DistTags)
}

type proxyPackumentResult struct {
	body        []byte
	contentType string
}

func (h *Handler) loadPackumentProxy(ctx context.Context, scoped *namespace.ScopedRegistry, spec *namespace.Spec, pkg string) (proxyPackumentResult, error) {
	logger := logging.FromContext(ctx)
	ns := scoped.Namespace()
	cacheKey := ns + "|" + pkg
	indexKey := proxyIndexCacheKey(pkg)

	if e, ok := h.proxy.l1.get(cacheKey); ok {
		return proxyPackumentResult{body: e.body, contentType: e.contentType}, nil
	}

	var (
		cachedBody        []byte
		cachedContentType string
		cachedFetchedAt   time.Time
		cachedFound       bool
	)
	if h.proxy.indexCache != nil {
		b, ct, fa, found, err := h.proxy.indexCache.Get(ctx, ns, indexKey)
		if err != nil {
			logger.DebugContext(ctx, "npm indexcache get failed", "error", err)
		} else if found {
			cachedBody, cachedContentType, cachedFetchedAt, cachedFound = b, ct, fa, true
			if h.proxy.now().Sub(fa) < h.proxy.indexCacheTTL {
				h.proxy.l1.put(cacheKey, l1IndexEntry{body: b, contentType: ct, fetchedAt: fa})
				return proxyPackumentResult{body: b, contentType: ct}, nil
			}
		}
	}

	fetcher, err := h.proxy.fetcherFor(spec.Proxy.Upstream)
	if err != nil {
		return proxyPackumentResult{}, err
	}

	resultV, err, _ := h.proxy.indexFlight.Do(cacheKey, func() (any, error) {
		if e, ok := h.proxy.l1.get(cacheKey); ok {
			return indexFlightResult{body: e.body, contentType: e.contentType}, nil
		}
		resp, ferr := fetcher.GetPackument(ctx, pkg)
		if ferr != nil {
			return indexFlightResult{}, ferr
		}
		body, rerr := proxynpm.RewritePackument(resp.Body, ns, pkg)
		if rerr != nil {
			return indexFlightResult{}, rerr
		}
		if h.proxy.indexCache != nil {
			if perr := h.proxy.indexCache.Put(ctx, ns, indexKey, body, resp.ContentType); perr != nil {
				logger.DebugContext(ctx, "npm indexcache put failed", "error", perr)
			}
		}
		h.proxy.l1.put(cacheKey, l1IndexEntry{body: body, contentType: resp.ContentType, fetchedAt: h.proxy.now()})
		return indexFlightResult{body: body, contentType: resp.ContentType}, nil
	})
	if err == nil {
		r := resultV.(indexFlightResult)
		return proxyPackumentResult{body: r.body, contentType: r.contentType}, nil
	}

	if errors.Is(err, proxy.ErrNotFound) {
		return proxyPackumentResult{}, err
	}
	if errors.Is(err, proxy.ErrUpstreamMalformed) {
		if cachedFound {
			logger.WarnContext(ctx, "serving stale npm packument after malformed upstream response",
				"namespace", ns, "package", pkg, "age", h.proxy.now().Sub(cachedFetchedAt), "error", err)
			return proxyPackumentResult{body: cachedBody, contentType: cachedContentType}, nil
		}
		return proxyPackumentResult{}, err
	}
	if cachedFound {
		logger.WarnContext(ctx, "serving stale npm packument after upstream error",
			"namespace", ns, "package", pkg, "age", h.proxy.now().Sub(cachedFetchedAt), "error", err)
		return proxyPackumentResult{body: cachedBody, contentType: cachedContentType}, nil
	}
	return proxyPackumentResult{}, err
}

type indexFlightResult struct {
	body        []byte
	contentType string
}

func (h *Handler) synthesizePackument(w http.ResponseWriter, req *http.Request, _ *namespace.ScopedRegistry, pkg string, upstreamErr error) {
	ctx := req.Context()
	artifactNS, nsErr := h.artifactNamespaceFor(req)
	if nsErr != nil {
		handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr,
			fmt.Sprintf("upstream unavailable and namespace lookup failed: %v", nsErr))
		return
	}
	out, status, err := h.buildPackument(ctx, req, artifactNS.Package(packageOwningRepo(pkg)), pkg)
	if err != nil {
		if status == http.StatusNotFound {
			handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr, "upstream unavailable, no cached packument")
			return
		}
		handler.WriteError(ctx, w, http.StatusServiceUnavailable, upstreamErr,
			fmt.Sprintf("upstream unavailable and synthesis failed: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) handleTarballGetProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, f *oci.RepoFile) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	if h.tryServeTarballFromRegistry(w, req, scoped, f) {
		return
	}

	pkg, ok := packageFromOwningRepo(f.OwningRepo)
	if !ok {
		handler.WriteError(ctx, w, http.StatusInternalServerError, nil, "invalid package repo")
		return
	}
	version := f.OwningTag
	filename := f.Name

	if h.proxy.negCache != nil {
		key := negativeKey(scoped.Namespace(), pkg, version, filename)
		if h.proxy.negCache.IsKnownMissing(key) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	ref := filter.Ref{Package: pkg, Version: version}
	if decision, denyFilter, err := spec.Proxy.Filters.Decide(ctx, ref); err != nil {
		logger.DebugContext(ctx, "npm proxy filter chain error", "error", err, "filter", filterKind(denyFilter))
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	} else if decision == filter.DecisionDeny {
		logger.InfoContext(ctx, "npm proxy tarball denied by filter", "package", pkg, "version", version, "filter", filterKind(denyFilter))
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	}

	fetcher, err := h.proxy.fetcherFor(spec.Proxy.Upstream)
	if err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		return
	}

	key := scoped.Namespace() + "|" + f.OwningRepo + "|" + version + "|" + filename
	isHead := req.Method == http.MethodHead
	amLeader := false
	var teeRef *tolerantWriter
	resultV, err, _ := h.proxy.fileFlight.Do(key, func() (any, error) {
		amLeader = true
		if hit, hitErr := h.peekTarball(ctx, scoped, f); hitErr == nil && hit {
			return fileFlightResult{cached: true}, nil
		}

		resp, ferr := fetcher.FetchTarball(ctx, pkg, version, filename)
		if ferr != nil {
			return fileFlightResult{}, ferr
		}
		defer resp.Body.Close()

		if decision, denyFilter, derr := spec.Proxy.Filters.Decide(ctx, filter.Ref{
			Package: pkg, Version: version, UploadTime: resp.UploadTime,
		}); derr != nil {
			return fileFlightResult{}, fmt.Errorf("filter error: %w (kind=%s)", derr, filterKind(denyFilter))
		} else if decision == filter.DecisionDeny {
			return fileFlightResult{filterDeny: true, filterName: filterKind(denyFilter)}, nil
		}

		var rewrittenPackument []byte
		packumentContentType := resp.PackumentContentType
		if resp.Packument != nil {
			if body, rerr := proxynpm.RewritePackument(resp.Packument, scoped.Namespace(), pkg); rerr == nil {
				rewrittenPackument = body
			} else {
				logger.DebugContext(ctx, "npm packument rewrite during tarball fill failed", "error", rerr)
			}
		}

		populated := &oci.RepoFile{
			OwningRepo: f.OwningRepo,
			OwningTag:  f.OwningTag,
			Name:       f.Name,
			MediaType:  pickTarballMediaType(resp.ContentType),
			Size:       resp.ContentLength,
		}
		var body io.Reader = resp.Body
		if !isHead {
			w.Header().Set("Content-Type", populated.MediaType)
			if resp.ContentLength > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
			}
			teeRef = &tolerantWriter{w: w}
			body = io.TeeReader(body, teeRef)
		}
		if _, addErr := scoped.AddCachedFile(ctx, populated, body); addErr != nil {
			if errors.Is(addErr, oci.ErrAlreadyExists) {
				return fileFlightResult{cached: true}, nil
			}
			return fileFlightResult{}, addErr
		}
		if len(rewrittenPackument) > 0 {
			h.cachePackument(ctx, scoped.Namespace(), pkg, rewrittenPackument, packumentContentType)
		}
		if len(resp.Version) > 0 {
			metaRF := &oci.RepoFile{
				OwningRepo: f.OwningRepo,
				OwningTag:  f.OwningTag,
				Name:       versionMetaName,
				MediaType:  "application/json",
				Size:       int64(len(resp.Version)),
			}
			if _, addErr := scoped.AddCachedFile(ctx, metaRF, bytes.NewReader(resp.Version)); addErr != nil && !errors.Is(addErr, oci.ErrAlreadyExists) {
				return fileFlightResult{}, addErr
			}
		}
		if err := h.ensureIndexSentinel(ctx, scoped, pkg); err != nil {
			logger.DebugContext(ctx, "npm proxy index sentinel write failed", "error", err)
		}
		return fileFlightResult{streamed: true}, nil
	})
	if err != nil {
		if amLeader && teeRef != nil && teeRef.used {
			logger.WarnContext(ctx, "npm proxy tarball fetch failed after response body started; suppressing error body",
				"error", err, "package", pkg, "version", version, "filename", filename)
			return
		}
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
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "proxy tarball fetch failed")
		}
		return
	}

	result := resultV.(fileFlightResult)
	if result.filterDeny {
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	}
	if result.streamed && amLeader && !isHead {
		return
	}
	if h.tryServeTarballFromRegistry(w, req, scoped, f) {
		return
	}
	handler.WriteError(ctx, w, http.StatusBadGateway, nil, "proxy fill reported success but cache miss on re-read")
}

func (h *Handler) cachePackument(ctx context.Context, ns, pkg string, body []byte, contentType string) {
	if contentType == "" {
		contentType = "application/json"
	}
	if h.proxy.indexCache != nil {
		if err := h.proxy.indexCache.Put(ctx, ns, proxyIndexCacheKey(pkg), body, contentType); err != nil {
			logging.FromContext(ctx).DebugContext(ctx, "npm indexcache put failed", "error", err)
		}
	}
	h.proxy.l1.put(ns+"|"+pkg, l1IndexEntry{body: body, contentType: contentType, fetchedAt: h.proxy.now()})
}

type tolerantWriter struct {
	w    io.Writer
	dead bool
	used bool
}

func (t *tolerantWriter) Write(p []byte) (int, error) {
	t.used = true
	if t.dead {
		return len(p), nil
	}
	if _, err := t.w.Write(p); err != nil {
		t.dead = true
	}
	return len(p), nil
}

type fileFlightResult struct {
	streamed   bool
	cached     bool
	filterDeny bool
	filterName string
}

func (h *Handler) peekTarball(ctx context.Context, scoped *namespace.ScopedRegistry, f *oci.RepoFile) (bool, error) {
	_, rc, err := scoped.ReadFile(ctx, f)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	rc.Close()
	return true, nil
}

func (h *Handler) tryServeTarballFromRegistry(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, f *oci.RepoFile) bool {
	ctx := req.Context()
	logger := logging.FromContext(ctx)
	if req.Method != http.MethodHead {
		if redirectURL, err := scoped.BlobRedirectURL(ctx, f); err == nil && redirectURL != "" {
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return true
		} else if err != nil {
			logger.DebugContext(ctx, "npm tarball redirect probe failed; streaming", "error", err)
		}
	}
	desc, rc, err := scoped.ReadFile(ctx, f)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false
		}
		h.writeRegistryError(ctx, w, err, "failed to read tarball")
		return true
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", desc.File.Size))
	w.Header().Set("X-Checksum-Sha256", desc.File.Digest.String())
	if req.Method == http.MethodHead {
		return true
	}
	if _, err := io.Copy(w, rc); err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "failed to stream tarball")
		return true
	}
	return true
}

func writeProxyBody(w http.ResponseWriter, req *http.Request, body []byte, contentType string) {
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if req.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func writeProxyPackumentBody(w http.ResponseWriter, req *http.Request, body []byte, contentType string) {
	rewritten, err := absolutizePackumentTarballPaths(body, req)
	if err != nil {
		logging.FromContext(req.Context()).DebugContext(req.Context(), "npm proxy response rewrite failed", "error", err)
		writeProxyBody(w, req, body, contentType)
		return
	}
	writeProxyBody(w, req, rewritten, contentType)
}

func absolutizePackumentTarballPaths(body []byte, req *http.Request) ([]byte, error) {
	if req.Host == "" {
		return body, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	versions, ok := doc["versions"].(map[string]any)
	if !ok {
		return body, nil
	}
	base := url.URL{Scheme: detectScheme(req), Host: req.Host}
	for _, rawVersion := range versions {
		versionDoc, ok := rawVersion.(map[string]any)
		if !ok {
			continue
		}
		dist, ok := versionDoc["dist"].(map[string]any)
		if !ok {
			continue
		}
		rawTarball, ok := dist["tarball"].(string)
		if !ok || !strings.HasPrefix(rawTarball, "/") {
			continue
		}
		rel, err := url.Parse(rawTarball)
		if err != nil || rel.IsAbs() || rel.Host != "" {
			continue
		}
		dist["tarball"] = base.ResolveReference(rel).String()
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func negativeKey(ns, pkg, version, filename string) string {
	return "npm|" + ns + "|" + pkg + "|" + version + "|" + filename
}

func packageFromOwningRepo(repo string) (string, bool) {
	pkg, err := parsePackageOwningRepo(repo)
	return pkg, err == nil
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

func pickTarballMediaType(upstream string) string {
	if upstream != "" {
		return upstream
	}
	return "application/octet-stream"
}

func proxyIndexCacheKey(pkg string) string {
	return packageOwningRepo(pkg)
}
