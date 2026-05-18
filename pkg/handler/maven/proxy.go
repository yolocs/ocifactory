package maven

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
	proxymaven "github.com/yolocs/ocifactory/pkg/proxy/maven"
)

var errProxyBodyTooLarge = errors.New("maven proxy body exceeds size cap")

// ProxyFetcher is the subset of [*pkg/proxy/maven.Fetcher] methods
// the handler depends on. It exists so tests can inject a fake upstream
// boundary; it is not an out-of-tree extension surface.
type ProxyFetcher interface {
	GetMetadata(ctx context.Context, repoPath string) (*proxymaven.MetadataResponse, error)
	FetchFile(ctx context.Context, repoPath, version, filename string) (*proxymaven.FileResponse, error)
	FetchPath(ctx context.Context, repoPath, filename string) (*proxymaven.FileResponse, error)
}

// FetcherFactory builds a [ProxyFetcher] for a namespace upstream URL.
type FetcherFactory func(upstream *url.URL) (ProxyFetcher, error)

// DefaultFetcherFactory builds the production Maven proxy fetcher.
func DefaultFetcherFactory(upstream *url.URL) (ProxyFetcher, error) {
	return proxymaven.New(upstream)
}

// WithFetcherFactory installs a custom Maven proxy fetcher factory.
func WithFetcherFactory(f FetcherFactory) Option {
	return func(c *handlerConfig) { c.fetcherFactory = f }
}

// WithProxyNegativeCache wires the in-memory negative cache for
// repeated artifact misses.
func WithProxyNegativeCache(c *indexcache.NegativeCache) Option {
	return func(cg *handlerConfig) { cg.negCache = c }
}

type proxyState struct {
	factory    FetcherFactory
	negCache   *indexcache.NegativeCache
	fetchers   sync.Map
	fileFlight singleflight.Group
}

func newProxyState(cfg handlerConfig) *proxyState {
	factory := cfg.fetcherFactory
	if factory == nil {
		factory = DefaultFetcherFactory
	}
	return &proxyState{factory: factory, negCache: cfg.negCache}
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

func (h *Handler) handleMetadataProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, repoPath string) {
	ctx := req.Context()
	if err := scoped.Authorize(ctx, auth.OpRead); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "namespace authorization failed")
		return
	}

	fetcher, err := h.proxy.fetcherFor(spec.Proxy.Upstream)
	if err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		return
	}
	resp, err := fetcher.GetMetadata(ctx, repoPath)
	if err != nil {
		switch {
		case errors.Is(err, proxy.ErrNotFound):
			http.Error(w, "metadata not found", http.StatusNotFound)
		case errors.Is(err, proxy.ErrUpstreamMalformed):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
		case errors.Is(err, proxy.ErrUpstreamUnavailable):
			handler.WriteError(ctx, w, http.StatusServiceUnavailable, err, "upstream unavailable")
		default:
			handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy metadata fetch failed")
		}
		return
	}
	writeProxyBytes(w, req, resp.Body, resp.ContentType)
}

func (h *Handler) handleMetadataSidecarProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, f *oci.RepoFile, repoPath string) {
	ctx := req.Context()

	if h.tryServeFromRegistry(w, req, scoped, f) {
		return
	}
	fetcher, err := h.proxy.fetcherFor(spec.Proxy.Upstream)
	if err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "proxy fetcher unavailable")
		return
	}

	key := scoped.Namespace() + "|" + f.OwningRepo + "|" + f.OwningTag + "|" + f.Name
	isHead := req.Method == http.MethodHead
	amLeader := false
	var teeRef *tolerantWriter
	resultV, err, _ := h.proxy.fileFlight.Do(key, func() (any, error) {
		amLeader = true
		if hit, hitErr := h.peekRegistry(ctx, scoped, f); hitErr == nil && hit {
			return fileFlightResult{cached: true}, nil
		}
		fileResp, ferr := fetcher.FetchPath(ctx, repoPath, f.Name)
		if ferr != nil {
			return fileFlightResult{}, ferr
		}
		defer fileResp.Body.Close()

		populated := &oci.RepoFile{
			OwningRepo:     f.OwningRepo,
			OwningTag:      f.OwningTag,
			Name:           f.Name,
			MediaType:      pickMediaType(f.Name, fileResp.ContentType),
			Size:           fileResp.ContentLength,
			AllowOverwrite: f.AllowOverwrite,
		}
		body, err := h.limitedProxyBody(fileResp.Body, fileResp.ContentLength)
		if err != nil {
			return fileFlightResult{}, err
		}
		if !isHead {
			w.Header().Set("Content-Type", populated.MediaType)
			if fileResp.ContentLength > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(fileResp.ContentLength, 10))
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
		return fileFlightResult{streamed: true}, nil
	})
	if err != nil {
		h.writeProxyFileError(w, req, err, amLeader, teeRef, packageRef(upstreamRepoPath(f.OwningRepo)), f.OwningTag, f.Name)
		return
	}

	result := resultV.(fileFlightResult)
	if result.streamed && amLeader && !isHead {
		return
	}
	if h.tryServeFromRegistry(w, req, scoped, f) {
		return
	}
	handler.WriteError(ctx, w, http.StatusBadGateway, nil, "proxy fill reported success but cache miss on re-read")
}

func (h *Handler) handleFileGetProxy(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, spec *namespace.Spec, f *oci.RepoFile) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	if h.tryServeFromRegistry(w, req, scoped, f) {
		return
	}

	repoPath := upstreamRepoPath(f.OwningRepo)
	pkg := packageRef(repoPath)
	version := f.OwningTag
	filename := f.Name
	if h.proxy.negCache != nil {
		key := negativeKey(scoped.Namespace(), f.OwningRepo, version, filename)
		if h.proxy.negCache.IsKnownMissing(key) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	ref := filter.Ref{Package: pkg, Version: version}
	if decision, denyFilter, err := spec.Proxy.Filters.Decide(ctx, ref); err != nil {
		logger.DebugContext(ctx, "maven proxy filter chain error", "error", err, "filter", filterKind(denyFilter))
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	} else if decision == filter.DecisionDeny {
		logger.InfoContext(ctx, "maven proxy file denied by filter", "package", pkg, "version", version, "filter", filterKind(denyFilter))
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
		if hit, hitErr := h.peekRegistry(ctx, scoped, f); hitErr == nil && hit {
			return fileFlightResult{cached: true}, nil
		}

		meta, ferr := fetcher.GetMetadata(ctx, repoPath)
		if ferr != nil {
			return fileFlightResult{}, ferr
		}
		if len(meta.Versions) > 0 && !contains(meta.Versions, version) {
			return fileFlightResult{}, fmt.Errorf("maven proxy: version %s missing from metadata for %s: %w",
				version, repoPath, proxy.ErrNotFound)
		}
		if decision, denyFilter, derr := spec.Proxy.Filters.Decide(ctx, filter.Ref{
			Package: pkg, Version: version, UploadTime: meta.LastUpdated,
		}); derr != nil {
			return fileFlightResult{}, fmt.Errorf("filter error: %w (kind=%s)", derr, filterKind(denyFilter))
		} else if decision == filter.DecisionDeny {
			return fileFlightResult{filterDeny: true, filterName: filterKind(denyFilter)}, nil
		} else if decision == filter.DecisionNeedsMoreData {
			return fileFlightResult{filterDeny: true, filterName: filterKind(denyFilter)}, nil
		}

		fileResp, ferr := fetcher.FetchFile(ctx, repoPath, version, filename)
		if ferr != nil {
			return fileFlightResult{}, ferr
		}
		defer fileResp.Body.Close()
		body, berr := h.limitedProxyBody(fileResp.Body, fileResp.ContentLength)
		if berr != nil {
			return fileFlightResult{}, berr
		}

		populated := &oci.RepoFile{
			OwningRepo: f.OwningRepo,
			OwningTag:  f.OwningTag,
			Name:       f.Name,
			MediaType:  pickMediaType(filename, fileResp.ContentType),
			Size:       fileResp.ContentLength,
			// Snapshot artifacts are mutable in hosted mode, but a
			// pull-through cache fill should keep the exact upstream
			// bytes it first observed unless the operator enables
			// backend overwrites globally.
			AllowOverwrite: false,
		}
		if !isHead {
			w.Header().Set("Content-Type", populated.MediaType)
			if fileResp.ContentLength > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(fileResp.ContentLength, 10))
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
		return fileFlightResult{streamed: true}, nil
	})
	if err != nil {
		if amLeader && teeRef != nil && teeRef.used {
			logger.WarnContext(ctx, "maven proxy file fetch failed after response body started; suppressing error body",
				"error", err, "package", pkg, "version", version, "filename", filename)
			return
		}
		switch {
		case errors.Is(err, proxy.ErrNotFound):
			if h.proxy.negCache != nil {
				h.proxy.negCache.RecordMiss(negativeKey(scoped.Namespace(), f.OwningRepo, version, filename))
			}
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, proxy.ErrUpstreamMalformed):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
		case errors.Is(err, proxy.ErrUpstreamUnavailable):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream unavailable")
		case errors.Is(err, errProxyBodyTooLarge):
			handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream body too large")
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
		logger.InfoContext(ctx, "maven proxy file denied by filter", "package", pkg, "version", version, "filter", result.filterName)
		http.Error(w, "filter denied", http.StatusNotFound)
		return
	}
	if result.streamed && amLeader && !isHead {
		return
	}
	if h.tryServeFromRegistry(w, req, scoped, f) {
		return
	}
	handler.WriteError(ctx, w, http.StatusBadGateway, nil, "proxy fill reported success but cache miss on re-read")
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

func (h *Handler) limitedProxyBody(r io.Reader, contentLength int64) (io.Reader, error) {
	limit := h.maxUploadBytes
	if limit <= 0 {
		limit = DefaultMaxUploadBytes
	}
	if contentLength > limit {
		return nil, fmt.Errorf("upstream content length %d exceeds %d-byte cap: %w", contentLength, limit, errProxyBodyTooLarge)
	}
	return &proxyLimitReader{r: r, remaining: limit}, nil
}

type proxyLimitReader struct {
	r         io.Reader
	remaining int64
}

func (r *proxyLimitReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		var one [1]byte
		n, err := r.r.Read(one[:])
		if n > 0 {
			return 0, errProxyBodyTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (h *Handler) writeProxyFileError(w http.ResponseWriter, req *http.Request, err error, amLeader bool, teeRef *tolerantWriter, pkg, version, filename string) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)
	if amLeader && teeRef != nil && teeRef.used {
		logger.WarnContext(ctx, "maven proxy file fetch failed after response body started; suppressing error body",
			"error", err, "package", pkg, "version", version, "filename", filename)
		return
	}
	switch {
	case errors.Is(err, proxy.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, proxy.ErrUpstreamMalformed):
		handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream malformed")
	case errors.Is(err, proxy.ErrUpstreamUnavailable):
		handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream unavailable")
	case errors.Is(err, errProxyBodyTooLarge):
		handler.WriteError(ctx, w, http.StatusBadGateway, err, "upstream body too large")
	default:
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusBadGateway, err, "proxy file fetch failed")
	}
}

func (h *Handler) peekRegistry(ctx context.Context, scoped *namespace.ScopedRegistry, f *oci.RepoFile) (bool, error) {
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

func (h *Handler) tryServeFromRegistry(w http.ResponseWriter, req *http.Request, scoped *namespace.ScopedRegistry, f *oci.RepoFile) bool {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	if req.Method != http.MethodHead {
		if redirectURL, err := scoped.BlobRedirectURL(ctx, f); err == nil && redirectURL != "" {
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return true
		} else if err != nil {
			logger.DebugContext(ctx, "maven proxy redirect probe failed; streaming", "error", err)
		}
	}
	desc, rc, err := scoped.ReadFile(ctx, f)
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
	defer rc.Close()

	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(desc.File.Size, 10))
	w.Header().Set("X-Checksum-Sha256", desc.File.Digest.String())
	if req.Method == http.MethodHead {
		return true
	}
	if _, err := io.Copy(w, rc); err != nil {
		logger.DebugContext(ctx, "copy maven proxy hit body", "error", err)
	}
	return true
}

func writeProxyBytes(w http.ResponseWriter, req *http.Request, body []byte, contentType string) {
	if contentType == "" {
		contentType = "text/xml"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if req.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func pickMediaType(filename, upstream string) string {
	if upstream != "" {
		return upstream
	}
	return detectMediaType(filename)
}

func negativeKey(ns, repo, version, filename string) string {
	return "maven|" + ns + "|" + repo + "|" + version + "|" + filename
}

func packageRef(repo string) string {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	if len(parts) < 2 {
		return repo
	}
	artifact := parts[len(parts)-1]
	groupID := strings.Join(parts[:len(parts)-1], ".")
	return groupID + ":" + artifact
}

func upstreamRepoPath(owningRepo string) string {
	return repoPartsFromOwningRepo(owningRepo)
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
