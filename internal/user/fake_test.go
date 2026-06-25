package user

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// fakeStore is an in-memory Store used to unit-test the Service and Handler
// without a database. It mirrors the real repository's contract: case-insensitive
// unique email, ErrEmailTaken on conflict, ErrNotFound on misses.
type fakeStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]*User
	createFn func(*User) error // optional overrides to force errors
	getEmail func(string) (*User, error)
	getID    func(uuid.UUID) (*User, error)
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: make(map[uuid.UUID]*User)}
}

func (f *fakeStore) Create(_ context.Context, u *User) error {
	if f.createFn != nil {
		return f.createFn(u)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if strings.EqualFold(existing.Email, u.Email) {
			return ErrEmailTaken
		}
	}
	// copy to avoid callers mutating stored state
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}

func (f *fakeStore) GetByEmail(_ context.Context, email string) (*User, error) {
	if f.getEmail != nil {
		return f.getEmail(email)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if strings.EqualFold(u.Email, email) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) GetByID(_ context.Context, id uuid.UUID) (*User, error) {
	if f.getID != nil {
		return f.getID(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *u
	return &cp, nil
}
