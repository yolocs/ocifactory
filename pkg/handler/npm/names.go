package npm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// packageOwningRepoPrefix is the OCI repo path prefix every published
// npm package lands under. The "u/" and "s/" sub-prefixes encode
// whether the original name was scoped without ambiguity — npm names
// cannot contain "/" themselves, and a scope name cannot start with
// "u" + "/" because npm forbids "/" in either half of the name.
const packageOwningRepoPrefix = "packages"

// validatePackageName returns nil when name is a syntactically valid
// npm package name (scoped or unscoped). It enforces the subset of
// rules that map cleanly onto OCI repo segments: lowercase alnum +
// "._-", no leading "." or "_", no whitespace, length cap, and the
// scoped form "@<scope>/<name>". Anything else (including "~",
// uppercase letters, or Unicode characters) is rejected.
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
		return nil
	}
	if !npmNameSegmentRegExp.MatchString(name) {
		return fmt.Errorf("%w: name %q has invalid characters", ErrInvalidPackageName, name)
	}
	return nil
}

// packageOwningRepo returns the OCI [oci.RepoFile.OwningRepo] string
// for an npm package. Callers must already have validated name via
// [validatePackageName]; this function does NOT re-validate.
//
//   - unscoped "foo"          → "packages/u/foo"
//   - scoped   "@scope/foo"   → "packages/s/scope/foo"
//
// The "u/" and "s/" sub-prefixes guarantee an unscoped name can never
// collide with the scope segment of a scoped name. Both forms are
// composed of npm-name segments, which are already OCI-distribution
// repo-segment-valid (lowercase alnum + ".-_").
func packageOwningRepo(name string) string {
	if strings.HasPrefix(name, "@") {
		rest := name[1:]
		slash := strings.IndexByte(rest, '/')
		scope, unscoped := rest[:slash], rest[slash+1:]
		return packageOwningRepoPrefix + "/s/" + scope + "/" + unscoped
	}
	return packageOwningRepoPrefix + "/u/" + name
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

// encodePackageNameTag encodes a package name into a value safe to use
// as an OCI tag in the [indexRepo]. OCI tags allow
// "[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}" so "@" and "/" must be encoded.
// Uses the same "_HH" percent-style scheme as
// pkg/namespace.encodeTag — uppercase hex, "_" itself escaped to
// "_5F" so the encoding is lossless.
//
// Callers must already have validated name via [validatePackageName].
func encodePackageNameTag(name string) (string, error) {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r == '_':
			b.WriteString("_5F")
		case r == '/':
			b.WriteString("_2F")
		case r == '@':
			b.WriteString("_40")
		case r == '.' || r == '-':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			return "", fmt.Errorf("%w: name %q contains character %q that cannot be encoded", ErrInvalidPackageName, name, r)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w: empty", ErrInvalidPackageName)
	}
	if b.Len() > 128 {
		return "", fmt.Errorf("%w: encoded name exceeds 128-char OCI tag limit", ErrInvalidPackageName)
	}
	return b.String(), nil
}

// decodePackageNameTag reverses [encodePackageNameTag]. A stray "_"
// not followed by two valid hex digits is treated as malformed.
func decodePackageNameTag(tag string) (string, error) {
	var b strings.Builder
	b.Grow(len(tag))
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(tag) {
			return "", fmt.Errorf("malformed escape at %d in %q: trailing underscore", i, tag)
		}
		hi, ok1 := unhex(tag[i+1])
		lo, ok2 := unhex(tag[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("malformed escape at %d in %q", i, tag)
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// tarballFilename returns the canonical tarball layer name for an npm
// (name, version) pair. Scoped names strip the "@scope/" prefix so
// the filename mirrors what real npm clients send in the
// _attachments map: "foo-1.0.0.tgz" for both "foo" and "@scope/foo".
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
