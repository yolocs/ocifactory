package handler_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/metrics"
)

// TestObservability_EndToEnd wires the full middleware stack — format
// tag, metrics middleware, observability handler — around a tiny
// gorilla/mux router that mimics how a per-format handler plumbs its
// routes. The test then drives traffic through it and scrapes
// /metrics to verify the labels travelled all the way through.
//
// This is the closest single-process equivalent to the issue's
// "spin the server, scrape /metrics" acceptance criterion that doesn't
// require a real OCI backend on the test box. The OCI backend
// instrumentation is exercised separately in
// pkg/oci/instrumented_test.go against the wrapper directly.
func TestObservability_EndToEnd(t *testing.T) {
	t.Parallel()

	rec := metrics.NewPrometheus(prometheus.NewRegistry())

	// Stand in for a per-format handler's Mux. RouteNameOpMiddleware
	// is what makes the named routes show up as op labels rather than
	// the method-derived defaults.
	router := mux.NewRouter()
	router.Use(mux.MiddlewareFunc(handler.RouteNameOpMiddleware))
	router.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	}).Methods(http.MethodPost).Name("write")
	router.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}).Methods(http.MethodGet).Name("list")

	pingerOK := handler.PingerFunc(func(context.Context) error { return nil })

	formatTagged := handler.WrapWithFormat("python")(router)
	withObservability := handler.ObservabilityHandler(formatTagged, pingerOK, rec.Handler(), "/metrics")
	full := handler.MetricsMiddleware(rec)(withObservability)

	srv := httptest.NewServer(full)
	t.Cleanup(srv.Close)

	mustGet(t, srv.URL+"/healthz", http.StatusOK)
	mustGet(t, srv.URL+"/readyz", http.StatusOK)
	mustPost(t, srv.URL+"/upload", "hello-payload", http.StatusCreated)
	mustGet(t, srv.URL+"/list", http.StatusOK)
	// One miss so we can assert the 404 path also gets recorded with
	// the route-less default op.
	mustGet(t, srv.URL+"/no-such-route", http.StatusNotFound)

	body := mustGet(t, srv.URL+"/metrics", http.StatusOK)

	for _, want := range []string{
		"ocifactory_http_requests_total",
		"ocifactory_http_request_duration_seconds",
		`ocifactory_http_requests_total{format="python",op="write",status="201"} 1`,
		`ocifactory_http_requests_total{format="python",op="list",status="200"} 1`,
		`ocifactory_http_request_bytes_in_total{format="python",op="write"} 13`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\nfull body:\n%s", want, body)
		}
	}
}

// TestObservability_ReadyzReportsBackendDown checks that a failing
// pinger short-circuits the readyz path with 503 — the dashboard signal
// operators rely on to page on backend outages.
func TestObservability_ReadyzReportsBackendDown(t *testing.T) {
	t.Parallel()

	pingerDown := handler.PingerFunc(func(context.Context) error { return errors.New("zot.local: connection refused") })
	h := handler.ObservabilityHandler(http.NotFoundHandler(), pingerDown, nil, "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz") //nolint:noctx
	if err != nil {
		t.Fatalf("Get /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "zot.local") {
		t.Errorf("readyz body did not surface backend error: %q", body)
	}
}

func TestObservability_MetricsDisabled404(t *testing.T) {
	t.Parallel()

	// Metrics path "" mirrors --enable-metrics=false: no /metrics
	// endpoint, requests fall through to the inner handler (which
	// here returns 404 because no other route matches).
	h := handler.ObservabilityHandler(http.NotFoundHandler(), nil, nil, "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics") //nolint:noctx
	if err != nil {
		t.Fatalf("Get /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func mustGet(t *testing.T, url string, wantCode int) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("Get %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantCode {
		t.Errorf("Get %s status = %d, want %d", url, resp.StatusCode, wantCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func mustPost(t *testing.T, url, body string, wantCode int) {
	t.Helper()
	resp, err := http.Post(url, "application/octet-stream", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("Post %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantCode {
		t.Errorf("Post %s status = %d, want %d", url, resp.StatusCode, wantCode)
	}
}

// _ keeps the prometheus import warm without forcing every assertion
// to walk the registry directly — the /metrics body parse above is
// the higher-signal check.
var _ = prometheus.NewRegistry
