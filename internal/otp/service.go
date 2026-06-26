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
)

// codeDigits is the length of a generated numeric code.
const codeDigits = 6

// Code is a stored one-time code.
type Code struct {
	ID         uuid.UUID
	Target     string
	Channel    Channel
	Purpose    string
	Code       string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

// Store is the persistence the Service needs. The concrete *Repository satisfies
// it in production; tests use an in-memory fake.
type Store interface {
	Save(ctx context.Context, c *Code) error
	LatestUnconsumed(ctx context.Context, target, purpose string) (*Code, error)
	MarkConsumed(ctx context.Context, id uuid.UUID) error
}

// Service generates, stores, sends and verifies one-time codes.
type Service struct {
	store  Store
	sender Sender
	ttl    time.Duration
}

func NewService(store Store, sender Sender, ttl time.Duration) *Service {
	return &Service{store: store, sender: sender, ttl: ttl}
}

// RequestCode generates a code for target+purpose, stores it, and sends it over
// the channel implied by the target.
func (s *Service) RequestCode(ctx context.Context, target, purpose string) error {
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
	// constant-time compare to avoid leaking the code via timing
	if subtle.ConstantTimeCompare([]byte(c.Code), []byte(code)) != 1 {
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
