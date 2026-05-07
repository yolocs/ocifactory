package python

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/renderer"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "python"
	ArtifactType = "application/vnd.ocifactory.python"

	maxPackageLength = 256
	maxVersionLength = 128

	// maxTextFieldBytes caps the size of any single non-file form
	// part the streaming multipart walker will accept (name, version,
	// :action, sha256_digest, comment, etc.). Every legitimate twine
	// field is well under 1 KiB; 8 KiB is a generous safety margin.
	maxTextFieldBytes = 8 << 10

	// maxTotalTextFieldBytes caps the cumulative size of every text
	// part across an upload, defending against a request that piles
	// up many sub-cap fields to exhaust the walker's working memory.
	maxTotalTextFieldBytes = 64 << 10

	// DefaultMaxUploadBytes caps the total request-body size accepted
	// by handleFilePut. The streaming multipart walker never spills
	// to disk, so this is purely a denial-of-service safeguard against
	// a single oversized request streaming forever; 1 GiB comfortably
	// exceeds every wheel and sdist on PyPI today and operators can
	// tighten it via WithMaxUploadBytes / --python-max-upload-bytes.
	DefaultMaxUploadBytes = 1 << 30

	// indexSentinelName and indexSentinelContent are the constant layer
	// name and body written under index/<pkgName>. handleSimpleIndex only
	// reads tag names, so the layer body is unused — keeping it constant
	// lets the OCI backend deduplicate the blob and the file manifest
	// across every upload of every package, so the index repo grows by
	// one tag per package rather than one layer per (package, version).
	indexSentinelName    = "present"
	indexSentinelContent = "1"
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
		"metadata": "text/plain; charset=utf-8",
	}

	// pkgNameRegExp is the regex matcher for package names.
	// Reference: https://packaging.python.org/specifications/core-metadata/#name.
	pkgNameRegExp = regexp.MustCompile("(?i)^([A-Z0-9]|[A-Z0-9][A-Z0-9-_.]*[A-Z0-9])$")

	// uploadFilenameRegExp gates the bare filename twine sends in the
	// `content` part. PyPI distribution filenames are conservatively
	// shaped: ASCII letters, digits, dot, underscore, plus, hyphen.
	// We reject anything else to keep the value safe to embed in the
	// OCI manifest annotation, the simple-index URL, and the
	// `<filename>.metadata` companion key. Path separators, control
	// characters, and `..` segments cannot reach the OCI backend.
	uploadFilenameRegExp = regexp.MustCompile(`^[A-Za-z0-9._+\-]+$`)

	//go:embed simple.html
	fs embed.FS
)

type Handler struct {
	registry       handler.Registry
	renderer       *renderer.Renderer
	indexCache     *simpleIndexCache
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64
}

// Option configures optional Handler behaviour.
type Option func(*handlerConfig)

type handlerConfig struct {
	simpleIndexCacheTTL time.Duration
	authMW              func(http.Handler) http.Handler
	maxUploadBytes      int64
}

// WithSimpleIndexCacheTTL sets the per-package simple-index cache TTL.
// A zero or negative value disables the cache. The default is
// DefaultSimpleIndexCacheTTL.
func WithSimpleIndexCacheTTL(ttl time.Duration) Option {
	return func(c *handlerConfig) {
		c.simpleIndexCacheTTL = ttl
	}
}

// WithMaxUploadBytes caps the total size of a multipart upload body
// the handler will accept. The default is DefaultMaxUploadBytes
// (1 GiB). A non-positive value disables the cap (not recommended).
func WithMaxUploadBytes(n int64) Option {
	return func(c *handlerConfig) {
		c.maxUploadBytes = n
	}
}

// WithAuthMiddleware installs an authentication middleware on
// every route the handler exposes. Pass nil (or omit the option)
// to leave the routes ungated — the serve command never does that
// in production, but tests use it to construct a handler without
// dragging in pkg/auth.
//
// The python handler chains the middleware on its root router so
// every PyPI route requires a verified AuthContext. Public-by-default
// formats (npm registry root, future Go module proxy reads) will
// take a different shape: chain on a sub-router, or accept a
// per-route policy.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) {
		c.authMW = mw
	}
}

// NewHandler creates a new Handler.
func NewHandler(registry handler.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{
		simpleIndexCacheTTL: DefaultSimpleIndexCacheTTL,
		maxUploadBytes:      DefaultMaxUploadBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	r, err := renderer.New(fs)
	if err != nil {
		return nil, fmt.Errorf("failed to create renderer: %w", err)
	}
	return &Handler{
		registry:       registry,
		renderer:       r,
		indexCache:     newSimpleIndexCache(cfg.simpleIndexCacheTTL),
		authMW:         cfg.authMW,
		maxUploadBytes: cfg.maxUploadBytes,
	}, nil
}

// Mux returns a new ServeMux that handles the Python handler's routes.
//
// Each route is .Name()'d so the metrics middleware uses a stable op
// label (write / read / list) instead of a method-derived default. The
// list label distinguishes simple-index enumeration from blob reads,
// which is the diagnostic split operators want to see on dashboards.
// RouteNameOpMiddleware copies the matched route's name into the
// per-request op holder; without it the names would be visible only
// inside gorilla/mux's request clone and the outer metrics middleware
// would fall back to the method-based default.
func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	router.Use(mux.MiddlewareFunc(handler.RouteNameOpMiddleware))
	if h.authMW != nil {
		// Authentication gates every PyPI route. Public-read
		// deployments would split this into sub-routers; for
		// the python format every endpoint is private.
		router.Use(mux.MiddlewareFunc(h.authMW))
	}

	// Handle both pip and twine operations
	router.HandleFunc("/", h.handleFilePut).Methods("PUT", "POST").Name("write")

	router.HandleFunc("/packages/{package}/{version}/{filename}", h.handleFileGet).Methods("GET", "HEAD").Name("read")

	router.HandleFunc("/simple/{package}/", h.handlePackageIndex).Methods("GET").Name("list")
	router.HandleFunc("/simple/{package}", h.handlePackageIndex).Methods("GET").Name("list")

	router.HandleFunc("/simple/", h.handleSimpleIndex).Methods("GET").Name("list")
	router.HandleFunc("/simple", h.handleSimpleIndex).Methods("GET").Name("list")

	return router
}

// handleSimpleIndex renders the root simple index — the list of every
// package the registry knows about. The index repo carries one tag per
// (already-normalized) package name; ListTags is enough to enumerate
// them.
func (h *Handler) handleSimpleIndex(w http.ResponseWriter, req *http.Request) {
	logger := logging.FromContext(req.Context())

	tags, err := h.registry.ListTags(req.Context(), "index")
	if err != nil && !errors.Is(err, errdef.ErrNotFound) {
		logger.ErrorContext(req.Context(), "failed to list package index", "error", err)
		http.Error(w, "failed to list package index", http.StatusInternalServerError)
		return
	}

	if pickContentType(req.Header.Get("Accept")) == contentTypeJSONv1 {
		writeJSONIndexList(w, tags)
		return
	}

	page := indexPage{Title: "Simple Index"}
	for _, tag := range tags {
		page.Files = append(page.Files, indexFile{
			Filename: tag,
			URL: (&url.URL{
				Scheme: req.URL.Scheme,
				Host:   req.URL.Host,
				Path:   "/simple/" + tag + "/",
			}).String(),
		})
	}
	h.renderer.RenderHTML(w, "simple.html", page)
}

// handleFilePut accepts a multipart upload from twine.
//
// Multipart parts are streamed via req.MultipartReader rather than
// buffered with ParseMultipartForm. The walker collects text fields
// (name, version, :action, ...) until it hits the file part; once it
// arrives the wheel/sdist body is streamed straight into AddFile via
// the OCI streaming push path. No bytes ever touch /tmp.
//
// The streaming design imposes one ordering requirement on clients:
// the metadata fields must arrive before the `content` part, since the
// walker can't construct the OCI repo path without them. Every Python
// upload tool in the wild (twine, flit, hatchling, poetry) already
// sends fields first; uploads that violate the order get a 400.
func (h *Handler) handleFilePut(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	if h.maxUploadBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, h.maxUploadBytes)
	}

	mr, err := req.MultipartReader()
	if err != nil {
		switch {
		case errors.Is(err, http.ErrNotMultipart) || errors.Is(err, http.ErrMissingBoundary):
			http.Error(w, "missing boundary in request or not a multipart request", http.StatusBadRequest)
		default:
			logger.DebugContext(ctx, "failed to open multipart reader", "error", err)
			http.Error(w, "request body is not valid form data", http.StatusBadRequest)
		}
		return
	}

	fields := map[string]string{}
	var totalText int
	var contentPart *multipart.Part

	// Phase 1: walk text parts until the file part appears (or the
	// request ends without one).
	for {
		p, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			writeMultipartReadError(w, perr, logger, ctx)
			return
		}
		if p.FormName() == "content" {
			contentPart = p
			break
		}
		v, rerr := readTextPart(p, maxTextFieldBytes)
		_ = p.Close()
		if rerr != nil {
			if errors.Is(rerr, errFieldTooLarge) {
				http.Error(w, fmt.Sprintf("field %q exceeds %d-byte limit", p.FormName(), maxTextFieldBytes), http.StatusRequestEntityTooLarge)
				return
			}
			writeMultipartReadError(w, rerr, logger, ctx)
			return
		}
		totalText += len(v)
		if totalText > maxTotalTextFieldBytes {
			http.Error(w, "form fields too large", http.StatusRequestEntityTooLarge)
			return
		}
		// First non-empty value wins.
		if v != "" {
			if _, ok := fields[p.FormName()]; !ok {
				fields[p.FormName()] = v
			}
		}
	}

	if contentPart == nil {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}
	defer contentPart.Close()

	pkgName := fields["name"]
	versionNum := fields["version"]

	if pkgName == "" || versionNum == "" {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}
	if len(pkgName) > maxPackageLength {
		http.Error(w, "package name is too long", http.StatusBadRequest)
		return
	}
	if !pkgNameRegExp.MatchString(pkgName) {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}
	if len(versionNum) > maxVersionLength {
		http.Error(w, "version is too long", http.StatusBadRequest)
		return
	}

	contentName := contentPart.FileName()
	if contentName == "" {
		http.Error(w, "missing filename for content", http.StatusBadRequest)
		return
	}
	if !uploadFilenameRegExp.MatchString(contentName) || contentName == "." || contentName == ".." {
		http.Error(w, "invalid filename for content", http.StatusBadRequest)
		return
	}

	normalizedName := normalize(pkgName)

	wheelRF := &oci.RepoFile{
		OwningRepo: "packages/" + normalizedName,
		OwningTag:  versionNum,
		Name:       contentName,
		MediaType:  detectMediaType(contentName),
	}
	if !h.streamAddFile(ctx, w, wheelRF, contentPart) {
		return
	}

	// The index repo write is a single sentinel per package, not per
	// version. handleSimpleIndex only reads tag names from "index"
	// via ListTags, so storing one constant placeholder layer under
	// index/<normalizedName> is enough — the OCI backend deduplicates
	// the identical blob and file manifest across every subsequent
	// upload of the same package.
	sentinelRF := &oci.RepoFile{
		OwningRepo: "index",
		OwningTag:  normalizedName,
		Name:       indexSentinelName,
		MediaType:  "text/plain",
		Size:       int64(len(indexSentinelContent)),
	}
	if !h.streamAddFile(ctx, w, sentinelRF, strings.NewReader(indexSentinelContent)) {
		return
	}

	// Drain any trailing parts the client may have sent after the
	// file. None of the standard uploaders do today, but RFC 7578
	// doesn't forbid it; discarding them keeps connection reuse
	// healthy without affecting the upload outcome.
	for {
		p, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			logger.DebugContext(ctx, "failed to drain trailing multipart part", "error", perr)
			break
		}
		_, _ = io.Copy(io.Discard, p)
		_ = p.Close()
	}

	h.indexCache.invalidate(normalizedName)
	w.WriteHeader(http.StatusCreated)
}

// errFieldTooLarge is returned by readTextPart when a non-file form
// part exceeds maxTextFieldBytes.
var errFieldTooLarge = errors.New("form field too large")

// readTextPart consumes a non-file form part into a string, capping at
// limit bytes. Anything over the cap is rejected with errFieldTooLarge
// so a malicious client can't pile a multi-megabyte text field into
// the walker's working memory before the file part arrives.
func readTextPart(p *multipart.Part, limit int) (string, error) {
	data, err := io.ReadAll(io.LimitReader(p, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", errFieldTooLarge
	}
	return string(data), nil
}

// writeMultipartReadError translates an error from a multipart NextPart
// or readTextPart call into the matching HTTP response.
func writeMultipartReadError(w http.ResponseWriter, err error, logger *slog.Logger, ctx context.Context) {
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
	default:
		logger.DebugContext(ctx, "failed to read multipart part", "error", err)
		http.Error(w, "request body is not valid form data", http.StatusBadRequest)
	}
}

// streamAddFile wraps registry.AddFile with the standard error → HTTP
// translation used by the upload path. It returns true on success and
// writes the appropriate error response (and returns false) on
// failure. Callers are expected to short-circuit subsequent steps on
// false return.
//
// MaxBytesError is detected inline so a request body that overflows
// h.maxUploadBytes mid-stream (e.g. while AddFile is reading the wheel
// body) returns 413 instead of a generic 500.
func (h *Handler) streamAddFile(ctx context.Context, w http.ResponseWriter, f *oci.RepoFile, content io.Reader) bool {
	logger := logging.FromContext(ctx)
	desc, err := h.registry.AddFile(ctx, f, content)
	if err != nil {
		logger.DebugContext(ctx, "failed to add file", "error", err)
		var maxBytesErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytesErr):
			http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
		case oci.HasCode(err, http.StatusUnauthorized):
			http.Error(w, err.Error(), http.StatusUnauthorized)
		case oci.HasCode(err, http.StatusForbidden):
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return false
	}
	logger.DebugContext(ctx, "added file", "descriptor", desc)
	return true
}

func (h *Handler) handleFileGet(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkg := vars["package"]
	version := vars["version"]
	filename := vars["filename"]

	if pkg == "" || version == "" || filename == "" {
		http.Error(w, fmt.Sprintf("invalid path: missing package, version, or filename in %s", req.URL.Path), http.StatusBadRequest)
		return
	}

	pkg = normalize(pkg)
	f := &oci.RepoFile{
		OwningRepo: "packages/" + pkg,
		OwningTag:  version,
		Name:       filename,
		MediaType:  detectMediaType(filename),
	}

	h.handleGet(w, req, f)
}

func (h *Handler) handlePackageIndex(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkg := vars["package"]
	if pkg == "" {
		http.Error(w, "invalid path: missing package name", http.StatusBadRequest)
		return
	}
	pkg = normalize(pkg)

	files, ok := h.indexCache.get(pkg)
	if !ok {
		var err error
		files, err = h.resolvePackageFiles(req.Context(), pkg)
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
		h.indexCache.put(pkg, files)
	}

	rendered := make([]indexFile, 0, len(files))
	for _, f := range files {
		rendered = append(rendered, indexFile{
			Filename: f.Filename,
			URL:      renderedFileURL(req, pkg, f),
			Sha256:   f.Sha256,
		})
	}

	if pickContentType(req.Header.Get("Accept")) == contentTypeJSONv1 {
		writeJSONPackageIndex(w, pkg, rendered)
		return
	}
	h.renderer.RenderHTML(w, "simple.html", indexPage{Title: pkg, Files: rendered})
}

// resolvePackageFiles enumerates a package's files. The returned slice
// is what goes into the simple-index cache; both HTML and JSON
// renderers pivot off it.
func (h *Handler) resolvePackageFiles(ctx context.Context, pkg string) ([]cachedFile, error) {
	rawFiles, err := h.registry.ListFiles(ctx, "packages/"+pkg)
	if err != nil {
		return nil, err
	}

	entries := make([]cachedFile, 0, len(rawFiles))
	for _, f := range rawFiles {
		entries = append(entries, cachedFile{
			Filename:  f.Name,
			OwningTag: f.OwningTag,
			Sha256:    strings.TrimPrefix(f.Digest, "sha256:"),
		})
	}
	return entries, nil
}

func renderedFileURL(req *http.Request, pkg string, f cachedFile) string {
	u := url.URL{
		Scheme: req.URL.Scheme,
		Host:   req.URL.Host,
		Path:   fmt.Sprintf("/packages/%s/%s/%s", pkg, f.OwningTag, f.Filename),
	}
	if f.Sha256 != "" {
		u.Fragment = "sha256=" + f.Sha256
	}
	return u.String()
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
