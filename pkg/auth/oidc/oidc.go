// Package oidc verifies OIDC ID tokens issued by a configured
// trusted issuer. One Authenticator instance handles one issuer;
// chain multiple to accept Google + GitHub Actions + ... .
package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/yolocs/ocifactory/pkg/auth"
)

// Authenticator is an auth.Authenticator that verifies OIDC ID
// tokens against one trusted issuer. Discovery (and JWKS fetch)
// happens lazily on the first request — initialisation failure
// surfaces as auth.ErrIssuerUnavailable so the operator can fix the
// network without restarting.
type Authenticator struct {
	issuer   string
	audience string

	httpClient *http.Client

	// mu guards lazy initialisation of provider/verifier. Once
	// set both fields are immutable (the underlying go-oidc
	// verifier handles JWKS rotation internally).
	mu       sync.RWMutex
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
}

// Option customises an Authenticator at construction time.
type Option func(*Authenticator)

// WithHTTPClient overrides the http.Client used for OIDC discovery
// and JWKS fetches. Defaults to http.DefaultClient. Tests use this
// to point at a fake JWKS server.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Authenticator) {
		if c != nil {
			a.httpClient = c
		}
	}
}

// New returns an Authenticator configured for one issuer URL and a
// required audience. issuer must match the token's iss claim
// exactly; audience must appear in the token's aud claim (which
// may be a string or array — go-oidc handles both).
//
// Discovery is deferred until the first Authenticate call so a
// process can boot without network access to the issuer; the
// trade-off is that the first request after a transient outage
// pays the discovery cost.
func New(issuer, audience string, opts ...Option) (*Authenticator, error) {
	if issuer == "" {
		return nil, errors.New("oidc: issuer is required")
	}
	if audience == "" {
		return nil, errors.New("oidc: audience is required")
	}
	a := &Authenticator{
		issuer:     issuer,
		audience:   audience,
		httpClient: http.DefaultClient,
	}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Issuer returns the configured issuer URL. Useful for logs and
// for the chain config to identify which authenticator is in play.
func (a *Authenticator) Issuer() string { return a.issuer }

// ensureVerifier lazily performs OIDC discovery against the
// configured issuer. Subsequent calls are read-locked fast paths.
//
// Returns auth.ErrIssuerUnavailable on any discovery failure so the
// middleware can map to 503 rather than 401. Once discovery
// succeeds the underlying *oidc.IDTokenVerifier is reused for the
// process's lifetime; go-oidc handles JWKS rotation internally
// (including a kid-miss refresh).
func (a *Authenticator) ensureVerifier(ctx context.Context) (*gooidc.IDTokenVerifier, error) {
	a.mu.RLock()
	v := a.verifier
	a.mu.RUnlock()
	if v != nil {
		return v, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, nil
	}

	dctx := gooidc.ClientContext(ctx, a.httpClient)
	provider, err := gooidc.NewProvider(dctx, a.issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: oidc discovery: %v", auth.ErrIssuerUnavailable, err)
	}
	a.provider = provider
	a.verifier = provider.Verifier(&gooidc.Config{ClientID: a.audience})
	return a.verifier, nil
}

// Authenticate extracts an OIDC token from r (Bearer header or
// sentinel-Basic), verifies it against the configured issuer +
// audience, and returns a Subject populated from the verified
// claims.
//
// Returns:
//   - auth.ErrNoCredential when the request carries no Bearer or
//     sentinel-Basic credential.
//   - auth.ErrIssuerUnavailable when discovery hasn't completed
//     (transient — middleware maps to 503).
//   - auth.ErrInvalidToken on signature, issuer, audience,
//     expiry, or not-yet-valid failures.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.Subject, error) {
	rawToken, ok := auth.ExtractOIDCToken(r)
	if !ok {
		return nil, auth.ErrNoCredential
	}

	verifier, err := a.ensureVerifier(r.Context())
	if err != nil {
		return nil, err
	}

	idToken, err := verifier.Verify(r.Context(), rawToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: claims decode: %v", auth.ErrInvalidToken, err)
	}

	subj := &auth.Subject{
		Issuer: idToken.Issuer,
		ID:     idToken.Subject,
		Claims: claims,
	}
	if email, _ := claims["email"].(string); email != "" {
		if verified, _ := claims["email_verified"].(bool); verified {
			subj.Email = email
		}
	}
	return subj, nil
}
