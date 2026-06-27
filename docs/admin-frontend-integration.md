# 管理后台前端对接指南（Phase A）

> 给后台管理端（web）前端对接用。本文聚焦 **admin 专属**的部分；通用鉴权细节
> （token 存储、401 决策树）见 [api.md](api.md) 与 [auth-token-storage.md](auth-token-storage.md)，
> 完整接口契约见 [openapi.yaml](openapi.yaml) 的 `Admin (…)` 标签。

## 0. 范围

Phase A **可真实对接**的后端接口：

- 登录（复用现有 `/api/v1/auth/*`）
- `GET /api/v1/me`（拿角色）
- `GET /api/v1/admin/profile`（后台门禁探针 + "已登录为 X" 头部）

B / C 段（`/admin/users` 列表、审核、天生币、首页看板）**契约已在 openapi.yaml 冻结，
但后端尚未实现**——前端可照契约先做 UI + mock，后端交付后直接联调。

---

## 1. 鉴权模型速览

| 凭据 | 存哪 | 怎么用 |
|------|------|--------|
| **access token** | **内存**（不要 localStorage） | 登录响应体里返回；每个受保护请求带 `Authorization: Bearer <access_token>`；约 15 分钟过期 |
| **refresh token** | **HttpOnly Cookie**（浏览器自动管，前端 JS 读不到） | 自动随 `/api/v1/auth/refresh`、`/logout` 发送；约 30 天 |

- **所有请求都要带凭据**：`fetch(..., { credentials: 'include' })` 或 axios `withCredentials: true`，否则 refresh cookie 不会发出。
- **单设备登录**：每次登录会踢掉该账号之前的会话。

---

## 2. admin 账号从哪来

- 由后端用 `make migrate && make seed`（配 `SEED_ADMIN_PHONE` / `SEED_ADMIN_PASSWORD`）引导。
- **不能自助注册**：`/api/v1/auth/register` 传 `role:"admin"` 会被拒（400）。
- **强烈建议：管理员用独立账号**（专门的手机号，只持有 `admin` 角色）。这样登录后
  `active_role` 直接就是 `admin`，前端无需做角色切换——多角色合并账号的切换方式见 §8。

---

## 3. 登录

```http
POST /api/v1/auth/login
Content-Type: application/json

{ "identifier": "13800138000", "password": "<admin 密码>" }
```

**200** —— 注意 refresh token **不在 body 里**（它是 cookie）：

```json
{
  "user": { "id": "...", "phone": "...", "display_name": "Administrator", "roles": ["admin"] },
  "access_token": "jwt...",
  "active_role": "admin",
  "expires_in": 900,
  "refresh_token_expires_at": 1719400000
}
```

前端：把 `access_token` 放内存；记录 `active_role`；refresh 不用管（在 cookie 里）。
（也支持验证码登录：`POST /auth/send-code` → `POST /auth/login/code`，响应结构相同。）

**登录失败：**
- `401 { "error": "invalid credentials" }` —— 账号或密码错（不区分，防枚举）。
- `403 { "error": "account disabled" }` —— 账号被管理员禁用。

---

## 4. 判断是否管理员 + 路由守卫

**关键：后台门禁看的是 token 的「当前激活角色」`active_role`，不是"持有"角色。**

进后台的判定 = `active_role === 'admin'`。守卫伪代码：

```ts
// 应用启动 / 刷新页面：内存里没有 access_token 时，先尝试 refresh 续期
async function bootstrapAuth() {
  if (!accessToken) {
    const ok = await tryRefresh();        // POST /auth/refresh，带 cookie
    if (!ok) return redirectToLogin();
  }
  const me = await GET('/api/v1/me');     // { user, active_role, ... }
  if (me.active_role !== 'admin') return denyOrLogin();
  enterConsole(me.user);
}
```

> `/api/v1/me` 还会返回 `onboarded` / `learning_settings`——那是**学员**字段，
> 后台一律忽略。

---

## 5. 调后台接口（门禁探针）

```http
GET /api/v1/admin/profile
Authorization: Bearer <access_token>
```

| 状态 | 含义 | 前端处理 |
|------|------|---------|
| **200** | 当前是 admin | `{ id, phone, display_name, roles, active_role }`，渲染"已登录为 X"头部 |
| **401** | token 缺失/过期 | 走 §6 刷新重试 |
| **403** `admin role required` | 已登录但 active role 不是 admin | 踢回登录 / 显示无权页 |

后续所有 `/api/v1/admin/*` 都是同一套门禁语义（401/403 一致）。

---

## 6. 401 处理（沿用既有决策树）

受保护接口返回 `401` 时：

1. `POST /api/v1/auth/refresh`（**无 body**，带 cookie）→ 成功拿到**新** `access_token`，
   存内存后**重试原请求**。
2. 若 `refresh` 自身返回 `401` → 会话已结束，清内存里的 token，跳登录页。

完整的 401 取值与分支见 [api.md](api.md#handling-401-errors)。建议用一个统一的请求拦截器
（axios interceptor / fetch wrapper）实现"401 → refresh → 重试一次"。

---

## 7. 错误格式

所有错误统一：

```json
{ "error": "human-readable message" }
```

后台相关常见值：`admin role required`（403）、`account disabled`（登录 403）、
`invalid credentials`（401）。

---

## 8. 多角色与角色切换 ✅

- **推荐路径（无坑）**：管理员用**独立 admin 账号** → 登录后 `active_role` 即 `admin`，
  直接进后台，不需要切角色。
- **已支持合并账号**：若某账号**同时持有** `teacher` + `admin`，可以
  `POST /auth/switch-role { role:"admin" }` 把激活角色切到 `admin`（返回 **200** + 一枚
  scope 为 admin 的新 access token），随后用它访问 `/api/v1/admin/*`。切到一个账号**未持有**
  的角色仍返回 **403**。
- **`add-role` 仍禁止 admin**：`POST /auth/roles { role:"admin" }` 返回 **400**。admin 只能
  由后端 seed/bootstrap 离线发放，绝不能自助获取——否则任意登录用户都能把自己提权成管理员。
  （switch-role 之所以能放行 admin，是因为它有 `HasRole` 门禁，只能激活已持有的角色，无法借此获取 admin。）

---

## 9. 即将到来（B / C 段，先按契约 mock）

- **列表分页约定**：查询 `?page=1&page_size=20`（`page_size` 上限 100），响应：
  ```json
  { "items": [ /* ... */ ], "page": { "page": 1, "page_size": 20, "total": 137 } }
  ```
- **用户管理（B 段）**：`GET /admin/users`（`role` / `q` 过滤）、`GET /admin/users/{id}`、启/禁用。
  后台可见手机号/邮箱（教师端是脱敏的）。
- **审核 / 天生币 / 看板（C 段）**：见 openapi.yaml `Admin (review/coin/audit/dashboard)` 标签
  与 [architecture.md](architecture.md) §9。

---

## 10. 联调自测清单

- [ ] 用 seed 出的 admin 账号 `/auth/login` 能登录，`access_token` + `active_role:"admin"`
- [ ] `GET /me` 的 `user.roles` 含 `admin`、`active_role` 为 `admin`
- [ ] `GET /admin/profile`：admin → **200**；非 admin 账号 → **403**；不带 token → **401**
- [ ] 故意让 access token 过期 → 拦截器 `refresh` 后自动重试通过
- [ ] 被禁用账号登录 → **403** `account disabled`
- [ ] 所有请求带 `credentials: 'include'` / `withCredentials`，刷新页面能靠 refresh cookie 续期
