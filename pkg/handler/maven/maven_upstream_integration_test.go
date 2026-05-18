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
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/auth/backend"
	"github.com/yolocs/ocifactory/pkg/handler/integrationtest"
	mavenhandler "github.com/yolocs/ocifactory/pkg/handler/maven"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// TestMavenIntegration_LiveCentralProxy proves Maven proxy mode works
// end to end against real Maven Central: a real ocifactory subprocess
// fetches, caches, and re-serves real Maven artifact bytes. Real Maven
// client behaviour is covered by the hermetic `integration` test; this
// live test intentionally keeps Maven Central traffic small and focused
// on the proxy path.
//
// Test artifact: org.slf4j:slf4j-api:1.7.36. Tiny, stable, and widely
// cached by Maven Central.
func TestMavenIntegration_LiveCentralProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-maven integration test in -short mode")
	}
	t.Parallel()

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

	t.Run("artifact_fetch_through_proxy", func(t *testing.T) {
		t.Parallel()

		artifactPath := strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID + "/" + version + "/" + artifactID + "-" + version + ".jar"
		artifactURL := repoURL + artifactPath
		body := getLiveMavenURL(t, artifactURL, 2*1024*1024)
		if len(body) == 0 {
			t.Fatalf("artifact body from %s is empty", artifactURL)
		}
		assertMavenArtifactCached(t, h.ZotURL, h.BackendRepo, "maven-central", groupID, artifactID, version)

		body2 := getLiveMavenURL(t, artifactURL, 2*1024*1024)
		if len(body2) != len(body) {
			t.Fatalf("cached artifact size = %d, want %d", len(body2), len(body))
		}
	})
}

func getLiveMavenURL(t *testing.T, url string, limit int64) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body:\n%s", url, resp.StatusCode, body)
	}
	return body
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

func assertMavenArtifactCached(t *testing.T, zotURL *url.URL, backendRepo, namespaceName, groupID, artifactID, version string) {
	t.Helper()
	regURL := &url.URL{Scheme: zotURL.Scheme, Host: zotURL.Host, Path: "/" + backendRepo}
	inner, err := oci.NewRegistry(regURL,
		oci.WithBackendAuth(backend.Anonymous()),
		oci.WithArtifactType(mavenhandler.ArtifactType),
	)
	if err != nil {
		t.Fatalf("oci.NewRegistry: %v", err)
	}
	f := &oci.RepoFile{
		OwningRepo: namespaceName + "/" + strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID,
		OwningTag:  version,
		Name:       artifactID + "-" + version + ".jar",
		MediaType:  "application/java-archive",
	}
	desc, rc, err := inner.ReadFile(t.Context(), f)
	if err != nil {
		t.Fatalf("read cached artifact from OCI: %v", err)
	}
	defer rc.Close()
	if desc.File.Size == 0 {
		t.Fatalf("cached artifact has zero size")
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("read cached artifact body: %v", err)
	}
}
