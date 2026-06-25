// Command migrate applies all pending database migrations and exits.
//
// Splitting this out of the server lets a deploy run migrations as a separate,
// controlled step *before* rolling out the app, instead of implicitly on every
// boot. It reads the same DATABASE_URL the server uses.
package main

import (
	"log/slog"
	"os"

	"github.com/darwish/tsz-go/internal/platform/database"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	if err := database.Migrate(url); err != nil {
		logger.Error("migrations failed", "err", err)
		os.Exit(1)
	}
	logger.Info("migrations applied")
}
