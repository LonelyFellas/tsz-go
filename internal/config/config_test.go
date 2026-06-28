package config

import (
	"testing"
	"time"
)

// setRequired sets the mandatory vars so individual tests can focus on the field
// under test. ADMIN_JWT_SECRET must differ from JWT_SECRET (realm isolation).
// t.Setenv auto-restores after the test.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://app:app@localhost:5432/tsz")
	t.Setenv("JWT_SECRET", "a-secret")
	t.Setenv("ADMIN_JWT_SECRET", "a-different-admin-secret")
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

func TestLoad_TrustedProxies(t *testing.T) {
	t.Run("parses and trims a CSV list", func(t *testing.T) {
		setRequired(t)
		t.Setenv("TRUSTED_PROXIES", " 10.0.0.0/8 , 192.168.1.1 ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"10.0.0.0/8", "192.168.1.1"}
		if len(cfg.TrustedProxies) != len(want) {
			t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
		}
		for i, p := range want {
			if cfg.TrustedProxies[i] != p {
				t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], p)
			}
		}
	})

	t.Run("unset trusts none", func(t *testing.T) {
		setRequired(t)
		t.Setenv("TRUSTED_PROXIES", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.TrustedProxies != nil {
			t.Errorf("TrustedProxies = %v, want nil", cfg.TrustedProxies)
		}
	})

	t.Run("rejects a malformed entry", func(t *testing.T) {
		setRequired(t)
		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8,not-an-ip")
		if _, err := Load(); err == nil {
			t.Fatal("expected an error for a malformed TRUSTED_PROXIES entry, got nil")
		}
	})
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
	if cfg.JWTTTL != 15*time.Minute {
		t.Errorf("JWTTTL default = %v, want 15m", cfg.JWTTTL)
	}
	if cfg.RefreshTokenTTL != 720*time.Hour {
		t.Errorf("RefreshTokenTTL default = %v, want 720h", cfg.RefreshTokenTTL)
	}
	if cfg.AdminJWTTTL != 15*time.Minute {
		t.Errorf("AdminJWTTTL default = %v, want 15m", cfg.AdminJWTTTL)
	}
	if cfg.AdminRefreshTokenTTL != 720*time.Hour {
		t.Errorf("AdminRefreshTokenTTL default = %v, want 720h", cfg.AdminRefreshTokenTTL)
	}
	if cfg.Env != "development" {
		t.Errorf("Env default = %q, want development", cfg.Env)
	}
	if cfg.AuthRateLimitPerMin != 30 {
		t.Errorf("AuthRateLimitPerMin default = %d, want 30", cfg.AuthRateLimitPerMin)
	}
	if cfg.AuthRateBurst != 10 {
		t.Errorf("AuthRateBurst default = %d, want 10", cfg.AuthRateBurst)
	}
	if !cfg.MetricsEnabled {
		t.Error("MetricsEnabled default = false, want true")
	}
	if cfg.TracingEndpoint != "" {
		t.Errorf("TracingEndpoint default = %q, want empty (tracing off)", cfg.TracingEndpoint)
	}
	if cfg.ServiceName != "tsz-go" {
		t.Errorf("ServiceName default = %q, want tsz-go", cfg.ServiceName)
	}
}

func TestLoad_InvalidDurationFallsBack(t *testing.T) {
	setRequired(t)
	t.Setenv("JWT_TTL", "not-a-duration")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JWTTTL != 15*time.Minute {
		t.Errorf("JWTTTL = %v, want 15m fallback on parse error", cfg.JWTTTL)
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

func TestLoad_MissingAdminJWTSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "a-secret")
	t.Setenv("ADMIN_JWT_SECRET", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when ADMIN_JWT_SECRET is missing")
	}
}

// The two signing keys must differ, or a web token could verify on the admin API.
func TestLoad_AdminSecretMustDifferFromWeb(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "same-secret")
	t.Setenv("ADMIN_JWT_SECRET", "same-secret")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when ADMIN_JWT_SECRET equals JWT_SECRET")
	}
}
