package travelmoney

import (
	"strings"
	"testing"
	"time"
)

// testEnv 封装固定时钟的服务环境。
type testEnv struct {
	svc *Service
	mem *MemoryStore
	nf  *RecordingNotifier
	now time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	base := time.Date(2026, 9, 18, 23, 0, 0, 0, time.UTC) // 午夜前一小时，便于构造跨午夜
	env := &testEnv{
		mem: NewMemoryStore(),
		nf:  &RecordingNotifier{},
		now: base,
	}
	svc, err := NewService(env.mem, env.nf, Config{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	env.svc = svc
	env.advance(0)
	return env
}

func (e *testEnv) advance(d time.Duration) {
	e.now = e.now.Add(d)
	e.svc.SetClock(func() time.Time { return e.now })
}

const (
	acctID = "acct-1"
	cardID = "card-1"
	pan    = "4532015112830366"
)

func (e *testEnv) bootstrap(t *testing.T, limit int64, feeBps int64) {
	t.Helper()
	if err := e.svc.OpenAccount(OpenAccountReq{
		AccountID: acctID, Holder: "张小明", BaseCcy: "USD", CreditLimitMinor: limit,
	}); err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	if err := e.svc.EnrollCard(EnrollCardReq{
		CardID: cardID, AccountID: acctID, PAN: pan, FeeBps: feeBps,
	}); err != nil {
		t.Fatalf("EnrollCard: %v", err)
	}
}

func (e *testEnv) fx(t *testing.T, id, base, quote string, micro int64, asOf time.Time, source string) bool {
	t.Helper()
	q, reason, err := e.svc.ReceiveFXRate(ReceiveFXReq{
		SnapshotID: id, Pair: CcyPair{Base: Currency(base), Quote: Currency(quote)},
		RateMicro: micro, AsOf: asOf, Source: source,
	})
	if err != nil {
		t.Fatalf("ReceiveFXRate %s: %v", id, err)
	}
	if q {
		t.Logf("snapshot %s quarantined: %s", id, reason)
	}
	return q
}

// ---------------------------------------------------------------------------
// 现场检查主剧本：预授权 → 离线补传 → 汇率更新 → 重启；核对余额/争议/通知
// ---------------------------------------------------------------------------

func TestOnSiteScenario_PreauthOfflineClearFXUpdateRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenFileStore(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}

	base := time.Date(2026, 9, 18, 23, 30, 0, 0, time.UTC)
	clock := base
	newSvc := func(nf Notifier) *Service {
		svc, err := NewService(store, nf, Config{})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		svc.SetClock(func() time.Time { return clock })
		return svc
	}

	nf1 := &RecordingNotifier{}
	svc := newSvc(nf1)
	cust := NewCustomerActor(acctID)
	agent := NewStaffActor("agent-7", false)

	// 1) 开户、登记卡片、初始汇率 EUR/USD = 1.10
	must(t, svc.OpenAccount(OpenAccountReq{AccountID: acctID, Holder: "张小明", BaseCcy: "USD", CreditLimitMinor: 100000}))
	must(t, svc.EnrollCard(EnrollCardReq{CardID: cardID, AccountID: acctID, PAN: pan, FeeBps: 0}))
	q, _, err := svc.ReceiveFXRate(ReceiveFXReq{SnapshotID: "fx-1", Pair: CcyPair{"EUR", "USD"}, RateMicro: 1_100_000, AsOf: clock, Source: "ecb"})
	must(t, err)
	if q {
		t.Fatal("fresh rate fx-1 must not be quarantined")
	}

	// 2) 跨午夜预授权：终端时间 23:50，清算文件次日才补送
	clock = base.Add(20 * time.Minute) // 23:50
	auth, err := svc.Authorize(AuthReq{
		CardID: cardID, Kind: KindPreauth, MCC: "7011", Country: "FR",
		MerchantName: "HOTEL LUTECE", RRN: "rrn-1001", STAN: "00011",
		TxnCcy: "EUR", AmountMinor: 10000, OccurredAt: clock,
	})
	must(t, err)
	if !auth.Approved || auth.BillAmount != 11000 {
		t.Fatalf("preauth hold = %d approved=%v, want 11000", auth.BillAmount, auth.Approved)
	}

	snap1 := mustSnapshot(t, svc, cust)
	if snap1.HeldMinor != 11000 || snap1.BalanceMinor != 0 || snap1.AvailableMinor != 89000 {
		t.Fatalf("after preauth snapshot = %+v", snap1)
	}

	// 3) 商户重试同一预授权（同 RRN）：幂等命中，不产生第二次冻结
	retry, err := svc.Authorize(AuthReq{
		CardID: cardID, Kind: KindPreauth, MCC: "7011", Country: "FR",
		MerchantName: "HOTEL LUTECE", RRN: "rrn-1001", STAN: "00011",
		TxnCcy: "EUR", AmountMinor: 10000,
	})
	must(t, err)
	if !retry.Approved || retry.TxnID != auth.TxnID || retry.Reason != "idempotent_hit" {
		t.Fatalf("retry not idempotent: %+v", retry)
	}
	snap1b := mustSnapshot(t, svc, cust)
	if snap1b.HeldMinor != 11000 {
		t.Fatalf("retry changed hold: %+v", snap1b)
	}

	// 4) 离线终端补传同一笔（终端时间已跨午夜，批次为次日批次），按原额清算
	clock = base.Add(40 * time.Minute) // 次日 00:10
	clear1, err := svc.Presentment(ClearReq{
		CardID: cardID, RRN: "rrn-1001", STAN: "00011", BatchNo: "B0919",
		FinalMinor: 10000, Offline: true, OccurredAt: clock,
	})
	must(t, err)
	if !clear1.Matched || clear1.FinalBill != 11000 || clear1.DeltaMinor != 0 {
		t.Fatalf("offline clearing mismatch: %+v", clear1)
	}

	// 5) 重复批次再次补送同 RRN：必须拦截，绝不二次扣款
	clock = clock.Add(time.Hour)
	dup, err := svc.Presentment(ClearReq{
		CardID: cardID, RRN: "rrn-1001", STAN: "00011", BatchNo: "B0919X",
		FinalMinor: 10000, Offline: true, OccurredAt: clock,
	})
	must(t, err)
	if dup.FinalBill != 11000 {
		t.Fatalf("duplicate presentment result = %+v", dup)
	}

	snap2 := mustSnapshot(t, svc, cust)
	if snap2.HeldMinor != 0 || snap2.BalanceMinor != 11000 || snap2.AvailableMinor != 89000 {
		t.Fatalf("after clearing snapshot = %+v", snap2)
	}

	// 6) 汇率更新到 1.12（偏离 1.82%，在 5% 阈值内）→ 正常生效，历史明细口径不变
	q, _, err = svc.ReceiveFXRate(ReceiveFXReq{SnapshotID: "fx-2", Pair: CcyPair{"EUR", "USD"}, RateMicro: 1_120_000, AsOf: clock, Source: "ecb"})
	must(t, err)
	if q {
		t.Fatal("rate 1.12 within tolerance must be accepted")
	}

	// 已清算交易的展示仍钉住各自快照（授权=清算=fx-1），新汇率不改写历史
	tv := mustTxn(t, svc, cust, auth.TxnID)
	if tv.Auth.SnapshotID != "fx-1" || tv.Final.SnapshotID != "fx-1" {
		t.Fatalf("historical fx basis altered: auth=%s final=%s", tv.Auth.SnapshotID, tv.Final.SnapshotID)
	}
	if tv.NetBilled != 11000 || tv.Auth.RateText != "1.1" {
		t.Fatalf("view = %+v", tv)
	}

	// 7) 客户发起争议；客服追加备注与结论（胜诉全额退款）
	must(t, svc.OpenDispute(cust, DisputeReq{DisputeID: "dsp-1", AccountID: acctID, TxnID: auth.TxnID, ReasonCode: "duplicate", Narrative: "同笔酒店扣款显示两次", ClaimMinor: 11000}))
	must(t, svc.AddDisputeNote(agent, "dsp-1", "调取收单批次，重复批次已拦截"))
	must(t, svc.ConcludeDispute(agent, "dsp-1", "won", 11000, "商户重复请款不成立，全额退还持卡人"))

	snap3 := mustSnapshot(t, svc, cust)
	if snap3.BalanceMinor != 0 || snap3.HeldMinor != 0 || snap3.AvailableMinor != 100000 {
		t.Fatalf("after dispute refund snapshot = %+v", snap3)
	}
	disputes, err := svc.ListDisputes(cust, acctID)
	must(t, err)
	if len(disputes) != 1 || disputes[0].Status != DisputeConcluded || len(disputes[0].Conclusions) != 1 || len(disputes[0].Notes) != 1 {
		t.Fatalf("dispute view = %+v", disputes)
	}
	beforeRestartNotifs := snap3.Notifications
	wantNotifs := 4 // authorized, cleared, dispute_opened, dispute_refund
	tmpls := nf1.Templates()
	if len(tmpls) != wantNotifs {
		t.Fatalf("notifications before restart = %v, want %d", tmpls, wantNotifs)
	}

	// 8) 重启服务：从事件日志重放。余额、争议状态一致；通知计数一致但不会重发。
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store2, err := OpenFileStore(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	nf2 := &RecordingNotifier{}
	svc2 := newSvcWithStore(t, store2, nf2, clock)

	snap4 := mustSnapshot(t, svc2, cust)
	if snap4.HeldMinor != 0 || snap4.BalanceMinor != 0 || snap4.AvailableMinor != 100000 {
		t.Fatalf("post-restart snapshot = %+v", snap4)
	}
	if snap4.Notifications != beforeRestartNotifs {
		t.Fatalf("notification count changed across restart: before=%d after=%d", beforeRestartNotifs, snap4.Notifications)
	}
	if nf2.Count() != 0 {
		t.Fatalf("replay re-sent %d notifications", nf2.Count())
	}
	disputes2, err := svc2.ListDisputes(cust, acctID)
	must(t, err)
	if len(disputes2) != 1 || disputes2[0].Status != DisputeConcluded {
		t.Fatalf("post-restart disputes = %+v", disputes2)
	}
	// 重启后仍能拦截重复请款（幂等索引由重放恢复）
	dup2, err := svc2.Presentment(ClearReq{CardID: cardID, RRN: "rrn-1001", STAN: "00011", FinalMinor: 10000, Offline: true})
	must(t, err)
	if dup2.FinalBill != 11000 {
		t.Fatalf("post-restart duplicate guard broken: %+v", dup2)
	}
	if got := mustSnapshot(t, svc2, cust); got.BalanceMinor != 0 {
		t.Fatalf("post-restart duplicate re-charged: %+v", got)
	}
}

func newSvcWithStore(t *testing.T, store Store, nf Notifier, clock time.Time) *Service {
	t.Helper()
	svc, err := NewService(store, nf, Config{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	c := clock
	svc.SetClock(func() time.Time { return c })
	return svc
}

// ---------------------------------------------------------------------------
// 异常汇率：隔离 + 人工复核，绝不静默采用
// ---------------------------------------------------------------------------

func TestAbnormalFXQuarantineAndReview(t *testing.T) {
	e := newTestEnv(t)
	e.bootstrap(t, 500000, 0)
	t0 := e.now
	e.fx(t, "fx-ok", "JPY", "USD", 6700, t0, "ecb") // 0.0067

	// 偏离 20%，必须隔离
	if !e.fx(t, "fx-bad", "JPY", "USD", 8040, t0, "unknown-feed") {
		t.Fatal("deviating rate must be quarantined")
	}
	quar, err := e.svc.ListQuarantine(NewStaffActor("a1", false))
	must(t, err)
	if len(quar) != 1 || quar[0].Reason != "rate_deviation_exceeds_threshold" || quar[0].ReferenceMicro != 6700 {
		t.Fatalf("quarantine = %+v", quar)
	}

	// 隔离期间新交易仍按上一有效汇率换算，绝不静默采用异常值
	r, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-j1", STAN: "1", TxnCcy: "JPY", AmountMinor: 100000, MCC: "5812", Country: "JP", MerchantName: "TOKYO BAR"})
	must(t, err)
	if !r.Approved || r.FX.SnapshotID != "fx-ok" || r.BillAmount != 670 {
		t.Fatalf("auth under quarantine = %+v", r)
	}

	// 人工驳回：隔离汇率废弃，后续仍按旧汇率
	must(t, e.svc.ReviewFX(NewStaffActor("a1", false), "fx-bad", false, "来源不可信"))
	r2, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-j2", STAN: "2", TxnCcy: "JPY", AmountMinor: 100000, MCC: "5812", Country: "JP", MerchantName: "TOKYO BAR"})
	must(t, err)
	if r2.FX.SnapshotID != "fx-ok" {
		t.Fatalf("rejected rate must not take effect: %+v", r2)
	}

	// 再来一张偏离大的，人工批准后生效
	if !e.fx(t, "fx-bad2", "JPY", "USD", 8040, e.now, "manual-feed") {
		t.Fatal("expected quarantine")
	}
	must(t, e.svc.ReviewFX(NewStaffActor("a2", true), "fx-bad2", true, ""))
	r3, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-j3", STAN: "3", TxnCcy: "JPY", AmountMinor: 100000, MCC: "5812", Country: "JP", MerchantName: "TOKYO BAR"})
	must(t, err)
	if r3.FX.SnapshotID != "fx-bad2" || r3.BillAmount != 804 {
		t.Fatalf("approved rate should be active: %+v", r3)
	}

	// 客户角色无权查看隔离队列或复核
	if _, err := e.svc.ListQuarantine(NewCustomerActor(acctID)); err == nil {
		t.Fatal("customer must not list quarantine")
	}
	if err := e.svc.ReviewFX(NewCustomerActor(acctID), "fx-x", true, ""); err == nil {
		t.Fatal("customer must not review fx")
	}

	// 过期快照必须隔离
	e.advance(25 * time.Hour)
	if !e.fx(t, "fx-old", "GBP", "USD", 1300000, t0, "ecb") {
		t.Fatal("expired snapshot must be quarantined")
	}
	// 非正数汇率必须隔离
	if !e.fx(t, "fx-zero", "GBP", "USD", 0, e.now, "ecb") {
		t.Fatal("non-positive rate must be quarantined")
	}
}

// ---------------------------------------------------------------------------
// 部分撤销 / 全额撤销 / 清算后撤销（退款）
// ---------------------------------------------------------------------------

func TestPartialAndPostClearingReversals(t *testing.T) {
	e := newTestEnv(t)
	e.bootstrap(t, 1_000_000, 0)
	e.fx(t, "fx-eur", "EUR", "USD", 1_000_000, e.now, "ecb") // 1:1 便于核对

	// 预授权 200 EUR，冻结 200 USD；部分撤销 50 → 释放 50
	a, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-p", STAN: "1", Kind: KindPreauth, TxnCcy: "EUR", AmountMinor: 20000, MCC: "7512", Country: "DE", MerchantName: "MUNICH RENTAL"})
	must(t, err)
	must(t, e.svc.Reverse(ReverseReq{CardID: cardID, TxnID: a.TxnID, RRN: "rrn-p", ReversalRRN: "rev-1", AmountMinor: 5000}))
	snap := mustSnapshot(t, e.svc, NewCustomerActor(acctID))
	if snap.HeldMinor != 15000 {
		t.Fatalf("after partial reversal held=%d, want 15000", snap.HeldMinor)
	}

	// 撤销重放（同 ReversalRRN）幂等，不重复释放
	must(t, e.svc.Reverse(ReverseReq{CardID: cardID, TxnID: a.TxnID, RRN: "rrn-p", ReversalRRN: "rev-1", AmountMinor: 5000}))
	if snap2 := mustSnapshot(t, e.svc, NewCustomerActor(acctID)); snap2.HeldMinor != 15000 {
		t.Fatalf("duplicate reversal changed held: %+v", snap2)
	}

	// 剩余 150 清算，随后清算后撤销 30 → 退款 30
	c, err := e.svc.Presentment(ClearReq{CardID: cardID, RRN: "rrn-p", STAN: "1", FinalMinor: 15000})
	must(t, err)
	if c.FinalBill != 15000 {
		t.Fatalf("clearing = %+v", c)
	}
	must(t, e.svc.Reverse(ReverseReq{CardID: cardID, TxnID: a.TxnID, RRN: "rrn-p", ReversalRRN: "rev-2", AmountMinor: 3000}))
	snap = mustSnapshot(t, e.svc, NewCustomerActor(acctID))
	if snap.BalanceMinor != 12000 || snap.HeldMinor != 0 {
		t.Fatalf("post-clearing reversal snapshot = %+v", snap)
	}
	tv := mustTxn(t, e.svc, NewCustomerActor(acctID), a.TxnID)
	if tv.NetBilled != 12000 || tv.RefundedMinor != 3000 {
		t.Fatalf("txn view = %+v", tv)
	}
}

// ---------------------------------------------------------------------------
// 补扣：清算金额高于授权剩余额（酒店杂费）
// ---------------------------------------------------------------------------

func TestSupplementalCharge(t *testing.T) {
	e := newTestEnv(t)
	e.bootstrap(t, 1_000_000, 0)
	e.fx(t, "fx", "EUR", "USD", 1_000_000, e.now, "ecb")
	a, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-s", STAN: "1", Kind: KindPreauth, TxnCcy: "EUR", AmountMinor: 10000, MCC: "7011", Country: "FR", MerchantName: "HOTEL"})
	must(t, err)
	c, err := e.svc.Presentment(ClearReq{CardID: cardID, RRN: "rrn-s", STAN: "1", FinalMinor: 12000})
	must(t, err)
	if c.DeltaMinor != 2000 || c.FinalBill != 12000 {
		t.Fatalf("supplemental clearing = %+v", c)
	}
	snap := mustSnapshot(t, e.svc, NewCustomerActor(acctID))
	if snap.BalanceMinor != 12000 || snap.HeldMinor != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	// 清算口径钉住当次快照，授权口径保持原值
	tv := mustTxn(t, e.svc, NewCustomerActor(acctID), a.TxnID)
	if tv.Auth.BillAmount != 10000 || tv.Final.BillAmount != 12000 {
		t.Fatalf("view = %+v", tv)
	}
}

// ---------------------------------------------------------------------------
// 无授权离线补传（force post）与跨午夜
// ---------------------------------------------------------------------------

func TestForcePostAndCrossMidnight(t *testing.T) {
	e := newTestEnv(t)
	e.bootstrap(t, 1_000_000, 100) // 1% 手续费
	// 无汇率：外币交易按临时口径占额，标记 pending，不静默编造汇率
	c, err := e.svc.Presentment(ClearReq{
		CardID: cardID, RRN: "rrn-f", STAN: "9", BatchNo: "B-1",
		TxnCcy: "EUR", FinalMinor: 5000, Offline: true,
		MCC: "5541", Country: "US", MerchantName: "GAS STATION", OccurredAt: e.now.Add(2 * time.Hour),
	})
	must(t, err)
	if c.Matched || c.FinalBill != 5050 || !c.RateFallback {
		t.Fatalf("force post = %+v", c)
	}
	// 同 RRN 重放（商户次日重试）不得二次入账
	c2, err := e.svc.Presentment(ClearReq{CardID: cardID, RRN: "rrn-f", STAN: "9", BatchNo: "B-2", TxnCcy: "EUR", FinalMinor: 5000, Offline: true})
	must(t, err)
	if c2.TxnID != c.TxnID {
		t.Fatalf("force post retry produced new txn: %+v", c2)
	}
	snap := mustSnapshot(t, e.svc, NewCustomerActor(acctID))
	if snap.BalanceMinor != 5050 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// 在线请款但无在先授权：拒绝（不允许凭空入账）
	if _, err := e.svc.Presentment(ClearReq{CardID: cardID, RRN: "rrn-x", FinalMinor: 100, Offline: false}); err == nil {
		t.Fatal("online presentment without auth must fail")
	}

	// OnlineOnly 卡片拒绝离线补传
	must(t, e.svc.EnrollCard(EnrollCardReq{CardID: "card-2", AccountID: acctID, PAN: "5555555555554444", Scope: AuthScope{OnlineOnly: true}}))
	if _, err := e.svc.Presentment(ClearReq{CardID: "card-2", RRN: "rrn-o", TxnCcy: "USD", FinalMinor: 100, Offline: true, MCC: "5812", Country: "US"}); err != nil {
		// forceDeclined 不返回错误，以余额未增为准
	}
	snap2 := mustSnapshot(t, e.svc, NewCustomerActor(acctID))
	if snap2.BalanceMinor != 5050 {
		t.Fatalf("online-only card force-posted: %+v", snap2)
	}
}

// ---------------------------------------------------------------------------
// 授权范围（MCC / 国家 / 限额）与余额不足
// ---------------------------------------------------------------------------

func TestAuthScopeAndInsufficientFunds(t *testing.T) {
	e := newTestEnv(t)
	must(t, e.svc.OpenAccount(OpenAccountReq{AccountID: "a2", Holder: "李芳", BaseCcy: "USD", CreditLimitMinor: 100000}))
	must(t, e.svc.EnrollCard(EnrollCardReq{
		CardID: "c2", AccountID: "a2", PAN: "4111111111111111",
		Scope: AuthScope{Countries: []string{"US"}, BlockedMCCs: []string{"7995"}, SingleTxLimit: 5000, DailyLimit: 8000},
	}))
	auth := func(rrn, country, mcc string, amt int64) AuthResult {
		r, err := e.svc.Authorize(AuthReq{CardID: "c2", RRN: rrn, STAN: "1", TxnCcy: "USD", AmountMinor: amt, MCC: mcc, Country: country, MerchantName: "X"})
		must(t, err)
		return r
	}
	if r := auth("r1", "US", "5812", 4000); !r.Approved {
		t.Fatalf("in-scope declined: %+v", r)
	}
	if r := auth("r2", "FR", "5812", 100); r.Approved || r.Reason != "country_not_allowed" {
		t.Fatalf("country check failed: %+v", r)
	}
	if r := auth("r3", "US", "7995", 100); r.Approved || r.Reason != "mcc_blocked" {
		t.Fatalf("mcc block failed: %+v", r)
	}
	if r := auth("r4", "US", "5812", 6000); r.Approved || r.Reason != "single_txn_limit_exceeded" {
		t.Fatalf("single limit failed: %+v", r)
	}
	if r := auth("r5", "US", "5812", 5000); r.Approved || r.Reason != "daily_limit_exceeded" {
		t.Fatalf("daily limit failed: %+v", r)
	}
	// 余额不足：用一张无任何限额的新卡，金额直接超出可用额度
	must(t, e.svc.EnrollCard(EnrollCardReq{CardID: "c3", AccountID: "a2", PAN: "4000000000000002"}))
	r6, err := e.svc.Authorize(AuthReq{CardID: "c3", RRN: "r6", STAN: "1", TxnCcy: "USD", AmountMinor: 2000000, MCC: "5812", Country: "US", MerchantName: "X"})
	must(t, err)
	if r6.Approved || r6.Reason != "insufficient_funds" {
		t.Fatalf("insufficient funds check failed: %+v", r6)
	}
}

// ---------------------------------------------------------------------------
// 过期授权 + 迟到的离线补传（释放后补扣）
// ---------------------------------------------------------------------------

func TestExpiredAuthThenLatePresentment(t *testing.T) {
	env := newTestEnv(t)
	env.bootstrap(t, 100000, 0)
	env.fx(t, "fx", "EUR", "USD", 1_000_000, env.now, "ecb")
	a, err := env.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-e", STAN: "1", Kind: KindPreauth, TxnCcy: "EUR", AmountMinor: 10000, MCC: "7011", Country: "FR", MerchantName: "H", TTL: time.Minute})
	must(t, err)
	env.advance(2 * time.Minute)
	n, err := env.svc.ExpireDue()
	must(t, err)
	if n != 1 {
		t.Fatalf("expired count = %d", n)
	}
	snap := mustSnapshot(t, env.svc, NewCustomerActor(acctID))
	if snap.HeldMinor != 0 {
		t.Fatalf("held after expiry = %d", snap.HeldMinor)
	}
	// 终端补传迟到：仍按 RRN 命中并补入账
	c, err := env.svc.Presentment(ClearReq{CardID: cardID, RRN: "rrn-e", STAN: "1", FinalMinor: 10000, Offline: true})
	must(t, err)
	if c.TxnID != a.TxnID || c.FinalBill != 10000 {
		t.Fatalf("late presentment = %+v", c)
	}
	snap = mustSnapshot(t, env.svc, NewCustomerActor(acctID))
	if snap.BalanceMinor != 10000 {
		t.Fatalf("balance after late presentment = %d", snap.BalanceMinor)
	}
}

// ---------------------------------------------------------------------------
// 脱敏、最小记录与按账户导出
// ---------------------------------------------------------------------------

func TestMaskingAndAccountScopedExport(t *testing.T) {
	e := newTestEnv(t)
	e.bootstrap(t, 100000, 0)
	r, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-m", STAN: "1", TxnCcy: "USD", AmountMinor: 1000, MCC: "5812", Country: "US", MerchantName: "CAFE"})
	must(t, err)

	// 卡号全程脱敏，原始 PAN 不出现在任何视图/导出中
	tv := mustTxn(t, e.svc, NewCustomerActor(acctID), r.TxnID)
	if tv.MaskedPAN != "453201******0366" || strings.Contains(tv.MaskedPAN, "511283") {
		t.Fatalf("masking failed: %q", tv.MaskedPAN)
	}
	lines, err := e.svc.ExportAccount(NewStaffActor("reg-1", true), acctID)
	must(t, err)
	if len(lines) < 3 {
		t.Fatalf("export too short: %d lines", len(lines))
	}
	joined := strings.Join(bytesToStrings(lines), "\n")
	if strings.Contains(joined, pan) {
		t.Fatal("raw PAN leaked into export")
	}
	if !strings.Contains(joined, "453201******0366") {
		t.Fatal("masked PAN missing from export")
	}

	// 客户不能导出他人账户
	must(t, e.svc.OpenAccount(OpenAccountReq{AccountID: "a9", Holder: "其他客户", BaseCcy: "USD", CreditLimitMinor: 1}))
	if _, err := e.svc.ExportAccount(NewCustomerActor(acctID), "a9"); err == nil {
		t.Fatal("customer exported another account")
	}
	// 客户也不能查看他人明细
	if _, err := e.svc.ListTransactions(NewCustomerActor(acctID), "a9"); err == nil {
		t.Fatal("customer listed another account's txns")
	}
}

func TestMaskPANEdgeCases(t *testing.T) {
	cases := map[string]string{
		"4532015112830366":    "453201******0366",
		"4532-0151-1283-0366": "453201******0366",
		"1234567890":          "**********",
		"":                    "",
	}
	for in, want := range cases {
		if got := MaskPAN(in); got != want {
			t.Errorf("MaskPAN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertMinorRounding(t *testing.T) {
	// 100 EUR @1.105555 -> half-up
	got, err := ConvertMinor(10000, 1_105_555)
	must(t, err)
	// 10000*1_105_555/1e6 = 11055.55 -> 11056
	if got != 11056 {
		t.Fatalf("ConvertMinor = %d, want 11056", got)
	}
	if _, err := ConvertMinor(100, 0); err != ErrInvalidRate {
		t.Fatalf("zero rate err = %v", err)
	}
	if FormatRate(1_100_000) != "1.1" || FormatRate(6700) != "0.0067" {
		t.Fatalf("FormatRate wrong: %s %s", FormatRate(1_100_000), FormatRate(6700))
	}
}

// ---------------------------------------------------------------------------
// 客服只能追加结论：争议结论单向、退款只发生一次
// ---------------------------------------------------------------------------

func TestDisputeAppendOnlyAndRBAC(t *testing.T) {
	e := newTestEnv(t)
	e.bootstrap(t, 100000, 0)
	a, err := e.svc.Authorize(AuthReq{CardID: cardID, RRN: "rrn-d", STAN: "1", TxnCcy: "USD", AmountMinor: 3000, MCC: "5812", Country: "US", MerchantName: "SHOP"})
	must(t, err)
	_, err = e.svc.Presentment(ClearReq{CardID: cardID, RRN: "rrn-d", STAN: "1", FinalMinor: 3000})
	must(t, err)

	cust := NewCustomerActor(acctID)
	agent := NewStaffActor("ag-1", false)
	must(t, e.svc.OpenDispute(cust, DisputeReq{DisputeID: "d1", AccountID: acctID, TxnID: a.TxnID, ReasonCode: "amount_diff", ClaimMinor: 500}))
	// 客服不能代客户发起争议
	if err := e.svc.OpenDispute(agent, DisputeReq{DisputeID: "d2", AccountID: acctID, TxnID: a.TxnID, ClaimMinor: 1}); err == nil {
		t.Fatal("agent must not open disputes")
	}
	// 客户不能追加结论
	if err := e.svc.ConcludeDispute(cust, "d1", "won", 1, "x"); err == nil {
		t.Fatal("customer must not conclude disputes")
	}
	// 主张超额被拒绝
	if err := e.svc.OpenDispute(cust, DisputeReq{DisputeID: "d3", AccountID: acctID, TxnID: a.TxnID, ClaimMinor: 999999}); err == nil {
		t.Fatal("over-claim must be rejected")
	}
	must(t, e.svc.ConcludeDispute(agent, "d1", "partial", 500, "部分金额不符"))
	// 结论一次性，不能二次追加终局结论
	if err := e.svc.ConcludeDispute(agent, "d1", "lost", 0, "改判"); err == nil {
		t.Fatal("dispute must be immutable after conclusion")
	}
	// 过程备注仍可追加（只追加，不改历史）
	must(t, e.svc.AddDisputeNote(agent, "d1", "归档备注"))
	snap := mustSnapshot(t, e.svc, NewCustomerActor(acctID))
	if snap.BalanceMinor != 2500 {
		t.Fatalf("balance = %d, want 2500", snap.BalanceMinor)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustSnapshot(t *testing.T, svc *Service, actor Actor) AccountView {
	t.Helper()
	s, err := svc.Snapshot(actor, acctID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return s
}

func mustTxn(t *testing.T, svc *Service, actor Actor, id string) TxnView {
	t.Helper()
	tv, err := svc.GetTransaction(actor, acctID, id)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	return tv
}

func bytesToStrings(b [][]byte) []string {
	out := make([]string, len(b))
	for i, x := range b {
		out[i] = string(x)
	}
	return out
}
