package oci

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/metrics"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// fakeRecorder captures every Recorder call so tests can assert on the
// observed labels without standing up Prometheus.
type fakeRecorder struct {
	mu      sync.Mutex
	backend []backendObs
	http    []httpObs
}

type backendObs struct {
	op       string
	status   string
	duration time.Duration
}

type httpObs struct {
	format          string
	op              string
	status          string
	duration        time.Duration
	bytesIn, bytesO int64
}

func (f *fakeRecorder) HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.http = append(f.http, httpObs{format, op, status, duration, bytesIn, bytesOut})
}

func (f *fakeRecorder) OCIBackendCall(op, status string, duration time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backend = append(f.backend, backendObs{op, status, duration})
}

func (f *fakeRecorder) backendOpStatuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.backend))
	for _, o := range f.backend {
		out = append(out, o.op+":"+o.status)
	}
	return out
}

// stubRepo lets us pre-program return values per method so we can
// exercise the wrapper's classification logic against synthetic
// successes and failures.
type stubRepo struct {
	pushErr    error
	fetchErr   error
	existsErr  error
	resolveErr error
	tagErr     error
	deleteErr  error
	tagsErr    error
	predErr    error
}

func (s *stubRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	if content != nil {
		_, _ = io.Copy(io.Discard, content)
	}
	return s.pushErr
}
func (s *stubRepo) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	return io.NopCloser(nil), nil
}
func (s *stubRepo) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	return false, s.existsErr
}
func (s *stubRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, s.resolveErr
}
func (s *stubRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	return s.tagErr
}
func (s *stubRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	return s.deleteErr
}
func (s *stubRepo) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	return s.tagsErr
}
func (s *stubRepo) Predecessors(ctx context.Context, node ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	return nil, s.predErr
}

func TestInstrumentedRepo_LabelsByMediaTypeAndError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		exercise func(repo destRepo)
		want     []string
	}{
		{
			name: "push blob ok",
			exercise: func(repo destRepo) {
				_ = repo.Push(t.Context(), ocispec.Descriptor{MediaType: "application/octet-stream"}, nil)
			},
			want: []string{"push_blob:ok"},
		},
		{
			name: "push manifest ok",
			exercise: func(repo destRepo) {
				_ = repo.Push(t.Context(), ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest}, nil)
			},
			want: []string{"push_manifest:ok"},
		},
		{
			name: "fetch manifest ok",
			exercise: func(repo destRepo) {
				_, _ = repo.Fetch(t.Context(), ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest})
			},
			want: []string{"fetch_manifest:ok"},
		},
		{
			name: "fetch blob ok",
			exercise: func(repo destRepo) {
				_, _ = repo.Fetch(t.Context(), ocispec.Descriptor{MediaType: "application/octet-stream"})
			},
			want: []string{"fetch_blob:ok"},
		},
		{
			name: "exists, resolve, tag, delete, list_tags, list_referrers",
			exercise: func(repo destRepo) {
				_, _ = repo.Exists(t.Context(), ocispec.Descriptor{})
				_, _ = repo.Resolve(t.Context(), "tag")
				_ = repo.Tag(t.Context(), ocispec.Descriptor{}, "tag")
				_ = repo.Delete(t.Context(), ocispec.Descriptor{})
				_ = repo.(interface {
					Tags(ctx context.Context, last string, fn func(tags []string) error) error
				}).Tags(t.Context(), "", func([]string) error { return nil })
				_, _ = repo.(interface {
					Predecessors(ctx context.Context, node ocispec.Descriptor) ([]ocispec.Descriptor, error)
				}).Predecessors(t.Context(), ocispec.Descriptor{})
			},
			want: []string{
				"exists:ok",
				"resolve:ok",
				"tag:ok",
				"delete:ok",
				"list_tags:ok",
				"list_referrers:ok",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &fakeRecorder{}
			repo := newInstrumentedRepo(&stubRepo{}, rec)
			tc.exercise(repo)
			if diff := cmp.Diff(tc.want, rec.backendOpStatuses()); diff != "" {
				t.Errorf("backend op:status mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInstrumentedRepo_StatusFromErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil ok", err: nil, want: metrics.StatusOK},
		{name: "not found", err: errdef.ErrNotFound, want: "not_found"},
		{name: "already exists", err: errdef.ErrAlreadyExists, want: "already_exists"},
		{name: "errcode 503", err: &errcode.ErrorResponse{StatusCode: 503}, want: "503"},
		{name: "generic", err: errors.New("boom"), want: metrics.StatusError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, statusFromErr(tc.err)); diff != "" {
				t.Errorf("statusFromErr(%v) mismatch (-want +got):\n%s", tc.err, diff)
			}
		})
	}
}

func TestInstrumentedRepo_NoOpRecorderSkipsWrapping(t *testing.T) {
	t.Parallel()

	stub := &stubRepo{}
	got := newInstrumentedRepo(stub, metrics.NoOp())
	// If wrapping were applied we'd see *instrumentedRepo; the no-op
	// short-circuit keeps the bare destRepo so the cost of the time
	// pair never lands on the metrics-disabled hot path.
	if _, wrapped := got.(*instrumentedRepo); wrapped {
		t.Errorf("newInstrumentedRepo wrapped despite NoOp recorder")
	}
}

// stubStreamPusher echoes a fixed descriptor / error so we can exercise
// the streaming-push wrapper without standing up an HTTP fake.
type stubStreamPusher struct {
	desc ocispec.Descriptor
	err  error
}

func (s *stubStreamPusher) Push(ctx context.Context, mediaType, expectedDigest string, content io.Reader) (ocispec.Descriptor, error) {
	if content != nil {
		_, _ = io.Copy(io.Discard, content)
	}
	return s.desc, s.err
}

func TestInstrumentedStreamPusher_Push(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "ok", err: nil, want: "push_blob_streaming:ok"},
		{name: "error", err: errors.New("net"), want: "push_blob_streaming:error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &fakeRecorder{}
			p := newInstrumentedStreamPusher(&stubStreamPusher{err: tc.err}, rec)
			_, _ = p.Push(t.Context(), "application/octet-stream", "", nil)
			if diff := cmp.Diff([]string{tc.want}, rec.backendOpStatuses()); diff != "" {
				t.Errorf("backend op:status mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
