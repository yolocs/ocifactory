// Package integrationtest is a shared helper for the per-format
// real-client integration tests. It spins up a zot OCI registry via
// testcontainers-go, builds the ocifactory binary into a temp dir, and
// starts it pointed at zot so the per-format test only has to drive
// the client subprocess (pip / twine / mvn / ...).
//
// All exports take a *testing.T; the package isn't intended for use
// outside tests. It lives in pkg/handler so format-specific tests can
// import it without dragging in a separate top-level integration tree.
package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/namespace/nsutil"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// zotImage matches the version pinned by pkg/oci's streaming
// integration test so both suites exercise the same registry build.
const zotImage = "ghcr.io/project-zot/zot-linux-amd64:v2.1.5"

// binaryBuildOnce makes sure the ocifactory binary is built at most
// once per test process, even when several integration tests run in
// parallel. The result is cached in builtBinaryPath / builtBinaryErr.
var (
	binaryBuildOnce sync.Once
	builtBinaryPath string
	builtBinaryErr  error
)

// Harness is a running zot + ocifactory pair ready for a real client
// to talk to. Returned by Start; cleanup is registered via t.Cleanup.
type Harness struct {
	// OcifactoryURL is the http://host:port the ocifactory subprocess
	// listens on. Pass it to pip / mvn / twine.
	OcifactoryURL *url.URL

	// ZotURL is the URL to the underlying zot registry. Tests rarely
	// need this directly; it's exported for assertions that want to
	// poke the OCI backend behind ocifactory.
	ZotURL *url.URL

	// BackendRepo is the repo prefix passed to ocifactory via
	// --backend-registry. zot serves it under /v2/<BackendRepo>/...
	BackendRepo string

	logFile *os.File
}

// Start brings up zot and ocifactory and returns a Harness pointing at
// both. It self-skips when Docker isn't reachable (developer laptop)
// and t.Fatals on any other unrecoverable setup failure.
//
// repoType selects which format ocifactory serves (one of
// "python", "maven", ...). extraArgs are appended verbatim to the
// ocifactory command line — use them for format-specific knobs the
// caller wants to tweak.
func Start(t *testing.T, repoType string, extraArgs ...string) *Harness {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	zot, zotURL := startZot(t, ctx)
	binary := buildBinary(t)

	backendRepo := "ocifactory-int"
	seedNamespace(t, ctx, zotURL, backendRepo, "default", nsutil.AllowIssuerSpec("anonymous"))

	logPath := filepath.Join(t.TempDir(), "ocifactory.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	// Close immediately — ocifactory's net/http will reopen the same
	// port. There's a small race window where another process could
	// claim it; in practice it's robust enough for CI and matches the
	// pattern used elsewhere in this repo.
	_ = listener.Close()

	args := []string{
		"serve",
		"--repo-type=" + repoType,
		fmt.Sprintf("--backend-registry=%s://%s/%s", zotURL.Scheme, zotURL.Host, backendRepo),
		fmt.Sprintf("--port=%d", port),
		"--disable-authn",
		// zot served over HTTP without auth needs no backend creds;
		// "anonymous" is the default but pin it explicitly so the
		// test isn't sensitive to default changes.
		"--backend-auth-kind=anonymous",
	}
	args = append(args, extraArgs...)

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Run in its own process group so we can kill the whole tree on
	// cleanup if ocifactory ever spawns a child (it doesn't today,
	// but this is cheap insurance).
	cmd.SysProcAttr = procAttr()

	if err := cmd.Start(); err != nil {
		_ = zot.Terminate(context.Background())
		t.Fatalf("start ocifactory: %v", err)
	}
	t.Cleanup(func() {
		stopProcess(cmd)
		// Surface the server log on failure so a test author can see
		// what ocifactory thought happened. Keep it terse on success.
		if t.Failed() {
			if data, err := os.ReadFile(logPath); err == nil {
				t.Logf("=== ocifactory.log ===\n%s\n=== end ocifactory.log ===", data)
			}
		}
	})

	ocifactoryURL := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	if err := waitForHealthz(ctx, ocifactoryURL); err != nil {
		t.Fatalf("ocifactory did not become healthy: %v", err)
	}

	return &Harness{
		OcifactoryURL: ocifactoryURL,
		ZotURL:        zotURL,
		BackendRepo:   backendRepo,
		logFile:       logFile,
	}
}

// SeedNamespace writes namespace metadata into the harness backend through the
// same namespace.Store API production control planes use. Tests can call this
// after Start to add non-default namespaces before a real client subprocess
// targets them.
func (h *Harness) SeedNamespace(t *testing.T, name string, spec namespace.Spec) {
	t.Helper()
	seedNamespace(t, t.Context(), h.ZotURL, h.BackendRepo, name, spec)
}

func seedNamespace(t *testing.T, ctx context.Context, zotURL *url.URL, backendRepo string, name string, spec namespace.Spec) {
	t.Helper()
	if zotURL.Hostname() != "127.0.0.1" && zotURL.Hostname() != "localhost" {
		t.Fatalf("refusing to seed namespace %q into non-loopback registry %s", name, zotURL)
	}
	backendURL, err := url.Parse(fmt.Sprintf("%s://%s/%s", zotURL.Scheme, zotURL.Host, backendRepo))
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	reg, err := oci.NewRegistry(backendURL)
	if err != nil {
		t.Fatalf("create namespace seed registry: %v", err)
	}
	store := namespace.NewStore(reg)
	nsutil.Seed(t, ctx, store, &namespace.Namespace{Name: name, Spec: spec})
}

func startZot(t *testing.T, ctx context.Context) (testcontainers.Container, *url.URL) {
	t.Helper()

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
	return zot, &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port.Port())}
}

// buildBinary builds the ocifactory binary into a per-process tempdir
// shared across every Harness in the test binary. It's safe to call
// from many tests in parallel — sync.Once collapses the work.
func buildBinary(t *testing.T) string {
	t.Helper()

	binaryBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ocifactory-int-bin-")
		if err != nil {
			builtBinaryErr = fmt.Errorf("mkdir tempdir: %w", err)
			return
		}
		bin := filepath.Join(dir, "ocifactory")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		root, err := repoRoot()
		if err != nil {
			builtBinaryErr = fmt.Errorf("locate repo root: %w", err)
			return
		}
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/ocifactory")
		cmd.Dir = root
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			builtBinaryErr = fmt.Errorf("go build ocifactory: %w\n%s", err, out)
			return
		}
		builtBinaryPath = bin
	})

	if builtBinaryErr != nil {
		t.Fatalf("build ocifactory binary: %v", builtBinaryErr)
	}
	return builtBinaryPath
}

// repoRoot walks up from this source file to find the directory
// containing go.mod. Robust against the test running from any
// subdirectory and avoids depending on $PWD.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above " + file)
		}
		dir = parent
	}
}

func waitForHealthz(ctx context.Context, base *url.URL) error {
	deadline := time.Now().Add(60 * time.Second)
	healthz := base.JoinPath("healthz").String()
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := client.Get(healthz)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out waiting for /healthz")
}

// SkipIfMissing skips t when any of the named binaries can't be
// resolved on $PATH. Use it from per-format tests to gracefully
// skip when (e.g.) twine isn't installed on a developer machine.
func SkipIfMissing(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("required binary %q not found on PATH: %v", b, err)
		}
	}
}
