package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestTokenRoundTrip(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour, RealmWeb)
	id := uuid.New()

	tok, err := tm.Generate(id, "student")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	got, err := tm.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Subject != id {
		t.Fatalf("got %s, want %s", got.Subject, id)
	}
	if got.Role != "student" {
		t.Fatalf("role = %q, want student", got.Role)
	}
	if got.Realm != RealmWeb {
		t.Fatalf("realm = %q, want %q", got.Realm, RealmWeb)
	}
}

func TestParseRejectsExpired(t *testing.T) {
	tm := NewTokenManager("test-secret", -time.Minute, RealmWeb) // already expired
	tok, err := tm.Generate(uuid.New(), "student")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := tm.Parse(tok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	signer := NewTokenManager("secret-a", time.Hour, RealmWeb)
	verifier := NewTokenManager("secret-b", time.Hour, RealmWeb)

	tok, _ := signer.Generate(uuid.New(), "student")
	if _, err := verifier.Parse(tok); err == nil {
		t.Fatal("expected token signed with a different secret to be rejected")
	}
}

// A token minted for one realm must be rejected by another realm's manager, even
// when (hypothetically) the secret matched — the realm claim is checked too.
func TestParseRejectsWrongRealm(t *testing.T) {
	web := NewTokenManager("same-secret", time.Hour, RealmWeb)
	adminTM := NewTokenManager("same-secret", time.Hour, RealmAdmin)

	tok, _ := web.Generate(uuid.New(), "student")
	if _, err := adminTM.Parse(tok); err == nil {
		t.Fatal("expected a web-realm token to be rejected by the admin manager")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	tm := NewTokenManager("secret", time.Hour, RealmWeb)
	for _, tok := range []string{"", "abc", "a.b.c", "....", "Bearer xyz"} {
		if _, err := tm.Parse(tok); err == nil {
			t.Errorf("expected %q to be rejected", tok)
		}
	}
}

// A token using the "none" algorithm must never be accepted — this is the
// classic JWT downgrade attack.
func TestParseRejectsNoneAlg(t *testing.T) {
	tm := NewTokenManager("secret", time.Hour, RealmWeb)
	claims := jwt.RegisteredClaims{
		Subject:   uuid.New().String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := tm.Parse(signed); err == nil {
		t.Fatal("expected 'none' algorithm token to be rejected")
	}
}

// A subject that is not a valid UUID must be rejected even if well-signed. The
// token carries the right realm so parsing reaches the subject check.
func TestParseRejectsBadSubject(t *testing.T) {
	tm := NewTokenManager("secret", time.Hour, RealmWeb)
	claims := tokenClaims{
		Realm: RealmWeb,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "not-a-uuid",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	signed, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
	if _, err := tm.Parse(signed); err == nil {
		t.Fatal("expected token with non-UUID subject to be rejected")
	}
}
