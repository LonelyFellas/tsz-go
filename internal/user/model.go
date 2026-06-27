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
	// RoleAdmin is the back-office role. It gates the /api/v1/admin/* API and is
	// never self-assignable: registration stays limited to student/teacher and the
	// first admin is bootstrapped out of band (see cmd/seed).
	RoleAdmin Role = "admin"
)

// Valid reports whether r is a known role. RoleAdmin is valid here (e.g. for the
// admin gate), but Register independently restricts the roles a user may grant
// themselves to student/teacher — see RegisterRequest's binding tag.
func (r Role) Valid() bool {
	return r == RoleStudent || r == RoleTeacher || r == RoleAdmin
}

// UserStatus is an account's lifecycle state. A disabled account is rejected at
// the login boundary (password/code login and refresh), so disabling takes
// effect within one access-token TTL.
type UserStatus string

const (
	StatusActive   UserStatus = "active"
	StatusDisabled UserStatus = "disabled"
)

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
	// Status is the account lifecycle state (active/disabled). Defaults to active
	// at the database layer; a disabled account cannot log in or refresh.
	Status UserStatus `json:"status"`
	Roles  []Role     `json:"roles"`
	// LastActiveRole is the role the most recently issued token acts as. It is
	// resumed on login and refresh so a switch-role survives token expiry. Empty
	// until the first token is issued; callers fall back to the default role (see
	// activeRole). Not serialized — the active role travels in auth responses.
	LastActiveRole Role      `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CEFRLevel is a learner's self-assessed difficulty band, from the Common
// European Framework of Reference (A1 easiest … C2 hardest). It scopes which
// study content is served. Empty until the learner finishes onboarding.
type CEFRLevel string

const (
	CEFRA1 CEFRLevel = "A1"
	CEFRA2 CEFRLevel = "A2"
	CEFRB1 CEFRLevel = "B1"
	CEFRB2 CEFRLevel = "B2"
	CEFRC1 CEFRLevel = "C1"
	CEFRC2 CEFRLevel = "C2"
)

// Valid reports whether l is a known CEFR level.
func (l CEFRLevel) Valid() bool {
	switch l {
	case CEFRA1, CEFRA2, CEFRB1, CEFRB2, CEFRC1, CEFRC2:
		return true
	}
	return false
}

// EnglishVariant is the accent and spelling convention a learner studies in: all
// audio and word spellings follow it. The choice is exclusive — British or
// American. Empty until onboarding.
type EnglishVariant string

const (
	VariantBritish  EnglishVariant = "BrE"
	VariantAmerican EnglishVariant = "AmE"
)

// Valid reports whether v is a known English variant.
func (v EnglishVariant) Valid() bool {
	return v == VariantBritish || v == VariantAmerican
}

// LearningSettings holds the two basic choices a learner makes during onboarding.
// They drive the whole study experience, so the frontend needs them on app load.
// A nil *LearningSettings means onboarding is not complete (the app shows the
// onboarding flow); the two fields are always set together, never one alone.
type LearningSettings struct {
	CEFRLevel      CEFRLevel      `json:"cefr_level"`
	EnglishVariant EnglishVariant `json:"english_variant"`
}
