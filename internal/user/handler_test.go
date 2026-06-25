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

// newTestHandler wires a Handler over a fake store and returns both so tests
// can seed data and drive HTTP requests.
func newTestHandler() (*Handler, *fakeStore, *auth.TokenManager) {
	store := newFakeStore()
	tm := auth.NewTokenManager("test-secret", time.Hour)
	return NewHandler(NewService(store, tm)), store, tm
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
		{"valid", `{"email":"a@b.com","password":"password123","display_name":"Alice"}`, http.StatusCreated},
		{"invalid email", `{"email":"not-an-email","password":"password123","display_name":"Alice"}`, http.StatusBadRequest},
		{"password too short", `{"email":"a@b.com","password":"short","display_name":"Alice"}`, http.StatusBadRequest},
		{"password too long", `{"email":"a@b.com","password":"` + strings.Repeat("x", 73) + `","display_name":"Alice"}`, http.StatusBadRequest},
		{"missing display name", `{"email":"a@b.com","password":"password123"}`, http.StatusBadRequest},
		{"empty body", `{}`, http.StatusBadRequest},
		{"malformed json", `{not json`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			w := doJSON(t, h.Register, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body)
			}
		})
	}
}

func TestHandler_Register_SuccessShape(t *testing.T) {
	h, _, tm := newTestHandler()
	w := doJSON(t, h.Register, `{"email":"a@b.com","password":"password123","display_name":"Alice"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	m := decode(t, w)

	// token must be present and valid
	token, _ := m["token"].(string)
	if token == "" {
		t.Fatal("response missing token")
	}
	if _, err := tm.Parse(token); err != nil {
		t.Errorf("returned token invalid: %v", err)
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
	h, _, _ := newTestHandler()
	body := `{"email":"dup@b.com","password":"password123","display_name":"Alice"}`

	if w := doJSON(t, h.Register, body); w.Code != http.StatusCreated {
		t.Fatalf("first register status = %d", w.Code)
	}
	w := doJSON(t, h.Register, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", w.Code)
	}
}

func TestHandler_Login(t *testing.T) {
	h, _, _ := newTestHandler()
	// seed a user via Register
	_ = doJSON(t, h.Register, `{"email":"u@b.com","password":"password123","display_name":"U"}`)

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"success", `{"email":"u@b.com","password":"password123"}`, http.StatusOK},
		{"wrong password", `{"email":"u@b.com","password":"nope"}`, http.StatusUnauthorized},
		{"unknown email", `{"email":"ghost@b.com","password":"password123"}`, http.StatusUnauthorized},
		{"invalid email format", `{"email":"bad","password":"password123"}`, http.StatusBadRequest},
		{"missing password", `{"email":"u@b.com"}`, http.StatusBadRequest},
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

// The 500 branches: when the store returns an unexpected (non-domain) error,
// every handler must respond 500 and must not leak the internal error.
func TestHandler_InternalErrors(t *testing.T) {
	boom := errors.New("db down")

	t.Run("register 500", func(t *testing.T) {
		h, store, _ := newTestHandler()
		store.createFn = func(*User) error { return boom }
		w := doJSON(t, h.Register, `{"email":"a@b.com","password":"password123","display_name":"A"}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "db down") {
			t.Errorf("response leaks internal error: %s", w.Body)
		}
	})

	t.Run("login 500", func(t *testing.T) {
		h, store, _ := newTestHandler()
		store.getEmail = func(string) (*User, error) { return nil, boom }
		w := doJSON(t, h.Login, `{"email":"a@b.com","password":"password123"}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("me 500", func(t *testing.T) {
		h, store, _ := newTestHandler()
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

func TestHandler_Me(t *testing.T) {
	h, store, _ := newTestHandler()

	// seed a user directly in the store
	id := uuid.New()
	_ = store.Create(nil, &User{ID: id, Email: "me@b.com", PasswordHash: "x", DisplayName: "Me"})

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
