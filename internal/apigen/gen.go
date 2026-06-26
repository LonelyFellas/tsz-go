// Package apigen is a design-first sample: Go types and a gin server interface
// generated from docs/openapi.yaml by oapi-codegen, with the OpenAPI spec as the
// single source of truth. It is scoped to one operation (getCurrentUser) to show
// the shape; the real handlers in internal/user are untouched. See sample.go for
// how a handler plugs into the generated interface.
package apigen

//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../docs/openapi.yaml
