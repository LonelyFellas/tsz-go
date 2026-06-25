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

func newTestService() (*Service, *fakeStore) {
	store := newFakeStore()
	tm := auth.NewTokenManager("test-secret", time.Hour)
	return NewService(store, tm), store
}

func TestService_Register_Success(t *testing.T) {
	svc, store := newTestService()

	u, token, err := svc.Register(context.Background(), "Alice@Example.com", "password123", "  Alice  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// email is normalized to lowercase
	if u.Email != "alice@example.com" {
		t.Errorf("email = %q, want normalized lowercase", u.Email)
	}
	// display name is trimmed
	if u.DisplayName != "Alice" {
		t.Errorf("display name = %q, want trimmed", u.DisplayName)
	}
	// a real UUID is assigned
	if u.ID == uuid.Nil {
		t.Error("expected a non-nil user ID")
	}
	// password is hashed, never stored plaintext, and verifies
	if u.PasswordHash == "" || u.PasswordHash == "password123" {
		t.Error("password must be hashed")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("password123")); err != nil {
		t.Errorf("stored hash does not verify: %v", err)
	}
	// a valid token referencing this user is returned
	gotID, err := svc.token.Parse(token)
	if err != nil || gotID != u.ID {
		t.Errorf("token did not parse back to user id: id=%s err=%v", gotID, err)
	}
	// it was actually persisted
	if _, err := store.GetByID(context.Background(), u.ID); err != nil {
		t.Errorf("user not persisted: %v", err)
	}
}

func TestService_Register_DuplicateEmail(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()

	if _, _, err := svc.Register(ctx, "dup@example.com", "password123", "First"); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	// same email, different case → still a conflict
	_, _, err := svc.Register(ctx, "DUP@example.com", "password123", "Second")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestService_Register_PropagatesStoreError(t *testing.T) {
	svc, store := newTestService()
	boom := errors.New("db down")
	store.createFn = func(*User) error { return boom }

	_, _, err := svc.Register(context.Background(), "x@example.com", "password123", "X")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestService_Register_BcryptError(t *testing.T) {
	svc, _ := newTestService()
	// bcrypt rejects passwords longer than 72 bytes; the service must surface
	// that as an error rather than storing a half-built user.
	long := strings.Repeat("x", 100)
	if _, _, err := svc.Register(context.Background(), "x@example.com", long, "X"); err == nil {
		t.Fatal("expected an error for an over-long password")
	}
}

func TestService_Login_Success(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "login@example.com", "password123", "L")

	// login with different casing in the email should still work
	u, token, err := svc.Login(ctx, "LOGIN@example.com", "password123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.ID != reg.ID {
		t.Errorf("logged in as %s, want %s", u.ID, reg.ID)
	}
	if _, err := svc.token.Parse(token); err != nil {
		t.Errorf("invalid token returned: %v", err)
	}
}

func TestService_Login_WrongPassword(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	_, _, _ = svc.Register(ctx, "wp@example.com", "password123", "W")

	_, _, err := svc.Login(ctx, "wp@example.com", "wrongpassword")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_Login_UnknownEmail(t *testing.T) {
	svc, _ := newTestService()

	// must return the same generic error as a wrong password, to avoid
	// leaking which emails are registered
	_, _, err := svc.Login(context.Background(), "nobody@example.com", "password123")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_Login_PropagatesUnexpectedStoreError(t *testing.T) {
	svc, store := newTestService()
	boom := errors.New("db exploded")
	store.getEmail = func(string) (*User, error) { return nil, boom }

	_, _, err := svc.Login(context.Background(), "any@example.com", "password123")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestService_GetByID(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	reg, _, _ := svc.Register(ctx, "g@example.com", "password123", "G")

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

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  Foo@Bar.COM ": "foo@bar.com",
		"a@b.com":        "a@b.com",
		"\tX@Y.Com\n":    "x@y.com",
	}
	for in, want := range cases {
		if got := normalizeEmail(in); got != want {
			t.Errorf("normalizeEmail(%q) = %q, want %q", strings.TrimSpace(in), got, want)
		}
	}
}
