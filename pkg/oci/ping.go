package oci

import (
	"context"
	"fmt"
	"net/http"

	"oras.land/oras-go/v2/registry/remote/retry"
)

// Ping issues HEAD /v2/ against the configured backend registry. It is
// the cheapest probe an OCI v2 registry universally supports — and is
// what /readyz uses to decide whether the backend is reachable.
//
// Most public registries answer an unauthenticated HEAD /v2/ with 401
// Unauthorized (challenging for a bearer token). That still proves the
// backend is up and reachable, so Ping treats 401 as success — only 5xx,
// 503-style outages, and transport errors fail the probe. ctx controls
// the deadline; the caller is expected to set one (e.g. 2s) so a
// readyz handler can never block longer than its probe timeout.
func (r *Registry) Ping(ctx context.Context) error {
	pingURL := r.baseURL.Scheme + "://" + r.baseURL.Host + "/v2/"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, pingURL, nil)
	if err != nil {
		return fmt.Errorf("build ping request: %w", err)
	}
	resp, err := retry.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ping %s: %w", pingURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		// Authentication challenge — registry is up; we just don't
		// have credentials. /readyz should pass.
		return nil
	default:
		return fmt.Errorf("ping %s returned status %d", pingURL, resp.StatusCode)
	}
}
