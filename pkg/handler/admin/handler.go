// Package admin exposes ocifactory control-plane HTTP endpoints.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/namespace"
)

const (
	// FormatLabel is the metrics format label used by the admin service.
	FormatLabel = "admin"

	contentTypeJSON = "application/json"
)

// Store is the namespace metadata persistence surface the admin API needs.
type Store interface {
	Get(ctx context.Context, name string) (*namespace.Namespace, error)
	List(ctx context.Context) ([]string, error)
	Put(ctx context.Context, ns *namespace.Namespace) error
	Delete(ctx context.Context, name string) error
}

// PackageRegistry enumerates and bulk-deletes the OCI sub-repos a
// namespace holds. The admin DELETE flow uses ListPackages to gate
// the 409 / cascade decision and CascadeDelete to drop every sub-repo
// before the namespace metadata is removed.
type PackageRegistry interface {
	ListPackages(ctx context.Context, name string) ([]string, error)
	CascadeDelete(ctx context.Context, name string) error
}

// Handler serves the admin HTTP API.
type Handler struct {
	store    Store
	packages PackageRegistry
}

// NewHandler returns an admin API handler backed by store and packages.
func NewHandler(store Store, packages PackageRegistry) (*Handler, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if packages == nil {
		return nil, fmt.Errorf("package registry is required")
	}
	return &Handler{store: store, packages: packages}, nil
}

// Mux returns the admin API router.
func (h *Handler) Mux() http.Handler {
	r := mux.NewRouter()
	r.Use(handler.RouteNameOpMiddleware)

	r.HandleFunc("/admin/v1/namespaces", h.listNamespaces).
		Methods(http.MethodGet).
		Name("admin_namespaces_list")
	r.HandleFunc("/admin/v1/namespaces/{name}", h.putNamespace).
		Methods(http.MethodPut).
		Name("admin_namespaces_put")
	r.HandleFunc("/admin/v1/namespaces/{name}", h.getNamespace).
		Methods(http.MethodGet).
		Name("admin_namespaces_get")
	r.HandleFunc("/admin/v1/namespaces/{name}", h.deleteNamespace).
		Methods(http.MethodDelete).
		Name("admin_namespaces_delete")

	return r
}

type listResponse struct {
	Namespaces []string `json:"namespaces"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (h *Handler) listNamespaces(w http.ResponseWriter, r *http.Request) {
	names, err := h.store.List(r.Context())
	if err != nil {
		writeInternal(r.Context(), w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, listResponse{Namespaces: names})
}

func (h *Handler) getNamespace(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ns, err := h.store.Get(r.Context(), name)
	if err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (h *Handler) putNamespace(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := namespace.ValidateName(name); err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}

	var spec namespace.Spec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("invalid JSON body: %v", err)})
		return
	}
	if err := spec.Validate(); err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}

	status := http.StatusOK
	// The 200-vs-201 distinction is best-effort: concurrent creates can
	// both observe ErrNotFound before either Put lands. PUT remains
	// idempotent because Store.Put is an upsert.
	if _, err := h.store.Get(r.Context(), name); err != nil {
		if errors.Is(err, namespace.ErrNotFound) {
			status = http.StatusCreated
		} else {
			writeAdminError(r.Context(), w, err)
			return
		}
	}

	ns := &namespace.Namespace{Name: name, Spec: spec}
	if err := h.store.Put(r.Context(), ns); err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}
	writeJSON(w, status, ns)
}

func (h *Handler) deleteNamespace(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if _, err := h.store.Get(r.Context(), name); err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}

	// ?cascade=true opts in to wiping every sub-repo under the
	// namespace before dropping its metadata. The explicit query
	// param is a guardrail against typo-DELETEs nuking a namespace
	// full of artifacts; an empty namespace deletes either way so
	// the common "I'm done with this" path stays a single curl.
	cascade := parseCascade(r.URL.Query().Get("cascade"))

	packages, err := h.packages.ListPackages(r.Context(), name)
	if err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}
	if len(packages) > 0 && !cascade {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "namespace is not empty; pass ?cascade=true to delete all packages"})
		return
	}
	if cascade {
		if err := h.packages.CascadeDelete(r.Context(), name); err != nil {
			writeAdminError(r.Context(), w, err)
			return
		}
	}
	if err := h.store.Delete(r.Context(), name); err != nil {
		writeAdminError(r.Context(), w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseCascade accepts the standard truthy spellings ("true", "1",
// "yes"). Anything else — including the absence of the query param —
// is false so a stray "?cascade=maybe" doesn't surprise the operator.
func parseCascade(v string) bool {
	switch v {
	case "true", "1", "yes":
		return true
	}
	return false
}

func writeAdminError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, namespace.ErrInvalidName), errors.Is(err, namespace.ErrInvalidPolicy):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, namespace.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
	default:
		writeInternal(ctx, w, err)
	}
}

func writeInternal(ctx context.Context, w http.ResponseWriter, err error) {
	handler.WriteError(ctx, w, http.StatusInternalServerError, err, "internal server error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
