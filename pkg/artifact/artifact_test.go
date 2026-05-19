package artifact

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	testIssuer = "https://accounts.google.com"
	testNS     = "default"
)

func TestNamespacePackagePutGetListAndTag(t *testing.T) {
	t.Parallel()

	_, store := setupStore(t)
	ctx := authedContext(t)

	ns, err := store.Namespace(ctx, testNS)
	if err != nil {
		t.Fatalf("Namespace: %v", err)
	}
	pkg := ns.Package("packages/requests")

	desc, err := pkg.PutFile(ctx, "1.0.0", FilePut{
		Name:      "requests.whl",
		MediaType: "application/octet-stream",
		Size:      int64(len("wheel")),
	}, strings.NewReader("wheel"))
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if desc.File.Size != int64(len("wheel")) {
		t.Fatalf("PutFile size = %d, want %d", desc.File.Size, len("wheel"))
	}

	versions, err := pkg.ListVersions(ctx)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if diff := cmp.Diff([]string{"1.0.0"}, versions); diff != "" {
		t.Errorf("ListVersions mismatch (-want +got):\n%s", diff)
	}

	files, err := pkg.ListFiles(ctx, ListFilesOptions{})
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	wantFiles := []FileInfo{{
		Package: "packages/requests",
		Version: "1.0.0",
		Name:    "requests.whl",
		Digest:  desc.File.Digest.String(),
	}}
	if diff := cmp.Diff(wantFiles, files); diff != "" {
		t.Errorf("ListFiles mismatch (-want +got):\n%s", diff)
	}

	if err := pkg.Tag(ctx, "latest", "1.0.0"); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	tags, err := pkg.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if diff := cmp.Diff([]Tag{{Name: "latest"}}, tags); diff != "" {
		t.Errorf("ListTags mismatch (-want +got):\n%s", diff)
	}

	handle, err := pkg.GetFileByTag(ctx, "latest", "requests.whl")
	if err != nil {
		t.Fatalf("GetFileByTag: %v", err)
	}
	rc, err := handle.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "wheel" {
		t.Errorf("Open body = %q, want %q", got, "wheel")
	}
}

func TestNamespaceValidatesExistence(t *testing.T) {
	t.Parallel()

	_, store := setupStore(t)

	if _, err := store.Namespace(authedContext(t), "missing"); err == nil {
		t.Fatal("Namespace(missing) = nil, want error")
	}
}

func TestPackageRejectsInvalidName(t *testing.T) {
	t.Parallel()

	_, store := setupStore(t)
	ns, err := store.Namespace(authedContext(t), testNS)
	if err != nil {
		t.Fatalf("Namespace: %v", err)
	}

	_, err = ns.Package("../escape").PutFile(authedContext(t), "1.0.0", FilePut{Name: "x"}, strings.NewReader("x"))
	if err == nil {
		t.Fatal("PutFile with escaping package = nil, want error")
	}
}

func TestFileHandleDownloadURLFallsBackToEmptyURL(t *testing.T) {
	t.Parallel()

	_, store := setupStore(t)
	ctx := authedContext(t)
	ns, err := store.Namespace(ctx, testNS)
	if err != nil {
		t.Fatalf("Namespace: %v", err)
	}
	pkg := ns.Package("packages/requests")
	if _, err := pkg.PutFile(ctx, "1.0.0", FilePut{Name: "requests.whl"}, strings.NewReader("wheel")); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	handle, err := pkg.GetFile(ctx, "1.0.0", "requests.whl")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	u, err := handle.DownloadURL(ctx)
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	if u != "" {
		t.Errorf("DownloadURL = %q, want empty fake-registry fallback", u)
	}
}

func setupStore(t *testing.T) (*oci.FakeRegistry, *Store) {
	t.Helper()

	fake := oci.NewFakeRegistry()
	nsStore := namespace.NewStore(fake)
	reg := namespace.NewRegistry(fake, nsStore, namespace.WithPolicyCacheTTL(0))
	if err := nsStore.Put(t.Context(), &namespace.Namespace{
		Name: testNS,
		Spec: namespace.Spec{
			Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: testIssuer}},
				Writers: []namespace.SubjectMatcher{{Issuer: testIssuer}},
			},
		},
	}); err != nil {
		t.Fatalf("Put namespace: %v", err)
	}
	return fake, NewStore(reg)
}

func authedContext(t *testing.T) context.Context {
	t.Helper()

	return auth.WithAuthContext(t.Context(), &auth.AuthContext{
		Issuer: testIssuer,
		ID:     "alice",
		Email:  "alice@example.com",
	})
}
