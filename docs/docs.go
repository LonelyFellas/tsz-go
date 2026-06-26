// Package docs embeds the OpenAPI specification so it can be served by the HTTP
// server without depending on the file being present at runtime. The spec is the
// source of truth for the Swagger UI mounted at /docs (see internal/platform/httpserver).
package docs

import _ "embed"

// OpenAPISpec is the raw OpenAPI 3.0 document (YAML) for the tsz-go API.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
