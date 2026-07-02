# 深度审查 Checklist

逐包过 diff 时对照本清单。每条发现都要有 `file:line` 级证据；只凭模式怀疑而没读到实证的，标「不确定」。

## A. Bug 猎手（Go 通用）

- **错误处理**：`err` 被吞（`_ =`、只 log 不 return、`if err != nil` 分支漏 return）；错误被 wrap 后丢失哨兵（`errors.Is/As` 还能不能匹配）；`defer rows.Close()` / `defer tx.Rollback()` 缺失。
- **事务边界**：事务里调了外部 IO（HTTP/OTP 发送）；提交前就把结果发给调用方；Rollback 被 err 覆盖；同一事务内读己写的假设。
- **并发**：goroutine 捕获循环变量；map 并发读写；没有 ctx 传播导致 goroutine 泄漏；`sync.Mutex` 复制；double-checked locking。
- **nil 与零值**：指针字段反序列化后未判 nil；`sql.NullXxx`/`*string` 直接解引用；时间零值 `time.Time{}` 被当有效值。
- **SQL**：字符串拼接进查询（哪怕是 ORDER BY 白名单也要核实）；`LIKE` 未转义 `%_`；分页 offset 溢出；缺索引的全表扫描（对照迁移里的索引）；`RETURNING` 与 struct 扫描列不匹配。
- **边界值**：空 slice vs nil 的 JSON 差异（`[]` vs `null`）；UTF-8 截断；时区（存储一律 UTC？）；整数溢出/负数长度。
- **HTTP/gin**：handler 里 `c.JSON` 后没 return；binding 校验缺失（`binding:"required"`）；query 参数默认值；分页参数上限（防 `limit=1000000`）。

## B. 本项目专属

- **双身份域**：web 用户（`/api/v1` + user 身份）与 admin（`/api/v1/admin` + admin 身份）是两套独立身份库。新端点必须挂对分组和鉴权中间件；admin 端点绝不能接受 web token，反之亦然。核对 `internal/platform/httpserver/router.go` 的分组层级。
- **鉴权中间件**：新路由是否在 `authed` 分组内；需要权限分级的（如超管操作）是否有对应检查；限流中间件对敏感端点（发码、登录）是否生效。
- **refresh token 轮转**：涉及 session 的改动要核对轮转/吊销语义没被破坏（logout-all、旧 token 复用检测）。
- **迁移**：up/down 成对且 down 能真实回滚（不是空文件）；编号不与 main 及在途分支冲突（当前已知：coin 分支占用 000017）；DDL 是否锁表（大表加列带 DEFAULT、加非 CONCURRENTLY 索引）；与代码同 PR 部署的兼容性（先跑迁移后发代码期间，旧代码对新 schema 是否兼容）。
- **API 文档三方同步**：router.go 新路由 ↔ `docs/openapi.yaml` ↔ `docs/api.md` 一致；`docs/docs_test.go` 的路由清单是否补上；请求/响应字段名与 handler 实际 JSON tag 一致（文档漂移是 MEDIUM，字段名错是 HIGH）。
- **日志**：错误路径有结构化日志且带关键字段（user/admin id、请求标识）；不许把密码、token、验证码原文打进日志（打了是 BLOCKER 级安全问题）。
- **配置**：新配置项有默认值和校验（对照 `internal/config`）；secret 不许硬编码。

## C. 稳定性

- 外部调用（DB 之外的 HTTP、SMS/邮件发送）有超时和失败降级。
- 新查询在数据量增长后的表现：List 类接口必须有分页上限；stats 类接口是否会全表扫描。
- 资源泄漏：`http.Response.Body`、`sql.Rows`、ticker/timer、文件句柄。
- panic 面：类型断言不带 ok、数组越界、除零；handler 层 panic 会被 gin recover 但等于 500，照样算 bug。

## D. 代码质量（只报影响稳定性/正确性的）

- fake 实现与真库行为差异是否已被契约测试覆盖（没覆盖 → MEDIUM，见 test-standards.md）。
- 重复实现：新代码是否重复造了 `internal/platform` 已有的轮子（错误响应、日志、分页解析）。
- 导出面：不需要导出的类型/函数别导出；接口定义在使用方而不是实现方。
- 纯风格问题（命名、注释密度）不进报告，除非误导性命名会引发误用。
