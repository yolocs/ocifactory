package handler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/ocifactory/pkg/logging"
)

// TestWriteError locks the public-vs-internal split: the body sent to
// the client carries only the public message and the requested status
// code, while the logger attached to ctx receives the wrapped internal
// error. Without that split, handlers leak ORAS-derived backend
// hostnames and paths to clients via http.Error(w, err.Error(), 500).
func TestWriteError(t *testing.T) {
	t.Parallel()

	internal := errors.New("zot.internal:5000/packages/foo: 500 internal server error")

	tests := []struct {
		name           string
		code           int
		public         string
		wantBody       string
		wantLogContain string
	}{
		{
			name:           "500 internal error",
			code:           500,
			public:         "internal error",
			wantBody:       "internal error",
			wantLogContain: "zot.internal:5000/packages/foo",
		},
		{
			name:           "503 with custom public message",
			code:           503,
			public:         "backend unavailable",
			wantBody:       "backend unavailable",
			wantLogContain: "zot.internal:5000/packages/foo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{
				Level: slog.LevelDebug,
			}))
			ctx := logging.WithLogger(context.Background(), logger)

			rec := httptest.NewRecorder()
			WriteError(ctx, rec, tc.code, internal, tc.public)

			if got, want := rec.Code, tc.code; got != want {
				t.Errorf("status = %d, want %d", got, want)
			}
			body := strings.TrimSpace(rec.Body.String())
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			// Public message must NOT contain the internal detail.
			if strings.Contains(body, tc.wantLogContain) {
				t.Errorf("body leaked internal detail %q: %q", tc.wantLogContain, body)
			}
			logged := logBuf.String()
			if !strings.Contains(logged, tc.wantLogContain) {
				t.Errorf("log missing internal detail %q: %q", tc.wantLogContain, logged)
			}
			if !strings.Contains(logged, "level=ERROR") {
				t.Errorf("log not at ERROR severity: %q", logged)
			}
		})
	}
}
