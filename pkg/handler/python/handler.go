package python

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
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
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "python"
	ArtifactType = "application/vnd.ocifactory.python"

	maxPackageLength = 256
	maxVersionLength = 128

	// maxMultipartMemory is the in-memory threshold for ParseMultipartForm.
	// Files larger than this spill to a temp file (auto-cleaned via
	// MultipartForm.RemoveAll), so streaming is preserved for large
	// wheels while small uploads stay in memory.
	maxMultipartMemory = 32 << 20

	// defaultMaxUploadBytes caps the total request-body size accepted
	// by handleFilePut. ParseMultipartForm's disk-spill is unbounded
	// without this — an authenticated attacker could fill the temp
	// disk with a single oversized request. 1 GiB comfortably exceeds
	// every wheel and sdist on PyPI today; operators can tighten it
	// via WithMaxUploadBytes.
	defaultMaxUploadBytes = 1 << 30

	// metadataResolveConcurrency bounds the number of concurrent
	// .metadata fetches the simple-index renderer issues on a cache
	// miss. Big enough to amortise round-trip latency for a package
	// with many versions, small enough not to flood the OCI backend.
	metadataResolveConcurrency = 8

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

type repoFile struct {
	oci.RepoFile
	Content io.ReadCloser
}

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
// the handler will accept. The default is defaultMaxUploadBytes
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
		maxUploadBytes:      defaultMaxUploadBytes,
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
// Multipart parsing is two-pass via ParseMultipartForm so name, version,
// and content can arrive in any order — RFC 7578 doesn't require a
// fixed order, and assuming twine's ordering was a latent bug. Files
// larger than maxMultipartMemory spill to a temp file, so streaming is
// preserved for large wheels.
//
// On wheel uploads the handler also extracts `*.dist-info/METADATA`
// from the wheel zip and stores it as a sibling file (PEP 658). Failure
// to extract is logged but does not block the upload — the wheel still
// publishes; clients just lose the metadata fast-path for that file.
func (h *Handler) handleFilePut(w http.ResponseWriter, req *http.Request) {
	logger := logging.FromContext(req.Context())

	// Cap the total request body before ParseMultipartForm spills past
	// maxMultipartMemory into a temp file. Without this, a single
	// authenticated upload can fill the temp disk.
	if h.maxUploadBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, h.maxUploadBytes)
	}

	if err := req.ParseMultipartForm(maxMultipartMemory); err != nil {
		var maxBytesErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytesErr):
			http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
		case errors.Is(err, http.ErrNotMultipart) || errors.Is(err, http.ErrMissingBoundary):
			http.Error(w, "missing boundary in request or not a multipart request", http.StatusBadRequest)
		default:
			logger.DebugContext(req.Context(), "failed to parse multipart form", "error", err)
			http.Error(w, "request body is not valid form data", http.StatusBadRequest)
		}
		return
	}
	defer func() {
		if req.MultipartForm != nil {
			_ = req.MultipartForm.RemoveAll()
		}
	}()

	pkgName := firstFormValue(req.MultipartForm, "name")
	versionNum := firstFormValue(req.MultipartForm, "version")

	contentFiles := req.MultipartForm.File["content"]
	if pkgName == "" || versionNum == "" || len(contentFiles) == 0 {
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

	normalizedName := normalize(pkgName)
	contentHeader := contentFiles[0]
	contentName := contentHeader.Filename
	if contentName == "" {
		http.Error(w, "missing filename for content", http.StatusBadRequest)
		return
	}
	if !uploadFilenameRegExp.MatchString(contentName) || contentName == "." || contentName == ".." {
		http.Error(w, "invalid filename for content", http.StatusBadRequest)
		return
	}

	contentFile, err := contentHeader.Open()
	if err != nil {
		logger.ErrorContext(req.Context(), "failed to open uploaded content", "error", err)
		http.Error(w, "failed to read content", http.StatusInternalServerError)
		return
	}
	// Ownership of contentFile is handed to handlePut at the bottom of
	// this function. Any early return before that point must close it
	// directly.

	var metaBytes []byte
	if isWheelFilename(contentName) {
		meta, mErr := extractWheelMetadata(contentFile, contentHeader.Size)
		switch {
		case mErr == nil:
			metaBytes = meta
		case errors.Is(mErr, errMetadataNotFound):
			// PEP 427 wheels SHOULD include METADATA but the upload
			// is still useful without it; just don't advertise PEP
			// 658 for this file.
			logger.DebugContext(req.Context(), "wheel has no METADATA member", "filename", contentName)
		default:
			logger.WarnContext(req.Context(), "failed to extract wheel METADATA", "error", mErr, "filename", contentName)
		}
		// Rewind regardless: subsequent AddFile must stream from the
		// start of the wheel.
		if _, err := contentFile.Seek(0, io.SeekStart); err != nil {
			contentFile.Close()
			logger.ErrorContext(req.Context(), "failed to rewind wheel content", "error", err)
			http.Error(w, "failed to read content", http.StatusInternalServerError)
			return
		}
	}

	uploads := []*repoFile{
		{
			RepoFile: oci.RepoFile{
				OwningRepo: "packages/" + normalizedName,
				OwningTag:  versionNum,
				Name:       contentName,
				MediaType:  detectMediaType(contentName),
				Size:       contentHeader.Size,
			},
			Content: contentFile,
		},
	}
	if metaBytes != nil {
		uploads = append(uploads, &repoFile{
			RepoFile: oci.RepoFile{
				OwningRepo: "packages/" + normalizedName,
				OwningTag:  versionNum,
				Name:       metadataCompanionName(contentName),
				MediaType:  detectMediaType(metadataCompanionName(contentName)),
				Size:       int64(len(metaBytes)),
			},
			Content: io.NopCloser(bytes.NewReader(metaBytes)),
		})
	}
	// The index repo write is a single sentinel per package, not per
	// version. handleSimpleIndex only reads tag names from "index" via
	// ListTags, so storing one constant placeholder layer under
	// index/<normalizedName> is enough — the OCI backend deduplicates
	// the identical blob and file manifest across every subsequent
	// upload of the same package.
	uploads = append(uploads, &repoFile{
		RepoFile: oci.RepoFile{
			OwningRepo: "index",
			OwningTag:  normalizedName,
			Name:       indexSentinelName,
			MediaType:  "text/plain",
		},
		Content: io.NopCloser(strings.NewReader(indexSentinelContent)),
	})

	if h.handlePut(req.Context(), w, uploads) {
		// A successful publish changes what /simple/<pkg>/ should
		// render; drop the cached file list so the next render
		// fetches fresh from the backend.
		h.indexCache.invalidate(normalizedName)
	}
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
			Filename:       f.Filename,
			URL:            renderedFileURL(req, pkg, f),
			Sha256:         f.Sha256,
			MetadataSha256: f.MetadataSha256,
			RequiresPython: f.RequiresPython,
		})
	}

	if pickContentType(req.Header.Get("Accept")) == contentTypeJSONv1 {
		writeJSONPackageIndex(w, pkg, rendered)
		return
	}
	h.renderer.RenderHTML(w, "simple.html", indexPage{Title: pkg, Files: rendered})
}

// resolvePackageFiles enumerates a package's files plus the per-wheel
// PEP 658 metadata + Requires-Python headers. The returned slice is
// what goes into the simple-index cache; both HTML and JSON renderers
// pivot off it.
//
// .metadata companion files are not returned in the slice — they are
// consumed as siblings of the wheels they describe. Their Digest gives
// us the PEP 658 hash without re-fetching, and their content is read
// once to extract Requires-Python (bounded concurrency, best-effort:
// unreadable companions log a warning and leave Requires-Python empty
// rather than failing the whole render).
func (h *Handler) resolvePackageFiles(ctx context.Context, pkg string) ([]cachedFile, error) {
	logger := logging.FromContext(ctx)

	rawFiles, err := h.registry.ListFiles(ctx, "packages/"+pkg)
	if err != nil {
		return nil, err
	}

	type sibKey struct{ tag, name string }
	siblings := make(map[sibKey]*oci.RepoFile, len(rawFiles))
	for _, f := range rawFiles {
		siblings[sibKey{tag: f.OwningTag, name: f.Name}] = f
	}

	entries := make([]cachedFile, 0, len(rawFiles))
	for _, f := range rawFiles {
		if strings.HasSuffix(f.Name, ".metadata") {
			// Companion file; surfaced via its sibling wheel below.
			continue
		}
		entry := cachedFile{
			Filename:  f.Name,
			OwningTag: f.OwningTag,
			Sha256:    strings.TrimPrefix(f.Digest, "sha256:"),
		}
		if isWheelFilename(f.Name) {
			if meta, ok := siblings[sibKey{tag: f.OwningTag, name: metadataCompanionName(f.Name)}]; ok {
				entry.MetadataSha256 = strings.TrimPrefix(meta.Digest, "sha256:")
			}
		}
		entries = append(entries, entry)
	}

	// Resolve Requires-Python for every wheel that has a companion.
	// Concurrent fetches with a small limit; per-fetch errors that
	// aren't context cancellation are best-effort (log and leave the
	// field empty) so a single broken companion doesn't tank the
	// whole render. Context cancellation IS propagated so the caller
	// skips caching partially-resolved entries.
	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(metadataResolveConcurrency)
	for i := range entries {
		e := &entries[i]
		if e.MetadataSha256 == "" || !isWheelFilename(e.Filename) {
			continue
		}
		eg.Go(func() error {
			rp, rerr := h.readRequiresPython(gctx, pkg, e.OwningTag, e.Filename)
			if rerr != nil {
				if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
					return rerr
				}
				logger.WarnContext(gctx, "failed to read wheel METADATA companion",
					"package", pkg, "version", e.OwningTag, "filename", e.Filename, "error", rerr)
				return nil
			}
			e.RequiresPython = rp
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return entries, nil
}

func (h *Handler) readRequiresPython(ctx context.Context, pkg, tag, wheelName string) (string, error) {
	f := &oci.RepoFile{
		OwningRepo: "packages/" + pkg,
		OwningTag:  tag,
		Name:       metadataCompanionName(wheelName),
	}
	_, rc, err := h.registry.ReadFile(ctx, f)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxMetadataSize+1))
	if err != nil {
		return "", err
	}
	return parseRequiresPython(data), nil
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

// handlePut writes every file in fs to the registry. It is also
// responsible for the HTTP response: on success it writes 201 Created and
// returns true; on the first failure it writes the appropriate error
// status and returns false. The boolean return lets handleFilePut perform
// post-write side effects (e.g. cache invalidation) only when the upload
// actually succeeded.
//
// handlePut closes each entry's Content as soon as its AddFile completes
// rather than deferring to function return; for a 3-entry batch
// (wheel + .metadata + index sentinel) the eager close keeps the
// multipart temp file open for one upload at a time instead of all three.
func (h *Handler) handlePut(ctx context.Context, w http.ResponseWriter, fs []*repoFile) bool {
	logger := logging.FromContext(ctx)

	// On any early-return path that hasn't reached the eager-close yet
	// we still need to release the remaining Content readers.
	cleanupFrom := 0
	defer func() {
		for i := cleanupFrom; i < len(fs); i++ {
			_ = fs[i].Content.Close()
		}
	}()

	for i, f := range fs {
		desc, err := h.registry.AddFile(ctx, &f.RepoFile, f.Content)
		_ = f.Content.Close()
		cleanupFrom = i + 1
		if err != nil {
			logger.DebugContext(ctx, "failed to add file", "error", err)
			if oci.HasCode(err, http.StatusUnauthorized) {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return false
			}
			if oci.HasCode(err, http.StatusForbidden) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return false
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return false
		}
		logger.DebugContext(ctx, "added file", "descriptor", desc)
	}
	w.WriteHeader(http.StatusCreated)
	return true
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

// firstFormValue returns the first value for a multipart form field, or
// "" if the field is absent or empty. ParseMultipartForm puts every
// occurrence into a slice; for single-valued fields we want the first
// non-empty entry.
func firstFormValue(form *multipart.Form, name string) string {
	if form == nil {
		return ""
	}
	for _, v := range form.Value[name] {
		if v != "" {
			return v
		}
	}
	return ""
}
