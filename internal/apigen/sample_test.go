package apigen

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSampleServer_GetCurrentUser proves the generated wiring works end to end:
// RegisterHandlers mounts the spec's route, and the typed MeResponse marshals to
// the JSON shape the spec promises.
func TestSampleServer_GetCurrentUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterHandlers(r, SampleServer{})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got MeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a valid MeResponse: %v\nbody: %s", err, w.Body)
	}
	if got.ActiveRole != Student {
		t.Errorf("active_role = %q, want %q", got.ActiveRole, Student)
	}
	if !got.ActiveRole.Valid() {
		t.Errorf("active_role %q is not a valid Role", got.ActiveRole)
	}
	if got.User.DisplayName != "Alice" {
		t.Errorf("user.display_name = %q, want Alice", got.User.DisplayName)
	}
}
