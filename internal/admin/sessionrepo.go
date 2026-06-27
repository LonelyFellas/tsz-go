package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darwish/tsz-go/internal/session"
)

// SessionRepository persists admin refresh tokens in admin_refresh_tokens. It
// satisfies session.Store, so the generic session.Service drives admin refresh
// exactly like web refresh — only the backing table differs (a separate table is
// required because refresh_tokens.user_id FKs to users, not admins).
//
// session.Token.UserID carries the admin id here (the session package is
// identity-agnostic; the column is admin_id).
type SessionRepository struct {
	db *pgxpool.Pool
}

func NewSessionRepository(db *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{db: db}
}

func (r *SessionRepository) Save(ctx context.Context, t *session.Token) error {
	const q = `
		INSERT INTO admin_refresh_tokens (id, admin_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`
	if err := r.db.QueryRow(ctx, q, t.ID, t.UserID, t.TokenHash, t.ExpiresAt).Scan(&t.CreatedAt); err != nil {
		return fmt.Errorf("insert admin refresh token: %w", err)
	}
	return nil
}

func (r *SessionRepository) FindByHash(ctx context.Context, hash string) (*session.Token, error) {
	const q = `
		SELECT id, admin_id, token_hash, expires_at, revoked_at, created_at
		FROM admin_refresh_tokens
		WHERE token_hash = $1`
	var t session.Token
	err := r.db.QueryRow(ctx, q, hash).Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query admin refresh token: %w", err)
	}
	return &t, nil
}

func (r *SessionRepository) Revoke(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE admin_refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("revoke admin refresh token: %w", err)
	}
	return nil
}

func (r *SessionRepository) RevokeAllForUser(ctx context.Context, adminID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE admin_refresh_tokens SET revoked_at = now() WHERE admin_id = $1 AND revoked_at IS NULL`, adminID)
	if err != nil {
		return fmt.Errorf("revoke admin refresh tokens: %w", err)
	}
	return nil
}
