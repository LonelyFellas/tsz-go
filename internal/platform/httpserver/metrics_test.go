package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMetrics_RecordsAndExposes(t *testing.T) {
	m := NewMetrics()
	engine := gin.New()
	engine.Use(m.Middleware())
	engine.GET("/api/v1/thing", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET(metricsPath, m.Handler())

	// One request to record, then scrape.
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil))

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, metricsPath, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `http_requests_total{method="GET",route="/api/v1/thing",status="200"} 1`) {
		t.Errorf("missing recorded request counter:\n%s", body)
	}
	if !strings.Contains(body, "http_request_duration_seconds") {
		t.Error("missing duration histogram")
	}
	// The scrape endpoint must not measure itself (would grow on every scrape).
	if strings.Contains(body, `route="/metrics"`) {
		t.Error("/metrics scrape should be excluded from its own metrics")
	}
}

// Unmatched paths collapse to a single "unmatched" label so a flood of random
// URLs can't explode the metric cardinality.
func TestMetrics_UnmatchedRouteLabel(t *testing.T) {
	m := NewMetrics()
	engine := gin.New()
	engine.Use(m.Middleware())
	engine.GET(metricsPath, m.Handler())

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/no/such/path", nil))

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, metricsPath, nil))

	if !strings.Contains(w.Body.String(), `route="unmatched"`) {
		t.Errorf("expected route=\"unmatched\" label:\n%s", w.Body.String())
	}
}
