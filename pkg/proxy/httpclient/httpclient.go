// Package httpclient is the shared HTTP client every per-format
// proxy fetcher under pkg/proxy/<format> composes.
//
// A Client applies a per-request timeout, bounded retries with
// exponential backoff on transient upstream failures, conditional
// GET (ETag / Last-Modified), and a hard redirect cap.
//
// Error classification is deliberately coarse and lives here so
// per-format fetchers don't each reinvent it:
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
//
// A Client is safe for concurrent use by multiple goroutines.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy"
)

const (
	// DefaultTimeout is the per-request timeout applied to each
	// individual attempt (the original call and every retry).
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the number of additional attempts the
	// client makes after a retryable failure. The first attempt is
	// not counted; total request count is 1 + MaxRetries.
	DefaultMaxRetries = 3

	// DefaultMaxRedirects caps the redirect chain the client will
	// follow. Anything beyond surfaces as proxy.ErrUpstreamMalformed.
	DefaultMaxRedirects = 10

	// DefaultInitialBackoff is the wait before the first retry.
	// Subsequent retries double the wait, capped at MaxBackoff.
	DefaultInitialBackoff = 250 * time.Millisecond

	// DefaultMaxBackoff caps exponential backoff growth.
	DefaultMaxBackoff = 5 * time.Second
)

// Credential is the optional upstream credential a Client attaches
// to each request.
//
// v1 leaves the credential UNUSED — the field exists on Options so
// that adding authenticated upstreams later does not reshape the
// constructor. When v1.x grows real credential support the rule
// will be: BearerToken wins over Username/Password; an empty
// Credential is the "anonymous" call public registries accept.
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

	// MaxRetries bounds retry attempts on transient failures
	// (network errors, per-request timeouts, and 5xx responses).
	// Zero falls back to DefaultMaxRetries; a negative value
	// disables retries entirely. 4xx is never retried.
	MaxRetries int

	// MaxRedirects caps the redirect chain. Zero falls back to
	// DefaultMaxRedirects; a negative value disables redirect
	// following entirely (the redirect response is returned to the
	// caller as-is).
	MaxRedirects int

	// InitialBackoff is the wait before the first retry. Subsequent
	// retries double the wait, capped at MaxBackoff. Zero falls
	// back to DefaultInitialBackoff.
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
	// requests. Defaults to http.DefaultTransport. Provided
	// primarily so tests can drop in an httptest server; production
	// code rarely needs to set it.
	Transport http.RoundTripper

	// sleep is the wait function used between retries. nil falls
	// back to time.Sleep; tests inject a no-op or counting
	// implementation. Kept unexported so it doesn't pollute the
	// public knob list.
	sleep func(time.Duration)
}

// Client is the configured HTTP client per-format proxy fetchers
// use to talk to upstream registries.
type Client struct {
	inner          *http.Client
	timeout        time.Duration
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	userAgent      string
	credential     Credential
	sleep          func(time.Duration)
}

// New constructs a Client from opts. Returning an error is reserved
// for future configuration that may be invalid; v1 always succeeds.
func New(opts Options) (*Client, error) {
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
	sleep := opts.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport
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
		inner: &http.Client{
			Transport:     transport,
			CheckRedirect: checkRedirect,
			// No client-level Timeout: we apply timeouts per
			// attempt via context.WithTimeout. A client-level
			// timeout would also abort body reads, which is fine,
			// but a context-based one composes with the caller's
			// own deadline.
		},
		timeout:        timeout,
		maxRetries:     maxRetries,
		initialBackoff: initialBackoff,
		maxBackoff:     maxBackoff,
		userAgent:      opts.UserAgent,
		credential:     opts.Credential,
		sleep:          sleep,
	}, nil
}

// GetOptions tunes a single Get call.
type GetOptions struct {
	// IfNoneMatch, when non-empty, is sent as the If-None-Match
	// request header. A matching 304 response surfaces as
	// Response.NotModified == true.
	IfNoneMatch string

	// IfModifiedSince, when non-empty, is sent as the
	// If-Modified-Since request header. Formatting (HTTP-date) is
	// the caller's responsibility — usually echoing back the
	// Last-Modified header from a previous response.
	IfModifiedSince string

	// Header is an optional set of additional request headers. It
	// is applied first; the conditional-GET headers and the
	// configured User-Agent override on conflict.
	Header http.Header
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
	// for the caller to store and pass back via
	// GetOptions.IfNoneMatch on a later conditional GET.
	ETag string

	// LastModified is the response's Last-Modified header value
	// (empty if absent), for the caller to store and pass back via
	// GetOptions.IfModifiedSince on a later conditional GET.
	LastModified string

	// NotModified is true iff StatusCode == 304.
	NotModified bool
}

// Get fetches rawURL, applying retries, conditional GET, and the
// configured redirect cap.
//
// See the package doc for the error-classification rules.
func (c *Client) Get(ctx context.Context, rawURL string, opts GetOptions) (*Response, error) {
	backoff := c.initialBackoff
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("%w: %w", proxy.ErrUpstreamUnavailable, err)
		}
		if attempt > 0 {
			c.sleep(backoff)
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
			// Drain & close so the connection can be reused on the
			// next attempt.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("%w: upstream returned %d", proxy.ErrUpstreamUnavailable, resp.StatusCode)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: exhausted retries", proxy.ErrUpstreamUnavailable)
	}
	return nil, lastErr
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

	resp, err := c.inner.Do(req)
	if err != nil {
		cancel()
		// CheckRedirect's malformed sentinel surfaces unchanged so
		// the caller can classify it without learning Go's url.Error
		// wrapping rules. Anything else (DNS, dial, transport,
		// context cancel/timeout) is "upstream unavailable".
		if errors.Is(err, proxy.ErrUpstreamMalformed) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", proxy.ErrUpstreamUnavailable, err)
	}

	// Do NOT call cancel() yet — the per-request timeout must outlive
	// the response so it bounds body reading too. Wrap the body so
	// closing it releases the context.
	return &Response{
		StatusCode:   resp.StatusCode,
		Header:       resp.Header,
		Body:         &cancelingBody{ReadCloser: resp.Body, cancel: cancel},
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		NotModified:  resp.StatusCode == http.StatusNotModified,
	}, nil
}

// cancelingBody wraps a response body so Close releases the
// per-request context. Without this the per-request timeout's
// cancel func would leak until the runtime's context finalizer
// fired.
type cancelingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelingBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
