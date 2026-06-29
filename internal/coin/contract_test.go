package coin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// This file is the single source of truth for the behaviour every coin.Store
// must honour. It runs against BOTH the in-memory fakeStore (here, no build tag)
// and the real Postgres Repository (repository_integration_test.go, under
// -tags=integration), so the fake can never silently drift from the database.
//
// Every subtest uses a fresh random owner id, so the shared, never-truncated
// integration DB does not pollute results across tests or runs.

func runStoreContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	// credit/debit post a signed entry directly through the Store (below the
	// service's biz-type validation), the way production callers' deltas arrive.
	credit := func(st Store, owner uuid.UUID, amt int64, idem *string) (*LedgerEntry, error) {
		return st.Post(ctx, LedgerEntry{Realm: RealmWeb, OwnerID: owner, Amount: amt, BizType: BizPlatformGrant}, idem)
	}
	debit := func(st Store, owner uuid.UUID, amt int64) (*LedgerEntry, error) {
		return st.Post(ctx, LedgerEntry{Realm: RealmWeb, OwnerID: owner, Amount: -amt, BizType: BizPlatformDeduct}, nil)
	}
	balance := func(t *testing.T, st Store, owner uuid.UUID) int64 {
		t.Helper()
		w, err := st.GetWallet(ctx, RealmWeb, owner)
		if err != nil {
			t.Fatalf("GetWallet: %v", err)
		}
		return w.Balance
	}

	t.Run("credit then balance and ledger reflect it", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		e, err := credit(st, owner, 889, nil)
		if err != nil {
			t.Fatalf("credit: %v", err)
		}
		if e.BalanceAfter != 889 || e.Amount != 889 {
			t.Fatalf("entry = %+v, want amount/balance_after 889", e)
		}
		if got := balance(t, st, owner); got != 889 {
			t.Fatalf("balance = %d, want 889", got)
		}
		items, total, err := st.ListByOwner(ctx, RealmWeb, owner, LedgerFilter{})
		if err != nil {
			t.Fatalf("ListByOwner: %v", err)
		}
		if total != 1 || len(items) != 1 || items[0].ID != e.ID {
			t.Fatalf("list = %+v (total %d), want the one entry", items, total)
		}
	})

	t.Run("missing wallet reads as zero, not an error", func(t *testing.T) {
		st := newStore()
		if got := balance(t, st, uuid.New()); got != 0 {
			t.Fatalf("balance of untouched wallet = %d, want 0", got)
		}
	})

	t.Run("debit beyond balance is rejected and leaves balance unchanged", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		if _, err := credit(st, owner, 100, nil); err != nil {
			t.Fatalf("credit: %v", err)
		}
		if _, err := debit(st, owner, 101); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("over-debit: err = %v, want ErrInsufficientBalance", err)
		}
		if got := balance(t, st, owner); got != 100 {
			t.Fatalf("balance = %d, want 100 (unchanged)", got)
		}
	})

	t.Run("idempotency key books exactly once", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		key := "recharge:" + uuid.NewString()
		a, err := credit(st, owner, 50, &key)
		if err != nil {
			t.Fatalf("first credit: %v", err)
		}
		b, err := credit(st, owner, 50, &key)
		if err != nil {
			t.Fatalf("second credit: %v", err)
		}
		if a.ID != b.ID {
			t.Fatalf("idempotent retry returned a new entry %v (want %v)", b.ID, a.ID)
		}
		if got := balance(t, st, owner); got != 50 {
			t.Fatalf("balance = %d, want 50 (booked once)", got)
		}
	})

	t.Run("reverse nets to zero and keeps the original on the books", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		orig, err := credit(st, owner, 976, nil)
		if err != nil {
			t.Fatalf("credit: %v", err)
		}
		by := uuid.New()
		rev, err := st.Reverse(ctx, orig.ID, by)
		if err != nil {
			t.Fatalf("Reverse: %v", err)
		}
		if rev.Amount != -976 || rev.ReversalOf == nil || *rev.ReversalOf != orig.ID {
			t.Fatalf("reversal = %+v, want amount -976 and reversal_of %v", rev, orig.ID)
		}
		if got := balance(t, st, owner); got != 0 {
			t.Fatalf("balance = %d, want 0 after reversal", got)
		}
		// Both the original and the reversal must remain on the books.
		_, total, err := st.ListByOwner(ctx, RealmWeb, owner, LedgerFilter{})
		if err != nil {
			t.Fatalf("ListByOwner: %v", err)
		}
		if total != 2 {
			t.Fatalf("ledger total = %d, want 2 (original + reversal)", total)
		}
	})

	t.Run("reverse is rejected the second time", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		orig, err := credit(st, owner, 10, nil)
		if err != nil {
			t.Fatalf("credit: %v", err)
		}
		if _, err := st.Reverse(ctx, orig.ID, uuid.New()); err != nil {
			t.Fatalf("first reverse: %v", err)
		}
		if _, err := st.Reverse(ctx, orig.ID, uuid.New()); !errors.Is(err, ErrAlreadyReversed) {
			t.Fatalf("second reverse: err = %v, want ErrAlreadyReversed", err)
		}
	})

	t.Run("a reversal cannot itself be reversed", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		orig, err := credit(st, owner, 10, nil)
		if err != nil {
			t.Fatalf("credit: %v", err)
		}
		rev, err := st.Reverse(ctx, orig.ID, uuid.New())
		if err != nil {
			t.Fatalf("reverse: %v", err)
		}
		if _, err := st.Reverse(ctx, rev.ID, uuid.New()); !errors.Is(err, ErrCannotReverseReversal) {
			t.Fatalf("reverse a reversal: err = %v, want ErrCannotReverseReversal", err)
		}
	})

	t.Run("reverse of an unknown entry is ErrNotFound", func(t *testing.T) {
		st := newStore()
		if _, err := st.Reverse(ctx, uuid.New(), uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("reverse missing: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("reverse fails when the coins were already spent", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		orig, err := credit(st, owner, 100, nil)
		if err != nil {
			t.Fatalf("credit: %v", err)
		}
		if _, err := debit(st, owner, 80); err != nil { // spent 80, only 20 left
			t.Fatalf("debit: %v", err)
		}
		if _, err := st.Reverse(ctx, orig.ID, uuid.New()); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("reverse spent: err = %v, want ErrInsufficientBalance", err)
		}
		if got := balance(t, st, owner); got != 20 {
			t.Fatalf("balance = %d, want 20 (reversal rolled back)", got)
		}
	})

	t.Run("concurrent debits never oversell", func(t *testing.T) {
		st := newStore()
		owner := uuid.New()
		if _, err := credit(st, owner, 50, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		const n = 100 // 100 racers each try to debit 1 from a balance of 50
		var success int64
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := debit(st, owner, 1); err == nil {
					atomic.AddInt64(&success, 1)
				}
			}()
		}
		wg.Wait()
		if success != 50 {
			t.Fatalf("successful debits = %d, want exactly 50", success)
		}
		if got := balance(t, st, owner); got != 0 {
			t.Fatalf("final balance = %d, want 0 (never negative, never oversold)", got)
		}
	})
}

// TestStoreContract_Fake runs the contract against the in-memory fake. The
// Postgres counterpart lives in repository_integration_test.go (-tags=integration).
func TestStoreContract_Fake(t *testing.T) {
	runStoreContract(t, func() Store { return newFakeStore() })
}
