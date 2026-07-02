// Package config loads runtime configuration from environment variables.
// At this scale a tiny manual loader beats pulling in a config framework.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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
	// AdminJWTSecret signs back-office (admin realm) access tokens. It MUST differ
	// from JWTSecret: the separate key is what makes a web token fail verification
	// on the admin API and vice versa. AdminJWTTTL / AdminRefreshTokenTTL mirror the
	// web TTLs but can be set shorter for the higher-privilege realm.
	AdminJWTSecret       string
	AdminJWTTTL          time.Duration
	AdminRefreshTokenTTL time.Duration
	OTPCodeTTL           time.Duration
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
	// MetricsEnabled exposes Prometheus metrics at /metrics (default true).
	MetricsEnabled bool
	// TracingEndpoint is the OTLP/HTTP collector address as host:port (e.g.
	// localhost:4318). Empty (the default) disables tracing entirely: the global
	// tracer stays a no-op, so the otelgin spans cost almost nothing and nothing
	// is exported. Set it to start shipping traces.
	TracingEndpoint string
	// TracingInsecure sends traces over plain HTTP to the collector (default
	// true); set TRACING_INSECURE=false when the collector terminates TLS.
	TracingInsecure bool
	// ServiceName labels metrics and traces (default "tsz-go").
	ServiceName string
	// CookieSecure marks the refresh-token cookies Secure (HTTPS-only). Defaults
	// to true outside development. Set COOKIE_SECURE=false ONLY on a plain-HTTP
	// test host (bare-IP access before TLS): browsers silently drop Secure
	// cookies on http origins, so the refresh cookie never lands and session
	// restore 401s on every page reload. Never set false in production.
	CookieSecure bool
	// TrustedProxies lists the proxy CIDRs/IPs whose X-Forwarded-For header is
	// honored when resolving the client IP. Empty (the default) trusts none, so
	// ClientIP() falls back to the direct peer address and a client cannot spoof
	// XFF to mint a fresh rate-limit bucket per request. Behind a load balancer,
	// set TRUSTED_PROXIES to the LB's subnet(s) so the real client IP is used.
	TrustedProxies []string
}

func Load() (Config, error) {
	cfg := Config{
		Port:                 getenv("PORT", "8080"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		JWTSecret:            os.Getenv("JWT_SECRET"),
		JWTTTL:               getdur("JWT_TTL", 15*time.Minute),
		RefreshTokenTTL:      getdur("REFRESH_TOKEN_TTL", 720*time.Hour),
		AdminJWTSecret:       os.Getenv("ADMIN_JWT_SECRET"),
		AdminJWTTTL:          getdur("ADMIN_JWT_TTL", 15*time.Minute),
		AdminRefreshTokenTTL: getdur("ADMIN_REFRESH_TOKEN_TTL", 720*time.Hour),
		OTPCodeTTL:           getdur("OTP_CODE_TTL", 5*time.Minute),
		OTPResendCooldown:    getdur("OTP_RESEND_COOLDOWN", 60*time.Second),
		OTPDailyLimit:        getint("OTP_DAILY_LIMIT", 10),
		AuthRateLimitPerMin:  getint("AUTH_RATE_LIMIT_PER_MIN", 30),
		AuthRateBurst:        getint("AUTH_RATE_BURST", 10),
		Env:                  getenv("APP_ENV", "development"),
		LogLevel:             getenv("LOG_LEVEL", "info"),
		AutoMigrate:          getbool("AUTO_MIGRATE", false),
		DocsEnabled:          getbool("DOCS_ENABLED", true),
		MetricsEnabled:       getbool("METRICS_ENABLED", true),
		TracingEndpoint:      os.Getenv("TRACING_ENDPOINT"),
		TracingInsecure:      getbool("TRACING_INSECURE", true),
		ServiceName:          getenv("SERVICE_NAME", "tsz-go"),
		TrustedProxies:       getcsv("TRUSTED_PROXIES"),
	}
	// Depends on cfg.Env, so it can't sit inside the literal above.
	cfg.CookieSecure = getbool("COOKIE_SECURE", cfg.Env != "development")

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return cfg, fmt.Errorf("config: JWT_SECRET is required")
	}
	if cfg.AdminJWTSecret == "" {
		return cfg, fmt.Errorf("config: ADMIN_JWT_SECRET is required")
	}
	if cfg.AdminJWTSecret == cfg.JWTSecret {
		return cfg, fmt.Errorf("config: ADMIN_JWT_SECRET must differ from JWT_SECRET (realm isolation)")
	}
	// Reject a malformed proxy list at startup rather than silently falling back
	// to trusting nothing (which would route every request through one bucket
	// behind an LB). Accept CIDR blocks and bare IPs, matching what gin allows.
	for _, p := range cfg.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil && net.ParseIP(p) == nil {
			return cfg, fmt.Errorf("config: TRUSTED_PROXIES entry %q is not a valid CIDR or IP", p)
		}
	}
	return cfg, nil
}

// getcsv reads a comma-separated env var into a trimmed, non-empty slice. An
// unset or blank value yields nil.
func getcsv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
