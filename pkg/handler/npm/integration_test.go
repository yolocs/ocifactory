//go:build integration

package npm

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/handler/integrationtest"
)

func TestNPMIntegration_RealClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping npm real-client integration test in -short mode")
	}
	t.Parallel()

	integrationtest.SkipIfMissing(t, "npm", "node")

	h := integrationtest.Start(t, "npm")
	registry := h.OcifactoryURL.String() + "/default/"

	t.Run("publish_install_dist_tag", func(t *testing.T) {
		t.Parallel()

		pkg := newNpmPackage(t, "ocifactory-int-basic", "1.0.0")
		npmOK(t, pkg, "publish", "--registry", registry, "--ignore-scripts")

		installDir := t.TempDir()
		npmOK(t, installDir, "init", "-y")
		npmOK(t, installDir, "install", "--registry", registry, "--ignore-scripts", "--no-audit", "--no-fund", "ocifactory-int-basic@1.0.0")

		npmOK(t, pkg, "dist-tag", "add", "ocifactory-int-basic@1.0.0", "next", "--registry", registry)
		installTagDir := t.TempDir()
		npmOK(t, installTagDir, "init", "-y")
		npmOK(t, installTagDir, "install", "--registry", registry, "--ignore-scripts", "--no-audit", "--no-fund", "ocifactory-int-basic@next")
	})

	t.Run("scoped_publish_install", func(t *testing.T) {
		t.Parallel()

		pkg := newNpmPackage(t, "@ocifactory-int/scoped", "1.0.0")
		npmOK(t, pkg, "publish", "--registry", registry, "--access", "public", "--ignore-scripts")

		installDir := t.TempDir()
		npmOK(t, installDir, "init", "-y")
		npmOK(t, installDir, "install", "--registry", registry, "--ignore-scripts", "--no-audit", "--no-fund", "@ocifactory-int/scoped@1.0.0")
	})
}

func newNpmPackage(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	pkg := map[string]any{
		"name":        name,
		"version":     version,
		"description": "ocifactory integration package",
		"main":        "index.js",
	}
	data, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		t.Fatalf("Marshal package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = 42;\n"), 0o644); err != nil {
		t.Fatalf("write index.js: %v", err)
	}
	return dir
}

func npmOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("npm", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), npmEnv(t)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func npmEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"NPM_CONFIG_AUDIT=false",
		"NPM_CONFIG_FUND=false",
		"NPM_CONFIG_PROGRESS=false",
		"NPM_CONFIG_LOGLEVEL=verbose",
		// npm requires a stable cache outside package dirs for parallel tests.
		fmt.Sprintf("NPM_CONFIG_CACHE=%s", filepath.Join(t.TempDir(), "npm-cache")),
	}
}
