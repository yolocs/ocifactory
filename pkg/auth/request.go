package auth

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// SentinelUsernames is the set of basic-auth usernames that
// indicate the password field carries an OIDC token rather than a
// real password. _oidc is canonical; oauth2accesstoken matches GAR
// tooling; _token matches npm registry conventions.
var SentinelUsernames = []string{"_oidc", "oauth2accesstoken", "_token"}

// IsSentinelUsername reports whether u is one of the well-known
// sentinel usernames that indicates the basic-auth password is an
// OIDC bearer token.
func IsSentinelUsername(u string) bool {
	for _, s := range SentinelUsernames {
		if u == s {
			return true
		}
	}
	return false
}

// ExtractBearerToken returns the token from a Bearer Authorization
// header, or ("", false) if the header is missing or doesn't use
// the Bearer scheme.
func ExtractBearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	// Case-insensitive scheme match per RFC 7235; values are
	// case-sensitive.
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// ExtractBasicAuth returns the decoded username and password from
// a Basic Authorization header. Returns ok=false when the header is
// missing, malformed, or uses a different scheme. Mirrors
// (*http.Request).BasicAuth but reads from the header directly so
// tests can construct requests without a server.
func ExtractBasicAuth(r *http.Request) (user, password string, ok bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", "", false
	}
	const prefix = "Basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	idx := strings.IndexByte(string(raw), ':')
	if idx < 0 {
		return "", "", false
	}
	return string(raw[:idx]), string(raw[idx+1:]), true
}

// ExtractOIDCToken returns the bearer token presented either via
// Authorization: Bearer ... or Authorization: Basic
// base64("<sentinel>:<token>"). Returns ok=false when the request
// carries no credential of either form.
//
// The two-form support exists because not every package manager
// speaks Bearer: pip's ~/.netrc, mvn's settings.xml, and curl-based
// shell scripts all default to Basic. The sentinel-username
// convention (popularised by GAR's oauth2accesstoken) lets those
// tools forward an OIDC token without protocol changes on their end.
func ExtractOIDCToken(r *http.Request) (string, bool) {
	if tok, ok := ExtractBearerToken(r); ok {
		return tok, true
	}
	user, pwd, ok := ExtractBasicAuth(r)
	if !ok {
		return "", false
	}
	if !IsSentinelUsername(user) {
		return "", false
	}
	if pwd == "" {
		return "", false
	}
	return pwd, true
}
