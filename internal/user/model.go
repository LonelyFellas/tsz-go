// Package user is a vertical slice for the user domain: HTTP handler, business
// logic (service), and data access (repository) live together here.
package user

import (
	"time"

	"github.com/google/uuid"
)

// Role is an identity a user can act as. A single account may hold more than one
// role (e.g. both student and teacher) and switch between them.
type Role string

const (
	RoleStudent Role = "student"
	RoleTeacher Role = "teacher"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	return r == RoleStudent || r == RoleTeacher
}

// User is the authentication identity. It is role-agnostic; the roles it holds
// are loaded into Roles, and the role it is currently acting as travels in the
// JWT rather than on the user record. Phone is the required primary identifier;
// Email is optional. Either may be used to log in.
type User struct {
	ID           uuid.UUID `json:"id"`
	Phone        string    `json:"phone"`
	Email        string    `json:"email,omitempty"`
	PasswordHash string    `json:"-"` // never serialized
	DisplayName  string    `json:"display_name"`
	Roles        []Role    `json:"roles"`
	// LastActiveRole is the role the most recently issued token acts as. It is
	// resumed on login and refresh so a switch-role survives token expiry. Empty
	// until the first token is issued; callers fall back to the default role (see
	// activeRole). Not serialized — the active role travels in auth responses.
	LastActiveRole Role      `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
