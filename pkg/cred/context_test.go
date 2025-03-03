package cred

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestWithCred(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cred     *Cred
		wantCred *Cred
	}{
		{
			name: "with basic cred",
			cred: &Cred{
				Basic: &BasicCred{
					User:     "user",
					Password: "password",
				},
			},
			wantCred: &Cred{
				Basic: &BasicCred{
					User:     "user",
					Password: "password",
				},
			},
		},
		{
			name: "with empty basic cred",
			cred: &Cred{
				Basic: &BasicCred{},
			},
			wantCred: &Cred{
				Basic: &BasicCred{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			gotCtx := WithCred(ctx, tt.cred)
			gotCred, ok := FromContext(gotCtx)

			if tt.wantCred == nil {
				if ok {
					t.Errorf("FromContext() ok = %v, want false", ok)
				}
				return
			}

			if !ok {
				t.Errorf("FromContext() ok = false, want true")
				return
			}

			if diff := cmp.Diff(tt.wantCred, gotCred); diff != "" {
				t.Errorf("FromContext() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFromContext(t *testing.T) {
	t.Parallel()

	t.Run("missing cred", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		gotCred, ok := FromContext(ctx)

		if ok {
			t.Errorf("FromContext() ok = true, want false")
		}

		if gotCred != nil {
			t.Errorf("FromContext() gotCred = %v, want nil", gotCred)
		}
	})

	t.Run("incorrect value type", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(context.Background(), credKey, "not a cred")
		gotCred, ok := FromContext(ctx)

		if ok {
			t.Errorf("FromContext() ok = true, want false")
		}

		if gotCred != nil {
			t.Errorf("FromContext() gotCred = %v, want nil", gotCred)
		}
	})
}
