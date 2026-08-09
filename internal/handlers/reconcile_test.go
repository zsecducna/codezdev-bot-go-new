package handlers

// reconcile_test.go — pins the REPORT-ONLY stuck-order reconcile pass
// (reconcile.go). The money-safety invariant under test: reconcile NEVER
// delivers, NEVER refunds, NEVER mutates order status, NEVER credits a
// balance. T3 is the load-bearing pin for that invariant.
//
// Fixture: newBuyBot wires a REAL *store.Store + *stock.Store + *config.Config
// + fakeTG (buy_test.go:23), with t.Cleanup(b.Shutdown) already draining the
// worker registry. The seed helpers seedOrderAt/seedOrderChatAt
// (store/stats_test.go:331/344) insert rows with an explicit status+ts so age
// assertions are deterministic (CreateOrder stamps now()).

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/store"
)

// captureLog redirects the standard logger's output to a buffer for the
// duration of fn, then restores the original output. Lets a test assert on
// exactly what (if anything) a call logged, instead of only inferring
// silence from the absence of some OTHER side effect (a DB write, a DM) —
// the read-only tables/status snapshot in TestReconcileReportMutatesNothing
// proves reconcile never WRITES, but says nothing about whether it LOGS, and
// the two are independent claims.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

// seedOrderAtBot inserts an order with an explicit status+ts into b's store.
// A thin wrapper over the handlers package's access to the store handle so
// tests do not reach for unexported store.db directly. Mirrors
// store/stats_test.go:331 seedOrderAt.
func seedOrderAtBot(t *testing.T, b *Bot, des, product, status string, ts float64) {
	t.Helper()
	if _, err := b.store.DB().Exec(
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?,?)",
		des, "7", "en", product, "T1", 1, 5000, status, ts); err != nil {
		t.Fatalf("seed %s: %v", des, err)
	}
}

// tableRowCount returns a table's row count for the money-safety snapshot
// (T3). Used to prove reconcile touched NONE of orders/balances/api_keys/
// refund_* tables. Reads via the store's own handle.
func tableRowCount(t *testing.T, b *Bot, table string) int64 {
	t.Helper()
	var n int64
	if err := b.store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// orderStatusOf reads one order's status — the mutation sentinel for T3.
// Named orderStatusOf to avoid clashing with the production helper
// orderStatus(*store.Order) in usdt.go:1110.
func orderStatusOf(t *testing.T, b *Bot, des string) string {
	t.Helper()
	var s string
	if err := b.store.DB().QueryRow("SELECT status FROM orders WHERE des=?", des).Scan(&s); err != nil {
		t.Fatalf("status %s: %v", des, err)
	}
	return s
}

// TestReconcileReportCountsPaidUndelivered is T1: the count is exactly the
// paid non-deposit product orders, with reserved/delivered/refunded/expired
// excluded by the status='paid' filter (misc.go:560-578). Falsification: drop
// the status filter from PaidUndelivered and this count jumps.
func TestReconcileReportCountsPaidUndelivered(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()
	_ = ctx

	seedOrderAtBot(t, b, "paid1", "kiro-pro", "paid", 3000)
	seedOrderAtBot(t, b, "paid2", "kiro-pro", "paid", 4000)
	// Must NOT count: idle cart, completed sale, refund, expiry.
	seedOrderAtBot(t, b, "res1", "kiro-pro", "reserved", 2000)
	seedOrderAtBot(t, b, "done1", "kiro-pro", "delivered", 2500)
	seedOrderAtBot(t, b, "ref1", "kiro-pro", "refunded", 2600)
	seedOrderAtBot(t, b, "exp1", "kiro-pro", "expired", 2700)

	count, _, err := b.reconcileReport()
	if err != nil {
		t.Fatalf("reconcileReport: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (the two paid non-deposit product orders) — "+
			"a reserved/delivered/refunded/expired row leaked through the status filter", count)
	}
}

// TestReconcileReportExcludesDeposits is T2: a stalled deposit top-up must
// NOT inflate the goods alarm — the deposit-product filter in the SQL
// (COALESCE(product, empty) != DepositProduct at misc.go:569) drops it. The
// deposit is deliberately the OLDEST row so a query that filters COUNT but
// forgets MIN(ts) still reports the right count with a WRONG age — both
// halves asserted.
func TestReconcileReportExcludesDeposits(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")

	// Oldest in the table, but a deposit — must not win the MIN(ts) age.
	seedOrderAtBot(t, b, "dep_old", store.DepositProduct, "paid", 1000)
	seedOrderAtBot(t, b, "paid1", "kiro-pro", "paid", 3000)

	count, oldestTS, err := b.reconcileReport()
	if err != nil {
		t.Fatalf("reconcileReport: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 — a __deposit__ order leaked into the "+
			"paid-not-delivered goods alarm (the A1 trap reappearing inside A4)", count)
	}
	if oldestTS != 3000 {
		t.Errorf("oldestTS = %v, want 3000 — got the paid DEPOSIT's ts (1000) if "+
			"the deposit filter is missing from MIN(ts), overstating the age of the "+
			"oldest customer actually waiting", oldestTS)
	}
}

// TestReconcileReportMutatesNothing is T3 — the money-safety pin. Snapshots
// row counts of orders/balances/api_keys/refund_requests/audit AND the seeded
// order's status BEFORE the pass; runs reconcileStuckOrdersOnce; asserts ALL
// are unchanged after. Also asserts fakeTG.sendCount()==0, proving no DM was
// sent (the report is log-only — there is no _notify_owners channel today).
//
// Falsification: add a spurious b.store.ClaimOrder(...) (or any write) inside
// reconcileStuckOrdersOnce and the status/count assertion fails.
func TestReconcileReportMutatesNothing(t *testing.T) {
	b, f, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()

	// The refund_requests table lives in store.db but is created by the
	// separate refund.Store (refund.go:85-104), which newBuyBot does not
	// open. Create it directly so the snapshot covers it — reconcile must
	// prove it touches no refund row, so the table must exist to count.
	if _, err := b.store.DB().Exec(
		"CREATE TABLE IF NOT EXISTS refund_requests (id INTEGER PRIMARY KEY AUTOINCREMENT)"); err != nil {
		t.Fatalf("create refund_requests: %v", err)
	}

	seedOrderAtBot(t, b, "paid1", "kiro-pro", "paid", 3000)
	seedOrderAtBot(t, b, "paid2", "kiro-lite", "paid", 4000)

	// Snapshot every table reconcile must leave untouched + the order status.
	ordersBefore := tableRowCount(t, b, "orders")
	balancesBefore := tableRowCount(t, b, "balances")
	apiKeysBefore := tableRowCount(t, b, "api_keys")
	refundsBefore := tableRowCount(t, b, "refund_requests")
	auditBefore := tableRowCount(t, b, "audit")
	statusBefore := orderStatusOf(t, b, "paid1")

	before := f.sendCount()

	b.reconcileStuckOrdersOnce(ctx)

	after := f.sendCount()
	if after != before {
		t.Errorf("fakeTG sendCount grew %d -> %d across reconcile — reconcile must "+
			"never DM (it is log-only; _notify_owners is a deferred M3.1 gap)",
			before, after)
	}
	if got := tableRowCount(t, b, "orders"); got != ordersBefore {
		t.Errorf("orders row count changed %d -> %d — reconcile must be read-only", ordersBefore, got)
	}
	if got := tableRowCount(t, b, "balances"); got != balancesBefore {
		t.Errorf("balances row count changed %d -> %d — reconcile must never credit", balancesBefore, got)
	}
	if got := tableRowCount(t, b, "api_keys"); got != apiKeysBefore {
		t.Errorf("api_keys row count changed %d -> %d — reconcile must never mint/deliver", apiKeysBefore, got)
	}
	if got := tableRowCount(t, b, "refund_requests"); got != refundsBefore {
		t.Errorf("refund_requests row count changed %d -> %d — reconcile must never refund", refundsBefore, got)
	}
	if got := tableRowCount(t, b, "audit"); got != auditBefore {
		t.Errorf("audit row count changed %d -> %d — reconcile must never write", auditBefore, got)
	}
	if got := orderStatusOf(t, b, "paid1"); got != statusBefore {
		t.Errorf("order status mutated %q -> %q — reconcile must NEVER mutate order status", statusBefore, got)
	}
}

// TestReconcileReportZeroIsQuiet is T4: with no paid product orders,
// reconcileReport returns count==0, which is the precondition for the
// silence in reconcileStuckOrdersOnce (count==0 -> early return, no OWNER
// ALERT). The store-level empty case is pinned in stats_test.go; this pins
// the handler's read of it.
//
// AUDIT FIX: the original version of this test stopped at reconcileReport()
// and never called reconcileStuckOrdersOnce, so it could not see whether the
// `if count == 0 { return }` guard (reconcile.go:71-76) still existed —
// deleting that guard left this test green while a healthy shop would log
// "OWNER ALERT: 0 PAID-BUT-UNDELIVERED order(s)" on every 10-minute tick,
// exactly the noise the guard exists to avoid. This version drives the
// actual guard via reconcileStuckOrdersOnce and asserts NOTHING was logged.
//
// Falsification: deleting the `if count == 0 { return }` guard makes
// captureLog's output non-empty (an OWNER ALERT line appears), and the
// t.Errorf below fires.
func TestReconcileReportZeroIsQuiet(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()

	// A reserved cart and a deposit must not count — both are non-alarm rows.
	seedOrderAtBot(t, b, "res1", "kiro-pro", "reserved", 100)
	seedOrderAtBot(t, b, "dep1", store.DepositProduct, "paid", 200)

	count, oldestTS, err := b.reconcileReport()
	if err != nil {
		t.Fatalf("reconcileReport: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d on a shop with no paid product orders, want 0 "+
			"(the silence precondition — count>0 would log an OWNER ALERT)", count)
	}
	if oldestTS != 0 {
		t.Errorf("oldestTS = %v with no qualifying rows, want 0", oldestTS)
	}

	out := captureLog(t, func() { b.reconcileStuckOrdersOnce(ctx) })
	if out != "" {
		t.Errorf("reconcileStuckOrdersOnce logged %q at count==0, want nothing "+
			"— the quiet-at-zero guard did not fire", out)
	}
}

// TestReconcileStuckOrdersOnceLogsOwnerAlertWhenStuck is the counterpart to
// TestReconcileReportZeroIsQuiet: with a genuine paid-but-undelivered order,
// reconcileStuckOrdersOnce must actually WRITE the OWNER ALERT line, not
// just refrain from writing when there is nothing to report.
//
// Before this test, that claim was only ever inferred from
// TestReconcileReportMutatesNothing's absence-of-DB-writes snapshot, which
// proves reconcile never WRITES to the database but says nothing about
// whether it logs anything at all — a reconcileStuckOrdersOnce that dropped
// the log.Printf call entirely would still pass every other test in this
// file. This test observes the log line directly.
//
// Falsification: deleting or gutting the log.Printf("OWNER ALERT: ...") call
// in reconcileStuckOrdersOnce makes captureLog's output empty (or missing
// the substrings below), and the assertions fire.
func TestReconcileStuckOrdersOnceLogsOwnerAlertWhenStuck(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()

	seedOrderAtBot(t, b, "paid1", "kiro-pro", "paid", 3000)

	out := captureLog(t, func() { b.reconcileStuckOrdersOnce(ctx) })
	if !strings.Contains(out, "OWNER ALERT") {
		t.Errorf("reconcileStuckOrdersOnce logged %q, want it to contain "+
			"\"OWNER ALERT\" — a stuck paid order must surface, not stay silent", out)
	}
	if !strings.Contains(out, "1 PAID-BUT-UNDELIVERED") {
		t.Errorf("reconcileStuckOrdersOnce logged %q, want the count (1) in the "+
			"alert line", out)
	}
}

// TestStartReconcileStuckOrdersDrainsOnShutdown is T5: with the interval
// shrunk to 1ms, StartReconcileStuckOrders actually ticks and Shutdown
// drains it without hanging. The deterministic half asserts the once-function
// sees a seeded paid order after a tick; the shutdown half asserts the
// registry's WaitGroup returns (Shutdown does not hang) — mirroring the
// existing drain assertions in the suite. Kept fast: no wall-clock sleep on
// the goroutine's behalf (the once-function is asserted directly).
func TestStartReconcileStuckOrdersDrainsOnShutdown(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()

	seedOrderAtBot(t, b, "paid1", "kiro-pro", "paid", float64(time.Now().Unix()))

	// Shrink the interval so the ticker fires promptly. The once-function
	// itself is asserted directly below (the goroutine is a thin wrapper),
	// so the test does not race the tick — it confirms the worker can be
	// started and then cleanly drained.
	b.reconcileInterval = time.Millisecond
	b.StartReconcileStuckOrders(ctx)

	// Give the worker one tick to run a pass. The pass is read-only, so
	// running it via the goroutine or directly makes no observable
	// difference; assert the direct call sees the seeded order to prove the
	// body the goroutine runs is correct.
	time.Sleep(20 * time.Millisecond)
	count, _, err := b.reconcileReport()
	if err != nil {
		t.Fatalf("reconcileReport: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (the seeded paid order) — proves the body "+
			"the ticker runs actually sees stuck orders", count)
	}

	// Shutdown must drain the goroutine without hanging. The workerRegistry
	// closes its stop chan; the loop's select returns on <-stop. If the loop
	// ignored stop this blocks forever and the test times out — exactly the
	// failure mode this pins.
	done := make(chan struct{})
	go func() {
		b.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		// drained cleanly
	case <-time.After(5 * time.Second):
		t.Fatal("bot.Shutdown did not drain the reconcile worker within 5s — " +
			"the ticker loop is not honoring the registry stop chan")
	}
}
