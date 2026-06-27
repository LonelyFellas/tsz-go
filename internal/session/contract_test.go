package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This file is the single source of truth for the behaviour every session.Store
// must honour. It runs against BOTH the in-memory fakeStore (here, no build tag)
// and the real Postgres Repository (repository_integration_test.go, under
// -tags=integration), so the fake can never silently drift from the database.
//
// refresh_tokens.user_id is a foreign key into users, so the contract takes a
// newUserID hook: the fake accepts any uuid, the integration caller seeds a real
// user row first.

func runStoreContract(t *testing.T, newStore func() Store, newUserID func() uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	mk := func(uid uuid.UUID) *Token {
		return &Token{ID: uuid.New(), UserID: uid, TokenHash: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour)}
	}

	t.Run("save then find by hash round-trips", func(t *testing.T) {
		st := newStore()
		tok := mk(newUserID())
		if err := st.Save(ctx, tok); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := st.FindByHash(ctx, tok.TokenHash)
		if err != nil {
			t.Fatalf("FindByHash: %v", err)
		}
		if got.ID != tok.ID || got.UserID != tok.UserID {
			t.Errorf("round-trip mismatch: got %+v", got)
		}
		if got.RevokedAt != nil {
			t.Errorf("a freshly saved token must not be revoked, got RevokedAt=%v", got.RevokedAt)
		}
	})

	t.Run("unknown hash returns ErrNotFound", func(t *testing.T) {
		st := newStore()
		if _, err := st.FindByHash(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("FindByHash unknown: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("revoke marks the token but find still returns it", func(t *testing.T) {
		// The Store keeps returning revoked tokens; deciding they are invalid is
		// the Service's job. Pinning this stops a fake from "helpfully" hiding a
		// revoked row and masking a Service-level validity bug.
		st := newStore()
		tok := mk(newUserID())
		if err := st.Save(ctx, tok); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := st.Revoke(ctx, tok.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		got, err := st.FindByHash(ctx, tok.TokenHash)
		if err != nil {
			t.Fatalf("FindByHash after revoke: %v", err)
		}
		if got.RevokedAt == nil {
			t.Errorf("revoked token should have RevokedAt set")
		}
	})

	t.Run("revoke is idempotent", func(t *testing.T) {
		st := newStore()
		tok := mk(newUserID())
		if err := st.Save(ctx, tok); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := st.Revoke(ctx, tok.ID); err != nil {
			t.Fatalf("Revoke #1: %v", err)
		}
		first, err := st.FindByHash(ctx, tok.TokenHash)
		if err != nil {
			t.Fatalf("FindByHash #1: %v", err)
		}
		if err := st.Revoke(ctx, tok.ID); err != nil {
			t.Fatalf("Revoke #2: %v", err)
		}
		second, err := st.FindByHash(ctx, tok.TokenHash)
		if err != nil {
			t.Fatalf("FindByHash #2: %v", err)
		}
		if first.RevokedAt == nil || second.RevokedAt == nil || !first.RevokedAt.Equal(*second.RevokedAt) {
			t.Errorf("re-revoke changed the timestamp: %v -> %v", first.RevokedAt, second.RevokedAt)
		}
	})

	t.Run("revoke all for user touches only that user", func(t *testing.T) {
		st := newStore()
		uidA, uidB := newUserID(), newUserID()
		a1, a2, b := mk(uidA), mk(uidA), mk(uidB)
		for _, tok := range []*Token{a1, a2, b} {
			if err := st.Save(ctx, tok); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}
		if err := st.RevokeAllForUser(ctx, uidA); err != nil {
			t.Fatalf("RevokeAllForUser: %v", err)
		}
		for _, tok := range []*Token{a1, a2} {
			got, err := st.FindByHash(ctx, tok.TokenHash)
			if err != nil {
				t.Fatalf("FindByHash: %v", err)
			}
			if got.RevokedAt == nil {
				t.Errorf("token for the revoked user should be revoked")
			}
		}
		got, err := st.FindByHash(ctx, b.TokenHash)
		if err != nil {
			t.Fatalf("FindByHash b: %v", err)
		}
		if got.RevokedAt != nil {
			t.Errorf("another user's token must stay active")
		}
	})
}

// TestStoreContract_Fake runs the contract against the in-memory fake. The
// Postgres counterpart lives in repository_integration_test.go.
func TestStoreContract_Fake(t *testing.T) {
	runStoreContract(t,
		func() Store { return newFakeStore() },
		uuid.New,
	)
}
