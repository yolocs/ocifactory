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
	"testing"

	"github.com/google/go-cmp/cmp"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

func TestUpsertFileLayer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		existingLayers []ocispec.Descriptor
		newFileDesc    ocispec.Descriptor
		wantUpdated    bool
		wantLayers     []ocispec.Descriptor
	}{
		{
			name:           "add new file",
			existingLayers: []ocispec.Descriptor{},
			newFileDesc: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    "sha256:123",
				Size:      100,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
			wantUpdated: true,
			wantLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
		},
		{
			name: "update existing file with different digest",
			existingLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
			newFileDesc: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    "sha256:456",
				Size:      200,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
			wantUpdated: true,
			wantLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:456",
					Size:      200,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
		},
		{
			name: "no update for same digest",
			existingLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
			newFileDesc: ocispec.Descriptor{
				MediaType: "text/plain",
				Digest:    "sha256:123",
				Size:      100,
				Annotations: map[string]string{
					FileNameAnnotation: "test.txt",
				},
			},
			wantUpdated: false,
			wantLayers: []ocispec.Descriptor{
				{
					MediaType: "text/plain",
					Digest:    "sha256:123",
					Size:      100,
					Annotations: map[string]string{
						FileNameAnnotation: "test.txt",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotUpdated, gotLayers := upsertFileLayer(tt.existingLayers, tt.newFileDesc)
			if gotUpdated != tt.wantUpdated {
				t.Errorf("upsertFileLayer() updated = %v, want %v", gotUpdated, tt.wantUpdated)
			}

			if diff := cmp.Diff(tt.wantLayers, gotLayers); diff != "" {
				t.Errorf("upsertFileLayer() layers mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

type inMemoryRepo struct {
	*memory.Store
	allTags map[string]string
}

func (r *inMemoryRepo) Tags(_ context.Context, _ string, fn func(tags []string) error) error {
	return fn(slices.Collect(maps.Keys(r.allTags)))
}

func (r *inMemoryRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	r.allTags[reference] = desc.Digest.String()
	return r.Store.Tag(ctx, desc, reference)
}

func (r *inMemoryRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	for tag, digest := range r.allTags {
		if digest == target.Digest.String() {
			delete(r.allTags, tag)
		}
	}
	return nil
}

// Intercept the Resolve call to return ErrNotFound if the target has been deleted.
func (r *inMemoryRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	target, err := r.Store.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	if _, ok := r.allTags[reference]; !ok {
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
	memRepo := &inMemoryRepo{Store: memory.New(), allTags: map[string]string{"v0": "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"}}
	r.newBackendFunc = func(ctx context.Context, f *RepoFile) (destRepo, error) {
		return memRepo, nil
	}

	f0 := &RepoFile{
		OwningRepo: "foobar",
		OwningTag:  "v0",
		Name:       "test.txt",
		Digest:     "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
	}

	// Read missing file -> ErrNotFound.
	if _, _, err := r.ReadFile(ctx, f0); err == nil {
		t.Errorf("ReadFile() before AddFile: got nil error, want not-found")
	} else if diff := testutil.DiffErrString(err, "not found"); diff != "" {
		t.Errorf("ReadFile() error diff: %s", diff)
	}

	// Add file.
	const content = "hello world"
	wantDesc, err := r.AddFile(ctx, f0, strings.NewReader(content))
	if err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}

	// Read by owning tag.
	if gotDesc, body, err := r.ReadFile(ctx, f0); err != nil {
		t.Errorf("ReadFile() by owning tag: %v", err)
	} else {
		defer body.Close()
		if diff := cmp.Diff(wantDesc, gotDesc); diff != "" {
			t.Errorf("ReadFile() desc mismatch (-want +got):\n%s", diff)
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
		Digest:     "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
	}); err != nil {
		t.Errorf("ReadFile() by ref tag: %v", err)
	} else {
		defer body.Close()
		if diff := cmp.Diff(wantDesc, gotDesc); diff != "" {
			t.Errorf("ReadFile() by ref tag desc mismatch (-want +got):\n%s", diff)
		}
		gotContent, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("ReadAll() unexpected error = %v", err)
		}
		if string(gotContent) != content {
			t.Errorf("ReadAll() content = %q, want %q", string(gotContent), content)
		}
	}

	// List tags.
	gotTags, err := r.ListTags(ctx, "foobar")
	if err != nil {
		t.Errorf("ListTags() error = %v", err)
	}
	if diff := cmp.Diff([]string{"v0"}, gotTags); diff != "" {
		t.Errorf("ListTags() mismatch (-want +got):\n%s", diff)
	}

	// List files.
	gotFiles, err := r.ListFiles(ctx, "foobar")
	if err != nil {
		t.Errorf("ListFiles() error = %v", err)
	}
	if diff := cmp.Diff([]*RepoFile{f0}, gotFiles); diff != "" {
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
			name:    "ref_ owning tag rejected",
			content: smallContent,
			repoFile: &RepoFile{
				OwningRepo: "pkg",
				OwningTag:  "ref_latest",
				Name:       "test.txt",
			},
			wantErrSubstr: "canonical tag cannot be prefixed with ref_",
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

// TestAddFile_StreamingDispatch verifies the size-based fork in AddFile:
//   - bodies at or below uploadMemThreshold take the buffered + monolithic
//     path (countingRepo.Push invoked, fakeStreamPusher.Push not invoked);
//   - bodies above uploadMemThreshold take the streaming path
//     (fakeStreamPusher.Push invoked, no buffered Push of the file blob).
//
// Both paths must end up with the same FileDescriptor digest+size and a
// tagged manifest in the backend.
func TestAddFile_StreamingDispatch(t *testing.T) {
	t.Parallel()

	const threshold int64 = 1024

	tests := []struct {
		name           string
		body           []byte
		wantStreamCall bool
		wantBufferPush bool
	}{
		{
			name:           "body just under threshold goes monolithic",
			body:           bytes.Repeat([]byte("a"), int(threshold)-1),
			wantStreamCall: false,
			wantBufferPush: true,
		},
		{
			name:           "body equal to threshold goes monolithic",
			body:           bytes.Repeat([]byte("b"), int(threshold)),
			wantStreamCall: false,
			wantBufferPush: true,
		},
		{
			name:           "body one over threshold goes streaming",
			body:           bytes.Repeat([]byte("c"), int(threshold)+1),
			wantStreamCall: true,
			wantBufferPush: false,
		},
		{
			name:           "body well over threshold goes streaming",
			body:           bytes.Repeat([]byte("d"), int(threshold)*8+7),
			wantStreamCall: true,
			wantBufferPush: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

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
