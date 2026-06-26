package otp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound is returned by a Store when no matching code exists.
	ErrNotFound = errors.New("verification code not found")
	// ErrInvalidCode covers every "this code won't let you in" case (wrong,
	// expired, already used, or none issued) — deliberately undifferentiated so
	// callers cannot probe which targets have a pending code.
	ErrInvalidCode = errors.New("invalid or expired verification code")
	// ErrRateLimited is returned by RequestCode when a target has asked for codes
	// too quickly or too often, bounding SMS/email cost and abuse.
	ErrRateLimited = errors.New("too many code requests, try again later")
)

// codeDigits is the length of a generated numeric code.
const codeDigits = 6

// maxVerifyAttempts caps wrong guesses against a single code before it is locked
// out. With a 6-digit code this keeps the odds of a successful online guess
// negligible (≤ maxVerifyAttempts / 1e6) regardless of how long the code is valid.
const maxVerifyAttempts = 5

// Code is a stored one-time code.
type Code struct {
	ID         uuid.UUID
	Target     string
	Channel    Channel
	Purpose    string
	Code       string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	Attempts   int
	CreatedAt  time.Time
}

// Store is the persistence the Service needs. The concrete *Repository satisfies
// it in production; tests use an in-memory fake.
type Store interface {
	Save(ctx context.Context, c *Code) error
	LatestUnconsumed(ctx context.Context, target, purpose string) (*Code, error)
	MarkConsumed(ctx context.Context, id uuid.UUID) error
	IncrementAttempts(ctx context.Context, id uuid.UUID) error
	// CountSince counts codes issued to target+purpose at or after `since`, for
	// rate limiting.
	CountSince(ctx context.Context, target, purpose string, since time.Time) (int, error)
}

// Service generates, stores, sends and verifies one-time codes.
type Service struct {
	store  Store
	sender Sender
	ttl    time.Duration
	// cooldown is the minimum interval between two codes to the same target; 0
	// disables the cooldown. dailyLimit caps codes per target per rolling 24h; 0
	// disables the cap. Together they bound SMS/email cost and abuse.
	cooldown   time.Duration
	dailyLimit int
}

func NewService(store Store, sender Sender, ttl, cooldown time.Duration, dailyLimit int) *Service {
	return &Service{store: store, sender: sender, ttl: ttl, cooldown: cooldown, dailyLimit: dailyLimit}
}

// RequestCode generates a code for target+purpose, stores it, and sends it over
// the channel implied by the target. It enforces per-target rate limits first,
// returning ErrRateLimited without sending anything when they are exceeded.
func (s *Service) RequestCode(ctx context.Context, target, purpose string) error {
	if err := s.checkRateLimit(ctx, target, purpose); err != nil {
		return err
	}

	code, err := generateCode()
	if err != nil {
		return fmt.Errorf("generate code: %w", err)
	}

	channel := ChannelFor(target)
	c := &Code{
		ID:        uuid.New(),
		Target:    target,
		Channel:   channel,
		Purpose:   purpose,
		Code:      code,
		ExpiresAt: time.Now().Add(s.ttl),
	}
	if err := s.store.Save(ctx, c); err != nil {
		return fmt.Errorf("save code: %w", err)
	}
	if err := s.sender.Send(ctx, channel, target, code); err != nil {
		return fmt.Errorf("send code: %w", err)
	}
	return nil
}

// checkRateLimit enforces the per-target cooldown and daily cap. Each limit is
// skipped when configured to 0. Returns ErrRateLimited when a limit is hit.
func (s *Service) checkRateLimit(ctx context.Context, target, purpose string) error {
	now := time.Now()
	if s.cooldown > 0 {
		n, err := s.store.CountSince(ctx, target, purpose, now.Add(-s.cooldown))
		if err != nil {
			return fmt.Errorf("cooldown check: %w", err)
		}
		if n > 0 {
			return ErrRateLimited
		}
	}
	if s.dailyLimit > 0 {
		n, err := s.store.CountSince(ctx, target, purpose, now.Add(-24*time.Hour))
		if err != nil {
			return fmt.Errorf("daily-limit check: %w", err)
		}
		if n >= s.dailyLimit {
			return ErrRateLimited
		}
	}
	return nil
}

// Verify checks a submitted code for target+purpose and, on success, consumes it
// so it cannot be reused. Every failure mode maps to ErrInvalidCode.
func (s *Service) Verify(ctx context.Context, target, purpose, code string) error {
	c, err := s.store.LatestUnconsumed(ctx, target, purpose)
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidCode
	}
	if err != nil {
		return fmt.Errorf("lookup code: %w", err)
	}

	if time.Now().After(c.ExpiresAt) {
		return ErrInvalidCode
	}
	// Too many wrong guesses already: the code is locked until it expires (or is
	// superseded by a fresh one). This is what bounds online brute-forcing.
	if c.Attempts >= maxVerifyAttempts {
		return ErrInvalidCode
	}
	// constant-time compare to avoid leaking the code via timing
	if subtle.ConstantTimeCompare([]byte(c.Code), []byte(code)) != 1 {
		// Count the failed guess so repeated attempts eventually lock the code.
		if err := s.store.IncrementAttempts(ctx, c.ID); err != nil {
			return fmt.Errorf("record failed attempt: %w", err)
		}
		return ErrInvalidCode
	}

	if err := s.store.MarkConsumed(ctx, c.ID); err != nil {
		return fmt.Errorf("consume code: %w", err)
	}
	return nil
}

// generateCode returns a zero-padded numeric code of codeDigits length, drawn
// from a cryptographically secure source.
func generateCode() (string, error) {
	const digits = "0123456789"
	b := make([]byte, codeDigits)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	return string(b), nil
}
