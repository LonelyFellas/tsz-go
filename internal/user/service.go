package user

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
	ErrInvalidCredentials      = errors.New("invalid credentials")
	ErrInvalidRole             = errors.New("invalid role")
	ErrRoleNotOwned            = errors.New("user does not have this role")
	ErrInvalidLearningSettings = errors.New("invalid learning settings")
	// ErrAccountDisabled is returned when a disabled account tries to log in or
	// refresh. It is distinct from ErrInvalidCredentials (the credentials are
	// valid; the account is locked) so the handler can surface a 403.
	ErrAccountDisabled = errors.New("account disabled")
)

// codePurposeLogin is the verification-code purpose for code-based login.
const codePurposeLogin = "login"

// Store is the persistence behavior the Service depends on. Defining it as an
// interface lets the service be unit-tested with an in-memory fake, while the
// concrete *Repository satisfies it in production.
type Store interface {
	Create(ctx context.Context, u *User, role Role) error
	CreateAdmin(ctx context.Context, u *User) error
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByPhone(ctx context.Context, phone string) (*User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	HasRole(ctx context.Context, userID uuid.UUID, role Role) (bool, error)
	AddRole(ctx context.Context, userID uuid.UUID, role Role) error
	AddAdminRole(ctx context.Context, userID uuid.UUID) error
	SetActiveRole(ctx context.Context, userID uuid.UUID, role Role) error
	GetLearningSettings(ctx context.Context, userID uuid.UUID) (*LearningSettings, error)
	SetLearningSettings(ctx context.Context, userID uuid.UUID, s *LearningSettings) error
}

// Codes is the verification-code behavior the Service depends on for code-based
// login. The otp.Service satisfies it; tests use a fake.
type Codes interface {
	RequestCode(ctx context.Context, target, purpose string) error
	Verify(ctx context.Context, target, purpose, code string) error
}

// Sessions is the refresh-token behavior the Service depends on. The
// session.Service satisfies it; tests use a fake. Issue mints a refresh token
// for a fresh login (and enforces single-device by revoking the user's others);
// Rotate exchanges a valid refresh token for a new one and the owning user;
// Revoke invalidates one (logout); RevokeAll invalidates every token a user
// holds (logout everywhere).
type Sessions interface {
	Issue(ctx context.Context, userID uuid.UUID) (string, error)
	Rotate(ctx context.Context, rawRefreshToken string) (uuid.UUID, string, error)
	Revoke(ctx context.Context, rawRefreshToken string) error
	RevokeAll(ctx context.Context, userID uuid.UUID) error
}

// Service holds the user business logic.
type Service struct {
	repo     Store
	token    *auth.TokenManager
	codes    Codes
	sessions Sessions
}

func NewService(repo Store, token *auth.TokenManager, codes Codes, sessions Sessions) *Service {
	return &Service{repo: repo, token: token, codes: codes, sessions: sessions}
}

// Register creates an account. Phone is the required identifier; email is
// optional. The initial identity is role, and the returned tokens are already
// scoped to that role as the active one.
func (s *Service) Register(ctx context.Context, phone, email, password, displayName string, role Role) (*User, string, string, error) {
	if !role.Valid() {
		return nil, "", "", ErrInvalidRole
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", "", fmt.Errorf("hash password: %w", err)
	}

	u := &User{
		ID:           uuid.New(),
		Phone:        normalizePhone(phone),
		Email:        normalizeEmail(email),
		PasswordHash: string(hash),
		DisplayName:  strings.TrimSpace(displayName),
	}
	if err := s.repo.Create(ctx, u, role); err != nil {
		return nil, "", "", err // includes ErrPhoneTaken / ErrEmailTaken
	}

	return s.issue(ctx, u)
}

// LoginPassword authenticates with an identifier (phone or email) and a password.
func (s *Service) LoginPassword(ctx context.Context, identifier, password string) (*User, string, string, error) {
	u, err := s.lookupByIdentifier(ctx, identifier)
	if errors.Is(err, ErrNotFound) {
		return nil, "", "", ErrInvalidCredentials
	}
	if err != nil {
		return nil, "", "", err
	}

	// Constant-time comparison; same generic error whether the identifier or the
	// password was wrong, to avoid leaking which accounts exist.
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, "", "", ErrInvalidCredentials
	}

	// Credentials check out, but a disabled account may not log in. Checked after
	// the password compare so a wrong password still returns the generic
	// credentials error and never reveals an account's disabled state.
	if u.Status == StatusDisabled {
		return nil, "", "", ErrAccountDisabled
	}

	return s.issue(ctx, u)
}

// RequestLoginCode sends a one-time login code to the identifier (phone → SMS,
// email → email). It does not reveal whether the identifier is registered.
func (s *Service) RequestLoginCode(ctx context.Context, identifier string) error {
	return s.codes.RequestCode(ctx, normalizeIdentifier(identifier), codePurposeLogin)
}

// LoginCode authenticates with an identifier (phone or email) and a one-time
// code previously sent via RequestLoginCode.
func (s *Service) LoginCode(ctx context.Context, identifier, code string) (*User, string, string, error) {
	u, err := s.lookupByIdentifier(ctx, identifier)
	if errors.Is(err, ErrNotFound) {
		return nil, "", "", ErrInvalidCredentials
	}
	if err != nil {
		return nil, "", "", err
	}

	if err := s.codes.Verify(ctx, normalizeIdentifier(identifier), codePurposeLogin, code); err != nil {
		return nil, "", "", ErrInvalidCredentials
	}

	// A valid code still can't unlock a disabled account.
	if u.Status == StatusDisabled {
		return nil, "", "", ErrAccountDisabled
	}

	return s.issue(ctx, u)
}

// Refresh rotates a refresh token and mints a fresh access token for its owner.
// The new access token resumes the user's last active role (the refresh token
// itself carries no role), so a prior switch-role survives token expiry. A
// revoked/expired/unknown refresh token
// — including one revoked because the user logged in on another device — surfaces
// as session.ErrInvalidRefreshToken.
func (s *Service) Refresh(ctx context.Context, rawRefreshToken string) (accessToken, refreshToken string, err error) {
	userID, newRefresh, err := s.sessions.Rotate(ctx, rawRefreshToken)
	if err != nil {
		return "", "", err // session.ErrInvalidRefreshToken for any invalid input
	}

	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	// A disabled account cannot refresh: surface the same invalid-refresh-token
	// error so the handler clears the cookie and returns 401. This makes a disable
	// effective within one access-token TTL even for already-issued refresh tokens.
	if u.Status == StatusDisabled {
		return "", "", session.ErrInvalidRefreshToken
	}
	access, err := s.token.Generate(userID, string(activeRole(u)))
	if err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	return access, newRefresh, nil
}

// Logout revokes a refresh token. It is idempotent (see session.Service.Revoke).
func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	return s.sessions.Revoke(ctx, rawRefreshToken)
}

// LogoutAll revokes every refresh token the user holds (logout everywhere).
// Driven by the authenticated user's ID rather than a presented refresh token,
// so a user can sign out other devices using only their access token. Idempotent
// (see session.Service.RevokeAll).
func (s *Service) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	return s.sessions.RevokeAll(ctx, userID)
}

// SwitchRole re-issues a token with a different active role. The user must
// already hold that role (acquire a new one via AddRole first).
func (s *Service) SwitchRole(ctx context.Context, userID uuid.UUID, role Role) (string, error) {
	if !role.Valid() {
		return "", ErrInvalidRole
	}
	has, err := s.repo.HasRole(ctx, userID, role)
	if err != nil {
		return "", err
	}
	if !has {
		return "", ErrRoleNotOwned
	}

	// Persist the new active role so it survives a token refresh, not just the
	// lifetime of this access token.
	if err := s.repo.SetActiveRole(ctx, userID, role); err != nil {
		return "", err
	}

	tok, err := s.token.Generate(userID, string(role))
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return tok, nil
}

// AddRole grants the user an additional identity and returns a token already
// switched to it. Returns ErrRoleTaken if they already hold the role.
func (s *Service) AddRole(ctx context.Context, userID uuid.UUID, role Role) (string, error) {
	if !role.Valid() {
		return "", ErrInvalidRole
	}
	if err := s.repo.AddRole(ctx, userID, role); err != nil {
		return "", err // includes ErrRoleTaken
	}

	// The new role becomes the active one; persist it so a refresh keeps it.
	if err := s.repo.SetActiveRole(ctx, userID, role); err != nil {
		return "", err
	}

	tok, err := s.token.Generate(userID, string(role))
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return tok, nil
}

// SeedAdmin ensures an admin account exists for the given phone, idempotently. It
// is the out-of-band bootstrap for the first back-office user (see cmd/seed) —
// admin is never self-registered. Behavior:
//
//   - phone unknown → create the account with the admin role.
//   - phone known, lacks admin → grant the admin role (password/display name of an
//     existing account are left untouched).
//   - phone known, already admin → no-op.
//
// Re-running is safe: it never duplicates an account and never errors on "already
// an admin". The returned user reflects the resulting account.
func (s *Service) SeedAdmin(ctx context.Context, phone, password, displayName string) (*User, error) {
	phone = normalizePhone(phone)

	existing, err := s.repo.GetByPhone(ctx, phone)
	switch {
	case err == nil:
		// Account exists; grant admin if it is missing, otherwise nothing to do.
		has, err := s.repo.HasRole(ctx, existing.ID, RoleAdmin)
		if err != nil {
			return nil, err
		}
		if !has {
			if err := s.repo.AddAdminRole(ctx, existing.ID); err != nil {
				return nil, err
			}
		}
		return s.repo.GetByID(ctx, existing.ID)
	case errors.Is(err, ErrNotFound):
		// No such account; create it fresh with the admin role.
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		u := &User{
			ID:           uuid.New(),
			Phone:        phone,
			PasswordHash: string(hash),
			DisplayName:  strings.TrimSpace(displayName),
		}
		if err := s.repo.CreateAdmin(ctx, u); err != nil {
			return nil, err
		}
		return u, nil
	default:
		return nil, err
	}
}

func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.GetByID(ctx, id)
}

// GetLearningSettings returns the learner's onboarding choices, or nil if they
// have not finished onboarding. The handler maps nil to "onboarded": false.
func (s *Service) GetLearningSettings(ctx context.Context, userID uuid.UUID) (*LearningSettings, error) {
	return s.repo.GetLearningSettings(ctx, userID)
}

// SetLearningSettings validates and persists the learner's CEFR level and English
// variant (both required, written together). It powers new-user onboarding and
// later edits from the settings screen. Returns ErrInvalidLearningSettings for an
// unknown level/variant and propagates ErrNoStudentProfile from the repository.
func (s *Service) SetLearningSettings(ctx context.Context, userID uuid.UUID, level CEFRLevel, variant EnglishVariant) (*LearningSettings, error) {
	if !level.Valid() || !variant.Valid() {
		return nil, ErrInvalidLearningSettings
	}
	ls := &LearningSettings{CEFRLevel: level, EnglishVariant: variant}
	if err := s.repo.SetLearningSettings(ctx, userID, ls); err != nil {
		return nil, err
	}
	return ls, nil
}

// issue returns the user together with a fresh access + refresh token pair. The
// access token resumes the user's last active role (falling back to their first
// role); minting the refresh token enforces single-device by revoking the user's
// other sessions.
func (s *Service) issue(ctx context.Context, u *User) (*User, string, string, error) {
	access, err := s.token.Generate(u.ID, string(activeRole(u)))
	if err != nil {
		return nil, "", "", fmt.Errorf("generate token: %w", err)
	}
	refresh, err := s.sessions.Issue(ctx, u.ID)
	if err != nil {
		return nil, "", "", fmt.Errorf("issue refresh token: %w", err)
	}
	return u, access, refresh, nil
}

// lookupByIdentifier resolves a user by email (if the identifier looks like an
// email) or by phone otherwise.
func (s *Service) lookupByIdentifier(ctx context.Context, identifier string) (*User, error) {
	if isEmail(identifier) {
		return s.repo.GetByEmail(ctx, normalizeEmail(identifier))
	}
	return s.repo.GetByPhone(ctx, normalizePhone(identifier))
}

// activeRole is the role a freshly issued token should act as: the user's last
// active role if it's still one they hold, otherwise their default role. This is
// what lets a switch-role survive a token refresh.
func activeRole(u *User) Role {
	for _, r := range u.Roles {
		if r == u.LastActiveRole {
			return u.LastActiveRole
		}
	}
	return defaultRole(u.Roles)
}

// defaultRole picks the fallback active role for a user with no recorded active
// role. Roles are loaded in a stable order, so this is deterministic.
func defaultRole(roles []Role) Role {
	if len(roles) == 0 {
		return ""
	}
	return roles[0]
}

func isEmail(s string) bool { return strings.Contains(s, "@") }

// normalizeIdentifier normalizes an identifier as either an email or a phone.
func normalizeIdentifier(identifier string) string {
	if isEmail(identifier) {
		return normalizeEmail(identifier)
	}
	return normalizePhone(identifier)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func normalizePhone(phone string) string {
	return strings.TrimSpace(phone)
}
