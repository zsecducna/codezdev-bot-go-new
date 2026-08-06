// Tests for the statistics-correctness work (.omc/artifacts/stats-fixes-spec.md
// steps 1-2): the refund_kind / list_total / code_discount column migration and
// the ClaimOrderKind invariant.
//
// Every test here was written BEFORE the implementation and confirmed failing
// against pre-change code — see the per-test "Falsification" notes.
package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// refundKindOf reads orders.refund_kind raw, so the assertions do not depend on
// scanOrder also being correct — a test that reuses the code under test to check
// the code under test can pass for the wrong reason.
func refundKindOf(t *testing.T, s *Store, des string) (string, bool) {
	t.Helper()
	var k sql.NullString
	if err := s.db.QueryRow("SELECT refund_kind FROM orders WHERE des=?", des).Scan(&k); err != nil {
		t.Fatalf("read refund_kind [des=%s]: %v", des, err)
	}
	return k.String, k.Valid
}

// strPtr is the test-local &"literal" helper.
func strPtr(s string) *string { return &s }

// seedOrder inserts a reserved order ready to be claimed onward.
func seedOrder(t *testing.T, s *Store, des, status string) {
	t.Helper()
	if err := s.CreateOrder(des, "7", "en", "kiro-pro", "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(%s): %v", des, err)
	}
	if status != "reserved" {
		if _, err := s.db.Exec("UPDATE orders SET status=? WHERE des=?", status, des); err != nil {
			t.Fatalf("seed status %s: %v", status, err)
		}
	}
}

// TestMigrationAddsStatsColumnsToOldSchema is spec T5. It builds a
// PRE-MIGRATION orders table by hand (same technique as TestMigrationOldSchema,
// store_test.go:119), seeds a legacy row, then boots the port over it and
// asserts the three statistics columns were added, that the legacy row reads
// NULL for all three (a backfill cannot invent history — spec §1.2), and that a
// second Open is a clean no-op.
//
// Falsification: against pre-change code this fails at the first
// "orders missing migrated column" — initSchema never added refund_kind,
// list_total or code_discount.
func TestMigrationAddsStatsColumnsToOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	// The OLD orders shape: none of the statistics columns.
	if _, err := raw.Exec("CREATE TABLE orders (" +
		"des TEXT PRIMARY KEY, chat_id TEXT, lang TEXT, product TEXT, " +
		"tier TEXT, qty INTEGER, total INTEGER, status TEXT, " +
		"paypal_order_id TEXT, ts REAL)"); err != nil {
		t.Fatalf("create old orders: %v", err)
	}
	// A legacy delivered row: the migration must preserve it untouched.
	if _, err := raw.Exec("INSERT INTO orders(des, chat_id, status, qty, total, ts) " +
		"VALUES ('legacy1','7','delivered',2,9000,1.0)"); err != nil {
		t.Fatalf("seed legacy order: %v", err)
	}
	// A legacy REFUNDED row — the case spec §1.2 is about: it cannot be
	// classified after the fact and must stay NULL, not be defaulted.
	if _, err := raw.Exec("INSERT INTO orders(des, chat_id, status, qty, total, ts) " +
		"VALUES ('legacy2','8','refunded',1,4000,2.0)"); err != nil {
		t.Fatalf("seed legacy refund: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on old schema: %v", err)
	}
	defer s.Close()

	cols := func(table string) map[string]bool {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		have, err := tableColumns(tx, table)
		if err != nil {
			t.Fatalf("tableColumns(%s): %v", table, err)
		}
		return have
	}
	for _, c := range []string{"refund_kind", "list_total", "code_discount"} {
		if !cols("orders")[c] {
			t.Errorf("orders missing migrated column %q", c)
		}
	}

	// The legacy rows survived, and their new columns are NULL — NOT 0, NOT a
	// default kind. NULL is the "predates this column" signal reports must
	// bucket separately (spec §1.2).
	for _, des := range []string{"legacy1", "legacy2"} {
		var rk sql.NullString
		var lt, cd sql.NullInt64
		if err := s.db.QueryRow(
			"SELECT refund_kind, list_total, code_discount FROM orders WHERE des=?",
			des).Scan(&rk, &lt, &cd); err != nil {
			t.Fatalf("read legacy row %s: %v", des, err)
		}
		if rk.Valid {
			t.Errorf("%s: refund_kind backfilled to %q — a backfill cannot invent history", des, rk.String)
		}
		if lt.Valid {
			t.Errorf("%s: list_total backfilled to %d — pre-discount price is unrecoverable", des, lt.Int64)
		}
		if cd.Valid {
			t.Errorf("%s: code_discount backfilled to %d — must stay NULL, not 0", des, cd.Int64)
		}
	}
	// The legacy row's real data is intact.
	var total int64
	if err := s.db.QueryRow("SELECT total FROM orders WHERE des='legacy1'").Scan(&total); err != nil {
		t.Fatalf("read legacy total: %v", err)
	}
	if total != 9000 {
		t.Errorf("legacy total = %d, want 9000 — migration corrupted existing data", total)
	}

	// Idempotence: re-opening must not error or duplicate a column.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotence): %v", err)
	}
	defer s2.Close()
}

// TestRefundKindClearedOnRevert is spec T2 — THE revert trap. Three handler
// sites do ClaimOrder(des, "refunded", "paid") (apikey_deliver.go:250,411 and
// purchase.go:343). If refund_kind survived that revert, a status='paid' row
// would keep a stale kind and every GROUP BY refund_kind report would count an
// order that is not refunded at all.
//
// Falsification: against pre-change code this fails to compile/run at
// ClaimOrderKind (undefined) — and the column it asserts on does not exist.
func TestRefundKindClearedOnRevert(t *testing.T) {
	s := newTestStore(t)
	seedOrder(t, s, "rev1", "paid")

	// Forward: paid -> refunded, classified as our own failed mint.
	won, err := s.ClaimOrderKind("rev1", "paid", "refunded", RefundKindMintFailed)
	if err != nil || won == nil {
		t.Fatalf("claim to refunded: won=%v err=%v", won != nil, err)
	}
	if k, ok := refundKindOf(t, s, "rev1"); !ok || k != RefundKindMintFailed {
		t.Fatalf("refund_kind after claim = %q (valid=%v), want %q", k, ok, RefundKindMintFailed)
	}

	// Revert via the UNCHANGED wrapper, exactly as the three handler sites call
	// it. The wrapper must clear the kind for free — that is the whole point of
	// making the invariant structural instead of a rule to remember.
	back, err := s.ClaimOrder("rev1", "refunded", "paid")
	if err != nil || back == nil {
		t.Fatalf("revert claim: back=%v err=%v", back != nil, err)
	}
	if back.Status != "paid" {
		t.Errorf("status after revert = %q, want paid", back.Status)
	}
	if k, ok := refundKindOf(t, s, "rev1"); ok {
		t.Errorf("refund_kind = %q after revert to 'paid' — INVARIANT BROKEN: "+
			"non-NULL kind on a non-refunded row poisons every GROUP BY refund_kind report", k)
	}
	// The scanned Order must agree with the raw column.
	if back.RefundKind != nil {
		t.Errorf("Order.RefundKind = %q after revert, want nil", *back.RefundKind)
	}
}

// TestClaimOrderKindRoundTripsEveryKind is spec T3b. It drives all five §2.1
// kinds and asserts each round-trips its own value, so a kind can never be
// silently folded into another bucket.
//
// SCOPE LIMIT, deliberate — and the name says only what the test can prove.
// The real refund call sites (SEVEN of them, see ClaimOrderKind's doc comment)
// live in internal/handlers, which this change does not touch. So this pins the
// STORE-side contract only. The last sub-test below pins that the plain
// wrapper degrades a refunded transition into the UNCLASSIFIED bucket rather
// than a real refund, which is still true today for any caller that reaches
// for the wrapper by mistake. An earlier name for this test
// ("TestEveryRefundedWriteSetsAKind") overclaimed the handler-side leg below.
//
// The handler-side leg — that no handler call site reaches 'refunded' without
// a kind — landed in spec step 3 as
// internal/handlers.TestNoBareClaimOrderToRefunded, a structural scan of every
// non-test .go file in internal/handlers for the invariant-breaking shape.
//
// Falsification: against pre-change code, ClaimOrderKind and the RefundKind*
// constants are undefined.
func TestClaimOrderKindRoundTripsEveryKind(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name string
		from string
		kind string
		// real: does this count toward the CUSTOMER-FACING refund rate? Pinned
		// per-case because getting this wrong is the original bug — a stockout
		// or a rescued top-up counted as a customer refund.
		real bool
	}{
		{"customer refund of a delivered sale", "delivered", RefundKindCustomer, true},
		{"admin cancel of a paid order", "paid", RefundKindAdminCancel, true},
		{"our API-key mint failed", "paid", RefundKindMintFailed, false},
		{"paid but folder stock was gone", "paid", RefundKindOutOfStock, false},
		{"late payment rescued to wallet", "expired", RefundKindLateSettled, false},
	}
	for _, c := range cases {
		if got := IsRealRefund(c.kind); got != c.real {
			t.Errorf("IsRealRefund(%q) = %v, want %v — %s", c.kind, got, c.real, c.name)
		}
	}
	// An unknown kind is never a real refund.
	if IsRealRefund("") || IsRealRefund("custmoer") {
		t.Error("IsRealRefund accepted an empty or unknown kind as a real refund")
	}
	// Nil-safety: report code iterates Orders whose RefundKind is legitimately
	// nil (unclassified). The pointer form must answer false, not panic — a
	// panic here would take down a money-reporting handler on the first
	// unclassified row, and today EVERY refunded row is unclassified.
	if IsRealRefundOrder(nil) {
		t.Error("IsRealRefundOrder(nil) = true")
	}
	if IsRealRefundOrder(&Order{Status: "refunded"}) {
		t.Error("IsRealRefundOrder counted a nil-kind (unclassified) row as a real refund")
	}
	if !IsRealRefundOrder(&Order{Status: "refunded", RefundKind: strPtr(RefundKindCustomer)}) {
		t.Error("IsRealRefundOrder missed a genuine customer refund")
	}
	for i, c := range cases {
		des := "kind" + string(rune('a'+i))
		seedOrder(t, s, des, c.from)
		won, err := s.ClaimOrderKind(des, c.from, "refunded", c.kind)
		if err != nil || won == nil {
			t.Fatalf("%s: claim failed won=%v err=%v", c.name, won != nil, err)
		}
		k, ok := refundKindOf(t, s, des)
		if !ok {
			t.Errorf("%s: refund_kind is NULL — this row would sit in the "+
				"unclassified bucket forever", c.name)
			continue
		}
		if k != c.kind {
			t.Errorf("%s: refund_kind = %q, want %q", c.name, k, c.kind)
		}
		if won.RefundKind == nil || *won.RefundKind != c.kind {
			t.Errorf("%s: returned Order.RefundKind mismatch", c.name)
		}
	}

	// An unknown kind must be REJECTED, not written. A typo silently creating a
	// sixth bucket is the same class of defect as the original overload.
	seedOrder(t, s, "bogus", "paid")
	if _, err := s.ClaimOrderKind("bogus", "paid", "refunded", "custmoer"); err == nil {
		t.Error("ClaimOrderKind accepted an unknown refund kind — typos must not " +
			"invent new buckets")
	}
	if _, ok := refundKindOf(t, s, "bogus"); ok {
		t.Error("rejected kind was written anyway")
	}
	var st string
	if err := s.db.QueryRow("SELECT status FROM orders WHERE des='bogus'").Scan(&st); err != nil {
		t.Fatalf("read bogus status: %v", err)
	}
	if st != "paid" {
		t.Errorf("status = %q after a rejected claim, want paid — a rejected kind "+
			"must not move the order either", st)
	}

	// Pre-step-3 behaviour, pinned so it is a known state and not a surprise:
	// the plain wrapper into 'refunded' yields a NULL kind, which reports must
	// treat as UNCLASSIFIED — never as a real refund (spec §1.2, §2.2).
	seedOrder(t, s, "legacypath", "paid")
	if _, err := s.ClaimOrder("legacypath", "paid", "refunded"); err != nil {
		t.Fatalf("wrapper claim to refunded: %v", err)
	}
	if _, ok := refundKindOf(t, s, "legacypath"); ok {
		t.Error("wrapper wrote a kind it was not given")
	}
	var unclassified int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM orders WHERE status='refunded' AND refund_kind IS NULL").
		Scan(&unclassified); err != nil {
		t.Fatalf("count unclassified: %v", err)
	}
	if unclassified != 1 {
		t.Errorf("unclassified refunded rows = %d, want 1", unclassified)
	}
	// And it must NOT be counted among the real refunds.
	var realRefunds int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM orders WHERE status='refunded' AND "+
			"refund_kind IN (?,?)", RefundKindCustomer, RefundKindAdminCancel).
		Scan(&realRefunds); err != nil {
		t.Fatalf("count real refunds: %v", err)
	}
	// Derived from the case table, not hard-coded, so adding a case later gives
	// a clear failure instead of a confusing off-by-N.
	wantReal := 0
	for _, c := range cases {
		if c.real {
			wantReal++
		}
	}
	if realRefunds != wantReal {
		t.Errorf("real refunds = %d, want %d (customer + admin_cancel only) — "+
			"an unclassified or operational row leaked into the refund rate",
			realRefunds, wantReal)
	}
}

// seedOrderAt inserts an order with an EXPLICIT status and ts, so age
// assertions are deterministic (CreateOrder stamps now()).
func seedOrderAt(t *testing.T, s *Store, des, product, status string, ts float64) {
	t.Helper()
	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?,?)",
		des, "7", "en", product, "T1", 1, 5000, status, ts); err != nil {
		t.Fatalf("seed %s: %v", des, err)
	}
}

// seedOrderChatAt is seedOrderAt with an explicit chat_id — needed whenever a
// test discriminates by COUNT(DISTINCT chat_id) (seedOrderAt always writes
// chat_id "7", indistinguishable across rows for that purpose).
func seedOrderChatAt(t *testing.T, s *Store, des, chatID, product, status string, ts float64) {
	t.Helper()
	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?,?)",
		des, chatID, "en", product, "T1", 1, 5000, status, ts); err != nil {
		t.Fatalf("seed %s: %v", des, err)
	}
}

// TestRevenueSummaryCustomersExcludesDepositOnly is spec T1 — the A1 fix, the
// highest-ranked audit finding. Customers/DeliveredCount/DeliveredRevenue
// (misc.go's GetRevenueSummary) either had NO deposit-product filter at all
// (Customers) or spelled it as a bare `product!=?` (DeliveredCount,
// TodayCount shares the identical query shape and fix, so is not duplicated
// here).
//
// THE DISCRIMINATION TRAP this test is built to avoid: a bare `product!=?`
// and a COALESCE-first filter agree on every row with a non-NULL
// product — `CreateOrder` (and this file's own seedOrder/seedOrderAt/
// seedOrderChatAt helpers) always write a non-NULL product, so a test that
// only seeds via those helpers cannot tell a present-but-wrong filter apart
// from a correct one (this is the exact failure mode
// TestRefundCountersSeparateKinds documents having already bitten this repo:
// "an earlier version of this test [stayed] green" after the deposit filter
// was stripped). This test therefore seeds one delivered, non-deposit row
// with product IS NULL via a raw INSERT that OMITS the product column
// (CreateOrder cannot produce this state through the public API), and asserts
// it is INCLUDED — proving COALESCE-first, not merely "some filter exists".
//
// Falsification, verified by TWO INDEPENDENT mutations against this exact
// test (executed and confirmed failing, then reverted — see the executor
// report): (1) reverting Customers to its pre-fix no-filter-at-all query
// makes it read 6, not 3 (every chat_id including the three deposit-only
// ones and the dual case). (2) reverting ONLY DeliveredCount/
// DeliveredRevenue's COALESCE-first filter back to a bare `product!=?`
// (Customers' fix left intact) makes `NULL != x` evaluate to NULL, not TRUE,
// in SQLite, so the NULL-product row silently drops out of BOTH the count
// and the sum — DeliveredCount reads 2, not 3, and DeliveredRevenue is short
// by that row's total (10000, not 17000). Each mutation is caught by a
// DIFFERENT assertion below, which is what proves the test discriminates the
// two fixes rather than passing on either one alone.
func TestRevenueSummaryCustomersExcludesDepositOnly(t *testing.T) {
	s := newTestStore(t)

	// Two real buyers.
	seedOrderChatAt(t, s, "buyerA", "A", "kiro-pro", "delivered", 100)
	seedOrderChatAt(t, s, "buyerB", "B", "kiro-pro", "delivered", 200)
	// Three wallet-only chats — deposited, never bought a product.
	seedOrderChatAt(t, s, "depC", "C", DepositProduct, "delivered", 300)
	seedOrderChatAt(t, s, "depD", "D", DepositProduct, "delivered", 400)
	seedOrderChatAt(t, s, "depE", "E", DepositProduct, "delivered", 500)
	// The dual case: chat A ALSO topped up. COUNT(DISTINCT chat_id) must not
	// count them twice, and this row must not count as a second "customer".
	seedOrderChatAt(t, s, "buyerA_dep", "A", DepositProduct, "delivered", 600)
	// The discriminating row: a real, non-deposit, delivered sale with product
	// IS NULL — a state CreateOrder cannot reach, constructed via raw SQL.
	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		"buyerF_nullprod", "F", "en", "T1", 1, 7000, "delivered", 700); err != nil {
		t.Fatalf("seed NULL-product row: %v", err)
	}

	r, err := s.GetRevenueSummary("")
	if err != nil {
		t.Fatalf("GetRevenueSummary: %v", err)
	}
	if r.Customers != 3 {
		t.Errorf("Customers = %d, want 3 (A, B, F — deposit-only chats C/D/E "+
			"excluded, chat A's dual deposit not double-counted)", r.Customers)
	}
	if r.DeliveredCount != 3 {
		t.Errorf("DeliveredCount = %d, want 3 — a bare `product!=?` would silently "+
			"drop the NULL-product row (buyerF_nullprod), undercounting", r.DeliveredCount)
	}
	const wantRevenue = 5000 + 5000 + 7000 // buyerA + buyerB + buyerF_nullprod
	if r.DeliveredRevenue != wantRevenue {
		t.Errorf("DeliveredRevenue = %d, want %d — the NULL-product row's total "+
			"must be included in the SUM, not just the COUNT", r.DeliveredRevenue, wantRevenue)
	}
}

// TestGetRevenueMatrixIncludesNullProductRow is the review-round coverage gap
// for the GetRevenueMatrix half of the A1-class NULL-product trap
// (stats-fixes-spec.md §5.1): a bare `product!=?` would silently DROP a
// delivered row whose product IS NULL, the same defect
// TestRevenueSummaryCustomersExcludesDepositOnly proves for
// GetRevenueSummary — but GetRevenueMatrix is a SEPARATE query in a separate
// function, so fixing one does not fix the other, and nothing previously
// exercised this one. Uncaught, this would make the /revenue matrix's
// GrandTotal/GrandCount disagree with the dashboard's
// DeliveredRevenue/DeliveredCount on the exact same range — the precise
// inconsistency this fix's own comment (misc.go, GetRevenueMatrix) cites as
// its justification.
//
// Seeds one ordinary delivered sale plus one delivered, non-deposit row with
// product IS NULL via a raw INSERT that omits the column (CreateOrder cannot
// reach this state), and asserts BOTH are folded into GrandTotal/GrandCount.
//
// Falsification: reverting GetRevenueMatrix's COALESCE-first filter back to
// a bare product!=? makes the NULL-product row's 7000 silently vanish from
// GrandTotal (reads 5000, not 12000) and GrandCount reads 1, not 2 — verified
// by mutation (see the executor report).
func TestGetRevenueMatrixIncludesNullProductRow(t *testing.T) {
	s := newTestStore(t)
	seedOrderAt(t, s, "prod1", "kiro-pro", "delivered", 1000) // total=5000, seedOrderAt's fixed amount

	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		"nullprod1", "9", "en", "T1", 1, 7000, "delivered", 2000); err != nil {
		t.Fatalf("seed NULL-product row: %v", err)
	}

	m, err := s.GetRevenueMatrix("", nil, 60)
	if err != nil {
		t.Fatalf("GetRevenueMatrix: %v", err)
	}
	const wantTotal = 5000 + 7000
	if m.GrandTotal != wantTotal {
		t.Errorf("GrandTotal = %d, want %d — the NULL-product row must be included, "+
			"matching GetRevenueSummary's DeliveredRevenue on the same range",
			m.GrandTotal, wantTotal)
	}
	if m.GrandCount != 2 {
		t.Errorf("GrandCount = %d, want 2", m.GrandCount)
	}
}

// TestRevenueSummaryRefundedRevenueTracksGenuineRefundsOnly is the store half
// of A7 (stats-fixes-spec.md §5.5): the dashboard's old single "Total" line
// was delivered-only, so a refund on an order from an earlier, already-closed
// period silently shrank it with nothing on screen explaining why. This adds
// RefundedRevenue — the money numerator for Gross = Net + Refunded — riding
// the SAME query as RefundedCount, and this test's job is proving it sums
// ONLY genuine customer refunds (customer + admin_cancel), never the
// operational-failure kinds (mint_failed, out_of_stock): those are OUR
// fulfilment failing, not money a customer asked back, and folding them in
// would make an outage or a stockout look like a bigger customer-refund
// number than it is (spec §2.2 — the same rule RefundedCount already
// enforces for the count; this test pins it for the SUM too, since a sum and
// a count over the same WHERE clause can drift independently if one query is
// hand-edited and the other isn't).
//
// Uses four DIFFERENT totals (2000/3000/1000/1500) specifically so a query
// that sums the wrong subset produces a wrong TOTAL, not just a wrong COUNT —
// a bug that swapped RefundedRevenue for, say, DeliveredRevenue's value would
// still coincidentally pass a same-total test.
//
// Falsification: against pre-change code, RevenueSummary has no
// RefundedRevenue field at all (compile failure) — the same "field does not
// exist" falsification TestRefundCountersSeparateKinds documents for its own
// new fields. Verified further by mutation (see the executor report): summing
// over ALL refund_kind values instead of IN (RefundKindCustomer,
// RefundKindAdminCancel) inflates RefundedRevenue from 5000 to 7500, which
// the exact-total assertion below catches.
func TestRevenueSummaryRefundedRevenueTracksGenuineRefundsOnly(t *testing.T) {
	s := newTestStore(t)

	mk := func(des, from string, total int64) {
		t.Helper()
		if err := s.CreateOrder(des, "7", "en", "kiro-pro", "T1", 1, total); err != nil {
			t.Fatalf("CreateOrder(%s): %v", des, err)
		}
		if from != "reserved" {
			if _, err := s.db.Exec("UPDATE orders SET status=? WHERE des=?", from, des); err != nil {
				t.Fatalf("seed %s -> %s: %v", des, from, err)
			}
		}
	}

	// One still-delivered sale — DeliveredRevenue/DeliveredCount (the Net line,
	// which A7 must leave UNCHANGED).
	mk("delivered1", "delivered", 5000)

	// Two GENUINE refunds of DIFFERENT totals.
	mk("refcust", "delivered", 2000)
	if _, err := s.ClaimOrderKind("refcust", "delivered", "refunded", RefundKindCustomer); err != nil {
		t.Fatalf("claim refcust: %v", err)
	}
	mk("refadmin", "paid", 3000)
	if _, err := s.ClaimOrderKind("refadmin", "paid", "refunded", RefundKindAdminCancel); err != nil {
		t.Fatalf("claim refadmin: %v", err)
	}

	// Two OPERATIONAL refunds — our own failures, must NOT count toward
	// RefundedRevenue.
	mk("refmint", "paid", 1000)
	if _, err := s.ClaimOrderKind("refmint", "paid", "refunded", RefundKindMintFailed); err != nil {
		t.Fatalf("claim refmint: %v", err)
	}
	mk("refoos", "paid", 1500)
	if _, err := s.ClaimOrderKind("refoos", "paid", "refunded", RefundKindOutOfStock); err != nil {
		t.Fatalf("claim refoos: %v", err)
	}

	r, err := s.GetRevenueSummary("")
	if err != nil {
		t.Fatalf("GetRevenueSummary: %v", err)
	}
	if r.RefundedCount != 2 {
		t.Errorf("RefundedCount = %d, want 2", r.RefundedCount)
	}
	const wantRefundedRevenue = 2000 + 3000
	if r.RefundedRevenue != wantRefundedRevenue {
		t.Errorf("RefundedRevenue = %d, want %d — must sum ONLY customer+admin_cancel "+
			"refunds, excluding mint_failed/out_of_stock operational failures",
			r.RefundedRevenue, wantRefundedRevenue)
	}
	if r.DeliveredRevenue != 5000 {
		t.Errorf("DeliveredRevenue (the new Net line) = %d, want 5000 — must be "+
			"UNCHANGED by this fix, since Net is exactly the OLD Total", r.DeliveredRevenue)
	}
	gross := r.DeliveredRevenue + r.RefundedRevenue
	const wantGross = 5000 + 2000 + 3000
	if gross != wantGross {
		t.Errorf("Gross (DeliveredRevenue+RefundedRevenue) = %d, want %d — this is a NEW "+
			"number higher than any 'Total' an admin has seen before, by design",
			gross, wantGross)
	}
}

// TestPaidUndeliveredExcludesDepositsAndReserved is the A4 store half: the
// count of orders where MONEY IS IN AND GOODS ARE NOT OUT, plus the age of the
// oldest so the alarm can say "oldest 3h ago" instead of a bare "2".
//
// The deposit trap is the point of this test. A __deposit__ order sitting in
// 'paid' is not a customer waiting on goods, so it must not inflate the alarm —
// that is A1's exact bug (a missing `AND product!=?`) reappearing inside A4's
// fix. The seeded deposit is deliberately the OLDEST row in the table, so a
// query that filters the COUNT but forgets the filter on MIN(ts) still reports
// a correct count with a WRONG age. Both halves are asserted separately.
//
// Falsification: against pre-change code PaidUndelivered is undefined. With the
// deposit filter dropped from either clause, the count or the age assertion
// below fails (verified by mutation).
func TestPaidUndeliveredExcludesDepositsAndReserved(t *testing.T) {
	s := newTestStore(t)

	// The oldest row in the whole table is a PAID DEPOSIT — the trap.
	seedOrderAt(t, s, "dep_old", DepositProduct, "paid", 1000)
	seedOrderAt(t, s, "dep_new", DepositProduct, "paid", 5000)

	// Real customer harm: paid, goods not delivered. Oldest of these is 3000.
	seedOrderAt(t, s, "paid_oldest", "kiro-pro", "paid", 3000)
	seedOrderAt(t, s, "paid_mid", "kiro-pro", "paid", 4000)
	seedOrderAt(t, s, "paid_new", "kiro-lite", "paid", 6000)

	// Must NOT count: idle carts, completed sales, refunds, expiries.
	seedOrderAt(t, s, "res1", "kiro-pro", "reserved", 2000)
	seedOrderAt(t, s, "res2", "kiro-pro", "reserved", 500) // older than everything
	seedOrderAt(t, s, "done1", "kiro-pro", "delivered", 2500)
	seedOrderAt(t, s, "ref1", "kiro-pro", "refunded", 2600)
	seedOrderAt(t, s, "exp1", "kiro-pro", "expired", 2700)

	count, oldestTS, err := s.PaidUndelivered("")
	if err != nil {
		t.Fatalf("PaidUndelivered: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3 (the paid non-deposit orders only) — "+
			"a deposit or a reserved cart leaked into the paid-not-delivered alarm", count)
	}
	if oldestTS != 3000 {
		t.Errorf("oldestTS = %v, want 3000 — got the paid DEPOSIT's ts (1000) if "+
			"the deposit filter is missing from MIN(ts), which would overstate the "+
			"age of the oldest customer actually waiting", oldestTS)
	}

	// Empty case: no paid orders at all must be a clean zero, not an error and
	// not a bogus age. The handler suppresses the alarm entirely on count==0,
	// so this is the healthy-shop path.
	s2 := newTestStore(t)
	seedOrderAt(t, s2, "onlyres", "kiro-pro", "reserved", 100)
	seedOrderAt(t, s2, "onlydep", DepositProduct, "paid", 200)
	count, oldestTS, err = s2.PaidUndelivered("")
	if err != nil {
		t.Fatalf("PaidUndelivered (empty): %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d on a shop with no paid product orders, want 0", count)
	}
	if oldestTS != 0 {
		t.Errorf("oldestTS = %v with no rows, want 0", oldestTS)
	}
}

// TestRevenueSummarySplitsReservedFromPaid pins the two new summary fields
// that the dashboard render (spec §5.3, admin.go's adminDashboardText)
// consumes. 'reserved' is an idle cart that expires by itself; 'paid' is
// money taken with nothing delivered. Summing them into one "Pending" number
// is what hides the second inside the first.
//
// The legacy combined PendingCount field is GONE: admin.go's dashboard render
// migrated off it (adminDashboardText now reads AwaitingPaymentCount /
// PaidUndeliveredCount only), which was the exact condition this test's
// former "PendingCount is deliberately LEFT INTACT" comment named as the
// trigger for deleting it (misc.go's own field doc said the same). Deleting
// a field nothing reads anymore, once its replacement has landed and been
// verified, completes A4 rather than broadening it.
//
// Falsification: against pre-change code the two split fields do not exist.
func TestRevenueSummarySplitsReservedFromPaid(t *testing.T) {
	s := newTestStore(t)
	seedOrderAt(t, s, "r1", "kiro-pro", "reserved", 1000)
	seedOrderAt(t, s, "r2", "kiro-pro", "reserved", 1100)
	seedOrderAt(t, s, "p1", "kiro-pro", "paid", 1200)
	seedOrderAt(t, s, "dep_res", DepositProduct, "reserved", 900)
	seedOrderAt(t, s, "dep_paid", DepositProduct, "paid", 950)

	r, err := s.GetRevenueSummary("")
	if err != nil {
		t.Fatalf("GetRevenueSummary: %v", err)
	}
	if r.AwaitingPaymentCount != 2 {
		t.Errorf("AwaitingPaymentCount = %d, want 2 (deposits excluded)", r.AwaitingPaymentCount)
	}
	if r.PaidUndeliveredCount != 1 {
		t.Errorf("PaidUndeliveredCount = %d, want 1 (deposits excluded)", r.PaidUndeliveredCount)
	}
	if r.PaidUndeliveredOldestTS != 1200 {
		t.Errorf("PaidUndeliveredOldestTS = %v, want 1200", r.PaidUndeliveredOldestTS)
	}
}

// TestRefundCountersSeparateKinds is spec T3 — the A2 counter split
// (GetRevenueSummary, stats-fixes-spec.md §5.2). Seeds one order of each
// real/operational refund_kind, two abandoned checkouts, and a __deposit__
// order for every same-shaped case, then asserts each new field counts ONLY
// its own NON-DEPOSIT bucket.
//
// The deposit rows are load-bearing, not decorative (review finding on this
// slice, proven by mutation): stripping the deposit-filter clause —
//
//	COALESCE(product,'')!=?
//
// — from all five misc.go queries left an earlier version of this test
// green, because the only deposit row it seeded used late_settled — a kind
// every query already excludes on the kind predicate alone, so the deposit
// filter was never actually exercised. Seeding a deposit row PER KIND
// (customer, mint_failed, out_of_stock, NULL, expired) means removing the
// filter now inflates a counter this test checks, for every bucket.
//
// (The SQL fragment above is deliberately an INDENTED doc-comment line, not
// inline prose: gofmt's doc-comment formatter silently turns two adjacent
// single-quote characters into one typographic close-quote when they appear
// in flowing text — verified reproducible against the stock Go toolchain,
// not a project quirk — which would corrupt this exact SQL fragment (an
// empty-string literal) on the next `gofmt -w` of this file. An indented
// line renders as a preformatted code block and is exempt.)
//
// Falsification: against pre-change code these fields do not exist. Verified
// by mutation: (1) reverting the RefundedCount query to the old
// `status IN ('refunded','expired')` (no kind filter, no deposit filter)
// makes RefundedCount count far more than 2; (2) stripping just the deposit
// filter from all five queries (kind/NULL/expired predicates kept) inflates
// RefundedCount/MintFailedCount/OutOfStockCount/UnclassifiedCount/
// ExpiredCount each by 1, from the matching deposit row.
func TestRefundCountersSeparateKinds(t *testing.T) {
	s := newTestStore(t)

	// One order of each real/operational kind — non-deposit product (seedOrder
	// always uses "kiro-pro").
	cases := []struct {
		des  string
		from string
		kind string
	}{
		{"cust1", "delivered", RefundKindCustomer},
		{"admcxl1", "paid", RefundKindAdminCancel},
		{"mint1", "paid", RefundKindMintFailed},
		{"oos1", "paid", RefundKindOutOfStock},
	}
	for _, c := range cases {
		seedOrder(t, s, c.des, c.from)
		if _, err := s.ClaimOrderKind(c.des, c.from, "refunded", c.kind); err != nil {
			t.Fatalf("claim %s: %v", c.des, err)
		}
	}
	// Two abandoned checkouts — never money, never refunded.
	seedOrder(t, s, "exp1", "expired")
	seedOrder(t, s, "exp2", "expired")

	// A __deposit__ order rescued to the wallet after the checkout window
	// closed — the exact A2 defect: a SUCCESS wearing status='refunded'. Must
	// not inflate RefundedCount, MintFailedCount, OutOfStockCount or
	// ExpiredCount.
	if err := s.CreateOrder("latedep1", "7", "en", DepositProduct, "", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(latedep1): %v", err)
	}
	if _, err := s.db.Exec("UPDATE orders SET status='expired' WHERE des=?", "latedep1"); err != nil {
		t.Fatalf("seed latedep1 -> expired: %v", err)
	}
	if _, err := s.ClaimOrderKind("latedep1", "expired", "refunded", RefundKindLateSettled); err != nil {
		t.Fatalf("claim latedep1: %v", err)
	}

	// One MORE deposit row per bucket below, each carrying the SAME kind (or
	// NULL / expired) as a non-deposit row above — the deposit filter's whole
	// job is keeping every one of these out of the count its kind would
	// otherwise land in.
	seedDeposit := func(des, from, kind string) {
		t.Helper()
		if err := s.CreateOrder(des, "7", "en", DepositProduct, "", 1, 5000); err != nil {
			t.Fatalf("CreateOrder(%s): %v", des, err)
		}
		if from != "reserved" {
			if _, err := s.db.Exec("UPDATE orders SET status=? WHERE des=?", from, des); err != nil {
				t.Fatalf("seed %s -> %s: %v", des, from, err)
			}
		}
		if _, err := s.ClaimOrderKind(des, from, "refunded", kind); err != nil {
			t.Fatalf("claim %s: %v", des, err)
		}
	}
	seedDeposit("depcust1", "delivered", RefundKindCustomer) // would-be RefundedCount leak
	seedDeposit("depmint1", "paid", RefundKindMintFailed)    // would-be MintFailedCount leak
	seedDeposit("depoos1", "paid", RefundKindOutOfStock)     // would-be OutOfStockCount leak

	// A deposit refunded with NO kind (the shape a plain-wrapper or
	// pre-migration row has) — would-be UnclassifiedCount leak.
	if err := s.CreateOrder("depnull1", "7", "en", DepositProduct, "", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(depnull1): %v", err)
	}
	if _, err := s.db.Exec("UPDATE orders SET status='refunded' WHERE des=?", "depnull1"); err != nil {
		t.Fatalf("seed depnull1 -> refunded (NULL kind): %v", err)
	}

	// A deposit sitting 'expired' — would-be ExpiredCount leak.
	if err := s.CreateOrder("depexp1", "7", "en", DepositProduct, "", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(depexp1): %v", err)
	}
	if _, err := s.db.Exec("UPDATE orders SET status='expired' WHERE des=?", "depexp1"); err != nil {
		t.Fatalf("seed depexp1 -> expired: %v", err)
	}

	r, err := s.GetRevenueSummary("")
	if err != nil {
		t.Fatalf("GetRevenueSummary: %v", err)
	}
	if r.RefundedCount != 2 {
		t.Errorf("RefundedCount = %d, want 2 (customer + admin_cancel only) — "+
			"an operational or late_settled row leaked into the customer refund count", r.RefundedCount)
	}
	if r.MintFailedCount != 1 {
		t.Errorf("MintFailedCount = %d, want 1", r.MintFailedCount)
	}
	if r.OutOfStockCount != 1 {
		t.Errorf("OutOfStockCount = %d, want 1", r.OutOfStockCount)
	}
	if r.UnclassifiedCount != 0 {
		t.Errorf("UnclassifiedCount = %d, want 0 — every row here was classified", r.UnclassifiedCount)
	}
	if r.ExpiredCount != 2 {
		t.Errorf("ExpiredCount = %d, want 2 — the rescued deposit is status='refunded', "+
			"not 'expired', and must not be counted here either", r.ExpiredCount)
	}
}

// TestClaimOrderKindRejectsMisuse covers the two guard branches and the
// self-transition hole the review found. Without these the branches could be
// deleted and every other test would stay green.
//
// Falsification: against pre-change code ClaimOrderKind does not exist; against
// a build with the guards removed, each sub-case below fails.
func TestClaimOrderKindRejectsMisuse(t *testing.T) {
	s := newTestStore(t)

	// A kind supplied with a NON-refunded target is a caller that has confused
	// itself — reject, and do not move the order.
	seedOrder(t, s, "mis1", "paid")
	if _, err := s.ClaimOrderKind("mis1", "paid", "delivered", RefundKindCustomer); err == nil {
		t.Error("accepted a refund kind for a 'delivered' transition")
	}
	var st string
	if err := s.db.QueryRow("SELECT status FROM orders WHERE des='mis1'").Scan(&st); err != nil {
		t.Fatalf("read mis1: %v", err)
	}
	if st != "paid" {
		t.Errorf("mis1 status = %q after rejected claim, want paid", st)
	}

	// Self-transition: refunded -> refunded would match the WHERE, report
	// RowsAffected()==1 and NULL out a kind on a row that is still legitimately
	// refunded. Must be rejected, and the existing kind must survive.
	seedOrder(t, s, "mis2", "paid")
	if _, err := s.ClaimOrderKind("mis2", "paid", "refunded", RefundKindCustomer); err != nil {
		t.Fatalf("setup claim: %v", err)
	}
	if _, err := s.ClaimOrder("mis2", "refunded", "refunded"); err == nil {
		t.Error("accepted a refunded -> refunded self-transition, which would " +
			"strip the kind off a still-refunded row")
	}
	if k, ok := refundKindOf(t, s, "mis2"); !ok || k != RefundKindCustomer {
		t.Errorf("refund_kind = %q (valid=%v) after rejected self-transition, want %q",
			k, ok, RefundKindCustomer)
	}

	// A LOSING claim (the double-refund guard) must return nil AND leave an
	// existing kind untouched — this is the path that protects real money.
	won, err := s.ClaimOrderKind("mis2", "paid", "refunded", RefundKindMintFailed)
	if err != nil {
		t.Fatalf("losing claim errored: %v", err)
	}
	if won != nil {
		t.Error("a claim whose fromStatus no longer matches reported a WIN — " +
			"the double-refund guard is broken")
	}
	if k, _ := refundKindOf(t, s, "mis2"); k != RefundKindCustomer {
		t.Errorf("losing claim overwrote refund_kind to %q, want %q untouched",
			k, RefundKindCustomer)
	}
}
