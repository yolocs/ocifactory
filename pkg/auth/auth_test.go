package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSubjectContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		set  *Subject
		want *Subject
		ok   bool
	}{
		{
			name: "round trip",
			set:  &Subject{Issuer: "https://example.com", ID: "abc"},
			want: &Subject{Issuer: "https://example.com", ID: "abc"},
			ok:   true,
		},
		{
			name: "nil subject still stored",
			set:  nil,
			want: nil,
			// type assertion against nil typed pointer
			// returns ok=true; this exists only to pin the
			// behaviour.
			ok: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := WithSubject(t.Context(), tc.set)
			got, ok := SubjectFromContext(ctx)
			if ok != tc.ok {
				t.Errorf("SubjectFromContext ok = %v, want %v", ok, tc.ok)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("SubjectFromContext mismatch (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		got, ok := SubjectFromContext(t.Context())
		if ok {
			t.Errorf("expected ok=false on empty context, got %v", got)
		}
	})
}

func TestAlwaysAnonymous(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	subj, err := AlwaysAnonymous.Authenticate(r)
	if err != nil {
		t.Fatalf("AlwaysAnonymous error = %v", err)
	}
	want := &Subject{Issuer: "anonymous", ID: "anonymous"}
	if diff := cmp.Diff(want, subj); diff != "" {
		t.Errorf("AlwaysAnonymous subject mismatch (-want +got):\n%s", diff)
	}
}
