package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy"
)

// newTestClient builds a Client with the no-op sleep injected so
// retry tests don't actually wait. Tests that want to assert on the
// sleep durations pass their own collector via opts.sleep.
func newTestClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.sleep == nil {
		opts.sleep = func(time.Duration) {}
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// readBody reads and closes the response body, failing the test on
// any error so the call site stays readable.
func readBody(t *testing.T, resp *Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestNew_Defaults(t *testing.T) {
	t.Parallel()

	c, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", c.timeout, DefaultTimeout)
	}
	if c.maxRetries != DefaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", c.maxRetries, DefaultMaxRetries)
	}
	if c.initialBackoff != DefaultInitialBackoff {
		t.Errorf("initialBackoff = %v, want %v", c.initialBackoff, DefaultInitialBackoff)
	}
	if c.maxBackoff != DefaultMaxBackoff {
		t.Errorf("maxBackoff = %v, want %v", c.maxBackoff, DefaultMaxBackoff)
	}
}

func TestNew_NegativeRetriesDisables(t *testing.T) {
	t.Parallel()

	c, err := New(Options{MaxRetries: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.maxRetries != 0 {
		t.Errorf("maxRetries = %d, want 0 (negative disables)", c.maxRetries)
	}
}

func TestCredential_IsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    Credential
		want bool
	}{
		{"empty", Credential{}, true},
		{"username_only", Credential{Username: "u"}, false},
		{"password_only", Credential{Password: "p"}, false},
		{"bearer_only", Credential{BearerToken: "t"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.c.IsZero(); got != tc.want {
				t.Errorf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClient_Get_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		io.WriteString(w, "hello")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.NotModified {
		t.Errorf("NotModified = true, want false")
	}
	if resp.ETag != `"v1"` {
		t.Errorf("ETag = %q, want %q", resp.ETag, `"v1"`)
	}
	if resp.LastModified != "Wed, 21 Oct 2015 07:28:00 GMT" {
		t.Errorf("LastModified = %q", resp.LastModified)
	}
	if got := readBody(t, resp); got != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
}

func TestClient_Get_RetriesOn5xx(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{MaxRetries: 3})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := readBody(t, resp); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("request count = %d, want 3 (2 fails then 1 success)", got)
	}
}

func TestClient_Get_RetryBudgetExhausted(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{MaxRetries: 2})
	_, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
	// MaxRetries=2 → 1 initial + 2 retries = 3 attempts.
	if got := hits.Load(); got != 3 {
		t.Errorf("request count = %d, want 3", got)
	}
}

func TestClient_Get_NoRetryOn4xx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
	}{
		{"400", http.StatusBadRequest},
		{"401", http.StatusUnauthorized},
		{"403", http.StatusForbidden},
		{"404", http.StatusNotFound},
		{"429", http.StatusTooManyRequests},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				http.Error(w, "nope", tc.code)
			}))
			t.Cleanup(srv.Close)

			c := newTestClient(t, Options{MaxRetries: 3})
			resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.code {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tc.code)
			}
			// Critical: 4xx must NOT trigger retries; the fetcher
			// translates 404 to ErrNotFound itself.
			if got := hits.Load(); got != 1 {
				t.Errorf("request count = %d, want 1 (no retry on 4xx)", got)
			}
		})
	}
}

func TestClient_Get_Timeout(t *testing.T) {
	t.Parallel()

	// Server sleeps longer than the client's per-request timeout. We
	// disable retries to keep the total test wall time near the
	// timeout itself.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{Timeout: 50 * time.Millisecond, MaxRetries: -1})
	_, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestClient_Get_ConditionalGet(t *testing.T) {
	t.Parallel()

	const etag = `"v42"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		io.WriteString(w, "fresh")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{})

	// First call: no ETag → fresh response.
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if resp.NotModified {
		t.Fatalf("first response NotModified = true, want false")
	}
	if resp.ETag != etag {
		t.Errorf("first response ETag = %q, want %q", resp.ETag, etag)
	}
	if got := readBody(t, resp); got != "fresh" {
		t.Errorf("first body = %q, want %q", got, "fresh")
	}

	// Second call: pass the ETag back → 304 → NotModified surfaced
	// to the caller without an error.
	resp2, err := c.Get(t.Context(), srv.URL, GetOptions{IfNoneMatch: etag})
	if err != nil {
		t.Fatalf("conditional Get: %v", err)
	}
	defer resp2.Body.Close()
	if !resp2.NotModified {
		t.Errorf("conditional response NotModified = false, want true")
	}
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional response StatusCode = %d, want 304", resp2.StatusCode)
	}
	if resp2.ETag != etag {
		t.Errorf("conditional response ETag = %q, want %q", resp2.ETag, etag)
	}
}

func TestClient_Get_IfModifiedSinceForwarded(t *testing.T) {
	t.Parallel()

	const want = "Wed, 21 Oct 2015 07:28:00 GMT"
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("If-Modified-Since")
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{IfModifiedSince: want})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if got != want {
		t.Errorf("server saw If-Modified-Since = %q, want %q", got, want)
	}
}

func TestClient_Get_RedirectsFollowedWithinCap(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/end", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "landed")
	})
	mux.HandleFunc("/r2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/r1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/r2", http.StatusFound)
	})
	srv := httptest.NewServer(&mux)
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{MaxRedirects: 5})
	resp, err := c.Get(t.Context(), srv.URL+"/r1", GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := readBody(t, resp); got != "landed" {
		t.Errorf("body = %q, want %q", got, "landed")
	}
}

func TestClient_Get_RedirectCapExceeded(t *testing.T) {
	t.Parallel()

	// Each request redirects back to the same URL; the client must
	// surface ErrUpstreamMalformed once the cap is reached, not loop
	// forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{MaxRedirects: 2, MaxRetries: -1})
	_, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamMalformed) {
		t.Fatalf("err = %v, want ErrUpstreamMalformed", err)
	}
	// Critically, malformed is NOT classified as unavailable —
	// retrying a redirect loop is useless.
	if errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Errorf("err satisfies ErrUpstreamUnavailable, should be malformed-only")
	}
}

func TestClient_Get_RedirectsDisabled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{MaxRedirects: -1})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("StatusCode = %d, want 302 (redirect not followed)", resp.StatusCode)
	}
}

func TestClient_Get_UserAgent(t *testing.T) {
	t.Parallel()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{UserAgent: "ocifactory-test/1.0"})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if got != "ocifactory-test/1.0" {
		t.Errorf("server saw User-Agent = %q, want %q", got, "ocifactory-test/1.0")
	}
}

func TestClient_Get_ExtraHeaders(t *testing.T) {
	t.Parallel()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Format")
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{
		Header: http.Header{"X-Format": {"python"}},
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if got != "python" {
		t.Errorf("server saw X-Format = %q, want %q", got, "python")
	}
}

func TestClient_Get_NetworkError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// Close immediately so the URL is unreachable for the upcoming
	// Get. This exercises the dial-failure path without depending on
	// firewall behaviour for a fixed-port "unreachable" address.
	srv.Close()

	c := newTestClient(t, Options{MaxRetries: -1})
	_, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestClient_Get_InvalidURL(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, Options{})
	_, err := c.Get(t.Context(), "://not-a-url", GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamMalformed) {
		t.Fatalf("err = %v, want ErrUpstreamMalformed", err)
	}
}

func TestClient_Get_BackoffGrowsExponentially(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var waits []time.Duration
	c, err := New(Options{
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		sleep:          func(d time.Duration) { waits = append(waits, d) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Get(t.Context(), srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
	// 3 retries → 3 sleeps before attempts 1, 2, 3. Should be
	// 10ms, 20ms, 40ms.
	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
	}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want length %d", waits, len(want))
	}
	for i, w := range want {
		if waits[i] != w {
			t.Errorf("wait[%d] = %v, want %v", i, waits[i], w)
		}
	}
}

func TestClient_Get_BackoffCappedAtMaxBackoff(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var waits []time.Duration
	c, err := New(Options{
		MaxRetries:     5,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     150 * time.Millisecond,
		sleep:          func(d time.Duration) { waits = append(waits, d) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _ = c.Get(t.Context(), srv.URL, GetOptions{})
	for i, w := range waits {
		if w > 150*time.Millisecond {
			t.Errorf("wait[%d] = %v exceeds max 150ms", i, w)
		}
	}
}

func TestClient_Get_ContextCancelStopsRetries(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel so the first ctx.Err() in the loop fires.

	c := newTestClient(t, Options{MaxRetries: 3})
	_, err := c.Get(ctx, srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want chain to include context.Canceled", err)
	}
}

func TestClient_Get_BodyCloseReleasesTimeoutContext(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "hello world")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{})
	resp, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, ok := resp.Body.(*cancelingBody)
	if !ok {
		t.Fatalf("resp.Body has type %T, want *cancelingBody", resp.Body)
	}
	// Closing must run the underlying body's Close AND the cancel
	// func. We can't observe the cancel from outside cleanly, so
	// re-read after close to confirm it errors instead of hanging.
	if err := body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := io.ReadAll(body); err == nil {
		// Reading a closed body returns an error; absence here would
		// mean we never actually closed.
		t.Errorf("ReadAll after Close: err = nil, want non-nil")
	}
}

func TestClient_Get_TransportOverride(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	// A trivial RoundTripper that rewrites every request to point at
	// srv. Exercises the Transport option without needing a custom
	// proxy server.
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		newURL := srv.URL + r.URL.Path
		newReq, err := http.NewRequestWithContext(r.Context(), r.Method, newURL, r.Body)
		if err != nil {
			return nil, err
		}
		return http.DefaultTransport.RoundTrip(newReq)
	})
	c := newTestClient(t, Options{Transport: rt})

	resp, err := c.Get(t.Context(), "http://ignored.invalid/foo", GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := readBody(t, resp); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

func TestClient_Get_5xxErrorBodyDoesNotLeak(t *testing.T) {
	t.Parallel()

	// Hand a large body with each 5xx so a leaked body would be
	// observable (the connection wouldn't be reused; we'd see fewer
	// hits than retries on a kept-alive server). The simpler signal:
	// after retries the client returns the sentinel without exposing
	// the 5xx body. We rely on the drain-and-close in Get; this test
	// asserts the contract by checking the sentinel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, strings.Repeat("x", 1<<14))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, Options{MaxRetries: 2})
	_, err := c.Get(t.Context(), srv.URL, GetOptions{})
	if !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
