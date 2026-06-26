package httpserver

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the RED (Rate, Errors, Duration) instruments for HTTP traffic
// plus a dedicated registry. The registry is private (not the global default)
// so tests can build independent instances and nothing leaks across them.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewMetrics builds the registry, registers the standard Go/process collectors
// alongside the HTTP instruments, and returns the handle used by the middleware
// and the /metrics handler.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by method, route and status.",
	}, []string{"method", "route", "status"})

	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	reg.MustRegister(requests, duration)

	return &Metrics{registry: reg, requests: requests, duration: duration}
}

// Middleware records the request count and latency. It labels by the matched
// route template (c.FullPath(), e.g. "/api/v1/auth/login") rather than the raw
// URL, so high-cardinality path segments (IDs) can't explode the series count.
// The /metrics scrape itself is skipped to avoid self-measurement noise.
func (m *Metrics) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == metricsPath {
			c.Next()
			return
		}
		if route == "" {
			route = "unmatched"
		}

		start := time.Now()
		c.Next()

		status := strconv.Itoa(c.Writer.Status())
		m.requests.WithLabelValues(c.Request.Method, route, status).Inc()
		m.duration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
	}
}

// Handler serves the registry in the Prometheus text exposition format.
func (m *Metrics) Handler() gin.HandlerFunc {
	h := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return gin.WrapH(h)
}
