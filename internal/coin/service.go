package coin

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	// ErrUnknownBizType means the biz type is not in the registry.
	ErrUnknownBizType = errors.New("unknown biz type")
	// ErrBizTypeNotAllowed means the biz type is not legal for this owner's
	// (realm, role) — e.g. a teacher earning a daily-task reward.
	ErrBizTypeNotAllowed = errors.New("biz type not allowed for role")
	// ErrAmountSign means the amount's sign contradicts the biz type's direction
	// (income must be positive, expense negative), or the amount is zero.
	ErrAmountSign = errors.New("amount sign does not match biz type")
)

// Store is the persistence behaviour the Service depends on. *Repository
// satisfies it in production; tests use an in-memory fake. Every method that
// changes a balance is atomic and refuses to go negative.
type Store interface {
	Post(ctx context.Context, e LedgerEntry, idem *string) (*LedgerEntry, error)
	Reverse(ctx context.Context, ledgerID, by uuid.UUID) (*LedgerEntry, error)
	GetWallet(ctx context.Context, realm Realm, ownerID uuid.UUID) (*Wallet, error)
	ListByOwner(ctx context.Context, realm Realm, ownerID uuid.UUID, f LedgerFilter) ([]LedgerEntry, int, error)
}

// Service holds the 天生币 business rules. It owns biz-type/role validation and
// sign checks; the Store owns atomicity and the non-negative invariant.
type Service struct {
	repo Store
}

func NewService(repo Store) *Service {
	return &Service{repo: repo}
}

// Owner identifies a wallet holder across the two identity realms. Role is the
// web active role (student/teacher) or the admin level (admin=词库管理员,
// super_admin); it is needed to validate which biz types the owner may carry.
type Owner struct {
	Realm Realm
	ID    uuid.UUID
	Role  string
}

func (o Owner) roleKey() string { return string(o.Realm) + ":" + o.Role }

// Post books a balance change for owner. magnitude is the absolute coin amount
// (> 0); its sign is derived from the biz type's direction, so callers never
// risk passing a wrong sign. note is free text; by is the operating admin for
// back-office actions (nil otherwise); idem makes the post idempotent.
//
// It validates that bt is known, legal for the owner's role, and that magnitude
// is positive, then delegates the atomic, non-negative balance change to the Store.
func (s *Service) Post(ctx context.Context, owner Owner, bt BizType, magnitude int64, note string, by *uuid.UUID, idem *string) (*LedgerEntry, error) {
	meta, err := s.validate(owner, bt, magnitude)
	if err != nil {
		return nil, err
	}
	amount := magnitude
	if meta.dir == DirExpense {
		amount = -magnitude
	}
	return s.repo.Post(ctx, LedgerEntry{
		Realm:     owner.Realm,
		OwnerID:   owner.ID,
		Amount:    amount,
		BizType:   bt,
		Note:      note,
		CreatedBy: by,
	}, idem)
}

// Grant is the super-admin path to credit coins to any holder (后台定向发币).
func (s *Service) Grant(ctx context.Context, owner Owner, magnitude int64, note string, by uuid.UUID) (*LedgerEntry, error) {
	return s.Post(ctx, owner, BizPlatformGrant, magnitude, note, &by, nil)
}

// Deduct is the super-admin path to debit coins from any holder (后台平台扣除).
// Fails with ErrInsufficientBalance if the holder lacks the coins.
func (s *Service) Deduct(ctx context.Context, owner Owner, magnitude int64, note string, by uuid.UUID) (*LedgerEntry, error) {
	return s.Post(ctx, owner, BizPlatformDeduct, magnitude, note, &by, nil)
}

// Reverse books a 红冲 for a ledger entry (图1 的「删除」). by is the operating
// admin. Returns ErrInsufficientBalance if the coins were already spent.
func (s *Service) Reverse(ctx context.Context, ledgerID, by uuid.UUID) (*LedgerEntry, error) {
	return s.repo.Reverse(ctx, ledgerID, by)
}

// Balance returns an owner's current 天生币 balance (zero if no activity).
func (s *Service) Balance(ctx context.Context, realm Realm, ownerID uuid.UUID) (*Wallet, error) {
	return s.repo.GetWallet(ctx, realm, ownerID)
}

// Ledger returns a page of an owner's own records (图2) plus the total count.
func (s *Service) Ledger(ctx context.Context, realm Realm, ownerID uuid.UUID, f LedgerFilter) ([]LedgerEntry, int, error) {
	return s.repo.ListByOwner(ctx, realm, ownerID, f)
}

// validate checks the biz type is known, legal for the owner's role, and that
// magnitude is a positive amount. It returns the resolved meta so the caller can
// apply the direction's sign.
func (s *Service) validate(owner Owner, bt BizType, magnitude int64) (bizMeta, error) {
	meta, ok := bizRegistry[bt]
	if !ok {
		return bizMeta{}, fmt.Errorf("%w: %q", ErrUnknownBizType, bt)
	}
	if !meta.roles[owner.roleKey()] {
		return bizMeta{}, fmt.Errorf("%w: %q for %s", ErrBizTypeNotAllowed, bt, owner.roleKey())
	}
	if magnitude <= 0 {
		return bizMeta{}, ErrAmountSign
	}
	return meta, nil
}
