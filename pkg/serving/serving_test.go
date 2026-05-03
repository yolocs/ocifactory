package serving

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestStartHTTPHandler_ServesAndShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()

	srv, err := New("0") // OS-assigned port
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.StartHTTPHandler(ctx, handler)
	}()

	// Poll until the server is ready (up to 1s).
	url := "http://" + srv.Addr()
	deadline := time.Now().Add(time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get(url) //nolint:noctx // test-local
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never became ready: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("StartHTTPHandler() returned unexpected error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down within 15s of context cancel")
	}
}

func TestNew_BadPort_ReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := New("not-a-port"); err == nil {
		t.Fatal("New(\"not-a-port\") returned nil error, want one")
	}
}
