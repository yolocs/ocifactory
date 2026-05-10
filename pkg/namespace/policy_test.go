package namespace_test

import (
	"errors"
	"testing"

	"github.com/yolocs/ocifactory/pkg/namespace"
)

func TestPolicy_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  namespace.Policy
		wantErr bool
	}{
		{
			name:   "empty-policy-is-valid",
			policy: namespace.Policy{},
		},
		{
			name: "issuer-only",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: "https://accounts.google.com"}},
			},
		},
		{
			name: "email-only",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Email: "alice@example.com"}},
			},
		},
		{
			name: "sub-match-only",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{SubMatch: "repo:org/.*"}},
			},
		},
		{
			name: "claims-match-only",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{ClaimsMatch: map[string]string{"repository_owner": "yolocs"}}},
			},
		},
		{
			name: "kind-only-oidc",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Kind: namespace.KindOIDC}},
			},
		},
		{
			name: "all-fields",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{
					Issuer:      "https://token.actions.githubusercontent.com",
					SubMatch:    "repo:yolocs/.*",
					Email:       "ci@yolocs.dev",
					ClaimsMatch: map[string]string{"ref": "refs/heads/main"},
					Kind:        namespace.KindOIDC,
				}},
			},
		},
		{
			name: "empty-matcher-rejected",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{}},
			},
			wantErr: true,
		},
		{
			name: "kind-basictoken-rejected",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Kind: namespace.KindBasicToken, SubMatch: "ci-bot"}},
			},
			wantErr: true,
		},
		{
			name: "kind-unknown-rejected",
			policy: namespace.Policy{
				Writers: []namespace.SubjectMatcher{{Kind: "saml", Issuer: "x"}},
			},
			wantErr: true,
		},
		{
			name: "sub-match-bad-regex",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{SubMatch: "(unclosed"}},
			},
			wantErr: true,
		},
		{
			name: "claims-match-bad-regex",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{ClaimsMatch: map[string]string{"repo": "(bad"}}},
			},
			wantErr: true,
		},
		{
			name: "claims-match-empty-key",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{ClaimsMatch: map[string]string{"": "x"}}},
			},
			wantErr: true,
		},
		{
			name: "writers-list-error-reported",
			policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{{Issuer: "ok"}},
				Writers: []namespace.SubjectMatcher{{}},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.policy.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error wrapping ErrInvalidPolicy")
				}
				if !errors.Is(err, namespace.ErrInvalidPolicy) {
					t.Errorf("Validate() = %v, want errors.Is(ErrInvalidPolicy)", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestPolicy_Validate_NilReceiver(t *testing.T) {
	t.Parallel()

	var p *namespace.Policy
	if err := p.Validate(); err != nil {
		t.Errorf("(*Policy)(nil).Validate() = %v, want nil", err)
	}
}
