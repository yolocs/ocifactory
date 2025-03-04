package oci

import (
	"errors"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// HasCode returns true if the error is an ErrorResponse and has the given code.
// The code is the HTTP status code.
func HasCode(err error, code int) bool {
	var ec *errcode.ErrorResponse
	return errors.As(err, &ec) && ec.StatusCode == code
}
