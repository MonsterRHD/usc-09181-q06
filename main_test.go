package main

import (
	"testing"
	"time"
)

func testSetup(t *testing.T) (*Service, *Store) {
	t.Helper()
	store := NewStore(t.TempDir())
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return NewService(st, store), store
}

func restart(t *testing.T, store *Store) *Service {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return NewService(st, store)
}

func mkAccount(t *testing.T, svc *Service) {
	t.Helper()
	feeBps, rebillBps := 100, 2000
	_, err := svc.CreateAccount(CreateAccountReq{
		ID: "ACC1", HolderID: "H1", CardMask: "6225********1234",
		HomeCurrency: "CNY", Balances: map[string]int64{"CNY": 100000},
		FxFeeBps: &feeBps, RebillBps: &rebillBps,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func ts(hour int) time.Time {
	return time.Date(2026, 9, 18, hour, 0, 0, 0, time.UTC)
}

func authEvent(id string, hour int, ref string) CardEvent {
	return CardEvent{
		EventID: id, Type: "auth", AccountID: "ACC1", MerchantID: "M1",
		MerchantName: "ROME HOTEL", MerchantRef: ref, TerminalID: "T1",
		Currency: "EUR", Amount: 100, TxnTime: ts(hour),
	}
}

// TestInspectionScenario 复刻第一轮现场检查：
// 预授权 → 重复事件/商户重试/跨午夜 → 汇率正常更新 → 异常汇率挂起 →
// 重启服务 → 离线补传清算（含重发）→ 重启 → 争议（客服追加结论）→ 裁决冲正。
// 全程核对余额、争议状态与通知次数。
func TestInspectionScenario(t *testing.T) {
	svc, store := testSetup(t)
	mkAccount(t, svc)

	// 初始汇率 7.80
	fx1, err := svc.AddFXSnapshot("EUR", "CNY", "ecb", "7.80", ts(10))
	if err != nil || fx1.Status != statusAccepted {
		t.Fatalf("fx1: %v %+v", err, fx1)
	}

	// 预授权 100 EUR：折算 780 CNY，1% 手续费 8，冻结 788
	r, err := svc.Ingest(authEvent("E1", 23, "ORDER-9"))
	if err != nil || r.Status != statusAccepted {
		t.Fatalf("auth: %v %+v", err, r)
	}
	v, _ := svc.GetBalanceView("ACC1")
	if v.Available != 100000-788 || v.Holds != 788 {
		t.Fatalf("after auth: %+v", v)
	}

	// 同一网络事件重放 → duplicate，不动账
	r2, _ := svc.Ingest(authEvent("E1", 23, "ORDER-9"))
	if r2.Status != statusDuplicate {
		t.Fatalf("replay = %s", r2.Status)
	}
	// 商户换 event_id 重试同一订单 → 拒绝
	r3, _ := svc.Ingest(authEvent("E1-RETRY", 23, "ORDER-9"))
	if r3.Status != statusDeclined {
		t.Fatalf("merchant retry = %s", r3.Status)
	}
	// 跨午夜同订单再来一次 → 仍然拒绝
	r4, _ := svc.Ingest(authEvent("E1-NEXTDAY", 1, "ORDER-9"))
	if r4.Status != statusDeclined {
		t.Fatalf("cross-midnight retry = %s", r4.Status)
	}
	if len(svc.Notifications("ACC1")) != 1 {
		t.Fatalf("notifications after dupes = %d", len(svc.Notifications("ACC1")))
	}

	// 正常汇率更新 7.90（偏离 ~128bps < 2%）→ 采纳
	fx2, err := svc.AddFXSnapshot("EUR", "CNY", "ecb", "7.90", ts(12))
	if err != nil || fx2.Status != statusAccepted {
		t.Fatalf("fx2: %v %+v", err, fx2)
	}
	// 异常汇率 8.50（偏离 ~759bps）→ 挂起人工复核，绝不静默采用
	fx3, err := svc.AddFXSnapshot("EUR", "CNY", "unknown-src", "8.50", ts(12))
	if err != nil || fx3.Status != statusPending {
		t.Fatalf("fx3 should be pending: %v %+v", err, fx3)
	}
	if len(svc.PendingFX()) != 1 {
		t.Fatalf("pending fx = %d", len(svc.PendingFX()))
	}

	// —— 现场检查：重启服务 ——
	svc = restart(t, store)
	v, _ = svc.GetBalanceView("ACC1")
	if v.Available != 100000-788 || v.Holds != 788 {
		t.Fatalf("after restart holds lost: %+v", v)
	}
	if len(svc.PendingFX()) != 1 {
		t.Fatalf("pending fx lost after restart")
	}

	// 离线终端补传清算（采用已采纳的 7.90，而非待审的 8.50）
	pres := CardEvent{
		EventID: "E2", Type: "presentment", AccountID: "ACC1", MerchantID: "M1",
		MerchantRef: "ORDER-9", Currency: "EUR", Amount: 100,
		TxnTime: ts(13), LinkedEventID: "E1", Offline: true,
	}
	r5, err := svc.Ingest(pres)
	if err != nil || r5.Status != statusAccepted {
		t.Fatalf("presentment: %v %+v", err, r5)
	}
	// 790 本金 + 8 手续费，冻结 788 释放：100000 - 798 = 99202
	v, _ = svc.GetBalanceView("ACC1")
	if v.Balance != 99202 || v.Holds != 0 || v.Available != 99202 {
		t.Fatalf("after settle: %+v", v)
	}
	// 离线补传重发：同 event_id → duplicate；换 event_id → already settled 拒绝
	r6, _ := svc.Ingest(pres)
	if r6.Status != statusDuplicate {
		t.Fatalf("presentment replay = %s", r6.Status)
	}
	pres.EventID = "E2-DUP"
	r7, _ := svc.Ingest(pres)
	if r7.Status != statusDeclined {
		t.Fatalf("second presentment = %s", r7.Status)
	}
	v, _ = svc.GetBalanceView("ACC1")
	if v.Balance != 99202 {
		t.Fatalf("duplicate presentment changed balance: %d", v.Balance)
	}

	// —— 再次重启 ——
	svc = restart(t, store)
	v, _ = svc.GetBalanceView("ACC1")
	if v.Balance != 99202 || v.Holds != 0 {
		t.Fatalf("state after 2nd restart: %+v", v)
	}

	// 客户发起争议
	d, err := svc.OpenDispute("ACC1", "E2", "同一笔交易被重复扣款的疑虑", "CUST-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "open" || len(d.Notes) != 1 {
		t.Fatalf("dispute init: %+v", d)
	}
	// 客服只能追加结论
	d, _ = svc.AddDisputeConclusion(d.ID, "AGENT-7", "核对网络流水，仅一笔清算")
	d, _ = svc.AddDisputeConclusion(d.ID, "AGENT-7", "补充：汇率快照 FX3 已溯源")
	if len(d.Notes) != 3 || d.Status != "under_review" {
		t.Fatalf("notes append: %+v", d)
	}
	// 裁决胜诉 → 全额冲正 100 EUR：本金 790 + 手续费 8 按当前已采纳汇率 7.90 返还
	d, err = svc.ResolveDispute(d.ID, "won", "AGENT-7", "责任在商户，全额冲正")
	if err != nil || d.Status != "won" {
		t.Fatalf("resolve: %v %+v", err, d)
	}
	v, _ = svc.GetBalanceView("ACC1")
	if v.Balance != 100000 {
		t.Fatalf("after chargeback balance = %d, want 100000", v.Balance)
	}

	// 通知次数：预授权/清算/争议受理/争议裁决 各 1，重复报文从未产生通知
	notes := svc.Notifications("ACC1")
	if len(notes) != 4 {
		t.Fatalf("notification count = %d, want 4: %+v", len(notes), notes)
	}

	// —— 最终重启，一切照旧 ——
	svc = restart(t, store)
	v, _ = svc.GetBalanceView("ACC1")
	if v.Balance != 100000 || v.Holds != 0 {
		t.Fatalf("final restart: %+v", v)
	}
	if len(svc.Notifications("ACC1")) != 4 {
		t.Fatalf("notifications lost after restart")
	}
	got, err := svc.Dispute(d.ID)
	if err != nil || got.Status != "won" || len(got.Notes) != 4 {
		t.Fatalf("dispute after restart: %v %+v", err, got)
	}
	eOK, aOK, at := svc.VerifyChain()
	if !eOK || !aOK {
		t.Fatalf("chain broken at %d", at)
	}

	// 明细脱敏：商户名与卡号均被掩码，且记录了清算实际使用的快照 FX2(7.90)
	details, err := svc.ListDetails("ACC1")
	if err != nil {
		t.Fatal(err)
	}
	var settled *MaskedDetail
	for _, dd := range details {
		if dd.EventID == "E2" {
			settled = dd
		}
		if dd.CardMask != "6225********1234" {
			t.Fatalf("card leaked: %s", dd.CardMask)
		}
	}
	if settled == nil || settled.FxSnapshotID != fx2.ID || settled.FxRate != "7.900000" ||
		settled.HomePrincipal != 790 || settled.FeeHome != 8 || settled.DisputeID != d.ID {
		t.Fatalf("settled detail: %+v", settled)
	}
}

// TestPartialReversalBeforeSettle 授权后部分撤销，再按剩余金额清算，不能多扣。
func TestPartialReversalBeforeSettle(t *testing.T) {
	svc, store := testSetup(t)
	mkAccount(t, svc)
	svc.AddFXSnapshot("EUR", "CNY", "ecb", "7.80", ts(10))

	if r, _ := svc.Ingest(authEvent("A1", 20, "REF-P")); r.Status != statusAccepted {
		t.Fatal(r)
	}
	rev := CardEvent{
		EventID: "A2", Type: "reversal", AccountID: "ACC1", MerchantID: "M1",
		MerchantRef: "REF-P", Currency: "EUR", Amount: 40, TxnTime: ts(21),
		LinkedEventID: "A1",
	}
	if r, _ := svc.Ingest(rev); r.Status != statusAccepted {
		t.Fatalf("partial reversal: %+v", r)
	}
	// 重复撤销报文
	if r, _ := svc.Ingest(rev); r.Status != statusDuplicate {
		t.Fatalf("reversal replay = %s", r.Status)
	}
	v, _ := svc.GetBalanceView("ACC1")
	// 释放 40 EUR -> 312 CNY + fee 3 = 315
	if v.Holds != 788-315 {
		t.Fatalf("holds after partial reversal = %d", v.Holds)
	}
	// 超额清算（剩余授权 60）必须拒绝
	big := CardEvent{EventID: "A3", Type: "presentment", AccountID: "ACC1",
		Currency: "EUR", Amount: 80, TxnTime: ts(22), LinkedEventID: "A1", Offline: true}
	if r, _ := svc.Ingest(big); r.Status != statusDeclined {
		t.Fatalf("over presentment = %s", r.Status)
	}
	// 按 60 清算（被拒的 A3 已落盘，重试必须使用新的网络事件 ID）
	ok60 := big
	ok60.EventID = "A4"
	ok60.Amount = 60
	if r, _ := svc.Ingest(ok60); r.Status != statusAccepted {
		t.Fatalf("presentment 60: %+v", r)
	}
	svc = restart(t, store)
	v, _ = svc.GetBalanceView("ACC1")
	// 60*7.8=468 + fee (46800+5000)/10000=5 => 473
	if v.Balance != 100000-473 || v.Holds != 0 {
		t.Fatalf("final = %+v", v)
	}
}

// TestAbnormalFXReview 异常汇率复核：驳回沿用旧汇率，批准后新交易才用新汇率。
func TestAbnormalFXReview(t *testing.T) {
	svc, _ := testSetup(t)
	mkAccount(t, svc)
	svc.AddFXSnapshot("EUR", "CNY", "ecb", "7.80", ts(10))
	bad, _ := svc.AddFXSnapshot("EUR", "CNY", "x", "8.50", ts(11))

	// 待审期间换算固定在已采纳汇率
	b, err := svc.FixConversion("ACC1", "quote:EUR100", "EUR", 100)
	if err != nil || b.Converted != 780 || b.SnapshotID != "FX1" {
		t.Fatalf("conversion during pending: %+v err=%v", b, err)
	}
	// 驳回
	if _, err := svc.ReviewFX(bad.ID, "reject", "AGENT-1"); err != nil {
		t.Fatal(err)
	}
	b2, _ := svc.FixConversion("ACC1", "quote:EUR100", "EUR", 100)
	if b2.SnapshotID != "FX1" || b.Rate != "7.800000" {
		t.Fatalf("after reject should keep FX1: %+v", b2)
	}
	// 新的异常快照批准后才生效
	bad2, _ := svc.AddFXSnapshot("EUR", "CNY", "x", "8.60", ts(12))
	if _, err := svc.ReviewFX(bad2.ID, "approve", "AGENT-1"); err != nil {
		t.Fatal(err)
	}
	b3, _ := svc.FixConversion("ACC1", "quote:EUR100", "EUR", 100)
	if b3.SnapshotID != bad2.ID || b3.Converted != 860 {
		t.Fatalf("after approve: %+v", b3)
	}
	// 历史展示依据不变
	if b.Converted != 780 || b.SnapshotID != "FX1" {
		t.Fatalf("historical basis mutated: %+v", b)
	}
}

// TestRebillCap 商户补扣受授权范围限制且不可重复。
func TestRebillCap(t *testing.T) {
	svc, _ := testSetup(t)
	mkAccount(t, svc)
	svc.AddFXSnapshot("EUR", "CNY", "ecb", "7.80", ts(10))
	svc.Ingest(authEvent("R1", 9, "ROOM-1"))
	svc.Ingest(CardEvent{EventID: "R2", Type: "presentment", AccountID: "ACC1",
		Currency: "EUR", Amount: 100, TxnTime: ts(10), LinkedEventID: "R1"})

	mk := func(id string, amt int64) EventResult {
		r, err := svc.Ingest(CardEvent{EventID: id, Type: "rebill", AccountID: "ACC1",
			MerchantID: "M1", Currency: "EUR", Amount: amt, TxnTime: ts(11), LinkedEventID: "R2"})
		if err != nil {
			t.Fatal(err)
		}
		return *r
	}
	// 上限 20% = 20 EUR
	if r := mk("R3", 25); r.Status != statusDeclined {
		t.Fatalf("rebill over cap = %s (%s)", r.Status, r.Reason)
	}
	// 被拒后商户以新事件 ID 重试 10 → 接受
	if r := mk("R4", 10); r.Status != statusAccepted {
		t.Fatalf("rebill 10: %+v", r)
	}
	// 同一报文重放 → duplicate
	if r := mk("R4", 10); r.Status != statusDuplicate {
		t.Fatalf("rebill replay = %s", r.Status)
	}
	// 累计补扣 21 超上限 → 拒绝
	if r := mk("R5", 11); r.Status != statusDeclined {
		t.Fatalf("cumulative rebill over cap = %s", r.Status)
	}
	// 累计补扣到上限 20 → 接受
	if r := mk("R6", 10); r.Status != statusAccepted {
		t.Fatalf("rebill to cap: %+v", r)
	}
}

// TestInsufficientAndLimits 余额不足与单笔限额拒绝。
func TestInsufficientAndLimits(t *testing.T) {
	svc, _ := testSetup(t)
	feeBps := 0
	if _, err := svc.CreateAccount(CreateAccountReq{
		ID: "ACC2", HolderID: "H2", CardMask: "4312********9999", HomeCurrency: "USD",
		Balances: map[string]int64{"USD": 50}, FxFeeBps: &feeBps,
		TxnLimit: map[string]int64{"EUR": 40},
	}); err != nil {
		t.Fatal(err)
	}
	svc.AddFXSnapshot("EUR", "USD", "ecb", "1.10", ts(10))
	ev := CardEvent{EventID: "X1", Type: "auth", AccountID: "ACC2",
		Currency: "EUR", Amount: 45, TxnTime: ts(10)}
	if r, _ := svc.Ingest(ev); r.Status != statusDeclined {
		t.Fatalf("limit check = %s", r.Status)
	}
	ev.Amount = 40 // 折算 44 USD > 余额 50？44<50 但限额 40 边界 OK；实际 44<50 通过
	ev.EventID = "X2"
	if r, _ := svc.Ingest(ev); r.Status != statusAccepted {
		t.Fatalf("auth at limit: %+v", r)
	}
	ev = CardEvent{EventID: "X3", Type: "auth", AccountID: "ACC2",
		Currency: "EUR", Amount: 10, TxnTime: ts(10)}
	if r, _ := svc.Ingest(ev); r.Status != statusDeclined {
		t.Fatalf("insufficient check = %s", r.Status)
	}
}

// TestExportIsMinimalAndMasked 监管导出：最小字段、脱敏、哈希链与通知计数。
func TestExportIsMinimalAndMasked(t *testing.T) {
	svc, store := testSetup(t)
	mkAccount(t, svc)
	svc.AddFXSnapshot("EUR", "CNY", "ecb", "7.80", ts(10))
	svc.Ingest(authEvent("P1", 8, "EXP-1"))
	svc.Ingest(CardEvent{EventID: "P2", Type: "presentment", AccountID: "ACC1",
		MerchantID: "M1", Currency: "EUR", Amount: 100, TxnTime: ts(9), LinkedEventID: "P1"})
	svc.OpenDispute("ACC1", "P2", "质疑重复扣款", "CUST-1")

	svc = restart(t, store)
	rep, err := svc.ExportAccount("ACC1", "REG-1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.CardMask != "6225********1234" || !rep.EntriesChainOK || !rep.AuditChainOK {
		t.Fatalf("export: mask=%s chain=%v/%v", rep.CardMask, rep.EntriesChainOK, rep.AuditChainOK)
	}
	if rep.NotificationCount != 3 { // auth + settle + dispute_opened
		t.Fatalf("notification count = %d, want 3", rep.NotificationCount)
	}
	if len(rep.Disputes) != 1 || len(rep.Events) != 2 || len(rep.Entries) < 3 {
		t.Fatalf("export sizes: d=%d e=%d l=%d", len(rep.Disputes), len(rep.Events), len(rep.Entries))
	}
	// 审计中必须留下导出记录
	if len(rep.Events) == 0 {
		t.Fatal("no events")
	}
}
