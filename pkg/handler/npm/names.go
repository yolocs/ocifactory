package npm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/yolocs/ocifactory/pkg/oci"
)

// ErrInvalidPackageName is returned when an npm package name does not
// satisfy the validation rules in [validatePackageName]. Handlers map
// it to HTTP 400.
var ErrInvalidPackageName = errors.New("invalid npm package name")

// maxPackageNameLength is the upper bound npm itself enforces on
// package names (214 chars including any "@scope/" prefix). Documented
// at https://github.com/npm/validate-npm-package-name.
const maxPackageNameLength = 214

// npmNameSegmentRegExp matches the unscoped portion of an npm package
// name and each half of a scoped name. npm's rules: lowercase only;
// alphanumeric, dot, hyphen, underscore; must start with a letter or
// digit (no leading dot or underscore). Tilde ("~") is technically
// permitted in some npm tooling but not in any OCI repository name
// segment regex, so we reject it.
var npmNameSegmentRegExp = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// validatePackageName returns nil when name is a syntactically valid
// npm package name (scoped or unscoped). It enforces the subset of
// rules that map cleanly onto OCI repo segments: lowercase alnum +
// "._-", no leading "." or "_", no whitespace, length cap, and the
// scoped form "@<scope>/<name>". Anything else (including "~",
// uppercase letters, or Unicode characters) is rejected.
//
// The encoded form (see [oci.EncodeTag]) is also checked
// against the 128-char OCI tag length cap so a syntactically valid
// but encoding-oversized name is rejected at publish time rather
// than silently failing later when [ensureIndexSentinel] tries to
// write the index repo entry.
func validatePackageName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidPackageName)
	}
	if len(name) > maxPackageNameLength {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidPackageName, maxPackageNameLength)
	}
	if strings.HasPrefix(name, "@") {
		// Scoped form: "@<scope>/<name>".
		rest := name[1:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return fmt.Errorf("%w: scoped name %q missing %q", ErrInvalidPackageName, name, "/")
		}
		scope, unscoped := rest[:slash], rest[slash+1:]
		if !npmNameSegmentRegExp.MatchString(scope) {
			return fmt.Errorf("%w: scope %q has invalid characters", ErrInvalidPackageName, scope)
		}
		if !npmNameSegmentRegExp.MatchString(unscoped) {
			return fmt.Errorf("%w: name %q has invalid characters", ErrInvalidPackageName, unscoped)
		}
		// Reject a second slash anywhere in the unscoped half —
		// IndexByte found only the first; a second would imply a
		// caller passed a path, not a name.
		if strings.ContainsRune(unscoped, '/') {
			return fmt.Errorf("%w: name %q contains %q", ErrInvalidPackageName, unscoped, "/")
		}
	} else if !npmNameSegmentRegExp.MatchString(name) {
		return fmt.Errorf("%w: name %q has invalid characters", ErrInvalidPackageName, name)
	}
	// Final guard: the index repo tag encoding has a hard 128-char
	// OCI tag cap. Names with many underscores or scoped names with
	// long components can exceed it after _HH expansion even though
	// they fit npm's own 214-char rule.
	if _, err := oci.EncodeTag(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPackageName, err)
	}
	return nil
}

// parsePackageOwningRepo reverses [packageOwningRepo]. Returns the
// npm name. Errors when owningRepo doesn't match either of the two
// shapes packageOwningRepo produces.
func parsePackageOwningRepo(owningRepo string) (string, error) {
	const sPrefix = packageOwningRepoPrefix + "/s/"
	const uPrefix = packageOwningRepoPrefix + "/u/"
	switch {
	case strings.HasPrefix(owningRepo, sPrefix):
		rest := strings.TrimPrefix(owningRepo, sPrefix)
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 || slash == len(rest)-1 {
			return "", fmt.Errorf("%w: scoped owning repo %q is malformed", ErrInvalidPackageName, owningRepo)
		}
		return "@" + rest[:slash] + "/" + rest[slash+1:], nil
	case strings.HasPrefix(owningRepo, uPrefix):
		rest := strings.TrimPrefix(owningRepo, uPrefix)
		if rest == "" || strings.ContainsRune(rest, '/') {
			return "", fmt.Errorf("%w: unscoped owning repo %q is malformed", ErrInvalidPackageName, owningRepo)
		}
		return rest, nil
	default:
		return "", fmt.Errorf("%w: owning repo %q lacks expected prefix", ErrInvalidPackageName, owningRepo)
	}
}

// tarballFilename returns the canonical tarball layer name for an npm
// (name, version) pair. Scoped names strip the "@scope/" prefix so the
// stored filename and the rewritten download URL stay simple (no path
// separators in the OCI layer name). For both "foo" and "@scope/foo"
// at version 1.0.0 this returns "foo-1.0.0.tgz".
//
// Callers must already have validated name.
func tarballFilename(name, version string) string {
	short := name
	if strings.HasPrefix(name, "@") {
		if slash := strings.IndexByte(name, '/'); slash >= 0 {
			short = name[slash+1:]
		}
	}
	return short + "-" + version + ".tgz"
}

// attachmentKey returns the key under which a real npm client stores
// a publish's tarball in the `_attachments` map of the publish JSON.
// npm always uses the full package name plus version, so for a scoped
// package "@scope/foo" at 1.0.0 the key is "@scope/foo-1.0.0.tgz".
// This differs from [tarballFilename], which we use for the stored
// OCI layer name and the rewritten download URL (where keeping a
// slash inside the filename segment is gratuitously awkward).
//
// Callers must already have validated name.
func attachmentKey(name, version string) string {
	return name + "-" + version + ".tgz"
}
