package indexcache

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// zotImage is pinned to the same tag the pkg/oci streaming-integration
// test uses so a single container layer is shared between both
// packages' integration runs on the same CI host.
const zotImage = "ghcr.io/project-zot/zot-linux-amd64:v2.1.5"

// TestCache_ZotIntegration exercises the full Put / Get / overwrite
// lifecycle against a real zot registry. It is the live counterpart
// to the in-memory unit tests and the regression guard for the cache
// repo-name format — `ocifactory-proxy-cache/index/<pkg>` has to pass
// oras-go's repositoryRegexp at remote.NewRepository parse time,
// which the in-memory fake does not enforce.
//
// Pass `-short` to skip in local fast-iteration loops. CI runs this
// on every PR (ubuntu-latest has Docker preinstalled), so a wire-
// level regression in the cache's ORAS usage is caught before merge.
func TestCache_ZotIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-zot integration test in -short mode")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	zot, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        zotImage,
			ExposedPorts: []string{"5000/tcp"},
			WaitingFor: wait.ForHTTP("/v2/").
				WithPort("5000/tcp").
				WithStatusCodeMatcher(func(status int) bool { return status == 200 }).
				WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		// Skip rather than fail so `go test ./...` on a Docker-less
		// laptop stays green. CI always has Docker.
		t.Skipf("could not start zot container (Docker available?): %v", err)
	}
	t.Cleanup(func() {
		if err := zot.Terminate(context.Background()); err != nil {
			t.Logf("zot terminate: %v", err)
		}
	})

	host, err := zot.Host(ctx)
	if err != nil {
		t.Fatalf("zot.Host: %v", err)
	}
	port, err := zot.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("zot.MappedPort: %v", err)
	}
	base := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port.Port())}

	c, err := NewCache(base)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}

	t.Run("miss before any put", func(t *testing.T) {
		t.Parallel()
		_, _, _, found, err := c.Get(ctx, "ns-miss", "never-written")
		if err != nil {
			t.Fatalf("Get() on miss returned err = %v, want nil", err)
		}
		if found {
			t.Errorf("Get() on miss found = true, want false")
		}
	})

	t.Run("roundtrip", func(t *testing.T) {
		t.Parallel()
		wantBody := []byte("<html><body>roundtrip body</body></html>")
		wantContentType := "text/html; charset=utf-8"

		before := time.Now().UTC()
		if err := c.Put(ctx, "ns-roundtrip", "pkg", wantBody, wantContentType); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
		after := time.Now().UTC()

		gotBody, gotContentType, gotFetchedAt, found, err := c.Get(ctx, "ns-roundtrip", "pkg")
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
		// Wall-clock tolerance per the issue's acceptance: a few
		// seconds to absorb container round-trip and clock skew.
		tolerance := 5 * time.Second
		if gotFetchedAt.Before(before.Add(-tolerance)) || gotFetchedAt.After(after.Add(tolerance)) {
			t.Errorf("Get() fetchedAt = %v, want within [%v, %v]", gotFetchedAt, before.Add(-tolerance), after.Add(tolerance))
		}
	})

	t.Run("overwrite mutates the current tag", func(t *testing.T) {
		t.Parallel()
		if err := c.Put(ctx, "ns-overwrite", "pkg", []byte("v1"), "text/plain"); err != nil {
			t.Fatalf("Put() v1 error = %v", err)
		}
		// Sleep just enough that the second Put stamps a later
		// fetched-at and the assertion can tell v1 from v2 by
		// timestamp. RFC3339Nano keeps sub-second resolution; 10ms
		// is plenty even after RTT.
		time.Sleep(10 * time.Millisecond)
		if err := c.Put(ctx, "ns-overwrite", "pkg", []byte("v2"), "application/json"); err != nil {
			t.Fatalf("Put() v2 error = %v", err)
		}

		gotBody, gotContentType, _, found, err := c.Get(ctx, "ns-overwrite", "pkg")
		if err != nil {
			t.Fatalf("Get() after overwrite error = %v", err)
		}
		if !found {
			t.Fatal("Get() after overwrite found = false, want true")
		}
		if diff := cmp.Diff([]byte("v2"), gotBody); diff != "" {
			t.Errorf("Get() body mismatch (-want +got):\n%s", diff)
		}
		if gotContentType != "application/json" {
			t.Errorf("Get() contentType = %q, want %q", gotContentType, "application/json")
		}
	})

	t.Run("namespace isolation", func(t *testing.T) {
		t.Parallel()
		if err := c.Put(ctx, "ns-iso-a", "shared", []byte("in-a"), "text/plain"); err != nil {
			t.Fatalf("Put() ns-iso-a error = %v", err)
		}
		if err := c.Put(ctx, "ns-iso-b", "shared", []byte("in-b"), "text/plain"); err != nil {
			t.Fatalf("Put() ns-iso-b error = %v", err)
		}

		gotA, _, _, found, err := c.Get(ctx, "ns-iso-a", "shared")
		if err != nil {
			t.Fatalf("Get(ns-iso-a) error = %v", err)
		}
		if !found {
			t.Fatal("Get(ns-iso-a) found = false, want true")
		}
		if diff := cmp.Diff([]byte("in-a"), gotA); diff != "" {
			t.Errorf("Get(ns-iso-a) body mismatch (-want +got):\n%s", diff)
		}

		gotB, _, _, found, err := c.Get(ctx, "ns-iso-b", "shared")
		if err != nil {
			t.Fatalf("Get(ns-iso-b) error = %v", err)
		}
		if !found {
			t.Fatal("Get(ns-iso-b) found = false, want true")
		}
		if diff := cmp.Diff([]byte("in-b"), gotB); diff != "" {
			t.Errorf("Get(ns-iso-b) body mismatch (-want +got):\n%s", diff)
		}
	})
}
