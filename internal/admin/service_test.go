package admin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/session"
)

func newTestService() (*Service, *fakeStore, *fakeSessions, *auth.TokenManager) {
	store := newFakeStore()
	sessions := newFakeSessions()
	tm := auth.NewTokenManager("admin-test-secret", time.Hour, auth.RealmAdmin)
	return NewService(store, tm, sessions), store, sessions, tm
}

// seedActive creates an active admin with the given level + password.
func seedActive(t *testing.T, svc *Service, phone, password string, level Level) *Admin {
	t.Helper()
	a, err := svc.Create(context.Background(), phone, "", password, "Name", level)
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return a
}

func TestService_Login(t *testing.T) {
	svc, _, _, tm := newTestService()
	ctx := context.Background()
	seedActive(t, svc, "13800138000", "password123", LevelSuperAdmin)

	t.Run("success issues admin-realm token", func(t *testing.T) {
		a, access, refresh, err := svc.Login(ctx, "13800138000", "password123")
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		if refresh == "" {
			t.Error("missing refresh token")
		}
		claims, err := tm.Parse(access)
		if err != nil {
			t.Fatalf("parse access: %v", err)
		}
		if claims.Subject != a.ID {
			t.Errorf("token subject = %s, want %s", claims.Subject, a.ID)
		}
		if claims.Realm != auth.RealmAdmin {
			t.Errorf("realm = %q, want admin", claims.Realm)
		}
		if claims.Role != string(LevelSuperAdmin) {
			t.Errorf("level claim = %q, want super_admin", claims.Role)
		}
	})

	t.Run("wrong password is generic invalid credentials", func(t *testing.T) {
		if _, _, _, err := svc.Login(ctx, "13800138000", "nope"); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("unknown identifier is invalid credentials", func(t *testing.T) {
		if _, _, _, err := svc.Login(ctx, "00000000000", "password123"); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
	})
}

func TestService_Login_DisabledRejected(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()
	a := seedActive(t, svc, "13800138000", "password123", LevelAdmin)
	if err := store.SetStatus(ctx, a.ID, StatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, _, _, err := svc.Login(ctx, "13800138000", "password123"); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("err = %v, want ErrAccountDisabled", err)
	}
}

func TestService_Refresh_DisabledRejected(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()
	a := seedActive(t, svc, "13800138000", "password123", LevelAdmin)
	_, _, refresh, err := svc.Login(ctx, "13800138000", "password123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// While active, refresh works.
	if _, newRefresh, err := svc.Refresh(ctx, refresh); err != nil {
		t.Fatalf("refresh while active: %v", err)
	} else {
		refresh = newRefresh
	}

	// Disabling makes the next refresh fail (effective within one access TTL).
	if err := store.SetStatus(ctx, a.ID, StatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := svc.Refresh(ctx, refresh); !errors.Is(err, session.ErrInvalidRefreshToken) {
		t.Errorf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestService_Create(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()

	t.Run("default level is admin", func(t *testing.T) {
		a, err := svc.Create(ctx, "13800138001", "", "password123", "A", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if a.Level != LevelAdmin {
			t.Errorf("level = %q, want admin", a.Level)
		}
	})

	t.Run("invalid level rejected", func(t *testing.T) {
		if _, err := svc.Create(ctx, "13800138002", "", "password123", "A", Level("root")); !errors.Is(err, ErrInvalidLevel) {
			t.Errorf("err = %v, want ErrInvalidLevel", err)
		}
	})

	t.Run("duplicate phone conflicts", func(t *testing.T) {
		if _, err := svc.Create(ctx, "13800138001", "", "password123", "A", LevelAdmin); !errors.Is(err, ErrPhoneTaken) {
			t.Errorf("err = %v, want ErrPhoneTaken", err)
		}
	})
}

func TestService_SeedSuperAdmin_Idempotent(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()

	first, err := svc.SeedSuperAdmin(ctx, " 13800138000 ", "password123", "Administrator")
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if first.Phone != "13800138000" {
		t.Errorf("phone = %q, want trimmed", first.Phone)
	}
	if first.Level != LevelSuperAdmin {
		t.Errorf("level = %q, want super_admin", first.Level)
	}

	second, err := svc.SeedSuperAdmin(ctx, "13800138000", "ignored", "Administrator")
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second seed created a new account: %s != %s", second.ID, first.ID)
	}
	if len(store.byID) != 1 {
		t.Errorf("account count = %d, want 1", len(store.byID))
	}
}

func TestService_SeedSuperAdmin_SelfHeals(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()

	// A pre-existing plain admin that has been disabled.
	plain := seedActive(t, svc, "13800138000", "password123", LevelAdmin)
	if err := store.SetStatus(ctx, plain.ID, StatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// Seeding over it must promote to super_admin AND re-activate, reusing the
	// same account (no duplicate).
	healed, err := svc.SeedSuperAdmin(ctx, "13800138000", "ignored", "Administrator")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if healed.ID != plain.ID {
		t.Errorf("seed created a new account: %s != %s", healed.ID, plain.ID)
	}
	if healed.Level != LevelSuperAdmin {
		t.Errorf("level = %q, want super_admin", healed.Level)
	}
	if healed.Status != StatusActive {
		t.Errorf("status = %q, want active", healed.Status)
	}
	if len(store.byID) != 1 {
		t.Errorf("account count = %d, want 1", len(store.byID))
	}
}

func TestService_SetStatus_Invalid(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	a := seedActive(t, svc, "13800138000", "password123", LevelAdmin)

	if err := svc.SetStatus(ctx, a.ID, Status("frozen")); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("err = %v, want ErrInvalidStatus", err)
	}
}

func TestService_SetStatus_LastSuperAdminProtected(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	super1 := seedActive(t, svc, "13800138000", "password123", LevelSuperAdmin)

	// Disabling the only super_admin is refused — no lockout.
	if err := svc.SetStatus(ctx, super1.ID, StatusDisabled); !errors.Is(err, ErrLastSuperAdmin) {
		t.Fatalf("err = %v, want ErrLastSuperAdmin", err)
	}

	// A disabled plain admin is irrelevant to the super_admin count.
	plain := seedActive(t, svc, "13800138001", "password123", LevelAdmin)
	if err := svc.SetStatus(ctx, plain.ID, StatusDisabled); err != nil {
		t.Fatalf("disable plain admin: %v", err)
	}
	if err := svc.SetStatus(ctx, super1.ID, StatusDisabled); !errors.Is(err, ErrLastSuperAdmin) {
		t.Fatalf("err = %v, want ErrLastSuperAdmin (plain admin must not count)", err)
	}

	// With a second active super_admin, disabling the first is allowed.
	super2 := seedActive(t, svc, "13800138002", "password123", LevelSuperAdmin)
	if err := svc.SetStatus(ctx, super1.ID, StatusDisabled); err != nil {
		t.Fatalf("disable with backup super present: %v", err)
	}

	// Now super2 is the last active one again — protected.
	if err := svc.SetStatus(ctx, super2.ID, StatusDisabled); !errors.Is(err, ErrLastSuperAdmin) {
		t.Fatalf("err = %v, want ErrLastSuperAdmin", err)
	}
}

// TestService_SetStatus_ConcurrentDisable exercises the TOCTOU window the
// store-level guard closes: with exactly two active super_admins, goroutines
// race to disable both. A check-then-write guard lets one request per target
// slip through and end at zero; the atomic guard must keep exactly one active.
func TestService_SetStatus_ConcurrentDisable(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()
	super1 := seedActive(t, svc, "13800138000", "password123", LevelSuperAdmin)
	super2 := seedActive(t, svc, "13800138001", "password123", LevelSuperAdmin)

	var wg sync.WaitGroup
	for range 10 {
		for _, id := range []uuid.UUID{super1.ID, super2.ID} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Both outcomes are legal per call; only the aggregate matters.
				_ = svc.SetStatus(ctx, id, StatusDisabled)
			}()
		}
	}
	wg.Wait()

	supers, _, err := store.List(ctx, ListFilter{Level: LevelSuperAdmin, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	active := 0
	for _, a := range supers {
		if a.Status == StatusActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active super_admins after concurrent disables = %d, want exactly 1", active)
	}
}
