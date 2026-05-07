package python

import (
	"bufio"
	"bytes"
	"errors"
	"path"
	"regexp"
	"strings"
)

// errMetadataNotFound is returned by the streaming METADATA extractor
// when the wheel zip has no `<distinfo>/METADATA` member. The upload
// path treats this as "no PEP 658 metadata to advertise" rather than a
// hard failure — the upload still succeeds; clients just lose the
// metadata fast-path.
var errMetadataNotFound = errors.New("wheel METADATA not found")

// wheelMetadataPath matches the `<distinfo>/METADATA` member inside a
// wheel zip per the wheel binary format (PEP 427).
var wheelMetadataPath = regexp.MustCompile(`^[^/]+\.dist-info/METADATA$`)

// maxMetadataSize caps how much of a `<wheel>.metadata` companion file
// the simple-index renderer reads when extracting Requires-Python and
// how much METADATA the streaming extractor will buffer during an
// upload. Real-world METADATA blobs are a few KB; 1 MiB is a generous
// upper bound that keeps a malicious or corrupted file from pinning
// memory.
const maxMetadataSize = 1 << 20

// maxWheelEntries caps how many local file headers the streaming
// METADATA extractor walks before refusing to continue. Legitimate
// wheels carry a few hundred entries at most; this cap is well clear
// of that and well short of the 65 535 ZIP spec limit. When the cap
// trips, the upload still succeeds — handleFilePut just doesn't
// advertise a PEP 658 metadata companion for the file.
const maxWheelEntries = 4096

// errWheelTooManyEntries means the zip exceeds maxWheelEntries local
// file headers. Treated as "no metadata available" by callers.
var errWheelTooManyEntries = errors.New("wheel has too many zip entries")

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
