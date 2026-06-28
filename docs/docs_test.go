package docs

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPISpecEmbedded guards against three regressions: the embed silently
// breaking, the spec becoming invalid YAML (which compiles fine via go:embed but
// white-screens Swagger UI at runtime), and the documented paths drifting out of
// sync with the routes registered in internal/platform/httpserver/router.go.
func TestOpenAPISpecEmbedded(t *testing.T) {
	if len(OpenAPISpec) == 0 {
		t.Fatal("OpenAPISpec is empty — embed failed")
	}

	// Must be valid YAML — a malformed spec passes go:embed but breaks the UI.
	var doc struct {
		OpenAPI string         `yaml:"openapi"`
		Info    map[string]any `yaml:"info"`
		Paths   map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(OpenAPISpec, &doc); err != nil {
		t.Fatalf("spec is not valid YAML: %v", err)
	}

	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi version = %q, want a 3.x version", doc.OpenAPI)
	}
	if doc.Info["title"] == nil {
		t.Error("spec missing info.title")
	}

	// Every routed path must be documented. Keep this in sync with router.go.
	for _, path := range []string{
		"/healthz",
		"/api/v1/auth/register",
		"/api/v1/auth/login",
		"/api/v1/auth/send-code",
		"/api/v1/auth/login/code",
		"/api/v1/auth/refresh",
		"/api/v1/auth/logout",
		"/api/v1/me",
		"/api/v1/me/contact/bind-code",
		"/api/v1/me/contact/bind",
		"/api/v1/auth/logout-all",
		"/api/v1/auth/switch-role",
		"/api/v1/auth/roles",
	} {
		if _, ok := doc.Paths[path]; !ok {
			t.Errorf("spec missing path %q", path)
		}
	}
}
