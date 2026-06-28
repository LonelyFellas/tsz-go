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
	"github.com/darwish/tsz-go/internal/otp"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestHandler wires a Handler over fakes and returns them so tests can seed
// data and drive HTTP requests.
func newTestHandler() (*Handler, *fakeStore, *fakeCodes, *fakeSessions, *auth.TokenManager) {
	store := newFakeStore()
	codes := newFakeCodes()
	sessions := newFakeSessions()
	tm := auth.NewTokenManager("test-secret", time.Hour, auth.RealmWeb)
	return NewHandler(NewService(store, tm, codes, sessions), CookieConfig{MaxAge: time.Hour}, 15*time.Minute, time.Hour), store, codes, sessions, tm
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

// doCookie drives a handler with the refresh token presented as a cookie, the
// way the refresh/logout endpoints now read it.
func doCookie(t *testing.T, h gin.HandlerFunc, refresh string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	if refresh != "" {
		c.Request.AddCookie(&http.Cookie{Name: refreshCookieName, Value: refresh})
	}
	h(c)
	return w
}

// refreshCookieValue extracts the refresh_token cookie set on a response.
func refreshCookieValue(w *httptest.ResponseRecorder) string {
	res := http.Response{Header: w.Header()}
	for _, ck := range res.Cookies() {
		if ck.Name == refreshCookieName {
			return ck.Value
		}
	}
	return ""
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
		{"valid email only", `{"email":"a@b.com","password":"password123","display_name":"Alice","role":"student"}`, http.StatusCreated},
		{"missing phone and email", `{"password":"password123","display_name":"Alice","role":"student"}`, http.StatusBadRequest},
		// phone is optional, but a supplied phone must still satisfy min length even
		// when a valid email is present — omitempty must not disable the rule.
		{"too-short phone with valid email", `{"phone":"12","email":"a@b.com","password":"password123","display_name":"Alice","role":"student"}`, http.StatusBadRequest},
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
	// the refresh token must NOT be in the body — it rides in an HttpOnly cookie
	if _, ok := m["refresh_token"]; ok {
		t.Error("refresh_token leaked into response body; it must be cookie-only")
	}
	if refreshCookieValue(w) == "" {
		t.Error("response missing refresh_token cookie")
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

// TestHandler_RefreshCookieAttributes locks in the security-critical cookie
// flags: HttpOnly (XSS can't read it), SameSite=Strict (CSRF defense), and the
// auth-scoped path. Secure tracks the configured value.
func TestHandler_RefreshCookieAttributes(t *testing.T) {
	store := newFakeStore()
	tm := auth.NewTokenManager("test-secret", time.Hour, auth.RealmWeb)
	h := NewHandler(NewService(store, tm, newFakeCodes(), newFakeSessions()), CookieConfig{Secure: true, MaxAge: time.Hour}, 15*time.Minute, time.Hour)

	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"a@b.com","password":"password123","display_name":"A","role":"student"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d", w.Code)
	}

	var ck *http.Cookie
	for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
		if c.Name == refreshCookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("no refresh_token cookie set")
	}
	if !ck.HttpOnly {
		t.Error("refresh cookie must be HttpOnly")
	}
	if !ck.Secure {
		t.Error("refresh cookie must be Secure when configured Secure=true")
	}
	if ck.SameSite != http.SameSiteStrictMode {
		t.Errorf("refresh cookie SameSite = %v, want Strict", ck.SameSite)
	}
	if ck.Path != refreshCookiePath {
		t.Errorf("refresh cookie Path = %q, want %q", ck.Path, refreshCookiePath)
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

func TestHandler_SendCode_RateLimited(t *testing.T) {
	h, _, codes, _, _ := newTestHandler()
	codes.reqFn = func(string, string) error { return otp.ErrRateLimited }

	if w := doJSON(t, h.SendCode, `{"identifier":"13800138000"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("send-code status = %d, want 429 (body: %s)", w.Code, w.Body)
	}
}

func TestHandler_ForgotAndResetPassword(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	// seed a user via Register
	_ = doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)

	// forgot is always 200, even for an unknown phone (no account probing)
	if w := doJSON(t, h.ForgotPassword, `{"identifier":"13800138000"}`); w.Code != http.StatusOK {
		t.Fatalf("forgot status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	if w := doJSON(t, h.ForgotPassword, `{"identifier":"19999999999"}`); w.Code != http.StatusOK {
		t.Fatalf("forgot unknown status = %d, want 200", w.Code)
	}
	if w := doJSON(t, h.ForgotPassword, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("forgot missing identifier status = %d, want 400", w.Code)
	}

	// the fake issues "123456"; resetting with it succeeds
	if w := doJSON(t, h.ResetPassword, `{"identifier":"13800138000","code":"123456","new_password":"newpassword456"}`); w.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	// the new password now logs in, the old one does not
	if w := doJSON(t, h.Login, `{"identifier":"13800138000","password":"newpassword456"}`); w.Code != http.StatusOK {
		t.Fatalf("login with new password status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	if w := doJSON(t, h.Login, `{"identifier":"13800138000","password":"password123"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password status = %d, want 401", w.Code)
	}
}

func TestHandler_ResetPassword_Errors(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	_ = doJSON(t, h.Register, `{"phone":"13800138000","email":"re@b.com","password":"password123","display_name":"RE","role":"student"}`)
	_ = doJSON(t, h.ForgotPassword, `{"identifier":"13800138000"}`)

	// wrong code → 400
	if w := doJSON(t, h.ResetPassword, `{"identifier":"13800138000","code":"000000","new_password":"newpassword456"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong-code status = %d, want 400 (body: %s)", w.Code, w.Body)
	}
	// short new password → 400 (binding)
	if w := doJSON(t, h.ResetPassword, `{"identifier":"13800138000","code":"123456","new_password":"short"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("short-password status = %d, want 400", w.Code)
	}
	// missing code → 400 (binding)
	if w := doJSON(t, h.ResetPassword, `{"identifier":"13800138000","new_password":"newpassword456"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing-code status = %d, want 400", w.Code)
	}
}

func TestHandler_ForgotPassword_RateLimited(t *testing.T) {
	h, _, codes, _, _ := newTestHandler()
	codes.reqFn = func(string, string) error { return otp.ErrRateLimited }

	if w := doJSON(t, h.ForgotPassword, `{"identifier":"13800138000"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("forgot status = %d, want 429 (body: %s)", w.Code, w.Body)
	}
}

// registerAndGetUserID seeds a user via Register and returns their ID, the way an
// authed handler would receive it from the access token's subject.
func registerAndGetUserID(t *testing.T, h *Handler, body string) uuid.UUID {
	t.Helper()
	w := doJSON(t, h.Register, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d (body: %s)", w.Code, w.Body)
	}
	user, _ := decode(t, w)["user"].(map[string]any)
	id, err := uuid.Parse(user["id"].(string))
	if err != nil {
		t.Fatalf("parse user id: %v", err)
	}
	return id
}

func TestHandler_RequestAndDeleteAccount(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	userID := registerAndGetUserID(t, h, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)

	// request a deletion code over the phone channel → 200
	if w := doJSONAuthed(t, h.RequestAccountDeletion, `{"channel":"phone"}`, userID); w.Code != http.StatusOK {
		t.Fatalf("request-deletion status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	// the fake issues "123456"; deleting with it → 204
	if w := doJSONAuthed(t, h.DeleteAccount, `{"channel":"phone","code":"123456"}`, userID); w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body: %s)", w.Code, w.Body)
	}
	// the account is gone: a second delete with the same token → 404
	if w := doJSONAuthed(t, h.DeleteAccount, `{"channel":"phone","code":"123456"}`, userID); w.Code != http.StatusNotFound {
		t.Fatalf("post-delete status = %d, want 404 (body: %s)", w.Code, w.Body)
	}
}

func TestHandler_DeleteAccount_Errors(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	// phone-only account: the email channel is unavailable
	userID := registerAndGetUserID(t, h, `{"phone":"13800138000","password":"password123","display_name":"NE","role":"student"}`)
	_ = doJSONAuthed(t, h.RequestAccountDeletion, `{"channel":"phone"}`, userID)

	// wrong code → 400
	if w := doJSONAuthed(t, h.DeleteAccount, `{"channel":"phone","code":"000000"}`, userID); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong-code status = %d, want 400 (body: %s)", w.Code, w.Body)
	}
	// missing code → 400 (binding)
	if w := doJSONAuthed(t, h.DeleteAccount, `{"channel":"phone"}`, userID); w.Code != http.StatusBadRequest {
		t.Fatalf("missing-code status = %d, want 400", w.Code)
	}
	// bad channel → 400 (binding oneof)
	if w := doJSONAuthed(t, h.DeleteAccount, `{"channel":"carrier-pigeon","code":"123456"}`, userID); w.Code != http.StatusBadRequest {
		t.Fatalf("bad-channel status = %d, want 400", w.Code)
	}
	// email channel on a phone-only account → 400 (channel unavailable), for both
	// the code request and the delete itself
	if w := doJSONAuthed(t, h.RequestAccountDeletion, `{"channel":"email"}`, userID); w.Code != http.StatusBadRequest {
		t.Fatalf("request email-channel status = %d, want 400 (body: %s)", w.Code, w.Body)
	}
	if w := doJSONAuthed(t, h.DeleteAccount, `{"channel":"email","code":"123456"}`, userID); w.Code != http.StatusBadRequest {
		t.Fatalf("delete email-channel status = %d, want 400 (body: %s)", w.Code, w.Body)
	}
}

func TestHandler_RequestAccountDeletion_RateLimited(t *testing.T) {
	h, _, codes, _, _ := newTestHandler()
	userID := registerAndGetUserID(t, h, `{"phone":"13800138000","email":"rl@b.com","password":"password123","display_name":"RL","role":"student"}`)
	codes.reqFn = func(string, string) error { return otp.ErrRateLimited }

	if w := doJSONAuthed(t, h.RequestAccountDeletion, `{"channel":"phone"}`, userID); w.Code != http.StatusTooManyRequests {
		t.Fatalf("request-deletion status = %d, want 429 (body: %s)", w.Code, w.Body)
	}
}

// registerAndGetRefresh seeds a user via Register and returns the refresh token
// from the response cookie.
func registerAndGetRefresh(t *testing.T, h *Handler) string {
	t.Helper()
	w := doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d", w.Code)
	}
	refresh := refreshCookieValue(w)
	if refresh == "" {
		t.Fatal("seed register did not set a refresh token cookie")
	}
	return refresh
}

func TestHandler_Refresh(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	refresh := registerAndGetRefresh(t, h)

	// rotate the refresh token → 200 with a fresh access (body) + rotated refresh
	// (cookie)
	w := doCookie(t, h.Refresh, refresh)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body: %s)", w.Code, w.Body)
	}
	m := decode(t, w)
	if m["access_token"] == "" {
		t.Errorf("refresh response missing access_token: %v", m)
	}
	if _, ok := m["refresh_token"]; ok {
		t.Error("refresh_token leaked into response body; it must be cookie-only")
	}
	rotated := refreshCookieValue(w)
	if rotated == "" || rotated == refresh {
		t.Error("refresh token cookie was not rotated")
	}

	// replaying the old refresh token → 401
	if w := doCookie(t, h.Refresh, refresh); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed refresh status = %d, want 401", w.Code)
	}
	// unknown refresh token → 401
	if w := doCookie(t, h.Refresh, "nope"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown refresh status = %d, want 401", w.Code)
	}
	// no cookie → 401
	if w := doCookie(t, h.Refresh, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing refresh cookie status = %d, want 401", w.Code)
	}
}

func TestHandler_Logout(t *testing.T) {
	h, _, _, _, _ := newTestHandler()
	refresh := registerAndGetRefresh(t, h)

	// logout → 204, and the refresh token is dead afterwards
	if w := doCookie(t, h.Logout, refresh); w.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", w.Code)
	}
	if w := doCookie(t, h.Refresh, refresh); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout refresh status = %d, want 401", w.Code)
	}
	// logout is idempotent: revoking again (or an unknown token) still 204
	if w := doCookie(t, h.Logout, refresh); w.Code != http.StatusNoContent {
		t.Fatalf("idempotent logout status = %d, want 204", w.Code)
	}
	// no cookie → still 204 (idempotent)
	if w := doCookie(t, h.Logout, ""); w.Code != http.StatusNoContent {
		t.Fatalf("logout without cookie status = %d, want 204", w.Code)
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
	refresh := refreshCookieValue(w)
	user, _ := body["user"].(map[string]any)
	userID, err := uuid.Parse(user["id"].(string))
	if err != nil {
		t.Fatalf("parse user id: %v", err)
	}

	// logout-all → 204, and the user's refresh token is dead afterwards
	if w := doJSONAuthed(t, h.LogoutAll, `{}`, userID); w.Code != http.StatusNoContent {
		t.Fatalf("logout-all status = %d, want 204", w.Code)
	}
	if w := doCookie(t, h.Refresh, refresh); w.Code != http.StatusUnauthorized {
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
	userID := claims.Subject

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
		w := doJSONAuthed(t, h.SwitchRole, `{"role":"superuser"}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	// admin is not a web role at all (the back office is a separate identity
	// realm), so it is rejected at the binding layer like any unknown value.
	t.Run("400 admin is not a web role", func(t *testing.T) {
		w := doJSONAuthed(t, h.SwitchRole, `{"role":"admin"}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body)
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
	userID := claims.Subject

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

	// Privilege-escalation guard: add-role must never accept admin, or any
	// authenticated user could self-promote into the back office. Unlike
	// switch-role, admin is rejected at the binding layer here.
	t.Run("400 admin is not self-grantable", func(t *testing.T) {
		w := doJSONAuthed(t, h.AddRole, `{"role":"admin"}`, userID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body)
		}
	})
}

// TestHandler_TokenExpiryFields verifies that every endpoint that issues tokens
// returns expires_in (access TTL in seconds) and refresh_token_expires_at (Unix
// timestamp) and that both values are consistent with the configured TTLs.
func TestHandler_TokenExpiryFields(t *testing.T) {
	const accessTTL = 15 * time.Minute
	const refreshTTL = time.Hour

	// assertExpiry checks expires_in and refresh_token_expires_at in a decoded body.
	assertExpiry := func(t *testing.T, m map[string]any, label string) {
		t.Helper()

		expiresIn, ok := m["expires_in"].(float64)
		if !ok {
			t.Errorf("%s: expires_in missing or wrong type (%T)", label, m["expires_in"])
		} else if int64(expiresIn) != int64(accessTTL.Seconds()) {
			t.Errorf("%s: expires_in = %v, want %v", label, int64(expiresIn), int64(accessTTL.Seconds()))
		}

		expiresAt, ok := m["refresh_token_expires_at"].(float64)
		if !ok {
			t.Errorf("%s: refresh_token_expires_at missing or wrong type (%T)", label, m["refresh_token_expires_at"])
		} else {
			// Allow ±5 s for test execution time.
			want := time.Now().Add(refreshTTL).Unix()
			if diff := int64(expiresAt) - want; diff < -5 || diff > 5 {
				t.Errorf("%s: refresh_token_expires_at = %v, want ~%v (diff %d s)", label, int64(expiresAt), want, diff)
			}
		}
	}

	t.Run("register", func(t *testing.T) {
		h, _, _, _, _ := newTestHandler()
		w := doJSON(t, h.Register, `{"phone":"13800138000","email":"a@b.com","password":"password123","display_name":"A","role":"student"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d", w.Code)
		}
		assertExpiry(t, decode(t, w), "register")
	})

	t.Run("login", func(t *testing.T) {
		h, _, _, _, _ := newTestHandler()
		_ = doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
		w := doJSON(t, h.Login, `{"identifier":"u@b.com","password":"password123"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		assertExpiry(t, decode(t, w), "login")
	})

	t.Run("login_code", func(t *testing.T) {
		h, _, _, _, _ := newTestHandler()
		_ = doJSON(t, h.Register, `{"phone":"13800138000","email":"u@b.com","password":"password123","display_name":"U","role":"student"}`)
		_ = doJSON(t, h.SendCode, `{"identifier":"13800138000"}`)
		w := doJSON(t, h.LoginCode, `{"identifier":"13800138000","code":"123456"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		assertExpiry(t, decode(t, w), "login_code")
	})

	t.Run("refresh", func(t *testing.T) {
		h, _, _, _, _ := newTestHandler()
		refresh := registerAndGetRefresh(t, h)
		w := doCookie(t, h.Refresh, refresh)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		assertExpiry(t, decode(t, w), "refresh")
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
		body := decode(t, w)
		user := body["user"].(map[string]any)
		if user["email"] != "me@b.com" {
			t.Errorf("email = %v", user["email"])
		}
		// A fresh user has not onboarded: learning_settings is null.
		if body["onboarded"] != false {
			t.Errorf("onboarded = %v, want false", body["onboarded"])
		}
		if body["learning_settings"] != nil {
			t.Errorf("learning_settings = %v, want null", body["learning_settings"])
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

func TestHandler_UpdateLearningSettings(t *testing.T) {
	h, _, _, _, tm := newTestHandler()

	// userIDFromRegister registers a user with the given role and returns its ID.
	userIDFromRegister := func(t *testing.T, body string) uuid.UUID {
		t.Helper()
		w := doJSON(t, h.Register, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("register status = %d", w.Code)
		}
		access, _ := decode(t, w)["access_token"].(string)
		claims, err := tm.Parse(access)
		if err != nil {
			t.Fatalf("parse token: %v", err)
		}
		return claims.Subject
	}

	student := userIDFromRegister(t, `{"phone":"13800138000","email":"s@b.com","password":"password123","display_name":"S","role":"student"}`)

	t.Run("200 sets settings and flips onboarded", func(t *testing.T) {
		w := doJSONAuthed(t, h.UpdateLearningSettings, `{"cefr_level":"B1","english_variant":"BrE"}`, student)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		body := decode(t, w)
		if body["onboarded"] != true {
			t.Errorf("onboarded = %v, want true", body["onboarded"])
		}
		ls := body["learning_settings"].(map[string]any)
		if ls["cefr_level"] != "B1" || ls["english_variant"] != "BrE" {
			t.Errorf("learning_settings = %v", ls)
		}

		// Me now reflects the completed onboarding.
		mw := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(mw)
		c.Request = httptest.NewRequest(http.MethodGet, "/me", nil)
		c.Set(auth.ContextUserIDKey, student)
		h.Me(c)
		mb := decode(t, mw)
		if mb["onboarded"] != true {
			t.Errorf("me onboarded = %v, want true", mb["onboarded"])
		}
	})

	t.Run("400 invalid level", func(t *testing.T) {
		w := doJSONAuthed(t, h.UpdateLearningSettings, `{"cefr_level":"Z9","english_variant":"BrE"}`, student)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("400 missing variant", func(t *testing.T) {
		w := doJSONAuthed(t, h.UpdateLearningSettings, `{"cefr_level":"B1"}`, student)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("409 teacher-only user has no student profile", func(t *testing.T) {
		teacher := userIDFromRegister(t, `{"phone":"13900139000","email":"t@b.com","password":"password123","display_name":"T","role":"teacher"}`)
		w := doJSONAuthed(t, h.UpdateLearningSettings, `{"cefr_level":"B1","english_variant":"AmE"}`, teacher)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", w.Code)
		}
	})
}

// Login must reject a disabled account with 403 account disabled, for both
// password and code login.
func TestHandler_Login_Disabled(t *testing.T) {
	seedDisabled := func(t *testing.T, h *Handler, store *fakeStore) {
		t.Helper()
		w := doJSON(t, h.Register, `{"phone":"13800138000","email":"d@b.com","password":"password123","display_name":"D","role":"student"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("seed register status = %d", w.Code)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, u := range store.byID {
			u.Status = StatusDisabled
		}
	}

	t.Run("password login", func(t *testing.T) {
		h, store, _, _, _ := newTestHandler()
		seedDisabled(t, h, store)
		w := doJSON(t, h.Login, `{"identifier":"d@b.com","password":"password123"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if body := decode(t, w); body["error"] != "account disabled" {
			t.Errorf("error = %v, want 'account disabled'", body["error"])
		}
	})

	t.Run("code login", func(t *testing.T) {
		h, store, codes, _, _ := newTestHandler()
		seedDisabled(t, h, store)
		if w := doJSON(t, h.SendCode, `{"identifier":"d@b.com"}`); w.Code != http.StatusOK {
			t.Fatalf("send-code status = %d, want 200", w.Code)
		}
		code := codes.codes["d@b.com"]
		w := doJSON(t, h.LoginCode, `{"identifier":"d@b.com","code":"`+code+`"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if body := decode(t, w); body["error"] != "account disabled" {
			t.Errorf("error = %v, want 'account disabled'", body["error"])
		}
	})
}
