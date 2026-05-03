// Package serving runs an HTTP server with graceful shutdown driven by context
// cancellation. The shape mirrors what we previously consumed from
// abcxyz/pkg/serving — minus the gRPC support, which ocifactory does not use.
package serving

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/yolocs/ocifactory/pkg/logging"
)

// Server wraps a TCP listener and exposes an HTTP-serving lifecycle that
// terminates cleanly when its context is cancelled.
type Server struct {
	listener net.Listener
}

// New listens on :port. If port is empty or "0" the OS picks one — Addr can
// then be inspected to discover the bound port.
func New(port string) (*Server, error) {
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, fmt.Errorf("listen on :%s: %w", port, err)
	}
	return &Server{listener: listener}, nil
}

// Addr returns the bound address (e.g. "[::]:8080"). Useful in tests where
// the OS chose the port.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// StartHTTPHandler builds a sensible default *http.Server around handler and
// runs it via StartHTTP.
func (s *Server) StartHTTPHandler(ctx context.Context, handler http.Handler) error {
	return s.StartHTTP(ctx, &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	})
}

// StartHTTP runs srv against the server's listener and blocks until ctx is
// cancelled. On cancellation the server is given up to 10 seconds to drain
// in-flight requests.
//
// Once StartHTTP returns the *Server must not be reused.
func (s *Server) StartHTTP(ctx context.Context, srv *http.Server) error {
	logger := logging.FromContext(ctx)

	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		logger.InfoContext(ctx, "server starting", "addr", s.listener.Addr().String())
		defer logger.InfoContext(ctx, "server stopped")
		if err := srv.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	<-doneCh
	return nil
}
