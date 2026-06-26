# 实现规格：Token 刷新 + 单设备登录（方案 A）

> 这是一份自包含的实现说明，供新 session 冷启动使用。读完即可动手，无需追溯历史对话。

## 分支

在 `feat/token-refresh-single-device` 上做（已基于 `test/integration-isolated-db` 创建）。
**不要从 `main` 切**：本功能依赖该分支上尚未合入 main 的「双角色 + 手机号/验证码登录」代码。

## 这个仓库当前已有的相关现状

- 纯无状态 JWT：`internal/auth/token.go`，HS256，`Generate(userID, role)` / `Parse → Claims{UserID, Role}`，TTL 来自 `config.JWTTTL`（默认 24h）。
- 中间件 `internal/platform/httpserver/middleware.go` 的 `AuthRequired` 本地验签，把 userID/role 放进 gin context，不查库。
- 用户域 `internal/user/`（vertical slice：handler/service/repository/model + fakes/tests）。
  - 登录方式：`LoginPassword(identifier, password)`、`LoginCode(identifier, code)`（验证码）、注册 `Register(...)`。
  - 这些方法目前都返回 `(*User, token string, error)`，handler 用 `authResponse{User, Token, ActiveRole}` 输出单个 token。
  - 角色：账号可同时持有 student/teacher，active role 在 JWT 里；`SwitchRole` / `AddRole` 会重签 token。
- 验证码包 `internal/otp/`（`Sender` 接口 + `MockSender`，`Service.RequestCode/Verify`，`Repository`）—— 可作为本功能 repository/service 分层的参照模板。
- 迁移：`internal/platform/database/migrations/`，目前到 `000002`（embed + golang-migrate 自动跑）。
- 配置：`internal/config/config.go`，env 驱动；已有 `JWTTTL`、`OTPCodeTTL`。

## 目标（方案 A —— 容忍延迟，不每请求查库）

1. **Token 刷新**：access token 短命（15min）+ refresh token 长命（30d）续期。
2. **单设备登录（严格）**：新设备登录时撤销该用户其它所有 refresh token；旧设备的 access 过期后下次 refresh 失败即被踢下线（延迟 ≤ access TTL）。
3. access token 保持无状态、每请求本地验签**不查库**（中间件几乎不变）；服务端状态只压在低频的 `/auth/refresh`、`/auth/logout` 上。

## 设计

### access / refresh 双 token

| | access | refresh |
|---|---|---|
| 形式 | JWT（沿用现有 token.go）| 不透明随机串（如 32 字节 base64url）|
| 存储 | 不存 | 存库，**只存哈希**（SHA-256 即可，refresh token 本身是高熵随机串，不需要 bcrypt）|
| TTL | 15min | 30d |
| 验证 | 中间件本地验签 | `/refresh`、`/logout` 时查库 |

### 数据表（新迁移 `000003_create_refresh_tokens.{up,down}.sql`）

```sql
CREATE TABLE refresh_tokens (
    id          UUID        PRIMARY KEY,
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user ON refresh_tokens(user_id);
CREATE UNIQUE INDEX refresh_tokens_hash ON refresh_tokens(token_hash);
```
（不加 device_id：本期是「严格单设备」。down 文件 `DROP TABLE IF EXISTS refresh_tokens;`）

### 新增/改动的代码

建议新建 `internal/session`（或放进 `internal/auth`）一个 refresh-token 的 service+repository，参照 `internal/otp` 的分层与 `Store` 接口风格：

- `Issue(ctx, userID) (rawRefreshToken string, err error)`：生成随机串 → 存哈希 → **先撤销该 user 其它所有 refresh token（单设备）** → 返回明文给客户端。
- `Rotate(ctx, rawRefreshToken) (userID, newRawRefreshToken, err error)`：校验（存在 / 未撤销 / 未过期）→ 撤销旧的 → 发新的（rotation）→ 返回 userID 供上层重签 access。失效一律返回统一错误 `ErrInvalidRefreshToken`。
- `Revoke(ctx, rawRefreshToken) error`：登出用。

哈希用 `crypto/sha256`，比对用查 `token_hash = $1`（高熵随机串可直接等值查，不必常数时间比对整库）。

### 接口

- `POST /api/v1/auth/refresh`  body `{"refresh_token":"..."}` → `{"access_token":"...","refresh_token":"<rotated>"}`；失效返回 401。
- `POST /api/v1/auth/logout`   body `{"refresh_token":"..."}` → 204/200；幂等（已撤销也返回成功）。
- 改造 register / login / login-code 的响应：从 `{token}` 改成同时返回 `access_token` + `refresh_token`。
  - **建议把响应结构统一**：`authResponse{User, AccessToken, RefreshToken, ActiveRole}`。注意这会改动现有 handler 测试与 e2e 里读 `token` 的地方。

### 与现有逻辑衔接

- 中间件 `AuthRequired` 不变（仍只验 access JWT）；只需把 access 的 TTL 调短。
- `SwitchRole` / `AddRole`：只重签 **access** token，refresh 不动（天然契合）。其响应目前是 `{token, active_role}`，改成 `{access_token, active_role}`。
- 角色变更最多 access TTL（15min）后在新 access 生效，符合预期。

### 配置（`internal/config/config.go` + `.env.example`）

- 复用现有 `JWTTTL` 作为 **access TTL**，把默认值改成 `15m`（env `JWT_TTL`）。
- 新增 `RefreshTokenTTL`，env `REFRESH_TOKEN_TTL`，默认 `720h`（30d）。

### 装配（`cmd/server/main.go`）

- 新建 refresh repository（pool）+ service（repo + RefreshTokenTTL），注入 user.Service（user.Service 需要新增一个依赖接口，类似现有的 `Codes` 接口，命名如 `Sessions`/`RefreshIssuer`，便于单测用 fake）。

## 测试要求（务必三层都覆盖，CI 跑 `go test -tags=integration`）

- **单元**（fake refresh store）：Issue 撤销旧 token、Rotate 成功+轮换、Rotate 对已撤销/已过期/不存在的统一返回错误、Revoke 幂等、登录触发单设备撤销。
- **集成**（真 SQL，build tag `integration`）：refresh repository 的 save/查/撤销/过期。参照 `internal/otp/repository_integration_test.go` 与 `internal/user/repository_integration_test.go`。
- **e2e**（`internal/platform/httpserver/e2e_integration_test.go`）：
  - 登录拿到 access+refresh → 用 refresh 换新 access（200）→ 旧 refresh 已轮换，重放旧 refresh → 401。
  - **单设备**：同一用户「第二次登录」后，用第一次的 refresh 去 refresh → 401（被踢下线）。
  - logout 后该 refresh 再用 → 401。

## 本机执行备忘

- `go` 不在默认 PATH：`export PATH=$PATH:/usr/local/go/bin`。
- 单元：`make test`；集成+e2e：`make test-integration`（连本机 docker 容器 `tsz-go-db-1` 的 `tsz_test` 库）。
- 新增迁移是 `000003`（增量，不改旧文件），集成测试连接时会自动 up，无需 drop 重建 `tsz_test`。
- gofmt：`gofmt -w internal/ cmd/`。
- **不要提交 `docs/architecture.md`**（会话开始前就存在的无关文件）。本规格文件 `docs/token-refresh-plan.md` 可提交可不提交，随你。

## 提交

完成后在 `feat/token-refresh-single-device` 上提交（commit message 结尾带
`Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`），按需 push。
