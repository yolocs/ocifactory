package python

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPickContentType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		accept string
		want   string
	}{
		{name: "empty defaults to HTML", accept: "", want: contentTypeHTML},
		{name: "wildcard defaults to HTML", accept: "*/*", want: contentTypeHTML},
		{name: "JSON v1 only", accept: contentTypeJSONv1, want: contentTypeJSONv1},
		{name: "HTML v1 only", accept: contentTypeHTMLv1, want: contentTypeHTML},
		{name: "text/html only", accept: contentTypeHTML, want: contentTypeHTML},
		{
			name:   "JSON preferred over HTML",
			accept: contentTypeJSONv1 + ", " + contentTypeHTML + ";q=0.5",
			want:   contentTypeJSONv1,
		},
		{
			name:   "HTML preferred via higher q",
			accept: contentTypeJSONv1 + ";q=0.5, " + contentTypeHTML + ";q=0.9",
			want:   contentTypeHTML,
		},
		{
			name:   "tie goes to JSON when explicitly asked",
			accept: contentTypeJSONv1 + ";q=0.7, " + contentTypeHTML + ";q=0.7",
			want:   contentTypeJSONv1,
		},
		{
			name:   "JSON with q=0 ignored",
			accept: contentTypeJSONv1 + ";q=0, " + contentTypeHTML,
			want:   contentTypeHTML,
		},
		{
			name:   "unrelated types fall back to HTML",
			accept: "application/xml, text/plain",
			want:   contentTypeHTML,
		},
		{
			name:   "malformed q parameter ignored, JSON still selected",
			accept: contentTypeJSONv1 + ";q=banana",
			want:   contentTypeJSONv1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pickContentType(tc.accept); got != tc.want {
				t.Errorf("pickContentType(%q) = %q, want %q", tc.accept, got, tc.want)
			}
		})
	}
}

func TestWriteJSONPackageIndex_Schema(t *testing.T) {
	t.Parallel()

	files := []indexFile{
		{
			Filename: "requests-2.31.0-py3-none-any.whl",
			URL:      "http://example/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl",
			Sha256:   "abc123",
		},
		{
			Filename: "requests-2.31.0.tar.gz",
			URL:      "http://example/packages/requests/2.31.0/requests-2.31.0.tar.gz",
			Sha256:   "ffffff",
		},
	}

	rec := httptest.NewRecorder()
	writeJSONPackageIndex(rec, "requests", files)

	if got, want := rec.Header().Get("Content-Type"), contentTypeJSONv1; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	var got simpleIndexJSONPackage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	want := simpleIndexJSONPackage{
		Meta: simpleIndexJSONMeta{APIVersion: pypiAPIVersion},
		Name: "requests",
		Files: []simpleIndexJSONFile{
			{
				Filename: "requests-2.31.0-py3-none-any.whl",
				URL:      "http://example/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl",
				Hashes:   map[string]string{"sha256": "abc123"},
			},
			{
				Filename: "requests-2.31.0.tar.gz",
				URL:      "http://example/packages/requests/2.31.0/requests-2.31.0.tar.gz",
				Hashes:   map[string]string{"sha256": "ffffff"},
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("JSON package index mismatch (-want +got):\n%s", diff)
	}
}

func TestWriteJSONIndexList_Schema(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writeJSONIndexList(rec, []string{"requests", "flask"})

	if got, want := rec.Header().Get("Content-Type"), contentTypeJSONv1; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	var got simpleIndexJSONList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	want := simpleIndexJSONList{
		Meta: simpleIndexJSONMeta{APIVersion: pypiAPIVersion},
		Projects: []simpleIndexJSONProject{
			{Name: "requests"},
			{Name: "flask"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("JSON index list mismatch (-want +got):\n%s", diff)
	}
}
