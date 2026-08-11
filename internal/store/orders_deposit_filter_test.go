// Coverage for #21: the three bare `product!=?` filters in orders.go
// (ReservedOrderForChat, HasDeliveredOrder, DeliveredProductSummary) fixed to
// the COALESCE-first form, matching misc.go's five statistics queries
// (stats_test.go's TestRevenueSummaryCustomersExcludesDepositOnly is the
// model this file follows). The exact fragment:
//
//	COALESCE(product,'')!=?
//
// THE DISCRIMINATION TRAP each test here is built to avoid: a bare
// `product!=?` and a COALESCE-first filter agree on every row with a
// non-NULL product — CreateOrder always writes one, so a test that only
// seeds through CreateOrder cannot tell a present-but-wrong filter apart from
// a correct one. Each test below therefore seeds its discriminating row via a
// raw INSERT that OMITS the product column (a state CreateOrder cannot
// produce through the public API), because `NULL != x` evaluates to NULL,
// not TRUE, in SQLite: a bare filter silently DROPS that row out of the WHERE
// clause entirely (it DEFLATES, never inflates), while the COALESCE turns the
// NULL into the empty string, which correctly compares != DepositProduct.
package store

import (
	"testing"
)

// TestReservedOrderForChatIncludesNullProduct is the discriminating fixture
// for orders.go's ReservedOrderForChat. Seeds ONLY a reserved, non-deposit
// order whose product IS NULL, then asserts it is still found.
//
// Falsification: reverting the fix back to a bare `product!=?` makes the
// WHERE clause drop the sole seeded row entirely (`NULL != DepositProduct`
// is NULL, not TRUE), so the query returns sql.ErrNoRows and the function
// returns (nil, nil) instead of the order — verified by mutation (temporarily
// reverting the fragment and confirming this test fails, then restoring it).
func TestReservedOrderForChatIncludesNullProduct(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		"nullres1", "77", "en", "T1", 1, 5000, "reserved", 100); err != nil {
		t.Fatalf("seed NULL-product reserved row: %v", err)
	}

	o, err := s.ReservedOrderForChat("77")
	if err != nil {
		t.Fatalf("ReservedOrderForChat: %v", err)
	}
	if o == nil {
		t.Fatal("ReservedOrderForChat = nil — a bare `product!=?` silently " +
			"dropped the NULL-product reserved row instead of matching it")
	}
	if o.Des != "nullres1" {
		t.Errorf("ReservedOrderForChat returned des=%q, want %q", o.Des, "nullres1")
	}
}

// TestHasDeliveredOrderIncludesNullProduct is the discriminating fixture for
// orders.go's HasDeliveredOrder. This function has zero callers in this repo
// today (verified by grep across internal/); the fix and this test exist for
// uniformity with the other two sites in orders.go, not because it closes an
// observed bug.
//
// Falsification: reverting to a bare `product!=?` drops the sole seeded
// NULL-product delivered row out of the WHERE clause, so the query finds no
// match and HasDeliveredOrder returns false instead of true — verified by
// mutation.
func TestHasDeliveredOrderIncludesNullProduct(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		"nulldel1", "88", "en", "T1", 1, 5000, "delivered", 100); err != nil {
		t.Fatalf("seed NULL-product delivered row: %v", err)
	}

	has, err := s.HasDeliveredOrder("88")
	if err != nil {
		t.Fatalf("HasDeliveredOrder: %v", err)
	}
	if !has {
		t.Error("HasDeliveredOrder = false — a bare `product!=?` silently " +
			"dropped the NULL-product delivered row instead of matching it")
	}
}

// TestDeliveredProductSummaryIncludesNullProduct is the discriminating
// fixture for orders.go's DeliveredProductSummary — the site that matters
// most of the three, since it feeds handlers.historySummaryText (/history).
// A bare filter here means a customer's /history and the admin dashboard's
// revenue queries (already COALESCE-first, misc.go) could disagree about the
// very same NULL-product delivered row.
//
// Seeds one ordinary delivered row and one NULL-product delivered row for the
// SAME chat, so the assertions catch both a dropped ROW (len, product
// visibility) and a wrong AGGREGATE (SUM/COUNT silently short by the dropped
// row's contribution) — the same two-pronged shape
// TestRevenueSummaryCustomersExcludesDepositOnly uses for misc.go's queries.
//
// Falsification: reverting to a bare `product!=?` drops the NULL-product row
// from the GROUP BY entirely, so len(items) reads 1 instead of 2 and the
// summed Paid across rows is short by 7000 (5000 instead of 12000) — verified
// by mutation.
func TestDeliveredProductSummaryIncludesNullProduct(t *testing.T) {
	s := newTestStore(t)

	if err := s.CreateOrder("prodnorm1", "99", "en", "kiro-pro", "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(prodnorm1): %v", err)
	}
	if _, err := s.db.Exec("UPDATE orders SET status='delivered' WHERE des=?", "prodnorm1"); err != nil {
		t.Fatalf("seed prodnorm1 -> delivered: %v", err)
	}
	if _, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, tier, qty, total, status, ts) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		"nullsum1", "99", "en", "T1", 2, 7000, "delivered", 200); err != nil {
		t.Fatalf("seed NULL-product delivered row: %v", err)
	}

	items, err := s.DeliveredProductSummary("99")
	if err != nil {
		t.Fatalf("DeliveredProductSummary: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("DeliveredProductSummary returned %d groups, want 2 — a bare "+
			"`product!=?` silently drops the NULL-product row's own GROUP BY "+
			"bucket", len(items))
	}
	var totalPaid, totalOrders int64
	var sawNullGroup bool
	for _, it := range items {
		totalPaid += it.Paid
		totalOrders += it.Orders
		if it.Product == "" {
			sawNullGroup = true
			if it.Accounts != 2 || it.Paid != 7000 || it.Orders != 1 {
				t.Errorf("NULL-product group = %+v, want Accounts=2 Paid=7000 Orders=1", it)
			}
		}
	}
	if !sawNullGroup {
		t.Error("no NULL-product group in DeliveredProductSummary — the row was dropped")
	}
	if totalPaid != 12000 {
		t.Errorf("total Paid across groups = %d, want 12000 (5000 + 7000) — the "+
			"NULL-product row's total must be included in the aggregate SUM, not "+
			"just visible as a row", totalPaid)
	}
	if totalOrders != 2 {
		t.Errorf("total Orders across groups = %d, want 2", totalOrders)
	}
}
