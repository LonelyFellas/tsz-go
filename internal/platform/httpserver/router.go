// Package httpserver builds the gin router and shared HTTP middleware.
package httpserver

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/darwish/tsz-go/internal/admin"
	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/user"
)

const metricsPath = "/metrics"

// Deps holds everything the router needs to register routes. Add new domain
// handlers here as the app grows.
type Deps struct {
	TokenManager *auth.TokenManager
	UserHandler  *user.Handler
	// AdminTokenManager verifies admin-realm tokens (separate signing key from
	// TokenManager); AdminHandler serves the back-office identity endpoints.
	AdminTokenManager *auth.TokenManager
	AdminHandler      *admin.Handler
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
	// TrustedProxies are the proxy CIDRs/IPs allowed to set X-Forwarded-For.
	// Empty trusts none, so ClientIP() uses the direct peer and XFF can't be
	// spoofed to dodge the per-IP rate limiter. Validated in config.Load.
	TrustedProxies []string
}

func NewRouter(deps Deps) *gin.Engine {
	r := gin.New()

	// Decide whose X-Forwarded-For to trust. gin trusts all proxies by default,
	// which would let any client spoof XFF and mint a fresh rate-limit bucket per
	// request; pinning the trusted set (empty = trust none) closes that. The list
	// is validated in config.Load, so an error here is unexpected — fail closed by
	// trusting none rather than silently reverting to trust-all.
	if err := r.SetTrustedProxies(deps.TrustedProxies); err != nil {
		slog.Error("invalid trusted proxies, trusting none", "err", err)
		_ = r.SetTrustedProxies(nil)
	}

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
		// Forgot-password: send an SMS reset code to a phone, then reset with it.
		public.POST("/password/forgot", deps.UserHandler.ForgotPassword)
		public.POST("/password/reset", deps.UserHandler.ResetPassword)

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
			// Self-service account deletion: request an OTP to the account's own
			// phone/email, then confirm with it to permanently delete the account.
			authed.POST("/auth/account/deletion-code", deps.UserHandler.RequestAccountDeletion)
			authed.DELETE("/auth/account", deps.UserHandler.DeleteAccount)
		}

		// Back office: a separate identity realm with its own token manager and
		// signing key. The /admin/auth/* login subgroup is public (gated only by
		// credentials / the admin refresh cookie) and throttled like web auth;
		// everything else requires a valid admin token, and account management
		// additionally requires super_admin.
		adminGroup := v1.Group("/admin")
		{
			adminAuth := adminGroup.Group("/auth")
			if deps.AuthRateLimiter != nil {
				adminAuth.Use(deps.AuthRateLimiter.Middleware())
			}
			adminAuth.POST("/login", deps.AdminHandler.Login)
			adminAuth.POST("/refresh", deps.AdminHandler.Refresh)
			adminAuth.POST("/logout", deps.AdminHandler.Logout)

			authed := adminGroup.Group("")
			authed.Use(AdminAuthRequired(deps.AdminTokenManager))
			{
				authed.POST("/auth/logout-all", deps.AdminHandler.LogoutAll)
				authed.GET("/profile", deps.AdminHandler.Profile)

				// Super-admin only: manage admin accounts.
				admins := authed.Group("/admins")
				admins.Use(RequireSuperAdmin())
				{
					admins.POST("", deps.AdminHandler.CreateAdmin)
					admins.GET("", deps.AdminHandler.ListAdmins)
					admins.PATCH("/:adminId/status", deps.AdminHandler.SetAdminStatus)
				}
			}
		}
	}

	return r
}
