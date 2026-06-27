// Package admin is a vertical slice for the back-office identity domain: HTTP
// handler, business logic (service), and data access (repository) live together
// here.
//
// Admin is a SEPARATE identity store from the web user (internal/user): different
// table (admins), different login, different JWT signing key. A web account and
// an admin account are unrelated even if they share a phone number — one can
// never act as the other. See docs/user-module-design.md.
package admin

import (
	"time"

	"github.com/google/uuid"
)

// Level is an admin account's privilege tier. A super_admin can additionally
// manage admin accounts (create / enable / disable); both tiers can use the rest
// of the back office. A single account holds exactly one level — admin is not a
// multi-role identity like a web user.
type Level string

const (
	LevelAdmin      Level = "admin"
	LevelSuperAdmin Level = "super_admin"
)

// Valid reports whether l is a known level.
func (l Level) Valid() bool {
	return l == LevelAdmin || l == LevelSuperAdmin
}

// Status is an admin account's lifecycle state. A disabled admin is rejected at
// the login boundary (password login and refresh), so disabling takes effect
// within one access-token TTL.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// Admin is a back-office identity. Phone is the required primary identifier;
// email is optional. Either may be used to log in.
type Admin struct {
	ID           uuid.UUID `json:"id"`
	Phone        string    `json:"phone"`
	Email        string    `json:"email,omitempty"`
	PasswordHash string    `json:"-"` // never serialized
	DisplayName  string    `json:"display_name"`
	Level        Level     `json:"level"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
