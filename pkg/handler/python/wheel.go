package python

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// errMetadataNotFound is returned by extractWheelMetadata when the zip
// has no `*.dist-info/METADATA` member. handleFilePut treats this as
// "no PEP 658 metadata to advertise" rather than a hard failure — the
// upload still succeeds; clients just lose the metadata fast-path.
var errMetadataNotFound = errors.New("wheel METADATA not found")

// wheelMetadataPath matches the `*.dist-info/METADATA` member inside a
// wheel zip per the wheel binary format (PEP 427).
var wheelMetadataPath = regexp.MustCompile(`^[^/]+\.dist-info/METADATA$`)

// maxMetadataSize caps how much of a `<wheel>.metadata` companion file
// the simple-index renderer reads when extracting Requires-Python.
// Real-world METADATA blobs are a few KB; 1 MiB is a generous upper
// bound that keeps a malicious or corrupted file from pinning memory.
const maxMetadataSize = 1 << 20

// extractWheelMetadata reads a wheel zip via ReaderAt+size and returns
// the bytes of its `*.dist-info/METADATA` entry. The wheel spec
// guarantees at most one such file per wheel; if absent,
// errMetadataNotFound is returned.
func extractWheelMetadata(r io.ReaderAt, size int64) ([]byte, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("read wheel zip: %w", err)
	}
	for _, f := range zr.File {
		if !wheelMetadataPath.MatchString(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open METADATA: %w", err)
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxMetadataSize+1))
	}
	return nil, errMetadataNotFound
}

// parseRequiresPython extracts the value of the `Requires-Python` header
// from a wheel METADATA blob, returning "" if absent. METADATA is
// RFC 5322-style: a blank line ends the header section, and the
// Core Metadata spec disallows duplicates so the first match is
// authoritative.
func parseRequiresPython(meta []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(meta))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, "Requires-Python:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isWheelFilename reports whether name looks like a wheel
// (`*.whl`). PEP 658 only mandates the METADATA companion for wheels;
// sdist PKG-INFO support is out of scope per issue #54.
func isWheelFilename(name string) bool {
	return strings.EqualFold(path.Ext(name), ".whl")
}

// metadataCompanionName returns the PEP 658 companion file name for a
// given wheel filename. Centralised so the upload path and the
// simple-index renderer stay in agreement.
func metadataCompanionName(wheelName string) string {
	return wheelName + ".metadata"
}
