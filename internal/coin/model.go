// Package coin is a vertical slice for the 天生币 (platform points) domain: HTTP
// handler, business logic (service), and data access (repository) live together
// here.
//
// 天生币 is an in-platform integer point currency, NOT a blockchain coin. Its
// holders span BOTH identity realms (see docs/coin-module-design.md): web users
// (student/teacher, internal/user) and the admin-realm 词库管理员 (internal/admin,
// level=admin). The super_admin holds no wallet — it only operates the platform.
//
// The ledger is the source of truth: every balance change appends one immutable
// coin_ledger row; coin_wallet.balance is a derived snapshot. Nothing ever
// updates or deletes a ledger row — a correction is a reversal (红冲): a new,
// opposite entry that nets the original to zero while keeping it on the books.
package coin

import (
	"time"

	"github.com/google/uuid"
)

// Realm names which identity store a wallet owner lives in. It mirrors the JWT
// realm claim (see internal/auth): web users vs. admin accounts are unrelated
// stores, so a wallet is keyed by (realm, owner_id), not a single foreign key.
type Realm string

const (
	RealmWeb   Realm = "web"
	RealmAdmin Realm = "admin"
)

// Valid reports whether r is a known realm.
func (r Realm) Valid() bool { return r == RealmWeb || r == RealmAdmin }

// Direction is whether a biz type credits (income) or debits (expense) the
// wallet. It is a static property of the BizType, used to validate that a posted
// amount carries the right sign (income must be positive, expense negative).
type Direction string

const (
	DirIncome  Direction = "income"
	DirExpense Direction = "expense"
)

// BizType is the "收支方式" of a ledger entry: why the balance moved. Which
// biz types are legal for which owner role is enforced in the service from the
// registry below — never trusted from the client.
type BizType string

const (
	// Income — web users (student/teacher).
	BizFirstLogin          BizType = "first_login"           // 首次登录
	BizPlatformRecharge    BizType = "platform_recharge"     // 平台充值 (支付接入留待单独立项)
	BizWordlistTipReceived BizType = "wordlist_tip_received" // 词表获得投币
	BizInviteFriend        BizType = "invite_friend"         // 邀请好友
	BizInvited             BizType = "invited"               // 被邀请
	BizDailyTask           BizType = "daily_task"            // 完成每日任务 (仅学生)

	// Income — admin 词库管理员.
	BizCreateWord     BizType = "create_word"     // 创建词汇
	BizCreateWordlist BizType = "create_wordlist" // 创建词表

	// Income — back-office initiated (super_admin grants coins to anyone).
	BizPlatformGrant BizType = "platform_grant" // 平台发放

	// Expense — web users (student/teacher).
	BizCreateCustomEntry BizType = "create_custom_entry" // 创建自定义词条
	BizGiveTip           BizType = "give_tip"            // 投币
	BizChangeDialect     BizType = "change_dialect"      // 修改学习的方言

	// Expense — back-office initiated (super_admin deducts from anyone).
	BizPlatformDeduct BizType = "platform_deduct" // 平台扣除

	// Expense — admin 词库管理员 only.
	BizWithdrawal BizType = "withdrawal" // 提现 (换现金), 挂 coin_withdrawal 单
)

// Origin is who triggers a biz type, which decides whether created_by is
// required (back-office actions must record the operating admin).
type Origin string

const (
	OriginUser     Origin = "user"       // user-initiated (give_tip)
	OriginSystem   Origin = "system"     // automatic (first_login, daily_task, create_word)
	OriginBackoffi Origin = "backoffice" // super_admin manual (platform_grant/deduct)
)

// bizMeta is the static description of a BizType. The registry below is the one
// place that defines direction, origin, and which (realm, role) owners may carry
// each biz type — the service validates against it.
type bizMeta struct {
	dir    Direction
	origin Origin
	// roles is the set of "<realm>:<role>" owners allowed to hold this biz type,
	// e.g. "web:student", "admin:admin". The role is the web active role
	// (student/teacher) or the admin level (admin=词库管理员, super_admin).
	roles map[string]bool
}

// webLearners / coinAdmin are the common owner sets, named for readability.
var (
	webLearners = map[string]bool{"web:student": true, "web:teacher": true}
	webStudent  = map[string]bool{"web:student": true}
	coinAdmin   = map[string]bool{"admin:admin": true} // 词库管理员
	anyHolder   = map[string]bool{
		"web:student": true, "web:teacher": true, "admin:admin": true,
	}
)

// bizRegistry is the single source of truth for biz-type semantics (see
// docs/coin-module-design.md §4). A biz type absent here is unknown and rejected.
var bizRegistry = map[BizType]bizMeta{
	BizFirstLogin:          {DirIncome, OriginSystem, webLearners},
	BizPlatformRecharge:    {DirIncome, OriginSystem, webLearners},
	BizWordlistTipReceived: {DirIncome, OriginSystem, webLearners},
	BizInviteFriend:        {DirIncome, OriginSystem, webLearners},
	BizInvited:             {DirIncome, OriginSystem, webLearners},
	BizDailyTask:           {DirIncome, OriginSystem, webStudent}, // 学生独有
	BizCreateWord:          {DirIncome, OriginSystem, coinAdmin},
	BizCreateWordlist:      {DirIncome, OriginSystem, coinAdmin},
	BizPlatformGrant:       {DirIncome, OriginBackoffi, anyHolder},

	BizCreateCustomEntry: {DirExpense, OriginUser, webLearners},
	BizGiveTip:           {DirExpense, OriginUser, webLearners},
	BizChangeDialect:     {DirExpense, OriginUser, webLearners},
	BizPlatformDeduct:    {DirExpense, OriginBackoffi, anyHolder},
	BizWithdrawal:        {DirExpense, OriginUser, coinAdmin},
}

// Wallet is the derived balance snapshot for one owner. Balance is always >= 0.
type Wallet struct {
	Realm   Realm     `json:"realm"`
	OwnerID uuid.UUID `json:"owner_id"`
	Balance int64     `json:"balance"`
	Version int64     `json:"-"`
}

// LedgerEntry is one immutable ledger row. Amount is signed: positive credits
// (收入), negative debits (支出); the UI's 类型 column is just its sign.
type LedgerEntry struct {
	ID           uuid.UUID  `json:"id"`
	Realm        Realm      `json:"realm"`
	OwnerID      uuid.UUID  `json:"owner_id"`
	Amount       int64      `json:"amount"`
	BalanceAfter int64      `json:"balance_after"`
	BizType      BizType    `json:"biz_type"`
	Note         string     `json:"note"`
	ReversalOf   *uuid.UUID `json:"reversal_of,omitempty"`
	CreatedBy    *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Direction reports whether the entry is income or expense (by amount sign).
func (e LedgerEntry) Direction() Direction {
	if e.Amount < 0 {
		return DirExpense
	}
	return DirIncome
}

// LedgerFilter narrows a per-owner ledger listing (图2). Direction empty means
// both; From/To are an inclusive created_at range (zero = unbounded). Page is
// 1-based; PageSize is clamped by the handler.
type LedgerFilter struct {
	Direction Direction
	From, To  time.Time
	Page      int
	PageSize  int
}
