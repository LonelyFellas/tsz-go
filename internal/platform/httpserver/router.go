// Package httpserver builds the gin router and shared HTTP middleware.
package httpserver

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/user"
)

const metricsPath = "/metrics"

// Deps holds everything the router needs to register routes. Add new domain
// handlers here as the app grows.
type Deps struct {
	TokenManager *auth.TokenManager
	UserHandler  *user.Handler
	// OpenAPISpec is the raw OpenAPI document served at /docs/openapi.yaml and
	// rendered by the Swagger UI at /docs. Docs are mounted only when EnableDocs
	// is true and the spec is non-empty.
	OpenAPISpec []byte
	EnableDocs  bool
	// AuthRateLimiter throttles the public auth endpoints per client IP. Nil
	// disables the throttle (e.g. in tests, or when configured off).
	AuthRateLimiter *IPRateLimiter
	// DB backs the /readyz probe. Nil makes readiness a pure liveness check.
	DB Pinger
	// Metrics, when set, mounts the Prometheus middleware and /metrics endpoint.
	Metrics *Metrics
	// ServiceName labels the otelgin trace spans.
	ServiceName string
}

func NewRouter(deps Deps) *gin.Engine {
	r := gin.New()

	// RequestID first so everything downstream can stamp the ID. otelgin opens a
	// span per request (a no-op tracer when tracing is disabled). Metrics sits
	// outside Recovery so a recovered panic is still counted as a 500. Recovery
	// before RequestLogger so a panic still produces a request log line.
	r.Use(RequestID())
	r.Use(otelgin.Middleware(deps.ServiceName))
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware())
	}
	r.Use(Recovery(), RequestLogger())

	// Liveness ("is the process up?") and readiness ("can it serve traffic?").
	// Orchestrators probe these separately, so they are kept distinct.
	r.GET("/healthz", livenessHandler)
	r.GET("/readyz", readinessHandler(deps.DB))

	if deps.Metrics != nil {
		r.GET(metricsPath, deps.Metrics.Handler())
	}

	if deps.EnableDocs {
		registerDocs(r, deps.OpenAPISpec)
	}

	v1 := r.Group("/api/v1")
	{
		// Public auth routes. Throttled per client IP to blunt credential
		// stuffing and SMS/email abuse from a single host; the per-target OTP
		// limits in internal/otp are the complementary second layer.
		public := v1.Group("/auth")
		if deps.AuthRateLimiter != nil {
			public.Use(deps.AuthRateLimiter.Middleware())
		}
		public.POST("/register", deps.UserHandler.Register)
		public.POST("/login", deps.UserHandler.Login)          // identifier + password
		public.POST("/send-code", deps.UserHandler.SendCode)   // request a login code
		public.POST("/login/code", deps.UserHandler.LoginCode) // identifier + code
		public.POST("/refresh", deps.UserHandler.Refresh)      // rotate refresh → new access
		public.POST("/logout", deps.UserHandler.Logout)        // revoke a refresh token

		// Authenticated routes.
		authed := v1.Group("")
		authed.Use(AuthRequired(deps.TokenManager))
		{
			authed.GET("/me", deps.UserHandler.Me)
			// Set the learner's CEFR level + English variant (onboarding & settings).
			authed.PUT("/me/learning-settings", deps.UserHandler.UpdateLearningSettings)
			// Revoke every refresh token the user holds (logout everywhere).
			authed.POST("/auth/logout-all", deps.UserHandler.LogoutAll)
			// Switch the active role to one the user already holds.
			authed.POST("/auth/switch-role", deps.UserHandler.SwitchRole)
			// Acquire an additional identity (e.g. a student who also teaches).
			authed.POST("/auth/roles", deps.UserHandler.AddRole)
		}

		// Back-office routes, gated on the admin active role. AuthRequired runs
		// first (401 for a missing/invalid token), then RequireRole (403 unless the
		// token is acting as admin). Phase A exposes only the profile probe.
		admin := v1.Group("/admin")
		admin.Use(AuthRequired(deps.TokenManager), RequireRole(auth.RoleAdmin))
		{
			admin.GET("/profile", deps.UserHandler.AdminProfile)
		}
	}

	return r
}
