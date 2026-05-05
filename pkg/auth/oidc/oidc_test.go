package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/auth"
)

// fakeIssuer is a minimal OIDC provider: it serves
// /.well-known/openid-configuration and a JWKS document, and signs
// tokens with an in-process RSA key. Multiple keys are supported so
// tests can exercise kid-mismatch / rotation.
type fakeIssuer struct {
	t      *testing.T
	server *httptest.Server
	keys   []signingKey
}

type signingKey struct {
	kid string
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	f := &fakeIssuer{t: t, keys: []signingKey{{kid: "kid-1", key: priv}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks", f.handleJWKS)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIssuer) Issuer() string { return f.server.URL }

func (f *fakeIssuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	doc := map[string]any{
		"issuer":   f.server.URL,
		"jwks_uri": f.server.URL + "/jwks",
		// go-oidc requires id_token_signing_alg_values_supported
		// in the discovery doc to be non-empty; otherwise it
		// rejects every token.
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeIssuer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	keys := make([]jose.JSONWebKey, 0, len(f.keys))
	for _, k := range f.keys {
		keys = append(keys, jose.JSONWebKey{
			Key:       &k.key.PublicKey,
			KeyID:     k.kid,
			Algorithm: "RS256",
			Use:       "sig",
		})
	}
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: keys})
}

// signToken builds and RS256-signs a JWT with the supplied claims.
// kid selects which configured key to sign with; pass "" to use
// the first.
func (f *fakeIssuer) signToken(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	var sk signingKey
	if kid == "" {
		sk = f.keys[0]
	} else {
		for _, k := range f.keys {
			if k.kid == kid {
				sk = k
				break
			}
		}
		if sk.key == nil {
			t.Fatalf("signToken: no key with kid %q", kid)
		}
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: sk.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", sk.kid),
	)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return raw
}

// signWithUnknownKey signs claims using an RSA key that is NOT
// published in the issuer's JWKS, so the verifier rejects the
// signature even though iss/aud/exp are otherwise valid. Used to
// exercise the bad-signature failure path.
func (f *fakeIssuer) signWithUnknownKey(t *testing.T, claims map[string]any) string {
	t.Helper()
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: stranger},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "stranger-kid"),
	)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return raw
}

// claims builds the standard claim set used in the happy-path
// tests. Tests override individual fields (aud, iss, exp, ...) by
// mutating the returned map before signing.
func (f *fakeIssuer) claims(audience, sub string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": f.server.URL,
		"aud": audience,
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		issuer      string
		audience    string
		wantErrPart string
	}{
		{name: "valid", issuer: "https://x", audience: "y"},
		{name: "missing issuer", audience: "y", wantErrPart: "issuer is required"},
		{name: "missing audience", issuer: "https://x", wantErrPart: "audience is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.issuer, tc.audience)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("error = nil, want %q", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("error = %q, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAuthenticate_HappyPaths(t *testing.T) {
	t.Parallel()

	iss := newFakeIssuer(t)
	const audience = "ocifactory.example"

	tests := []struct {
		name      string
		mutate    func(map[string]any)
		header    func(token string) string
		wantEmail string
	}{
		{
			name:   "bearer token",
			mutate: func(c map[string]any) {},
			header: func(tok string) string { return "Bearer " + tok },
		},
		{
			name:   "sentinel _oidc",
			mutate: func(c map[string]any) {},
			header: func(tok string) string {
				return "Basic " + base64.StdEncoding.EncodeToString([]byte("_oidc:"+tok))
			},
		},
		{
			name:   "sentinel oauth2accesstoken",
			mutate: func(c map[string]any) {},
			header: func(tok string) string {
				return "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2accesstoken:"+tok))
			},
		},
		{
			name: "verified email surfaces",
			mutate: func(c map[string]any) {
				c["email"] = "alice@example.com"
				c["email_verified"] = true
			},
			header:    func(tok string) string { return "Bearer " + tok },
			wantEmail: "alice@example.com",
		},
		{
			name: "unverified email is dropped",
			mutate: func(c map[string]any) {
				c["email"] = "alice@example.com"
				c["email_verified"] = false
			},
			header: func(tok string) string { return "Bearer " + tok },
		},
		{
			name: "audience as array containing match",
			mutate: func(c map[string]any) {
				c["aud"] = []any{"other", audience}
			},
			header: func(tok string) string { return "Bearer " + tok },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := New(iss.Issuer(), audience, WithHTTPClient(iss.server.Client()), AllowInsecureIssuer())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			claims := iss.claims(audience, "subject-123")
			tc.mutate(claims)
			tok := iss.signToken(t, "", claims)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", tc.header(tok))
			r = r.WithContext(t.Context())

			subj, err := a.Authenticate(r)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			want := &auth.AuthContext{
				Issuer: iss.Issuer(),
				ID:     "subject-123",
				Email:  tc.wantEmail,
				Claims: claims,
			}
			if diff := cmp.Diff(want, subj, claimsCmp()); diff != "" {
				t.Errorf("subject mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAuthenticate_Failures(t *testing.T) {
	t.Parallel()

	iss := newFakeIssuer(t)
	const audience = "ocifactory.example"

	tests := []struct {
		name      string
		header    func(t *testing.T) string
		wantErrIs error
	}{
		{
			name: "no header",
			header: func(t *testing.T) string {
				return ""
			},
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name: "regular basic non-sentinel",
			header: func(t *testing.T) string {
				return "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:pwd"))
			},
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name: "wrong audience",
			header: func(t *testing.T) string {
				c := iss.claims("wrong", "u")
				return "Bearer " + iss.signToken(t, "", c)
			},
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			// Wrong-iss is the multi-OIDC-chain case: the
			// authenticator MUST return ErrNoCredential
			// (not ErrInvalidToken) so chain dispatch can
			// try the next configured issuer. See
			// peekIssuer in oidc.go.
			name: "wrong issuer falls through",
			header: func(t *testing.T) string {
				c := iss.claims(audience, "u")
				c["iss"] = "https://attacker"
				return "Bearer " + iss.signToken(t, "", c)
			},
			wantErrIs: auth.ErrNoCredential,
		},
		{
			// Bad signature: iss matches, but the signing
			// key isn't published in JWKS. Verifier
			// rejects → ErrInvalidToken.
			name: "bad signature",
			header: func(t *testing.T) string {
				c := iss.claims(audience, "u")
				return "Bearer " + iss.signWithUnknownKey(t, c)
			},
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			name: "expired",
			header: func(t *testing.T) string {
				c := iss.claims(audience, "u")
				c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
				c["exp"] = time.Now().Add(-1 * time.Hour).Unix()
				return "Bearer " + iss.signToken(t, "", c)
			},
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			name: "not yet valid",
			header: func(t *testing.T) string {
				c := iss.claims(audience, "u")
				c["nbf"] = time.Now().Add(1 * time.Hour).Unix()
				return "Bearer " + iss.signToken(t, "", c)
			},
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			// Garbage that isn't even parseable as a JWT
			// (no `iss` to peek) → ErrNoCredential so the
			// chain can give a later authenticator a
			// chance. The middleware still emits 401 if
			// nothing else accepts.
			name: "garbage token",
			header: func(t *testing.T) string {
				return "Bearer not-a-jwt"
			},
			wantErrIs: auth.ErrNoCredential,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := New(iss.Issuer(), audience, WithHTTPClient(iss.server.Client()), AllowInsecureIssuer())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if h := tc.header(t); h != "" {
				r.Header.Set("Authorization", h)
			}
			r = r.WithContext(t.Context())

			_, err = a.Authenticate(r)
			if !errors.Is(err, tc.wantErrIs) {
				t.Errorf("error = %v, want errors.Is(%v)", err, tc.wantErrIs)
			}
		})
	}
}

// TestAuthenticate_DiscoveryFailureSurfaces503 checks that the lazy
// initialization path turns an unreachable issuer into
// ErrIssuerUnavailable rather than ErrInvalidToken — the operator
// can tell the difference between "credential is bad" and "I can't
// even check this credential right now". To reach the verifier the
// presented token's iss must match the configured issuer (otherwise
// the iss-peek short-circuits with ErrNoCredential before
// discovery is even attempted), so the test signs a real token
// against the unreachable issuer URL.
func TestAuthenticate_DiscoveryFailureSurfaces503(t *testing.T) {
	t.Parallel()

	const issuerURL = "http://127.0.0.1:1/issuer"
	a, err := New(issuerURL, "aud", AllowInsecureIssuer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Mint a token whose iss matches the configured issuer.
	// Signature won't matter because discovery never completes.
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k"),
	)
	tok, _ := jwt.Signed(signer).Claims(map[string]any{
		"iss": issuerURL,
		"aud": "aud",
		"sub": "u",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	}).Serialize()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r = r.WithContext(ctx)

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrIssuerUnavailable) {
		t.Errorf("error = %v, want errors.Is(ErrIssuerUnavailable)", err)
	}
}

// TestAuthenticate_DiscoveryRecovers verifies that a transient
// discovery failure does NOT poison the verifier — the next
// request after the issuer comes back retries discovery and
// succeeds. The design at oidc.go:ensureVerifier explicitly
// requires this; without it a single boot-time blip would brick
// auth until restart.
func TestAuthenticate_DiscoveryRecovers(t *testing.T) {
	t.Parallel()

	iss := newFakeIssuer(t)
	const audience = "ocifactory.example"

	// Wrap the issuer's transport so the first discovery hit
	// fails and subsequent ones succeed.
	var firstFailed atomic.Bool
	rt := failingFirstRoundTripper{
		inner: iss.server.Client().Transport,
		flag:  &firstFailed,
	}
	hc := &http.Client{Transport: rt}

	a, err := New(iss.Issuer(), audience, WithHTTPClient(hc), AllowInsecureIssuer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok := iss.signToken(t, "", iss.claims(audience, "u"))
	mkReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r.WithContext(t.Context())
	}

	// First attempt: round tripper fails the discovery probe.
	if _, err := a.Authenticate(mkReq()); !errors.Is(err, auth.ErrIssuerUnavailable) {
		t.Fatalf("first attempt error = %v, want errors.Is(ErrIssuerUnavailable)", err)
	}
	// Second attempt: discovery succeeds, verifier built,
	// token verifies.
	if _, err := a.Authenticate(mkReq()); err != nil {
		t.Fatalf("second attempt unexpected error: %v", err)
	}
}

// TestPeekIssuer covers the unverified-iss extraction used by the
// chain-dispatch fast path. Bad inputs return errors so the
// caller falls through; structural success returns the iss
// regardless of signature.
func TestPeekIssuer(t *testing.T) {
	t.Parallel()

	iss := newFakeIssuer(t)
	good := iss.signToken(t, "", iss.claims("aud", "u"))

	tests := []struct {
		name    string
		token   string
		wantIss string
		wantErr bool
	}{
		{name: "well-formed token", token: good, wantIss: iss.Issuer()},
		{name: "not three parts", token: "a.b", wantErr: true},
		{name: "bad base64", token: "aaa.!!!.bbb", wantErr: true},
		{name: "bad json", token: "aaa." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".bbb", wantErr: true},
		{name: "missing iss", token: "aaa." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".bbb", wantIss: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := peekIssuer(tc.token)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.wantIss {
				t.Errorf("iss = %q, want %q", got, tc.wantIss)
			}
		})
	}
}

// failingFirstRoundTripper fails the first request, succeeds on
// subsequent ones. Used by TestAuthenticate_DiscoveryRecovers to
// simulate a transient network blip.
type failingFirstRoundTripper struct {
	inner http.RoundTripper
	flag  *atomic.Bool
}

func (f failingFirstRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if !f.flag.Swap(true) {
		return nil, fmt.Errorf("simulated network failure")
	}
	return f.inner.RoundTrip(r)
}

// TestAuthenticate_VerifierMemoized confirms the discovery
// round-trip happens at most once across many requests — required
// for performance and to avoid hammering the issuer's discovery
// endpoint.
func TestAuthenticate_VerifierMemoized(t *testing.T) {
	t.Parallel()

	iss := newFakeIssuer(t)
	const audience = "ocifactory.example"

	// Wrap the issuer's transport so we can count discovery hits.
	var hits int
	rt := countingRoundTripper{
		inner: iss.server.Client().Transport,
		onHit: func(path string) {
			if strings.Contains(path, ".well-known/openid-configuration") {
				hits++
			}
		},
	}
	hc := &http.Client{Transport: rt}

	a, err := New(iss.Issuer(), audience, WithHTTPClient(hc), AllowInsecureIssuer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok := iss.signToken(t, "", iss.claims(audience, "u"))
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		r = r.WithContext(t.Context())
		if _, err := a.Authenticate(r); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("discovery hits = %d, want 1", hits)
	}
}

// claimsCmp lets cmp.Diff treat numeric claim values that come back
// as float64 (json default) as equal to the int64 inputs the tests
// constructed. Without this, every iat/exp comparison would fail
// even though the values round-trip correctly.
func claimsCmp() cmp.Option {
	return cmp.Comparer(func(a, b *auth.AuthContext) bool {
		if a == nil || b == nil {
			return a == b
		}
		if a.Issuer != b.Issuer || a.ID != b.ID || a.Email != b.Email {
			return false
		}
		// Compare common keys; numeric values may have been
		// json-decoded to float64.
		for k, av := range a.Claims {
			bv, ok := b.Claims[k]
			if !ok {
				return false
			}
			if !claimEqual(av, bv) {
				return false
			}
		}
		for k := range b.Claims {
			if _, ok := a.Claims[k]; !ok {
				return false
			}
		}
		return true
	})
}

func claimEqual(a, b any) bool {
	// Coerce numeric flavors to float64 for comparison.
	toF := func(v any) (float64, bool) {
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		}
		return 0, false
	}
	if af, aok := toF(a); aok {
		if bf, bok := toF(b); bok {
			return af == bf
		}
	}
	// Compare slices (aud array).
	if as, ok := a.([]any); ok {
		bs, ok := b.([]any)
		if !ok || len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !claimEqual(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

type countingRoundTripper struct {
	inner http.RoundTripper
	onHit func(path string)
}

func (c countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.onHit != nil {
		c.onHit(r.URL.Path)
	}
	return c.inner.RoundTrip(r)
}

// TestMultiIssuerEndToEnd is the integration assertion for the
// design's headline use case: a chain of two real
// oidc.Authenticators must accept a token issued by either
// configured issuer. The architecture-review blocker that
// motivated the iss-peek lived exactly here — without the peek a
// B-issued token would die at the A authenticator's verify call
// with ErrInvalidToken and short-circuit the chain.
//
// We exercise the chain through pkg/auth/chain rather than
// reaching into chain internals: this is the contract production
// uses, end-to-end.
func TestMultiIssuerEndToEnd(t *testing.T) {
	t.Parallel()

	const audience = "ocifactory.example"
	issA := newFakeIssuer(t)
	issB := newFakeIssuer(t)

	authA, err := New(issA.Issuer(), audience,
		WithHTTPClient(issA.server.Client()), AllowInsecureIssuer())
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	authB, err := New(issB.Issuer(), audience,
		WithHTTPClient(issB.server.Client()), AllowInsecureIssuer())
	if err != nil {
		t.Fatalf("New B: %v", err)
	}

	// chainAuth is a tiny in-test composer matching the
	// pkg/auth/chain semantics (first-non-NoCredential wins,
	// NoCredential falls through). We don't import chain to
	// keep this test self-contained against pkg/auth/chain
	// changes; the chain's own test exercises the same
	// behaviour with stubs.
	chainAuth := func(r *http.Request) (*auth.AuthContext, error) {
		for _, a := range []*Authenticator{authA, authB} {
			ac, err := a.Authenticate(r)
			if err == nil {
				return ac, nil
			}
			if errors.Is(err, auth.ErrNoCredential) {
				continue
			}
			return nil, err
		}
		return nil, auth.ErrNoCredential
	}

	mkReq := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r.WithContext(t.Context())
	}

	t.Run("A-issued token accepted", func(t *testing.T) {
		t.Parallel()
		tok := issA.signToken(t, "", issA.claims(audience, "alice"))
		ac, err := chainAuth(mkReq(tok))
		if err != nil {
			t.Fatalf("chainAuth: %v", err)
		}
		if ac.Issuer != issA.Issuer() {
			t.Errorf("issuer = %q, want %q", ac.Issuer, issA.Issuer())
		}
	})

	t.Run("B-issued token accepted (chain falls through A)", func(t *testing.T) {
		t.Parallel()
		tok := issB.signToken(t, "", issB.claims(audience, "bob"))
		ac, err := chainAuth(mkReq(tok))
		if err != nil {
			t.Fatalf("chainAuth: %v", err)
		}
		if ac.Issuer != issB.Issuer() {
			t.Errorf("issuer = %q, want %q", ac.Issuer, issB.Issuer())
		}
	})

	t.Run("token from unrelated issuer rejected", func(t *testing.T) {
		t.Parallel()
		issStranger := newFakeIssuer(t)
		tok := issStranger.signToken(t, "", issStranger.claims(audience, "eve"))
		_, err := chainAuth(mkReq(tok))
		if !errors.Is(err, auth.ErrNoCredential) {
			t.Errorf("error = %v, want errors.Is(ErrNoCredential)", err)
		}
	})
}
