package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// defaultStreamChunkSize is the chunk size used for chunked PATCH uploads.
// 4 MiB is large enough to amortize per-request HTTP/TLS overhead while still
// keeping per-upload memory bounded — at this size, instance memory is
// dominated by concurrent requests, not by any single body.
const defaultStreamChunkSize = 4 * 1024 * 1024

// defaultChunkPool reuses default-sized chunk buffers across streaming
// uploads. Without it, N concurrent large uploads each hold an independent
// 4 MiB byte slice (N × 4 MiB transient heap) — enough to OOM a Cloud Run
// instance under a burst. The pool entries are pointers because storing a
// slice value in a sync.Pool boxes it on every Put, defeating the point.
var defaultChunkPool = sync.Pool{
	New: func() any {
		b := make([]byte, defaultStreamChunkSize)
		return &b
	},
}

// abortTimeout caps how long abortUpload is willing to wait for the
// registry to acknowledge a DELETE. The whole point of abort is cleanup
// after a failure, so we don't want to compound the original problem by
// blocking on a slow registry.
const abortTimeout = 5 * time.Second

// httpDoer is the minimal interface satisfied by both *http.Client and
// auth.Client. The streaming pusher only needs Do — accepting the smaller
// interface keeps tests free of Client construction.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// streamingPusher is the interface used by Registry.AddFile to push a blob
// without buffering the whole body. It is implemented by *streamPusher in
// production and stubbed in tests.
type streamingPusher interface {
	Push(ctx context.Context, mediaType, expectedDigest string, content io.Reader) (ocispec.Descriptor, error)
}

// streamPusher pushes a blob to an OCI registry using the chunked PATCH
// upload protocol from distribution-spec v1.1.1 ("Pushing a blob in chunks").
// It computes the SHA-256 digest while streaming so the body is never held
// in full on the heap or on disk. Memory is O(chunkSize); disk is zero.
//
// Wire protocol per request:
//
//	POST   /v2/<repo>/blobs/uploads/                   -> 202 + Location
//	PATCH  <Location> Content-Range: 0-N1              -> 202 + Location
//	PATCH  <Location> Content-Range: N1+1-N2           -> 202 + Location
//	  ... repeated until body is drained ...
//	PUT    <Location>?digest=<sha256:...>              -> 201 (commits blob)
//
// On any error after POST, the open upload session is best-effort cleaned up
// with DELETE <Location>.
type streamPusher struct {
	client    httpDoer
	plainHTTP bool
	ref       registry.Reference
	chunkSize int
}

func newStreamPusher(client httpDoer, ref registry.Reference, plainHTTP bool) *streamPusher {
	return &streamPusher{
		client:    client,
		plainHTTP: plainHTTP,
		ref:       ref,
		chunkSize: defaultStreamChunkSize,
	}
}

// Push streams content to the backend, returning a descriptor whose Digest
// and Size are computed from the streamed bytes. MediaType is set from the
// caller's argument; annotations are not set here (callers attach their own).
//
// If expectedDigest is non-empty and disagrees with the digest computed from
// the stream, Push deletes the upload session before the final commit and
// returns ErrDigestMismatch. The bandwidth for the PATCHes that already ran
// is still spent — the trade-off documented in #38 — but we never make the
// blob visible.
func (p *streamPusher) Push(ctx context.Context, mediaType, expectedDigest string, content io.Reader) (ocispec.Descriptor, error) {
	// Mirrors what oras-go's blobStore.Push sets on the context. Without
	// these scope hints, the auth.Client would only request a pull token on
	// its first 401 retry, which would then 401 again on the PATCH/PUT.
	ctx = auth.AppendRepositoryScope(ctx, p.ref, auth.ActionPull, auth.ActionPush)

	location, err := p.startUpload(ctx)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to start blob upload: %w", err)
	}

	digester := digest.SHA256.Digester()
	teed := io.TeeReader(content, digester.Hash())

	// Pool reuse for the default chunk size; tests with smaller sizes
	// allocate ad-hoc to keep pool entries uniformly sized.
	var chunk []byte
	if p.chunkSize == defaultStreamChunkSize {
		bufPtr := defaultChunkPool.Get().(*[]byte)
		defer defaultChunkPool.Put(bufPtr)
		chunk = *bufPtr
	} else {
		chunk = make([]byte, p.chunkSize)
	}

	var size int64
	for {
		// io.ReadFull lets us send full-sized chunks for everything but the
		// tail without a manual loop. EOF / ErrUnexpectedEOF both mean "this
		// is the last chunk"; non-EOF errors propagate.
		n, rerr := io.ReadFull(teed, chunk)
		if n > 0 {
			next, perr := p.patchChunk(ctx, location, chunk[:n], size, size+int64(n)-1)
			if perr != nil {
				p.abortUpload(ctx, location)
				return ocispec.Descriptor{}, fmt.Errorf("failed to PATCH chunk at offset %d: %w", size, perr)
			}
			location = next
			size += int64(n)
		}
		if rerr == nil {
			continue
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		p.abortUpload(ctx, location)
		return ocispec.Descriptor{}, fmt.Errorf("failed to read upload body: %w", rerr)
	}

	finalDigest := digester.Digest()

	// Compare against the caller-supplied digest BEFORE committing. On
	// mismatch we DELETE the upload so the blob never lands in the
	// registry, even though the bytes were already transmitted.
	if expectedDigest != "" {
		expected, err := digest.Parse(expectedDigest)
		if err != nil {
			p.abortUpload(ctx, location)
			return ocispec.Descriptor{}, fmt.Errorf("invalid expected digest %q: %w", expectedDigest, err)
		}
		if expected != finalDigest {
			p.abortUpload(ctx, location)
			return ocispec.Descriptor{}, fmt.Errorf("%w: %q != %q", ErrDigestMismatch, finalDigest, expected)
		}
	}

	if err := p.commitUpload(ctx, location, finalDigest); err != nil {
		p.abortUpload(ctx, location)
		return ocispec.Descriptor{}, fmt.Errorf("failed to commit blob upload: %w", err)
	}

	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    finalDigest,
		Size:      size,
	}, nil
}

// startUpload sends POST /v2/<repo>/blobs/uploads/ and returns the upload
// session URL the registry hands back in the Location header.
func (p *streamPusher) startUpload(ctx context.Context) (string, error) {
	uploadURL := p.uploadInitURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, nil)
	if err != nil {
		return "", err
	}
	req.ContentLength = 0

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", parseStreamErrorResponse(resp)
	}
	return resolveLocation(req.URL, resp)
}

// patchChunk sends a single PATCH with a Content-Range header. start and
// endInclusive are byte offsets per the OCI spec ("0-N" inclusive on both
// ends).
func (p *streamPusher) patchChunk(ctx context.Context, location string, chunk []byte, start, endInclusive int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, location, bytes.NewReader(chunk))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", start, endInclusive))

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", parseStreamErrorResponse(resp)
	}
	return resolveLocation(req.URL, resp)
}

// commitUpload sends the final PUT with ?digest=<sha256:...>. The body is
// always empty: all bytes were transmitted via PATCH already. A 201 Created
// response is the registry's acknowledgement that the blob is now visible
// at finalDigest.
func (p *streamPusher) commitUpload(ctx context.Context, location string, finalDigest digest.Digest) error {
	commitURL, err := appendDigestQuery(location, finalDigest.String())
	if err != nil {
		return fmt.Errorf("failed to build commit URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, commitURL, nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return parseStreamErrorResponse(resp)
	}
	return nil
}

// abortUpload best-effort cancels an in-flight upload session so the
// registry can reclaim partial bytes. Errors are intentionally swallowed:
// the caller already has a "real" error to return and an abort failure is
// strictly less interesting than the underlying cause.
//
// The DELETE runs on a context detached from ctx's cancellation: ctx is
// often already cancelled (request timeout, client disconnect) by the time
// we abort, and an abort that quietly does nothing is the worst outcome —
// it leaves the upload session pinned server-side until the registry's
// own GC runs (and on quota-bound registries like GAR, charges against
// the partial-upload allowance until then).
func (p *streamPusher) abortUpload(ctx context.Context, location string) {
	if location == "" {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(abortCtx, http.MethodDelete, location, nil)
	if err != nil {
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	// Drain whatever the registry sent back so the connection stays
	// reusable for the next request — unbounded but small in practice
	// (registries return JSON or an empty body).
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func (p *streamPusher) uploadInitURL() string {
	scheme := "https"
	if p.plainHTTP {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v2/%s/blobs/uploads/", scheme, p.ref.Host(), p.ref.Repository)
}

// resolveLocation returns the absolute Location URL from resp, applying the
// same port-443 workaround oras-go uses (some registries omit an explicit
// :443 in their Location even though the request had it).
func resolveLocation(reqURL *url.URL, resp *http.Response) (string, error) {
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("missing or invalid Location header: %w", err)
	}
	if reqURL.Hostname() == loc.Hostname() && reqURL.Port() == "443" && loc.Port() == "" {
		loc.Host = loc.Hostname() + ":" + reqURL.Port()
	}
	return loc.String(), nil
}

// appendDigestQuery returns location with a digest query parameter set,
// preserving any other query parameters the registry put there.
func appendDigestQuery(location, dgst string) (string, error) {
	u, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("digest", dgst)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseStreamErrorResponse mirrors oras-go's internal errutil.ParseErrorResponse.
// We re-implement it because the upstream is in an internal/ package; the
// shape of the returned error is identical so callers can match on
// errcode.ErrorResponse via errors.As (and pkg/oci.HasCode keeps working).
//
// Callers are still responsible for closing resp.Body. The body is fully
// drained here so the underlying connection stays in net/http's
// keep-alive pool — without the drain, an upstream that sends an
// error JSON longer than a partial-decode terminus (or just a non-JSON
// HTML 502 page from a frontend proxy) would leave bytes on the wire and
// force the next request onto a fresh TCP+TLS handshake.
func parseStreamErrorResponse(resp *http.Response) error {
	result := &errcode.ErrorResponse{
		Method:     resp.Request.Method,
		URL:        resp.Request.URL,
		StatusCode: resp.StatusCode,
	}
	var body struct {
		Errors errcode.Errors `json:"errors"`
	}
	const maxErrorBytes = 8 * 1024
	lr := io.LimitReader(resp.Body, maxErrorBytes)
	if err := json.NewDecoder(lr).Decode(&body); err == nil {
		result.Errors = body.Errors
	}
	_, _ = io.Copy(io.Discard, lr)
	return result
}
