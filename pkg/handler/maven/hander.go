package maven

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/abcxyz/pkg/logging"
	"github.com/gorilla/mux"
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
	router := mux.NewRouter()

	// 1. Archetype Catalog
	// Handles GET, HEAD, PUT, POST for /archetype-catalog.xml
	router.HandleFunc("/archetype-catalog.xml", h.handleArchetypeCatalog).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	// 2. Snapshot Metadata (e.g., group/artifact/1.0-SNAPSHOT/maven-metadata.xml)
	// Handles GET, HEAD, PUT, POST for snapshot metadata files.
	// Example: /{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml
	router.HandleFunc("/{repoParts:.+}/{versionSnapshot:.+-SNAPSHOT}/maven-metadata.xml", h.handleSnapshotMetadata).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	// 3. Artifact Metadata (e.g., group/artifact/maven-metadata.xml or group/artifact/version/maven-metadata.xml for releases)
	// Handles GET, HEAD, PUT, POST for non-snapshot metadata files. This must be after snapshot metadata.
	// Example: /{groupId}/{artifactId}/maven-metadata.xml
	router.HandleFunc("/{repoParts:.+}/maven-metadata.xml", h.handleArtifactMetadata).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	// 4. Regular Artifact Files (e.g., group/artifact/version/file.jar)
	// Handles GET, HEAD, PUT, POST for general artifact files. This is the most general route and must be last.
	// Example: /{groupId}/{artifactId}/{version}/{filename.ext}
	router.HandleFunc("/{repoParts:.+}/{version:.+}/{filename:.+}", h.handleRegularArtifact).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	return router
}

// handleArchetypeCatalog handles requests for archetype-catalog.xml.
func (h *Handler) handleArchetypeCatalog(w http.ResponseWriter, req *http.Request) {
	f := &oci.RepoFile{
		OwningRepo: "archetype",
		OwningTag:  "latest",
		Name:       "archetype-catalog.xml",
		MediaType:  "text/xml",
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, f)
	} else { // GET, HEAD
		h.handleGet(w, req, f)
	}
}

// handleSnapshotMetadata handles requests for snapshot maven-metadata.xml files.
func (h *Handler) handleSnapshotMetadata(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"]             // This is groupId/artifactId
	versionSnapshot := vars["versionSnapshot"] // This is version-SNAPSHOT

	f := &oci.RepoFile{
		OwningRepo: repoParts,
		OwningTag:  versionSnapshot + "-metadata", // e.g., 1.0-SNAPSHOT-metadata
		Name:       "maven-metadata.xml",
		MediaType:  "text/xml",
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, f)
	} else { // GET, HEAD
		h.handleGet(w, req, f)
	}
}

// handleArtifactMetadata handles requests for non-snapshot maven-metadata.xml files.
func (h *Handler) handleArtifactMetadata(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"] // This is groupId/artifactId or groupId/artifactId/version for versioned metadata

	f := &oci.RepoFile{
		OwningRepo: repoParts,
		OwningTag:  "metadata", // For release artifact or version metadata
		Name:       "maven-metadata.xml",
		MediaType:  "text/xml",
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, f)
	} else { // GET, HEAD
		h.handleGet(w, req, f)
	}
}

// handleRegularArtifact handles requests for regular artifact files.
func (h *Handler) handleRegularArtifact(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"] // groupId/artifactId
	version := vars["version"]
	filename := vars["filename"]

	f := &oci.RepoFile{
		OwningRepo: repoParts,
		OwningTag:  version,
		Name:       filename,
		MediaType:  detectMediaType(filename),
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, f)
	} else { // GET, HEAD
		h.handleGet(w, req, f)
	}
}

// handlePut processes PUT/POST requests to add a file.
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
