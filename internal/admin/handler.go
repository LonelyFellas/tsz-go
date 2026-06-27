package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/session"
)

const (
	// refreshCookieName carries the admin refresh token. Delivered HttpOnly so
	// browser JS can never read it. Deliberately distinct from the web cookie name.
	refreshCookieName = "admin_refresh_token"
	// refreshCookiePath scopes the cookie to the admin auth endpoints, so it is
	// never sent to web routes (and the web cookie is never sent here).
	refreshCookiePath = "/api/v1/admin"
)

// CookieConfig controls how the admin refresh-token cookie is emitted (mirrors
// user.CookieConfig).
type CookieConfig struct {
	Secure bool
	MaxAge time.Duration
}

// Handler adapts HTTP requests to the admin Service.
type Handler struct {
	svc             *Service
	cookie          CookieConfig
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

func NewHandler(svc *Service, cookie CookieConfig, accessTokenTTL, refreshTokenTTL time.Duration) *Handler {
	return &Handler{svc: svc, cookie: cookie, accessTokenTTL: accessTokenTTL, refreshTokenTTL: refreshTokenTTL}
}

func internalError(c *gin.Context, err error) {
	_ = c.Error(err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

func (h *Handler) setRefreshCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookieName, token, int(h.cookie.MaxAge.Seconds()), refreshCookiePath, "", h.cookie.Secure, true)
}

func (h *Handler) clearRefreshCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookieName, "", -1, refreshCookiePath, "", h.cookie.Secure, true)
}

// authResponse is the admin login payload. The refresh token is NOT in the body
// — it rides in the admin_refresh_token cookie.
type authResponse struct {
	Admin                 *Admin `json:"admin"`
	AccessToken           string `json:"access_token"`
	Level                 Level  `json:"level"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresAt int64  `json:"refresh_token_expires_at"`
}

func (h *Handler) newAuthResponse(a *Admin, accessToken string) authResponse {
	return authResponse{
		Admin:                 a,
		AccessToken:           accessToken,
		Level:                 a.Level,
		ExpiresIn:             int64(h.accessTokenTTL.Seconds()),
		RefreshTokenExpiresAt: time.Now().Add(h.refreshTokenTTL).Unix(),
	}
}

type loginRequest struct {
	Identifier string `json:"identifier" binding:"required"` // phone or email
	Password   string `json:"password" binding:"required"`
}

// Login authenticates an admin with identifier + password.
func (h *Handler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	a, access, refresh, err := h.svc.Login(c.Request.Context(), req.Identifier, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	case errors.Is(err, ErrAccountDisabled):
		c.JSON(http.StatusForbidden, gin.H{"error": "account disabled"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	h.setRefreshCookie(c, refresh)
	c.JSON(http.StatusOK, h.newAuthResponse(a, access))
}

// Refresh exchanges the admin refresh-token cookie for a new access token and a
// rotated refresh token.
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

// Logout revokes the admin refresh token carried by the cookie and clears it.
func (h *Handler) Logout(c *gin.Context) {
	token, _ := c.Cookie(refreshCookieName)
	if token != "" {
		if err := h.svc.Logout(c.Request.Context(), token); err != nil {
			internalError(c, err)
			return
		}
	}
	h.clearRefreshCookie(c)
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}

// LogoutAll revokes every refresh token the authenticated admin holds.
func (h *Handler) LogoutAll(c *gin.Context) {
	adminID := c.MustGet(auth.ContextAdminIDKey).(uuid.UUID)
	if err := h.svc.LogoutAll(c.Request.Context(), adminID); err != nil {
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}

// profileResponse is the gate probe: the signed-in admin's own identity.
type profileResponse struct {
	ID          uuid.UUID `json:"id"`
	Phone       string    `json:"phone"`
	DisplayName string    `json:"display_name"`
	Level       Level     `json:"level"`
}

// Profile returns the signed-in admin's identity. 401 is handled upstream by the
// admin gate, so this only sees an authenticated admin (200, or 404 if vanished).
func (h *Handler) Profile(c *gin.Context) {
	adminID := c.MustGet(auth.ContextAdminIDKey).(uuid.UUID)
	a, err := h.svc.GetByID(c.Request.Context(), adminID)
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "admin not found"})
		return
	case err != nil:
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, profileResponse{ID: a.ID, Phone: a.Phone, DisplayName: a.DisplayName, Level: a.Level})
}

type createAdminRequest struct {
	Phone       string `json:"phone" binding:"required,min=5,max=20"`
	Email       string `json:"email" binding:"omitempty,email"`
	Password    string `json:"password" binding:"required,min=8,max=72"`
	DisplayName string `json:"display_name" binding:"required,min=1,max=50"`
	Level       string `json:"level" binding:"omitempty,oneof=admin super_admin"`
}

// CreateAdmin provisions a new admin (super-admin only; gated by the router).
func (h *Handler) CreateAdmin(c *gin.Context) {
	var req createAdminRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	a, err := h.svc.Create(c.Request.Context(), req.Phone, req.Email, req.Password, req.DisplayName, Level(req.Level))
	switch {
	case errors.Is(err, ErrPhoneTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "phone already registered"})
		return
	case errors.Is(err, ErrEmailTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		return
	case errors.Is(err, ErrInvalidLevel):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid level"})
		return
	case err != nil:
		internalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, a)
}

// ListAdmins returns a paginated admin directory (super-admin only).
func (h *Handler) ListAdmins(c *gin.Context) {
	page := clampInt(parseInt(c.Query("page"), 1), 1, 1<<31)
	pageSize := clampInt(parseInt(c.Query("page_size"), 20), 1, 100)

	admins, total, err := h.svc.List(c.Request.Context(), ListFilter{
		Level:  Level(c.Query("level")),
		Query:  c.Query("q"),
		Limit:  pageSize,
		Offset: (page - 1) * pageSize,
	})
	if err != nil {
		internalError(c, err)
		return
	}
	if admins == nil {
		admins = []Admin{}
	}
	c.JSON(http.StatusOK, gin.H{
		"items": admins,
		"page":  gin.H{"page": page, "page_size": pageSize, "total": total},
	})
}

type setStatusRequest struct {
	Status string `json:"status" binding:"required,oneof=active disabled"`
}

// SetAdminStatus enables/disables an admin (super-admin only).
func (h *Handler) SetAdminStatus(c *gin.Context) {
	id, err := uuid.Parse(c.Param("adminId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admin id"})
		return
	}
	var req setStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	switch err := h.svc.SetStatus(c.Request.Context(), id, Status(req.Status)); {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "admin not found"})
		return
	case errors.Is(err, ErrLastSuperAdmin):
		c.JSON(http.StatusConflict, gin.H{"error": "cannot disable the last active super admin"})
		return
	case err != nil:
		internalError(c, err)
		return
	}

	a, err := h.svc.GetByID(c.Request.Context(), id)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, a)
}

func parseInt(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return fallback
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
