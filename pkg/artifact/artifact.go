// Package artifact exposes the handler-facing storage nouns used by
// ocifactory: package, version, tag, and file.
package artifact

import (
	"context"
	"io"

	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

// Namespace is a namespace-scoped view of artifact storage.
type Namespace interface {
	Name() string
	Spec(ctx context.Context) (*namespace.Spec, error)
	Package(name string) Package
	ListPackages(ctx context.Context) ([]string, error)
}

// Package is a namespace-local package/repository.
type Package interface {
	Name() string
	PutFile(ctx context.Context, version string, file FilePut, body io.Reader) (*FileDescriptor, error)
	PutCachedFile(ctx context.Context, version string, file FilePut, body io.Reader) (*FileDescriptor, error)
	GetFile(ctx context.Context, version, name string) (FileHandle, error)
	GetFileByTag(ctx context.Context, tag, name string) (FileHandle, error)
	ListVersions(ctx context.Context) ([]string, error)
	ListTags(ctx context.Context) ([]Tag, error)
	ResolveTag(ctx context.Context, tag string) (Version, error)
	ListFiles(ctx context.Context, opts ListFilesOptions) ([]FileInfo, error)
	Tag(ctx context.Context, tag, version string) error
}

// FilePut describes a file write under a package version.
type FilePut struct {
	Name           string
	MediaType      string
	Digest         string
	Size           int64
	AllowOverwrite bool
}

// FileDescriptor identifies a stored file. It aliases the low-level
// descriptor until handlers no longer need OCI descriptor details.
type FileDescriptor = oci.FileDescriptor

// FileInfo is the handler-facing description of a file.
type FileInfo struct {
	Package   string
	Version   string
	Name      string
	MediaType string
	Digest    string
	Size      int64
}

// Tag is a package tag or alias. Version is optional for now because
// the current low-level registry exposes aliases separately from their
// target version unless a caller resolves a known file through the tag.
type Tag struct {
	Name    string
	Version string
}

// Version is a canonical package version.
type Version struct {
	Name string
}

// ListFilesOptions filters package file listings.
type ListFilesOptions struct {
	Version string
}

// FileHandle is a read-capable handle for one package file.
type FileHandle interface {
	Info() FileInfo
	DownloadURL(ctx context.Context) (string, error)
	Open(ctx context.Context) (io.ReadCloser, error)
}
