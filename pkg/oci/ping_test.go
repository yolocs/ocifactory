package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestRegistry_Ping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "200 ok passes", statusCode: http.StatusOK, wantErr: false},
		{name: "401 challenge passes", statusCode: http.StatusUnauthorized, wantErr: false},
		{name: "500 fails", statusCode: http.StatusInternalServerError, wantErr: true},
		{name: "503 fails", statusCode: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("Ping used %s, want HEAD", r.Method)
				}
				if r.URL.Path != "/v2/" {
					t.Errorf("Ping path = %q, want /v2/", r.URL.Path)
				}
				w.WriteHeader(tc.statusCode)
			}))
			t.Cleanup(srv.Close)

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			r, err := NewRegistry(u)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cancel)

			err = r.Ping(ctx)
			if (err != nil) != tc.wantErr {
				t.Errorf("Ping() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRegistry_Ping_TransportError(t *testing.T) {
	t.Parallel()

	// Point at a closed listener address so Do() returns immediately.
	r, err := NewRegistry(&url.URL{Scheme: "http", Host: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)
	if err := r.Ping(ctx); err == nil {
		t.Fatal("Ping against unreachable host returned nil error")
	}
}
