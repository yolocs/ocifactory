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

// maxWheelEntries caps how many entries we tolerate in a wheel zip
// before refusing to scan further. Legitimate wheels carry a few
// hundred entries at most; this cap is well clear of that and well
// short of the 65 535 ZIP spec limit, so a malicious wheel cannot
// force archive/zip's File slice to grow without bound. When the cap
// trips, the upload still succeeds — handleFilePut just doesn't
// advertise a PEP 658 metadata companion for the file.
const maxWheelEntries = 4096

// errWheelTooManyEntries means the zip's central directory exceeds
// maxWheelEntries. Treated as "no metadata available" by callers.
var errWheelTooManyEntries = errors.New("wheel has too many zip entries")

// extractWheelMetadata reads a wheel zip via ReaderAt+size and returns
// the bytes of its `*.dist-info/METADATA` entry. The wheel spec
// guarantees at most one such file per wheel; if absent,
// errMetadataNotFound is returned.
//
// Defence-in-depth: the central directory is capped at maxWheelEntries
// to bound memory before we walk it, and a member's declared
// uncompressed size is checked against maxMetadataSize before opening
// it so a malformed ZIP64 header can't make the LimitReader irrelevant.
func extractWheelMetadata(r io.ReaderAt, size int64) ([]byte, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("read wheel zip: %w", err)
	}
	if len(zr.File) > maxWheelEntries {
		return nil, errWheelTooManyEntries
	}
	for _, f := range zr.File {
		if !wheelMetadataPath.MatchString(f.Name) {
			continue
		}
		if f.UncompressedSize64 > maxMetadataSize {
			return nil, fmt.Errorf("METADATA declared size %d exceeds cap %d", f.UncompressedSize64, maxMetadataSize)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open METADATA: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxMetadataSize+1))
		closeErr := rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read METADATA: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close METADATA: %w", closeErr)
		}
		if int64(len(data)) > maxMetadataSize {
			return nil, fmt.Errorf("METADATA exceeds %d bytes", maxMetadataSize)
		}
		return data, nil
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
	scanner.Buffer(make([]byte, 0, 4096), maxMetadataSize)
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
