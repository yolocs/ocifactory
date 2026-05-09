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
	authMW   func(http.Handler) http.Handler
	authz    auth.Authorizer
}

// Option configures optional Handler behaviour.
type Option func(*handlerConfig)

type handlerConfig struct {
	authMW func(http.Handler) http.Handler
	authz  auth.Authorizer
}

// WithAuthMiddleware installs an authentication middleware on
// every Maven route. Pass nil (or omit) to leave routes ungated;
// production wiring always supplies a middleware.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) {
		c.authMW = mw
	}
}

// WithAuthorizer installs an auth.Authorizer that every Maven
// route consults after authentication. See python.WithAuthorizer
// for the full contract — the wiring here is symmetric.
func WithAuthorizer(a auth.Authorizer) Option {
	return func(c *handlerConfig) {
		c.authz = a
	}
}

func NewHandler(registry handler.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Handler{registry: registry, authMW: cfg.authMW, authz: cfg.authz}, nil
}

// authorize translates the configured Authorizer's decision into
// an HTTP response. Returns true when the caller should proceed,
// false when an error response has been written.
func (h *Handler) authorize(ctx context.Context, w http.ResponseWriter, act auth.Action) bool {
	err := auth.Check(ctx, h.authz, act)
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrUnauthorized) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	handler.WriteError(ctx, w, http.StatusInternalServerError, err, "authorization error")
	return false
}

// opForMethod maps an HTTP method onto an auth.Op. PUT/POST are
// writes; everything else (GET/HEAD) is a read.
func opForMethod(m string) auth.Op {
	if m == http.MethodPut || m == http.MethodPost {
		return auth.OpWrite
	}
	return auth.OpRead
}

func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	if h.authMW != nil {
		// Maven's whole route surface is private — every
		// route requires a verified AuthContext.
		router.Use(mux.MiddlewareFunc(h.authMW))
	}

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
	if !h.authorize(req.Context(), w, auth.Action{Repo: f.OwningRepo, Format: RepoType, Op: opForMethod(req.Method)}) {
		return
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
	if !h.authorize(req.Context(), w, auth.Action{Repo: f.OwningRepo, Format: RepoType, Op: opForMethod(req.Method)}) {
		return
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
	if !h.authorize(req.Context(), w, auth.Action{Repo: f.OwningRepo, Format: RepoType, Op: opForMethod(req.Method)}) {
		return
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
	if !h.authorize(req.Context(), w, auth.Action{Repo: f.OwningRepo, Format: RepoType, Op: opForMethod(req.Method)}) {
		return
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

	body, err := h.maybeVerifyChecksum(req, f)
	if err != nil {
		logger.DebugContext(req.Context(), "checksum verification failed", "error", err)
		code := httpStatus(err)
		if code == 0 {
			handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "internal error")
			return
		}
		// Checksum-validation errors carry deliberately framed
		// public messages (4xx); pass them through.
		http.Error(w, err.Error(), code)
		return
	}

	desc, err := h.registry.AddFile(req.Context(), f, body)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to add file", "error", err)
		if errors.Is(err, oci.ErrAlreadyExists) {
			http.Error(w, "file already exists in version", http.StatusConflict)
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
		handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "internal error")
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
func (h *Handler) maybeVerifyChecksum(req *http.Request, f *oci.RepoFile) (io.Reader, error) {
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

	if err := verifyChecksumUpload(req.Context(), h.registry, f, body); err != nil {
		return nil, err
	}

	f.Size = int64(len(body))
	return bytes.NewReader(body), nil
}

func (h *Handler) handleGet(w http.ResponseWriter, req *http.Request, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	// HEAD wants headers only — never redirect. The redirect path is
	// only worth taking when the response would otherwise transfer
	// bytes; HEAD has no egress to save.
	if req.Method != http.MethodHead {
		if redirectURL, err := h.registry.BlobRedirectURL(req.Context(), f); err == nil && redirectURL != "" {
			logger.DebugContext(req.Context(), "redirecting blob fetch to backend", "url", redirectURL)
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return
		} else if err != nil {
			// Best-effort — fall through to the streaming path.
			logger.DebugContext(req.Context(), "blob redirect probe failed; falling back to streaming", "error", err)
		}
	}

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
		handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "internal error")
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
