# tsz-go — 「天生会背」Go 后端

Gin + pgx + golang-migrate。模块化布局：`internal/<module>`（model / repository /
service / handler / fake / contract_test），平台层在 `internal/platform`。
入口：`cmd/server`（服务）、`cmd/migrate`（迁移）、`cmd/seed`（首个超管）。

## 常用命令

- `make dev` — Postgres 起 Docker + air 热重载（改代码免重启）
- `make test` — 纯单元测试，不接库
- `make test-integration` — 单元 + 集成，跑本地 docker `tsz-go-db-1` 的 `tsz_test` 库
- `make migrate-create name=xxx` — 生成下一号迁移对；`make lint` — golangci-lint
- 合并前跑 `/deep-review`；部署/看线上日志用 `/deploy` skill

## 铁律（违反会炸生产或炸 CI）

**迁移编号全局递增，永不复用。** 目录 `internal/platform/database/migrations/`。
golang-migrate 不回头应用低于当前版本的迁移——分支上的迁移号若被 main 抢先占用，
必须改号到最新号之后再合并（先例：coin 000017 → 000019）。改号后若某环境已用旧号
建过表，用 `IF NOT EXISTS` 幂等建表。

**集成测试共享库永不清库。** `tsz_test` 跨包并行共享、禁 TRUNCATE / TestMain 清库。
测试数据必须全局唯一（uuid 邮箱、crypto/rand 手机号），**禁止进程内自增计数器**
（跨运行会撞历史残留行）。

**双身份库。** web 用户（users，学生/教师）与 admin（admins，超管/词库管理员）是
两套独立身份，不共享账号体系。事实来源 `docs/user-module-design.md`。跨两库引用
（如 coin 的 owner）用 `(realm, id)` 多态弱引用，不建外键。

**API 文档随手同步。** 改了接口必须同步 `docs/api.md`（手写主文档）和
`docs/openapi.yaml`（CI 有 redocly lint 门禁）。

**前后端共享的对接文档放上一级 `../docs`（tsz-core/docs）**，不进本仓库；本仓库
`docs/` 只放后端自身的设计与 API 文档。

## 测试架构

分层：纯单元 → service+fake → handler（`gin.CreateTestContext`）→ repository 集成
（`//go:build integration`）→ e2e。每个有状态包一个无 tag 的 `contract_test.go`，
`runStoreContract` 同一套断言跑 fake 和真 Postgres，杜绝 fake 漂移；fault-injection
的 `*Fn`/`*Err` hook 不属于契约。

## 服务器

47.121.142.19（ssh 别名 `tshb-test`）虽叫测试机，实为对外服务主机，**按生产对待**：
动它之前一律走 `/deploy` skill 的流程，禁止即兴 SSH 改动。
