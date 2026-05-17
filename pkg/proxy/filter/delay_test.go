package filter_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/proxy/filter"
)

func TestDelay_Decide(t *testing.T) {
	t.Parallel()

	// Pin a wall clock so test outcomes are deterministic.
	frozen := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		minAge     time.Duration
		uploadTime time.Time
		want       filter.Decision
	}{
		{
			name:       "zero-upload-time-needs-more-data",
			minAge:     24 * time.Hour,
			uploadTime: time.Time{},
			want:       filter.DecisionNeedsMoreData,
		},
		{
			name:       "younger-than-min-age-denied",
			minAge:     24 * time.Hour,
			uploadTime: frozen.Add(-1 * time.Hour),
			want:       filter.DecisionDeny,
		},
		{
			name:       "exactly-min-age-allowed",
			minAge:     24 * time.Hour,
			uploadTime: frozen.Add(-24 * time.Hour),
			want:       filter.DecisionAllow,
		},
		{
			name:       "older-than-min-age-allowed",
			minAge:     24 * time.Hour,
			uploadTime: frozen.Add(-7 * 24 * time.Hour),
			want:       filter.DecisionAllow,
		},
		{
			name:       "future-upload-time-treated-as-fresh",
			minAge:     1 * time.Hour,
			uploadTime: frozen.Add(1 * time.Hour),
			want:       filter.DecisionDeny,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &filter.Delay{MinAge: tc.minAge}
			ctx := filter.WithClock(t.Context(), func() time.Time { return frozen })
			got, err := d.Decide(ctx, filter.Ref{Package: "pkg", Version: "1.0.0", UploadTime: tc.uploadTime})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got != tc.want {
				t.Errorf("Decide() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDelay_ContextlessUsesWallClock pins the production code path:
// when no clock is installed via WithClock, Delay falls back to
// time.Now. Using a clearly stale upload time (10y in the past) makes
// the assertion robust against wall-clock skew.
func TestDelay_ContextlessUsesWallClock(t *testing.T) {
	t.Parallel()
	d := &filter.Delay{MinAge: time.Hour}
	got, err := d.Decide(t.Context(), filter.Ref{
		Package:    "pkg",
		Version:    "1.0.0",
		UploadTime: time.Now().Add(-10 * 365 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got != filter.DecisionAllow {
		t.Errorf("Decide() = %v, want %v", got, filter.DecisionAllow)
	}
}

func TestDelay_Kind(t *testing.T) {
	t.Parallel()
	d := &filter.Delay{MinAge: time.Hour}
	if got := d.Kind(); got != filter.KindDelay {
		t.Errorf("Kind() = %q, want %q", got, filter.KindDelay)
	}
}

func TestDelay_Construction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "valid-hours",
			body: `{"kind":"delay","min_age":"24h"}`,
		},
		{
			name: "valid-compound",
			body: `{"kind":"delay","min_age":"1h30m"}`,
		},
		{
			name:    "missing-min-age",
			body:    `{"kind":"delay"}`,
			wantErr: true,
		},
		{
			name:    "zero-min-age",
			body:    `{"kind":"delay","min_age":"0s"}`,
			wantErr: true,
		},
		{
			name:    "negative-min-age",
			body:    `{"kind":"delay","min_age":"-1h"}`,
			wantErr: true,
		},
		{
			name:    "unparseable",
			body:    `{"kind":"delay","min_age":"forever"}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := filter.UnmarshalFilter([]byte(tc.body))
			if tc.wantErr {
				if err == nil || !errors.Is(err, filter.ErrInvalidFilter) {
					t.Errorf("UnmarshalFilter(%q) error = %v, want errors.Is ErrInvalidFilter", tc.body, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalFilter(%q) error: %v", tc.body, err)
			}
		})
	}
}
