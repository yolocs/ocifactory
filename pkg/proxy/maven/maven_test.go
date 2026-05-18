package maven

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/yolocs/ocifactory/pkg/proxy"
)

func TestFetcherGetMetadata(t *testing.T) {
	t.Parallel()

	const metadataBody = `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>com.example</groupId>
  <artifactId>demo</artifactId>
  <versioning>
    <latest>1.2.0</latest>
    <release>1.2.0</release>
    <versions>
      <version>1.0.0</version>
      <version>1.2.0</version>
    </versions>
    <lastUpdated>20260516040506</lastUpdated>
  </versioning>
</metadata>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/com/example/demo/maven-metadata.xml"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(metadataBody))
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}
	f, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := f.GetMetadata(t.Context(), "com/example/demo")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	wantUpdated := time.Date(2026, 5, 16, 4, 5, 6, 0, time.UTC)
	want := &MetadataResponse{
		Body:        []byte(metadataBody),
		ContentType: "text/xml",
		Versions:    []string{"1.0.0", "1.2.0"},
		LastUpdated: wantUpdated,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetMetadata mismatch (-want +got):\n%s", diff)
	}
}

func TestFetcherFetchFile(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/com/example/demo/1.2.0/demo-1.2.0.jar"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/java-archive")
		w.Header().Set("Content-Length", "7")
		_, _ = w.Write([]byte("jarbody"))
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}
	f, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := f.FetchFile(t.Context(), "com/example/demo", "1.2.0", "demo-1.2.0.jar")
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	want := &FileResponse{
		Body:          resp.Body,
		ContentType:   "application/java-archive",
		ContentLength: 7,
	}
	if diff := cmp.Diff(want, resp, cmpopts.IgnoreFields(FileResponse{}, "Body")); diff != "" {
		t.Errorf("FetchFile response mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]byte("jarbody"), body); diff != "" {
		t.Errorf("FetchFile body mismatch (-want +got):\n%s", diff)
	}
}

func TestFetcherMapsUpstreamStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{
			name:       "not found",
			statusCode: http.StatusNotFound,
			wantErr:    proxy.ErrNotFound,
		},
		{
			name:       "unavailable",
			statusCode: http.StatusBadGateway,
			wantErr:    proxy.ErrUpstreamUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
			}))
			defer upstream.Close()

			base, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatalf("Parse upstream URL: %v", err)
			}
			f, err := New(base)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = f.GetMetadata(t.Context(), "com/example/demo")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetMetadata error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}

			_, err = f.FetchFile(t.Context(), "com/example/demo", "1.2.0", "demo-1.2.0.jar")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("FetchFile error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}
