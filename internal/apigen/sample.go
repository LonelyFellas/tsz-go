package apigen

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SampleServer is a stand-in implementation of the generated ServerInterface.
//
// This is the whole point of the design-first shape: the spec (docs/openapi.yaml)
// defines the route and the MeResponse type, oapi-codegen generates the interface
// and the typed models, and a handler just fills in the typed response. There is
// no hand-written request/response struct to drift, and the compiler enforces
// that the handler matches the spec — drop or rename a field in the spec, regen,
// and code that no longer matches stops compiling. That replaces runtime contract
// tests with a build-time guarantee.
//
// In a real migration, internal/user.Handler would implement ServerInterface and
// load the user from the service; here we return a fixed value just to exercise
// the generated wiring end to end.
type SampleServer struct{}

// Compile-time check that SampleServer satisfies the generated interface.
var _ ServerInterface = (*SampleServer)(nil)

// GetCurrentUser implements GET /api/v1/me.
func (SampleServer) GetCurrentUser(c *gin.Context) {
	now := time.Now()
	c.JSON(http.StatusOK, MeResponse{
		ActiveRole: Student,
		User: User{
			Id:          uuid.New(),
			Phone:       "13800138000",
			DisplayName: "Alice",
			Roles:       []Role{Student, Teacher},
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	})
}
