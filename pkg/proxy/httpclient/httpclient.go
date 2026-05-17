// Package httpclient is the shared HTTP client per-format proxy
// fetchers under pkg/proxy/<format> compose. A Client applies a
// per-request timeout, bounded retries with exponential backoff on
// transient upstream failures, conditional GET (ETag /
// Last-Modified), and a hard redirect cap.
//
// Error classification lives here so per-format fetchers don't each
// reinvent it:
//
//   - 304 Not Modified surfaces as Response.NotModified == true,
//     never as an error.
//   - Network errors, per-request timeouts, and 5xx responses
//     (after the retry budget is exhausted) surface as
//     proxy.ErrUpstreamUnavailable.
//   - Redirect-cap and other transport configuration failures
//     surface as proxy.ErrUpstreamMalformed.
//   - 4xx responses (other than 304) surface as a Response with the
//     body intact; the per-format fetcher decides which codes map
//     to proxy.ErrNotFound and which to other format-specific
//     sentinels.
//   - Caller-side cancellation (ctx.Err() != nil before any
//     upstream call) surfaces unwrapped so errors.Is(err,
//     context.Canceled) is true without errors.Is(err,
//     proxy.ErrUpstreamUnavailable).
//
// A Client is safe for concurrent use by multiple goroutines.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http2"

	"github.com/yolocs/ocifactory/pkg/proxy"
)

const (
	// DefaultTimeout is the per-request timeout applied to each
	// individual attempt (original call and every retry).
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the number of additional attempts after a
	// retryable failure. Total request count is 1 + MaxRetries.
	DefaultMaxRetries = 3

	// DefaultMaxRedirects caps the redirect chain. Beyond this the
	// client surfaces proxy.ErrUpstreamMalformed.
	DefaultMaxRedirects = 10

	// DefaultInitialBackoff is the wait before the first retry.
	DefaultInitialBackoff = 250 * time.Millisecond

	// DefaultMaxBackoff caps exponential backoff growth.
	DefaultMaxBackoff = 5 * time.Second
)

// Credential is the optional upstream credential a Client attaches
// to each request. v1 leaves the credential UNUSED — the field
// exists on Options so adding authenticated upstreams later does
// not reshape the constructor.
type Credential struct {
	Username    string
	Password    string
	BearerToken string
}

// IsZero reports whether c carries no credential material.
func (c Credential) IsZero() bool {
	return c == Credential{}
}

// Options configures a Client.
type Options struct {
	// Timeout is the per-request timeout applied to each attempt.
	// Zero falls back to DefaultTimeout.
	Timeout time.Duration

	// MaxRetries bounds retry attempts on transient failures.
	// Zero falls back to DefaultMaxRetries; a negative value
	// disables retries entirely. 4xx is never retried.
	MaxRetries int

	// MaxRedirects caps the redirect chain. Zero falls back to
	// DefaultMaxRedirects; a negative value disables redirect
	// following (the redirect response is returned as-is).
	MaxRedirects int

	// InitialBackoff is the wait before the first retry; doubles
	// on subsequent retries up to MaxBackoff. Zero falls back to
	// DefaultInitialBackoff.
	InitialBackoff time.Duration

	// MaxBackoff caps exponential backoff growth. Zero falls back
	// to DefaultMaxBackoff.
	MaxBackoff time.Duration

	// UserAgent sets the User-Agent header on every request. Empty
	// leaves the Go default in place.
	UserAgent string

	// Credential is the optional upstream credential. Unused in v1
	// (see Credential godoc).
	Credential Credential

	// Transport overrides the http.RoundTripper used to send
	// requests. Defaults to http.DefaultTransport.
	Transport http.RoundTripper
}

// Client is the configured HTTP client per-format proxy fetchers
// use to talk to upstream registries.
type Client struct {
	inner          *http.Client
	timeout        time.Duration
	maxRetries     int
	maxRedirects   int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	userAgent      string
	credential     Credential

	// wait blocks for d or until ctx is cancelled. Tests replace
	// it with a no-op (to skip real sleeps) or a collector (to
	// assert backoff durations). Kept off Options so the test seam
	// doesn't pollute the public config surface.
	wait func(ctx context.Context, d time.Duration) error
}

// New constructs a Client from opts.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxRetries := opts.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = DefaultMaxRedirects
	}
	initialBackoff := opts.InitialBackoff
	if initialBackoff <= 0 {
		initialBackoff = DefaultInitialBackoff
	}
	maxBackoff := opts.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = DefaultMaxBackoff
	}
	transport := opts.Transport
	if transport == nil {
		transport = newDefaultTransport()
	}

	checkRedirect := func(_ *http.Request, via []*http.Request) error {
		if maxRedirects < 0 {
			return http.ErrUseLastResponse
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: redirect chain exceeded %d hops", proxy.ErrUpstreamMalformed, maxRedirects)
		}
		return nil
	}

	return &Client{
		// No client-level Timeout: timeouts are applied per attempt
		// via context.WithTimeout so they compose with the caller's
		// own deadline and bound body reading.
		inner: &http.Client{
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
		timeout:        timeout,
		maxRetries:     maxRetries,
		maxRedirects:   maxRedirects,
		initialBackoff: initialBackoff,
		maxBackoff:     maxBackoff,
		userAgent:      opts.UserAgent,
		credential:     opts.Credential,
		wait:           ctxWait,
	}
}

// GetOptions tunes a single Get call.
type GetOptions struct {
	// IfNoneMatch, when non-empty, is sent as the If-None-Match
	// request header. A matching 304 response surfaces as
	// Response.NotModified == true.
	IfNoneMatch string

	// IfModifiedSince, when non-empty, is sent as the
	// If-Modified-Since request header. Formatting (HTTP-date) is
	// the caller's responsibility — usually echoing the
	// Last-Modified header from a previous response.
	IfModifiedSince string

	// Header is an optional set of additional request headers. It
	// is applied first; the conditional-GET headers and the
	// configured User-Agent override on conflict.
	Header http.Header

	// ValidateRedirect, when set, is called for each redirect target
	// before the client follows it. Returning an error stops the
	// request and surfaces proxy.ErrUpstreamMalformed to callers.
	ValidateRedirect func(*url.URL) error
}

// Response is the result of a successful Get. The caller MUST close
// Body when done with it.
type Response struct {
	// StatusCode is the final HTTP status code (after retries and
	// redirects). 304 sets NotModified.
	StatusCode int

	// Header is the final response header.
	Header http.Header

	// Body is the response body. Non-nil but empty for 304. The
	// caller MUST close it — closing also releases the per-request
	// timeout's resources.
	Body io.ReadCloser

	// ETag is the response's ETag header value (empty if absent),
	// for the caller to pass back via GetOptions.IfNoneMatch on a
	// later conditional GET.
	ETag string

	// LastModified is the response's Last-Modified header value
	// (empty if absent), for the caller to pass back via
	// GetOptions.IfModifiedSince on a later conditional GET.
	LastModified string

	// NotModified is true iff StatusCode == 304.
	NotModified bool
}

// Get fetches rawURL, applying retries, conditional GET, and the
// configured redirect cap. See the package doc for error
// classification rules.
func (c *Client) Get(ctx context.Context, rawURL string, opts GetOptions) (*Response, error) {
	backoff := c.initialBackoff
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, ctxErr(lastErr, err)
		}
		if attempt > 0 {
			if err := c.wait(ctx, backoff); err != nil {
				return nil, ctxErr(lastErr, err)
			}
			backoff *= 2
			if backoff > c.maxBackoff {
				backoff = c.maxBackoff
			}
		}

		resp, err := c.do(ctx, rawURL, opts)
		if err != nil {
			if errors.Is(err, proxy.ErrUpstreamUnavailable) {
				lastErr = err
				continue
			}
			return nil, err
		}
		if resp.StatusCode >= 500 {
			// Drain so the underlying connection can be reused on
			// the next attempt; bounded by the per-request context
			// the body still carries.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("%w: upstream returned %d", proxy.ErrUpstreamUnavailable, resp.StatusCode)
			continue
		}
		return resp, nil
	}
	// Loop exit without return implies every iteration continued,
	// each of which sets lastErr — so lastErr is non-nil here.
	return nil, lastErr
}

// ctxErr blends a prior upstream failure (lastErr) with a context
// error (cause) into a single chain. When lastErr is nil the caller
// cancelled before any upstream attempt completed, so the cause is
// purely a caller-side cancel and surfaces unwrapped.
func ctxErr(lastErr, cause error) error {
	if lastErr == nil {
		return cause
	}
	return fmt.Errorf("%w: %w", lastErr, cause)
}

func (c *Client) do(ctx context.Context, rawURL string, opts GetOptions) (*Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: build request: %w", proxy.ErrUpstreamMalformed, err)
	}
	for k, vs := range opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if opts.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", opts.IfNoneMatch)
	}
	if opts.IfModifiedSince != "" {
		req.Header.Set("If-Modified-Since", opts.IfModifiedSince)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	client := *c.inner
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if c.maxRedirects < 0 {
			return http.ErrUseLastResponse
		}
		if len(via) >= c.maxRedirects {
			return fmt.Errorf("%w: redirect chain exceeded %d hops", proxy.ErrUpstreamMalformed, c.maxRedirects)
		}
		if opts.ValidateRedirect != nil {
			if err := opts.ValidateRedirect(next.URL); err != nil {
				return fmt.Errorf("%w: redirect target %q: %w", proxy.ErrUpstreamMalformed, next.URL.String(), err)
			}
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		cancel()
		// Defensive: net/http's docs say Body is closed when
		// CheckRedirect returns an error, but it costs us nothing
		// to be explicit and protect against future changes.
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if errors.Is(err, proxy.ErrUpstreamMalformed) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", proxy.ErrUpstreamUnavailable, err)
	}

	// Do NOT call cancel() yet — the per-request timeout must
	// outlive the response so it bounds body reading too. Closing
	// the body releases the context.
	return &Response{
		StatusCode:   resp.StatusCode,
		Header:       resp.Header,
		Body:         &cancelingBody{ReadCloser: resp.Body, cancel: cancel},
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		NotModified:  resp.StatusCode == http.StatusNotModified,
	}, nil
}

// newDefaultTransport builds the http.RoundTripper used when the
// caller does not supply one. It diverges from http.DefaultTransport
// in two places that matter for a pull-through proxy:
//
//   - MaxIdleConnsPerHost is bumped from the Go default of 2 to 100.
//     A proxy fronts a small number of upstream hosts (pypi.org,
//     registry.npmjs.org, repo1.maven.org) with potentially many
//     concurrent client requests; the default ceiling makes anything
//     past the second concurrent request churn a fresh TCP+TLS
//     handshake on every call.
//
//   - HTTP/2 ReadIdleTimeout / PingTimeout are configured so the
//     transport notices half-open connections (NAT timeouts, load
//     balancer resets) and reconnects, instead of pinning streams to
//     a dead connection until the per-request timeout fires.
func newDefaultTransport() http.RoundTripper {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// Best-effort: enable HTTP/2 and tune dead-connection
	// detection. ConfigureTransports failure (e.g. running on a
	// future Go where the function signature changes) just leaves
	// us with HTTP/2-via-ALPN minus the read-idle ping — not worth
	// failing New over.
	if h2, err := http2.ConfigureTransports(t); err == nil && h2 != nil {
		h2.ReadIdleTimeout = 30 * time.Second
		h2.PingTimeout = 15 * time.Second
	}
	return t
}

// ctxWait blocks for d or returns early on ctx cancellation.
func ctxWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// cancelingBody wraps a response body so Close releases the
// per-request context, preventing the cancel func from leaking
// until the runtime's context finalizer fires.
type cancelingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelingBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
