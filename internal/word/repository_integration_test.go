//go:build integration

// Exercises the real words SQL against a live Postgres. Run with:
//
//	make test-integration
//
// The shared Store contract (contract_test.go) is the whole coverage — it
// holds the fake and the database to one behaviour, including audio
// preservation (seeded here via raw SQL through the contractEnv.setAudio seam,
// because no Store method writes audio until the TTS/upload endpoints land).
package word

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"testing"

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

// randPhone mints identifiers unique across runs (the shared test DB is never
// truncated), same as the other packages.
func randPhone() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10_000_000_000))
	if err != nil {
		panic(err) // crypto/rand failure is not a condition tests can proceed past
	}
	return fmt.Sprintf("1%010d", n.Int64())
}

func pgEnv(t *testing.T, pool *pgxpool.Pool, repo *Repository) contractEnv {
	t.Helper()
	return contractEnv{
		store: repo,
		newAdmin: func(name string) uuid.UUID {
			id := uuid.New()
			if _, err := pool.Exec(context.Background(),
				`INSERT INTO admins (id, phone, password_hash, display_name) VALUES ($1, $2, 'x', $3)`,
				id, randPhone(), name); err != nil {
				t.Fatalf("insert admin: %v", err)
			}
			return id
		},
		setAudio: func(table string, nodeID uuid.UUID, url, source string) {
			// Table names come from the contract's fixed carrier list, never
			// user input; this is what the future TTS endpoint will execute.
			if _, err := pool.Exec(context.Background(),
				fmt.Sprintf(`UPDATE %s SET audio_url = $2, audio_source = $3 WHERE id = $1`, table),
				nodeID, url, source); err != nil {
				t.Fatalf("set audio on %s: %v", table, err)
			}
		},
	}
}

// TestStoreContract_Postgres runs the shared word.Store contract against the
// real Postgres Repository, holding the fake and the database to one behaviour.
func TestStoreContract_Postgres(t *testing.T) {
	pool := newTestPool(t)
	repo := NewRepository(pool)
	runStoreContract(t, func(t *testing.T) contractEnv { return pgEnv(t, pool, repo) })
}

// Audio preservation is covered by the shared contract case ("audio survives
// tree saves on every carrier"), which runs against this repository through
// pgEnv's setAudio seam — no repo-only audio test is needed anymore.
