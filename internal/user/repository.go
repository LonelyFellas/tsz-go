package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound   = errors.New("user not found")
	ErrEmailTaken = errors.New("email already registered")
)

// Repository is the data-access boundary for users. SQL is hand-written here;
// to adopt sqlc later, generate typed query methods and swap the bodies — the
// service layer depends only on these method signatures, not on pgx.
type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Create(ctx context.Context, u *User) error {
	const q = `
		INSERT INTO users (id, email, password_hash, display_name)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at, updated_at`

	err := r.db.QueryRow(ctx, q, u.ID, u.Email, u.PasswordHash, u.DisplayName).
		Scan(&u.CreatedAt, &u.UpdatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	const q = `
		SELECT id, email, password_hash, display_name, created_at, updated_at
		FROM users WHERE lower(email) = lower($1)`
	return r.scanOne(ctx, q, email)
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
	const q = `
		SELECT id, email, password_hash, display_name, created_at, updated_at
		FROM users WHERE id = $1`
	return r.scanOne(ctx, q, id)
}

func (r *Repository) scanOne(ctx context.Context, query string, arg any) (*User, error) {
	var u User
	err := r.db.QueryRow(ctx, query, arg).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query user: %w", err)
	}
	return &u, nil
}
