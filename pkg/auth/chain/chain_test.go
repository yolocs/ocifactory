package chain

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/auth"
)

func fixed(subj *auth.Subject, err error) auth.Authenticator {
	return auth.AuthenticatorFunc(func(*http.Request) (*auth.Subject, error) { return subj, err })
}

func TestChain_Authenticate(t *testing.T) {
	t.Parallel()

	subjA := &auth.Subject{Issuer: "a", ID: "a-user"}
	subjB := &auth.Subject{Issuer: "b", ID: "b-user"}

	tests := []struct {
		name        string
		children    []auth.Authenticator
		wantSubject *auth.Subject
		wantErrIs   error
	}{
		{
			name:      "empty chain returns ErrNoCredential",
			children:  nil,
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name: "first child wins",
			children: []auth.Authenticator{
				fixed(subjA, nil),
				fixed(subjB, nil),
			},
			wantSubject: subjA,
		},
		{
			name: "no-credential falls through",
			children: []auth.Authenticator{
				fixed(nil, auth.ErrNoCredential),
				fixed(subjB, nil),
			},
			wantSubject: subjB,
		},
		{
			name: "all no-credential returns ErrNoCredential",
			children: []auth.Authenticator{
				fixed(nil, auth.ErrNoCredential),
				fixed(nil, auth.ErrNoCredential),
			},
			wantErrIs: auth.ErrNoCredential,
		},
		{
			name: "invalid token short-circuits",
			children: []auth.Authenticator{
				fixed(nil, auth.ErrInvalidToken),
				fixed(subjB, nil), // never reached
			},
			wantErrIs: auth.ErrInvalidToken,
		},
		{
			name: "issuer unavailable propagates",
			children: []auth.Authenticator{
				fixed(nil, auth.ErrIssuerUnavailable),
				fixed(subjB, nil),
			},
			wantErrIs: auth.ErrIssuerUnavailable,
		},
		{
			name: "nil children skipped",
			children: []auth.Authenticator{
				nil,
				fixed(subjA, nil),
			},
			wantSubject: subjA,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(tc.children...)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			subj, err := c.Authenticate(r)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Errorf("error = %v, want errors.Is(%v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantSubject, subj); diff != "" {
				t.Errorf("subject mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestChain_MultiIssuerFallthrough is the integration assertion
// for the multi-OIDC use case the auth design specifies:
// [oidc(A), oidc(B)] must accept a B-issued token by skipping A.
// The chain alone doesn't know how to peek issuers — that's
// pkg/auth/oidc's responsibility — but the contract enforced
// here (ErrNoCredential falls through, ErrInvalidToken
// short-circuits) is what makes the dispatch work.
func TestChain_MultiIssuerFallthrough(t *testing.T) {
	t.Parallel()

	issuerA := auth.AuthenticatorFunc(func(*http.Request) (*auth.Subject, error) {
		// Pretends to be Google's authenticator: doesn't
		// recognise this token's issuer, falls through.
		return nil, auth.ErrNoCredential
	})
	issuerB := auth.AuthenticatorFunc(func(*http.Request) (*auth.Subject, error) {
		return &auth.Subject{Issuer: "B", ID: "user"}, nil
	})

	c := New(issuerA, issuerB)
	subj, err := c.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if subj == nil || subj.Issuer != "B" {
		t.Errorf("subject.Issuer = %v, want B", subj)
	}
}

// TestChain_OrderingPreserved guarantees that callers can rely on the
// declaration-order semantics — once a child accepts, no later child
// runs. Two side-effecting children make the assertion trivial.
func TestChain_OrderingPreserved(t *testing.T) {
	t.Parallel()

	var calls []string
	probe := func(name string, ret error) auth.Authenticator {
		return auth.AuthenticatorFunc(func(*http.Request) (*auth.Subject, error) {
			calls = append(calls, name)
			if ret != nil {
				return nil, ret
			}
			return &auth.Subject{Issuer: name}, nil
		})
	}

	c := New(
		probe("first", auth.ErrNoCredential),
		probe("second", nil),
		probe("third", nil),
	)
	if _, err := c.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatalf("Authenticate error: %v", err)
	}
	want := []string{"first", "second"}
	if diff := cmp.Diff(want, calls); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}
}
