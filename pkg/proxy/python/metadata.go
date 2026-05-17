package python

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy"
)

// pypiJSON is the subset of the PyPI JSON API response we read. The
// upstream response is much richer; we only capture what the proxy
// actually consumes (file URLs, hashes, sizes, upload times).
//
// Decoding tolerates missing fields — PyPI sometimes omits
// `upload_time_iso_8601` for very old packages — but rejects bodies
// that don't even carry a `urls` array, which is the shape every
// per-version response must have.
type pypiJSON struct {
	Info struct {
		Name string `json:"name"`
	} `json:"info"`
	URLs []pypiJSONFile `json:"urls"`
}

type pypiJSONFile struct {
	Filename          string            `json:"filename"`
	URL               string            `json:"url"`
	Digests           map[string]string `json:"digests"`
	Size              int64             `json:"size"`
	UploadTimeISO8601 string            `json:"upload_time_iso_8601"`
}

func decodeVersionMetadata(body []byte, pkg, version string) (*VersionMetadata, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("python proxy: metadata %s %s: empty body: %w",
			pkg, version, proxy.ErrUpstreamMalformed)
	}
	var raw pypiJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("python proxy: metadata %s %s: decode: %v: %w",
			pkg, version, err, proxy.ErrUpstreamMalformed)
	}
	// PyPI returns 200 with an empty urls array when a version
	// exists but has no published files — treat that the same as
	// a 404 from the caller's perspective so the proxy short-circuits
	// instead of streaming nothing.
	if len(raw.URLs) == 0 {
		return nil, fmt.Errorf("python proxy: metadata %s %s: no files in response: %w",
			pkg, version, proxy.ErrNotFound)
	}

	meta := &VersionMetadata{
		Package: raw.Info.Name,
		Version: version,
		Files:   make([]FileMetadata, 0, len(raw.URLs)),
	}
	if meta.Package == "" {
		meta.Package = pkg
	}

	var earliest time.Time
	for _, u := range raw.URLs {
		fm := FileMetadata{
			Filename: u.Filename,
			URL:      u.URL,
			SHA256:   u.Digests["sha256"],
			Size:     u.Size,
		}
		if u.UploadTimeISO8601 != "" {
			if t, perr := time.Parse(time.RFC3339Nano, u.UploadTimeISO8601); perr == nil {
				fm.UploadTime = t
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
			}
		}
		meta.Files = append(meta.Files, fm)
	}
	meta.UploadTime = earliest
	return meta, nil
}

// FindFile returns the [FileMetadata] entry whose Filename matches the
// requested name, or false when no entry matches. Comparison is
// case-sensitive — PyPI distribution filenames are canonical and
// case-changes are not equivalent on the wire.
func (m *VersionMetadata) FindFile(filename string) (FileMetadata, bool) {
	if m == nil {
		return FileMetadata{}, false
	}
	for _, f := range m.Files {
		if f.Filename == filename {
			return f, true
		}
	}
	return FileMetadata{}, false
}
