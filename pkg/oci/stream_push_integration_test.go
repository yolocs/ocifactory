package oci

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// zotImage is the image used by the streaming integration test. zot is a
// CNCF Sandbox OCI registry that implements the full distribution-spec
// v1.1.1 — including chunked PATCH — and runs anonymously by default.
//
// Pinned to a specific tag rather than :latest so the test is reproducible
// across machines and CI runs. Bump intentionally.
const zotImage = "ghcr.io/project-zot/zot-linux-amd64:v2.1.5"

// TestAddFile_StreamingIntegration_Zot pushes a body well above any
// reasonable RAM ceiling through Registry.AddFile against a real zot
// registry and reads it back, verifying the streaming chunked PATCH
// pusher round-trips correctly end-to-end.
//
// It is the live counterpart to the httptest-fake unit tests in
// stream_push_test.go and satisfies issue #38's acceptance criterion of
// an integration test against zot. It spins up zot via testcontainers-go
// on every run, so it requires Docker (or another OCI-compatible runtime
// that testcontainers can talk to) to be available.
//
// Pass `-short` to skip it in local fast-iteration loops. CI runs every
// PR (ubuntu-latest has Docker preinstalled), so regressions in the
// chunked-PATCH wire path are caught before merge.
func TestAddFile_StreamingIntegration_Zot(t *testing.T) {
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
			// zot serves /v2/ as a 200 once the storage backend is
			// initialised — that's the cheapest readiness probe.
			WaitingFor: wait.ForHTTP("/v2/").
				WithPort("5000/tcp").
				WithStatusCodeMatcher(func(status int) bool { return status == 200 }).
				WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		// Most common reason this fails on a developer laptop is that
		// Docker isn't installed or the daemon isn't running. Skip
		// rather than fail — a missing local runtime shouldn't break
		// `go test ./...`.
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

	r, err := NewRegistry(base)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	tests := []struct {
		name string
		size int
	}{
		// Sized to fall on either side of uploadMemThreshold (4 MiB).
		// The small case takes the buffered + monolithic path; the
		// large case is the one this whole feature is about — a body
		// that would have pinned ~32 MiB of RAM under the old temp-file
		// flow streams cleanly via chunked PATCH.
		{name: "small body via buffered path", size: 1 * 1024 * 1024},
		{name: "large body via streaming path", size: 32 * 1024 * 1024},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := make([]byte, tc.size)
			if _, err := io.ReadFull(rand.Reader, body); err != nil {
				t.Fatalf("rand.Read: %v", err)
			}

			f := &RepoFile{
				OwningRepo: fmt.Sprintf("ocifactory/integration/%d", tc.size),
				OwningTag:  "v1",
				Name:       "blob.bin",
			}

			desc, err := r.AddFile(ctx, f, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("AddFile() error = %v", err)
			}
			if desc.File.Size != int64(tc.size) {
				t.Errorf("AddFile() desc.File.Size = %d, want %d", desc.File.Size, tc.size)
			}
			wantDigest := sha256Digest(body)
			if desc.File.Digest != wantDigest {
				t.Errorf("AddFile() desc.File.Digest = %s, want %s", desc.File.Digest, wantDigest)
			}

			gotDesc, rc, err := r.ReadFile(ctx, f)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			defer rc.Close()
			if gotDesc.File.Digest != wantDigest {
				t.Errorf("ReadFile() desc.File.Digest = %s, want %s", gotDesc.File.Digest, wantDigest)
			}
			gotBody, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if !bytes.Equal(body, gotBody) {
				t.Errorf("ReadFile body length = %d, want %d (digest match=%v)",
					len(gotBody), len(body), sha256Digest(gotBody) == wantDigest)
			}
		})
	}
}
