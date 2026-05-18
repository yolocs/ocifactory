package maven

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy/indexcache"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "maven"
	ArtifactType = "application/vnd.ocifactory.maven"

	// DefaultMaxUploadBytes caps the total request-body size accepted
	// by every PUT route. 1 GiB matches python's default; mvn deploy
	// of artifacts above 1 GiB is rare and the operator can opt in
	// via WithMaxUploadBytes / --maven-max-upload-bytes.
	DefaultMaxUploadBytes int64 = 1 << 30
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
	registry       *namespace.Registry
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64
	proxy          *proxyState
}

// Option configures optional Handler behaviour.
type Option func(*handlerConfig)

type handlerConfig struct {
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64
	fetcherFactory FetcherFactory
	negCache       *indexcache.NegativeCache
}

// WithAuthMiddleware installs an authentication middleware on
// every Maven route. Pass nil (or omit) to leave routes ungated;
// production wiring always supplies a middleware.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) {
		c.authMW = mw
	}
}

// WithMaxUploadBytes caps the total request-body size accepted by
// every Maven PUT route. The default is DefaultMaxUploadBytes
// (1 GiB). A non-positive value disables the cap (not recommended).
func WithMaxUploadBytes(n int64) Option {
	return func(c *handlerConfig) {
		c.maxUploadBytes = n
	}
}

// NewHandler creates a new Handler.
//
// registry is the data-plane wrapper that hands out per-request
// [*namespace.ScopedRegistry] views via [registry.For]. Every routed
// handler func resolves the namespace from the request URL
// (`/{namespace}/maven2/...`) and obtains a scoped view at the top.
func NewHandler(registry *namespace.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{
		maxUploadBytes: DefaultMaxUploadBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	h := &Handler{
		registry:       registry,
		authMW:         cfg.authMW,
		maxUploadBytes: cfg.maxUploadBytes,
	}
	h.proxy = newProxyState(cfg)
	return h, nil
}

// Mux returns the maven handler's router. Every route lives under
// the `/{namespace}/maven2` subrouter — the `maven2/` segment after
// the namespace makes the URL self-describing for operators routing
// multiple format handlers behind a single hostname per namespace.
func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	router.Use(mux.MiddlewareFunc(handler.RouteNameOpMiddleware))
	if h.authMW != nil {
		// Maven's whole route surface is private — every
		// route requires a verified AuthContext.
		router.Use(mux.MiddlewareFunc(h.authMW))
	}

	nsr := router.PathPrefix("/{namespace}/maven2").Subrouter()

	readMethods := []string{http.MethodGet, http.MethodHead}
	writeMethods := []string{http.MethodPut, http.MethodPost}

	// 1. Archetype Catalog
	nsr.HandleFunc("/archetype-catalog.xml", h.handleArchetypeCatalog).Methods(readMethods...).Name("read")
	nsr.HandleFunc("/archetype-catalog.xml", h.handleArchetypeCatalog).Methods(writeMethods...).Name("write")

	// 2. Metadata checksum sidecars. These must be registered before
	// the metadata and generic artifact routes so maven-metadata.xml.sha1
	// is owned by the metadata object, not by a fake artifact version.
	for _, ext := range []string{"md5", "sha1", "sha256", "sha512"} {
		filename := "maven-metadata.xml." + ext
		nsr.HandleFunc("/{repoParts:.+}/{versionSnapshot:.+-SNAPSHOT}/"+filename, h.handleSnapshotMetadataSidecar).Methods(readMethods...).Name("read")
		nsr.HandleFunc("/{repoParts:.+}/{versionSnapshot:.+-SNAPSHOT}/"+filename, h.handleSnapshotMetadataSidecar).Methods(writeMethods...).Name("write")
		nsr.HandleFunc("/{repoParts:.+}/"+filename, h.handleArtifactMetadataSidecar).Methods(readMethods...).Name("read")
		nsr.HandleFunc("/{repoParts:.+}/"+filename, h.handleArtifactMetadataSidecar).Methods(writeMethods...).Name("write")
	}

	// 3. Snapshot Metadata (e.g., group/artifact/1.0-SNAPSHOT/maven-metadata.xml)
	nsr.HandleFunc("/{repoParts:.+}/{versionSnapshot:.+-SNAPSHOT}/maven-metadata.xml", h.handleSnapshotMetadata).Methods(readMethods...).Name("read")
	nsr.HandleFunc("/{repoParts:.+}/{versionSnapshot:.+-SNAPSHOT}/maven-metadata.xml", h.handleSnapshotMetadata).Methods(writeMethods...).Name("write")

	// 4. Artifact Metadata (release; must be registered after snapshot).
	nsr.HandleFunc("/{repoParts:.+}/maven-metadata.xml", h.handleArtifactMetadata).Methods(readMethods...).Name("read")
	nsr.HandleFunc("/{repoParts:.+}/maven-metadata.xml", h.handleArtifactMetadata).Methods(writeMethods...).Name("write")

	// 5. Regular Artifact Files. Most general; must be last.
	nsr.HandleFunc("/{repoParts:.+}/{version:.+}/{filename:.+}", h.handleRegularArtifact).Methods(readMethods...).Name("read")
	nsr.HandleFunc("/{repoParts:.+}/{version:.+}/{filename:.+}", h.handleRegularArtifact).Methods(writeMethods...).Name("write")

	return router
}

// scopedFor returns the per-request [*namespace.ScopedRegistry] for
// the namespace in req's URL. Cheap to construct; called per request.
func (h *Handler) scopedFor(req *http.Request) *namespace.ScopedRegistry {
	return h.registry.For(mux.Vars(req)["namespace"])
}

// handleArchetypeCatalog handles requests for archetype-catalog.xml.
func (h *Handler) handleArchetypeCatalog(w http.ResponseWriter, req *http.Request) {
	scoped := h.scopedFor(req)
	if _, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy && (req.Method == http.MethodPut || req.Method == http.MethodPost) {
		http.Error(w, "uploads disabled on proxy namespaces", http.StatusMethodNotAllowed)
		return
	}
	f := &oci.RepoFile{
		OwningRepo: "archetype",
		OwningTag:  "latest",
		Name:       "archetype-catalog.xml",
		MediaType:  "text/xml",
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, scoped, f)
	} else { // GET, HEAD
		h.handleGet(w, req, scoped, f)
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

	scoped := h.scopedFor(req)
	f := &oci.RepoFile{
		OwningRepo: packageOwningRepo(repoParts),
		OwningTag:  versionSnapshot + "-metadata", // e.g., 1.0-SNAPSHOT-metadata
		Name:       "maven-metadata.xml",
		MediaType:  "text/xml",
		// Per-version snapshot metadata is rewritten on every deploy
		// (unique snapshots track the latest timestamped build).
		// Always allow overwrite here so the second `mvn deploy` of a
		// snapshot doesn't 409 on the metadata write.
		AllowOverwrite: true,
	}
	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			http.Error(w, "uploads disabled on proxy namespaces", http.StatusMethodNotAllowed)
			return
		}
		h.handleMetadataProxy(w, req, scoped, spec, repoParts+"/"+versionSnapshot)
		return
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, scoped, f)
	} else { // GET, HEAD
		h.handleGet(w, req, scoped, f)
	}
}

func (h *Handler) handleSnapshotMetadataSidecar(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"]
	versionSnapshot := vars["versionSnapshot"]
	filename := path.Base(req.URL.Path)

	if err := validatePath(repoParts, versionSnapshot, filename); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	scoped := h.scopedFor(req)
	f := &oci.RepoFile{
		OwningRepo:     packageOwningRepo(repoParts),
		OwningTag:      versionSnapshot + "-metadata",
		Name:           filename,
		MediaType:      detectMediaType(filename),
		AllowOverwrite: true,
	}
	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			http.Error(w, "uploads disabled on proxy namespaces", http.StatusMethodNotAllowed)
			return
		}
		h.handleMetadataSidecarProxy(w, req, scoped, spec, f, repoParts+"/"+versionSnapshot)
		return
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, scoped, f)
	} else {
		h.handleGet(w, req, scoped, f)
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

	scoped := h.scopedFor(req)
	f := &oci.RepoFile{
		OwningRepo: packageOwningRepo(repoParts),
		OwningTag:  "metadata", // For release artifact or version metadata
		Name:       "maven-metadata.xml",
		MediaType:  "text/xml",
	}
	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			http.Error(w, "uploads disabled on proxy namespaces", http.StatusMethodNotAllowed)
			return
		}
		h.handleMetadataProxy(w, req, scoped, spec, repoParts)
		return
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, scoped, f)
	} else { // GET, HEAD
		h.handleGet(w, req, scoped, f)
	}
}

func (h *Handler) handleArtifactMetadataSidecar(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repoParts := vars["repoParts"]
	filename := path.Base(req.URL.Path)

	if err := validatePath(repoParts, "", filename); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	scoped := h.scopedFor(req)
	f := &oci.RepoFile{
		OwningRepo: packageOwningRepo(repoParts),
		OwningTag:  "metadata",
		Name:       filename,
		MediaType:  detectMediaType(filename),
	}
	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			http.Error(w, "uploads disabled on proxy namespaces", http.StatusMethodNotAllowed)
			return
		}
		h.handleMetadataSidecarProxy(w, req, scoped, spec, f, repoParts)
		return
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, scoped, f)
	} else {
		h.handleGet(w, req, scoped, f)
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

	scoped := h.scopedFor(req)
	f := &oci.RepoFile{
		OwningRepo: packageOwningRepo(repoParts),
		OwningTag:  version,
		Name:       filename,
		MediaType:  detectMediaType(filename),
		// Maven snapshot versions are mutable by design: non-unique
		// snapshots overwrite the same artifact path on every deploy,
		// and unique snapshots leave behind a per-version
		// maven-metadata.xml that the snapshot-metadata route also
		// rewrites. Release versions stay immutable and respect the
		// registry-level --allow-overwrite default.
		AllowOverwrite: isSnapshotVersion(version),
	}
	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			http.Error(w, "uploads disabled on proxy namespaces", http.StatusMethodNotAllowed)
			return
		}
		h.handleFileGetProxy(w, req, scoped, spec, f)
		return
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		h.handlePut(w, req, scoped, f)
	} else { // GET, HEAD
		h.handleGet(w, req, scoped, f)
	}
}

// handlePut processes PUT/POST requests to add a file.
func (h *Handler) handlePut(w http.ResponseWriter, req *http.Request, scoped handler.Registry, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	if h.maxUploadBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, h.maxUploadBytes)
	}

	defer req.Body.Close()

	body, err := h.maybeVerifyChecksum(req, scoped, f)
	if err != nil {
		logger.DebugContext(req.Context(), "checksum verification failed", "error", err)
		if handler.WriteNamespaceError(w, err) {
			return
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
			return
		}
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

	desc, err := scoped.AddFile(req.Context(), f, body)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to add file", "error", err)
		if handler.WriteNamespaceError(w, err) {
			return
		}
		var maxBytesErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytesErr):
			http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
		case errors.Is(err, oci.ErrAlreadyExists):
			http.Error(w, "file already exists in version", http.StatusConflict)
		case oci.HasCode(err, http.StatusUnauthorized):
			http.Error(w, err.Error(), http.StatusUnauthorized)
		case oci.HasCode(err, http.StatusForbidden):
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "internal error")
		}
		return
	}
	logger.DebugContext(req.Context(), "added file", "descriptor", desc)
	w.WriteHeader(http.StatusCreated)
}

// maybeVerifyChecksum returns the request body to forward to AddFile.
// For non-checksum filenames it returns req.Body untouched and forwards
// req.ContentLength via f.Size so AddFile keeps its streaming fast path.
// For checksum filenames it buffers the (always tiny) body, validates it
// against the previously-uploaded companion artifact (looked up through
// the per-request scoped registry view), and returns a reader over the
// buffered bytes so AddFile still sees an io.Reader.
func (h *Handler) maybeVerifyChecksum(req *http.Request, scoped handler.Registry, f *oci.RepoFile) (io.Reader, error) {
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

	if err := verifyChecksumUpload(req.Context(), scoped, f, body); err != nil {
		return nil, err
	}

	f.Size = int64(len(body))
	return bytes.NewReader(body), nil
}

func (h *Handler) handleGet(w http.ResponseWriter, req *http.Request, scoped handler.Registry, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())

	// HEAD wants headers only — never redirect. The redirect path is
	// only worth taking when the response would otherwise transfer
	// bytes; HEAD has no egress to save.
	if req.Method != http.MethodHead {
		if redirectURL, err := scoped.BlobRedirectURL(req.Context(), f); err == nil && redirectURL != "" {
			logger.DebugContext(req.Context(), "redirecting blob fetch to backend", "url", redirectURL)
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return
		} else if err != nil {
			// Best-effort — fall through to the streaming path. A
			// hard namespace authz failure surfaces from ReadFile
			// below, so the redirect probe doesn't need its own
			// mapping path.
			logger.DebugContext(req.Context(), "blob redirect probe failed; falling back to streaming", "error", err)
		}
	}

	desc, r, err := scoped.ReadFile(req.Context(), f)
	if err != nil {
		logger.DebugContext(req.Context(), "failed to read file", "error", err)
		if handler.WriteNamespaceError(w, err) {
			return
		}
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
