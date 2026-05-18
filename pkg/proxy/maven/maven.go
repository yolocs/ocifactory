// Package maven is the per-format proxy fetcher for Maven 2 layout
// repositories such as Maven Central. It fetches maven-metadata.xml
// and streams artifact files from group/artifact/version paths.
package maven

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/httpclient"
)

const (
	userAgent        = "ocifactory-maven-proxy"
	maxMetadataBytes = 2 << 20
)

// MetadataResponse is the result of a successful maven-metadata.xml
// fetch. Body is the raw upstream XML; Versions and LastUpdated are
// parsed best-effort from <versioning> for the handler's proxy path.
type MetadataResponse struct {
	Body        []byte
	ContentType string
	Versions    []string
	LastUpdated time.Time
}

// FileResponse is the result of a successful [Fetcher.FetchFile] call.
// Body is the upstream stream the caller must close.
type FileResponse struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
}

// Fetcher is a Maven-2-layout upstream client. A Fetcher is safe for
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
// absolute http or https URL, typically https://repo.maven.apache.org/maven2.
func New(upstream *url.URL, opts ...Option) (*Fetcher, error) {
	if upstream == nil {
		return nil, errors.New("maven proxy: upstream URL is required")
	}
	if !upstream.IsAbs() || upstream.Host == "" {
		return nil, fmt.Errorf("maven proxy: upstream %q must be absolute", upstream)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("maven proxy: upstream %q scheme must be http or https", upstream)
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

// GetMetadata fetches <repoPath>/maven-metadata.xml from upstream.
// repoPath is the Maven path form "group/path/artifact" or, for
// snapshot metadata, "group/path/artifact/version-SNAPSHOT".
func (f *Fetcher) GetMetadata(ctx context.Context, repoPath string) (*MetadataResponse, error) {
	if repoPath == "" {
		return nil, fmt.Errorf("maven proxy: GetMetadata: repo path is required")
	}
	u, err := f.mavenURL(repoPath, "maven-metadata.xml")
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Get(ctx, u, httpclient.GetOptions{
		Header: http.Header{"Accept": []string{"application/xml, text/xml, */*;q=0.1"}},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("maven proxy: metadata %s: %w", repoPath, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("maven proxy: metadata %s: status %d: %w",
			repoPath, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}
	body, err := readLimited(resp.Body, maxMetadataBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return nil, fmt.Errorf("maven proxy: metadata %s exceeds %d bytes: %w", repoPath, maxMetadataBytes, proxy.ErrUpstreamMalformed)
		}
		return nil, fmt.Errorf("maven proxy: read metadata %s: %w", repoPath, proxy.ErrUpstreamUnavailable)
	}
	versions, lastUpdated, err := parseMetadata(body)
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/xml"
	}
	return &MetadataResponse{
		Body:        body,
		ContentType: ct,
		Versions:    versions,
		LastUpdated: lastUpdated,
	}, nil
}

// FetchFile opens a streaming GET for
// <repoPath>/<version>/<filename>. The returned Body must be closed.
func (f *Fetcher) FetchFile(ctx context.Context, repoPath, version, filename string) (*FileResponse, error) {
	if repoPath == "" || version == "" || filename == "" {
		return nil, fmt.Errorf("maven proxy: FetchFile: repo path, version, and filename are required")
	}
	return f.fetchPath(ctx, repoPath, version, filename)
}

// FetchPath opens a streaming GET for <repoPath>/<filename>. It is
// used for Maven metadata checksum sidecars, which live next to
// maven-metadata.xml rather than under a version directory.
func (f *Fetcher) FetchPath(ctx context.Context, repoPath, filename string) (*FileResponse, error) {
	if repoPath == "" || filename == "" {
		return nil, fmt.Errorf("maven proxy: FetchPath: repo path and filename are required")
	}
	return f.fetchPath(ctx, repoPath, filename)
}

func (f *Fetcher) fetchPath(ctx context.Context, parts ...string) (*FileResponse, error) {
	u, err := f.mavenURL(parts...)
	if err != nil {
		return nil, err
	}
	path := strings.Join(parts, "/")
	resp, err := f.client.Get(ctx, u, httpclient.GetOptions{})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("maven proxy: file %s: %w", path, proxy.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("maven proxy: file %s: status %d: %w",
			path, resp.StatusCode, proxy.ErrUpstreamUnavailable)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	var size int64 = -1
	if s := resp.Header.Get("Content-Length"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
			size = n
		}
	}
	return &FileResponse{Body: resp.Body, ContentType: ct, ContentLength: size}, nil
}

func (f *Fetcher) mavenURL(parts ...string) (string, error) {
	segments := make([]string, 0, len(parts)*2)
	for _, part := range parts {
		if part == "" {
			return "", errors.New("maven proxy: empty path segment")
		}
		for _, s := range strings.Split(part, "/") {
			if s == "" || s == "." || s == ".." {
				return "", fmt.Errorf("maven proxy: invalid path segment %q", s)
			}
			segments = append(segments, s)
		}
	}
	return f.upstream.JoinPath(segments...).String(), nil
}

type metadataXML struct {
	Versioning struct {
		Versions struct {
			Version []string `xml:"version"`
		} `xml:"versions"`
		LastUpdated string `xml:"lastUpdated"`
	} `xml:"versioning"`
}

func parseMetadata(body []byte) ([]string, time.Time, error) {
	var doc metadataXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, time.Time{}, fmt.Errorf("maven proxy: decode metadata: %v: %w", err, proxy.ErrUpstreamMalformed)
	}
	versions := append([]string(nil), doc.Versioning.Versions.Version...)
	lastUpdated, err := parseMavenTime(doc.Versioning.LastUpdated)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("maven proxy: decode metadata lastUpdated %q: %v: %w",
			doc.Versioning.LastUpdated, err, proxy.ErrUpstreamMalformed)
	}
	return versions, lastUpdated, nil
}

func parseMavenTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	layouts := []string{"20060102150405", "200601021504", "20060102"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("unsupported timestamp format")
}

var errBodyTooLarge = errors.New("body too large")

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}
