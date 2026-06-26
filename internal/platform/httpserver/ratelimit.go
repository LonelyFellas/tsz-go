package httpserver

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// IPRateLimiter throttles requests per client IP with a token bucket per IP. It
// guards the unauthenticated auth endpoints (login, send-code, …), where the
// per-target OTP limits in internal/otp don't stop credential stuffing or an
// attacker cycling through identifiers from a single host.
//
// Buckets are created lazily and swept after an idle period so memory stays
// bounded under a churn of distinct IPs.
//
// Note: it keys on gin's ClientIP(), which trusts X-Forwarded-For from
// configured proxies. Behind a proxy/LB, set the engine's trusted proxies so a
// client can't spoof the header and mint a fresh bucket per request.
type IPRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*ipBucket
	lastedAt time.Time // last sweep time

	limit      rate.Limit
	burst      int
	idleTTL    time.Duration
	retryAfter string // precomputed Retry-After header value (seconds)
}

type ipBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewIPRateLimiter builds a limiter allowing perMinute sustained requests per IP
// with room for a short burst. Idle buckets are evicted after idleTTL.
func NewIPRateLimiter(perMinute, burst int, idleTTL time.Duration) *IPRateLimiter {
	limit := rate.Limit(float64(perMinute) / 60.0)
	// Seconds to refill one token, surfaced to the client via Retry-After.
	retrySecs := int(math.Ceil(60.0 / float64(perMinute)))
	return &IPRateLimiter{
		buckets:    make(map[string]*ipBucket),
		lastedAt:   time.Now(),
		limit:      limit,
		burst:      burst,
		idleTTL:    idleTTL,
		retryAfter: strconv.Itoa(retrySecs),
	}
}

// allow records the IP and reports whether the request is within budget.
func (l *IPRateLimiter) allow(ip string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, ok := l.buckets[ip]
	if !ok {
		b = &ipBucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[ip] = b
	}
	b.lastSeen = now
	return b.limiter.Allow()
}

// sweep evicts buckets idle longer than idleTTL. It runs at most once per
// idleTTL window (the caller already holds the mutex), so it adds negligible
// overhead to the hot path.
func (l *IPRateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastedAt) < l.idleTTL {
		return
	}
	l.lastedAt = now
	for ip, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.idleTTL {
			delete(l.buckets, ip)
		}
	}
}

// Middleware throttles by client IP, returning 429 with a Retry-After header
// once an IP exhausts its budget.
func (l *IPRateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.allow(c.ClientIP()) {
			c.Header("Retry-After", l.retryAfter)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests, slow down"})
			return
		}
		c.Next()
	}
}
