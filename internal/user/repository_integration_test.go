//go:build integration

// These tests exercise the real SQL against a live Postgres. Run with:
//
//	DATABASE_URL=postgres://app:app@localhost:5432/tsz?sslmode=disable \
//	  go test -tags=integration ./internal/user/...
//
// They are excluded from the default `go test ./...` run via the build tag.
package user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/platform/database"
)

func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	if err := database.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := database.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewRepository(pool)
}

func uniqueEmail() string { return "it-" + uuid.NewString() + "@example.com" }

var phoneSeq int64

// uniquePhone returns a distinct 11-digit phone for each call so tests never
// collide on the unique phone index.
func uniquePhone() string { return fmt.Sprintf("1%010d", atomic.AddInt64(&phoneSeq, 1)) }

func TestRepository_CreateAndGet(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	u := &User{ID: uuid.New(), Phone: uniquePhone(), Email: uniqueEmail(), PasswordHash: "hash", DisplayName: "IT"}
	if err := repo.Create(ctx, u, RoleStudent); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Create populates timestamps from the DB
	if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
		t.Error("expected timestamps to be set by the database")
	}

	byID, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.Email != u.Email {
		t.Errorf("email = %q, want %q", byID.Email, u.Email)
	}
	// the initial role is persisted and loaded back
	if len(byID.Roles) != 1 || byID.Roles[0] != RoleStudent {
		t.Errorf("roles = %v, want [student]", byID.Roles)
	}

	// acquiring a second role; HasRole and the loaded set both reflect it
	if err := repo.AddRole(ctx, u.ID, RoleTeacher); err != nil {
		t.Fatalf("add role: %v", err)
	}
	if err := repo.AddRole(ctx, u.ID, RoleTeacher); !errors.Is(err, ErrRoleTaken) {
		t.Errorf("duplicate AddRole err = %v, want ErrRoleTaken", err)
	}
	has, err := repo.HasRole(ctx, u.ID, RoleTeacher)
	if err != nil || !has {
		t.Errorf("HasRole(teacher) = %v, err = %v; want true", has, err)
	}
	reloaded, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// roles are ordered: student, teacher
	if len(reloaded.Roles) != 2 {
		t.Errorf("roles = %v, want both student and teacher", reloaded.Roles)
	}

	// case-insensitive lookup must find the row
	byEmail, err := repo.GetByEmail(ctx, strings.ToUpper(u.Email))
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if byEmail.ID != u.ID {
		t.Errorf("id = %s, want %s", byEmail.ID, u.ID)
	}

	// lookup by phone must also find the row
	byPhone, err := repo.GetByPhone(ctx, u.Phone)
	if err != nil {
		t.Fatalf("get by phone: %v", err)
	}
	if byPhone.ID != u.ID {
		t.Errorf("id = %s, want %s", byPhone.ID, u.ID)
	}
}

func TestRepository_OptionalEmail(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// a user with no email is valid; multiple such users must not collide on the
	// partial unique email index
	a := &User{ID: uuid.New(), Phone: uniquePhone(), PasswordHash: "h", DisplayName: "A"}
	b := &User{ID: uuid.New(), Phone: uniquePhone(), PasswordHash: "h", DisplayName: "B"}
	if err := repo.Create(ctx, a, RoleStudent); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := repo.Create(ctx, b, RoleStudent); err != nil {
		t.Fatalf("create b (no email should not collide): %v", err)
	}

	got, err := repo.GetByPhone(ctx, a.Phone)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Email != "" {
		t.Errorf("email = %q, want empty", got.Email)
	}
}

func TestRepository_DuplicatePhone(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	phone := uniquePhone()

	first := &User{ID: uuid.New(), Phone: phone, Email: uniqueEmail(), PasswordHash: "h", DisplayName: "A"}
	if err := repo.Create(ctx, first, RoleStudent); err != nil {
		t.Fatalf("first create: %v", err)
	}
	dup := &User{ID: uuid.New(), Phone: phone, Email: uniqueEmail(), PasswordHash: "h", DisplayName: "B"}
	if err := repo.Create(ctx, dup, RoleStudent); !errors.Is(err, ErrPhoneTaken) {
		t.Fatalf("err = %v, want ErrPhoneTaken", err)
	}
}

func TestRepository_DuplicateEmail(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	email := uniqueEmail()

	first := &User{ID: uuid.New(), Phone: uniquePhone(), Email: email, PasswordHash: "h", DisplayName: "A"}
	if err := repo.Create(ctx, first, RoleStudent); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// same email (upper-cased), different phone, must violate the case-insensitive unique index
	dup := &User{ID: uuid.New(), Phone: uniquePhone(), Email: strings.ToUpper(email), PasswordHash: "h", DisplayName: "B"}
	if err := repo.Create(ctx, dup, RoleStudent); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestRepository_NotFound(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByEmail(ctx, uniqueEmail()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByEmail err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByPhone(ctx, uniquePhone()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByPhone err = %v, want ErrNotFound", err)
	}
}

// TestRepository_QueryError covers the non-ErrNoRows / non-unique-violation
// error branches by querying through a pool that has been closed.
func TestRepository_QueryError(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	pool, err := database.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	repo := NewRepository(pool)
	pool.Close() // force all subsequent queries to error

	ctx := context.Background()

	if _, err := repo.GetByID(ctx, uuid.New()); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID err = %v, want a real query error", err)
	}
	if _, err := repo.GetByEmail(ctx, uniqueEmail()); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("GetByEmail err = %v, want a real query error", err)
	}
	if _, err := repo.GetByPhone(ctx, uniquePhone()); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("GetByPhone err = %v, want a real query error", err)
	}
	u := &User{ID: uuid.New(), Phone: uniquePhone(), Email: uniqueEmail(), PasswordHash: "h", DisplayName: "X"}
	if err := repo.Create(ctx, u, RoleStudent); err == nil || errors.Is(err, ErrEmailTaken) {
		t.Errorf("Create err = %v, want a real query error", err)
	}
}
