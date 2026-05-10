package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
	"github.com/yolocs/ocifactory/pkg/serving"
)

type Registry interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
	ListFiles(ctx context.Context, repo string) ([]*oci.RepoFile, error)

	// BlobRedirectURL returns a presigned URL the client can follow to
	// download the file's blob directly from the backend's CDN/object
	// store, or ("", nil) when the backend serves blobs inline (or
	// redirects are disabled). Hard failures return ("", err); callers
	// should fall back to ReadFile in that case.
	BlobRedirectURL(ctx context.Context, f *oci.RepoFile) (string, error)
}

// WriteNamespaceError translates namespace sentinel errors to HTTP
// 4xx responses and returns whether it wrote a response. When err is
// nil or unrelated, returns false and writes nothing — the caller
// handles remaining branches (errdef.ErrNotFound, oci.HasCode, ...).
func WriteNamespaceError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, namespace.ErrInvalidName),
		errors.Is(err, namespace.ErrInvalidOwningRepo):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	case errors.Is(err, namespace.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return true
	case errors.Is(err, auth.ErrUnauthorized):
		http.Error(w, "forbidden", http.StatusForbidden)
		return true
	}
	return false
}

// WriteError logs internal at ERROR (so operators see the backend
// detail in the server logs) and writes public to the client with the
// given status code. Use this for 5xx responses where the underlying
// error often carries backend hostnames, paths, or auth-translation
// noise that clients have no business seeing — leaking them via
// http.Error(w, err.Error(), 500) is a small but real
// deployment-topology disclosure.
//
// 4xx branches that already construct their own public message
// (NotFound, Unauthorized, Forbidden, BadRequest from validation)
// keep using http.Error directly; their messages are not leaky.
func WriteError(ctx context.Context, w http.ResponseWriter, code int, internal error, public string) {
	logging.FromContext(ctx).ErrorContext(ctx, "handler error",
		"code", code,
		"error", internal,
	)
	http.Error(w, public, code)
}

type Middleware func(next http.Handler) http.Handler

// Server is a wrapper around serving.Server that allows for adding middlewares.
type Server struct {
	svr         *serving.Server
	middlewares []Middleware
}

func NewServer(port string, middlewares ...Middleware) (*Server, error) {
	svr, err := serving.New(port)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}
	return &Server{svr: svr, middlewares: middlewares}, nil
}

// Start starts the server with the given handler and middlewares and blocks
// until the provided context is closed. When the provided context is closed,
// the HTTP server is gracefully stopped with a timeout of 10 seconds; once a
// server has been stopped, it is NOT safe for reuse.
func (s *Server) Start(ctx context.Context, handler http.Handler) error {
	h := handler
	for i := len(s.middlewares) - 1; i >= 0; i-- {
		h = s.middlewares[i](h)
	}
	return s.svr.StartHTTPHandler(ctx, h)
}

// Logger is a middleware that adds a logger to the request context.
// Use OCIFACTORY_LOG_LEVEL, OCIFACTORY_LOG_FORMAT, and OCIFACTORY_LOG_DEBUG to
// configure the logger.
func Loggeer(next http.Handler) http.Handler {
	logger := logging.NewFromEnv("OCIFACTORY_")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "request", "method", r.Method, "url", r.URL.String())
		r = r.WithContext(logging.WithLogger(r.Context(), logger))
		next.ServeHTTP(w, r)
	})
}
