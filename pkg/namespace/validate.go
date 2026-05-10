package namespace

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidName is the sentinel for names rejected by [ValidateName].
var ErrInvalidName = errors.New("invalid namespace name")

// reservedNames is conservative on purpose: better to over-reserve in
// v1 than free a name that later collides with new server-internal
// routing. Covers built-in URL prefixes, per-format URL prefixes used
// by existing handlers, and OCI/internal prefixes used by the storage
// layer.
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

// ValidateName returns nil iff name is a legal namespace name:
// 1-64 lowercase ASCII alphanumerics and '-', no leading/trailing
// '-', no leading '_' or '.', not in [reservedNames]. Errors wrap
// [ErrInvalidName].
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
