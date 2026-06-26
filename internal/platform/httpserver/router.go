// Package httpserver builds the gin router and shared HTTP middleware.
package httpserver

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/user"
)

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
}

func NewRouter(deps Deps) *gin.Engine {
	r := gin.New()
	// RequestID first so Recovery and RequestLogger can stamp the ID; Recovery
	// before RequestLogger so a panic still produces a request log line.
	r.Use(RequestID(), Recovery(), RequestLogger())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	if deps.EnableDocs {
		registerDocs(r, deps.OpenAPISpec)
	}

	v1 := r.Group("/api/v1")
	{
		// Public routes.
		v1.POST("/auth/register", deps.UserHandler.Register)
		v1.POST("/auth/login", deps.UserHandler.Login)          // identifier + password
		v1.POST("/auth/send-code", deps.UserHandler.SendCode)   // request a login code
		v1.POST("/auth/login/code", deps.UserHandler.LoginCode) // identifier + code
		v1.POST("/auth/refresh", deps.UserHandler.Refresh)      // rotate refresh → new access
		v1.POST("/auth/logout", deps.UserHandler.Logout)        // revoke a refresh token

		// Authenticated routes.
		authed := v1.Group("")
		authed.Use(AuthRequired(deps.TokenManager))
		{
			authed.GET("/me", deps.UserHandler.Me)
			// Revoke every refresh token the user holds (logout everywhere).
			authed.POST("/auth/logout-all", deps.UserHandler.LogoutAll)
			// Switch the active role to one the user already holds.
			authed.POST("/auth/switch-role", deps.UserHandler.SwitchRole)
			// Acquire an additional identity (e.g. a student who also teaches).
			authed.POST("/auth/roles", deps.UserHandler.AddRole)
		}
	}

	return r
}
