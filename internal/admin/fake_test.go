package admin

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/session"
)

// fakeStore is an in-memory Store for unit-testing the Service without a DB. It
// mirrors the repository contract: unique phone, case-insensitive unique email,
// ErrPhoneTaken/ErrEmailTaken on conflict, ErrNotFound on misses.
type fakeStore struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]*Admin
	getErr error // optional override to force GetByID to fail
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: make(map[uuid.UUID]*Admin)}
}

func (f *fakeStore) Create(_ context.Context, a *Admin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byID {
		if e.Phone == a.Phone {
			return ErrPhoneTaken
		}
		if a.Email != "" && strings.EqualFold(e.Email, a.Email) {
			return ErrEmailTaken
		}
	}
	if a.Status == "" {
		a.Status = StatusActive // mirror the DB column default
	}
	cp := *a
	f.byID[a.ID] = &cp
	return nil
}

func (f *fakeStore) GetByID(_ context.Context, id uuid.UUID) (*Admin, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeStore) GetByPhone(_ context.Context, phone string) (*Admin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.byID {
		if a.Phone == phone {
			cp := *a
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) GetByEmail(_ context.Context, email string) (*Admin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.byID {
		if a.Email != "" && strings.EqualFold(a.Email, email) {
			cp := *a
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) List(_ context.Context, ff ListFilter) ([]Admin, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []Admin
	for _, a := range f.byID {
		if ff.Level != "" && a.Level != ff.Level {
			continue
		}
		all = append(all, *a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Phone < all[j].Phone })
	total := int64(len(all))
	lo := ff.Offset
	if lo > len(all) {
		lo = len(all)
	}
	hi := lo + ff.Limit
	if hi > len(all) {
		hi = len(all)
	}
	return all[lo:hi], total, nil
}

func (f *fakeStore) SetStatus(_ context.Context, id uuid.UUID, s Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[id]
	if !ok {
		return ErrNotFound
	}
	a.Status = s
	return nil
}

func (f *fakeStore) SetLevel(_ context.Context, id uuid.UUID, l Level) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[id]
	if !ok {
		return ErrNotFound
	}
	a.Level = l
	return nil
}

// fakeSessions is an in-memory Sessions mirroring session.Service: Issue enforces
// single-device, Rotate is single-use, Revoke/RevokeAll are idempotent.
type fakeSessions struct {
	mu     sync.Mutex
	active map[string]uuid.UUID
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{active: make(map[string]uuid.UUID)}
}

func (f *fakeSessions) Issue(_ context.Context, adminID uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for raw, id := range f.active {
		if id == adminID {
			delete(f.active, raw)
		}
	}
	raw := uuid.NewString()
	f.active[raw] = adminID
	return raw, nil
}

func (f *fakeSessions) Rotate(_ context.Context, raw string) (uuid.UUID, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.active[raw]
	if !ok {
		return uuid.Nil, "", session.ErrInvalidRefreshToken
	}
	delete(f.active, raw)
	newRaw := uuid.NewString()
	f.active[newRaw] = id
	return id, newRaw, nil
}

func (f *fakeSessions) Revoke(_ context.Context, raw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, raw)
	return nil
}

func (f *fakeSessions) RevokeAll(_ context.Context, adminID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for raw, id := range f.active {
		if id == adminID {
			delete(f.active, raw)
		}
	}
	return nil
}
