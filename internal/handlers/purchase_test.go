package handlers

// purchase_test.go — owner-alert regression coverage for the two #39 sites in
// purchase.go: deliverPaid's out-of-stock refund double-failure (revert ALSO
// failed) and its partial-delivery branch. Both leave the order in a status
// store.PaidUndelivered (misc.go: WHERE status='paid') structurally cannot
// see ('refunded' or 'delivered', never 'paid'), so before #39 the CRITICAL
// log line was the only trace either event ever left.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
)

// blockRefundedToPaidRevert installs a trigger that fails ONLY the exact
// refunded->paid UPDATE ClaimOrder issues on a revert, leaving every other
// order transition (including the paid->refunded claim that must succeed
// FIRST) untouched — the same asymmetric-failure shape a disk-full/
// SQLITE_BUSY error on a SECOND write, after the first already committed,
// would produce. Shared by purchase_test.go and apikey_deliver_test.go — both
// need to force the "credit failed, AND the revert ALSO failed" double
// failure without breaking the claim that must land first.
func blockRefundedToPaidRevert(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().Exec(`CREATE TRIGGER block_revert BEFORE UPDATE ON orders
		WHEN OLD.status='refunded' AND NEW.status='paid'
		BEGIN SELECT RAISE(ABORT, 'forced test failure: blocked refunded->paid revert'); END`); err != nil {
		t.Fatalf("install revert-blocking trigger: %v", err)
	}
}

// TestDeliverPaidOutOfStockRefundRevertFailedNotifiesOwner covers
// purchase.go:342 (#39 site 1): the reserved rows are gone (out-of-stock
// refund), CreditBalance then errors, AND the compensating revert to 'paid'
// ALSO errors — the order is stuck 'refunded' with nothing credited, and
// (unlike a successful revert, which lands back on 'paid' where the existing
// stuck-paid reconcile catches it) store.PaidUndelivered's WHERE status='paid'
// scan can never see this row again. Deleting the notifyOwners call in
// deliverPaid must turn this test red.
func TestDeliverPaidOutOfStockRefundRevertFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, stk, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
	o, err := st.ReservedOrderForChat("100")
	if err != nil || o == nil {
		t.Fatal("expected a reserved order")
	}
	// Move straight to 'paid' — the debit path is irrelevant to this
	// failure; only CreditBalance/the revert need to be made to fail below.
	if _, err := st.ClaimOrder(o.Des, "reserved", "paid"); err != nil {
		t.Fatal(err)
	}
	// Rows gone -> Finalize claims nothing -> deliverPaid takes the
	// out-of-stock refund branch.
	if _, err := stk.Release("prodA", o.Des); err != nil {
		t.Fatal(err)
	}
	// Force CreditBalance to error...
	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}
	// ...AND force the compensating revert to ALSO error.
	blockRefundedToPaidRevert(t, st)

	if b.deliverPaid(ctx, o, o.Des) {
		t.Fatal("deliverPaid must report failure when both the credit and the revert fail")
	}
	got, _ := st.GetOrder(o.Des)
	if got.Status != "refunded" {
		t.Fatalf("order status = %q, want 'refunded' (the first claim committed; only the revert failed)", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "REFUND REVERT FAILED") {
		t.Fatalf("owner alert = %v, want a REFUND REVERT FAILED message", alert)
	}
	if !strings.Contains(alert.Text, o.Des) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
	if !strings.Contains(alert.Text, "MANUAL FIX REQUIRED") {
		t.Errorf("owner alert missing the action line: %q", alert.Text)
	}
}

// TestDeliverPaidPartialDeliveryNotifiesOwner covers purchase.go:353 (#39
// site 8): one of two reserved credential files is corrupt, so Finalize skips
// it and the customer receives fewer accounts than they paid for. The order
// still proceeds straight to 'delivered' — no backstop ever revisits it — so
// the audit row (LogAction) was the only trace before #39. Deleting the
// notifyOwners call in deliverPaid must turn this test red.
func TestDeliverPaidPartialDeliveryNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 2)
	const chat = 100
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 2)
	o, err := st.ReservedOrderForChat("100")
	if err != nil || o == nil {
		t.Fatal("expected a reserved order")
	}
	// Corrupt one of the two reserved credential files (empty it out) —
	// stock.go's Finalize skips an unreadable/empty file rather than
	// aborting the whole delivery.
	reservedDir := filepath.Join(dir, "accounts", "prodA", "reserved", o.Des)
	entries, err := os.ReadDir(reservedDir)
	if err != nil {
		t.Fatalf("read reserved dir %s: %v", reservedDir, err)
	}
	var txtFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			txtFiles = append(txtFiles, e.Name())
		}
	}
	if len(txtFiles) != 2 {
		t.Fatalf("expected 2 reserved credential files in %s, got %v", reservedDir, txtFiles)
	}
	if err := os.WriteFile(filepath.Join(reservedDir, txtFiles[0]), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(o.Des, "reserved", "paid"); err != nil {
		t.Fatal(err)
	}
	o, _ = st.GetOrder(o.Des)

	if !b.deliverPaid(ctx, o, o.Des) {
		t.Fatal("deliverPaid must still report success — one account short-delivered is not a total failure")
	}
	got, _ := st.GetOrder(o.Des)
	if got.Status != "delivered" {
		t.Fatalf("order status = %q, want delivered", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "PARTIAL DELIVERY") {
		t.Fatalf("owner alert = %v, want a PARTIAL DELIVERY message", alert)
	}
	if !strings.Contains(alert.Text, "1/2") {
		t.Errorf("owner alert missing the short-delivery count (want 1/2): %q", alert.Text)
	}
	if !strings.Contains(alert.Text, o.Des) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
}
