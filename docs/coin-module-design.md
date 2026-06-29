# 天生币模块详细设计 —— 跨双身份库的积分账本

> 本文是「**天生币**」（平台内虚拟积分，非区块链）模块的**细节设计 + 前端对接契约**，落地为
> 一个新纵切 `internal/coin`：**append-only 流水（事实来源）+ 余额快照 + 提现单**。
>
> 天生币横跨[双身份库](user-module-design.md)：持有者既有 web 用户（学生/教师），也有 admin
> 库的词库管理员。本文是天生币模块当前唯一事实来源；接口契约最终以 [openapi.yaml](openapi.yaml)
> 为准，落地时与之对齐，并同步 [api.md](api.md)、[architecture.md](architecture.md)。
>
> 名词：天生平台产品名「天生会背」（背单词）。**币恒为整数**；"现金"只在提现单出现。

---

## 1. 设计原则

1. **账本即会计系统，正确性高于一切。** 天生币的难点 100% 在账本一致性，而非功能数量。
   余额一旦能算错 / 超扣 / 对不上流水，整个币就废了。下列三条不变量（§5）是地基，不可破。
2. **流水是唯一事实来源，余额是派生快照。** 每一笔变动写一条**不可变**流水（append-only），
   `coin_wallet.balance` 只是为了快速读取的快照，任何时候都能用流水重算校验。
3. **删除 = 红冲，永不硬删。** 图1 后台的「删除」不真删记录，而是生成一条反向流水把余额冲平，
   原始记录永久保留可审计（§5.2）。
4. **币恒为整数，提现才有现金。** 天生币以整数计（图里 +889 / −675）；"换成现金67.5"是提现单按
   汇率算出的派生值，**不进币账本**，避免浮点误差。
5. **跨双身份库用多态主人，不破坏隔离。** 持有者用 `(owner_realm, owner_id)` 弱引用 web/admin 两库，
   不强加单一外键；展示时各自 join 回身份库取昵称/头像/联系方式（§3.1）。
6. **依赖方向不变：** `handler → service → repository`，与 user/admin 同构的纵切。

---

## 2. 谁持有天生币

| 维度 | web 学生 | web 教师 | admin 词库管理员 | admin 超管 |
|------|---------|---------|------------------|-----------|
| 身份库 | `users`（role=student） | `users`（role=teacher） | `admins`（level=`admin`） | `admins`（level=`super_admin`） |
| `owner_realm` | `web` | `web` | `admin` | — |
| 持币钱包 | ✅ | ✅ | ✅ | ❌ **不持币** |
| 可提现 | ❌ | ❌ | ✅（换现金） | ❌ |
| 出现在全平台列表(图1) | ✅ | ✅ | ✅ | ❌ |

> **超管不持币**：图1 全平台列表只展示学生/教师/词库管理员；超管只做平台管理（含给他人发币/扣币、
> 红冲），其操作者身份记在流水的 `created_by`，自己没有钱包。

---

## 3. 数据模型

### 3.1 多态主人 `(owner_realm, owner_id)`

天生币持有者横跨两个**互不关联**的身份库，无法用单一外键。采用：

- `owner_realm ∈ {'web','admin'}`，`owner_id` 指向对应库主键（`users.id` / `admins.id`）。
- **不建数据库外键、不 CASCADE。** 这是有意的：**账本必须比账号活得久**——web 用户可注销
  （`users` 硬删 + CASCADE），但其天生币流水（尤其词库管理员涉及现金）不能随之消失。owner 用
  弱引用，展示时 LEFT JOIN 回身份库；账号已删则昵称栏回退占位（如「已注销用户」）。
- 角色展示（图1「角色类型」列）由 `owner_realm` + join 后的 role/level 推导，不冗余存进流水。

### 3.2 `coin_wallet` —— 余额快照

```sql
-- 每个持币主体一行。balance 是派生快照(可用 coin_ledger 重算), 恒非负。
-- TEXT + CHECK(无 enum 类型)沿用 users/admins 风格。
CREATE TABLE coin_wallet (
    owner_realm TEXT        NOT NULL CHECK (owner_realm IN ('web', 'admin')),
    owner_id    UUID        NOT NULL,
    balance     BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0), -- 整数, 防透支
    version     BIGINT      NOT NULL DEFAULT 0,  -- 乐观锁版本, 每次变动 +1
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_realm, owner_id)
);
```

### 3.3 `coin_ledger` —— 不可变流水（事实来源）

```sql
-- Append-only。每一笔天生币变动一条; 永不 UPDATE/DELETE。balance 由它推导。
CREATE TABLE coin_ledger (
    id            UUID        PRIMARY KEY,
    owner_realm   TEXT        NOT NULL CHECK (owner_realm IN ('web', 'admin')),
    owner_id      UUID        NOT NULL,
    -- 正=收入, 负=支出(图1的 +889 / -675)。类型列(收入/支出)= 符号, 不单独存。
    amount        BIGINT      NOT NULL CHECK (amount <> 0),
    balance_after BIGINT      NOT NULL,            -- 该笔后的余额, 可重算自检
    biz_type      TEXT        NOT NULL,            -- 收支方式枚举, 见 §4
    note          TEXT        NOT NULL DEFAULT '', -- 备注(图里过长以"..."表示, 悬浮看全文)
    -- 红冲: 指向被冲掉的原始流水。原始流水永久保留, 仅追加一条反向流水。
    reversal_of   UUID        REFERENCES coin_ledger(id),
    -- 幂等键: 外部触发的入账(充值回调/任务发放/邀请)带它, 防重复入账。
    idempotency_key TEXT,
    -- 操作者: 平台扣除/发币/红冲等由后台发起时记 admin.id; 用户自发行为为 NULL。
    created_by    UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 个人流水(图2)与全平台按主体筛选(图1)都按 owner + 时间倒序翻页。
CREATE INDEX coin_ledger_owner_idx ON coin_ledger (owner_realm, owner_id, created_at DESC);
-- 全平台按时间倒序翻页(图1 默认视图)。
CREATE INDEX coin_ledger_created_idx ON coin_ledger (created_at DESC);
-- 幂等: 同一外部事件只入账一次。
CREATE UNIQUE INDEX coin_ledger_idem_uniq ON coin_ledger (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- 一条流水至多被红冲一次(防重复红冲)。
CREATE UNIQUE INDEX coin_ledger_reversal_uniq ON coin_ledger (reversal_of)
    WHERE reversal_of IS NOT NULL;
```

### 3.4 `coin_withdrawal` —— 提现单（仅词库管理员）

```sql
-- 仅 admin 库词库管理员可提现。提现 = 扣币(进 ledger) + 换现金(财务流程)。
-- cash_amount 是按汇率算出的派生现金, 故用 NUMERIC; 币本身始终整数。
CREATE TABLE coin_withdrawal (
    id          UUID        PRIMARY KEY,
    owner_id    UUID        NOT NULL,                       -- admins.id (词库管理员)
    coin_amount BIGINT      NOT NULL CHECK (coin_amount > 0), -- 扣的天生币(整数)
    cash_amount NUMERIC(12,2) NOT NULL,                     -- 派生现金(币 / 汇率)
    status      TEXT        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'approved', 'paid', 'rejected')),
    -- 对应的扣币流水(biz_type='withdrawal')。驳回时该流水红冲、币退回。
    ledger_id   UUID        REFERENCES coin_ledger(id),
    note        TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX coin_withdrawal_owner_idx ON coin_withdrawal (owner_id, created_at DESC);
```

> **汇率 10:1**（图2：−675 币 → 现金 67.5）。汇率定为配置项 `COIN_CASH_RATE`（默认 10），
> 不写死在代码，便于以后调整。`cash_amount = coin_amount / rate`，提现单创建时一次性定格。

---

## 4. 收支方式 `biz_type` 枚举 + 角色约束

`biz_type` 是流水的"收支方式"（图里的列）。**哪些 biz_type 对哪个角色合法，由 service 层强校验**，
不靠前端藏按钮。非法组合（如让教师领"每日任务"币、让学生"创建词表"挣币）一律拒绝。

| realm / 角色 | 收入 biz_type | 支出 biz_type |
|------|------|------|
| web / **学生** | `first_login` 首次登录、`platform_recharge` 平台充值、`wordlist_tip_received` 词表获得投币、`invite_friend` 邀请好友、`invited` 被邀请、`daily_task` **完成每日任务** | `create_custom_entry` 创建自定义词条、`platform_deduct` 平台扣除、`give_tip` 投币、`change_dialect` 修改学习的方言 |
| web / **教师** | `first_login`、`platform_recharge`、`wordlist_tip_received`、`invite_friend`、`invited`（**无** `daily_task`） | `create_custom_entry`、`platform_deduct`、`give_tip`、`change_dialect` |
| admin / **词库管理员** | `create_word` 创建词汇、`create_wordlist` 创建词表 | `platform_deduct` 平台扣除、`withdrawal` **提现（换现金）** |

> **`withdrawal` ≠ `platform_deduct`：** 图2 备注把"换成现金"显示为「平台扣除」，但语义上是**提现**，
> 必须用独立 `biz_type='withdrawal'` 并挂 `coin_withdrawal` 单，与管理员手动扣币（`platform_deduct`）
> 区分开——否则提现金额无法对账、驳回退币也无从下手。前端展示文案可仍叫"平台扣除"，但底层类型不同。

实现上每个 biz_type 还带两个静态属性，集中在 `model.go` 一张表里定义：

- **方向**：收入 / 支出（决定 amount 符号，service 据此防止"收入记成负数"之类错误）。
- **触发方**：用户自发（`give_tip`）/ 系统自动（`first_login`、`daily_task`、`create_word`）/
  后台手动（`platform_deduct`）。后台手动类必须带 `created_by`。

---

## 5. 三条写入不变量（service + repository 共同保证）

### 5.1 原子记账 + 防超扣

任何余额变动是**一个数据库事务**里的两步：插一条 `coin_ledger` + 改 `coin_wallet`。扣减用**带条件的
原子 UPDATE**，靠 `RowsAffected` 判定，天然防并发超扣：

```sql
-- 在事务内。$amt 为带符号增量(收入正/支出负)。余额扣到负数则 0 行受影响 → 余额不足。
UPDATE coin_wallet
   SET balance = balance + $amt, version = version + 1, updated_at = now()
 WHERE owner_realm = $realm AND owner_id = $id AND balance + $amt >= 0
RETURNING balance;   -- 即该笔的 balance_after, 回填进 ledger
```

- `RowsAffected = 0` → 余额不足，返回 `ErrInsufficientBalance`，整个事务回滚。
- 钱包不存在时**懒创建**（首次入账即 `INSERT ... ON CONFLICT DO NOTHING` 建 0 余额行再扣/加）。
- 两个并发扣款被行锁/条件更新串行化，不会双花。**契约测试必须覆盖并发不超扣**。

### 5.2 红冲（图1「删除」语义）

后台点「删除」某条流水 → 不删，调 `Reverse(ledgerID, by)`，在一个事务内：

1. 读原始流水（`reversal_of IS NULL` 且未被冲过，否则 `ErrAlreadyReversed`）。
2. 插一条反向流水：`amount = -原始.amount`、`biz_type` 不变、`note` 标注"红冲 #原始"、
   `reversal_of = 原始.id`、`created_by = 操作管理员`。
3. 同一事务按 §5.1 把余额冲回。
4. 原始流水**原样保留**，可审计。`coin_ledger_reversal_uniq` 唯一索引防同一条被红冲两次。

> 边界：若红冲会让余额变负（用户已花掉这笔币），按业务定——默认**允许余额为负**仅在红冲场景
> 放开 `balance >= 0` 约束？**不**。改为：红冲也走非负校验，冲不动则返回 `ErrInsufficientBalance`，
> 由后台人工介入（这是真实会计里"无法红冲已消费金额"的正确行为）。该取舍落地前与产品再确认一次。

### 5.3 幂等

外部事件触发的入账（充值回调、每日任务发放、邀请奖励）一律带 `idempotency_key`（如
`recharge:<订单号>`、`daily_task:<userID>:<日期>`）。唯一索引挡重复，回调重试/用户连点只入账一次：
插入命中冲突时，返回已存在的那条流水而非报错（幂等读）。

---

## 6. 模块结构 `internal/coin`

沿用 user/admin 的纵切：`model.go / repository.go / service.go / handler.go` + 测试三件套。

### 6.1 领域类型（`model.go` 草图）

```go
type Realm string
const ( RealmWeb Realm = "web"; RealmAdmin Realm = "admin" )

type BizType string // first_login, platform_recharge, ... withdrawal (见 §4)

// Direction 由 BizType 静态决定; service 据此校验 amount 符号。
type Direction string
const ( DirIncome Direction = "income"; DirExpense Direction = "expense" )

type Wallet struct {
    Realm   Realm
    OwnerID uuid.UUID
    Balance int64
    Version int64
}

// LedgerEntry 是一条不可变流水。
type LedgerEntry struct {
    ID           uuid.UUID
    Realm        Realm
    OwnerID      uuid.UUID
    Amount       int64      // 正收入/负支出
    BalanceAfter int64
    BizType      BizType
    Note         string
    ReversalOf   *uuid.UUID // 红冲指向原始
    CreatedBy    *uuid.UUID
    CreatedAt    time.Time
}

// LedgerView 是给图1/图2 的展示行: 流水 + join 回身份库的展示字段。
type LedgerView struct {
    LedgerEntry
    DisplayName string // join users/admins
    AvatarURL   string
    RoleLabel   string // 学生/教师/词库管理员(由 realm+role 推导)
    Phone       string // 仅图1 全平台视图
    Email       string
}
```

### 6.2 Repository 接口（数据访问边界）

```go
// Post 是余额变动的唯一入口: 事务内"插流水 + 原子改余额(防超扣)"。
func (r *Repository) Post(ctx context.Context, e LedgerEntry, idem *string) (*LedgerEntry, error)
// Reverse 红冲一条流水(§5.2)。
func (r *Repository) Reverse(ctx context.Context, ledgerID, by uuid.UUID) (*LedgerEntry, error)
func (r *Repository) GetWallet(ctx context.Context, realm Realm, ownerID uuid.UUID) (*Wallet, error)
// ListByOwner: 图2 个人收支记录, 分页 + 收入/支出过滤。
func (r *Repository) ListByOwner(ctx context.Context, realm Realm, ownerID uuid.UUID, f LedgerFilter) ([]LedgerView, int, error)
// ListAll: 图1 全平台, 关键字(昵称/手机/邮箱)/角色/时间过滤, join 身份库。
func (r *Repository) ListAll(ctx context.Context, f PlatformFilter) ([]LedgerView, int, error)
// 提现单
func (r *Repository) CreateWithdrawal(ctx context.Context, w *Withdrawal) error
func (r *Repository) SetWithdrawalStatus(ctx context.Context, id uuid.UUID, s WithdrawalStatus, by uuid.UUID) error
```

> SQL 手写（沿用 user 模块；以后转 sqlc 只换实现，service 只依赖签名）。

### 6.3 Service 职责

- `biz_type` × 角色合法性校验（§4）、方向与 amount 符号一致性校验。
- 各业务发币入口（首次登录、邀请、创建词表、每日任务…）封装成意图明确的方法，
  内部拼好 `biz_type`/`idempotency_key` 调 `repo.Post`，业务方不直接碰账本。
- 提现状态机（§7.3）。

---

## 7. API 契约

通用约定同[用户模块](user-module-design.md#6-api-契约)：错误 `{ "error": "<message>" }`；access token 走
`Authorization: Bearer`；列表分页 `?page=&page_size=`（上限 100），响应 `{ "items": [...], "page": {...} }`。

### 7.1 个人视图（图2「我的天生币 / 收支记录」）

web 与 admin 两端各自一套，**只看自己**（owner = 当前 token 主体），无法看他人。

| 方法 | 路径 | 端 | 说明 |
|------|------|----|------|
| GET | `/api/v1/me/coin/balance` | web | 当前用户天生币余额 |
| GET | `/api/v1/me/coin/ledger` | web | 个人流水，`?direction=income\|expense`、时间过滤、分页 |
| GET | `/api/v1/admin/me/coin/balance` | admin | 当前词库管理员余额 |
| GET | `/api/v1/admin/me/coin/ledger` | admin | 个人流水（图2），同上过滤 |

- 字段：序号（前端按页算）、类型（收入/支出，= amount 符号）、收支方式、天生币（±amount）、备注、变更时间。
- 鉴权：web 用 `AuthRequired`，admin 用 `AdminAuthRequired`；owner 强制取自 token，不接受 query 传入。

### 7.2 全平台视图（图1「天生币记录」，后台）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/admin/coin/ledger` | 全平台流水，关键字/角色/时间过滤，join 身份库 |
| POST | `/api/v1/admin/coin/adjust` | 后台手动发币/扣币（`platform_deduct` 或定向发币），带 `created_by` |
| DELETE | `/api/v1/admin/coin/ledger/{id}` | **红冲**该流水（非硬删，§5.2），返回新生成的反向流水 |

- 过滤参数：`?q=`（昵称/手机/邮箱）、`?realm=`/`?role=`（学生/教师/词库管理员）、`?direction=`、
  `?from=&to=`（变更时间）、分页。
- 权限：本组用 `AdminAuthRequired`；定向发币/红冲是否限超管，见 §9 待定项。
- 响应行含 头像/昵称/角色/绑定电话/绑定邮箱/类型/收支方式/天生币/备注/变更时间（对齐图1列）。

### 7.3 提现（仅词库管理员）

| 方法 | 路径 | 端 | 说明 |
|------|------|----|------|
| POST | `/api/v1/admin/me/coin/withdraw` | admin（词库管理员） | 申请提现，body `{ coin_amount }`；扣币(`withdrawal`)+建 pending 单 |
| GET | `/api/v1/admin/me/coin/withdrawals` | admin | 我的提现单列表 |
| GET | `/api/v1/admin/coin/withdrawals` | 超管/财务 | 全部提现单（审核用） |
| PATCH | `/api/v1/admin/coin/withdrawals/{id}` | 超管/财务 | 审核：`approved`/`paid`/`rejected`；驳回则**红冲退币** |

状态机：`pending →（审核）approved →（打款）paid`，或 `pending →（驳回）rejected`。
驳回/取消时：红冲对应的 `withdrawal` 扣币流水，币退回钱包。打款 `paid` 是线下财务动作，
后端只记状态，**不在系统内转真钱**。

> 超管本身不持币，但作为审核者操作提现单；`created_by` 记其 admin.id。

---

## 8. 错误码一览

| 场景 | HTTP | message |
|------|------|---------|
| 余额不足（扣币/提现/红冲冲不动） | 409 | `insufficient balance` |
| biz_type 与角色不匹配 | 400 | `biz type not allowed for role` |
| 红冲一条已被红冲的流水 | 409 | `ledger already reversed` |
| 红冲一条本身是红冲的流水 | 400 | `cannot reverse a reversal` |
| 提现金额非正 / 非法 | 400 | 校验信息 |
| 非词库管理员申请提现 | 403 | `withdrawal not allowed` |
| 提现单状态流转非法（如 paid→pending） | 409 | `invalid withdrawal transition` |
| 流水 / 提现单不存在 | 404 | `not found` |
| 缺/坏 token、领域不符 | 401 | `invalid or expired token` |

---

## 9. 已拍板的关键取舍

> 下列决定数据/接口细节，已与产品确认（2026-06-29）。实现按此为准。

1. **红冲冲不动（用户已花掉该笔币）→ 报错、人工介入。** ✅ 已定。红冲走非负校验，冲不动返回
   `insufficient balance`，余额**恒非负**，由后台人工判断（符合真实会计：无法红冲已消费金额）。
   **不**提供"强制冲成负余额"模式（§5.2）。
2. **后台定向发币 / 扣币 / 红冲 → 仅超管。** ✅ 已定。这些改余额的写操作挂 `RequireSuperAdmin`；
   普通词库管理员只能看自己（图2），不能给他人发币/扣币/红冲。
3. **币不允许负余额。** ✅ 已定。`balance >= 0` 数据库硬约束（§3.2），无任何例外。
4. **汇率 10:1 且提现必须整除。** ✅ 已定。`COIN_CASH_RATE=10`（配置项）；提现币数必须是汇率的
   整数倍，`cash_amount = coin_amount / rate` 恒为整洁值，杜绝碎额小数（§3.4）。
5. **充值（`platform_recharge`）本期不做，只留 biz_type 占位。** ✅ 已定。涉及真钱入金/支付网关，
   **单独立项**；第一段保留枚举值但无任何支付接入路径。

---

## 10. 分期交付

对齐 admin 的三段式，每段独立可上线、可测：

### 第一段 —— 账本内核（重中之重）
- 迁移：`coin_wallet` + `coin_ledger`。
- `repo.Post` / `repo.Reverse` + service 的 biz_type×角色校验。
- 后台：全平台列表（图1，7.2 GET）、手动发币/扣币、红冲（删除）。
- **测试是这段的核心产出**：契约测试（fake==真库）覆盖
  ① 并发扣款不超扣 ② 幂等键重复只入账一次 ③ 红冲后余额一致且原记录保留
  ④ 余额永不为负。集成测试守既有铁律（数据全局唯一、禁用进程内计数器、禁清库）。

### 第二段 —— 个人视图 + 自动入账
- 个人收支记录（图2，7.1）web/admin 两端。
- 接入各业务触发点发币：首次登录、邀请好友/被邀请、创建词汇/词表、每日任务、投币、
  创建自定义词条、修改方言等（每个封装成 service 方法，带幂等键）。

### 第三段 —— 提现
- `coin_withdrawal` 迁移 + 提现申请/列表/审核（7.3）+ 状态机 + 驳回红冲退币。
- 配置 `COIN_CASH_RATE`、最小提现额。

---

## 11. 联调自测清单

- [ ] 后台手动给某学生发 +100 → 学生 `GET /me/coin/balance` 为 100，个人流水(图2)出现该笔
- [ ] 并发两次扣同一钱包，总额超过余额 → 只成功一次，余额不为负（无超扣）
- [ ] 同一充值订单号回调两次 → 只入账一次（幂等）
- [ ] 后台「删除」一条 +976 → 生成 −976 红冲流水，原记录仍在，余额冲平
- [ ] 红冲一条已被红冲的流水 → 409 `ledger already reversed`
- [ ] 让教师领 `daily_task` 币 → 400 `biz type not allowed for role`
- [ ] 词库管理员提现 675 币 → 扣币 + 生成 pending 单，cash_amount=67.5（汇率10）
- [ ] 驳回该提现单 → 红冲退币，余额恢复
- [ ] web 用户敲 `/api/v1/admin/coin/*` → 401；admin token 敲 `/api/v1/me/coin/*` → 401
- [ ] 全平台列表(图1)关键字搜手机号/角色过滤/时间范围均生效，账号已注销者昵称回退占位

---

## 12. 后续待办 / 硬化项

> 开工前为空。评审与第一段落地中识别出的"有意推迟项"记录于此，每条标**状态**与**触发条件**，
> 仿 [user-module-design.md §12](user-module-design.md) 的写法，避免遗忘。

- [ ] **充值 / 支付网关接入**（`platform_recharge`）—— 状态：**未做**，触发：产品确定要做付费充值时。
- [ ] **提现财务对账 / 打款回执**——状态：**未做**，触发：第三段提现上线、有真实打款流水时。
- [ ] **余额定期重算对账任务**（用 ledger 重算 wallet.balance 校验快照未漂移）——状态：**未做**，
      触发：账本规模变大或出现疑似不一致时；建议作为定时任务/巡检。
