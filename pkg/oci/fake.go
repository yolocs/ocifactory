package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

type FakeRegistry struct {
	Files map[string][]byte
	Tags  map[string][]string
}

func NewFakeRegistry() *FakeRegistry {
	return &FakeRegistry{
		Files: make(map[string][]byte),
		Tags:  make(map[string][]string),
	}
}

func (r *FakeRegistry) AddTag(repo, tag string) {
	if _, ok := r.Tags[repo]; !ok {
		r.Tags[repo] = []string{}
	}
	r.Tags[repo] = append(r.Tags[repo], tag)
}

func (r *FakeRegistry) AddFile(ctx context.Context, f *RepoFile, ro io.Reader) (*FileDescriptor, error) {
	content, err := io.ReadAll(ro)
	if err != nil {
		return nil, err
	}

	key := f.OwningRepo + "/" + f.OwningTag + "/" + f.Name
	r.Files[key] = content

	r.AddTag(f.OwningRepo, f.OwningTag)

	desc := generateDescriptor(content, f)

	return &FileDescriptor{
		File: desc,
	}, nil
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
	key := f.OwningRepo + "/" + f.OwningTag + "/" + f.Name
	content, ok := r.Files[key]
	if !ok {
		return nil, nil, fmt.Errorf("file not found: %s: %w", key, errdef.ErrNotFound)
	}

	desc := generateDescriptor(content, f)

	return &FileDescriptor{
		File: desc,
	}, io.NopCloser(bytes.NewReader(content)), nil
}

func (r *FakeRegistry) ListTags(ctx context.Context, repo string) ([]string, error) {
	tags, ok := r.Tags[repo]
	if !ok {
		return []string{}, nil
	}
	return tags, nil
}
