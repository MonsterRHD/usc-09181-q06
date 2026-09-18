package travelmoney

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Notifier 通知出口。实现需自身幂等：同一 NotificationID 只发一次。
type Notifier interface {
	Send(NotificationSentEvent)
}

// Store 只追加事件存储。
type Store interface {
	Append(env Envelope, payload any) error
	Replay(handle func(Envelope) error) error
}

// DeviationToleranceBps 新汇率相对当前有效汇率的允许偏离（5%）。
const DeviationToleranceBps int64 = 500

// MaxFXAge 汇率快照“新鲜度”上限；超过则视为过期，不能静默采用。
const MaxFXAge = 24 * time.Hour

// Config 服务可调参数。
type Config struct {
	Freshness    time.Time     // 用于“当前时刻”判定；零值用系统时钟
	AuthTTL      time.Duration // 授权/预授权冻结有效期
	DeviationBps int64
}

// Service 资金管家应用服务。
type Service struct {
	store Store
	state *State
	nf    Notifier
	cfg   Config
	now   func() time.Time
}

// NewService 打开服务并重放历史事件恢复状态（重启服务）。
func NewService(store Store, nf Notifier, cfg Config) (*Service, error) {
	if cfg.AuthTTL == 0 {
		cfg.AuthTTL = 7 * 24 * time.Hour
	}
	if cfg.DeviationBps == 0 {
		cfg.DeviationBps = DeviationToleranceBps
	}
	s := &Service{store: store, nf: nf, cfg: cfg, state: newState(), now: time.Now}
	if !cfg.Freshness.IsZero() {
		base := cfg.Freshness
		s.now = func() time.Time { return base }
	}
	if err := store.Replay(func(env Envelope) error {
		s.state.apply(env, false, nil)
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// SetClock 仅供测试固定时钟。
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) nowTime() time.Time { return s.now().UTC() }

// ---------------------------------------------------------------------------
// 请求/结果结构
// ---------------------------------------------------------------------------

type OpenAccountReq struct {
	AccountID        string
	Holder           string
	BaseCcy          Currency
	CreditLimitMinor int64
}

type EnrollCardReq struct {
	CardID    string
	AccountID string
	PAN       string // 明文只在调用栈内存在，落盘前脱敏
	Scope     AuthScope
	FeeBps    int64
}

type AuthReq struct {
	CardID       string
	TxnID        string // 客户端可指定以做请求级幂等；空则系统生成
	Kind         TxnKind
	MCC          string
	Country      string
	MerchantName string
	RRN          string // 终端检索参考号（收单机构唯一性由收单行保证）
	STAN         string
	TxnCcy       Currency
	AmountMinor  int64
	OccurredAt   time.Time // 终端交易时间；零值用当前时间
	TTL          time.Duration
}

type AuthResult struct {
	TxnID        string
	Approved     bool
	Reason       string
	BillCcy      Currency
	BillAmount   int64
	FeeMinor     int64
	FX           FXSnapshotRef
	RateFallback bool // true=无有效快照按 1:0 临时冻结
}

type ClearReq struct {
	CardID       string
	TxnID        string // 可空，按 RRN 匹配原授权
	RRN          string
	STAN         string
	BatchNo      string
	TxnCcy       Currency // 无在先授权的离线补传必须携带交易币种
	FinalMinor   int64    // 最终交易币种金额（补扣/部分请款）
	Offline      bool     // 离线终端补传
	MCC          string   // 无授权补传时需要
	Country      string
	MerchantName string
	OccurredAt   time.Time
}

type ReverseReq struct {
	CardID      string
	TxnID       string
	RRN         string
	ReversalRRN string
	AmountMinor int64 // 撤销交易币种金额；0 或 >=剩余授权 视为全额
	OccurredAt  time.Time
}

type DisputeReq struct {
	DisputeID  string
	AccountID  string
	TxnID      string
	ReasonCode string
	Narrative  string
	ClaimMinor int64
}

// ---------------------------------------------------------------------------
// 账户 / 卡片
// ---------------------------------------------------------------------------

func (s *Service) OpenAccount(r OpenAccountReq) error {
	if r.AccountID == "" || r.Holder == "" || r.BaseCcy == "" {
		return serr("invalid_argument", "account id, holder and base currency required")
	}
	if r.CreditLimitMinor < 0 {
		return serr("invalid_argument", "credit limit must be non-negative")
	}
	if _, ok := s.state.Accounts[r.AccountID]; ok {
		return serr("already_exists", "account %s exists", r.AccountID)
	}
	e := AccountOpenedEvent{
		AccountID: r.AccountID, Holder: r.Holder, BaseCcy: r.BaseCcy,
		CreditLimitMinor: r.CreditLimitMinor, OpenedAt: s.nowTime(),
	}
	return s.append(r.AccountID, EvAccountOpened, e)
}

func (s *Service) EnrollCard(r EnrollCardReq) error {
	a := s.state.Accounts[r.AccountID]
	if a == nil {
		return wrapErr("not_found", ErrNotFound)
	}
	if _, ok := s.state.Cards[r.CardID]; ok {
		return serr("already_exists", "card %s exists", r.CardID)
	}
	e := CardEnrolledEvent{
		CardID: r.CardID, AccountID: r.AccountID, MaskedPAN: MaskPAN(r.PAN),
		AuthScope: r.Scope, FeeBps: r.FeeBps, EnrolledAt: s.nowTime(),
	}
	return s.append(r.AccountID, EvCardEnrolled, e)
}

// ---------------------------------------------------------------------------
// 汇率快照：异常 -> 隔离 -> 人工复核，绝不静默采用
// ---------------------------------------------------------------------------

type ReceiveFXReq struct {
	SnapshotID string
	Pair       CcyPair
	RateMicro  int64
	AsOf       time.Time
	Source     string
}

// ReceiveFXRate 接收一张汇率快照。返回 quarantine=true 表示进入人工复核。
// 判定规则：非正数、过期（AsOf 早于当前-24h）、相对当前有效汇率偏离 >5%。
func (s *Service) ReceiveFXRate(r ReceiveFXReq) (quarantined bool, reason string, err error) {
	if r.SnapshotID == "" || r.Pair.Base == "" || r.Pair.Quote == "" {
		return false, "", serr("invalid_argument", "snapshot id and currency pair required")
	}
	if r.AsOf.IsZero() {
		r.AsOf = s.nowTime()
	}
	switch {
	case r.RateMicro <= 0:
		reason = "rate_not_positive"
	case s.nowTime().Sub(r.AsOf.UTC()) > MaxFXAge:
		reason = "rate_expired"
	default:
		if active := s.state.ActiveFX(r.Pair); active != nil {
			dev := deviationBps(r.RateMicro, active.RateMicro)
			if dev > s.cfg.DeviationBps {
				reason = "rate_deviation_exceeds_threshold"
			}
		}
	}
	rec := FXRateReceivedEvent{
		SnapshotID: r.SnapshotID, Pair: r.Pair, RateMicro: r.RateMicro,
		AsOf: r.AsOf.UTC(), Source: r.Source, Quarantined: reason != "",
		Reason: reason, ReceivedAt: s.nowTime(),
	}
	if err = s.append("", EvFXRateReceived, rec); err != nil {
		return false, "", err
	}
	if reason != "" {
		var ref int64
		if active := s.state.ActiveFX(r.Pair); active != nil {
			ref = active.RateMicro
		}
		q := FXRateQuarantinedEvent{
			SnapshotID: r.SnapshotID, Pair: r.Pair, RateMicro: r.RateMicro,
			ReferenceMicro: ref, Reason: reason, At: s.nowTime(),
		}
		if err = s.append("", EvFXRateQuarantined, q); err != nil {
			return true, reason, err
		}
		return true, reason, nil
	}
	return false, "", nil
}

// ReviewFX 人工复核隔离中的汇率（仅员工；异常汇率绝不静默采用）。
func (s *Service) ReviewFX(actor Actor, snapshotID string, approve bool, rejectReason string) error {
	if !actor.IsStaff() {
		return wrapErr("forbidden", ErrForbidden)
	}
	var found bool
	for _, fx := range s.state.FX {
		if fx.quarantine[snapshotID] != nil {
			found = true
			break
		}
	}
	if !found {
		return wrapErr("not_found", ErrNotFound)
	}
	if approve {
		return s.append("", EvFXRateApproved, FXRateApprovedEvent{
			SnapshotID: snapshotID, Reviewer: actor.StaffID(), At: s.nowTime(),
		})
	}
	return s.append("", EvFXRateRejected, FXRateRejectedEvent{
		SnapshotID: snapshotID, Reviewer: actor.StaffID(), Reason: rejectReason, At: s.nowTime(),
	})
}

func deviationBps(a, b int64) int64 {
	if a == b {
		return 0
	}
	// |a-b|/b * 10000，用整数做避免浮点
	d := a - b
	if d < 0 {
		d = -d
	}
	return (d*10_000 + b/2) / b
}

// fxForConvert 取换算依据。若交易币种与账单币种相同，用单位汇率。
// 否则取该货币对当前有效快照；不存在时回退 1:0 临时占额并标记 fallback，
// 绝不使用隔离中的异常汇率。
func (s *Service) fxForConvert(txn, bill Currency) (FXSnapshotRef, bool) {
	if txn == bill {
		return FXSnapshotRef{
			Pair: CcyPair{Base: txn, Quote: bill}, RateMicro: RateScale,
			RateText: FormatRate(RateScale), AsOf: s.nowTime(), Source: "identity",
		}, false
	}
	if active := s.state.ActiveFX(CcyPair{Base: txn, Quote: bill}); active != nil {
		return *active, false
	}
	return FXSnapshotRef{
		Pair: CcyPair{Base: txn, Quote: bill}, RateMicro: RateScale,
		RateText: "PENDING_RATE", AsOf: s.nowTime(), Source: "pending",
	}, true
}

// ---------------------------------------------------------------------------
// 授权（刷卡 / 预授权）
// ---------------------------------------------------------------------------

func (s *Service) Authorize(r AuthReq) (AuthResult, error) {
	card := s.state.Cards[r.CardID]
	if card == nil {
		return AuthResult{}, wrapErr("card_not_found", ErrNotFound)
	}
	acct := s.state.Accounts[card.AccountID]
	if r.AmountMinor <= 0 || r.RRN == "" {
		return AuthResult{}, serr("invalid_argument", "positive amount and RRN required")
	}
	if r.Kind == "" {
		r.Kind = KindPurchase
	}
	occ := r.OccurredAt.UTC()
	if occ.IsZero() {
		occ = s.nowTime()
	}

	// 幂等：同卡 + RRN 的任何在先请求（批准/拒绝/离线补传）都直接命中，
	// 商户重试、跨午夜重复送、重复批次都不会产生第二笔。
	key := txnKey(r.CardID, r.RRN)
	if id, ok := s.state.TxnByKey[key]; ok {
		s.recordDuplicate(r.CardID, r.RRN, r.STAN, "", id, "duplicate_authorization_rrn")
		t := s.state.Txns[id]
		return AuthResult{
			TxnID: id, Approved: t.Status != StatusReversed,
			Reason: "idempotent_hit", BillCcy: t.BillCcy, BillAmount: t.AuthBill,
			FeeMinor: t.AuthFee, FX: t.AuthFX,
		}, nil
	}

	txnID := r.TxnID
	if txnID == "" {
		txnID = "txn_" + randID()
	}

	fx, fallback := s.fxForConvert(r.TxnCcy, acct.BaseCcy)
	billAmt, err := ConvertMinor(r.AmountMinor, fx.RateMicro)
	if err != nil {
		return AuthResult{}, wrapErr("conversion_error", err)
	}
	fee := FeeBps(billAmt, card.FeeBps)
	hold := billAmt + fee

	// 持卡人授权范围校验（日限额按账单币种占用额累计）。
	if reason := checkScope(card.Scope, r, acct, occ, hold); reason != "" {
		return s.decline(txnID, r, reason, occ)
	}

	if hold > acct.Available() {
		return s.decline(txnID, r, "insufficient_funds", occ)
	}

	ttl := r.TTL
	if ttl == 0 {
		ttl = s.cfg.AuthTTL
	}
	e := AuthorizedEvent{
		TxnID: txnID, CardID: r.CardID, AccountID: acct.ID, Kind: r.Kind,
		MCC: r.MCC, Country: r.Country, MerchantName: r.MerchantName,
		RRN: r.RRN, STAN: r.STAN, TxnCcy: r.TxnCcy, TxnAmount: r.AmountMinor,
		BillCcy: acct.BaseCcy, BillAmount: hold, FeeMinor: fee, FeeBps: card.FeeBps,
		FX: fx, ExpiresAt: s.nowTime().Add(ttl), OccurredAt: occ, PostedAt: s.nowTime(),
	}
	if err := s.append(acct.ID, EvAuthorized, e); err != nil {
		return AuthResult{}, err
	}
	s.notify(acct.ID, txnID, "", "push", "txn_authorized")
	return AuthResult{
		TxnID: txnID, Approved: true, BillCcy: acct.BaseCcy,
		BillAmount: hold, FeeMinor: fee, FX: fx, RateFallback: fallback,
	}, nil
}

func checkScope(scope AuthScope, r AuthReq, acct *Account, occ time.Time, holdMinor int64) string {
	if len(scope.BlockedMCCs) > 0 && contains(scope.BlockedMCCs, r.MCC) {
		return "mcc_blocked"
	}
	if len(scope.AllowedMCCs) > 0 && !contains(scope.AllowedMCCs, r.MCC) {
		return "mcc_not_allowed"
	}
	if len(scope.Countries) > 0 && !contains(scope.Countries, r.Country) {
		return "country_not_allowed"
	}
	if scope.SingleTxLimit > 0 && r.AmountMinor > scope.SingleTxLimit {
		return "single_txn_limit_exceeded"
	}
	if scope.DailyLimit > 0 && acct.DailySpent[dayKey(occ)]+holdMinor > scope.DailyLimit {
		return "daily_limit_exceeded"
	}
	return ""
}

func (s *Service) decline(txnID string, r AuthReq, reason string, occ time.Time) (AuthResult, error) {
	e := AuthDeclinedEvent{
		TxnID: txnID, CardID: r.CardID, RRN: r.RRN, STAN: r.STAN,
		Reason: reason, OccurredAt: occ, PostedAt: s.nowTime(),
	}
	card := s.state.Cards[r.CardID]
	if err := s.append(card.AccountID, EvAuthDeclined, e); err != nil {
		return AuthResult{}, err
	}
	return AuthResult{TxnID: txnID, Approved: false, Reason: reason}, nil
}

func (s *Service) recordDuplicate(cardID, rrn, stan, batch, hitTxn, reason string) {
	card := s.state.Cards[cardID]
	var acct string
	if card != nil {
		acct = card.AccountID
	}
	e := DuplicateRejectedEvent{
		CardID: cardID, RRN: rrn, STAN: stan, BatchNo: batch,
		TxnID: hitTxn, Reason: reason, At: s.nowTime(),
	}
	_ = s.append(acct, EvDuplicateRejected, e)
}

// ExpireDue 扫描到期未清算授权并释放冻结。
func (s *Service) ExpireDue() (int, error) {
	now := s.nowTime()
	var due []string
	for id, t := range s.state.Txns {
		if !t.Cleared && t.HeldBill > 0 && !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt) {
			due = append(due, id)
		}
	}
	for _, id := range due {
		t := s.state.Txns[id]
		e := AuthExpiredEvent{TxnID: id, ReleasedMinor: t.HeldBill, At: now}
		if err := s.append(t.AccountID, EvAuthExpired, e); err != nil {
			return len(due) - 1, err
		}
	}
	return len(due), nil
}

// ---------------------------------------------------------------------------
// 撤销（支持部分撤销；清算前后皆可）
// ---------------------------------------------------------------------------

func (s *Service) Reverse(r ReverseReq) error {
	if r.ReversalRRN == "" {
		return serr("invalid_argument", "reversal rrn required")
	}
	t := s.findTxn(r.CardID, r.TxnID, r.RRN)
	if t == nil {
		return wrapErr("txn_not_found", ErrNotFound)
	}
	if s.state.RevByKey[txnKey(r.CardID, r.ReversalRRN)] {
		s.recordDuplicate(r.CardID, r.ReversalRRN, "", "", t.ID, "duplicate_reversal_rrn")
		return nil
	}
	var remaining, amount int64
	if t.Cleared {
		remaining = t.FinalTxn - t.PostRevTxn
	} else {
		remaining = t.TxnAmount - t.RevTxnPre
	}
	if remaining <= 0 {
		return serr("already_fully_reversed", "txn %s has no remaining amount", t.ID)
	}
	amount = r.AmountMinor
	if amount <= 0 || amount >= remaining {
		amount = remaining // 全额撤销
	}

	e := ReversedEvent{
		TxnID: t.ID, ReversalRRN: r.ReversalRRN, AmountMinor: amount,
		AfterCleared: t.Cleared, PostedAt: s.nowTime(),
	}
	if t.Cleared {
		// 清算后撤销：按最终清算快照比例退还账单金额与手续费。
		e.BillRefundMinor = prorate(amount, t.FinalTxn, t.FinalBill)
		e.FeeRefundMinor = prorate(amount, t.FinalTxn, t.FinalFee)
		e.FX = t.ClearFX
	} else {
		// 清算前撤销：按授权快照比例释放冻结；若授权已过期释放，HeldBill=0 即无冻结可放。
		e.BillRefundMinor = min64(prorate(amount, t.TxnAmount, t.AuthBill), t.HeldBill)
		// 手续费按比例随冻结释放，且不超过冻结中仍含的手续费。
		feeLeft := t.AuthFee - t.FeeReleased
		e.FeeRefundMinor = min64(prorate(amount, t.TxnAmount, t.AuthFee), feeLeft)
		e.FX = t.AuthFX
	}
	if err := s.append(t.AccountID, EvReversed, e); err != nil {
		return err
	}
	s.notify(t.AccountID, t.ID, "", "push", "txn_reversed")
	return nil
}

func prorate(part, total, whole int64) int64 {
	if total <= 0 || whole <= 0 {
		return 0
	}
	return (part*whole + total/2) / total
}

// ---------------------------------------------------------------------------
// 清算 / 补扣 / 离线补传
// ---------------------------------------------------------------------------

// ClearResult 清算结果。
type ClearResult struct {
	TxnID        string
	Matched      bool // 是否命中在先授权
	Offline      bool
	FinalBill    int64 // 最终入账账单金额（含费）
	FinalFee     int64
	DeltaMinor   int64 // 相对冻结的补扣(+)/释放(-)
	FX           FXSnapshotRef
	RateFallback bool
}

// Presentment 处理商户请款：在线请款或离线终端补传。
// 同一 RRN 重复送（商户重试/重复批次/跨午夜补送）只命中一次。
func (s *Service) Presentment(r ClearReq) (ClearResult, error) {
	if r.RRN == "" || r.FinalMinor <= 0 {
		return ClearResult{}, serr("invalid_argument", "rrn and positive final amount required")
	}
	card := s.state.Cards[r.CardID]
	if card == nil {
		return ClearResult{}, wrapErr("card_not_found", ErrNotFound)
	}
	occ := r.OccurredAt.UTC()
	if occ.IsZero() {
		occ = s.nowTime()
	}

	key := txnKey(r.CardID, r.RRN)
	if id, ok := s.state.TxnByKey[key]; ok {
		t := s.state.Txns[id]
		if t.Cleared {
			// 已入账：重复请款拦截，不发生第二次扣款。
			s.recordDuplicate(r.CardID, r.RRN, r.STAN, r.BatchNo, id, "duplicate_presentment_rrn")
			return ClearResult{TxnID: id, Matched: true, Offline: r.Offline, FinalBill: t.FinalBill}, nil
		}
		return s.clearExisting(t, r, occ)
	}

	// 无在先授权：离线终端补传（force post）。
	if !r.Offline {
		return ClearResult{}, serr("no_authorization", "RRN %s has no prior authorization", r.RRN)
	}
	if r.TxnCcy == "" {
		return ClearResult{}, serr("invalid_argument", "txn currency required for offline force post")
	}
	if card.Scope.OnlineOnly {
		return s.forceDeclined(card, r, "offline_not_allowed_by_scope", occ)
	}
	if reason := checkScopeStatic(card.Scope, r.MCC, r.Country, r.FinalMinor); reason != "" {
		return s.forceDeclined(card, r, reason, occ)
	}
	return s.forcePost(card, r, occ)
}

func checkScopeStatic(scope AuthScope, mcc, country string, amountMinor int64) string {
	if len(scope.BlockedMCCs) > 0 && contains(scope.BlockedMCCs, mcc) {
		return "mcc_blocked"
	}
	if len(scope.AllowedMCCs) > 0 && !contains(scope.AllowedMCCs, mcc) {
		return "mcc_not_allowed"
	}
	if len(scope.Countries) > 0 && !contains(scope.Countries, country) {
		return "country_not_allowed"
	}
	if scope.SingleTxLimit > 0 && amountMinor > scope.SingleTxLimit {
		return "single_txn_limit_exceeded"
	}
	return ""
}

func (s *Service) clearExisting(t *Txn, r ClearReq, occ time.Time) (ClearResult, error) {
	remaining := t.RemainingAuthAmount()
	// 最终请款低于剩余授权=部分请款（多余冻结释放）；高于剩余授权=补扣（如酒店杂费）。
	partial := r.FinalMinor < remaining
	final := r.FinalMinor
	// 清算以“当前生效汇率快照”为准重算最终账单金额；授权时钉住的快照只用于展示冻结口径。
	fx, fallback := s.fxForConvert(t.TxnCcy, t.BillCcy)
	billAmt, err := ConvertMinor(final, fx.RateMicro)
	if err != nil {
		return ClearResult{}, wrapErr("conversion_error", err)
	}
	card := s.state.Cards[t.CardID]
	fee := FeeBps(billAmt, card.FeeBps)
	finalBill := billAmt + fee
	prior := t.HeldBill
	delta := finalBill - prior
	// 补扣超出可用余额：仍入账（商户已请款，事后追收），但触发通知。
	e := ClearedEvent{
		TxnID: t.ID, RRN: r.RRN, STAN: r.STAN, BatchNo: r.BatchNo,
		FinalTxnAmount: final, FinalBillAmount: finalBill, FinalFeeMinor: fee,
		PriorHeldMinor: prior, DeltaMinor: delta, Offline: r.Offline, Partial: partial,
		FX: fx, OccurredAt: occ, PostedAt: s.nowTime(),
	}
	if err := s.append(t.AccountID, EvCleared, e); err != nil {
		return ClearResult{}, err
	}
	tmpl := "txn_cleared"
	if delta > 0 {
		tmpl = "txn_cleared_supplemental" // 补扣
	}
	s.notify(t.AccountID, t.ID, "", "push", tmpl)
	return ClearResult{
		TxnID: t.ID, Matched: true, Offline: r.Offline,
		FinalBill: finalBill, FinalFee: fee, DeltaMinor: delta,
		FX: fx, RateFallback: fallback,
	}, nil
}

func (s *Service) forcePost(card *Card, r ClearReq, occ time.Time) (ClearResult, error) {
	acct := s.state.Accounts[card.AccountID]
	fx, fallback := s.fxForConvert(r.TxnCcy, acct.BaseCcy)
	billAmt, err := ConvertMinor(r.FinalMinor, fx.RateMicro)
	if err != nil {
		return ClearResult{}, wrapErr("conversion_error", err)
	}
	fee := FeeBps(billAmt, card.FeeBps)
	finalBill := billAmt + fee
	if card.Scope.DailyLimit > 0 && acct.DailySpent[dayKey(occ)]+finalBill > card.Scope.DailyLimit {
		return s.forceDeclined(card, r, "daily_limit_exceeded", occ)
	}
	txnID := "txn_" + randID()
	e := ForcePostedEvent{
		TxnID: txnID, CardID: card.CardID, AccountID: acct.ID,
		MCC: r.MCC, Country: r.Country, MerchantName: r.MerchantName,
		RRN: r.RRN, STAN: r.STAN, BatchNo: r.BatchNo,
		TxnCcy: r.TxnCcy, TxnAmount: r.FinalMinor,
		BillCcy: acct.BaseCcy, BillAmount: finalBill, FeeMinor: fee, FeeBps: card.FeeBps,
		FX: fx, OccurredAt: occ, PostedAt: s.nowTime(),
	}
	if err := s.append(acct.ID, EvForcePosted, e); err != nil {
		return ClearResult{}, err
	}
	s.notify(acct.ID, txnID, "", "push", "txn_force_posted")
	return ClearResult{
		TxnID: txnID, Matched: false, Offline: true,
		FinalBill: finalBill, FinalFee: fee, DeltaMinor: finalBill,
		FX: fx, RateFallback: fallback,
	}, nil
}

func (s *Service) forceDeclined(card *Card, r ClearReq, reason string, occ time.Time) (ClearResult, error) {
	e := AuthDeclinedEvent{
		TxnID: "dp_" + randID(), CardID: card.CardID, RRN: r.RRN, STAN: r.STAN,
		Reason: reason, OccurredAt: occ, PostedAt: s.nowTime(),
	}
	if err := s.append(card.AccountID, EvAuthDeclined, e); err != nil {
		return ClearResult{}, err
	}
	return ClearResult{Matched: false, Offline: true, FinalBill: 0}, nil
}

func (s *Service) findTxn(cardID, txnID, rrn string) *Txn {
	if txnID != "" {
		return s.state.Txns[txnID]
	}
	if rrn != "" {
		if id, ok := s.state.TxnByKey[txnKey(cardID, rrn)]; ok {
			return s.state.Txns[id]
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func randID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
