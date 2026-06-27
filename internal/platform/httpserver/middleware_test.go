package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	applog "github.com/darwish/tsz-go/internal/platform/log"
)

func init() { gin.SetMode(gin.TestMode) }

func TestAuthRequired(t *testing.T) {
	tm := auth.NewTokenManager("secret", time.Hour)
	validUser := uuid.New()
	validToken, _ := tm.Generate(validUser, "student")

	expired := auth.NewTokenManager("secret", -time.Minute)
	expiredToken, _ := expired.Generate(uuid.New(), "student")

	wrongSigner := auth.NewTokenManager("other-secret", time.Hour)
	wrongToken, _ := wrongSigner.Generate(uuid.New(), "student")

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"valid bearer", "Bearer " + validToken, http.StatusOK},
		{"case-insensitive scheme", "bearer " + validToken, http.StatusOK},
		{"missing header", "", http.StatusUnauthorized},
		{"no bearer prefix", validToken, http.StatusUnauthorized},
		{"wrong scheme", "Basic " + validToken, http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"garbage token", "Bearer not.a.jwt", http.StatusUnauthorized},
		{"expired token", "Bearer " + expiredToken, http.StatusUnauthorized},
		{"wrong secret", "Bearer " + wrongToken, http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, engine := gin.CreateTestContext(w)

			var gotID any
			engine.Use(AuthRequired(tm))
			engine.GET("/protected", func(c *gin.Context) {
				gotID = c.MustGet(auth.ContextUserIDKey)
				c.Status(http.StatusOK)
			})

			c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tt.header != "" {
				c.Request.Header.Set("Authorization", tt.header)
			}
			engine.ServeHTTP(w, c.Request)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			// on success the authenticated user id must be in context
			if tt.wantStatus == http.StatusOK {
				if gotID != validUser {
					t.Errorf("context user id = %v, want %v", gotID, validUser)
				}
			}
		})
	}
}

func TestAuthRequired_ContextRole(t *testing.T) {
	tm := auth.NewTokenManager("secret", time.Hour)
	token, _ := tm.Generate(uuid.New(), "teacher")

	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)

	var gotRole any
	engine.Use(AuthRequired(tm))
	engine.GET("/protected", func(c *gin.Context) {
		gotRole = c.MustGet(auth.ContextRoleKey)
		c.Status(http.StatusOK)
	})

	c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	c.Request.Header.Set("Authorization", "Bearer "+token)
	engine.ServeHTTP(w, c.Request)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotRole != "teacher" {
		t.Errorf("context role = %v, want teacher", gotRole)
	}
}

// RequireRole gates on the active role from the token: 403 unless it matches,
// pass-through (200) when it does, and 403 when AuthRequired never ran (no role
// in context). Mounted after AuthRequired in production.
func TestRequireRole(t *testing.T) {
	tm := auth.NewTokenManager("secret", time.Hour)
	adminToken, _ := tm.Generate(uuid.New(), auth.RoleAdmin)
	studentToken, _ := tm.Generate(uuid.New(), "student")

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"admin passes", "Bearer " + adminToken, http.StatusOK},
		{"non-admin forbidden", "Bearer " + studentToken, http.StatusForbidden},
		{"missing token unauthorized", "", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, engine := gin.CreateTestContext(w)

			var reached bool
			engine.Use(AuthRequired(tm), RequireRole(auth.RoleAdmin))
			engine.GET("/admin/x", func(c *gin.Context) {
				reached = true
				c.Status(http.StatusOK)
			})

			c.Request = httptest.NewRequest(http.MethodGet, "/admin/x", nil)
			if tt.header != "" {
				c.Request.Header.Set("Authorization", tt.header)
			}
			engine.ServeHTTP(w, c.Request)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if reached != (tt.wantStatus == http.StatusOK) {
				t.Errorf("handler reached = %v, want %v", reached, tt.wantStatus == http.StatusOK)
			}
		})
	}
}

// RequireRole on its own (no AuthRequired upstream, so no role in context) must
// abort with 403 rather than pass through.
func TestRequireRole_NoRoleInContext(t *testing.T) {
	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)

	engine.Use(RequireRole(auth.RoleAdmin))
	engine.GET("/admin/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	c.Request = httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	engine.ServeHTTP(w, c.Request)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestRequestLogger(t *testing.T) {
	// capture slog output as JSON so we can assert on the emitted fields
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)

	var downstreamCalled bool
	engine.Use(RequestLogger())
	engine.GET("/widgets", func(c *gin.Context) {
		downstreamCalled = true
		c.Status(http.StatusTeapot)
	})

	c.Request = httptest.NewRequest(http.MethodGet, "/widgets", nil)
	engine.ServeHTTP(w, c.Request)

	// the middleware must pass control to the handler
	if !downstreamCalled {
		t.Fatal("RequestLogger did not call Next(); downstream handler never ran")
	}

	// exactly one structured line must be emitted
	var logged map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}

	if logged["msg"] != "http_request" {
		t.Errorf("msg = %v, want http_request", logged["msg"])
	}
	if logged["method"] != http.MethodGet {
		t.Errorf("method = %v, want GET", logged["method"])
	}
	if logged["path"] != "/widgets" {
		t.Errorf("path = %v, want /widgets", logged["path"])
	}
	// status is recorded after Next(), so it reflects the handler's response
	if status, _ := logged["status"].(float64); int(status) != http.StatusTeapot {
		t.Errorf("status = %v, want %d", logged["status"], http.StatusTeapot)
	}
	if _, ok := logged["duration_ms"]; !ok {
		t.Error("log line missing duration_ms")
	}
	if _, ok := logged["ip"]; !ok {
		t.Error("log line missing ip")
	}
}

// A 500 must be logged at Error with the cause a handler attached via c.Error,
// so failures are never silently swallowed.
func TestRequestLogger_LogsErrorCause(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)
	engine.Use(RequestLogger())
	engine.GET("/boom", func(c *gin.Context) {
		_ = c.Error(errors.New("db exploded"))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	})

	c.Request = httptest.NewRequest(http.MethodGet, "/boom", nil)
	engine.ServeHTTP(w, c.Request)

	var logged map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	if logged["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", logged["level"])
	}
	if errStr, _ := logged["err"].(string); errStr == "" || !bytes.Contains([]byte(errStr), []byte("db exploded")) {
		t.Errorf("err = %v, want it to contain the attached cause", logged["err"])
	}
}

// Liveness probes must not produce log lines.
func TestRequestLogger_SkipsHealthz(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)
	engine.Use(RequestLogger())
	engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })

	c.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	engine.ServeHTTP(w, c.Request)

	if buf.Len() != 0 {
		t.Errorf("expected no log output for /healthz, got %q", buf.String())
	}
}

func TestRequestID(t *testing.T) {
	t.Run("generates when absent and exposes in context + header", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, engine := gin.CreateTestContext(w)

		var gotID string
		engine.Use(RequestID())
		engine.GET("/x", func(c *gin.Context) {
			gotID = applog.RequestIDFromContext(c.Request.Context())
			c.Status(http.StatusOK)
		})

		c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
		engine.ServeHTTP(w, c.Request)

		if gotID == "" {
			t.Fatal("request ID not propagated into context")
		}
		if h := w.Header().Get(requestIDHeader); h != gotID {
			t.Errorf("response header %s = %q, want %q", requestIDHeader, h, gotID)
		}
	})

	t.Run("honors inbound header", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, engine := gin.CreateTestContext(w)

		var gotID string
		engine.Use(RequestID())
		engine.GET("/x", func(c *gin.Context) {
			gotID = applog.RequestIDFromContext(c.Request.Context())
			c.Status(http.StatusOK)
		})

		c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
		c.Request.Header.Set(requestIDHeader, "upstream-id")
		engine.ServeHTTP(w, c.Request)

		if gotID != "upstream-id" {
			t.Errorf("request ID = %q, want upstream-id", gotID)
		}
	})
}

func TestRecovery(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)
	engine.Use(Recovery())
	engine.GET("/panic", func(c *gin.Context) { panic("kaboom") })

	c.Request = httptest.NewRequest(http.MethodGet, "/panic", nil)
	engine.ServeHTTP(w, c.Request)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	var logged map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	if logged["msg"] != "panic recovered" {
		t.Errorf("msg = %v, want 'panic recovered'", logged["msg"])
	}
	if logged["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", logged["level"])
	}
	if _, ok := logged["stack"]; !ok {
		t.Error("panic log missing stack trace")
	}
}
