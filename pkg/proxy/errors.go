// Package proxy holds the shared building blocks (typed upstream
// errors here; the HTTP client in pkg/proxy/httpclient) that the
// per-format proxy fetchers under pkg/proxy/<format> compose.
package proxy

import "errors"

// ErrUpstreamUnavailable indicates the upstream registry could not
// be reached or returned a transient error after the configured
// retry budget was exhausted. Wrap with %w when surfacing from a
// per-format fetcher.
var ErrUpstreamUnavailable = errors.New("proxy: upstream unavailable")

// ErrNotFound indicates the upstream registry reported that the
// requested package, version, or file does not exist (HTTP 404 or
// the format's equivalent signal). Distinct from
// ErrUpstreamUnavailable so handlers can distinguish "missing" from
// "broken" — only the former is a 404 worth returning to the
// client unchanged.
var ErrNotFound = errors.New("proxy: upstream not found")

// ErrUpstreamMalformed indicates the upstream returned a response
// that did not parse against the format's expected schema (e.g. a
// simple-index HTML body with no anchor tags, a packument JSON
// missing the versions map, a maven-metadata.xml without
// <versioning>). Distinct from ErrUpstreamUnavailable so handlers
// don't waste retry budget on a fundamentally broken response.
var ErrUpstreamMalformed = errors.New("proxy: upstream malformed response")
