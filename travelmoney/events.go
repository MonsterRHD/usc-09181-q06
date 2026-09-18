package travelmoney

import (
	"encoding/json"
	"time"
)

// 事件类型常量。事件一经追加不可修改（争议结论只能以追加结论事件体现）。
const (
	EvAccountOpened     = "AccountOpened"
	EvCardEnrolled      = "CardEnrolled"
	EvAuthorized        = "Authorized" // 刷卡 / 预授权批准
	EvAuthDeclined      = "AuthDeclined"
	EvReversed          = "Reversed" // 撤销（含部分撤销）
	EvCleared           = "Cleared"  // 清算 / 补扣（在线请款或离线补传）
	EvForcePosted       = "ForcePosted"
	EvFXRateReceived    = "FXRateReceived"    // 外部汇率快照送达（含已隔离）
	EvFXRateQuarantined = "FXRateQuarantined" // 异常汇率被隔离，等待人工复核
	EvFXRateApproved    = "FXRateApproved"    // 人工批准隔离汇率
	EvFXRateRejected    = "FXRateRejected"    // 人工驳回隔离汇率
	EvAuthExpired       = "AuthExpired"       // 授权过期，冻结自动释放
	EvDuplicateRejected = "DuplicateRejected" // 重复请款/商户重试被幂等拦截（最小审计记录）
	EvDisputeOpened     = "DisputeOpened"
	EvDisputeNoted      = "DisputeNoted"     // 客服追加调查备注（只能追加）
	EvDisputeConcluded  = "DisputeConcluded" // 客服追加最终调查结论
	EvNotificationSent  = "NotificationSent"
)

// FXSnapshotRef 是钉在每一次金额展示/换算上的汇率快照依据。
// 事件里保存的是送达当时的快照副本（值拷贝），后续新汇率不会改写历史展示。
type FXSnapshotRef struct {
	SnapshotID string    `json:"snapshot_id"`
	Pair       CcyPair   `json:"pair"`
	RateMicro  int64     `json:"rate_micro"`
	RateText   string    `json:"rate_text"`
	AsOf       time.Time `json:"as_of"`
	Source     string    `json:"source"`
}

// AccountOpenedEvent 开户。
type AccountOpenedEvent struct {
	AccountID        string    `json:"account_id"`
	Holder           string    `json:"holder"`
	BaseCcy          Currency  `json:"base_ccy"`
	CreditLimitMinor int64     `json:"credit_limit_minor"` // 授信额度（base 币种最小单位）
	OpenedAt         time.Time `json:"opened_at"`
}

// CardEnrolledEvent 卡片登记。落盘时只有脱敏卡号与授权范围。
type CardEnrolledEvent struct {
	CardID     string    `json:"card_id"`
	AccountID  string    `json:"account_id"`
	MaskedPAN  string    `json:"masked_pan"`
	AuthScope  AuthScope `json:"auth_scope"` // 持卡人授权范围
	FeeBps     int64     `json:"fee_bps"`    // 外币兑换手续费（基点）
	EnrolledAt time.Time `json:"enrolled_at"`
}

// AuthScope 持卡人对卡片使用范围的授权（授权范围内才放行交易）。
type AuthScope struct {
	AllowedMCCs   []string `json:"allowed_mccs,omitempty"` // 空表示不限
	BlockedMCCs   []string `json:"blocked_mccs,omitempty"`
	Countries     []string `json:"countries,omitempty"`   // ISO 国家码白名单，空表示不限
	SingleTxLimit int64    `json:"single_tx_limit_minor"` // 单笔上限（卡片币种最小单位），0 表示不限
	DailyLimit    int64    `json:"daily_limit_minor"`     // 日累计上限，0 表示不限
	OnlineOnly    bool     `json:"online_only,omitempty"` // true 时拒绝离线补传
}

// AuthorizedEvent 刷卡 / 预授权批准并冻结余额。
type AuthorizedEvent struct {
	TxnID        string  `json:"txn_id"`
	CardID       string  `json:"card_id"`
	AccountID    string  `json:"account_id"`
	Kind         TxnKind `json:"kind"` // purchase | preauth | force_post
	MCC          string  `json:"mcc"`
	Country      string  `json:"country"`
	MerchantName string  `json:"merchant_name"`
	// 幂等三元组：同卡 + 终端检索参考号 + 系统追踪号唯一。
	RRN        string        `json:"rrn"`
	STAN       string        `json:"stan"`
	BatchNo    string        `json:"batch_no,omitempty"`
	TxnCcy     Currency      `json:"txn_ccy"`
	TxnAmount  int64         `json:"txn_amount_minor"`
	BillCcy    Currency      `json:"bill_ccy"`
	BillAmount int64         `json:"bill_amount_minor"` // 冻结额（含手续费）
	FeeMinor   int64         `json:"fee_minor"`
	FeeBps     int64         `json:"fee_bps"`
	FX         FXSnapshotRef `json:"fx"`
	// ExpiresAt 预授权/授权的过期时间；过期未清算自动释放（离线补传可在过期后到达）。
	ExpiresAt  time.Time `json:"expires_at"`
	OccurredAt time.Time `json:"occurred_at"` // 终端交易时间（可能跨午夜）
	PostedAt   time.Time `json:"posted_at"`   // 系统记账时间
}

// AuthDeclinedEvent 拒绝（余额不足/超出授权范围等），只做最小审计记录。
type AuthDeclinedEvent struct {
	TxnID      string    `json:"txn_id"`
	CardID     string    `json:"card_id"`
	RRN        string    `json:"rrn"`
	STAN       string    `json:"stan"`
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurred_at"`
	PostedAt   time.Time `json:"posted_at"`
}

// ReversedEvent 撤销。Amount 为撤销的交易币种金额，可部分撤销；
// 清算前撤销按比例释放冻结，清算后撤销按比例退还账单金额。
type ReversedEvent struct {
	TxnID           string        `json:"txn_id"`
	ReversalRRN     string        `json:"reversal_rrn"`
	AmountMinor     int64         `json:"amount_minor"`
	BillRefundMinor int64         `json:"bill_refund_minor"` // 清算后撤销时的退款额
	FeeRefundMinor  int64         `json:"fee_refund_minor"`
	FX              FXSnapshotRef `json:"fx"`
	AfterCleared    bool          `json:"after_cleared"`
	PostedAt        time.Time     `json:"posted_at"`
}

// ClearedEvent 在线请款 / 离线补传的最终清算。
// FinalBillMinor 是本次清算实际入账的账单金额（含最终手续费），
// 与冻结额之间的差额自动补扣或释放；Final=false 表示部分请款（剩余冻结释放）。
type ClearedEvent struct {
	TxnID           string        `json:"txn_id"`
	RRN             string        `json:"rrn"`
	STAN            string        `json:"stan"`
	BatchNo         string        `json:"batch_no,omitempty"`
	FinalTxnAmount  int64         `json:"final_txn_amount_minor"`
	FinalBillAmount int64         `json:"final_bill_amount_minor"`
	FinalFeeMinor   int64         `json:"final_fee_minor"`
	PriorHeldMinor  int64         `json:"prior_held_minor"` // 清算前冻结
	DeltaMinor      int64         `json:"delta_minor"`      // 相对冻结的补扣(+)/释放(-)
	Offline         bool          `json:"offline"`          // 离线终端补传
	Partial         bool          `json:"partial"`
	FX              FXSnapshotRef `json:"fx"`
	OccurredAt      time.Time     `json:"occurred_at"`
	PostedAt        time.Time     `json:"posted_at"`
}

// ForcePostedEvent 无在先授权的离线补传（脱机终端直接请款）。
type ForcePostedEvent struct {
	TxnID        string        `json:"txn_id"`
	CardID       string        `json:"card_id"`
	AccountID    string        `json:"account_id"`
	MCC          string        `json:"mcc"`
	Country      string        `json:"country"`
	MerchantName string        `json:"merchant_name"`
	RRN          string        `json:"rrn"`
	STAN         string        `json:"stan"`
	BatchNo      string        `json:"batch_no,omitempty"`
	TxnCcy       Currency      `json:"txn_ccy"`
	TxnAmount    int64         `json:"txn_amount_minor"`
	BillCcy      Currency      `json:"bill_ccy"`
	BillAmount   int64         `json:"bill_amount_minor"`
	FeeMinor     int64         `json:"fee_minor"`
	FeeBps       int64         `json:"fee_bps"`
	FX           FXSnapshotRef `json:"fx"`
	OccurredAt   time.Time     `json:"occurred_at"`
	PostedAt     time.Time     `json:"posted_at"`
}

// FXRateReceivedEvent 汇率快照送达。
type FXRateReceivedEvent struct {
	SnapshotID  string    `json:"snapshot_id"`
	Pair        CcyPair   `json:"pair"`
	RateMicro   int64     `json:"rate_micro"`
	AsOf        time.Time `json:"as_of"`
	Source      string    `json:"source"`
	Quarantined bool      `json:"quarantined"`
	Reason      string    `json:"reason,omitempty"`
	ReceivedAt  time.Time `json:"received_at"`
}

// FXRateQuarantinedEvent 异常汇率隔离（与 FXRateReceived 成对出现，便于审计）。
type FXRateQuarantinedEvent struct {
	SnapshotID     string    `json:"snapshot_id"`
	Pair           CcyPair   `json:"pair"`
	RateMicro      int64     `json:"rate_micro"`
	ReferenceMicro int64     `json:"reference_micro"`
	Reason         string    `json:"reason"`
	At             time.Time `json:"at"`
}

// FXRateApprovedEvent 人工复核通过，隔离汇率转为生效。
type FXRateApprovedEvent struct {
	SnapshotID string    `json:"snapshot_id"`
	Reviewer   string    `json:"reviewer"`
	At         time.Time `json:"at"`
}

// FXRateRejectedEvent 人工复核驳回，隔离汇率废弃。
type FXRateRejectedEvent struct {
	SnapshotID string    `json:"snapshot_id"`
	Reviewer   string    `json:"reviewer"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// AuthExpiredEvent 授权到期未清算，冻结自动释放；离线补传之后仍可按 RRN 命中并补入账。
type AuthExpiredEvent struct {
	TxnID         string    `json:"txn_id"`
	ReleasedMinor int64     `json:"released_minor"`
	At            time.Time `json:"at"`
}

// DuplicateRejectedEvent 重复请款被拦截（商户重试/重复批次/跨午夜补送）。
type DuplicateRejectedEvent struct {
	CardID  string    `json:"card_id"`
	RRN     string    `json:"rrn"`
	STAN    string    `json:"stan"`
	BatchNo string    `json:"batch_no,omitempty"`
	TxnID   string    `json:"txn_id,omitempty"` // 命中的原始交易（如有）
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// DisputeOpenedEvent 客户发起争议。
type DisputeOpenedEvent struct {
	DisputeID  string    `json:"dispute_id"`
	AccountID  string    `json:"account_id"`
	TxnID      string    `json:"txn_id"`
	ReasonCode string    `json:"reason_code"` // duplicate / unauthorized / amount_diff / fx_dispute ...
	Narrative  string    `json:"narrative"`
	ClaimMinor int64     `json:"claim_minor"` // 客户主张金额（账单币种）
	OpenedAt   time.Time `json:"opened_at"`
}

// DisputeNotedEvent 客服追加调查过程备注（不改变争议状态、不产生资金动作）。
type DisputeNotedEvent struct {
	DisputeID string    `json:"dispute_id"`
	AgentID   string    `json:"agent_id"`
	Note      string    `json:"note"`
	At        time.Time `json:"at"`
}

// DisputeConcludedEvent 客服追加调查结论（争议本身不可被改写）。
type DisputeConcludedEvent struct {
	DisputeID   string    `json:"dispute_id"`
	Outcome     string    `json:"outcome"` // won | lost | partial | withdrawn
	RefundMinor int64     `json:"refund_minor"`
	Conclusion  string    `json:"conclusion"`
	AgentID     string    `json:"agent_id"`
	ConcludedAt time.Time `json:"concluded_at"`
}

// NotificationEvent 通知（短信/推送/邮件）。事件落盘后才视为“已发送”，
// 重启重放不会重发：Applier 遇到该事件只计数。
type NotificationSentEvent struct {
	NotificationID string    `json:"notification_id"`
	AccountID      string    `json:"account_id"`
	TxnID          string    `json:"txn_id,omitempty"`
	DisputeID      string    `json:"dispute_id,omitempty"`
	Channel        string    `json:"channel"` // sms | push | email
	Template       string    `json:"template"`
	SentAt         time.Time `json:"sent_at"`
}

// Envelope 是事件流里的一条记录。
type Envelope struct {
	Seq       int64           `json:"seq"`
	AccountID string          `json:"account_id"` // 全局事件也可能为空
	Type      string          `json:"type"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

func kindOf(t string) any {
	switch t {
	case EvAccountOpened:
		return &AccountOpenedEvent{}
	case EvCardEnrolled:
		return &CardEnrolledEvent{}
	case EvAuthorized:
		return &AuthorizedEvent{}
	case EvAuthDeclined:
		return &AuthDeclinedEvent{}
	case EvReversed:
		return &ReversedEvent{}
	case EvCleared:
		return &ClearedEvent{}
	case EvForcePosted:
		return &ForcePostedEvent{}
	case EvFXRateReceived:
		return &FXRateReceivedEvent{}
	case EvFXRateQuarantined:
		return &FXRateQuarantinedEvent{}
	case EvFXRateApproved:
		return &FXRateApprovedEvent{}
	case EvFXRateRejected:
		return &FXRateRejectedEvent{}
	case EvAuthExpired:
		return &AuthExpiredEvent{}
	case EvDuplicateRejected:
		return &DuplicateRejectedEvent{}
	case EvDisputeOpened:
		return &DisputeOpenedEvent{}
	case EvDisputeNoted:
		return &DisputeNotedEvent{}
	case EvDisputeConcluded:
		return &DisputeConcludedEvent{}
	case EvNotificationSent:
		return &NotificationSentEvent{}
	default:
		return nil
	}
}
