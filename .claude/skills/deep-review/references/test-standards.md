# tsz-go 测试架构标准

本文是审查「测试架构合规」时的事实来源。违反标注【铁律】的条目一律 BLOCKER。

## 测试金字塔（五层）

| 层 | 位置/形态 | 依赖 |
|---|---|---|
| 纯单元 | `xxx_test.go`，无外部依赖 | 无 |
| service + fake | `service_test.go` 配 `fake_test.go` 内存实现 | 无 |
| handler | `handler_test.go`，用 `gin.CreateTestContext` | 无 |
| repository 集成 | `repository_integration_test.go`，`//go:build integration` | 真 Postgres |
| 全链路 e2e | `internal/platform/httpserver/e2e_integration_test.go` | 真 Postgres |

`make test` 不接库；`make test-integration` / CI 跑 `go test -race -tags=integration -cover ./...`，连 `tsz_test` 库。

审查要点：新代码落在哪一层就要有哪一层的测试。一个新的 handler 端点至少要有 handler 层测试；新的 repository 方法必须进契约测试（见下）；e2e 覆盖新路由的鉴权挂载是加分项。

## Store 契约测试（fake == 真库）

每个有状态包（有 repository + fake 的包，现有：user/session/otp/admin/word）必须有一个**无 build tag** 的 `contract_test.go`，内含 `runStoreContract(t, newStore ...)` 作为唯一事实来源：

- fake 侧：`TestStoreContract_Fake`（无 tag，`make test` 就跑）
- 真库侧：`TestStoreContract_Postgres`（在 `repository_integration_test.go`，integration tag）

目的：杜绝 fake 静默漂移——service 测试全靠 fake，fake 行为和真库不一致时测试会给出虚假的绿。

审查要点：
- 新增 Store 方法却没加进 `runStoreContract` → BLOCKER（fake 从此可以漂移）。
- fault-injection 用的 `*Fn`/`*Err` hook **不属于**契约，留在 service/handler 测试里，不要求进契约。
- 已知例外：`admin.Store.List` 的 total/分页是整表统计，被共享测试库污染，排除在契约外，仅由 fake-backed service 测试覆盖。遇到同类「整表统计」方法可比照处理，但必须在报告里写明。

## 集成测试隔离【铁律】

测试库 `tsz_test` 是**共享且永不清理**的，这样多包才能在 `go test ./...` 下并行。因此：

1. 测试数据必须**全局唯一**：uuid 邮箱/target/hash、`crypto/rand` 手机号。
2. **禁止进程内自增计数器**造数据——计数器跨运行会撞上历史残留行（真实事故：`TestRepository_NotFound` 偶发失败）。
3. **禁止 TRUNCATE / TestMain 清库 / DELETE 全表**——会破坏跨包并行。
4. 断言不许依赖全表状态（count(*)、无 where 的 List 长度等），残留数据会让它偶发失败。

审查时逐个读新增的 `*_integration_test.go`，对照这四条。

## 覆盖率口径

门禁看的是**改动行覆盖率**（本次 diff 中的可执行行有多少被 unit+integration 任一 profile 覆盖），不是全仓库总覆盖率。阈值 80%，`cmd/` 装配代码豁免。工具：`scripts/diff_coverage.go`，产物 `tmp/deep-review/diff_coverage.txt`。
