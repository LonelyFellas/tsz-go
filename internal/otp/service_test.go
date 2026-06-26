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

func (f *fakeStore) IncrementAttempts(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.byID[id]; ok {
		c.Attempts++
	}
	return nil
}

func (f *fakeStore) CountSince(_ context.Context, target, purpose string, since time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.byID {
		if c.Target == target && c.Purpose == purpose && !c.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func TestChannelFor(t *testing.T) {
	if ChannelFor("a@b.com") != ChannelEmail {
		t.Error("email target should map to email channel")
	}
	if ChannelFor("13800138000") != ChannelSMS {
		t.Error("phone target should map to sms channel")
	}
}

func TestService_RequestCode_Cooldown(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, time.Minute, time.Minute, 0) // 1m cooldown, no daily cap
	ctx := context.Background()

	if err := svc.RequestCode(ctx, "13800138000", "login"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	// a second request within the cooldown is rejected and sends nothing new
	if err := svc.RequestCode(ctx, "13800138000", "login"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second request err = %v, want ErrRateLimited", err)
	}
	// a different target is unaffected
	if err := svc.RequestCode(ctx, "13900139000", "login"); err != nil {
		t.Errorf("other target should not be rate limited: %v", err)
	}
}

func TestService_RequestCode_DailyLimit(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, time.Minute, 0, 2) // no cooldown, cap 2/day
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := svc.RequestCode(ctx, "a@b.com", "login"); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	// the third request in the window exceeds the daily cap
	if err := svc.RequestCode(ctx, "a@b.com", "login"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("over-cap request err = %v, want ErrRateLimited", err)
	}
}

func TestService_RequestAndVerify(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, time.Minute, 0, 0)
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

// A code must lock after maxVerifyAttempts wrong guesses — even the correct code
// is then rejected — so a 6-digit code can't be brute-forced within its TTL.
func TestService_Verify_AttemptLimit(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, time.Minute, 0, 0)
	ctx := context.Background()

	if err := svc.RequestCode(ctx, "13800138000", "login"); err != nil {
		t.Fatalf("request: %v", err)
	}
	code := sender.LastCode("13800138000")

	// exhaust the allowed wrong guesses
	for i := 0; i < maxVerifyAttempts; i++ {
		if err := svc.Verify(ctx, "13800138000", "login", "000000"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("wrong guess %d err = %v, want ErrInvalidCode", i+1, err)
		}
	}

	// the code is now locked: the correct code no longer works
	if err := svc.Verify(ctx, "13800138000", "login", code); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("correct code after lockout err = %v, want ErrInvalidCode", err)
	}

	// a freshly requested code is accepted again (lockout is per-code)
	if err := svc.RequestCode(ctx, "13800138000", "login"); err != nil {
		t.Fatalf("re-request: %v", err)
	}
	if err := svc.Verify(ctx, "13800138000", "login", sender.LastCode("13800138000")); err != nil {
		t.Errorf("fresh code verify: %v", err)
	}
}

func TestService_Verify_NoCode(t *testing.T) {
	svc := NewService(newFakeStore(), NewMockSender(), time.Minute, 0, 0)
	if err := svc.Verify(context.Background(), "13800138000", "login", "123456"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err = %v, want ErrInvalidCode", err)
	}
}

func TestService_Verify_Expired(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, -time.Minute, 0, 0) // already expired on issue
	ctx := context.Background()

	if err := svc.RequestCode(ctx, "a@b.com", "login"); err != nil {
		t.Fatalf("request: %v", err)
	}
	code := sender.LastCode("a@b.com")
	if err := svc.Verify(ctx, "a@b.com", "login", code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err = %v, want ErrInvalidCode", err)
	}
}

func TestService_SecondCodeInvalidatesFirst(t *testing.T) {
	store := newFakeStore()
	sender := NewMockSender()
	svc := NewService(store, sender, time.Minute, 0, 0)
	ctx := context.Background()

	// request a first code
	if err := svc.RequestCode(ctx, "13800138000", "login"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	firstCode := sender.LastCode("13800138000")

	// request a second code — must supersede the first
	if err := svc.RequestCode(ctx, "13800138000", "login"); err != nil {
		t.Fatalf("second request: %v", err)
	}
	secondCode := sender.LastCode("13800138000")

	if firstCode == secondCode {
		t.Skip("codes happened to be identical; retry to avoid false-negative")
	}

	// the first code is now rejected
	if err := svc.Verify(ctx, "13800138000", "login", firstCode); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("first code after re-request: err = %v, want ErrInvalidCode", err)
	}

	// only the second code works
	if err := svc.Verify(ctx, "13800138000", "login", secondCode); err != nil {
		t.Errorf("second code verify: %v", err)
	}
}

func TestService_RequestCode_SaveError(t *testing.T) {
	store := newFakeStore()
	store.saveErr = errors.New("db down")
	svc := NewService(store, NewMockSender(), time.Minute, 0, 0)
	if err := svc.RequestCode(context.Background(), "13800138000", "login"); err == nil {
		t.Fatal("expected error when store.Save fails")
	}
}
