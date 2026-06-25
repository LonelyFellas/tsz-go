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
}

func NewRouter(deps Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), RequestLogger())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	{
		// Public routes.
		v1.POST("/auth/register", deps.UserHandler.Register)
		v1.POST("/auth/login", deps.UserHandler.Login)

		// Authenticated routes.
		authed := v1.Group("")
		authed.Use(AuthRequired(deps.TokenManager))
		{
			authed.GET("/me", deps.UserHandler.Me)
		}
	}

	return r
}
