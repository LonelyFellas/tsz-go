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
	"os"
	"strings"
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

func TestRepository_CreateAndGet(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	u := &User{ID: uuid.New(), Email: uniqueEmail(), PasswordHash: "hash", DisplayName: "IT"}
	if err := repo.Create(ctx, u); err != nil {
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

	// case-insensitive lookup must find the row
	byEmail, err := repo.GetByEmail(ctx, strings.ToUpper(u.Email))
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if byEmail.ID != u.ID {
		t.Errorf("id = %s, want %s", byEmail.ID, u.ID)
	}
}

func TestRepository_DuplicateEmail(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	email := uniqueEmail()

	first := &User{ID: uuid.New(), Email: email, PasswordHash: "h", DisplayName: "A"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// same email (upper-cased) must violate the case-insensitive unique index
	dup := &User{ID: uuid.New(), Email: strings.ToUpper(email), PasswordHash: "h", DisplayName: "B"}
	if err := repo.Create(ctx, dup); !errors.Is(err, ErrEmailTaken) {
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
	u := &User{ID: uuid.New(), Email: uniqueEmail(), PasswordHash: "h", DisplayName: "X"}
	if err := repo.Create(ctx, u); err == nil || errors.Is(err, ErrEmailTaken) {
		t.Errorf("Create err = %v, want a real query error", err)
	}
}

