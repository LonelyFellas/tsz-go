// Package auth handles JWT issuing/parsing. Stateless tokens fit toC well:
// no server-side session store to operate or scale.
package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Context keys under which the authenticated principal is stored on the gin
// context. They live here so both the middleware and the domain handlers can
// reference them without creating an import cycle.
//
// Web and admin are separate identity realms, so their principals are stored
// under separate keys — a handler can never accidentally read an admin id where
// it expected a web user id.
const (
	ContextUserIDKey = "auth.userID"
	ContextRoleKey   = "auth.role"
	// ContextAdminIDKey and ContextAdminLevelKey hold the authenticated admin's
	// id and level (admin/super_admin), populated by the admin gate.
	ContextAdminIDKey    = "auth.adminID"
	ContextAdminLevelKey = "auth.adminLevel"
)

// Token realms. A token is bound to exactly one realm and is signed with that
// realm's key; Parse rejects a token whose realm does not match the manager's.
// This is what keeps a web token off the admin API and vice versa — the boundary
// is enforced by the signing key first and re-checked by the realm claim.
const (
	RealmWeb   = "web"
	RealmAdmin = "admin"
)

// Claims is what a parsed token decodes to: who the principal is, the realm the
// token belongs to, and a realm-specific role/level string ("active role" for
// web, level for admin).
type Claims struct {
	Subject uuid.UUID
	Realm   string
	Role    string
}

// tokenClaims is the on-the-wire JWT payload: the registered claims plus our
// custom realm and role claims.
type tokenClaims struct {
	Realm string `json:"realm"`
	Role  string `json:"role"`
	jwt.RegisteredClaims
}

// TokenManager signs and verifies access tokens for a single realm. Construct
// one per realm (web, admin) with that realm's own secret, so a token signed by
// one manager fails verification under the other.
type TokenManager struct {
	secret []byte
	ttl    time.Duration
	realm  string
}

// NewTokenManager builds a token manager bound to a realm. secret must differ
// between realms for the cross-realm rejection to hold.
func NewTokenManager(secret string, ttl time.Duration, realm string) *TokenManager {
	return &TokenManager{secret: []byte(secret), ttl: ttl, realm: realm}
}

// Generate issues a signed HS256 token whose subject is the principal ID and
// which carries this manager's realm plus the given role/level.
func (m *TokenManager) Generate(subject uuid.UUID, role string) (string, error) {
	now := time.Now()
	claims := tokenClaims{
		Realm: m.realm,
		Role:  role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// Parse validates a token and returns its claims. It rejects a token whose realm
// claim does not match this manager's realm (defence in depth on top of the
// per-realm signing key).
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
	if claims.Realm != m.realm {
		return Claims{}, fmt.Errorf("token realm %q does not match %q", claims.Realm, m.realm)
	}

	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Claims{}, fmt.Errorf("invalid token subject: %w", err)
	}
	return Claims{Subject: id, Realm: claims.Realm, Role: claims.Role}, nil
}
