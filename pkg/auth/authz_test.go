package auth

import (
	"context"
	"errors"
	"testing"
)

func TestCheck(t *testing.T) {
	t.Parallel()

	allow := AuthorizerFunc(func(context.Context, *AuthContext, Action) error { return nil })
	deny := AuthorizerFunc(func(context.Context, *AuthContext, Action) error { return ErrUnauthorized })
	boom := errors.New("boom")
	broken := AuthorizerFunc(func(context.Context, *AuthContext, Action) error { return boom })

	tests := []struct {
		name    string
		authz   Authorizer
		ac      *AuthContext
		setACOn bool
		want    error
	}{
		{
			name:    "nil authz allows",
			authz:   nil,
			ac:      &AuthContext{Issuer: "i", ID: "u"},
			setACOn: true,
			want:    nil,
		},
		{
			name:    "missing AuthContext denies",
			authz:   allow,
			setACOn: false,
			want:    ErrUnauthorized,
		},
		{
			name:    "nil AuthContext on context denies",
			authz:   allow,
			ac:      nil,
			setACOn: true,
			want:    ErrUnauthorized,
		},
		{
			name:    "allow",
			authz:   allow,
			ac:      &AuthContext{Issuer: "i", ID: "u"},
			setACOn: true,
			want:    nil,
		},
		{
			name:    "deny propagates ErrUnauthorized",
			authz:   deny,
			ac:      &AuthContext{Issuer: "i", ID: "u"},
			setACOn: true,
			want:    ErrUnauthorized,
		},
		{
			name:    "internal error propagates",
			authz:   broken,
			ac:      &AuthContext{Issuer: "i", ID: "u"},
			setACOn: true,
			want:    boom,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tc.setACOn {
				ctx = WithAuthContext(ctx, tc.ac)
			}
			got := Check(ctx, tc.authz, Action{Repo: "r", Format: "f", Op: OpRead})
			if !errors.Is(got, tc.want) && got != tc.want {
				t.Errorf("Check() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllowAllAndDenyAll(t *testing.T) {
	t.Parallel()

	ac := &AuthContext{Issuer: "i", ID: "u"}
	act := Action{Repo: "r", Format: "f", Op: OpRead}

	if err := AllowAll.Authorize(t.Context(), ac, act); err != nil {
		t.Errorf("AllowAll.Authorize = %v, want nil", err)
	}
	if err := DenyAll.Authorize(t.Context(), ac, act); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("DenyAll.Authorize = %v, want ErrUnauthorized", err)
	}
}
