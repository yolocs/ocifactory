package maven

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
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
	registry *namespace.Registry
	authMW   func(http.Handler) http.Handler
}

// Option configures optional Handler behaviour.
type Option func(*handlerConfig)

type handlerConfig struct {
	authMW func(http.Handler) http.Handler
}

// WithAuthMiddleware installs an authentication middleware on
// every Maven route. Pass nil (or omit) to leave routes ungated;
// production wiring always supplies a middleware.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) {
		c.authMW = mw
	}
}

// NewHandler creates a Handler whose routes require a leading namespace
// segment and whose backend operations are scoped through namespace.Registry.For
// for each request.
func NewHandler(registry *namespace.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if registry == nil {
		return nil, fmt.Errorf("registry must not be nil")
	}
	return &Handler{registry: registry, authMW: cfg.authMW}, nil
}

func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	if h.authMW != nil {
		// Maven's whole route surface is private — every
		// route requires a verified AuthContext.
		router.Use(mux.MiddlewareFunc(h.authMW))
	}

	nsRouter := router.PathPrefix("/{namespace}/maven2").Subrouter()
	registerRoutes(nsRouter, h)

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
		h.handlePut(w, req, h.scopedRegistry(req), f)
	} else { // GET, HEAD
		h.handleGet(w, req, h.scopedRegistry(req), f)
	}
}

// handleSnapshotMetadata handles requests for snapshot maven-metadata.xml files.
func (h *Handler) handleSnapshotMetadata(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"]             // This is groupId/artifactId
	versionSnapshot := vars["versionSnapshot"] // This is version-SNAPSHOT

	if err := validatePath(repoParts, versionSnapshot, ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f := &oci.RepoFile{
		OwningRepo: repoParts,
		OwningTag:  versionSnapshot + "-metadata", // e.g., 1.0-SNAPSHOT-metadata
		Name:       "maven-metadata.xml",
		MediaType:  "text/xml",
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, h.scopedRegistry(req), f)
	} else { // GET, HEAD
		h.handleGet(w, req, h.scopedRegistry(req), f)
	}
}

// handleArtifactMetadata handles requests for non-snapshot maven-metadata.xml files.
func (h *Handler) handleArtifactMetadata(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"] // This is groupId/artifactId or groupId/artifactId/version for versioned metadata

	if err := validatePath(repoParts, "", ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f := &oci.RepoFile{
		OwningRepo: repoParts,
		OwningTag:  "metadata", // For release artifact or version metadata
		Name:       "maven-metadata.xml",
		MediaType:  "text/xml",
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, h.scopedRegistry(req), f)
	} else { // GET, HEAD
		h.handleGet(w, req, h.scopedRegistry(req), f)
	}
}

// handleRegularArtifact handles requests for regular artifact files.
func (h *Handler) handleRegularArtifact(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"] // groupId/artifactId
	version := vars["version"]
	filename := vars["filename"]

	if err := validatePath(repoParts, version, filename); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f := &oci.RepoFile{
		OwningRepo: repoParts,
		OwningTag:  version,
		Name:       filename,
		MediaType:  detectMediaType(filename),
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, h.scopedRegistry(req), f)
	} else { // GET, HEAD
		h.handleGet(w, req, h.scopedRegistry(req), f)
	}
}

// handlePut processes PUT/POST requests to add a file.
func (h *Handler) handlePut(w http.ResponseWriter, req *http.Request, registry handler.Registry, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	defer req.Body.Close()

	body, err := h.maybeVerifyChecksum(req, registry, f)
	if err != nil {
		logger.DebugContext(req.Context(), "checksum verification failed", "error", err)
		code := httpStatus(err)
		if code == 0 {
			writeRegistryError(req.Context(), w, err, "internal error")
			return
		}
		// Checksum-validation errors carry deliberately framed
		// public messages (4xx); pass them through.
		http.Error(w, err.Error(), code)
		return
	}

	desc, err := registry.AddFile(req.Context(), f, body)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to add file", "error", err)
		if errors.Is(err, oci.ErrAlreadyExists) {
			http.Error(w, "file already exists in version", http.StatusConflict)
			return
		}
		writeRegistryError(req.Context(), w, err, "internal error")
		return
	}
	logger.DebugContext(req.Context(), "added file", "descriptor", desc)
	w.WriteHeader(http.StatusCreated)
}

// maybeVerifyChecksum returns the request body to forward to AddFile.
// For non-checksum filenames it returns req.Body untouched and forwards
// req.ContentLength via f.Size so AddFile keeps its streaming fast path.
// For checksum filenames it buffers the (always tiny) body, validates it
// against the previously-uploaded companion artifact, and returns a
// reader over the buffered bytes so AddFile still sees an io.Reader.
func (h *Handler) maybeVerifyChecksum(req *http.Request, registry handler.Registry, f *oci.RepoFile) (io.Reader, error) {
	if ext, _, _ := checksumExt(f.Name); ext == "" {
		// Forward the HTTP Content-Length so AddFile can short-circuit
		// the peek-and-decide buffer for sized uploads (mvn deploy
		// always sets it). req.ContentLength is -1 for chunked /
		// unknown, which AddFile treats as "size unknown" and falls
		// back to peeking.
		f.Size = req.ContentLength
		return req.Body, nil
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxChecksumBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read checksum body: %w", err)
	}
	if len(body) > maxChecksumBodyBytes {
		return nil, badRequest("checksum body exceeds %d bytes", maxChecksumBodyBytes)
	}

	if err := verifyChecksumUpload(req.Context(), registry, f, body); err != nil {
		return nil, err
	}

	f.Size = int64(len(body))
	return bytes.NewReader(body), nil
}

func (h *Handler) handleGet(w http.ResponseWriter, req *http.Request, registry handler.Registry, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	// HEAD wants headers only — never redirect. The redirect path is
	// only worth taking when the response would otherwise transfer
	// bytes; HEAD has no egress to save.
	if req.Method != http.MethodHead {
		if redirectURL, err := registry.BlobRedirectURL(req.Context(), f); err == nil && redirectURL != "" {
			logger.DebugContext(req.Context(), "redirecting blob fetch to backend", "url", redirectURL)
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return
		} else if err != nil {
			// Best-effort — fall through to the streaming path.
			logger.DebugContext(req.Context(), "blob redirect probe failed; falling back to streaming", "error", err)
		}
	}

	desc, r, err := registry.ReadFile(req.Context(), f)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to read file", "error", err)
		if errors.Is(err, errdef.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeRegistryError(req.Context(), w, err, "internal error")
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
		handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "internal error")
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

func registerRoutes(router *mux.Router, h *Handler) {
	// 1. Archetype Catalog
	router.HandleFunc("/archetype-catalog.xml", h.handleArchetypeCatalog).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	// 2. Snapshot Metadata (e.g., group/artifact/1.0-SNAPSHOT/maven-metadata.xml).
	router.HandleFunc("/{repoParts:.+}/{versionSnapshot:.+-SNAPSHOT}/maven-metadata.xml", h.handleSnapshotMetadata).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	// 3. Artifact Metadata (e.g., group/artifact/maven-metadata.xml or group/artifact/version/maven-metadata.xml for releases).
	router.HandleFunc("/{repoParts:.+}/maven-metadata.xml", h.handleArtifactMetadata).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)

	// 4. Regular Artifact Files (e.g., group/artifact/version/file.jar).
	router.HandleFunc("/{repoParts:.+}/{version:.+}/{filename:.+}", h.handleRegularArtifact).Methods(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost)
}

func (h *Handler) scopedRegistry(req *http.Request) handler.Registry {
	return h.registry.For(mux.Vars(req)["namespace"])
}

func writeRegistryError(ctx context.Context, w http.ResponseWriter, err error, public string) {
	switch {
	case errors.Is(err, namespace.ErrInvalidName), errors.Is(err, namespace.ErrInvalidOwningRepo):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, namespace.ErrNotFound), errors.Is(err, errdef.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, auth.ErrUnauthorized), oci.HasCode(err, http.StatusForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case oci.HasCode(err, http.StatusUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	default:
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, public)
	}
}
