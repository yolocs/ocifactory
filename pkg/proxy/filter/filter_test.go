package filter_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/proxy/filter"
)

// stubFilter is a Filter that returns a fixed decision and counts
// how many times Decide has been invoked, so chain tests can pin
// ordering and short-circuit behavior.
type stubFilter struct {
	name     string
	decision filter.Decision
	err      error
	calls    int
}

func (s *stubFilter) Decide(ctx context.Context, ref filter.Ref) (filter.Decision, error) {
	s.calls++
	return s.decision, s.err
}

func (s *stubFilter) Kind() string { return s.name }

// chainPath returns the Filters.Decide tuple in a comparable shape.
type chainPath struct {
	Decision filter.Decision
	Matched  string // "" if no filter matched
}

func runChain(t *testing.T, fs filter.Filters, ref filter.Ref) (chainPath, error) {
	t.Helper()
	d, f, err := fs.Decide(t.Context(), ref)
	out := chainPath{Decision: d}
	if f != nil {
		if k, ok := f.(filter.Kinded); ok {
			out.Matched = k.Kind()
		}
	}
	return out, err
}

// TestFilters_Decide covers the chain composition semantics:
//   - Empty chain allows.
//   - All-abstain chain allows.
//   - First explicit Allow or Deny short-circuits.
//   - Abstain advances; NeedsMoreData short-circuits.
//   - An error short-circuits with the deciding filter returned.
func TestFilters_Decide(t *testing.T) {
	t.Parallel()

	// Every test case targets a file download (ref.Version set);
	// index requests are covered by TestFilters_DecideSkipsIndex.
	ref := filter.Ref{Package: "pkg", Version: "1.0.0"}

	tests := []struct {
		name      string
		chain     filter.Filters
		want      chainPath
		wantErr   bool
		wantCalls []int
	}{
		{
			name:      "empty-chain-allows",
			chain:     filter.Filters{},
			want:      chainPath{Decision: filter.DecisionAllow},
			wantCalls: nil,
		},
		{
			name: "all-abstain-allows",
			chain: filter.Filters{
				&stubFilter{name: "a", decision: filter.DecisionAbstain},
				&stubFilter{name: "b", decision: filter.DecisionAbstain},
				&stubFilter{name: "c", decision: filter.DecisionAbstain},
			},
			want:      chainPath{Decision: filter.DecisionAllow},
			wantCalls: []int{1, 1, 1},
		},
		{
			name: "first-allow-short-circuits",
			chain: filter.Filters{
				&stubFilter{name: "a", decision: filter.DecisionAbstain},
				&stubFilter{name: "b", decision: filter.DecisionAllow},
				&stubFilter{name: "c", decision: filter.DecisionDeny},
			},
			want:      chainPath{Decision: filter.DecisionAllow, Matched: "b"},
			wantCalls: []int{1, 1, 0},
		},
		{
			name: "first-deny-short-circuits",
			chain: filter.Filters{
				&stubFilter{name: "a", decision: filter.DecisionAbstain},
				&stubFilter{name: "b", decision: filter.DecisionDeny},
				&stubFilter{name: "c", decision: filter.DecisionAllow},
			},
			want:      chainPath{Decision: filter.DecisionDeny, Matched: "b"},
			wantCalls: []int{1, 1, 0},
		},
		{
			name: "abstain-then-needs-more-data",
			chain: filter.Filters{
				&stubFilter{name: "a", decision: filter.DecisionAbstain},
				&stubFilter{name: "b", decision: filter.DecisionNeedsMoreData},
				&stubFilter{name: "c", decision: filter.DecisionAllow},
			},
			want:      chainPath{Decision: filter.DecisionNeedsMoreData, Matched: "b"},
			wantCalls: []int{1, 1, 0},
		},
		{
			name: "error-short-circuits",
			chain: filter.Filters{
				&stubFilter{name: "a", decision: filter.DecisionAbstain},
				&stubFilter{name: "b", decision: filter.DecisionDeny, err: errors.New("boom")},
				&stubFilter{name: "c", decision: filter.DecisionAllow},
			},
			want:      chainPath{Decision: filter.DecisionDeny, Matched: "b"},
			wantErr:   true,
			wantCalls: []int{1, 1, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := runChain(t, tc.chain, ref)
			if (err != nil) != tc.wantErr {
				t.Errorf("Decide() err = %v, wantErr=%v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Filters.Decide mismatch (-want +got):\n%s", diff)
			}
			for i, want := range tc.wantCalls {
				got := tc.chain[i].(*stubFilter).calls
				if got != want {
					t.Errorf("filter[%d] calls = %d, want %d", i, got, want)
				}
			}
		})
	}
}

// TestFilters_DecideSkipsIndex pins the load-bearing property that
// filtering applies to file downloads only. An index-style request
// (empty Ref.Version) returns DecisionAllow without invoking any
// filter, even one that would deny by package name.
func TestFilters_DecideSkipsIndex(t *testing.T) {
	t.Parallel()

	denyAll := &stubFilter{name: "deny-everything", decision: filter.DecisionDeny}
	fs := filter.Filters{denyAll}

	d, f, err := fs.Decide(t.Context(), filter.Ref{Package: "evil"})
	if err != nil {
		t.Fatalf("Decide err: %v", err)
	}
	if d != filter.DecisionAllow {
		t.Errorf("Decision = %v, want Allow", d)
	}
	if f != nil {
		t.Errorf("Filter = %T, want nil", f)
	}
	if denyAll.calls != 0 {
		t.Errorf("filter calls = %d, want 0 (chain bypassed for index)", denyAll.calls)
	}
}

// TestFilters_AllowlistOverridesDelay pins the headline policy
// example: an allowlisted package bypasses delay even when the
// version is younger than MinAge.
func TestFilters_AllowlistOverridesDelay(t *testing.T) {
	t.Parallel()

	frozen := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	fs := filter.Filters{
		&filter.Allowlist{Patterns: []string{"requests"}},
		&filter.Delay{MinAge: 24 * time.Hour},
	}
	ctx := filter.WithClock(t.Context(), func() time.Time { return frozen })
	ref := filter.Ref{Package: "requests", Version: "2.31.0", UploadTime: frozen.Add(-time.Hour)}

	d, f, err := fs.Decide(ctx, ref)
	if err != nil {
		t.Fatalf("Decide err: %v", err)
	}
	if d != filter.DecisionAllow {
		t.Errorf("Decision = %v, want Allow", d)
	}
	if k, _ := f.(filter.Kinded); k == nil || k.Kind() != filter.KindAllowlist {
		t.Errorf("deciding filter = %T, want *Allowlist", f)
	}
}

// TestFilters_DelayHandlesAbstainedRef pins the typical chain:
// allow-listed names bypass, deny-listed names short-circuit, and
// everything else falls through to delay.
func TestFilters_DelayHandlesAbstainedRef(t *testing.T) {
	t.Parallel()

	frozen := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	fs := filter.Filters{
		&filter.Allowlist{Patterns: []string{"@myorg/*"}},
		&filter.Denylist{Patterns: []string{"evil-*"}},
		&filter.Delay{MinAge: 24 * time.Hour},
	}
	ctx := filter.WithClock(t.Context(), func() time.Time { return frozen })

	tests := []struct {
		name     string
		ref      filter.Ref
		wantDec  filter.Decision
		wantKind string
		wantErr  bool
	}{
		{
			name:     "allowlisted-bypasses-delay",
			ref:      filter.Ref{Package: "@myorg/sdk", Version: "1.0.0", UploadTime: frozen.Add(-time.Hour)},
			wantDec:  filter.DecisionAllow,
			wantKind: filter.KindAllowlist,
		},
		{
			name:     "denylisted-short-circuits",
			ref:      filter.Ref{Package: "evil-pkg", Version: "1.0.0", UploadTime: frozen.Add(-30 * 24 * time.Hour)},
			wantDec:  filter.DecisionDeny,
			wantKind: filter.KindDenylist,
		},
		{
			name:     "fresh-version-denied-by-delay",
			ref:      filter.Ref{Package: "neutral", Version: "1.0.0", UploadTime: frozen.Add(-time.Hour)},
			wantDec:  filter.DecisionDeny,
			wantKind: filter.KindDelay,
		},
		{
			name:     "aged-version-allowed-by-delay",
			ref:      filter.Ref{Package: "neutral", Version: "1.0.0", UploadTime: frozen.Add(-30 * 24 * time.Hour)},
			wantDec:  filter.DecisionAllow,
			wantKind: filter.KindDelay,
		},
		{
			name:     "missing-upload-time-needs-more-data",
			ref:      filter.Ref{Package: "neutral", Version: "1.0.0"},
			wantDec:  filter.DecisionNeedsMoreData,
			wantKind: filter.KindDelay,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, f, err := fs.Decide(ctx, tc.ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Decide err = %v, wantErr=%v", err, tc.wantErr)
			}
			if d != tc.wantDec {
				t.Errorf("Decision = %v, want %v", d, tc.wantDec)
			}
			if k, _ := f.(filter.Kinded); k == nil || k.Kind() != tc.wantKind {
				t.Errorf("deciding filter = %T, want kind %q", f, tc.wantKind)
			}
		})
	}
}

// TestDecision_String pins the Stringer output used in logs / test
// messages. Unknown values are surfaced rather than silently
// formatted, so a future contributor adding a Decision constant
// can't forget to add the case.
func TestDecision_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   filter.Decision
		want string
	}{
		{filter.DecisionAllow, "allow"},
		{filter.DecisionDeny, "deny"},
		{filter.DecisionAbstain, "abstain"},
		{filter.DecisionNeedsMoreData, "needs-more-data"},
		{filter.Decision(99), "decision(99)"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.String(); got != tc.want {
				t.Errorf("Decision(%d).String() = %q, want %q", int(tc.in), got, tc.want)
			}
		})
	}
}

// TestFilters_JSONRoundtrip pins both the byte-stable marshaled form
// and the unmarshal-back-to-typed-values dispatch. The wire shape is
// load-bearing for the namespace spec on-disk format, so a refactor
// can't silently rename `kind`, `patterns`, or `min_age`.
func TestFilters_JSONRoundtrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   filter.Filters
		want string
	}{
		{
			name: "nil",
			in:   nil,
			want: `null`,
		},
		{
			name: "empty",
			in:   filter.Filters{},
			want: `[]`,
		},
		{
			name: "allowlist-patterns",
			in:   filter.Filters{&filter.Allowlist{Patterns: []string{"foo", "bar*"}}},
			want: `[{"kind":"allow","patterns":["foo","bar*"]}]`,
		},
		{
			name: "allowlist-rules",
			in:   filter.Filters{&filter.Allowlist{Rules: []filter.Rule{{Package: "requests", Version: "2.31.*"}}}},
			want: `[{"kind":"allow","rules":[{"package":"requests","version":"2.31.*"}]}]`,
		},
		{
			name: "allowlist-mixed",
			in: filter.Filters{&filter.Allowlist{
				Patterns: []string{"safe"},
				Rules:    []filter.Rule{{Package: "requests", Version: "2.31.*"}},
			}},
			want: `[{"kind":"allow","patterns":["safe"],"rules":[{"package":"requests","version":"2.31.*"}]}]`,
		},
		{
			name: "denylist-patterns",
			in:   filter.Filters{&filter.Denylist{Patterns: []string{"evil"}}},
			want: `[{"kind":"deny","patterns":["evil"]}]`,
		},
		{
			name: "denylist-rules-version-pin",
			in:   filter.Filters{&filter.Denylist{Rules: []filter.Rule{{Package: "log4j-core", Version: "2.14.*"}}}},
			want: `[{"kind":"deny","rules":[{"package":"log4j-core","version":"2.14.*"}]}]`,
		},
		{
			name: "delay-hours",
			in:   filter.Filters{&filter.Delay{MinAge: 24 * time.Hour}},
			want: `[{"kind":"delay","min_age":"24h0m0s"}]`,
		},
		{
			name: "delay-compound",
			in:   filter.Filters{&filter.Delay{MinAge: time.Hour + 30*time.Minute}},
			want: `[{"kind":"delay","min_age":"1h30m0s"}]`,
		},
		{
			name: "mixed-chain",
			in: filter.Filters{
				&filter.Allowlist{Patterns: []string{"@myorg/*"}},
				&filter.Denylist{Rules: []filter.Rule{{Package: "log4j-core", Version: "2.14.*"}}},
				&filter.Delay{MinAge: 24 * time.Hour},
			},
			want: `[{"kind":"allow","patterns":["@myorg/*"]},{"kind":"deny","rules":[{"package":"log4j-core","version":"2.14.*"}]},{"kind":"delay","min_age":"24h0m0s"}]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal:\n got: %s\nwant: %s", got, tc.want)
			}
			var rt filter.Filters
			if err := json.Unmarshal([]byte(tc.want), &rt); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if diff := cmp.Diff(tc.in, rt); diff != "" {
				t.Errorf("Roundtrip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFilters_UnmarshalErrors covers the rejection paths: malformed
// JSON, missing kind, unknown kind, and invalid filter bodies.
func TestFilters_UnmarshalErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{
			name:    "not-an-array",
			body:    `{"kind":"allow"}`,
			wantErr: nil, // json error, not ErrInvalidFilter
		},
		{
			name:    "missing-kind",
			body:    `[{"patterns":["foo"]}]`,
			wantErr: filter.ErrInvalidFilter,
		},
		{
			name:    "unknown-kind",
			body:    `[{"kind":"nope"}]`,
			wantErr: filter.ErrInvalidFilter,
		},
		{
			name:    "invalid-body",
			body:    `[{"kind":"allow","patterns":[]}]`,
			wantErr: filter.ErrInvalidFilter,
		},
		{
			name:    "invalid-rule-empty",
			body:    `[{"kind":"deny","rules":[{}]}]`,
			wantErr: filter.ErrInvalidFilter,
		},
		{
			name:    "malformed",
			body:    `[`,
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fs filter.Filters
			err := json.Unmarshal([]byte(tc.body), &fs)
			if err == nil {
				t.Fatalf("Unmarshal(%q) = nil, want error", tc.body)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Unmarshal(%q) error = %v, want errors.Is %v", tc.body, err, tc.wantErr)
			}
		})
	}
}

// TestFilters_Validate covers the namespace-spec validation hook:
// any filter that exposes validation reports its errors with a
// positional prefix. Built-in filters already validate at unmarshal
// time, but the spec layer still calls Validate to defend against
// values constructed in Go code (programmatic admin tools, tests)
// rather than through JSON.
func TestFilters_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fs      filter.Filters
		wantErr bool
	}{
		{
			name: "all-valid",
			fs: filter.Filters{
				&filter.Allowlist{Patterns: []string{"foo"}},
				&filter.Delay{MinAge: time.Hour},
			},
		},
		{
			name:    "invalid-allowlist",
			fs:      filter.Filters{&filter.Allowlist{}},
			wantErr: true,
		},
		{
			name: "invalid-delay-after-valid-allowlist",
			fs: filter.Filters{
				&filter.Allowlist{Patterns: []string{"foo"}},
				&filter.Delay{MinAge: 0},
			},
			wantErr: true,
		},
		{
			name: "invalid-empty-rule",
			fs: filter.Filters{
				&filter.Denylist{Rules: []filter.Rule{{}}},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.fs.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
