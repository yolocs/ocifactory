// Package npm implements the npm registry HTTP protocol on top of an
// OCI backend. The endpoints implemented cover `npm publish`, `npm
// install`, and `npm dist-tag add|ls|rm` — see
// [`docs/repos/npm.md`](../../../docs/repos/npm.md) for the operator-
// facing description.
package npm

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/proxy/indexcache"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "npm"
	ArtifactType = "application/vnd.ocifactory.npm"

	// DefaultMaxUploadBytes caps the total request-body size accepted
	// by the publish endpoint. npm publish bodies are JSON with a
	// base64-encoded tarball, so a 100 MiB tarball balloons to
	// ~134 MiB on the wire. 1 GiB leaves slack for the largest
	// packages real users publish; operators tighten via
	// --npm-max-upload-bytes / [WithMaxUploadBytes].
	DefaultMaxUploadBytes int64 = 1 << 30

	// maxVersionLength caps the length of any single version string
	// the publish path will accept. Real semver is far below this;
	// the cap exists to keep a malicious client from piling
	// unbounded characters into an OCI tag.
	maxVersionLength = 128

	// maxTarballBytes caps the size of any single base64-decoded
	// _attachments entry. npm itself caps tarballs at 100 MiB; 500
	// MiB leaves slack for the largest legitimate packages while
	// stopping a publisher from declaring a tiny version and
	// stuffing a 750+ MiB blob into the attachment to amplify
	// memory pressure through the JSON decode + base64 decode
	// pipeline. Lives separately from DefaultMaxUploadBytes (the
	// outer wire cap) because per-attachment bounds let us reject
	// pathological multi-version publishes the outer cap would
	// otherwise admit.
	maxTarballBytes int64 = 500 << 20

	// indexSentinelName / indexSentinelContent mirror python: a
	// constant placeholder layer whose presence is what we read off
	// the index repo's tag list. The body is unused.
	indexSentinelName    = "present"
	indexSentinelContent = "1"

	// versionMetaName is the layer name we store the per-version
	// packument fragment under. Same `(OwningRepo, OwningTag)` as the
	// tarball so a single AddFile pair per version captures both the
	// payload and the metadata reader path needs to reconstruct the
	// packument.
	versionMetaName = "package.json"

	// distTagsMediaType is the content type we serve dist-tag GET
	// responses with. npm sends Accept: application/json but is
	// happy with the bare octet-stream too; setting it explicitly
	// keeps reverse-proxy caching layers happy.
	distTagsMediaType = "application/json"
)

// Handler is the npm format handler. Build one with [NewHandler] and
// register its mux via [Handler.Mux].
type Handler struct {
	registry       *namespace.Registry
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64
	proxy          *proxyState
}

// Option configures optional Handler behaviour. Constructed via the
// With* helpers below.
type Option func(*handlerConfig)

type handlerConfig struct {
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64

	fetcherFactory       FetcherFactory
	indexCache           *indexcache.Cache
	negCache             *indexcache.NegativeCache
	proxyIndexCacheTTL   time.Duration
	proxyL1IndexCacheTTL time.Duration
}

// WithAuthMiddleware installs an authentication middleware on every
// route the handler exposes. Pass nil (or omit the option) to leave
// routes ungated — production wiring (cmd/ocifactory serve) always
// supplies one; tests use the absent form to construct a handler
// without dragging in pkg/auth.
//
// The npm handler chains the middleware on a `/{namespace}` sub-router
// so every npm route requires a verified AuthContext. Mirrors the
// python and maven conventions; there are no public-by-default
// endpoints in v1.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) {
		c.authMW = mw
	}
}

// WithMaxUploadBytes caps the total publish-body size the handler
// will accept. Defaults to [DefaultMaxUploadBytes]. A non-positive
// value disables the cap (not recommended).
func WithMaxUploadBytes(n int64) Option {
	return func(c *handlerConfig) {
		c.maxUploadBytes = n
	}
}

// NewHandler constructs a new npm format handler.
//
// registry is the data-plane wrapper that hands out per-request
// [*namespace.ScopedRegistry] views via [namespace.Registry.For]. Every
// routed handler resolves the namespace from the request URL
// (`/{namespace}/...`) and obtains a scoped view at the top.
func NewHandler(registry *namespace.Registry, opts ...Option) (*Handler, error) {
	if registry == nil {
		return nil, errors.New("registry must not be nil")
	}
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

// Mux returns the npm handler's router. Every npm route lives under
// `/{namespace}/...` so the handler resolves the namespace from the
// URL on each request and routes through the namespace-scoped
// registry view.
func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	router.Use(mux.MiddlewareFunc(handler.RouteNameOpMiddleware))

	nsr := router.PathPrefix("/{namespace}").Subrouter()
	if h.authMW != nil {
		nsr.Use(mux.MiddlewareFunc(h.authMW))
	}

	// Package read APIs. The regex on {package} keeps scoped vs
	// unscoped routing inside one route — gorilla/mux matches the
	// most specific path-with-segments path first, so the dist-tags
	// and tarball routes need to come before the bare-package route.

	// Dist-tag endpoints — npm dist-tag ls / add / rm.
	nsr.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags", h.handleDistTagList).Methods(http.MethodGet, http.MethodHead).Name("list")
	nsr.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags/{tag}", h.handleDistTagPut).Methods(http.MethodPut, http.MethodPost).Name("write")
	nsr.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags/{tag}", h.handleDistTagDelete).Methods(http.MethodDelete).Name("write")

	// Tarball download.
	nsr.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-/{filename:.+\\.tgz}", h.handleTarballGet).Methods(http.MethodGet, http.MethodHead).Name("read")

	// Out-of-scope endpoints: unpublish (yank), npm login, whoami,
	// search. Each gets a deliberate 404 so a curious client doesn't
	// see a stack trace.
	nsr.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-rev/{revision}", h.handleUnsupported).Methods(http.MethodDelete).Name("read")
	nsr.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-/{filename:.+\\.tgz}/-rev/{revision}", h.handleUnsupported).Methods(http.MethodDelete).Name("read")
	nsr.HandleFunc("/-/user/{username:org\\.couchdb\\.user:[^/]+}", h.handleUnsupported).Methods(http.MethodPut).Name("read")
	nsr.HandleFunc("/-/whoami", h.handleUnsupported).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/-/npm/v1/user", h.handleUnsupported).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/-/v1/search", h.handleUnsupported).Methods(http.MethodGet, http.MethodHead).Name("read")

	// Publish.
	nsr.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}", h.handlePublish).Methods(http.MethodPut).Name("write")

	// Packument (full package metadata document).
	nsr.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}", h.handlePackument).Methods(http.MethodGet, http.MethodHead).Name("read")

	// Ping / registry root probe.
	nsr.HandleFunc("/-/ping", h.handlePing).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/", h.handlePing).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("", h.handlePing).Methods(http.MethodGet, http.MethodHead).Name("read")

	return router
}

// scopedFor returns the per-request [*namespace.ScopedRegistry] for
// the namespace in req's URL. Cheap to construct; called per request.
func (h *Handler) scopedFor(req *http.Request) *namespace.ScopedRegistry {
	return h.registry.For(mux.Vars(req)["namespace"])
}

// handlePing answers npm's registry-root probe. The body shape is
// loosely defined upstream — npm's ping prints whatever the registry
// returns. An empty JSON object keeps clients happy and gives the
// server log a recognisable response.
func (h *Handler) handlePing(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write([]byte("{}"))
	}
}

// handleUnsupported is the explicit 404 for v1 out-of-scope endpoints
// (yank, login, whoami, search). Returning 404 rather than 501 keeps
// the npm client from treating the registry as broken — it just
// behaves as though the feature isn't available.
func (h *Handler) handleUnsupported(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodDelete {
		scoped := h.scopedFor(req)
		if _, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
			return
		} else if isProxy {
			http.Error(w, "writes disabled on proxy namespaces", http.StatusMethodNotAllowed)
			return
		}
	}
	http.Error(w, "operation not supported by this registry", http.StatusNotFound)
}

// handlePublish accepts the CouchDB-style npm publish document. The
// body shape is documented in the issue (one packument with
// `_attachments` carrying the base64-encoded tarball and `versions`
// carrying per-version metadata).
//
// Flow:
//  1. Cap the body via MaxBytesReader so an oversize publish never
//     pays for the JSON decode.
//  2. Decode the JSON.
//  3. Validate the URL package name against the body's name.
//  4. For each version in `versions`:
//     a. Decode the matching `_attachments[<short>-<v>.tgz]`.
//     b. Recompute the sha1 / sha512 checksums against `dist.shasum`
//     and `dist.integrity`, rejecting any mismatch.
//     c. AddFile the tarball (the .tgz layer).
//     d. AddFile the version metadata (`package.json` layer in the
//     same OCI tag).
//  5. For each dist-tag mapping, AppendRefs to the named version.
//  6. Best-effort: write the per-package index sentinel.
//  7. Respond 201 with the standard `{"ok": true, "id": ..., "rev": ...}` shape.
func (h *Handler) handlePublish(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)
	scoped := h.scopedFor(req)

	if _, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		http.Error(w, "publishes disabled on proxy namespaces", http.StatusMethodNotAllowed)
		return
	}

	if h.maxUploadBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, h.maxUploadBytes)
	}
	defer req.Body.Close()

	pkgFromURL := mux.Vars(req)["package"]
	if err := validatePackageName(pkgFromURL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		logger.DebugContext(ctx, "failed to read npm publish body", "error", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var doc publishDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		logger.DebugContext(ctx, "failed to decode npm publish body", "error", err)
		http.Error(w, "request body is not a valid npm publish document", http.StatusBadRequest)
		return
	}

	if doc.Name != pkgFromURL {
		http.Error(w, fmt.Sprintf("body name %q does not match URL %q", doc.Name, pkgFromURL), http.StatusBadRequest)
		return
	}
	if len(doc.Versions) == 0 {
		http.Error(w, "publish document must contain at least one version", http.StatusBadRequest)
		return
	}

	repo := packageOwningRepo(pkgFromURL)

	// Phase 1: validate every version's metadata and attachment up
	// front and stage the decoded tarballs. Nothing here touches the
	// OCI backend, so a single bad sha512 / oversized attachment /
	// dist-tag pointing at a non-existent version fails the whole
	// publish before any side effect lands. The npm CLI publishes
	// one version at a time in practice; the staging map is rarely
	// larger than 1.
	type stagedVersion struct {
		raw         json.RawMessage
		tarball     []byte
		tarballName string
	}
	staged := make(map[string]stagedVersion, len(doc.Versions))

	for version, raw := range doc.Versions {
		if version == "" {
			http.Error(w, "publish document contains an empty version key", http.StatusBadRequest)
			return
		}
		if len(version) > maxVersionLength {
			http.Error(w, "version is too long", http.StatusBadRequest)
			return
		}
		if !versionTagSafe(version) {
			http.Error(w, fmt.Sprintf("version %q contains characters that cannot be encoded as an OCI tag", version), http.StatusBadRequest)
			return
		}

		var meta versionMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			http.Error(w, fmt.Sprintf("version %q metadata is not valid JSON: %s", version, err), http.StatusBadRequest)
			return
		}
		if meta.Version != "" && meta.Version != version {
			http.Error(w, fmt.Sprintf("version key %q does not match versions[%q].version=%q", version, version, meta.Version), http.StatusBadRequest)
			return
		}
		if meta.Name != "" && meta.Name != pkgFromURL {
			http.Error(w, fmt.Sprintf("versions[%q].name=%q does not match package name %q", version, meta.Name, pkgFromURL), http.StatusBadRequest)
			return
		}

		// npm keys `_attachments` by the full package name + version
		// ("@scope/foo-1.0.0.tgz" for scoped, "foo-1.0.0.tgz" for
		// unscoped). The stored OCI layer name, however, drops the
		// "@scope/" prefix to avoid a slash inside a filename segment
		// of the rewritten download URL.
		attachKey := attachmentKey(pkgFromURL, version)
		attachment, ok := doc.Attachments[attachKey]
		if !ok {
			http.Error(w, fmt.Sprintf("missing attachment for tarball %q", attachKey), http.StatusBadRequest)
			return
		}
		tarballBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(attachment.Data))
		if err != nil {
			http.Error(w, fmt.Sprintf("attachment %q data is not valid base64: %s", attachKey, err), http.StatusBadRequest)
			return
		}
		if int64(len(tarballBytes)) > maxTarballBytes {
			http.Error(w, fmt.Sprintf("tarball %q exceeds %d-byte per-tarball cap", attachKey, maxTarballBytes), http.StatusRequestEntityTooLarge)
			return
		}
		if err := verifyChecksums(tarballBytes, meta.Dist); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Drop the base64-encoded string from the parsed document so
		// GC can reclaim it — we already have the decoded bytes
		// staged and don't need the source representation again.
		attachment.Data = ""
		doc.Attachments[attachKey] = attachment

		staged[version] = stagedVersion{raw: raw, tarball: tarballBytes, tarballName: tarballFilename(pkgFromURL, version)}
	}

	for tag, version := range doc.DistTags {
		if !distTagSafe(tag) {
			http.Error(w, fmt.Sprintf("dist-tag %q contains characters that cannot be encoded as an OCI tag", tag), http.StatusBadRequest)
			return
		}
		if version == "" {
			http.Error(w, fmt.Sprintf("dist-tag %q points at empty version", tag), http.StatusBadRequest)
			return
		}
		if _, ok := staged[version]; !ok {
			// npm publish documents only carry dist-tags pointing
			// at versions in the same body. Targets not in the
			// staged set are rejected here so a typo'd publisher
			// gets a clean 400 instead of leaving the publish
			// half-applied. Operators reassign dist-tags to
			// previously-published versions via the direct
			// `npm dist-tag add` endpoint.
			http.Error(w, fmt.Sprintf("dist-tag %q points at version %q which is not included in this publish", tag, version), http.StatusBadRequest)
			return
		}
	}

	// Phase 2: write every staged version's tarball + metadata
	// layers. By construction nothing here can fail validation —
	// only an OCI-backend error (or a 409 from a duplicate version)
	// remains. A partial-publish across multiple versions in this
	// loop is unrecoverable; npm publishes are single-version in
	// practice, so the staged map almost always has size 1.
	for version, s := range staged {
		tarballRF := &oci.RepoFile{
			OwningRepo: repo,
			OwningTag:  version,
			Name:       s.tarballName,
			MediaType:  "application/octet-stream",
			Size:       int64(len(s.tarball)),
		}
		if !h.addFile(ctx, scoped, w, tarballRF, bytes.NewReader(s.tarball)) {
			return
		}

		metaRF := &oci.RepoFile{
			OwningRepo: repo,
			OwningTag:  version,
			Name:       versionMetaName,
			MediaType:  "application/json",
			Size:       int64(len(s.raw)),
		}
		if !h.addFile(ctx, scoped, w, metaRF, bytes.NewReader(s.raw)) {
			return
		}
	}

	// Phase 3: dist-tag updates. Each target is already known to
	// reference a staged (and now-written) canonical version.
	for tag, version := range doc.DistTags {
		if err := scoped.AppendRefs(ctx, repo, version, tag); err != nil {
			h.writeRegistryError(ctx, w, err, "failed to update dist-tag")
			return
		}
	}

	if err := h.ensureIndexSentinel(ctx, scoped, pkgFromURL); err != nil {
		// Best-effort — the package is published either way. Log
		// at WARN so the operator notices a systematic problem
		// (e.g. a permissions error on the index repo) but don't
		// fail the publish.
		logger.WarnContext(ctx, "failed to write index sentinel; publish succeeded anyway",
			"package", pkgFromURL, "error", err,
		)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":  true,
		"id":  pkgFromURL,
		"rev": newRev(),
	})
}

// addFile wraps scoped.AddFile with the standard error → HTTP
// translation used by every npm publish step. Returns true on success
// and writes the appropriate error response (and returns false) on
// failure.
func (h *Handler) addFile(ctx context.Context, scoped handler.Registry, w http.ResponseWriter, f *oci.RepoFile, body io.Reader) bool {
	if _, err := scoped.AddFile(ctx, f, body); err != nil {
		h.writeRegistryError(ctx, w, err, "failed to add file")
		return false
	}
	return true
}

// writeRegistryError translates a scoped-registry error into a
// canonical HTTP status. Mirrors the python / maven handler's mapping
// table so an operator triaging an npm publish sees the same status
// codes they would for an analogous failure in another format.
func (h *Handler) writeRegistryError(ctx context.Context, w http.ResponseWriter, err error, public string) {
	logger := logging.FromContext(ctx)
	logger.DebugContext(ctx, "npm handler registry error", "error", err)
	if handler.WriteNamespaceError(w, err) {
		return
	}
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
	case errors.Is(err, oci.ErrAlreadyExists):
		http.Error(w, "version already published", http.StatusConflict)
	case errors.Is(err, oci.ErrAliasCollision):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errdef.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case oci.HasCode(err, http.StatusUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case oci.HasCode(err, http.StatusForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, public)
	}
}

// handlePackument builds the full packument JSON document for one
// package by enumerating canonical version tags, fetching the per-
// version metadata layer, and resolving dist-tag aliases.
//
// dist.tarball URLs are rewritten to point at this server rather than
// the upstream npm registry the publisher originally encoded — npm
// clients follow the URL in the response and we want them to come
// back to us, not to npmjs.org.
func (h *Handler) handlePackument(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scoped := h.scopedFor(req)
	pkg := mux.Vars(req)["package"]

	if err := validatePackageName(pkg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		h.handlePackumentProxy(w, req, scoped, spec, pkg)
		return
	}

	out, status, err := h.buildPackument(ctx, req, scoped, pkg)
	if err != nil {
		if status == http.StatusNotFound {
			http.Error(w, "package not found", http.StatusNotFound)
			return
		}
		if status == 0 {
			h.writeRegistryError(ctx, w, err, "failed to build packument")
			return
		}
		handler.WriteError(ctx, w, status, err, "failed to build packument")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodHead {
		// No body for HEAD, but a Content-Length would be a lie
		// without serialising the whole document; skip it.
		return
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "failed to encode packument")
		return
	}
}

func (h *Handler) buildPackument(ctx context.Context, req *http.Request, scoped handler.Registry, pkg string) (*packument, int, error) {
	repo := packageOwningRepo(pkg)

	files, err := scoped.ListFiles(ctx, repo)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, http.StatusNotFound, err
		}
		return nil, 0, err
	}
	if len(files) == 0 {
		return nil, http.StatusNotFound, errdef.ErrNotFound
	}

	// Group canonical files by version. Each (version, file) row in
	// the resulting map tells us whether the version is real (has a
	// package.json) and what the tarball's digest is for the
	// rewritten dist block.
	type versionFiles struct {
		tarballName   string
		tarballDigest string
		metaDigest    string
	}
	byVersion := map[string]*versionFiles{}
	for _, f := range files {
		vf, ok := byVersion[f.OwningTag]
		if !ok {
			vf = &versionFiles{}
			byVersion[f.OwningTag] = vf
		}
		switch {
		case f.Name == versionMetaName:
			vf.metaDigest = f.Digest
		case strings.HasSuffix(f.Name, ".tgz"):
			vf.tarballName = f.Name
			vf.tarballDigest = f.Digest
		}
	}

	out := packument{
		ID:       pkg,
		Name:     pkg,
		DistTags: map[string]string{},
		Versions: map[string]json.RawMessage{},
	}

	canonicalSet := map[string]struct{}{}
	for version, vf := range byVersion {
		if vf.metaDigest == "" {
			// No metadata layer — skip; the version was never
			// fully published. Likeliest cause is an aborted
			// publish midway through; ignore rather than fail
			// the whole packument.
			continue
		}
		canonicalSet[version] = struct{}{}

		metaRF := &oci.RepoFile{
			OwningRepo: repo,
			OwningTag:  version,
			Name:       versionMetaName,
			MediaType:  "application/json",
		}
		_, rc, err := scoped.ReadFile(ctx, metaRF)
		if err != nil {
			return nil, 0, err
		}
		raw, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			return nil, http.StatusInternalServerError, rerr
		}

		// Rewrite dist.tarball to point at this server. Preserve
		// every other field by round-tripping through a generic
		// map. json.RawMessage is fine for storage but doesn't
		// let us mutate.
		//
		// The dist field is replaced unconditionally — if a
		// publisher submitted dist as an array, a string, or any
		// other non-map shape, we still control the tarball URL
		// our clients see. A malicious publisher cannot leave the
		// original (e.g. registry.npmjs.org) tarball URL in the
		// served packument by sending a weird dist shape.
		var versionDoc map[string]any
		if err := json.Unmarshal(raw, &versionDoc); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		dist, ok := versionDoc["dist"].(map[string]any)
		if !ok {
			dist = map[string]any{}
			versionDoc["dist"] = dist
		}
		if vf.tarballName != "" {
			dist["tarball"] = tarballURL(req, pkg, vf.tarballName)
		}
		// We do NOT fabricate dist.shasum from the OCI layer
		// digest — that digest is sha256 while npm clients
		// expect sha1 in dist.shasum, and serving a 64-char
		// "sha1" makes clients fail at verification. With the
		// publish-time requirement that every publish carries
		// dist.shasum and/or dist.integrity (see verifyChecksums),
		// the stored metadata always has at least one valid
		// checksum that round-trips here untouched.
		merged, mErr := json.Marshal(versionDoc)
		if mErr != nil {
			return nil, http.StatusInternalServerError, mErr
		}
		out.Versions[version] = merged
	}

	if len(out.Versions) == 0 {
		return nil, http.StatusNotFound, errdef.ErrNotFound
	}

	// Resolve dist-tags. Every tag that ListTags returns but
	// ListFiles didn't cover (those are the canonical versions) is
	// an alias. We resolve each alias by fetching its package.json
	// via RefTag and matching the descriptor digest back against
	// the canonical version we already know.
	allTags, tagErr := scoped.ListTags(ctx, repo)
	if tagErr != nil && !errors.Is(tagErr, errdef.ErrNotFound) {
		return nil, 0, tagErr
	}
	digestByVersion := map[string]string{}
	for version, vf := range byVersion {
		if vf.metaDigest != "" {
			digestByVersion[vf.metaDigest] = version
		}
	}
	for _, tag := range allTags {
		if _, isCanonical := canonicalSet[tag]; isCanonical {
			continue
		}
		aliasRF := &oci.RepoFile{
			OwningRepo: repo,
			RefTag:     tag,
			Name:       versionMetaName,
			MediaType:  "application/json",
		}
		desc, rc, err := scoped.ReadFile(ctx, aliasRF)
		if err != nil {
			// Stray alias we can't resolve — skip rather than
			// failing the whole packument.
			continue
		}
		_ = rc.Close()
		target := digestByVersion[desc.File.Digest.String()]
		if target == "" {
			continue
		}
		out.DistTags[tag] = target
	}

	// No server-side fallback for an unset "latest" dist-tag. The
	// npm CLI always sets `dist-tags.latest` on publish; if it's
	// missing the publisher explicitly chose to omit it and `npm
	// install foo` should fail with "no matching version" the same
	// way it would against npmjs.org. Lexical sorting was wrong
	// (10.0.0 < 2.0.0 lexically) and proper semver isn't worth
	// pulling in for a behaviour npm doesn't require.
	return &out, 0, nil
}

// handleTarballGet streams a .tgz blob back to the client. Mirrors
// the maven / python file-get flow: try a backend redirect, fall back
// to streaming through ocifactory.
func (h *Handler) handleTarballGet(w http.ResponseWriter, req *http.Request) {
	scoped := h.scopedFor(req)
	vars := mux.Vars(req)
	pkg := vars["package"]
	filename := vars["filename"]

	if err := validatePackageName(pkg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.Error(w, "invalid tarball filename", http.StatusBadRequest)
		return
	}
	version, err := tarballVersion(pkg, filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f := &oci.RepoFile{
		OwningRepo: packageOwningRepo(pkg),
		OwningTag:  version,
		Name:       filename,
		MediaType:  "application/octet-stream",
	}

	if spec, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		h.handleTarballGetProxy(w, req, scoped, spec, f)
		return
	}

	if !h.tryServeTarballFromRegistry(w, req, scoped, f) {
		http.Error(w, "tarball not found", http.StatusNotFound)
		return
	}
}

// handleDistTagList answers `npm dist-tag ls`. Resolves the
// alias-target map the same way [handlePackument] does.
func (h *Handler) handleDistTagList(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scoped := h.scopedFor(req)
	pkg := mux.Vars(req)["package"]
	if err := validatePackageName(pkg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo := packageOwningRepo(pkg)

	tags, err := scoped.ListTags(ctx, repo)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			http.Error(w, "package not found", http.StatusNotFound)
			return
		}
		h.writeRegistryError(ctx, w, err, "failed to list tags")
		return
	}
	files, err := scoped.ListFiles(ctx, repo)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			http.Error(w, "package not found", http.StatusNotFound)
			return
		}
		h.writeRegistryError(ctx, w, err, "failed to list files")
		return
	}

	// A package with no canonical files is not a published package
	// — back-end registries return an empty list (or normalize a
	// real NAME_UNKNOWN to ErrNotFound, handled above), and so does
	// the fake. Either way, surface 404 so dist-tag ls of an
	// unknown package looks the same as packument GET of the same
	// package.
	if len(files) == 0 {
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}

	canonicalSet := map[string]struct{}{}
	digestByVersion := map[string]string{}
	for _, f := range files {
		canonicalSet[f.OwningTag] = struct{}{}
		if f.Name == versionMetaName {
			digestByVersion[f.Digest] = f.OwningTag
		}
	}

	out := map[string]string{}
	for _, tag := range tags {
		if _, isCanonical := canonicalSet[tag]; isCanonical {
			continue
		}
		aliasRF := &oci.RepoFile{
			OwningRepo: repo,
			RefTag:     tag,
			Name:       versionMetaName,
			MediaType:  "application/json",
		}
		desc, rc, err := scoped.ReadFile(ctx, aliasRF)
		if err != nil {
			continue
		}
		_ = rc.Close()
		if v, ok := digestByVersion[desc.File.Digest.String()]; ok {
			out[tag] = v
		}
	}

	w.Header().Set("Content-Type", distTagsMediaType)
	if req.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

// handleDistTagPut accepts `npm dist-tag add` and assigns an alias.
// Body is a JSON-encoded version string ("1.2.3", quoted).
func (h *Handler) handleDistTagPut(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scoped := h.scopedFor(req)
	vars := mux.Vars(req)
	pkg := vars["package"]
	tag := vars["tag"]

	if _, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		http.Error(w, "dist-tag writes disabled on proxy namespaces", http.StatusMethodNotAllowed)
		return
	}

	if err := validatePackageName(pkg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !distTagSafe(tag) {
		http.Error(w, "invalid dist-tag", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, 4096))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()
	version := strings.TrimSpace(string(body))
	// npm sends a JSON-encoded string (with quotes); accept either
	// quoted or bare to be forgiving.
	if len(version) >= 2 && version[0] == '"' && version[len(version)-1] == '"' {
		var unquoted string
		if err := json.Unmarshal([]byte(version), &unquoted); err != nil {
			http.Error(w, "invalid dist-tag body", http.StatusBadRequest)
			return
		}
		version = unquoted
	}
	if version == "" {
		http.Error(w, "dist-tag body must be a version string", http.StatusBadRequest)
		return
	}
	if len(version) > maxVersionLength || !versionTagSafe(version) {
		http.Error(w, "invalid dist-tag target version", http.StatusBadRequest)
		return
	}

	repo := packageOwningRepo(pkg)
	if err := scoped.AppendRefs(ctx, repo, version, tag); err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			http.Error(w, fmt.Sprintf("version %q not found", version), http.StatusNotFound)
			return
		}
		h.writeRegistryError(ctx, w, err, "failed to update dist-tag")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleDistTagDelete drops a dist-tag alias. v1 returns 501 with a
// JSON body — the namespace wrapper does not yet expose a single-tag
// delete from the data plane, and adding one purely for dist-tag
// removal is more surface than the v1 issue justifies. Operators
// re-point unwanted dist-tags to a known version instead.
func (h *Handler) handleDistTagDelete(w http.ResponseWriter, req *http.Request) {
	scoped := h.scopedFor(req)
	if _, isProxy, ok := h.dispatchProxy(w, req, scoped); !ok {
		return
	} else if isProxy {
		http.Error(w, "dist-tag writes disabled on proxy namespaces", http.StatusMethodNotAllowed)
		return
	}
	http.Error(w, "dist-tag removal is not supported in this version", http.StatusNotImplemented)
}

// ensureIndexSentinel writes the per-package sentinel in the index
// repo iff no tag for the package exists yet — identical to python's
// pattern. The index repo's tags are encoded npm names; the layer
// body is a constant placeholder.
func (h *Handler) ensureIndexSentinel(ctx context.Context, scoped handler.Registry, npmName string) error {
	encoded, err := encodePackageNameTag(npmName)
	if err != nil {
		return err
	}
	tags, err := scoped.ListTags(ctx, "index")
	if err != nil && !errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("list index tags: %w", err)
	}
	for _, t := range tags {
		if t == encoded {
			return nil
		}
	}
	sentinelRF := &oci.RepoFile{
		OwningRepo: "index",
		OwningTag:  encoded,
		Name:       indexSentinelName,
		MediaType:  "text/plain",
		Size:       int64(len(indexSentinelContent)),
	}
	if _, err := scoped.AddFile(ctx, sentinelRF, strings.NewReader(indexSentinelContent)); err != nil && !errors.Is(err, oci.ErrAlreadyExists) {
		return fmt.Errorf("add index sentinel: %w", err)
	}
	return nil
}

// verifyChecksums recomputes the sha1 (dist.shasum) and sha512
// (dist.integrity, SRI-prefixed) checksums for the decoded tarball
// and returns an error when either disagrees with what the
// publisher claimed. At least one of the two must be supplied —
// the read path serves whichever fields the publisher submitted,
// so a publish carrying neither would land a tarball that
// downstream `npm install` clients then verify against fabricated
// digests they trust because the registry served them.
func verifyChecksums(tarball []byte, dist versionDist) error {
	if dist.Shasum == "" && dist.Integrity == "" {
		return fmt.Errorf("publish must include dist.shasum or dist.integrity for the tarball")
	}
	if dist.Shasum != "" {
		sum := sha1.Sum(tarball)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, dist.Shasum) {
			return fmt.Errorf("tarball sha1 %s does not match dist.shasum %s", got, dist.Shasum)
		}
	}
	if dist.Integrity != "" {
		const sha512Prefix = "sha512-"
		if !strings.HasPrefix(dist.Integrity, sha512Prefix) {
			return fmt.Errorf("dist.integrity %q has unsupported algorithm (only sha512- is supported)", dist.Integrity)
		}
		want, err := base64.StdEncoding.DecodeString(dist.Integrity[len(sha512Prefix):])
		if err != nil {
			return fmt.Errorf("dist.integrity %q is not valid base64: %w", dist.Integrity, err)
		}
		sum := sha512.Sum512(tarball)
		if !bytes.Equal(sum[:], want) {
			return fmt.Errorf("tarball sha512 does not match dist.integrity")
		}
	}
	return nil
}

// tarballURL builds the absolute URL clients should follow to fetch
// the tarball from this ocifactory instance.
//
// Scheme resolution: X-Forwarded-Proto wins so deployments behind a
// TLS-terminating reverse proxy (Cloud Run, an L7 LB, nginx) serve
// https URLs even though req.TLS is nil at the Go layer. Falls back
// to req.TLS, then req.URL.Scheme, then http. Operators who don't
// strip an upstream-supplied X-Forwarded-Proto from untrusted
// clients should front ocifactory with a proxy that overwrites it.
//
// Path segments are PathEscaped so any future relaxation of the
// version / filename charset doesn't silently produce malformed
// URLs. Scoped names (`@scope/name`) are split into two path
// segments so the `@` and `/` are preserved without double-encoding.
func tarballURL(req *http.Request, pkg, filename string) string {
	scheme := detectScheme(req)
	ns := mux.Vars(req)["namespace"]
	var pkgPath string
	if strings.HasPrefix(pkg, "@") {
		if slash := strings.IndexByte(pkg, '/'); slash >= 0 {
			pkgPath = url.PathEscape(pkg[:slash]) + "/" + url.PathEscape(pkg[slash+1:])
		} else {
			pkgPath = url.PathEscape(pkg)
		}
	} else {
		pkgPath = url.PathEscape(pkg)
	}
	u := url.URL{
		Scheme: scheme,
		Host:   req.Host,
		Path:   "/" + url.PathEscape(ns) + "/" + pkgPath + "/-/" + url.PathEscape(filename),
	}
	return u.String()
}

// detectScheme returns the URL scheme clients should see for a
// reflected URL. Honours X-Forwarded-Proto first so reverse-proxied
// deployments get https; falls back to req.TLS for direct-TLS
// listeners and finally to req.URL.Scheme / "http".
func detectScheme(req *http.Request) string {
	if proto := req.Header.Get("X-Forwarded-Proto"); proto != "" {
		// Take the first value if a chain of proxies produced a
		// comma-separated list.
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = proto[:i]
		}
		return strings.TrimSpace(proto)
	}
	if req.TLS != nil {
		return "https"
	}
	if req.URL.Scheme != "" {
		return req.URL.Scheme
	}
	return "http"
}

// tarballVersion extracts the version segment from an npm tarball
// filename, which is always "<short>-<version>.tgz" where <short> is
// the unscoped half of the name. Returns an error when the filename
// doesn't match.
func tarballVersion(pkg, filename string) (string, error) {
	short := pkg
	if strings.HasPrefix(pkg, "@") {
		if slash := strings.IndexByte(pkg, '/'); slash >= 0 {
			short = pkg[slash+1:]
		}
	}
	prefix := short + "-"
	if !strings.HasPrefix(filename, prefix) || !strings.HasSuffix(filename, ".tgz") {
		return "", fmt.Errorf("tarball filename %q does not match package %q", filename, pkg)
	}
	version := strings.TrimSuffix(strings.TrimPrefix(filename, prefix), ".tgz")
	if version == "" {
		return "", fmt.Errorf("tarball filename %q has empty version", filename)
	}
	return version, nil
}

// versionTagSafe returns whether v is a legal OCI tag value. OCI
// tag regex is `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}` — the same
// classes semver naturally produces, plus prerelease and build
// metadata.
func versionTagSafe(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	if !isTagFirstByte(v[0]) {
		return false
	}
	for i := 1; i < len(v); i++ {
		if !isTagByte(v[i]) {
			return false
		}
	}
	return true
}

// distTagSafe mirrors versionTagSafe for dist-tag names. npm dist-tag
// names must not look like valid semver versions but the surface
// charset is the same.
func distTagSafe(t string) bool {
	return versionTagSafe(t)
}

func isTagFirstByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

func isTagByte(b byte) bool {
	if isTagFirstByte(b) {
		return true
	}
	return b == '.' || b == '-'
}

// newRev fabricates a CouchDB-flavoured revision string for publish
// responses. The npm client does not validate it.
func newRev() string {
	return fmt.Sprintf("%d-%x", time.Now().UnixMilli(), time.Now().UnixNano()&0xffffffff)
}

// publishDocument is the slice of the npm publish body we actually
// look at. Versions stays as RawMessage so we round-trip the
// publisher's exact bytes for storage.
type publishDocument struct {
	Name        string                     `json:"name"`
	Versions    map[string]json.RawMessage `json:"versions"`
	DistTags    map[string]string          `json:"dist-tags"`
	Attachments map[string]attachment      `json:"_attachments"`
}

type attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int    `json:"length"`
}

type versionMetadata struct {
	Name    string      `json:"name"`
	Version string      `json:"version"`
	Dist    versionDist `json:"dist"`
}

type versionDist struct {
	Shasum    string `json:"shasum"`
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
}

// packument is the slim shape we serve back for GET /{name}.
// Versions is kept as raw JSON so we don't need to round-trip every
// optional field through a typed struct.
type packument struct {
	ID       string                     `json:"_id"`
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}
