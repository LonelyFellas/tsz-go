# 管理后台前端对接指南（双身份库）

> 给后台管理端（web 控制台）前端对接用。身份模型见 [user-module-design.md](user-module-design.md)，
> 通用鉴权细节见 [api.md](api.md) 与 [auth-token-storage.md](auth-token-storage.md)，
> 完整接口契约见 [openapi.yaml](openapi.yaml)（`Admin (…)` 标签）。

## 0. 一句话模型

**admin 是一套和 web 端完全独立的账号体系。** 后台前端有自己独立的登录、独立的 token、
独立的 refresh cookie，**与学员/教师端互不通用**。同一手机号在两边是两个无关账号。

- 后台登录走 `/api/v1/admin/auth/*`（**不是** `/api/v1/auth/*`）。
- 后台 token 用独立密钥签名：拿 web token 敲后台接口 → **401**；反之亦然。
- admin 不能自助注册；账号由超管在后台创建，首个超管由后端 `make seed` 引导。

---

## 1. 鉴权模型速览

| 凭据 | 存哪 | 怎么用 |
|------|------|--------|
| **admin access token** | **内存**（不要 localStorage） | 登录响应体返回；每个后台请求带 `Authorization: Bearer <access_token>`；约 15 分钟过期 |
| **admin refresh token** | **HttpOnly Cookie** `admin_refresh_token`（浏览器自动管，JS 读不到） | 自动随 `/api/v1/admin/auth/refresh`、`/logout` 发送；约 30 天；path 限定 `/api/v1/admin` |

- **所有请求都要带凭据**：`fetch(..., { credentials: 'include' })` 或 axios `withCredentials: true`，
  否则 refresh cookie 不会发出。
- **单设备登录**：每次登录会踢掉该 admin 之前的会话。
- admin 的 cookie path 是 `/api/v1/admin`，和 web 的 `/api/v1/auth` **互不串台** —— 后台前端即使
  和 web 前端同域，两套登录态也不会相互污染。

---

## 2. admin 账号从哪来

- **首个超管**：后端用 `make migrate && make seed`（配 `SEED_ADMIN_PHONE` / `SEED_ADMIN_PASSWORD`）引导，
  `level=super_admin`。
- **其余 admin**：由超管在后台调 `POST /api/v1/admin/admins` 创建（见 §7）。
- **不能自助注册**：没有 admin 注册接口；web 的 `/api/v1/auth/register` 只发学员/教师账号，
  和后台无关。

---

## 3. 登录

```http
POST /api/v1/admin/auth/login
Content-Type: application/json

{ "identifier": "15257294120", "password": "<admin 密码>" }
```

**200** —— 注意 refresh token **不在 body 里**（它在 `admin_refresh_token` cookie）：

```json
{
  "admin": { "id": "...", "phone": "...", "display_name": "Administrator", "level": "super_admin", "status": "active" },
  "access_token": "jwt...",
  "level": "super_admin",
  "expires_in": 900,
  "refresh_token_expires_at": 1719400000
}
```

前端：把 `access_token` 放内存；记录 `level`（`admin` / `super_admin`）；refresh 不用管（在 cookie 里）。

**登录失败：**
- `400` —— 参数校验失败（缺字段等）。
- `401 { "error": "invalid credentials" }` —— 账号或密码错（不区分，防枚举）。
- `403 { "error": "account disabled" }` —— 账号被超管禁用。

> admin **仅支持密码登录**，没有短信验证码登录。

---

## 4. 路由守卫 + 判断身份

后台门禁 = **持有有效的 admin token**。判定 = `GET /api/v1/admin/profile` 返回 **200**。
（不需要再看「角色」——admin 不是 web 角色，是独立身份。）

```ts
// 控制台启动 / 刷新页面：内存里没有 access_token 时，先尝试 refresh 续期
async function bootstrapAdmin() {
  if (!adminAccessToken) {
    const ok = await tryAdminRefresh();        // POST /api/v1/admin/auth/refresh，带 cookie
    if (!ok) return redirectToAdminLogin();
  }
  const me = await GET('/api/v1/admin/profile'); // 200 即放行
  if (me.level === 'super_admin') enableSuperAdminMenus();
  enterConsole(me);
}
```

```http
GET /api/v1/admin/profile
Authorization: Bearer <admin access_token>
```

| 状态 | 含义 | 前端处理 |
|------|------|---------|
| **200** | 有效 admin | `{ id, phone, display_name, level }`，渲染"已登录为 X"头部；按 `level` 控制菜单 |
| **401** | token 缺失/过期，**或拿的是 web token** | 走 §5 刷新重试；refresh 也失败则跳登录 |

> 拿一个 **web** access token 来敲后台接口 → **401**（验签失败，密钥不同），不是 403。前端无需特殊
> 处理，按统一 401 流程走即可。

---

## 5. 401 处理（后台自己的一套）

后台受保护接口返回 `401` 时：

1. `POST /api/v1/admin/auth/refresh`（**无 body**，带 cookie）→ 成功拿到**新** `access_token`，
   存内存后**重试原请求**。
2. 若 `refresh` 自身返回 `401` → 会话已结束，清内存里的 token，跳后台登录页。

建议用一个**独立于 web 端**的请求拦截器实现"401 → admin refresh → 重试一次"——不要和 web 端
的拦截器共用（refresh 端点、cookie 都不同）。完整 401 取值见 [api.md](api.md#handling-401-errors)。

注销：`POST /api/v1/admin/auth/logout`（当前会话）；`POST /api/v1/admin/auth/logout-all`（全端登出）。

---

## 6. 错误格式

所有错误统一 `{ "error": "human-readable message" }`。后台常见值：
`invalid credentials`（401）、`account disabled`（登录 403）、`super admin required`（403）、
`invalid or expired token`（401）、`phone already registered`（建号 409）。

---

## 7. 超管：管理 admin 账号（`level === 'super_admin'` 才显示入口）

普通 admin 调用以下接口会得到 **403** `super admin required`，前端应**直接隐藏入口**，不要依赖后端兜底。

**创建 admin**

```http
POST /api/v1/admin/admins
Authorization: Bearer <super_admin access_token>

{ "phone": "15257294120", "password": "<初始密码>", "display_name": "审核员小王", "level": "admin" }
```
- `201` → 返回新建的 `Admin`；`level` 缺省为 `admin`。
- `409 { "error": "phone already registered" }` —— 该手机号已是某个 admin。

**列表 / 启禁用**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/admin/admins?page=&page_size=&level=&q=` | 分页列表，可按 level / 关键字过滤 |
| PATCH | `/api/v1/admin/admins/{id}/status` | body `{ "status": "active" \| "disabled" }`，禁用即时生效（≤ access TTL） |

---

## 8. 列表分页约定（后台所有列表通用）

查询 `?page=1&page_size=20`（`page_size` 上限 100），响应：

```json
{ "items": [ /* ... */ ], "page": { "page": 1, "page_size": 20, "total": 137 } }
```

---

## 9. 即将到来（B / C 段，先按契约 mock）

契约已在 [openapi.yaml](openapi.yaml) 冻结、后端尚未实现；前端可照契约先做 UI + mock：

- **用户管理（B 段）**：`GET /admin/users`（`role`/`q` 过滤，管的是 **web 学员/教师**）、详情、启禁用。
  后台可见手机号/邮箱（教师端是脱敏的）。
- **审核 / 天生币 / 看板（C 段）**：见 openapi.yaml `Admin (review/coin/audit/dashboard)` 标签
  与 [architecture.md](architecture.md) §9。

> 这些后台业务接口都用 **admin token**（`adminBearerAuth`），401/超管 403 语义同上。

---

## 10. 联调自测清单

- [ ] seed 出的超管 `/api/v1/admin/auth/login` 能登录，拿到 `access_token` + `level:"super_admin"`
- [ ] `GET /api/v1/admin/profile`：有效 admin → **200**；不带 token → **401**
- [ ] 用 **web** 端登录拿的 token 敲 `/api/v1/admin/profile` → **401**（验证两套身份隔离）
- [ ] 普通 admin 调 `POST /api/v1/admin/admins` → **403** `super admin required`；超管调 → **201**
- [ ] 超管禁用某 admin → 该 admin 下次 refresh 被拒、access 失效（≤TTL）
- [ ] 故意让 access token 过期 → 后台拦截器 `admin refresh` 后自动重试通过
- [ ] 所有请求带 `credentials: 'include'` / `withCredentials`，刷新页面能靠 admin refresh cookie 续期
