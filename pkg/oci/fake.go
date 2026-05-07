package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"slices"
	"strings"

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
	Files   map[string][]byte
	Tags    map[string][]string
	Aliases map[string]string
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
	if slices.Contains(r.Tags[repo], tag) {
		return
	}
	r.Tags[repo] = append(r.Tags[repo], tag)
}

func (r *FakeRegistry) AddFile(ctx context.Context, f *RepoFile, ro io.Reader) (*FileDescriptor, error) {
	if f.OwningTag == "" {
		return nil, fmt.Errorf("OwningTag must be set")
	}
	// Refuse to clobber an alias tag with a canonical version push — the
	// real registry does the same via the artifactType HEAD probe.
	if _, ok := r.Aliases[aliasKey(f.OwningRepo, f.OwningTag)]; ok {
		return nil, fmt.Errorf("%w: tag %q in repo %q is an alias", ErrAliasCollision, f.OwningTag, f.OwningRepo)
	}

	content, err := io.ReadAll(ro)
	if err != nil {
		return nil, err
	}

	key := f.OwningRepo + "/" + f.OwningTag + "/" + f.Name
	r.Files[key] = content
	r.AddTag(f.OwningRepo, f.OwningTag)

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
	tags := append([]string{}, r.Tags[repo]...)
	prefix := repo + "/"
	for k := range r.Aliases {
		if strings.HasPrefix(k, prefix) {
			tags = append(tags, strings.TrimPrefix(k, prefix))
		}
	}
	return tags, nil
}

// ListFiles enumerates files keyed under the repo, ignoring alias tags
// (alias resolution would double-count files that already appear under
// their canonical version). Digest is computed from the stored content
// to mirror the real Registry's behaviour, where the digest comes from
// the file manifest's FileDigestAnnotation.
func (r *FakeRegistry) ListFiles(ctx context.Context, repo string) ([]*RepoFile, error) {
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

func aliasKey(repo, alias string) string {
	return repo + "/" + alias
}
