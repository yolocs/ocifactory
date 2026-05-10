package namespace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"

	"oras.land/oras-go/v2/errdef"

	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	// DefaultPrefix is the default OCI repo prefix that holds every
	// namespace's metadata. Operators override it via [WithPrefix]
	// when ocifactory shares an OCI registry with other systems and
	// they want to park ocifactory under their own namespacing.
	DefaultPrefix = "_namespaces"

	// DefaultArtifactType is the artifactType stamped onto namespace
	// metadata manifests. Distinct from the data-plane artifactTypes
	// (e.g. application/vnd.ocifactory.python) so an operator
	// inspecting their OCI registry can tell metadata manifests apart
	// at a glance.
	DefaultArtifactType = "application/vnd.ocifactory.namespace"

	// indexRepoSegment is the sub-repo under the prefix whose tags
	// enumerate every existing namespace. Distribution's _catalog is
	// optional and inconsistently implemented across registries, so
	// we maintain our own index instead.
	indexRepoSegment = "_index"

	// metadataTag is the single tag every per-namespace repo carries.
	// One tag per repo keeps the layout discoverable: an operator
	// listing tags on _namespaces/<name> sees exactly _metadata.
	metadataTag = "_metadata"

	// specFileName is the filename of the spec layer inside the
	// metadata manifest. The literal value is part of the on-disk
	// layout and must not change without a migration.
	specFileName = "spec.json"

	// indexSentinelName is the filename of the placeholder layer
	// under each tag in the index repo. The body is fixed; only the
	// tag (= namespace name) is meaningful.
	indexSentinelName = "present"
)

// indexSentinelBody is the constant payload of every index-repo
// sentinel. Kept as bytes so each AddFile gets a fresh reader without
// copying the literal at the call site.
var indexSentinelBody = []byte("present\n")

// ErrNotFound is returned by [Store.Get] (and Delete-then-cascade
// callers) when the requested namespace does not exist. Wrapped with
// %w by methods, so use errors.Is to check.
var ErrNotFound = errors.New("namespace not found")

// Store is the namespace persistence interface. The OCI-backed
// implementation in this package is the only one we ship; the
// interface exists so tests and downstream consumers can substitute
// fakes without depending on the OCI types.
type Store interface {
	// Get returns the namespace with the given name. Returns
	// [ErrNotFound] (wrapped) when the namespace does not exist.
	Get(ctx context.Context, name string) (*Namespace, error)

	// List returns the names of every namespace currently registered.
	// Order is whatever the underlying tag listing returns.
	List(ctx context.Context) ([]string, error)

	// Put creates or updates a namespace (upsert semantics, mirroring
	// the admin API's PUT verb).
	Put(ctx context.Context, ns *Namespace) error

	// Delete removes a namespace's metadata and index entry.
	// Cascade onto the data-plane sub-repos is intentionally NOT
	// performed here; that lives in a separate downstream issue so
	// "drop the metadata" and "garbage-collect the artifacts" stay
	// independently invocable.
	Delete(ctx context.Context, name string) error
}

// Backend is the subset of *pkg/oci.Registry that pkg/namespace
// depends on. It is exposed so tests can substitute pkg/oci's
// in-memory FakeRegistry without going through a real OCI registry.
// Production code passes *oci.Registry directly.
type Backend interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
	DeleteTagFiles(ctx context.Context, repo string, tag string) error
}

// store is the OCI-backed [Store] implementation. Constructed via
// [NewStore]; the type is unexported because callers should depend on
// the [Store] interface, not the concrete implementation.
type store struct {
	backend      Backend
	prefix       string
	artifactType string
}

// Option configures a [store] at construction time.
type Option func(*store)

// WithPrefix overrides the OCI repo prefix used for namespace
// metadata. Defaults to [DefaultPrefix].
func WithPrefix(prefix string) Option {
	return func(s *store) { s.prefix = prefix }
}

// WithArtifactType overrides the artifactType stamped on namespace
// metadata manifests. Defaults to [DefaultArtifactType]. Reserved as
// an option so a future migration can flip the type without forcing a
// rewrite of every operator's flag invocation.
func WithArtifactType(at string) Option {
	return func(s *store) { s.artifactType = at }
}

// NewStore constructs an OCI-backed namespace store. The supplied
// backend is the metadata persistence layer; in production this is a
// *pkg/oci.Registry, in tests it is typically a *pkg/oci.FakeRegistry.
//
// Returns the concrete type so callers that want to enrich the store
// with additional methods (caches, audit hooks) in their own packages
// can do so without an interface dance; the methods on *store satisfy
// the [Store] interface.
func NewStore(backend Backend, opts ...Option) *store {
	s := &store{
		backend:      backend,
		prefix:       DefaultPrefix,
		artifactType: DefaultArtifactType,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// repoFor returns the per-namespace OCI repo path. Each namespace
// gets its own repo so format-agnostic per-namespace state (the
// package index from a downstream issue, future quotas, audit data)
// can live alongside the spec without polluting the data plane.
func (s *store) repoFor(name string) string {
	return path.Join(s.prefix, name)
}

// indexRepo returns the OCI repo path of the global index. One tag
// per existing namespace; tag listing yields the namespace catalogue
// without leaning on distribution's _catalog endpoint.
func (s *store) indexRepo() string {
	return path.Join(s.prefix, indexRepoSegment)
}

// Get reads the metadata layer of a namespace and decodes it.
func (s *store) Get(ctx context.Context, name string) (*Namespace, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	rf := &oci.RepoFile{
		OwningRepo: s.repoFor(name),
		OwningTag:  metadataTag,
		Name:       specFileName,
	}
	_, rc, err := s.backend.ReadFile(ctx, rf)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, fmt.Errorf("read namespace %q: %w", name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read namespace %q body: %w", name, err)
	}
	var spec Spec
	if err := json.Unmarshal(body, &spec); err != nil {
		return nil, fmt.Errorf("decode namespace %q spec: %w", name, err)
	}
	return &Namespace{Name: name, Spec: spec}, nil
}

// List returns the names of every registered namespace. An absent
// index repo (no Put has ever happened) is reported as an empty list
// rather than an error — first-Put-after-empty is a normal startup
// state, not a failure.
func (s *store) List(ctx context.Context) ([]string, error) {
	tags, err := s.backend.ListTags(ctx, s.indexRepo())
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list namespace index: %w", err)
	}
	return tags, nil
}

// Put upserts a namespace: writes the spec layer and ensures an
// index-repo entry exists.
//
// The metadata write is delete-then-add: oci.Registry's default
// (immutable) AddFile contract rejects re-uploads, but admin PUT
// semantics demand upsert. Deleting the existing tag first means a
// PUT works regardless of whether the registry was constructed with
// WithAllowOverwrite — and since the namespace metadata path is the
// admin plane (single-writer), we don't need stronger transactional
// guarantees than "delete then write".
func (s *store) Put(ctx context.Context, ns *Namespace) error {
	if ns == nil {
		return errors.New("namespace must not be nil")
	}
	if err := ValidateName(ns.Name); err != nil {
		return err
	}
	body, err := json.Marshal(ns.Spec)
	if err != nil {
		return fmt.Errorf("encode spec for %q: %w", ns.Name, err)
	}

	if err := s.backend.DeleteTagFiles(ctx, s.repoFor(ns.Name), metadataTag); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("clear namespace %q metadata: %w", ns.Name, err)
	}

	metaRF := &oci.RepoFile{
		OwningRepo: s.repoFor(ns.Name),
		OwningTag:  metadataTag,
		Name:       specFileName,
		MediaType:  "application/json",
		Size:       int64(len(body)),
	}
	if _, err := s.backend.AddFile(ctx, metaRF, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("write namespace %q metadata: %w", ns.Name, err)
	}

	// Index-entry write is idempotent: skip when the sentinel is
	// already there to honour the immutable-add contract on the
	// AddFile underneath. Re-Putting the same namespace mustn't 409
	// on the index sentinel.
	tags, err := s.backend.ListTags(ctx, s.indexRepo())
	if err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("list namespace index: %w", err)
	}
	for _, t := range tags {
		if t == ns.Name {
			return nil
		}
	}
	indexRF := &oci.RepoFile{
		OwningRepo: s.indexRepo(),
		OwningTag:  ns.Name,
		Name:       indexSentinelName,
		MediaType:  "text/plain",
		Size:       int64(len(indexSentinelBody)),
	}
	if _, err := s.backend.AddFile(ctx, indexRF, bytes.NewReader(indexSentinelBody)); err != nil {
		return fmt.Errorf("write namespace %q index entry: %w", ns.Name, err)
	}
	return nil
}

// Delete removes a namespace's metadata and index entry. Returns
// [ErrNotFound] (wrapped) when the namespace does not exist; the
// probe avoids reporting success on a no-op delete, which would mask
// admin-tool bugs.
//
// Data-plane cascade (purging artifacts under the namespace) is
// intentionally NOT performed here — see the package doc.
func (s *store) Delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := s.Get(ctx, name); err != nil {
		return err
	}
	if err := s.backend.DeleteTagFiles(ctx, s.repoFor(name), metadataTag); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("delete namespace %q metadata: %w", name, err)
	}
	if err := s.backend.DeleteTagFiles(ctx, s.indexRepo(), name); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("delete namespace %q index entry: %w", name, err)
	}
	return nil
}
