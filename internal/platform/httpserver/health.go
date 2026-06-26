package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Pinger reports whether a backing dependency is reachable. *pgxpool.Pool
// satisfies it, so the readiness probe can verify the database round-trips.
type Pinger interface {
	Ping(ctx context.Context) error
}

// livenessHandler answers "is the process up?" — a pure, dependency-free check
// so an orchestrator's liveness probe never restarts a healthy process just
// because a downstream (e.g. the DB) is briefly unavailable.
func livenessHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readinessHandler answers "can this instance serve traffic?" — it pings the DB
// so a load balancer stops routing to an instance that is up but can't reach its
// database. With no Pinger wired (e.g. in tests) it reports ready.
func readinessHandler(db Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if db == nil {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "reason": "database"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}
