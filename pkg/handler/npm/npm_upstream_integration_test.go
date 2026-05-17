//go:build npmupstream

// Real-npm-registry integration test for the npm proxy handler. Gated
// behind a separate `npmupstream` build tag (NOT `integration`)
// because the test is intentionally non-hermetic: it talks to
// https://registry.npmjs.org over the public internet. npm registry
// outages, rate limits, or response-shape changes will turn this test
// red.
//
// Invoke via `go test -tags=npmupstream ./pkg/handler/npm/...`.

package npm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/auth/backend"
	"github.com/yolocs/ocifactory/pkg/handler/integrationtest"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestNpmIntegration_LiveRegistryProxy proves npm proxy mode works end
// to end against the public npm registry: a real npm install through a
// real ocifactory subprocess fetches, caches, and re-serves real package
// bytes. Unit tests cover this flow with stubs; this test catches npm
// packument or tarball response-shape drift that stubs cannot.
//
// Test package: is-number@7.0.0. Tiny, dependency-free, and stable for
// years, so repeated CI runs avoid unnecessary upstream bandwidth.
func TestNpmIntegration_LiveRegistryProxy(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping live-npm integration test in -short mode")
	}

	integrationtest.SkipIfMissing(t, "npm")

	h := integrationtest.Start(t, "npm")
	seedNPMProxyNamespace(t, h.ZotURL, h.BackendRepo, "npm-cache", "https://registry.npmjs.org")

	const (
		pkgName = "is-number"
		version = "7.0.0"
	)
	registry := h.OcifactoryURL.String() + "/npm-cache/"

	t.Run("packument_rewrites_tarball_urls", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+pkgName, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET packument: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			t.Fatalf("status = %d, want 200; body:\n%s", resp.StatusCode, body)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		if err != nil {
			t.Fatalf("read packument: %v", err)
		}
		tarball := packumentTarballURL(t, body, version)
		wantPath := "/npm-cache/" + pkgName + "/-/" + pkgName + "-" + version + ".tgz"
		if !strings.Contains(tarball, wantPath) {
			t.Errorf("dist.tarball = %q, want path containing %q", tarball, wantPath)
		}
		if strings.Contains(tarball, "registry.npmjs.org") {
			t.Errorf("dist.tarball leaked unrewritten upstream URL: %q", tarball)
		}
	})

	t.Run("npm_install_through_proxy", func(t *testing.T) {
		t.Parallel()

		consumer := newLiveNPMConsumerProject(t)
		runLiveNPM(t, consumer, registry,
			"install",
			"--ignore-scripts",
			"--no-audit",
			"--no-fund",
			"--prefer-online",
			"--package-lock=false",
			fmt.Sprintf("%s@%s", pkgName, version),
		)
		assertLiveNPMInstalled(t, consumer, pkgName, version)

		// A second fresh consumer proves the cache survives a client
		// restart. We cannot directly observe upstream traffic here,
		// but a working second install proves the cached packument,
		// metadata, and tarball round-trip through OCI correctly.
		consumer2 := newLiveNPMConsumerProject(t)
		runLiveNPM(t, consumer2, registry,
			"install",
			"--ignore-scripts",
			"--no-audit",
			"--no-fund",
			"--prefer-online",
			"--package-lock=false",
			fmt.Sprintf("%s@%s", pkgName, version),
		)
		assertLiveNPMInstalled(t, consumer2, pkgName, version)
	})
}

// seedNPMProxyNamespace writes a proxy-mode namespace into the OCI
// backend the ocifactory subprocess reads from. Mirrors
// integrationtest.seedDefaultNamespace but with Mode=proxy and an
// anonymous Writers list so the data-plane AddFile during cache fill
// is authorized.
func seedNPMProxyNamespace(t *testing.T, zotURL *url.URL, backendRepo, name, upstream string) {
	t.Helper()
	regURL := &url.URL{Scheme: zotURL.Scheme, Host: zotURL.Host, Path: "/" + backendRepo}
	inner, err := oci.NewRegistry(regURL,
		oci.WithBackendAuth(backend.Anonymous()),
		oci.WithArtifactType(namespace.ArtifactType),
	)
	if err != nil {
		t.Fatalf("oci.NewRegistry: %v", err)
	}
	store := namespace.NewStore(inner)
	spec := namespace.Spec{
		Mode:  namespace.ModeProxy,
		Proxy: namespace.Proxy{Upstream: upstream},
		Policy: namespace.Policy{
			Readers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
			Writers: []namespace.SubjectMatcher{{Issuer: "anonymous"}},
		},
	}
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("seed proxy namespace %q: %v", name, err)
	}
}

func newLiveNPMConsumerProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{
		"name":    "ocifactory-live-npm-consumer",
		"version": "0.0.0",
		"private": true,
	}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), manifestBytes, 0o600); err != nil {
		t.Fatalf("write consumer package.json: %v", err)
	}
	return dir
}

func runLiveNPM(t *testing.T, dir, registry string, args ...string) {
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
	cfgDir := t.TempDir()
	globalRC := filepath.Join(cfgDir, ".npmrc-global")
	if err := os.WriteFile(globalRC, nil, 0o600); err != nil {
		t.Fatalf("write empty global npmrc: %v", err)
	}
	cmd.Env = append(os.Environ(),
		"npm_config_userconfig="+filepath.Join(dir, ".npmrc"),
		"npm_config_globalconfig="+globalRC,
		"npm_config_cache="+t.TempDir(),
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func assertLiveNPMInstalled(t *testing.T, dir, name, version string) {
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

func packumentTarballURL(t *testing.T, body []byte, version string) string {
	t.Helper()
	var p struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("parse packument: %v", err)
	}
	v, ok := p.Versions[version]
	if !ok {
		t.Fatalf("packument missing version %q", version)
	}
	return v.Dist.Tarball
}
