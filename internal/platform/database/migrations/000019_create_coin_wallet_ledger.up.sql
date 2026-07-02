-- 天生币账本: 跨双身份库的平台积分。持有者既有 web 用户(学生/教师), 也有 admin 库的
-- 词库管理员; 故用 (owner_realm, owner_id) 多态弱引用两库, 不建外键(账本须比账号活得久)。
-- TEXT + CHECK(无 enum 类型)沿用 users/admins 风格 —— pgx 扫描简单, 迁移便宜。

-- IF NOT EXISTS: 本迁移原编号 000017, 曾以旧号应用到共享测试库; words 抢先合入 main
-- 占用 000018 后改号 000019。幂等建表让已有旧表的库直接跳过, 全新库照常创建。

-- 余额快照。balance 是派生值(可用 coin_ledger 重算), 恒非负。
CREATE TABLE IF NOT EXISTS coin_wallet (
    owner_realm TEXT        NOT NULL CHECK (owner_realm IN ('web', 'admin')),
    owner_id    UUID        NOT NULL,
    balance     BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    version     BIGINT      NOT NULL DEFAULT 0, -- 乐观锁版本, 每次变动 +1
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_realm, owner_id)
);

-- 不可变流水(事实来源)。append-only: 永不 UPDATE/DELETE; 撤销靠红冲(追加反向流水)。
CREATE TABLE IF NOT EXISTS coin_ledger (
    id              UUID        PRIMARY KEY,
    owner_realm     TEXT        NOT NULL CHECK (owner_realm IN ('web', 'admin')),
    owner_id        UUID        NOT NULL,
    amount          BIGINT      NOT NULL CHECK (amount <> 0), -- 正=收入, 负=支出
    balance_after   BIGINT      NOT NULL,                     -- 该笔后的余额, 可重算自检
    biz_type        TEXT        NOT NULL,                     -- 收支方式枚举(见 internal/coin)
    note            TEXT        NOT NULL DEFAULT '',
    reversal_of     UUID        REFERENCES coin_ledger(id),   -- 红冲: 指向被冲的原始流水
    idempotency_key TEXT,                                     -- 外部触发入账防重
    created_by      UUID,                                     -- 后台发起时记 admin.id
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 个人流水(图2)与全平台按主体筛选(图1): owner + 时间倒序翻页。
CREATE INDEX IF NOT EXISTS coin_ledger_owner_idx ON coin_ledger (owner_realm, owner_id, created_at DESC);
-- 全平台按时间倒序翻页(图1 默认视图)。
CREATE INDEX IF NOT EXISTS coin_ledger_created_idx ON coin_ledger (created_at DESC);
-- 幂等: 同一外部事件只入账一次。
CREATE UNIQUE INDEX IF NOT EXISTS coin_ledger_idem_uniq ON coin_ledger (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- 一条流水至多被红冲一次。
CREATE UNIQUE INDEX IF NOT EXISTS coin_ledger_reversal_uniq ON coin_ledger (reversal_of)
    WHERE reversal_of IS NOT NULL;
