package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNoOp_DoesNothing(t *testing.T) {
	t.Parallel()

	r := NoOp()
	r.HTTPRequest("python", "read", "200", time.Millisecond, 10, 20)
	r.OCIBackendCall("push_blob", StatusOK, time.Millisecond)
	r.BlobRedirect("redirected")
	// The point of NoOp is that it has no observable side effects; if
	// we reached this line without panicking the contract is met.
}

func TestHTTPStatusLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "explicit ok", code: 200, want: "200"},
		{name: "not found", code: 404, want: "404"},
		{name: "zero defaults to 200", code: 0, want: "200"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, HTTPStatusLabel(tc.code)); diff != "" {
				t.Errorf("HTTPStatusLabel(%d) mismatch (-want +got):\n%s", tc.code, diff)
			}
		})
	}
}

func TestPrometheus_HTTPRequest_RecordsCounters(t *testing.T) {
	t.Parallel()

	p := NewPrometheus(prometheus.NewRegistry())

	p.HTTPRequest("python", "read", "200", 5*time.Millisecond, 100, 200)
	p.HTTPRequest("python", "read", "200", 10*time.Millisecond, 50, 75)
	p.HTTPRequest("python", "read", "404", 1*time.Millisecond, 0, 9)

	if got, want := testutil.ToFloat64(p.httpRequestsTotal.WithLabelValues("python", "read", "200")), 2.0; got != want {
		t.Errorf("requests_total{python,read,200} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.httpRequestsTotal.WithLabelValues("python", "read", "404")), 1.0; got != want {
		t.Errorf("requests_total{python,read,404} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.httpBytesIn.WithLabelValues("python", "read")), 150.0; got != want {
		t.Errorf("bytes_in_total{python,read} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.httpBytesOut.WithLabelValues("python", "read")), 284.0; got != want {
		t.Errorf("bytes_out_total{python,read} = %v, want %v", got, want)
	}
}

func TestPrometheus_OCIBackendCall_RecordsCounters(t *testing.T) {
	t.Parallel()

	p := NewPrometheus(prometheus.NewRegistry())

	p.OCIBackendCall("push_blob", StatusOK, time.Millisecond)
	p.OCIBackendCall("push_blob", StatusOK, time.Millisecond)
	p.OCIBackendCall("push_blob", StatusError, time.Millisecond)
	p.OCIBackendCall("fetch_manifest", StatusOK, time.Millisecond)

	if got, want := testutil.ToFloat64(p.backendRequestsTotal.WithLabelValues("push_blob", StatusOK)), 2.0; got != want {
		t.Errorf("backend_requests_total{push_blob,ok} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.backendRequestsTotal.WithLabelValues("push_blob", StatusError)), 1.0; got != want {
		t.Errorf("backend_requests_total{push_blob,error} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.backendRequestsTotal.WithLabelValues("fetch_manifest", StatusOK)), 1.0; got != want {
		t.Errorf("backend_requests_total{fetch_manifest,ok} = %v, want %v", got, want)
	}
}

func TestPrometheus_BlobRedirect_RecordsCounters(t *testing.T) {
	t.Parallel()

	p := NewPrometheus(prometheus.NewRegistry())

	p.BlobRedirect("redirected")
	p.BlobRedirect("redirected")
	p.BlobRedirect("inline")
	p.BlobRedirect("error")

	if got, want := testutil.ToFloat64(p.blobRedirectTotal.WithLabelValues("redirected")), 2.0; got != want {
		t.Errorf("blob_redirect_total{redirected} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.blobRedirectTotal.WithLabelValues("inline")), 1.0; got != want {
		t.Errorf("blob_redirect_total{inline} = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(p.blobRedirectTotal.WithLabelValues("error")), 1.0; got != want {
		t.Errorf("blob_redirect_total{error} = %v, want %v", got, want)
	}
}

func TestPrometheus_Handler_ExposesRegisteredMetrics(t *testing.T) {
	t.Parallel()

	p := NewPrometheus(prometheus.NewRegistry())
	p.HTTPRequest("maven", "write", "201", time.Millisecond, 1, 2)
	p.OCIBackendCall("push_blob", StatusOK, time.Millisecond)
	p.BlobRedirect("redirected")

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		"ocifactory_http_requests_total",
		"ocifactory_http_request_duration_seconds",
		"ocifactory_http_request_bytes_in_total",
		"ocifactory_http_response_bytes_out_total",
		"ocifactory_oci_backend_requests_total",
		"ocifactory_oci_backend_request_duration_seconds",
		"ocifactory_blob_redirect_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q\n%s", want, body)
		}
	}
}
