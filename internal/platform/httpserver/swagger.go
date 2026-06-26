package httpserver

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

// swaggerUIAssets holds a pinned copy of the Swagger UI bundle (swagger-ui-dist
// v5.17.14) so the docs render fully offline — no CDN or network access needed.
// Refresh by re-downloading swagger-ui.css and swagger-ui-bundle.js from
// https://cdn.jsdelivr.net/npm/swagger-ui-dist@<version>/ into ./swaggerui.
//
//go:embed swaggerui/swagger-ui.css swaggerui/swagger-ui-bundle.js
var swaggerUIAssets embed.FS

// swaggerUIHTML renders the OpenAPI spec served at /docs/openapi.yaml using the
// locally embedded Swagger UI assets.
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>tsz-go API — Swagger UI</title>
  <link rel="stylesheet" href="/docs/static/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="/docs/static/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: '/docs/openapi.yaml',
      dom_id: '#swagger-ui',
      withCredentials: true,
    });
  </script>
</body>
</html>`

// registerDocs mounts the Swagger UI at /docs, its static assets under
// /docs/static, and the raw OpenAPI spec at /docs/openapi.yaml. It is a no-op
// when spec is empty so the server still boots even if the spec was not embedded.
func registerDocs(r *gin.Engine, spec []byte) {
	if len(spec) == 0 {
		return
	}

	r.GET("/docs", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(swaggerUIHTML))
	})
	r.GET("/docs/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", spec)
	})
	// Serve the embedded bundle. StaticFS strips the mount path, so the FS is
	// rooted at swaggerui/ to match the embedded paths.
	assets, _ := fs.Sub(swaggerUIAssets, "swaggerui")
	r.StaticFS("/docs/static", http.FS(assets))
}
