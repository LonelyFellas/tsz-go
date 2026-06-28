//go:build integration

// End-to-end tests through the real router, middleware, service, repository and
// a live Postgres — the closest thing to a black-box test of the running server.
//
//	make test-integration
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/otp"
	"github.com/darwish/tsz-go/internal/platform/database"
	"github.com/darwish/tsz-go/internal/session"
	"github.com/darwish/tsz-go/internal/user"
)

// buildRealRouter wires the full stack and returns the router plus the mock OTP
// sender, so the test can read back the code that was "sent".
func buildRealRouter(t *testing.T) (http.Handler, *otp.MockSender) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping e2e test")
	}
	if err := database.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := database.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	tm := auth.NewTokenManager("e2e-secret", time.Hour, auth.RealmWeb)
	sender := otp.NewMockSender()
	otpSvc := otp.NewService(otp.NewRepository(pool), sender, time.Minute, 0, 0) // rate limiting off; tested in otp unit tests
	sessionSvc := session.NewService(session.NewRepository(pool), time.Hour)
	repo := user.NewRepository(pool)
	svc := user.NewService(repo, tm, otpSvc, sessionSvc)
	return NewRouter(Deps{TokenManager: tm, UserHandler: user.NewHandler(svc, user.CookieConfig{MaxAge: time.Hour}, 15*time.Minute, time.Hour)}), sender
}

// refreshCookieFrom pulls the refresh_token cookie value out of a response. The
// refresh token rides in an HttpOnly cookie, not the JSON body.
func refreshCookieFrom(w *httptest.ResponseRecorder) string {
	res := http.Response{Header: w.Header()}
	for _, ck := range res.Cookies() {
		if ck.Name == "refresh_token" {
			return ck.Value
		}
	}
	return ""
}

// loginTokens logs in by password and returns the access token (body) and the
// refresh token (Set-Cookie).
func loginTokens(t *testing.T, r http.Handler, identifier, password string) (access, refresh string) {
	t.Helper()
	w := req(t, r, http.MethodPost, "/api/v1/auth/login",
		`{"identifier":"`+identifier+`","password":"`+password+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", w.Code, w.Body)
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return resp.AccessToken, refreshCookieFrom(w)
}

// refreshTokens calls /auth/refresh with the refresh token presented as a cookie
// (as a browser would), and returns the recorder so the caller can read the new
// access token (body) and rotated refresh token (Set-Cookie).
func refreshTokens(t *testing.T, r http.Handler, refresh string) *httptest.ResponseRecorder {
	t.Helper()
	return reqCookie(t, r, http.MethodPost, "/api/v1/auth/refresh", "", refresh)
}

// TestE2E_RefreshAndSingleDevice covers the refresh-token rotation and strict
// single-device guarantees end-to-end against a live Postgres.
func TestE2E_RefreshAndSingleDevice(t *testing.T) {
	r, _ := buildRealRouter(t)
	email := "e2e-rt-" + uuid.NewString() + "@example.com"
	phone := fmt.Sprintf("1%010d", time.Now().UnixNano()%1e10)
	body := `{"phone":"` + phone + `","email":"` + email + `","password":"password123","display_name":"RT","role":"student"}`

	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, ""); w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", w.Code, w.Body)
	}

	// ---- refresh rotation ----
	// log in on "device 1", then refresh to get a new access + rotated refresh
	_, refresh1 := loginTokens(t, r, email, "password123")

	w := refreshTokens(t, r, refresh1)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body=%s", w.Code, w.Body)
	}
	var rotated struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	rotatedRefresh := refreshCookieFrom(w)
	if rotated.AccessToken == "" || rotatedRefresh == "" {
		t.Fatal("refresh response missing access token (body) or refresh cookie")
	}
	// the new access token works against an authed route
	if w := req(t, r, http.MethodGet, "/api/v1/me", "", rotated.AccessToken); w.Code != http.StatusOK {
		t.Fatalf("me with refreshed access status = %d", w.Code)
	}
	// replaying the old (now rotated) refresh token → 401
	if w := refreshTokens(t, r, refresh1); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed old refresh status = %d, want 401", w.Code)
	}
	// the rotated refresh token is the live one
	refresh2 := rotatedRefresh

	// ---- strict single-device ----
	// "device 2" logs in; this must revoke device 1's (rotated) refresh token
	_, deviceB := loginTokens(t, r, email, "password123")
	if w := refreshTokens(t, r, refresh2); w.Code != http.StatusUnauthorized {
		t.Fatalf("kicked device-1 refresh status = %d, want 401", w.Code)
	}
	// device 2 still refreshes fine
	w = refreshTokens(t, r, deviceB)
	if w.Code != http.StatusOK {
		t.Fatalf("device-2 refresh status = %d, want 200", w.Code)
	}
	deviceBRefresh := refreshCookieFrom(w)

	// ---- logout ----
	// logout device 2 → 204; afterwards its refresh token is dead
	if w := reqCookie(t, r, http.MethodPost, "/api/v1/auth/logout", "", deviceBRefresh); w.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", w.Code)
	}
	if w := refreshTokens(t, r, deviceBRefresh); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout refresh status = %d, want 401", w.Code)
	}
	// logout is idempotent
	if w := reqCookie(t, r, http.MethodPost, "/api/v1/auth/logout", "", deviceBRefresh); w.Code != http.StatusNoContent {
		t.Fatalf("idempotent logout status = %d, want 204", w.Code)
	}
}

// TestE2E_AccountDeletion drives the self-service account-deletion flow end to
// end against the real DB. It is the only test that exercises migration 000013
// (persisting a code with purpose 'account_deletion' must not violate the
// verification_codes CHECK), and it proves the delete cascades: the user, their
// roles/profiles and their sessions are all gone, and the phone is free to reuse.
func TestE2E_AccountDeletion(t *testing.T) {
	r, sender := buildRealRouter(t)
	email := "e2e-" + uuid.NewString() + "@example.com"
	phone := fmt.Sprintf("1%010d", time.Now().UnixNano()%1e10)

	body := `{"phone":"` + phone + `","email":"` + email + `","password":"password123","display_name":"DEL","role":"student"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, ""); w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", w.Code, w.Body)
	}
	access, refresh := loginTokens(t, r, phone, "password123")

	// the deletion endpoints require auth → 401 without a token
	if w := req(t, r, http.MethodPost, "/api/v1/auth/account/deletion-code", `{"channel":"phone"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth deletion-code status = %d, want 401", w.Code)
	}

	// request a deletion code over the phone channel, read it from the mock sender
	if w := req(t, r, http.MethodPost, "/api/v1/auth/account/deletion-code", `{"channel":"phone"}`, access); w.Code != http.StatusOK {
		t.Fatalf("deletion-code status = %d, body=%s", w.Code, w.Body)
	}
	code := sender.LastCode(phone)
	if code == "" {
		t.Fatal("mock sender did not capture a deletion code")
	}

	// a wrong code does not delete the account → 400
	if w := req(t, r, http.MethodDelete, "/api/v1/auth/account", `{"channel":"phone","code":"000000"}`, access); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong-code delete status = %d, want 400, body=%s", w.Code, w.Body)
	}

	// delete with the real code → 204
	if w := req(t, r, http.MethodDelete, "/api/v1/auth/account", `{"channel":"phone","code":"`+code+`"}`, access); w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204, body=%s", w.Code, w.Body)
	}

	// the delete cascaded to sessions: the pre-delete refresh token is dead
	if w := reqCookie(t, r, http.MethodPost, "/api/v1/auth/refresh", "", refresh); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-delete refresh status = %d, want 401", w.Code)
	}
	// the account is gone: neither password login nor a stale access token works
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login", `{"identifier":"`+phone+`","password":"password123"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("login after delete status = %d, want 401", w.Code)
	}
	if w := req(t, r, http.MethodGet, "/api/v1/me", "", access); w.Code != http.StatusNotFound {
		t.Fatalf("me with stale token status = %d, want 404", w.Code)
	}

	// the phone is free to reuse: a fresh registration with it succeeds
	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, ""); w.Code != http.StatusCreated {
		t.Fatalf("re-register with freed phone status = %d, want 201, body=%s", w.Code, w.Body)
	}
}

func req(t *testing.T, r http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Buffer
	if body != "" {
		rdr = bytes.NewBufferString(body)
	} else {
		rdr = bytes.NewBuffer(nil)
	}
	httpReq := httptest.NewRequest(method, path, rdr)
	httpReq.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	return w
}

// reqCookie is like req but presents the refresh token as a cookie (as a browser
// would), used for the cookie-based /auth/refresh and /auth/logout endpoints.
func reqCookie(t *testing.T, r http.Handler, method, path, body, refresh string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Buffer
	if body != "" {
		rdr = bytes.NewBufferString(body)
	} else {
		rdr = bytes.NewBuffer(nil)
	}
	httpReq := httptest.NewRequest(method, path, rdr)
	httpReq.Header.Set("Content-Type", "application/json")
	if refresh != "" {
		httpReq.AddCookie(&http.Cookie{Name: "refresh_token", Value: refresh})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	return w
}

func TestE2E_RegisterLoginMe(t *testing.T) {
	r, sender := buildRealRouter(t)
	email := "e2e-" + uuid.NewString() + "@example.com"
	phone := fmt.Sprintf("1%010d", time.Now().UnixNano()%1e10)
	body := `{"phone":"` + phone + `","email":"` + email + `","password":"password123","display_name":"E2E","role":"student"}`

	// healthz
	if w := req(t, r, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", w.Code)
	}

	// register
	w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", w.Code, w.Body)
	}
	var reg struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if reg.AccessToken == "" || reg.User.ID == "" {
		t.Fatal("register response missing access token or user id")
	}
	// the refresh token must arrive as an HttpOnly cookie, not in the body
	if refreshCookieFrom(w) == "" {
		t.Fatal("register response missing refresh_token cookie")
	}

	// duplicate register → 409
	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, ""); w.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d, want 409", w.Code)
	}

	// password login by email → 200, fresh token
	loginBody := `{"identifier":"` + email + `","password":"password123"}`
	w = req(t, r, http.MethodPost, "/api/v1/auth/login", loginBody, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d", w.Code)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &login)

	// password login by phone → 200
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login", `{"identifier":"`+phone+`","password":"password123"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("phone login status = %d", w.Code)
	}

	// code login: request a code (always 200), read it from the mock sender, log in
	if w := req(t, r, http.MethodPost, "/api/v1/auth/send-code", `{"identifier":"`+phone+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("send-code status = %d", w.Code)
	}
	code := sender.LastCode(phone)
	if code == "" {
		t.Fatal("mock sender did not capture a code")
	}
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login/code", `{"identifier":"`+phone+`","code":"`+code+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("login-by-code status = %d, body=%s", w.Code, w.Body)
	}
	// the code is single-use: replaying it → 401
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login/code", `{"identifier":"`+phone+`","code":"`+code+`"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code status = %d, want 401", w.Code)
	}

	// me with token → 200 and matches the registered user
	w = req(t, r, http.MethodGet, "/api/v1/me", "", login.AccessToken)
	if w.Code != http.StatusOK {
		t.Fatalf("me status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(reg.User.ID)) {
		t.Errorf("me did not return the expected user: %s", w.Body)
	}
	// a freshly registered user has not completed onboarding
	if !bytes.Contains(w.Body.Bytes(), []byte(`"onboarded":false`)) {
		t.Errorf("new user should report onboarded:false: %s", w.Body)
	}

	// complete onboarding: set CEFR level + English variant
	if w := req(t, r, http.MethodPut, "/api/v1/me/learning-settings", `{"cefr_level":"B1","english_variant":"BrE"}`, login.AccessToken); w.Code != http.StatusOK {
		t.Fatalf("update learning-settings status = %d, body=%s", w.Code, w.Body)
	}
	// me now reflects the completed onboarding and the chosen settings
	w = req(t, r, http.MethodGet, "/api/v1/me", "", login.AccessToken)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"onboarded":true`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"cefr_level":"B1"`)) {
		t.Errorf("me should reflect completed onboarding: %s", w.Body)
	}

	// switching to a role the user does not yet hold → 403
	if w := req(t, r, http.MethodPost, "/api/v1/auth/switch-role", `{"role":"teacher"}`, login.AccessToken); w.Code != http.StatusForbidden {
		t.Fatalf("switch-role-unowned status = %d, want 403", w.Code)
	}

	// acquire the teacher identity → 201 with a token already switched to it
	w = req(t, r, http.MethodPost, "/api/v1/auth/roles", `{"role":"teacher"}`, login.AccessToken)
	if w.Code != http.StatusCreated {
		t.Fatalf("add-role status = %d, body=%s", w.Code, w.Body)
	}
	var added struct {
		AccessToken string `json:"access_token"`
		ActiveRole  string `json:"active_role"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if added.ActiveRole != "teacher" {
		t.Errorf("added active_role = %q, want teacher", added.ActiveRole)
	}

	// me now reports both roles
	w = req(t, r, http.MethodGet, "/api/v1/me", "", added.AccessToken)
	if w.Code != http.StatusOK {
		t.Fatalf("me status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"active_role":"teacher"`)) {
		t.Errorf("me active_role not teacher: %s", w.Body)
	}

	// switching back to student now succeeds → 200
	if w := req(t, r, http.MethodPost, "/api/v1/auth/switch-role", `{"role":"student"}`, login.AccessToken); w.Code != http.StatusOK {
		t.Fatalf("switch-role status = %d, want 200", w.Code)
	}

	// me without token → 401
	if w := req(t, r, http.MethodGet, "/api/v1/me", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me-without-token status = %d, want 401", w.Code)
	}

	// login wrong password → 401
	bad := `{"identifier":"` + email + `","password":"wrongpass"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login", bad, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad-login status = %d, want 401", w.Code)
	}
}

// TestE2E_PasswordReset drives the forgot-password flow end to end against the
// real DB. It is also the only test that exercises migration 000012: persisting
// a code with purpose 'password_reset' must not violate the verification_codes
// CHECK constraint (the in-memory fake does not enforce it, so unit tests can't
// catch a missing/incorrect migration here).
func TestE2E_PasswordReset(t *testing.T) {
	r, sender := buildRealRouter(t)
	email := "e2e-" + uuid.NewString() + "@example.com"
	phone := fmt.Sprintf("1%010d", time.Now().UnixNano()%1e10)

	// register, then log in to get a session we can later prove the reset revoked
	body := `{"phone":"` + phone + `","email":"` + email + `","password":"password123","display_name":"PR","role":"student"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, ""); w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", w.Code, w.Body)
	}
	_, refresh := loginTokens(t, r, phone, "password123")

	// request a reset code (always 200), read it from the mock sender
	if w := req(t, r, http.MethodPost, "/api/v1/auth/password/forgot", `{"identifier":"`+phone+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("forgot status = %d, body=%s", w.Code, w.Body)
	}
	code := sender.LastCode(phone)
	if code == "" {
		t.Fatal("mock sender did not capture a reset code")
	}

	// reset with the code → 200
	resetBody := `{"identifier":"` + phone + `","code":"` + code + `","new_password":"newpassword456"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/password/reset", resetBody, ""); w.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body=%s", w.Code, w.Body)
	}

	// the reset revoked all prior sessions: the pre-reset refresh token is dead.
	// Checked before any new login, so it proves the reset (not a later login) did it.
	if w := reqCookie(t, r, http.MethodPost, "/api/v1/auth/refresh", "", refresh); w.Code != http.StatusUnauthorized {
		t.Fatalf("pre-reset refresh status = %d, want 401", w.Code)
	}

	// old password no longer works; the new one does
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login", `{"identifier":"`+phone+`","password":"password123"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password status = %d, want 401", w.Code)
	}
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login", `{"identifier":"`+phone+`","password":"newpassword456"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("login with new password status = %d, want 200, body=%s", w.Code, w.Body)
	}

	// the reset code is single-use: replaying it → 400
	if w := req(t, r, http.MethodPost, "/api/v1/auth/password/reset", resetBody, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("replayed reset code status = %d, want 400", w.Code)
	}
}

// TestE2E_EmailOnlyRegistration exercises migration 000014 against the real DB:
// an account may register with an email and no phone. This is the path the
// migration unlocks (phone NULL, the partial unique index, and the
// "phone or email present" CHECK), which the in-memory fake can't validate. It
// then proves the account is fully usable email-side: email login works, and the
// forgot-password code is delivered to and verified against the email.
func TestE2E_EmailOnlyRegistration(t *testing.T) {
	r, sender := buildRealRouter(t)
	email := "e2e-eo-" + uuid.NewString() + "@example.com"

	// register with no phone at all → 201 (would fail before 000014)
	body := `{"email":"` + email + `","password":"password123","display_name":"EO","role":"student"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", body, ""); w.Code != http.StatusCreated {
		t.Fatalf("email-only register status = %d, body=%s", w.Code, w.Body)
	}

	// the account logs in by email
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login",
		`{"identifier":"`+email+`","password":"password123"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("email login status = %d, body=%s", w.Code, w.Body)
	}

	// forgot-password over the email channel: the code is sent to the email and
	// the reset succeeds, proving the identifier-based flow end to end.
	if w := req(t, r, http.MethodPost, "/api/v1/auth/password/forgot", `{"identifier":"`+email+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("forgot status = %d, body=%s", w.Code, w.Body)
	}
	code := sender.LastCode(email)
	if code == "" {
		t.Fatal("mock sender did not capture a reset code for the email")
	}
	resetBody := `{"identifier":"` + email + `","code":"` + code + `","new_password":"newpassword456"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/password/reset", resetBody, ""); w.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body=%s", w.Code, w.Body)
	}

	// the new password (set via the email reset) logs in; the old one does not
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login",
		`{"identifier":"`+email+`","password":"newpassword456"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("login with new password status = %d, want 200, body=%s", w.Code, w.Body)
	}
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login",
		`{"identifier":"`+email+`","password":"password123"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password status = %d, want 401", w.Code)
	}

	// ---- account deletion for an email-only account ----
	access, _ := loginTokens(t, r, email, "newpassword456")

	// the phone channel is unavailable for an account with no phone → 400, and no
	// code is sent. This is the deletionTarget guard that opened up once phone
	// became optional.
	if w := req(t, r, http.MethodPost, "/api/v1/auth/account/deletion-code", `{"channel":"phone"}`, access); w.Code != http.StatusBadRequest {
		t.Fatalf("phone-channel deletion-code status = %d, want 400 (no phone on file), body=%s", w.Code, w.Body)
	}

	// the email channel works: request a code, then confirm the deletion with it.
	if w := req(t, r, http.MethodPost, "/api/v1/auth/account/deletion-code", `{"channel":"email"}`, access); w.Code != http.StatusOK {
		t.Fatalf("email-channel deletion-code status = %d, body=%s", w.Code, w.Body)
	}
	delCode := sender.LastCode(email)
	if delCode == "" {
		t.Fatal("mock sender did not capture a deletion code for the email")
	}
	if w := req(t, r, http.MethodDelete, "/api/v1/auth/account", `{"channel":"email","code":"`+delCode+`"}`, access); w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204, body=%s", w.Code, w.Body)
	}

	// the account is gone: email login no longer works, and the email is free to reuse.
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login",
		`{"identifier":"`+email+`","password":"newpassword456"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("login after delete status = %d, want 401", w.Code)
	}
	reuse := `{"email":"` + email + `","password":"password123","display_name":"EO2","role":"student"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/register", reuse, ""); w.Code != http.StatusCreated {
		t.Fatalf("re-register with freed email status = %d, want 201, body=%s", w.Code, w.Body)
	}
}
