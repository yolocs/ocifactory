package oci

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	digest "github.com/opencontainers/go-digest"
	"github.com/yolocs/ocifactory/pkg/testutil"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// fakeRegistry is a minimal in-process OCI registry that implements the
// blob upload subset of distribution-spec v1.1.1 used by streamPusher:
//
//   - POST   /v2/<repo>/blobs/uploads/                 -> 202 + Location
//   - PATCH  /v2/_uploads/<id>   Content-Range: a-b    -> 202 + Location
//   - PUT    /v2/_uploads/<id>?digest=<sha256:...>     -> 201
//   - DELETE /v2/_uploads/<id>                          -> 204
//
// It records every request it receives so tests can assert on the wire
// protocol (Content-Range math, single PUT with ?digest=, DELETE on abort).
type fakeRegistry struct {
	mu sync.Mutex

	server *httptest.Server

	requests []recordedReq
	uploads  map[string]*uploadSession // upload-id -> bytes received so far
	commits  map[digest.Digest][]byte  // committed blobs by digest

	// Per-test fault injection.
	failNthPatch    int // when > 0, return failPatchStatus on the Nth PATCH (1-indexed)
	failPatchStatus int

	patchSeen int
}

type uploadSession struct {
	id      string
	body    []byte
	deleted bool
}

type recordedReq struct {
	method       string
	path         string
	contentRange string
	query        string
	bodyLen      int
}

func newFakeRegistry() *fakeRegistry {
	r := &fakeRegistry{
		uploads: map[string]*uploadSession{},
		commits: map[digest.Digest][]byte{},
	}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

func (f *fakeRegistry) close() { f.server.Close() }

func (f *fakeRegistry) baseURL() *url.URL {
	u, _ := url.Parse(f.server.URL)
	return u
}

func (f *fakeRegistry) handle(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, _ := io.ReadAll(req.Body)
	rec := recordedReq{
		method:       req.Method,
		path:         req.URL.Path,
		contentRange: req.Header.Get("Content-Range"),
		query:        req.URL.RawQuery,
		bodyLen:      len(body),
	}
	f.requests = append(f.requests, rec)

	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/blobs/uploads/"):
		id := fmt.Sprintf("up-%d", len(f.uploads)+1)
		f.uploads[id] = &uploadSession{id: id}
		w.Header().Set("Location", "/v2/_uploads/"+id)
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPatch && strings.HasPrefix(req.URL.Path, "/v2/_uploads/"):
		f.patchSeen++
		id := strings.TrimPrefix(req.URL.Path, "/v2/_uploads/")
		sess, ok := f.uploads[id]
		if !ok || sess.deleted {
			http.Error(w, "unknown upload", http.StatusNotFound)
			return
		}
		if f.failNthPatch > 0 && f.patchSeen == f.failNthPatch {
			writeRegistryError(w, f.failPatchStatus, "INTERNAL", "injected PATCH failure")
			return
		}
		// Validate Content-Range matches what we expect: bytes appended
		// at the current offset.
		var start, end int64
		if _, err := fmt.Sscanf(rec.contentRange, "%d-%d", &start, &end); err != nil {
			http.Error(w, "bad content-range", http.StatusBadRequest)
			return
		}
		if start != int64(len(sess.body)) || end-start+1 != int64(len(body)) {
			http.Error(w, fmt.Sprintf("bad content-range %q for upload at offset %d (len=%d)", rec.contentRange, len(sess.body), len(body)), http.StatusBadRequest)
			return
		}
		sess.body = append(sess.body, body...)
		w.Header().Set("Location", "/v2/_uploads/"+id)
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut && strings.HasPrefix(req.URL.Path, "/v2/_uploads/"):
		id := strings.TrimPrefix(req.URL.Path, "/v2/_uploads/")
		sess, ok := f.uploads[id]
		if !ok || sess.deleted {
			http.Error(w, "unknown upload", http.StatusNotFound)
			return
		}
		dgst := req.URL.Query().Get("digest")
		if dgst == "" {
			http.Error(w, "missing digest", http.StatusBadRequest)
			return
		}
		// The body of the PUT may be empty (chunked path) or non-empty
		// (monolithic). Append whatever was sent.
		sess.body = append(sess.body, body...)
		gotDigest := sha256Digest(sess.body)
		if string(gotDigest) != dgst {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		f.commits[gotDigest] = sess.body
		delete(f.uploads, id)
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/v2/_uploads/"):
		id := strings.TrimPrefix(req.URL.Path, "/v2/_uploads/")
		if sess, ok := f.uploads[id]; ok {
			sess.deleted = true
			delete(f.uploads, id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := fmt.Sprintf(`{"errors":[{"code":%q,"message":%q}]}`, code, message)
	_, _ = io.WriteString(w, body)
}

func newPusherFor(t *testing.T, fr *fakeRegistry, repo string, chunkSize int) *streamPusher {
	t.Helper()
	base := fr.baseURL()
	ref := registry.Reference{
		Registry:   base.Host,
		Repository: repo,
	}
	// httptest.Server.Close calls CloseIdleConnections on
	// http.DefaultTransport, which would race parallel sibling tests
	// sharing http.DefaultClient. Give each pusher its own transport.
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(func() { client.Transport.(*http.Transport).CloseIdleConnections() })
	p := newStreamPusher(client, ref, true /* plainHTTP */)
	if chunkSize > 0 {
		p.chunkSize = chunkSize
	}
	return p
}

// TestStreamPusher_Push exercises the chunked PATCH protocol for a range
// of body sizes. The fake registry validates Content-Range math on every
// PATCH; the test then asserts the recorded request sequence and the
// committed blob bytes.
func TestStreamPusher_Push(t *testing.T) {
	t.Parallel()

	const chunkSize = 16

	tests := []struct {
		name        string
		body        []byte
		wantPatches int
	}{
		{name: "empty body", body: []byte{}, wantPatches: 0},
		{name: "smaller than one chunk", body: []byte("hello"), wantPatches: 1},
		{name: "exactly one chunk", body: bytes.Repeat([]byte("a"), chunkSize), wantPatches: 1},
		{name: "one chunk plus tail", body: bytes.Repeat([]byte("b"), chunkSize+3), wantPatches: 2},
		{name: "exact multiple of chunk", body: bytes.Repeat([]byte("c"), chunkSize*3), wantPatches: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fr := newFakeRegistry()
			t.Cleanup(fr.close)

			p := newPusherFor(t, fr, "repo/x", chunkSize)
			ctx := t.Context()

			desc, err := p.Push(ctx, "application/octet-stream", "", bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("Push() error = %v", err)
			}

			wantDigest := sha256Digest(tc.body)
			if desc.Digest != wantDigest {
				t.Errorf("desc.Digest = %s, want %s", desc.Digest, wantDigest)
			}
			if desc.Size != int64(len(tc.body)) {
				t.Errorf("desc.Size = %d, want %d", desc.Size, len(tc.body))
			}
			if got, ok := fr.commits[wantDigest]; !ok {
				t.Errorf("registry never committed digest %s; commits=%v", wantDigest, fr.commits)
			} else if !bytes.Equal(tc.body, got) {
				t.Errorf("committed body mismatch:\n got=%q\nwant=%q", got, tc.body)
			}

			// Wire-protocol assertions: 1 POST + N PATCH + 1 PUT.
			gotMethods := methodSeq(fr.requests)
			wantMethods := []string{"POST"}
			for range tc.wantPatches {
				wantMethods = append(wantMethods, "PATCH")
			}
			wantMethods = append(wantMethods, "PUT")
			if diff := cmp.Diff(wantMethods, gotMethods); diff != "" {
				t.Errorf("request sequence mismatch (-want +got):\n%s", diff)
			}

			// PUT must carry digest=<computed> in the query string.
			putReq := fr.requests[len(fr.requests)-1]
			if !strings.Contains(putReq.query, "digest="+url.QueryEscape(string(wantDigest))) {
				t.Errorf("PUT query = %q, want digest=%s", putReq.query, wantDigest)
			}
			// PUT body must be empty — all content went via PATCHes.
			if putReq.bodyLen != 0 {
				t.Errorf("PUT body length = %d, want 0 (chunked path commits with empty body)", putReq.bodyLen)
			}

			// Content-Range math: the i-th PATCH carries
			// "i*chunkSize - (i*chunkSize + len-1)".
			offset := int64(0)
			patchIdx := 0
			for _, req := range fr.requests {
				if req.method != "PATCH" {
					continue
				}
				patchIdx++
				wantStart := offset
				wantEnd := offset + int64(req.bodyLen) - 1
				wantRange := fmt.Sprintf("%d-%d", wantStart, wantEnd)
				if req.contentRange != wantRange {
					t.Errorf("PATCH %d Content-Range = %q, want %q", patchIdx, req.contentRange, wantRange)
				}
				offset += int64(req.bodyLen)
			}
		})
	}
}

// TestStreamPusher_DigestMismatch verifies that a caller-supplied digest
// that disagrees with the streamed bytes triggers a DELETE on the upload
// session and surfaces ErrDigestMismatch — no blob ever lands in the
// registry's committed map.
func TestStreamPusher_DigestMismatch(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistry()
	t.Cleanup(fr.close)

	p := newPusherFor(t, fr, "repo/x", 8)
	body := bytes.Repeat([]byte("x"), 32)
	wrongDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	_, err := p.Push(t.Context(), "application/octet-stream", wrongDigest, bytes.NewReader(body))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Push() error = %v, want errors.Is ErrDigestMismatch", err)
	}
	if len(fr.commits) != 0 {
		t.Errorf("registry committed %d blobs after digest mismatch, want 0", len(fr.commits))
	}
	// Last request must be DELETE — we abort before PUT-commit when we
	// detect the mismatch, otherwise the registry would never see a PUT
	// and there'd be nothing to assert. Confirm DELETE was sent.
	lastMethod := fr.requests[len(fr.requests)-1].method
	if lastMethod != "DELETE" {
		t.Errorf("last request method = %q, want DELETE", lastMethod)
	}
}

// TestStreamPusher_PatchFailure verifies that a registry-side 5xx on a
// mid-upload PATCH causes streamPusher to abort with DELETE and return a
// wrapped error. v1 policy is restart-on-retry, not resume; the abort
// makes that explicit.
func TestStreamPusher_PatchFailure(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistry()
	fr.failNthPatch = 2
	fr.failPatchStatus = http.StatusInternalServerError
	t.Cleanup(fr.close)

	p := newPusherFor(t, fr, "repo/x", 8)
	body := bytes.Repeat([]byte("y"), 24) // 3 chunks of 8

	_, err := p.Push(t.Context(), "application/octet-stream", "", bytes.NewReader(body))
	if err == nil {
		t.Fatalf("Push() error = nil, want non-nil")
	}
	if diff := testutil.DiffErrString(err, "failed to PATCH chunk"); diff != "" {
		t.Errorf("error wrap mismatch: %s", diff)
	}
	// Confirm we surfaced the registry's structured error.
	var ec *errcode.ErrorResponse
	if !errors.As(err, &ec) {
		t.Errorf("error %v does not unwrap to *errcode.ErrorResponse", err)
	} else if ec.StatusCode != http.StatusInternalServerError {
		t.Errorf("ec.StatusCode = %d, want %d", ec.StatusCode, http.StatusInternalServerError)
	}

	// No blob committed; abort DELETE was sent.
	if len(fr.commits) != 0 {
		t.Errorf("registry committed %d blobs on PATCH failure, want 0", len(fr.commits))
	}
	lastMethod := fr.requests[len(fr.requests)-1].method
	if lastMethod != "DELETE" {
		t.Errorf("last request method = %q, want DELETE", lastMethod)
	}
}

// TestStreamPusher_ReaderError covers the "io.Reader returns an error
// mid-stream" path: the upload session must be DELETEd and the underlying
// error wrapped, not silently swallowed.
func TestStreamPusher_ReaderError(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistry()
	t.Cleanup(fr.close)

	p := newPusherFor(t, fr, "repo/x", 4)

	body := &errAfterReader{
		Reader:    bytes.NewReader(bytes.Repeat([]byte("z"), 6)),
		breakAt:   5,
		breakWith: errors.New("network gremlin"),
	}
	_, err := p.Push(t.Context(), "application/octet-stream", "", body)
	if err == nil {
		t.Fatalf("Push() error = nil, want non-nil")
	}
	if diff := testutil.DiffErrString(err, "network gremlin"); diff != "" {
		t.Errorf("error wrap mismatch: %s", diff)
	}
	if len(fr.commits) != 0 {
		t.Errorf("registry committed %d blobs on reader error, want 0", len(fr.commits))
	}
}

// errAfterReader wraps an io.Reader and returns a fixed error after
// breakAt bytes have been delivered to the caller. Used to simulate a
// torn upload body.
type errAfterReader struct {
	io.Reader
	delivered int
	breakAt   int
	breakWith error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.delivered >= e.breakAt {
		return 0, e.breakWith
	}
	max := e.breakAt - e.delivered
	if len(p) > max {
		p = p[:max]
	}
	n, err := e.Reader.Read(p)
	e.delivered += n
	if err != nil {
		return n, err
	}
	return n, nil
}

func methodSeq(reqs []recordedReq) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.method
	}
	return out
}

// TestStreamPusher_NewStreamPusherFromRegistry exercises the production
// factory wired up in NewRegistry — that the constructed pusher targets
// the right repository URL and reuses the registry's auth setup. We point
// it at the fake registry and verify a round-trip works.
func TestStreamPusher_NewStreamPusherFromRegistry(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistry()
	t.Cleanup(fr.close)

	r, err := NewRegistry(fr.baseURL())
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	r.uploadMemThreshold = 4

	pusher, err := r.newStreamPusher(t.Context(), &RepoFile{OwningRepo: "demo"})
	if err != nil {
		t.Fatalf("newStreamPusher() error = %v", err)
	}
	sp, ok := pusher.(*streamPusher)
	if !ok {
		t.Fatalf("newStreamPusher() returned %T, want *streamPusher", pusher)
	}
	// Override chunk size so the test body fits in the test registry's
	// chunk math. plainHTTP is already wired via NewRegistry from the
	// http:// httptest URL.
	sp.chunkSize = 8

	body := []byte("hello, streaming world")
	desc, err := sp.Push(t.Context(), "application/octet-stream", "", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	wantDigest := sha256Digest(body)
	if desc.Digest != wantDigest {
		t.Errorf("desc.Digest = %s, want %s", desc.Digest, wantDigest)
	}
	if got, ok := fr.commits[wantDigest]; !ok {
		t.Errorf("registry never committed digest %s", wantDigest)
	} else if !bytes.Equal(body, got) {
		t.Errorf("committed body = %q, want %q", got, body)
	}
}

// TestStreamPusher_NewStreamPusherInvalidRef ensures the factory errors
// out cleanly when the configured repo name is invalid.
func TestStreamPusher_NewStreamPusherInvalidRef(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(&url.URL{Scheme: "https", Host: "example.com"})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	_, err = r.newStreamPusher(t.Context(), &RepoFile{OwningRepo: "Has Spaces!"})
	if err == nil {
		t.Errorf("newStreamPusher() error = nil, want non-nil for invalid repo name")
	}
}
