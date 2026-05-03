package renderer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

func TestNew_ParsesHTMLTemplates(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte(`hello {{.Name}}`)},
		"nested/sub.html": &fstest.MapFile{Data: []byte(`sub {{.Name}}`)},
		"styles.css":      &fstest.MapFile{Data: []byte(`/* skipped: not html */`)},
	}

	r, err := New(fsys)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name     string
		template string
		data     any
		want     string
	}{
		{name: "root template", template: "index.html", data: struct{ Name string }{"world"}, want: "hello world"},
		{name: "nested template", template: "sub.html", data: struct{ Name string }{"bar"}, want: "sub bar"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			r.RenderHTML(rec, tc.template, tc.data)

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html prefix", ct)
			}
		})
	}
}

func TestRenderHTML_UnknownTemplate_Returns500(t *testing.T) {
	t.Parallel()

	r, err := New(fstest.MapFS{"only.html": &fstest.MapFile{Data: []byte(`x`)}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec := httptest.NewRecorder()
	r.RenderHTML(rec, "missing.html", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestNew_BadTemplate_PropagatesError(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"broken.html": &fstest.MapFile{Data: []byte(`{{ if }} unterminated`)},
	}

	_, err := New(fsys)
	if err == nil {
		t.Fatal("New() expected error for malformed template, got nil")
	}
	if diff := cmp.Diff(true, strings.Contains(err.Error(), "broken.html")); diff != "" {
		t.Errorf("error should mention the bad template (-want +got):\n%s", diff)
	}
}
