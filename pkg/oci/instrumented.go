package oci

import (
	"context"
	"errors"
	"io"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/metrics"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Backend operation labels recorded via metrics.Recorder.OCIBackendCall.
// Centralised so producer (instrumentedRepo, streamPusher) and consumer
// (operators reading dashboards) share the same vocabulary.
const (
	opPushBlob       = "push_blob"
	opPushManifest   = "push_manifest"
	opFetchBlob      = "fetch_blob"
	opFetchManifest  = "fetch_manifest"
	opExists         = "exists"
	opResolve        = "resolve"
	opTag            = "tag"
	opDelete         = "delete"
	opDeleteTag      = "delete_tag"
	opListTags       = "list_tags"
	opListReferrers  = "list_referrers"
	opPredecessors   = "predecessors"
	opStreamPushBlob = "push_blob_streaming"
)

// instrumentedRepo wraps a destRepo and records one Recorder observation
// per backend method call. The wrapper is the single point we maintain
// to label backend ops; individual call sites in registry.go stay
// untouched.
//
// Push and Fetch dispatch on descriptor.MediaType so we can distinguish
// blob from manifest traffic in dashboards — operators want
// push_manifest separated from push_blob to tell "uploading lots of
// content" from "churning the manifest graph".
type instrumentedRepo struct {
	destRepo
	rec metrics.Recorder
}

func newInstrumentedRepo(inner destRepo, rec metrics.Recorder) destRepo {
	if rec == nil || metrics.IsNoOp(rec) {
		return inner
	}
	return &instrumentedRepo{destRepo: inner, rec: rec}
}

func (r *instrumentedRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	op := opPushBlob
	if isManifestMediaType(expected.MediaType) {
		op = opPushManifest
	}
	start := time.Now()
	err := r.destRepo.Push(ctx, expected, content)
	r.rec.OCIBackendCall(op, statusFromErr(err), time.Since(start))
	return err
}

func (r *instrumentedRepo) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	op := opFetchBlob
	if isManifestMediaType(target.MediaType) {
		op = opFetchManifest
	}
	start := time.Now()
	rc, err := r.destRepo.Fetch(ctx, target)
	r.rec.OCIBackendCall(op, statusFromErr(err), time.Since(start))
	return rc, err
}

func (r *instrumentedRepo) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	start := time.Now()
	ok, err := r.destRepo.Exists(ctx, target)
	r.rec.OCIBackendCall(opExists, statusFromErr(err), time.Since(start))
	return ok, err
}

func (r *instrumentedRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	start := time.Now()
	desc, err := r.destRepo.Resolve(ctx, reference)
	r.rec.OCIBackendCall(opResolve, statusFromErr(err), time.Since(start))
	return desc, err
}

func (r *instrumentedRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	start := time.Now()
	err := r.destRepo.Tag(ctx, desc, reference)
	r.rec.OCIBackendCall(opTag, statusFromErr(err), time.Since(start))
	return err
}

func (r *instrumentedRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	start := time.Now()
	err := r.destRepo.Delete(ctx, target)
	r.rec.OCIBackendCall(opDelete, statusFromErr(err), time.Since(start))
	return err
}

func (r *instrumentedRepo) DeleteTag(ctx context.Context, tag string) error {
	start := time.Now()
	err := r.destRepo.DeleteTag(ctx, tag)
	r.rec.OCIBackendCall(opDeleteTag, statusFromErr(err), time.Since(start))
	return err
}

func (r *instrumentedRepo) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	start := time.Now()
	err := r.destRepo.Tags(ctx, last, fn)
	r.rec.OCIBackendCall(opListTags, statusFromErr(err), time.Since(start))
	return err
}

func (r *instrumentedRepo) Predecessors(ctx context.Context, node ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	start := time.Now()
	descs, err := r.destRepo.Predecessors(ctx, node)
	// Predecessors backs the OCI 1.1 referrers fallback path; surface
	// it under list_referrers so dashboards group both fast-path and
	// fallback traffic together rather than requiring operators to
	// know which API the registry implements.
	r.rec.OCIBackendCall(opListReferrers, statusFromErr(err), time.Since(start))
	return descs, err
}

// isManifestMediaType reports whether mt names an OCI or Docker image
// manifest. Used by Push/Fetch to pick between push_blob/push_manifest
// and fetch_blob/fetch_manifest. We accept both the OCI and Docker
// flavours because some backends (Docker Hub) advertise the latter.
func isManifestMediaType(mt string) bool {
	switch mt {
	case ocispec.MediaTypeImageManifest,
		ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}
	return false
}

// instrumentedStreamPusher wraps a streamingPusher so each Push call —
// which under the hood is a POST + N×PATCH + PUT chunked-upload session
// — produces a single OCI backend observation labelled
// push_blob_streaming. Per-chunk metrics would explode cardinality
// without aiding diagnostics: operators care about end-to-end upload
// latency, not the per-PATCH spread.
type instrumentedStreamPusher struct {
	inner streamingPusher
	rec   metrics.Recorder
}

func newInstrumentedStreamPusher(inner streamingPusher, rec metrics.Recorder) streamingPusher {
	if rec == nil || metrics.IsNoOp(rec) {
		return inner
	}
	return &instrumentedStreamPusher{inner: inner, rec: rec}
}

func (p *instrumentedStreamPusher) Push(ctx context.Context, mediaType, expectedDigest string, content io.Reader) (ocispec.Descriptor, error) {
	start := time.Now()
	desc, err := p.inner.Push(ctx, mediaType, expectedDigest, content)
	p.rec.OCIBackendCall(opStreamPushBlob, statusFromErr(err), time.Since(start))
	return desc, err
}

// statusFromErr translates a backend error into a low-cardinality status
// label suitable for a Prometheus counter. Successes are "ok"; HTTP
// failures use the integer status code; everything else (timeouts,
// network resets, ErrNotFound, ErrAlreadyExists) collapses into
// "error" so the cardinality doesn't blow up.
func statusFromErr(err error) string {
	if err == nil {
		return metrics.StatusOK
	}
	// ErrNotFound and ErrAlreadyExists are part of normal flow for the
	// dedup-on-Exists and resolve-then-create paths — labelling them
	// "error" would skew dashboards. They get their own labels.
	if errors.Is(err, errdef.ErrNotFound) {
		return "not_found"
	}
	if errors.Is(err, errdef.ErrAlreadyExists) {
		return "already_exists"
	}
	var ec *errcode.ErrorResponse
	if errors.As(err, &ec) && ec.StatusCode != 0 {
		return metrics.HTTPStatusLabel(ec.StatusCode)
	}
	return metrics.StatusError
}
