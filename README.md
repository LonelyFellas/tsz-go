# tsz-go

A pragmatic Go backend scaffold for a toC product at small scale: a **modular
monolith** (Gin) on a single **Postgres**, deployed via Docker Compose. Built to
move fast on business logic, not to prematurely scale.

## Stack

| Concern    | Choice                                  |
|------------|-----------------------------------------|
| HTTP       | Gin                                     |
| DB         | PostgreSQL via pgx/v5 (`pgxpool`)       |
| Migrations | golang-migrate, embedded + auto-run     |
| Auth       | JWT (HS256) + bcrypt                    |
| Config     | environment variables                   |
| Logging    | `log/slog` (structured JSON, stdlib)    |

## Layout

```
cmd/server/            entry point — wires deps & starts the server
internal/
  config/              env-based configuration
  auth/                JWT issuing/parsing
  user/                user domain (vertical slice)
    handler.go         HTTP layer
    service.go         business logic
    repository.go      data access (hand-written SQL over pgx)
    model.go
  platform/            cross-cutting infrastructure
    database/          pgx pool + embedded migrations
    httpserver/        router + middleware
```

Dependency direction is inward: handler → service → repository.

## Run locally

```bash
# 1. start everything (app + postgres)
make up           # docker compose up -d --build

# 2. or run the app on the host against a local postgres
cp .env.example .env       # then export the vars or use direnv
make run
```

## Migrations

Migrations are a **standalone step**, not run on every app boot — this keeps
production deploys controlled (migrate first, then roll out the app).

```bash
make migrate   # runs ./cmd/migrate against $DATABASE_URL
```

`docker compose up` runs a one-shot `migrate` service before the app starts, so
the local stack is still ready out of the box. To make the server migrate on
boot (handy for quick local runs), set `AUTO_MIGRATE=true`.

## Testing

```bash
make test              # unit tests, no DB required
make test-integration  # unit + integration tests against a live Postgres
```

Integration tests run against a dedicated `tsz_test` database (created automatically
if missing) so they never pollute the `tsz` development database. Override the target
DB by exporting `DATABASE_URL` before running `make test-integration`.

## API

```bash
# register
curl -X POST localhost:8080/api/v1/auth/register \
  -H 'content-type: application/json' \
  -d '{"email":"a@b.com","password":"password123","display_name":"Alice"}'

# login
curl -X POST localhost:8080/api/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"a@b.com","password":"password123"}'

# authenticated
curl localhost:8080/api/v1/me -H "authorization: Bearer <token>"
```

## Adding a feature

1. `make migrate-create name=add_posts` → fill in the up/down SQL.
2. Create `internal/<domain>/` with model/repository/service/handler.
3. Register routes in `internal/platform/httpserver/router.go` and wire the deps
   in `cmd/server/main.go`.

## Upgrading the data layer to sqlc (later)

The repository is the only place that touches SQL. To adopt
[sqlc](https://sqlc.dev) without touching the service layer:

1. Add `sqlc.yaml` and a `query.sql` per domain.
2. `sqlc generate` to produce typed query code.
3. Swap the bodies of the `Repository` methods to call the generated code —
   their signatures stay the same, so `service.go` is untouched.

## When to scale up (not yet)

- DB CPU consistently high / slow queries → add indexes first, then a read replica.
- Slow background work (email, reports) → add [asynq](https://github.com/hibiken/asynq) (Redis).
- Hot read paths hammering the DB → add a Redis cache.
- Don't reach for microservices/k8s until org & deploy boundaries demand it.
