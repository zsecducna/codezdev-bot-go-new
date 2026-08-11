package handlers

// adminorders_test.go — tests for the 🧾 ORDERS category + refund subsystem
// (adminorders.go). Money-critical: these tests exist to prove the maths and
// the double-refund guard, not just that a button answers.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/refund"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
)

// fileExists reports whether path names a regular, readable file.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// refundedAmountFor reads the raw refunded_amount column back for an order.
// There is no Order.RefundedAmount field yet (stage S3, the stats read side,
// hasn't landed), so tests reach for the column directly, the way the
// eventual stats query will. ok is false when the column is still NULL.
func refundedAmountFor(t *testing.T, st *store.Store, des string) (amount int64, ok bool) {
	t.Helper()
	var v sql.NullInt64
	if err := st.DB().QueryRow("SELECT refunded_amount FROM orders WHERE des=?", des).Scan(&v); err != nil {
		t.Fatalf("read refunded_amount(%s): %v", des, err)
	}
	return v.Int64, v.Valid
}

// newOrdersTestBot wires a Bot to real Store/Stock/RefundStore/Config
// instances rooted at a fresh temp dir — the refund flows need all four
// (money, folder-state, refund-request queue, settings/suspend map).
func newOrdersTestBot(t *testing.T, adminID string) (*Bot, *fakeTG, *store.Store, *stock.Store, *refund.Store, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	rs, err := refund.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("refund open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, RefundStore: rs, Config: cfg, AdminID: adminID})
	t.Cleanup(func() { b.Shutdown(); st.Close(); rs.Close() })
	return b, f, st, stk, rs, cfg, dir
}

// ============================================================================
// refundAmount — warranty-time proration
// ============================================================================

func TestRefundAmountDayTier(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	purchaseTS := float64(t0.Unix())

	cases := []struct {
		name       string
		elapsed    time.Duration
		wantUnit   int64
		wantAmount int64
	}{
		{"immediately after purchase", 0, 1, 6000},           // day 1 of 7, remaining 6 -> 7000*6/7=6000
		{"3 days later", 3 * 24 * time.Hour, 4, 3000},        // day 4, remaining 3 -> 3000
		{"fully elapsed (7 days)", 7 * 24 * time.Hour, 8, 0}, // day 8 of 7 -> remaining clamps to 0
		{"just under 1 day", 23 * time.Hour, 1, 6000},        // still day 1 (whole-day floor)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b.now = func() time.Time { return t0.Add(c.elapsed) }
			o := &store.Order{Des: "d1", Product: "prod", Tier: "BH7D", Total: 7000, TS: purchaseTS}
			amount, unitsTotal, unitNum, unit := b.refundAmount(o, nil)
			if unitsTotal != 7 || unit != "Day" {
				t.Fatalf("unitsTotal=%d unit=%q, want 7/Day", unitsTotal, unit)
			}
			if unitNum != c.wantUnit {
				t.Errorf("unitNum=%d, want %d", unitNum, c.wantUnit)
			}
			if amount != c.wantAmount {
				t.Errorf("amount=%d, want %d", amount, c.wantAmount)
			}
			if amount < 0 || amount > o.Total {
				t.Errorf("amount %d out of [0,%d] bounds", amount, o.Total)
			}
		})
	}
}

func TestRefundAmountHourTier(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return t0.Add(5 * time.Hour) }
	o := &store.Order{Des: "d1", Product: "prod", Tier: "BH12H", Total: 1200, TS: float64(t0.Unix())}
	amount, unitsTotal, unitNum, unit := b.refundAmount(o, nil)
	if unitsTotal != 12 || unit != "Hour" {
		t.Fatalf("unitsTotal=%d unit=%q, want 12/Hour", unitsTotal, unit)
	}
	if unitNum != 6 { // floor(5h/1h)+1 = 6
		t.Errorf("unitNum=%d, want 6", unitNum)
	}
	if amount != 600 { // (12-6)/12 * 1200 = 600
		t.Errorf("amount=%d, want 600", amount)
	}
}

func TestRefundAmountKBHAndUnknownTier(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	for _, tier := range []string{"KBH", "", "GARBAGE"} {
		o := &store.Order{Des: "d1", Product: "prod", Tier: tier, Total: 5000, TS: float64(time.Now().Unix())}
		amount, unitsTotal, unitNum, unit := b.refundAmount(o, nil)
		if amount != 0 || unitsTotal != 0 || unitNum != 0 || unit != "" {
			t.Errorf("tier %q: got (%d,%d,%d,%q), want all zero/empty (routes to force-full)",
				tier, amount, unitsTotal, unitNum, unit)
		}
	}
}

// TestRefundAmountSuspendClockStop proves the elapsed clock STOPS at the
// product's suspend timestamp — a suspended product's warranty must not keep
// ticking after it was pulled from sale, so review delay after suspension
// never shrinks the payout below what was fair at suspend time.
func TestRefundAmountSuspendClockStop(t *testing.T) {
	b, _, _, _, _, cfg, _ := newOrdersTestBot(t, "1")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	suspendAt := t0.Add(5 * 24 * time.Hour) // suspended after 5 days

	s := cfg.Settings()
	s["suspended"] = map[string]any{"prod": map[string]any{"ts": float64(suspendAt.Unix()), "by": "1"}}
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// "Now" is 20 days after purchase — long past suspension.
	b.now = func() time.Time { return t0.Add(20 * 24 * time.Hour) }
	o := &store.Order{Des: "d1", Product: "prod", Tier: "BH30D", Total: 30000, TS: float64(t0.Unix())}
	amount, unitsTotal, unitNum, _ := b.refundAmount(o, nil)
	if unitsTotal != 30 {
		t.Fatalf("unitsTotal=%d, want 30", unitsTotal)
	}
	// Clock capped at day 5 (suspend ts): unitNum = floor(5)+1 = 6, remaining = 24.
	if unitNum != 6 {
		t.Errorf("unitNum=%d, want 6 (clock should stop at suspend ts)", unitNum)
	}
	wantAmount := int64(30000 * 24 / 30) // 24000
	if amount != wantAmount {
		t.Errorf("amount=%d, want %d (suspend-ts clock stop not applied)", amount, wantAmount)
	}
}

// TestRefundAmountFrozenAsOf proves an explicit asOf snapshot wins over both
// "now" and the suspend ts when it is the earliest of the three — the
// customer-request-approval freeze (refappr:) must never let review delay
// shrink an already-approved payout.
func TestRefundAmountFrozenAsOf(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return t0.Add(20 * 24 * time.Hour) } // "now" is way later
	asOf := float64(t0.Add(2 * 24 * time.Hour).Unix())              // frozen at day 2

	o := &store.Order{Des: "d1", Product: "prod", Tier: "BH30D", Total: 30000, TS: float64(t0.Unix())}
	amount, unitsTotal, unitNum, _ := b.refundAmount(o, &asOf)
	if unitsTotal != 30 {
		t.Fatalf("unitsTotal=%d, want 30", unitsTotal)
	}
	if unitNum != 3 { // floor(2)+1 = 3
		t.Errorf("unitNum=%d, want 3 (frozen as_of not honored)", unitNum)
	}
	wantAmount := int64(30000 * 27 / 30) // remaining 27 -> 27000
	if amount != wantAmount {
		t.Errorf("amount=%d, want %d", amount, wantAmount)
	}
}

// ============================================================================
// doRefund — the atomic executor
// ============================================================================

// TestDoRefundDepositRefused proves the deposit guard lives in the executor,
// not only the UI: doRefund must refuse a deposit-product order even called
// directly, with NO balance change and NO claim.
func TestDoRefundDepositRefused(t *testing.T) {
	b, _, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", depositProduct, "", 1, 50000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)

	res := b.doRefund(context.Background(), o, "balance", false)
	if res != nil {
		t.Fatalf("doRefund on a deposit order returned %+v, want nil", res)
	}
	got, _ := st.GetOrder(des)
	if got.Status != "delivered" {
		t.Errorf("deposit order status changed to %q — refund guard bypassed", got.Status)
	}
	if bal, _ := st.GetBalance("42"); bal != 0 {
		t.Errorf("balance = %d, want 0 (deposit must never be double-credited via refund)", bal)
	}
}

// TestDoRefundNormalLeavesStockInSold proves the invariant "a normal refund
// NEVER touches stock": the delivered account file must stay in sold/, never
// move back to active/, while the order flips to refunded and the wallet is
// credited exactly once.
func TestDoRefundNormalLeavesStockInSold(t *testing.T) {
	b, _, st, _, _, _, dir := newOrdersTestBot(t, "1")
	const product = "prodx"
	seedProduct(t, dir, product, 7000, 1) // writes active/prodx-u0.txt + price_KBH_VND

	activeFile := filepath.Join(dir, "accounts", product, "active", product+"-u0.txt")
	soldDir := filepath.Join(dir, "accounts", product, "sold")
	if err := os.MkdirAll(soldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	soldFile := filepath.Join(soldDir, product+"-u0.txt")
	if err := os.Rename(activeFile, soldFile); err != nil {
		t.Fatal(err)
	}

	des := store.NewDes()
	if err := st.CreateOrder(des, "555", "en", product, "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)

	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) } // refund immediately after purchase
	res := b.doRefund(context.Background(), o, "balance", false)
	if res == nil {
		t.Fatal("doRefund returned nil, want a result")
	}
	if !fileExists(soldFile) {
		t.Error("account file no longer in sold/ — a normal refund must never touch stock")
	}
	if fileExists(activeFile) {
		t.Error("account file leaked back into active/ on a NORMAL (non-force) refund")
	}
	got, _ := st.GetOrder(des)
	if got.Status != "refunded" {
		t.Errorf("status=%q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance("555"); bal != res.Amount {
		t.Errorf("balance=%d, want %d (res.Amount)", bal, res.Amount)
	}

	// Double-refund guard: a second attempt on the same (now-refunded) row
	// must be refused, not double-pay.
	res2 := b.doRefund(context.Background(), o, "balance", false)
	if res2 != nil {
		t.Fatal("second doRefund call on an already-refunded order must return nil")
	}
	if bal, _ := st.GetBalance("555"); bal != res.Amount {
		t.Errorf("balance changed on second refund attempt: %d, want unchanged %d", bal, res.Amount)
	}
}

// ============================================================================
// refunded_amount — S2 bookkeeping (task #20)
// ============================================================================

// TestDoRefundRecordsRefundedAmount proves the write is CREDITED, not
// intended: a partial warranty refund (only some days remain) must record the
// same partial figure that was actually credited to the wallet, not the
// order's full total. Deleting the SetRefundedAmount call in doRefund's
// balance-success branch turns this test red (refunded_amount stays NULL).
func TestDoRefundRecordsRefundedAmount(t *testing.T) {
	b, _, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) } // refund immediately -> 6/7 days left, NOT the full total

	res := b.doRefund(context.Background(), o, "balance", false)
	if res == nil {
		t.Fatal("doRefund returned nil, want a result")
	}
	if res.Amount == o.Total {
		t.Fatalf("res.Amount = %d equals o.Total %d — this test needs a genuinely PARTIAL refund to be meaningful", res.Amount, o.Total)
	}
	got, ok := refundedAmountFor(t, st, des)
	if !ok || got != res.Amount {
		t.Errorf("refunded_amount = (valid=%v, %d), want (true, %d) — the actual CREDITED amount, not o.Total (%d)",
			ok, got, res.Amount, o.Total)
	}
}

// TestDoRefundCreditFailedRecordsZeroRefundedAmount proves the CreditFailed
// branch — where the claim committed 'refunded' but CreditBalance errored and
// ZERO money moved — records 0, not the would-be amount and not NULL. This is
// exactly the path a naive "write refunded_amount alongside the claim" would
// get wrong: the claim already committed by the time CreditBalance is even
// attempted. Deleting the SetRefundedAmount(des, 0) call turns this test red
// (refunded_amount stays NULL, which reads as "unknown" instead of the true
// "definitely credited zero").
func TestDoRefundCreditFailedRecordsZeroRefundedAmount(t *testing.T) {
	b, _, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) }

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	res := b.doRefund(context.Background(), o, "balance", false)
	if res == nil || !res.CreditFailed {
		t.Fatalf("res = %+v, want a claimed refund with CreditFailed=true", res)
	}
	got, ok := refundedAmountFor(t, st, des)
	if !ok || got != 0 {
		t.Errorf("refunded_amount = (valid=%v, %d), want (true, 0) — nothing was actually credited", ok, got)
	}
}

// TestDoRefundManualRecordsRefundedAmount proves the "manual" payout method
// (admin pays out off-platform, no CreditBalance call at all) still records
// refunded_amount — cash left the business, so the honest revenue figure is
// the computed amount, even though it can't be verified against a wallet
// transaction. Deleting the SetRefundedAmount call in doRefund's manual
// branch turns this test red.
func TestDoRefundManualRecordsRefundedAmount(t *testing.T) {
	b, _, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) }

	res := b.doRefund(context.Background(), o, "manual", false)
	if res == nil {
		t.Fatal("doRefund returned nil, want a result")
	}
	if bal, _ := st.GetBalance("42"); bal != 0 {
		t.Errorf("balance = %d, want 0 — manual payout must never call CreditBalance", bal)
	}
	got, ok := refundedAmountFor(t, st, des)
	if !ok || got != res.Amount {
		t.Errorf("refunded_amount = (valid=%v, %d), want (true, %d)", ok, got, res.Amount)
	}
}

// ============================================================================
// Refund-request state machine
// ============================================================================

// TestRefundApproveThenDoubleApproveLosesCleanly proves refund_requests'
// pending->approved transition is single-winner: approving twice must not
// re-notify, re-freeze, or error the second time — it must simply report
// "no longer pending" and change nothing further.
func TestRefundApproveThenDoubleApproveLosesCleanly(t *testing.T) {
	b, f, st, _, rs, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "77", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	id, err := rs.CreateRequest(des, "77", o.Product, o.Tier, o.Total, o.TS, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "refappr:"+itoa64(id)))
	// First approve must have flipped the request and produced a refund
	// method-picker screen (or a force-full escalation) — either way, some
	// message landed.
	if lastSendMsg(f) == nil {
		t.Fatal("first refappr: produced no reply")
	}
	req, err := rs.GetRequest(id)
	if err != nil || req == nil || req.Status != refund.StatusApproved {
		t.Fatalf("request not approved after first refappr:: %+v err=%v", req, err)
	}

	f.Sends = nil
	b.Process(ctx, callbackUpdate(1, "refappr:"+itoa64(id)))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "no longer pending") {
		t.Errorf("second refappr: should report 'no longer pending', got %+v", last)
	}
}

// TestRefundRejectPersistsReasonAndBlocksReplay proves reject records the
// reason and, once decided, a second reject/approve attempt on the same
// request id changes nothing further.
func TestRefundRejectPersistsReasonAndBlocksReplay(t *testing.T) {
	b, _, st, _, rs, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "88", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	id, err := rs.CreateRequest(des, "88", o.Product, o.Tier, o.Total, o.TS, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Arm the reject-reason prompt, then type the reason.
	b.Process(ctx, callbackUpdate(1, "refrej:"+itoa64(id)))
	b.Process(ctx, textUpdate(1, "fraud suspected"))

	req, err := rs.GetRequest(id)
	if err != nil || req == nil {
		t.Fatalf("GetRequest: %+v err=%v", req, err)
	}
	if req.Status != refund.StatusRejected {
		t.Fatalf("status=%q, want rejected", req.Status)
	}
	if req.RejectReason == nil || *req.RejectReason != "fraud suspected" {
		t.Errorf("reject reason = %v, want %q", req.RejectReason, "fraud suspected")
	}

	// A second reject attempt on the same, now-decided request must change
	// NOTHING — assert by re-arming and retyping, then checking the reason
	// is untouched.
	b.Process(ctx, callbackUpdate(1, "refrej:"+itoa64(id)))
	b.Process(ctx, textUpdate(1, "different reason"))
	req2, _ := rs.GetRequest(id)
	if req2.RejectReason == nil || *req2.RejectReason != "fraud suspected" {
		t.Errorf("reject reason changed on replay: %v, want unchanged %q", req2.RejectReason, "fraud suspected")
	}
}

// ============================================================================
// Capability gate — full-admin-only vs orders_view for a standard admin
// ============================================================================

// TestOrdersCapabilityGate proves the central admincap.go chokepoint denies a
// standard (granted, non-super) admin on the full-admin-only refund/delete
// callbacks while allowing the orders_view-capability ones through to a
// real reply.
func TestOrdersCapabilityGate(t *testing.T) {
	b, f, st, _, rs, _, _ := newOrdersTestBot(t, "1") // super admin is chat 1
	b.adminsMu.Lock()
	b.admins["7"] = true // chat 7: standard admin, default caps only
	b.adminsMu.Unlock()

	ctx := context.Background()

	t.Run("full-admin-only denied", func(t *testing.T) {
		for _, data := range []string{
			"adm:getord", "adm:refund", "refappr:1", "admrefdel",
			"refdelconf", "refdelone:x", "refdeloneok:x",
		} {
			f.Sends, f.Answers = nil, nil
			b.Process(ctx, callbackUpdate(7, data))
			wantAdminCapDenied(t, f, data)
		}
	})

	// orders_view is a GRANTABLE capability in Python (admincap.go's own
	// header), but Go has no per-admin grant UI yet — every standard admin is
	// fixed to defaultStdCaps (stock_read/hold_restore/audit/toggle_sales),
	// which deliberately does NOT include orders_view (it is OFF by default
	// until an owner grants it — admincap.go's own doc). So there is no way
	// to exercise "an actual standard admin WITH orders_view" end-to-end yet;
	// the correct thing to prove today is that admincap.go's OWN
	// classification table marks these callbacks as orders_view-gated (not
	// full-admin-only) — i.e. the day a grant UI lands, these unlock
	// correctly with zero changes needed here.
	t.Run("orders_view classification (admincap.go table)", func(t *testing.T) {
		for _, data := range []string{"adm:orders", "admord:x", "refrev:1", "refrej:1"} {
			cap, governed := stdAdminCbCap(data)
			if !governed || cap != "orders_view" {
				t.Errorf("%s: classified as (cap=%q, governed=%v), want (orders_view, true)", data, cap, governed)
			}
		}
		for _, cat := range []string{"orders", "refundreq"} {
			if need := catCaps[cat]; !need["orders_view"] {
				t.Errorf("catCaps[%q] = %v, want orders_view", cat, need)
			}
		}
	})

	// A full admin (super admin, chat 1) must reach every one of these —
	// both the full-admin-only ones AND the orders_view ones — since the
	// chokepoint always passes a super admin regardless of capability.
	t.Run("full admin reaches everything", func(t *testing.T) {
		des := store.NewDes()
		if err := st.CreateOrder(des, "999", "en", "prod", "BH7D", 1, 7000); err != nil {
			t.Fatal(err)
		}
		o, _ := st.GetOrder(des)
		id, err := rs.CreateRequest(des, "999", o.Product, o.Tier, o.Total, o.TS, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, data := range []string{"adm:orders", "admcat:orders", "admcat:refundreq", "refrev:" + itoa64(id), "adm:getord", "adm:refund"} {
			f.Sends, f.Answers = nil, nil
			b.Process(ctx, callbackUpdate(1, data))
			if lastSendMsg(f) == nil {
				t.Errorf("%s: super admin got no reply", data)
			}
		}
	})
}

// ============================================================================
// Revenue view — layout + truncation
// ============================================================================

func TestRevenueByDateTextBasic(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	m := &store.RevenueMatrix{
		Products: []string{"prodA", "prodB"},
		Rows: []store.RevenueDay{
			{Date: "2026-01-05", Cells: map[string]int64{"prodA": 5000, "prodB": 2000}, Total: 7000, Count: 2},
			{Date: "2026-01-04", Cells: map[string]int64{"prodA": 1000}, Total: 1000, Count: 1},
		},
		ProductTotals: map[string]int64{"prodA": 6000, "prodB": 2000},
		ProductCounts: map[string]int64{"prodA": 2, "prodB": 1},
		GrandTotal:    8000, GrandCount: 3,
	}
	text := b.revenueByDateText(m, "Last 7 days")
	for _, want := range []string{"01-05", "01-04", "prodA", "prodB", "Σ"} {
		if !strings.Contains(text, want) {
			t.Errorf("revenueByDateText missing %q:\n%s", want, text)
		}
	}
}

func TestRevenueByDateTextEmptyRange(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	m := &store.RevenueMatrix{}
	text := b.revenueByDateText(m, "Today")
	if !strings.Contains(text, "no delivered orders in this range") {
		t.Errorf("empty-range text = %q", text)
	}
}

// TestRevenueByDateTextTruncation proves the 3900-char budget drops OLDEST
// day-blocks first (they sort last, since Rows is most-recent-first) and
// always keeps the Σ footer.
func TestRevenueByDateTextTruncation(t *testing.T) {
	b, _, _, _, _, _, _ := newOrdersTestBot(t, "1")
	var rows []store.RevenueDay
	// 200 days of one product each — guaranteed to blow the 3900-char budget.
	for i := 0; i < 200; i++ {
		date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -i).Format("2006-01-02")
		rows = append(rows, store.RevenueDay{
			Date: date, Cells: map[string]int64{"prodA": 1000}, Total: 1000, Count: 1,
		})
	}
	m := &store.RevenueMatrix{
		Products: []string{"prodA"}, Rows: rows,
		ProductTotals: map[string]int64{"prodA": 200000}, ProductCounts: map[string]int64{"prodA": 200},
		GrandTotal: 200000, GrandCount: 200,
	}
	text := b.revenueByDateText(m, "All time")
	if len(text) > 4096 {
		t.Fatalf("revenue text is %d bytes, exceeds Telegram's 4096 hard cap", len(text))
	}
	if !strings.Contains(text, "older days not shown") {
		t.Error("truncated revenue text missing the 'older days not shown' marker")
	}
	if !strings.Contains(text, "Σ "+b.usdtDisp(200000)) {
		t.Error("truncated revenue text is missing its Σ footer")
	}
	// The MOST RECENT day (today, i=0) must still be present — only the
	// oldest days should have been dropped.
	mostRecent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format("01-02")
	if !strings.Contains(text, mostRecent) {
		t.Error("truncation dropped the most-recent day — must drop OLDEST first")
	}
}

// ============================================================================
// R1 — CreditBalance failure must never claim success
// ============================================================================

// TestDoRefundCreditBalanceFailureNeverClaimsSuccess proves the executor-level
// contract: when CreditBalance errors AFTER the claim already committed, the
// result is flagged CreditFailed, the customer DM is SKIPPED (never tell them
// it worked when the money never moved), and the order still legitimately
// flips to 'refunded' (the claim itself must not be rolled back — nothing
// will ever retry a 'refunded' order, which is exactly why the failure must
// be surfaced loudly elsewhere instead).
func TestDoRefundCreditBalanceFailureNeverClaimsSuccess(t *testing.T) {
	b, f, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) }

	// Simulate a genuine persist failure: CreditBalance's target table is
	// gone, while ClaimOrder (a different table) still works fine — the same
	// asymmetry a disk-full/SQLITE_BUSY error would produce.
	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	res := b.doRefund(context.Background(), o, "balance", false)
	if res == nil {
		t.Fatal("doRefund returned nil — the claim itself must still win even though the credit fails")
	}
	if !res.CreditFailed {
		t.Error("res.CreditFailed = false, want true")
	}
	if res.Notified {
		t.Error("res.Notified = true — the customer DM must be SKIPPED when the credit failed")
	}
	got, _ := st.GetOrder(des)
	if got.Status != "refunded" {
		t.Errorf("status=%q, want refunded (the claim committed even though the credit failed)", got.Status)
	}
	if lastSendMsgTo(f, 42) != nil {
		t.Error("buyer chat 42 received a DM despite the credit failing — must never claim success")
	}
}

// TestDoRefundCreditFailedNotifiesOwner covers adminorders.go:275 (#39 site
// 4): the owner-alert twin of TestDoRefundCreditBalanceFailureNeverClaimsSuccess
// above — CreditBalance errors AFTER the claim already committed, and no
// automatic path will ever retry a 'refunded' order. Before #39 the CRITICAL
// log line was the only trace. Deleting the notifyOwners call in doRefund
// must turn this test red.
func TestDoRefundCreditFailedNotifiesOwner(t *testing.T) {
	b, f, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) }

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	res := b.doRefund(context.Background(), o, "balance", false)
	if res == nil || !res.CreditFailed {
		t.Fatalf("res = %+v, want a claimed refund with CreditFailed=true", res)
	}
	alert := lastSendMsgTo(f, 1)
	if alert == nil || !strings.Contains(alert.Text, "REFUND CREDIT FAILED") {
		t.Fatalf("owner alert = %v, want a REFUND CREDIT FAILED message", alert)
	}
	if !strings.Contains(alert.Text, des) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
}

// TestSendRefundReceiptCreditFailedHeader proves the UI-level contract: the
// admin-facing receipt must show a LOUD failure header (never the quiet ✅
// "balance now 0"), and the buyer must receive nothing at all.
func TestSendRefundReceiptCreditFailedHeader(t *testing.T) {
	b, f, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	b.now = func() time.Time { return time.Unix(int64(o.TS), 0) }

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "refundm:"+des+":balance"))

	admMsg := lastSendMsgTo(f, 1)
	if admMsg == nil || !strings.Contains(admMsg.Text, "WALLET CREDIT FAILED") {
		t.Fatalf("admin receipt must show the loud credit-failed header; got %+v", admMsg)
	}
	if strings.Contains(admMsg.Text, "✅ Order REFUNDED") {
		t.Error("must not ALSO show the normal ✅ success header")
	}
	if lastSendMsgTo(f, 42) != nil {
		t.Error("buyer must receive NO DM when the credit failed")
	}
}

// ============================================================================
// R2 — peek, not pop: an intervening nav tap must not disarm a capture
// ============================================================================

// TestCapGetOrdPeekSurvivesNavTap proves adm:getord's pending flag survives a
// slash command typed before the order ID — the admin must still be able to
// complete the lookup on the next reply instead of the flow being silently
// destroyed.
func TestCapGetOrdPeekSurvivesNavTap(t *testing.T) {
	b, f, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prodx", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "adm:getord"))
	b.Process(ctx, textUpdate(1, "/start")) // accidental nav tap — must NOT disarm
	f.Sends = nil
	b.Process(ctx, textUpdate(1, des))
	// sendOrderDetail sends the detail screen AND (since this order was never
	// delivered) a trailing "no delivery file" notice, so the detail screen is
	// not necessarily the LAST message — check every send from this call.
	if !anySendContains(f, des) {
		t.Errorf("getord flag was disarmed by an intervening nav tap; sends=%+v", f.Sends)
	}
}

// TestCapRefundPeekSurvivesNavTap is TestCapGetOrdPeekSurvivesNavTap's twin
// for adm:refund.
func TestCapRefundPeekSurvivesNavTap(t *testing.T) {
	b, f, st, _, _, _, dir := newOrdersTestBot(t, "1")
	const product = "prody"
	seedProduct(t, dir, product, 7000, 1)
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", product, "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "adm:refund"))
	b.Process(ctx, textUpdate(1, "/start")) // accidental nav tap — must NOT disarm
	f.Sends = nil
	b.Process(ctx, textUpdate(1, des))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, des) {
		t.Errorf("refund flag was disarmed by an intervening nav tap; got %+v", last)
	}
}

// TestCapRefundRejectPeekSurvivesNavTap is the same proof for refrej:'s
// pending id.
func TestCapRefundRejectPeekSurvivesNavTap(t *testing.T) {
	b, _, st, _, rs, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "88", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)
	id, err := rs.CreateRequest(des, "88", o.Product, o.Tier, o.Total, o.TS, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "refrej:"+itoa64(id)))
	b.Process(ctx, textUpdate(1, "/start")) // accidental nav tap — must NOT disarm
	b.Process(ctx, textUpdate(1, "fraud suspected"))

	req, err := rs.GetRequest(id)
	if err != nil || req == nil {
		t.Fatalf("GetRequest: %+v err=%v", req, err)
	}
	if req.Status != refund.StatusRejected {
		t.Fatalf("status=%q, want rejected — refundreject flag was disarmed by the intervening nav tap", req.Status)
	}
	if req.RejectReason == nil || *req.RejectReason != "fraud suspected" {
		t.Errorf("reject reason = %v, want %q", req.RejectReason, "fraud suspected")
	}
}

// ============================================================================
// Force-full refund -> stock restore, and stale-screen re-validation
// ============================================================================

// TestForceRefundThenStockRestoreFlow drives the full force-full -> frstock:
// -> frstockyes: chain: the account stays in sold/ immediately after the
// force-refund (a normal refund NEVER auto-restores stock), and only moves
// back to active/ after the explicit DANGER-gated frstockyes: confirm.
func TestForceRefundThenStockRestoreFlow(t *testing.T) {
	b, f, st, stk, _, _, dir := newOrdersTestBot(t, "1")
	const product = "prodz"
	seedProduct(t, dir, product, 7000, 1)

	activeFile := filepath.Join(dir, "accounts", product, "active", product+"-u0.txt")
	soldFile := filepath.Join(dir, "accounts", product, "sold", product+"-u0.txt")

	des := store.NewDes()
	// Drive the REAL delivery path (Reserve -> Finalize) rather than hand-
	// moving the file: Finalize is what writes ./orders/<des>.txt, the record
	// cbForceRestore's b.stock.OrderUsernames(des) depends on to know which
	// account belongs to this order.
	if _, err := stk.Reserve(product, 1, des, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := stk.Finalize(product, des, "", "en"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOrder(des, "777", "en", product, "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "frefund:"+des))
	b.Process(ctx, callbackUpdate(1, "frefundc:"+des+":balance"))
	b.Process(ctx, callbackUpdate(1, "frefundm:"+des+":balance"))

	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "FORCE-REFUNDED") {
		t.Fatalf("force-full receipt missing; got %+v", last)
	}
	got, _ := st.GetOrder(des)
	if got.Status != "refunded" {
		t.Fatalf("status=%q, want refunded", got.Status)
	}
	if !fileExists(soldFile) {
		t.Fatal("account file should still be in sold/ immediately after force-refund (before restore)")
	}

	b.Process(ctx, callbackUpdate(1, "frstock:"+des))
	confirm := lastSendMsg(f)
	if confirm == nil || !strings.Contains(confirm.Text, "DANGER") {
		t.Fatalf("frstock: confirm missing DANGER warning; got %+v", confirm)
	}
	b.Process(ctx, callbackUpdate(1, "frstockyes:"+des))

	if fileExists(soldFile) {
		t.Error("account file still in sold/ after frstockyes: — restore did not move it")
	}
	if !fileExists(activeFile) {
		t.Error("account file not restored to active/ after frstockyes:")
	}
}

// TestRefundMethodStaleScreenRevalidation proves refundm:/frefundm: re-check
// the order's CURRENT status rather than trusting the tapped button: an order
// refunded out from under a stale screen must be refused, not double-paid.
func TestRefundMethodStaleScreenRevalidation(t *testing.T) {
	b, f, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	// Refund it out from under a hypothetical stale screen.
	if _, err := st.ClaimOrder(des, "delivered", "refunded"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Process(ctx, callbackUpdate(1, "refundm:"+des+":balance"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "already refunded") {
		t.Errorf("stale refundm: screen must re-validate and refuse; got %+v", last)
	}

	f.Sends = nil
	b.Process(ctx, callbackUpdate(1, "frefundm:"+des+":balance"))
	last2 := lastSendMsg(f)
	if last2 == nil || !strings.Contains(last2.Text, "already refunded") {
		t.Errorf("stale frefundm: screen must re-validate and refuse; got %+v", last2)
	}
}

// ============================================================================
// refundAsOf must NOT be consumed on a LOST claim
// ============================================================================

// TestRefundAsOfNotConsumedOnLostClaim proves dropRefundAsOf only ever fires
// on the WINNING claim: a frozen snapshot must survive a lost claim intact so
// a retry (or the next tap) still sees the approved-request freeze.
func TestRefundAsOfNotConsumedOnLostClaim(t *testing.T) {
	b, _, st, _, _, _, _ := newOrdersTestBot(t, "1")
	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(des)

	b.freezeRefundAsOf(des, 123456)
	// Simulate a LOST claim: someone else already flipped the order away
	// from the status doRefund is about to act on.
	if _, err := st.ClaimOrder(des, "delivered", "refunded"); err != nil {
		t.Fatal(err)
	}

	res := b.doRefund(context.Background(), o, "balance", false)
	if res != nil {
		t.Fatal("doRefund on a lost claim must return nil")
	}
	if v, ok := b.refundAsOf(des); !ok || v != 123456 {
		t.Errorf("refundAsOf(des) = (%v,%v), want (123456,true) — a LOST claim must NOT consume the frozen snapshot", v, ok)
	}
}

// ============================================================================
// entraRunRefundDelete — a not-clean run marks NOTHING
// ============================================================================

// writeEntraStub writes a POSIX shell-script stand-in for
// entra_create_users.py, matching the exact filename internal/entra's runner
// resolves — the same technique internal/entra's own tests use
// (writeStubScript, users_test.go), invoked here since entraRunRefundDelete
// resolves the script directory via KIRO_SCRIPT_DIR (live, at call time) and
// the interpreter via b.loginPythonStr() -> config.Config.KiroLoginPython(),
// which reads config.Load's frozen env snapshot — so KIRO_LOGIN_PYTHON must
// be set before the Bot is built, unlike KIRO_SCRIPT_DIR.
func writeEntraStub(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "entra_create_users.py")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestEntraRunRefundDeleteNotCleanMarksNothing proves a batch with ANY
// failed/protected email marks NO order processed — a re-run must stay safe
// rather than risk falsely recording a still-live account as deleted.
func TestEntraRunRefundDeleteNotCleanMarksNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	// KIRO_LOGIN_PYTHON must be set BEFORE newOrdersTestBot builds the Bot:
	// entraRunRefundDelete now resolves the interpreter through
	// b.loginPythonStr() -> config.Config.KiroLoginPython(), which reads
	// config.Load's frozen env snapshot (task #16), not a live os.Getenv —
	// same reason newUSDTBot sets its env before calling New(). KIRO_SCRIPT_DIR
	// is unaffected (scriptDir() still reads live ambient env at call time)
	// but is set here too so both interpreter and script directory come from
	// the one fixture setup.
	scriptDir := t.TempDir()
	writeEntraStub(t, scriptDir, `echo '{"ok":true,"targeted":2,"deleted":1,"purged":0,"not_found":0,"protected":0,"failed":1,"deleted_upns":["a@x.com"]}'`)
	t.Setenv("KIRO_SCRIPT_DIR", scriptDir)
	t.Setenv("KIRO_LOGIN_PYTHON", "sh")
	b, f, st, _, _, _, _ := newOrdersTestBot(t, "1")

	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prodx", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "refunded"); err != nil {
		t.Fatal(err)
	}

	b.entraRunRefundDelete(context.Background(), 1, []string{des}, []string{"a@x.com", "b@x.com"})

	got, _ := st.GetOrder(des)
	if got.EntraDeletedTS != nil {
		t.Error("order marked entra_deleted despite a not-clean run (failed>0) — must mark NOTHING")
	}
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "wasn't fully clean") {
		t.Errorf("expected the not-clean warning, got %+v", last)
	}
}

// TestEntraRunRefundDeleteUsesEnvOnlyLoginPython pins task #16's
// KIRO_LOGIN_PYTHON fix at this file's SoftDeleteByUPN call site: the
// interpreter must reach the subprocess from dir/.env alone, with
// KIRO_LOGIN_PYTHON never touching the ambient process environment
// (os.Setenv/t.Setenv is deliberately avoided here, unlike
// TestEntraRunRefundDeleteNotCleanMarksNothing above, which pins a
// different behaviour and is free to use the simpler t.Setenv fixture).
// config.Load(dir) is called directly here — not through newOrdersTestBot,
// which owns its own temp dir — so the .env write below happens BEFORE the
// Bot's cfg snapshot is built.
//
// The stub script is POSIX shell, not Python. If entraRunRefundDelete still
// resolved the interpreter via provision.LoginPythonFromEnv's raw
// os.Getenv (which finds nothing here — .env-only, no ambient var), it
// would fall back to "python3" and choke trying to parse the shell script
// as Python, surfacing as "Entra delete failed" instead of the clean-run
// success this test asserts.
func TestEntraRunRefundDeleteUsesEnvOnlyLoginPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	if _, ok := os.LookupEnv("KIRO_LOGIN_PYTHON"); ok {
		t.Fatal("fixture broken: KIRO_LOGIN_PYTHON is set in the ambient environment, so this test would pass even if the fix read raw env")
	}

	dir := t.TempDir()
	scriptDir := t.TempDir()
	writeEntraStub(t, scriptDir, `echo '{"ok":true,"targeted":1,"deleted":1,"purged":0,"not_found":0,"protected":0,"failed":0,"deleted_upns":["a@x.com"]}'`)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KIRO_LOGIN_PYTHON=sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// KIRO_SCRIPT_DIR is a separate, still-live os.Getenv read (scriptDir(),
	// otp.go) untouched by this fix — set it ambient, matching its own
	// resolution rule, not through .env.
	t.Setenv("KIRO_SCRIPT_DIR", scriptDir)

	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	rs, err := refund.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("refund open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, RefundStore: rs, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close(); rs.Close() })

	des := store.NewDes()
	if err := st.CreateOrder(des, "42", "en", "prodx", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "refunded"); err != nil {
		t.Fatal(err)
	}

	b.entraRunRefundDelete(context.Background(), 1, []string{des}, []string{"a@x.com"})

	got, _ := st.GetOrder(des)
	if got.EntraDeletedTS == nil {
		t.Fatalf("order not marked entra_deleted after a clean run — want the .env-only KIRO_LOGIN_PYTHON=sh to reach SoftDeleteByUPN; last message: %+v", lastSendMsg(f))
	}
}
