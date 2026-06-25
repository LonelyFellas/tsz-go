package database

import (
	"context"
	"testing"
)

func TestConnect_InvalidURL(t *testing.T) {
	if _, err := Connect(context.Background(), "://not a valid url"); err == nil {
		t.Fatal("expected error for an unparseable database URL")
	}
}

func TestConnect_Unreachable(t *testing.T) {
	// Valid DSN, but nothing is listening on port 1 → Ping must fail and the
	// pool must be cleaned up (no leak, no hang).
	url := "postgres://app:app@127.0.0.1:1/tsz?sslmode=disable&connect_timeout=1"
	if _, err := Connect(context.Background(), url); err == nil {
		t.Fatal("expected ping to fail against an unreachable server")
	}
}
