// Package logging configures the project-wide structured logger.
//
// We use the standard library's log/slog as the single logging API. A handler
// is selected from configuration: text for humans, json for log aggregators.
// Once OpenTelemetry is wired in, an OTLP log bridge is added on top of the
// existing handler.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

type Options struct {
	Level  slog.Level
	Format Format
	Output io.Writer
}

// Setup installs a slog handler as the default logger and returns it.
func Setup(opts Options) *slog.Logger {
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	handlerOpts := &slog.HandlerOptions{
		Level:     opts.Level,
		AddSource: opts.Level <= slog.LevelDebug,
	}
	var h slog.Handler
	switch opts.Format {
	case FormatJSON:
		h = slog.NewJSONHandler(opts.Output, handlerOpts)
	default:
		h = slog.NewTextHandler(opts.Output, handlerOpts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

// ParseLevel maps human-friendly level names to slog levels.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}

// ParseFormat maps a name to a Format value.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text":
		return FormatText, nil
	case "json":
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("unknown log format %q", s)
	}
}

// FromContext returns a request-scoped logger if one has been attached;
// otherwise the default logger is returned.
func FromContext(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && v != nil {
		return v
	}
	return slog.Default()
}

// WithLogger attaches a logger to a context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

type loggerKey struct{}
