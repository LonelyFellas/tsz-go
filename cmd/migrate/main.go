// Command migrate applies all pending database migrations and exits.
//
// Splitting this out of the server lets a deploy run migrations as a separate,
// controlled step *before* rolling out the app, instead of implicitly on every
// boot. It reads the same DATABASE_URL the server uses.
package main

import (
	"os"

	"github.com/darwish/tsz-go/internal/platform/database"
	applog "github.com/darwish/tsz-go/internal/platform/log"
)

func main() {
	logger := applog.New(os.Getenv("LOG_LEVEL"))

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
