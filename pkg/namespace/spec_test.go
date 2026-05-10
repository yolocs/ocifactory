package namespace

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestSpec_JSONRoundtrip pins the on-disk JSON shape so future spec
// evolution is intentional. Each test case marshals the typed spec
// and asserts byte-equality with the golden JSON, then unmarshals
// the golden back into a fresh Spec and asserts cmp.Diff equality
// with the original.
func TestSpec_JSONRoundtrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "empty",
			spec: Spec{},
			want: `{}`,
		},
		{
			name: "policy-readers-only",
			spec: Spec{
				Policy: Policy{
					Readers: []SubjectMatcher{
						{Issuer: "https://accounts.google.com", Email: "alice@example.com"},
					},
				},
			},
			want: `{"policy":{"readers":[{"issuer":"https://accounts.google.com","email":"alice@example.com"}]}}`,
		},
		{
			name: "policy-readers-and-writers",
			spec: Spec{
				Policy: Policy{
					Readers: []SubjectMatcher{
						{Issuer: "https://accounts.google.com", Email: "alice@example.com"},
					},
					Writers: []SubjectMatcher{
						{
							Issuer:   "https://token.actions.githubusercontent.com",
							SubMatch: "repo:org/repo:ref:refs/heads/main",
							Kind:     "oidc",
						},
					},
				},
			},
			want: `{"policy":{"readers":[{"issuer":"https://accounts.google.com","email":"alice@example.com"}],"writers":[{"issuer":"https://token.actions.githubusercontent.com","sub_match":"repo:org/repo:ref:refs/heads/main","kind":"oidc"}]}}`,
		},
		{
			name: "claims-match",
			spec: Spec{
				Policy: Policy{
					Writers: []SubjectMatcher{
						{
							Issuer:      "https://token.actions.githubusercontent.com",
							ClaimsMatch: map[string]string{"repository_owner": "yolocs"},
							Kind:        "oidc",
						},
					},
				},
			},
			want: `{"policy":{"writers":[{"issuer":"https://token.actions.githubusercontent.com","claims_match":{"repository_owner":"yolocs"},"kind":"oidc"}]}}`,
		},
		{
			name: "format-block-preserved",
			spec: Spec{
				Format: map[string]json.RawMessage{
					"python": json.RawMessage(`{"max_upload_bytes":104857600}`),
				},
			},
			want: `{"format":{"python":{"max_upload_bytes":104857600}}}`,
		},
		{
			name: "basictoken-kind",
			spec: Spec{
				Policy: Policy{
					Writers: []SubjectMatcher{
						{Kind: "basictoken", SubMatch: "ci-bot"},
					},
				},
			},
			want: `{"policy":{"writers":[{"sub_match":"ci-bot","kind":"basictoken"}]}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal mismatch:\n got: %s\nwant: %s", got, tc.want)
			}

			var roundtripped Spec
			if err := json.Unmarshal([]byte(tc.want), &roundtripped); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if diff := cmp.Diff(tc.spec, roundtripped); diff != "" {
				t.Errorf("Roundtrip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
