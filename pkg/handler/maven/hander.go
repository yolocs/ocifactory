package maven

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/abcxyz/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "maven"
	ArtifactType = "application/vnd.ocifactory.maven"
)

var (
	mimeTypes = map[string]string{
		"xml":    "text/xml",
		"pom":    "text/xml",
		"jar":    "application/java-archive",
		"md5":    "text/plain",
		"sha1":   "text/plain",
		"sha256": "text/plain",
		"sha512": "text/plain",
		"zip":    "application/zip",
		"war":    "application/zip",
		"ear":    "application/zip",
		"tar":    "application/x-tar",
		"swc":    "application/zip",
		"swf":    "application/x-shockwave-flash",
		"gz":     "application/x-gzip",
		"tgz":    "application/x-tgz",
		"bz2":    "application/x-bzip2",
		"tbz":    "application/x-bzip2",
		"asc":    "text/plain",
		"rpm":    "application/octet-stream",
		"deb":    "application/octet-stream",
	}
)

type Handler struct {
	registry handler.Registry
}

func NewHandler(registry handler.Registry) (*Handler, error) {
	return &Handler{registry: registry}, nil
}

func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()

	// Metadata. As a special case, GET also matches HEAD.
	mux.HandleFunc(`PUT /{filepath...}`, h.handleFilePut)
	mux.HandleFunc(`GET /{filepath...}`, h.handleFileGet)

	return mux
}

func (h *Handler) handleFilePut(w http.ResponseWriter, req *http.Request) {
	p := strings.Trim(req.PathValue("filepath"), "/")
	f, err := pathToRepoFile(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handlePut(w, req, f)
}

func (h *Handler) handleFileGet(w http.ResponseWriter, req *http.Request) {
	p := strings.Trim(req.PathValue("filepath"), "/")
	f, err := pathToRepoFile(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleGet(w, req, f)
}

func pathToRepoFile(p string) (*oci.RepoFile, error) {
	if p == "archetype-catalog.xml" {
		return &oci.RepoFile{
			OwningRepo: "archetype",
			OwningTag:  "latest",
			Name:       p,
			MediaType:  "text/xml",
		}, nil
	}

	parts := strings.Split(p, "/")
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid path: %s", p)
	}

	fn := parts[len(parts)-1]
	if fn == "maven-metadata.xml" {
		if strings.Contains(parts[len(parts)-2], "-SNAPSHOT") {
			// This is a version level maven-metadata.xml for snapshots.
			return &oci.RepoFile{
				OwningRepo: strings.Join(parts[:len(parts)-2], "/"), // groupId/artifactId
				OwningTag:  parts[len(parts)-2] + "-metadata",       // versionId-metadata
				Name:       "maven-metadata.xml",
				MediaType:  "text/xml",
			}, nil
		} else {
			// This is a group/artifact level maven-metadata.xml for releases.
			return &oci.RepoFile{
				OwningRepo: strings.Join(parts[:len(parts)-1], "/"), // groupId/artifactId
				OwningTag:  "metadata",                              // metadata
				Name:       "maven-metadata.xml",
				MediaType:  "text/xml",
			}, nil
		}
	}

	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid path: %s", p)
	}

	return &oci.RepoFile{
		OwningRepo: strings.Join(parts[:len(parts)-2], "/"), // groupId/artifactId
		OwningTag:  parts[len(parts)-2],                     // versionId
		Name:       fn,
		MediaType:  detectMediaType(fn),
	}, nil
}

func (h *Handler) handlePut(w http.ResponseWriter, req *http.Request, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	defer req.Body.Close()
	desc, err := h.registry.AddFile(req.Context(), f, req.Body)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to add file", "error", err)
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
	logger.DebugContext(req.Context(), "added file", "descriptor", desc)
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
