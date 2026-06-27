package otp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This file is the single source of truth for the behaviour every otp.Store must
// honour. It runs against BOTH the in-memory fakeStore (here, no build tag) and
// the real Postgres Repository (repository_integration_test.go, under
// -tags=integration), so the fake can never silently drift from the database.

// ctTarget mints a target unique across runs. The integration tests share a
// persistent Postgres that is never truncated, so scoping every case to its own
// target keeps CountSince / LatestUnconsumed from seeing other tests' codes.
func ctTarget() string { return "ct-" + uuid.NewString() }

func runStoreContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	mk := func(target, purpose, code string) *Code {
		return &Code{
			ID:        uuid.New(),
			Target:    target,
			Channel:   ChannelFor(target),
			Purpose:   purpose,
			Code:      code,
			ExpiresAt: time.Now().Add(time.Hour),
		}
	}

	t.Run("save then latest-unconsumed round-trips", func(t *testing.T) {
		st := newStore()
		target := ctTarget()
		c := mk(target, "login", "111111")
		if err := st.Save(ctx, c); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := st.LatestUnconsumed(ctx, target, "login")
		if err != nil {
			t.Fatalf("LatestUnconsumed: %v", err)
		}
		if got.ID != c.ID || got.Code != "111111" {
			t.Errorf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("latest-unconsumed returns the newest code", func(t *testing.T) {
		st := newStore()
		target := ctTarget()
		older := mk(target, "login", "111111")
		if err := st.Save(ctx, older); err != nil {
			t.Fatalf("Save older: %v", err)
		}
		newer := mk(target, "login", "222222")
		if err := st.Save(ctx, newer); err != nil {
			t.Fatalf("Save newer: %v", err)
		}
		got, err := st.LatestUnconsumed(ctx, target, "login")
		if err != nil {
			t.Fatalf("LatestUnconsumed: %v", err)
		}
		if got.ID != newer.ID {
			t.Errorf("got %s (code %q), want newest %s", got.ID, got.Code, newer.ID)
		}
	})

	t.Run("no unconsumed code returns ErrNotFound", func(t *testing.T) {
		st := newStore()
		if _, err := st.LatestUnconsumed(ctx, ctTarget(), "login"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("LatestUnconsumed empty: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("consumed codes are excluded", func(t *testing.T) {
		st := newStore()
		target := ctTarget()
		c := mk(target, "login", "111111")
		if err := st.Save(ctx, c); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := st.MarkConsumed(ctx, c.ID); err != nil {
			t.Fatalf("MarkConsumed: %v", err)
		}
		if _, err := st.LatestUnconsumed(ctx, target, "login"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after consume: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("increment attempts persists", func(t *testing.T) {
		st := newStore()
		target := ctTarget()
		c := mk(target, "login", "111111")
		if err := st.Save(ctx, c); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := st.IncrementAttempts(ctx, c.ID); err != nil {
			t.Fatalf("IncrementAttempts #1: %v", err)
		}
		if err := st.IncrementAttempts(ctx, c.ID); err != nil {
			t.Fatalf("IncrementAttempts #2: %v", err)
		}
		got, err := st.LatestUnconsumed(ctx, target, "login")
		if err != nil {
			t.Fatalf("LatestUnconsumed: %v", err)
		}
		if got.Attempts != 2 {
			t.Errorf("attempts = %d, want 2", got.Attempts)
		}
	})

	t.Run("count since is inclusive and scoped to target", func(t *testing.T) {
		// purpose is fixed to "login": the verification_codes schema constrains it
		// to that single value, so target is the only scoping dimension the real DB
		// can exercise here.
		st := newStore()
		target := ctTarget()
		before := time.Now().Add(-time.Minute)
		for i := 0; i < 3; i++ {
			if err := st.Save(ctx, mk(target, "login", "111111")); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}
		// A code for a different target must not be counted.
		if err := st.Save(ctx, mk(ctTarget(), "login", "111111")); err != nil {
			t.Fatalf("Save other target: %v", err)
		}
		n, err := st.CountSince(ctx, target, "login", before)
		if err != nil {
			t.Fatalf("CountSince: %v", err)
		}
		if n != 3 {
			t.Errorf("count = %d, want 3 (scoped to target)", n)
		}
		// A window opening in the future excludes everything.
		n, err = st.CountSince(ctx, target, "login", time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("CountSince future: %v", err)
		}
		if n != 0 {
			t.Errorf("future-window count = %d, want 0", n)
		}
	})
}

// TestStoreContract_Fake runs the contract against the in-memory fake. The
// Postgres counterpart lives in repository_integration_test.go.
func TestStoreContract_Fake(t *testing.T) {
	runStoreContract(t, func() Store { return newFakeStore() })
}
