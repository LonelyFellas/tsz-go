package httpserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
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
