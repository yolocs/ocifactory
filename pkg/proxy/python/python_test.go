package python

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy"
	"github.com/yolocs/ocifactory/pkg/proxy/httpclient"
)

// stubUpstream is a tiny PyPI emulator built around an
// [http.ServeMux] so individual tests can plug in just the routes
// they exercise.
type stubUpstream struct {
	*httptest.Server
	mux *http.ServeMux

	indexHits    atomic.Int64
	metadataHits atomic.Int64
	fileHits     atomic.Int64
	topLevelHits atomic.Int64
}

func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &stubUpstream{Server: srv, mux: mux}
}

// newFetcher returns a Fetcher wired to a stub upstream with a tight
// httpclient timeout so test latency stays predictable.
func (s *stubUpstream) newFetcher(t *testing.T, opts ...Option) *Fetcher {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	allOpts := []Option{
		WithClient(httpclient.New(httpclient.Options{
			Timeout:        500 * time.Millisecond,
			MaxRetries:     -1,
			InitialBackoff: time.Millisecond,
		})),
	}
	allOpts = append(allOpts, opts...)
	f, err := New(u, allOpts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		urlStr  string
		wantErr bool
	}{
		{name: "empty url", urlStr: "", wantErr: true},
		{name: "relative", urlStr: "/foo", wantErr: true},
		{name: "missing scheme", urlStr: "//pypi.org", wantErr: true},
		{name: "ftp scheme", urlStr: "ftp://pypi.org", wantErr: true},
		{name: "https ok", urlStr: "https://pypi.org", wantErr: false},
		{name: "http ok", urlStr: "http://localhost:8080", wantErr: false},
		{name: "trailing slash stripped", urlStr: "https://pypi.org/", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var u *url.URL
			if tc.urlStr != "" {
				var err error
				u, err = url.Parse(tc.urlStr)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
			}
			_, err := New(u)
			if (err != nil) != tc.wantErr {
				t.Errorf("New(%q) err=%v, wantErr=%v", tc.urlStr, err, tc.wantErr)
			}
		})
	}
}

func TestGetSimpleIndex(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	body := `<html><body><a href="https://files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl#sha256=abc">requests-2.31.0-py3-none-any.whl</a></body></html>`
	s.mux.HandleFunc("/simple/requests/", func(w http.ResponseWriter, r *http.Request) {
		s.indexHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+html")
		_, _ = io.WriteString(w, body)
	})
	s.mux.HandleFunc("/simple/missing/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	f := s.newFetcher(t)
	got, err := f.GetSimpleIndex(t.Context(), "requests")
	if err != nil {
		t.Fatalf("GetSimpleIndex: %v", err)
	}
	if string(got.Body) != body {
		t.Errorf("body mismatch:\nwant: %q\ngot:  %q", body, string(got.Body))
	}
	if got.ContentType != "application/vnd.pypi.simple.v1+html" {
		t.Errorf("ContentType = %q, want PEP 691 html", got.ContentType)
	}

	if _, err := f.GetSimpleIndex(t.Context(), "missing"); !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("missing package: got %v, want ErrNotFound", err)
	}

	if _, err := f.GetSimpleIndex(t.Context(), ""); err == nil {
		t.Errorf("empty pkg: want error")
	}
}

func TestGetTopLevelIndex(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	body := `<html><body><a href="/simple/requests/">requests</a></body></html>`
	s.mux.HandleFunc("/simple/", func(w http.ResponseWriter, r *http.Request) {
		s.topLevelHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	})

	f := s.newFetcher(t)
	got, err := f.GetTopLevelIndex(t.Context())
	if err != nil {
		t.Fatalf("GetTopLevelIndex: %v", err)
	}
	if string(got.Body) != body {
		t.Errorf("body mismatch: got %q want %q", string(got.Body), body)
	}
	if got.ContentType != "text/html; charset=utf-8" {
		t.Errorf("ContentType = %q", got.ContentType)
	}
	if s.topLevelHits.Load() != 1 {
		t.Errorf("topLevelHits = %d, want 1", s.topLevelHits.Load())
	}
}

func TestGetVersionMetadata(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	pypi := `{
		"info": {"name": "requests"},
		"urls": [
			{
				"filename": "requests-2.31.0-py3-none-any.whl",
				"url": "https://files.pythonhosted.org/packages/.../requests-2.31.0-py3-none-any.whl",
				"digests": {"sha256": "abc123"},
				"size": 64909,
				"upload_time_iso_8601": "2023-05-22T15:12:44.123456Z"
			},
			{
				"filename": "requests-2.31.0.tar.gz",
				"url": "https://files.pythonhosted.org/packages/.../requests-2.31.0.tar.gz",
				"digests": {"sha256": "def456"},
				"size": 110586,
				"upload_time_iso_8601": "2023-05-22T15:12:42.123456Z"
			}
		]
	}`
	s.mux.HandleFunc("/pypi/requests/2.31.0/json", func(w http.ResponseWriter, r *http.Request) {
		s.metadataHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pypi)
	})
	s.mux.HandleFunc("/pypi/requests/9.9.9/json", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	s.mux.HandleFunc("/pypi/empty/1.0.0/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"info":{"name":"empty"},"urls":[]}`)
	})
	s.mux.HandleFunc("/pypi/bad/1.0.0/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	})

	f := s.newFetcher(t)

	meta, err := f.GetVersionMetadata(t.Context(), "requests", "2.31.0")
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if meta.Package != "requests" || meta.Version != "2.31.0" || len(meta.Files) != 2 {
		t.Errorf("unexpected meta: %+v", meta)
	}
	if meta.UploadTime.IsZero() {
		t.Errorf("UploadTime is zero")
	}
	// Earliest among the file timestamps.
	wantUpload, _ := time.Parse(time.RFC3339Nano, "2023-05-22T15:12:42.123456Z")
	if !meta.UploadTime.Equal(wantUpload) {
		t.Errorf("UploadTime = %v, want %v", meta.UploadTime, wantUpload)
	}

	// In-memory cache: second call shouldn't touch upstream.
	if _, err := f.GetVersionMetadata(t.Context(), "requests", "2.31.0"); err != nil {
		t.Fatalf("second GetVersionMetadata: %v", err)
	}
	if got := s.metadataHits.Load(); got != 1 {
		t.Errorf("metadataHits = %d, want 1 (cache should suppress second call)", got)
	}

	if _, err := f.GetVersionMetadata(t.Context(), "requests", "9.9.9"); !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("missing version: got %v, want ErrNotFound", err)
	}
	if _, err := f.GetVersionMetadata(t.Context(), "empty", "1.0.0"); !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("empty urls: got %v, want ErrNotFound", err)
	}
	if _, err := f.GetVersionMetadata(t.Context(), "bad", "1.0.0"); !errors.Is(err, proxy.ErrUpstreamMalformed) {
		t.Errorf("malformed json: got %v, want ErrUpstreamMalformed", err)
	}
}

func TestFetchFile(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	const want = "wheel-body-bytes"
	s.mux.HandleFunc("/files/requests-2.31.0-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		s.fileHits.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", "16")
		_, _ = io.WriteString(w, want)
	})
	s.mux.HandleFunc("/files/missing.whl", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	f := s.newFetcher(t)
	resp, err := f.FetchFile(t.Context(), s.URL+"/files/requests-2.31.0-py3-none-any.whl")
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	defer resp.Body.Close()
	if resp.ContentType != "application/zip" {
		t.Errorf("ContentType = %q", resp.ContentType)
	}
	if resp.ContentLength != 16 {
		t.Errorf("ContentLength = %d, want 16", resp.ContentLength)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	if _, err := f.FetchFile(t.Context(), s.URL+"/files/missing.whl"); !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("missing file: got %v, want ErrNotFound", err)
	}
	if _, err := f.FetchFile(t.Context(), ""); err == nil {
		t.Errorf("empty url: want error")
	}
}

func TestGetIndex_UpstreamErrorClassification(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	s.mux.HandleFunc("/simple/serverboom/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	s.mux.HandleFunc("/simple/teapot/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "teapot", http.StatusTeapot)
	})

	f := s.newFetcher(t)
	if _, err := f.GetSimpleIndex(t.Context(), "serverboom"); !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Errorf("5xx: got %v, want ErrUpstreamUnavailable", err)
	}
	// Unexpected 4xx (other than 404) classifies as unavailable —
	// the response shape isn't useful and the caller would just
	// repeat the request.
	if _, err := f.GetSimpleIndex(t.Context(), "teapot"); !errors.Is(err, proxy.ErrUpstreamUnavailable) {
		t.Errorf("unexpected 4xx: got %v, want ErrUpstreamUnavailable", err)
	}
}

func TestGetSimpleIndex_DefaultContentType(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	s.mux.HandleFunc("/simple/nctype/", func(w http.ResponseWriter, r *http.Request) {
		// Clear the default Content-Type that net/http sniffs.
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, "<html></html>")
	})
	f := s.newFetcher(t)
	resp, err := f.GetSimpleIndex(t.Context(), "nctype")
	if err != nil {
		t.Fatalf("GetSimpleIndex: %v", err)
	}
	if resp.ContentType != "text/html" {
		t.Errorf("ContentType = %q, want default text/html", resp.ContentType)
	}
}

func TestFetcher_UpstreamGetter(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("https://pypi.org/")
	f, err := New(u)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := f.Upstream().String(); got != "https://pypi.org" {
		t.Errorf("Upstream() = %q, want trailing-slash stripped form", got)
	}
}

// guard against accidental dependency on a global default client.
func TestFetcher_UsesProvidedClient(t *testing.T) {
	t.Parallel()
	s := newStubUpstream(t)
	s.mux.HandleFunc("/simple/x/", func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if !strings.Contains(ua, "custom-ua-for-test") {
			t.Errorf("User-Agent = %q, want substring custom-ua-for-test", ua)
		}
		_, _ = io.WriteString(w, "<html></html>")
	})
	client := httpclient.New(httpclient.Options{UserAgent: "custom-ua-for-test"})
	f := s.newFetcher(t, WithClient(client))
	if _, err := f.GetSimpleIndex(t.Context(), "x"); err != nil {
		t.Fatalf("GetSimpleIndex: %v", err)
	}
}
