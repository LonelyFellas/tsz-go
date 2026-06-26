package user

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound   = errors.New("user not found")
	ErrEmailTaken = errors.New("email already registered")
	ErrPhoneTaken = errors.New("phone already registered")
	ErrRoleTaken  = errors.New("user already has this role")
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

// Create inserts a new user together with their first role and the matching
// role profile, all in one transaction so a user is never left half-built.
func (r *Repository) Create(ctx context.Context, u *User, role Role) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful commit

	const insertUser = `
		INSERT INTO users (id, phone, email, password_hash, display_name, last_active_role)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`

	err = tx.QueryRow(ctx, insertUser, u.ID, u.Phone, nullable(u.Email), u.PasswordHash, u.DisplayName, role).
		Scan(&u.CreatedAt, &u.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		if strings.Contains(pgErr.ConstraintName, "phone") {
			return ErrPhoneTaken
		}
		return ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}

	if err := addRoleTx(ctx, tx, u.ID, role); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	u.Roles = []Role{role}
	u.LastActiveRole = role
	return nil
}

// SetActiveRole records the role a user is now acting as, so it survives a token
// refresh. Called whenever a new active role is chosen (switch-role / add-role).
func (r *Repository) SetActiveRole(ctx context.Context, userID uuid.UUID, role Role) error {
	if _, err := r.db.Exec(ctx,
		`UPDATE users SET last_active_role = $2, updated_at = now() WHERE id = $1`,
		userID, role); err != nil {
		return fmt.Errorf("set active role: %w", err)
	}
	return nil
}

// AddRole grants an additional role to an existing user, creating the matching
// role profile. Used when a user acquires a second identity (e.g. a student who
// also wants to teach). Returns ErrRoleTaken if they already hold it.
func (r *Repository) AddRole(ctx context.Context, userID uuid.UUID, role Role) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := addRoleTx(ctx, tx, userID, role); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// addRoleTx inserts the role membership and its role-specific profile row within
// an existing transaction.
func addRoleTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, role Role) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, userID, role)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrRoleTaken
	}
	if err != nil {
		return fmt.Errorf("insert role: %w", err)
	}

	var profileSQL string
	switch role {
	case RoleStudent:
		profileSQL = `INSERT INTO student_profiles (user_id) VALUES ($1)`
	case RoleTeacher:
		profileSQL = `INSERT INTO teacher_profiles (user_id) VALUES ($1)`
	default:
		return fmt.Errorf("unknown role %q", role)
	}
	if _, err := tx.Exec(ctx, profileSQL, userID); err != nil {
		return fmt.Errorf("insert role profile: %w", err)
	}
	return nil
}

// HasRole reports whether the user currently holds the given role.
func (r *Repository) HasRole(ctx context.Context, userID uuid.UUID, role Role) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = $2)`,
		userID, role).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check role: %w", err)
	}
	return exists, nil
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	const q = `
		SELECT id, phone, email, password_hash, display_name, last_active_role, created_at, updated_at
		FROM users WHERE lower(email) = lower($1)`
	return r.getOne(ctx, q, email)
}

func (r *Repository) GetByPhone(ctx context.Context, phone string) (*User, error) {
	const q = `
		SELECT id, phone, email, password_hash, display_name, last_active_role, created_at, updated_at
		FROM users WHERE phone = $1`
	return r.getOne(ctx, q, phone)
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
	const q = `
		SELECT id, phone, email, password_hash, display_name, last_active_role, created_at, updated_at
		FROM users WHERE id = $1`
	return r.getOne(ctx, q, id)
}

// getOne scans a single user row and then loads its roles.
func (r *Repository) getOne(ctx context.Context, query string, arg any) (*User, error) {
	var u User
	var email *string          // email is nullable
	var lastActiveRole *string // NULL until the first token is issued
	err := r.db.QueryRow(ctx, query, arg).Scan(
		&u.ID, &u.Phone, &email, &u.PasswordHash, &u.DisplayName, &lastActiveRole, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query user: %w", err)
	}
	if email != nil {
		u.Email = *email
	}
	if lastActiveRole != nil {
		u.LastActiveRole = Role(*lastActiveRole)
	}

	roles, err := r.loadRoles(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	u.Roles = roles
	return &u, nil
}

func (r *Repository) loadRoles(ctx context.Context, userID uuid.UUID) ([]Role, error) {
	rows, err := r.db.Query(ctx,
		`SELECT role FROM user_roles WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		return nil, fmt.Errorf("query roles: %w", err)
	}
	defer rows.Close()

	var roles []Role
	for rows.Next() {
		var role Role
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate roles: %w", err)
	}
	return roles, nil
}

// nullable maps an empty string to a SQL NULL so optional columns (email) are
// stored as NULL rather than "", keeping the partial unique index meaningful.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
