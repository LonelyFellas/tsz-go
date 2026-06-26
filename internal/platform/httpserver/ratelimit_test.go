package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// An IP gets burst requests, then 429s; a different IP is unaffected (buckets
// are keyed per IP).
func TestIPRateLimiter_Middleware(t *testing.T) {
	const burst = 3
	// 60/min => 1 token/sec refill, so within a tight loop only the burst lands.
	rl := NewIPRateLimiter(60, burst, time.Minute)

	engine := gin.New()
	engine.Use(rl.Middleware())
	engine.POST("/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	call := func(ip string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		req.RemoteAddr = ip + ":12345"
		engine.ServeHTTP(w, req)
		return w.Code
	}

	// First `burst` requests from one IP pass.
	for i := 0; i < burst; i++ {
		if code := call("1.2.3.4"); code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, code)
		}
	}
	// The next is throttled, with a Retry-After hint.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.RemoteAddr = "1.2.3.4:12345"
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}

	// A different IP still has its full budget.
	if code := call("5.6.7.8"); code != http.StatusOK {
		t.Errorf("second IP status = %d, want 200 (buckets must be per-IP)", code)
	}
}

// Buckets idle past idleTTL are evicted on the next sweep, keeping memory bounded.
func TestIPRateLimiter_SweepEvictsIdle(t *testing.T) {
	rl := NewIPRateLimiter(60, 1, 10*time.Millisecond)

	rl.allow("9.9.9.9")
	if got := len(rl.buckets); got != 1 {
		t.Fatalf("bucket count = %d, want 1 after first call", got)
	}

	// Wait past the idle TTL, then touch a different IP to trigger a sweep.
	time.Sleep(20 * time.Millisecond)
	rl.allow("8.8.8.8")

	if _, exists := rl.buckets["9.9.9.9"]; exists {
		t.Error("idle bucket 9.9.9.9 should have been swept")
	}
	if _, exists := rl.buckets["8.8.8.8"]; !exists {
		t.Error("active bucket 8.8.8.8 should remain")
	}
}
