package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	nsmeta "github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

type artifactNamespace struct {
	view *NamespaceView
}

func (n artifactNamespace) Name() string {
	return n.view.Namespace()
}

func (n artifactNamespace) Spec(ctx context.Context) (*nsmeta.Spec, error) {
	return n.view.Spec(ctx)
}

func (n artifactNamespace) Package(name string) Package {
	return artifactPackage{view: n.view, name: name}
}

func (n artifactNamespace) ListPackages(ctx context.Context) ([]string, error) {
	return n.view.ListPackages(ctx)
}

type artifactPackage struct {
	view *NamespaceView
	name string
}

func (p artifactPackage) Name() string {
	return p.name
}

func (p artifactPackage) PutFile(ctx context.Context, version string, file FilePut, body io.Reader) (*FileDescriptor, error) {
	return p.putFile(ctx, version, file, body, false)
}

func (p artifactPackage) PutCachedFile(ctx context.Context, version string, file FilePut, body io.Reader) (*FileDescriptor, error) {
	return p.putFile(ctx, version, file, body, true)
}

func (p artifactPackage) putFile(ctx context.Context, version string, file FilePut, body io.Reader, cached bool) (*FileDescriptor, error) {
	rf, err := p.repoFile(version, "", file.Name)
	if err != nil {
		return nil, err
	}
	rf.MediaType = file.MediaType
	rf.Digest = file.Digest
	rf.Size = file.Size
	rf.AllowOverwrite = file.AllowOverwrite
	if cached {
		return p.view.AddCachedFile(ctx, rf, body)
	}
	return p.view.AddFile(ctx, rf, body)
}

func (p artifactPackage) GetFile(_ context.Context, version, name string) (FileHandle, error) {
	rf, err := p.repoFile(version, "", name)
	if err != nil {
		return nil, err
	}
	return &fileHandle{
		view: p.view,
		file: rf,
		info: FileInfo{
			Package: p.name,
			Version: version,
			Name:    name,
		},
	}, nil
}

func (p artifactPackage) GetFileByTag(_ context.Context, tag, name string) (FileHandle, error) {
	rf, err := p.repoFile("", tag, name)
	if err != nil {
		return nil, err
	}
	return &fileHandle{
		view: p.view,
		file: rf,
		info: FileInfo{
			Package: p.name,
			Name:    name,
		},
	}, nil
}

func (p artifactPackage) ListVersions(ctx context.Context) ([]string, error) {
	files, err := p.ListFiles(ctx, ListFilesOptions{})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, file := range files {
		if file.Version == "" {
			continue
		}
		seen[file.Version] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for version := range seen {
		out = append(out, version)
	}
	sort.Strings(out)
	return out, nil
}

func (p artifactPackage) ListTags(ctx context.Context) ([]Tag, error) {
	allTags, err := p.view.ListTags(ctx, p.name)
	if err != nil {
		return nil, err
	}
	versions, err := p.ListVersions(ctx)
	if err != nil {
		return nil, err
	}
	versionSet := make(map[string]struct{}, len(versions))
	for _, version := range versions {
		versionSet[version] = struct{}{}
	}
	out := make([]Tag, 0, len(allTags))
	for _, tag := range allTags {
		if _, isVersion := versionSet[tag]; isVersion {
			continue
		}
		out = append(out, Tag{Name: tag})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p artifactPackage) ResolveTag(ctx context.Context, tag string) (Version, error) {
	version, err := p.view.ResolveTag(ctx, p.name, tag)
	if err != nil {
		return Version{}, err
	}
	return Version{Name: version}, nil
}

func (p artifactPackage) ListFiles(ctx context.Context, opts ListFilesOptions) ([]FileInfo, error) {
	files, err := p.view.ListFiles(ctx, p.name)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(files))
	for _, file := range files {
		if opts.Version != "" && file.OwningTag != opts.Version {
			continue
		}
		out = append(out, FileInfo{
			Package: p.name,
			Version: file.OwningTag,
			Name:    file.Name,
			Digest:  file.Digest,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (p artifactPackage) Tag(ctx context.Context, tag, version string) error {
	if tag == "" {
		return errors.New("tag must not be empty")
	}
	return p.view.AppendRefs(ctx, p.name, version, tag)
}

func (p artifactPackage) repoFile(version, tag, name string) (*oci.RepoFile, error) {
	if name == "" {
		return nil, errors.New("file name must not be empty")
	}
	if version == "" && tag == "" {
		return nil, errors.New("version or tag must not be empty")
	}
	if version != "" && tag != "" {
		return nil, errors.New("version and tag are mutually exclusive")
	}
	return &oci.RepoFile{
		OwningRepo: p.name,
		OwningTag:  version,
		RefTag:     tag,
		Name:       name,
	}, nil
}

type fileHandle struct {
	view *NamespaceView
	file *oci.RepoFile
	info FileInfo
}

func (h *fileHandle) Info() FileInfo {
	return h.info
}

func (h *fileHandle) DownloadURL(ctx context.Context) (string, error) {
	if h == nil || h.file == nil {
		return "", errors.New("file handle must not be nil")
	}
	return h.view.BlobRedirectURL(ctx, h.file)
}

func (h *fileHandle) Open(ctx context.Context) (io.ReadCloser, error) {
	if h == nil || h.file == nil {
		return nil, errors.New("file handle must not be nil")
	}
	desc, rc, err := h.view.ReadFile(ctx, h.file)
	if err != nil {
		return nil, err
	}
	h.info.Digest = desc.File.Digest.String()
	h.info.Size = desc.File.Size
	if h.file.OwningTag != "" {
		h.info.Version = h.file.OwningTag
	}
	h.info.MediaType = desc.File.MediaType
	return rc, nil
}

func (p artifactPackage) String() string {
	return fmt.Sprintf("%s/%s", p.view.Namespace(), p.name)
}
