package observability

// logger.go — Structured JSON logger with request correlation context.
//
// Source: IMPLEMENTATION_PLAN_V1 §A7 (structured logging scaffold),
//         11-01-logging-strategy-and-user-visible-status.md.
//
// All log entries use log/slog with JSON output.
// Required fields on every entry: time, level, msg.
// Required fields on request-scoped entries: request_id, host_id (where applicable).
// Secrets, credentials, raw request bodies are never logged. Source: AUTH_OWNERSHIP_MODEL_V1 §2.

import (
	"context"
	"log/slog"
	"os"
)

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	hostIDKey    contextKey = "log_host_id"
	instanceIDKey contextKey = "log_instance_id"
)

// New constructs a JSON logger writing to stdout.
// level: "debug", "info", "warn", "error". Defaults to "info" if unrecognised.
func New(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// WithRequestID returns a context carrying the request ID for log correlation.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

// WithHostID returns a context carrying the host_id for log correlation.
func WithHostID(ctx context.Context, hostID string) context.Context {
	return context.WithValue(ctx, hostIDKey, hostID)
}

// WithInstanceID returns a context carrying the instance_id for log correlation.
func WithInstanceID(ctx context.Context, instanceID string) context.Context {
	return context.WithValue(ctx, instanceIDKey, instanceID)
}

// FromContext extracts log attributes from context and returns them as slog.Attr slice.
// Call: logger.InfoContext(ctx, "msg", observability.FromContext(ctx)...)
func FromContext(ctx context.Context) []any {
	var attrs []any
	if v, ok := ctx.Value(requestIDKey).(string); ok && v != "" {
		attrs = append(attrs, slog.String("request_id", v))
	}
	if v, ok := ctx.Value(hostIDKey).(string); ok && v != "" {
		attrs = append(attrs, slog.String("host_id", v))
	}
	if v, ok := ctx.Value(instanceIDKey).(string); ok && v != "" {
		attrs = append(attrs, slog.String("instance_id", v))
	}
	return attrs
}
