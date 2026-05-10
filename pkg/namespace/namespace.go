// Package namespace defines the namespace data model and OCI-backed
// storage. A namespace is a single URL prefix that operates as a
// logically-separate registry on top of the shared OCI backend (e.g.
// /myteam/simple/..., /myteam/maven2/...). The package owns:
//
//   - The typed [Namespace], [Spec], and policy types.
//   - The [Store] interface for create/read/update/delete plus listing.
//   - An OCI-backed [Store] implementation that uses *pkg/oci.Registry
//     as the only persistence backend (no DB, per project policy).
//
// The HTTP admin surface and any data-plane wrappers live in
// downstream packages and consume this one.
package namespace

import "encoding/json"

// Namespace is the in-memory representation of a namespace and its
// spec. Name is the namespace identifier as it appears in URLs;
// validation rules live on [ValidateName].
type Namespace struct {
	Name string
	Spec Spec
}

// Spec is the persisted body of a namespace. It is the single place
// where namespace-level configuration lives; the on-disk
// representation is the JSON encoding of this struct.
type Spec struct {
	// Policy carries the authz rules. Empty/absent Policy is treated
	// as deny-all by the authorizer (defined in a sibling design issue).
	// omitzero (Go 1.24+) drops the field when every Policy slice is
	// nil; omitempty alone would still emit "policy":{} for an empty
	// struct and lock that shape into the spec roundtrip golden.
	Policy Policy `json:"policy,omitzero"`

	// Format is reserved for future format-specific knobs (e.g.
	// per-format upload limits, immutability toggles). Leave empty for
	// v1; preserved across roundtrips so unknown keys written by a
	// newer ocifactory aren't silently dropped by an older one.
	Format map[string]json.RawMessage `json:"format,omitempty"`
}

// Policy is the authz block of a [Spec]. The matcher shape is a
// skeleton in this issue; the companion design issue fleshes out
// evaluation semantics.
type Policy struct {
	Readers []SubjectMatcher `json:"readers,omitempty"`
	Writers []SubjectMatcher `json:"writers,omitempty"`
}

// SubjectMatcher matches an authenticated subject against a set of
// claim predicates. All non-empty fields must match for the matcher
// to apply.
type SubjectMatcher struct {
	Issuer      string            `json:"issuer,omitempty"`
	SubMatch    string            `json:"sub_match,omitempty"`
	Email       string            `json:"email,omitempty"`
	ClaimsMatch map[string]string `json:"claims_match,omitempty"`

	// Kind selects the credential family this matcher applies to.
	// "oidc" (the default) matches OIDC identities; "basictoken"
	// matches static-token identities.
	Kind string `json:"kind,omitempty"`
}
