package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory Store for unit-testing the Service. It mirrors the
// real repository's contract: equality lookup by hash, idempotent revoke, and
// per-user bulk revoke.
type fakeStore struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]*Token
	saveErr error
}

func newFakeStore() *fakeStore { return &fakeStore{byID: make(map[uuid.UUID]*Token)} }

func (f *fakeStore) Save(_ context.Context, t *Token) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.CreatedAt = time.Now()
	cp := *t
	f.byID[t.ID] = &cp
	return nil
}

func (f *fakeStore) FindByHash(_ context.Context, hash string) (*Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byID {
		if t.TokenHash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) Revoke(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.byID[id]; ok && t.RevokedAt == nil {
		now := time.Now()
		t.RevokedAt = &now
	}
	return nil
}

func (f *fakeStore) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for _, t := range f.byID {
		if t.UserID == userID && t.RevokedAt == nil {
			t.RevokedAt = &now
		}
	}
	return nil
}

// activeForUser counts the user's tokens that are not yet revoked.
func (f *fakeStore) activeForUser(userID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.byID {
		if t.UserID == userID && t.RevokedAt == nil {
			n++
		}
	}
	return n
}

func newTestService() (*Service, *fakeStore) {
	store := newFakeStore()
	return NewService(store, time.Hour), store
}

func TestService_Issue_RevokesPrevious(t *testing.T) {
	svc, store := newTestService()
	ctx := context.Background()
	userID := uuid.New()

	first, err := svc.Issue(ctx, userID)
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	// a second login (issue) for the same user must revoke the first token:
	// strict single-device.
	second, err := svc.Issue(ctx, userID)
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}
	if first == second {
		t.Fatal("expected distinct refresh tokens")
	}
	if n := store.activeForUser(userID); n != 1 {
		t.Errorf("active tokens = %d, want exactly 1 after second login", n)
	}

	// the first token is now revoked → rotating it fails
	if _, _, err := svc.Rotate(ctx, first); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("rotate revoked-by-relogin err = %v, want ErrInvalidRefreshToken", err)
	}
	// the second still works
	if _, _, err := svc.Rotate(ctx, second); err != nil {
		t.Errorf("rotate current token: %v", err)
	}
}

func TestService_Issue_OtherUsersUntouched(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	aliceTok, _ := svc.Issue(ctx, alice)
	// bob logging in must not revoke alice's token
	if _, err := svc.Issue(ctx, bob); err != nil {
		t.Fatalf("bob issue: %v", err)
	}
	if _, _, err := svc.Rotate(ctx, aliceTok); err != nil {
		t.Errorf("alice's token should survive bob's login, got %v", err)
	}
}

func TestService_Rotate_Success(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	userID := uuid.New()

	raw, _ := svc.Issue(ctx, userID)
	gotUser, rotated, err := svc.Rotate(ctx, raw)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if gotUser != userID {
		t.Errorf("rotate user = %s, want %s", gotUser, userID)
	}
	if rotated == "" || rotated == raw {
		t.Error("rotate must return a fresh, distinct token")
	}
	// the old token is single-use: replaying it fails
	if _, _, err := svc.Rotate(ctx, raw); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("replayed rotate err = %v, want ErrInvalidRefreshToken", err)
	}
	// the rotated token works
	if _, _, err := svc.Rotate(ctx, rotated); err != nil {
		t.Errorf("rotated token rotate: %v", err)
	}
}

func TestService_Rotate_Unknown(t *testing.T) {
	svc, _ := newTestService()
	if _, _, err := svc.Rotate(context.Background(), "does-not-exist"); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestService_Rotate_Revoked(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	raw, _ := svc.Issue(ctx, uuid.New())
	if err := svc.Revoke(ctx, raw); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := svc.Rotate(ctx, raw); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestService_Rotate_Expired(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, -time.Minute) // tokens are already expired on issue
	ctx := context.Background()
	raw, _ := svc.Issue(ctx, uuid.New())
	if _, _, err := svc.Rotate(ctx, raw); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestService_Revoke_Idempotent(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	raw, _ := svc.Issue(ctx, uuid.New())

	if err := svc.Revoke(ctx, raw); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	// revoking again is not an error (logout is idempotent)
	if err := svc.Revoke(ctx, raw); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	// revoking a token that never existed is also fine
	if err := svc.Revoke(ctx, "never-issued"); err != nil {
		t.Errorf("revoke unknown: %v", err)
	}
}

func TestService_RevokeAll(t *testing.T) {
	svc, store := newTestService()
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	// Seed alice with two tokens (bypass Issue's single-device revoke so we can
	// prove RevokeAll clears *all* of them, not just the latest).
	a1, _ := svc.issue(ctx, alice)
	a2, _ := svc.issue(ctx, alice)
	bobTok, _ := svc.Issue(ctx, bob)

	if err := svc.RevokeAll(ctx, alice); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n := store.activeForUser(alice); n != 0 {
		t.Errorf("alice active tokens = %d, want 0", n)
	}
	// both of alice's tokens are dead
	for _, tok := range []string{a1, a2} {
		if _, _, err := svc.Rotate(ctx, tok); !errors.Is(err, ErrInvalidRefreshToken) {
			t.Errorf("rotate revoked token err = %v, want ErrInvalidRefreshToken", err)
		}
	}
	// bob is untouched
	if _, _, err := svc.Rotate(ctx, bobTok); err != nil {
		t.Errorf("bob's token should survive alice's logout-all, got %v", err)
	}
	// idempotent: revoking again (or for a user with no tokens) is fine
	if err := svc.RevokeAll(ctx, alice); err != nil {
		t.Errorf("second revoke all: %v", err)
	}
	if err := svc.RevokeAll(ctx, uuid.New()); err != nil {
		t.Errorf("revoke all for unknown user: %v", err)
	}
}

func TestService_Issue_SaveError(t *testing.T) {
	store := newFakeStore()
	store.saveErr = errors.New("db down")
	svc := NewService(store, time.Hour)
	if _, err := svc.Issue(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected error when store.Save fails")
	}
}
