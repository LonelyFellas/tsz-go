package user

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

// This file is the single source of truth for the behavioural contract every
// Store implementation must honour. It runs against BOTH the in-memory fakeStore
// (here, with no build tag) and the real Postgres-backed Repository (in
// repository_integration_test.go, under -tags=integration). Keeping one set of
// assertions means the fake can never silently drift away from real database
// behaviour: a schema or query change that breaks a contract fails the
// integration run, and a fake that stops mirroring it fails the unit run.
//
// Fault injection (forcing Create/Get to error via the fake's *Fn hooks) is NOT
// part of the contract — that stays in service_test.go / handler_test.go, since
// the real DB has no equivalent knobs.

// randPhone and ctEmail mint identifiers that are unique *across runs*, not just
// within one process. The integration tests share a persistent Postgres that is
// never truncated (so packages can run in parallel against it), which means a
// per-process counter would collide with rows left by a previous run. Drawing
// from crypto/rand over the full 10-digit space makes that collision negligible,
// the same way email uniqueness already rides on a uuid.
func randPhone() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10_000_000_000))
	if err != nil {
		panic(err) // crypto/rand failure is not a condition tests can proceed past
	}
	return fmt.Sprintf("1%010d", n.Int64())
}

func ctEmail() string { return "ct-" + uuid.NewString() + "@example.com" }

func rolesContain(roles []Role, want Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// runStoreContract exercises the behaviour every Store must share. newStore is
// called once per sub-test; for the fake it returns a fresh isolated store, for
// the real repo it returns one backed by a shared pool (sub-tests stay isolated
// by always minting unique phones/emails/IDs).
func runStoreContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	mkUser := func() *User {
		return &User{ID: uuid.New(), Phone: randPhone(), Email: ctEmail(), PasswordHash: "h", DisplayName: "C"}
	}

	t.Run("create then get round-trips", func(t *testing.T) {
		st := newStore()
		u := mkUser()
		if err := st.Create(ctx, u, RoleStudent); err != nil {
			t.Fatalf("Create: %v", err)
		}
		for name, get := range map[string]func() (*User, error){
			"by id":    func() (*User, error) { return st.GetByID(ctx, u.ID) },
			"by phone": func() (*User, error) { return st.GetByPhone(ctx, u.Phone) },
			"by email": func() (*User, error) { return st.GetByEmail(ctx, u.Email) },
		} {
			got, err := get()
			if err != nil {
				t.Fatalf("Get %s: %v", name, err)
			}
			if got.ID != u.ID || got.Phone != u.Phone || got.Email != u.Email {
				t.Fatalf("Get %s: identity mismatch: %+v", name, got)
			}
			if got.Status != StatusActive {
				t.Errorf("Get %s: status = %q, want %q (DB column default)", name, got.Status, StatusActive)
			}
			if got.LastActiveRole != RoleStudent {
				t.Errorf("Get %s: last active role = %q, want %q", name, got.LastActiveRole, RoleStudent)
			}
			if !rolesContain(got.Roles, RoleStudent) {
				t.Errorf("Get %s: roles = %v, want to contain %q", name, got.Roles, RoleStudent)
			}
		}
	})

	t.Run("duplicate phone is rejected", func(t *testing.T) {
		st := newStore()
		a := mkUser()
		if err := st.Create(ctx, a, RoleStudent); err != nil {
			t.Fatalf("Create a: %v", err)
		}
		b := mkUser()
		b.Phone = a.Phone
		if err := st.Create(ctx, b, RoleStudent); !errors.Is(err, ErrPhoneTaken) {
			t.Fatalf("Create dup phone: err = %v, want ErrPhoneTaken", err)
		}
	})

	t.Run("duplicate email is rejected case-insensitively", func(t *testing.T) {
		st := newStore()
		a := mkUser()
		if err := st.Create(ctx, a, RoleStudent); err != nil {
			t.Fatalf("Create a: %v", err)
		}
		b := mkUser()
		b.Email = strings.ToUpper(a.Email)
		if err := st.Create(ctx, b, RoleStudent); !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("Create dup email (upper): err = %v, want ErrEmailTaken", err)
		}
	})

	t.Run("empty emails do not conflict", func(t *testing.T) {
		st := newStore()
		a := mkUser()
		a.Email = ""
		if err := st.Create(ctx, a, RoleStudent); err != nil {
			t.Fatalf("Create a (no email): %v", err)
		}
		b := mkUser()
		b.Email = ""
		if err := st.Create(ctx, b, RoleStudent); err != nil {
			t.Fatalf("Create b (no email): %v — empty email must not collide", err)
		}
	})

	t.Run("lookups are case-insensitive on email", func(t *testing.T) {
		st := newStore()
		u := mkUser()
		if err := st.Create(ctx, u, RoleStudent); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := st.GetByEmail(ctx, strings.ToUpper(u.Email))
		if err != nil {
			t.Fatalf("GetByEmail (upper): %v", err)
		}
		if got.ID != u.ID {
			t.Fatalf("GetByEmail (upper): got %v, want %v", got.ID, u.ID)
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

	t.Run("add role grants membership and rejects duplicates", func(t *testing.T) {
		st := newStore()
		u := mkUser()
		if err := st.Create(ctx, u, RoleStudent); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if has, err := st.HasRole(ctx, u.ID, RoleTeacher); err != nil || has {
			t.Fatalf("HasRole teacher before add: has=%v err=%v, want false/nil", has, err)
		}
		if err := st.AddRole(ctx, u.ID, RoleTeacher); err != nil {
			t.Fatalf("AddRole teacher: %v", err)
		}
		if has, err := st.HasRole(ctx, u.ID, RoleTeacher); err != nil || !has {
			t.Fatalf("HasRole teacher after add: has=%v err=%v, want true/nil", has, err)
		}
		if err := st.AddRole(ctx, u.ID, RoleTeacher); !errors.Is(err, ErrRoleTaken) {
			t.Fatalf("AddRole teacher again: err = %v, want ErrRoleTaken", err)
		}
		got, err := st.GetByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if !rolesContain(got.Roles, RoleStudent) || !rolesContain(got.Roles, RoleTeacher) {
			t.Errorf("roles = %v, want both student and teacher", got.Roles)
		}
	})

	t.Run("set active role persists", func(t *testing.T) {
		st := newStore()
		u := mkUser()
		if err := st.Create(ctx, u, RoleStudent); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := st.AddRole(ctx, u.ID, RoleTeacher); err != nil {
			t.Fatalf("AddRole: %v", err)
		}
		if err := st.SetActiveRole(ctx, u.ID, RoleTeacher); err != nil {
			t.Fatalf("SetActiveRole: %v", err)
		}
		got, err := st.GetByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.LastActiveRole != RoleTeacher {
			t.Errorf("last active role = %q, want %q", got.LastActiveRole, RoleTeacher)
		}
	})

	t.Run("learning settings require a student profile", func(t *testing.T) {
		st := newStore()
		u := mkUser()
		if err := st.Create(ctx, u, RoleTeacher); err != nil {
			t.Fatalf("Create teacher: %v", err)
		}
		err := st.SetLearningSettings(ctx, u.ID, &LearningSettings{CEFRLevel: CEFRB1, EnglishVariant: VariantBritish})
		if !errors.Is(err, ErrNoStudentProfile) {
			t.Fatalf("SetLearningSettings on teacher: err = %v, want ErrNoStudentProfile", err)
		}
	})

	t.Run("learning settings round-trip for a student", func(t *testing.T) {
		st := newStore()
		u := mkUser()
		if err := st.Create(ctx, u, RoleStudent); err != nil {
			t.Fatalf("Create student: %v", err)
		}
		// Freshly created student has not onboarded: nil, not an error.
		if got, err := st.GetLearningSettings(ctx, u.ID); err != nil || got != nil {
			t.Fatalf("GetLearningSettings before onboarding: got=%v err=%v, want nil/nil", got, err)
		}
		want := &LearningSettings{CEFRLevel: CEFRB2, EnglishVariant: VariantAmerican}
		if err := st.SetLearningSettings(ctx, u.ID, want); err != nil {
			t.Fatalf("SetLearningSettings: %v", err)
		}
		got, err := st.GetLearningSettings(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetLearningSettings after set: %v", err)
		}
		if got == nil || *got != *want {
			t.Errorf("learning settings = %v, want %v", got, want)
		}
	})
}

// TestStoreContract_Fake runs the contract against the in-memory fake. The
// Postgres counterpart lives in repository_integration_test.go.
func TestStoreContract_Fake(t *testing.T) {
	runStoreContract(t, func() Store { return newFakeStore() })
}
