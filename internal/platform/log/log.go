// Package log centralizes slog setup. It builds the JSON logger the whole app
// shares and stamps a per-request ID onto every record drawn from a request's
// context, so handlers and services only need slog.InfoContext(ctx, ...) — the
// request_id field is added for them.
package log

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const requestIDKey ctxKey = 0

// WithRequestID returns a context carrying id. Any slog call made with that
// context (InfoContext/ErrorContext/...) is automatically stamped with it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID carried by ctx, or "" if none.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// New returns a slog.Logger writing JSON to stdout at the given level. The
// level string is parsed leniently (debug/info/warn/error, case-insensitive);
// anything unrecognized — including "" — falls back to info.
func New(level string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)})
	return slog.New(&contextHandler{Handler: h})
}

// contextHandler decorates each record with the request_id carried in the
// context. It delegates everything else to the wrapped handler.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs/WithGroup must re-wrap so the context decoration survives derived
// loggers (slog.With(...)), not just the root one.
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
