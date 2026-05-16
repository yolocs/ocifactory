package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// KindDelay is the JSON discriminator for [Delay].
const KindDelay = "delay"

// Delay denies any version whose upstream upload time is more recent
// than MinAge ago. It returns [DecisionNeedsMoreData] when
// [Ref.UploadTime] is zero, signalling the chain caller to fetch
// upstream metadata before re-running the chain.
//
// Use case: defang typosquats and freshly-published malicious
// versions by forcing every fetched version to "age" upstream first.
// A 24h delay is a common starting point.
//
// Delay holds nothing but configuration; the wall clock comes from
// the request context via [WithClock] (tests) or [time.Now]
// (production). That keeps the value comparable by [cmp.Diff] in
// spec roundtrip tests and free of per-instance mutable state.
type Delay struct {
	// MinAge is the minimum age a version must have upstream before
	// this filter allows it through. Must be positive; zero or
	// negative is rejected at construction.
	MinAge time.Duration `json:"-"`
}

// Kind returns [KindDelay].
func (d *Delay) Kind() string { return KindDelay }

// Allow returns [DecisionNeedsMoreData] when ref.UploadTime is zero
// (the chain should re-run after upstream metadata fetch), otherwise
// [DecisionDeny] when the version is younger than MinAge and
// [DecisionAllow] when it has aged enough.
func (d *Delay) Allow(ctx context.Context, ref Ref) (Decision, error) {
	if ref.UploadTime.IsZero() {
		return DecisionNeedsMoreData, nil
	}
	now := ClockFromContext(ctx)
	if now().Sub(ref.UploadTime) < d.MinAge {
		return DecisionDeny, nil
	}
	return DecisionAllow, nil
}

// MarshalJSON emits the kind discriminator plus a human-friendly
// duration string (e.g. "24h0m0s") via [time.Duration.String]. The
// parse side accepts any form [time.ParseDuration] accepts ("24h",
// "1m30s", …).
func (d *Delay) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind   string `json:"kind"`
		MinAge string `json:"min_age"`
	}{KindDelay, d.MinAge.String()})
}

// UnmarshalJSON parses the duration string form. Numeric durations
// (e.g. `"min_age": 86400000000000`) are not supported: the operator
// is expected to write the human form so the spec is readable when
// inspected directly.
func (d *Delay) UnmarshalJSON(data []byte) error {
	var aux struct {
		Kind   string `json:"kind"`
		MinAge string `json:"min_age"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.MinAge == "" {
		// Leave MinAge zero; validate() rejects it with a clearer
		// message than ParseDuration's "invalid duration".
		return nil
	}
	dur, err := time.ParseDuration(aux.MinAge)
	if err != nil {
		return fmt.Errorf("%w: delay min_age %q: %v", ErrInvalidFilter, aux.MinAge, err)
	}
	d.MinAge = dur
	return nil
}

// validate runs at construction time.
func (d *Delay) validate() error {
	if d.MinAge <= 0 {
		return fmt.Errorf("%w: delay min_age must be positive (got %s)", ErrInvalidFilter, d.MinAge)
	}
	return nil
}

// clockKey is the unexported context key under which [WithClock]
// stores a test clock. Unexported so callers must go through
// [WithClock] / [ClockFromContext].
type clockKey struct{}

// WithClock returns a context that overrides the wall clock used by
// [Delay] and any future time-dependent filter. Intended for tests;
// production code never calls this and gets [time.Now].
func WithClock(ctx context.Context, now func() time.Time) context.Context {
	if now == nil {
		return ctx
	}
	return context.WithValue(ctx, clockKey{}, now)
}

// ClockFromContext returns the clock installed by [WithClock], or
// [time.Now] if none is set.
func ClockFromContext(ctx context.Context) func() time.Time {
	if fn, ok := ctx.Value(clockKey{}).(func() time.Time); ok {
		return fn
	}
	return time.Now
}
