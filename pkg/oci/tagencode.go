package oci

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// EncodeTag escapes an arbitrary string into a value that is valid as
// an OCI tag. The encoding uses "_HH" uppercase hex escapes, including
// "_5F" for "_" itself, so it round-trips without collisions.
func EncodeTag(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isSafeTagByte(c) && (b.Len() > 0 || isSafeFirstTagByte(c)) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "_%02X", c)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("encoded OCI tag must not be empty")
	}
	if b.Len() > 128 {
		return "", fmt.Errorf("encoded OCI tag exceeds 128-byte limit")
	}
	return b.String(), nil
}

// DecodeTag reverses [EncodeTag].
func DecodeTag(tag string) (string, error) {
	var b strings.Builder
	b.Grow(len(tag))
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(tag) {
			return "", fmt.Errorf("malformed escape at %d: trailing underscore", i)
		}
		decoded, err := hex.DecodeString(tag[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("malformed escape at %d: %w", i, err)
		}
		b.WriteByte(decoded[0])
		i += 2
	}
	return b.String(), nil
}

func isSafeFirstTagByte(c byte) bool {
	return isAlphaNum(c)
}

func isSafeTagByte(c byte) bool {
	return isAlphaNum(c) || c == '.' || c == '-'
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
