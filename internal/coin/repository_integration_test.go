//go:build integration

// Exercises the real coin ledger SQL against a live Postgres. Run with:
//
//	make test-integration
//
// The shared Store contract (contract_test.go) runs against the real Repository
// here, holding the in-memory fake and the database to one behaviour — the
// atomic, non-negative balance change and the reversal rules in particular.
package coin

import (
	"context"
	"os"
	"testing"

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

// TestStoreContract_Postgres runs the shared coin.Store contract against the real
// Postgres Repository.
func TestStoreContract_Postgres(t *testing.T) {
	repo := newTestRepo(t)
	runStoreContract(t, func() Store { return repo })
}
