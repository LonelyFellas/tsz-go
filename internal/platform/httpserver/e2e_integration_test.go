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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/platform/database"
	"github.com/darwish/tsz-go/internal/user"
)

func buildRealRouter(t *testing.T) http.Handler {
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
	repo := user.NewRepository(pool)
	svc := user.NewService(repo, tm)
	return NewRouter(Deps{TokenManager: tm, UserHandler: user.NewHandler(svc)})
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
	r := buildRealRouter(t)
	email := "e2e-" + uuid.NewString() + "@example.com"
	body := `{"email":"` + email + `","password":"password123","display_name":"E2E"}`

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

	// login → 200, fresh token
	loginBody := `{"email":"` + email + `","password":"password123"}`
	w = req(t, r, http.MethodPost, "/api/v1/auth/login", loginBody, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d", w.Code)
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &login)

	// me with token → 200 and matches the registered user
	w = req(t, r, http.MethodGet, "/api/v1/me", "", login.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("me status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(reg.User.ID)) {
		t.Errorf("me did not return the expected user: %s", w.Body)
	}

	// me without token → 401
	if w := req(t, r, http.MethodGet, "/api/v1/me", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me-without-token status = %d, want 401", w.Code)
	}

	// login wrong password → 401
	bad := `{"email":"` + email + `","password":"wrongpass"}`
	if w := req(t, r, http.MethodPost, "/api/v1/auth/login", bad, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad-login status = %d, want 401", w.Code)
	}
}
