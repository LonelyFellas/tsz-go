package otp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory Store for unit-testing the Service.
type fakeStore struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]*Code
	saveErr error
}

func newFakeStore() *fakeStore { return &fakeStore{byID: make(map[uuid.UUID]*Code)} }

func (f *fakeStore) Save(_ context.Context, c *Code) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c.CreatedAt = time.Now()
	cp := *c
	f.byID[c.ID] = &cp
	return nil
}

func (f *fakeStore) LatestUnconsumed(_ context.Context, target, purpose string) (*Code, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var latest *Code
	for _, c := range f.byID {
		if c.Target == target && c.Purpose == purpose && c.ConsumedAt == nil {
			if latest == nil || c.CreatedAt.After(latest.CreatedAt) {
				latest = c
			}
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	cp := *latest
	return &cp, nil
}

func (f *fakeStore) MarkConsumed(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.byID[id]; ok {
		now := time.Now()
		c.ConsumedAt = &now
	}
	return nil
}

func TestChannelFor(t *testing.T) {
	if ChannelFor("a@b.com") != ChannelEmail {
		t.Error("email target should map to email channel")
	}
	if ChannelFor("13800138000") != ChannelSMS {
		t.Error("phone target should map to sms channel")
	}
}

func TestService_RequestAndVerify(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, time.Minute)
	ctx := context.Background()

	if err := svc.RequestCode(ctx, "13800138000", "login"); err != nil {
		t.Fatalf("request: %v", err)
	}
	code := sender.LastCode("13800138000")
	if len(code) != codeDigits {
		t.Fatalf("code = %q, want %d digits", code, codeDigits)
	}

	// wrong code is rejected
	if err := svc.Verify(ctx, "13800138000", "login", "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("wrong code err = %v, want ErrInvalidCode", err)
	}

	// correct code succeeds
	if err := svc.Verify(ctx, "13800138000", "login", code); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// and is single-use afterwards
	if err := svc.Verify(ctx, "13800138000", "login", code); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("reused code err = %v, want ErrInvalidCode", err)
	}
}

func TestService_Verify_NoCode(t *testing.T) {
	svc := NewService(newFakeStore(), NewMockSender(), time.Minute)
	if err := svc.Verify(context.Background(), "13800138000", "login", "123456"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err = %v, want ErrInvalidCode", err)
	}
}

func TestService_Verify_Expired(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, -time.Minute) // already expired on issue
	ctx := context.Background()

	if err := svc.RequestCode(ctx, "a@b.com", "login"); err != nil {
		t.Fatalf("request: %v", err)
	}
	code := sender.LastCode("a@b.com")
	if err := svc.Verify(ctx, "a@b.com", "login", code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err = %v, want ErrInvalidCode", err)
	}
}

func TestService_RequestCode_SaveError(t *testing.T) {
	store := newFakeStore()
	store.saveErr = errors.New("db down")
	svc := NewService(store, NewMockSender(), time.Minute)
	if err := svc.RequestCode(context.Background(), "13800138000", "login"); err == nil {
		t.Fatal("expected error when store.Save fails")
	}
}
