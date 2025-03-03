package cred

import "context"

// contextKey is a private string type to prevent collisions in the context map.
type contextKey string

// credKey points to the value in the context where the cred is stored.
const credKey = contextKey("cred")

// Cred represents the credentials used to authenticate with the OCI registry.
type Cred struct {
	Basic *BasicCred
}

// BasicCred represents the basic authentication credentials.
type BasicCred struct {
	User     string
	Password string
}

// WithCred adds the cred to the context.
func WithCred(ctx context.Context, cred *Cred) context.Context {
	return context.WithValue(ctx, credKey, cred)
}

// FromContext extracts the cred from the context.
func FromContext(ctx context.Context) (*Cred, bool) {
	cred, ok := ctx.Value(credKey).(*Cred)
	return cred, ok
}
