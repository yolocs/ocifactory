// Package echo is a no-op artifact format that exists to give CI an
// auth target. It does not store anything and does not depend on the
// OCI backend; its routes return small JSON envelopes describing the
// authenticated AuthContext on the request.
//
// The end-to-end CI job in .github/workflows/ci.yml mints a real
// GitHub Actions OIDC token, starts ocifactory with --repo-type=echo
// in front of an OIDC authenticator, and asserts the standard cases
// (valid token → 200, missing/garbage/wrong-audience → 401). That
// proves the wiring all the way from the binary through pkg/auth/oidc
// against a real issuer's JWKS, which the unit tests can't.
//
// Echo is not a real artifact format and intentionally has no
// ArtifactType — it never writes manifests.
package echo

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/handler"
)

// RepoType is the value passed to --repo-type to select this handler.
const RepoType = "echo"

// Handler serves the echo routes.
type Handler struct {
	authMW func(http.Handler) http.Handler
}

// Option configures optional Handler behaviour.
type Option func(*handlerConfig)

type handlerConfig struct {
	authMW func(http.Handler) http.Handler
}

// WithAuthMiddleware installs an authentication middleware on every
// echo route. Pass nil (or omit) to leave routes ungated; the serve
// command always passes one.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) {
		c.authMW = mw
	}
}

// NewHandler creates a new echo Handler.
func NewHandler(opts ...Option) *Handler {
	var cfg handlerConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Handler{authMW: cfg.authMW}
}

// Mux returns the echo router. Both routes are auth-gated when
// WithAuthMiddleware is set; the route names ("read" / "whoami")
// flow into the metrics op label via RouteNameOpMiddleware.
func (h *Handler) Mux() http.Handler {
	router := mux.NewRouter()
	router.Use(mux.MiddlewareFunc(handler.RouteNameOpMiddleware))
	if h.authMW != nil {
		router.Use(mux.MiddlewareFunc(h.authMW))
	}

	router.HandleFunc("/echo/{message}", h.handleEcho).Methods("GET").Name("read")
	router.HandleFunc("/whoami", h.handleWhoami).Methods("GET").Name("whoami")

	return router
}

// echoResponse is the body returned by GET /echo/{message}.
type echoResponse struct {
	Message string         `json:"message"`
	Caller  *callerSummary `json:"caller,omitempty"`
}

// whoamiResponse is the body returned by GET /whoami.
type whoamiResponse struct {
	Caller *callerSummary `json:"caller,omitempty"`
}

// callerSummary is the JSON-safe projection of an *auth.AuthContext.
// We don't echo the full Claims map by default — it's verified but
// can be large and may contain claims the operator considers
// sensitive (email, organization, repo). The CI test only needs
// issuer + sub to prove the chain worked.
type callerSummary struct {
	Issuer string `json:"issuer"`
	ID     string `json:"id"`
}

func (h *Handler) handleEcho(w http.ResponseWriter, r *http.Request) {
	msg := mux.Vars(r)["message"]
	writeJSON(w, http.StatusOK, echoResponse{
		Message: msg,
		Caller:  callerFromContext(r),
	})
}

func (h *Handler) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, whoamiResponse{
		Caller: callerFromContext(r),
	})
}

func callerFromContext(r *http.Request) *callerSummary {
	ac, ok := auth.FromContext(r.Context())
	if !ok || ac == nil {
		return nil
	}
	return &callerSummary{Issuer: ac.Issuer, ID: ac.ID}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
