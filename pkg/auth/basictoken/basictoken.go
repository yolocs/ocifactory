// Package basictoken authenticates HTTP Basic credentials against
// a static (username, hashed token) list. It is the escape-hatch
// authenticator for environments where OIDC isn't viable —
// air-gapped builds, local development, simple scripts, and
// anything that already speaks Basic.
//
// Tokens are stored as bcrypt hashes; plaintext is never accepted.
// Use the standalone tooling (or any bcrypt utility) to generate
// hashes:
//
//	htpasswd -nbB myuser mypassword
package basictoken

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/yolocs/ocifactory/pkg/auth"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Authenticator verifies Basic auth credentials against a static
// list of (user, bcrypt-hashed-token) pairs.
//
// Sentinel usernames (the OIDC bridge: _oidc, oauth2accesstoken,
// _token) are deliberately rejected here — they belong to the OIDC
// authenticator. Putting an entry for "_oidc" in the token list is
// a configuration error.
type Authenticator struct {
	// users maps username -> bcrypt hash. Read-only after
	// construction; safe for concurrent reads without locking.
	users map[string][]byte
}

// New returns an Authenticator built from the given (user, hash)
// map. Each hash MUST be a bcrypt-encoded byte string (the output
// of bcrypt.GenerateFromPassword). Empty input is allowed and
// produces an Authenticator that rejects everything with
// ErrInvalidToken — matching the deny-everything behaviour of an
// empty allowlist.
func New(users map[string]string) (*Authenticator, error) {
	out := make(map[string][]byte, len(users))
	for user, hash := range users {
		if user == "" {
			return nil, errors.New("basictoken: user name is empty")
		}
		if auth.IsSentinelUsername(user) {
			return nil, fmt.Errorf("basictoken: %q is reserved for OIDC sentinel usage", user)
		}
		if hash == "" {
			return nil, fmt.Errorf("basictoken: empty hash for user %q", user)
		}
		// Detect plaintext-looking entries early so we don't
		// silently 401 forever. bcrypt.Cost rejects anything
		// that isn't a bcrypt hash.
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return nil, fmt.Errorf("basictoken: hash for user %q is not a bcrypt hash: %w", user, err)
		}
		out[user] = []byte(hash)
	}
	return &Authenticator{users: out}, nil
}

// Authenticate inspects r for an Authorization: Basic credential
// and verifies it against the configured token list.
//
// Returns:
//   - auth.ErrNoCredential when the request has no Basic header,
//     or the username is one of the OIDC sentinels (so the OIDC
//     authenticator can claim it).
//   - auth.ErrInvalidToken on missing user, hash mismatch, or
//     malformed input.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.Subject, error) {
	user, password, ok := auth.ExtractBasicAuth(r)
	if !ok {
		return nil, auth.ErrNoCredential
	}
	if auth.IsSentinelUsername(user) {
		// Belongs to the OIDC authenticator. Fall through so
		// chain dispatch reaches it.
		return nil, auth.ErrNoCredential
	}
	hash, found := a.users[user]
	if !found {
		// Constant-time-ish: still pay the bcrypt cost so a
		// timing oracle can't enumerate usernames cheaply.
		// CompareHashAndPassword does a structural check
		// before the slow path; this is a best-effort
		// mitigation, not a guarantee.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, fmt.Errorf("%w: unknown user", auth.ErrInvalidToken)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}
	return &auth.Subject{
		Issuer: "basictoken",
		ID:     user,
	}, nil
}

// dummyHash is a precomputed bcrypt hash used to keep the
// unknown-user path roughly as expensive as the known-user-wrong-
// password path. Generated once at init; the plaintext doesn't
// matter because no real comparison succeeds.
var dummyHash = mustHash("ocifactory-basictoken-dummy")

func mustHash(s string) []byte {
	h, err := bcrypt.GenerateFromPassword([]byte(s), bcrypt.MinCost)
	if err != nil {
		panic(fmt.Sprintf("basictoken: bcrypt of dummy failed: %v", err))
	}
	return h
}

// FileSchema is the on-disk YAML shape for a token list. Operators
// edit this file directly; ocifactory loads it via LoadFile.
type FileSchema struct {
	// Users maps username to bcrypt hash. Generate hashes with
	// htpasswd -nbB or any bcrypt CLI; never paste plaintext.
	Users map[string]string `yaml:"users"`
}

// LoadFile reads a YAML file off disk and constructs an
// Authenticator from its contents. Any structural problem (parse
// error, malformed hash, sentinel username) is surfaced rather than
// silently dropped.
func LoadFile(path string) (*Authenticator, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("basictoken: read %q: %w", path, err)
	}
	var s FileSchema
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("basictoken: parse %q: %w", path, err)
	}
	return New(s.Users)
}
