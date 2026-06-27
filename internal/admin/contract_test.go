package admin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// This file is the single source of truth for the behaviour every admin.Store
// must honour. It runs against BOTH the in-memory fakeStore (here, no build tag)
// and the real Postgres Repository (repository_integration_test.go, under
// -tags=integration), so the fake can never silently drift from the database.
//
// List is deliberately excluded: its total and pagination are computed over the
// whole admins table, which the shared, never-truncated test DB pollutes across
// tests and runs, so the result can't be asserted exactly against both the fake
// and the real repo. List stays covered by the fake-backed service tests.

// randPhone / ctEmail mint identifiers unique across runs (the test DB is shared
// and never truncated), the same way the user package does it.
func randPhone() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10_000_000_000))
	if err != nil {
		panic(err) // crypto/rand failure is not a condition tests can proceed past
	}
	return fmt.Sprintf("1%010d", n.Int64())
}

func ctEmail() string { return "ct-admin-" + uuid.NewString() + "@example.com" }

func runStoreContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	mk := func() *Admin {
		return &Admin{ID: uuid.New(), Phone: randPhone(), Email: ctEmail(), PasswordHash: "h", DisplayName: "A", Level: LevelAdmin}
	}

	t.Run("create then get round-trips", func(t *testing.T) {
		st := newStore()
		a := mk()
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		for name, get := range map[string]func() (*Admin, error){
			"by id":    func() (*Admin, error) { return st.GetByID(ctx, a.ID) },
			"by phone": func() (*Admin, error) { return st.GetByPhone(ctx, a.Phone) },
			"by email": func() (*Admin, error) { return st.GetByEmail(ctx, a.Email) },
		} {
			got, err := get()
			if err != nil {
				t.Fatalf("Get %s: %v", name, err)
			}
			if got.ID != a.ID || got.Phone != a.Phone || got.Email != a.Email {
				t.Fatalf("Get %s: identity mismatch: %+v", name, got)
			}
			if got.Status != StatusActive {
				t.Errorf("Get %s: status = %q, want %q (DB column default)", name, got.Status, StatusActive)
			}
			if got.Level != LevelAdmin {
				t.Errorf("Get %s: level = %q, want %q", name, got.Level, LevelAdmin)
			}
		}
	})

	t.Run("duplicate phone is rejected", func(t *testing.T) {
		st := newStore()
		a := mk()
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create a: %v", err)
		}
		b := mk()
		b.Phone = a.Phone
		if err := st.Create(ctx, b); !errors.Is(err, ErrPhoneTaken) {
			t.Fatalf("Create dup phone: err = %v, want ErrPhoneTaken", err)
		}
	})

	t.Run("duplicate email is rejected case-insensitively", func(t *testing.T) {
		st := newStore()
		a := mk()
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create a: %v", err)
		}
		b := mk()
		b.Email = strings.ToUpper(a.Email)
		if err := st.Create(ctx, b); !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("Create dup email (upper): err = %v, want ErrEmailTaken", err)
		}
	})

	t.Run("empty emails do not conflict", func(t *testing.T) {
		st := newStore()
		a := mk()
		a.Email = ""
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create a (no email): %v", err)
		}
		b := mk()
		b.Email = ""
		if err := st.Create(ctx, b); err != nil {
			t.Fatalf("Create b (no email): %v — empty email must not collide", err)
		}
	})

	t.Run("lookups are case-insensitive on email", func(t *testing.T) {
		st := newStore()
		a := mk()
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := st.GetByEmail(ctx, strings.ToUpper(a.Email))
		if err != nil {
			t.Fatalf("GetByEmail (upper): %v", err)
		}
		if got.ID != a.ID {
			t.Fatalf("GetByEmail (upper): got %v, want %v", got.ID, a.ID)
		}
	})

	t.Run("missing rows return ErrNotFound", func(t *testing.T) {
		st := newStore()
		if _, err := st.GetByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetByID miss: err = %v, want ErrNotFound", err)
		}
		if _, err := st.GetByPhone(ctx, randPhone()); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetByPhone miss: err = %v, want ErrNotFound", err)
		}
		if _, err := st.GetByEmail(ctx, ctEmail()); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetByEmail miss: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("set status persists and rejects unknown ids", func(t *testing.T) {
		st := newStore()
		a := mk()
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := st.SetStatus(ctx, a.ID, StatusDisabled); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		got, err := st.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Status != StatusDisabled {
			t.Errorf("status = %q, want %q", got.Status, StatusDisabled)
		}
		if err := st.SetStatus(ctx, uuid.New(), StatusDisabled); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetStatus unknown: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("set level persists and rejects unknown ids", func(t *testing.T) {
		st := newStore()
		a := mk()
		if err := st.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := st.SetLevel(ctx, a.ID, LevelSuperAdmin); err != nil {
			t.Fatalf("SetLevel: %v", err)
		}
		got, err := st.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Level != LevelSuperAdmin {
			t.Errorf("level = %q, want %q", got.Level, LevelSuperAdmin)
		}
		if err := st.SetLevel(ctx, uuid.New(), LevelSuperAdmin); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetLevel unknown: err = %v, want ErrNotFound", err)
		}
	})
}

// TestStoreContract_Fake runs the contract against the in-memory fake. The
// Postgres counterpart lives in repository_integration_test.go.
func TestStoreContract_Fake(t *testing.T) {
	runStoreContract(t, func() Store { return newFakeStore() })
}
