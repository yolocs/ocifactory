package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// Blob-redirect outcome labels recorded via metrics.Recorder.BlobRedirect.
// "redirected" — the backend returned a presigned URL the caller can 307 to.
// "inline"     — the backend serves the blob inline; caller falls back to ReadFile.
// "error"      — the probe failed and the caller should fall back to ReadFile.
const (
	BlobRedirectOutcomeRedirected = "redirected"
	BlobRedirectOutcomeInline     = "inline"
	BlobRedirectOutcomeError      = "error"
)

// BlobRedirectURL returns a URL the client can follow to download the
// blob directly from the backend's CDN/object store, bypassing
// ocifactory's data path.
//
// Returns ("", nil) when the backend serves blobs inline (no redirect
// available) or when --disable-blob-redirect is set; the caller should
// fall back to ReadFile in that case. Returns ("", err) on hard
// failures — the caller should also fall back to ReadFile, treating
// the redirect probe as best-effort.
//
// Implementation: resolve the file blob descriptor the same way
// ReadFile does, then issue a HEAD against the backend's
// /v2/<repo>/blobs/<digest> using a non-redirect-following auth.Client.
// A 3xx response surfaces the Location header; a 200 means the backend
// is willing to serve the bytes itself.
func (r *Registry) BlobRedirectURL(ctx context.Context, f *RepoFile) (string, error) {
	if r.disableBlobRedirect {
		return "", nil
	}
	url, err := r.blobRedirectURL(ctx, f)
	r.rec.BlobRedirect(blobRedirectOutcome(url, err))
	return url, err
}

func (r *Registry) blobRedirectURL(ctx context.Context, f *RepoFile) (string, error) {
	if f.OwningTag == "" && f.RefTag == "" {
		return "", fmt.Errorf("either OwningTag or RefTag must be set")
	}

	backend, err := r.newBackendFunc(ctx, f)
	if err != nil {
		return "", err
	}

	canonicalTag, err := r.resolveCanonicalTag(ctx, backend, f)
	if err != nil {
		return "", err
	}

	fileManifestDesc, err := backend.Resolve(ctx, fileTagFor(canonicalTag, f.Name))
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return "", fmt.Errorf("file %q not found in version %q: %w", f.Name, canonicalTag, errdef.ErrNotFound)
		}
		return "", fmt.Errorf("failed to resolve file manifest tag: %w", err)
	}
	blobDesc, err := fetchBlobDescriptor(ctx, backend, fileManifestDesc)
	if err != nil {
		return "", err
	}
	return r.probeBlobRedirect(ctx, f, blobDesc)
}

// probeBlobRedirect issues a HEAD against the backend's
// /v2/<repo>/blobs/<digest> endpoint without following redirects, so a
// 3xx Location is surfaced directly. Backends that serve blobs inline
// answer 200 and we return ("", nil) so the caller falls through to
// streaming via ReadFile.
func (r *Registry) probeBlobRedirect(ctx context.Context, f *RepoFile, blobDesc ocispec.Descriptor) (string, error) {
	repoRef := r.baseURL.Host + r.baseURL.Path + "/" + f.OwningRepo
	ref, err := registry.ParseReference(repoRef)
	if err != nil {
		return "", fmt.Errorf("failed to parse repository reference %q: %w", repoRef, err)
	}

	scheme := "https"
	if r.baseURL.Scheme == "http" {
		scheme = "http"
	}
	blobURL := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, ref.Host(), ref.Repository, blobDesc.Digest)

	// AppendRepositoryScope mirrors what oras-go sets on the context
	// for any blob fetch — without it, the auth.Client would only
	// learn it needs a pull token after the first 401 retry.
	probeCtx := auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)

	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, blobURL, nil)
	if err != nil {
		return "", fmt.Errorf("build redirect probe request: %w", err)
	}

	resp, err := r.probeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("blob redirect probe transport error: %w", err)
	}
	defer resp.Body.Close()
	// HEAD has no body in practice, but drain anyway so the connection
	// stays in net/http's keep-alive pool.
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc, err := resp.Location()
		if err != nil {
			return "", fmt.Errorf("backend returned %d but Location header was missing or invalid: %w", resp.StatusCode, err)
		}
		return loc.String(), nil
	case resp.StatusCode == http.StatusOK:
		return "", nil
	default:
		return "", fmt.Errorf("blob redirect probe returned %s", resp.Status)
	}
}

func blobRedirectOutcome(url string, err error) string {
	switch {
	case err != nil:
		return BlobRedirectOutcomeError
	case url == "":
		return BlobRedirectOutcomeInline
	default:
		return BlobRedirectOutcomeRedirected
	}
}
