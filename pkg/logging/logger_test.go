package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLookupLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    slog.Level
		wantErr bool
	}{
		{name: "DEBUG", input: "DEBUG", want: slog.LevelDebug},
		{name: "info lower", input: "info", want: slog.LevelInfo},
		{name: "warn alias", input: "WARN", want: slog.LevelWarn},
		{name: "warning full", input: "Warning", want: slog.LevelWarn},
		{name: "err alias", input: "ERR", want: slog.LevelError},
		{name: "trim spaces", input: "  DEBUG  ", want: slog.LevelDebug},
		{name: "unknown", input: "FATAL", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := lookupLevel(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("lookupLevel(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("lookupLevel(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestLookupFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Format
		wantErr bool
	}{
		{name: "JSON", input: "JSON", want: FormatJSON},
		{name: "json lower", input: "json", want: FormatJSON},
		{name: "TEXT", input: "TEXT", want: FormatText},
		{name: "unknown", input: "yaml", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := lookupFormat(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("lookupFormat(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("lookupFormat(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestFromContext_NoLogger_ReturnsDefault(t *testing.T) {
	t.Parallel()

	got := FromContext(t.Context())
	if got == nil {
		t.Fatal("FromContext(empty ctx) returned nil")
	}
}

func TestWithLogger_RoundTrip(t *testing.T) {
	t.Parallel()

	want := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	ctx := WithLogger(t.Context(), want)
	if got := FromContext(ctx); got != want {
		t.Errorf("FromContext after WithLogger returned a different logger")
	}
}

func TestNew_JSON_RenamesAttrsForCloudLogging(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, FormatJSON, false)
	logger.InfoContext(context.Background(), "hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log line: %v\nraw: %s", err, buf.String())
	}

	for _, want := range []string{"severity", "message"} {
		if _, ok := got[want]; !ok {
			t.Errorf("log line missing %q key; got %v", want, got)
		}
	}
	for _, banned := range []string{"level", "msg"} {
		if _, ok := got[banned]; ok {
			t.Errorf("log line still has stdlib slog key %q (should be renamed)", banned)
		}
	}
}

func TestNew_DebugDropsLevelFloor(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, slog.LevelError, FormatJSON, true) // ignore level when debug
	logger.DebugContext(context.Background(), "should appear")

	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("debug=true should bypass level floor; got: %q", buf.String())
	}
}
