package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestTokenRoundTrip(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour)
	id := uuid.New()

	tok, err := tm.Generate(id)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	got, err := tm.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != id {
		t.Fatalf("got %s, want %s", got, id)
	}
}

func TestParseRejectsExpired(t *testing.T) {
	tm := NewTokenManager("test-secret", -time.Minute) // already expired
	tok, err := tm.Generate(uuid.New())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := tm.Parse(tok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	signer := NewTokenManager("secret-a", time.Hour)
	verifier := NewTokenManager("secret-b", time.Hour)

	tok, _ := signer.Generate(uuid.New())
	if _, err := verifier.Parse(tok); err == nil {
		t.Fatal("expected token signed with a different secret to be rejected")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	tm := NewTokenManager("secret", time.Hour)
	for _, tok := range []string{"", "abc", "a.b.c", "....", "Bearer xyz"} {
		if _, err := tm.Parse(tok); err == nil {
			t.Errorf("expected %q to be rejected", tok)
		}
	}
}

// A token using the "none" algorithm must never be accepted — this is the
// classic JWT downgrade attack.
func TestParseRejectsNoneAlg(t *testing.T) {
	tm := NewTokenManager("secret", time.Hour)
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

// A subject that is not a valid UUID must be rejected even if well-signed.
func TestParseRejectsBadSubject(t *testing.T) {
	tm := NewTokenManager("secret", time.Hour)
	claims := jwt.RegisteredClaims{
		Subject:   "not-a-uuid",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	signed, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
	if _, err := tm.Parse(signed); err == nil {
		t.Fatal("expected token with non-UUID subject to be rejected")
	}
}
