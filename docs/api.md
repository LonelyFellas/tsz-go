# API Reference

REST API for the tsz-go backend. All endpoints are JSON over HTTP.

- **Base URL:** `http://<host>:<port>` (e.g. `http://localhost:8080`)
- **API prefix:** `/api/v1`
- **Content-Type:** `application/json` for all request bodies.

## Authentication

The API uses **JWT access tokens** + **refresh tokens**, stored differently by design (see [auth-token-storage.md](auth-token-storage.md) for the full rationale and frontend guide):

- **Access token** — returned in the JSON body. Lifetime **15 minutes** (default, `JWT_TTL`). Carries the user ID and active role. Send it on protected endpoints via the header `Authorization: Bearer <access_token>`. Keep it in memory, not `localStorage`.
- **Refresh token** — delivered as an **HttpOnly, Secure, SameSite=Strict cookie** (scoped to `Path=/api/v1/auth`), **not in the body**. Lifetime **30 days** (default, `REFRESH_TOKEN_TTL`). The browser sends it automatically to `/auth/refresh` and `/auth/logout`; frontend JS never reads or stores it. Make requests with credentials (`withCredentials: true` / `credentials: 'include'`).

> **Why a cookie?** A refresh token is the long-lived "master key". HttpOnly keeps JS (and thus XSS) from ever reading it; SameSite=Strict + the narrow path defend against CSRF. The short-lived access token stays in memory, so an XSS at worst grabs a token that expires in ≤15 min.

### Single-device login

Login is **strict single-device**. Each successful login/register issues a refresh token and **revokes the user's previous sessions**. When the refresh cookie is used, it is **rotated** (the old one stops working and a new one is set via `Set-Cookie`). A device that was logged out elsewhere keeps working only until its access token expires (≤ 15 min), after which its next refresh fails with `401`.

> **Frontend implication:** keep the `access_token` in memory; the refresh token rides in the cookie automatically. On a `401` from a protected endpoint, call `/auth/refresh` (no body), save the **new** `access_token` it returns, and retry. If refresh itself returns `401`, the session is dead — send the user back to login.

## Handling 401 errors

All `401` responses share the same shape but come from different sources. Use the `error` field to tell them apart.

### All possible 401 error values

| `error` value | Source | Meaning |
|---|---|---|
| `invalid credentials` | `/auth/login`, `/auth/login/code` | Wrong phone/email or password/code; also returned after 5 consecutive wrong code attempts (code is locked) |
| `missing refresh token` | `/auth/refresh` | No refresh-token cookie sent (requires `withCredentials`) |
| `invalid refresh token` | `/auth/refresh` | Refresh token expired, revoked, or tampered; stale cookie is cleared |
| `missing or malformed authorization header` | Any authenticated endpoint | `Authorization` header is absent or not `Bearer <token>` |
| `invalid or expired token` | Any authenticated endpoint | Access token has expired or is tampered |

### Decision logic for the frontend

```
response.status === 401 ?
  ├── request was /auth/login or /auth/login/code
  │     → show "手机号或密码错误" to the user (never tell them which one is wrong)
  │
  ├── request was /auth/refresh
  │     → session is over, clear tokens, redirect to login
  │
  └── any other (authenticated) endpoint
        error === "missing or malformed authorization header"
          → bug in your code, access token was not attached — fix the request
        error === "invalid or expired token"
          → call /auth/refresh to get new tokens, then retry the original request
          → if /auth/refresh also returns 401, session is over → redirect to login
```

### Recommended implementation

```ts
// http.ts
let accessToken: string | null = null
export const setAccessToken = (t: string | null) => { accessToken = t }

// withCredentials makes the browser send/receive the refresh cookie
const http = axios.create({ baseURL: '/api/v1', withCredentials: true })

// Attach the in-memory access token to every request
http.interceptors.request.use(config => {
  if (accessToken) config.headers.Authorization = `Bearer ${accessToken}`
  return config
})

// Handle 401 centrally
http.interceptors.response.use(
  res => res,
  async err => {
    if (!axios.isAxiosError(err)) return Promise.reject(err)

    const status = err.response?.status
    const errorMsg = err.response?.data?.error
    const url = err.config?.url ?? ''

    // ① Login / code-login: wrong credentials — let the calling function handle it
    if (status === 401 && (url.includes('/auth/login') || url.includes('/auth/login/code'))) {
      return Promise.reject(err)
    }

    // ② Refresh failed — session is over
    if (status === 401 && url.includes('/auth/refresh')) {
      setAccessToken(null)
      window.location.href = '/login'
      return Promise.reject(err)
    }

    // ③ Access token expired on a protected endpoint — try to refresh once
    if (status === 401 && errorMsg === 'invalid or expired token' && !err.config._retry) {
      err.config._retry = true
      try {
        const { data } = await http.post('/auth/refresh')   // no body; refresh cookie is sent automatically
        setAccessToken(data.access_token)                    // rotated refresh token comes back as a cookie
        err.config.headers.Authorization = `Bearer ${data.access_token}`
        return http(err.config)   // retry original request
      } catch {
        // refresh itself returned 401 → handled by case ② above on the retry
        return Promise.reject(err)
      }
    }

    return Promise.reject(err)
  }
)

export default http
```

```ts
// api/auth.ts — login only needs to handle its own 401
import http, { setAccessToken } from './http'

export async function login(identifier: string, password: string) {
  try {
    const { data } = await http.post('/auth/login', { identifier, password })
    setAccessToken(data.access_token)   // refresh token lives in the cookie; nothing to store
    return data
  } catch (err) {
    if (axios.isAxiosError(err) && err.response?.status === 401) {
      throw new Error('手机号或密码错误')
    }
    throw err
  }
}
```

```ts
// component — no HTTP knowledge needed
try {
  await login(identifier, password)
  router.push('/dashboard')
} catch (err) {
  setError(err.message)  // "手机号或密码错误"
}
```

### Why not distinguish "wrong password" from "account not found"?

Telling the user which one is wrong lets an attacker enumerate valid accounts (try many phone numbers; whichever says "wrong password" exists). Returning a single `invalid credentials` for both is intentional.

---

## Error format

Errors return the relevant HTTP status with a body:

```json
{ "error": "human-readable message" }
```

Validation failures (`400`) return the raw validator message in `error`.

## Common types

### User

```json
{
  "id": "uuid",
  "phone": "13800138000",
  "email": "user@example.com",      // omitted if not set
  "display_name": "Alice",
  "roles": ["student", "teacher"],
  "created_at": "2026-06-26T10:00:00Z",
  "updated_at": "2026-06-26T10:00:00Z"
}
```

`roles` — a user may hold both `student` and `teacher`. The role currently in effect travels in the JWT (the `active_role` field below), not on the user record.

### Auth response

Returned by register / login / login-with-code:

```json
{
  "user": { /* User */ },
  "access_token": "jwt...",
  "active_role": "student"
}
```

> The refresh token is **not** in the body — it is set as an HttpOnly cookie via the `Set-Cookie` response header (see [Authentication](#authentication)).

---

## Endpoints

### Health

#### `GET /healthz`
Liveness check. No auth.

**200**
```json
{ "status": "ok" }
```

---

### Public — Auth

#### `POST /api/v1/auth/register`
Create an account. Returns tokens (auto-login).

**Body**
| Field | Type | Rules |
|---|---|---|
| `phone` | string | required, 5–20 chars. Primary identifier. |
| `email` | string | optional, valid email. |
| `password` | string | required, 8–72 chars. |
| `display_name` | string | required, 1–50 chars. |
| `role` | string | required, `student` or `teacher`. |

```json
{
  "phone": "13800138000",
  "email": "alice@example.com",
  "password": "s3cretpass",
  "display_name": "Alice",
  "role": "student"
}
```

**201** → [Auth response](#auth-response)
**400** validation error
**409** `phone already registered` / `email already registered`

---

#### `POST /api/v1/auth/login`
Login with identifier + password.

**Body**
| Field | Type | Rules |
|---|---|---|
| `identifier` | string | required. Phone **or** email. |
| `password` | string | required. |

```json
{ "identifier": "13800138000", "password": "s3cretpass" }
```

**200** → [Auth response](#auth-response)
**400** validation error
**401** `invalid credentials`

---

#### `POST /api/v1/auth/send-code`
Request a one-time login code (sent to the identifier). Code lifetime: **5 minutes** (`OTP_CODE_TTL`).

> Always returns **200**, even for unknown identifiers — so it can't be used to probe which accounts exist. Do not treat 200 as proof the account exists.

**Body**
| Field | Type | Rules |
|---|---|---|
| `identifier` | string | required. Phone or email. |

**200**
```json
{ "status": "sent" }
```
**429** `too many code requests, try again later` — per-target rate limit hit: a code was requested again within the resend cooldown (`OTP_RESEND_COOLDOWN`, default 60s) or beyond the daily cap (`OTP_DAILY_LIMIT`, default 10 per 24h). Bounds SMS/email cost; reveals nothing about account existence.

---

#### `POST /api/v1/auth/login/code`
Login with identifier + one-time code.

**Body**
| Field | Type | Rules |
|---|---|---|
| `identifier` | string | required. Phone or email. |
| `code` | string | required. The code from `send-code`. |

```json
{ "identifier": "13800138000", "code": "123456" }
```

**200** → [Auth response](#auth-response)
**400** validation error
**401** `invalid credentials` (wrong, expired, or unknown code)

---

#### `POST /api/v1/auth/refresh`
Exchange the refresh-token cookie for a new access token + **rotated** refresh token.

**Cookie** — `refresh_token` (sent automatically by the browser; requires `withCredentials`/`credentials: 'include'`). No request body.

**200**
```json
{ "access_token": "jwt..." }
```
> The rotated refresh token is returned via a fresh `Set-Cookie` header; the previous one is now invalid. Nothing to read or store on the frontend.

The new access token keeps the user's **last active role** — a prior `switch-role` survives the refresh rather than reverting to the default role.

**401** `missing refresh token` (no cookie) / `invalid refresh token` (invalid / revoked / expired) → session is over, re-login. On an invalid token the stale cookie is also cleared.

---

#### `POST /api/v1/auth/logout`
Revoke the current refresh token and clear its cookie. Idempotent.

**Cookie** — `refresh_token` (sent automatically). No request body.

**204** No Content (also returned if the cookie was absent or already revoked). The refresh cookie is expired via `Set-Cookie` either way.

---

### Authenticated

All endpoints below require `Authorization: Bearer <access_token>`.
Missing/malformed header → **401** `missing or malformed authorization header`.
Invalid/expired token → **401** `invalid or expired token`.

#### `GET /api/v1/me`
Current user + the role the token is acting as.

**200**
```json
{
  "user": { /* User */ },
  "active_role": "student"
}
```
**404** `user not found`

---

#### `POST /api/v1/auth/logout-all`
Revoke **every** refresh token the authenticated user holds (logout on all devices). Driven by the access token's subject, so no refresh token is needed in the body — handy for signing out other devices from the current one. Idempotent.

**Body** — none.

**204** No Content (also returned if the user has no active sessions).

---

#### `POST /api/v1/auth/switch-role`
Re-issue an access token scoped to a role the user **already holds**. Returns only a new access token (refresh token unchanged).

**Body**
| Field | Type | Rules |
|---|---|---|
| `role` | string | required, `student` or `teacher`. |

**200**
```json
{ "access_token": "jwt...", "active_role": "teacher" }
```
**400** validation error
**403** `user does not have this role`

---

#### `POST /api/v1/auth/roles`
Acquire an additional identity (e.g. a student who also starts teaching), then switch to it. Returns a new access token scoped to the new role.

**Body**
| Field | Type | Rules |
|---|---|---|
| `role` | string | required, `student` or `teacher`. |

**201**
```json
{ "access_token": "jwt...", "active_role": "teacher" }
```
**400** validation error
**409** `user already has this role`

---

## Typical flows

All requests must be made with credentials enabled (`withCredentials: true` / `credentials: 'include'`) so the refresh cookie flows.

**New user**
1. `POST /auth/register` → keep `access_token` in memory (refresh cookie is set automatically).
2. Call protected endpoints with `Authorization: Bearer <access_token>`.

**Returning user (password)**
1. `POST /auth/login` → keep the `access_token` in memory.

**Returning user (code)**
1. `POST /auth/send-code` → show "code sent".
2. `POST /auth/login/code` → keep the `access_token` in memory.

**Restoring a session after page reload** (access token lives in memory, so it's gone on reload)
- On app start → `POST /auth/refresh` (no body). If `200`, you're logged in; if `401`, go to login.

**Keeping the session alive**
- On `401` from a protected call → `POST /auth/refresh` (no body) → save the new `access_token` → retry the original request.
- If `/auth/refresh` returns `401` → clear the in-memory token and redirect to login.

**Dual-role user**
- Already has the role: `POST /auth/switch-role`, then replace the stored access token.
- Adding a new role: `POST /auth/roles`, then replace the stored access token.
