package npm

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/handler"
)

const (
	RepoType     = "npm"
	ArtifactType = "application/vnd.ocifactory.npm"
)

type Handler struct {
	registry handler.Registry
}

func NewHandler(registry handler.Registry) (*Handler, error) {
	return &Handler{registry: registry}, nil
}

func (h *Handler) Mux() http.Handler {
	r := mux.NewRouter()

	// Package Read APIs
	// GET /{package}/{versionOrTag} - Must be specific, order matters with mux
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/{versionOrTag}", getPackageVersionMetadataHandler).Methods(http.MethodGet, http.MethodHead)
	// GET /{package} - General package info (full metadata)
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}", getPackageMetadataHandler).Methods(http.MethodGet, http.MethodHead)

	// Tarball Download
	// GET /@scope/package/-/package-version.tgz
	// GET /package/-/package-version.tgz
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-/{filename:.+\\.tgz}", downloadTarballHandler).Methods(http.MethodGet, http.MethodHead)

	// Package Write APIs (Publish, Unpublish)
	// PUT /@scope/package
	// PUT /package
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}", publishPackageHandler).Methods(http.MethodPut)

	// Unpublish specific version: DELETE /@scope/package/-/filename.tgz/-rev/revision
	// Unpublish specific version: DELETE /package/-/filename.tgz/-rev/revision
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-/{filename:.+\\.tgz}/-rev/{revision}", unpublishPackageHandler).Methods(http.MethodDelete)
	// Unpublish entire package: DELETE /@scope/package/-rev/revision
	// Unpublish entire package: DELETE /package/-rev/revision
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-rev/{revision}", unpublishPackageHandler).Methods(http.MethodDelete)

	// User Management & Authentication not supported.
	// PUT /-/user/org.couchdb.user:{username}
	// r.HandleFunc("/-/user/{username:org\\.couchdb\\.user:[^/]+}", UserLoginHandler).Methods(http.MethodPut)
	// GET /-/whoami or /-/npm/v1/user
	// r.HandleFunc("/-/whoami", WhoamiHandler).Methods(http.MethodGet, http.MethodHead)
	// r.HandleFunc("/-/npm/v1/user", WhoamiHandler).Methods(http.MethodGet, http.MethodHead) // Newer endpoint

	// Dist Tags (npm dist-tag add/rm/ls)
	// The npm client often modifies dist-tags by PUTting the whole package document.
	// However, a more direct API might look like this:
	// PUT /-/package/@scope/pkg/dist-tags/latest (body: "1.0.0")
	// These are examples and might vary based on exact npm client behavior with different registry versions.
	// A common way npm CLI handles this is to GET the package doc, modify dist-tags, then PUT the package doc.
	// The following are more explicit/granular endpoints if you want to implement them directly.
	r.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags/{tag}", distTagAddHandler).Methods(http.MethodPut, http.MethodPost)
	r.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags/{tag}", distTagRmHandler).Methods(http.MethodDelete)
	r.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags", distTagLsHandler).Methods(http.MethodGet, http.MethodHead)

	// Ping.
	r.HandleFunc("/-/ping", pingHandler).Methods(http.MethodGet)
	r.HandleFunc("/", pingHandler).Methods(http.MethodGet)

	return r
}

func getPackageVersionMetadataHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement package version metadata handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func getPackageMetadataHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement package metadata handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func downloadTarballHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement tarball download handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func publishPackageHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement package publish handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func unpublishPackageHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement package unpublish handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func distTagAddHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement dist-tag add handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func distTagRmHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement dist-tag remove handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func distTagLsHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement dist-tag list handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func pingHandler(w http.ResponseWriter, req *http.Request) {
	// TODO: Implement ping handler
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
