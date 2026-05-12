package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/internal/version"
	"github.com/yolocs/ocifactory/pkg/metrics"
)

// recordingRecorder captures HTTPRequest observations so tests can
// assert on the labels MetricsMiddleware produced.
type recordingRecorder struct {
	mu    sync.Mutex
	calls []httpCall
}

type httpCall struct {
	format, op, status string
	bytesIn, bytesOut  int64
	durationGTZero     bool
}

func (r *recordingRecorder) HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, httpCall{
		format: format, op: op, status: status,
		bytesIn: bytesIn, bytesOut: bytesOut,
		durationGTZero: duration > 0,
	})
}
func (r *recordingRecorder) OCIBackendCall(string, string, time.Duration) {}

func (r *recordingRecorder) BlobRedirect(string) {}

func (r *recordingRecorder) snapshot() []httpCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]httpCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestMetricsMiddleware_LabelsAndStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		body      string
		respWrite func(w http.ResponseWriter)
		setRoute  func(*mux.Router, http.Handler)
		setFormat string
		setOp     string
		want      httpCall
	}{
		{
			name:   "GET defaults to read 200",
			method: http.MethodGet,
			respWrite: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("hello"))
			},
			setFormat: "python",
			want: httpCall{
				format: "python", op: "read", status: "200",
				bytesIn: 0, bytesOut: 5, durationGTZero: true,
			},
		},
		{
			name:   "POST with body and explicit status",
			method: http.MethodPost,
			body:   "12345",
			respWrite: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusCreated)
			},
			setFormat: "python",
			want: httpCall{
				format: "python", op: "write", status: "201",
				bytesIn: 5, bytesOut: 0, durationGTZero: true,
			},
		},
		{
			name:   "SetOp from inner handler overrides method",
			method: http.MethodGet,
			respWrite: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("ok"))
			},
			setFormat: "python",
			setOp:     "list",
			want: httpCall{
				format: "python", op: "list", status: "200",
				bytesOut: 2, durationGTZero: true,
			},
		},
		{
			name:   "missing format defaults to unknown",
			method: http.MethodDelete,
			respWrite: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNoContent)
			},
			want: httpCall{
				format: "unknown", op: "delete", status: "204",
				durationGTZero: true,
			},
		},
		{
			name:   "route name takes priority over method",
			method: http.MethodGet,
			respWrite: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("listy"))
			},
			setFormat: "python",
			setRoute: func(r *mux.Router, inner http.Handler) {
				r.Handle("/named", inner).Methods(http.MethodGet).Name("list")
			},
			want: httpCall{
				format: "python", op: "list", status: "200",
				bytesOut: 5, durationGTZero: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := &recordingRecorder{}
			finalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.setOp != "" {
					SetOp(r.Context(), tc.setOp)
				}
				if tc.respWrite != nil {
					tc.respWrite(w)
				}
			})

			formatWrap := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.setFormat != "" {
						SetFormat(r.Context(), tc.setFormat)
					}
					next.ServeHTTP(w, r)
				})
			}

			var routed http.Handler = finalHandler
			if tc.setRoute != nil {
				router := mux.NewRouter()
				router.Use(mux.MiddlewareFunc(RouteNameOpMiddleware))
				tc.setRoute(router, finalHandler)
				routed = router
			}

			// MetricsMiddleware on the outside is what production wires
			// up: it injects the metrics-state holder before any inner
			// middleware (format tagging, route-name op, format
			// handlers) gets to write to it. With the order reversed
			// the inner SetFormat / SetOp calls would no-op.
			h := MetricsMiddleware(rec)(formatWrap(routed))

			path := "/"
			if tc.setRoute != nil {
				path = "/named"
			}
			req := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
			req.ContentLength = int64(len(tc.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			calls := rec.snapshot()
			if len(calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(calls))
			}
			if diff := cmp.Diff(tc.want, calls[0], cmp.AllowUnexported(httpCall{})); diff != "" {
				t.Errorf("HTTPRequest call mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestObservabilityHandler_Healthz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		method   string
		wantCode int
		wantBody string
	}{
		{name: "GET ok", method: http.MethodGet, wantCode: http.StatusOK, wantBody: "ok"},
		{name: "HEAD ok", method: http.MethodHead, wantCode: http.StatusOK},
		{name: "POST not allowed", method: http.MethodPost, wantCode: http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("inner handler unexpectedly called for %s", r.URL.Path)
			})
			h := ObservabilityHandler(inner, nil, nil, "")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(tc.method, "/healthz", nil))
			if rr.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantCode)
			}
			if tc.wantBody != "" && rr.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q", rr.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestObservabilityHandler_Readyz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pinger      Pinger
		wantCode    int
		wantStatus  string
		wantBackend string
	}{
		{
			name:        "no pinger returns 200 ready",
			pinger:      nil,
			wantCode:    http.StatusOK,
			wantStatus:  "ready",
			wantBackend: "not_configured",
		},
		{
			name:        "pinger ok returns 200 ready",
			pinger:      PingerFunc(func(context.Context) error { return nil }),
			wantCode:    http.StatusOK,
			wantStatus:  "ready",
			wantBackend: "ok",
		},
		{
			name:        "pinger fails returns 503 not_ready",
			pinger:      PingerFunc(func(context.Context) error { return errors.New("backend down") }),
			wantCode:    http.StatusServiceUnavailable,
			wantStatus:  "not_ready",
			wantBackend: "backend down",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("inner handler unexpectedly called")
			})
			h := ObservabilityHandler(inner, tc.pinger, nil, "")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rr.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantCode)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json prefix", ct)
			}
			var got readyzResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body: %v\nbody: %s", err, rr.Body.String())
			}
			want := readyzResponse{
				Status:  tc.wantStatus,
				Backend: tc.wantBackend,
				Build: buildInfo{
					Version: version.Version,
					Commit:  version.Commit,
					OSArch:  version.OSArch,
				},
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("/readyz body mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestObservabilityHandler_Readyz_HEAD(t *testing.T) {
	t.Parallel()

	// HEAD must return the headers GET would (Content-Type: JSON)
	// but no body — load balancers issue HEAD probes and shouldn't
	// have to drain a body.
	h := ObservabilityHandler(http.NotFoundHandler(), nil, nil, "")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/readyz", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD response body = %q, want empty", rr.Body.String())
	}
}

func TestObservabilityHandler_Readyz_CachesResult(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	pinger := PingerFunc(func(context.Context) error {
		calls.Add(1)
		return nil
	})
	h := ObservabilityHandler(http.NotFoundHandler(), pinger, nil, "")

	const probes = 50
	var wg sync.WaitGroup
	for i := 0; i < probes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		}()
	}
	wg.Wait()

	// All probes within the readyzCacheTTL window should collapse onto
	// at most a small number of underlying pings (the first one plus
	// any that lost the cache-fill race). The exact count depends on
	// scheduler timing; the contract worth asserting is "<< probes",
	// not "exactly 1".
	if got := calls.Load(); got > 5 || got < 1 {
		t.Errorf("backend ping called %d times for %d probes; want a small handful", got, probes)
	}
}

func TestObservabilityHandler_MetricsPathRouting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		metricsPath string
		reqPath     string
		wantBody    string
		wantInner   int64
		wantMetrics int64
	}{
		{
			name:        "metrics path serves metrics",
			metricsPath: "/metrics",
			reqPath:     "/metrics",
			wantBody:    "metrics-out",
			wantMetrics: 1,
		},
		{
			name:        "non-metrics path falls through",
			metricsPath: "/metrics",
			reqPath:     "/some/other/path",
			wantBody:    "inner",
			wantInner:   1,
		},
		{
			name:        "metrics disabled (empty path) falls through",
			metricsPath: "",
			reqPath:     "/metrics",
			wantBody:    "inner",
			wantInner:   1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var innerCalls, metricsHits atomic.Int64
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				innerCalls.Add(1)
				_, _ = w.Write([]byte("inner"))
			})
			metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metricsHits.Add(1)
				_, _ = w.Write([]byte("metrics-out"))
			})
			h := ObservabilityHandler(inner, nil, metricsHandler, tc.metricsPath)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.reqPath, nil))
			body, _ := io.ReadAll(rr.Body)
			if string(body) != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if got := innerCalls.Load(); got != tc.wantInner {
				t.Errorf("innerCalls = %d, want %d", got, tc.wantInner)
			}
			if got := metricsHits.Load(); got != tc.wantMetrics {
				t.Errorf("metricsHits = %d, want %d", got, tc.wantMetrics)
			}
		})
	}
}

func TestWrapWithFormat_TagsContext(t *testing.T) {
	t.Parallel()

	var seenFormat string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenFormat = FormatFromContext(r.Context())
	})
	// WrapWithFormat writes into the metrics-state holder; the holder
	// is injected by MetricsMiddleware, which is why we wrap with it
	// here even though the test only inspects format propagation.
	h := MetricsMiddleware(nil)(WrapWithFormat("maven")(inner))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if seenFormat != "maven" {
		t.Errorf("FormatFromContext = %q, want %q", seenFormat, "maven")
	}
}

func TestMetricsMiddleware_NoRecorder_StillSafe(t *testing.T) {
	t.Parallel()

	h := MetricsMiddleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	// The contract: nil recorder must not panic and must not interfere
	// with the inner handler. If we got this far without panicking, met.
	_ = metrics.NoOp() // keep import warm even if NewPrometheus isn't used here
}
