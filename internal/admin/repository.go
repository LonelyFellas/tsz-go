package admin

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
	ErrNotFound   = errors.New("admin not found")
	ErrEmailTaken = errors.New("email already registered")
	ErrPhoneTaken = errors.New("phone already registered")
)

// Repository is the data-access boundary for admin accounts. SQL is hand-written
// here, mirroring internal/user's repository style.
type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// Create inserts a new admin account. Returns ErrPhoneTaken/ErrEmailTaken on a
// unique conflict. The DB defaults status to 'active'; level must be set by the
// caller. On success the generated/defaulted columns are scanned back into a.
func (r *Repository) Create(ctx context.Context, a *Admin) error {
	const q = `
		INSERT INTO admins (id, phone, email, password_hash, display_name, level)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING status, created_at, updated_at`

	err := r.db.QueryRow(ctx, q, a.ID, a.Phone, nullable(a.Email), a.PasswordHash, a.DisplayName, a.Level).
		Scan(&a.Status, &a.CreatedAt, &a.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		if strings.Contains(pgErr.ConstraintName, "phone") {
			return ErrPhoneTaken
		}
		return ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("insert admin: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*Admin, error) {
	const q = `
		SELECT id, phone, email, password_hash, display_name, level, status, created_at, updated_at
		FROM admins WHERE id = $1`
	return r.getOne(ctx, q, id)
}

func (r *Repository) GetByPhone(ctx context.Context, phone string) (*Admin, error) {
	const q = `
		SELECT id, phone, email, password_hash, display_name, level, status, created_at, updated_at
		FROM admins WHERE phone = $1`
	return r.getOne(ctx, q, phone)
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*Admin, error) {
	const q = `
		SELECT id, phone, email, password_hash, display_name, level, status, created_at, updated_at
		FROM admins WHERE lower(email) = lower($1)`
	return r.getOne(ctx, q, email)
}

// ListFilter narrows and paginates a List query. Level/Query are optional.
type ListFilter struct {
	Level  Level
	Query  string
	Limit  int
	Offset int
}

// List returns a page of admins (newest first) and the total matching count.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]Admin, int64, error) {
	var conds []string
	var args []any
	if f.Level != "" {
		args = append(args, f.Level)
		conds = append(conds, fmt.Sprintf("level = $%d", len(args)))
	}
	if f.Query != "" {
		args = append(args, "%"+f.Query+"%")
		n := len(args)
		conds = append(conds, fmt.Sprintf("(phone ILIKE $%d OR email ILIKE $%d OR display_name ILIKE $%d)", n, n, n))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int64
	if err := r.db.QueryRow(ctx, "SELECT count(*) FROM admins "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admins: %w", err)
	}

	args = append(args, f.Limit, f.Offset)
	q := fmt.Sprintf(`
		SELECT id, phone, email, password_hash, display_name, level, status, created_at, updated_at
		FROM admins %s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query admins: %w", err)
	}
	defer rows.Close()

	var admins []Admin
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, 0, err
		}
		admins = append(admins, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate admins: %w", err)
	}
	return admins, total, nil
}

// SetStatus updates an admin's lifecycle state. Disabling locks the account out
// at the next login or refresh. Returns ErrNotFound if no such admin exists.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, s Status) error {
	ct, err := r.db.Exec(ctx,
		`UPDATE admins SET status = $2, updated_at = now() WHERE id = $1`, id, s)
	if err != nil {
		return fmt.Errorf("set admin status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetLevel changes an admin's privilege tier. Used by the seed bootstrap to
// self-heal a pre-existing account up to super_admin.
func (r *Repository) SetLevel(ctx context.Context, id uuid.UUID, l Level) error {
	ct, err := r.db.Exec(ctx,
		`UPDATE admins SET level = $2, updated_at = now() WHERE id = $1`, id, l)
	if err != nil {
		return fmt.Errorf("set admin level: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// row is the subset of pgx.Row / pgx.Rows that scanAdmin needs.
type row interface {
	Scan(dest ...any) error
}

func (r *Repository) getOne(ctx context.Context, query string, arg any) (*Admin, error) {
	a, err := scanAdmin(r.db.QueryRow(ctx, query, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query admin: %w", err)
	}
	return a, nil
}

func scanAdmin(r row) (*Admin, error) {
	var a Admin
	var email *string // nullable
	if err := r.Scan(
		&a.ID, &a.Phone, &email, &a.PasswordHash, &a.DisplayName, &a.Level, &a.Status, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if email != nil {
		a.Email = *email
	}
	return &a, nil
}

// nullable maps an empty string to a SQL NULL so optional columns (email) are
// stored as NULL rather than "", keeping the partial unique index meaningful.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
