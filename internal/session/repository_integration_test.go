//go:build integration

// Exercises the real refresh_tokens SQL against a live Postgres. Run with:
//
//	make test-integration
package session

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darwish/tsz-go/internal/platform/database"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

// seedUser inserts a bare user row so refresh_tokens' FK is satisfied, and
// returns its id.
func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	phone := "1" + id.String()[:10]
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, phone, password_hash, display_name) VALUES ($1, $2, $3, $4)`,
		id, phone, "hash", "RT")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newToken(userID uuid.UUID, hash string, expires time.Time) *Token {
	return &Token{ID: uuid.New(), UserID: userID, TokenHash: hash, ExpiresAt: expires}
}

func TestRepository_SaveFindRevoke(t *testing.T) {
	pool := newTestPool(t)
	repo := NewRepository(pool)
	ctx := context.Background()
	userID := seedUser(t, pool)

	tok := newToken(userID, "hash-"+uuid.NewString(), time.Now().Add(time.Hour))
	if err := repo.Save(ctx, tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	if tok.CreatedAt.IsZero() {
		t.Error("expected created_at to be populated by the DB")
	}

	got, err := repo.FindByHash(ctx, tok.TokenHash)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.UserID != userID || got.RevokedAt != nil {
		t.Errorf("found = %+v, want user %s and not revoked", got, userID)
	}

	// revoke, then the row reads back as revoked
	if err := repo.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err = repo.FindByHash(ctx, tok.TokenHash)
	if err != nil {
		t.Fatalf("find after revoke: %v", err)
	}
	if got.RevokedAt == nil {
		t.Error("expected revoked_at to be set after Revoke")
	}
	revokedAt := *got.RevokedAt

	// revoke is idempotent: the original timestamp is preserved
	if err := repo.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	got, _ = repo.FindByHash(ctx, tok.TokenHash)
	if !got.RevokedAt.Equal(revokedAt) {
		t.Errorf("revoked_at changed on re-revoke: %v != %v", got.RevokedAt, revokedAt)
	}
}

func TestRepository_NotFound(t *testing.T) {
	repo := NewRepository(newTestPool(t))
	if _, err := repo.FindByHash(context.Background(), "no-such-hash-"+uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRepository_RevokeAllForUser(t *testing.T) {
	pool := newTestPool(t)
	repo := NewRepository(pool)
	ctx := context.Background()
	alice := seedUser(t, pool)
	bob := seedUser(t, pool)

	a1 := newToken(alice, "h-"+uuid.NewString(), time.Now().Add(time.Hour))
	a2 := newToken(alice, "h-"+uuid.NewString(), time.Now().Add(time.Hour))
	b1 := newToken(bob, "h-"+uuid.NewString(), time.Now().Add(time.Hour))
	for _, tok := range []*Token{a1, a2, b1} {
		if err := repo.Save(ctx, tok); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	if err := repo.RevokeAllForUser(ctx, alice); err != nil {
		t.Fatalf("revoke all: %v", err)
	}

	// both of alice's tokens are revoked
	for _, tok := range []*Token{a1, a2} {
		got, err := repo.FindByHash(ctx, tok.TokenHash)
		if err != nil {
			t.Fatalf("find alice token: %v", err)
		}
		if got.RevokedAt == nil {
			t.Errorf("alice token %s should be revoked", tok.ID)
		}
	}
	// bob's token is untouched
	got, err := repo.FindByHash(ctx, b1.TokenHash)
	if err != nil {
		t.Fatalf("find bob token: %v", err)
	}
	if got.RevokedAt != nil {
		t.Error("bob's token must not be revoked by alice's bulk revoke")
	}
}

func TestRepository_Expiry(t *testing.T) {
	pool := newTestPool(t)
	repo := NewRepository(pool)
	ctx := context.Background()
	userID := seedUser(t, pool)

	// a token saved already-expired reads back with an expires_at in the past, so
	// the service layer's expiry check can reject it.
	tok := newToken(userID, "h-"+uuid.NewString(), time.Now().Add(-time.Minute))
	if err := repo.Save(ctx, tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := repo.FindByHash(ctx, tok.TokenHash)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !time.Now().After(got.ExpiresAt) {
		t.Errorf("expires_at = %v, want in the past", got.ExpiresAt)
	}
}
