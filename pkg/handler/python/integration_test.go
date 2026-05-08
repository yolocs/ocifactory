package python_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/handler/integrationtest"
)

// TestPythonIntegration_RealClients exercises the Python handler with
// real twine and pip subprocesses talking to a live ocifactory pointed
// at a real zot.
//
// It self-skips under -short and when Docker / pip / twine aren't
// available, mirroring the existing pkg/oci zot test's policy: a
// missing local runtime should never break `go test ./...` on a
// developer laptop. CI installs the toolchain and runs the full
// suite.
func TestPythonIntegration_RealClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping python real-client integration test in -short mode")
	}
	t.Parallel()

	integrationtest.SkipIfMissing(t, "python3", "pip", "twine")

	h := integrationtest.Start(t, "python")

	// Subtests share the same ocifactory + zot to avoid spinning up
	// a fresh container per case. They write to disjoint package
	// names so they don't interact.
	t.Run("twine_upload_then_pip_download", func(t *testing.T) {
		t.Parallel()

		const pkgName = "ocifactory-int-basic"
		const version = "1.2.3"

		whl := newTestWheel(t, pkgName, version)

		uploadWithTwine(t, h.OcifactoryURL.String(), whl)
		downloadAndCheck(t, h.OcifactoryURL.String(), pkgName, version, whl)
	})

	t.Run("pep503_normalization_roundtrip", func(t *testing.T) {
		t.Parallel()

		// Upload as "Foo_Int", install as "foo-int" — PEP 503
		// requires both forms to resolve to the same package.
		const uploadName = "Foo_Int"
		const installName = "foo-int"
		const version = "0.0.1"

		whl := newTestWheel(t, uploadName, version)

		uploadWithTwine(t, h.OcifactoryURL.String(), whl)
		downloadAndCheck(t, h.OcifactoryURL.String(), installName, version, whl)
	})

	t.Run("pip_install_in_fresh_venv", func(t *testing.T) {
		t.Parallel()

		// Some pip builds refuse `install` for a non-PEP427
		// filename; the venv path catches anything `pip download`
		// would silently let through.
		const pkgName = "ocifactory-int-install"
		const version = "0.5.0"

		whl := newTestWheel(t, pkgName, version)

		uploadWithTwine(t, h.OcifactoryURL.String(), whl)

		venvDir := t.TempDir()
		runOK(t, "python3", "-m", "venv", venvDir)
		pip := filepath.Join(venvDir, "bin", "pip")
		if _, err := os.Stat(pip); err != nil {
			pip = filepath.Join(venvDir, "Scripts", "pip.exe")
		}
		runOK(t, pip,
			"install",
			"--no-cache-dir",
			"--no-deps",
			"--index-url", h.OcifactoryURL.String()+"/simple/",
			"--trusted-host", trustedHostFromBase(t, h.OcifactoryURL.String()),
			fmt.Sprintf("%s==%s", pkgName, version),
		)
	})
}

// uploadWithTwine drives `twine upload` against the running
// ocifactory. With --disable-authn the server takes any credential,
// but twine requires non-empty username/password in the env to send
// a Basic header at all.
func uploadWithTwine(t *testing.T, base string, whl *testWheel) {
	t.Helper()

	cmd := exec.Command(
		"twine", "upload",
		"--repository-url", base+"/",
		"--non-interactive",
		"--disable-progress-bar",
		"--verbose",
		whl.Path,
	)
	cmd.Env = append(os.Environ(),
		"TWINE_USERNAME=integration",
		"TWINE_PASSWORD=integration",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("twine upload: %v\n%s", err, out)
	}
}

// downloadAndCheck pulls the wheel back via `pip download` and asserts
// the bytes match what was uploaded.
func downloadAndCheck(t *testing.T, base, pkgName, version string, whl *testWheel) {
	t.Helper()

	dst := t.TempDir()
	cmd := exec.Command(
		"pip", "download",
		"--no-cache-dir",
		"--no-deps",
		"--dest", dst,
		"--index-url", base+"/simple/",
		"--trusted-host", trustedHostFromBase(t, base),
		fmt.Sprintf("%s==%s", pkgName, version),
	)
	cmd.Env = append(os.Environ(),
		// pip's keyring backend is overkill here and emits a noisy
		// warning when no keyring is configured.
		"PYTHON_KEYRING_BACKEND=keyring.backends.null.Keyring",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pip download: %v\n%s", err, out)
	}

	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read pip dst: %v", err)
	}
	var found string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".whl") {
			found = filepath.Join(dst, e.Name())
			break
		}
	}
	if found == "" {
		t.Fatalf("pip download: no .whl in %s (entries: %v)", dst, entries)
	}

	got, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("read downloaded wheel: %v", err)
	}
	if !bytes.Equal(got, whl.Bytes) {
		t.Fatalf("pip download bytes mismatch: got %d bytes (sha256=%x), want %d bytes (sha256=%x)",
			len(got), sha256.Sum256(got), len(whl.Bytes), sha256.Sum256(whl.Bytes))
	}
}

// runOK runs cmd and t.Fatals with the combined output on a non-zero
// exit. Use it for setup steps where the expected outcome is success
// and any other result is a bug in the test harness.
func runOK(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// testWheel is a freshly synthesized PEP 427 wheel on disk. Building
// the wheel in Go avoids a `python -m build` dependency on the
// runner — twine accepts any zip with a valid dist-info, and pip
// installs it.
type testWheel struct {
	Path  string
	Bytes []byte
}

// newTestWheel builds a minimal but spec-valid wheel for (name,
// version). The wheel contains:
//
//   - <distName>-<version>.dist-info/METADATA  (PKG-INFO 2.1)
//   - <distName>-<version>.dist-info/WHEEL     (Wheel-Version: 1.0)
//   - <distName>-<version>.dist-info/RECORD    (lists the above two)
//
// distName is the wheel's filename-safe form of name (PEP 427
// requires `-` separators in the version segment but allows
// underscores in the project name; we pass-through whatever the test
// supplies). The wheel intentionally has no payload module — pip is
// happy to "install" a metadata-only wheel and our PEP 658 metadata
// extraction path runs against a known-shape dist-info either way.
func newTestWheel(t *testing.T, name, version string) *testWheel {
	t.Helper()

	// PEP 427 forbids dashes in the project segment of a wheel
	// filename, so swap them for underscores. Versions may not
	// contain dashes either.
	distName := strings.ReplaceAll(name, "-", "_")
	if strings.Contains(version, "-") {
		t.Fatalf("test bug: wheel version %q must not contain '-'", version)
	}

	distInfo := fmt.Sprintf("%s-%s.dist-info", distName, version)
	metadata := fmt.Sprintf(
		"Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: ocifactory integration test\n",
		name, version,
	)
	wheelMeta := "Wheel-Version: 1.0\nGenerator: ocifactory-integration-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"

	type fileEntry struct {
		Name    string
		Content []byte
	}
	entries := []fileEntry{
		{Name: distInfo + "/METADATA", Content: []byte(metadata)},
		{Name: distInfo + "/WHEEL", Content: []byte(wheelMeta)},
	}

	// RECORD is mandatory and must list every file in the wheel
	// (including itself, but with empty hash + size). Its rows are
	// `<path>,sha256=<urlsafe-b64-no-pad>,<size>`.
	var record bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", e.Name, urlsafeB64Sha256(e.Content), len(e.Content))
	}
	fmt.Fprintf(&record, "%s/RECORD,,\n", distInfo)
	entries = append(entries, fileEntry{Name: distInfo + "/RECORD", Content: record.Bytes()})

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	for _, e := range entries {
		f, err := zw.Create(e.Name)
		if err != nil {
			t.Fatalf("zip create %s: %v", e.Name, err)
		}
		if _, err := f.Write(e.Content); err != nil {
			t.Fatalf("zip write %s: %v", e.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	dir := t.TempDir()
	filename := fmt.Sprintf("%s-%s-py3-none-any.whl", distName, version)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, zipBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write wheel: %v", err)
	}
	return &testWheel{Path: path, Bytes: zipBuf.Bytes()}
}

// trustedHostFromBase pulls the host[:port] out of the harness URL so
// pip's --trusted-host flag can match. pip refuses to download from
// http:// origins by default and emits a noisy warning when the
// origin is HTTPS-but-untrusted; --trusted-host silences both.
func trustedHostFromBase(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse harness URL %q: %v", base, err)
	}
	return u.Host
}

func urlsafeB64Sha256(b []byte) string {
	sum := sha256.Sum256(b)
	// PEP 376 uses unpadded urlsafe base64.
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
