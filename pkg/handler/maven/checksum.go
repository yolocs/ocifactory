package maven

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"

	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/oci"
	"oras.land/oras-go/v2/errdef"
)

// maxChecksumBodyBytes caps the bytes verifyChecksumUpload reads from the
// uploaded body before deciding the checksum is malformed. Maven checksum
// files are a single hex digest line, optionally followed by a filename.
// 1 KiB is comfortably above any legitimate file (sha512 hex is 128 bytes
// + a filename suffix) and a tight DoS bound for the buffered path.
const maxChecksumBodyBytes = 1 << 10

// httpError carries an HTTP status code alongside an error message so
// handlePut can map helper failures (path validation, checksum mismatch,
// missing companion artifact) to the right status without re-parsing the
// error string.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &httpError{code: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

// httpStatus extracts the status code carried by *httpError, or returns
// 0 if err does not wrap one.
func httpStatus(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.code
	}
	return 0
}

// checksumAlgos enumerates the Maven checksum file extensions and the
// hash function each implies. Order matters in the lookup loop only for
// the (very unlikely) case where one extension is a suffix of another;
// today they are all distinct.
var checksumAlgos = []struct {
	ext  string
	name string
	new  func() hash.Hash
}{
	{ext: ".sha512", name: "sha512", new: sha512.New},
	{ext: ".sha256", name: "sha256", new: sha256.New},
	{ext: ".sha1", name: "sha1", new: sha1.New},
	{ext: ".md5", name: "md5", new: md5.New},
}

// checksumExt returns the matching checksum extension and the hash
// constructor for filename, or ("", "", nil) if filename is not a
// checksum companion.
func checksumExt(filename string) (string, string, func() hash.Hash) {
	for _, a := range checksumAlgos {
		if strings.HasSuffix(filename, a.ext) {
			return a.ext, a.name, a.new
		}
	}
	return "", "", nil
}

// verifyChecksumUpload validates that a Maven checksum companion file
// (.sha1 / .md5 / .sha256 / .sha512) actually matches the digest of the
// previously-uploaded artifact in the same (OwningRepo, OwningTag).
//
// Returns nil for non-checksum filenames so callers can invoke it
// unconditionally. Returns a 400-coded *httpError when:
//   - the body is malformed (not a hex digest of the right length),
//   - the companion artifact is not present (Maven uploads the artifact
//     first; a sha1 arriving before its jar is rejected so corrupt or
//     out-of-order pipelines fail loudly rather than store dangling
//     checksums),
//   - the body's digest disagrees with the recomputed digest of the
//     companion blob.
//
// For .sha256 the descriptor returned by ReadFile already carries the
// digest; we skip re-hashing the blob. For .sha1 / .md5 / .sha512 ORAS
// does not expose those, so we stream-hash the blob. Acceptable because
// checksum files always arrive immediately after the artifact (warm in
// the backend's cache) and re-hashing a few hundred MiB at the edge is
// negligible against the cost of accepting silently-corrupt artifacts.
func verifyChecksumUpload(ctx context.Context, registry handler.Registry, f *oci.RepoFile, body []byte) error {
	ext, algoName, newHash := checksumExt(f.Name)
	if ext == "" {
		return nil
	}

	companionName := strings.TrimSuffix(f.Name, ext)
	if companionName == "" {
		return badRequest("checksum filename %q has no companion artifact name", f.Name)
	}

	expected, err := parseChecksumBody(body)
	if err != nil {
		return badRequest("malformed %s checksum body: %v", algoName, err)
	}

	companion := &oci.RepoFile{
		OwningRepo: f.OwningRepo,
		OwningTag:  f.OwningTag,
		Name:       companionName,
	}
	desc, rc, err := registry.ReadFile(ctx, companion)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return badRequest("companion artifact %q must be uploaded before its %s checksum", companionName, algoName)
		}
		return fmt.Errorf("failed to read companion artifact %q: %w", companionName, err)
	}
	defer rc.Close()

	var actual string
	if ext == ".sha256" && desc != nil && desc.File.Digest.Algorithm() == "sha256" {
		// Backend already recorded the sha256 digest at push time —
		// reuse it instead of streaming the whole blob through a hash
		// we'd just compare to the same value.
		actual = desc.File.Digest.Hex()
	} else {
		h := newHash()
		if _, err := io.Copy(h, rc); err != nil {
			return fmt.Errorf("failed to hash companion artifact %q: %w", companionName, err)
		}
		actual = hex.EncodeToString(h.Sum(nil))
	}

	if !strings.EqualFold(actual, expected) {
		return badRequest("%s checksum mismatch for %q: client sent %q, computed %q", algoName, companionName, expected, actual)
	}
	return nil
}

// parseChecksumBody extracts the hex digest from a Maven checksum file.
// The canonical format is a single line of lowercase hex; some tools
// append "  filename" (two-space-separated, à la GNU coreutils) or
// "*filename". We accept any of these and ignore everything past the
// first whitespace run.
func parseChecksumBody(body []byte) (string, error) {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "", fmt.Errorf("empty body")
	}
	// Take the first whitespace-delimited token.
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		s = s[:i]
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("not a hex digest: %w", err)
	}
	return s, nil
}
