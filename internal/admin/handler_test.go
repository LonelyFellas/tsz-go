package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// newStatusRouter wires just the SetAdminStatus route onto a bare gin engine.
// SetAdminStatus reads no auth context (the super-admin gate lives in middleware,
// covered separately), so no context seeding is needed here.
func newStatusRouter(svc *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(svc, CookieConfig{}, time.Minute, time.Hour)
	r := gin.New()
	r.PATCH("/admin/admins/:adminId/status", h.SetAdminStatus)
	return r
}

// TestHandler_SetAdminStatus pins the handler's error→HTTP-status mapping:
// the service logic and the middleware gate are tested elsewhere, this asserts
// the wiring in between (404/409/400/200).
func TestHandler_SetAdminStatus(t *testing.T) {
	tests := []struct {
		name string
		// setup seeds the store and returns the path id + request body.
		setup func(t *testing.T, svc *Service) (id, body string)
		want  int
	}{
		{
			name: "disable a plain admin → 200",
			setup: func(t *testing.T, svc *Service) (string, string) {
				a := seedActive(t, svc, "13800138000", "password123", LevelAdmin)
				return a.ID.String(), `{"status":"disabled"}`
			},
			want: http.StatusOK,
		},
		{
			name: "unknown admin id → 404",
			setup: func(_ *testing.T, _ *Service) (string, string) {
				return uuid.NewString(), `{"status":"disabled"}`
			},
			want: http.StatusNotFound,
		},
		{
			name: "disable the last active super_admin → 409",
			setup: func(t *testing.T, svc *Service) (string, string) {
				a := seedActive(t, svc, "13800138000", "password123", LevelSuperAdmin)
				return a.ID.String(), `{"status":"disabled"}`
			},
			want: http.StatusConflict,
		},
		{
			name: "malformed admin id → 400",
			setup: func(_ *testing.T, _ *Service) (string, string) {
				return "not-a-uuid", `{"status":"disabled"}`
			},
			want: http.StatusBadRequest,
		},
		{
			name: "invalid status value → 400",
			setup: func(t *testing.T, svc *Service) (string, string) {
				a := seedActive(t, svc, "13800138000", "password123", LevelAdmin)
				return a.ID.String(), `{"status":"frozen"}`
			},
			want: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := newTestService()
			id, body := tt.setup(t, svc)
			r := newStatusRouter(svc)

			req := httptest.NewRequest(http.MethodPatch, "/admin/admins/"+id+"/status", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tt.want, w.Body.String())
			}
		})
	}
}
