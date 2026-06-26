package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/config"
	"github.com/darwish/tsz-go/internal/otp"
	"github.com/darwish/tsz-go/internal/platform/database"
	"github.com/darwish/tsz-go/internal/platform/httpserver"
	applog "github.com/darwish/tsz-go/internal/platform/log"
	"github.com/darwish/tsz-go/internal/session"
	"github.com/darwish/tsz-go/internal/user"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// run wires up every dependency and starts the HTTP server. Keeping the
// assembly in one place (instead of a DI framework) makes the dependency
// graph obvious at this project size.
func run() error {
	// Bootstrap a structured logger before config so even config errors are
	// JSON; once config is loaded, re-set it at the configured level.
	slog.SetDefault(applog.New("info"))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(applog.New(cfg.LogLevel))

	ctx := context.Background()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrations run as a separate step in production (see ./cmd/migrate). Only
	// migrate on boot when explicitly opted in via AUTO_MIGRATE=true.
	if cfg.AutoMigrate {
		if err := database.Migrate(cfg.DatabaseURL); err != nil {
			return err
		}
		slog.Info("migrations applied")
	}

	// Dependency wiring: repository -> service -> handler.
	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)

	// Verification codes (OTP). The mock sender just logs the code; swap in a
	// real SMS/email provider here when that integration lands.
	otpService := otp.NewService(otp.NewRepository(pool), otp.NewMockSender(), cfg.OTPCodeTTL, cfg.OTPResendCooldown, cfg.OTPDailyLimit)

	// Refresh tokens back the access/refresh scheme and strict single-device login.
	sessionService := session.NewService(session.NewRepository(pool), cfg.RefreshTokenTTL)

	userRepo := user.NewRepository(pool)
	userService := user.NewService(userRepo, tokenManager, otpService, sessionService)
	// Refresh tokens ride in an HttpOnly cookie. Secure is on outside dev so the
	// cookie is HTTPS-only in production; MaxAge mirrors the refresh-token TTL.
	userHandler := user.NewHandler(userService, user.CookieConfig{
		Secure: cfg.Env != "development",
		MaxAge: cfg.RefreshTokenTTL,
	}, cfg.JWTTTL, cfg.RefreshTokenTTL)

	router := httpserver.NewRouter(httpserver.Deps{
		TokenManager: tokenManager,
		UserHandler:  userHandler,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", srv.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return err
	case <-stop:
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
