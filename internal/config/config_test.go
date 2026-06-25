package config

import (
	"testing"
	"time"
)

// setRequired sets the two mandatory vars so individual tests can focus on the
// field under test. t.Setenv auto-restores after the test.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://app:app@localhost:5432/tsz")
	t.Setenv("JWT_SECRET", "a-secret")
}

func TestLoad_Success(t *testing.T) {
	setRequired(t)
	t.Setenv("PORT", "9090")
	t.Setenv("JWT_TTL", "2h")
	t.Setenv("APP_ENV", "production")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q, want 9090", cfg.Port)
	}
	if cfg.JWTTTL != 2*time.Hour {
		t.Errorf("JWTTTL = %v, want 2h", cfg.JWTTTL)
	}
	if cfg.Env != "production" {
		t.Errorf("Env = %q, want production", cfg.Env)
	}
}

func TestLoad_Defaults(t *testing.T) {
	setRequired(t)
	// empty values must fall back to defaults
	t.Setenv("PORT", "")
	t.Setenv("JWT_TTL", "")
	t.Setenv("APP_ENV", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port default = %q, want 8080", cfg.Port)
	}
	if cfg.JWTTTL != 24*time.Hour {
		t.Errorf("JWTTTL default = %v, want 24h", cfg.JWTTTL)
	}
	if cfg.Env != "development" {
		t.Errorf("Env default = %q, want development", cfg.Env)
	}
}

func TestLoad_InvalidDurationFallsBack(t *testing.T) {
	setRequired(t)
	t.Setenv("JWT_TTL", "not-a-duration")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JWTTTL != 24*time.Hour {
		t.Errorf("JWTTTL = %v, want 24h fallback on parse error", cfg.JWTTTL)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "a-secret")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when DATABASE_URL is missing")
	}
}

func TestLoad_MissingJWTSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when JWT_SECRET is missing")
	}
}
