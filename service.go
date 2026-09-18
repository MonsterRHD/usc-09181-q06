package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 默认监管参数，可按账户覆盖。
const (
	defaultFxFeeBps  = 150  // 外币手续费 1.5%
	defaultRebillBps = 2000 // 允许商户补扣上限 20%
	DefaultFxDevBps  = 200  // 默认汇率偏离复核阈值 2%
	identitySnap     = "identity"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrInvalid       = errors.New("invalid request")
	ErrFxUnavailable = errors.New("no acceptable fx rate")
)

type Service struct {
	mu     sync.Mutex
	state  *State
	store  *Store
	clock  func() time.Time
	devBps int64 // 汇率异常阈值
}

func NewService(s *State, st *Store) *Service {
	return &Service{state: s, store: st, clock: time.Now, devBps: DefaultFxDevBps}
}

func (svc *Service) nextID(prefix string) string {
	svc.state.Counter++
	return fmt.Sprintf("%s%d", prefix, svc.state.Counter)
}

// ---------- 账户 ----------

type CreateAccountReq struct {
	ID           string           `json:"id"`
	HolderID     string           `json:"holder_id"`
	CardMask     string           `json:"card_mask"`
	HomeCurrency string           `json:"home_currency"`
	Balances     map[string]int64 `json:"balances"`
	TxnLimit     map[string]int64 `json:"txn_limit"`
	FxFeeBps     *int             `json:"fx_fee_bps"`
	RebillBps    *int             `json:"rebill_bps"`
}

func (svc *Service) CreateAccount(req CreateAccountReq) (*Account, error) {
	if req.ID == "" || !knownCurrency(req.HomeCurrency) {
		return nil, fmt.Errorf("%w: id and supported home_currency required", ErrInvalid)
	}
	if !isMaskedCard(req.CardMask) {
		return nil, fmt.Errorf("%w: card_mask must be masked, e.g. 6225********1234", ErrInvalid)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if _, ok := svc.state.Accounts[req.ID]; ok {
		return nil, fmt.Errorf("%w: account exists", ErrConflict)
	}
	bal := map[string]int64{}
	for c, v := range req.Balances {
		if knownCurrency(c) && v >= 0 {
			bal[c] = v
		}
	}
	a := &Account{
		ID: req.ID, HolderID: req.HolderID, CardMask: req.CardMask,
		HomeCurrency: req.HomeCurrency, Balances: bal,
		Holds: map[string]int64{}, Fees: map[string]int64{}, TxnLimit: map[string]int64{},
		FxFeeBps: defaultFxFeeBps, RebillBps: defaultRebillBps,
		CreatedAt: svc.clock(),
	}
	for k, v := range req.TxnLimit {
		if knownCurrency(k) && v > 0 {
			a.TxnLimit[k] = v
		}
	}
	if req.FxFeeBps != nil && *req.FxFeeBps >= 0 {
		a.FxFeeBps = *req.FxFeeBps
	}
	if req.RebillBps != nil && *req.RebillBps >= 0 {
		a.RebillBps = *req.RebillBps
	}
	svc.state.Accounts[req.ID] = a
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return a, nil
}

func isMaskedCard(m string) bool {
	return len(m) >= 8 && strings.Contains(m, "*")
}

// ---------- 汇率快照 ----------

func fxPair(from, to string) string { return from + "/" + to }

// AddFXSnapshot 接收汇率快照。相对当前已采纳汇率偏离超阈值 → 挂起人工复核，绝不静默采用。
func (svc *Service) AddFXSnapshot(from, to, source, rateStr string, effectiveAt time.Time) (*FXSnapshot, error) {
	if !knownCurrency(from) || !knownCurrency(to) || from == to {
		return nil, fmt.Errorf("%w: currency pair", ErrInvalid)
	}
	rate, err := parseRateMicros(rateStr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	pair := fxPair(from, to)
	fx := svc.state.FX[pair]
	if fx == nil {
		fx = &FXPair{Pair: pair}
		svc.state.FX[pair] = fx
	}
	snap := &FXSnapshot{
		ID: svc.nextID("FX"), Pair: pair, RateMicros: rate, Rate: formatRate(rate),
		Source: source, EffectiveAt: effectiveAt, ReceivedAt: svc.clock(),
		Status: statusAccepted,
	}
	if cur := fx.currentSnapshotLocked(); cur != nil {
		snap.PriorRateMicros = cur.RateMicros
		snap.DeviationBps = devBps(cur.RateMicros, rate)
		if snap.DeviationBps > svc.devBps {
			snap.Status = statusPending
			fx.PendingIDs = append(fx.PendingIDs, snap.ID)
			fx.Snapshots = append(fx.Snapshots, snap)
			svc.auditLocked("fx_flag", "system", "system", "",
				fmt.Sprintf("snapshot %s %s rate=%s deviation=%dbps exceeds threshold %dbps",
					snap.ID, pair, snap.Rate, snap.DeviationBps, svc.devBps))
			if err := svc.persistLocked(); err != nil {
				return nil, err
			}
			return snap, nil
		}
	}
	fx.CurrentID = snap.ID
	fx.Snapshots = append(fx.Snapshots, snap)
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return snap, nil
}

// ReviewFX 人工复核异常快照：approve 采纳为当前汇率，reject 驳回（永不采用）。
func (svc *Service) ReviewFX(snapshotID, decision, agentID string) (*FXSnapshot, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: reviewer id required", ErrInvalid)
	}
	if decision != "approve" && decision != "reject" {
		return nil, fmt.Errorf("%w: decision must be approve|reject", ErrInvalid)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	snap, fx := svc.findSnapshotLocked(snapshotID)
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot", ErrNotFound)
	}
	if snap.Status != statusPending {
		return nil, fmt.Errorf("%w: snapshot not pending review", ErrConflict)
	}
	now := svc.clock()
	snap.ReviewedBy = agentID
	snap.ReviewedAt = &now
	if decision == "approve" {
		snap.Status = statusAccepted
		fx.CurrentID = snap.ID
	} else {
		snap.Status = "rejected"
	}
	fx.PendingIDs = removeString(fx.PendingIDs, snap.ID)
	svc.auditLocked("fx_review:"+decision, "agent", agentID, "",
		fmt.Sprintf("snapshot %s %s rate=%s", snap.ID, snap.Pair, snap.Rate))
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return snap, nil
}

func (fx *FXPair) currentSnapshotLocked() *FXSnapshot {
	if fx.CurrentID == "" {
		return nil
	}
	return fx.snapshotByID(fx.CurrentID)
}

func (fx *FXPair) snapshotByID(id string) *FXSnapshot {
	for i := len(fx.Snapshots) - 1; i >= 0; i-- {
		if fx.Snapshots[i].ID == id {
			return fx.Snapshots[i]
		}
	}
	return nil
}

func (svc *Service) findSnapshotLocked(id string) (*FXSnapshot, *FXPair) {
	for _, fx := range svc.state.FX {
		if s := fx.snapshotByID(id); s != nil {
			return s, fx
		}
	}
	return nil, nil
}

// acceptedRateLocked 返回当前已采纳汇率；异常待复核快照永不会被返回。
func (svc *Service) acceptedRateLocked(from, to string) (*FXSnapshot, error) {
	if from == to {
		return &FXSnapshot{ID: identitySnap, RateMicros: 1_000_000, Rate: "1.000000", Status: statusAccepted}, nil
	}
	fx := svc.state.FX[fxPair(from, to)]
	if fx == nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrFxUnavailable, from, to)
	}
	s := fx.currentSnapshotLocked()
	if s == nil || s.Status != statusAccepted {
		return nil, fmt.Errorf("%w: %s/%s", ErrFxUnavailable, from, to)
	}
	return s, nil
}

// ---------- 卡事件处理 ----------

func (svc *Service) Ingest(ev CardEvent) (*EventResult, error) {
	if err := validateEvent(ev); err != nil {
		return nil, err
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	acc := svc.state.Accounts[ev.AccountID]
	if acc == nil {
		return nil, fmt.Errorf("%w: account", ErrNotFound)
	}
	// 1) 网络事件 ID 幂等：重放原样返回，不再次动账、不再次通知。
	if existing, ok := svc.state.Events[ev.EventID]; ok {
		return &EventResult{Status: statusDuplicate,
			Reason:  "event already processed",
			EntryIDs: append([]string(nil), existing.Result.EntryIDs...)}, nil
	}
	// 先插入占位，handler 内可回写授权累计额。
	pe := &ProcessedEvent{Event: ev, ReceivedAt: svc.clock()}
	svc.state.Events[ev.EventID] = pe
	var res EventResult
	switch ev.Type {
	case "auth":
		res = svc.handleAuthLocked(acc, pe)
	case "reversal":
		res = svc.handleReversalLocked(acc, pe)
	case "presentment":
		res = svc.handlePresentmentLocked(acc, pe)
	case "rebill":
		res = svc.handleRebillLocked(acc, pe)
	default:
		res = EventResult{Status: statusDeclined, Reason: "unknown event type"}
	}
	pe.Result = res
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return &res, nil
}

func validateEvent(ev CardEvent) error {
	if ev.EventID == "" || ev.AccountID == "" || ev.Amount <= 0 {
		return fmt.Errorf("%w: event_id, account_id and positive amount required", ErrInvalid)
	}
	if !knownCurrency(ev.Currency) {
		return fmt.Errorf("%w: currency %q", ErrInvalid, ev.Currency)
	}
	if ev.TxnTime.IsZero() {
		return fmt.Errorf("%w: txn_time required", ErrInvalid)
	}
	switch ev.Type {
	case "presentment", "rebill":
		if ev.LinkedEventID == "" {
			return fmt.Errorf("%w: %s requires linked_event_id", ErrInvalid, ev.Type)
		}
	case "auth", "reversal":
	default:
		return fmt.Errorf("%w: unknown type %q", ErrInvalid, ev.Type)
	}
	return nil
}

func (svc *Service) handleAuthLocked(acc *Account, pe *ProcessedEvent) EventResult {
	ev := pe.Event
	// 2) 商户重试去重：同账户+商户+订单号的授权只认第一笔（仅成功时占用）。
	var refIdx string
	if ev.MerchantRef != "" {
		refIdx = refKey(ev.AccountID, ev.MerchantID, ev.MerchantRef)
		if firstID, dup := svc.state.RefIndex[refIdx]; dup {
			return EventResult{Status: statusDeclined, Reason: "duplicate merchant_ref; already authorized as " + firstID}
		}
	}
	rate, err := svc.acceptedRateLocked(ev.Currency, acc.HomeCurrency)
	if err != nil {
		return EventResult{Status: statusDeclined, Reason: err.Error()}
	}
	homePrincipal, fee := svc.pricingLocked(acc, ev.Currency, ev.Amount, rate)
	total := homePrincipal + fee
	if lim, ok := acc.TxnLimit[ev.Currency]; ok && ev.Amount > lim {
		return EventResult{Status: statusDeclined, Reason: "amount exceeds cardholder per-transaction limit"}
	}
	if acc.Balances[acc.HomeCurrency]-acc.Holds[acc.HomeCurrency] < total {
		return EventResult{Status: statusDeclined, Reason: "insufficient available balance (incl. estimated fee)"}
	}
	e := svc.appendEntryLocked(acc, pe, "hold", ev.Amount, fee, 0, total, rate)
	pe.AuthAmount = ev.Amount
	pe.HeldRemaining = total
	if refIdx != "" {
		svc.state.RefIndex[refIdx] = ev.EventID
	}
	svc.notifyLocked(acc.ID, "auth_held", ev.EventID, "预授权冻结",
		fmt.Sprintf("%s %d 已预授权，预计费用 %d %s，冻结合计 %d（汇率快照 %s=%s）",
			ev.Currency, ev.Amount, fee, acc.HomeCurrency, total, rate.ID, rate.Rate))
	return EventResult{Status: statusAccepted, EntryIDs: []string{e.ID}}
}

func (svc *Service) handleReversalLocked(acc *Account, pe *ProcessedEvent) EventResult {
	ev := pe.Event
	if ev.LinkedEventID == "" {
		return EventResult{Status: statusDeclined, Reason: "reversal requires linked_event_id"}
	}
	orig, ok := svc.state.Events[ev.LinkedEventID]
	if !ok {
		return EventResult{Status: statusDeclined, Reason: "linked event not found"}
	}
	if orig.Event.AccountID != ev.AccountID || orig.Event.Currency != ev.Currency {
		return EventResult{Status: statusDeclined, Reason: "account/currency mismatch with linked event"}
	}
	var refIdx string
	if ev.MerchantRef != "" {
		refIdx = refKey(ev.AccountID, ev.MerchantID, "REV:"+ev.MerchantRef)
		if firstID, dup := svc.state.RefIndex[refIdx]; dup {
			return EventResult{Status: statusDeclined, Reason: "duplicate reversal ref; already processed as " + firstID}
		}
	}
	// 退货/撤销无论挂在授权、补传还是补扣报文上，都归并到承载清算累计额的结算事件。
	root := svc.settleRootLocked(orig)
	var res EventResult
	switch {
	case orig.Event.Type == "auth" && !orig.Settled:
		res = svc.reverseAuthLocked(acc, orig, pe)
	case root.Settled:
		res = svc.refundAfterSettleLocked(acc, root, pe)
	default:
		res = EventResult{Status: statusDeclined, Reason: "linked event not in a reversible state"}
	}
	if res.Status == statusAccepted && refIdx != "" {
		svc.state.RefIndex[refIdx] = ev.EventID
	}
	return res
}

// settleRootLocked 返回承载清算累计额（SettledAmount/Rebilled/RefundedPost）的事件：
// 补传与补扣报文本身不落这些字段，沿链接回溯到已结算的授权。
func (svc *Service) settleRootLocked(pe *ProcessedEvent) *ProcessedEvent {
	if pe.Settled {
		return pe
	}
	if lid := pe.Event.LinkedEventID; lid != "" {
		if parent, ok := svc.state.Events[lid]; ok && parent.Settled {
			return parent
		}
	}
	return pe
}

// reverseAuthLocked 授权后清算前的（部分）撤销：按比例解冻本金与预计费用。
func (svc *Service) reverseAuthLocked(acc *Account, orig, pe *ProcessedEvent) EventResult {
	ev := pe.Event
	remaining := orig.AuthAmount - orig.ReversedPre
	if remaining <= 0 {
		return EventResult{Status: statusDeclined, Reason: "authorization already fully reversed"}
	}
	if ev.Amount > remaining {
		return EventResult{Status: statusDeclined, Reason: "reversal exceeds unreversed authorized amount"}
	}
	rate, err := svc.acceptedRateLocked(ev.Currency, acc.HomeCurrency)
	if err != nil {
		return EventResult{Status: statusDeclined, Reason: err.Error()}
	}
	homeAmt, fee := svc.pricingLocked(acc, ev.Currency, ev.Amount, rate)
	release := homeAmt + fee
	if release > orig.HeldRemaining {
		release = orig.HeldRemaining // 末笔取整保护
	}
	orig.ReversedPre += ev.Amount
	orig.HeldRemaining -= release
	e := svc.appendEntryLocked(acc, pe, "hold_release", -ev.Amount, -fee, 0, -release, rate)
	svc.notifyLocked(acc.ID, "auth_released", ev.EventID, "撤销释放冻结",
		fmt.Sprintf("%s %d%s，释放冻结 %d %s", ev.Currency, ev.Amount,
			partialTag(ev.Amount, remaining+ev.Amount), release, acc.HomeCurrency))
	return EventResult{Status: statusAccepted, EntryIDs: []string{e.ID}}
}

// refundAfterSettleLocked 清算后的（部分）退货/撤销：退回本金及对应手续费。
func (svc *Service) refundAfterSettleLocked(acc *Account, orig, pe *ProcessedEvent) EventResult {
	ev := pe.Event
	refundable := orig.SettledAmount + orig.Rebilled - orig.RefundedPost - orig.Chargeback
	if refundable <= 0 {
		return EventResult{Status: statusDeclined, Reason: "transaction already fully refunded"}
	}
	if ev.Amount > refundable {
		return EventResult{Status: statusDeclined, Reason: "refund exceeds remaining transaction amount"}
	}
	rate, err := svc.acceptedRateLocked(ev.Currency, acc.HomeCurrency)
	if err != nil {
		return EventResult{Status: statusDeclined, Reason: err.Error()}
	}
	homeAmt, fee := svc.pricingLocked(acc, ev.Currency, ev.Amount, rate)
	refund := homeAmt + fee
	orig.RefundedPost += ev.Amount
	e := svc.appendEntryLocked(acc, pe, "reversal", ev.Amount, fee, refund, 0, rate)
	svc.notifyLocked(acc.ID, "refund", ev.EventID, "退货/撤销入账",
		fmt.Sprintf("%s %d 已退回 %d %s（含手续费 %d，快照 %s）",
			ev.Currency, ev.Amount, refund, acc.HomeCurrency, fee, rate.ID))
	return EventResult{Status: statusAccepted, EntryIDs: []string{e.ID}}
}

func (svc *Service) handlePresentmentLocked(acc *Account, pe *ProcessedEvent) EventResult {
	ev := pe.Event
	orig, ok := svc.state.Events[ev.LinkedEventID]
	if !ok || orig.Event.Type != "auth" {
		return EventResult{Status: statusDeclined, Reason: "linked authorization not found"}
	}
	if orig.Event.AccountID != ev.AccountID || orig.Event.Currency != ev.Currency {
		return EventResult{Status: statusDeclined, Reason: "account/currency mismatch with authorization"}
	}
	if orig.Settled {
		// 离线终端补传 / 网络重发：同一授权只能清算一次。
		return EventResult{Status: statusDeclined, Reason: "authorization already settled; duplicate presentment rejected"}
	}
	if ev.MerchantRef != "" && orig.Event.MerchantRef != "" && ev.MerchantRef != orig.Event.MerchantRef {
		return EventResult{Status: statusDeclined, Reason: "merchant_ref does not match authorization"}
	}
	if ev.Amount > orig.AuthAmount-orig.ReversedPre {
		return EventResult{Status: statusDeclined, Reason: "presentment exceeds remaining authorized amount"}
	}
	// 清算采用“当前已采纳”快照（可能与预授权不同，例如期间汇率正常更新）。
	rate, err := svc.acceptedRateLocked(ev.Currency, acc.HomeCurrency)
	if err != nil {
		return EventResult{Status: statusDeclined, Reason: err.Error()}
	}
	homePrincipal, fee := svc.pricingLocked(acc, ev.Currency, ev.Amount, rate)
	need := homePrincipal + fee
	release := orig.HeldRemaining
	// 释放冻结后余额必须足以覆盖清算（异常波动未被采纳，因此此处一般成立）。
	if acc.Balances[acc.HomeCurrency]-(acc.Holds[acc.HomeCurrency]-release) < need {
		return EventResult{Status: statusDeclined, Reason: "settlement exceeds available funds after hold release"}
	}
	var ids []string
	er := svc.appendEntryLocked(acc, pe, "hold_release", 0, 0, 0, -release, rate)
	ids = append(ids, er.ID)
	es := svc.appendEntryLocked(acc, pe, "settle", -ev.Amount, 0, -homePrincipal, 0, rate)
	ids = append(ids, es.ID)
	if fee > 0 {
		ef := svc.appendEntryLocked(acc, pe, "fee", 0, -fee, -fee, 0, rate)
		ids = append(ids, ef.ID)
		acc.Fees[acc.HomeCurrency] += fee
	}
	orig.Settled = true
	orig.SettledAmount = ev.Amount
	orig.HeldRemaining = 0
	tag := "清算完成"
	if ev.Offline {
		tag = "离线补传清算完成"
	}
	svc.notifyLocked(acc.ID, "settled", ev.EventID, tag,
		fmt.Sprintf("%s %d 按快照 %s=%s 清算 %d %s，手续费 %d，释放冻结 %d",
			ev.Currency, ev.Amount, rate.ID, rate.Rate, homePrincipal, acc.HomeCurrency, fee, release))
	return EventResult{Status: statusAccepted, EntryIDs: ids}
}

func (svc *Service) handleRebillLocked(acc *Account, pe *ProcessedEvent) EventResult {
	ev := pe.Event
	orig, ok := svc.state.Events[ev.LinkedEventID]
	if !ok || !orig.Settled {
		return EventResult{Status: statusDeclined, Reason: "rebill requires a settled transaction"}
	}
	if orig.Event.AccountID != ev.AccountID || orig.Event.Currency != ev.Currency {
		return EventResult{Status: statusDeclined, Reason: "account/currency mismatch"}
	}
	// 商户补扣受持卡人授权范围约束：累计补扣 ≤ 清算额 × RebillBps。
	rebillCap := orig.SettledAmount * int64(acc.RebillBps) / 10_000
	if ev.Amount > rebillCap-orig.Rebilled {
		return EventResult{Status: statusDeclined, Reason: "rebill exceeds cardholder-authorized incremental amount"}
	}
	rate, err := svc.acceptedRateLocked(ev.Currency, acc.HomeCurrency)
	if err != nil {
		return EventResult{Status: statusDeclined, Reason: err.Error()}
	}
	homePrincipal, fee := svc.pricingLocked(acc, ev.Currency, ev.Amount, rate)
	if acc.Balances[acc.HomeCurrency]-acc.Holds[acc.HomeCurrency] < homePrincipal+fee {
		return EventResult{Status: statusDeclined, Reason: "insufficient available balance for rebill"}
	}
	e := svc.appendEntryLocked(acc, pe, "rebill", -ev.Amount, -fee, -(homePrincipal + fee), 0, rate)
	if fee > 0 {
		acc.Fees[acc.HomeCurrency] += fee
	}
	orig.Rebilled += ev.Amount
	svc.notifyLocked(acc.ID, "rebill", ev.EventID, "商户补扣",
		fmt.Sprintf("%s %d 已补扣，合计 %d %s，手续费 %d（快照 %s=%s）",
			ev.Currency, ev.Amount, homePrincipal, acc.HomeCurrency, fee, rate.ID, rate.Rate))
	return EventResult{Status: statusAccepted, EntryIDs: []string{e.ID}}
}

// pricingLocked 按固定快照折算本金并计算外币手续费。
func (svc *Service) pricingLocked(acc *Account, ccy string, amount int64, rate *FXSnapshot) (homePrincipal, fee int64) {
	homePrincipal = amount
	if ccy != acc.HomeCurrency {
		homePrincipal = convert(amount, rate.RateMicros)
		fee = feeBpsAmount(homePrincipal, acc.feeBps(ccy))
	}
	return homePrincipal, fee
}

// ---------- 账务流水（不可变 + 哈希链） ----------

func (svc *Service) appendEntryLocked(acc *Account, pe *ProcessedEvent, kind string,
	amount, fee, dBalance, dHold int64, rate *FXSnapshot) *Entry {
	ev := pe.Event
	seq := len(svc.state.Entries) + 1
	e := &Entry{
		Seq: seq, ID: fmt.Sprintf("L%d", seq), AccountID: acc.ID, EventID: ev.EventID,
		Kind: kind, Currency: ev.Currency, Amount: amount, Fee: fee,
		DeltaBalance: dBalance, DeltaHold: dHold, LinkedEventID: ev.LinkedEventID,
		BusinessTime: ev.TxnTime, RecordedAt: svc.clock(), PrevHash: svc.state.LastEntryHash,
		FxSnapshotID: identitySnap,
	}
	if ev.Currency != acc.HomeCurrency {
		e.SourceCurrency = ev.Currency
		e.SourceAmount = abs64(amount)
		e.FxRateMicros = rate.RateMicros
		e.FxSnapshotID = rate.ID
	}
	acc.Balances[acc.HomeCurrency] += dBalance
	acc.Holds[acc.HomeCurrency] += dHold
	e.Hash = hashEntry(e)
	svc.state.LastEntryHash = e.Hash
	svc.state.Entries = append(svc.state.Entries, e)
	return e
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func partialTag(part, total int64) string {
	if part < total {
		return "（部分撤销）"
	}
	return ""
}

func hashEntry(e *Entry) string {
	s := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s|%d|%d|%d|%d|%s|%d|%s|%s",
		e.PrevHash, e.Seq, e.ID, e.AccountID, e.EventID, e.Kind, e.Currency,
		e.Amount, e.Fee, e.DeltaBalance, e.DeltaHold, e.LinkedEventID,
		e.FxRateMicros, e.FxSnapshotID, e.BusinessTime.UTC().Format(time.RFC3339Nano))
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---------- 通知（去重计数） ----------

func (svc *Service) notifyLocked(accountID, ntype, eventID, title, body string) {
	dedup := ntype + "|" + eventID
	for _, n := range svc.state.Notifications[accountID] {
		if n.DedupKey == dedup {
			return
		}
	}
	n := &Notification{
		ID: svc.nextID("N"), AccountID: accountID, Type: ntype, DedupKey: dedup,
		Title: title, Body: body, EventID: eventID, CreatedAt: svc.clock(),
	}
	svc.state.Notifications[accountID] = append(svc.state.Notifications[accountID], n)
}

// ---------- 展示与换算依据固定 ----------

type BalanceView struct {
	Currency   string           `json:"currency"`
	Balance    int64            `json:"balance"`
	Holds      int64            `json:"holds"`
	Available  int64            `json:"available"`
	SubWallets map[string]int64 `json:"sub_wallets,omitempty"`
}

func (svc *Service) GetBalanceView(accountID string) (*BalanceView, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	acc, ok := svc.state.Accounts[accountID]
	if !ok {
		return nil, fmt.Errorf("%w: account", ErrNotFound)
	}
	v := &BalanceView{
		Currency: acc.HomeCurrency, Balance: acc.Balances[acc.HomeCurrency],
		Holds: acc.Holds[acc.HomeCurrency],
		Available: acc.Balances[acc.HomeCurrency] - acc.Holds[acc.HomeCurrency],
	}
	for c, b := range acc.Balances {
		if c != acc.HomeCurrency {
			if v.SubWallets == nil {
				v.SubWallets = map[string]int64{}
			}
			v.SubWallets[c] = b
		}
	}
	return v, nil
}

// FixConversion 为某一次展示固定换算依据；历史展示永久可追溯到具体快照。
func (svc *Service) FixConversion(accountID, subject, fromCCY string, amountMinor int64) (*DisplayBasis, error) {
	if amountMinor < 0 {
		return nil, fmt.Errorf("%w: amount must be non-negative", ErrInvalid)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	acc, ok := svc.state.Accounts[accountID]
	if !ok {
		return nil, fmt.Errorf("%w: account", ErrNotFound)
	}
	rate, err := svc.acceptedRateLocked(fromCCY, acc.HomeCurrency)
	if err != nil {
		return nil, err
	}
	converted := amountMinor
	if fromCCY != acc.HomeCurrency {
		converted = convert(amountMinor, rate.RateMicros)
	}
	b := &DisplayBasis{
		ID: svc.nextID("B"), AccountID: accountID, Subject: subject,
		FromCurrency: fromCCY, ToCurrency: acc.HomeCurrency, SourceAmount: amountMinor,
		RateMicros: rate.RateMicros, Rate: formatRate(rate.RateMicros), SnapshotID: rate.ID,
		Converted: converted, CreatedAt: svc.clock(),
	}
	svc.state.DisplayBases = append(svc.state.DisplayBases, b)
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

// MaskedDetail 客户可见的脱敏交易明细。
type MaskedDetail struct {
	EventID       string    `json:"event_id"`
	Type          string    `json:"type"`
	MerchantName  string    `json:"merchant_name"`
	MerchantRef   string    `json:"merchant_ref,omitempty"`
	CardMask      string    `json:"card_mask"`
	Currency      string    `json:"currency"`
	Amount        int64     `json:"amount"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	Offline       bool      `json:"offline"`
	TxnTime       time.Time `json:"txn_time"`
	HomeCurrency  string    `json:"home_currency"`
	FxSnapshotID  string    `json:"fx_snapshot_id,omitempty"`
	FxRate        string    `json:"fx_rate,omitempty"`
	HomePrincipal int64     `json:"home_principal,omitempty"`
	FeeHome       int64     `json:"fee_home,omitempty"`
	LinkedEventID string    `json:"linked_event_id,omitempty"`
	DisputeID     string    `json:"dispute_id,omitempty"`
}

func (svc *Service) ListDetails(accountID string) ([]*MaskedDetail, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	acc, ok := svc.state.Accounts[accountID]
	if !ok {
		return nil, fmt.Errorf("%w: account", ErrNotFound)
	}
	disputeByEvent := map[string]string{}
	for _, d := range svc.state.Disputes {
		if d.AccountID == accountID {
			disputeByEvent[d.EventID] = d.ID
		}
	}
	ids := make([]string, 0, len(svc.state.Events))
	for id, pe := range svc.state.Events {
		if pe.Event.AccountID == accountID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		ti, tj := svc.state.Events[ids[i]].ReceivedAt, svc.state.Events[ids[j]].ReceivedAt
		if ti.Equal(tj) {
			return ids[i] < ids[j]
		}
		return ti.Before(tj)
	})
	out := make([]*MaskedDetail, 0, len(ids))
	for _, id := range ids {
		pe := svc.state.Events[id]
		ev := pe.Event
		d := &MaskedDetail{
			EventID: ev.EventID, Type: ev.Type, MerchantName: maskMerchant(ev.MerchantName),
			MerchantRef: ev.MerchantRef, CardMask: acc.CardMask,
			Currency: ev.Currency, Amount: ev.Amount, Status: pe.Result.Status, Reason: pe.Result.Reason,
			Offline: ev.Offline, TxnTime: ev.TxnTime, HomeCurrency: acc.HomeCurrency,
			LinkedEventID: ev.LinkedEventID, DisputeID: disputeByEvent[ev.EventID],
		}
		home, fee, snapID, rateStr := svc.totalsFromEntriesLocked(pe)
		d.HomePrincipal = home
		d.FeeHome = fee
		d.FxSnapshotID = snapID
		d.FxRate = rateStr
		out = append(out, d)
	}
	return out, nil
}

// totalsFromEntriesLocked 以不可变流水为唯一口径汇总本币本金/费用与历史快照，绝不用当前汇率重算历史。
func (svc *Service) totalsFromEntriesLocked(pe *ProcessedEvent) (home, fee int64, snapID, rateStr string) {
	for _, e := range svc.state.Entries {
		if e.EventID != pe.Event.EventID {
			continue
		}
		if e.FxSnapshotID != "" && e.FxSnapshotID != identitySnap {
			snapID = e.FxSnapshotID
			rateStr = formatRate(e.FxRateMicros)
		}
		switch e.Kind {
		case "settle":
			home += -e.DeltaBalance
		case "rebill":
			home += -e.DeltaBalance + e.Fee
			fee += -e.Fee
		case "fee":
			fee += -e.Fee
		case "reversal": // 清算后退货：本金与退回费用均按正量展示，方向由事件类型表达
			home += e.DeltaBalance - e.Fee
			fee += e.Fee
		case "chargeback":
			home += e.DeltaBalance
		case "hold":
			home += e.DeltaHold - e.Fee
			fee += e.Fee
		case "hold_release":
			if e.Amount != 0 { // 清算前（部分）撤销：展示释放的本金与费用
				home += -(e.DeltaHold - e.Fee)
				fee += -e.Fee
			}
		}
	}
	return home, fee, snapID, rateStr
}

func maskMerchant(name string) string {
	r := []rune(name)
	switch {
	case len(r) == 0:
		return ""
	case len(r) <= 2:
		return strings.Repeat("*", len(r))
	default:
		return string(r[:1]) + strings.Repeat("*", len(r)-2) + string(r[len(r)-1:])
	}
}

// ---------- 争议 ----------

func (svc *Service) OpenDispute(accountID, eventID, reason, customerID string) (*Dispute, error) {
	if reason == "" || customerID == "" {
		return nil, fmt.Errorf("%w: reason and customer id required", ErrInvalid)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	pe, ok := svc.state.Events[eventID]
	if !ok || pe.Event.AccountID != accountID {
		return nil, fmt.Errorf("%w: event", ErrNotFound)
	}
	if pe.Result.Status != statusAccepted {
		return nil, fmt.Errorf("%w: dispute only for accepted transactions", ErrConflict)
	}
	for _, d := range svc.state.Disputes {
		if d.AccountID == accountID && d.EventID == eventID && d.Status != "lost" {
			return nil, fmt.Errorf("%w: dispute already open (%s)", ErrConflict, d.ID)
		}
	}
	now := svc.clock()
	d := &Dispute{
		ID: svc.nextID("D"), AccountID: accountID, EventID: eventID, Reason: reason,
		Status: "open", CreatedAt: now,
		Notes: []DisputeNote{{
			Seq: 1, AuthorID: customerID, Role: "customer", Kind: "statement",
			Content: reason, CreatedAt: now,
		}},
	}
	svc.state.Disputes[d.ID] = d
	svc.notifyLocked(accountID, "dispute_opened", eventID, "争议已受理",
		"交易 "+eventID+" 的争议 "+d.ID+" 已受理")
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return d, nil
}

// AddDisputeConclusion 客服只能追加调查结论；无任何修改/删除交易或历史记录的路径。
func (svc *Service) AddDisputeConclusion(disputeID, agentID, content string) (*Dispute, error) {
	if agentID == "" || content == "" {
		return nil, fmt.Errorf("%w: agent_id and content required", ErrInvalid)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.state.Disputes[disputeID]
	if !ok {
		return nil, fmt.Errorf("%w: dispute", ErrNotFound)
	}
	if d.Status == "won" || d.Status == "lost" {
		return nil, fmt.Errorf("%w: dispute already resolved", ErrConflict)
	}
	d.Notes = append(d.Notes, DisputeNote{
		Seq: len(d.Notes) + 1, AuthorID: agentID, Role: "agent", Kind: "conclusion",
		Content: content, CreatedAt: svc.clock(),
	})
	if d.Status == "open" {
		d.Status = "under_review"
	}
	svc.auditLocked("dispute_note", "agent", agentID, d.AccountID,
		fmt.Sprintf("dispute %s conclusion appended", d.ID))
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	cp := *d
	cp.Notes = append([]DisputeNote(nil), d.Notes...)
	return &cp, nil
}

// ResolveDispute 裁决：won 触发冲正，lost 仅关闭；裁决说明同样只能追加。
func (svc *Service) ResolveDispute(disputeID, decision, agentID, conclusion string) (*Dispute, error) {
	if decision != "won" && decision != "lost" {
		return nil, fmt.Errorf("%w: decision must be won|lost", ErrInvalid)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.state.Disputes[disputeID]
	if !ok {
		return nil, fmt.Errorf("%w: dispute", ErrNotFound)
	}
	if d.Status == "won" || d.Status == "lost" {
		return nil, fmt.Errorf("%w: already resolved", ErrConflict)
	}
	if conclusion != "" {
		d.Notes = append(d.Notes, DisputeNote{
			Seq: len(d.Notes) + 1, AuthorID: agentID, Role: "agent", Kind: "conclusion",
			Content: conclusion, CreatedAt: svc.clock(),
		})
	}
	d.Status = decision
	if decision == "won" {
		svc.chargebackLocked(svc.state.Accounts[d.AccountID],
			svc.settleRootLocked(svc.state.Events[d.EventID]))
	}
	svc.auditLocked("dispute_resolve:"+decision, "agent", agentID, d.AccountID,
		fmt.Sprintf("dispute %s event %s", d.ID, d.EventID))
	svc.notifyLocked(d.AccountID, "dispute_resolved", d.EventID, "争议裁决结果",
		fmt.Sprintf("争议 %s 裁决：%s", d.ID, decision))
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	cp := *d
	cp.Notes = append([]DisputeNote(nil), d.Notes...)
	return &cp, nil
}

func (svc *Service) chargebackLocked(acc *Account, pe *ProcessedEvent) {
	refundable := pe.SettledAmount + pe.Rebilled - pe.RefundedPost - pe.Chargeback
	if refundable <= 0 {
		return
	}
	rate, err := svc.acceptedRateLocked(pe.Event.Currency, acc.HomeCurrency)
	if err != nil {
		rate = &FXSnapshot{ID: "unavailable", RateMicros: 1_000_000, Rate: "1.000000"}
	}
	// 胜诉全额冲正：本金连同对应外币手续费一并返还。
	homeAmt, fee := svc.pricingLocked(acc, pe.Event.Currency, refundable, rate)
	cbPe := &ProcessedEvent{Event: CardEvent{
		EventID: "CB:" + pe.Event.EventID, Type: "chargeback", AccountID: acc.ID,
		MerchantID: pe.Event.MerchantID, Currency: pe.Event.Currency, Amount: refundable,
		TxnTime: svc.clock(), LinkedEventID: pe.Event.EventID,
	}}
	svc.appendEntryLocked(acc, cbPe, "chargeback", refundable, 0, homeAmt+fee, 0, rate)
	pe.Chargeback += refundable
}

// ---------- 查询 / 监管导出 / 审计 ----------

func (svc *Service) Notifications(accountID string) []*Notification {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	out := make([]*Notification, len(svc.state.Notifications[accountID]))
	copy(out, svc.state.Notifications[accountID])
	return out
}

func (svc *Service) Dispute(disputeID string) (*Dispute, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.state.Disputes[disputeID]
	if !ok {
		return nil, fmt.Errorf("%w: dispute", ErrNotFound)
	}
	cp := *d
	cp.Notes = append([]DisputeNote(nil), d.Notes...)
	return &cp, nil
}

func (svc *Service) PendingFX() []*FXSnapshot {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	var out []*FXSnapshot
	for _, fx := range svc.state.FX {
		for _, id := range fx.PendingIDs {
			if s := fx.snapshotByID(id); s != nil {
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func hashAudit(a *AuditRecord) string {
	s := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", a.PrevHash, a.Seq, a.ID, a.Action, a.ActorID, a.AccountID, a.Detail)
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (svc *Service) auditLocked(action, role, actorID, accountID, detail string) {
	a := &AuditRecord{
		Seq: len(svc.state.Audit) + 1, ID: svc.nextID("A"), Action: action,
		ActorRole: role, ActorID: actorID, AccountID: accountID, Detail: detail,
		CreatedAt: svc.clock(), PrevHash: svc.state.LastAuditHash,
	}
	a.Hash = hashAudit(a)
	svc.state.LastAuditHash = a.Hash
	svc.state.Audit = append(svc.state.Audit, a)
}

func (svc *Service) verifyChainLocked() (entriesOK, auditOK bool, brokenAt int) {
	prev := ""
	for i, e := range svc.state.Entries {
		if e.PrevHash != prev || e.Hash != hashEntry(e) {
			return false, true, i + 1
		}
		prev = e.Hash
	}
	prev = ""
	for i, a := range svc.state.Audit {
		if a.PrevHash != prev || a.Hash != hashAudit(a) {
			return true, false, i + 1
		}
		prev = a.Hash
	}
	return true, true, 0
}

func (svc *Service) VerifyChain() (entriesOK, auditOK bool, brokenAt int) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.verifyChainLocked()
}

// ExportReport 按账户导出监管最小记录：仅掩码卡，无 PAN/令牌外泄。
type ExportReport struct {
	GeneratedAt       time.Time       `json:"generated_at"`
	AccountID         string          `json:"account_id"`
	HolderID          string          `json:"holder_id"`
	CardMask          string          `json:"card_mask"`
	HomeCurrency      string          `json:"home_currency"`
	BalanceView       *BalanceView    `json:"balance_view"`
	Events            []*MaskedDetail `json:"events"`
	Entries           []*Entry        `json:"entries"`
	Disputes          []*Dispute      `json:"disputes"`
	NotificationCount int             `json:"notification_count"`
	EntriesChainOK    bool            `json:"entries_chain_ok"`
	AuditChainOK      bool            `json:"audit_chain_ok"`
}

func (svc *Service) ExportAccount(accountID, agentID string) (*ExportReport, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	acc, ok := svc.state.Accounts[accountID]
	if !ok {
		return nil, fmt.Errorf("%w: account", ErrNotFound)
	}
	details := make([]*MaskedDetail, 0)
	disputeByEvent := map[string]string{}
	disputes := []*Dispute{}
	for _, d := range svc.state.Disputes {
		if d.AccountID == accountID {
			disputeByEvent[d.EventID] = d.ID
			disputes = append(disputes, d)
		}
	}
	sort.Slice(disputes, func(i, j int) bool { return disputes[i].ID < disputes[j].ID })
	var entries []*Entry
	for _, e := range svc.state.Entries {
		if e.AccountID != accountID {
			continue
		}
		entries = append(entries, e)
	}
	ids := make([]string, 0)
	for id, pe := range svc.state.Events {
		if pe.Event.AccountID == accountID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		ti, tj := svc.state.Events[ids[i]].ReceivedAt, svc.state.Events[ids[j]].ReceivedAt
		if ti.Equal(tj) {
			return ids[i] < ids[j]
		}
		return ti.Before(tj)
	})
	for _, id := range ids {
		pe := svc.state.Events[id]
		ev := pe.Event
		home, fee, snapID, rateStr := svc.totalsFromEntriesLocked(pe)
		details = append(details, &MaskedDetail{
			EventID: ev.EventID, Type: ev.Type, MerchantName: maskMerchant(ev.MerchantName),
			MerchantRef: ev.MerchantRef, CardMask: acc.CardMask, Currency: ev.Currency,
			Amount: ev.Amount, Status: pe.Result.Status, Reason: pe.Result.Reason,
			Offline: ev.Offline, TxnTime: ev.TxnTime, HomeCurrency: acc.HomeCurrency,
			HomePrincipal: home, FeeHome: fee, FxSnapshotID: snapID, FxRate: rateStr,
			LinkedEventID: ev.LinkedEventID, DisputeID: disputeByEvent[ev.EventID],
		})
	}
	svc.auditLocked("regulatory_export", "agent", agentID, accountID,
		fmt.Sprintf("export %d entries, %d disputes", len(entries), len(disputes)))
	entriesOK, auditOK, _ := svc.verifyChainLocked()
	if err := svc.persistLocked(); err != nil {
		return nil, err
	}
	return &ExportReport{
		GeneratedAt: svc.clock(), AccountID: accountID, HolderID: acc.HolderID,
		CardMask: acc.CardMask, HomeCurrency: acc.HomeCurrency,
		BalanceView: &BalanceView{
			Currency: acc.HomeCurrency, Balance: acc.Balances[acc.HomeCurrency],
			Holds: acc.Holds[acc.HomeCurrency],
			Available: acc.Balances[acc.HomeCurrency] - acc.Holds[acc.HomeCurrency],
		},
		Events: details, Entries: entries, Disputes: disputes,
		NotificationCount: len(svc.state.Notifications[accountID]),
		EntriesChainOK: entriesOK, AuditChainOK: auditOK,
	}, nil
}

func (svc *Service) persistLocked() error { return svc.store.Save(svc.state) }

func refKey(account, merchant, ref string) string {
	return account + "|" + merchant + "|" + ref
}

func removeString(xs []string, s string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
