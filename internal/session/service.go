// Package session manages refresh tokens for the access/refresh token scheme.
//
// Access tokens are stateless JWTs verified locally by the auth middleware;
// they are short-lived. Refresh tokens are opaque, high-entropy random strings
// that are long-lived and tracked here so they can be rotated and revoked. Only
// their SHA-256 hash is stored — the raw token is high-entropy, so an equality
// lookup on the hash is enough (no bcrypt, no constant-time scan of the table).
//
// Login is strict single-device: issuing a token first revokes the user's other
// refresh tokens, so a stale device is kicked off once its access token expires
// and its next refresh fails (delay ≤ access TTL).
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound is returned by a Store when no matching token row exists.
	ErrNotFound = errors.New("refresh token not found")
	// ErrInvalidRefreshToken covers every "this refresh token won't work" case
	// (unknown, revoked, or expired) — deliberately undifferentiated so callers
	// can't probe which tokens once existed.
	ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")
)

// rawTokenBytes is the number of random bytes behind a refresh token before
// base64url encoding. 32 bytes = 256 bits of entropy.
const rawTokenBytes = 32

// Token is a stored refresh token. Only token_hash is persisted from the raw
// secret; the raw value is returned to the client once at issue time.
type Token struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// Store is the persistence the Service needs. The concrete *Repository satisfies
// it in production; tests use an in-memory fake.
type Store interface {
	Save(ctx context.Context, t *Token) error
	FindByHash(ctx context.Context, hash string) (*Token, error)
	Revoke(ctx context.Context, id uuid.UUID) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
}

// Service issues, rotates and revokes refresh tokens.
type Service struct {
	store Store
	ttl   time.Duration
}

func NewService(store Store, ttl time.Duration) *Service {
	return &Service{store: store, ttl: ttl}
}

// Issue mints a refresh token for a login. To enforce strict single-device
// login, it first revokes every other refresh token the user holds, then stores
// the new one and returns the raw secret to the caller.
func (s *Service) Issue(ctx context.Context, userID uuid.UUID) (string, error) {
	if err := s.store.RevokeAllForUser(ctx, userID); err != nil {
		return "", fmt.Errorf("revoke existing tokens: %w", err)
	}
	return s.issue(ctx, userID)
}

// Rotate validates a presented refresh token and, on success, revokes it and
// issues a fresh one for the same user (refresh-token rotation), returning the
// user ID so the caller can re-sign an access token. Any invalid input (unknown,
// revoked, or expired) maps to ErrInvalidRefreshToken.
func (s *Service) Rotate(ctx context.Context, rawRefreshToken string) (uuid.UUID, string, error) {
	t, err := s.store.FindByHash(ctx, hashToken(rawRefreshToken))
	if errors.Is(err, ErrNotFound) {
		return uuid.Nil, "", ErrInvalidRefreshToken
	}
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("lookup refresh token: %w", err)
	}
	if t.RevokedAt != nil || time.Now().After(t.ExpiresAt) {
		return uuid.Nil, "", ErrInvalidRefreshToken
	}

	if err := s.store.Revoke(ctx, t.ID); err != nil {
		return uuid.Nil, "", fmt.Errorf("revoke rotated token: %w", err)
	}
	newRaw, err := s.issue(ctx, t.UserID)
	if err != nil {
		return uuid.Nil, "", err
	}
	return t.UserID, newRaw, nil
}

// Revoke invalidates a refresh token (logout). It is idempotent: an unknown or
// already-revoked token is not an error, so logout always succeeds.
func (s *Service) Revoke(ctx context.Context, rawRefreshToken string) error {
	t, err := s.store.FindByHash(ctx, hashToken(rawRefreshToken))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup refresh token: %w", err)
	}
	if err := s.store.Revoke(ctx, t.ID); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// issue generates a fresh random token, stores its hash, and returns the raw
// secret. It does not revoke anything — callers decide the single-device policy.
func (s *Service) issue(ctx context.Context, userID uuid.UUID) (string, error) {
	raw, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	t := &Token{
		ID:        uuid.New(),
		UserID:    userID,
		TokenHash: hashToken(raw),
		ExpiresAt: time.Now().Add(s.ttl),
	}
	if err := s.store.Save(ctx, t); err != nil {
		return "", fmt.Errorf("save refresh token: %w", err)
	}
	return raw, nil
}

// generateToken returns a URL-safe, high-entropy random refresh token drawn from
// a cryptographically secure source.
func generateToken() (string, error) {
	b := make([]byte, rawTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken maps a raw refresh token to the value stored/looked up in the DB.
// SHA-256 is sufficient here because the input is already high-entropy random.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
