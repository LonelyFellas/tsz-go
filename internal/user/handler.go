package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
)

// Handler adapts HTTP requests to the Service. It owns request/response shapes
// and validation; all business rules live in the Service.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type registerRequest struct {
	Phone       string `json:"phone" binding:"required,min=5,max=20"`
	Email       string `json:"email" binding:"omitempty,email"`          // optional
	Password    string `json:"password" binding:"required,min=8,max=72"` // bcrypt caps at 72 bytes
	DisplayName string `json:"display_name" binding:"required,min=1,max=50"`
	Role        string `json:"role" binding:"required,oneof=student teacher"`
}

type authResponse struct {
	User       *User  `json:"user"`
	Token      string `json:"token"`
	ActiveRole string `json:"active_role"`
}

func (h *Handler) Register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, token, err := h.svc.Register(c.Request.Context(), req.Phone, req.Email, req.Password, req.DisplayName, Role(req.Role))
	switch {
	case errors.Is(err, ErrPhoneTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "phone already registered"})
		return
	case errors.Is(err, ErrEmailTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusCreated, authResponse{User: u, Token: token, ActiveRole: req.Role})
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

	u, token, err := h.svc.LoginPassword(c.Request.Context(), req.Identifier, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, authResponse{User: u, Token: token, ActiveRole: string(defaultRole(u.Roles))})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
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

	u, token, err := h.svc.LoginCode(c.Request.Context(), req.Identifier, req.Code)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, authResponse{User: u, Token: token, ActiveRole: string(defaultRole(u.Roles))})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"token": token, "active_role": req.Role})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"token": token, "active_role": req.Role})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": u, "active_role": activeRole})
}
