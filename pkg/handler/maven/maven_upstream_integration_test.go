//go:build mavenupstream

// Real-Maven-Central integration test for the Maven proxy handler.
// Gated behind a separate `mavenupstream` build tag (NOT `integration`)
// because the test is intentionally non-hermetic: it talks to
// https://repo.maven.apache.org/maven2 over the public internet. Maven
// Central outages, rate limits, or response-shape changes will turn
// this test red.
//
// Invoke via `go test -tags=mavenupstream ./pkg/handler/maven/...`.

package maven_test

import (
	"context"
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

// TestMavenIntegration_LiveCentralProxy proves Maven proxy mode works
// end to end against real Maven Central: a real mvn dependency:get
// through a real ocifactory subprocess fetches, caches, and re-serves
// real Maven artifact bytes.
//
// Test artifact: org.slf4j:slf4j-api:1.7.36. Tiny, dependency-free
// for our purposes when resolved with -Dtransitive=false, and stable.
func TestMavenIntegration_LiveCentralProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-maven integration test in -short mode")
	}
	t.Parallel()

	integrationtest.SkipIfMissing(t, "mvn", "java")

	h := integrationtest.Start(t, "maven")
	seedMavenProxyNamespace(t, h.ZotURL, h.BackendRepo, "maven-central", "https://repo.maven.apache.org/maven2")

	const (
		groupID    = "org.slf4j"
		artifactID = "slf4j-api"
		version    = "1.7.36"
	)
	repoURL := h.OcifactoryURL.String() + "/maven-central/maven2/"

	t.Run("metadata_passthrough", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		mdURL := repoURL + strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID + "/maven-metadata.xml"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, mdURL, nil)
		if err != nil {
			t.Fatalf("build metadata request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET metadata: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("metadata status = %d, want 200; body:\n%s", resp.StatusCode, body)
		}
		text := string(body)
		for _, want := range []string{
			"<groupId>" + groupID + "</groupId>",
			"<artifactId>" + artifactID + "</artifactId>",
			"<version>" + version + "</version>",
			"<lastUpdated>",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("metadata missing %q; body excerpt:\n%s", want, text)
			}
		}
	})

	t.Run("mvn_dependency_get_through_proxy", func(t *testing.T) {
		t.Parallel()

		coordinate := fmt.Sprintf("%s:%s:%s", groupID, artifactID, version)

		localRepo := t.TempDir()
		runLiveMavenGet(t, repoURL, localRepo, coordinate)
		assertMavenArtifactCached(t, localRepo, groupID, artifactID, version)

		// A second fresh local repo proves the cached artifact can be
		// served back through OCI to a new Maven client invocation.
		localRepo2 := t.TempDir()
		runLiveMavenGet(t, repoURL, localRepo2, coordinate)
		assertMavenArtifactCached(t, localRepo2, groupID, artifactID, version)
	})
}

// seedMavenProxyNamespace writes a proxy-mode namespace into the OCI
// backend the ocifactory subprocess reads from. Mirrors
// integrationtest.seedDefaultNamespace but with Mode=proxy.
func seedMavenProxyNamespace(t *testing.T, zotURL *url.URL, backendRepo, name, upstream string) {
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

func runLiveMavenGet(t *testing.T, repoURL, localRepo, coordinate string) {
	t.Helper()
	settings := liveMavenSettings(t, localRepo)
	cmd := exec.Command("mvn",
		"-B",
		"-s", settings,
		"-Dmaven.repo.local="+localRepo,
		"-DremoteRepositories=ocifactory::default::"+repoURL,
		"-Dartifact="+coordinate,
		"-Dtransitive=false",
		"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get",
	)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mvn dependency:get %s: %v\n%s", coordinate, err, out)
	}
}

func liveMavenSettings(t *testing.T, localRepo string) string {
	t.Helper()
	settings := fmt.Sprintf(`<settings>
  <localRepository>%s</localRepository>
  <interactiveMode>false</interactiveMode>
</settings>
`, localRepo)
	path := filepath.Join(t.TempDir(), "settings.xml")
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings.xml: %v", err)
	}
	return path
}

func assertMavenArtifactCached(t *testing.T, localRepo, groupID, artifactID, version string) {
	t.Helper()
	jarPath := filepath.Join(localRepo,
		strings.ReplaceAll(groupID, ".", string(filepath.Separator)),
		artifactID,
		version,
		fmt.Sprintf("%s-%s.jar", artifactID, version),
	)
	info, err := os.Stat(jarPath)
	if err != nil {
		t.Fatalf("stat resolved jar %s: %v", jarPath, err)
	}
	if info.Size() == 0 {
		t.Fatalf("resolved jar %s is empty", jarPath)
	}
}
