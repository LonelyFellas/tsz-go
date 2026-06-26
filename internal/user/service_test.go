package user

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/darwish/tsz-go/internal/auth"
)

func newTestService() (*Service, *fakeStore, *fakeCodes) {
	store := newFakeStore()
	codes := newFakeCodes()
	tm := auth.NewTokenManager("test-secret", time.Hour)
	return NewService(store, tm, codes), store, codes
}

func TestService_Register_Success(t *testing.T) {
	svc, store, _ := newTestService()

	u, token, err := svc.Register(context.Background(), " 13800138000 ", "Alice@Example.com", "password123", "  Alice  ", RoleStudent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// phone is trimmed, email normalized to lowercase, display name trimmed
	if u.Phone != "13800138000" {
		t.Errorf("phone = %q, want trimmed", u.Phone)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("email = %q, want normalized lowercase", u.Email)
	}
	if u.DisplayName != "Alice" {
		t.Errorf("display name = %q, want trimmed", u.DisplayName)
	}
	// the registered role is recorded on the user
	if len(u.Roles) != 1 || u.Roles[0] != RoleStudent {
		t.Errorf("roles = %v, want [student]", u.Roles)
	}
	// password is hashed, never stored plaintext, and verifies
	if u.PasswordHash == "" || u.PasswordHash == "password123" {
		t.Error("password must be hashed")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("password123")); err != nil {
		t.Errorf("stored hash does not verify: %v", err)
	}
	// a valid token referencing this user and role is returned
	claims, err := svc.token.Parse(token)
	if err != nil || claims.UserID != u.ID {
		t.Errorf("token did not parse back to user id: id=%s err=%v", claims.UserID, err)
	}
	if claims.Role != string(RoleStudent) {
		t.Errorf("token role = %q, want student", claims.Role)
	}
	// it was actually persisted
	if _, err := store.GetByID(context.Background(), u.ID); err != nil {
		t.Errorf("user not persisted: %v", err)
	}
}

func TestService_Register_OptionalEmail(t *testing.T) {
	svc, _, _ := newTestService()
	u, _, err := svc.Register(context.Background(), "13800138001", "", "password123", "NoEmail", RoleTeacher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Email != "" {
		t.Errorf("email = %q, want empty", u.Email)
	}
}

func TestService_Register_DuplicatePhone(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	if _, _, err := svc.Register(ctx, "13800138000", "a@example.com", "password123", "First", RoleStudent); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	_, _, err := svc.Register(ctx, "13800138000", "b@example.com", "password123", "Second", RoleStudent)
	if !errors.Is(err, ErrPhoneTaken) {
		t.Fatalf("err = %v, want ErrPhoneTaken", err)
	}
}

func TestService_Register_DuplicateEmail(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	if _, _, err := svc.Register(ctx, "13800138000", "dup@example.com", "password123", "First", RoleStudent); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	// same email, different case and phone → still a conflict
	_, _, err := svc.Register(ctx, "13800138001", "DUP@example.com", "password123", "Second", RoleStudent)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestService_Register_PropagatesStoreError(t *testing.T) {
	svc, store, _ := newTestService()
	boom := errors.New("db down")
	store.createFn = func(*User, Role) error { return boom }

	_, _, err := svc.Register(context.Background(), "13800138000", "x@example.com", "password123", "X", RoleStudent)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestService_Register_BcryptError(t *testing.T) {
	svc, _, _ := newTestService()
	// bcrypt rejects passwords longer than 72 bytes; the service must surface
	// that as an error rather than storing a half-built user.
	long := strings.Repeat("x", 100)
	if _, _, err := svc.Register(context.Background(), "13800138000", "x@example.com", long, "X", RoleStudent); err == nil {
		t.Fatal("expected an error for an over-long password")
	}
}

func TestService_Register_InvalidRole(t *testing.T) {
	svc, _, _ := newTestService()
	if _, _, err := svc.Register(context.Background(), "13800138000", "r@example.com", "password123", "R", Role("admin")); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("err = %v, want ErrInvalidRole", err)
	}
}

func TestService_LoginPassword_Success(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "13800138000", "login@example.com", "password123", "L", RoleStudent)

	// login by email (different casing) should work
	u, token, err := svc.LoginPassword(ctx, "LOGIN@example.com", "password123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.ID != reg.ID {
		t.Errorf("logged in as %s, want %s", u.ID, reg.ID)
	}
	claims, err := svc.token.Parse(token)
	if err != nil {
		t.Errorf("invalid token returned: %v", err)
	}
	if claims.Role != string(RoleStudent) {
		t.Errorf("login token role = %q, want student", claims.Role)
	}

	// login by phone should also work
	if _, _, err := svc.LoginPassword(ctx, "13800138000", "password123"); err != nil {
		t.Errorf("phone login failed: %v", err)
	}
}

func TestService_LoginPassword_WrongPassword(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	_, _, _ = svc.Register(ctx, "13800138000", "wp@example.com", "password123", "W", RoleStudent)

	_, _, err := svc.LoginPassword(ctx, "wp@example.com", "wrongpassword")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginPassword_UnknownIdentifier(t *testing.T) {
	svc, _, _ := newTestService()

	// same generic error as a wrong password, to avoid leaking which accounts exist
	if _, _, err := svc.LoginPassword(context.Background(), "nobody@example.com", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := svc.LoginPassword(context.Background(), "19999999999", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("phone err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginPassword_PropagatesUnexpectedStoreError(t *testing.T) {
	svc, store, _ := newTestService()
	boom := errors.New("db exploded")
	store.getEmail = func(string) (*User, error) { return nil, boom }

	_, _, err := svc.LoginPassword(context.Background(), "any@example.com", "password123")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestService_LoginCode_Success(t *testing.T) {
	svc, _, codes := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "13800138000", "code@example.com", "password123", "C", RoleStudent)

	if err := svc.RequestLoginCode(ctx, "13800138000"); err != nil {
		t.Fatalf("request code: %v", err)
	}
	// the fake "sent" a deterministic code to the (normalized) target
	if codes.codes["13800138000"] != "123456" {
		t.Fatalf("code not recorded for target: %v", codes.codes)
	}

	u, token, err := svc.LoginCode(ctx, "13800138000", "123456")
	if err != nil {
		t.Fatalf("login by code: %v", err)
	}
	if u.ID != reg.ID {
		t.Errorf("logged in as %s, want %s", u.ID, reg.ID)
	}
	if _, err := svc.token.Parse(token); err != nil {
		t.Errorf("invalid token: %v", err)
	}

	// the code is single-use: a second attempt fails
	if _, _, err := svc.LoginCode(ctx, "13800138000", "123456"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("reused code err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginCode_WrongCode(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	_, _, _ = svc.Register(ctx, "13800138000", "code@example.com", "password123", "C", RoleStudent)
	_ = svc.RequestLoginCode(ctx, "13800138000")

	if _, _, err := svc.LoginCode(ctx, "13800138000", "000000"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginCode_UnknownUser(t *testing.T) {
	svc, _, _ := newTestService()
	// no such user → generic error, and we never reach code verification
	if _, _, err := svc.LoginCode(context.Background(), "19999999999", "123456"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_SwitchRole(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "13800138000", "sw@example.com", "password123", "SW", RoleStudent)

	// switching to a role the user does not hold is rejected
	if _, err := svc.SwitchRole(ctx, reg.ID, RoleTeacher); !errors.Is(err, ErrRoleNotOwned) {
		t.Fatalf("err = %v, want ErrRoleNotOwned", err)
	}

	// acquire the teacher identity, then switching to it issues a teacher token
	if _, err := svc.AddRole(ctx, reg.ID, RoleTeacher); err != nil {
		t.Fatalf("add role: %v", err)
	}
	tok, err := svc.SwitchRole(ctx, reg.ID, RoleTeacher)
	if err != nil {
		t.Fatalf("switch role: %v", err)
	}
	claims, err := svc.token.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.Role != string(RoleTeacher) {
		t.Errorf("active role = %q, want teacher", claims.Role)
	}
}

func TestService_AddRole_Duplicate(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "13800138000", "ar@example.com", "password123", "AR", RoleStudent)

	if _, err := svc.AddRole(ctx, reg.ID, RoleStudent); !errors.Is(err, ErrRoleTaken) {
		t.Fatalf("err = %v, want ErrRoleTaken", err)
	}
}

func TestService_GetByID(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "13800138000", "g@example.com", "password123", "G", RoleStudent)

	got, err := svc.GetByID(ctx, reg.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email != "g@example.com" {
		t.Errorf("email = %q", got.Email)
	}

	if _, err := svc.GetByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNormalizeIdentifier(t *testing.T) {
	cases := map[string]string{
		"  Foo@Bar.COM ": "foo@bar.com",
		" 13800138000 ":  "13800138000",
		"\tX@Y.Com\n":    "x@y.com",
	}
	for in, want := range cases {
		if got := normalizeIdentifier(in); got != want {
			t.Errorf("normalizeIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}
