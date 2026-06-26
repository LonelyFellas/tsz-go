package user

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/otp"
	"github.com/darwish/tsz-go/internal/session"
)

const (
	// refreshCookieName is the cookie that carries the refresh token. It is
	// delivered HttpOnly so browser JS can never read it (an XSS can't exfiltrate
	// the long-lived credential); the access token still travels in the JSON body
	// and is sent back via the Authorization header.
	refreshCookieName = "refresh_token"
	// refreshCookiePath scopes the cookie to the auth endpoints that actually need
	// it (refresh + logout). Ordinary API calls never carry it, shrinking both the
	// CSRF surface and accidental exposure.
	refreshCookiePath = "/api/v1/auth"
)

// CookieConfig controls how the refresh-token cookie is emitted.
type CookieConfig struct {
	// Secure restricts the cookie to HTTPS. Must be false for local http dev
	// (browsers drop Secure cookies on http) and true in production.
	Secure bool
	// MaxAge is the cookie lifetime; mirrors the refresh-token TTL so the cookie
	// expires together with the token it carries.
	MaxAge time.Duration
}

// Handler adapts HTTP requests to the Service. It owns request/response shapes
// and validation; all business rules live in the Service.
type Handler struct {
	svc             *Service
	cookie          CookieConfig
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

func NewHandler(svc *Service, cookie CookieConfig, accessTokenTTL, refreshTokenTTL time.Duration) *Handler {
	return &Handler{svc: svc, cookie: cookie, accessTokenTTL: accessTokenTTL, refreshTokenTTL: refreshTokenTTL}
}

// internalError attaches err to the gin context (so the request logger records
// the real cause) and returns a generic 500. The client never sees internal
// details; our logs always do.
func internalError(c *gin.Context, err error) {
	_ = c.Error(err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

// newAuthResponse builds an authResponse with token expiry fields populated.
func (h *Handler) newAuthResponse(u *User, accessToken, activeRole string) authResponse {
	now := time.Now()
	return authResponse{
		User:                  u,
		AccessToken:           accessToken,
		ActiveRole:            activeRole,
		ExpiresIn:             int64(h.accessTokenTTL.Seconds()),
		RefreshTokenExpiresAt: now.Add(h.refreshTokenTTL).Unix(),
	}
}

// setRefreshCookie writes the refresh token as an HttpOnly, SameSite=Strict
// cookie. Strict keeps it off cross-site requests, which (together with the
// path scoping) defends the refresh/logout endpoints against CSRF.
func (h *Handler) setRefreshCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookieName, token, int(h.cookie.MaxAge.Seconds()), refreshCookiePath, "", h.cookie.Secure, true)
}

// clearRefreshCookie expires the refresh cookie (MaxAge<0 deletes it). Used on
// logout and whenever a presented refresh token turns out to be invalid.
func (h *Handler) clearRefreshCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookieName, "", -1, refreshCookiePath, "", h.cookie.Secure, true)
}

type registerRequest struct {
	Phone       string `json:"phone" binding:"required,min=5,max=20"`
	Email       string `json:"email" binding:"omitempty,email"`          // optional
	Password    string `json:"password" binding:"required,min=8,max=72"` // bcrypt caps at 72 bytes
	DisplayName string `json:"display_name" binding:"required,min=1,max=50"`
	Role        string `json:"role" binding:"required,oneof=student teacher"`
}

// authResponse is the unified login/register payload: the user plus a short-lived
// access token. The refresh token is NOT in the body — it is delivered out of
// band as an HttpOnly cookie (see setRefreshCookie) so JS can't touch it.
type authResponse struct {
	User                  *User  `json:"user"`
	AccessToken           string `json:"access_token"`
	ActiveRole            string `json:"active_role"`
	ExpiresIn             int64  `json:"expires_in"`               // access token TTL in seconds
	RefreshTokenExpiresAt int64  `json:"refresh_token_expires_at"` // Unix timestamp (seconds)
}

func (h *Handler) Register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, access, refresh, err := h.svc.Register(c.Request.Context(), req.Phone, req.Email, req.Password, req.DisplayName, Role(req.Role))
	switch {
	case errors.Is(err, ErrPhoneTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "phone already registered"})
		return
	case errors.Is(err, ErrEmailTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	h.setRefreshCookie(c, refresh)
	c.JSON(http.StatusCreated, h.newAuthResponse(u, access, req.Role))
}

type loginRequest struct {
	Identifier string `json:"identifier" binding:"required"` // phone or email
	Password   string `json:"password" binding:"required"`
}

// Login authenticates with an identifier (phone or email) and password.
func (h *Handler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, access, refresh, err := h.svc.LoginPassword(c.Request.Context(), req.Identifier, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	h.setRefreshCookie(c, refresh)
	c.JSON(http.StatusOK, h.newAuthResponse(u, access, string(activeRole(u))))
}

type sendCodeRequest struct {
	Identifier string `json:"identifier" binding:"required"` // phone or email
}

// SendCode issues a one-time login code to the identifier. Always 200 (even for
// unknown identifiers) so it can't be used to probe which accounts exist.
func (h *Handler) SendCode(c *gin.Context) {
	var req sendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.svc.RequestLoginCode(c.Request.Context(), req.Identifier); err != nil {
		if errors.Is(err, otp.ErrRateLimited) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many code requests, try again later"})
			return
		}
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

type loginCodeRequest struct {
	Identifier string `json:"identifier" binding:"required"` // phone or email
	Code       string `json:"code" binding:"required"`
}

// LoginCode authenticates with an identifier (phone or email) and a one-time code.
func (h *Handler) LoginCode(c *gin.Context) {
	var req loginCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, access, refresh, err := h.svc.LoginCode(c.Request.Context(), req.Identifier, req.Code)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	h.setRefreshCookie(c, refresh)
	c.JSON(http.StatusOK, h.newAuthResponse(u, access, string(activeRole(u))))
}

// Refresh exchanges the refresh-token cookie for a new access token and a rotated
// refresh token (set back as a fresh cookie). A missing cookie or an
// invalid/revoked/expired token → 401, and a stale cookie is cleared.
func (h *Handler) Refresh(c *gin.Context) {
	token, err := c.Cookie(refreshCookieName)
	if err != nil || token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
		return
	}

	access, refresh, err := h.svc.Refresh(c.Request.Context(), token)
	switch {
	case errors.Is(err, session.ErrInvalidRefreshToken):
		h.clearRefreshCookie(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	h.setRefreshCookie(c, refresh)
	c.JSON(http.StatusOK, gin.H{
		"access_token":             access,
		"expires_in":               int64(h.accessTokenTTL.Seconds()),
		"refresh_token_expires_at": time.Now().Add(h.refreshTokenTTL).Unix(),
	})
}

// Logout revokes the refresh token carried by the cookie and clears the cookie.
// Idempotent: a missing/already-revoked token still returns 204.
func (h *Handler) Logout(c *gin.Context) {
	token, _ := c.Cookie(refreshCookieName)
	if token != "" {
		if err := h.svc.Logout(c.Request.Context(), token); err != nil {
			internalError(c, err)
			return
		}
	}
	h.clearRefreshCookie(c)

	// 204 has no body; flush the status now so it's emitted even with no write.
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}

// LogoutAll revokes every refresh token the authenticated user holds (logout
// everywhere), using the user ID from the access token rather than a presented
// refresh token. Idempotent: a user with no active sessions still returns 204.
func (h *Handler) LogoutAll(c *gin.Context) {
	userID := c.MustGet(auth.ContextUserIDKey).(uuid.UUID)

	if err := h.svc.LogoutAll(c.Request.Context(), userID); err != nil {
		internalError(c, err)
		return
	}

	// 204 has no body; flush the status now so it's emitted even with no write.
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}

type roleRequest struct {
	Role string `json:"role" binding:"required,oneof=student teacher"`
}

// SwitchRole re-issues a token scoped to a role the user already holds.
func (h *Handler) SwitchRole(c *gin.Context) {
	userID := c.MustGet(auth.ContextUserIDKey).(uuid.UUID)

	var req roleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	token, err := h.svc.SwitchRole(c.Request.Context(), userID, Role(req.Role))
	switch {
	case errors.Is(err, ErrRoleNotOwned):
		c.JSON(http.StatusForbidden, gin.H{"error": "user does not have this role"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"access_token": token, "active_role": req.Role})
}

// AddRole grants the user a second identity and returns a token switched to it.
func (h *Handler) AddRole(c *gin.Context) {
	userID := c.MustGet(auth.ContextUserIDKey).(uuid.UUID)

	var req roleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	token, err := h.svc.AddRole(c.Request.Context(), userID, Role(req.Role))
	switch {
	case errors.Is(err, ErrRoleTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "user already has this role"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"access_token": token, "active_role": req.Role})
}

func (h *Handler) Me(c *gin.Context) {
	userID := c.MustGet(auth.ContextUserIDKey).(uuid.UUID)
	activeRole, _ := c.Get(auth.ContextRoleKey)

	u, err := h.svc.GetByID(c.Request.Context(), userID)
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": u, "active_role": activeRole})
}
