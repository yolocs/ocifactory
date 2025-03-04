package oci

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

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
