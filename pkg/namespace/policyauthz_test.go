package namespace_test

import (
	"errors"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/namespace"
)

const ghIssuer = "https://token.actions.githubusercontent.com"

func TestPolicyAuthorizer_UnknownOpReturnsErrUnknownOp(t *testing.T) {
	t.Parallel()

	a, err := namespace.NewPolicyAuthorizer(namespace.Policy{
		Readers: []namespace.SubjectMatcher{{Issuer: ghIssuer}},
		Writers: []namespace.SubjectMatcher{{Issuer: ghIssuer}},
	})
	if err != nil {
		t.Fatalf("NewPolicyAuthorizer: %v", err)
	}
	ac := &auth.AuthContext{Issuer: ghIssuer, ID: "x"}

	err = a.Authorize(t.Context(), ac, auth.Op("delete"))
	if err == nil {
		t.Fatalf("Authorize(unknown op): nil, want error wrapping ErrUnknownOp")
	}
	if !errors.Is(err, auth.ErrUnknownOp) {
		t.Errorf("Authorize(unknown op) = %v, want errors.Is(ErrUnknownOp)", err)
	}
	if errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Authorize(unknown op) = %v, must NOT wrap ErrUnauthorized", err)
	}
}

func TestNewPolicyAuthorizer_RejectsInvalidPolicy(t *testing.T) {
	t.Parallel()

	_, err := namespace.NewPolicyAuthorizer(namespace.Policy{
		Readers: []namespace.SubjectMatcher{{}},
	})
	if err == nil {
		t.Fatalf("NewPolicyAuthorizer: nil, want error wrapping ErrInvalidPolicy")
	}
	if !errors.Is(err, namespace.ErrInvalidPolicy) {
		t.Errorf("NewPolicyAuthorizer = %v, want errors.Is(ErrInvalidPolicy)", err)
	}
}

func TestPolicyAuthorizer_Authorize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		policy    namespace.Policy
		ac        *auth.AuthContext
		op        auth.Op
		wantAllow bool
	}{
		{
			name:   "empty-policy-deny-read",
			policy: namespace.Policy{},
			ac:     &auth.AuthContext{Issuer: ghIssuer, ID: "anything"},
			op:     auth.OpRead,
		},
		{
			name:   "empty-policy-deny-write",
			policy: namespace.Policy{},
			ac:     &auth.AuthContext{Issuer: ghIssuer, ID: "anything"},
			op:     auth.OpWrite,
		},
		{
			name: "nil-subject-denied",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: ghIssuer}},
			},
			ac: nil,
			op: auth.OpRead,
		},
		{
			name: "issuer-only-match",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: ghIssuer}},
			},
			ac:        &auth.AuthContext{Issuer: ghIssuer, ID: "anyone"},
			op:        auth.OpRead,
			wantAllow: true,
		},
		{
			name: "issuer-only-mismatch",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: ghIssuer}},
			},
			ac: &auth.AuthContext{Issuer: "https://accounts.google.com", ID: "anyone"},
			op: auth.OpRead,
		},
		{
			name: "email-only-match",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: "alice@example.com"}},
			},
			ac:        &auth.AuthContext{Issuer: "x", ID: "y", Email: "alice@example.com"},
			op:        auth.OpRead,
			wantAllow: true,
		},
		{
			name: "email-only-mismatch",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: "alice@example.com"}},
			},
			ac: &auth.AuthContext{Issuer: "x", ID: "y", Email: "bob@example.com"},
			op: auth.OpRead,
		},
		{
			name: "sub-match-regex",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{SubMatch: "repo:yolocs/.*:ref:refs/heads/main"}},
			},
			ac:        &auth.AuthContext{Issuer: ghIssuer, ID: "repo:yolocs/ocifactory:ref:refs/heads/main"},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "sub-match-anchored-rejects-suffix",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{SubMatch: "repo:yolocs/ocifactory"}},
			},
			ac: &auth.AuthContext{Issuer: ghIssuer, ID: "repo:yolocs/ocifactory:ref:refs/heads/main"},
			op: auth.OpWrite,
		},
		{
			name: "sub-match-anchored-rejects-prefix",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{SubMatch: "yolocs/ocifactory"}},
			},
			ac: &auth.AuthContext{Issuer: ghIssuer, ID: "evil-yolocs/ocifactory"},
			op: auth.OpWrite,
		},
		{
			name: "anded-fields-all-match",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:   ghIssuer,
					SubMatch: "repo:yolocs/.*",
					Email:    "ci@yolocs.dev",
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "repo:yolocs/ocifactory:ref:refs/heads/main",
				Email:  "ci@yolocs.dev",
			},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "anded-fields-one-mismatch",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer: ghIssuer,
					Email:  "ci@yolocs.dev",
				}},
			},
			ac: &auth.AuthContext{Issuer: ghIssuer, ID: "x", Email: "other@yolocs.dev"},
			op: auth.OpWrite,
		},
		{
			name: "ored-list-second-matches",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{
					{Issuer: ghIssuer, Email: "alice@example.com"},
					{Issuer: ghIssuer, Email: "bob@example.com"},
				},
			},
			ac:        &auth.AuthContext{Issuer: ghIssuer, ID: "x", Email: "bob@example.com"},
			op:        auth.OpRead,
			wantAllow: true,
		},
		{
			name: "ored-list-none-match",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{
					{Issuer: ghIssuer, Email: "alice@example.com"},
					{Issuer: ghIssuer, Email: "bob@example.com"},
				},
			},
			ac: &auth.AuthContext{Issuer: ghIssuer, ID: "x", Email: "carol@example.com"},
			op: auth.OpRead,
		},
		{
			name: "read-not-implied-by-write",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Issuer: ghIssuer}},
			},
			ac: &auth.AuthContext{Issuer: ghIssuer, ID: "x"},
			op: auth.OpRead,
		},
		{
			name: "claims-match-string",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"repository_owner": "yolocs"},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"repository_owner": "yolocs"},
			},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "claims-match-string-mismatch",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"repository_owner": "yolocs"},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"repository_owner": "someoneelse"},
			},
			op: auth.OpWrite,
		},
		{
			name: "claims-match-missing-claim",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"repository_owner": "yolocs"},
				}},
			},
			ac: &auth.AuthContext{Issuer: ghIssuer, ID: "x", Claims: map[string]any{}},
			op: auth.OpWrite,
		},
		{
			name: "claims-match-array-json-form",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer: ghIssuer,
					// Arrays render as JSON; the regex matches against
					// the literal JSON shape.
					ClaimsMatch: map[string]string{"groups": `\["admins","ops"\]`},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"groups": []any{"admins", "ops"}},
			},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "claims-match-object-json-form",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"meta": `\{"a":1,"b":2\}`},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"meta": map[string]any{"b": 2, "a": 1}},
			},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "default-kind-oidc-matches",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: ghIssuer, Kind: namespace.KindOIDC}},
			},
			ac:        &auth.AuthContext{Issuer: ghIssuer, ID: "x"},
			op:        auth.OpRead,
			wantAllow: true,
		},
		{
			// Numeric claims arrive as float64 from encoding/json.
			// json.Marshal renders them without trailing ".0" when
			// integral, so the regex matches the bare digits.
			name: "claims-match-numeric",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"iat": `[0-9]+`},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"iat": float64(1234567890)},
			},
			op:        auth.OpWrite,
			wantAllow: true,
		},
		{
			name: "claims-match-numeric-mismatch",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"iat": "999"},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"iat": float64(1234567890)},
			},
			op: auth.OpWrite,
		},
		{
			name: "claims-match-boolean",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"email_verified": "true"},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				Claims: map[string]any{"email_verified": true},
			},
			op:        auth.OpRead,
			wantAllow: true,
		},
		{
			name: "claims-match-null-mismatch",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{
					Issuer:      ghIssuer,
					ClaimsMatch: map[string]string{"nbf": "[0-9]+"},
				}},
			},
			ac: &auth.AuthContext{
				Issuer: ghIssuer,
				ID:     "x",
				// JSON null encodes as "null", which won't match a digits regex.
				Claims: map[string]any{"nbf": nil},
			},
			op: auth.OpRead,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, err := namespace.NewPolicyAuthorizer(tc.policy)
			if err != nil {
				t.Fatalf("NewPolicyAuthorizer: %v", err)
			}
			err = a.Authorize(t.Context(), tc.ac, tc.op)
			if tc.wantAllow {
				if err != nil {
					t.Errorf("Authorize: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Authorize: nil, want error wrapping ErrUnauthorized")
			}
			if !errors.Is(err, auth.ErrUnauthorized) {
				t.Errorf("Authorize = %v, want errors.Is(ErrUnauthorized)", err)
			}
		})
	}
}
