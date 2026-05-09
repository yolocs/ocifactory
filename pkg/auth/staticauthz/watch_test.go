package staticauthz

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// reloadWaitTimeout is how long the tests wait for a Watch
// reload to land. Generous so a slow CI runner doesn't flake;
// the actual debounce is reloadDebounce, which is much shorter.
const reloadWaitTimeout = 5 * time.Second

// waitForDecision polls until the authorizer's verdict on the
// probe action matches wantAllow, or the timeout fires. Tests
// use this instead of sleeping for the debounce window because
// debounce + fsnotify latency varies across kernels and CI
// environments.
func waitForDecision(t *testing.T, a *Authorizer, ac *auth.AuthContext, act auth.Action, wantAllow bool) {
	t.Helper()
	deadline := time.Now().Add(reloadWaitTimeout)
	for time.Now().Before(deadline) {
		err := a.Authorize(t.Context(), ac, act)
		got := err == nil
		if got == wantAllow {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("authorizer never reached wantAllow=%v within %s", wantAllow, reloadWaitTimeout)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

const denyConfig = `default: deny
rules: []
`

const allowAllConfig = `default: allow
rules: []
`

const allowExampleConfig = `default: deny
rules:
  - subject:
      issuer: https://example.com
    allow:
      - { repo: "**", format: "*", op: "*" }
`

// TestWatch_PicksUpInPlaceEdit confirms the simplest case: an
// operator overwrites the file in place and the watcher reloads.
func TestWatch_PicksUpInPlaceEdit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "authz.yaml")
	writeFile(t, path, denyConfig)

	a, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	ac := &auth.AuthContext{Issuer: "https://example.com", ID: "u"}
	act := auth.Action{Repo: "packages/foo", Format: "python", Op: auth.OpRead}

	// Sanity: deny config rejects.
	if err := a.Authorize(ctx, ac, act); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("pre-reload Authorize() = %v, want ErrUnauthorized", err)
	}

	writeFile(t, path, allowExampleConfig)
	waitForDecision(t, a, ac, act, true)
}

// TestWatch_KubernetesConfigMapSymlinkSwap simulates the
// kubernetes ConfigMap atomic-rename pattern:
//
//	<dir>/authz.yaml -> ..data/authz.yaml          (stable symlink)
//	<dir>/..data     -> ..2026_05_09_12_00_00.0/   (rotated symlink)
//
// On update, k8s creates a new timestamped directory, points a
// ..data_tmp symlink at it, and renames ..data_tmp -> ..data.
// fsnotify watching the file path alone misses this because the
// path's inode changes. Watch on the parent directory + re-read
// via the original path is the reliable shape.
func TestWatch_KubernetesConfigMapSymlinkSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows; reload still works on linux/darwin")
	}
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "authz.yaml")

	// Initial state: timestamped data dir, ..data symlink at it,
	// authz.yaml symlink to ..data/authz.yaml.
	dataV1 := filepath.Join(dir, "..2026_05_09_12_00_00.0")
	if err := os.Mkdir(dataV1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	writeFile(t, filepath.Join(dataV1, "authz.yaml"), denyConfig)
	if err := os.Symlink(filepath.Base(dataV1), filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	if err := os.Symlink("..data/authz.yaml", path); err != nil {
		t.Fatalf("symlink authz.yaml: %v", err)
	}

	a, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	ac := &auth.AuthContext{Issuer: "https://example.com", ID: "u"}
	act := auth.Action{Repo: "x", Format: "python", Op: auth.OpRead}
	if err := a.Authorize(ctx, ac, act); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("pre-rotation Authorize() = %v, want ErrUnauthorized", err)
	}

	// Simulate a kubelet ConfigMap rotation: write a new
	// timestamped dir, create ..data_tmp pointing at it, then
	// atomically rename ..data_tmp -> ..data. The "authz.yaml"
	// symlink in the parent directory does not move — it
	// resolves to the new content because ..data does.
	dataV2 := filepath.Join(dir, "..2026_05_09_12_00_05.0")
	if err := os.Mkdir(dataV2, 0o755); err != nil {
		t.Fatalf("mkdir v2: %v", err)
	}
	writeFile(t, filepath.Join(dataV2, "authz.yaml"), allowExampleConfig)
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(dataV2), tmpLink); err != nil {
		t.Fatalf("symlink ..data_tmp: %v", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("rename ..data_tmp -> ..data: %v", err)
	}
	// k8s also removes the old timestamped dir; do the same so
	// any final RemoveDir event is exercised.
	_ = os.RemoveAll(dataV1)

	waitForDecision(t, a, ac, act, true)
}

// TestWatch_BadFileKeepsPreviousPolicy confirms that writing
// invalid YAML does NOT replace the in-memory rules — operators
// shouldn't be able to brick authorization with a typo.
func TestWatch_BadFileKeepsPreviousPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "authz.yaml")
	writeFile(t, path, allowAllConfig)

	a, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	ac := &auth.AuthContext{Issuer: "https://example.com", ID: "u"}
	act := auth.Action{Repo: "x", Format: "python", Op: auth.OpRead}
	if err := a.Authorize(ctx, ac, act); err != nil {
		t.Fatalf("baseline Authorize: %v", err)
	}

	// Write garbage. Watcher must NOT replace the loaded rules.
	writeFile(t, path, "default: ::not yaml::")

	// Wait long enough for any reload to settle, then confirm
	// the original allow-all is still in effect.
	time.Sleep(reloadDebounce + 200*time.Millisecond)
	if err := a.Authorize(ctx, ac, act); err != nil {
		t.Errorf("after bad reload, Authorize() = %v, want nil (old policy should remain)", err)
	}
}

// TestWatch_RejectsNoPathInstance confirms an Authorizer built
// with New (programmatic, no source path) fails Watch loudly
// rather than silently doing nothing.
func TestWatch_RejectsNoPathInstance(t *testing.T) {
	t.Parallel()

	a, err := New(Config{Default: DefaultDeny})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Watch(t.Context()); err == nil {
		t.Errorf("Watch on no-path Authorizer succeeded; want error")
	}
}

// TestWatch_StopsOnContextCancel confirms canceling the context
// shuts the goroutine down. We can't easily probe goroutine
// lifetime from outside, but we can confirm Watch returns and
// that subsequent edits do NOT trigger reloads (a reasonable
// proxy: the watcher is gone).
func TestWatch_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "authz.yaml")
	writeFile(t, path, denyConfig)

	a, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	if err := a.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	// Give the goroutine a moment to notice cancel.
	time.Sleep(50 * time.Millisecond)

	// Edit the file. If the watcher is still alive it would
	// reload to allow-all; if it stopped, the policy stays as
	// deny.
	writeFile(t, path, allowAllConfig)
	time.Sleep(reloadDebounce + 200*time.Millisecond)

	ac := &auth.AuthContext{Issuer: "i", ID: "u"}
	act := auth.Action{Repo: "x", Format: "y", Op: auth.OpRead}
	if err := a.Authorize(t.Context(), ac, act); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("after cancel, Authorize() = %v, want ErrUnauthorized (watcher should have stopped)", err)
	}
}

// TestWatch_ConcurrentReadDuringSwap stresses the atomic.Pointer
// hand-off: reads run continuously while reload swaps the rule
// set, and every read must see a self-consistent verdict (never
// a torn state). Asserts at minimum that no goroutine panics
// under -race.
func TestWatch_ConcurrentReadDuringSwap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "authz.yaml")
	writeFile(t, path, denyConfig)

	a, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		ac := &auth.AuthContext{Issuer: "https://example.com", ID: "u"}
		act := auth.Action{Repo: "x", Format: "python", Op: auth.OpRead}
		for !stop.Load() {
			_ = a.Authorize(ctx, ac, act)
		}
		close(done)
	}()

	// Toggle policies a handful of times so the reload path
	// exercises the atomic swap repeatedly while reads are in
	// flight.
	for i := 0; i < 4; i++ {
		if i%2 == 0 {
			writeFile(t, path, allowExampleConfig)
		} else {
			writeFile(t, path, denyConfig)
		}
		time.Sleep(reloadDebounce + 200*time.Millisecond)
	}

	stop.Store(true)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader goroutine did not exit")
	}
}

