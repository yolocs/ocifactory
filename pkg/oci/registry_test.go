package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/cred"
	"github.com/yolocs/ocifactory/pkg/testutil"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

func TestNewRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		baseURL          *url.URL
		opts             []RegistryOption
		wantErr          bool
		wantArtifactType string
	}{
		{
			name:             "default options",
			baseURL:          &url.URL{Scheme: "https", Host: "example.com"},
			opts:             nil,
			wantErr:          false,
			wantArtifactType: DefaultArtifactType,
		},
		{
			name:             "with artifact type",
			baseURL:          &url.URL{Scheme: "https", Host: "example.com"},
			opts:             []RegistryOption{WithArtifactType("application/custom")},
			wantErr:          false,
			wantArtifactType: "application/custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewRegistry(tt.baseURL, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewRegistry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if got.artifactType != tt.wantArtifactType {
				t.Errorf("NewRegistry() artifactType = %v, want %v", got.artifactType, tt.wantArtifactType)
			}

			if diff := cmp.Diff(tt.baseURL, got.baseURL); diff != "" {
				t.Errorf("NewRegistry() baseURL mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDetectFileMediaType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		file     *RepoFile
		expected string
	}{
		{
			name: "with explicit media type",
			file: &RepoFile{
				Name:      "test.bin",
				MediaType: "application/custom",
			},
			expected: "application/custom",
		},
		{
			name: "txt file",
			file: &RepoFile{
				Name: "test.txt",
			},
			expected: "text/plain",
		},
		{
			name: "html file",
			file: &RepoFile{
				Name: "test.html",
			},
			expected: "text/html",
		},
		{
			name: "xml file",
			file: &RepoFile{
				Name: "test.xml",
			},
			expected: "application/xml",
		},
		{
			name: "json file",
			file: &RepoFile{
				Name: "test.json",
			},
			expected: "application/json",
		},
		{
			name: "tar file",
			file: &RepoFile{
				Name: "test.tar",
			},
			expected: "application/x-tar",
		},
		{
			name: "gz file",
			file: &RepoFile{
				Name: "test.gz",
			},
			expected: "application/x-gzip",
		},
		{
			name: "tgz file",
			file: &RepoFile{
				Name: "test.tgz",
			},
			expected: "application/x-gzip",
		},
		{
			name: "zip file",
			file: &RepoFile{
				Name: "test.zip",
			},
			expected: "application/zip",
		},
		{
			name: "unknown extension",
			file: &RepoFile{
				Name: "test.unknown",
			},
			expected: "application/octet-stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := detectFileMediaType(tt.file)
			if got != tt.expected {
				t.Errorf("detectFileMediaType() = %v, want %v", got, tt.expected)
			}
		})
	}
}

type inMemoryRepo struct {
	*memory.Store

	mu      sync.Mutex
	allTags map[string]string
}

func (r *inMemoryRepo) Tags(_ context.Context, _ string, fn func(tags []string) error) error {
	r.mu.Lock()
	tags := slices.Collect(maps.Keys(r.allTags))
	r.mu.Unlock()
	return fn(tags)
}

func (r *inMemoryRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	r.mu.Lock()
	r.allTags[reference] = desc.Digest.String()
	r.mu.Unlock()
	return r.Store.Tag(ctx, desc, reference)
}

func (r *inMemoryRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	r.mu.Lock()
	for tag, digest := range r.allTags {
		if digest == target.Digest.String() {
			delete(r.allTags, tag)
		}
	}
	r.mu.Unlock()
	return nil
}

// Intercept the Resolve call to return ErrNotFound if the target has been deleted.
func (r *inMemoryRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	target, err := r.Store.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	r.mu.Lock()
	_, ok := r.allTags[reference]
	r.mu.Unlock()
	if !ok {
		return ocispec.Descriptor{}, errdef.ErrNotFound
	}
	return target, nil
}

// TestAddReadRoundtrip exercises the full add → read → ref → list → delete
// lifecycle in sequence. It is intentionally a single non-subtest function:
// each phase depends on state established by the previous one (the in-memory
// backend, the recorded wantDesc), so subtests with t.Parallel would race.
func TestAddReadRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	r, err := NewRegistry(
		&url.URL{Scheme: "https", Host: "example.com"},
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	// Override the newBackendFunc to use the memory backend.
	memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{}}
	r.newBackendFunc = func(ctx context.Context, f *RepoFile) (destRepo, error) {
		return memRepo, nil
	}

	const content = "hello world"
	wantBlobDigest := sha256Digest([]byte(content))

	f0 := &RepoFile{
		OwningRepo: "foobar",
		OwningTag:  "v0",
		Name:       "test.txt",
		Digest:     wantBlobDigest.String(),
	}

	// Read missing file -> ErrNotFound.
	if _, _, err := r.ReadFile(ctx, f0); err == nil {
		t.Errorf("ReadFile() before AddFile: got nil error, want not-found")
	} else if diff := testutil.DiffErrString(err, "not found"); diff != "" {
		t.Errorf("ReadFile() error diff: %s", diff)
	}

	// Add file.
	wantDesc, err := r.AddFile(ctx, f0, strings.NewReader(content))
	if err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}
	if wantDesc.File.Digest != wantBlobDigest {
		t.Errorf("AddFile() blob digest = %s, want %s", wantDesc.File.Digest, wantBlobDigest)
	}

	// Read by owning tag.
	if gotDesc, body, err := r.ReadFile(ctx, f0); err != nil {
		t.Errorf("ReadFile() by owning tag: %v", err)
	} else {
		defer body.Close()
		if got, want := gotDesc.File.Digest, wantBlobDigest; got != want {
			t.Errorf("ReadFile() blob digest = %s, want %s", got, want)
		}
		gotContent, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}
		if string(gotContent) != content {
			t.Errorf("ReadAll() content = %q, want %q", string(gotContent), content)
		}
	}

	// Append ref tags.
	if err := r.AppendRefs(ctx, "foobar", "v0", "tag1", "tag2"); err != nil {
		t.Fatalf("AppendRefs() error = %v", err)
	}

	// Read by ref tag.
	if gotDesc, body, err := r.ReadFile(ctx, &RepoFile{
		OwningRepo: "foobar",
		RefTag:     "tag1",
		Name:       "test.txt",
		Digest:     wantBlobDigest.String(),
	}); err != nil {
		t.Errorf("ReadFile() by ref tag: %v", err)
	} else {
		defer body.Close()
		if got, want := gotDesc.File.Digest, wantBlobDigest; got != want {
			t.Errorf("ReadFile() by ref tag blob digest = %s, want %s", got, want)
		}
		gotContent, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}
		if string(gotContent) != content {
			t.Errorf("ReadAll() content = %q, want %q", string(gotContent), content)
		}
	}

	// List tags. Both canonical version tags and alias tags are returned in
	// the new layout — the legacy ref_ filter is gone.
	gotTags, err := r.ListTags(ctx, "foobar")
	if err != nil {
		t.Errorf("ListTags() error = %v", err)
	}
	slices.Sort(gotTags)
	if diff := cmp.Diff([]string{"tag1", "tag2", "v0"}, gotTags); diff != "" {
		t.Errorf("ListTags() mismatch (-want +got):\n%s", diff)
	}

	// List files. The new ListFiles filters to files under canonical
	// version manifests (so aliases don't double-count), and reads the
	// blob digest from the file manifest's annotation.
	wantFile := &RepoFile{
		Name:       "test.txt",
		OwningRepo: "foobar",
		OwningTag:  "v0",
		Digest:     wantBlobDigest.String(),
	}
	gotFiles, err := r.ListFiles(ctx, "foobar")
	if err != nil {
		t.Errorf("ListFiles() error = %v", err)
	}
	if diff := cmp.Diff([]*RepoFile{wantFile}, gotFiles); diff != "" {
		t.Errorf("ListFiles() mismatch (-want +got):\n%s", diff)
	}

	// Delete tag files, then re-list.
	if err := r.DeleteTagFiles(ctx, "foobar", "v0"); err != nil {
		t.Errorf("DeleteTagFiles() error = %v", err)
	}
	gotFiles, err = r.ListFiles(ctx, "foobar")
	if err != nil {
		t.Errorf("ListFiles() after delete: %v", err)
	}
	if len(gotFiles) != 0 {
		t.Errorf("ListFiles() after delete = %v, want empty", gotFiles)
	}
}

// countingRepo wraps inMemoryRepo to record blob/manifest pushes and
// optionally inject a Push error. The recorded counts let tests assert that
// the streaming AddFile path performs a single backend push for the file
// blob (plus the empty config + manifest pushes from oras.PackManifest).
type countingRepo struct {
	*inMemoryRepo

	pushCalls    int            // total Push calls observed
	pushedBytes  map[string]int // digest → length of pushed body
	failOnDigest digest.Digest  // when set, Push for this digest returns failErr
	failErr      error
}

func (r *countingRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	r.pushCalls++
	body, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("countingRepo.Push read body: %w", err)
	}
	if r.pushedBytes == nil {
		r.pushedBytes = map[string]int{}
	}
	r.pushedBytes[expected.Digest.String()] = len(body)
	if r.failErr != nil && expected.Digest == r.failOnDigest {
		return r.failErr
	}
	return r.inMemoryRepo.Store.Push(ctx, expected, bytes.NewReader(body))
}

func sha256Digest(b []byte) digest.Digest {
	sum := sha256.Sum256(b)
	return digest.NewDigestFromBytes(digest.SHA256, sum[:])
}

// TestAddFile_BufferedPath covers the buffered + monolithic upload path
// (small bodies, plus the disable-streaming opt-out spilling to a temp
// file). The streaming chunked-PATCH path is exercised by
// TestAddFile_StreamingDispatch and the streamPusher unit tests.
func TestAddFile_BufferedPath(t *testing.T) {
	t.Parallel()

	// Tiny in-memory threshold so the spill path runs on a few-KB body
	// instead of multi-MB. AddFile's behaviour is identical at any
	// threshold; the production default is exercised by the
	// TestStageUpload table below.
	const testMemThreshold = 1024

	smallContent := []byte("hello world")
	largeContent := bytes.Repeat([]byte("x"), testMemThreshold+128)

	tests := []struct {
		name           string
		content        []byte
		repoFile       *RepoFile
		failPush       bool
		wantErrSubstr  string
		wantTargetErr  error
		wantFileDigest digest.Digest
		wantFileSize   int64
	}{
		{
			name:    "small file under threshold",
			content: smallContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       "test.txt",
			},
			wantFileDigest: sha256Digest(smallContent),
			wantFileSize:   int64(len(smallContent)),
		},
		{
			name:    "large file spills to temp file",
			content: largeContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       "big.bin",
			},
			wantFileDigest: sha256Digest(largeContent),
			wantFileSize:   int64(len(largeContent)),
		},
		{
			name:    "supplied digest matches",
			content: smallContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       "test.txt",
				Digest:     string(sha256Digest(smallContent)),
			},
			wantFileDigest: sha256Digest(smallContent),
			wantFileSize:   int64(len(smallContent)),
		},
		{
			name:    "supplied digest mismatch returns ErrDigestMismatch",
			content: smallContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       "test.txt",
				Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
			wantErrSubstr: "file digest mismatch",
			wantTargetErr: ErrDigestMismatch,
		},
		{
			name:    "backend Push failure is wrapped and skips manifest update",
			content: smallContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       "test.txt",
			},
			failPush:      true,
			wantErrSubstr: "failed to push file blob",
		},
		{
			name:    "missing OwningTag rejected",
			content: smallContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				Name:       "test.txt",
			},
			wantErrSubstr: "OwningTag must be set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{}}
			counting := &countingRepo{inMemoryRepo: memRepo}
			if tc.failPush {
				counting.failOnDigest = sha256Digest(tc.content)
				counting.failErr = errors.New("backend down")
			}

			r, err := NewRegistry(&url.URL{Scheme: "https", Host: "example.com"})
			if err != nil {
				t.Fatalf("NewRegistry() error = %v", err)
			}
			r.uploadMemThreshold = testMemThreshold
			// Pin the buffered + monolithic path so this test exercises
			// the temp-file spill on largeContent. Streaming dispatch is
			// covered separately.
			r.disableStreamingPush = true
			r.newBackendFunc = func(_ context.Context, _ *RepoFile) (destRepo, error) {
				return counting, nil
			}

			desc, err := r.AddFile(ctx, tc.repoFile, bytes.NewReader(tc.content))
			if tc.wantErrSubstr != "" {
				if diff := testutil.DiffErrString(err, tc.wantErrSubstr); diff != "" {
					t.Fatalf("AddFile() error: %s", diff)
				}
				if tc.wantTargetErr != nil && !errors.Is(err, tc.wantTargetErr) {
					t.Fatalf("AddFile() errors.Is(%v, %v) = false", err, tc.wantTargetErr)
				}
				// On any error (validation, mismatch, push failure) the
				// manifest must not have been tagged.
				if _, ok := memRepo.allTags[tc.repoFile.OwningTag]; ok {
					t.Errorf("AddFile() tagged manifest despite error")
				}
				return
			}
			if err != nil {
				t.Fatalf("AddFile() unexpected error = %v", err)
			}

			if got, want := desc.File.Digest, tc.wantFileDigest; got != want {
				t.Errorf("AddFile() file digest = %s, want %s", got, want)
			}
			if got, want := desc.File.Size, tc.wantFileSize; got != want {
				t.Errorf("AddFile() file size = %d, want %d", got, want)
			}

			// File blob should have been pushed exactly once with the full body.
			gotBytes, ok := counting.pushedBytes[string(tc.wantFileDigest)]
			if !ok {
				t.Fatalf("AddFile() did not push file blob with digest %s; pushed = %v", tc.wantFileDigest, counting.pushedBytes)
			}
			if int64(gotBytes) != tc.wantFileSize {
				t.Errorf("AddFile() pushed blob size = %d, want %d", gotBytes, tc.wantFileSize)
			}

			// Re-adding the same content must not re-push the blob: the
			// Exists check should short-circuit it, and the unchanged
			// manifest path should skip the manifest re-pack as well.
			pushesAfterFirst := counting.pushCalls
			if _, err := r.AddFile(ctx, tc.repoFile, bytes.NewReader(tc.content)); err != nil {
				t.Fatalf("AddFile() second call error = %v", err)
			}
			if counting.pushCalls != pushesAfterFirst {
				t.Errorf("AddFile() second call pushed %d more times, want 0", counting.pushCalls-pushesAfterFirst)
			}
		})
	}
}

func TestStageUploadWithThreshold(t *testing.T) {
	t.Parallel()

	const threshold int64 = 1024

	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty body", body: []byte{}},
		{name: "small body", body: []byte("hello, world")},
		{name: "exactly threshold", body: bytes.Repeat([]byte("a"), int(threshold))},
		{name: "one over threshold", body: bytes.Repeat([]byte("b"), int(threshold)+1)},
		{name: "well over threshold", body: bytes.Repeat([]byte("c"), int(threshold)*4+17)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			staged, err := stageUploadWithThreshold(bytes.NewReader(tc.body), threshold)
			if err != nil {
				t.Fatalf("stageUploadWithThreshold() error = %v", err)
			}
			t.Cleanup(staged.cleanup)

			if got, want := staged.size, int64(len(tc.body)); got != want {
				t.Errorf("size = %d, want %d", got, want)
			}
			if got, want := staged.digest, sha256Digest(tc.body); got != want {
				t.Errorf("digest = %s, want %s", got, want)
			}

			rd, err := staged.reader()
			if err != nil {
				t.Fatalf("reader() error = %v", err)
			}
			gotBody, err := io.ReadAll(rd)
			if err != nil {
				t.Fatalf("ReadAll(reader) error = %v", err)
			}
			if diff := cmp.Diff(tc.body, gotBody); diff != "" {
				t.Errorf("staged body mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// fakeStreamPusher is a streamingPusher stub used by AddFile dispatch tests
// to assert that the streaming path was (or was not) taken without spinning
// up an HTTP server. Push records the body it received and computes the
// digest so the caller can verify it.
type fakeStreamPusher struct {
	calls       int
	receivedLen int64
	receivedSHA digest.Digest
	pushErr     error
}

func (p *fakeStreamPusher) Push(_ context.Context, mediaType, expectedDigest string, content io.Reader) (ocispec.Descriptor, error) {
	p.calls++
	body, err := io.ReadAll(content)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	p.receivedLen = int64(len(body))
	p.receivedSHA = sha256Digest(body)
	if p.pushErr != nil {
		return ocispec.Descriptor{}, p.pushErr
	}
	if expectedDigest != "" && string(p.receivedSHA) != expectedDigest {
		return ocispec.Descriptor{}, fmt.Errorf("%w: %q != %q", ErrDigestMismatch, p.receivedSHA, expectedDigest)
	}
	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    p.receivedSHA,
		Size:      p.receivedLen,
	}, nil
}

// TestAddFile_StreamingDispatch verifies the path fork in AddFile across
// all three dispatch signals:
//   - RepoFile.Size > 0 short-circuits the peek and dispatches directly;
//   - Size == 0 (unknown) falls back to the peek-and-decide buffer;
//   - WithStreamingPushDisabled forces the buffered path regardless of
//     body size.
//
// Both paths must end up with the same FileDescriptor digest+size and a
// tagged manifest in the backend.
func TestAddFile_StreamingDispatch(t *testing.T) {
	t.Parallel()

	const threshold int64 = 1024

	tests := []struct {
		name             string
		body             []byte
		size             int64 // RepoFile.Size; 0 == unknown / peek path
		disableStreaming bool
		wantStreamCall   bool
		wantBufferPush   bool
	}{
		{
			name:           "unknown size, body under threshold, peek then buffered",
			body:           bytes.Repeat([]byte("a"), int(threshold)-1),
			wantStreamCall: false,
			wantBufferPush: true,
		},
		{
			name:           "unknown size, body equal to threshold, peek then buffered",
			body:           bytes.Repeat([]byte("b"), int(threshold)),
			wantStreamCall: false,
			wantBufferPush: true,
		},
		{
			name:           "unknown size, body one over threshold, peek then streaming",
			body:           bytes.Repeat([]byte("c"), int(threshold)+1),
			wantStreamCall: true,
			wantBufferPush: false,
		},
		{
			name:           "known size under threshold short-circuits to buffered",
			body:           bytes.Repeat([]byte("d"), int(threshold)/2),
			size:           threshold / 2,
			wantStreamCall: false,
			wantBufferPush: true,
		},
		{
			name:           "known size over threshold short-circuits to streaming",
			body:           bytes.Repeat([]byte("e"), int(threshold)*4+3),
			size:           threshold*4 + 3,
			wantStreamCall: true,
			wantBufferPush: false,
		},
		{
			name:             "streaming disabled forces buffered path even for large body",
			body:             bytes.Repeat([]byte("f"), int(threshold)*8+11),
			disableStreaming: true,
			wantStreamCall:   false,
			wantBufferPush:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{}}
			counting := &countingRepo{inMemoryRepo: memRepo}
			pusher := &fakeStreamPusher{}

			opts := []RegistryOption{}
			if tc.disableStreaming {
				opts = append(opts, WithStreamingPushDisabled(true))
			}
			r, err := NewRegistry(&url.URL{Scheme: "https", Host: "example.com"}, opts...)
			if err != nil {
				t.Fatalf("NewRegistry() error = %v", err)
			}
			r.uploadMemThreshold = threshold
			r.newBackendFunc = func(_ context.Context, _ *RepoFile) (destRepo, error) {
				return counting, nil
			}
			r.newStreamPusherFunc = func(_ context.Context, _ *RepoFile) (streamingPusher, error) {
				return pusher, nil
			}

			f := &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       "blob.bin",
				Size:       tc.size,
			}

			desc, err := r.AddFile(ctx, f, bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("AddFile() error = %v", err)
			}

			wantDigest := sha256Digest(tc.body)
			if desc.File.Digest != wantDigest {
				t.Errorf("File.Digest = %s, want %s", desc.File.Digest, wantDigest)
			}
			if desc.File.Size != int64(len(tc.body)) {
				t.Errorf("File.Size = %d, want %d", desc.File.Size, len(tc.body))
			}

			if got, want := pusher.calls > 0, tc.wantStreamCall; got != want {
				t.Errorf("streaming pusher invoked = %v (calls=%d), want %v", got, pusher.calls, want)
			}
			_, bufferPushed := counting.pushedBytes[wantDigest.String()]
			if got, want := bufferPushed, tc.wantBufferPush; got != want {
				t.Errorf("buffered backend Push of file blob = %v, want %v (pushed=%v)", got, want, counting.pushedBytes)
			}

			if _, ok := memRepo.allTags[f.OwningTag]; !ok {
				t.Errorf("AddFile() did not tag manifest under %q", f.OwningTag)
			}
		})
	}
}

// TestAddFile_StreamingDigestMismatch verifies the streaming path surfaces
// ErrDigestMismatch when RepoFile.Digest disagrees with the streamed body,
// without leaving a tagged manifest behind.
func TestAddFile_StreamingDigestMismatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const threshold int64 = 64
	body := bytes.Repeat([]byte("z"), int(threshold)*4)

	memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{}}
	counting := &countingRepo{inMemoryRepo: memRepo}
	pusher := &fakeStreamPusher{}

	r, err := NewRegistry(&url.URL{Scheme: "https", Host: "example.com"})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	r.uploadMemThreshold = threshold
	r.newBackendFunc = func(_ context.Context, _ *RepoFile) (destRepo, error) {
		return counting, nil
	}
	r.newStreamPusherFunc = func(_ context.Context, _ *RepoFile) (streamingPusher, error) {
		return pusher, nil
	}

	f := &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v1",
		Name:       "blob.bin",
		Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
	if _, err := r.AddFile(ctx, f, bytes.NewReader(body)); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("AddFile() error = %v, want errors.Is ErrDigestMismatch", err)
	}
	if pusher.calls == 0 {
		t.Errorf("streaming pusher never called, want exactly one call")
	}
	if _, ok := memRepo.allTags[f.OwningTag]; ok {
		t.Errorf("AddFile() tagged manifest despite digest mismatch")
	}
}

// TestBufferUploadHead covers the peek-and-decide helper that AddFile uses
// to pick between the buffered and streaming paths.
func TestBufferUploadHead(t *testing.T) {
	t.Parallel()

	const threshold int64 = 16

	tests := []struct {
		name     string
		body     []byte
		wantFull bool
		wantHead int
	}{
		{name: "empty body", body: []byte{}, wantFull: true, wantHead: 0},
		{name: "well under threshold", body: []byte("hi"), wantFull: true, wantHead: 2},
		{name: "exactly threshold", body: bytes.Repeat([]byte("a"), int(threshold)), wantFull: true, wantHead: int(threshold)},
		{name: "one over threshold", body: bytes.Repeat([]byte("b"), int(threshold)+1), wantFull: false, wantHead: int(threshold) + 1},
		{name: "well over threshold", body: bytes.Repeat([]byte("c"), int(threshold)*4), wantFull: false, wantHead: int(threshold) + 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := bytes.NewReader(tc.body)
			head, full, err := bufferUploadHead(r, threshold)
			if err != nil {
				t.Fatalf("bufferUploadHead() error = %v", err)
			}
			if full != tc.wantFull {
				t.Errorf("full = %v, want %v", full, tc.wantFull)
			}
			if len(head) != tc.wantHead {
				t.Errorf("len(head) = %d, want %d", len(head), tc.wantHead)
			}

			// What's left in r combined with head must equal the body in
			// order — that's the contract the streaming path relies on.
			rest, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll(rest) error = %v", err)
			}
			combined := append(append([]byte{}, head...), rest...)
			if diff := cmp.Diff(tc.body, combined); diff != "" {
				t.Errorf("head+rest mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAuthClientMemoization locks in the per-credentials caching behaviour
// in authClientFromContext: two calls with the same basic-auth context
// must return the same *auth.Client (so the per-client token cache is
// shared across PATCHes and across the pusher/backend split inside one
// AddFile call), and calls with different credentials must NOT share.
//
// The bearer-token re-fetch storm this guards against was the #1 finding
// from the pre-merge memory-leak audit; do not delete this test without
// a replacement.
func TestAuthClientMemoization(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(&url.URL{Scheme: "https", Host: "example.com"})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	ctx1 := cred.WithCred(t.Context(), &cred.Cred{
		Basic: &cred.BasicCred{User: "alice", Password: "p1"},
	})
	ctx2 := cred.WithCred(t.Context(), &cred.Cred{
		Basic: &cred.BasicCred{User: "alice", Password: "p1"},
	})
	ctx3 := cred.WithCred(t.Context(), &cred.Cred{
		Basic: &cred.BasicCred{User: "bob", Password: "p2"},
	})

	c1 := r.authClientFromContext(ctx1)
	c2 := r.authClientFromContext(ctx2)
	c3 := r.authClientFromContext(ctx3)

	if c1 == nil || c2 == nil || c3 == nil {
		t.Fatalf("authClientFromContext returned nil for credentialled contexts: %v %v %v", c1, c2, c3)
	}
	if c1 != c2 {
		t.Errorf("same credentials produced different clients: %p vs %p", c1, c2)
	}
	if c1 == c3 {
		t.Errorf("different credentials produced the same client: %p", c1)
	}
	if c1.Cache == nil {
		t.Errorf("auth.Client.Cache must be set so token fetches are amortised across PATCHes; got nil")
	}

	// No credentials -> nil client (auth-free request path).
	if got := r.authClientFromContext(t.Context()); got != nil {
		t.Errorf("expected nil client for no-credentials context, got %v", got)
	}
}

// newTestRegistry builds a Registry whose backend is a fresh in-memory
// store. Returned together with the backing store so tests can assert on
// post-conditions directly.
func newTestRegistry(t *testing.T) (*Registry, *inMemoryRepo) {
	t.Helper()
	r, err := NewRegistry(&url.URL{Scheme: "https", Host: "example.com"})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{}}
	r.newBackendFunc = func(_ context.Context, _ *RepoFile) (destRepo, error) {
		return memRepo, nil
	}
	return r, memRepo
}

// TestAddFile_ConcurrentSameVersion proves the cross-request race in #26
// is fixed: N goroutines uploading distinct files into the same OwningTag
// must all succeed and all files must be readable afterwards. Under the
// pre-redesign aggregated-manifest layout this test was deterministic to
// fail because of the un-CAS'd tag write — the loser dropped its layer.
func TestAddFile_ConcurrentSameVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	r, _ := newTestRegistry(t)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("content-%d", i)
			f := &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "v1",
				Name:       fmt.Sprintf("file-%d.txt", i),
			}
			if _, err := r.AddFile(ctx, f, strings.NewReader(content)); err != nil {
				errs[i] = err
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d AddFile() error = %v", i, err)
		}
	}

	files, err := r.ListFiles(ctx, "pkg")
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if got, want := len(files), goroutines; got != want {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Name
		}
		t.Fatalf("ListFiles() = %d files, want %d (got: %v)", got, want, names)
	}

	// Every file must round-trip via ReadFile too — proves the file
	// manifests aren't just listed but actually fetchable end-to-end.
	for i := 0; i < goroutines; i++ {
		f := &RepoFile{
			OwningRepo: "pkg",
			OwningTag:  "v1",
			Name:       fmt.Sprintf("file-%d.txt", i),
		}
		_, body, err := r.ReadFile(ctx, f)
		if err != nil {
			t.Errorf("ReadFile(%s) error = %v", f.Name, err)
			continue
		}
		got, _ := io.ReadAll(body)
		body.Close()
		if want := fmt.Sprintf("content-%d", i); string(got) != want {
			t.Errorf("ReadFile(%s) = %q, want %q", f.Name, got, want)
		}
	}
}

// TestAppendRefs_CollidesWithVersion verifies the alias-vs-version
// namespace check on the AppendRefs side: pointing an alias at a name
// that's already a canonical version tag is refused with
// ErrAliasCollision rather than silently overwriting the version.
func TestAppendRefs_CollidesWithVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	r, _ := newTestRegistry(t)

	if _, err := r.AddFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v1",
		Name:       "a.txt",
	}, strings.NewReader("a")); err != nil {
		t.Fatalf("AddFile(v1) error = %v", err)
	}
	if _, err := r.AddFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v2",
		Name:       "b.txt",
	}, strings.NewReader("b")); err != nil {
		t.Fatalf("AddFile(v2) error = %v", err)
	}

	// "v2" is already a canonical version — cannot be repurposed as an
	// alias of v1.
	err := r.AppendRefs(ctx, "pkg", "v1", "v2")
	if !errors.Is(err, ErrAliasCollision) {
		t.Fatalf("AppendRefs(collide) error = %v, want errors.Is ErrAliasCollision", err)
	}
}

// TestAddFile_CollidesWithAlias verifies the inverse check on the
// AddFile side: pushing a canonical version under a name that's already
// an alias is refused with ErrAliasCollision.
func TestAddFile_CollidesWithAlias(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	r, _ := newTestRegistry(t)

	if _, err := r.AddFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v1",
		Name:       "a.txt",
	}, strings.NewReader("a")); err != nil {
		t.Fatalf("AddFile(v1) error = %v", err)
	}
	if err := r.AppendRefs(ctx, "pkg", "v1", "latest"); err != nil {
		t.Fatalf("AppendRefs(latest) error = %v", err)
	}

	// "latest" is now an alias — adding a file whose OwningTag is "latest"
	// must refuse rather than clobber the alias.
	_, err := r.AddFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "latest",
		Name:       "x.txt",
	}, strings.NewReader("x"))
	if !errors.Is(err, ErrAliasCollision) {
		t.Fatalf("AddFile(latest) error = %v, want errors.Is ErrAliasCollision", err)
	}
}

// TestAppendRefs_RepointReapsOldAlias verifies that re-pointing an
// existing alias to a new canonical version replaces the alias manifest
// and best-effort deletes the previous one. Without the cleanup, every
// re-point would leave behind an orphaned alias manifest still
// discoverable via the old version's referrers list.
func TestAppendRefs_RepointReapsOldAlias(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	r, memRepo := newTestRegistry(t)

	if _, err := r.AddFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v1",
		Name:       "a.txt",
	}, strings.NewReader("a")); err != nil {
		t.Fatalf("AddFile(v1) error = %v", err)
	}
	if _, err := r.AddFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		OwningTag:  "v2",
		Name:       "b.txt",
	}, strings.NewReader("b")); err != nil {
		t.Fatalf("AddFile(v2) error = %v", err)
	}

	// First point latest -> v1, capture the alias manifest digest.
	if err := r.AppendRefs(ctx, "pkg", "v1", "latest"); err != nil {
		t.Fatalf("AppendRefs(latest -> v1) error = %v", err)
	}
	memRepo.mu.Lock()
	v1AliasDigest := memRepo.allTags["latest"]
	memRepo.mu.Unlock()
	if v1AliasDigest == "" {
		t.Fatalf("latest tag missing after first AppendRefs")
	}

	// Re-point latest -> v2.
	if err := r.AppendRefs(ctx, "pkg", "v2", "latest"); err != nil {
		t.Fatalf("AppendRefs(latest -> v2) error = %v", err)
	}
	memRepo.mu.Lock()
	v2AliasDigest := memRepo.allTags["latest"]
	memRepo.mu.Unlock()
	if v2AliasDigest == "" {
		t.Fatalf("latest tag missing after re-point")
	}
	if v1AliasDigest == v2AliasDigest {
		t.Fatalf("alias manifest digest unchanged after re-point: %s", v1AliasDigest)
	}

	// Verify ReadFile via "latest" now yields v2's file.
	_, body, err := r.ReadFile(ctx, &RepoFile{
		OwningRepo: "pkg",
		RefTag:     "latest",
		Name:       "b.txt",
	})
	if err != nil {
		t.Fatalf("ReadFile(latest, b.txt) error = %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if string(got) != "b" {
		t.Errorf("ReadFile(latest, b.txt) = %q, want %q", got, "b")
	}
}
