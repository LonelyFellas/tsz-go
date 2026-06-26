# API Reference

REST API for the tsz-go backend. All endpoints are JSON over HTTP.

- **Base URL:** `http://<host>:<port>` (e.g. `http://localhost:8080`)
- **API prefix:** `/api/v1`
- **Content-Type:** `application/json` for all request bodies.

## Authentication

The API uses **JWT access tokens** + **refresh tokens**.

- Send the access token on protected endpoints via the header:
  `Authorization: Bearer <access_token>`
- **Access token** lifetime: **15 minutes** (default, `JWT_TTL`). Carries the user ID and active role.
- **Refresh token** lifetime: **30 days** (default, `REFRESH_TOKEN_TTL`). Used only against `POST /api/v1/auth/refresh` to obtain a fresh access token.

### Single-device login

Login is **strict single-device**. Each successful login/register issues a refresh token and **revokes the user's previous sessions**. When a refresh token is used, it is **rotated** (the old one stops working and a new one is returned). A device that was logged out elsewhere keeps working only until its access token expires (≤ 15 min), after which its next refresh fails with `401`.

> **Frontend implication:** store both `access_token` and `refresh_token`. On a `401` from a protected endpoint, call `/auth/refresh`, save the **new** `refresh_token` it returns, and retry. If refresh itself returns `401`, the session is dead — send the user back to login.

## Handling 401 errors

All `401` responses share the same shape but come from different sources. Use the `error` field to tell them apart.

### All possible 401 error values

| `error` value | Source | Meaning |
|---|---|---|
| `invalid credentials` | `/auth/login`, `/auth/login/code` | Wrong phone/email or password/code |
| `invalid refresh token` | `/auth/refresh` | Refresh token expired, revoked, or invalid |
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
const http = axios.create({ baseURL: '/api/v1' })

// Attach access token to every request
http.interceptors.request.use(config => {
  const token = localStorage.getItem('access_token')
  if (token) config.headers.Authorization = `Bearer ${token}`
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
      localStorage.removeItem('access_token')
      localStorage.removeItem('refresh_token')
      window.location.href = '/login'
      return Promise.reject(err)
    }

    // ③ Access token expired on a protected endpoint — try to refresh once
    if (status === 401 && errorMsg === 'invalid or expired token' && !err.config._retry) {
      err.config._retry = true
      try {
        const refreshToken = localStorage.getItem('refresh_token')
        const { data } = await http.post('/auth/refresh', { refresh_token: refreshToken })
        localStorage.setItem('access_token', data.access_token)
        localStorage.setItem('refresh_token', data.refresh_token)         // always save the new one
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
export async function login(identifier: string, password: string) {
  try {
    const { data } = await http.post('/auth/login', { identifier, password })
    localStorage.setItem('access_token', data.access_token)
    localStorage.setItem('refresh_token', data.refresh_token)
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
  "refresh_token": "opaque...",
  "active_role": "student"
}
```

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
Exchange a refresh token for a new access token + **rotated** refresh token.

**Body**
| Field | Type | Rules |
|---|---|---|
| `refresh_token` | string | required. |

**200**
```json
{ "access_token": "jwt...", "refresh_token": "opaque..." }
```
> Save the returned `refresh_token`; the one you sent is now invalid.

The new access token keeps the user's **last active role** — a prior `switch-role` survives the refresh rather than reverting to the default role.

**400** validation error
**401** `invalid refresh token` (invalid / revoked / expired) → session is over, re-login.

---

#### `POST /api/v1/auth/logout`
Revoke a refresh token. Idempotent.

**Body**
| Field | Type | Rules |
|---|---|---|
| `refresh_token` | string | required. |

**204** No Content (also returned if the token was already revoked/missing).
**400** validation error

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

**New user**
1. `POST /auth/register` → store `access_token` + `refresh_token`.
2. Call protected endpoints with `Authorization: Bearer <access_token>`.

**Returning user (password)**
1. `POST /auth/login` → store tokens.

**Returning user (code)**
1. `POST /auth/send-code` → show "code sent".
2. `POST /auth/login/code` → store tokens.

**Keeping the session alive**
- On `401` from a protected call → `POST /auth/refresh` → save new tokens → retry the original request.
- If `/auth/refresh` returns `401` → clear tokens and redirect to login.

**Dual-role user**
- Already has the role: `POST /auth/switch-role`, then replace the stored access token.
- Adding a new role: `POST /auth/roles`, then replace the stored access token.
