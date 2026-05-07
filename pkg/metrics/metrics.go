// Package metrics records HTTP- and OCI-backend-layer telemetry behind a
// pluggable Recorder interface. The default implementation is
// Prometheus-backed; callers that want to disable instrumentation (tests,
// the --enable-metrics=false runtime path) use NoOp instead.
//
// The interface intentionally hides the Prometheus types from callers so
// neither pkg/handler nor pkg/oci has a build-time dependency on
// prometheus/client_golang. This keeps the door open for an OpenTelemetry
// recorder later without churning every call site.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Recorder is the surface every ocifactory call site uses to emit
// metrics. Implementations must be safe for concurrent use.
type Recorder interface {
	// HTTPRequest records the outcome of one inbound HTTP request.
	// format identifies the per-format handler (e.g. "python", "maven");
	// op is a low-cardinality verb (e.g. "read", "write", "list");
	// status is the textual HTTP status code (e.g. "200").
	HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64)

	// OCIBackendCall records one outbound call to the OCI backend
	// registry. op names the operation in pkg/oci's vocabulary
	// (e.g. "push_blob", "fetch_manifest", "list_tags"); status is
	// either "ok" or the HTTP status code returned by the backend, or
	// "error" for non-HTTP failures.
	OCIBackendCall(op, status string, duration time.Duration)

	// BlobRedirect records the outcome of one BlobRedirectURL probe.
	// outcome is "redirected" (caller will 307 to the backend's
	// presigned URL), "inline" (backend serves bytes itself; caller
	// falls back to ReadFile), or "error" (probe failed; caller
	// also falls back to ReadFile).
	BlobRedirect(outcome string)
}

// StatusOK is the conventional success label for OCIBackendCall.
const StatusOK = "ok"

// StatusError is the conventional non-HTTP-failure label for
// OCIBackendCall (e.g. context cancellation, network teardown).
const StatusError = "error"

// HTTPStatusLabel formats an integer HTTP status code into the textual
// label form used by HTTPRequest. Centralised so producer and scraper
// agree on how 0 / unset is rendered.
func HTTPStatusLabel(code int) string {
	if code == 0 {
		// http.ResponseWriter implicit 200 if no header was written.
		code = http.StatusOK
	}
	return strconv.Itoa(code)
}

type noopRecorder struct{}

func (noopRecorder) HTTPRequest(string, string, string, time.Duration, int64, int64) {}
func (noopRecorder) OCIBackendCall(string, string, time.Duration)                    {}
func (noopRecorder) BlobRedirect(string)                                             {}

// NoOp returns a Recorder that discards every observation. Used by tests
// and by serve when --enable-metrics=false.
func NoOp() Recorder { return noopRecorder{} }

// IsNoOp reports whether rec is the no-op recorder. pkg/oci uses it to
// skip the per-call wrapping (and the time.Now() pair that goes with
// it) on the metrics-disabled path. Type-checked here rather than at
// the call site so the noopRecorder type can stay unexported.
func IsNoOp(rec Recorder) bool {
	_, ok := rec.(noopRecorder)
	return ok
}

// Prometheus is a Recorder backed by github.com/prometheus/client_golang.
// It owns its *prometheus.Registry rather than using the package-global
// DefaultRegisterer so tests can construct independent recorders without
// metric-redeclaration panics.
type Prometheus struct {
	reg *prometheus.Registry

	httpRequestsTotal   *prometheus.CounterVec
	httpRequestDuration *prometheus.HistogramVec
	httpBytesIn         *prometheus.CounterVec
	httpBytesOut        *prometheus.CounterVec

	backendRequestsTotal   *prometheus.CounterVec
	backendRequestDuration *prometheus.HistogramVec

	blobRedirectTotal *prometheus.CounterVec
}

// NewPrometheus constructs a Prometheus-backed Recorder. Pass nil for reg
// to get a fresh, isolated *prometheus.Registry; pass a shared one to
// expose ocifactory's metrics alongside other collectors via the same
// Handler. The returned recorder also registers the standard Go runtime
// and process collectors against reg.
func NewPrometheus(reg *prometheus.Registry) *Prometheus {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	p := &Prometheus{
		reg: reg,
		httpRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocifactory_http_requests_total",
			Help: "Total HTTP requests handled by ocifactory, labelled by format, op, and HTTP status code.",
		}, []string{"format", "op", "status"}),
		httpRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ocifactory_http_request_duration_seconds",
			Help:    "End-to-end ocifactory HTTP handler latency, labelled by format and op.",
			Buckets: prometheus.DefBuckets,
		}, []string{"format", "op"}),
		httpBytesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocifactory_http_request_bytes_in_total",
			Help: "Total request body bytes received, derived from Content-Length.",
		}, []string{"format", "op"}),
		httpBytesOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocifactory_http_response_bytes_out_total",
			Help: "Total response body bytes written.",
		}, []string{"format", "op"}),
		backendRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocifactory_oci_backend_requests_total",
			Help: "Total calls made to the OCI backend, labelled by operation and status.",
		}, []string{"op", "status"}),
		backendRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ocifactory_oci_backend_request_duration_seconds",
			Help:    "OCI backend call latency, labelled by operation.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		blobRedirectTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ocifactory_blob_redirect_total",
			Help: "Total BlobRedirectURL probe outcomes, labelled by outcome (redirected, inline, error).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(
		p.httpRequestsTotal,
		p.httpRequestDuration,
		p.httpBytesIn,
		p.httpBytesOut,
		p.backendRequestsTotal,
		p.backendRequestDuration,
		p.blobRedirectTotal,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return p
}

// HTTPRequest implements Recorder.
func (p *Prometheus) HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64) {
	p.httpRequestsTotal.WithLabelValues(format, op, status).Inc()
	p.httpRequestDuration.WithLabelValues(format, op).Observe(duration.Seconds())
	if bytesIn > 0 {
		p.httpBytesIn.WithLabelValues(format, op).Add(float64(bytesIn))
	}
	if bytesOut > 0 {
		p.httpBytesOut.WithLabelValues(format, op).Add(float64(bytesOut))
	}
}

// OCIBackendCall implements Recorder.
func (p *Prometheus) OCIBackendCall(op, status string, duration time.Duration) {
	p.backendRequestsTotal.WithLabelValues(op, status).Inc()
	p.backendRequestDuration.WithLabelValues(op).Observe(duration.Seconds())
}

// BlobRedirect implements Recorder.
func (p *Prometheus) BlobRedirect(outcome string) {
	p.blobRedirectTotal.WithLabelValues(outcome).Inc()
}

// Handler returns an http.Handler that serves the Prometheus exposition
// for this recorder. Wire it into the parent server mux at the
// configured --metrics-path.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{Registry: p.reg})
}

// Registry exposes the underlying *prometheus.Registry for tests that
// want to scrape directly.
func (p *Prometheus) Registry() *prometheus.Registry { return p.reg }
