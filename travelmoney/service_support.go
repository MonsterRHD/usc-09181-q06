package travelmoney

import (
	"encoding/json"
	"sync"
	"time"
)

// append 落盘一条事件并立即物化；通知在事件落盘后由 applier 触发一次。
func (s *Service) append(accountID, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return wrapErr("encode_error", err)
	}
	env := Envelope{
		Seq: s.state.Seq + 1, AccountID: accountID, Type: typ,
		At: s.nowTime(), Payload: raw,
	}
	if err := s.store.Append(env, payload); err != nil {
		return wrapErr("store_error", err)
	}
	s.state.apply(env, true, s.nf)
	return nil
}

// notify 只做“登记一条已发通知事件”，真正发送由 applier 对新事件触发。
func (s *Service) notify(accountID, txnID, disputeID, channel, template string) {
	e := NotificationSentEvent{
		NotificationID: "ntf_" + randID(),
		AccountID:      accountID,
		TxnID:          txnID,
		DisputeID:      disputeID,
		Channel:        channel,
		Template:       template,
		SentAt:         s.nowTime(),
	}
	_ = s.append(accountID, EvNotificationSent, e)
}

// ---------------------------------------------------------------------------
// 争议：客户可发起；客服只能追加备注/结论；结论单向不可改写
// ---------------------------------------------------------------------------

// OpenDispute 客户就一笔已入账交易发起争议（客服只能追加结论，不能代发起）。
func (s *Service) OpenDispute(actor Actor, r DisputeReq) error {
	if actor.Role != RoleCustomer || actor.AccountID != r.AccountID {
		return wrapErr("forbidden", ErrForbidden)
	}
	a := s.state.Accounts[r.AccountID]
	if a == nil {
		return wrapErr("not_found", ErrNotFound)
	}
	t := a.Txns[r.TxnID]
	if t == nil || !t.Cleared {
		return serr("invalid_argument", "dispute requires a cleared transaction")
	}
	if r.DisputeID == "" {
		r.DisputeID = "dsp_" + randID()
	}
	if _, exists := a.Disputes[r.DisputeID]; exists {
		return serr("already_exists", "dispute %s exists", r.DisputeID)
	}
	if r.ClaimMinor <= 0 {
		return serr("invalid_argument", "claim amount must be positive")
	}
	if maxClaim := t.NetBilled(); r.ClaimMinor > maxClaim {
		return serr("invalid_argument", "claim %d exceeds net billed %d", r.ClaimMinor, maxClaim)
	}
	e := DisputeOpenedEvent{
		DisputeID: r.DisputeID, AccountID: r.AccountID, TxnID: r.TxnID,
		ReasonCode: r.ReasonCode, Narrative: r.Narrative, ClaimMinor: r.ClaimMinor,
		OpenedAt: s.nowTime(),
	}
	if err := s.append(r.AccountID, EvDisputeOpened, e); err != nil {
		return err
	}
	s.notify(r.AccountID, r.TxnID, r.DisputeID, "push", "dispute_opened")
	return nil
}

// AddDisputeNote 客服追加调查备注（客户角色无权）。
func (s *Service) AddDisputeNote(actor Actor, disputeID, note string) error {
	if !actor.IsStaff() {
		return wrapErr("forbidden", ErrForbidden)
	}
	d := s.findDispute(disputeID)
	if d == nil {
		return wrapErr("not_found", ErrNotFound)
	}
	e := DisputeNotedEvent{
		DisputeID: disputeID, AgentID: actor.StaffID(), Note: note, At: s.nowTime(),
	}
	return s.append(d.AccountID, EvDisputeNoted, e)
}

// ConcludeDispute 客服追加最终调查结论（一次性，之后争议只读）。
func (s *Service) ConcludeDispute(actor Actor, disputeID, outcome string, refundMinor int64, conclusion string) error {
	if !actor.IsStaff() {
		return wrapErr("forbidden", ErrForbidden)
	}
	d := s.findDispute(disputeID)
	if d == nil {
		return wrapErr("not_found", ErrNotFound)
	}
	if d.Status == DisputeConcluded {
		return serr("conflict", "dispute %s already concluded", disputeID)
	}
	switch outcome {
	case "won", "lost", "partial", "withdrawn":
	default:
		return serr("invalid_argument", "unknown outcome %q", outcome)
	}
	t := s.state.Txns[d.TxnID]
	if refundMinor < 0 || refundMinor > d.ClaimMinor || refundMinor > t.NetBilled() {
		return serr("invalid_argument", "refund out of allowed range")
	}
	e := DisputeConcludedEvent{
		DisputeID: disputeID, Outcome: outcome, RefundMinor: refundMinor,
		Conclusion: conclusion, AgentID: actor.StaffID(), ConcludedAt: s.nowTime(),
	}
	if err := s.append(d.AccountID, EvDisputeConcluded, e); err != nil {
		return err
	}
	if refundMinor > 0 {
		s.notify(d.AccountID, d.TxnID, disputeID, "push", "dispute_refund")
	}
	return nil
}

func (s *Service) findDispute(id string) *Dispute {
	for _, a := range s.state.Accounts {
		if d := a.Disputes[id]; d != nil {
			return d
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// RBAC
// ---------------------------------------------------------------------------

// Role 访问角色。
type Role string

const (
	RoleCustomer Role = "customer" // 只能访问本人账户
	RoleAgent    Role = "agent"    // 客服：查看、追加调查，不能发起争议或改账
	RoleAdmin    Role = "admin"    // 合规管理员：可导出、复核汇率
)

// Actor 调用方身份。
type Actor struct {
	Role      Role
	AccountID string // 客户角色的归属账户
	StaffIDv  string // 客服/管理员工号
}

// NewCustomerActor 构造客户身份。
func NewCustomerActor(accountID string) Actor { return Actor{Role: RoleCustomer, AccountID: accountID} }

// NewStaffActor 构造客服身份。
func NewStaffActor(id string, admin bool) Actor {
	r := RoleAgent
	if admin {
		r = RoleAdmin
	}
	return Actor{Role: r, StaffIDv: id}
}

func (a Actor) StaffID() string { return a.StaffIDv }

func (a Actor) IsStaff() bool { return a.Role == RoleAgent || a.Role == RoleAdmin }

// CanAccessAccount 客户只能访问本人账户；员工可访问任意账户。
func (a Actor) CanAccessAccount(accountID string) bool {
	if a.IsStaff() {
		return true
	}
	return a.Role == RoleCustomer && a.AccountID == accountID
}

// ---------------------------------------------------------------------------
// 查询视图（脱敏明细 + 固定换算依据）
// ---------------------------------------------------------------------------

// ConversionView 一笔金额展示所钉住的换算依据。
type ConversionView struct {
	TxnCcy      Currency `json:"txn_ccy"`
	TxnAmount   int64    `json:"txn_amount_minor"`
	BillCcy     Currency `json:"bill_ccy"`
	BillAmount  int64    `json:"bill_amount_minor"`
	FeeMinor    int64    `json:"fee_minor,omitempty"`
	RateText    string   `json:"rate"` // 展示汇率（快照固定值）
	RateMicro   int64    `json:"rate_micro"`
	SnapshotID  string   `json:"fx_snapshot_id"` // 对应汇率快照
	FXAsOf      string   `json:"fx_as_of"`       // 汇率时点
	FXSource    string   `json:"fx_source"`
	PendingRate bool     `json:"pending_rate,omitempty"` // true=送达时无有效汇率，按临时口径占额
}

func conversionView(fx FXSnapshotRef, txnAmt, billAmt, fee int64) ConversionView {
	return ConversionView{
		TxnCcy: fx.Pair.Base, TxnAmount: txnAmt,
		BillCcy: fx.Pair.Quote, BillAmount: billAmt, FeeMinor: fee,
		RateText: fx.RateText, RateMicro: fx.RateMicro,
		SnapshotID: fx.SnapshotID, FXAsOf: fx.AsOf.Format(time.RFC3339), FXSource: fx.Source,
		PendingRate: fx.Source == "pending",
	}
}

// TxnView 客户可见的脱敏交易明细。
// Auth 段固定授权/预授权当时的换算口径；Final 段固定清算时的换算口径，
// 两者分开解释“预授权—汇率—最终清算”的关系。
type TxnView struct {
	TxnID         string          `json:"txn_id"`
	MaskedPAN     string          `json:"masked_pan"`
	Kind          TxnKind         `json:"kind"`
	Status        TxnStatus       `json:"status"`
	MCC           string          `json:"mcc,omitempty"`
	Country       string          `json:"country,omitempty"`
	MerchantName  string          `json:"merchant"`
	STAN          string          `json:"stan,omitempty"`
	BatchNo       string          `json:"batch_no,omitempty"`
	Auth          *ConversionView `json:"authorization,omitempty"`
	Final         *ConversionView `json:"final_clearing,omitempty"`
	HeldMinor     int64           `json:"held_minor"`
	RefundedMinor int64           `json:"refunded_minor"`
	DisputeRefund int64           `json:"dispute_refund_minor"`
	NetBilled     int64           `json:"net_billed_minor"`
	Offline       bool            `json:"offline"`
	OccurredAt    string          `json:"occurred_at"`
	PostedAt      string          `json:"posted_at"`
}

// AccountView 账户余额视图。
type AccountView struct {
	AccountID        string   `json:"account_id"`
	BaseCcy          Currency `json:"base_ccy"`
	CreditLimitMinor int64    `json:"credit_limit_minor"`
	HeldMinor        int64    `json:"held_minor"`      // 在冻（预授权/未清算授权）
	BalanceMinor     int64    `json:"balance_minor"`   // 已出账应还
	AvailableMinor   int64    `json:"available_minor"` // 持卡人授权范围内的可用余额
	Notifications    int      `json:"notification_count"`
}

// Snapshot 返回账户余额。
func (s *Service) Snapshot(actor Actor, accountID string) (AccountView, error) {
	if !actor.CanAccessAccount(accountID) {
		return AccountView{}, wrapErr("forbidden", ErrForbidden)
	}
	a := s.state.Accounts[accountID]
	if a == nil {
		return AccountView{}, wrapErr("not_found", ErrNotFound)
	}
	return AccountView{
		AccountID: a.ID, BaseCcy: a.BaseCcy, CreditLimitMinor: a.CreditLimit,
		HeldMinor: a.Held, BalanceMinor: a.Balance, AvailableMinor: a.Available(),
		Notifications: a.NotifCount,
	}, nil
}

// ListTransactions 返回脱敏交易明细（客户仅本人账户）。
func (s *Service) ListTransactions(actor Actor, accountID string) ([]TxnView, error) {
	if !actor.CanAccessAccount(accountID) {
		return nil, wrapErr("forbidden", ErrForbidden)
	}
	a := s.state.Accounts[accountID]
	if a == nil {
		return nil, wrapErr("not_found", ErrNotFound)
	}
	out := make([]TxnView, 0, len(a.Txns))
	for _, t := range a.Txns {
		out = append(out, s.txnView(t))
	}
	return out, nil
}

// GetTransaction 返回单笔脱敏明细。
func (s *Service) GetTransaction(actor Actor, accountID, txnID string) (TxnView, error) {
	if !actor.CanAccessAccount(accountID) {
		return TxnView{}, wrapErr("forbidden", ErrForbidden)
	}
	a := s.state.Accounts[accountID]
	if a == nil {
		return TxnView{}, wrapErr("not_found", ErrNotFound)
	}
	t := a.Txns[txnID]
	if t == nil {
		return TxnView{}, wrapErr("not_found", ErrNotFound)
	}
	return s.txnView(t), nil
}

func (s *Service) txnView(t *Txn) TxnView {
	v := TxnView{
		TxnID: t.ID, Kind: t.Kind, Status: t.Status,
		MCC: t.MCC, Country: t.Country, MerchantName: t.MerchantName,
		STAN: t.STAN, BatchNo: t.BatchNo,
		HeldMinor:     t.HeldBill,
		RefundedMinor: t.RefundedBill + t.FeeRefunded,
		DisputeRefund: t.DisputeRefund,
		Offline:       t.OfflineClear,
		OccurredAt:    t.OccurredAt.Format(time.RFC3339),
		PostedAt:      t.PostedAt.Format(time.RFC3339),
	}
	if c := s.state.Cards[t.CardID]; c != nil {
		v.MaskedPAN = c.MaskedPAN
	}
	authAmt := t.TxnAmount
	if t.Kind != KindForcePost {
		cv := conversionView(t.AuthFX, authAmt, t.AuthBill, t.AuthFee)
		v.Auth = &cv
	}
	if t.Cleared {
		cv := conversionView(t.ClearFX, t.FinalTxn, t.FinalBill, t.FinalFee)
		v.Final = &cv
		v.NetBilled = t.NetBilled()
	}
	return v
}

// NetBilled 客户实际承担的净额 = 最终入账 - 撤销退款 - 争议退款。
func (t *Txn) NetBilled() int64 {
	return t.FinalBill - t.RefundedBill - t.FeeRefunded - t.DisputeRefund
}

// DisputeView 争议视图：结论/备注均为追加序列，历史不可改。
type DisputeView struct {
	DisputeID   string                  `json:"dispute_id"`
	TxnID       string                  `json:"txn_id"`
	ReasonCode  string                  `json:"reason_code"`
	Narrative   string                  `json:"narrative"`
	ClaimMinor  int64                   `json:"claim_minor"`
	Status      DisputeStatus           `json:"status"`
	OpenedAt    string                  `json:"opened_at"`
	Notes       []DisputeNotedEvent     `json:"notes,omitempty"`
	Conclusions []DisputeConcludedEvent `json:"conclusions,omitempty"`
}

// ListDisputes 列出争议（客户仅本人账户）。
func (s *Service) ListDisputes(actor Actor, accountID string) ([]DisputeView, error) {
	if !actor.CanAccessAccount(accountID) {
		return nil, wrapErr("forbidden", ErrForbidden)
	}
	a := s.state.Accounts[accountID]
	if a == nil {
		return nil, wrapErr("not_found", ErrNotFound)
	}
	out := make([]DisputeView, 0, len(a.Disputes))
	for _, d := range a.Disputes {
		out = append(out, DisputeView{
			DisputeID: d.ID, TxnID: d.TxnID, ReasonCode: d.ReasonCode,
			Narrative: d.Narrative, ClaimMinor: d.ClaimMinor, Status: d.Status,
			OpenedAt: d.OpenedAt.Format(time.RFC3339),
			Notes:    d.Notes, Conclusions: d.Conclusions,
		})
	}
	return out, nil
}

// QuarantineView 待人工复核汇率。
type QuarantineView struct {
	SnapshotID     string  `json:"snapshot_id"`
	Pair           CcyPair `json:"pair"`
	RateMicro      int64   `json:"rate_micro"`
	RateText       string  `json:"rate"`
	ReferenceMicro int64   `json:"reference_micro"`
	Reason         string  `json:"reason"`
	AsOf           string  `json:"as_of"`
}

// ListQuarantine 员工查看待复核异常汇率。
func (s *Service) ListQuarantine(actor Actor) ([]QuarantineView, error) {
	if !actor.IsStaff() {
		return nil, wrapErr("forbidden", ErrForbidden)
	}
	var out []QuarantineView
	for pair, fx := range s.state.FX {
		for _, q := range fx.quarantine {
			out = append(out, QuarantineView{
				SnapshotID: q.Snapshot.SnapshotID, Pair: pair,
				RateMicro: q.Snapshot.RateMicro, RateText: FormatRate(q.Snapshot.RateMicro),
				ReferenceMicro: q.RefMicro, Reason: q.Snapshot.Reason,
				AsOf: q.Snapshot.AsOf.Format(time.RFC3339),
			})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 按账户导出（监管最小记录）
// ---------------------------------------------------------------------------

// ExportAccount 导出某一账户的事件记录（JSON Lines 字节）。
// 事件天然只含最小字段：卡号脱敏、拒绝记录仅审计字段、汇率依据随笔固定，
// 因此导出内容即“满足监管的最小记录”，不含任何系统内部状态。
func (s *Service) ExportAccount(actor Actor, accountID string) ([][]byte, error) {
	if !actor.IsStaff() && !actor.CanAccessAccount(accountID) {
		return nil, wrapErr("forbidden", ErrForbidden)
	}
	if a := s.state.Accounts[accountID]; a == nil {
		return nil, wrapErr("not_found", ErrNotFound)
	}
	var out [][]byte
	err := s.store.Replay(func(env Envelope) error {
		if env.AccountID != accountID {
			return nil
		}
		raw, err := json.Marshal(env)
		if err != nil {
			return err
		}
		out = append(out, raw)
		return nil
	})
	return out, err
}

// MemoryStore 内存事件存储（测试用）。
type MemoryStore struct {
	mu     sync.Mutex
	events []Envelope
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Append(env Envelope, _ any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, env)
	return nil
}

func (m *MemoryStore) Replay(handle func(Envelope) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, env := range m.events {
		if err := handle(env); err != nil {
			return err
		}
	}
	return nil
}

// CountTypes 返回各类型事件数量（测试/诊断用）。
func (m *MemoryStore) CountTypes() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, e := range m.events {
		out[e.Type]++
	}
	return out
}

// RecordingNotifier 记录实际发送的通知（重启后重放不会新增）。
type RecordingNotifier struct {
	mu   sync.Mutex
	Sent []NotificationSentEvent
}

func (n *RecordingNotifier) Send(e NotificationSentEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Sent = append(n.Sent, e)
}

func (n *RecordingNotifier) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.Sent)
}

func (n *RecordingNotifier) Templates() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.Sent))
	for _, e := range n.Sent {
		out = append(out, e.Template)
	}
	return out
}
