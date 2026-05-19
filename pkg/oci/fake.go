package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

// FakeRegistry is the in-memory implementation of handler.Registry used by
// every package's tests. The on-disk shape it preserves — Files keyed by
// "<repo>/<tag>/<name>" and Tags keyed by repo — predates the OCI 1.1
// referrers redesign and is reached into directly by handler tests in
// other packages, so the field types stay as-is. Aliases is the new map
// modelling the alias-manifest layer of the redesign without disturbing
// the existing fields.
//
// The fake is not a faithful in-memory OCI registry — it only models the
// concepts pkg/oci's public surface exposes. In particular, it does not
// model manifest digests, layers, or referrer descriptors. Tests that
// need those should use the inMemoryRepo wrapper around oras-go's
// content/memory.Store instead (see registry_test.go).
type FakeRegistry struct {
	// mu guards Files, Tags, and Aliases against concurrent
	// mutation. Single-threaded handler tests don't need it; the
	// lock exists for tests that exercise wrappers concurrently
	// (e.g. pkg/namespace's package-index race test). Public field
	// access from tests still works — callers reading the maps
	// outside of test goroutines see a consistent snapshot.
	mu      sync.Mutex
	Files   map[string][]byte
	Tags    map[string][]string
	Aliases map[string]string

	// AllowOverwrite mirrors WithAllowOverwrite on the real Registry.
	// When false (the default), AddFile returns ErrAlreadyExists for a
	// re-upload of an existing (OwningRepo, OwningTag, Name); when
	// true the existing entry is replaced. A per-call
	// RepoFile.AllowOverwrite=true forces the same loosening for a
	// single call even when this is false — matches the real Registry
	// "force on, not force off" semantics. Tests that exercise the
	// 409-on-re-upload branch leave both off; tests for overwrite flip
	// either on per-test.
	AllowOverwrite bool
}

func NewFakeRegistry() *FakeRegistry {
	return &FakeRegistry{
		Files:   make(map[string][]byte),
		Tags:    make(map[string][]string),
		Aliases: make(map[string]string),
	}
}

// AddTag records a canonical version tag for a repo. Kept as an exported
// helper because handler tests (notably python) seed canonical tags
// directly to set up package-list expectations.
func (r *FakeRegistry) AddTag(repo, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addTagLocked(repo, tag)
}

func (r *FakeRegistry) addTagLocked(repo, tag string) {
	if slices.Contains(r.Tags[repo], tag) {
		return
	}
	r.Tags[repo] = append(r.Tags[repo], tag)
}

func (r *FakeRegistry) AddFile(ctx context.Context, f *RepoFile, ro io.Reader) (*FileDescriptor, error) {
	if f.OwningTag == "" {
		return nil, fmt.Errorf("OwningTag must be set")
	}

	// Pre-flight checks under the lock so we can refuse the upload
	// without consuming the body — matches the real Registry's
	// pre-upload rejection contract that handler tests rely on for
	// twine-retry simulation.
	key := f.OwningRepo + "/" + f.OwningTag + "/" + f.Name
	r.mu.Lock()
	if _, ok := r.Aliases[aliasKey(f.OwningRepo, f.OwningTag)]; ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: tag %q in repo %q is an alias", ErrAliasCollision, f.OwningTag, f.OwningRepo)
	}
	if _, exists := r.Files[key]; exists && !r.AllowOverwrite && !f.AllowOverwrite {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAlreadyExists, key)
	}
	r.mu.Unlock()

	// Read the body without holding the lock — a slow reader (e.g.
	// one blocked on a channel) would otherwise stall every
	// concurrent fake op. The race window is benign: a colliding
	// concurrent upload that wins between the pre-check and the
	// store below either gets ErrAlreadyExists itself or, with
	// AllowOverwrite, is overwritten in turn — same outcome as a
	// real backend racing two PUTs.
	content, err := io.ReadAll(ro)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.Files[key] = content
	r.addTagLocked(f.OwningRepo, f.OwningTag)

	desc := generateDescriptor(content, f)
	return &FileDescriptor{File: desc}, nil
}

func generateDescriptor(content []byte, f *RepoFile) ocispec.Descriptor {
	h := sha256.New()
	h.Write(content)
	d := fmt.Sprintf("sha256:%x", h.Sum(nil))

	return ocispec.Descriptor{
		MediaType: detectFileMediaType(f),
		Digest:    digest.Digest(d),
		Size:      int64(len(content)),
		Annotations: map[string]string{
			FileNameAnnotation: f.Name,
		},
	}
}

func (r *FakeRegistry) ReadFile(ctx context.Context, f *RepoFile) (*FileDescriptor, io.ReadCloser, error) {
	if f.OwningTag == "" && f.RefTag == "" {
		return nil, nil, fmt.Errorf("either OwningTag or RefTag must be set")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	tag := f.OwningTag
	if tag == "" {
		canonical, ok := r.Aliases[aliasKey(f.OwningRepo, f.RefTag)]
		if !ok {
			return nil, nil, fmt.Errorf("alias %q not found in repo %q: %w", f.RefTag, f.OwningRepo, errdef.ErrNotFound)
		}
		tag = canonical
	}

	key := f.OwningRepo + "/" + tag + "/" + f.Name
	content, ok := r.Files[key]
	if !ok {
		return nil, nil, fmt.Errorf("file not found: %s: %w", key, errdef.ErrNotFound)
	}

	desc := generateDescriptor(content, f)
	return &FileDescriptor{File: desc}, io.NopCloser(bytes.NewReader(content)), nil
}

// AppendRefs creates one alias per ref pointing at canonicalTag. Refuses
// to overwrite an existing canonical version tag (alias collision); a
// re-point of an existing alias is allowed and replaces the previous
// target.
func (r *FakeRegistry) AppendRefs(ctx context.Context, repo string, canonicalTag string, refs ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.Tags[repo], canonicalTag) {
		return fmt.Errorf("canonical tag %q not found in repo %q: %w", canonicalTag, repo, errdef.ErrNotFound)
	}
	for _, ref := range refs {
		if slices.Contains(r.Tags[repo], ref) {
			return fmt.Errorf("%w: ref %q already exists as a canonical tag in repo %q", ErrAliasCollision, ref, repo)
		}
	}
	for _, ref := range refs {
		r.Aliases[aliasKey(repo, ref)] = canonicalTag
	}
	return nil
}

// ListTags returns canonical tags plus alias tags for a repo, mirroring
// the new pkg/oci.Registry behaviour. Aliases are reported with their
// alias name; callers that need to discriminate use the manifest type
// check (not modelled here — tests that need it use the inMemoryRepo
// wrapper).
func (r *FakeRegistry) ListTags(ctx context.Context, repo string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tags := append([]string{}, r.Tags[repo]...)
	prefix := repo + "/"
	for k := range r.Aliases {
		if strings.HasPrefix(k, prefix) {
			tags = append(tags, strings.TrimPrefix(k, prefix))
		}
	}
	return tags, nil
}

// ResolveTag resolves a canonical or alias tag to its canonical version.
func (r *FakeRegistry) ResolveTag(ctx context.Context, repo, tag string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.Tags[repo], tag) {
		return tag, nil
	}
	if canonical, ok := r.Aliases[aliasKey(repo, tag)]; ok {
		return canonical, nil
	}
	return "", fmt.Errorf("tag %q not found in repo %q: %w", tag, repo, errdef.ErrNotFound)
}

// ListFiles enumerates files keyed under the repo, ignoring alias tags
// (alias resolution would double-count files that already appear under
// their canonical version). Digest is computed from the stored content
// to mirror the real Registry's behaviour, where the digest comes from
// the file manifest's FileDigestAnnotation.
func (r *FakeRegistry) ListFiles(ctx context.Context, repo string) ([]*RepoFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var filesList []*RepoFile
	for key, content := range r.Files {
		parts := strings.Split(key, "/")
		if len(parts) < 3 {
			continue
		}
		fn, tag, rp := parts[len(parts)-1], parts[len(parts)-2], strings.Join(parts[:len(parts)-2], "/")
		if rp != repo {
			continue
		}
		filesList = append(filesList, &RepoFile{
			Name:       fn,
			OwningRepo: rp,
			OwningTag:  tag,
			Digest:     fmt.Sprintf("sha256:%x", sha256.Sum256(content)),
		})
	}
	return filesList, nil
}

// BlobRedirectURL always returns ("", nil), matching the inline-serving
// branch of the real Registry's behaviour. Handlers that try the
// redirect path against the fake fall through to ReadFile, which is
// exactly what their tests want — blob streaming is exercised either
// way.
func (r *FakeRegistry) BlobRedirectURL(ctx context.Context, f *RepoFile) (string, error) {
	return "", nil
}

// DeleteTagFiles mirrors *Registry.DeleteTagFiles for the in-memory
// fake. An alias tag is unbound; a canonical tag has every file
// keyed under it removed and the tag itself dropped from the repo's
// tag list. A tag that doesn't exist returns errdef.ErrNotFound so
// callers can use errors.Is to distinguish "already gone" from a
// genuine error.
func (r *FakeRegistry) DeleteTagFiles(ctx context.Context, repo string, tag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	aliasK := aliasKey(repo, tag)
	if _, ok := r.Aliases[aliasK]; ok {
		delete(r.Aliases, aliasK)
		return nil
	}
	if !slices.Contains(r.Tags[repo], tag) {
		return fmt.Errorf("tag %q not found in repo %q: %w", tag, repo, errdef.ErrNotFound)
	}
	prefix := repo + "/" + tag + "/"
	for k := range r.Files {
		if strings.HasPrefix(k, prefix) {
			delete(r.Files, k)
		}
	}
	r.Tags[repo] = slices.DeleteFunc(r.Tags[repo], func(t string) bool { return t == tag })
	if len(r.Tags[repo]) == 0 {
		delete(r.Tags, repo)
	}
	return nil
}

// DeleteRepoFiles mirrors *Registry.DeleteRepoFiles for the in-memory
// fake. Every canonical version (and its files) under repo is removed,
// then aliases anchored to repo are unbound. A repo with no canonical
// versions and no aliases is a no-op — matches the real Registry's
// best-effort cleanup contract.
func (r *FakeRegistry) DeleteRepoFiles(ctx context.Context, repo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := repo + "/"
	for k := range r.Files {
		if strings.HasPrefix(k, prefix) {
			delete(r.Files, k)
		}
	}
	delete(r.Tags, repo)
	for k := range r.Aliases {
		if strings.HasPrefix(k, prefix) {
			delete(r.Aliases, k)
		}
	}
	return nil
}

func aliasKey(repo, alias string) string {
	return repo + "/" + alias
}
