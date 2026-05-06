package python

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// PEP 691 simple-index media types.
const (
	contentTypeJSONv1 = "application/vnd.pypi.simple.v1+json"
	contentTypeHTMLv1 = "application/vnd.pypi.simple.v1+html"
	contentTypeHTML   = "text/html"

	// pypiAPIVersion advertises the PEP 691 schema version we serve.
	// 1.1 is the version that adds PEP 700 fields; we only emit a
	// strict subset (filename/url/hashes/requires-python/metadata
	// flags), all of which are valid 1.0 fields too. Clients that
	// don't recognise 1.1 fall back to the common subset.
	pypiAPIVersion = "1.1"
)

// pickContentType inspects an Accept header and returns the best PyPI
// simple-index content type to render in. The JSON variant is selected
// only when `application/vnd.pypi.simple.v1+json` appears with a
// q-value at least as high as any HTML alternative; otherwise the
// legacy HTML rendering is used. The default (no Accept header, or
// unparseable) is HTML for backwards compatibility with pre-PEP-691
// clients.
func pickContentType(accept string) string {
	if accept == "" {
		return contentTypeHTML
	}
	jsonQ, htmlQ := -1.0, -1.0
	for _, raw := range strings.Split(accept, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		mt, params, err := mime.ParseMediaType(raw)
		if err != nil {
			continue
		}
		q := 1.0
		if v := params["q"]; v != "" {
			if f, perr := strconv.ParseFloat(v, 64); perr == nil {
				q = f
			}
		}
		if q == 0 {
			continue
		}
		switch mt {
		case contentTypeJSONv1:
			if q > jsonQ {
				jsonQ = q
			}
		case contentTypeHTMLv1, contentTypeHTML:
			if q > htmlQ {
				htmlQ = q
			}
		}
	}
	if jsonQ < 0 {
		return contentTypeHTML
	}
	if htmlQ < 0 || jsonQ >= htmlQ {
		return contentTypeJSONv1
	}
	return contentTypeHTML
}

// indexFile is the per-file payload shared by the HTML template and
// the JSON encoder when rendering /simple/<pkg>/.
type indexFile struct {
	// Filename is the bare file name (e.g. `requests-2.31.0-py3-none-any.whl`).
	Filename string
	// URL is the absolute URL pip should fetch to download the file.
	URL string
	// Sha256 is the file blob's hex sha256 (no `sha256:` prefix).
	Sha256 string
	// MetadataSha256 is the hex sha256 of the PEP 658 metadata
	// companion, when one exists. Empty for sdists and any wheel
	// uploaded before PEP 658 support landed.
	MetadataSha256 string
	// RequiresPython is the wheel's `Requires-Python` value, if any.
	RequiresPython string
}

// indexPage is the data passed to the simple.html template.
type indexPage struct {
	Title string
	Files []indexFile
}

// simpleIndexJSONFile is the JSON wire shape per PEP 691 §"file" /
// PEP 714 §"core-metadata". The hyphenated keys match the spec.
type simpleIndexJSONFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	// CoreMetadata and DistInfoMetadata carry the same value; PEP 714
	// renamed the field, so we emit both for client compatibility.
	// pip 23.1+ reads core-metadata; older clients fall back to
	// dist-info-metadata.
	CoreMetadata     map[string]string `json:"core-metadata,omitempty"`
	DistInfoMetadata map[string]string `json:"dist-info-metadata,omitempty"`
}

type simpleIndexJSONMeta struct {
	APIVersion string `json:"api-version"`
}

type simpleIndexJSONPackage struct {
	Meta  simpleIndexJSONMeta   `json:"meta"`
	Name  string                `json:"name"`
	Files []simpleIndexJSONFile `json:"files"`
}

type simpleIndexJSONProject struct {
	Name string `json:"name"`
}

type simpleIndexJSONList struct {
	Meta     simpleIndexJSONMeta      `json:"meta"`
	Projects []simpleIndexJSONProject `json:"projects"`
}

func writeJSONPackageIndex(w http.ResponseWriter, name string, files []indexFile) {
	out := simpleIndexJSONPackage{
		Meta:  simpleIndexJSONMeta{APIVersion: pypiAPIVersion},
		Name:  name,
		Files: make([]simpleIndexJSONFile, 0, len(files)),
	}
	for _, f := range files {
		jf := simpleIndexJSONFile{
			Filename:       f.Filename,
			URL:            f.URL,
			Hashes:         map[string]string{},
			RequiresPython: f.RequiresPython,
		}
		if f.Sha256 != "" {
			jf.Hashes["sha256"] = f.Sha256
		}
		if f.MetadataSha256 != "" {
			jf.CoreMetadata = map[string]string{"sha256": f.MetadataSha256}
			jf.DistInfoMetadata = map[string]string{"sha256": f.MetadataSha256}
		}
		out.Files = append(out.Files, jf)
	}
	w.Header().Set("Content-Type", contentTypeJSONv1)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

func writeJSONIndexList(w http.ResponseWriter, projects []string) {
	out := simpleIndexJSONList{
		Meta:     simpleIndexJSONMeta{APIVersion: pypiAPIVersion},
		Projects: make([]simpleIndexJSONProject, 0, len(projects)),
	}
	for _, p := range projects {
		out.Projects = append(out.Projects, simpleIndexJSONProject{Name: p})
	}
	w.Header().Set("Content-Type", contentTypeJSONv1)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
