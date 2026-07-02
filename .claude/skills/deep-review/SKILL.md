---
name: deep-review
description: 对 tsz-go 当前分支做合并前深度审查：机械门禁（build/vet/测试/改动行覆盖率/迁移编号/API 文档同步）+ 分层人工审查（bug、测试架构合规、安全、稳定性）+ 影响面分析，输出带严重级别的结构化报告和明确的可合并结论。当用户要求严格审查、深度 review、合并前检查、稳定性评估、检查测试覆盖、检查改动影响面，或提到 /deep-review 时，务必使用本 skill（即使用户只说"帮我看看这个分支能不能合"这类模糊说法）。Use for strict pre-merge review, diff coverage gating, blast-radius analysis on this repo.
---

# Deep Review — tsz-go 合并前深度审查

目标：保证系统稳定运行。比官方 /code-review 更严格：**跑测试、量化改动行覆盖率、校验本仓库特有的测试架构约定、做影响面分析**，最后给出明确的合并结论。

审查对象默认是「当前分支相对 main 的全部改动 + 工作区未提交改动」。用户指定了 PR 或其他基线时以用户为准。

## 阶段 0 — 确定范围

```bash
BASE=$(git merge-base main HEAD)
git diff --stat $BASE                       # 已提交 + 未提交的全部改动
git status --porcelain                      # 未跟踪文件也算审查对象
```

把改动文件按包分组，记下：新增的包、被改的迁移、router.go 是否变动。后续阶段都围绕这份清单。

## 阶段 1 — 机械门禁（脚本，不靠眼力）

```bash
bash .claude/skills/deep-review/scripts/preflight.sh
```

脚本做的事：build、vet、gofmt、staticcheck/govulncheck（未安装则提示）、单元测试、自动拉起 db 容器跑集成测试（`-race -tags=integration`）、迁移编号检查（重复=FAIL、空号=WARN）、router↔openapi.yaml↔api.md 同步启发式检查、**改动行覆盖率**（阈值 80%，`cmd/` 装配代码豁免）。

产物在 `tmp/deep-review/`：`unit.out`、`integration.out`（coverprofile）、`diff_coverage.txt`（逐文件未覆盖行号）。

要求：
- 任何 FAIL 都必须出现在最终报告里，不允许沉默吞掉。
- 集成测试没跑成（Docker 不在等）时，报告里显式写「集成测试未执行」并降级结论，不能假装通过。
- 覆盖率低于阈值时，把 `diff_coverage.txt` 里未覆盖的行读出来看：是真逻辑没测（BLOCKER），还是错误分支/装配代码（可降级为 HIGH/MEDIUM，说明理由）。

## 阶段 2 — 分层深度审查（人工，按 checklist）

先读这两份参考，再逐包过 diff：

- `references/review-checklist.md` — Go bug 模式 + 本项目专属检查项（安全、稳定性、代码质量）
- `references/test-standards.md` — 测试金字塔五层、Store 契约测试、集成测试隔离铁律

逐包审查时读**完整文件**而不是只看 diff hunk——很多 bug（锁没配对、事务没回滚、错误分支漏 return）只有看全函数才能发现。测试文件同样要审：断言是否真的会失败、测试数据是否违反隔离铁律。

## 阶段 3 — 影响面分析

对每个被修改（不只是新增）的导出符号：

```bash
grep -rn --include='*.go' '<Symbol>' cmd internal | grep -v '_test.go'
```

- 列出所有调用方包，确认它们的行为假设没被这次改动打破。
- 调用方所在包的测试是否在阶段 1 跑过、是否覆盖到受影响路径。
- 改了中间件/router 分组的，逐条列出受影响的路由。
- 改了迁移或表结构的，grep 该表名找出所有 SQL 使用点。

## 报告模板（严格照此输出）

```markdown
# Deep Review 报告 — <分支> vs main

## 结论：可合并 / 修复 BLOCKER 后可合并 / 不建议合并

## 机械门禁
| 检查 | 结果 |
|---|---|
（preflight 每项的 PASS/FAIL/SKIP，覆盖率写具体数字）

## 发现
### [BLOCKER] <一句话标题>
- 位置：`file:line`
- 问题：…
- 证据：…（引用代码/测试输出，不许凭空推断）
- 建议：…

（HIGH / MEDIUM / LOW 同格式，按严重度排序）

## 影响面
- 改动波及的包与调用方清单
- 未被本次测试覆盖的波及路径

## 测试架构合规
- 新有状态包契约测试：有/无
- 隔离铁律：通过/违规点
```

## 严重级别定义（写死，不许临场放宽）

**BLOCKER**（不修不能合）：
- 改动行覆盖率 < 80%（`cmd/` 豁免；错误分支例外需逐条说明理由）
- 新的有状态包缺 `contract_test.go` 契约测试（fake + Postgres 双跑）
- 集成测试违反隔离铁律（进程内计数器、TRUNCATE/清库、非全局唯一数据）
- 迁移编号与 main 或已知在途分支冲突、up/down 不成对、down 不能真实回滚
- 会丢数据/panic/死锁/越权的 bug
- 新端点鉴权挂错身份域（web 挂成 admin 或反之）或漏挂

**HIGH**：不修会在生产出错但有条件触发（竞态、超时缺失、错误吞掉、资源泄漏）。
**MEDIUM**：正确性没问题但侵蚀稳定性/可维护性（日志缺关键字段、文档漂移、fake 与真库行为差异未被契约覆盖）。
**LOW**：风格与小改进。不确定的发现要标注「不确定」，不许硬拔成高级别。

结论规则：有 BLOCKER → 「修复 BLOCKER 后可合并」或「不建议合并」（多个结构性 BLOCKER 时用后者）；只有 HIGH 及以下 → 「可合并」，但 HIGH 必须列入合并后必办清单。
