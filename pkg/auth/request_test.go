package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractBearerToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{name: "missing", header: "", wantOK: false},
		{name: "bearer", header: "Bearer abc", wantToken: "abc", wantOK: true},
		{name: "bearer mixed case", header: "bearer xyz", wantToken: "xyz", wantOK: true},
		{name: "bearer trailing whitespace", header: "Bearer   abc  ", wantToken: "abc", wantOK: true},
		{name: "bearer empty token", header: "Bearer ", wantOK: false},
		{name: "basic", header: "Basic dXNlcjpwd2Q=", wantOK: false},
		{name: "garbage", header: "Foo bar", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			tok, ok := ExtractBearerToken(r)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tok != tc.wantToken {
				t.Errorf("token = %q, want %q", tok, tc.wantToken)
			}
		})
	}
}

func TestExtractBasicAuth(t *testing.T) {
	t.Parallel()

	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	tests := []struct {
		name     string
		header   string
		wantUser string
		wantPwd  string
		wantOK   bool
	}{
		{name: "standard", header: "Basic " + encode("alice:p1"), wantUser: "alice", wantPwd: "p1", wantOK: true},
		{name: "empty password ok", header: "Basic " + encode("alice:"), wantUser: "alice", wantPwd: "", wantOK: true},
		{name: "no colon", header: "Basic " + encode("aliceonly"), wantOK: false},
		{name: "bad base64", header: "Basic !!!notbase64!!!", wantOK: false},
		{name: "bearer", header: "Bearer xyz", wantOK: false},
		{name: "missing", header: "", wantOK: false},
		{name: "case-insensitive scheme", header: "basic " + encode("alice:p"), wantUser: "alice", wantPwd: "p", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			u, p, ok := ExtractBasicAuth(r)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if u != tc.wantUser {
				t.Errorf("user = %q, want %q", u, tc.wantUser)
			}
			if p != tc.wantPwd {
				t.Errorf("pwd = %q, want %q", p, tc.wantPwd)
			}
		})
	}
}

func TestExtractOIDCToken(t *testing.T) {
	t.Parallel()

	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	tests := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{name: "bearer", header: "Bearer jwt", wantToken: "jwt", wantOK: true},
		{name: "sentinel _oidc", header: "Basic " + encode("_oidc:tok"), wantToken: "tok", wantOK: true},
		{name: "sentinel oauth2accesstoken", header: "Basic " + encode("oauth2accesstoken:tok"), wantToken: "tok", wantOK: true},
		{name: "sentinel _token", header: "Basic " + encode("_token:tok"), wantToken: "tok", wantOK: true},
		{name: "non-sentinel basic", header: "Basic " + encode("alice:pwd"), wantOK: false},
		{name: "sentinel empty token", header: "Basic " + encode("_oidc:"), wantOK: false},
		{name: "missing", header: "", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			tok, ok := ExtractOIDCToken(r)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tok != tc.wantToken {
				t.Errorf("token = %q, want %q", tok, tc.wantToken)
			}
		})
	}
}

func TestIsSentinelUsername(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user string
		want bool
	}{
		{name: "_oidc", user: "_oidc", want: true},
		{name: "oauth2accesstoken", user: "oauth2accesstoken", want: true},
		{name: "_token", user: "_token", want: true},
		{name: "alice", user: "alice", want: false},
		{name: "empty", user: "", want: false},
		{name: "case sensitive", user: "_OIDC", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSentinelUsername(tc.user); got != tc.want {
				t.Errorf("IsSentinelUsername(%q) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}
