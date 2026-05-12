// Package namespace defines the namespace data model and OCI-backed
// metadata storage. A namespace is a single URL prefix that operates
// as a logically-separate registry on top of the shared OCI backend
// (e.g. /myteam/simple/..., /myteam/maven2/...).
package namespace

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CurrentSchemaVersion is the [Spec] shape ocifactory writes today.
// Bump only as part of a deliberate, backwards-incompatible change to
// the persisted shape; pair the bump with a read-side migration so
// older bodies remain loadable.
const CurrentSchemaVersion = 1

// ErrUnsupportedSchemaVersion is the sentinel returned by
// [Spec.Validate] when a body claims a schema version higher than
// [CurrentSchemaVersion]. Callers can surface it as a 400 to keep the
// operator-facing message clear.
var ErrUnsupportedSchemaVersion = errors.New("unsupported namespace schema_version")

// Namespace is a namespace and its spec. Name is the identifier as
// it appears in URLs; validation rules live on [ValidateName].
type Namespace struct {
	Name string `json:"name"`
	Spec Spec   `json:"spec"`
}

// Spec is the persisted body of a [Namespace].
type Spec struct {
	// SchemaVersion identifies the persisted-shape this body was
	// written against. Unset/zero is treated as 1 so legacy bodies
	// (written before this field was introduced) load without a
	// coordinated migration. ocifactory rejects bodies whose
	// SchemaVersion is greater than [CurrentSchemaVersion] rather
	// than silently dropping fields an older binary can't see.
	SchemaVersion int `json:"schema_version,omitempty"`

	// Policy is the authz block. An empty Policy is deny-all.
	//
	// omitzero (Go 1.24+) is required here: omitempty does not omit
	// a zero-value struct, which would lock "policy":{} into the
	// on-disk shape for every namespace that hasn't set a policy.
	Policy Policy `json:"policy,omitzero"`

	// Format is reserved for future format-specific knobs. Preserved
	// across roundtrips so a newer ocifactory's keys aren't silently
	// dropped by an older one.
	Format map[string]json.RawMessage `json:"format,omitempty"`
}

// Validate returns nil iff s is safe to persist and enforce.
func (s *Spec) Validate() error {
	if s == nil {
		return nil
	}
	v := s.SchemaVersion
	if v == 0 {
		v = 1
	}
	if v > CurrentSchemaVersion {
		return fmt.Errorf("%w %d (this ocifactory understands up to %d)", ErrUnsupportedSchemaVersion, v, CurrentSchemaVersion)
	}
	return s.Policy.Validate()
}

// Normalize stamps [CurrentSchemaVersion] onto s. The admin write path
// calls it before persisting so every body on disk carries an explicit
// version. Idempotent.
func (s *Spec) Normalize() {
	if s == nil {
		return
	}
	s.SchemaVersion = CurrentSchemaVersion
}
