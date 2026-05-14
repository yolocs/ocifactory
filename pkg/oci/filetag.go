package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fileTagPrefix marks deterministic file-manifest tags so they can be
// filtered out of the "versions + aliases" view exposed via ListTags.
// The OCI tag grammar ([A-Za-z0-9_][A-Za-z0-9._-]{0,127}) admits the
// prefix, and "_f_" is short enough that the full tag stays well under
// the 128-character limit even with a 64-char sha256 hex suffix (66
// chars total).
const fileTagPrefix = "_f_"

// fileTagFor maps (owningTag, filename) onto the deterministic OCI tag
// that AddFile attaches to the file manifest. ReadFile and
// BlobRedirectURL resolve this tag directly, replacing the
// version-manifest → Referrers → file-manifest descriptor triple with a
// single Resolve. The 0x00 separator prevents "1.0" + "0foo" colliding
// with "1.00" + "foo".
//
// The handler is responsible for any normalization it wants on the
// inputs (lowercasing, percent-decoding, etc.) before hashing — this
// helper is just a deterministic mapper, not a normalizer.
func fileTagFor(owningTag, filename string) string {
	h := sha256.New()
	h.Write([]byte(owningTag))
	h.Write([]byte{0x00})
	h.Write([]byte(filename))
	return fileTagPrefix + hex.EncodeToString(h.Sum(nil))
}

// isFileTag reports whether tag was produced by fileTagFor. Used to
// keep deterministic file tags out of the "versions + aliases" view in
// ListTags and to skip them in ListFiles' per-tag iteration.
func isFileTag(tag string) bool {
	return strings.HasPrefix(tag, fileTagPrefix)
}
