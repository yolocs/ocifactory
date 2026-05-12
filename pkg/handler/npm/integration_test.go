//go:build integration

// Real-client integration test for the npm handler. Gated behind
// the `integration` build tag because the standard go-test runner
// already has node / npm preinstalled — without the tag,
// `go test ./...` on a stock ubuntu runner would try to run this
// test for real and conflict with the dedicated client-integration
// job that owns the toolchain. Run via `go test -tags=integration`.

package npm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/handler/integrationtest"
)

// TestNpmIntegration_RealClients exercises the npm handler with a
// real npm client subprocess talking to a live ocifactory pointed at
// a real zot.
//
// Self-skips under -short and when Docker / npm aren't available,
// matching the python integration test's policy: a missing local
// runtime should never break `go test ./...` on a developer laptop.
// CI installs the toolchain and runs the full suite.
func TestNpmIntegration_RealClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping npm real-client integration test in -short mode")
	}
	t.Parallel()

	integrationtest.SkipIfMissing(t, "npm")

	h := integrationtest.Start(t, "npm")
	registry := h.OcifactoryURL.String() + "/default/"

	t.Run("publish_then_install_unscoped", func(t *testing.T) {
		t.Parallel()

		const pkg = "ocifactory-int-npm-basic"
		const version = "1.0.0"

		pkgDir := newNpmPackage(t, pkg, version)
		runNpm(t, pkgDir, registry, "publish")

		installDir := newConsumerProject(t, pkg, version)
		runNpm(t, installDir, registry, "install", "--no-audit", "--no-fund", "--prefer-online", fmt.Sprintf("%s@%s", pkg, version))

		assertInstalled(t, installDir, pkg, version)
	})

	t.Run("publish_then_install_scoped", func(t *testing.T) {
		t.Parallel()

		const pkg = "@ocifactory-int/scoped-basic"
		const version = "1.0.0"

		pkgDir := newNpmPackage(t, pkg, version)
		runNpm(t, pkgDir, registry, "publish")

		installDir := newConsumerProject(t, pkg, version)
		runNpm(t, installDir, registry, "install", "--no-audit", "--no-fund", "--prefer-online", fmt.Sprintf("%s@%s", pkg, version))

		assertInstalled(t, installDir, pkg, version)
	})

	t.Run("dist_tag_add_resolves", func(t *testing.T) {
		t.Parallel()

		const pkg = "ocifactory-int-npm-disttag"
		const version = "1.2.3"

		pkgDir := newNpmPackage(t, pkg, version)
		runNpm(t, pkgDir, registry, "publish")

		// Assign a `next` dist-tag and verify `npm install @next`
		// resolves to the version we just published.
		runNpm(t, pkgDir, registry, "dist-tag", "add", fmt.Sprintf("%s@%s", pkg, version), "next")

		installDir := newConsumerProject(t, pkg, "next")
		runNpm(t, installDir, registry, "install", "--no-audit", "--no-fund", "--prefer-online", fmt.Sprintf("%s@next", pkg))
		assertInstalled(t, installDir, pkg, version)
	})
}

// newNpmPackage materialises a temp directory shaped like a
// publishable npm package: package.json with the supplied name and
// version, plus a no-op index.js so npm pack doesn't refuse the
// payload. Returns the directory path.
func newNpmPackage(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()

	manifest := map[string]any{
		"name":        name,
		"version":     version,
		"description": "ocifactory npm integration test fixture",
		"main":        "index.js",
		"license":     "Apache-2.0",
	}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), manifestBytes, 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = 'ocifactory-int';\n"), 0o600); err != nil {
		t.Fatalf("write index.js: %v", err)
	}
	return dir
}

// newConsumerProject builds a tiny consumer project so npm install
// has somewhere to land its node_modules.
func newConsumerProject(t *testing.T, _, _ string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{
		"name":    "ocifactory-int-consumer",
		"version": "0.0.0",
		"private": true,
	}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), manifestBytes, 0o600); err != nil {
		t.Fatalf("write consumer package.json: %v", err)
	}
	return dir
}

// runNpm drives `npm <args>` inside dir, pointed at the integration
// harness's registry. ocifactory was started with --disable-authn,
// so npm just needs a syntactically-valid _authToken to send a Bearer
// header; we hand it a dummy value.
func runNpm(t *testing.T, dir, registry string, args ...string) {
	t.Helper()
	u, err := url.Parse(registry)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	npmrc := fmt.Sprintf(
		"registry=%s\n//%s%s:_authToken=integration-token\n",
		registry, u.Host, u.Path,
	)
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte(npmrc), 0o600); err != nil {
		t.Fatalf("write .npmrc: %v", err)
	}

	full := append([]string{"--registry=" + registry}, args...)
	cmd := exec.Command("npm", full...)
	cmd.Dir = dir
	// npm chokes when HOME is empty; t.TempDir() gives us a
	// guaranteed-writeable location that gets cleaned up on test
	// exit.
	cmd.Env = append(os.Environ(),
		"npm_config_userconfig="+filepath.Join(dir, ".npmrc"),
		"npm_config_globalconfig="+filepath.Join(dir, ".npmrc"),
		"npm_config_cache="+t.TempDir(),
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// assertInstalled checks that node_modules/<name>/package.json exists
// in dir and reports the version we asked for. node_modules nests
// scoped names under their @scope subdirectory, so the path differs
// between scoped and unscoped names.
func assertInstalled(t *testing.T, dir, name, version string) {
	t.Helper()
	pkgPath := filepath.Join(dir, "node_modules", filepath.FromSlash(name), "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatalf("read installed package.json (%s): %v", pkgPath, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse installed package.json: %v", err)
	}
	if got, _ := m["name"].(string); got != name {
		t.Errorf("installed name=%q, want %q", got, name)
	}
	if got, _ := m["version"].(string); got != version {
		t.Errorf("installed version=%q, want %q", got, version)
	}
}

// unused now but kept so future tests that want to make their own
// tarball without invoking `npm pack` have a worked example.
var _ = makeTarball

func makeTarball(t *testing.T, name, version string) ([]byte, string) {
	t.Helper()
	manifest := map[string]any{
		"name":    name,
		"version": version,
	}
	manifestBytes, _ := json.Marshal(manifest)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	files := []struct {
		name string
		body []byte
	}{
		{name: "package/package.json", body: manifestBytes},
		{name: "package/index.js", body: []byte("module.exports = 'ok';\n")},
	}
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), fmt.Sprintf("sha256:%x", sum)
}
