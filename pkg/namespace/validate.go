package namespace

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidName is the sentinel returned by [ValidateName] (and by
// [Store] methods that take a name) for any name that violates the
// namespace naming rules.
var ErrInvalidName = errors.New("invalid namespace name")

// reservedNames is the conservative deny-list of names ocifactory will
// not let an operator create. The list overlaps with built-in URL
// prefixes (admin, healthz, …), per-format URL prefixes used by
// existing handlers (simple, maven2, v2, npm), and the OCI/internal
// prefixes the storage layer reserves (_namespaces, _packages, _meta,
// _catalog). Better to over-reserve in v1 than to free a name that
// later collides with new server-internal routing.
var reservedNames = map[string]struct{}{
	"admin":       {},
	"healthz":     {},
	"readyz":      {},
	"metrics":     {},
	"simple":      {},
	"maven2":      {},
	"v2":          {},
	"npm":         {},
	"_meta":       {},
	"_catalog":    {},
	"_namespaces": {},
	"_packages":   {},
}

// ValidateName returns nil iff name is a legal namespace name. Rules:
//
//   - Length 1-64.
//   - Lowercase ASCII alphanumeric and '-' only.
//   - Must start and end with an alphanumeric (no leading/trailing '-').
//   - Must not start with '_' or '.' (reserved for internal use).
//   - Must not be one of the reserved names listed in [reservedNames].
//
// Errors wrap [ErrInvalidName] so callers can use errors.Is.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidName)
	}
	if len(name) > 64 {
		return fmt.Errorf("%w: %q exceeds 64 characters", ErrInvalidName, name)
	}
	if strings.HasPrefix(name, "_") {
		return fmt.Errorf("%w: %q starts with reserved prefix '_'", ErrInvalidName, name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: %q starts with reserved prefix '.'", ErrInvalidName, name)
	}
	if _, ok := reservedNames[name]; ok {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidName, name)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("%w: %q must not start or end with '-'", ErrInvalidName, name)
			}
		default:
			return fmt.Errorf("%w: %q contains invalid character %q", ErrInvalidName, name, r)
		}
	}
	return nil
}
