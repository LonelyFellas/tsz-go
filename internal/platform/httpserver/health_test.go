package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakePinger stands in for *pgxpool.Pool in readiness tests.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestReadiness(t *testing.T) {
	tests := []struct {
		name string
		db   Pinger
		want int
	}{
		{"nil db reports ready", nil, http.StatusOK},
		{"healthy db is ready", fakePinger{}, http.StatusOK},
		{"unreachable db is unavailable", fakePinger{err: errors.New("conn refused")}, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, engine := gin.CreateTestContext(w)
			engine.GET("/readyz", readinessHandler(tt.db))

			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestLiveness(t *testing.T) {
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.GET("/healthz", livenessHandler)

	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if want := `{"status":"ok"}`; w.Body.String() != want {
		t.Errorf("body = %s, want %s", w.Body.String(), want)
	}
}
