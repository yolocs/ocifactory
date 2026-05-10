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
	// DefaultPrefix is the OCI repo prefix that holds every
	// namespace's metadata when no [WithPrefix] override is given.
	DefaultPrefix = "_namespaces"

	indexRepoSegment  = "_index"
	metadataTag       = "_metadata"
	specFileName      = "spec.json"
	indexSentinelName = "present"
)

var indexSentinelBody = []byte("present\n")

// ErrNotFound is returned by [Store] methods (wrapped with %w) when
// the requested namespace does not exist.
var ErrNotFound = errors.New("namespace not found")

// Backend is the subset of *pkg/oci.Registry that pkg/namespace
// needs. It is exposed so tests can substitute pkg/oci.FakeRegistry;
// production passes *oci.Registry directly.
type Backend interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
	DeleteTagFiles(ctx context.Context, repo string, tag string) error
}

// Store persists namespace metadata in an OCI registry. Per-namespace
// metadata lives at <prefix>/<name>:_metadata; an enumerable index
// lives at <prefix>/_index, one tag per namespace. Distribution's
// _catalog endpoint is optional and inconsistently implemented across
// registries, so we maintain the index ourselves.
type Store struct {
	backend Backend
	prefix  string
}

type Option func(*Store)

// WithPrefix overrides [DefaultPrefix].
func WithPrefix(prefix string) Option {
	return func(s *Store) { s.prefix = prefix }
}

// NewStore wraps backend in a namespace [Store]. In production
// backend is a *pkg/oci.Registry; tests typically pass
// *pkg/oci.FakeRegistry.
func NewStore(backend Backend, opts ...Option) *Store {
	s := &Store{backend: backend, prefix: DefaultPrefix}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Store) repoFor(name string) string {
	return path.Join(s.prefix, name)
}

func (s *Store) indexRepo() string {
	return path.Join(s.prefix, indexRepoSegment)
}

// Get returns the namespace with the given name, or [ErrNotFound]
// (wrapped) when it does not exist.
func (s *Store) Get(ctx context.Context, name string) (*Namespace, error) {
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
// index repo is reported as an empty list rather than an error.
func (s *Store) List(ctx context.Context) ([]string, error) {
	tags, err := s.backend.ListTags(ctx, s.indexRepo())
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list namespace index: %w", err)
	}
	return tags, nil
}

// Put upserts a namespace.
//
// Metadata: delete-then-add, since oci.Registry's default AddFile
// contract is immutable but admin PUT semantics demand upsert.
//
// Index entry: AddFile with [oci.ErrAlreadyExists] swallowed. The
// sentinel is content-identical across writes, so an idempotent
// re-add reaches the same end state as "list + skip" without the
// extra ListTags round-trip.
func (s *Store) Put(ctx context.Context, ns *Namespace) error {
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

	if err := swallowNotFound(s.backend.DeleteTagFiles(ctx, s.repoFor(ns.Name), metadataTag)); err != nil {
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

	indexRF := &oci.RepoFile{
		OwningRepo: s.indexRepo(),
		OwningTag:  ns.Name,
		Name:       indexSentinelName,
		MediaType:  "text/plain",
		Size:       int64(len(indexSentinelBody)),
	}
	if _, err := s.backend.AddFile(ctx, indexRF, bytes.NewReader(indexSentinelBody)); err != nil && !errors.Is(err, oci.ErrAlreadyExists) {
		return fmt.Errorf("write namespace %q index entry: %w", ns.Name, err)
	}
	return nil
}

// Delete removes a namespace's metadata and index entry. Returns
// [ErrNotFound] (wrapped) when the namespace does not exist.
//
// Data-plane cascade (purging artifacts under the namespace) is not
// performed here — see the package doc.
func (s *Store) Delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	err := s.backend.DeleteTagFiles(ctx, s.repoFor(name), metadataTag)
	if errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return fmt.Errorf("delete namespace %q metadata: %w", name, err)
	}
	if err := swallowNotFound(s.backend.DeleteTagFiles(ctx, s.indexRepo(), name)); err != nil {
		return fmt.Errorf("delete namespace %q index entry: %w", name, err)
	}
	return nil
}

func swallowNotFound(err error) error {
	if errors.Is(err, errdef.ErrNotFound) {
		return nil
	}
	return err
}
