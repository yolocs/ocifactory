package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/metrics"
)

// Pinger probes the OCI backend for /readyz. *oci.Registry satisfies it
// out of the box; defining it here keeps pkg/handler free of an import
// on pkg/oci so the dependency graph stays one-way.
type Pinger interface {
	Ping(ctx context.Context) error
}

// readyzProbeTimeout caps how long /readyz is willing to spend pinging
// the backend per probe. Picked at the high end of what a load
// balancer's readiness probe will tolerate.
const readyzProbeTimeout = 2 * time.Second

// readyzCacheTTL is how long a successful probe result is reused
// across concurrent /readyz hits. Without it a probe storm (k8s default
// readiness interval is 10s; multiplied by N pods sharing a backend) can
// hammer the registry. 1s is short enough that a backend outage is
// noticed within one cache window, long enough to flatten the spike.
const readyzCacheTTL = time.Second

type contextKey string

const (
	metricsStateKey contextKey = "metrics-state"
)

// metricsState is the mutable holder MetricsMiddleware injects into
// every request's context. Inner middlewares (WrapWithFormat,
// RouteNameOpMiddleware, format-handler-specific overrides) write the
// labels they want recorded; the outer middleware reads them after the
// chain returns.
//
// The holder exists because gorilla/mux clones the request via
// r.WithContext(...) when it dispatches a route — any context.Value()
// stored *into* that clone is invisible by the time we regain control
// in the outer middleware. A pointer-based holder sidesteps the
// problem entirely: every middleware sees the same struct, the writes
// are visible up the chain.
type metricsState struct {
	mu     sync.Mutex
	format string
	op     string
}

func (s *metricsState) setFormat(format string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.format = format
}

func (s *metricsState) setOp(op string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.op = op
}

func (s *metricsState) snapshot() (format, op string) {
	if s == nil {
		return "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.format, s.op
}

// SetFormat records the format label on the request context. Format
// handlers should call this once at the top of their handler chain
// (typically via WrapWithFormat).
func SetFormat(ctx context.Context, format string) {
	if s, ok := ctx.Value(metricsStateKey).(*metricsState); ok {
		s.setFormat(format)
	}
}

// FormatFromContext returns the format set by SetFormat, or "" if
// none was. Exposed for tests; production code reads it implicitly
// through MetricsMiddleware.
func FormatFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(metricsStateKey).(*metricsState); ok {
		f, _ := s.snapshot()
		return f
	}
	return ""
}

// SetOp records an op label on the request context. Safe to call from
// any middleware or handler downstream of MetricsMiddleware. Pairs
// with RouteNameOpMiddleware, which copies a gorilla/mux route name
// into the same holder so format handlers don't have to call SetOp by
// hand on every route.
func SetOp(ctx context.Context, op string) {
	if s, ok := ctx.Value(metricsStateKey).(*metricsState); ok {
		s.setOp(op)
	}
}

// OpFromContext returns the most recent op set via SetOp, or "" if
// none has been written. Exposed for tests; production code reads it
// implicitly through MetricsMiddleware.
func OpFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(metricsStateKey).(*metricsState); ok {
		_, op := s.snapshot()
		return op
	}
	return ""
}

// RouteNameOpMiddleware copies the matched gorilla/mux route's Name()
// into the op holder. Install it via router.Use() inside each format
// handler's Mux() so the metrics middleware can pick up "list" /
// "read" / "write" labels without every handler having to call SetOp
// by hand.
func RouteNameOpMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route := mux.CurrentRoute(r); route != nil {
			if name := route.GetName(); name != "" {
				SetOp(r.Context(), name)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// WrapWithFormat returns a middleware that tags every request with the
// supplied format label. Apply it once per format handler so
// MetricsMiddleware can attribute traffic to the right format.
//
// Writes the label into the metrics-state holder rather than mutating
// the context directly, so the value propagates back up through any
// gorilla/mux dispatch happening downstream.
func WrapWithFormat(format string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetFormat(r.Context(), format)
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder wraps an http.ResponseWriter so MetricsMiddleware can
// observe the status code and bytes written that the inner handler
// produced. Defaults status to 200 to match http.ResponseWriter's
// implicit behaviour when WriteHeader is never called.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytesOut    int64
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytesOut += int64(n)
	return n, err
}

// MetricsMiddleware records one HTTPRequest observation per inbound
// request. It injects an op-state holder into context so inner
// middleware (notably RouteNameOpMiddleware) can publish a label that
// survives gorilla/mux's per-request context clone — without the
// holder, route names set inside the mux are invisible by the time
// this middleware regains control.
//
// Format label flows from WrapWithFormat (set on the way in); op label
// is the most recently SetOp'd value, falling back to a coarse
// method-based bucket when the inner handler chose not to label
// itself.
func MetricsMiddleware(rec metrics.Recorder) Middleware {
	if rec == nil {
		rec = metrics.NoOp()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := &metricsState{}
			r = r.WithContext(context.WithValue(r.Context(), metricsStateKey, state))

			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sr, r)

			format, op := state.snapshot()
			if format == "" {
				format = "unknown"
			}
			if op == "" {
				op = methodOp(r.Method)
			}
			bytesIn := r.ContentLength
			if bytesIn < 0 {
				bytesIn = 0
			}
			rec.HTTPRequest(format, op, metrics.HTTPStatusLabel(sr.status), time.Since(start), bytesIn, sr.bytesOut)
		})
	}
}

// methodOp is the fallback op label when the route hasn't named itself
// and no context override is set. The classification is intentionally
// coarse — a single "write" bucket covers PUT/POST/PATCH/DELETE
// because bursting them open into per-method labels would multiply
// cardinality without aiding any diagnostic question on the dashboard.
func methodOp(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read"
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		return "write"
	case http.MethodDelete:
		return "delete"
	default:
		return "other"
	}
}

// ObservabilityHandler builds a parent http.Handler that intercepts
// /healthz, /readyz, and the configured metrics path before delegating
// every other request to inner. Returning a single composed handler
// lets the Server wire it the same way it wires any other handler — the
// observability endpoints don't need a sidecar listener.
//
// metricsPath empty disables /metrics serving (the corresponding flag
// is --enable-metrics=false). pinger may be nil; in that case /readyz
// always answers 200, which matches a deployment that hasn't wired up
// a backend probe yet.
//
// The /readyz cache is per parent handler (not global) so two
// independently-constructed servers — exotic but legal — keep
// independent state.
func ObservabilityHandler(inner http.Handler, pinger Pinger, metricsHandler http.Handler, metricsPath string) http.Handler {
	probe := newReadyzProbe(pinger)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			handleHealthz(w, r)
			return
		case "/readyz":
			probe.serve(w, r)
			return
		}
		if metricsPath != "" && metricsHandler != nil && r.URL.Path == metricsPath {
			metricsHandler.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok"))
	}
}

// readyzProbe caches the most recent backend probe result for
// readyzCacheTTL so concurrent /readyz hits collapse onto a single
// backend round-trip per window.
type readyzProbe struct {
	pinger Pinger

	mu        sync.Mutex
	cachedErr error
	cachedAt  time.Time
}

func newReadyzProbe(pinger Pinger) *readyzProbe {
	return &readyzProbe{pinger: pinger}
}

func (p *readyzProbe) serve(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if p.pinger == nil {
		// No backend wired in — readiness collapses to liveness. We
		// don't ship a 200 with a misleading "ok" body in this mode;
		// just say so explicitly.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ready (no backend pinger configured)"))
		}
		return
	}

	probeCtx, cancel := context.WithTimeout(r.Context(), readyzProbeTimeout)
	defer cancel()
	err := p.probe(probeCtx)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		logger.DebugContext(r.Context(), "readyz probe failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, "not ready: %s", err)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ready"))
	}
}

// probe returns the most recent error within the cache TTL, otherwise
// runs a fresh ping and stores the result. The cache covers both
// success and failure: a single probe storm during an outage shouldn't
// fan out to N concurrent retries.
func (p *readyzProbe) probe(ctx context.Context) error {
	p.mu.Lock()
	if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < readyzCacheTTL {
		err := p.cachedErr
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()

	err := p.pinger.Ping(ctx)

	p.mu.Lock()
	p.cachedErr = err
	p.cachedAt = time.Now()
	p.mu.Unlock()
	return err
}

// PingerFunc adapts a function to the Pinger interface so callers can
// wire one up without defining a type. Useful in tests.
type PingerFunc func(ctx context.Context) error

func (f PingerFunc) Ping(ctx context.Context) error { return f(ctx) }
