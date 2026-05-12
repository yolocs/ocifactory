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
	"path"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	RepoType     = "npm"
	ArtifactType = "application/vnd.ocifactory.npm"

	DefaultMaxUploadBytes int64 = 1 << 30

	packageRepoPrefix    = "packages"
	packumentRepoPrefix  = "packuments"
	packumentTag         = "current"
	packumentFileName    = "packument.json"
	indexSentinelName    = "present"
	indexSentinelContent = "1"
)

type Handler struct {
	registry       *namespace.Registry
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64
}

type Option func(*handlerConfig)

type handlerConfig struct {
	authMW         func(http.Handler) http.Handler
	maxUploadBytes int64
}

func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) { c.authMW = mw }
}

func WithMaxUploadBytes(n int64) Option {
	return func(c *handlerConfig) { c.maxUploadBytes = n }
}

func NewHandler(registry *namespace.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{maxUploadBytes: DefaultMaxUploadBytes}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Handler{registry: registry, authMW: cfg.authMW, maxUploadBytes: cfg.maxUploadBytes}, nil
}

func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	router.Use(mux.MiddlewareFunc(handler.RouteNameOpMiddleware))
	if h.authMW != nil {
		router.Use(mux.MiddlewareFunc(h.authMW))
	}

	nsr := router.PathPrefix("/{namespace}").Subrouter()
	pkg := `{package:(?:@[^/]+/)?[^/@][^/]*}`

	nsr.HandleFunc("/", h.ping).Methods(http.MethodGet).Name("read")
	nsr.HandleFunc("/-/ping", h.ping).Methods(http.MethodGet).Name("read")
	nsr.HandleFunc("/-/package/"+pkg+"/dist-tags/{tag}", h.distTagAdd).Methods(http.MethodPut, http.MethodPost).Name("write")
	nsr.HandleFunc("/-/package/"+pkg+"/dist-tags/{tag}", h.distTagRm).Methods(http.MethodDelete).Name("write")
	nsr.HandleFunc("/-/package/"+pkg+"/dist-tags", h.distTagLs).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/"+pkg+"/-/{filename:.+\\.tgz}", h.downloadTarball).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/"+pkg+"/{versionOrTag}", h.getPackageVersionMetadata).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/"+pkg, h.getPackageMetadata).Methods(http.MethodGet, http.MethodHead).Name("read")
	nsr.HandleFunc("/"+pkg, h.publishPackage).Methods(http.MethodPut).Name("write")
	nsr.HandleFunc("/"+pkg+"/-/{filename:.+\\.tgz}/-rev/{revision}", h.unpublishPackage).Methods(http.MethodDelete).Name("write")
	nsr.HandleFunc("/"+pkg+"/-rev/{revision}", h.unpublishPackage).Methods(http.MethodDelete).Name("write")

	return router
}

func (h *Handler) scopedFor(req *http.Request) *namespace.ScopedRegistry {
	return h.registry.For(mux.Vars(req)["namespace"])
}

func (h *Handler) getPackageMetadata(w http.ResponseWriter, req *http.Request) {
	pm, err := h.readPackument(req)
	if err != nil {
		h.writeReadError(w, req, err, "package not found")
		return
	}
	writeJSON(w, req, http.StatusOK, pm)
}

func (h *Handler) getPackageVersionMetadata(w http.ResponseWriter, req *http.Request) {
	pm, err := h.readPackument(req)
	if err != nil {
		h.writeReadError(w, req, err, "package not found")
		return
	}
	key := mux.Vars(req)["versionOrTag"]
	version := key
	if tagged, ok := pm.DistTags[key]; ok {
		version = tagged
	}
	vi, ok := pm.Versions[version]
	if !ok {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}
	writeJSON(w, req, http.StatusOK, vi)
}

func (h *Handler) downloadTarball(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	name := vars["package"]
	filename := vars["filename"]
	if err := validatePackageName(name); err != nil || path.Base(filename) != filename || strings.Contains(filename, "..") {
		http.Error(w, "invalid tarball path", http.StatusBadRequest)
		return
	}
	version, ok := versionFromTarballName(name, filename)
	if !ok {
		http.Error(w, "tarball filename does not match package", http.StatusBadRequest)
		return
	}
	encoded, err := encodePackageName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := &oci.RepoFile{OwningRepo: packageRepo(encoded), OwningTag: version, Name: filename, MediaType: "application/octet-stream"}
	h.handleGet(w, req, h.scopedFor(req), f)
}

func (h *Handler) publishPackage(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	name := mux.Vars(req)["package"]
	if err := validatePackageName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.maxUploadBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, h.maxUploadBytes)
	}
	defer req.Body.Close()

	var pm PackageMetadata
	if err := json.NewDecoder(req.Body).Decode(&pm); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, fmt.Sprintf("upload exceeds %d-byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid package metadata", http.StatusBadRequest)
		return
	}
	if pm.DistTags == nil {
		pm.DistTags = map[string]string{}
	}
	if pm.Name != name {
		http.Error(w, "package name does not match URL", http.StatusBadRequest)
		return
	}
	if len(pm.Versions) == 0 || len(pm.Attachments) == 0 {
		http.Error(w, "publish requires versions and attachments", http.StatusBadRequest)
		return
	}
	encoded, err := encodePackageName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scoped := h.scopedFor(req)
	existing, existingErr := h.readPackument(req)
	if existingErr != nil && !errors.Is(existingErr, errdef.ErrNotFound) {
		if handler.WriteNamespaceError(w, existingErr) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, existingErr, "internal error")
		return
	}
	published := map[string]VersionInfo{}
	for version, vi := range pm.Versions {
		filename := tarballFileName(name, version)
		att, ok := pm.Attachments[filename]
		if !ok {
			att, ok = attachmentForVersion(pm.Attachments, vi, filename)
		}
		if !ok {
			http.Error(w, fmt.Sprintf("missing attachment for %s", version), http.StatusBadRequest)
			return
		}
		tarball, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			http.Error(w, "invalid attachment data", http.StatusBadRequest)
			return
		}
		if err := verifyChecksums(tarball, vi); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vi.Name = name
		vi.Version = version
		vi.Dist.Shasum = sha1Hex(tarball)
		vi.Shasum = vi.Dist.Shasum
		vi.Dist.Integrity = sha512SRI(tarball)
		vi.Dist.Tarball = tarballURL(req, name, version)
		pm.Versions[version] = vi
		published[version] = vi
		f := &oci.RepoFile{OwningRepo: packageRepo(encoded), OwningTag: version, Name: filename, MediaType: "application/octet-stream", Size: int64(len(tarball))}
		if _, err := scoped.AddFile(ctx, f, bytes.NewReader(tarball)); err != nil {
			if handler.WriteNamespaceError(w, err) {
				return
			}
			if errors.Is(err, oci.ErrAlreadyExists) {
				http.Error(w, "version already exists", http.StatusConflict)
				return
			}
			handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
			return
		}
	}
	for tag, version := range pm.DistTags {
		if _, ok := pm.Versions[version]; !ok {
			http.Error(w, fmt.Sprintf("dist-tag %q points at unknown version %q", tag, version), http.StatusBadRequest)
			return
		}
		if err := scoped.AppendRefs(ctx, packageRepo(encoded), version, tag); err != nil {
			if handler.WriteNamespaceError(w, err) {
				return
			}
			handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
			return
		}
	}
	if existing != nil {
		for version, vi := range existing.Versions {
			if _, ok := published[version]; !ok {
				pm.Versions[version] = vi
			}
		}
		for tag, version := range existing.DistTags {
			if _, ok := pm.DistTags[tag]; !ok {
				pm.DistTags[tag] = version
			}
		}
		if pm.Time == nil {
			pm.Time = existing.Time
		} else if pm.Time["created"] == "" && existing.Time != nil {
			pm.Time["created"] = existing.Time["created"]
		}
	}
	pm.Attachments = nil
	pm.ID = name
	pm.Rev = revString()
	if pm.Time == nil {
		pm.Time = map[string]string{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if pm.Time["created"] == "" {
		pm.Time["created"] = now
	}
	pm.Time["modified"] = now
	if err := h.writePackument(ctx, scoped, encoded, &pm); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return
	}
	if err := h.ensureIndexSentinel(ctx, scoped, encoded); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return
	}
	writeJSON(w, req, http.StatusCreated, ModifyResponse{Ok: true, ID: name, Rev: pm.Rev})
}

func (h *Handler) unpublishPackage(w http.ResponseWriter, req *http.Request) {
	http.Error(w, "unpublish is not implemented", http.StatusNotImplemented)
}

func (h *Handler) distTagAdd(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	vars := mux.Vars(req)
	name, tag := vars["package"], vars["tag"]
	if err := validatePackageName(name); err != nil || tag == "" || strings.Contains(tag, "/") {
		http.Error(w, "invalid dist-tag request", http.StatusBadRequest)
		return
	}
	var version string
	if err := json.NewDecoder(req.Body).Decode(&version); err != nil || version == "" {
		http.Error(w, "dist-tag body must be a JSON version string", http.StatusBadRequest)
		return
	}
	pm, err := h.readPackument(req)
	if err != nil {
		h.writeReadError(w, req, err, "package not found")
		return
	}
	if _, ok := pm.Versions[version]; !ok {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}
	if pm.DistTags == nil {
		pm.DistTags = map[string]string{}
	}
	encoded, _ := encodePackageName(name)
	scoped := h.scopedFor(req)
	if err := scoped.AppendRefs(ctx, packageRepo(encoded), version, tag); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return
	}
	pm.DistTags[tag] = version
	pm.Rev = revString()
	if pm.Time == nil {
		pm.Time = map[string]string{}
	}
	pm.Time["modified"] = time.Now().UTC().Format(time.RFC3339)
	if err := h.writePackument(ctx, scoped, encoded, pm); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return
	}
	writeJSON(w, req, http.StatusCreated, ModifyResponse{Ok: true, ID: name, Rev: pm.Rev})
}

func (h *Handler) distTagRm(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	vars := mux.Vars(req)
	name, tag := vars["package"], vars["tag"]
	if err := validatePackageName(name); err != nil || tag == "" || strings.Contains(tag, "/") {
		http.Error(w, "invalid dist-tag request", http.StatusBadRequest)
		return
	}
	pm, err := h.readPackument(req)
	if err != nil {
		h.writeReadError(w, req, err, "package not found")
		return
	}
	if _, ok := pm.DistTags[tag]; !ok {
		http.Error(w, "dist-tag not found", http.StatusNotFound)
		return
	}
	encoded, _ := encodePackageName(name)
	scoped := h.scopedFor(req)
	if err := scoped.DeleteTagFiles(ctx, packageRepo(encoded), tag); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return
	}
	delete(pm.DistTags, tag)
	pm.Rev = revString()
	if pm.Time == nil {
		pm.Time = map[string]string{}
	}
	pm.Time["modified"] = time.Now().UTC().Format(time.RFC3339)
	if err := h.writePackument(ctx, scoped, encoded, pm); err != nil {
		if handler.WriteNamespaceError(w, err) {
			return
		}
		handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal error")
		return
	}
	writeJSON(w, req, http.StatusOK, ModifyResponse{Ok: true, ID: name, Rev: pm.Rev})
}

func (h *Handler) distTagLs(w http.ResponseWriter, req *http.Request) {
	pm, err := h.readPackument(req)
	if err != nil {
		h.writeReadError(w, req, err, "package not found")
		return
	}
	writeJSON(w, req, http.StatusOK, pm.DistTags)
}

func (h *Handler) ping(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, req, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) readPackument(req *http.Request) (*PackageMetadata, error) {
	name := mux.Vars(req)["package"]
	if err := validatePackageName(name); err != nil {
		return nil, err
	}
	encoded, err := encodePackageName(name)
	if err != nil {
		return nil, err
	}
	_, r, err := h.scopedFor(req).ReadFile(req.Context(), &oci.RepoFile{OwningRepo: packumentRepo(encoded), OwningTag: packumentTag, Name: packumentFileName})
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var pm PackageMetadata
	if err := json.NewDecoder(r).Decode(&pm); err != nil {
		return nil, err
	}
	for version, vi := range pm.Versions {
		vi.Dist.Tarball = tarballURL(req, name, version)
		pm.Versions[version] = vi
	}
	return &pm, nil
}

func (h *Handler) writePackument(ctx context.Context, scoped *namespace.ScopedRegistry, encoded string, pm *PackageMetadata) error {
	data, err := json.Marshal(pm)
	if err != nil {
		return err
	}
	_, err = scoped.AddFile(ctx, &oci.RepoFile{OwningRepo: packumentRepo(encoded), OwningTag: packumentTag, Name: packumentFileName, MediaType: "application/json", Size: int64(len(data))}, bytes.NewReader(data))
	if errors.Is(err, oci.ErrAlreadyExists) {
		// Packuments are materialized indexes and are intentionally mutable.
		// The real backend honors --allow-overwrite; tests use the fake's
		// default immutability, so replace via a best-effort delete first.
		if delErr := scoped.DeleteTagFiles(ctx, packumentRepo(encoded), packumentTag); delErr != nil && !errors.Is(delErr, errdef.ErrNotFound) {
			return delErr
		}
		_, err = scoped.AddFile(ctx, &oci.RepoFile{OwningRepo: packumentRepo(encoded), OwningTag: packumentTag, Name: packumentFileName, MediaType: "application/json", Size: int64(len(data))}, bytes.NewReader(data))
	}
	return err
}

func (h *Handler) ensureIndexSentinel(ctx context.Context, scoped *namespace.ScopedRegistry, encoded string) error {
	_, err := scoped.AddFile(ctx, &oci.RepoFile{OwningRepo: "index", OwningTag: encoded, Name: indexSentinelName, MediaType: "text/plain", Size: int64(len(indexSentinelContent))}, strings.NewReader(indexSentinelContent))
	if errors.Is(err, oci.ErrAlreadyExists) {
		return nil
	}
	return err
}

func (h *Handler) handleGet(w http.ResponseWriter, req *http.Request, scoped handler.Registry, f *oci.RepoFile) {
	logger := logging.FromContext(req.Context())
	if req.Method != http.MethodHead {
		if redirectURL, err := scoped.BlobRedirectURL(req.Context(), f); err == nil && redirectURL != "" {
			logger.DebugContext(req.Context(), "redirecting blob fetch to backend", "url", redirectURL)
			http.Redirect(w, req, redirectURL, http.StatusTemporaryRedirect)
			return
		} else if err != nil {
			logger.DebugContext(req.Context(), "blob redirect probe failed; falling back to streaming", "error", err)
		}
	}
	desc, r, err := scoped.ReadFile(req.Context(), f)
	if err != nil {
		h.writeReadError(w, req, err, "tarball not found")
		return
	}
	defer r.Close()
	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", desc.File.Size))
	if req.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, r); err != nil {
		handler.WriteError(req.Context(), w, http.StatusInternalServerError, err, "internal error")
	}
}

func (h *Handler) writeReadError(w http.ResponseWriter, req *http.Request, err error, notFound string) {
	if handler.WriteNamespaceError(w, err) {
		return
	}
	if errors.Is(err, errdef.ErrNotFound) {
		http.Error(w, notFound, http.StatusNotFound)
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
}

func writeJSON(w http.ResponseWriter, req *http.Request, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if req.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func packageRepo(encoded string) string           { return packageRepoPrefix + "/" + encoded }
func packumentRepo(encoded string) string         { return packumentRepoPrefix + "/" + encoded }
func tarballFileName(name, version string) string { return packageBase(name) + "-" + version + ".tgz" }

func versionFromTarballName(name, filename string) (string, bool) {
	prefix, suffix := packageBase(name)+"-", ".tgz"
	if !strings.HasPrefix(filename, prefix) || !strings.HasSuffix(filename, suffix) {
		return "", false
	}
	version := strings.TrimSuffix(strings.TrimPrefix(filename, prefix), suffix)
	return version, version != "" && !strings.Contains(version, "/")
}

func attachmentForVersion(atts map[string]AttachmentStub, vi VersionInfo, filename string) (AttachmentStub, bool) {
	if vi.Dist.Tarball != "" {
		if u, err := url.Parse(vi.Dist.Tarball); err == nil {
			if att, ok := atts[path.Base(u.Path)]; ok {
				return att, true
			}
		}
	}
	for name, att := range atts {
		if path.Base(name) == filename {
			return att, true
		}
	}
	if len(atts) == 1 {
		for _, att := range atts {
			return att, true
		}
	}
	return AttachmentStub{}, false
}

func verifyChecksums(data []byte, vi VersionInfo) error {
	sha1 := sha1Hex(data)
	if vi.Dist.Shasum != "" && vi.Dist.Shasum != sha1 {
		return fmt.Errorf("sha1 checksum mismatch")
	}
	if vi.Shasum != "" && vi.Shasum != sha1 {
		return fmt.Errorf("sha1 checksum mismatch")
	}
	integrity := sha512SRI(data)
	if vi.Dist.Integrity != "" && vi.Dist.Integrity != integrity {
		return fmt.Errorf("sha512 integrity mismatch")
	}
	return nil
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func sha512SRI(data []byte) string {
	sum := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func tarballURL(req *http.Request, name, version string) string {
	scheme := req.URL.Scheme
	if scheme == "" {
		if req.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	ns := mux.Vars(req)["namespace"]
	p := "/" + ns + "/" + name + "/-/" + tarballFileName(name, version)
	return (&url.URL{Scheme: scheme, Host: host, Path: p}).String()
}

func revString() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
