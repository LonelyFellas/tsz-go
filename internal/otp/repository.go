package otp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository persists verification codes in Postgres.
type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Save(ctx context.Context, c *Code) error {
	const q = `
		INSERT INTO verification_codes (id, target, channel, purpose, code, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`
	err := r.db.QueryRow(ctx, q, c.ID, c.Target, c.Channel, c.Purpose, c.Code, c.ExpiresAt).
		Scan(&c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert code: %w", err)
	}
	return nil
}

func (r *Repository) LatestUnconsumed(ctx context.Context, target, purpose string) (*Code, error) {
	const q = `
		SELECT id, target, channel, purpose, code, expires_at, consumed_at, created_at
		FROM verification_codes
		WHERE target = $1 AND purpose = $2 AND consumed_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1`

	var c Code
	err := r.db.QueryRow(ctx, q, target, purpose).Scan(
		&c.ID, &c.Target, &c.Channel, &c.Purpose, &c.Code, &c.ExpiresAt, &c.ConsumedAt, &c.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query code: %w", err)
	}
	return &c, nil
}

func (r *Repository) MarkConsumed(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE verification_codes SET consumed_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark consumed: %w", err)
	}
	return nil
}

// CountSince counts codes issued to target+purpose at or after `since`. Backed by
// the (target, purpose, created_at) index that LatestUnconsumed already uses.
func (r *Repository) CountSince(ctx context.Context, target, purpose string, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM verification_codes
		 WHERE target = $1 AND purpose = $2 AND created_at >= $3`,
		target, purpose, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count codes: %w", err)
	}
	return n, nil
}
