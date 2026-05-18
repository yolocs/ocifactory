//go:build pypiupstream

// Real-PyPI integration test for the python proxy handler. Gated
// behind a separate `pypiupstream` build tag (NOT `integration`)
// because the test is intentionally non-hermetic: it talks to
// https://pypi.org over the public internet. PyPI outages, rate
// limits, or response-shape changes will turn this test red. CI runs
// it on every PR in the separate live-upstream workflow.
//
// Invoke via `go test -tags=pypiupstream ./pkg/handler/python/...`.

package python_test

import (
	"context"
	"fmt"
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

// TestPythonIntegration_LivePypiProxy proves the proxy mode works end
// to end against real PyPI: a real pip install through a real
// ocifactory subprocess that fetches, caches, and re-serves real
// package bytes. The unit tests cover the same flow with stubs; this
// test catches PyPI response-shape drift that stubs can't.
//
// Test package: `six==1.16.0`. Tiny (~11 KB), depless, frozen for
// years. If this ever bit-rots we replace it; we do NOT pick something
// big like requests or numpy because nightly bandwidth adds up.
func TestPythonIntegration_LivePypiProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-pypi integration test in -short mode")
	}
	t.Parallel()

	integrationtest.SkipIfMissing(t, "python3", "pip")

	h := integrationtest.Start(t, "python")
	seedPyPIProxyNamespace(t, h.ZotURL, h.BackendRepo, "pypi-cache", "https://pypi.org")

	const (
		pkgName = "six"
		version = "1.16.0"
	)
	indexURL := h.OcifactoryURL.String() + "/pypi-cache/simple/"
	trustedHost := h.OcifactoryURL.Host

	t.Run("simple_index_rewrites_pypi_hrefs", func(t *testing.T) {
		// A raw GET to /pypi-cache/simple/six/ should return the
		// PEP 503 index with hrefs rewritten back through ocifactory.
		// If PyPI changed the JSON / HTML shape in a way pkg/proxy/python
		// no longer parses, the response either misses the package or
		// contains absolute files.pythonhosted.org links — both are bugs.
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL+pkgName+"/", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /simple/%s/: %v", pkgName, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		buf := make([]byte, 256*1024)
		n, _ := resp.Body.Read(buf)
		body := string(buf[:n])
		if !strings.Contains(body, "/pypi-cache/packages/"+pkgName+"/") {
			t.Errorf("simple index did not contain rewritten href for %s; body excerpt:\n%s", pkgName, body)
		}
		if strings.Contains(body, "files.pythonhosted.org") {
			t.Errorf("simple index leaked an unrewritten upstream href; body excerpt:\n%s", body)
		}
	})

	t.Run("pip_install_through_proxy", func(t *testing.T) {
		// Fresh venv → cold-miss install. ocifactory fetches from PyPI,
		// AddFile streams the wheel into zot, tees to pip. After this
		// finishes the package + version exist under
		// pypi-cache/packages/<pkgName>/<version>/ in the OCI backend.
		venv := t.TempDir()
		runOK(t, "python3", "-m", "venv", venv)
		pip := pipBin(venv)

		runOK(t, pip,
			"install",
			"--no-cache-dir",
			"--no-deps",
			"--index-url", indexURL,
			"--trusted-host", trustedHost,
			fmt.Sprintf("%s==%s", pkgName, version),
		)

		// A second fresh venv proves the cache survives a client
		// restart. We can't observe upstream traffic from this side,
		// but if pip still gets a working wheel we've at least proven
		// the cached blob roundtrips through ReadFile correctly.
		venv2 := t.TempDir()
		runOK(t, "python3", "-m", "venv", venv2)
		pip2 := pipBin(venv2)
		runOK(t, pip2,
			"install",
			"--no-cache-dir",
			"--no-deps",
			"--index-url", indexURL,
			"--trusted-host", trustedHost,
			fmt.Sprintf("%s==%s", pkgName, version),
		)

		// Import six in the second venv to prove the wheel is
		// structurally valid (extractable, not corrupted on the
		// stream). `six` exposes a __version__ string we can compare.
		py := pythonBin(venv2)
		cmd := exec.Command(py, "-c", "import six; print(six.__version__)")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("import six: %v\n%s", err, out)
		}
		got := strings.TrimSpace(string(out))
		if got != version {
			t.Fatalf("six.__version__ = %q, want %q", got, version)
		}
	})
}

// seedPyPIProxyNamespace writes a proxy-mode namespace into the OCI
// backend the ocifactory subprocess reads from. Mirrors
// integrationtest.seedDefaultNamespace but with Mode=proxy and an
// anonymous Writers list so the data-plane AddFile during cache
// fill is authorized.
func seedPyPIProxyNamespace(t *testing.T, zotURL *url.URL, backendRepo, name, upstream string) {
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

func pipBin(venv string) string {
	bin := filepath.Join(venv, "bin", "pip")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	return filepath.Join(venv, "Scripts", "pip.exe")
}

func pythonBin(venv string) string {
	bin := filepath.Join(venv, "bin", "python")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	return filepath.Join(venv, "Scripts", "python.exe")
}

// runOK runs cmd and t.Fatals with the combined output on a non-zero
// exit. Same helper as integration_test.go; duplicated rather than
// widening that file's build tag so `pypiupstream` and `integration`
// stay independent test suites.
func runOK(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}
