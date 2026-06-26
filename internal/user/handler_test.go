package user

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestHandler wires a Handler over fakes and returns them so tests can seed
// data and drive HTTP requests.
func newTestHandler() (*Handler, *fakeStore, *fakeCodes, *fakeSessions, *auth.TokenManager) {
	store := newFakeStore()
	codes := newFakeCodes()
	sessions := newFakeSessions()
	tm := auth.NewTokenManager("test-secret", time.Hour)
	return NewHandler(NewService(store, tm, codes, sessions)), store, codes, sessions, tm
}

func doJSON(t *testing.T, h gin.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h(c)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestHandler_Register(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"valid", `{"phone":"13800138000","email":"a@b.com","password":"password123","display_name":"Alice","role":"student"}`, http.StatusCreated},
		{"valid no email", `{"phone":"13800138001","password":"password123","display_name":"Bob","role":"teacher"}`, http.StatusCreated},
		{"missing phone", `{"email":"a@b.com","password":"password123","display_name":"Alice","role":"student"}`, http.StatusBadRequest},
		{"invalid email", `{"phone":"13800138000","email":"not-an-email","password":"password123","display_name":"Alice","role":"student"}`, http.StatusBadRequest},
		{"password too short", `{"phone":"13800138000","password":"short","display_name":"Alice","role":"student"}`, http.StatusBadRequest},
		{"password too long", `{"phone":"13800138000","password":"` + strings.Repeat("x", 73) + `","display_name":"Alice","role":"student"}`, http.StatusBadRequest},
		{"missing display name", `{"phone":"13800138000","password":"password123","role":"student"}`, http.StatusBadRequest},
		{"missing role", `{"phone":"13800138000","password":"password123","display_name":"Alice"}`, http.StatusBadRequest},
		{"invalid role", `{"phone":"13800138000","password":"password123","display_name":"Alice","role":"admin"}`, http.StatusBadRequest},
		{"empty body", `{}`, http.StatusBadRequest},
		{"malformed json", `{not json`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, _, _ := newTestHandler()
			w := doJSON(t, h.Register, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body)
			}
		})
	}
}

func TestHandler_Register_SuccessShape(t *testing.T) {
	h, _, _, _, tm := newTestHandler()
	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"a@b.com","password":"password123","display_name":"Alice","role":"student"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	m := decode(t, w)

	// the active role is echoed back
	if m["active_role"] != "student" {
		t.Errorf("active_role = %v, want student", m["active_role"])
	}

	// access token must be present and valid
	access, _ := m["access_token"].(string)
	if access == "" {
		t.Fatal("response missing access_token")
	}
	if _, err := tm.Parse(access); err != nil {
		t.Errorf("returned access token invalid: %v", err)
	}
	// a refresh token must be returned alongside it
	if refresh, _ := m["refresh_token"].(string); refresh == "" {
		t.Error("response missing refresh_token")
	}

	// user object must NOT leak the password hash
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), "PasswordHash") {
		t.Errorf("response leaks password hash: %s", w.Body)
	}
	user, _ := m["user"].(map[string]any)
	if user["email"] != "a@b.com" {
		t.Errorf("user.email = %v", user["email"])
	}
}

func TestHandler_Register_Duplicate409(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	body := `{"phone":"13800138000","email":"dup@b.com","password":"password123","display_name":"Alice","role":"student"}`

	if w := doJSON(t, h.Register, body); w.Code != http.StatusCreated {
		t.Fatalf("first register status = %d", w.Code)
	}
	w := doJSON(t, h.Register, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", w.Code)
	}
}

func TestHandler_Login(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	// seed a user via Register
	_ = doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"success by email", `{"identifier":"u@b.com","password":"password123"}`, http.StatusOK},
		{"success by phone", `{"identifier":"13800138000","password":"password123"}`, http.StatusOK},
		{"wrong password", `{"identifier":"u@b.com","password":"nope"}`, http.StatusUnauthorized},
		{"unknown identifier", `{"identifier":"ghost@b.com","password":"password123"}`, http.StatusUnauthorized},
		{"missing identifier", `{"password":"password123"}`, http.StatusBadRequest},
		{"missing password", `{"identifier":"u@b.com"}`, http.StatusBadRequest},
		{"malformed json", `nope`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doJSON(t, h.Login, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body)
			}
		})
	}
}

func TestHandler_SendCodeAndLoginCode(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	// seed a user via Register
	_ = doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)

	// send-code is always 200, even for an unknown identifier (no account probing)
	if w := doJSON(t, h.SendCode, `{"identifier":"13800138000"}`); w.Code != http.StatusOK {
		t.Fatalf("send-code status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	if w := doJSON(t, h.SendCode, `{"identifier":"19999999999"}`); w.Code != http.StatusOK {
		t.Fatalf("send-code unknown status = %d, want 200", w.Code)
	}
	if w := doJSON(t, h.SendCode, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("send-code missing identifier status = %d, want 400", w.Code)
	}

	// the fake issues "123456"; logging in with it succeeds
	if w := doJSON(t, h.LoginCode, `{"identifier":"13800138000","code":"123456"}`); w.Code != http.StatusOK {
		t.Fatalf("login-code status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	// wrong code → 401
	if w := doJSON(t, h.LoginCode, `{"identifier":"13800138000","code":"000000"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("login-code wrong status = %d, want 401", w.Code)
	}
	// missing code → 400
	if w := doJSON(t, h.LoginCode, `{"identifier":"13800138000"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("login-code missing code status = %d, want 400", w.Code)
	}
}

// registerAndGetRefresh seeds a user via Register and returns the refresh token
// from the response.
func registerAndGetRefresh(t *testing.T, h *Handler) string {
	t.Helper()
	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d", w.Code)
	}
	refresh, _ := decode(t, w)["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("seed register did not return a refresh token")
	}
	return refresh
}

func TestHandler_Refresh(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	refresh := registerAndGetRefresh(t, h)

	// rotate the refresh token → 200 with a fresh access + rotated refresh
	w := doJSON(t, h.Refresh, `{"refresh_token":"`+refresh+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	m := decode(t, w)
	if m["access_token"] == "" || m["refresh_token"] == "" {
		t.Errorf("refresh response missing tokens: %v", m)
	}
	rotated, _ := m["refresh_token"].(string)
	if rotated == refresh {
		t.Error("refresh token was not rotated")
	}

	// replaying the old refresh token → 401
	if w := doJSON(t, h.Refresh, `{"refresh_token":"`+refresh+`"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed refresh status = %d, want 401", w.Code)
	}
	// unknown refresh token → 401
	if w := doJSON(t, h.Refresh, `{"refresh_token":"nope"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown refresh status = %d, want 401", w.Code)
	}
	// missing field → 400
	if w := doJSON(t, h.Refresh, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing refresh_token status = %d, want 400", w.Code)
	}
}

func TestHandler_Logout(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	refresh := registerAndGetRefresh(t, h)

	// logout → 204, and the refresh token is dead afterwards
	if w := doJSON(t, h.Logout, `{"refresh_token":"`+refresh+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", w.Code)
	}
	if w := doJSON(t, h.Refresh, `{"refresh_token":"`+refresh+`"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout refresh status = %d, want 401", w.Code)
	}
	// logout is idempotent: revoking again (or an unknown token) still 204
	if w := doJSON(t, h.Logout, `{"refresh_token":"`+refresh+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("idempotent logout status = %d, want 204", w.Code)
	}
	// missing field → 400
	if w := doJSON(t, h.Logout, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing refresh_token status = %d, want 400", w.Code)
	}
}

func TestHandler_LogoutAll(t *testing.T) {
	h, _, _, _, _ := newTestHandler()

	// Register seeds a user + refresh token; grab the user ID too so we can drive
	// LogoutAll the way AuthRequired would (from the access token's subject).
	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d", w.Code)
	}
	body := decode(t, w)
	refresh, _ := body["refresh_token"].(string)
	user, _ := body["user"].(map[string]any)
	userID, err := uuid.Parse(user["id"].(string))
	if err != nil {
		t.Fatalf("parse user id: %v", err)
	}

	// logout-all → 204, and the user's refresh token is dead afterwards
	if w := doJSONAuthed(t, h.LogoutAll, `{}`, userID); w.Code != http.StatusNoContent {
		t.Fatalf("logout-all status = %d, want 204", w.Code)
	}
	if w := doJSON(t, h.Refresh, `{"refresh_token":"`+refresh+`"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout-all refresh status = %d, want 401", w.Code)
	}
	// idempotent: a user with no active sessions still returns 204
	if w := doJSONAuthed(t, h.LogoutAll, `{}`, userID); w.Code != http.StatusNoContent {
		t.Fatalf("idempotent logout-all status = %d, want 204", w.Code)
	}
}

// The 500 branches: when the store returns an unexpected (non-domain) error,
// every handler must respond 500 and must not leak the internal error.
func TestHandler_InternalErrors(t *testing.T) {
	boom := errors.New("db down")

	t.Run("register 500", func(t *testing.T) {
		h, store, _, _, _ := newTestHandler()
		store.createFn = func(*User, Role) error { return boom }
		w := doJSON(t, h.Register, `{"phone":"13800138000","email":"a@b.com","password":"password123","display_name":"A","role":"student"}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "db down") {
			t.Errorf("response leaks internal error: %s", w.Body)
		}
	})

	t.Run("login 500", func(t *testing.T) {
		h, store, _, _, _ := newTestHandler()
		store.getEmail = func(string) (*User, error) { return nil, boom }
		w := doJSON(t, h.Login, `{"identifier":"a@b.com","password":"password123"}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("me 500", func(t *testing.T) {
		h, store, _, _, _ := newTestHandler()
		store.getID = func(uuid.UUID) (*User, error) { return nil, boom }
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/me", nil)
		c.Set(auth.ContextUserIDKey, uuid.New())
		h.Me(c)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}

// doJSONAuthed is like doJSON but injects a userID into the gin context to
// simulate a request that has already passed AuthRequired middleware.
func doJSONAuthed(t *testing.T, h gin.HandlerFunc, body string, userID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(auth.ContextUserIDKey, userID)
	h(c)
	return w
}

func TestHandler_SwitchRole(t *testing.T) {
	h, _, _, _, tm := newTestHandler()
	// seed a student; register returns a student token
	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d", w.Code)
	}
	m := decode(t, w)
	access, _ := m["access_token"].(string)
	claims, err := tm.Parse(access)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	userID := claims.UserID

	t.Run("200 valid role switch", func(t *testing.T) {
		w := doJSONAuthed(t, h.SwitchRole, `{"role":"student"}`, userID)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body)
		}
		m := decode(t, w)
		if m["active_role"] != "student" {
			t.Errorf("active_role = %v, want student", m["active_role"])
		}
		if token, _ := m["access_token"].(string); token == "" {
			t.Error("response missing access_token")
		}
	})

	t.Run("403 user does not hold the role", func(t *testing.T) {
		w := doJSONAuthed(t, h.SwitchRole, `{"role":"teacher"}`, userID)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body)
		}
	})

	t.Run("400 missing role field", func(t *testing.T) {
		w := doJSONAuthed(t, h.SwitchRole, `{}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("400 invalid role value", func(t *testing.T) {
		w := doJSONAuthed(t, h.SwitchRole, `{"role":"admin"}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})
}

func TestHandler_AddRole(t *testing.T) {
	h, _, _, _, tm := newTestHandler()
	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d", w.Code)
	}
	claims, err := tm.Parse(decode(t, w)["access_token"].(string))
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	userID := claims.UserID

	t.Run("201 new role added", func(t *testing.T) {
		w := doJSONAuthed(t, h.AddRole, `{"role":"teacher"}`, userID)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body)
		}
		m := decode(t, w)
		if m["active_role"] != "teacher" {
			t.Errorf("active_role = %v, want teacher", m["active_role"])
		}
		if token, _ := m["access_token"].(string); token == "" {
			t.Error("response missing access_token")
		}
	})

	t.Run("409 user already has role", func(t *testing.T) {
		// student role was set at register; adding it again → 409
		w := doJSONAuthed(t, h.AddRole, `{"role":"student"}`, userID)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body)
		}
	})

	t.Run("400 missing role field", func(t *testing.T) {
		w := doJSONAuthed(t, h.AddRole, `{}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("400 invalid role value", func(t *testing.T) {
		w := doJSONAuthed(t, h.AddRole, `{"role":"superuser"}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})
}

func TestHandler_Me(t *testing.T) {
	h, store, _, _, _ := newTestHandler()

	// seed a user directly in the store
	id := uuid.New()
	_ = store.Create(nil, &User{ID: id, Phone: "13800138000", Email: "me@b.com", PasswordHash: "x", DisplayName: "Me"}, RoleStudent)

	t.Run("found", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/me", nil)
		c.Set(auth.ContextUserIDKey, id) // simulate AuthRequired middleware
		h.Me(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		user := decode(t, w)["user"].(map[string]any)
		if user["email"] != "me@b.com" {
			t.Errorf("email = %v", user["email"])
		}
	})

	t.Run("user vanished", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/me", nil)
		c.Set(auth.ContextUserIDKey, uuid.New()) // valid token, but no such user
		h.Me(c)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
}
