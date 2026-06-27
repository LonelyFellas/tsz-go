package user

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/session"
)

// fakeStore is an in-memory Store used to unit-test the Service and Handler
// without a database. It mirrors the real repository's contract: unique phone,
// case-insensitive unique email, ErrPhoneTaken/ErrEmailTaken on conflict,
// ErrNotFound on misses, and per-user role membership.
type fakeStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]*User
	roles    map[uuid.UUID]map[Role]bool
	settings map[uuid.UUID]*LearningSettings
	createFn func(*User, Role) error // optional overrides to force errors
	getEmail func(string) (*User, error)
	getPhone func(string) (*User, error)
	getID    func(uuid.UUID) (*User, error)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byID:     make(map[uuid.UUID]*User),
		roles:    make(map[uuid.UUID]map[Role]bool),
		settings: make(map[uuid.UUID]*LearningSettings),
	}
}

func (f *fakeStore) Create(_ context.Context, u *User, role Role) error {
	if f.createFn != nil {
		return f.createFn(u, role)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if existing.Phone == u.Phone {
			return ErrPhoneTaken
		}
		if u.Email != "" && strings.EqualFold(existing.Email, u.Email) {
			return ErrEmailTaken
		}
	}
	u.Roles = []Role{role}
	u.LastActiveRole = role
	if u.Status == "" {
		u.Status = StatusActive // mirror the DB column default
	}
	// copy to avoid callers mutating stored state
	cp := *u
	f.byID[u.ID] = &cp
	f.roles[u.ID] = map[Role]bool{role: true}
	return nil
}

func (f *fakeStore) SetActiveRole(_ context.Context, userID uuid.UUID, role Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.byID[userID]; u != nil {
		u.LastActiveRole = role
	}
	return nil
}

func (f *fakeStore) AddRole(_ context.Context, userID uuid.UUID, role Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roles[userID][role] {
		return ErrRoleTaken
	}
	if f.roles[userID] == nil {
		f.roles[userID] = make(map[Role]bool)
	}
	f.roles[userID][role] = true
	if u := f.byID[userID]; u != nil {
		u.Roles = append(u.Roles, role)
	}
	return nil
}

func (f *fakeStore) HasRole(_ context.Context, userID uuid.UUID, role Role) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roles[userID][role], nil
}

func (f *fakeStore) GetByEmail(_ context.Context, email string) (*User, error) {
	if f.getEmail != nil {
		return f.getEmail(email)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if u.Email != "" && strings.EqualFold(u.Email, email) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) GetByPhone(_ context.Context, phone string) (*User, error) {
	if f.getPhone != nil {
		return f.getPhone(phone)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if u.Phone == phone {
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

func (f *fakeStore) GetLearningSettings(_ context.Context, userID uuid.UUID) (*LearningSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.settings[userID]; s != nil {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeStore) SetLearningSettings(_ context.Context, userID uuid.UUID, s *LearningSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirror the real repo: settings hang off the student profile, so a user who
	// is not a student has nowhere to store them.
	if !f.roles[userID][RoleStudent] {
		return ErrNoStudentProfile
	}
	cp := *s
	f.settings[userID] = &cp
	return nil
}

func (f *fakeStore) SetPassword(_ context.Context, userID uuid.UUID, passwordHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[userID]
	if !ok {
		return ErrNotFound
	}
	u.PasswordHash = passwordHash
	return nil
}

func (f *fakeStore) Delete(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[userID]; !ok {
		return ErrNotFound
	}
	// Mirror the DB's ON DELETE CASCADE: the user and everything hanging off them
	// goes away together.
	delete(f.byID, userID)
	delete(f.roles, userID)
	delete(f.settings, userID)
	return nil
}

// fakeCodes is an in-memory Codes used to unit-test code-based login. RequestCode
// records the "sent" code per target; Verify checks it and (on success) clears it.
type fakeCodes struct {
	mu       sync.Mutex
	codes    map[string]string // target -> code
	verifyFn func(target, purpose, code string) error
	reqFn    func(target, purpose string) error
}

func newFakeCodes() *fakeCodes {
	return &fakeCodes{codes: make(map[string]string)}
}

func (f *fakeCodes) RequestCode(_ context.Context, target, purpose string) error {
	if f.reqFn != nil {
		return f.reqFn(target, purpose)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[target] = "123456" // deterministic for tests
	return nil
}

func (f *fakeCodes) Verify(_ context.Context, target, purpose, code string) error {
	if f.verifyFn != nil {
		return f.verifyFn(target, purpose, code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want, ok := f.codes[target]; !ok || want != code {
		return ErrInvalidCredentials
	}
	delete(f.codes, target)
	return nil
}

// fakeSessions is an in-memory Sessions used to unit-test the Service and
// Handler. It mirrors the real session.Service contract: Issue enforces strict
// single-device (revokes the user's other tokens), Rotate is single-use, and
// Revoke is idempotent. Invalid tokens map to session.ErrInvalidRefreshToken.
type fakeSessions struct {
	mu       sync.Mutex
	active   map[string]uuid.UUID // raw token -> owning user
	issueErr error                // optional override to force Issue to fail
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{active: make(map[string]uuid.UUID)}
}

func (f *fakeSessions) Issue(_ context.Context, userID uuid.UUID) (string, error) {
	if f.issueErr != nil {
		return "", f.issueErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// strict single-device: revoke the user's other tokens
	for raw, uid := range f.active {
		if uid == userID {
			delete(f.active, raw)
		}
	}
	raw := uuid.NewString()
	f.active[raw] = userID
	return raw, nil
}

func (f *fakeSessions) Rotate(_ context.Context, raw string) (uuid.UUID, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, ok := f.active[raw]
	if !ok {
		return uuid.Nil, "", session.ErrInvalidRefreshToken
	}
	delete(f.active, raw)
	newRaw := uuid.NewString()
	f.active[newRaw] = uid
	return uid, newRaw, nil
}

func (f *fakeSessions) Revoke(_ context.Context, raw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, raw)
	return nil
}

func (f *fakeSessions) RevokeAll(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for raw, uid := range f.active {
		if uid == userID {
			delete(f.active, raw)
		}
	}
	return nil
}
