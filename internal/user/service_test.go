package user

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/session"
)

func newTestService() (*Service, *fakeStore, *fakeCodes, *fakeSessions) {
	store := newFakeStore()
	codes := newFakeCodes()
	sessions := newFakeSessions()
	tm := auth.NewTokenManager("test-secret", time.Hour, auth.RealmWeb)
	return NewService(store, tm, codes, sessions), store, codes, sessions
}

func TestService_Register_Success(t *testing.T) {
	svc, store, _, _ := newTestService()

	u, access, refresh, err := svc.Register(context.Background(), " 13800138000 ", "Alice@Example.com", "password123", "  Alice  ", RoleStudent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// phone is trimmed, email normalized to lowercase, display name trimmed
	if u.Phone != "13800138000" {
		t.Errorf("phone = %q, want trimmed", u.Phone)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("email = %q, want normalized lowercase", u.Email)
	}
	if u.DisplayName != "Alice" {
		t.Errorf("display name = %q, want trimmed", u.DisplayName)
	}
	// the registered role is recorded on the user
	if len(u.Roles) != 1 || u.Roles[0] != RoleStudent {
		t.Errorf("roles = %v, want [student]", u.Roles)
	}
	// password is hashed, never stored plaintext, and verifies
	if u.PasswordHash == "" || u.PasswordHash == "password123" {
		t.Error("password must be hashed")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("password123")); err != nil {
		t.Errorf("stored hash does not verify: %v", err)
	}
	// a valid access token referencing this user and role is returned
	claims, err := svc.token.Parse(access)
	if err != nil || claims.Subject != u.ID {
		t.Errorf("token did not parse back to user id: id=%s err=%v", claims.Subject, err)
	}
	if claims.Role != string(RoleStudent) {
		t.Errorf("token role = %q, want student", claims.Role)
	}
	// a refresh token is issued alongside the access token
	if refresh == "" {
		t.Error("expected a refresh token")
	}
	// it was actually persisted
	if _, err := store.GetByID(context.Background(), u.ID); err != nil {
		t.Errorf("user not persisted: %v", err)
	}
}

func TestService_Register_OptionalEmail(t *testing.T) {
	svc, _, _, _ := newTestService()
	u, _, _, err := svc.Register(context.Background(), "13800138001", "", "password123", "NoEmail", RoleTeacher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Email != "" {
		t.Errorf("email = %q, want empty", u.Email)
	}
}

func TestService_Register_DuplicatePhone(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()

	if _, _, _, err := svc.Register(ctx, "13800138000", "a@example.com", "password123", "First", RoleStudent); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	_, _, _, err := svc.Register(ctx, "13800138000", "b@example.com", "password123", "Second", RoleStudent)
	if !errors.Is(err, ErrPhoneTaken) {
		t.Fatalf("err = %v, want ErrPhoneTaken", err)
	}
}

func TestService_Register_DuplicateEmail(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()

	if _, _, _, err := svc.Register(ctx, "13800138000", "dup@example.com", "password123", "First", RoleStudent); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	// same email, different case and phone → still a conflict
	_, _, _, err := svc.Register(ctx, "13800138001", "DUP@example.com", "password123", "Second", RoleStudent)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestService_Register_PropagatesStoreError(t *testing.T) {
	svc, store, _, _ := newTestService()
	boom := errors.New("db down")
	store.createFn = func(*User, Role) error { return boom }

	_, _, _, err := svc.Register(context.Background(), "13800138000", "x@example.com", "password123", "X", RoleStudent)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestService_Register_BcryptError(t *testing.T) {
	svc, _, _, _ := newTestService()
	// bcrypt rejects passwords longer than 72 bytes; the service must surface
	// that as an error rather than storing a half-built user.
	long := strings.Repeat("x", 100)
	if _, _, _, err := svc.Register(context.Background(), "13800138000", "x@example.com", long, "X", RoleStudent); err == nil {
		t.Fatal("expected an error for an over-long password")
	}
}

func TestService_Register_InvalidRole(t *testing.T) {
	svc, _, _, _ := newTestService()
	// An unknown role is rejected by the service. (Admin is now a *valid* Role, so
	// it no longer fails here — admin self-registration is instead blocked at the
	// handler's binding tag; see TestHandler_Register "invalid role".)
	if _, _, _, err := svc.Register(context.Background(), "13800138000", "r@example.com", "password123", "R", Role("superuser")); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("err = %v, want ErrInvalidRole", err)
	}
}

func TestService_LoginPassword_Success(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "login@example.com", "password123", "L", RoleStudent)

	// login by email (different casing) should work
	u, access, refresh, err := svc.LoginPassword(ctx, "LOGIN@example.com", "password123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.ID != reg.ID {
		t.Errorf("logged in as %s, want %s", u.ID, reg.ID)
	}
	claims, err := svc.token.Parse(access)
	if err != nil {
		t.Errorf("invalid token returned: %v", err)
	}
	if claims.Role != string(RoleStudent) {
		t.Errorf("login token role = %q, want student", claims.Role)
	}
	if refresh == "" {
		t.Error("expected a refresh token")
	}

	// login by phone should also work
	if _, _, _, err := svc.LoginPassword(ctx, "13800138000", "password123"); err != nil {
		t.Errorf("phone login failed: %v", err)
	}
}

func TestService_LoginPassword_WrongPassword(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	_, _, _, _ = svc.Register(ctx, "13800138000", "wp@example.com", "password123", "W", RoleStudent)

	_, _, _, err := svc.LoginPassword(ctx, "wp@example.com", "wrongpassword")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginPassword_UnknownIdentifier(t *testing.T) {
	svc, _, _, _ := newTestService()

	// same generic error as a wrong password, to avoid leaking which accounts exist
	if _, _, _, err := svc.LoginPassword(context.Background(), "nobody@example.com", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if _, _, _, err := svc.LoginPassword(context.Background(), "19999999999", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("phone err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginPassword_PropagatesUnexpectedStoreError(t *testing.T) {
	svc, store, _, _ := newTestService()
	boom := errors.New("db exploded")
	store.getEmail = func(string) (*User, error) { return nil, boom }

	_, _, _, err := svc.LoginPassword(context.Background(), "any@example.com", "password123")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestService_LoginCode_Success(t *testing.T) {
	svc, _, codes, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "code@example.com", "password123", "C", RoleStudent)

	if err := svc.RequestLoginCode(ctx, "13800138000"); err != nil {
		t.Fatalf("request code: %v", err)
	}
	// the fake "sent" a deterministic code to the (normalized) target
	if codes.codes["13800138000"] != "123456" {
		t.Fatalf("code not recorded for target: %v", codes.codes)
	}

	u, access, _, err := svc.LoginCode(ctx, "13800138000", "123456")
	if err != nil {
		t.Fatalf("login by code: %v", err)
	}
	if u.ID != reg.ID {
		t.Errorf("logged in as %s, want %s", u.ID, reg.ID)
	}
	if _, err := svc.token.Parse(access); err != nil {
		t.Errorf("invalid token: %v", err)
	}

	// the code is single-use: a second attempt fails
	if _, _, _, err := svc.LoginCode(ctx, "13800138000", "123456"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("reused code err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginCode_WrongCode(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	_, _, _, _ = svc.Register(ctx, "13800138000", "code@example.com", "password123", "C", RoleStudent)
	_ = svc.RequestLoginCode(ctx, "13800138000")

	if _, _, _, err := svc.LoginCode(ctx, "13800138000", "000000"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginCode_UnknownUser(t *testing.T) {
	svc, _, _, _ := newTestService()
	// no such user → generic error, and we never reach code verification
	if _, _, _, err := svc.LoginCode(context.Background(), "19999999999", "123456"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

// Logging in a second time must invalidate the first login's refresh token:
// strict single-device, driven through the Service.
func TestService_Login_SingleDevice(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	_, _, firstRefresh, _ := svc.Register(ctx, "13800138000", "sd@example.com", "password123", "SD", RoleStudent)

	// a second login (new device) issues a new refresh token...
	_, _, secondRefresh, err := svc.LoginPassword(ctx, "13800138000", "password123")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if firstRefresh == secondRefresh {
		t.Fatal("expected a distinct refresh token on re-login")
	}

	// ...and the first device's refresh token is now dead
	if _, _, err := svc.Refresh(ctx, firstRefresh); !errors.Is(err, session.ErrInvalidRefreshToken) {
		t.Errorf("old-device refresh err = %v, want ErrInvalidRefreshToken", err)
	}
	// the new device still works
	if _, _, err := svc.Refresh(ctx, secondRefresh); err != nil {
		t.Errorf("new-device refresh: %v", err)
	}
}

func TestService_Refresh_RotatesAndReSignsAccess(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	reg, _, refresh, _ := svc.Register(ctx, "13800138000", "rf@example.com", "password123", "RF", RoleStudent)

	access, rotated, err := svc.Refresh(ctx, refresh)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// the new access token is valid and scoped to the user
	claims, err := svc.token.Parse(access)
	if err != nil || claims.Subject != reg.ID {
		t.Errorf("refreshed access invalid: id=%s err=%v", claims.Subject, err)
	}
	if rotated == "" || rotated == refresh {
		t.Error("refresh must rotate the refresh token")
	}
	// replaying the old refresh token fails (single-use)
	if _, _, err := svc.Refresh(ctx, refresh); !errors.Is(err, session.ErrInvalidRefreshToken) {
		t.Errorf("replayed refresh err = %v, want ErrInvalidRefreshToken", err)
	}
}

// Regression: a switch-role must survive a token refresh. Previously Refresh
// always re-issued the default (first) role, silently undoing a prior switch.
func TestService_Refresh_PreservesActiveRole(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	reg, _, refresh, _ := svc.Register(ctx, "13800138000", "pa@example.com", "password123", "PA", RoleStudent)

	// become a teacher and switch to that role
	if _, err := svc.AddRole(ctx, reg.ID, RoleTeacher); err != nil {
		t.Fatalf("add role: %v", err)
	}
	if _, err := svc.SwitchRole(ctx, reg.ID, RoleTeacher); err != nil {
		t.Fatalf("switch role: %v", err)
	}

	// refreshing the access token must keep the teacher role, not reset to student
	access, _, err := svc.Refresh(ctx, refresh)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	claims, err := svc.token.Parse(access)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.Role != string(RoleTeacher) {
		t.Errorf("refreshed active role = %q, want teacher (switch-role lost on refresh)", claims.Role)
	}
}

// disable flips a user's stored status to disabled, simulating a back-office
// disable so the login/refresh rejection paths can be exercised.
func disable(t *testing.T, store *fakeStore, id uuid.UUID) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	u, ok := store.byID[id]
	if !ok {
		t.Fatalf("user %s not in store", id)
	}
	u.Status = StatusDisabled
}

func TestService_LoginPassword_Disabled(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "d@example.com", "password123", "D", RoleStudent)
	disable(t, store, reg.ID)

	// correct password, but the account is disabled → ErrAccountDisabled, not a
	// successful login and not the generic credentials error.
	if _, _, _, err := svc.LoginPassword(ctx, "13800138000", "password123"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled", err)
	}
	// a wrong password on a disabled account still returns the generic error,
	// never revealing the disabled state.
	if _, _, _, err := svc.LoginPassword(ctx, "13800138000", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong-password err = %v, want ErrInvalidCredentials", err)
	}
}

func TestService_LoginCode_Disabled(t *testing.T) {
	svc, store, codes, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "dc@example.com", "password123", "DC", RoleStudent)
	disable(t, store, reg.ID)

	if err := svc.RequestLoginCode(ctx, "13800138000"); err != nil {
		t.Fatalf("request code: %v", err)
	}
	code := codes.codes["13800138000"]
	if _, _, _, err := svc.LoginCode(ctx, "13800138000", code); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled", err)
	}
}

// A disabled account cannot refresh even with a previously valid refresh token:
// the disable takes effect within one access-token TTL.
func TestService_Refresh_Disabled(t *testing.T) {
	svc, store, _, _ := newTestService()
	ctx := context.Background()
	reg, _, refresh, _ := svc.Register(ctx, "13800138000", "rd@example.com", "password123", "RD", RoleStudent)
	disable(t, store, reg.ID)

	if _, _, err := svc.Refresh(ctx, refresh); !errors.Is(err, session.ErrInvalidRefreshToken) {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestService_ResetPassword_Success(t *testing.T) {
	svc, _, codes, _ := newTestService()
	ctx := context.Background()
	_, _, oldRefresh, _ := svc.Register(ctx, "13800138000", "rp@example.com", "password123", "RP", RoleStudent)

	if err := svc.RequestPasswordReset(ctx, "13800138000"); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	code := codes.codes["13800138000"]
	if code == "" {
		t.Fatalf("reset code not recorded for target: %v", codes.codes)
	}

	if err := svc.ResetPassword(ctx, "13800138000", code, "newpassword456"); err != nil {
		t.Fatalf("reset password: %v", err)
	}

	// every prior session is revoked: the pre-reset refresh token is dead.
	if _, _, err := svc.Refresh(ctx, oldRefresh); !errors.Is(err, session.ErrInvalidRefreshToken) {
		t.Errorf("post-reset refresh err = %v, want ErrInvalidRefreshToken", err)
	}
	// the old password no longer works...
	if _, _, _, err := svc.LoginPassword(ctx, "13800138000", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("login with old password err = %v, want ErrInvalidCredentials", err)
	}
	// ...and the new one does.
	if _, _, _, err := svc.LoginPassword(ctx, "13800138000", "newpassword456"); err != nil {
		t.Errorf("login with new password: %v", err)
	}
	// the reset code is single-use: replaying it fails.
	if err := svc.ResetPassword(ctx, "13800138000", code, "another123"); !errors.Is(err, ErrInvalidResetCode) {
		t.Errorf("reused reset code err = %v, want ErrInvalidResetCode", err)
	}
}

func TestService_ResetPassword_WrongCode(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	_, _, _, _ = svc.Register(ctx, "13800138000", "rpw@example.com", "password123", "RPW", RoleStudent)
	_ = svc.RequestPasswordReset(ctx, "13800138000")

	if err := svc.ResetPassword(ctx, "13800138000", "000000", "newpassword456"); !errors.Is(err, ErrInvalidResetCode) {
		t.Fatalf("err = %v, want ErrInvalidResetCode", err)
	}
	// the password was not changed: the original still logs in.
	if _, _, _, err := svc.LoginPassword(ctx, "13800138000", "password123"); err != nil {
		t.Errorf("original password should still work: %v", err)
	}
}

// A valid code for a phone with no account must stay generic (ErrInvalidResetCode),
// never revealing that the number is unregistered.
func TestService_ResetPassword_UnknownPhone(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()

	if err := svc.RequestPasswordReset(ctx, "19999999999"); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	if err := svc.ResetPassword(ctx, "19999999999", "123456", "newpassword456"); !errors.Is(err, ErrInvalidResetCode) {
		t.Fatalf("err = %v, want ErrInvalidResetCode", err)
	}
}

// A disabled account cannot reset its way back in: after the code checks out, the
// disabled state surfaces (mirroring code login).
func TestService_ResetPassword_Disabled(t *testing.T) {
	svc, store, codes, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "rpd@example.com", "password123", "RPD", RoleStudent)
	disable(t, store, reg.ID)

	_ = svc.RequestPasswordReset(ctx, "13800138000")
	code := codes.codes["13800138000"]
	if err := svc.ResetPassword(ctx, "13800138000", code, "newpassword456"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled", err)
	}
}

func TestService_Logout_RevokesRefresh(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	_, _, refresh, _ := svc.Register(ctx, "13800138000", "lo@example.com", "password123", "LO", RoleStudent)

	if err := svc.Logout(ctx, refresh); err != nil {
		t.Fatalf("logout: %v", err)
	}
	// the refresh token no longer works
	if _, _, err := svc.Refresh(ctx, refresh); !errors.Is(err, session.ErrInvalidRefreshToken) {
		t.Errorf("post-logout refresh err = %v, want ErrInvalidRefreshToken", err)
	}
	// logout is idempotent
	if err := svc.Logout(ctx, refresh); err != nil {
		t.Errorf("second logout: %v", err)
	}
}

func TestService_SwitchRole(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "sw@example.com", "password123", "SW", RoleStudent)

	// switching to a role the user does not hold is rejected
	if _, err := svc.SwitchRole(ctx, reg.ID, RoleTeacher); !errors.Is(err, ErrRoleNotOwned) {
		t.Fatalf("err = %v, want ErrRoleNotOwned", err)
	}

	// acquire the teacher identity, then switching to it issues a teacher token
	if _, err := svc.AddRole(ctx, reg.ID, RoleTeacher); err != nil {
		t.Fatalf("add role: %v", err)
	}
	tok, err := svc.SwitchRole(ctx, reg.ID, RoleTeacher)
	if err != nil {
		t.Fatalf("switch role: %v", err)
	}
	claims, err := svc.token.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.Role != string(RoleTeacher) {
		t.Errorf("active role = %q, want teacher", claims.Role)
	}
}

func TestService_AddRole_Duplicate(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "ar@example.com", "password123", "AR", RoleStudent)

	if _, err := svc.AddRole(ctx, reg.ID, RoleStudent); !errors.Is(err, ErrRoleTaken) {
		t.Fatalf("err = %v, want ErrRoleTaken", err)
	}
}

func TestService_GetByID(t *testing.T) {
	svc, _, _, _ := newTestService()
	ctx := context.Background()
	reg, _, _, _ := svc.Register(ctx, "13800138000", "g@example.com", "password123", "G", RoleStudent)

	got, err := svc.GetByID(ctx, reg.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email != "g@example.com" {
		t.Errorf("email = %q", got.Email)
	}

	if _, err := svc.GetByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNormalizeIdentifier(t *testing.T) {
	cases := map[string]string{
		"  Foo@Bar.COM ": "foo@bar.com",
		" 13800138000 ":  "13800138000",
		"\tX@Y.Com\n":    "x@y.com",
	}
	for in, want := range cases {
		if got := normalizeIdentifier(in); got != want {
			t.Errorf("normalizeIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRole_Valid(t *testing.T) {
	cases := map[Role]bool{
		RoleStudent:     true,
		RoleTeacher:     true,
		Role("admin"):   false, // admin is a separate identity realm, not a web role
		Role(""):        false,
		Role("Student"): false, // case-sensitive
	}
	for r, want := range cases {
		if got := r.Valid(); got != want {
			t.Errorf("Role(%q).Valid() = %v, want %v", string(r), got, want)
		}
	}
}

func TestDefaultRole(t *testing.T) {
	// empty roles → zero Role (the user holds no identity yet)
	if got := defaultRole(nil); got != "" {
		t.Errorf("defaultRole(nil) = %q, want empty", got)
	}
	if got := defaultRole([]Role{}); got != "" {
		t.Errorf("defaultRole([]) = %q, want empty", got)
	}
	// first role wins (roles load in a stable order)
	if got := defaultRole([]Role{RoleTeacher, RoleStudent}); got != RoleTeacher {
		t.Errorf("defaultRole = %q, want teacher", got)
	}
}
