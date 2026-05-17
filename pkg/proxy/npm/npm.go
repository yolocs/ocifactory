// Package npm is the per-format proxy fetcher for the npm registry
// HTTP API. It fetches packument JSON (`GET /<pkg>`) and resolves
// tarball URLs from that packument before streaming the file body.
package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/httpclient"
)

const userAgent = "ocifactory-npm-proxy"

// PackumentResponse is the result of a successful upstream packument
// fetch.
type PackumentResponse struct {
	Body        []byte
	ContentType string
}

// TarballResponse is the result of a successful [Fetcher.FetchTarball]
// call. Body is the upstream stream the caller must close.
type TarballResponse struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64

	// Packument carries the raw upstream package metadata fetched to
	// resolve the tarball URL. The handler rewrites and caches it as a
	// free side effect of a file cache miss.
	Packument            []byte
	PackumentContentType string

	// Version is the raw `versions[version]` JSON object from the
	// packument. The handler stores it as package.json alongside the
	// cached tarball so packument synthesis can work while upstream is
	// degraded.
	Version json.RawMessage

	// UploadTime is parsed from packument `time[version]`, when
	// present, and feeds metadata-dependent proxy filters.
	UploadTime time.Time
}

// Fetcher is an npm-shaped upstream client. A Fetcher is safe for
// concurrent use.
type Fetcher struct {
	upstream *url.URL
	client   *httpclient.Client
}

// Option configures a [Fetcher].
type Option func(*config)

type config struct {
	client *httpclient.Client
}

// WithClient overrides the shared HTTP client. Defaults to a fresh
// [httpclient.New] with package defaults.
func WithClient(c *httpclient.Client) Option {
	return func(o *config) { o.client = c }
}

// New constructs a Fetcher that talks to upstream. upstream must be an
// absolute http or https URL, typically https://registry.npmjs.org.
func New(upstream *url.URL, opts ...Option) (*Fetcher, error) {
	if upstream == nil {
		return nil, errors.New("npm proxy: upstream URL is required")
	}
	if !upstream.IsAbs() || upstream.Host == "" {
		return nil, fmt.Errorf("npm proxy: upstream %q must be absolute", upstream)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("npm proxy: upstream %q scheme must be http or https", upstream)
	}

	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.client == nil {
		cfg.client = httpclient.New(httpclient.Options{UserAgent: userAgent})
	}
	u := *upstream
	u.Path = strings.TrimRight(u.Path, "/")
	return &Fetcher{upstream: &u, client: cfg.client}, nil
}

// Upstream returns the canonical upstream URL the fetcher talks to.
func (f *Fetcher) Upstream() *url.URL { return f.upstream }

// GetPackument fetches upstream `GET /<pkg>`.
func (f *Fetcher) GetPackument(ctx context.Context, pkg string) (*PackumentResponse, error) {
	if pkg == "" {
		return nil, fmt.Errorf("npm proxy: GetPackument: package is required")
	}
	u := f.packageURL(pkg)
	resp, err := f.client.Get(ctx, u, httpclient.GetOptions{
		Header: http.Header{
			"Accept": []string{"application/vnd.npm.install-v1+json, application/json"},
		},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("npm proxy: packument %s: %w", pkg, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("npm proxy: packument %s: status %d: %w",
			pkg, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("npm proxy: read packument %s: %w", pkg, proxy.ErrUpstreamUnavailable)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	return &PackumentResponse{Body: body, ContentType: ct}, nil
}

// FetchTarball resolves filename through the upstream packument, then
// fetches the tarball URL advertised for that version.
func (f *Fetcher) FetchTarball(ctx context.Context, pkg, version, filename string) (*TarballResponse, error) {
	if pkg == "" || version == "" || filename == "" {
		return nil, fmt.Errorf("npm proxy: FetchTarball: package, version, and filename are required")
	}
	packument, err := f.GetPackument(ctx, pkg)
	if err != nil {
		return nil, err
	}
	meta, err := decodePackument(packument.Body, pkg)
	if err != nil {
		return nil, err
	}
	versionRaw, ok := meta.Versions[version]
	if !ok {
		return nil, fmt.Errorf("npm proxy: %s@%s missing from packument: %w", pkg, version, proxy.ErrNotFound)
	}
	var versionDoc npmVersion
	if err := json.Unmarshal(versionRaw, &versionDoc); err != nil {
		return nil, fmt.Errorf("npm proxy: decode %s@%s version metadata: %v: %w",
			pkg, version, err, proxy.ErrUpstreamMalformed)
	}
	if versionDoc.Dist.Tarball == "" {
		return nil, fmt.Errorf("npm proxy: %s@%s missing dist.tarball: %w", pkg, version, proxy.ErrUpstreamMalformed)
	}
	tarballURL, err := url.Parse(versionDoc.Dist.Tarball)
	if err != nil || !tarballURL.IsAbs() || tarballURL.Host == "" {
		return nil, fmt.Errorf("npm proxy: %s@%s invalid dist.tarball %q: %w",
			pkg, version, versionDoc.Dist.Tarball, proxy.ErrUpstreamMalformed)
	}
	if got := path.Base(tarballURL.Path); got != filename {
		return nil, fmt.Errorf("npm proxy: %s@%s tarball filename %q not in packument (got %q): %w",
			pkg, version, filename, got, proxy.ErrNotFound)
	}

	resp, err := f.client.Get(ctx, tarballURL.String(), httpclient.GetOptions{})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("npm proxy: tarball %s@%s %s: %w", pkg, version, filename, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("npm proxy: tarball %s@%s %s: status %d: %w",
			pkg, version, filename, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	var size int64
	if s := resp.Header.Get("Content-Length"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			size = n
		}
	}
	return &TarballResponse{
		Body:                 resp.Body,
		ContentType:          ct,
		ContentLength:        size,
		Packument:            packument.Body,
		PackumentContentType: packument.ContentType,
		Version:              append(json.RawMessage(nil), versionRaw...),
		UploadTime:           meta.UploadTime(version),
	}, nil
}

func (f *Fetcher) packageURL(pkg string) string {
	u := *f.upstream
	if strings.HasPrefix(pkg, "@") {
		scope, name, ok := strings.Cut(pkg, "/")
		if ok {
			return u.JoinPath(scope, name).String()
		}
	}
	return u.JoinPath(pkg).String()
}

type npmPackument struct {
	Name     string                     `json:"name"`
	Versions map[string]json.RawMessage `json:"versions"`
	Time     map[string]string          `json:"time"`
}

func (p *npmPackument) UploadTime(version string) time.Time {
	if p == nil || p.Time == nil {
		return time.Time{}
	}
	raw := p.Time[version]
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

type npmVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball string `json:"tarball"`
	} `json:"dist"`
}

func decodePackument(body []byte, pkg string) (*npmPackument, error) {
	var p npmPackument
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("npm proxy: decode packument %s: %v: %w", pkg, err, proxy.ErrUpstreamMalformed)
	}
	if p.Name == "" {
		return nil, fmt.Errorf("npm proxy: packument %s missing name: %w", pkg, proxy.ErrUpstreamMalformed)
	}
	if p.Versions == nil {
		return nil, fmt.Errorf("npm proxy: packument %s missing versions: %w", pkg, proxy.ErrUpstreamMalformed)
	}
	return &p, nil
}
