package auth

import (
	"testing"
	"time"

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
