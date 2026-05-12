package npm

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestPublishReadDownload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pkgName string
	}{
		{name: "unscoped", pkgName: "left-pad"},
		{name: "scoped", pkgName: "@scope/foo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, fake)
			tarball := []byte("tarball bytes for " + tc.pkgName)
			publishOK(t, h, tc.pkgName, "1.0.0", tarball, map[string]string{"latest": "1.0.0"})

			rec := serve(t, h, http.MethodGet, "/"+testNS+"/"+tc.pkgName, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("packument status = %d, body %q", rec.Code, rec.Body.String())
			}
			var got PackageMetadata
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("Decode packument: %v", err)
			}
			wantTags := map[string]string{"latest": "1.0.0"}
			if diff := cmp.Diff(wantTags, got.DistTags); diff != "" {
				t.Errorf("dist-tags mismatch (-want +got):\n%s", diff)
			}
			if _, ok := got.Versions["1.0.0"]; !ok {
				t.Fatalf("packument missing version 1.0.0: %#v", got.Versions)
			}

			filename := tarballFileName(tc.pkgName, "1.0.0")
			rec = serve(t, h, http.MethodGet, "/"+testNS+"/"+tc.pkgName+"/-/"+filename, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("tarball status = %d, body %q", rec.Code, rec.Body.String())
			}
			if diff := cmp.Diff(string(tarball), rec.Body.String()); diff != "" {
				t.Errorf("tarball mismatch (-want +got):\n%s", diff)
			}

			encoded, err := encodePackageName(tc.pkgName)
			if err != nil {
				t.Fatalf("encodePackageName: %v", err)
			}
			_ = fakeFile(t, fake, testNS+"/index/"+encoded+"/present")
		})
	}
}

func TestPublishSecondVersionUpdatesPackumentAndDistTag(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, fake)
	publishOK(t, h, "left-pad", "1.0.0", []byte("one"), map[string]string{"latest": "1.0.0"})
	publishOK(t, h, "left-pad", "1.1.0", []byte("two"), map[string]string{"latest": "1.1.0"})

	rec := serve(t, h, http.MethodGet, "/"+testNS+"/left-pad", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("packument status = %d, body %q", rec.Code, rec.Body.String())
	}
	var got PackageMetadata
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode packument: %v", err)
	}
	wantVersions := []string{"1.0.0", "1.1.0"}
	var gotVersions []string
	for v := range got.Versions {
		gotVersions = append(gotVersions, v)
	}
	slices.Sort(gotVersions)
	if diff := cmp.Diff(wantVersions, gotVersions); diff != "" {
		t.Errorf("versions mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("1.1.0", got.DistTags["latest"]); diff != "" {
		t.Errorf("latest mismatch (-want +got):\n%s", diff)
	}
}

func TestPublishValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pkgName    string
		body       []byte
		wantStatus int
	}{
		{name: "mismatched name", pkgName: "left-pad", body: publishBody(t, "other", "1.0.0", []byte("x"), map[string]string{"latest": "1.0.0"}), wantStatus: http.StatusBadRequest},
		{name: "bad base64", pkgName: "left-pad", body: []byte(`{"name":"left-pad","versions":{"1.0.0":{"name":"left-pad","version":"1.0.0","dist":{}}},"dist-tags":{"latest":"1.0.0"},"_attachments":{"left-pad-1.0.0.tgz":{"data":"!!!"}}}`), wantStatus: http.StatusBadRequest},
		{name: "checksum mismatch", pkgName: "left-pad", body: []byte(`{"name":"left-pad","versions":{"1.0.0":{"name":"left-pad","version":"1.0.0","dist":{"shasum":"bad"}}},"dist-tags":{"latest":"1.0.0"},"_attachments":{"left-pad-1.0.0.tgz":{"data":"eA=="}}}`), wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := oci.NewFakeRegistry()
			h, _ := newTestHandler(t, fake)
			rec := serve(t, h, http.MethodPut, "/"+testNS+"/"+tc.pkgName, tc.body)
			if diff := cmp.Diff(tc.wantStatus, rec.Code); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
		})
	}
}

func TestPublishMaxBytesAndOverwrite(t *testing.T) {
	t.Parallel()

	t.Run("exact cap succeeds and one byte over is rejected", func(t *testing.T) {
		t.Parallel()

		body := publishBody(t, "left-pad", "1.0.0", []byte("x"), map[string]string{"latest": "1.0.0"})
		fake := oci.NewFakeRegistry()
		h, _ := newTestHandler(t, fake, WithMaxUploadBytes(int64(len(body))))
		rec := serve(t, h, http.MethodPut, "/"+testNS+"/left-pad", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("exact cap status = %d, body %q", rec.Code, rec.Body.String())
		}

		fake = oci.NewFakeRegistry()
		h, _ = newTestHandler(t, fake, WithMaxUploadBytes(int64(len(body)-1)))
		rec = serve(t, h, http.MethodPut, "/"+testNS+"/left-pad", body)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("over cap status = %d, body %q", rec.Code, rec.Body.String())
		}
		if got := len(fake.Files); got != 2 { // namespace metadata files only
			t.Fatalf("fake files after rejected upload = %d, want only namespace metadata", got)
		}
	})

	t.Run("existing version conflict and overwrite", func(t *testing.T) {
		t.Parallel()

		fake := oci.NewFakeRegistry()
		h, _ := newTestHandler(t, fake)
		publishOK(t, h, "left-pad", "1.0.0", []byte("one"), map[string]string{"latest": "1.0.0"})
		rec := serve(t, h, http.MethodPut, "/"+testNS+"/left-pad", publishBody(t, "left-pad", "1.0.0", []byte("two"), map[string]string{"latest": "1.0.0"}))
		if rec.Code != http.StatusConflict {
			t.Fatalf("conflict status = %d, body %q", rec.Code, rec.Body.String())
		}

		fake.AllowOverwrite = true
		rec = serve(t, h, http.MethodPut, "/"+testNS+"/left-pad", publishBody(t, "left-pad", "1.0.0", []byte("two"), map[string]string{"latest": "1.0.0"}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("overwrite status = %d, body %q", rec.Code, rec.Body.String())
		}
		got := fakeFile(t, fake, packageRepoForTest(t, "left-pad")+"/1.0.0/left-pad-1.0.0.tgz")
		if diff := cmp.Diff("two", string(got)); diff != "" {
			t.Errorf("stored bytes mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestDistTags(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, fake)
	publishOK(t, h, "left-pad", "1.0.0", []byte("one"), map[string]string{"latest": "1.0.0"})
	publishOK(t, h, "left-pad", "1.1.0", []byte("two"), map[string]string{"latest": "1.1.0"})

	rec := serve(t, h, http.MethodPut, "/"+testNS+"/-/package/left-pad/dist-tags/next", []byte(`"1.0.0"`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("dist-tag add status = %d, body %q", rec.Code, rec.Body.String())
	}
	rec = serve(t, h, http.MethodGet, "/"+testNS+"/-/package/left-pad/dist-tags", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dist-tag list status = %d, body %q", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode dist-tags: %v", err)
	}
	want := map[string]string{"latest": "1.1.0", "next": "1.0.0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dist-tags mismatch (-want +got):\n%s", diff)
	}

	rec = serve(t, h, http.MethodDelete, "/"+testNS+"/-/package/left-pad/dist-tags/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dist-tag delete status = %d, body %q", rec.Code, rec.Body.String())
	}
	rec = serve(t, h, http.MethodPut, "/"+testNS+"/-/package/left-pad/dist-tags/missing", []byte(`"9.9.9"`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dist-tag missing version status = %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestHeadAndUnknowns(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, fake)
	publishOK(t, h, "left-pad", "1.0.0", []byte("one"), map[string]string{"latest": "1.0.0"})

	rec := serve(t, h, http.MethodHead, "/"+testNS+"/left-pad/-/left-pad-1.0.0.tgz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD tarball status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("HEAD body = %q, want empty", got)
	}
	if got := rec.Header().Get("Content-Length"); got == "" {
		t.Fatalf("HEAD missing Content-Length")
	}

	tests := []struct {
		name       string
		target     string
		wantStatus int
	}{
		{name: "unknown package", target: "/" + testNS + "/missing", wantStatus: http.StatusNotFound},
		{name: "unknown namespace", target: "/missing-ns/left-pad", wantStatus: http.StatusNotFound},
		{name: "bad tarball name", target: "/" + testNS + "/left-pad/-/other-1.0.0.tgz", wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := serve(t, h, http.MethodGet, tc.target, nil)
			if diff := cmp.Diff(tc.wantStatus, rec.Code); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
		})
	}
}

func TestRootAndPing(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	h, _ := newTestHandler(t, fake)
	for _, target := range []string{"/" + testNS + "/", "/" + testNS + "/-/ping"} {
		t.Run(strings.TrimPrefix(target, "/"), func(t *testing.T) {
			t.Parallel()
			rec := serve(t, h, http.MethodGet, target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
			}
		})
	}
}
