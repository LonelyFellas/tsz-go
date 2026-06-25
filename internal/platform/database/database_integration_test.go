//go:build integration

package database

import (
	"context"
	"os"
	"testing"
)

func dbURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	return url
}

func TestConnect_Success(t *testing.T) {
	pool, err := Connect(context.Background(), dbURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(context.Background()); err != nil {
		t.Errorf("ping after connect: %v", err)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	url := dbURL(t)
	// First run applies (or is already current); the second run must be a
	// no-op and must NOT return an error. This guards startup-on-every-boot.
	if err := Migrate(url); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := Migrate(url); err != nil {
		t.Fatalf("second migrate (should be no-op): %v", err)
	}
}
