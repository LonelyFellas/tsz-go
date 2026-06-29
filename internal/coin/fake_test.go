package coin

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory Store for unit-testing the Service and for running
// the shared contract without a DB. It mirrors the repository's atomic
// semantics: a single mutex serializes balance changes, debits refuse to go
// negative (ErrInsufficientBalance), idempotency keys book exactly once, and
// reversals follow the same not-found / already-reversed / cannot-reverse rules.
type fakeStore struct {
	mu       sync.Mutex
	wallets  map[string]*Wallet
	ledger   []LedgerEntry
	byIdem   map[string]uuid.UUID
	reversed map[uuid.UUID]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		wallets:  make(map[string]*Wallet),
		byIdem:   make(map[string]uuid.UUID),
		reversed: make(map[uuid.UUID]bool),
	}
}

func walletKey(realm Realm, ownerID uuid.UUID) string {
	return string(realm) + ":" + ownerID.String()
}

func (f *fakeStore) ensureWallet(realm Realm, ownerID uuid.UUID) *Wallet {
	k := walletKey(realm, ownerID)
	w, ok := f.wallets[k]
	if !ok {
		w = &Wallet{Realm: realm, OwnerID: ownerID}
		f.wallets[k] = w
	}
	return w
}

func (f *fakeStore) findByID(id uuid.UUID) *LedgerEntry {
	for i := range f.ledger {
		if f.ledger[i].ID == id {
			cp := f.ledger[i]
			return &cp
		}
	}
	return nil
}

func (f *fakeStore) Post(_ context.Context, e LedgerEntry, idem *string) (*LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if idem != nil {
		if id, ok := f.byIdem[*idem]; ok {
			return f.findByID(id), nil // already booked → no balance change
		}
	}
	w := f.ensureWallet(e.Realm, e.OwnerID)
	if w.Balance+e.Amount < 0 {
		return nil, ErrInsufficientBalance
	}
	w.Balance += e.Amount
	w.Version++

	e.ID = uuid.New()
	e.BalanceAfter = w.Balance
	e.CreatedAt = time.Now()
	f.ledger = append(f.ledger, e)
	if idem != nil {
		f.byIdem[*idem] = e.ID
	}
	cp := e
	return &cp, nil
}

func (f *fakeStore) Reverse(_ context.Context, ledgerID, by uuid.UUID) (*LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	orig := f.findByID(ledgerID)
	if orig == nil {
		return nil, ErrNotFound
	}
	if orig.ReversalOf != nil {
		return nil, ErrCannotReverseReversal
	}
	if f.reversed[ledgerID] {
		return nil, ErrAlreadyReversed
	}
	w := f.ensureWallet(orig.Realm, orig.OwnerID)
	if w.Balance-orig.Amount < 0 {
		return nil, ErrInsufficientBalance
	}
	w.Balance -= orig.Amount
	w.Version++

	rev := LedgerEntry{
		ID:           uuid.New(),
		Realm:        orig.Realm,
		OwnerID:      orig.OwnerID,
		Amount:       -orig.Amount,
		BalanceAfter: w.Balance,
		BizType:      orig.BizType,
		Note:         fmt.Sprintf("红冲 #%s", ledgerID),
		ReversalOf:   &ledgerID,
		CreatedBy:    &by,
		CreatedAt:    time.Now(),
	}
	f.ledger = append(f.ledger, rev)
	f.reversed[ledgerID] = true
	cp := rev
	return &cp, nil
}

func (f *fakeStore) GetWallet(_ context.Context, realm Realm, ownerID uuid.UUID) (*Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.wallets[walletKey(realm, ownerID)]; ok {
		cp := *w
		return &cp, nil
	}
	return &Wallet{Realm: realm, OwnerID: ownerID}, nil
}

func (f *fakeStore) ListByOwner(_ context.Context, realm Realm, ownerID uuid.UUID, ff LedgerFilter) ([]LedgerEntry, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var all []LedgerEntry
	for _, e := range f.ledger {
		if e.Realm != realm || e.OwnerID != ownerID {
			continue
		}
		if ff.Direction == DirIncome && e.Amount <= 0 {
			continue
		}
		if ff.Direction == DirExpense && e.Amount >= 0 {
			continue
		}
		if !ff.From.IsZero() && e.CreatedAt.Before(ff.From) {
			continue
		}
		if !ff.To.IsZero() && e.CreatedAt.After(ff.To) {
			continue
		}
		all = append(all, e)
	}
	// Newest first, id as a stable tiebreak — mirrors the repo's ORDER BY.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID.String() > all[j].ID.String()
	})

	total := len(all)
	limit, offset := pageBounds(ff.Page, ff.PageSize)
	if offset > len(all) {
		offset = len(all)
	}
	hi := offset + limit
	if hi > len(all) {
		hi = len(all)
	}
	return all[offset:hi], total, nil
}

// compile-time guard: fakeStore satisfies Store.
var _ Store = (*fakeStore)(nil)
