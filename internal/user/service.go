package user

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/session"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidRole        = errors.New("invalid role")
	// ErrMissingIdentifier is returned by Register when neither a phone nor an
	// email is supplied. An account must be reachable by at least one identifier
	// (the database enforces the same via the users_phone_or_email_present CHECK).
	ErrMissingIdentifier       = errors.New("phone or email is required")
	ErrRoleNotOwned            = errors.New("user does not have this role")
	ErrInvalidLearningSettings = errors.New("invalid learning settings")
	// ErrAccountDisabled is returned when a disabled account tries to log in or
	// refresh. It is distinct from ErrInvalidCredentials (the credentials are
	// valid; the account is locked) so the handler can surface a 403.
	ErrAccountDisabled = errors.New("account disabled")
	// ErrInvalidResetCode is returned by ResetPassword when the submitted reset
	// code is wrong, expired, already used, or never issued. It is deliberately
	// undifferentiated (and also covers "no account for this phone") so the flow
	// never reveals which phone numbers are registered.
	ErrInvalidResetCode = errors.New("invalid or expired reset code")
	// ErrInvalidDeletionCode is returned by DeleteAccount when the submitted
	// deletion code is wrong, expired, already used, or never issued. Like the
	// reset-code error it stays undifferentiated so a failed confirmation reveals
	// nothing beyond "that code won't delete the account".
	ErrInvalidDeletionCode = errors.New("invalid or expired deletion code")
	// ErrChannelUnavailable is returned when the caller asks to verify over a
	// channel their account has no contact for — e.g. the email channel for an
	// account with no email on file. The frontend should simply not offer that
	// tab (see the deletion screen's phone/email toggle).
	ErrChannelUnavailable = errors.New("verification channel unavailable for this account")
	// ErrInvalidChannel is returned for a deletion channel that is neither phone
	// nor email. The handler validates this first, so it is a defensive guard for
	// direct service callers (tests).
	ErrInvalidChannel = errors.New("invalid verification channel")
	// ErrInvalidDisplayName is returned by UpdateDisplayName when the new name is
	// blank after trimming (whitespace-only). The handler's binding enforces the
	// 1–50 length on the raw value; this catches the trim-to-empty case it can't.
	ErrInvalidDisplayName = errors.New("display name cannot be blank")
	// ErrInvalidContact is returned when the value to bind is neither a valid email
	// nor a plausible phone number. The contact's shape (an "@" → email) decides
	// which it is, so a single field can carry either.
	ErrInvalidContact = errors.New("invalid contact")
	// ErrInvalidBindCode is returned by BindContact when the submitted code is
	// wrong, expired, already used, or never issued. Like the other code errors it
	// stays undifferentiated.
	ErrInvalidBindCode = errors.New("invalid or expired verification code")
)

// Contact channels a bind can target. Unlike the deletion channels (which the
// client picks), the bind channel is derived from the contact value's shape, so
// these are internal: the service classifies, the repository writes the matching
// column.
const (
	ContactChannelPhone = "phone"
	ContactChannelEmail = "email"
)

// Verification channels a self-service account deletion can be confirmed over.
// The deletion screen shows them as the phone/email tabs; the code is always
// sent to the account's own contact on file, never to a value the caller types.
const (
	DeletionChannelPhone = "phone"
	DeletionChannelEmail = "email"
)

// Verification-code purposes. Each value is also constrained in the
// verification_codes.purpose CHECK (see the migrations), so adding one here
// means adding it there too.
const (
	codePurposeLogin           = "login"
	codePurposePasswordReset   = "password_reset"
	codePurposeAccountDeletion = "account_deletion"
	codePurposeContactBind     = "contact_bind"
)

// Store is the persistence behavior the Service depends on. Defining it as an
// interface lets the service be unit-tested with an in-memory fake, while the
// concrete *Repository satisfies it in production.
type Store interface {
	Create(ctx context.Context, u *User, role Role) error
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByPhone(ctx context.Context, phone string) (*User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	HasRole(ctx context.Context, userID uuid.UUID, role Role) (bool, error)
	AddRole(ctx context.Context, userID uuid.UUID, role Role) error
	SetActiveRole(ctx context.Context, userID uuid.UUID, role Role) error
	GetLearningSettings(ctx context.Context, userID uuid.UUID) (*LearningSettings, error)
	SetLearningSettings(ctx context.Context, userID uuid.UUID, s *LearningSettings) error
	SetPassword(ctx context.Context, userID uuid.UUID, passwordHash string) error
	// SetDisplayName overwrites a user's display name. Returns ErrNotFound if no
	// user has the given ID.
	SetDisplayName(ctx context.Context, userID uuid.UUID, displayName string) error
	// SetContact writes a phone or email (chosen by channel) onto the user. It
	// returns ErrPhoneTaken/ErrEmailTaken if the value already belongs to another
	// account (the partial unique indexes), and ErrNotFound if no user has the ID.
	SetContact(ctx context.Context, userID uuid.UUID, channel, value string) error
	// Delete removes a user and (via ON DELETE CASCADE) every row that hangs off
	// them: roles, role profiles, refresh tokens. Returns ErrNotFound if no user
	// has the given ID.
	Delete(ctx context.Context, userID uuid.UUID) error
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

// Register creates an account identified by a phone, an email, or both — at
// least one is required. The initial identity is role, and the returned tokens
// are already scoped to that role as the active one.
func (s *Service) Register(ctx context.Context, phone, email, password, displayName string, role Role) (*User, string, string, error) {
	if !role.Valid() {
		return nil, "", "", ErrInvalidRole
	}

	phone, email = normalizePhone(phone), normalizeEmail(email)
	if phone == "" && email == "" {
		return nil, "", "", ErrMissingIdentifier
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", "", fmt.Errorf("hash password: %w", err)
	}

	u := &User{
		ID:           uuid.New(),
		Phone:        phone,
		Email:        email,
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

// RequestPasswordReset sends a one-time reset code to the identifier (phone →
// SMS, email → email). Like RequestLoginCode it does not check whether the
// identifier is registered, so it cannot be used to probe which accounts exist;
// an unregistered identifier simply receives a code that will never resolve to an
// account at ResetPassword time.
func (s *Service) RequestPasswordReset(ctx context.Context, identifier string) error {
	return s.codes.RequestCode(ctx, normalizeIdentifier(identifier), codePurposePasswordReset)
}

// ResetPassword completes the forgot-password flow: it verifies the reset code
// previously sent to the identifier, then sets a new password and revokes every
// existing session so any attacker holding old tokens is locked out. A wrong/
// expired code OR an unregistered identifier both surface as ErrInvalidResetCode,
// so the flow never reveals which identifiers are registered. A disabled account
// is rejected after the code check (mirroring code login) and cannot reset its
// way back in.
func (s *Service) ResetPassword(ctx context.Context, identifier, code, newPassword string) error {
	target := normalizeIdentifier(identifier)

	if err := s.codes.Verify(ctx, target, codePurposePasswordReset, code); err != nil {
		return ErrInvalidResetCode
	}

	u, err := s.lookupByIdentifier(ctx, identifier)
	if errors.Is(err, ErrNotFound) {
		// Valid code but no account: the code was just consumed, so this can't be
		// replayed. Stay generic so existence isn't leaked.
		return ErrInvalidResetCode
	}
	if err != nil {
		return err
	}
	if u.Status == StatusDisabled {
		return ErrAccountDisabled
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.repo.SetPassword(ctx, u.ID, string(hash)); err != nil {
		return err
	}

	// Invalidate all outstanding sessions: a password reset should sign the user
	// out everywhere, including any device an attacker may have been using.
	if err := s.sessions.RevokeAll(ctx, u.ID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return nil
}

// RequestAccountDeletion sends a one-time confirmation code to the account's own
// contact on the chosen channel (phone → SMS, email → email). The caller is the
// authenticated account owner, so the code always goes to the contact already on
// file — never to a value supplied in the request — which is what makes the code
// proof of ownership. Asking to verify over a channel the account has no contact
// for (e.g. email on a phone-only account) returns ErrChannelUnavailable.
func (s *Service) RequestAccountDeletion(ctx context.Context, userID uuid.UUID, channel string) error {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err // includes ErrNotFound
	}
	target, err := deletionTarget(u, channel)
	if err != nil {
		return err
	}
	return s.codes.RequestCode(ctx, target, codePurposeAccountDeletion)
}

// DeleteAccount permanently removes the authenticated user's account after
// verifying the confirmation code sent to the chosen channel. It first revokes
// every session (so the user is signed out everywhere immediately) and then
// deletes the account; the delete also cascades to every owned row — roles,
// profiles, and any refresh tokens the revoke did not already cover. A wrong/
// expired code surfaces as ErrInvalidDeletionCode; an already-deleted account
// (stale token) surfaces as ErrNotFound.
func (s *Service) DeleteAccount(ctx context.Context, userID uuid.UUID, channel, code string) error {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err // includes ErrNotFound
	}
	target, err := deletionTarget(u, channel)
	if err != nil {
		return err
	}

	if err := s.codes.Verify(ctx, target, codePurposeAccountDeletion, code); err != nil {
		return ErrInvalidDeletionCode
	}

	// Revoke sessions explicitly rather than leaning on the row cascade alone, so
	// "delete signs you out everywhere" holds even if the FK action ever changes.
	if err := s.sessions.RevokeAll(ctx, u.ID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return s.repo.Delete(ctx, u.ID)
}

// deletionTarget resolves the contact a deletion code is sent to / verified
// against for the chosen channel. An account may now have only one of phone/
// email, so either channel can be unavailable: asking to verify over a channel
// the account has no contact for returns ErrChannelUnavailable (the frontend
// should not offer that tab).
func deletionTarget(u *User, channel string) (string, error) {
	switch channel {
	case DeletionChannelPhone:
		if u.Phone == "" {
			return "", ErrChannelUnavailable
		}
		return u.Phone, nil
	case DeletionChannelEmail:
		if u.Email == "" {
			return "", ErrChannelUnavailable
		}
		return u.Email, nil
	default:
		return "", ErrInvalidChannel
	}
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

// UpdateDisplayName changes the authenticated user's display name (the "昵称" on
// the edit-profile screen). The name is trimmed; a blank result is rejected with
// ErrInvalidDisplayName. Returns the refreshed user on success, or ErrNotFound if
// the account no longer exists (stale token).
func (s *Service) UpdateDisplayName(ctx context.Context, userID uuid.UUID, displayName string) (*User, error) {
	name := strings.TrimSpace(displayName)
	if name == "" {
		return nil, ErrInvalidDisplayName
	}
	if err := s.repo.SetDisplayName(ctx, userID, name); err != nil {
		return nil, err // includes ErrNotFound
	}
	return s.repo.GetByID(ctx, userID)
}

// RequestContactBindCode sends a one-time code to a NEW contact the user wants to
// attach (phone → SMS, email → email). Unlike the login/reset/deletion codes —
// which go to a contact already on file — this code goes to the value in the
// request, so verifying it later proves the user controls that new contact. The
// contact is validated and checked for availability first, so a malformed value
// (ErrInvalidContact) or one already taken by another account
// (ErrPhoneTaken/ErrEmailTaken) returns without sending anything.
func (s *Service) RequestContactBindCode(ctx context.Context, userID uuid.UUID, contact string) error {
	channel, normalized, err := classifyContact(contact)
	if err != nil {
		return err
	}
	if err := s.ensureContactAvailable(ctx, userID, channel, normalized); err != nil {
		return err
	}
	return s.codes.RequestCode(ctx, normalized, codePurposeContactBind)
}

// BindContact verifies the code sent by RequestContactBindCode and, on success,
// writes the new phone/email onto the user — binding a missing contact or
// replacing an existing one. Availability is re-checked before the code is
// consumed (so a conflict doesn't waste the code), and the unique index is the
// final guard against a concurrent bind of the same value (surfaces as
// ErrPhoneTaken/ErrEmailTaken). Returns the refreshed user on success.
func (s *Service) BindContact(ctx context.Context, userID uuid.UUID, contact, code string) (*User, error) {
	channel, normalized, err := classifyContact(contact)
	if err != nil {
		return nil, err
	}
	if err := s.ensureContactAvailable(ctx, userID, channel, normalized); err != nil {
		return nil, err
	}
	if err := s.codes.Verify(ctx, normalized, codePurposeContactBind, code); err != nil {
		return nil, ErrInvalidBindCode
	}
	if err := s.repo.SetContact(ctx, userID, channel, normalized); err != nil {
		return nil, err // includes ErrPhoneTaken/ErrEmailTaken (race) and ErrNotFound
	}
	return s.repo.GetByID(ctx, userID)
}

// ensureContactAvailable reports whether normalized (an email or phone, per
// channel) is free to bind: free if no account holds it, or if the only holder is
// the caller (re-binding one's own contact is a harmless no-op). A value held by
// another account returns ErrEmailTaken/ErrPhoneTaken.
func (s *Service) ensureContactAvailable(ctx context.Context, userID uuid.UUID, channel, normalized string) error {
	var existing *User
	var err error
	switch channel {
	case ContactChannelEmail:
		existing, err = s.repo.GetByEmail(ctx, normalized)
	case ContactChannelPhone:
		existing, err = s.repo.GetByPhone(ctx, normalized)
	default:
		return ErrInvalidContact
	}
	if errors.Is(err, ErrNotFound) {
		return nil // nobody holds it
	}
	if err != nil {
		return err
	}
	if existing.ID == userID {
		return nil // already mine
	}
	if channel == ContactChannelEmail {
		return ErrEmailTaken
	}
	return ErrPhoneTaken
}

// classifyContact decides whether a contact value is an email or a phone (an "@"
// means email) and returns the channel plus its normalized form. Validation
// mirrors registration: a valid email address, or a 5–20 char phone.
func classifyContact(contact string) (channel, normalized string, err error) {
	if isEmail(contact) {
		email := normalizeEmail(contact)
		addr, perr := mail.ParseAddress(email)
		if perr != nil || addr.Address != email {
			return "", "", ErrInvalidContact
		}
		return ContactChannelEmail, email, nil
	}
	phone := normalizePhone(contact)
	if len(phone) < 5 || len(phone) > 20 {
		return "", "", ErrInvalidContact
	}
	return ContactChannelPhone, phone, nil
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
