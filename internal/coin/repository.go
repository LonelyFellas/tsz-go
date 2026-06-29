package coin

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
	// ErrNotFound means no ledger entry has the given id.
	ErrNotFound = errors.New("ledger entry not found")
	// ErrInsufficientBalance means a debit (or a reversal of a credit already
	// spent) would drive the balance below zero. The whole post is rolled back.
	ErrInsufficientBalance = errors.New("insufficient balance")
	// ErrAlreadyReversed means the target entry has already been reversed once.
	ErrAlreadyReversed = errors.New("ledger already reversed")
	// ErrCannotReverseReversal means the target is itself a reversal entry; a
	// reversal is final and cannot be reversed again.
	ErrCannotReverseReversal = errors.New("cannot reverse a reversal")
)

// Repository is the data-access boundary for the coin ledger. SQL is hand-written
// here; the service depends only on the Store method signatures, not on pgx.
type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// Post is the single entry point for a balance change. In one transaction it
// applies the signed delta to the wallet (creating it lazily, refusing to go
// negative) and appends an immutable ledger row. e.Amount carries the sign
// (positive credits, negative debits); e.ID/BalanceAfter/CreatedAt are filled by
// this method. The caller (service) has already validated biz type, role and sign.
//
// idem, when non-nil, makes the post idempotent: a second call with the same key
// performs no balance change and returns the original entry, so a retried webhook
// or a double click books exactly once.
func (r *Repository) Post(ctx context.Context, e LedgerEntry, idem *string) (*LedgerEntry, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful commit

	// Idempotency short-circuit: if this key already booked, return that entry and
	// touch nothing. Covers the common case (sequential retry).
	if idem != nil {
		if existing, err := getByIdemTx(ctx, tx, *idem); err == nil {
			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("commit tx: %w", err)
			}
			return existing, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	bal, err := applyDeltaTx(ctx, tx, e.Realm, e.OwnerID, e.Amount)
	if err != nil {
		return nil, err // ErrInsufficientBalance or a wrapped pg error
	}

	e.ID = uuid.New()
	e.BalanceAfter = bal
	if err := insertLedgerTx(ctx, tx, &e, idem); err != nil {
		// Lost an idempotency race: another tx booked the same key concurrently.
		// Roll back our (now duplicate) delta and return the winner's entry.
		if idem != nil && isUniqueViolation(err, "idem") {
			_ = tx.Rollback(ctx)
			return r.getByIdem(ctx, *idem)
		}
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return &e, nil
}

// Reverse books a 红冲 for ledgerID: it appends an opposite entry that nets the
// original to zero, keeping the original on the books. The original must exist,
// not already be reversed, and not itself be a reversal. If the owner has since
// spent the credited coins so the reversal would go negative, it fails with
// ErrInsufficientBalance and a human must intervene (mirrors real bookkeeping).
func (r *Repository) Reverse(ctx context.Context, ledgerID, by uuid.UUID) (*LedgerEntry, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var orig LedgerEntry
	var reversalOf *uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT owner_realm, owner_id, amount, biz_type, reversal_of
		   FROM coin_ledger WHERE id = $1 FOR UPDATE`, ledgerID).
		Scan(&orig.Realm, &orig.OwnerID, &orig.Amount, &orig.BizType, &reversalOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load original: %w", err)
	}
	if reversalOf != nil {
		return nil, ErrCannotReverseReversal
	}

	var alreadyReversed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM coin_ledger WHERE reversal_of = $1)`, ledgerID).
		Scan(&alreadyReversed); err != nil {
		return nil, fmt.Errorf("check reversed: %w", err)
	}
	if alreadyReversed {
		return nil, ErrAlreadyReversed
	}

	bal, err := applyDeltaTx(ctx, tx, orig.Realm, orig.OwnerID, -orig.Amount)
	if err != nil {
		return nil, err // ErrInsufficientBalance when the coins are already spent
	}

	rev := LedgerEntry{
		ID:           uuid.New(),
		Realm:        orig.Realm,
		OwnerID:      orig.OwnerID,
		Amount:       -orig.Amount,
		BalanceAfter: bal,
		BizType:      orig.BizType,
		Note:         fmt.Sprintf("红冲 #%s", ledgerID),
		ReversalOf:   &ledgerID,
		CreatedBy:    &by,
	}
	if err := insertLedgerTx(ctx, tx, &rev, nil); err != nil {
		// Concurrent reverse of the same entry: the partial unique index on
		// reversal_of rejects the second one.
		if isUniqueViolation(err, "reversal") {
			return nil, ErrAlreadyReversed
		}
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return &rev, nil
}

// GetWallet returns an owner's balance snapshot. A wallet that has never been
// posted to does not exist yet; that is reported as a zero-balance wallet, not an
// error (no row means no activity, which is balance 0).
func (r *Repository) GetWallet(ctx context.Context, realm Realm, ownerID uuid.UUID) (*Wallet, error) {
	w := Wallet{Realm: realm, OwnerID: ownerID}
	err := r.db.QueryRow(ctx,
		`SELECT balance, version FROM coin_wallet WHERE owner_realm = $1 AND owner_id = $2`,
		realm, ownerID).Scan(&w.Balance, &w.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return &w, nil // no activity yet → zero balance
	}
	if err != nil {
		return nil, fmt.Errorf("query wallet: %w", err)
	}
	return &w, nil
}

// ListByOwner returns a page of one owner's ledger (图2 "我的收支记录") plus the
// total matching count, newest first. The filter narrows by direction and a
// created_at range; the service clamps the page size.
func (r *Repository) ListByOwner(ctx context.Context, realm Realm, ownerID uuid.UUID, f LedgerFilter) ([]LedgerEntry, int, error) {
	where := []string{"owner_realm = $1", "owner_id = $2"}
	args := []any{realm, ownerID}
	switch f.Direction {
	case DirIncome:
		where = append(where, "amount > 0")
	case DirExpense:
		where = append(where, "amount < 0")
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		where = append(where, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM coin_ledger WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count ledger: %w", err)
	}

	limit, offset := pageBounds(f.Page, f.PageSize)
	args = append(args, limit, offset)
	q := fmt.Sprintf(
		`SELECT id, owner_realm, owner_id, amount, balance_after, biz_type, note, reversal_of, created_by, created_at
		   FROM coin_ledger WHERE %s
		  ORDER BY created_at DESC, id DESC
		  LIMIT $%d OFFSET $%d`, cond, len(args)-1, len(args))
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query ledger: %w", err)
	}
	defer rows.Close()

	var out []LedgerEntry
	for rows.Next() {
		e, err := scanLedger(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate ledger: %w", err)
	}
	return out, total, nil
}

// applyDeltaTx lazily creates the wallet and applies a signed delta, refusing to
// drop below zero. The conditional UPDATE is what makes concurrent debits safe:
// the row lock serializes them and the balance guard rejects an oversell with
// zero rows affected. Returns the new balance.
func applyDeltaTx(ctx context.Context, tx pgx.Tx, realm Realm, ownerID uuid.UUID, delta int64) (int64, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO coin_wallet (owner_realm, owner_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, realm, ownerID); err != nil {
		return 0, fmt.Errorf("ensure wallet: %w", err)
	}
	var bal int64
	err := tx.QueryRow(ctx,
		`UPDATE coin_wallet
		    SET balance = balance + $3, version = version + 1, updated_at = now()
		  WHERE owner_realm = $1 AND owner_id = $2 AND balance + $3 >= 0
		RETURNING balance`, realm, ownerID, delta).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInsufficientBalance
	}
	if err != nil {
		return 0, fmt.Errorf("apply delta: %w", err)
	}
	return bal, nil
}

// insertLedgerTx writes one immutable ledger row, filling created_at from the DB
// default back onto e.
func insertLedgerTx(ctx context.Context, tx pgx.Tx, e *LedgerEntry, idem *string) error {
	err := tx.QueryRow(ctx,
		`INSERT INTO coin_ledger
		   (id, owner_realm, owner_id, amount, balance_after, biz_type, note, reversal_of, idempotency_key, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 RETURNING created_at`,
		e.ID, e.Realm, e.OwnerID, e.Amount, e.BalanceAfter, e.BizType, e.Note, e.ReversalOf, idem, e.CreatedBy).
		Scan(&e.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert ledger: %w", err)
	}
	return nil
}

func (r *Repository) getByIdem(ctx context.Context, idem string) (*LedgerEntry, error) {
	row := r.db.QueryRow(ctx, selectLedger+` WHERE idempotency_key = $1`, idem)
	return scanLedger(row)
}

func getByIdemTx(ctx context.Context, tx pgx.Tx, idem string) (*LedgerEntry, error) {
	row := tx.QueryRow(ctx, selectLedger+` WHERE idempotency_key = $1`, idem)
	return scanLedger(row)
}

const selectLedger = `SELECT id, owner_realm, owner_id, amount, balance_after, biz_type, note, reversal_of, created_by, created_at FROM coin_ledger`

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanLedger(row rowScanner) (*LedgerEntry, error) {
	var e LedgerEntry
	if err := row.Scan(&e.ID, &e.Realm, &e.OwnerID, &e.Amount, &e.BalanceAfter,
		&e.BizType, &e.Note, &e.ReversalOf, &e.CreatedBy, &e.CreatedAt); err != nil {
		return nil, err // pgx.ErrNoRows passes through for the idempotency check
	}
	return &e, nil
}

// pageBounds turns a 1-based page + size into LIMIT/OFFSET with sane defaults.
func pageBounds(page, size int) (limit, offset int) {
	if size <= 0 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	if page <= 0 {
		page = 1
	}
	return size, (page - 1) * size
}

// isUniqueViolation reports whether err is a Postgres unique_violation on an
// index whose name contains the given fragment (e.g. "idem", "reversal").
func isUniqueViolation(err error, indexFragment string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return strings.Contains(pgErr.ConstraintName, indexFragment)
	}
	return false
}

// compile-time guard: Repository satisfies the service's Store.
var _ Store = (*Repository)(nil)
