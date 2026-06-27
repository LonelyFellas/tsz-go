# 用户模块详细设计 — 双身份库（web / admin 独立）

> 本文是用户/认证模块的**细节设计 + 前端对接契约**，落地方案为「**方案 2 + 3**」：
> 独立 `admins` 表 + 共享认证原语 + admin 独立 JWT 签名密钥。
>
> 它**推翻**了早期管理后台 Phase A 的「单一身份系统」决策（admin 曾是 `users` 上的一个角色，
> 见 commit `2e8ca48`）。本文是当前唯一事实来源；已同步 [architecture.md](architecture.md) §9、
> [api.md](api.md) 的「Admin (back office)」节、[admin-frontend-integration.md](admin-frontend-integration.md)。
> 接口契约最终以 [openapi.yaml](openapi.yaml) 为准；本文与之对齐。

---

## 1. 设计原则

1. **两套身份完全独立。** web 端（学员/教师）与 admin 端（管理员/超管）是两条互不相干的
   账号记录，分表存储。同一手机号可在两边各存在一个**毫无关联**的账号。
2. **互相不能登录、不能使用。**
   - web 用户拿不到、也用不了 admin 的任何接口；admin 账号也无法登录 web 端。
   - 这条**不靠中间件"记得检查"**，而是由 **token 签名密钥** 物理隔离强制（见 §4）。
3. **admin 只能内部创建。** 没有 admin 自助注册；首个**超管**由 `cmd/seed` 带外引导，
   之后的 admin 由超管在后台创建。
4. **认证机器复用，不重复造轮子。** 密码哈希、JWT 签发、refresh 轮换三件套抽成领域无关
   原语，web 与 admin 两个 service 共用，避免两套实现漂移。
5. **依赖方向不变：** `handler → service → repository`，每个领域一个纵切（vertical slice）。

---

## 2. 两套身份对照

| 维度 | web 身份（user） | admin 身份（admin） |
|------|------------------|----------------------|
| 存储表 | `users` + `user_roles` + `*_profiles` | `admins` + `admin_refresh_tokens` |
| 角色/层级 | `student` / `teacher`（可多持、可切换） | `admin` / `super_admin`（单层级，存 `admins.level`） |
| 账号来源 | 自助注册 `/auth/register` | 超管创建；首个超管 `cmd/seed` |
| 登录入口 | `/api/v1/auth/*` | `/api/v1/admin/auth/*` |
| token 领域 | `realm=web`，**web 密钥**签名 | `realm=admin`，**admin 密钥**签名 |
| refresh cookie | `refresh_token`，path=`/api/v1/auth` | `admin_refresh_token`，path=`/api/v1/admin` |
| 禁用 | `users.status` | `admins.status` |
| profile | student/teacher_profiles、学习设置 | 无（admin 不持有学习身份） |

---

## 3. 数据模型

### 3.1 web 侧（**不变**）

`users` / `user_roles` / `student_profiles` / `teacher_profiles` 保持现状，
**唯一变化是回收 admin**：`user_roles.role` 的 CHECK 收回到 `('student','teacher')`，
`users.last_active_role` 同理。`users` 上不再出现 `admin` 角色。

### 3.2 admin 侧（**新增**）

```sql
-- 后台身份，与 users 完全独立。phone 在本表内唯一（与 users.phone 互不影响）。
CREATE TABLE admins (
    id            UUID PRIMARY KEY,
    phone         TEXT        NOT NULL,
    email         TEXT,                          -- 可选
    password_hash TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,
    level         TEXT        NOT NULL DEFAULT 'admin'
                  CHECK (level IN ('admin', 'super_admin')),
    status        TEXT        NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'disabled')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX admins_phone_unique ON admins (phone);
CREATE UNIQUE INDEX admins_email_unique ON admins (lower(email)) WHERE email IS NOT NULL;

-- admin 的 refresh 不能复用 refresh_tokens（那张表的 user_id FK 指向 users）。
-- 结构与语义一致，只是 admin_id FK 指向 admins，并独立计数"单设备登录"。
CREATE TABLE admin_refresh_tokens (
    id          UUID        PRIMARY KEY,
    admin_id    UUID        NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admin_refresh_tokens_admin ON admin_refresh_tokens (admin_id);
CREATE UNIQUE INDEX admin_refresh_tokens_hash ON admin_refresh_tokens (token_hash);
```

> **审计/天生币 FK 受益：** `admin_audit_log.actor_id`、`coin_ledger.operator_id`
> 直接 FK → `admins.id`，数据库层强约束「操作者必是管理员」。被操作对象（如某个 web
> 学员）走弱引用 `ref_type + ref_id`，不破坏隔离。详见 [architecture.md](architecture.md) §9.3/§9.4。

---

## 4. Token 设计（双密钥 + realm）

access token 仍是无状态 HS256 JWT，新增两点：

- **`realm` claim**：`"web"` 或 `"admin"`。
- **`role` claim**：web 是激活角色（`student`/`teacher`）；admin 是层级（`admin`/`super_admin`）。

```jsonc
// web access token payload
{ "sub": "<user uuid>",  "realm": "web",   "role": "student", "iat": ..., "exp": ... }
// admin access token payload
{ "sub": "<admin uuid>", "realm": "admin", "role": "super_admin", "iat": ..., "exp": ... }
```

**两套独立签名密钥**（核心隔离手段）：

- web token 用 `JWT_SECRET` 签名；admin token 用新增的 `ADMIN_JWT_SECRET` 签名。
- web 的鉴权中间件只用 web 密钥验签 → admin token 拿到 web 接口**验签直接失败 → 401**；反之亦然。
- 所以「互相不能用」是**密钥强制**的；`realm` claim 是第二层显式校验 + 让中间件区分层级，
  二者叠加（belt-and-suspenders）。
- admin 密钥泄露不波及 web，反之亦然；admin 可单独配更短 TTL、独立轮换。

| 凭据 | TTL（默认） | 配置项 |
|------|-------------|--------|
| web access | 15m | `JWT_TTL` / `JWT_SECRET` |
| web refresh | 30d | `REFRESH_TOKEN_TTL` |
| admin access | 15m（建议可调更短） | `ADMIN_JWT_TTL` / `ADMIN_JWT_SECRET` |
| admin refresh | 30d（建议可调更短） | `ADMIN_REFRESH_TOKEN_TTL` |

> 实现上 `auth.TokenManager` 已是「(secret, ttl)」的实例，天然支持双实例：装配两个
> `TokenManager`（web / admin），各自的 `Generate`/`Parse` 带上 `realm`。`Parse` 额外
> 校验 `realm` 是否等于该实例期望的领域，不符即视为无效 token。

---

## 5. 鉴权中间件语义

| 中间件 | 作用 | 失败 |
|--------|------|------|
| `AuthRequired(webTM)` | 验 web token（web 密钥 + `realm=web`），写入 userID/role | 401 `invalid or expired token` |
| `AdminAuthRequired(adminTM)` | 验 admin token（admin 密钥 + `realm=admin`），写入 adminID/level | 401 |
| `RequireSuperAdmin` | 挂在 `AdminAuthRequired` 之后，要求 `level=super_admin` | 403 `super admin required` |

- web 受保护路由组用 `AuthRequired`；admin 路由组用 `AdminAuthRequired`。
- 超管专属操作（建/禁用 admin）再叠 `RequireSuperAdmin`。
- 旧的 `RequireRole("admin")` 与 `auth.RoleAdmin` 常量**移除**。

---

## 6. API 契约

通用约定（两端一致）：错误统一 `{ "error": "<message>" }`；access token 走
`Authorization: Bearer`；refresh token 仅在 HttpOnly cookie，不进 body；列表分页
`?page=&page_size=`（上限 100），响应 `{ "items": [...], "page": {...} }`。

### 6.1 Web 端（现状，realm=web）— 不变

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/auth/register` | 注册，`role ∈ {student,teacher}`（传 admin → 400） |
| POST | `/api/v1/auth/login` | 密码登录（identifier=phone/email） |
| POST | `/api/v1/auth/send-code` · `/login/code` | 验证码登录 |
| POST | `/api/v1/auth/refresh` · `/logout` | 刷新 / 注销 |
| GET  | `/api/v1/me` | 当前用户 + active_role + 学习设置 + onboarded |
| POST | `/api/v1/auth/switch-role` · `/auth/roles` · `/auth/logout-all` | 角色切换 / 加角色 / 全端登出 |
| PUT  | `/api/v1/me/learning-settings` | 学习设置 |

> ⚠️ `switch-role` 的 `oneof` 收回到 `student teacher`（移除 admin）；admin 不再是 web 角色。

### 6.2 Admin 端（新增，realm=admin）

所有路径前缀 `/api/v1/admin`。**无 register**。

#### 登录 `POST /api/v1/admin/auth/login`

```jsonc
// 请求
{ "identifier": "15257294120", "password": "<admin 密码>" }
// 200 —— refresh 在 admin_refresh_token cookie 里，不在 body
{
  "admin": { "id": "...", "phone": "...", "display_name": "Administrator", "level": "super_admin" },
  "access_token": "jwt...",
  "level": "super_admin",
  "expires_in": 900,
  "refresh_token_expires_at": 1719400000
}
```

- 401 `invalid credentials`（账号或密码错，不区分，防枚举）
- 403 `account disabled`（被禁用）
- 仅密码登录；admin **不启用**短信验证码登录（缩小攻击面），如需可后续单独加。

#### 刷新 / 注销

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/admin/auth/refresh` | 无 body，带 `admin_refresh_token` cookie → 新 access + 轮换 refresh |
| POST | `/api/v1/admin/auth/logout` | 注销当前会话（幂等，204） |
| POST | `/api/v1/admin/auth/logout-all` | 注销该 admin 全部会话（204） |

#### 自身资料 `GET /api/v1/admin/profile`

门禁探针 + 「已登录为 X」头部。

```jsonc
// 200
{ "id": "...", "phone": "...", "display_name": "...", "level": "admin" }
// 401 token 缺失/过期/非 admin 领域；403 不会在这里出现（profile 不限超管）
```

#### 超管管理 admin 账号（`RequireSuperAdmin`）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/admin/admins` | 创建 admin。body `{ phone, password, display_name, level }`（`level` 默认 `admin`） |
| GET  | `/api/v1/admin/admins` | 列表（分页，可 `?level=` / `?q=` 过滤） |
| PATCH| `/api/v1/admin/admins/{id}/status` | 启/禁用，body `{ status: "active"\|"disabled" }` |

- 创建冲突（phone 已存在于 admins）→ 409 `phone already registered`。
- 非超管访问以上 → 403 `super admin required`。
- 禁用即时生效：admin 侧 refresh 同样校验 status（≤ access TTL 内失效）。
- 禁用「最后一个 active 状态的 `super_admin`」（含禁用自己）→ 409
  `cannot disable the last active super admin`，防止后台无人可管账号。

> B/C 段后台业务接口（`/admin/users` 管 web 用户、审核、天生币、看板）的领域门禁都用
> `AdminAuthRequired`，契约见 openapi.yaml；本文只定身份与账号管理部分。

---

## 7. 错误码一览

| 场景 | web | admin |
|------|-----|-------|
| 缺/坏 token、token 过期、领域不符 | 401 `invalid or expired token` | 同左 |
| 凭据错 | 401 `invalid credentials` | 同左 |
| 账号禁用（登录/刷新） | 403 `account disabled` | 同左 |
| 非超管访问超管接口 | — | 403 `super admin required` |
| 禁用最后一个活跃超管 | — | 409 `cannot disable the last active super admin` |
| 注册/建号手机号已占用 | 409 `phone already registered` | 同左（admins 表内） |
| 参数校验失败 | 400 + 校验信息 | 同左 |

---

## 8. 引导首个超管（`cmd/seed`）

`cmd/seed` 从「在 `users` 建 admin 角色」改为「在 `admins` 建 `level=super_admin`」，
幂等不变：

- env：`DATABASE_URL`、`SEED_ADMIN_PHONE`、`SEED_ADMIN_PASSWORD`、`SEED_ADMIN_DISPLAY_NAME`(可选)。
- 已存在该 phone 的 admin：确保其为 active（不重复建、不报错）；不存在：建号 + `super_admin`。
- 之后所有 admin 由超管走 `POST /api/v1/admin/admins` 创建。

`make seed` 命令不变。

---

## 9. 前端对接

### 9.1 两个独立的登录态

后台前端与 web 前端**各自维护一套独立登录态**，互不复用：

| | web 前端 | admin 控制台前端 |
|------|---------|------------------|
| 登录 | `POST /api/v1/auth/login` | `POST /api/v1/admin/auth/login` |
| access token | 内存 | 内存（**与 web 的 token 不通用**） |
| refresh | `refresh_token` cookie（自动） | `admin_refresh_token` cookie（自动） |
| 续期 | `POST /api/v1/auth/refresh` | `POST /api/v1/admin/auth/refresh` |
| 身份探针 | `GET /api/v1/me` | `GET /api/v1/admin/profile` |

- **access token 存内存**，不要放 localStorage；每请求带 `Authorization: Bearer <access>`。
- **所有请求带凭据**：`fetch(..., { credentials: 'include' })` 或 axios `withCredentials: true`，
  否则 refresh cookie 不发出。
- 两套 cookie 因 path 不同（`/api/v1/auth` vs `/api/v1/admin`）互不串台：web 的 refresh
  cookie 不会发往 admin 接口，反之亦然。

### 9.2 后台路由守卫

```ts
// 控制台启动 / 刷新页面
async function bootstrapAdmin() {
  if (!adminAccessToken) {
    const ok = await POST('/api/v1/admin/auth/refresh'); // 带 admin cookie
    if (!ok) return redirectToAdminLogin();
  }
  const me = await GET('/api/v1/admin/profile');         // 200 即放行
  if (me.level === 'super_admin') enableSuperAdminMenus();
  enterConsole(me);
}
```

- 拿一个 **web** 的 access token 去敲 `/api/v1/admin/*` → **401**（验签失败，密钥不同）。
- 拿 **admin** token 去敲 web 受保护接口 → 同样 **401**。前端无需特殊处理，按统一 401 流程走即可。

### 9.3 401 决策树（两端各一套，逻辑相同）

受保护接口 401 → 调对应的 `…/auth/refresh`（无 body，带 cookie）→ 成功则存新 access
并**重试原请求一次**；refresh 自身 401 → 会话结束，清内存 token，跳对应登录页。建议用统一
拦截器实现。

### 9.4 超管专属 UI

`profile.level === 'super_admin'` 时才显示「管理员管理」菜单（建号/禁用）。普通 admin 调
这些接口会拿到 403 `super admin required`，前端应直接隐藏入口而非依赖后端兜底。

---

## 10. 与 Phase A 的差异（需要推翻/清理）

| Phase A（已合并） | 本设计 |
|-------------------|--------|
| 「单一身份系统」，admin 复用 `/auth/login` | **两套独立身份**，admin 走 `/api/v1/admin/auth/*` |
| `user_roles.role` 放开 `admin`（迁移 000007） | **收回** `('student','teacher')`；admin 不进 user_roles |
| `users` 持有 admin 角色、`switch-role` 可切 admin | admin 与 users 无关；switch-role 收回 student/teacher |
| `auth.RoleAdmin` 常量 + `RequireRole("admin")` | 移除；改 `AdminAuthRequired` / `RequireSuperAdmin` |
| `repo.AddAdminRole` / `svc.SeedAdmin`（写 users） | 迁到 admin 域，写 `admins` 表 |
| `cmd/seed` 建 user+admin 角色 | 建 `admins`，`level=super_admin` |
| `GET /admin/profile` 返回 `roles/active_role` | 返回 `level` |
| 单一 `JWT_SECRET` | 新增 `ADMIN_JWT_SECRET`（+ 可选 admin TTL） |

已同步文档：`architecture.md §9`、`api.md`「Admin (back office)」、`admin-frontend-integration.md`
（已按双身份库重写）、`openapi.yaml`（admin 标签、securityScheme、admin auth/accounts 端点）、
`.env.example`（新增 `ADMIN_JWT_SECRET` 等）。旧的单一身份 Phase A 文档（`admin-phase-a-plan.md`
及旧版前端对接文档）已删除——其 A 段实现清单随本设计作废，B/C 段业务拆解保留在
`architecture.md §9` 与 `openapi.yaml`。

---

## 11. 联调自测清单

- [ ] 超管 `cmd/seed` 出账号，`/api/v1/admin/auth/login` 成功，返回 `level` 与 admin access token
- [ ] 同一手机号在 web 端 `register` 一个 student/teacher 账号，与 admin 账号**互不影响**、各自独立登录
- [ ] web access token 敲 `/api/v1/admin/profile` → **401**；admin token 敲 `/api/v1/me` → **401**
- [ ] 普通 admin 调 `POST /api/v1/admin/admins` → **403**；超管调 → **201**
- [ ] 超管禁用某 admin → 该 admin access 失效（≤TTL）、refresh 立即被拒
- [ ] web 与 admin 两套 refresh cookie 互不串台（看请求只在各自 path 发送）
- [ ] 两端各自的「401 → refresh → 重试」拦截器通过
```

## 12. 后续待办 / 硬化项（合并后未做，勿忘）

> 双身份核心已于 PR #22 合并上线。以下为评审时识别、当时**有意推迟**的项,按优先级排列。

### 12.1 部署前必做（生产阻断项）
- [ ] 将 `JWT_SECRET` 与 `ADMIN_JWT_SECRET` 从 `docker-compose.yml` 里的 `change-me-…` 占位值换成**真随机长串**;两者**必须不同**,否则服务拒绝启动(`internal/config`)。

### 12.2 健壮性硬化（可选,非阻断）
- [ ] **最后超管守卫的 TOCTOU 竞态**:`Service.SetStatus` / `isLastActiveSuperAdmin`(`internal/admin/service.go`)是「先读后写」两步、无事务/锁。两个并发禁用请求理论上可同时通过检查 → 最终 0 个活跃超管。后台基本单运营者,概率极低。彻底堵死的做法:用一条带条件的原子 `UPDATE … WHERE NOT(是最后一个活跃超管)` 让 DB 层判断,或加 DB 约束。
- [ ] **`Limit: 1000` 魔法上限**:`isLastActiveSuperAdmin` 用翻页数超管,超过 1000 个会截断。改为专用 `CountActiveSuperAdmins(ctx)` 计数查询更准更省。

### 12.3 测试补强（可选）
- [ ] **handler 层缺 409/404 映射断言**:`SetAdminStatus`(`internal/admin/handler.go`)里 `ErrLastSuperAdmin→409`、`ErrNotFound→404` 的 HTTP 映射,service 与 middleware 都测了,但 handler 级没有直接断言。补一个表驱动 handler 测试。
