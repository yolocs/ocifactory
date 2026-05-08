package oci

import (
	"errors"
	"fmt"
	"net/http"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// HasCode returns true if the error is an ErrorResponse and has the given code.
// The code is the HTTP status code.
func HasCode(err error, code int) bool {
	var ec *errcode.ErrorResponse
	return errors.As(err, &ec) && ec.StatusCode == code
}

// normalizeNotFound translates a backend HTTP 404 (e.g. zot's
// NAME_UNKNOWN response for a never-pushed repository) into
// errdef.ErrNotFound. oras-go surfaces these as errcode.ErrorResponse
// values that don't satisfy errors.Is(err, errdef.ErrNotFound), so
// handlers that probe for "does this thing exist?" via ListTags would
// otherwise treat a missing repo as a 500. Wrapping with multi-%w
// preserves the original ErrorResponse for HasCode / errors.As callers
// while making the canonical sentinel detectable.
func normalizeNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errdef.ErrNotFound) {
		return err
	}
	if HasCode(err, http.StatusNotFound) {
		return fmt.Errorf("%w: %w", errdef.ErrNotFound, err)
	}
	return err
}
