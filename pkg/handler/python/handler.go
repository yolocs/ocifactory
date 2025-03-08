package python

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/abcxyz/pkg/logging"
	"github.com/abcxyz/pkg/renderer"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "python"
	ArtifactType = "application/vnd.ocifactory.python"

	maxPackageLength = 256
	maxVersionLength = 128
)

var (
	mimeTypes = map[string]string{
		"whl":      "application/x-wheel+zip",
		"gz":       "application/x-gzip",
		"bz2":      "application/x-bzip2",
		"zip":      "application/zip",
		"py":       "text/x-python",
		"egg":      "text/plain",
		"egg-info": "text/plain",
	}

	// pkgNameRegExp is the regex matcher for package names.
	// Reference: https://packaging.python.org/specifications/core-metadata/#name.
	pkgNameRegExp = regexp.MustCompile("(?i)^([A-Z0-9]|[A-Z0-9][A-Z0-9-_.]*[A-Z0-9])$")

	//go:embed simple.html
	fs embed.FS
)

type index struct {
	Title string
	Files []fileResult
}

type fileResult struct {
	FileName string
	FileURL  *url.URL
}

type repoFile struct {
	oci.RepoFile
	Content io.ReadCloser
}

type Handler struct {
	registry handler.Registry
	renderer *renderer.Renderer
}

// NewHandler creates a new Handler.
func NewHandler(registry handler.Registry) (*Handler, error) {
	r, err := renderer.New(context.Background(), fs)
	if err != nil {
		return nil, fmt.Errorf("failed to create renderer: %w", err)
	}
	return &Handler{registry: registry, renderer: r}, nil
}

// Mux returns a new ServeMux that handles the Python handler's routes.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()

	// Handle both pip and twine operations
	mux.HandleFunc(`PUT /{$}`, h.handleFilePut)  // For twine uploads
	mux.HandleFunc(`POST /{$}`, h.handleFilePut) // For twine uploads

	mux.HandleFunc(`GET /packages/{package}/{version}/{filename}`, h.handleFileGet) // For pip downloads

	mux.HandleFunc(`GET /simple/{package}/`, h.handlePackageIndex) // For package index (with trailing slash)
	mux.HandleFunc(`GET /simple/{package}`, h.handlePackageIndex)  // For package index (without trailing slash)

	mux.HandleFunc(`GET /simple/`, h.handleSimpleIndex) // For pip index (with trailing slash)
	mux.HandleFunc(`GET /simple`, h.handleSimpleIndex)  // For pip index (without trailing slash)

	return mux
}

// handleSimpleIndex handles the simple index request.
// For each package, we will create a new tag in the index repository.
// With that, we can call "list tags" to get all the packages.
func (h *Handler) handleSimpleIndex(w http.ResponseWriter, req *http.Request) {
	logger := logging.FromContext(req.Context())

	idx := index{Title: "Simple Index"}
	tags, err := h.registry.ListTags(req.Context(), "index")
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) { // No index yet, so we just render an empty index
			h.renderer.RenderHTML(w, "simple.html", idx)
			return
		}
		logger.ErrorContext(req.Context(), "failed to list package index", "error", err)
		http.Error(w, "failed to list package index", http.StatusInternalServerError)
		return
	}

	for _, tag := range tags {
		idx.Files = append(idx.Files, fileResult{FileName: tag, FileURL: &url.URL{
			Scheme: req.URL.Scheme,
			Host:   req.URL.Host,
			Path:   fmt.Sprintf("/simple/%s/", tag),
		}})
	}

	h.renderer.RenderHTML(w, "simple.html", idx)
}

// handleFilePut handles the file put request.
func (h *Handler) handleFilePut(w http.ResponseWriter, req *http.Request) {
	logger := logging.FromContext(req.Context())
	var pkgName, versionNum, contentName string

	reader, err := req.MultipartReader()
	if err != nil {
		if err == http.ErrNotMultipart || err == http.ErrMissingBoundary {
			http.Error(w, "missing boundary in request or not a multipart request", http.StatusBadRequest)
			return
		}
		logger.ErrorContext(req.Context(), "failed to create multipart reader", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.DebugContext(req.Context(), "failed to read multipart request", "error", err)
			http.Error(w, "request body is not valid form data", http.StatusBadRequest)
			return
		}

		switch p.FormName() {
		case "name":
			pkgNameBytes, err := io.ReadAll(io.LimitReader(p, maxPackageLength+1))
			if err != nil {
				logger.DebugContext(req.Context(), "failed to read package name", "error", err)
				http.Error(w, "failed to read package name", http.StatusBadRequest)
				return
			}
			pkgName = string(pkgNameBytes)
			if len(pkgName) > maxPackageLength {
				logger.DebugContext(req.Context(), "package name is too long", "name", pkgName, "max_length", maxPackageLength)
				http.Error(w, "package name is too long", http.StatusBadRequest)
				return
			}
			if !pkgNameRegExp.MatchString(pkgName) {
				logger.DebugContext(req.Context(), "invalid package name", "name", pkgName)
				http.Error(w, "invalid package name", http.StatusBadRequest)
				return
			}
		case "version":
			versionBytes, err := io.ReadAll(io.LimitReader(p, maxVersionLength+1))
			if err != nil {
				logger.DebugContext(req.Context(), "failed to read version", "error", err)
				http.Error(w, "failed to read version", http.StatusBadRequest)
				return
			}
			versionNum = string(versionBytes)
			if len(versionNum) > maxVersionLength {
				logger.DebugContext(req.Context(), "version is too long", "version", versionNum, "max_length", maxVersionLength)
				http.Error(w, "version is too long", http.StatusBadRequest)
				return
			}
		case "content":
			if versionNum == "" || pkgName == "" {
				logger.DebugContext(req.Context(), "version or package name is not set")
				http.Error(w, "version or package name is not set", http.StatusBadRequest)
				return
			}
			contentName = p.FileName()
			// Every time we upload a file, we also write a new tag in the index repository.
			// If the package/version already exists, it shouldn't cause a real write.
			fs := []*repoFile{
				{
					RepoFile: oci.RepoFile{
						OwningRepo: "packages/" + pkgName,
						OwningTag:  versionNum,
						Name:       contentName,
						MediaType:  detectMediaType(contentName),
					},
					Content: p,
				},
				{
					RepoFile: oci.RepoFile{
						OwningRepo: "index",
						OwningTag:  pkgName,
						Name:       versionNum,
						MediaType:  "text/plain",
					},
					Content: io.NopCloser(strings.NewReader(versionNum)),
				},
			}
			h.handlePut(req.Context(), w, fs)
		}
	}

	if pkgName == "" || versionNum == "" || contentName == "" {
		logger.DebugContext(req.Context(), "missing required fields")
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}
}

func (h *Handler) handleFileGet(w http.ResponseWriter, req *http.Request) {
	f, err := repoFileFromReq(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.OwningRepo = "packages/" + f.OwningRepo

	h.handleGet(w, req, f)
}

func repoFileFromReq(req *http.Request) (*oci.RepoFile, error) {
	pkg := req.PathValue("package")
	version := req.PathValue("version")
	filename := req.PathValue("filename")

	if pkg == "" || version == "" || filename == "" {
		return nil, fmt.Errorf("invalid path: %s", req.URL.Path)
	}

	return &oci.RepoFile{
		OwningRepo: pkg,
		OwningTag:  version,
		Name:       filename,
		MediaType:  detectMediaType(filename),
	}, nil
}

func (h *Handler) handlePackageIndex(w http.ResponseWriter, req *http.Request) {
	pkg := req.PathValue("package")
	if pkg == "" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	files, err := h.registry.ListFiles(req.Context(), "packages/"+pkg)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if oci.HasCode(err, http.StatusUnauthorized) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if oci.HasCode(err, http.StatusForbidden) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	idx := index{Title: pkg}
	for _, f := range files {
		idx.Files = append(idx.Files, fileResult{FileName: f.Name, FileURL: repoFileURL(req, f)})
	}

	h.renderer.RenderHTML(w, "simple.html", idx)
}

func repoFileURL(req *http.Request, f *oci.RepoFile) *url.URL {
	return &url.URL{
		Scheme:   req.URL.Scheme,
		Host:     req.URL.Host,
		Path:     fmt.Sprintf("/%s/%s/%s", f.OwningRepo, f.OwningTag, f.Name),
		Fragment: fmt.Sprintf("sha256=%s", strings.TrimPrefix(f.Digest, "sha256:")),
	}
}

func (h *Handler) handlePut(ctx context.Context, w http.ResponseWriter, fs []*repoFile) {
	logger := logging.FromContext(ctx)

	for _, f := range fs {
		defer f.Content.Close()
		desc, err := h.registry.AddFile(ctx, &f.RepoFile, f.Content)
		if err != nil {
			logger.DebugContext(ctx, "failed to add file", "error", err)
			if oci.HasCode(err, http.StatusUnauthorized) {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			if oci.HasCode(err, http.StatusForbidden) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.DebugContext(ctx, "added file", "descriptor", desc)
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) handleGet(w http.ResponseWriter, req *http.Request, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	desc, r, err := h.registry.ReadFile(req.Context(), f)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to read file", "error", err)
		if errors.Is(err, errdef.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if oci.HasCode(err, http.StatusUnauthorized) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if oci.HasCode(err, http.StatusForbidden) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Close()
	logger.DebugContext(req.Context(), "read file", "descriptor", desc)

	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", desc.File.Size))
	w.Header().Set("X-Checksum-Sha256", desc.File.Digest.String())
	if req.Method == http.MethodHead {
		return
	}

	if _, err := io.Copy(w, r); err != nil {
		logger.DebugContext(req.Context(), "failed to write response", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func detectMediaType(filename string) string {
	ext := strings.Trim(path.Ext(filename), ".")
	if mt, ok := mimeTypes[ext]; ok {
		return mt
	}
	return "application/octet-stream"
}
