// Package python is the per-format proxy fetcher for PyPI. It speaks
// two upstream APIs:
//
//   - PEP 503 simple HTML index — passthrough for /simple/ and
//     /simple/<pkg>/ requests, with absolute file URLs rewritten so
//     pip routes file downloads back through ocifactory.
//   - PyPI JSON API (/pypi/<pkg>/<version>/json) — used on a file
//     cache miss to resolve the upstream file URL and upload_time
//     before committing to the upstream byte transfer.
//
// File downloads themselves stream straight from the URL the JSON API
// pointed at (typically files.pythonhosted.org). The fetcher does not
// know about OCI: it returns response bodies and the caller (the
// python handler's proxy path) tees them through
// [pkg/namespace.ScopedRegistry.AddFile] while serving the client.
//
// There is no shared Fetcher interface — see the design issue
// (#118) for why. The handler holds a concrete *Fetcher and calls the
// methods this package exposes.
package python

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/httpclient"
)

const (
	// DefaultVersionMetadataCacheTTL is how long a successful
	// [Fetcher.GetVersionMetadata] result stays in the per-fetcher
	// in-memory cache. Sized for pip's typical resolution burst —
	// when a single install lands several files of the same version
	// in quick succession, only the first call hits upstream.
	DefaultVersionMetadataCacheTTL = 60 * time.Second

	// DefaultVersionMetadataCacheSize bounds the number of
	// (package, version) metadata entries the per-fetcher cache
	// retains. Generous so a heavy resolution burst doesn't evict
	// itself; bounded so memory is predictable.
	DefaultVersionMetadataCacheSize = 4096
)

// IndexResponse is the result of a successful index fetch.
//
// Body is the raw upstream payload — callers rewrite it before
// serving and (typically) hand the same bytes to
// [pkg/proxy/indexcache.Cache.Put] for the on-OCI cache. ContentType
// is the upstream Content-Type header verbatim so the cache and the
// client see the same value (PyPI emits both
// `text/html; charset=utf-8` and
// `application/vnd.pypi.simple.v1+html`).
type IndexResponse struct {
	Body        []byte
	ContentType string
}

// VersionMetadata is the subset of PyPI's JSON API response the file
// proxy needs.
type VersionMetadata struct {
	// Package is the upstream-canonical (not necessarily PEP 503
	// normalized) package name from the JSON API's `info.name`. The
	// caller uses it as the source of truth when assembling the
	// version manifest's owning repo segment.
	Package string

	// Version is the version this metadata describes. Echoed back
	// from the request so callers don't need to re-thread it.
	Version string

	// Files lists every distribution PyPI advertises for this
	// version (wheel, sdist, .whl.metadata sidecars when present).
	// The order is whatever PyPI returned; callers match on
	// Filename to find the one the client requested.
	Files []FileMetadata

	// UploadTime is the publication time of the version, picked as
	// the earliest upload_time across [Files]. Zero when none of
	// the upstream entries carried a parseable timestamp.
	UploadTime time.Time
}

// FileMetadata identifies one distribution file under a version.
type FileMetadata struct {
	Filename string
	URL      string
	SHA256   string
	Size     int64

	// UploadTime is the per-file publication timestamp from PyPI's
	// JSON `upload_time_iso_8601`. Zero when missing or unparseable.
	UploadTime time.Time
}

// FileResponse is the result of a successful [Fetcher.FetchFile] call.
// Body is the upstream stream the caller MUST close.
type FileResponse struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
}

// Fetcher is the PyPI-shaped upstream client.
//
// A Fetcher is safe for concurrent use. One per upstream URL is the
// typical wiring; the python handler caches Fetchers keyed by
// (namespace, upstream) when different namespaces point at different
// upstreams.
type Fetcher struct {
	upstream *url.URL
	client   *httpclient.Client
	metaTTL  time.Duration
	metaLRU  *expirable.LRU[string, *VersionMetadata]
	now      func() time.Time
}

// Option configures a [Fetcher].
type Option func(*config)

type config struct {
	client        *httpclient.Client
	metaCacheSize int
	metaCacheTTL  time.Duration
	now           func() time.Time
}

// WithClient overrides the shared HTTP client. Defaults to a fresh
// [httpclient.New] with the package defaults.
func WithClient(c *httpclient.Client) Option {
	return func(o *config) { o.client = c }
}

// WithVersionMetadataCache tunes the in-memory cache size and TTL the
// fetcher uses to memoise successful [Fetcher.GetVersionMetadata]
// results. A non-positive size or TTL falls back to the defaults.
func WithVersionMetadataCache(size int, ttl time.Duration) Option {
	return func(o *config) {
		o.metaCacheSize = size
		o.metaCacheTTL = ttl
	}
}

// withNow overrides the wall clock used to stamp cache-miss decisions.
// Test seam; not exported.
func withNow(now func() time.Time) Option {
	return func(o *config) { o.now = now }
}

// New constructs a [Fetcher] that talks to upstream. upstream must be
// an absolute http or https URL — the PyPI canonical value is
// `https://pypi.org`.
func New(upstream *url.URL, opts ...Option) (*Fetcher, error) {
	if upstream == nil {
		return nil, errors.New("python proxy: upstream URL is required")
	}
	if !upstream.IsAbs() || upstream.Host == "" {
		return nil, fmt.Errorf("python proxy: upstream %q must be absolute", upstream)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("python proxy: upstream %q scheme must be http or https", upstream)
	}

	cfg := config{
		metaCacheSize: DefaultVersionMetadataCacheSize,
		metaCacheTTL:  DefaultVersionMetadataCacheTTL,
		now:           func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.client == nil {
		cfg.client = httpclient.New(httpclient.Options{UserAgent: "ocifactory-pypi-proxy"})
	}
	if cfg.metaCacheSize <= 0 {
		cfg.metaCacheSize = DefaultVersionMetadataCacheSize
	}
	if cfg.metaCacheTTL <= 0 {
		cfg.metaCacheTTL = DefaultVersionMetadataCacheTTL
	}
	// Strip a trailing slash on the upstream so path joins don't
	// double-slash when the operator wrote "https://pypi.org/".
	u := *upstream
	u.Path = strings.TrimRight(u.Path, "/")

	return &Fetcher{
		upstream: &u,
		client:   cfg.client,
		metaTTL:  cfg.metaCacheTTL,
		metaLRU:  expirable.NewLRU[string, *VersionMetadata](cfg.metaCacheSize, nil, cfg.metaCacheTTL),
		now:      cfg.now,
	}, nil
}

// Upstream returns the canonical upstream URL the fetcher talks to.
// Useful for log lines and caching keys.
func (f *Fetcher) Upstream() *url.URL { return f.upstream }

// GetTopLevelIndex fetches the upstream `/simple/` index. The response
// body is PEP 503 HTML listing every project on the upstream — for
// real PyPI this is megabytes, so callers are expected to cache it.
func (f *Fetcher) GetTopLevelIndex(ctx context.Context) (*IndexResponse, error) {
	u := f.upstream.JoinPath("simple").String() + "/"
	return f.getIndex(ctx, u)
}

// GetSimpleIndex fetches the per-package simple index. pkg is the
// PEP 503 normalized name (lowercased, runs of `-_.` collapsed to a
// single `-`); upstream redirects from the un-normalized name are
// followed by the shared HTTP client.
func (f *Fetcher) GetSimpleIndex(ctx context.Context, pkg string) (*IndexResponse, error) {
	if pkg == "" {
		return nil, fmt.Errorf("python proxy: GetSimpleIndex: package is required")
	}
	u := f.upstream.JoinPath("simple", pkg).String() + "/"
	return f.getIndex(ctx, u)
}

func (f *Fetcher) getIndex(ctx context.Context, rawURL string) (*IndexResponse, error) {
	resp, err := f.client.Get(ctx, rawURL, httpclient.GetOptions{
		// Accept HTML — PyPI honours both legacy and PEP 691 media
		// types. We pass them all and take whatever the upstream
		// chose, since we just relay bytes downstream.
		Header: http.Header{
			"Accept": []string{
				"application/vnd.pypi.simple.v1+html, text/html;q=0.9, */*;q=0.1",
			},
		},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("python proxy: index %s: %w", rawURL, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("python proxy: index %s: unexpected status %d: %w",
			rawURL, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("python proxy: read index %s: %w", rawURL, proxy.ErrUpstreamUnavailable)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/html"
	}
	return &IndexResponse{Body: body, ContentType: ct}, nil
}

// GetVersionMetadata fetches PyPI's JSON metadata for a specific
// version. Results are memoised in-process for
// [DefaultVersionMetadataCacheTTL] so a resolution burst hitting
// several files of the same version pays for one upstream call.
//
// Returns [proxy.ErrNotFound] when upstream reports the package or
// version doesn't exist; [proxy.ErrUpstreamUnavailable] for retries
// exhausted; [proxy.ErrUpstreamMalformed] when the response can't be
// decoded against the expected shape.
func (f *Fetcher) GetVersionMetadata(ctx context.Context, pkg, version string) (*VersionMetadata, error) {
	if pkg == "" || version == "" {
		return nil, fmt.Errorf("python proxy: GetVersionMetadata: package and version are required")
	}
	key := pkg + "@" + version
	if v, ok := f.metaLRU.Get(key); ok {
		return v, nil
	}

	u := f.upstream.JoinPath("pypi", pkg, version, "json").String()
	resp, err := f.client.Get(ctx, u, httpclient.GetOptions{
		Header: http.Header{"Accept": []string{"application/json"}},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("python proxy: metadata %s %s: %w", pkg, version, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("python proxy: metadata %s %s: status %d: %w",
			pkg, version, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("python proxy: metadata %s %s: read: %w", pkg, version, proxy.ErrUpstreamUnavailable)
	}

	meta, err := decodeVersionMetadata(body, pkg, version)
	if err != nil {
		return nil, err
	}
	f.metaLRU.Add(key, meta)
	return meta, nil
}

// FetchFile opens a streaming GET against rawURL. The caller is
// expected to have obtained rawURL from a prior
// [Fetcher.GetVersionMetadata] response (or a rewritten simple-index
// anchor) so the URL has already been validated against the upstream
// surface.
//
// The returned [FileResponse.Body] MUST be closed by the caller —
// closing also releases the underlying per-request timeout.
func (f *Fetcher) FetchFile(ctx context.Context, rawURL string) (*FileResponse, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("python proxy: FetchFile: url is required")
	}
	resp, err := f.client.Get(ctx, rawURL, httpclient.GetOptions{})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("python proxy: file %s: %w", rawURL, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("python proxy: file %s: status %d: %w",
			rawURL, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}
	contentType := resp.Header.Get("Content-Type")
	contentLength := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		// Best-effort: a missing or malformed Content-Length isn't
		// fatal — the OCI streaming push will measure as it goes.
		// proxy.ErrUpstreamMalformed is reserved for shape errors
		// the caller couldn't recover from.
		var n int64
		if _, scanErr := fmt.Sscanf(cl, "%d", &n); scanErr == nil && n >= 0 {
			contentLength = n
		}
	}
	return &FileResponse{
		Body:          resp.Body,
		ContentType:   contentType,
		ContentLength: contentLength,
	}, nil
}
