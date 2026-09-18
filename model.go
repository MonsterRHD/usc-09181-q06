package main

import "time"

// 事件结果状态
const (
	statusAccepted  = "accepted"  // 已入账
	statusDeclined  = "declined"  // 业务拒绝（余额不足、超授权等）
	statusDuplicate = "duplicate" // 同一网络事件 ID 重放，原样返回，不再次动账
	statusPending   = "pending_review"
)

// 账户：多币种钱包。只保存卡号令牌与掩码，永不保存完整 PAN（监管最小记录）。
type Account struct {
	ID           string           `json:"id"`
	HolderID     string           `json:"holder_id"`
	CardToken    string           `json:"card_token,omitempty"` // 内部令牌，不对客户展示
	CardMask     string           `json:"card_mask"`            // 如 "6225********1234"
	HomeCurrency string           `json:"home_currency"`
	Balances     map[string]int64 `json:"balances"` // 已清算余额（最小单位）
	Holds        map[string]int64 `json:"holds"`    // 预授权冻结总额
	Fees         map[string]int64 `json:"fees"`     // 已实现手续费累计
	TxnLimit     map[string]int64 `json:"txn_limit"` // 持卡人单笔授权上限（含费）
	FxFeeBps     int              `json:"fx_fee_bps"` // 非本币交易手续费（基点）
	RebillBps    int              `json:"rebill_bps"` // 允许商户补扣占清算额比例（基点）
	CreatedAt    time.Time        `json:"created_at"`
}

func (a *Account) feeBps(ccy string) int {
	if ccy == a.HomeCurrency {
		return 0
	}
	return a.FxFeeBps
}

// CardEvent 是网络/终端上送事件。
// Type: auth 预授权 | reversal 撤销(可部分) | presentment 清算/离线补传 | rebill 商户补扣
type CardEvent struct {
	EventID       string    `json:"event_id"`        // 网络唯一流水（RRN/STAN），幂等键
	Type          string    `json:"type"`
	AccountID     string    `json:"account_id"`
	MerchantID    string    `json:"merchant_id"`
	MerchantName  string    `json:"merchant_name"`
	MerchantRef   string    `json:"merchant_ref"` // 商户订单/ folio 号，用于重试去重
	TerminalID    string    `json:"terminal_id"`
	Currency      string    `json:"currency"`
	Amount        int64     `json:"amount"` // 最小单位，恒为正；方向由类型决定
	TxnTime       time.Time `json:"txn_time"`
	LinkedEventID string    `json:"linked_event_id"` // 关联的授权/原交易
	Offline       bool      `json:"offline"`
}

type EventResult struct {
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
	EntryIDs []string `json:"entry_ids,omitempty"`
}

// ProcessedEvent 记录事件处理结果及授权生命周期累计额。
type ProcessedEvent struct {
	Event         CardEvent   `json:"event"`
	Result        EventResult `json:"result"`
	ReceivedAt    time.Time   `json:"received_at"`
	AuthAmount    int64       `json:"auth_amount,omitempty"`
	HeldRemaining int64       `json:"held_remaining,omitempty"` // 当前仍冻结（本金+费）
	ReversedPre   int64       `json:"reversed_pre,omitempty"`   // 清算前累计部分撤销本金
	Settled       bool        `json:"settled,omitempty"`
	SettledAmount int64       `json:"settled_amount,omitempty"`
	RefundedPost  int64       `json:"refunded_post,omitempty"` // 清算后撤销/退货已退本金
	Rebilled      int64       `json:"rebilled,omitempty"`
	Chargeback    int64       `json:"chargeback,omitempty"`
}

// Entry 是不可变账务流水，以哈希链串接，任何追溯改账都会断链。
type Entry struct {
	Seq           int       `json:"seq"`
	ID            string    `json:"id"`
	AccountID     string    `json:"account_id"`
	EventID       string    `json:"event_id"`
	Kind          string    `json:"kind"` // hold|hold_release|settle|fee|reversal|rebill|chargeback
	Currency      string    `json:"currency"`
	Amount        int64     `json:"amount"` // 本金部分
	Fee           int64     `json:"fee,omitempty"`
	DeltaBalance  int64     `json:"delta_balance"`
	DeltaHold     int64     `json:"delta_hold"`
	LinkedEventID string    `json:"linked_event_id,omitempty"`
	SourceCurrency string   `json:"source_currency,omitempty"` // 外币交易的原始币种
	SourceAmount  int64     `json:"source_amount,omitempty"`   // 原始本金（最小单位）
	FxRateMicros  int64     `json:"fx_rate_micros,omitempty"`  // 该笔入账实际固定的汇率
	FxSnapshotID  string    `json:"fx_snapshot_id,omitempty"`  // 同源时为 "identity"
	BusinessTime  time.Time `json:"business_time"`
	RecordedAt    time.Time `json:"recorded_at"`
	PrevHash      string    `json:"prev_hash"`
	Hash          string    `json:"hash"`
}

type Notification struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Type      string    `json:"type"`
	DedupKey  string    `json:"dedup_key"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	EventID   string    `json:"event_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// FXSnapshot 汇率快照。异常（偏离在审阈值）进入 pending_review，绝不静默采用。
type FXSnapshot struct {
	ID             string     `json:"id"`
	Pair           string     `json:"pair"`
	RateMicros     int64      `json:"rate_micros"`
	Rate           string     `json:"rate"`
	Source         string     `json:"source"`
	EffectiveAt    time.Time  `json:"effective_at"`
	ReceivedAt     time.Time  `json:"received_at"`
	Status         string     `json:"status"` // accepted | pending_review | rejected
	PriorRateMicros int64     `json:"prior_rate_micros,omitempty"`
	DeviationBps   int64      `json:"deviation_bps,omitempty"`
	ReviewedBy     string     `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time `json:"reviewed_at,omitempty"`
}

type FXPair struct {
	Pair       string       `json:"pair"`
	CurrentID  string       `json:"current_id"`
	PendingIDs []string     `json:"pending_ids"`
	Snapshots  []*FXSnapshot `json:"snapshots"`
}

// DisplayBasis 固定“某一次展示”所用的换算依据，不可变。
type DisplayBasis struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	Subject      string    `json:"subject"` // 如 balance:USD 或 event:E123
	FromCurrency string    `json:"from_currency"`
	ToCurrency   string    `json:"to_currency"`
	SourceAmount int64     `json:"source_amount"`
	RateMicros   int64     `json:"rate_micros"`
	Rate         string    `json:"rate"`
	SnapshotID   string    `json:"snapshot_id"` // 同源（1:1）时为 "identity"
	Converted    int64     `json:"converted_amount"`
	CreatedAt    time.Time `json:"created_at"`
}

type DisputeNote struct {
	Seq      int       `json:"seq"`
	AuthorID string    `json:"author_id"`
	Role     string    `json:"role"` // customer | agent
	Kind     string    `json:"kind"` // statement（客户陈述）| conclusion（客服调查结论）
	Content  string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Dispute struct {
	ID        string          `json:"id"`
	AccountID string          `json:"account_id"`
	EventID   string          `json:"event_id"`
	Reason    string          `json:"reason"`
	Status    string          `json:"status"` // open | under_review | won | lost
	CreatedAt time.Time       `json:"created_at"`
	Notes     []DisputeNote   `json:"notes"` // 只追加
}

// AuditRecord 敏感操作审计（汇率复核、争议裁决、监管导出），同样哈希链。
type AuditRecord struct {
	Seq       int       `json:"seq"`
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	ActorRole string    `json:"actor_role"`
	ActorID   string    `json:"actor_id"`
	AccountID string    `json:"account_id,omitempty"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
}

type State struct {
	Counter       int64                       `json:"counter"`
	Accounts      map[string]*Account         `json:"accounts"`
	Events        map[string]*ProcessedEvent  `json:"events"`
	RefIndex      map[string]string           `json:"ref_index"` // kind|account|merchant|ref -> eventID
	Entries       []*Entry                    `json:"entries"`
	Disputes      map[string]*Dispute         `json:"disputes"`
	Notifications map[string][]*Notification  `json:"notifications"`
	FX            map[string]*FXPair          `json:"fx"`
	DisplayBases  []*DisplayBasis             `json:"display_bases"`
	Audit         []*AuditRecord              `json:"audit"`
	LastEntryHash string                      `json:"last_entry_hash"`
	LastAuditHash string                      `json:"last_audit_hash"`
}
