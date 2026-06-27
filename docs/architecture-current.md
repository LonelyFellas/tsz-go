# tsz-go 当前架构（as-built）

> 本文描述**仓库现状**，与代码一一对应。面向未来的产品/演进规划见 [architecture.md](architecture.md)。

## 1. 一句话概述

一个小规模 toC 后端：**模块化单体**（Gin）跑在单个 **PostgreSQL** 上，
通过 Docker Compose 部署。已落地的业务有两块身份系统：
1. **web 用户身份**（注册、密码登录、验证码登录、令牌刷新、student/teacher 多角色切换）；
2. **后台管理身份（双身份库）**——一套与 web **完全独立**的 admin 账号体系，独立签名密钥、
   独立 refresh、`admin`/`super_admin` 两级。两端互不通用（详见 [user-module-design.md](user-module-design.md)）。

## 2. 技术栈

| 关注点 | 选型 | 位置 |
|--------|------|------|
| HTTP 框架 | Gin v1.12 | `internal/platform/httpserver` |
| 数据库 | PostgreSQL（pgx/v5 `pgxpool`，手写 SQL，无 ORM） | `internal/platform/database` |
| 迁移 | golang-migrate，SQL 文件 `embed` 进二进制 | `internal/platform/database/migrations` |
| 认证 | JWT（HS256）access token + 服务端 refresh token；bcrypt 存密码；**web/admin 双 realm**（独立密钥 + `realm` 声明） | `internal/auth`、`internal/session` |
| 后台身份 | 独立 admin 账号体系（垂直切片，复用 session 原语，独立 refresh 表） | `internal/admin` |
| 配置 | 纯环境变量，手写 loader（无框架） | `internal/config` |
| 日志 | 标准库 `log/slog`（结构化 JSON） | `cmd/server`、中间件 |

Go 1.26。模块名 `github.com/darwish/tsz-go`。

## 3. 目录结构与分层

```
cmd/
  server/      程序入口：装配依赖、起 HTTP 服务、优雅关闭
  migrate/     独立迁移入口（生产环境单独跑迁移）
  seed/        一次性引导首个超管（写 admins 表，幂等）
internal/
  config/      环境变量配置
  auth/        JWT 签发/解析（无状态，带 web/admin realm）
  session/     refresh token：service + repository（轮换、单设备登录）
  otp/         一次性验证码：service、repository、可插拔 Sender（当前为 mock）
  user/        web 用户域（垂直切片）
    handler.go     HTTP 层
    service.go     业务逻辑
    repository.go  数据访问（pgx 上手写 SQL）
    model.go       领域模型 / Role
  admin/       后台管理域（垂直切片，与 user 同构）
    handler.go / service.go / repository.go / model.go
    sessionrepo.go  admin_refresh_tokens 仓储（喂给 session.Service）
  platform/    横切基础设施
    database/    pgx 连接池 + 内嵌迁移
    httpserver/  router + 中间件（含 AdminAuthRequired / RequireSuperAdmin）
docs/          架构、API、设计说明文档
```

**依赖方向自外向内**：`handler → service → repository`。
业务域（`user`、`admin`）按**垂直切片**组织——HTTP、业务、数据访问同包内聚；
跨域的基础设施放在 `platform/` 下。`admin` 与 `user` 是**两套独立身份**，不共享账号表。

## 4. 运行时架构

```
  客户端 (App / Web)
        │ HTTPS / REST (JSON)
        ▼
┌─────────────────────────────────────────────┐
│              tsz-go (单进程)                  │
│                                               │
│  Gin Router  /api/v1/*                        │
│   ├─ 中间件: Recovery, RequestLogger          │
│   ├─ AuthRequired       (web realm JWT)       │
│   │     user.Handler ─► user.Service ─► Repo  │
│   │                        │  │  │            │
│   │            auth(web)◄──┘  │  └──► session  │
│   │                          └──► otp         │
│   └─ AdminAuthRequired  (admin realm JWT)     │
│      [+RequireSuperAdmin on account mgmt]     │
│         admin.Handler ─► admin.Service ─► Repo │
│                            │   └──► session    │
│                  auth(admin)◄┘  (admin_refresh)│
└──────────────────────────┬────────────────────┘
                           │ pgxpool
                           ▼
                      PostgreSQL
```

依赖装配全部集中在 [cmd/server/main.go](../cmd/server/main.go) 的 `run()`，
手工 wiring（无 DI 框架），所以依赖图一眼可见：

```
TokenManager(web) ─┐
otp.Service ───────┼─► user.Service ──► user.Handler ──┐
session.Service ───┘                                   ├─► Router
TokenManager(admin)┐                                   │
adminSession.Svc ──┼─► admin.Service ─► admin.Handler ─┘
（admin repo）─────┘
```

两套身份各自一个 `TokenManager`（不同密钥 + realm）和一套 refresh 仓储；
装配仍集中在 `cmd/server/main.go`，无 DI 框架。

## 5. 模块职责

### auth — 无状态令牌
- `TokenManager` 用 HS256 签发/校验 JWT。`sub` 为用户 ID，自定义 `role`
  声明携带**当前活跃角色**。中间件本地校验，**不查库**，所以靠短 TTL
  （默认 15m）来限制被吊销会话的存活时间。

### session — refresh token（有状态）
- refresh token 是高熵随机串，库里只存其 **SHA-256 哈希**。
- 每次刷新都**轮换**（旧的失效、发新的）。
- **严格单设备登录**：签发新 token 时吊销该用户其余 token。
- 只有低频的 `/auth/refresh`、`/auth/logout` 路径会触达此表。

### otp — 一次性验证码
- 验证码登录用。`Sender` 接口可插拔，当前是 mock（只打日志），
  接真实 SMS/邮件时在 `main.go` 换实现即可。
- 有**防滥用**：重发冷却（默认 60s）、每日上限（默认 10 次/目标）、
  失败尝试次数上限（防 6 位码在 TTL 内被在线爆破）。

### user — web 用户域（垂直切片）
- 身份与角色无关：一个账号可同时持有 `student` / `teacher` 多个角色，
  当前活跃角色随 JWT 走，并持久化为 `last_active_role` 以便**跨刷新保留**。
- 手机号为必填主标识，邮箱可选，两者均可登录。
- Service 依赖 `Store`/`Codes`/`Sessions` **接口**，单测用内存 fake 替换。

### admin — 后台管理域（双身份库）
- 与 `user` **完全独立**的账号体系：独立的 `admins` 表、独立的 `admin_refresh_tokens`、
  **独立的签名密钥**（`ADMIN_JWT_SECRET`，且 `realm=admin`）。web token 敲后台 → 401，反之亦然——
  纵深防御（密钥层 + realm 声明双重隔离）。同一手机号在两边是两个无关账号。
- 两级权限 `admin` / `super_admin`；`super_admin` 才能管理 admin 账号（建号 / 列表 / 启禁用），
  由中间件 `RequireSuperAdmin` 把关。
- **不可自助注册**：首个超管由 `cmd/seed` 带外引导（幂等 + 自愈提权），其余由超管在后台创建。
- 仅密码登录（无验证码）。复用 `session.Service`（喂 `admin_refresh_tokens` 仓储）实现轮换 + 单设备。
- 守卫：禁用账号在登录/刷新被拒；**最后一个活跃 `super_admin` 不可禁用**（409），防后台锁死。

## 6. 数据模型（PostgreSQL）

迁移文件见 `internal/platform/database/migrations/`（11 个版本，000001–000011）。

| 表 | 作用 | 关键点 |
|----|------|--------|
| `users` | web 认证身份 | phone 唯一；email 唯一（条件索引 + 小写）；`last_active_role`、`status` |
| `user_roles` | 用户持有的角色（多对一账号） | `(user_id, role)` 主键，`TEXT + CHECK`（限 `student`/`teacher`）；admin **不进**此表 |
| `student_profiles` / `teacher_profiles` | 角色专属资料 | 每用户每角色一条 |
| `verification_codes` | OTP 验证码 | 单次使用 `consumed_at`、过期 `expires_at`、`attempts` 上限 |
| `refresh_tokens` | web refresh token | 存 `token_hash`，`revoked_at` 支撑单设备/登出 |
| `admins` | **后台账号（独立身份库）** | phone 唯一、email 可选；`level`（admin/super_admin）、`status`；与 `users` 无外键关联 |
| `admin_refresh_tokens` | 后台 refresh token | 与 `refresh_tokens` 同构，独立成表 |

设计取向：`TEXT + CHECK` 取代 enum 类型，让 pgx 扫描简单、迁移廉价。
身份演进：000007 曾放开 `user_roles` 的 admin 角色，**000009 已收回**（admin 改走独立 `admins` 表）。

## 7. HTTP 接口

挂载于 [router.go](../internal/platform/httpserver/router.go)，前缀 `/api/v1`。

**公开：**
- `POST /auth/register` — 注册
- `POST /auth/login` — 标识符 + 密码
- `POST /auth/send-code` — 申请登录验证码
- `POST /auth/login/code` — 标识符 + 验证码
- `POST /auth/refresh` — 轮换 refresh → 新 access
- `POST /auth/logout` — 吊销一个 refresh token

**需认证（`AuthRequired`）：**
- `GET /me`
- `POST /auth/logout-all` — 吊销全部 refresh token
- `POST /auth/switch-role` — 切到已持有的角色
- `POST /auth/roles` — 获取新身份

**后台（`/api/v1/admin/*`，admin realm token）：**
- `POST /admin/auth/login` `/refresh` `/logout` `/logout-all` — 后台登录态（独立 cookie `admin_refresh_token`，path 限 `/api/v1/admin`）
- `GET /admin/profile` — 后台门禁探针，返回 `level`
- 经 `AdminAuthRequired` 守卫；以下再加 `RequireSuperAdmin`：
  - `POST /admin/admins`、`GET /admin/admins`、`PATCH /admin/admins/{id}/status` — 超管管理 admin 账号

另有 `GET /healthz` 健康检查。完整请求/响应见 [api.md](api.md)，后台前端对接见 [admin-frontend-integration.md](admin-frontend-integration.md)。

## 8. 令牌方案

| | access token | refresh token |
|--|--------------|----------------|
| 形态 | JWT (HS256)，无状态 | 不透明随机串，库里存哈希 |
| 校验 | 中间件本地校验，不查库 | 查 `refresh_tokens` 表 |
| TTL | 短（默认 15m） | 长（默认 720h / 30d） |
| 传递 | `Authorization: Bearer` 头 | HttpOnly Cookie（生产 Secure） |
| 轮换 | 无 | 每次刷新轮换 |

短 access + 有状态 refresh 的组合：日常请求零数据库开销，吊销/单设备
限制只在低频刷新路径上生效。前端存储约定见 [auth-token-storage.md](auth-token-storage.md)。

## 9. 配置（环境变量）

`config.Load()` 读取，缺 `DATABASE_URL` / `JWT_SECRET` / `ADMIN_JWT_SECRET` 直接报错退出；
且 `ADMIN_JWT_SECRET` **必须不等于** `JWT_SECRET`（相同则拒绝启动，强制两套身份密钥隔离）。

| 变量 | 默认 | 说明 |
|------|------|------|
| `PORT` | 8080 | 监听端口 |
| `DATABASE_URL` | （必填） | Postgres DSN |
| `JWT_SECRET` | （必填） | web realm HS256 密钥 |
| `ADMIN_JWT_SECRET` | （必填，须 ≠ `JWT_SECRET`） | admin realm HS256 密钥 |
| `JWT_TTL` / `ADMIN_JWT_TTL` | 15m / 15m | 两端 access token 寿命 |
| `REFRESH_TOKEN_TTL` / `ADMIN_REFRESH_TOKEN_TTL` | 720h / 720h | 两端 refresh token 寿命 |
| `OTP_CODE_TTL` / `OTP_RESEND_COOLDOWN` / `OTP_DAILY_LIMIT` | 5m / 60s / 10 | 验证码策略 |
| `APP_ENV` | development | 控制 Cookie Secure 等 |
| `AUTO_MIGRATE` | false | 启动时是否自动迁移 |

后台引导用的 `SEED_ADMIN_PHONE` / `SEED_ADMIN_PASSWORD` / `SEED_ADMIN_DISPLAY_NAME` 由 `cmd/seed`
直接读环境变量（不走 `config.Load`），见 §10。

## 10. 构建、迁移与部署

- **本地开发**：`make dev` — Postgres 跑在 Docker，应用用 `air` 原生热重载。
- **迁移**：默认**独立步骤**（`cmd/migrate` / `make migrate`）；仅当
  `AUTO_MIGRATE=true` 时随启动执行。迁移 SQL 用 `embed` 打进二进制。
- **容器**：`docker-compose.yml` 三个 service —— `db`、一次性 `migrate`
  （跑完才起 app）、`app`。镜像由根 `Dockerfile` 构建。
- **优雅关闭**：监听 SIGINT/SIGTERM，10s 超时内 `srv.Shutdown`。
- **CI**：`.github/workflows/ci.yml`。

## 11. 测试策略

- **单元测试**（`make test`，无需数据库）：service 层用接口 fake 隔离。
- **集成测试**（`make test-integration`，`-tags=integration`）：跑在专用
  `tsz_test` 库上，覆盖 repository 与 `httpserver` 的端到端（`e2e_integration_test.go`）。

## 12. 现状边界

- 已实现的业务为 **web 认证/用户** + **后台管理身份（双身份库，A 段）**；后台的
  用户管理、审核、天生币、看板等业务（B/C 段，契约已在 [openapi.yaml](openapi.yaml) 冻结）
  **尚未落地**；产品规划中的班级、词表、学习、题库等域（见 [architecture.md](architecture.md)）亦未落地。
- `otp.Sender` 仍是 mock，未接真实短信/邮件通道。
- 无缓存层、无消息队列、无对象存储——当前规模不需要。
- 上线前需替换 `JWT_SECRET` / `ADMIN_JWT_SECRET` 占位值（见 [user-module-design.md](user-module-design.md) §12.1）。
