package travelmoney

import (
	"encoding/json"
	"time"
)

// TxnKind 交易种类。
type TxnKind string

const (
	KindPurchase  TxnKind = "purchase" // 普通刷卡
	KindPreauth   TxnKind = "preauth"  // 预授权（酒店/租车）
	KindForcePost TxnKind = "force_post"
)

// TxnStatus 交易状态。
type TxnStatus string

const (
	StatusPending     TxnStatus = "pending"      // 已授权、冻结中
	StatusCleared     TxnStatus = "cleared"      // 已清算
	StatusForcePosted TxnStatus = "force_posted" // 无授权离线补传，直接入账
	StatusReversed    TxnStatus = "reversed"     // 清算前全额撤销
	StatusExpired     TxnStatus = "expired"      // 授权过期自动释放（仍可被离线补传命中）
)

// DisputeStatus 争议状态。
type DisputeStatus string

const (
	DisputeOpen      DisputeStatus = "open"
	DisputeConcluded DisputeStatus = "concluded"
)

// Txn 是一笔交易的物化状态（由事件重放得到，本身从不被直接改写）。
type Txn struct {
	ID            string        `json:"id"`
	CardID        string        `json:"card_id"`
	AccountID     string        `json:"account_id"`
	Kind          TxnKind       `json:"kind"`
	MCC           string        `json:"mcc"`
	Country       string        `json:"country"`
	MerchantName  string        `json:"merchant_name"`
	RRN           string        `json:"rrn"`
	STAN          string        `json:"stan"`
	BatchNo       string        `json:"batch_no"`
	TxnCcy        Currency      `json:"txn_ccy"`
	TxnAmount     int64         `json:"txn_amount_minor"` // 原始授权金额（交易币种）
	BillCcy       Currency      `json:"bill_ccy"`
	AuthBill      int64         `json:"auth_bill_minor"` // 授权时冻结额（账单币种，含费）
	AuthFee       int64         `json:"auth_fee_minor"`
	AuthFX        FXSnapshotRef `json:"auth_fx"`
	HeldBill      int64         `json:"held_bill_minor"` // 当前仍冻结的部分
	Status        TxnStatus     `json:"status"`
	RevTxnPre     int64         `json:"rev_txn_pre_minor"`  // 清算前累计撤销（交易币种）
	ReleasedPre   int64         `json:"released_pre_minor"` // 清算前累计释放（账单币种）
	FeeReleased   int64         `json:"fee_released_minor"`
	Cleared       bool          `json:"cleared"`
	OfflineClear  bool          `json:"offline_clear"`
	FinalTxn      int64         `json:"final_txn_amount_minor"`
	FinalBill     int64         `json:"final_bill_amount_minor"`
	FinalFee      int64         `json:"final_fee_minor"`
	ClearFX       FXSnapshotRef `json:"clear_fx"`
	PostRevTxn    int64         `json:"post_rev_txn_minor"`  // 清算后累计撤销（交易币种）
	RefundedBill  int64         `json:"refunded_bill_minor"` // 撤销退款（账单币种）
	FeeRefunded   int64         `json:"fee_refunded_minor"`
	DisputeRefund int64         `json:"dispute_refund_minor"` // 争议胜诉退款
	OccurredAt    time.Time     `json:"occurred_at"`
	PostedAt      time.Time     `json:"posted_at"`
	ExpiresAt     time.Time     `json:"expires_at"`
}

// RemainingAuthAmount 清算前仍可被请款的交易币种金额。
func (t *Txn) RemainingAuthAmount() int64 { return t.TxnAmount - t.RevTxnPre }

// Dispute 争议（结论只能追加，状态机 open -> concluded 单向）。
type Dispute struct {
	ID          string                  `json:"id"`
	AccountID   string                  `json:"account_id"`
	TxnID       string                  `json:"txn_id"`
	ReasonCode  string                  `json:"reason_code"`
	Narrative   string                  `json:"narrative"`
	ClaimMinor  int64                   `json:"claim_minor"`
	Status      DisputeStatus           `json:"status"`
	OpenedAt    time.Time               `json:"opened_at"`
	Notes       []DisputeNotedEvent     `json:"notes,omitempty"`
	Conclusions []DisputeConcludedEvent `json:"conclusions,omitempty"`
}

// Card 卡片（只存脱敏卡号）。
type Card struct {
	CardID    string    `json:"card_id"`
	AccountID string    `json:"account_id"`
	MaskedPAN string    `json:"masked_pan"`
	Scope     AuthScope `json:"scope"`
	FeeBps    int64     `json:"fee_bps"`
}

// Account 账户物化状态。
type Account struct {
	ID          string              `json:"id"`
	Holder      string              `json:"holder"`
	BaseCcy     Currency            `json:"base_ccy"`
	CreditLimit int64               `json:"credit_limit_minor"`
	Held        int64               `json:"held_minor"`    // 在冻金额合计
	Balance     int64               `json:"balance_minor"` // 已出账应还金额（退款为减项）
	Cards       map[string]*Card    `json:"-"`
	Txns        map[string]*Txn     `json:"-"`
	Disputes    map[string]*Dispute `json:"-"`
	NotifCount  int                 `json:"notification_count"`
	DailySpent  map[string]int64    `json:"-"` // UTC 日期 -> 当日已占用累计（账单币种）
}

// Available 可用余额 = 授信额度 - 应还 - 在冻。
func (a *Account) Available() int64 { return a.CreditLimit - a.Balance - a.Held }

// QuarantineItem 待人工复核的异常汇率。
type QuarantineItem struct {
	Snapshot FXRateReceivedEvent `json:"snapshot"`
	RefMicro int64               `json:"reference_micro"`
}

type fxState struct {
	active     *FXSnapshotRef
	quarantine map[string]*QuarantineItem
}

// State 是整个事件流重放后的物化视图。
type State struct {
	Seq      int64
	Accounts map[string]*Account
	Cards    map[string]*Card  // cardID -> Card（全局索引）
	TxnByKey map[string]string // 幂等键 cardID|RRN -> txnID
	RevByKey map[string]bool   // cardID|reversalRRN 去重
	Txns     map[string]*Txn
	FX       map[CcyPair]*fxState
	NotifSeq map[string]int // accountID -> 已发通知数
}

func newState() *State {
	return &State{
		Accounts: map[string]*Account{},
		Cards:    map[string]*Card{},
		TxnByKey: map[string]string{},
		RevByKey: map[string]bool{},
		Txns:     map[string]*Txn{},
		FX:       map[CcyPair]*fxState{},
		NotifSeq: map[string]int{},
	}
}

func txnKey(cardID, rrn string) string { return cardID + "|" + rrn }

func (s *State) fxFor(p CcyPair) *fxState {
	fx := s.FX[p]
	if fx == nil {
		fx = &fxState{quarantine: map[string]*QuarantineItem{}}
		s.FX[p] = fx
	}
	return fx
}

// ActiveFX 返回某货币对当前生效的汇率快照；没有则 nil。
func (s *State) ActiveFX(p CcyPair) *FXSnapshotRef {
	if fx := s.FX[p]; fx != nil {
		return fx.active
	}
	return nil
}

// apply 把一条事件应用到物化视图。live=false 表示重放恢复，
// 此时只重建计数、不触发任何外部通知。
func (s *State) apply(env Envelope, live bool, nf Notifier) {
	s.Seq = env.Seq
	switch env.Type {
	case EvAccountOpened:
		var e AccountOpenedEvent
		decode(env.Payload, &e)
		s.Accounts[e.AccountID] = &Account{
			ID: e.AccountID, Holder: e.Holder, BaseCcy: e.BaseCcy,
			CreditLimit: e.CreditLimitMinor,
			Cards:       map[string]*Card{}, Txns: map[string]*Txn{},
			Disputes: map[string]*Dispute{}, DailySpent: map[string]int64{},
		}

	case EvCardEnrolled:
		var e CardEnrolledEvent
		decode(env.Payload, &e)
		c := &Card{CardID: e.CardID, AccountID: e.AccountID, MaskedPAN: e.MaskedPAN, Scope: e.AuthScope, FeeBps: e.FeeBps}
		if a := s.Accounts[e.AccountID]; a != nil {
			a.Cards[e.CardID] = c
		}
		s.Cards[e.CardID] = c

	case EvAuthorized:
		var e AuthorizedEvent
		decode(env.Payload, &e)
		t := &Txn{
			ID: e.TxnID, CardID: e.CardID, AccountID: e.AccountID, Kind: e.Kind,
			MCC: e.MCC, Country: e.Country, MerchantName: e.MerchantName,
			RRN: e.RRN, STAN: e.STAN, BatchNo: e.BatchNo,
			TxnCcy: e.TxnCcy, TxnAmount: e.TxnAmount,
			BillCcy: e.BillCcy, AuthBill: e.BillAmount, AuthFee: e.FeeMinor,
			AuthFX: e.FX, HeldBill: e.BillAmount, Status: StatusPending,
			OccurredAt: e.OccurredAt, PostedAt: e.PostedAt, ExpiresAt: e.ExpiresAt,
		}
		a := s.Accounts[e.AccountID]
		a.Held += e.BillAmount
		a.Txns[e.TxnID] = t
		s.Txns[e.TxnID] = t
		s.TxnByKey[txnKey(e.CardID, e.RRN)] = e.TxnID
		a.DailySpent[dayKey(e.OccurredAt)] += e.BillAmount

	case EvAuthDeclined:
		// 拒绝只保留最小审计字段（在事件载荷里），不建交易、不占额。

	case EvReversed:
		var e ReversedEvent
		decode(env.Payload, &e)
		t := s.Txns[e.TxnID]
		if t == nil {
			return
		}
		a := s.Accounts[t.AccountID]
		s.RevByKey[txnKey(t.CardID, e.ReversalRRN)] = true
		if e.AfterCleared {
			t.PostRevTxn += e.AmountMinor
			t.RefundedBill += e.BillRefundMinor
			t.FeeRefunded += e.FeeRefundMinor
			a.Balance -= e.BillRefundMinor
		} else {
			t.RevTxnPre += e.AmountMinor
			t.ReleasedPre += e.BillRefundMinor
			t.FeeReleased += e.FeeRefundMinor
			t.HeldBill -= e.BillRefundMinor
			a.Held -= e.BillRefundMinor
			if t.HeldBill == 0 && t.RevTxnPre >= t.TxnAmount {
				t.Status = StatusReversed
			}
		}

	case EvCleared:
		var e ClearedEvent
		decode(env.Payload, &e)
		t := s.Txns[e.TxnID]
		if t == nil || t.Cleared {
			return
		}
		a := s.Accounts[t.AccountID]
		a.Held -= e.PriorHeldMinor
		t.HeldBill = 0
		a.Balance += e.FinalBillAmount
		t.Cleared = true
		t.Status = StatusCleared
		t.OfflineClear = e.Offline
		t.FinalTxn = e.FinalTxnAmount
		t.FinalBill = e.FinalBillAmount
		t.FinalFee = e.FinalFeeMinor
		t.ClearFX = e.FX

	case EvForcePosted:
		var e ForcePostedEvent
		decode(env.Payload, &e)
		t := &Txn{
			ID: e.TxnID, CardID: e.CardID, AccountID: e.AccountID,
			Kind: KindForcePost, MCC: e.MCC, Country: e.Country, MerchantName: e.MerchantName,
			RRN: e.RRN, STAN: e.STAN, BatchNo: e.BatchNo,
			TxnCcy: e.TxnCcy, TxnAmount: e.TxnAmount,
			BillCcy: e.BillCcy, FinalBill: e.BillAmount, FinalFee: e.FeeMinor,
			AuthFX: e.FX, ClearFX: e.FX, Status: StatusForcePosted, Cleared: true,
			OfflineClear: true, FinalTxn: e.TxnAmount,
			OccurredAt: e.OccurredAt, PostedAt: e.PostedAt,
		}
		a := s.Accounts[e.AccountID]
		a.Balance += e.BillAmount
		a.DailySpent[dayKey(e.OccurredAt)] += e.BillAmount
		a.Txns[e.TxnID] = t
		s.Txns[e.TxnID] = t
		s.TxnByKey[txnKey(e.CardID, e.RRN)] = e.TxnID

	case EvFXRateReceived:
		var e FXRateReceivedEvent
		decode(env.Payload, &e)
		fx := s.fxFor(e.Pair)
		if e.Quarantined {
			fx.quarantine[e.SnapshotID] = &QuarantineItem{Snapshot: e}
			break
		}
		ref := snapshotRefOf(e)
		fx.active = &ref

	case EvFXRateQuarantined:
		var e FXRateQuarantinedEvent
		decode(env.Payload, &e)
		if q := s.FX[e.Pair]; q != nil {
			if item := q.quarantine[e.SnapshotID]; item != nil {
				item.RefMicro = e.ReferenceMicro
			}
		}

	case EvFXRateApproved:
		var e FXRateApprovedEvent
		decode(env.Payload, &e)
		for _, fx := range s.FX {
			if item := fx.quarantine[e.SnapshotID]; item != nil {
				r := snapshotRefOf(item.Snapshot)
				fx.active = &r
				delete(fx.quarantine, e.SnapshotID)
			}
		}

	case EvFXRateRejected:
		var e FXRateRejectedEvent
		decode(env.Payload, &e)
		for _, fx := range s.FX {
			delete(fx.quarantine, e.SnapshotID)
		}

	case EvAuthExpired:
		var e AuthExpiredEvent
		decode(env.Payload, &e)
		if t := s.Txns[e.TxnID]; t != nil && !t.Cleared && t.HeldBill > 0 {
			s.Accounts[t.AccountID].Held -= e.ReleasedMinor
			t.HeldBill = 0
			t.Status = StatusExpired
		}

	case EvDuplicateRejected:
		// 仅审计留痕，无资金动作。

	case EvDisputeOpened:
		var e DisputeOpenedEvent
		decode(env.Payload, &e)
		d := &Dispute{
			ID: e.DisputeID, AccountID: e.AccountID, TxnID: e.TxnID,
			ReasonCode: e.ReasonCode, Narrative: e.Narrative, ClaimMinor: e.ClaimMinor,
			Status: DisputeOpen, OpenedAt: e.OpenedAt,
		}
		s.Accounts[e.AccountID].Disputes[e.DisputeID] = d

	case EvDisputeNoted:
		var e DisputeNotedEvent
		decode(env.Payload, &e)
		for _, d := range findDisputes(s, e.DisputeID) {
			d.Notes = append(d.Notes, e)
		}

	case EvDisputeConcluded:
		var e DisputeConcludedEvent
		decode(env.Payload, &e)
		for _, d := range findDisputes(s, e.DisputeID) {
			d.Conclusions = append(d.Conclusions, e)
			d.Status = DisputeConcluded
			if e.RefundMinor > 0 {
				if t := s.Txns[d.TxnID]; t != nil {
					t.DisputeRefund += e.RefundMinor
					s.Accounts[t.AccountID].Balance -= e.RefundMinor
				}
			}
		}

	case EvNotificationSent:
		var e NotificationSentEvent
		decode(env.Payload, &e)
		if a := s.Accounts[e.AccountID]; a != nil {
			a.NotifCount++
		}
		s.NotifSeq[e.AccountID]++
		if live && nf != nil {
			nf.Send(e) // 仅对落盘后的新事件触发一次；重启重放不会重发
		}
	}
}

func findDisputes(s *State, id string) []*Dispute {
	for _, a := range s.Accounts {
		if d := a.Disputes[id]; d != nil {
			return []*Dispute{d}
		}
	}
	return nil
}

func snapshotRefOf(e FXRateReceivedEvent) FXSnapshotRef {
	return FXSnapshotRef{
		SnapshotID: e.SnapshotID, Pair: e.Pair, RateMicro: e.RateMicro,
		RateText: FormatRate(e.RateMicro), AsOf: e.AsOf, Source: e.Source,
	}
}

func decode(raw json.RawMessage, v any) {
	if err := json.Unmarshal(raw, v); err != nil {
		panic("travelmoney: corrupt event payload: " + err.Error())
	}
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }
