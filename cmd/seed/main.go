// Command seed bootstraps the first back-office admin and exits.
//
// Admin is never self-registered (registration is limited to student/teacher),
// so the first admin is created out of band by this one-shot command — mirroring
// ./cmd/migrate's "separate, controlled step" model rather than running on every
// boot. It is idempotent: re-running never duplicates an account and never errors
// if the admin already exists, so it is safe to wire into a deploy step.
//
// It reads its configuration straight from the environment (no config.Load), the
// same way ./cmd/migrate does:
//
//   - DATABASE_URL          (required) — same DSN the server uses
//   - SEED_ADMIN_PHONE      (required) — the admin's login phone
//   - SEED_ADMIN_PASSWORD   (required) — the admin's password (used only on create)
//   - SEED_ADMIN_DISPLAY_NAME (optional) — defaults to "Administrator"
package main

import (
	"context"
	"os"

	"github.com/darwish/tsz-go/internal/platform/database"
	applog "github.com/darwish/tsz-go/internal/platform/log"
	"github.com/darwish/tsz-go/internal/user"
)

const defaultAdminDisplayName = "Administrator"

func main() {
	logger := applog.New(os.Getenv("LOG_LEVEL"))

	url := os.Getenv("DATABASE_URL")
	phone := os.Getenv("SEED_ADMIN_PHONE")
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	displayName := os.Getenv("SEED_ADMIN_DISPLAY_NAME")
	if displayName == "" {
		displayName = defaultAdminDisplayName
	}

	switch {
	case url == "":
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	case phone == "":
		logger.Error("SEED_ADMIN_PHONE is required")
		os.Exit(1)
	case password == "":
		logger.Error("SEED_ADMIN_PASSWORD is required")
		os.Exit(1)
	}

	ctx := context.Background()

	pool, err := database.Connect(ctx, url)
	if err != nil {
		logger.Error("connect to database failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// SeedAdmin only touches the repository, so the token/codes/sessions
	// dependencies of the full service are irrelevant here and passed as nil.
	svc := user.NewService(user.NewRepository(pool), nil, nil, nil)

	u, err := svc.SeedAdmin(ctx, phone, password, displayName)
	if err != nil {
		logger.Error("seed admin failed", "err", err)
		os.Exit(1)
	}

	logger.Info("admin seeded", "id", u.ID, "phone", u.Phone)
}
