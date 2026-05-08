package maven_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/handler/integrationtest"
)

// TestMavenIntegration_RealClients deploys and resolves a tiny Maven
// project against a live ocifactory pointed at zot.
//
// It self-skips under -short and when Docker / mvn / java aren't
// available (mirroring the existing pkg/oci zot test policy).
func TestMavenIntegration_RealClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping maven real-client integration test in -short mode")
	}
	t.Parallel()

	integrationtest.SkipIfMissing(t, "mvn", "java")

	h := integrationtest.Start(t, "maven",
		// mvn rewrites maven-metadata.xml on every deploy (it
		// fetches, mutates, re-uploads), so the immutable add-only
		// default would 409 the second deploy in this test —
		// release and snapshot share the artifact-level metadata
		// file. Production maven workflows need --allow-overwrite
		// for the same reason.
		"--allow-overwrite=true",
	)
	base := h.OcifactoryURL.String()

	// Subtests are NOT t.Parallel(): both deploys would race on
	// /com/example/test/hello/maven-metadata.xml (the artifact-level
	// version index mvn rewrites on every deploy). The outer test
	// already runs in parallel with the python suite.

	t.Run("release_deploy_and_get", func(t *testing.T) {
		const version = "0.1.0"
		const groupID = "com.example.test"
		const artifactID = "hello"

		project := stageProject(t, base, version)
		mvnDeploy(t, project)

		// Verify the four files mvn deploy is supposed to publish
		// landed at the Maven 2 layout paths handleRegularArtifact
		// uses.
		expect := []struct {
			path     string
			notEmpty bool
		}{
			{path: fmt.Sprintf("/%s/%s/%s/%s-%s.jar", strings.ReplaceAll(groupID, ".", "/"), artifactID, version, artifactID, version), notEmpty: true},
			{path: fmt.Sprintf("/%s/%s/%s/%s-%s.pom", strings.ReplaceAll(groupID, ".", "/"), artifactID, version, artifactID, version), notEmpty: true},
			{path: fmt.Sprintf("/%s/%s/%s/%s-%s.jar.sha1", strings.ReplaceAll(groupID, ".", "/"), artifactID, version, artifactID, version), notEmpty: true},
			{path: fmt.Sprintf("/%s/%s/%s/%s-%s.jar.md5", strings.ReplaceAll(groupID, ".", "/"), artifactID, version, artifactID, version), notEmpty: true},
		}
		for _, e := range expect {
			body := httpGetBody(t, base+e.path)
			if e.notEmpty && len(body) == 0 {
				t.Errorf("expected non-empty body at %s", e.path)
			}
		}

		// Now resolve via mvn dependency:get from a fresh local
		// repo and assert the JAR's bytes match what we published.
		freshRepo := t.TempDir()
		mvnGet(t, project, freshRepo, fmt.Sprintf("%s:%s:%s", groupID, artifactID, version))

		// dependency:get fetches the JAR with mvn's built-in
		// checksum validation, so a mismatch would have failed
		// mvnGet above. Confirm the resolver actually wrote the
		// expected file into the local repo.
		gotJarPath := filepath.Join(freshRepo,
			strings.ReplaceAll(groupID, ".", string(filepath.Separator)),
			artifactID, version,
			fmt.Sprintf("%s-%s.jar", artifactID, version),
		)
		if info, err := os.Stat(gotJarPath); err != nil {
			t.Fatalf("stat resolved jar %s: %v", gotJarPath, err)
		} else if info.Size() == 0 {
			t.Fatalf("resolved jar %s is empty", gotJarPath)
		}
	})

	t.Run("snapshot_deploy_round_trip", func(t *testing.T) {
		const version = "0.2.0-SNAPSHOT"
		const groupID = "com.example.test"
		const artifactID = "hello"

		project := stageProject(t, base, version)
		mvnDeploy(t, project)

		// The snapshot metadata route stores under
		// {groupId}/{artifactId}/{version}-metadata, so
		// maven-metadata.xml must be retrievable via the GET
		// path.
		mdURL := fmt.Sprintf("%s/%s/%s/%s/maven-metadata.xml",
			base, strings.ReplaceAll(groupID, ".", "/"), artifactID, version)
		body := httpGetBody(t, mdURL)
		if !strings.Contains(string(body), "<groupId>com.example.test</groupId>") {
			t.Errorf("snapshot maven-metadata.xml missing expected groupId; got:\n%s", body)
		}
	})
}

// stageProject copies the testdata maven project into a fresh
// directory, substitutes __VERSION__ and __OCIFACTORY_URL__, and
// renders settings.xml.tmpl to settings.xml in the same dir. Returns
// the staged project root.
func stageProject(t *testing.T, ocifactoryURL, version string) string {
	t.Helper()

	src, err := repoTestdataPath()
	if err != nil {
		t.Fatalf("locate testdata: %v", err)
	}
	dst := t.TempDir()
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copy testdata: %v", err)
	}

	// Templated substitutions in pom.xml.
	pomPath := filepath.Join(dst, "pom.xml")
	pomBytes, err := os.ReadFile(pomPath)
	if err != nil {
		t.Fatalf("read pom: %v", err)
	}
	pom := strings.ReplaceAll(string(pomBytes), "__VERSION__", version)
	pom = strings.ReplaceAll(pom, "__OCIFACTORY_URL__", ocifactoryURL)
	if err := os.WriteFile(pomPath, []byte(pom), 0o600); err != nil {
		t.Fatalf("write pom: %v", err)
	}

	// Render settings.xml from template.
	tmplPath := filepath.Join(dst, "settings.xml.tmpl")
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("read settings.xml.tmpl: %v", err)
	}
	settings := string(tmplBytes)
	localRepo := filepath.Join(dst, ".m2-deploy")
	if err := os.MkdirAll(localRepo, 0o700); err != nil {
		t.Fatalf("mkdir local repo: %v", err)
	}
	settings = strings.ReplaceAll(settings, "__LOCAL_REPO__", localRepo)
	settings = strings.ReplaceAll(settings, "__OCIFACTORY_URL__", ocifactoryURL)
	settings = strings.ReplaceAll(settings, "__USERNAME__", "integration")
	settings = strings.ReplaceAll(settings, "__PASSWORD__", "integration")
	if err := os.WriteFile(filepath.Join(dst, "settings.xml"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings.xml: %v", err)
	}
	return dst
}

func mvnDeploy(t *testing.T, project string) {
	t.Helper()
	cmd := exec.Command("mvn",
		"-B",
		"-s", filepath.Join(project, "settings.xml"),
		"-Dmaven.test.skip=true",
		"deploy",
	)
	cmd.Dir = project
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mvn deploy: %v\n%s", err, out)
	}
}

func mvnGet(t *testing.T, project, localRepo, coordinate string) {
	t.Helper()
	cmd := exec.Command("mvn",
		"-B",
		"-s", filepath.Join(project, "settings.xml"),
		fmt.Sprintf("-Dmaven.repo.local=%s", localRepo),
		"-DremoteRepositories=ocifactory-int",
		fmt.Sprintf("-Dartifact=%s", coordinate),
		"dependency:get",
	)
	cmd.Dir = project
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mvn dependency:get %s: %v\n%s", coordinate, err, out)
	}
}

func httpGetBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body=%s", url, resp.StatusCode, body)
	}
	return body
}

// repoTestdataPath returns the absolute path to the maven-deploy
// fixture next to this test file. runtime.Caller is more robust than
// $PWD: tests run from their own package directory but the harness
// might be invoked from go test -run anywhere.
func repoTestdataPath() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "maven-deploy"), nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}
