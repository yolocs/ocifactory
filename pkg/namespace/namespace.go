// Package namespace defines the namespace data model and OCI-backed
// metadata storage. A namespace is a single URL prefix that operates
// as a logically-separate registry on top of the shared OCI backend
// (e.g. /myteam/simple/..., /myteam/maven2/...).
package namespace

import "encoding/json"

// Namespace is a namespace and its spec. Name is the identifier as
// it appears in URLs; validation rules live on [ValidateName].
type Namespace struct {
	Name string
	Spec Spec
}

// Spec is the persisted body of a [Namespace].
type Spec struct {
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

// Policy is the authz block of a [Spec].
type Policy struct {
	Readers []SubjectMatcher `json:"readers,omitempty"`
	Writers []SubjectMatcher `json:"writers,omitempty"`
}

// SubjectMatcher matches an authenticated subject. All non-empty
// fields must match for the matcher to apply.
type SubjectMatcher struct {
	Issuer      string            `json:"issuer,omitempty"`
	SubMatch    string            `json:"sub_match,omitempty"`
	Email       string            `json:"email,omitempty"`
	ClaimsMatch map[string]string `json:"claims_match,omitempty"`
	// Kind selects the credential family. "oidc" (default) matches
	// OIDC identities; "basictoken" matches static-token identities.
	Kind string `json:"kind,omitempty"`
}
