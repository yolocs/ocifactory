package namespace

import (
	"encoding/json"
	"errors"
	"strings"
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
		{
			name: "schema-version-current",
			spec: Spec{
				SchemaVersion: CurrentSchemaVersion,
				Policy: Policy{
					Readers: []SubjectMatcher{{Email: "alice@example.com"}},
				},
			},
			want: `{"schema_version":1,"policy":{"readers":[{"email":"alice@example.com"}]}}`,
		},
		{
			// "Empty" above already covers the hosted-by-default case
			// (Mode == "" resolves to hosted on Validate); this case
			// pins the explicit form so a client that opts into
			// "mode":"hosted" gets the same on-disk shape on read.
			name: "hosted-mode-explicit",
			spec: Spec{Mode: ModeHosted},
			want: `{"mode":"hosted"}`,
		},
		{
			name: "proxy-mode-upstream-only",
			spec: Spec{
				Mode:  ModeProxy,
				Proxy: Proxy{Upstream: "https://pypi.org"},
			},
			want: `{"mode":"proxy","proxy":{"upstream":"https://pypi.org"}}`,
		},
		{
			name: "proxy-mode-full-block",
			spec: Spec{
				Mode: ModeProxy,
				Proxy: Proxy{
					Upstream: "https://registry.npmjs.org",
					Filters: []Filter{
						json.RawMessage(`{"type":"allowlist","patterns":["@myorg/*"]}`),
						json.RawMessage(`{"type":"delay","min_age":"24h"}`),
					},
				},
			},
			want: `{"mode":"proxy","proxy":{"upstream":"https://registry.npmjs.org","filters":[{"type":"allowlist","patterns":["@myorg/*"]},{"type":"delay","min_age":"24h"}]}}`,
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

// TestSpec_Validate_SchemaVersion covers the three states the field
// can be in on disk: legacy (zero), current, and future-unknown.
func TestSpec_Validate_SchemaVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    Spec
		wantErr error
	}{
		{
			name: "zero-treated-as-current",
			spec: Spec{},
		},
		{
			name: "explicit-current",
			spec: Spec{SchemaVersion: CurrentSchemaVersion},
		},
		{
			name:    "future-unknown",
			spec:    Spec{SchemaVersion: CurrentSchemaVersion + 1},
			wantErr: ErrUnsupportedSchemaVersion,
		},
		{
			name:    "future-way-out",
			spec:    Spec{SchemaVersion: 999},
			wantErr: ErrUnsupportedSchemaVersion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.spec.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() error %v, want errors.Is %v", err, tc.wantErr)
			}
			// The operator-facing message must name the highest version
			// this binary understands so the upgrade path is obvious.
			if msg := err.Error(); !strings.Contains(msg, "understands up to") {
				t.Errorf("Validate() error %q missing supported-max hint", msg)
			}
		})
	}
}

// TestSpec_Validate_ModeAndProxy covers the Mode/Proxy validation
// rules: hosted (empty + explicit) accepts no proxy block; proxy
// requires an absolute http(s) upstream URL.
func TestSpec_Validate_ModeAndProxy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    Spec
		wantErr error
	}{
		{
			name: "empty-mode-no-proxy-ok",
			spec: Spec{},
		},
		{
			name: "explicit-hosted-no-proxy-ok",
			spec: Spec{Mode: ModeHosted},
		},
		{
			name: "proxy-with-upstream-ok",
			spec: Spec{
				Mode:  ModeProxy,
				Proxy: Proxy{Upstream: "https://pypi.org"},
			},
		},
		{
			name: "proxy-with-http-upstream-ok",
			spec: Spec{
				Mode:  ModeProxy,
				Proxy: Proxy{Upstream: "http://internal-mirror.example.test"},
			},
		},
		{
			name:    "unknown-mode",
			spec:    Spec{Mode: "virtual"},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "proxy-without-upstream",
			spec:    Spec{Mode: ModeProxy},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "proxy-with-empty-upstream-and-filters",
			spec: Spec{
				Mode: ModeProxy,
				Proxy: Proxy{Filters: []Filter{
					[]byte(`{"type":"allowlist"}`),
				}},
			},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "hosted-rejects-proxy-block",
			spec: Spec{
				Mode:  ModeHosted,
				Proxy: Proxy{Upstream: "https://pypi.org"},
			},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "hosted-default-rejects-proxy-block",
			spec: Spec{
				Proxy: Proxy{Upstream: "https://pypi.org"},
			},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "hosted-rejects-proxy-filters",
			spec: Spec{
				Proxy: Proxy{Filters: []Filter{[]byte(`{}`)}},
			},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "proxy-upstream-must-be-absolute",
			spec: Spec{
				Mode:  ModeProxy,
				Proxy: Proxy{Upstream: "/pypi.org"},
			},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "proxy-upstream-rejects-non-http-scheme",
			spec: Spec{
				Mode:  ModeProxy,
				Proxy: Proxy{Upstream: "ftp://pypi.org"},
			},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "proxy-upstream-rejects-unparseable",
			spec: Spec{
				Mode:  ModeProxy,
				Proxy: Proxy{Upstream: "http://[::1"},
			},
			wantErr: ErrInvalidProxy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.spec.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() error %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

// TestSpec_Normalize_ModeCanonical pins the on-disk-shape
// canonicalisation: explicit Mode "hosted" collapses to empty so the
// persisted body for a hosted namespace stays compact (just
// {"schema_version":1} in the common case), while "proxy" survives.
func TestSpec_Normalize_ModeCanonical(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Spec
		want Spec
	}{
		{
			name: "explicit-hosted-collapses",
			in:   Spec{Mode: ModeHosted},
			want: Spec{SchemaVersion: CurrentSchemaVersion},
		},
		{
			name: "proxy-mode-preserved",
			in:   Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://pypi.org"}},
			want: Spec{SchemaVersion: CurrentSchemaVersion, Mode: ModeProxy, Proxy: Proxy{Upstream: "https://pypi.org"}},
		},
		{
			name: "empty-mode-stays-empty",
			in:   Spec{},
			want: Spec{SchemaVersion: CurrentSchemaVersion},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.in
			got.Normalize()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Normalize() mismatch (-want +got):\n%s", diff)
			}
			// Idempotent.
			got.Normalize()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Normalize() second pass mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSpec_Validate_NilReceiver guards the documented nil-safe path
// used by callers that don't always allocate a spec.
func TestSpec_Validate_NilReceiver(t *testing.T) {
	t.Parallel()

	var s *Spec
	if err := s.Validate(); err != nil {
		t.Errorf("(*Spec)(nil).Validate() = %v, want nil", err)
	}
}

// TestSpec_Normalize asserts Normalize stamps CurrentSchemaVersion
// idempotently and leaves other fields untouched. The legacy-promotion
// case is the load-bearing one: bodies written before the field
// existed must come out of the write path stamped with v1.
func TestSpec_Normalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Spec
		want Spec
	}{
		{
			name: "legacy-promoted",
			in:   Spec{Policy: Policy{Readers: []SubjectMatcher{{Email: "alice@example.com"}}}},
			want: Spec{SchemaVersion: CurrentSchemaVersion, Policy: Policy{Readers: []SubjectMatcher{{Email: "alice@example.com"}}}},
		},
		{
			name: "already-current",
			in:   Spec{SchemaVersion: CurrentSchemaVersion},
			want: Spec{SchemaVersion: CurrentSchemaVersion},
		},
		{
			name: "older-value-overwritten",
			in:   Spec{SchemaVersion: 0},
			want: Spec{SchemaVersion: CurrentSchemaVersion},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.in
			got.Normalize()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Normalize() mismatch (-want +got):\n%s", diff)
			}
			// Idempotent.
			got.Normalize()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Normalize() second pass mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSpec_Normalize_NilReceiver guards the documented nil-safe path.
func TestSpec_Normalize_NilReceiver(t *testing.T) {
	t.Parallel()

	var s *Spec
	s.Normalize()
}
