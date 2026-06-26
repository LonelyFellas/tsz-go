package httpserver

import (
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
