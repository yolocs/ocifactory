package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/abcxyz/pkg/logging"
	"github.com/abcxyz/pkg/serving"
	"github.com/yolocs/ocifactory/pkg/cred"
	"github.com/yolocs/ocifactory/pkg/oci"
)

type Registry interface {
	AddFile(ctx context.Context, f *oci.RepoFile, ro io.Reader) (*oci.FileDescriptor, error)
	ReadFile(ctx context.Context, f *oci.RepoFile) (*oci.FileDescriptor, io.ReadCloser, error)
	ListTags(ctx context.Context, repo string) ([]string, error)
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

// PassThroughAuth is a middleware that passes through basic auth credentials
// from the request to the context.
func PassThroughAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If we have basic auth, then pass through it via context.
		user, pwd, ok := r.BasicAuth()
		if ok {
			r = r.WithContext(cred.WithCred(
				r.Context(),
				&cred.Cred{Basic: &cred.BasicCred{User: user, Password: pwd}}),
			)
		}
		next.ServeHTTP(w, r)
	})
}

// Logger is a middleware that adds a logger to the request context.
// Use OCIFACTORY_LOG_LEVEL, OCIFACTORY_LOG_FORMAT, and OCIFACTORY_LOG_DEBUG to
// configure the logger.
func Loggeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(logging.WithLogger(r.Context(), logging.NewFromEnv("OCIFACTORY_")))
		next.ServeHTTP(w, r)
	})
}
