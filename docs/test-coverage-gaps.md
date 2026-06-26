# Test Coverage Gaps

Current state: unit + integration tests exist for auth, session, otp, user handler/service/repository, and two E2E flows. The gaps below are ordered by priority.

## 1. Handler — SwitchRole and AddRole ✅

`internal/user/handler_test.go` — added `TestHandler_SwitchRole` and `TestHandler_AddRole`:

- `POST /api/v1/auth/switch-role`
  - 200: valid role switch returns new access token and active_role
  - 403: user does not hold the requested role
  - 400: missing or invalid role field

- `POST /api/v1/auth/roles`
  - 201: new role added, returns access token scoped to new role
  - 409: user already has this role
  - 400: missing or invalid role field

## 2. OTP — second code invalidates the first ✅

`internal/otp/service_test.go` — added `TestService_SecondCodeInvalidatesFirst`:

- Send code twice to the same identifier; verify that the first code is rejected and only the second code is accepted.

## 3. E2E — OTP login flow ✅

Already covered by `TestE2E_RegisterLoginMe` in `internal/platform/httpserver/e2e_integration_test.go` (lines 219–232):
`POST /auth/send-code` → read code from `MockSender` → `POST /auth/login/code` → assert 200, then assert replay returns 401.

## 4. Middleware — active_role written to context ✅

`internal/platform/httpserver/middleware_test.go` — added `TestAuthRequired_ContextRole`:

- After a valid token is parsed, verify that `auth.role` is correctly set on the gin context so downstream handlers can read it.

## 5. Middleware — RequestLogger ✅

`internal/platform/httpserver/middleware_test.go` — added `TestRequestLogger`:

- Captures slog JSON output and asserts one `http_request` line is emitted with `method`, `path`, `status` (read after `Next()`), `duration_ms` and `ip`.
- Verifies the middleware calls `Next()` so the downstream handler runs.

## 6. user/model — Role.Valid and defaultRole ✅

`internal/user/service_test.go` — added `TestRole_Valid` and `TestDefaultRole`:

- `Role.Valid()`: known roles valid; unknown / empty / wrong-case strings invalid.
- `defaultRole()`: empty slice → zero `Role`; otherwise the first (stably-ordered) role wins.

---

## Remaining (intentionally not unit-tested)

The three repository layers (`user`, `session`, `otp`) are covered by `//go:build integration` tests against a live Postgres rather than mocked unit tests — pgx is impractical to mock and the integration tests exercise the real SQL, constraints, and transactions. This is a deliberate strategy, not a gap.
