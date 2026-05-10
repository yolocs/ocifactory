package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yolocs/ocifactory/pkg/auth"
)

func TestAuthContext_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ac   *auth.AuthContext
		want string
	}{
		{
			name: "nil",
			ac:   nil,
			want: "",
		},
		{
			name: "issuer-and-id",
			ac:   &auth.AuthContext{Issuer: "https://accounts.google.com", ID: "112233"},
			want: "https://accounts.google.com#112233",
		},
		{
			name: "anonymous",
			ac:   &auth.AuthContext{Issuer: "anonymous", ID: "anonymous"},
			want: "anonymous#anonymous",
		},
		{
			name: "empty-id",
			ac:   &auth.AuthContext{Issuer: "https://accounts.google.com"},
			want: "https://accounts.google.com#",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ac.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAllowAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ac   *auth.AuthContext
		op   auth.Op
	}{
		{name: "nil-subject-read", ac: nil, op: auth.OpRead},
		{name: "nil-subject-write", ac: nil, op: auth.OpWrite},
		{name: "subject-read", ac: &auth.AuthContext{Issuer: "x", ID: "y"}, op: auth.OpRead},
		{name: "subject-write", ac: &auth.AuthContext{Issuer: "x", ID: "y"}, op: auth.OpWrite},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := auth.AllowAll.Authorize(t.Context(), tc.ac, tc.op); err != nil {
				t.Errorf("AllowAll.Authorize: %v, want nil", err)
			}
		})
	}
}

func TestDenyAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ac   *auth.AuthContext
		op   auth.Op
	}{
		{name: "nil-subject", ac: nil, op: auth.OpRead},
		{name: "with-subject-read", ac: &auth.AuthContext{Issuer: "x", ID: "y"}, op: auth.OpRead},
		{name: "with-subject-write", ac: &auth.AuthContext{Issuer: "x", ID: "y"}, op: auth.OpWrite},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := auth.DenyAll.Authorize(t.Context(), tc.ac, tc.op)
			if err == nil {
				t.Fatalf("DenyAll.Authorize: nil error, want one wrapping ErrUnauthorized")
			}
			if !errors.Is(err, auth.ErrUnauthorized) {
				t.Errorf("DenyAll.Authorize = %v, want errors.Is(ErrUnauthorized)", err)
			}
		})
	}
}

func TestAuthorizerFunc(t *testing.T) {
	t.Parallel()

	called := false
	var f auth.Authorizer = auth.AuthorizerFunc(func(_ context.Context, _ *auth.AuthContext, op auth.Op) error {
		called = true
		if op != auth.OpRead {
			t.Errorf("op = %s, want %s", op, auth.OpRead)
		}
		return nil
	})

	if err := f.Authorize(t.Context(), nil, auth.OpRead); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !called {
		t.Fatal("AuthorizerFunc was not invoked")
	}
}
