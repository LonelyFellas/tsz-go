#!/usr/bin/env bash
# One-shot deploy/update for the split (app-only) production host.
#
#   ./deploy.sh
#
# Pulls the latest code, rebuilds the image, rolls the containers, and waits
# for the app to report ready (which also confirms the DB is reachable). Run it
# from the app host; secrets come from the sibling .env (see docker-compose.prod.yml).
set -euo pipefail

cd "$(dirname "$0")"

COMPOSE_FILE="docker-compose.prod.yml"
HEALTH_URL="http://localhost:8080/readyz"
HEALTH_TIMEOUT=90   # seconds to wait for /readyz after the roll

if [[ ! -f .env ]]; then
  echo "ERROR: .env not found next to this script. Create it first (DATABASE_URL, JWT_SECRET, ADMIN_JWT_SECRET)." >&2
  exit 1
fi

echo "==> Pulling latest code"
git pull --ff-only

echo "==> Building and rolling containers"
# --build recompiles the image; the one-shot migrate service runs to completion
# before app starts, so schema changes are applied before the new code serves.
docker compose -f "$COMPOSE_FILE" up -d --build

echo "==> Waiting for ${HEALTH_URL} (up to ${HEALTH_TIMEOUT}s)"
deadline=$(( SECONDS + HEALTH_TIMEOUT ))
until curl -fsS -o /dev/null "$HEALTH_URL"; do
  if (( SECONDS >= deadline )); then
    echo "ERROR: app did not become ready within ${HEALTH_TIMEOUT}s. Recent logs:" >&2
    docker compose -f "$COMPOSE_FILE" logs --tail=40 app >&2
    exit 1
  fi
  sleep 2
done

echo "==> Deployed. Container status:"
docker compose -f "$COMPOSE_FILE" ps

# Reclaim disk from the image layers the rebuild just orphaned. Safe: only
# dangling (untagged) images are removed.
echo "==> Pruning dangling images"
docker image prune -f >/dev/null

echo "==> Done."
