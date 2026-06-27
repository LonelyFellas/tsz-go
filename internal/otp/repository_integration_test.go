//go:build integration

// Exercises the real verification_codes SQL against a live Postgres. Run with:
//
//	make test-integration
package otp

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

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

func uniqueTarget() string { return "it-" + uuid.NewString() }

// TestStoreContract_Postgres runs the shared Store contract (contract_test.go)
// against the real Postgres Repository, holding the fake and the database to the
// same behaviour.
func TestStoreContract_Postgres(t *testing.T) {
	repo := newTestRepo(t)
	runStoreContract(t, func() Store { return repo })
}

func TestRepository_SaveLatestConsume(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	target := uniqueTarget()

	// an older code, then a newer one — LatestUnconsumed must return the newest
	older := &Code{ID: uuid.New(), Target: target, Channel: ChannelSMS, Purpose: "login", Code: "111111", ExpiresAt: time.Now().Add(time.Minute)}
	if err := repo.Save(ctx, older); err != nil {
		t.Fatalf("save older: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // ensure distinct created_at ordering
	newer := &Code{ID: uuid.New(), Target: target, Channel: ChannelSMS, Purpose: "login", Code: "222222", ExpiresAt: time.Now().Add(time.Minute)}
	if err := repo.Save(ctx, newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	if newer.CreatedAt.IsZero() {
		t.Error("expected created_at to be populated by the DB")
	}

	got, err := repo.LatestUnconsumed(ctx, target, "login")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got.Code != "222222" {
		t.Errorf("code = %q, want the newest (222222)", got.Code)
	}

	// after consuming the newest, the next lookup falls back to the older one
	if err := repo.MarkConsumed(ctx, got.ID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	got2, err := repo.LatestUnconsumed(ctx, target, "login")
	if err != nil {
		t.Fatalf("latest after consume: %v", err)
	}
	if got2.Code != "111111" {
		t.Errorf("code = %q, want the older (111111) after consuming newest", got2.Code)
	}
}

func TestRepository_NotFound(t *testing.T) {
	repo := newTestRepo(t)
	if _, err := repo.LatestUnconsumed(context.Background(), uniqueTarget(), "login"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
