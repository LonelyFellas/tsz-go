//go:build integration

// Exercises the real admins SQL against a live Postgres. Run with:
//
//	make test-integration
//
// Before this file the admin repository had no integration coverage at all; the
// shared Store contract (contract_test.go) is its first exercise against a real
// database, so it doubles as both drift protection and basic SQL coverage.
package admin

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

// TestStoreContract_Postgres runs the shared admin.Store contract against the
// real Postgres Repository, holding the fake and the database to one behaviour.
func TestStoreContract_Postgres(t *testing.T) {
	repo := newTestRepo(t)
	runStoreContract(t, func() Store { return repo })
}
