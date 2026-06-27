// Package auth handles JWT issuing/parsing. Stateless tokens fit toC well:
// no server-side session store to operate or scale.
package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Context keys under which the authenticated user's ID and active role are
// stored on the gin context. They live here so both the middleware and the
// domain handlers can reference them without creating an import cycle.
const (
	ContextUserIDKey = "auth.userID"
	ContextRoleKey   = "auth.role"
)

// RoleAdmin is the active-role value that the admin gate checks for. It is
// duplicated here (as a plain string) so the httpserver middleware can gate on it
// without importing the user package — which would create an import cycle, since
// user depends on auth. The user package owns the canonical user.RoleAdmin; this
// constant must stay in sync with it.
const RoleAdmin = "admin"

// Claims is what a parsed token decodes to: who the user is, and which role
// they are currently acting as ("active role"). A user may hold several roles;
// switching role re-issues a token with a different Role.
type Claims struct {
	UserID uuid.UUID
	Role   string
}

// tokenClaims is the on-the-wire JWT payload: the registered claims plus our
// custom active-role claim.
type tokenClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type TokenManager struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), ttl: ttl}
}

// Generate issues a signed HS256 token whose subject is the user ID and which
// carries the active role.
func (m *TokenManager) Generate(userID uuid.UUID, role string) (string, error) {
	now := time.Now()
	claims := tokenClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// Parse validates a token and returns the user ID and active role it encodes.
func (m *TokenManager) Parse(tokenString string) (Claims, error) {
	claims := &tokenClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	})
	if err != nil || !token.Valid {
		return Claims{}, fmt.Errorf("invalid token: %w", err)
	}

	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Claims{}, fmt.Errorf("invalid token subject: %w", err)
	}
	return Claims{UserID: id, Role: claims.Role}, nil
}
