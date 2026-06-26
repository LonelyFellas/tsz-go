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

	tm := auth.NewTokenManager("e2e-secret", time.Hour)
	sender := otp.NewMockSender()
	otpSvc := otp.NewService(otp.NewRepository(pool), sender, time.Minute)
	repo := user.NewRepository(pool)
	svc := user.NewService(repo, tm, otpSvc)
	return NewRouter(Deps{TokenManager: tm, UserHandler: user.NewHandler(svc)}), sender
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
		Token string `json:"token"`
		User  struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if reg.Token == "" || reg.User.ID == "" {
		t.Fatal("register response missing token or user id")
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
		Token string `json:"token"`
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
	w = req(t, r, http.MethodGet, "/api/v1/me", "", login.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("me status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(reg.User.ID)) {
		t.Errorf("me did not return the expected user: %s", w.Body)
	}

	// switching to a role the user does not yet hold → 403
	if w := req(t, r, http.MethodPost, "/api/v1/auth/switch-role", `{"role":"teacher"}`, login.Token); w.Code != http.StatusForbidden {
		t.Fatalf("switch-role-unowned status = %d, want 403", w.Code)
	}

	// acquire the teacher identity → 201 with a token already switched to it
	w = req(t, r, http.MethodPost, "/api/v1/auth/roles", `{"role":"teacher"}`, login.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("add-role status = %d, body=%s", w.Code, w.Body)
	}
	var added struct {
		Token      string `json:"token"`
		ActiveRole string `json:"active_role"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if added.ActiveRole != "teacher" {
		t.Errorf("added active_role = %q, want teacher", added.ActiveRole)
	}

	// me now reports both roles
	w = req(t, r, http.MethodGet, "/api/v1/me", "", added.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("me status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"active_role":"teacher"`)) {
		t.Errorf("me active_role not teacher: %s", w.Body)
	}

	// switching back to student now succeeds → 200
	if w := req(t, r, http.MethodPost, "/api/v1/auth/switch-role", `{"role":"student"}`, login.Token); w.Code != http.StatusOK {
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
