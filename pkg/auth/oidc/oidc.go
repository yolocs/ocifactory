// Package oidc verifies OIDC ID tokens issued by a configured
// trusted issuer. One Authenticator instance handles one issuer;
// chain multiple to accept Google + GitHub Actions + ... .
package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/yolocs/ocifactory/pkg/auth"
)

// defaultHTTPTimeout is the per-request timeout applied to the
// default http.Client used for OIDC discovery and JWKS fetch. A
// slow or malicious issuer can otherwise hang the first auth
// call indefinitely. Operators can override the whole client
// via WithHTTPClient.
const defaultHTTPTimeout = 10 * time.Second

// maxDiscoveryBytes caps the body size of the OIDC discovery
// document and the JWKS document the default client will read. A
// hostile issuer otherwise OOMs the server at boot. The cap is
// generous (Google's JWKS sits ~3 KiB, GitHub's ~3 KiB; OIDC
// discovery docs are <2 KiB) so legitimate issuers never trip it.
const maxDiscoveryBytes = 1 << 20 // 1 MiB

// maxJWTBytes is the largest token peekIssuer will inspect before
// rejecting outright. JWTs of this size are pathological: a real
// Google ID token is ~1 KiB, a GitHub Actions OIDC token ~2 KiB.
// The cap exists to keep the unauthenticated-request path from
// turning into a per-request CPU/memory pressure vector when an
// attacker streams oversized Bearer values within Go's default
// MaxHeaderBytes.
const maxJWTBytes = 8 * 1024

// supportedSigningAlgs pins the asymmetric algorithms the verifier
// will accept. Pinning closes the door on a malicious discovery
// document advertising a symmetric alg (e.g. HS256) wired to a
// JWKS the attacker controls. RS256 + ES256 covers what every real
// public OIDC issuer uses today.
var supportedSigningAlgs = []string{"RS256", "ES256"}

// Authenticator is an auth.Authenticator that verifies OIDC ID
// tokens against one trusted issuer. Discovery (and JWKS fetch)
// happens lazily on the first request — initialisation failure
// surfaces as auth.ErrIssuerUnavailable so the operator can fix the
// network without restarting.
type Authenticator struct {
	issuer        string
	audience      string
	allowInsecure bool

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
// and JWKS fetches. Use this to plumb a corporate proxy, supply
// custom TLS, or stub the transport in tests. Defaults to a
// process-shared client with a sensible timeout and a body-size
// cap on responses.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Authenticator) {
		if c != nil {
			a.httpClient = c
		}
	}
}

// AllowInsecureIssuer opts in to accepting an `http://` issuer URL.
// Off by default — JWKS over plaintext makes signature verification
// trivially MITM-able. Tests use this to point at a local httptest
// server; production must not.
func AllowInsecureIssuer() Option {
	return func(a *Authenticator) {
		a.allowInsecure = true
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
		httpClient: defaultHTTPClient(),
	}
	for _, o := range opts {
		o(a)
	}
	if !a.allowInsecure && strings.HasPrefix(issuer, "http://") {
		return nil, fmt.Errorf("oidc: refusing http:// issuer %q (use https:// or AllowInsecureIssuer for tests)", issuer)
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
// middleware can map to 503 rather than 401. On failure we do NOT
// memoize; the next request retries discovery — required by the
// design's "JWKS endpoint unreachable on startup → don't crash"
// rule. Once discovery succeeds the underlying *oidc.IDTokenVerifier
// is reused for the process's lifetime; go-oidc handles JWKS
// rotation internally (including a kid-miss refresh).
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
		return nil, fmt.Errorf("%w: oidc discovery for %s: %w", auth.ErrIssuerUnavailable, a.issuer, err)
	}
	a.provider = provider
	a.verifier = provider.Verifier(&gooidc.Config{
		ClientID: a.audience,
		// Pin every check to its safe default explicitly. The
		// zero value of these fields is already false, but
		// pinning them documents intent and survives go-oidc
		// future struct changes.
		SkipClientIDCheck:          false,
		SkipExpiryCheck:            false,
		SkipIssuerCheck:            false,
		InsecureSkipSignatureCheck: false,
		// Pin the algorithm allowlist so a malicious
		// discovery doc can't advertise (e.g.) HS256 paired
		// with a JWKS the attacker controls.
		SupportedSigningAlgs: supportedSigningAlgs,
	})
	return a.verifier, nil
}

// Authenticate extracts an OIDC token from r (Bearer header or
// sentinel-Basic), peeks the unverified `iss` claim to see if this
// authenticator owns the token, and on a match verifies it against
// the configured issuer + audience.
//
// The unverified-iss peek is what makes a chain of multiple
// `oidc.Authenticator`s work: a token issued by GitHub Actions
// hits the Google authenticator first, doesn't match its iss, and
// returns ErrNoCredential so the chain falls through. Without the
// peek every wrong-issuer token would short-circuit the chain
// with ErrInvalidToken.
//
// Returns:
//   - auth.ErrNoCredential when the request carries no Bearer or
//     sentinel-Basic credential, or when the token's iss doesn't
//     match this authenticator (so the chain can try the next).
//   - auth.ErrIssuerUnavailable when discovery hasn't completed
//     (transient — middleware maps to 503).
//   - auth.ErrInvalidToken on signature, audience, expiry, or
//     not-yet-valid failures, OR on a malformed JWT that we can't
//     even peek.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.AuthContext, error) {
	rawToken, ok := auth.ExtractOIDCToken(r)
	if !ok {
		return nil, auth.ErrNoCredential
	}

	iss, err := peekIssuer(rawToken)
	if err != nil {
		// Unparseable JWT. Don't claim it — let the chain
		// move on. If no later authenticator handles it the
		// middleware emits 401 ErrNoCredential, which is the
		// right response for "I don't recognise this".
		return nil, auth.ErrNoCredential
	}
	if iss != a.issuer {
		return nil, auth.ErrNoCredential
	}

	verifier, err := a.ensureVerifier(r.Context())
	if err != nil {
		return nil, err
	}

	idToken, err := verifier.Verify(r.Context(), rawToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: claims decode: %w", auth.ErrInvalidToken, err)
	}

	subj := &auth.AuthContext{
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

// peekIssuer returns the iss claim from a JWT WITHOUT verifying
// the signature. Used only to route the token to the right
// authenticator in a multi-issuer chain; the actual verification
// (which validates iss, aud, exp, signature) happens afterwards.
//
// A malformed token surfaces as a non-nil error; the caller treats
// that as "not my problem" and returns ErrNoCredential so the
// chain keeps going.
func peekIssuer(rawJWT string) (string, error) {
	if len(rawJWT) > maxJWTBytes {
		return "", fmt.Errorf("token exceeds %d bytes", maxJWTBytes)
	}
	parts := strings.Split(rawJWT, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("base64 decode payload: %w", err)
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", fmt.Errorf("json decode payload: %w", err)
	}
	return c.Iss, nil
}

// defaultHTTPClient is the http.Client used for discovery and JWKS
// when WithHTTPClient isn't passed. It enforces a per-request
// timeout and bounds the response body so a hostile or
// misconfigured issuer can't OOM or hang the server.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultHTTPTimeout,
		Transport: &boundedBodyTransport{
			inner:    http.DefaultTransport,
			maxBytes: maxDiscoveryBytes,
		},
	}
}

// boundedBodyTransport wraps an http.RoundTripper so every response
// body is capped at maxBytes. Used to defend the OIDC default
// client against unbounded JWKS / discovery payloads.
type boundedBodyTransport struct {
	inner    http.RoundTripper
	maxBytes int64
}

func (t *boundedBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedReadCloser{rc: resp.Body, n: t.maxBytes}
	return resp, nil
}

type cappedReadCloser struct {
	rc io.ReadCloser
	n  int64
}

func (c *cappedReadCloser) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, fmt.Errorf("oidc: response body exceeds %d bytes", maxDiscoveryBytes)
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.rc.Read(p)
	c.n -= int64(n)
	return n, err
}

func (c *cappedReadCloser) Close() error { return c.rc.Close() }
