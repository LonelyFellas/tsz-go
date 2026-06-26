// Package config loads runtime configuration from environment variables.
// At this scale a tiny manual loader beats pulling in a config framework.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
	// JWTTTL is the access-token lifetime. Short by design (default 15m): an
	// access token stays stateless and is never checked against the DB, so a
	// short TTL bounds how long a revoked session keeps working.
	JWTTTL time.Duration
	// RefreshTokenTTL is the refresh-token lifetime (default 30d). Refresh tokens
	// are tracked server-side and rotated on every use.
	RefreshTokenTTL time.Duration
	OTPCodeTTL      time.Duration
	Env             string
	// AutoMigrate runs migrations on server startup. Off by default so
	// production migrates as a separate step (see ./cmd/migrate); handy to
	// enable locally via AUTO_MIGRATE=true.
	AutoMigrate bool
}

func Load() (Config, error) {
	cfg := Config{
		Port:            getenv("PORT", "8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		JWTSecret:       os.Getenv("JWT_SECRET"),
		JWTTTL:          getdur("JWT_TTL", 15*time.Minute),
		RefreshTokenTTL: getdur("REFRESH_TOKEN_TTL", 720*time.Hour),
		OTPCodeTTL:      getdur("OTP_CODE_TTL", 5*time.Minute),
		Env:             getenv("APP_ENV", "development"),
		AutoMigrate:     getbool("AUTO_MIGRATE", false),
	}

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return cfg, fmt.Errorf("config: JWT_SECRET is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getdur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func getbool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
