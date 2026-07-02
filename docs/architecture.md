# 天生会背 — 技术架构方案

> 基于产品需求脑图整理，与当前 `tsz-go` 仓库（Go 模块化单体）对齐。

---

## 1. 产品概览

**天生会背** 是英语词汇记忆产品。层级上**只有班级**：用户可归属班级，教师按班带课，管理员在后台维护班级与内容。

| 端 | 说明 |
|----|------|
| **学员端** | 登录注册、当前学习集、多题型练习、学习统计 |
| **教师端** | 本班管理、学习进度报告、词表配置（不可见学员联系方式） |
| **管理后台** | 班级、教师、词表、题库、标准词库维护 |
| **落地页** | 首页、功能入口（低优先级：资源下载） |

**关键非功能需求：**

- **权限**：教师只看自己的班；管理员看全部
- **学习模型**：词项覆盖、题型轮换、优先级
- **多媒体**：TTS 发音、连读显示、音频校正
- **外部集成**：第三方词典、COCA 词频、科大讯飞 TTS
- **隐私**：教师侧联系方式等字段脱敏

---

## 2. 架构选型

### 推荐：模块化单体 + 前后端分离

```
┌──────────────────────────────────────────────────────────────┐
│           学员 App │ 教师 Web │ 管理后台 │ 落地页              │
└────────────────────────────┬─────────────────────────────────┘
                             │ HTTPS / REST
┌────────────────────────────▼─────────────────────────────────┐
│                      tsz-go API                               │
│   class │ vocabulary │ learning │ progress │ question │ …    │
└────────────┬───────────────────┬─────────────────────────────┘
             │                   │
        PostgreSQL          OSS（音频）
```

| 方案 | 结论 |
|------|------|
| **模块化单体** | **首选**：业务耦合在「学习 ↔ 词表 ↔ 班级」，单库事务简单 |
| 微服务 | 规模与团队未到拆分边界前不引入 |
| Serverless | 学习会话有状态、TTS 批量任务，不适合 |

**后期可按需加：**

- Redis + [asynq](https://github.com/hibiken/asynq)：TTS、报告导出等异步任务
- Redis 缓存：标准词库热读
- 读副本：统计查询

---

## 3. 系统边界

```
              ┌──────────────────────────┐
              │        管理后台           │
              │  班级 / 词库 / 题库 / 教师  │
              └────────────┬─────────────┘
                           │
                ┌──────────┴──────────┐
                │                     │
         ┌──────▼──────┐      ┌──────▼──────┐
         │   教师端     │      │   学员端     │
         │ 本班/词表/报告│      │ 学习/统计   │
         └─────────────┘      └─────────────┘
```

- 统一 API：`/api/v1/...`
- 鉴权：**角色 + 班级范围**（如 `class_id IN (...)`）
- 前端独立仓库，OpenAPI 生成客户端类型

**用户与班级：**

- 绑定班级的用户：使用教师/管理员下发的词表
- 未绑定班级的用户：自行维护词表与学习集（可购会员，见需求脑图）

---

## 4. 后端模块

垂直切片：`handler → service → repository`。

```
internal/
├── config/
├── auth/           # JWT、角色、班级范围
├── class/          # 班级、用户-班级关系、教师任课
├── user/           # 账号、登录注册（已有）
├── vocabulary/     # 词表、标准/自定义词库
├── question/       # 题库
├── learning/       # 学习集、会话、学习模型引擎
├── progress/       # 学习统计、教师报告
├── task/           # 任务下发、完成统计
├── review/         # 审核状态机（词表 / 教师申请 / 评论）
├── wallet/         # 天生币账本、发放/扣除、排行
├── stats/          # 后台首页数据聚合
├── audit/          # 后台敏感操作审计日志
├── integration/    # 词典、COCA、科大讯飞 TTS
├── media/          # 音频、OSS
└── platform/
    ├── database/
    ├── httpserver/
    ├── queue/      # 后期
    └── storage/    # 后期
```

| 模块 | 职责 |
|------|------|
| `class` | 班级 CRUD、用户归属班级、教师与班级关系 |
| `vocabulary` | 词表文件夹、排序（教材序/字母/COCA）、标准库引用 |
| `question` | 选择/填空/完形/阅读等题型 |
| `learning` | 学习模型调度、当前学习集、答题记录 |
| `progress` | 学员统计；教师报告仅限本人布置的词表 |
| `task` | 学习任务下发、班级完成率统计 |
| `review` | 共享审核状态机：词表、教师入驻申请、评论 |
| `wallet` | 天生币不可变账本、发放/扣除、教师排行 |
| `stats` | 后台首页各类计数（累计/今日/近 N 日）聚合 |
| `audit` | 后台敏感操作留痕（发币、审核、删用户…） |
| `integration` | 外部 API 防腐层 |
| `media` | TTS 任务、音频存储 |

---

## 5. 学习模型引擎

独立为 `learning` 域内的纯函数 + 状态机，便于单测。

**规则：**

1. 一轮内尽量让每个词至少出现一次
2. 相邻题目题型不重复
3. 高优先级词优先

```
LearningSet
  ├── WordItems[]      词 + 优先级 + 掌握度
  ├── QuestionTypes[]  闪卡、判断、选择、有声/无声拼写…
  └── Scheduler          输出下一题

Scheduler：
  - 每词「本轮是否已出现」
  - 记录上一题题型
  - 候选排序：priority DESC → 未出现 → 掌握度 ASC
  - 选词 + 选题型（排除上一题型）
```

| 实体 | 用途 |
|------|------|
| `learning_sets` | 当前学习集 |
| `learning_sessions` | 练习会话 |
| `learning_attempts` | 单题作答 |
| `word_mastery` | 掌握度 |

---

## 6. 词库

| 类型 | 说明 |
|------|------|
| **标准词库** | 四级难度，词性/音标/例句可编辑，TTS 可校正 |
| **非标准词库** | 词典拉取 + TTS 自动生成 |
| **班级 / 教师词表** | 引用标准库或自定义 |

**文件夹分类：** 通用（年级/学期/单元）、雅思/托福、专业（化学/经济/物理…）

**排序：** 教材序（默认）、字母序、COCA 词频

**TTS 流水线（异步）：**

```
词项 → 词典 API → 科大讯飞 TTS → OSS → 记录 audio_url → 后台可校正
```

**智能词库创编（后端已实现）：** admin 侧词条创作（方言/词形/发音/语法结构/多维
词义/关联词，草稿→发布）已落地——数据模型与接口契约见
[wordlist-module-design.md](wordlist-module-design.md)，代码 `internal/word/`，
路由 `/api/v1/admin/words`；TTS/OSS 音频只留字段，接入时走上面的流水线。

---

## 7. 权限模型

### 班级结构

```
Class（班级）
  ├── 教师（可多个）
  └── 学员（用户）
```

### 角色

| 角色 | 范围 | 约束 |
|------|------|------|
| `admin` | 全系统 | 班级、词库、题库 |
| `teacher` | 自己的班 | 不见学员联系方式；报告仅含自己布置的词表 |
| `user` | 本人数据 + 所属班级内容 | 未绑班则仅个人词表与学习集 |

### 实现

- JWT：`sub`, `roles[]`, `class_ids[]`（或查库推导）
- service 层校验班级范围，不只靠 handler
- 按角色过滤 DTO 敏感字段
- 标准词库与班级词表通过引用关联

---

## 8. 数据模型（简化 ER）

```
Class ──< ClassUser >── User
  │
  ├── role: teacher | member
  └──< ClassWordList >── WordList

WordList ──< WordListItem >── Word ──< WordSense / WordAudio
  │
  ├── scope: standard | class | teacher
  └── folder, sort_mode

LearningSet ──< LearningSession ──< LearningAttempt

QuestionBank ──< Question
```

**索引关注：**

- `class_users(class_id, user_id)`
- `learning_attempts(user_id, created_at)`
- `word_list_items(word_list_id, sort_order)`
- `word_lists(scope, owner_id)`

---

## 9. 管理后台（审核 / 天生币 / 审计 / 看板）

管理后台与学员端、教师端共用**同一套 tsz-go API 与数据库**，不拆独立服务。
后台接口统一挂在 `/api/v1/admin/...`，由 admin 鉴权中间件保护。

但**身份是两套独立的库**：admin 账号存 `admins` 表，与 web 端 `users` 完全分离——
互不能登录、互不能使用，同一手机号可在两边各存一个无关联账号。隔离由 admin 独立的 JWT
签名密钥强制（web token 拿到 admin 接口验签即失败）。完整设计与前端对接见
[user-module-design.md](user-module-design.md)，本节只讲后台业务（审核 / 天生币 / 审计 / 看板）。

```
                /api/v1/admin/...
        ┌───────────────┬───────────────┬──────────────┐
     用户/班级       审核管理        天生币管理      首页数据
   user / class    review          wallet          stats
        │             │               │ + audit        │
        └─────────────┴───────────────┴────────────────┘
                        同一 PostgreSQL
```

### 9.1 后台身份与权限

身份模型见 [user-module-design.md](user-module-design.md)，要点：

- **独立身份库 `admins`**，与 `users` 分表；`admins.level ∈ {admin, super_admin}`
  （web 端 `user_roles` 收回 `student`/`teacher`，不再有 admin）。
- 登录走 `/api/v1/admin/auth/login`，签发 `realm=admin` 且用 **独立密钥**
  （`ADMIN_JWT_SECRET`）签名的 access token；中间件 `AdminAuthRequired` 验签 + 验 realm。
- **超管专属**：建/禁用 admin 账号需 `super_admin`（`RequireSuperAdmin` → 403）。首个超管由
  `cmd/seed` 带外引导。
- 后台可见学员联系方式（教师端仍脱敏，见第 7 节）。

初期**不上细粒度 RBAC**：单 `admin` 层级即可操作审核/天生币/看板，super_admin 仅多出账号管理。
原型「审核管理」独立菜单暗示可能出现**专职审核员 ≠ 超管**——等真有这需求，再在 admin 库内加
权限点枚举（`review`/`coin`/`content`/`setting`…）按点校验，而非现在过早 RBAC。

### 9.2 审核管理（review）

词表审核、教师入驻申请、评论审核三者流程一致：`pending → approved | rejected`。

**设计取舍：** 不建「万能多态审核表」（`target_type + target_id` 破坏外键完整性、查询困难）。
改为**各业务实体自带审核字段**，状态流转逻辑抽到共享 `review` 包复用：

```
<entity>.status         pending | approved | rejected
<entity>.reviewed_by    admin user id
<entity>.reviewed_at    timestamptz
<entity>.reject_reason  text
```

| 审核对象 | 触发来源 | 通过后果 |
|---------|---------|---------|
| 词表 | 教师/用户提交自定义词表 | 词表对班级/公开可见 |
| 教师入驻申请 | 用户申请成为教师 | 账号获得 `teacher` 角色 |
| 评论 | 用户发表评论 | 评论公开展示 |

每次审核动作写一条 `audit`（见 9.4）。

### 9.3 天生币（wallet）——账本优先

天生币是**类货币资产**，有发放/扣除、并需「教师持币排行 TOP 10」。

**硬约束：余额不可用一个 `balance` 列直接 `UPDATE`。** 真相是不可变流水账本：

```
coin_ledger（只追加，不修改 / 删除）
  ├── account_id      持有者（user）
  ├── delta           变动量（发放为正，扣除为负）
  ├── type            grant | deduct | reward | consume | ...
  ├── ref_type/ref_id 关联业务（任务、订单…）
  ├── operator_id     操作的 admin（系统发放为 null）
  ├── idempotency_key 幂等键，唯一约束，防重复发放
  └── created_at
```

- **余额** = 账本对 `account_id` 求和；高频读可加带版本号的缓存余额列，但账本永远是源
- **发放/扣除** 在单事务内写账本（+ 可选更新缓存余额），失败整体回滚
- **幂等**：同一 `idempotency_key` 重复提交不二次入账
- **排行榜** = 账本按 `account_id` 聚合，配合教师角色过滤；量大后落每日快照表
- 所有发放/扣除写 `audit`

### 9.4 审计日志（audit）

发币、审核、删改用户等敏感操作必须可追溯——处理货币与内容审核的系统无审计不合规。

```
admin_audit_log
  ├── actor_id    操作的 admin
  ├── action      coin.grant | review.approve | user.delete | ...
  ├── target      ref_type + ref_id
  ├── detail      jsonb（变更前后 / 金额 / 理由）
  └── created_at
```

只追加；后台提供按 actor / action / 时间范围的查询。

### 9.5 首页数据（stats）

累计 / 今日 / 近三日 / 近 7 日 / 本周 / 本月 等计数（用户、任务、词库、天生币）。

- **当前规模直接 `COUNT(*) + 时间窗口` 查询即可**，不提前上物化视图
- 各卡片对应一个聚合查询，`stats` 模块统一编排返回
- 后期慢了再加：每日汇总表（rollup）或读副本（见第 11 节），天生币排行可走 9.3 的快照表

---

## 10. 前端建议

| 端 | 技术方向 |
|----|----------|
| 学员 App | Flutter / RN / Uni-app |
| 教师 / 管理后台 | React + Ant Design 或 Vue + Element Plus |
| 落地页 | Next.js / 静态站 |

---

## 11. 外部集成

| 集成 | 方式 |
|------|------|
| 词典 API | `integration/dictionary` |
| COCA 词频 | 导入 PostgreSQL |
| 科大讯飞 TTS | 异步任务 + 重试 |
| 对象存储 | 预签名 URL |

---

## 12. 部署

**当前：** Docker Compose（app + postgres + migrate）

**生产演进：** LB → tsz-go × N → PostgreSQL（+ 副本）/ Redis / OSS

- 日志：`slog` JSON
- 迁移：`cmd/migrate` 独立执行，生产不用 `AUTO_MIGRATE=true`

---

## 13. 实施路线

### Phase 1 — 基础

- [x] 用户注册登录、JWT
- [ ] 角色 RBAC + 班级范围
- [ ] 班级 CRUD、用户绑班
- [ ] 基础词表 CRUD

### Phase 2 — 学习闭环

- [ ] 学习集、调度器
- [ ] 闪卡 / 判断 / 选择 / 拼写
- [ ] 学习记录与统计
- [ ] 教师进度报告

### Phase 3 — 内容

- [ ] 标准词库 CMS
- [ ] TTS pipeline + 音频校正
- [ ] 题库
- [ ] 会员（未绑班用户）

### Phase 4 — 管理后台

- [ ] `admin` 角色 + 权限点中间件
- [ ] 审核管理（词表 / 教师申请 / 评论）
- [ ] 天生币账本 + 发放/扣除 + 排行
- [ ] 审计日志
- [ ] 首页数据看板

### Phase 5 — 规模化

- [ ] Redis + asynq
- [ ] 读副本、报表导出

---

## 14. 与现有代码衔接

1. `make migrate-create name=...`
2. 新建 `internal/<domain>/`（参考 `internal/user/`）
3. `cmd/server/main.go` 装配
4. `router.go` 注册路由
5. service 层做权限与班级范围校验

Repository 可后续换 [sqlc](https://sqlc.dev)，service 接口不变。

---

## 15. 总结

| 项 | 结论 |
|----|------|
| 架构 | 模块化单体 + PostgreSQL |
| 层级 | **仅班级** |
| 角色 | `admin` / `teacher` / `user` |
| 核心难点 | 学习调度引擎、词库 CMS + TTS |
