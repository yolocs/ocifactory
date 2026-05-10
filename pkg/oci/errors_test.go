package oci

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

func TestHasCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		code     int
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			code:     http.StatusNotFound,
			expected: false,
		},
		{
			name:     "non-ErrorResponse error",
			err:      errors.New("some error"),
			code:     http.StatusNotFound,
			expected: false,
		},
		{
			name: "ErrorResponse with matching code",
			err: &errcode.ErrorResponse{
				StatusCode: http.StatusNotFound,
				Method:     "GET",
				URL:        &url.URL{},
			},
			code:     http.StatusNotFound,
			expected: true,
		},
		{
			name: "ErrorResponse with non-matching code",
			err: &errcode.ErrorResponse{
				StatusCode: http.StatusBadRequest,
				Method:     "GET",
				URL:        &url.URL{},
			},
			code:     http.StatusNotFound,
			expected: false,
		},
		{
			name: "wrapped ErrorResponse with matching code",
			err: errors.New("wrapped: " + (&errcode.ErrorResponse{
				StatusCode: http.StatusNotFound,
				Method:     "GET",
				URL:        &url.URL{},
			}).Error()),
			code:     http.StatusNotFound,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := HasCode(tt.err, tt.code)
			if result != tt.expected {
				t.Errorf("HasCode() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestNormalizeNotFound(t *testing.T) {
	t.Parallel()

	resp404 := &errcode.ErrorResponse{
		StatusCode: http.StatusNotFound,
		Method:     "GET",
		URL:        &url.URL{Scheme: "http", Host: "example", Path: "/v2/x/tags/list"},
		Errors:     errcode.Errors{{Code: "NAME_UNKNOWN", Message: "name unknown"}},
	}

	tests := []struct {
		name       string
		err        error
		wantNotFnd bool // errors.Is(err, errdef.ErrNotFound) after normalize
		wantHas404 bool // HasCode(err, 404) after normalize
	}{
		{
			name:       "nil",
			err:        nil,
			wantNotFnd: false,
			wantHas404: false,
		},
		{
			name:       "errdef.ErrNotFound passes through",
			err:        fmt.Errorf("missing: %w", errdef.ErrNotFound),
			wantNotFnd: true,
			wantHas404: false,
		},
		{
			name:       "404 ErrorResponse becomes errdef.ErrNotFound while preserving HasCode",
			err:        resp404,
			wantNotFnd: true,
			wantHas404: true,
		},
		{
			name:       "wrapped 404 ErrorResponse",
			err:        fmt.Errorf("list tags: %w", resp404),
			wantNotFnd: true,
			wantHas404: true,
		},
		{
			name:       "non-404 ErrorResponse is unchanged",
			err:        &errcode.ErrorResponse{StatusCode: http.StatusUnauthorized, Method: "GET", URL: &url.URL{}},
			wantNotFnd: false,
			wantHas404: false,
		},
		{
			name:       "unrelated error",
			err:        errors.New("boom"),
			wantNotFnd: false,
			wantHas404: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeNotFound(tc.err)
			if (got == nil) != (tc.err == nil) {
				t.Fatalf("normalizeNotFound nil-ness changed: got %v, in %v", got, tc.err)
			}
			if errors.Is(got, errdef.ErrNotFound) != tc.wantNotFnd {
				t.Errorf("errors.Is(got, errdef.ErrNotFound) = %v, want %v (got=%v)",
					errors.Is(got, errdef.ErrNotFound), tc.wantNotFnd, got)
			}
			if HasCode(got, http.StatusNotFound) != tc.wantHas404 {
				t.Errorf("HasCode(got, 404) = %v, want %v (got=%v)",
					HasCode(got, http.StatusNotFound), tc.wantHas404, got)
			}
		})
	}
}
