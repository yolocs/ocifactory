// Package logging is a small slog wrapper with environment-driven configuration.
//
// It produces JSON output by default (suitable for Cloud Run and other
// structured log collectors) and renames slog's standard attribute keys
// to match Google Cloud Logging's special payload fields.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Format is the structured log output encoding.
type Format int

const (
	FormatJSON Format = iota
	FormatText
)

// New returns an slog.Logger that writes records to w. When debug is true the
// minimum level is dropped to slog.LevelDebug-8 (effectively "log everything")
// and source location is included.
func New(w io.Writer, level slog.Level, format Format, debug bool) *slog.Logger {
	if debug {
		level = slog.LevelDebug - 8
	}
	opts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   debug,
		ReplaceAttr: cloudLoggingAttrs,
	}

	var h slog.Handler
	switch format {
	case FormatJSON:
		h = slog.NewJSONHandler(w, opts)
	case FormatText:
		h = slog.NewTextHandler(w, opts)
	default:
		panic(fmt.Sprintf("log: unknown format %d", format))
	}
	return slog.New(h)
}

// NewFromEnv reads ${prefix}LOG_LEVEL, ${prefix}LOG_FORMAT, and
// ${prefix}LOG_DEBUG, falling back to the unprefixed names. Defaults are
// INFO, JSON, and false. It panics if any value is malformed — these are
// startup-time configuration errors.
func NewFromEnv(prefix string) *slog.Logger {
	level := slog.LevelInfo
	if v := lookupEnv(prefix+"LOG_LEVEL", "LOG_LEVEL"); v != "" {
		l, err := lookupLevel(v)
		if err != nil {
			panic(fmt.Sprintf("log: %s: %v", prefix+"LOG_LEVEL", err))
		}
		level = l
	}

	format := FormatJSON
	if v := lookupEnv(prefix+"LOG_FORMAT", "LOG_FORMAT"); v != "" {
		f, err := lookupFormat(v)
		if err != nil {
			panic(fmt.Sprintf("log: %s: %v", prefix+"LOG_FORMAT", err))
		}
		format = f
	}

	debug := false
	if v := lookupEnv(prefix+"LOG_DEBUG", "LOG_DEBUG"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			panic(fmt.Sprintf("log: %s: %v", prefix+"LOG_DEBUG", err))
		}
		debug = b
	}

	return New(os.Stdout, level, format, debug)
}

// defaultLogger is the fallback returned by FromContext when no logger is
// attached to the context.
var defaultLogger = sync.OnceValue(func() *slog.Logger {
	return New(os.Stdout, slog.LevelInfo, FormatJSON, false)
})

type contextKey struct{}

// WithLogger returns a child context carrying logger.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, logger)
}

// FromContext returns the logger attached to ctx, or a process-wide default
// JSON logger writing to stdout at INFO level.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return l
	}
	return defaultLogger()
}

// lookupEnv returns the first non-empty environment value among names.
func lookupEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func lookupLevel(s string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR", "ERR":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown level %q (want DEBUG, INFO, WARN, ERROR)", s)
	}
}

func lookupFormat(s string) (Format, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "JSON":
		return FormatJSON, nil
	case "TEXT":
		return FormatText, nil
	default:
		return 0, fmt.Errorf("unknown format %q (want JSON or TEXT)", s)
	}
}

// cloudLoggingAttrs renames slog's standard attribute keys so logs ingested
// by Google Cloud Logging are parsed as structured payloads. See
// https://cloud.google.com/logging/docs/structured-logging#special-payload-fields.
func cloudLoggingAttrs(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.LevelKey:
		a.Key = "severity"
	case slog.MessageKey:
		a.Key = "message"
	case slog.SourceKey:
		a.Key = "logging.googleapis.com/sourceLocation"
	}
	return a
}
