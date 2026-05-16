package proxy

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinels_Roundtrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"ErrUpstreamUnavailable", ErrUpstreamUnavailable},
		{"ErrNotFound", ErrNotFound},
		{"ErrUpstreamMalformed", ErrUpstreamMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("fetch foo: %w", tc.err)
			if !errors.Is(wrapped, tc.err) {
				t.Errorf("errors.Is(wrapped, %v) = false, want true", tc.err)
			}
			if wrapped.Error() == "" {
				t.Errorf("wrapped error has empty message")
			}
		})
	}
}

func TestSentinels_Distinct(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		name string
		a, b error
	}{
		{"unavailable_vs_notfound", ErrUpstreamUnavailable, ErrNotFound},
		{"unavailable_vs_malformed", ErrUpstreamUnavailable, ErrUpstreamMalformed},
		{"notfound_vs_malformed", ErrNotFound, ErrUpstreamMalformed},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			if errors.Is(p.a, p.b) {
				t.Errorf("sentinels not distinct: errors.Is(%v, %v) = true", p.a, p.b)
			}
			if errors.Is(p.b, p.a) {
				t.Errorf("sentinels not distinct: errors.Is(%v, %v) = true", p.b, p.a)
			}
		})
	}
}

func TestSentinels_UnwrapChain(t *testing.T) {
	t.Parallel()

	// Per-format fetchers commonly wrap a transport-layer error and the
	// proxy sentinel together so callers get both classification and
	// detail. Verify both ends remain reachable via errors.Is.
	inner := errors.New("dial tcp 1.2.3.4:443: i/o timeout")
	wrapped := fmt.Errorf("fetch metadata: %w: %w", ErrUpstreamUnavailable, inner)

	if !errors.Is(wrapped, ErrUpstreamUnavailable) {
		t.Errorf("errors.Is(wrapped, ErrUpstreamUnavailable) = false, want true")
	}
	if !errors.Is(wrapped, inner) {
		t.Errorf("errors.Is(wrapped, inner) = false, want true")
	}
	if errors.Is(wrapped, ErrNotFound) {
		t.Errorf("errors.Is(wrapped, ErrNotFound) = true, want false")
	}
}
