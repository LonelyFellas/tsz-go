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
	// OTPResendCooldown is the minimum gap between two codes to the same target
	// (default 60s); OTPDailyLimit caps codes per target per rolling 24h (default
	// 10). Together they bound SMS/email cost and abuse. 0 disables either limit.
	OTPResendCooldown time.Duration
	OTPDailyLimit     int
	// AuthRateLimitPerMin caps requests per client IP to the public auth
	// endpoints over a rolling minute (default 30); AuthRateBurst is the
	// short-burst allowance on top (default 10). This is an IP-level layer over
	// the per-target OTP limits, blunting credential stuffing and a single host
	// cycling identifiers. 0 (or less) for AuthRateLimitPerMin disables it.
	AuthRateLimitPerMin int
	AuthRateBurst       int
	Env                 string
	// LogLevel sets the minimum slog level (debug/info/warn/error). Defaults to
	// info; bump to debug to investigate an incident without recompiling.
	LogLevel string
	// AutoMigrate runs migrations on server startup. Off by default so
	// production migrates as a separate step (see ./cmd/migrate); handy to
	// enable locally via AUTO_MIGRATE=true.
	AutoMigrate bool
	// DocsEnabled mounts the Swagger UI (/docs) and OpenAPI spec
	// (/docs/openapi.yaml). On by default; set DOCS_ENABLED=false to hide the API
	// surface in production.
	DocsEnabled bool
}

func Load() (Config, error) {
	cfg := Config{
		Port:                getenv("PORT", "8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		JWTSecret:           os.Getenv("JWT_SECRET"),
		JWTTTL:              getdur("JWT_TTL", 15*time.Minute),
		RefreshTokenTTL:     getdur("REFRESH_TOKEN_TTL", 720*time.Hour),
		OTPCodeTTL:          getdur("OTP_CODE_TTL", 5*time.Minute),
		OTPResendCooldown:   getdur("OTP_RESEND_COOLDOWN", 60*time.Second),
		OTPDailyLimit:       getint("OTP_DAILY_LIMIT", 10),
		AuthRateLimitPerMin: getint("AUTH_RATE_LIMIT_PER_MIN", 30),
		AuthRateBurst:       getint("AUTH_RATE_BURST", 10),
		Env:                 getenv("APP_ENV", "development"),
		LogLevel:            getenv("LOG_LEVEL", "info"),
		AutoMigrate:         getbool("AUTO_MIGRATE", false),
		DocsEnabled:         getbool("DOCS_ENABLED", true),
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

func getint(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
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
