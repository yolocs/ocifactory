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
// how many times Allow has been invoked, so chain tests can pin
// ordering and short-circuit behavior.
type stubFilter struct {
	name     string
	decision filter.Decision
	err      error
	calls    int
}

func (s *stubFilter) Allow(ctx context.Context, ref filter.Ref) (filter.Decision, error) {
	s.calls++
	return s.decision, s.err
}

func (s *stubFilter) Kind() string { return s.name }

// chainPath returns the chain.Allow tuple in a comparable shape.
type chainPath struct {
	Decision filter.Decision
	Matched  string // "" if no filter matched
}

func runChain(t *testing.T, c filter.Chain, ref filter.Ref) (chainPath, error) {
	t.Helper()
	d, f, err := c.Allow(t.Context(), ref)
	out := chainPath{Decision: d}
	if f != nil {
		if k, ok := f.(filter.Kinded); ok {
			out.Matched = k.Kind()
		}
	}
	return out, err
}

// TestChain_Allow covers the core composition semantics: an empty
// chain allows; a chain of allows allows; the first deny wins and
// short-circuits; NeedsMoreData is "remembered" without
// short-circuiting; and an error short-circuits with the filter
// returned.
func TestChain_Allow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chain     filter.Chain
		want      chainPath
		wantErr   bool
		wantCalls []int // expected call count per filter, by index
	}{
		{
			name:      "empty-chain-allows",
			chain:     filter.Chain{},
			want:      chainPath{Decision: filter.DecisionAllow},
			wantCalls: nil,
		},
		{
			name: "all-allow",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionAllow},
				&stubFilter{name: "b", decision: filter.DecisionAllow},
				&stubFilter{name: "c", decision: filter.DecisionAllow},
			},
			want:      chainPath{Decision: filter.DecisionAllow},
			wantCalls: []int{1, 1, 1},
		},
		{
			name: "first-deny-wins",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionAllow},
				&stubFilter{name: "b", decision: filter.DecisionDeny},
				&stubFilter{name: "c", decision: filter.DecisionAllow},
			},
			want:      chainPath{Decision: filter.DecisionDeny, Matched: "b"},
			wantCalls: []int{1, 1, 0},
		},
		{
			// First deny is the one returned, even when a later
			// filter would also deny. Pinning this so observability
			// labels stay stable (one deny per request).
			name: "two-denies-first-wins",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionDeny},
				&stubFilter{name: "b", decision: filter.DecisionDeny},
			},
			want:      chainPath{Decision: filter.DecisionDeny, Matched: "a"},
			wantCalls: []int{1, 0},
		},
		{
			name: "needs-more-data-does-not-short-circuit",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionAllow},
				&stubFilter{name: "b", decision: filter.DecisionNeedsMoreData},
				&stubFilter{name: "c", decision: filter.DecisionAllow},
			},
			want:      chainPath{Decision: filter.DecisionNeedsMoreData, Matched: "b"},
			wantCalls: []int{1, 1, 1},
		},
		{
			// Deny later in the chain takes precedence over an earlier
			// NeedsMoreData. The chain caller's contract is: if Deny,
			// reject; if NeedsMoreData, fetch metadata and re-run; if
			// Allow, proceed. A deny-after-needs-more-data must not
			// leak through as NeedsMoreData.
			name: "deny-trumps-needs-more-data",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionNeedsMoreData},
				&stubFilter{name: "b", decision: filter.DecisionDeny},
			},
			want:      chainPath{Decision: filter.DecisionDeny, Matched: "b"},
			wantCalls: []int{1, 1},
		},
		{
			name: "first-needs-more-data-is-returned",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionAllow},
				&stubFilter{name: "b", decision: filter.DecisionNeedsMoreData},
				&stubFilter{name: "c", decision: filter.DecisionNeedsMoreData},
			},
			want:      chainPath{Decision: filter.DecisionNeedsMoreData, Matched: "b"},
			wantCalls: []int{1, 1, 1},
		},
		{
			name: "error-short-circuits",
			chain: filter.Chain{
				&stubFilter{name: "a", decision: filter.DecisionAllow},
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
			got, err := runChain(t, tc.chain, filter.Ref{Package: "pkg"})
			if (err != nil) != tc.wantErr {
				t.Errorf("Allow() err = %v, wantErr=%v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Chain.Allow mismatch (-want +got):\n%s", diff)
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

// TestChain_NeedsMoreDataReRun pins the documented re-run semantics:
// callers re-invoke the chain after enriching the ref, and a filter
// that previously returned NeedsMoreData now sees enough data to
// decide.
func TestChain_NeedsMoreDataReRun(t *testing.T) {
	t.Parallel()

	frozen := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	chain := filter.Chain{
		&filter.Allowlist{Patterns: []string{"requests"}},
		&filter.Delay{MinAge: 24 * time.Hour},
	}
	ctx := filter.WithClock(t.Context(), func() time.Time { return frozen })

	// Pass 1: no upload time yet.
	ref := filter.Ref{Package: "requests", Version: "2.31.0"}
	d, f, err := chain.Allow(ctx, ref)
	if err != nil {
		t.Fatalf("pass 1 err: %v", err)
	}
	if d != filter.DecisionNeedsMoreData {
		t.Fatalf("pass 1 decision = %v, want NeedsMoreData", d)
	}
	if k, _ := f.(filter.Kinded); k == nil || k.Kind() != filter.KindDelay {
		t.Errorf("pass 1 pending filter = %T, want *Delay", f)
	}

	// Pass 2: upload time fresh — still under threshold, expect deny.
	ref.UploadTime = frozen.Add(-1 * time.Hour)
	d, f, err = chain.Allow(ctx, ref)
	if err != nil {
		t.Fatalf("pass 2 err: %v", err)
	}
	if d != filter.DecisionDeny {
		t.Fatalf("pass 2 decision = %v, want Deny", d)
	}
	if k, _ := f.(filter.Kinded); k == nil || k.Kind() != filter.KindDelay {
		t.Errorf("pass 2 deny filter = %T, want *Delay", f)
	}

	// Pass 3: upload time aged out — expect allow.
	ref.UploadTime = frozen.Add(-30 * 24 * time.Hour)
	d, f, err = chain.Allow(ctx, ref)
	if err != nil {
		t.Fatalf("pass 3 err: %v", err)
	}
	if d != filter.DecisionAllow {
		t.Fatalf("pass 3 decision = %v, want Allow", d)
	}
	if f != nil {
		t.Errorf("pass 3 matched filter = %T, want nil", f)
	}
}

// TestChain_DenylistShortCircuitsBeforeDelay pins that name-only
// filters can deny before any metadata-dependent filter would have
// asked for more data — the load-bearing property that lets a proxy
// reject by name without ever calling upstream.
func TestChain_DenylistShortCircuitsBeforeDelay(t *testing.T) {
	t.Parallel()

	delay := &filter.Delay{MinAge: 24 * time.Hour}
	chain := filter.Chain{
		&filter.Denylist{Patterns: []string{"evil"}},
		delay,
	}

	d, f, err := chain.Allow(t.Context(), filter.Ref{Package: "evil"})
	if err != nil {
		t.Fatalf("Allow err: %v", err)
	}
	if d != filter.DecisionDeny {
		t.Errorf("Decision = %v, want Deny", d)
	}
	if k, _ := f.(filter.Kinded); k == nil || k.Kind() != filter.KindDenylist {
		t.Errorf("matched filter = %T, want *Denylist", f)
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
