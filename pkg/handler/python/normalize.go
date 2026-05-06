package python

import (
	"regexp"
	"strings"
)

// pep503Separators matches one or more of `.`, `-`, or `_`. Per PEP 503
// (https://peps.python.org/pep-0503/) any run of these characters in a
// project name collapses to a single `-`.
var pep503Separators = regexp.MustCompile(`[-_.]+`)

// normalize implements the PEP 503 name-normalization algorithm:
// lowercase, then replace every run of `.`, `-`, or `_` with a single
// `-`. `Foo_Bar`, `foo-bar`, and `foo.bar` all normalize to `foo-bar`.
//
// The python handler stores and looks up packages under the normalized
// name, so a `pip install foo-bar` after `twine upload Foo_Bar` resolves
// to the same package — the behaviour a spec-conforming PyPI client
// expects. Migration policy for un-normalized data already in an OCI
// backend is documented in docs/repos/python.md.
func normalize(name string) string {
	return pep503Separators.ReplaceAllString(strings.ToLower(name), "-")
}
