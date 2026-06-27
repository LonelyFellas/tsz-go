package httpserver

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	applog "github.com/darwish/tsz-go/internal/platform/log"
)

// requestIDHeader carries the request ID in and out. An inbound value (e.g. from
// an upstream proxy or gateway) is honored so a request can be traced end to end.
const requestIDHeader = "X-Request-ID"

// healthPath is logged-skipped: liveness probes hit it constantly and would
// otherwise drown the logs.
const healthPath = "/healthz"

// RequestID assigns each request an ID (honoring an inbound X-Request-ID),
// echoes it on the response, and stashes it in the request context so every
// slog call made with that context is stamped with it. Register it first so the
// recovery and request loggers downstream can see the ID.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		c.Header(requestIDHeader, id)
		c.Request = c.Request.WithContext(applog.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

// Recovery turns a panic into a 500 and logs it through slog (with the
// request_id and a stack trace) instead of gin's default plaintext stderr dump,
// keeping panics in the same structured stream as everything else.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Captured here, while the panicking frames are still on the
				// stack (recover runs before unwinding), so the trace is useful.
				slog.ErrorContext(c.Request.Context(), "panic recovered",
					"err", err,
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"stack", string(debug.Stack()),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			}
		}()
		c.Next()
	}
}

// RequestLogger emits one structured log line per request via slog. The level
// tracks the outcome — 5xx at Error (with any error handlers attached via
// c.Error, so a 500's cause is never lost), 4xx at Warn, the rest at Info — and
// health checks are skipped entirely.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == healthPath {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()

		ctx := c.Request.Context()
		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
		}
		// Surface the underlying cause of failures. Without this a 500 logs with
		// no clue why; handlers attach the real error via c.Error(err).
		if len(c.Errors) > 0 {
			attrs = append(attrs, "err", c.Errors.String())
		}

		switch {
		case status >= http.StatusInternalServerError:
			slog.ErrorContext(ctx, "http_request", attrs...)
		case status >= http.StatusBadRequest:
			slog.WarnContext(ctx, "http_request", attrs...)
		default:
			slog.InfoContext(ctx, "http_request", attrs...)
		}
	}
}

// AuthRequired validates the Bearer token and stores the user ID in context.
func AuthRequired(tm *auth.TokenManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed authorization header"})
			return
		}

		claims, err := tm.Parse(parts[1])
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set(auth.ContextUserIDKey, claims.UserID)
		c.Set(auth.ContextRoleKey, claims.Role)
		c.Next()
	}
}

// RequireRole gates a route on the token's active role, aborting with 403 unless
// it matches. Mount it AFTER AuthRequired (which populates the role in context);
// on its own it has no token to read. The role comparison is against the active
// role only — a multi-role account must switch into the role first.
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if got, _ := c.Get(auth.ContextRoleKey); got != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin role required"})
			return
		}
		c.Next()
	}
}
