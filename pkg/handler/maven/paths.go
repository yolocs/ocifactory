package maven

import (
	"fmt"
	"strings"
)

// validatePath rejects path components that contain traversal segments
// (".", ".."), empty segments, or characters outside the conservative
// Maven coordinate grammar [A-Za-z0-9._-] (with "/" only allowed inside
// repoParts, where it separates groupId/artifactId path components).
//
// The Maven coordinate grammar is well-defined: groupId / artifactId /
// version segments match [A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*. Filenames
// add the extension dot but otherwise stay within the same character
// class. Anything outside this grammar is non-conformant and a likely
// traversal attempt — we reject at the handler boundary as defence in
// depth, before the OwningRepo string flows into ORAS.
//
// Empty inputs are tolerated (so callers can pass "" for routes where
// only some components are bound).
func validatePath(repoParts, version, filename string) error {
	if repoParts != "" {
		if err := validateRepoParts(repoParts); err != nil {
			return fmt.Errorf("invalid repoParts %q: %w", repoParts, err)
		}
	}
	if version != "" {
		if err := validateSegment(version); err != nil {
			return fmt.Errorf("invalid version %q: %w", version, err)
		}
	}
	if filename != "" {
		if err := validateSegment(filename); err != nil {
			return fmt.Errorf("invalid filename %q: %w", filename, err)
		}
	}
	return nil
}

// validateRepoParts splits repoParts on "/" and validates each segment
// individually. Leading, trailing, or doubled slashes produce empty
// segments and are rejected.
func validateRepoParts(repoParts string) error {
	if strings.HasPrefix(repoParts, "/") || strings.HasSuffix(repoParts, "/") {
		return fmt.Errorf("leading or trailing slash")
	}
	for _, seg := range strings.Split(repoParts, "/") {
		if err := validateSegment(seg); err != nil {
			return err
		}
	}
	return nil
}

// validateSegment rejects empty, ".", "..", and any segment containing
// characters outside [A-Za-z0-9._-]. Maven group/artifact/version
// segments and artifact filenames all fit this class.
func validateSegment(seg string) error {
	switch seg {
	case "":
		return fmt.Errorf("empty segment")
	case ".", "..":
		return fmt.Errorf("traversal segment %q", seg)
	}
	for _, r := range seg {
		if !isSafePathChar(r) {
			return fmt.Errorf("invalid character %q", r)
		}
	}
	return nil
}

func isSafePathChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '-':
		return true
	}
	return false
}
