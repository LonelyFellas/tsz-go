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
| Auth       | JWT (HS256) + bcrypt, role-aware        |
| Config     | environment variables                   |
| Logging    | `log/slog` (structured JSON, stdlib)    |

## Layout

```
cmd/server/            entry point — wires deps & starts the server
internal/
  config/              env-based configuration
  auth/                JWT issuing/parsing
  session/             refresh tokens: service + repository (rotation, single-device)
  otp/                 one-time codes: service, repository, pluggable Sender (mock)
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

## Roles & identity

Auth identity and role are **decoupled**. A single account (`users`) can hold
more than one role — `student`, `teacher`, or both — tracked in `user_roles`,
with role-specific data living in `student_profiles` / `teacher_profiles`.

The **active role** travels in the JWT, not on the user record. Switching
identity therefore means re-issuing a token scoped to a different role; there is
no server-side session to update. A user picks an initial role at registration,
can acquire a second one later, and switches between the roles they hold.

## Login methods & verification codes

Phone is the **required** identifier at registration; email is optional. A user
can log in with **either** identifier (phone or email), via **either** method:

- **password** — `POST /auth/login`
- **one-time code** — `POST /auth/send-code` then `POST /auth/login/code`

Codes (`verification_codes`) are single-use and time-boxed (`OTP_CODE_TTL`,
default 5m). Delivery goes through a `Sender` (`internal/otp`); today it's a
**mock** that just logs the code (`"otp_code_sent" ... code=...`) — swap in a
real SMS/email provider in `cmd/server/main.go` without touching call sites. The
channel is inferred from the target: an `@` → email, otherwise SMS.

## Tokens & single-device sessions

Every login (register, password, code) returns **two** tokens:

- **access token** — a short-lived JWT (`JWT_TTL`, default **15m**). It carries
  the active role and is verified locally by the middleware on every request —
  **never** checked against the DB, so the auth path stays stateless.
- **refresh token** — a long-lived (`REFRESH_TOKEN_TTL`, default **30d**) opaque
  random string. Only its SHA-256 hash is stored (`refresh_tokens`, in
  `internal/session`). It is used solely against the low-frequency
  `/auth/refresh` and `/auth/logout` endpoints.

`POST /auth/refresh` rotates the refresh token (the old one is single-use) and
mints a fresh access token. `POST /auth/logout` revokes a refresh token and is
idempotent.

Login is **strict single-device**: issuing a refresh token revokes the user's
other refresh tokens, so a previous device is kicked off as soon as its access
token expires and its next refresh returns 401 (delay ≤ the access TTL).

`switch-role` / `roles` re-sign only the **access** token; the refresh token is
untouched, so a role change takes effect within at most one access TTL.

## API

Full reference: [docs/api.md](docs/api.md). Interactive docs (Swagger UI) are
served at **`/docs`** when `DOCS_ENABLED=true` (default), backed by the OpenAPI
spec at [docs/openapi.yaml](docs/openapi.yaml) (also reachable at
`/docs/openapi.yaml`). Set `DOCS_ENABLED=false` to hide them in production.

```bash
# register — phone + role required; email optional
curl -X POST localhost:8080/api/v1/auth/register \
  -H 'content-type: application/json' \
  -d '{"phone":"13800138000","email":"a@b.com","password":"password123","display_name":"Alice","role":"student"}'
# → 201 {"user":{...,"phone":"13800138000","roles":["student"]},
#        "access_token":"<jwt>","refresh_token":"<opaque>","active_role":"student"}

# password login — identifier is a phone OR email
curl -X POST localhost:8080/api/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"identifier":"13800138000","password":"password123"}'
# → 200 {"user":{...},"access_token":"<jwt>","refresh_token":"<opaque>","active_role":"student"}

# code login — step 1: request a code (always 200; mock logs the code)
curl -X POST localhost:8080/api/v1/auth/send-code \
  -H 'content-type: application/json' \
  -d '{"identifier":"13800138000"}'
# code login — step 2: exchange the code for access+refresh (401 if wrong/expired/used)
curl -X POST localhost:8080/api/v1/auth/login/code \
  -H 'content-type: application/json' \
  -d '{"identifier":"13800138000","code":"123456"}'

# refresh — rotate the refresh token, get a fresh access token (401 if invalid/revoked/expired)
curl -X POST localhost:8080/api/v1/auth/refresh \
  -H 'content-type: application/json' \
  -d '{"refresh_token":"<opaque>"}'
# → 200 {"access_token":"<jwt>","refresh_token":"<rotated opaque>"}

# logout — revoke a refresh token (204; idempotent)
curl -X POST localhost:8080/api/v1/auth/logout \
  -H 'content-type: application/json' \
  -d '{"refresh_token":"<opaque>"}'

# current user — returns all held roles plus the active one
curl localhost:8080/api/v1/me -H "authorization: Bearer <access_token>"
# → 200 {"user":{...,"roles":["student","teacher"]},"active_role":"student"}

# acquire a second identity — returns an access token already switched to it (409 if already held)
curl -X POST localhost:8080/api/v1/auth/roles \
  -H "authorization: Bearer <access_token>" -H 'content-type: application/json' \
  -d '{"role":"teacher"}'

# switch active role — to one the user already holds (403 if not held)
curl -X POST localhost:8080/api/v1/auth/switch-role \
  -H "authorization: Bearer <access_token>" -H 'content-type: application/json' \
  -d '{"role":"teacher"}'
# → 200 {"access_token":"<jwt scoped to teacher>","active_role":"teacher"}
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
