# API Reference

REST API for the tsz-go backend. All endpoints are JSON over HTTP.

- **Base URL:** `http://<host>:<port>` (e.g. `http://localhost:8080`)
- **API prefix:** `/api/v1`
- **Content-Type:** `application/json` for all request bodies.
- **Interactive docs:** Swagger UI at `/docs`, OpenAPI spec at `/docs/openapi.yaml` (source: [openapi.yaml](openapi.yaml)). Disable with `DOCS_ENABLED=false`.

## Authentication

The API uses **JWT access tokens** + **refresh tokens**, stored differently by design (see [auth-token-storage.md](auth-token-storage.md) for the full rationale and frontend guide):

- **Access token** — returned in the JSON body. Lifetime **15 minutes** (default, `JWT_TTL`). Carries the user ID and active role. Send it on protected endpoints via the header `Authorization: Bearer <access_token>`. Keep it in memory, not `localStorage`.
- **Refresh token** — delivered as an **HttpOnly, Secure, SameSite=Strict cookie** (scoped to `Path=/api/v1/auth`), **not in the body**. Lifetime **30 days** (default, `REFRESH_TOKEN_TTL`). The browser sends it automatically to `/auth/refresh` and `/auth/logout`; frontend JS never reads or stores it. Make requests with credentials (`withCredentials: true` / `credentials: 'include'`).

> **Two independent identity realms.** Everything above is the **web** realm (students/teachers). The **admin** back office is a *separate* identity store (`admins` table) with its **own** login (`/api/v1/admin/auth/*`), its own signing key (`ADMIN_JWT_SECRET`), and its own refresh cookie (`admin_refresh_token`, scoped to `Path=/api/v1/admin`). A web token can never pass an admin endpoint and an admin token can never pass a web endpoint — they fail signature verification under the other realm's key, so the boundary is enforced by the key itself, not just a role check. See [user-module-design.md](user-module-design.md) for the full model and the admin section below for the contract.

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
  "avatar_url": "",                 // empty until a storage backend (OSS) lands
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
  "active_role": "student",
  "expires_in": 900,
  "refresh_token_expires_at": 1719400000
}
```

`expires_in` — access-token lifetime in seconds. `refresh_token_expires_at` — refresh-token expiry as a Unix timestamp (seconds).

> The refresh token is **not** in the body — it is set as an HttpOnly cookie via the `Set-Cookie` response header (see [Authentication](#authentication)).

### Learning settings

A learner's two basic onboarding choices. They drive the whole study experience (which content is served, and the accent + spelling used), so the frontend needs them on app load.

```json
{
  "cefr_level": "B1",        // A1 | A2 | B1 | B2 | C1 | C2
  "english_variant": "BrE"   // BrE (British) | AmE (American)
}
```

The two fields are always set **together** — there is no half-set state. Until onboarding is done, `learning_settings` is `null` and `onboarded` is `false` (see [`GET /api/v1/me`](#get-apiv1me)). Read the server-derived `onboarded` flag rather than re-deriving it from `learning_settings == null`, so the rule for "new user" can change server-side without a frontend release.

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

An account is identified by a **phone, an email, or both** — at least one is
required. Whichever is supplied can later be used to log in.

**Body**
| Field | Type | Rules |
|---|---|---|
| `phone` | string | optional*, 5–20 chars. |
| `email` | string | optional*, valid email. |
| `password` | string | required, 8–72 chars. |
| `display_name` | string | required, 1–50 chars. |
| `role` | string | required, `student` or `teacher`. |

\* `phone` and `email` are each optional, but **at least one must be present**.

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
**400** validation error, or `phone or email is required` when both are omitted
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

#### `POST /api/v1/auth/password/forgot`
Request a password-reset code, sent to the identifier — **SMS** for a phone, **email** for an email. Code lifetime: **5 minutes** (`OTP_CODE_TTL`). Pair with `/auth/password/reset`.

> Always returns **200**, even for unknown identifiers — so it can't be used to probe which accounts exist. Do not treat 200 as proof the account exists.

**Body**
| Field | Type | Rules |
|---|---|---|
| `identifier` | string | required. The account's phone **or** email. |

**200**
```json
{ "status": "sent" }
```
**429** `too many code requests, try again later` — per-target rate limit hit (resend cooldown `OTP_RESEND_COOLDOWN`, or daily cap `OTP_DAILY_LIMIT`).

---

#### `POST /api/v1/auth/password/reset`
Verify the reset code and set a new password. On success **every existing session is revoked** server-side, so the user must log in again with the new password.

**Body**
| Field | Type | Rules |
|---|---|---|
| `identifier` | string | required. The phone or email the code was sent to. |
| `code` | string | required. The code from `password/forgot`. |
| `new_password` | string | required. 8–72 chars (bcrypt caps input at 72 bytes). |

```json
{ "identifier": "13800138000", "code": "123456", "new_password": "newpassword456" }
```

**200**
```json
{ "status": "reset" }
```
**400** validation error, or `invalid or expired reset code` — wrong/expired code (also returned for an unknown identifier, to avoid account probing).
**403** `account disabled` — the account is disabled and cannot be reset into.

---

#### `POST /api/v1/auth/refresh`
Exchange the refresh-token cookie for a new access token + **rotated** refresh token.

**Cookie** — `refresh_token` (sent automatically by the browser; requires `withCredentials`/`credentials: 'include'`). No request body.

**200**
```json
{
  "access_token": "jwt...",
  "expires_in": 900,
  "refresh_token_expires_at": 1719400000
}
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
Current user, the role the token is acting as, and the learner's onboarding state.

**200**
```json
{
  "user": { /* User */ },
  "active_role": "student",
  "learning_settings": { "cefr_level": "B1", "english_variant": "BrE" },
  "onboarded": true
}
```
`learning_settings` is `null` and `onboarded` is `false` until the learner completes onboarding (see [Learning settings](#learning-settings)). Show the onboarding flow when `onboarded` is `false`.

**404** `user not found`

---

#### `PATCH /api/v1/me`
Edit the authenticated user's profile. Today this updates only the **display name** (the "昵称" on the edit-profile screen); avatar upload lands with the OSS storage backend. Returns the refreshed user.

**Body**
| Field | Type | Rules |
|---|---|---|
| `display_name` | string | required, 1–50 chars. Trimmed; a whitespace-only value is rejected. |

```json
{ "display_name": "新昵称" }
```

**200**
```json
{ "user": { /* User */ } }
```
**400** validation error, or `display name cannot be blank` (whitespace-only after trimming)
**404** `user not found` — the account no longer exists (stale token)

---

#### `POST /api/v1/me/contact/bind-code`
Send a one-time code to a **new** contact (phone or email) the user wants to **bind or change** — the "绑定邮箱 / 绑定手机" flow. Unlike `send-code` / `password/forgot` / `account/deletion-code` (which send to a contact **already on file**), this code goes to the **value in the request**, so confirming it proves the user controls that new contact. Code lifetime: **5 minutes** (`OTP_CODE_TTL`). Pair with `POST /me/contact/bind`.

The contact's shape decides the channel: a value containing `@` is treated as an **email** (delivered by email), otherwise a **phone** (delivered by SMS). The value is validated and checked for availability **before** any code is sent.

**Body**
| Field | Type | Rules |
|---|---|---|
| `contact` | string | required. The new phone (5–20 chars) or a valid email. |

```json
{ "contact": "newuser@example.com" }
```

**200**
```json
{ "status": "sent" }
```
**400** `invalid contact` — neither a valid email nor a 5–20 char phone
**409** `email already registered` / `phone already registered` — the contact already belongs to **another** account (nothing is sent)
**429** `too many code requests, try again later` — per-target OTP rate limit (`OTP_RESEND_COOLDOWN` / `OTP_DAILY_LIMIT`)

> Unlike the public account-probing-safe endpoints, this **authenticated** flow deliberately returns `409` for a taken contact: the user needs to know their chosen email/phone is unavailable, and they are already signed in.

---

#### `POST /api/v1/me/contact/bind`
Verify the code from `bind-code` and write the new phone/email onto the account — **binding a missing contact or replacing an existing one**. Availability is re-checked before the code is consumed; the database's unique index is the final guard against a concurrent bind of the same value. Returns the refreshed user.

**Body**
| Field | Type | Rules |
|---|---|---|
| `contact` | string | required. Must match the contact `bind-code` was sent to. |
| `code` | string | required. The code from `bind-code`. |

```json
{ "contact": "newuser@example.com", "code": "123456" }
```

**200**
```json
{ "user": { /* User */ } }
```
**400** validation error, `invalid contact`, or `invalid or expired verification code`
**409** `email already registered` / `phone already registered` — taken by another account (including a concurrent bind)
**404** `user not found` — the account no longer exists (stale token)

---

#### `PUT /api/v1/me/learning-settings`
Set the learner's CEFR level + English variant. Backs both **new-user onboarding** and later edits from the settings screen (e.g. the BrE/AmE toggle). Both fields are required and written together.

**Body**
| Field | Type | Rules |
|---|---|---|
| `cefr_level` | string | required, one of `A1` `A2` `B1` `B2` `C1` `C2`. |
| `english_variant` | string | required, `BrE` or `AmE`. |

**200**
```json
{
  "learning_settings": { "cefr_level": "B1", "english_variant": "BrE" },
  "onboarded": true
}
```
**400** validation error
**409** `learning settings require a student profile` — the account has no student profile to attach them to (e.g. teacher-only).

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
| `role` | string | required, `student`, `teacher`, or `admin`. |

`admin` is accepted here (unlike `add-role`) because switching only ever activates a role the account already owns — it is gated on `HasRole`, so it cannot be used to acquire admin. A multi-role account enters the back office by switching its active role to `admin`.

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
| `role` | string | required, `student` or `teacher`. `admin` is **not** accepted — admin is never self-granted (it is provisioned out of band), so allowing it here would be a privilege-escalation hole. |

**201**
```json
{ "access_token": "jwt...", "active_role": "teacher" }
```
**400** validation error
**409** `user already has this role`

---

#### `POST /api/v1/auth/account/deletion-code`
Request a one-time code to confirm **account deletion**, sent to the account's own phone or email (picked by `channel`). Code lifetime: **5 minutes** (`OTP_CODE_TTL`). Pair with `DELETE /auth/account`.

> The code always goes to the contact **on file**, never to a value in the request — so it proves you own the account.

**Body**
| Field | Type | Rules |
|---|---|---|
| `channel` | string | required, `phone` or `email`. The contact to send the code to. |

**200**
```json
{ "status": "sent" }
```
**400** `verification channel unavailable for this account` — the account has no contact for the chosen channel (e.g. `email` on a phone-only account). The deletion screen should hide that tab.
**429** `too many code requests, try again later` — per-target rate limit hit (`OTP_RESEND_COOLDOWN` / `OTP_DAILY_LIMIT`).

---

#### `DELETE /api/v1/auth/account`
Permanently delete the authenticated account once the confirmation code checks out. **Irreversible.** The delete cascades to every owned row — roles, profiles, sessions — so the user is signed out everywhere and the freed phone/email can be reused by a new registration.

**Body**
| Field | Type | Rules |
|---|---|---|
| `channel` | string | required, `phone` or `email`. Must match the channel the code was sent to. |
| `code` | string | required. The code from `account/deletion-code`. |

```json
{ "channel": "phone", "code": "123456" }
```

**204** No Content — the account was deleted.
**400** validation error, `invalid or expired deletion code` (wrong/expired code), or `verification channel unavailable for this account`.
**404** `user not found` — the account was already deleted (stale token).

---

### Admin (back office)

The `/api/v1/admin/*` endpoints back the management console (dashboard, user
administration, moderation, Tiansheng-coin, audit). Admin is a **separate identity
realm** (`admins` table), not a role on a web account — see the realm note under
[Authentication](#authentication) and [user-module-design.md](user-module-design.md).

**Logging in is its own flow:**

- `POST /api/v1/admin/auth/login` (identifier + password) → an **admin-realm**
  access token (signed with `ADMIN_JWT_SECRET`) + an `admin_refresh_token` cookie
  (scoped to `Path=/api/v1/admin`). Password only — no SMS-code login for admin.
- `POST /api/v1/admin/auth/refresh` / `/logout` / `/logout-all` mirror the web ones,
  but on the admin cookie.
- Admin is **not self-registerable**. The first **super_admin** is seeded out of band
  (`make seed`); further admins are created by a super_admin via
  `POST /api/v1/admin/admins`.

**Gate semantics:**

- Missing/invalid/expired token, **or a web-realm token** → **401** `invalid or
  expired token` (a web token fails verification under the admin key — there is no
  "wrong realm 403", it simply isn't a valid admin token).
- Authenticated admin but the action needs **super_admin** (create/disable admin
  accounts) → **403** `super admin required`.
- Disabling the **last active `super_admin`** (self-disable included) → **409**
  `cannot disable the last active super admin` — the back office can never be
  left with no one able to manage accounts.

```
Authorization: Bearer <admin-realm access_token>
```

The admin's own identity probe is `GET /api/v1/admin/profile` →
`{ id, phone, display_name, level }` (`level` is `admin` or `super_admin`).

Beyond auth, these endpoints differ from the teacher-facing API in two ways:

- **No class scoping** — admin sees all users, classes, and content (the
  teacher views are filtered to the teacher's own classes).
- **Contact fields are visible** — phone/email are returned in the admin user
  directory; the teacher API masks them.

Conventions used across the admin list endpoints:

- **Pagination** — `?page=1&page_size=20` (max `page_size` 100); responses wrap
  rows as `{ "items": [...], "page": { "page", "page_size", "total" } }`.
- **Sensitive writes are audited** — coin grant/deduct, review approve/reject,
  and user changes each append an entry queryable via `GET /api/v1/admin/audit-logs`.
- **Coin is ledger-backed** — grant/deduct take an `idempotency_key` (a retry is
  a no-op), balances are derived, and a deduct that would go negative returns
  **409** `insufficient balance`.

The full endpoint list, request/response schemas, and error cases live in the
OpenAPI spec ([openapi.yaml](openapi.yaml), tags `Admin (…)`) and the design
rationale in [architecture.md](architecture.md) §9.

#### Smart wordlist authoring (`/api/v1/admin/words`) — implemented

The 智能词库 content hub. Data model, decisions (D1–D17) and validation rules:
[wordlist-module-design.md](wordlist-module-design.md); full schemas: OpenAPI
tag `Admin (words)`. Any admin may author (no super_admin gate).

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/admin/words` | Create a shell (`{headword, kind}`, kind `word`/`phrase`) → **201** draft. **409** duplicate headword+kind. |
| `GET` | `/admin/words/{id}` | Whole entry tree; `updated_at` is the optimistic-lock token. |
| `PUT` | `/admin/words/{id}/content` | **保存** — full-tree replace. Body carries `base_updated_at` (mismatch → **409**) and the tree; node ids are **client-generated UUIDs, stable across saves**. Drafts validate structurally only; published entries must stay complete (**422** otherwise). Audio fields & relation snapshots are server-owned, ignored on input. |
| `POST` | `/admin/words/{id}/publish` | **提交** — completeness check (design §7) then `draft → published`; failure → **422** `{error, details[]}`. Republish is idempotent and re-triggers question generation. |
| `GET` | `/admin/words` | List page: `q` (词汇/创建人), `gloss`, `kind`, `pos`, `level`, `status`, `created_from/to` (RFC3339) + pagination; rows carry derived `gloss` / `pos_list` / `levels`. |
| `GET` | `/admin/words/stats` | `{total, today, month}` creation counters (Asia/Shanghai). |
| `DELETE` | `/admin/words/{id}` | Delete the tree; inbound related-word links keep snapshots, lose FKs. |
| `POST` | `/admin/words/batch-delete` | `{ids[]}` (≤100) → `{deleted}`; unknown ids skipped. |
| `GET` | `/admin/words/related-search` | 添加关联词 dialog: `q`, optional `kind`, `limit` → entries with their senses (gloss = first zh definition). |

---

## Typical flows

All requests must be made with credentials enabled (`withCredentials: true` / `credentials: 'include'`) so the refresh cookie flows.

**New user**
1. `POST /auth/register` → keep `access_token` in memory (refresh cookie is set automatically).
2. `GET /me` → if `onboarded` is `false`, run onboarding: have the user pick a CEFR level + English variant, then `PUT /me/learning-settings`.
3. Call protected endpoints with `Authorization: Bearer <access_token>`.

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
