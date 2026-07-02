package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/session"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidLevel       = errors.New("invalid level")
	ErrInvalidStatus      = errors.New("invalid status")
	// ErrLastSuperAdmin guards against disabling the only active super_admin,
	// which would leave the back office with no one able to manage accounts.
	// Returned by Store.SetStatus, where the check is atomic with the update.
	ErrLastSuperAdmin = errors.New("cannot disable the last active super admin")
	// ErrAccountDisabled is returned when a disabled admin tries to log in. Kept
	// distinct from ErrInvalidCredentials so the handler can surface a 403.
	ErrAccountDisabled = errors.New("account disabled")
)

// Store is the persistence behaviour the Service depends on. *Repository
// satisfies it in production; tests use an in-memory fake.
type Store interface {
	Create(ctx context.Context, a *Admin) error
	GetByID(ctx context.Context, id uuid.UUID) (*Admin, error)
	GetByPhone(ctx context.Context, phone string) (*Admin, error)
	GetByEmail(ctx context.Context, email string) (*Admin, error)
	List(ctx context.Context, f ListFilter) ([]Admin, int64, error)
	SetStatus(ctx context.Context, id uuid.UUID, s Status) error
	SetLevel(ctx context.Context, id uuid.UUID, l Level) error
}

// Sessions is the refresh-token behaviour the Service depends on. A
// session.Service backed by the admin_refresh_tokens store satisfies it.
type Sessions interface {
	Issue(ctx context.Context, adminID uuid.UUID) (string, error)
	Rotate(ctx context.Context, rawRefreshToken string) (uuid.UUID, string, error)
	Revoke(ctx context.Context, rawRefreshToken string) error
	RevokeAll(ctx context.Context, adminID uuid.UUID) error
}

// Service holds the back-office identity business logic.
type Service struct {
	repo     Store
	token    *auth.TokenManager // admin realm
	sessions Sessions
}

func NewService(repo Store, token *auth.TokenManager, sessions Sessions) *Service {
	return &Service{repo: repo, token: token, sessions: sessions}
}

// Login authenticates an admin by identifier (phone or email) + password and
// issues an admin-realm access token plus a refresh token. Admin has no
// verification-code login — password only.
func (s *Service) Login(ctx context.Context, identifier, password string) (*Admin, string, string, error) {
	a, err := s.lookupByIdentifier(ctx, identifier)
	if errors.Is(err, ErrNotFound) {
		return nil, "", "", ErrInvalidCredentials
	}
	if err != nil {
		return nil, "", "", err
	}

	// Constant-time compare; the same generic error whether identifier or
	// password was wrong, so the response can't be used to probe which admins exist.
	if err := bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(password)); err != nil {
		return nil, "", "", ErrInvalidCredentials
	}
	// Checked after the password compare so a wrong password never reveals an
	// account's disabled state.
	if a.Status == StatusDisabled {
		return nil, "", "", ErrAccountDisabled
	}

	return s.issue(ctx, a)
}

// Refresh rotates a refresh token and mints a fresh admin access token for its
// owner. A disabled admin is rejected (mapped to session.ErrInvalidRefreshToken),
// so a disable is effective within one access-token TTL.
func (s *Service) Refresh(ctx context.Context, rawRefreshToken string) (accessToken, refreshToken string, err error) {
	adminID, newRefresh, err := s.sessions.Rotate(ctx, rawRefreshToken)
	if err != nil {
		return "", "", err // session.ErrInvalidRefreshToken for any invalid input
	}
	a, err := s.repo.GetByID(ctx, adminID)
	if err != nil {
		return "", "", err
	}
	if a.Status == StatusDisabled {
		return "", "", session.ErrInvalidRefreshToken
	}
	access, err := s.token.Generate(a.ID, string(a.Level))
	if err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	return access, newRefresh, nil
}

// Logout revokes a single refresh token (idempotent).
func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	return s.sessions.Revoke(ctx, rawRefreshToken)
}

// LogoutAll revokes every refresh token an admin holds (logout everywhere).
func (s *Service) LogoutAll(ctx context.Context, adminID uuid.UUID) error {
	return s.sessions.RevokeAll(ctx, adminID)
}

// GetByID loads an admin (used by the profile probe).
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*Admin, error) {
	return s.repo.GetByID(ctx, id)
}

// Create provisions a new admin account. This is the super-admin-only path for
// growing the back office; the handler gates it on level. level defaults to
// LevelAdmin when empty. Returns ErrPhoneTaken/ErrEmailTaken on conflict.
func (s *Service) Create(ctx context.Context, phone, email, password, displayName string, level Level) (*Admin, error) {
	if level == "" {
		level = LevelAdmin
	}
	if !level.Valid() {
		return nil, ErrInvalidLevel
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	a := &Admin{
		ID:           uuid.New(),
		Phone:        normalizePhone(phone),
		Email:        normalizeEmail(email),
		PasswordHash: string(hash),
		DisplayName:  strings.TrimSpace(displayName),
		Level:        level,
	}
	if err := s.repo.Create(ctx, a); err != nil {
		return nil, err // includes ErrPhoneTaken / ErrEmailTaken
	}
	return a, nil
}

// List returns a page of admins and the total matching count.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Admin, int64, error) {
	return s.repo.List(ctx, f)
}

// SetStatus enables or disables an admin. Disabling takes effect within one
// access-token TTL (login and refresh both reject a disabled account). The
// store refuses to disable the last active super_admin (ErrLastSuperAdmin);
// that guard is enforced atomically there so concurrent disables can't race
// past a check-then-write window.
func (s *Service) SetStatus(ctx context.Context, id uuid.UUID, status Status) error {
	if status != StatusActive && status != StatusDisabled {
		return ErrInvalidStatus
	}
	return s.repo.SetStatus(ctx, id, status)
}

// SeedSuperAdmin ensures an active super_admin exists for the given phone,
// idempotently. It is the out-of-band bootstrap for the first back-office account
// (see cmd/seed):
//   - phone unknown → create an active super_admin;
//   - phone known → self-heal: promote to super_admin and re-activate if needed,
//     so seeding always yields a usable super_admin even over a pre-existing plain
//     or disabled account. The password is left untouched on an existing account.
//
// Re-running never duplicates or errors.
func (s *Service) SeedSuperAdmin(ctx context.Context, phone, password, displayName string) (*Admin, error) {
	phone = normalizePhone(phone)
	existing, err := s.repo.GetByPhone(ctx, phone)
	switch {
	case errors.Is(err, ErrNotFound):
		return s.Create(ctx, phone, "", password, displayName, LevelSuperAdmin)
	case err != nil:
		return nil, err
	}

	if existing.Level != LevelSuperAdmin {
		if err := s.repo.SetLevel(ctx, existing.ID, LevelSuperAdmin); err != nil {
			return nil, err
		}
		existing.Level = LevelSuperAdmin
	}
	if existing.Status != StatusActive {
		if err := s.repo.SetStatus(ctx, existing.ID, StatusActive); err != nil {
			return nil, err
		}
		existing.Status = StatusActive
	}
	return existing, nil
}

// issue returns the admin together with a fresh access + refresh token pair.
// Minting the refresh token enforces single-device by revoking the admin's other
// sessions.
func (s *Service) issue(ctx context.Context, a *Admin) (*Admin, string, string, error) {
	access, err := s.token.Generate(a.ID, string(a.Level))
	if err != nil {
		return nil, "", "", fmt.Errorf("generate token: %w", err)
	}
	refresh, err := s.sessions.Issue(ctx, a.ID)
	if err != nil {
		return nil, "", "", fmt.Errorf("issue refresh token: %w", err)
	}
	return a, access, refresh, nil
}

func (s *Service) lookupByIdentifier(ctx context.Context, identifier string) (*Admin, error) {
	if isEmail(identifier) {
		return s.repo.GetByEmail(ctx, normalizeEmail(identifier))
	}
	return s.repo.GetByPhone(ctx, normalizePhone(identifier))
}

func isEmail(s string) bool { return strings.Contains(s, "@") }

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func normalizePhone(phone string) string {
	return strings.TrimSpace(phone)
}
