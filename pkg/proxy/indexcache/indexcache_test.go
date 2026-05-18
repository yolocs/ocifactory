package indexcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// inMemoryTargets is the test factory passed to Cache.newTargetFunc.
// Each (repoPath) gets its own memory.Store so namespace and package
// isolation surface naturally — two cache entries in different
// namespaces (or in the same namespace but different packages) live
// in different stores, exactly like they would on a real OCI backend.
type inMemoryTargets struct {
	mu     sync.Mutex
	stores map[string]*memory.Store
}

func newInMemoryTargets() *inMemoryTargets {
	return &inMemoryTargets{stores: map[string]*memory.Store{}}
}

func (f *inMemoryTargets) open(_ context.Context, repoPath string) (oras.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.stores[repoPath]; ok {
		return s, nil
	}
	s := memory.New()
	f.stores[repoPath] = s
	return s, nil
}

// newTestCache builds a Cache wired to in-memory targets, with the
// wall-clock pinned so tests can assert fetchedAt exactly. The
// returned tick helper advances the cache's clock so tests that
// overwrite an entry can distinguish v1 and v2 by timestamp.
func newTestCache(t *testing.T) (*Cache, func(time.Time)) {
	t.Helper()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	c, err := NewCache(&url.URL{Scheme: "https", Host: "registry.example.com"})
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	tick := func(next time.Time) {
		mu.Lock()
		defer mu.Unlock()
		now = next
	}
	c.newTargetFunc = newInMemoryTargets().open
	return c, tick
}

func TestNewCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL *url.URL
		wantErr bool
	}{
		{
			name:    "https baseURL",
			baseURL: &url.URL{Scheme: "https", Host: "example.com"},
		},
		{
			name:    "http baseURL switches to plain HTTP",
			baseURL: &url.URL{Scheme: "http", Host: "zot.local:5000"},
		},
		{
			name:    "nil baseURL is rejected",
			baseURL: nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewCache(tc.baseURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewCache() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewCache() error = %v", err)
			}
			if c == nil {
				t.Fatal("NewCache() = nil, want non-nil")
			}
			if c.authClient == nil {
				t.Error("NewCache() authClient = nil, want non-nil (default anonymous)")
			}
			if c.newTargetFunc == nil {
				t.Error("NewCache() newTargetFunc = nil, want non-nil")
			}
			wantPlainHTTP := tc.baseURL.Scheme == "http"
			if c.plainHTTP != wantPlainHTTP {
				t.Errorf("NewCache() plainHTTP = %v, want %v", c.plainHTTP, wantPlainHTTP)
			}
		})
	}
}

func TestCache_RepoPathRejectsTraversal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		namespace string
		pkg       string
	}{
		{name: "namespace traversal", namespace: "../beta", pkg: "requests"},
		{name: "package traversal", namespace: "alpha", pkg: "../../../beta/packages/foo"},
		{name: "absolute package path", namespace: "alpha", pkg: "/beta/packages/foo"},
		{name: "unclean package path", namespace: "alpha", pkg: "packages/../foo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, _ := newTestCache(t)
			if _, err := c.repoPath(tc.namespace, tc.pkg); err == nil {
				t.Fatalf("repoPath(%q, %q) error = nil, want non-nil", tc.namespace, tc.pkg)
			}
		})
	}
}

func TestCache_GetMiss(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)

	body, contentType, fetchedAt, found, err := c.Get(ctx, "default", "missing")
	if err != nil {
		t.Fatalf("Get() on miss returned err = %v, want nil", err)
	}
	if found {
		t.Errorf("Get() on miss found = true, want false")
	}
	if body != nil {
		t.Errorf("Get() on miss body = %q, want nil", body)
	}
	if contentType != "" {
		t.Errorf("Get() on miss contentType = %q, want empty", contentType)
	}
	if !fetchedAt.IsZero() {
		t.Errorf("Get() on miss fetchedAt = %v, want zero", fetchedAt)
	}
}

func TestCache_PutGetRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)

	wantBody := []byte("<html><body>simple-index</body></html>")
	wantContentType := "text/html; charset=utf-8"
	// The cache stamps Cache.now() (pinned by newTestCache) onto
	// FetchedAtAnnotation; capturing it here lets the assertion
	// below compare exactly. Tolerance against the real wall clock
	// is covered by TestCache_FetchedAtWallClock.
	wantFetchedAt := c.now()

	if err := c.Put(ctx, "default", "requests", wantBody, wantContentType); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	gotBody, gotContentType, gotFetchedAt, found, err := c.Get(ctx, "default", "requests")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !found {
		t.Fatal("Get() found = false, want true")
	}
	if diff := cmp.Diff(wantBody, gotBody); diff != "" {
		t.Errorf("Get() body mismatch (-want +got):\n%s", diff)
	}
	if gotContentType != wantContentType {
		t.Errorf("Get() contentType = %q, want %q", gotContentType, wantContentType)
	}
	if !gotFetchedAt.Equal(wantFetchedAt) {
		t.Errorf("Get() fetchedAt = %v, want %v", gotFetchedAt, wantFetchedAt)
	}
}

func TestCache_PutOverwrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, tick := newTestCache(t)

	if err := c.Put(ctx, "default", "requests", []byte("v1"), "text/plain"); err != nil {
		t.Fatalf("Put() first error = %v", err)
	}

	// Advance the clock so the second Put stamps a different
	// fetchedAt and the assertion below can distinguish v1 from v2
	// by timestamp alone.
	newNow := c.now().Add(5 * time.Second)
	tick(newNow)

	if err := c.Put(ctx, "default", "requests", []byte("v2-overwrite"), "application/json"); err != nil {
		t.Fatalf("Put() second error = %v", err)
	}

	gotBody, gotContentType, gotFetchedAt, found, err := c.Get(ctx, "default", "requests")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !found {
		t.Fatal("Get() found = false, want true")
	}
	if diff := cmp.Diff([]byte("v2-overwrite"), gotBody); diff != "" {
		t.Errorf("Get() body mismatch (-want +got):\n%s", diff)
	}
	if gotContentType != "application/json" {
		t.Errorf("Get() contentType = %q, want %q", gotContentType, "application/json")
	}
	if !gotFetchedAt.Equal(newNow) {
		t.Errorf("Get() fetchedAt = %v, want %v", gotFetchedAt, newNow)
	}
}

func TestCache_NamespaceIsolation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)

	if err := c.Put(ctx, "ns-a", "shared-name", []byte("in-a"), "text/plain"); err != nil {
		t.Fatalf("Put() in ns-a error = %v", err)
	}
	if err := c.Put(ctx, "ns-b", "shared-name", []byte("in-b"), "text/plain"); err != nil {
		t.Fatalf("Put() in ns-b error = %v", err)
	}

	// ns-a sees ns-a's entry only.
	gotA, _, _, foundA, err := c.Get(ctx, "ns-a", "shared-name")
	if err != nil {
		t.Fatalf("Get() in ns-a error = %v", err)
	}
	if !foundA {
		t.Fatal("Get() in ns-a found = false, want true")
	}
	if diff := cmp.Diff([]byte("in-a"), gotA); diff != "" {
		t.Errorf("Get() ns-a body mismatch (-want +got):\n%s", diff)
	}

	// ns-b sees ns-b's entry only.
	gotB, _, _, foundB, err := c.Get(ctx, "ns-b", "shared-name")
	if err != nil {
		t.Fatalf("Get() in ns-b error = %v", err)
	}
	if !foundB {
		t.Fatal("Get() in ns-b found = false, want true")
	}
	if diff := cmp.Diff([]byte("in-b"), gotB); diff != "" {
		t.Errorf("Get() ns-b body mismatch (-want +got):\n%s", diff)
	}

	// A third namespace with the same package name is still a miss.
	_, _, _, foundC, err := c.Get(ctx, "ns-c", "shared-name")
	if err != nil {
		t.Fatalf("Get() in ns-c error = %v", err)
	}
	if foundC {
		t.Error("Get() in ns-c found = true, want false (never written)")
	}
}

func TestCache_PackageIsolation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)

	if err := c.Put(ctx, "default", "alpha", []byte("alpha-body"), "text/plain"); err != nil {
		t.Fatalf("Put(alpha) error = %v", err)
	}
	if err := c.Put(ctx, "default", "beta", []byte("beta-body"), "text/plain"); err != nil {
		t.Fatalf("Put(beta) error = %v", err)
	}

	gotAlpha, _, _, found, err := c.Get(ctx, "default", "alpha")
	if err != nil {
		t.Fatalf("Get(alpha) error = %v", err)
	}
	if !found {
		t.Fatal("Get(alpha) found = false, want true")
	}
	if diff := cmp.Diff([]byte("alpha-body"), gotAlpha); diff != "" {
		t.Errorf("Get(alpha) body mismatch (-want +got):\n%s", diff)
	}

	gotBeta, _, _, found, err := c.Get(ctx, "default", "beta")
	if err != nil {
		t.Fatalf("Get(beta) error = %v", err)
	}
	if !found {
		t.Fatal("Get(beta) found = false, want true")
	}
	if diff := cmp.Diff([]byte("beta-body"), gotBeta); diff != "" {
		t.Errorf("Get(beta) body mismatch (-want +got):\n%s", diff)
	}
}

func TestCache_FetchedAtWallClock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Construct a Cache without overriding c.now so the production
	// time.Now path stamps the manifest. The acceptance criterion
	// is "preserved within a small wall-clock tolerance" — sub-second
	// is plenty given everything runs in-process.
	c, err := NewCache(&url.URL{Scheme: "https", Host: "registry.example.com"})
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	targets := newInMemoryTargets()
	c.newTargetFunc = targets.open

	before := time.Now().UTC()
	if err := c.Put(ctx, "default", "pkg", []byte("body"), "text/plain"); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	after := time.Now().UTC()

	_, _, fetchedAt, found, err := c.Get(ctx, "default", "pkg")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !found {
		t.Fatal("Get() found = false, want true")
	}
	// Allow a 1s window on each side to absorb sub-microsecond
	// truncation in RFC3339Nano + any scheduling latency between
	// time.Now reads. The test is asserting "the timestamp survives
	// the roundtrip", not microsecond fidelity.
	tolerance := time.Second
	if fetchedAt.Before(before.Add(-tolerance)) || fetchedAt.After(after.Add(tolerance)) {
		t.Errorf("Get() fetchedAt = %v, want within [%v, %v]", fetchedAt, before.Add(-tolerance), after.Add(tolerance))
	}
}

func TestCache_EmptyBody(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)

	if err := c.Put(ctx, "default", "empty", []byte{}, "text/plain"); err != nil {
		t.Fatalf("Put(empty) error = %v", err)
	}

	body, contentType, _, found, err := c.Get(ctx, "default", "empty")
	if err != nil {
		t.Fatalf("Get(empty) error = %v", err)
	}
	if !found {
		t.Fatal("Get(empty) found = false, want true")
	}
	if len(body) != 0 {
		t.Errorf("Get(empty) body = %q, want empty", body)
	}
	if contentType != "text/plain" {
		t.Errorf("Get(empty) contentType = %q, want %q", contentType, "text/plain")
	}
}

func TestCache_RepoPath(t *testing.T) {
	t.Parallel()
	c, err := NewCache(&url.URL{Scheme: "https", Host: "example.com"})
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	tests := []struct {
		name string
		ns   string
		pkg  string
		want string
	}{
		{
			name: "simple ns and pkg",
			ns:   "default",
			pkg:  "requests",
			want: "default/ocifactory-proxy-cache/index/requests",
		},
		{
			name: "scoped npm package",
			ns:   "default",
			pkg:  "@scope/name",
			want: "default/ocifactory-proxy-cache/index/@scope/name",
		},
		{
			name: "maven groupId path",
			ns:   "internal",
			pkg:  "com/google/guava/guava",
			want: "internal/ocifactory-proxy-cache/index/com/google/guava/guava",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := c.repoPath(tc.ns, tc.pkg)
			if err != nil {
				t.Fatalf("repoPath(%q, %q) error = %v, want nil", tc.ns, tc.pkg, err)
			}
			if got != tc.want {
				t.Errorf("repoPath(%q, %q) = %q, want %q", tc.ns, tc.pkg, got, tc.want)
			}
		})
	}
}

func TestCache_GetTargetOpenError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)
	sentinel := errors.New("backend exploded")
	c.newTargetFunc = func(_ context.Context, _ string) (oras.Target, error) {
		return nil, sentinel
	}

	_, _, _, found, err := c.Get(ctx, "default", "foo")
	if err == nil {
		t.Fatal("Get() error = nil, want target-open failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Get() error = %v, want wrap of %v", err, sentinel)
	}
	if found {
		t.Errorf("Get() found = true, want false on error")
	}
}

func TestCache_PutTargetOpenError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, _ := newTestCache(t)
	sentinel := errors.New("backend exploded")
	c.newTargetFunc = func(_ context.Context, _ string) (oras.Target, error) {
		return nil, sentinel
	}

	err := c.Put(ctx, "default", "foo", []byte("body"), "text/plain")
	if err == nil {
		t.Fatal("Put() error = nil, want target-open failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Put() error = %v, want wrap of %v", err, sentinel)
	}
}

// TestCache_GetMalformedManifest pre-populates the in-memory target
// with a manifest tagged `current` whose contents violate one of the
// Get-side invariants (wrong artifactType, wrong layer count, missing
// or bad fetched-at annotation) and checks Get surfaces it as an
// error rather than silently returning bad data. These states can't
// arise via Put, but a future bug or an operator scribbling on the
// cache repo with `oras` would produce them.
func TestCache_GetMalformedManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		mutate         func(m *ocispec.Manifest)
		wantErrSubstr  string
		wantFoundFalse bool
	}{
		{
			name: "wrong artifactType",
			mutate: func(m *ocispec.Manifest) {
				m.ArtifactType = "application/vnd.somebody.else"
			},
			wantErrSubstr: "unexpected artifactType",
		},
		{
			name: "two layers",
			mutate: func(m *ocispec.Manifest) {
				m.Layers = append(m.Layers, m.Layers[0])
			},
			wantErrSubstr: "manifest has 2 layers",
		},
		{
			name: "missing fetched-at annotation",
			mutate: func(m *ocispec.Manifest) {
				delete(m.Annotations, FetchedAtAnnotation)
			},
			wantErrSubstr: "missing proxy.cache.fetched-at",
		},
		{
			name: "malformed fetched-at annotation",
			mutate: func(m *ocispec.Manifest) {
				m.Annotations[FetchedAtAnnotation] = "not a timestamp"
			},
			wantErrSubstr: "parse proxy.cache.fetched-at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			store := memory.New()
			c, err := NewCache(&url.URL{Scheme: "https", Host: "example.com"})
			if err != nil {
				t.Fatalf("NewCache() error = %v", err)
			}
			c.newTargetFunc = func(_ context.Context, _ string) (oras.Target, error) {
				return store, nil
			}

			seedMalformedManifest(t, store, tc.mutate)

			_, _, _, found, err := c.Get(ctx, "default", "pkg")
			if err == nil {
				t.Fatalf("Get() error = nil, want %q", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("Get() error = %q, want substring %q", err, tc.wantErrSubstr)
			}
			if found {
				t.Errorf("Get() found = true, want false on malformed manifest")
			}
		})
	}
}

// seedMalformedManifest writes a well-formed cache manifest into
// store, then lets the caller mutate it before pushing + tagging. The
// well-formed baseline keeps every test focused on the one field it
// is breaking.
func seedMalformedManifest(t *testing.T, store *memory.Store, mutate func(*ocispec.Manifest)) {
	t.Helper()
	ctx := t.Context()

	body := []byte("payload")
	layerDesc := content.NewDescriptorFromBytes(layerMediaType, body)
	if err := store.Push(ctx, layerDesc, bytes.NewReader(body)); err != nil {
		t.Fatalf("seed: push body: %v", err)
	}

	emptyDesc := ocispec.DescriptorEmptyJSON
	if err := store.Push(ctx, emptyDesc, bytes.NewReader(emptyDesc.Data)); err != nil {
		t.Fatalf("seed: push config: %v", err)
	}

	manifest := ocispec.Manifest{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       emptyDesc,
		Layers:       []ocispec.Descriptor{layerDesc},
		Annotations: map[string]string{
			FetchedAtAnnotation:   time.Now().UTC().Format(time.RFC3339Nano),
			ContentTypeAnnotation: "text/plain",
		},
	}
	mutate(&manifest)

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("seed: marshal manifest: %v", err)
	}
	manifestDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBytes)
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatalf("seed: push manifest: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, CacheTag); err != nil {
		t.Fatalf("seed: tag manifest: %v", err)
	}
}
