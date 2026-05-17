package npm

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/ocifactory/pkg/proxy"
)

func TestFetcherGetPackument(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/left-pad" {
			t.Errorf("path = %q, want /left-pad", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.npm.install-v1+json, application/json" {
			t.Errorf("Accept = %q, want npm packument accept", got)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"name":"left-pad","versions":{"1.0.0":{"name":"left-pad","version":"1.0.0"}}}`))
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	f, err := New(u)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := f.GetPackument(t.Context(), "left-pad")
	if err != nil {
		t.Fatalf("GetPackument: %v", err)
	}

	want := &PackumentResponse{
		Body:        []byte(`{"name":"left-pad","versions":{"1.0.0":{"name":"left-pad","version":"1.0.0"}}}`),
		ContentType: "application/json; charset=utf-8",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetPackument mismatch (-want +got):\n%s", diff)
	}
}

func TestFetcherFetchTarballUsesPackumentURL(t *testing.T) {
	t.Parallel()

	const body = "fake tgz"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@scope/pkg":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"name": "@scope/pkg",
				"time": {"1.2.3": "2026-05-01T02:03:04.000Z"},
				"versions": {
					"1.2.3": {
						"name": "@scope/pkg",
							"version": "1.2.3",
							"dist": {
							"tarball": "http://` + r.Host + `/@scope/pkg/-/pkg-1.2.3.tgz",
							"shasum": "abc"
						}
					}
				}
			}`))
		case "/@scope/pkg/-/pkg-1.2.3.tgz":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "8")
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	f, err := New(u)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := f.FetchTarball(t.Context(), "@scope/pkg", "1.2.3", "pkg-1.2.3.tgz")
	if err != nil {
		t.Fatalf("FetchTarball: %v", err)
	}
	defer got.Body.Close()
	gotBody, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(gotBody) != body {
		t.Errorf("body = %q, want %q", string(gotBody), body)
	}
	if got.ContentType != "application/octet-stream" {
		t.Errorf("ContentType = %q, want application/octet-stream", got.ContentType)
	}
	if got.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", got.ContentLength, len(body))
	}
	if string(got.Version) == "" {
		t.Errorf("Version metadata is empty")
	}
	if got.UploadTime.IsZero() {
		t.Errorf("UploadTime is zero, want parsed packument time")
	}
}

func TestFetcherFetchTarballClassifiesErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   error
	}{
		{name: "missing packument", status: http.StatusNotFound, want: proxy.ErrNotFound},
		{name: "upstream unavailable", status: http.StatusBadGateway, want: proxy.ErrUpstreamUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tc.status)
			}))
			t.Cleanup(upstream.Close)

			u, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			f, err := New(u)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = f.FetchTarball(t.Context(), "left-pad", "1.0.0", "left-pad-1.0.0.tgz")
			if !errors.Is(err, tc.want) {
				t.Errorf("FetchTarball error = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}
