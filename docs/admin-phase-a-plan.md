# 管理后台 Phase A — 实现计划（admin 角色 + 门禁 + 引导）

> 给「对照文档开发」的新 session 用。本文是**可逐条执行的实现清单**，不是泛设计。
> 上层设计依据见 [architecture.md](architecture.md) §9，接口契约见 [openapi.yaml](openapi.yaml)
> （`Admin (…)` 标签），鉴权说明见 [api.md](api.md) 的「Admin (back office)」节。

整个管理后台分三段交付（见 architecture.md §13 Phase 4）。**本文只覆盖 A 段**，
目标是让前端能立刻开工：登录走现有接口，后台路由门禁可用（200/401/403 行为正确）。

---

## 1. 范围（A 段做什么 / 不做什么）

**做：**

- 新增 `admin` 角色（贯通 model / 迁移 / token 校验）。
- `RequireAdmin` 门禁中间件 + `/api/v1/admin` 路由组。
- 唯一探针端点 `GET /api/v1/admin/profile`：返回当前管理员身份，给前端验证门禁与做"已登录为 X"头部。
- 用户 `status`（active/disabled）字段 + **登录链路拒绝禁用用户**（为 B 段的启/禁用打底）。
- `cmd/seed`：带外引导首个 admin（**已拍板，不做自助注册**）。

**不做（留给 B / C 段）：**

- ❌ `GET /admin/users` 列表/详情/启禁用 → **B 段**
- ❌ 权限点枚举（review/coin/...）→ 单 `admin` 角色足够，等出现"专职审核员"再加
- ❌ 审核 / 天生币 / 首页看板 → C 段及以后
- ❌ 审计表 → 建议 C 段随发币/审核一起建，A 段不碰

---

## 2. 已锁定的设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| admin 登录入口 | **复用现有 `/api/v1/auth/login`** | 单一身份系统；门禁加在 admin API 上，不在登录上 |
| 首个 admin 引导 | **独立 `cmd/seed` 命令**（仿 `cmd/migrate`） | 契合「迁移是独立步骤、不在每次启动时跑」的理念，生产可控 |
| admin 能否自助注册 | **否** | `/auth/register` 仍只许 `student`/`teacher` |
| 权限粒度 | **单 `admin` 角色，不上权限点** | 避免过早 RBAC |
| 禁用用户登录 | **登录 service 拒绝**，refresh 也校验 | 让禁用 ≤ access TTL 内即时生效 |

---

## 3. 数据模型变更（两条迁移）

用 `make migrate-create name=...` 生成，编号接在 `000006` 之后。

### 3.1 `000007_allow_admin_role`

`user_roles.role` 现有 `CHECK (role IN ('student','teacher'))`，需放开 `admin`。

```sql
-- up
ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD  CONSTRAINT user_roles_role_check
    CHECK (role IN ('student', 'teacher', 'admin'));
```
```sql
-- down  （回滚前须确保无 admin 行，否则约束加不回去）
ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD  CONSTRAINT user_roles_role_check
    CHECK (role IN ('student', 'teacher'));
```
> 约束名 `user_roles_role_check` 是 Postgres 对内联 CHECK 的默认命名；执行前用
> `\d user_roles` 确认实际名字（若不符，改成实际名）。

### 3.2 `000008_add_user_status`

```sql
-- up
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'disabled'));
```
```sql
-- down
ALTER TABLE users DROP COLUMN status;
```

admin **不建** student/teacher profile（`student_profiles`/`teacher_profiles` 与其无关）。

---

## 4. 代码变更（逐文件）

依赖方向不变：`handler → service → repository`。

### 4.1 `internal/user/model.go`

- `Role` 增加 `RoleAdmin Role = "admin"`。
- `Role.Valid()` 纳入 `RoleAdmin`。
- 新增状态类型与字段：
  ```go
  type UserStatus string
  const (
      StatusActive   UserStatus = "active"
      StatusDisabled UserStatus = "disabled"
  )
  ```
  在 `User` 结构体加 `Status UserStatus`（json `status`）。

> ⚠️ `RoleAdmin` 进入 `Valid()` 后，要确认 `Register` 路径**不会**因此接受 admin
> 角色——见 4.3。

### 4.2 `internal/user/repository.go`

- `getOne` 的 SELECT 增加 `status` 列，扫进 `User.Status`（`Create` 不用显式写
  status，靠列默认 `'active'`）。
- 新增 admin 创建（供 seed 用），**不建 profile**：
  ```go
  func (r *Repository) CreateAdmin(ctx context.Context, u *User) error
  ```
  事务内 `INSERT users` + `INSERT user_roles(user_id,'admin')`。可参照现有 `Create`
  去掉 profile 分支。
- 新增状态更新（B 段启/禁用会用，A 段可先加上）：
  ```go
  func (r *Repository) SetStatus(ctx context.Context, userID uuid.UUID, s UserStatus) error
  ```

### 4.3 `internal/user/service.go` 与 `handler.go`

- **保持 `Register` 只许 student/teacher**：`RegisterRequest` 的 role 校验维持
  `oneof=student teacher`（确认 handler 绑定 tag 未放开 admin）。
- **登录拒绝禁用用户**：`LoginPassword` 与 `LoginCode` 取到 user 后，若
  `u.Status == StatusDisabled`，返回 `403 account disabled`（不进 `issue`）。
- **refresh 校验**：`Refresh` 解析出 userID 后加载 user，若禁用则拒绝
  （复用现有 `invalid refresh token` 形状或 `account disabled`），使禁用即时生效。
- 新增引导用 service 方法（幂等）：
  ```go
  // SeedAdmin 确保存在一个以 phone 为标识的 admin。
  // 已存在该 phone：补 admin 角色（若缺）；不存在：建账号 + admin 角色。
  func (s *Service) SeedAdmin(ctx context.Context, phone, password, displayName string) (*User, error)
  ```
  复用现有密码哈希（`Register` 用的 bcrypt 逻辑），调用 `repo.CreateAdmin`
  或 `repo.AddRole`。

### 4.4 `internal/platform/httpserver/middleware.go`

`AuthRequired` 已把 role 存进 `auth.ContextRoleKey`。新增：

```go
// RequireRole aborts with 403 unless the active role matches. Mount AFTER AuthRequired.
func RequireRole(role string) gin.HandlerFunc {
    return func(c *gin.Context) {
        if got, _ := c.Get(auth.ContextRoleKey); got != role {
            c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin role required"})
            return
        }
        c.Next()
    }
}
```
A 段只需 `RequireRole(string(user.RoleAdmin))`；先不引入权限点版本。
（注意 import 环：middleware 用字面量 `"admin"` 或从 `auth` 取常量，避免 `httpserver → user` 反向依赖。建议在 `auth` 或 `middleware` 内定义 `const RoleAdmin = "admin"`。）

### 4.5 `internal/platform/httpserver/router.go`

在 `v1` 组内新增 admin 组：

```go
admin := v1.Group("/admin")
admin.Use(AuthRequired(deps.TokenManager), RequireRole("admin"))
{
    admin.GET("/profile", deps.UserHandler.AdminProfile)
}
```
`Deps` 暂不用加新依赖（复用 `UserHandler`）。

### 4.6 探针端点 handler — `AdminProfile`

`GET /api/v1/admin/profile`：从 context 取 userID，加载 user，返回身份。
契约见第 6 节 / openapi.yaml `adminProfile`。

```json
// 200
{ "id": "...", "phone": "...", "display_name": "...", "roles": ["admin"], "active_role": "admin" }
```
401 / 403 由中间件产生，handler 只处理 200 与 404。

### 4.7 `cmd/seed/main.go`（新增，仿 `cmd/migrate`）

独立一次性命令，直接读 env（不经 `config.Load`，与 `cmd/migrate` 一致）：

- 必需：`DATABASE_URL`、`SEED_ADMIN_PHONE`、`SEED_ADMIN_PASSWORD`
- 可选：`SEED_ADMIN_DISPLAY_NAME`（缺省给个默认，如 `"Administrator"`）
- 装配 user 仓库 + service（不需要 token/codes/sessions，可传 nil 或最小依赖；
  若 `NewService` 强依赖它们，给 seed 单独走 `repo.CreateAdmin`/`SeedAdmin`），
  调 `SeedAdmin`，**幂等**：重复执行不报错、不重复建号。
- 成功 `logger.Info("admin seeded", "phone", ...)`；缺 env 则 `Error` + `os.Exit(1)`。

### 4.8 `internal/config/config.go`

A 段服务端**无需**新增 config（seed 走 `cmd/seed` 自己读 env）。
如果选择让 server 也能读 seed 配置则不需要——保持服务端干净。

### 4.9 `Makefile` + `.env.example`

- Makefile 加：
  ```make
  seed: ## Seed the first admin (needs DATABASE_URL + SEED_ADMIN_* env)
  	$(GO) run ./cmd/seed
  ```
  并把 `seed` 加进 `.PHONY`。
- `.env.example` 增加并注释：`SEED_ADMIN_PHONE` / `SEED_ADMIN_PASSWORD` /
  `SEED_ADMIN_DISPLAY_NAME`，标注「仅本地/首次引导用，勿提交真实值」。

---

## 5. 执行顺序

1. 两条迁移（3.1、3.2）→ `make migrate`
2. model（4.1）→ repository（4.2）→ service/handler（4.3、4.6）
3. middleware（4.4）→ router（4.5）
4. `cmd/seed`（4.7）+ Makefile/.env（4.9）
5. 文档/契约（第 6 节）
6. 测试（第 7 节）→ `make test` / `make test-integration`

---

## 6. 契约与文档同步

- **openapi.yaml**：`GET /api/v1/admin/profile`（operationId `adminProfile`，标签
  `Admin (users)`）与 `AdminProfile` schema **本次已加好**，新 session 直接对照实现，
  改完跑 `npx @redocly/cli lint docs/openapi.yaml` 必须通过（CI 同款）。
- **api.md**：「Admin (back office)」节已说明 admin 角色 + 403；若新增可点出
  `GET /admin/profile` 为门禁探针（可选）。
- 改了接口记得回写 api.md + openapi.yaml 两份（见 reference 内存）。

---

## 7. 验收标准（Definition of Done）

- [ ] `make migrate` 后 `user_roles` 接受 `admin`，`users` 有 `status` 列
- [ ] `make seed` 幂等：跑两次都成功，只有一个 admin 账号
- [ ] 该 admin 用 `/api/v1/auth/login` 能登录，`/me` 的 `roles` 含 `admin`
- [ ] `GET /api/v1/admin/profile`：
  - 无 token → **401** `missing or malformed authorization header`
  - 过期/错误 token → **401** `invalid or expired token`
  - 有效但 active role 非 admin → **403** `admin role required`
  - admin token → **200** 返回身份
- [ ] 被禁用用户（`status=disabled`）：密码/验证码登录 → **403** `account disabled`；
      已有 refresh token 也无法刷新出新 access token
- [ ] `register` 仍拒绝 `role=admin`
- [ ] `redocly lint docs/openapi.yaml` 通过
- [ ] 单测 + 集成测试通过：middleware 的 401/403/200 分支、`SeedAdmin` 幂等、
      登录拒绝禁用用户

---

## 8. 交接到 B 段

A 段交付后契约已冻结，前端可并行做**登录 + 路由守卫 + "已登录为 X"头部**。
B 段在 admin 组下叠加 `GET /admin/users`（列表/搜索/分页）、`GET /admin/users/{id}`、
启/禁用（复用 4.2 的 `SetStatus`），范式（分页 `{items, page}`、门禁）已就位。
