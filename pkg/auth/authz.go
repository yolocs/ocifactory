package auth

import (
	"context"
	"errors"
)

// Op enumerates the high-level operation kinds an authorizer
// distinguishes between. Today only read and write are defined —
// every routed handler in every format maps onto one of these. New
// operation kinds (delete, list, ...) get added as separate string
// constants when a format actually exposes a route that needs the
// distinction; until then keeping the surface narrow keeps policy
// configurations short and unambiguous.
type Op string

const (
	// OpRead covers GET / HEAD reads of artifact bytes and any
	// listing endpoint a client uses to enumerate available
	// artifacts. The split between "fetch one file" and "list every
	// file" is intentionally absent at this level — operators who
	// want a caller to read at all almost always want them to be
	// able to discover what is readable.
	OpRead Op = "read"

	// OpWrite covers PUT / POST uploads that mutate the registry.
	// Delete is folded into write today; ocifactory's surface does
	// not expose a delete endpoint to clients, so there is no
	// policy distinction to make.
	OpWrite Op = "write"
)

// Action is what an Authorizer is asked to allow or deny. Format
// handlers translate the inbound HTTP request into an Action right
// before authorizing — Repo matches the OCI repo path used by
// pkg/oci.RepoFile.OwningRepo so policies are written against the
// same names operators see in their backend OCI registry.
type Action struct {
	// Repo is the OCI repository path the action targets, e.g.
	// "packages/requests" for python, "com/example/foo" for maven,
	// or "index" for the python simple-index list endpoint.
	Repo string

	// Format is the artifact format the route belongs to, e.g.
	// "python" or "maven". Lets policy distinguish callers that
	// should be able to write Python wheels but not Maven jars
	// without the operator listing every per-language repo.
	Format string

	// Op is the operation kind. See OpRead / OpWrite.
	Op Op
}

// Authorizer decides whether ac may perform act. Implementations
// return nil to allow, ErrUnauthorized to deny, or any other error
// (wrapped however they like) to signal the authorizer itself
// failed.
//
// ctx is passed in so implementations can cancel or time out remote
// lookups (GitHub team membership, IAM bindings, ...) without
// having to plumb a context themselves. The static config-file
// implementation ignores it.
//
// ac is the *AuthContext the authentication middleware installed
// on the request context. It is never nil at the call site —
// authentication runs first and 401s the request when it cannot
// resolve a caller, so authorization only ever sees verified
// identities.
type Authorizer interface {
	Authorize(ctx context.Context, ac *AuthContext, act Action) error
}

// AuthorizerFunc adapts a plain function to the Authorizer
// interface. Useful in tests and for one-off authorizers.
type AuthorizerFunc func(ctx context.Context, ac *AuthContext, act Action) error

// Authorize calls f(ctx, ac, act).
func (f AuthorizerFunc) Authorize(ctx context.Context, ac *AuthContext, act Action) error {
	return f(ctx, ac, act)
}

// Sentinel errors. Authorizer implementations MUST return one of
// these (or wrap with %w) so per-format handlers can map the
// failure to the right HTTP status without inspecting concrete
// types.
var (
	// ErrUnauthorized is returned when the caller is known but is
	// not permitted to perform the requested action. Maps to 403.
	ErrUnauthorized = errors.New("not authorized")

	// ErrAuthzInternal is returned when the authorizer itself
	// failed (config-file unreadable, remote lookup down, ...).
	// Maps to 500 — distinct from ErrUnauthorized so transient
	// authorization-system failures don't look like a legitimate
	// policy denial.
	ErrAuthzInternal = errors.New("authorization error")
)

// AllowAll is a permit-everything Authorizer. Useful for local
// development, for tests that want to focus on something other
// than authorization, and for deployments running behind a
// separate authz layer (mesh policy, k8s NetworkPolicy + a single
// trusted client, ...). Production deployments behind no other
// gate should use a real authorizer.
var AllowAll Authorizer = AuthorizerFunc(func(context.Context, *AuthContext, Action) error {
	return nil
})

// DenyAll is the explicit deny authorizer. Tests use it to assert
// that a route actually calls the authorizer; production should
// never wire it.
var DenyAll Authorizer = AuthorizerFunc(func(context.Context, *AuthContext, Action) error {
	return ErrUnauthorized
})

// Check is the helper format handlers call at the start of every
// routed handler function once the Action has been parsed from the
// path / body. It loads the AuthContext from ctx and delegates to
// the authorizer.
//
// When authz is nil the function returns nil — handlers wired
// without an Authorizer (legacy, tests) stay open. The serve
// command always supplies one in production.
//
// When the AuthContext is missing the function returns
// ErrUnauthorized — the authentication middleware should have
// rejected the request before reaching here, so a missing context
// at this point is treated as deny rather than allow. Callers map
// this to 403 like any other authorization denial.
func Check(ctx context.Context, authz Authorizer, act Action) error {
	if authz == nil {
		return nil
	}
	ac, ok := FromContext(ctx)
	if !ok || ac == nil {
		return ErrUnauthorized
	}
	return authz.Authorize(ctx, ac, act)
}
