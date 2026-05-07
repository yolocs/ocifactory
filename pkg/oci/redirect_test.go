package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

// fakeBackend is a minimal HTTP server that answers HEAD against
// /v2/<repo>/blobs/<digest> with whatever the test configured. Returned
// from newFakeBackend so tests can mutate behaviour mid-flight.
type fakeBackend struct {
	server *httptest.Server

	// status is the status code the next HEAD request will get.
	status int
	// location is the Location header set on 3xx responses.
	location string
	// hits records HEAD-on-blob requests so tests can assert HEAD vs. GET.
	hits int
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{status: http.StatusOK}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /v2/ probe — accept anything (oras-go pings this on auth setup).
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Only HEAD on /v2/.../blobs/<digest> matters for the redirect probe.
		if r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/") {
			fb.hits++
			if fb.location != "" {
				w.Header().Set("Location", fb.location)
			}
			w.WriteHeader(fb.status)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fb.server.Close)
	return fb
}

func (b *fakeBackend) baseURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(b.server.URL)
	if err != nil {
		t.Fatalf("parse fake backend URL: %v", err)
	}
	return u
}

// newRedirectTestRegistry seeds a Registry whose newBackendFunc returns
// a shared in-memory store (so AddFile / referrers resolution works
// without touching the network) but whose baseURL points at fb so the
// HTTP redirect probe lands on the fake backend.
func newRedirectTestRegistry(t *testing.T, fb *fakeBackend, opts ...RegistryOption) (*Registry, *inMemoryRepo) {
	t.Helper()
	r, err := NewRegistry(fb.baseURL(t), opts...)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{}}
	r.newBackendFunc = func(_ context.Context, _ *RepoFile) (destRepo, error) {
		return memRepo, nil
	}
	return r, memRepo
}

func seedFile(t *testing.T, r *Registry) *RepoFile {
	t.Helper()
	ctx := t.Context()
	f := &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v1",
		Name:       "thing.txt",
	}
	if _, err := r.AddFile(ctx, f, strings.NewReader("hello world")); err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}
	return f
}

func TestBlobRedirectURL_Redirected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fb := newFakeBackend(t)
	fb.status = http.StatusTemporaryRedirect
	fb.location = "https://cdn.example.com/blob?signature=xyz"

	rec := &fakeRecorder{}
	r, _ := newRedirectTestRegistry(t, fb, WithMetrics(rec))
	f := seedFile(t, r)

	got, err := r.BlobRedirectURL(ctx, f)
	if err != nil {
		t.Fatalf("BlobRedirectURL() error = %v", err)
	}
	if want := "https://cdn.example.com/blob?signature=xyz"; got != want {
		t.Errorf("BlobRedirectURL() = %q, want %q", got, want)
	}
	if fb.hits != 1 {
		t.Errorf("fake backend hits = %d, want 1", fb.hits)
	}
	if diff := cmp.Diff([]string{BlobRedirectOutcomeRedirected}, rec.redirectOutcomes()); diff != "" {
		t.Errorf("redirect outcomes mismatch (-want +got):\n%s", diff)
	}
}

func TestBlobRedirectURL_InlineBackend(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fb := newFakeBackend(t)
	fb.status = http.StatusOK

	rec := &fakeRecorder{}
	r, _ := newRedirectTestRegistry(t, fb, WithMetrics(rec))
	f := seedFile(t, r)

	got, err := r.BlobRedirectURL(ctx, f)
	if err != nil {
		t.Fatalf("BlobRedirectURL() error = %v", err)
	}
	if got != "" {
		t.Errorf("BlobRedirectURL() = %q, want empty (inline backend)", got)
	}
	if diff := cmp.Diff([]string{BlobRedirectOutcomeInline}, rec.redirectOutcomes()); diff != "" {
		t.Errorf("redirect outcomes mismatch (-want +got):\n%s", diff)
	}
}

func TestBlobRedirectURL_Disabled(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fb := newFakeBackend(t)
	fb.status = http.StatusTemporaryRedirect
	fb.location = "https://cdn.example.com/should-not-be-followed"

	rec := &fakeRecorder{}
	r, _ := newRedirectTestRegistry(t, fb, WithMetrics(rec), WithBlobRedirectDisabled(true))
	f := seedFile(t, r)

	got, err := r.BlobRedirectURL(ctx, f)
	if err != nil {
		t.Fatalf("BlobRedirectURL() error = %v", err)
	}
	if got != "" {
		t.Errorf("BlobRedirectURL() = %q, want empty (disabled)", got)
	}
	if fb.hits != 0 {
		t.Errorf("fake backend hits = %d, want 0 (probe should be skipped)", fb.hits)
	}
	if outcomes := rec.redirectOutcomes(); len(outcomes) != 0 {
		t.Errorf("redirect outcomes = %v, want none (disabled path emits no metric)", outcomes)
	}
}

func TestBlobRedirectURL_NotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fb := newFakeBackend(t)
	rec := &fakeRecorder{}
	r, _ := newRedirectTestRegistry(t, fb, WithMetrics(rec))
	seedFile(t, r) // seed something else

	missing := &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v1",
		Name:       "does-not-exist.txt",
	}
	got, err := r.BlobRedirectURL(ctx, missing)
	if err == nil {
		t.Fatalf("BlobRedirectURL() = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("BlobRedirectURL() error = %v, want not-found", err)
	}
	if diff := cmp.Diff([]string{BlobRedirectOutcomeError}, rec.redirectOutcomes()); diff != "" {
		t.Errorf("redirect outcomes mismatch (-want +got):\n%s", diff)
	}
}

func TestBlobRedirectURL_BackendError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fb := newFakeBackend(t)
	// 418 is a non-retryable status — keeps the test fast. 5xx
	// triggers oras-go's retry policy, which would multiply the test
	// run time by the retry budget.
	fb.status = http.StatusTeapot

	rec := &fakeRecorder{}
	r, _ := newRedirectTestRegistry(t, fb, WithMetrics(rec))
	f := seedFile(t, r)

	got, err := r.BlobRedirectURL(ctx, f)
	if err == nil {
		t.Fatalf("BlobRedirectURL() = %q, want error", got)
	}
	if got != "" {
		t.Errorf("BlobRedirectURL() = %q on error, want empty", got)
	}
	if diff := cmp.Diff([]string{BlobRedirectOutcomeError}, rec.redirectOutcomes()); diff != "" {
		t.Errorf("redirect outcomes mismatch (-want +got):\n%s", diff)
	}
}

func TestBlobRedirectURL_RedirectMissingLocation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fb := newFakeBackend(t)
	fb.status = http.StatusTemporaryRedirect
	fb.location = "" // 3xx with no Location → treat as error.

	r, _ := newRedirectTestRegistry(t, fb)
	f := seedFile(t, r)

	if _, err := r.BlobRedirectURL(ctx, f); err == nil {
		t.Fatalf("BlobRedirectURL() error = nil, want error for missing Location")
	}
}

// TestBlobRedirectOutcome covers the small classifier directly so
// future plumbing changes can't regress the metric labels without an
// obvious test failure.
func TestBlobRedirectOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		err  error
		want string
	}{
		{name: "redirected", url: "https://cdn.example.com/x", want: BlobRedirectOutcomeRedirected},
		{name: "inline", url: "", want: BlobRedirectOutcomeInline},
		{name: "error wins over url", url: "https://cdn.example.com/x", err: errdef.ErrNotFound, want: BlobRedirectOutcomeError},
		{name: "error", url: "", err: errdef.ErrNotFound, want: BlobRedirectOutcomeError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := blobRedirectOutcome(tc.url, tc.err); got != tc.want {
				t.Errorf("blobRedirectOutcome(%q, %v) = %q, want %q", tc.url, tc.err, got, tc.want)
			}
		})
	}
}
