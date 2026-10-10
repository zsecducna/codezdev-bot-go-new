package handlers

// apikey_deliver_test.go — owner-alert regression coverage for the four #39
// sites in apikey_deliver.go: the api_keys row persist failure, both
// mint-fail/recharge-fail refund double-failures (credit AND the revert-to-
// 'paid' both fail), and the recharge-applied stamp failure. All four leave
// the order (or the key's local credit state) in a shape no other backstop
// ever revisits — before #39 the CRITICAL log line was the only trace.

import (
	"context"
	"strings"
	"testing"
)

// TestApikeyRowPersistFailedNotifiesOwner covers apikey_deliver.go:123 (#39
// site 6): the key IS minted upstream (billable, real money spent) but the
// api_keys row insert fails — without that row a settle replay would
// double-mint. The order still proceeds to 'delivered' (the buyer holds the
// key from the in-memory response), so nothing else ever revisits this row.
// Deleting the notifyOwners call in deliverApikey must turn this test red.
func TestApikeyRowPersistFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "aaaa1111aaaa1111aaaa1111aaaa1111"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if _, err := st.DB().Exec(`CREATE TRIGGER block_apikey_insert BEFORE INSERT ON api_keys
		BEGIN SELECT RAISE(ABORT, 'forced test failure: blocked api_keys insert'); END`); err != nil {
		t.Fatalf("install insert-blocking trigger: %v", err)
	}

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("deliverApikey must still deliver the freshly-minted key even when the row persist fails")
	}
	if row, _ := st.APIKeyForOrder(des); row != nil {
		t.Fatalf("api_keys row should NOT exist (the insert was blocked), got %+v", row)
	}
	if n := countContains(f, "API-KEY ROW PERSIST FAILED"); n != 1 {
		t.Errorf("owner alert count = %d, want 1", n)
	}
}

// TestApikeyMintFailRefundRevertFailedNotifiesOwner covers
// apikey_deliver.go:265 (#39 site 2) — the mint-fail-refund twin of
// purchase.go's out-of-stock double failure: the mint fails, the refund
// claim wins (paid->refunded), CreditBalance then errors, AND the
// compensating revert to 'paid' ALSO errors. The order is stuck 'refunded'
// with nothing credited, invisible to store.PaidUndelivered's
// WHERE status='paid' scan forever. Deleting the notifyOwners call in
// apikeyMintFailRefund must turn this test red.
func TestApikeyMintFailRefundRevertFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "bbbb2222bbbb2222bbbb2222bbbb2222"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")
	fl.CreateStatus = 400 // llmgate create rejected (bedrock mints there now)

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}
	blockRefundedToPaidRevert(t, st) // purchase_test.go

	if b.deliverApikey(ctx, o, des) {
		t.Fatal("deliverApikey must report failure when the mint, credit AND revert all fail")
	}
	got, _ := st.GetOrder(des)
	if got.Status != "refunded" {
		t.Fatalf("order status = %q, want refunded (the claim committed; only the revert failed)", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "REFUND REVERT FAILED") {
		t.Fatalf("owner alert = %v, want a REFUND REVERT FAILED message", alert)
	}
	if !strings.Contains(alert.Text, des) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
}

// TestApikeyRechargeFailRefundRevertFailedNotifiesOwner covers
// apikey_deliver.go:439 (#39 site 3) — the recharge-fail-refund twin of the
// test above: the top-up call fails, the refund claim wins, CreditBalance
// errors, AND the revert to 'paid' ALSO errors.
func TestApikeyRechargeFailRefundRevertFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "cccc3333cccc3333cccc3333cccc3333"
	const rcDes = "dddd4444dddd4444dddd4444dddd4444"
	seedKey(t, st, fk, keyDes, chat, "poolkey-rcrevert", "sk-test-rcrevert-0123456789a", "bedrock", 1000, 1000, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")
	fk.RechargeStatus = 400 // recharge rejected

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}
	blockRefundedToPaidRevert(t, st)

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("deliverApikeyRecharge must report failure when the recharge, credit AND revert all fail")
	}
	got, _ := st.GetOrder(rcDes)
	if got.Status != "refunded" {
		t.Fatalf("order status = %q, want refunded (the claim committed; only the revert failed)", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "REFUND REVERT FAILED") {
		t.Fatalf("owner alert = %v, want a REFUND REVERT FAILED message", alert)
	}
	if !strings.Contains(alert.Text, rcDes) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
}

// TestApikeyRechargeAppliedStampFailedNotifiesOwner covers
// apikey_deliver.go:377 (#39 site 7): Kiro-Go CONFIRMS the top-up (credits
// are live on the key) but the local applied_ts stamp write fails.
//
// The order does NOT stay 'paid' — an earlier version of this comment said
// it did. The stamp failure does not block the orders table, so control
// falls through and the paid->delivered claim succeeds; the row ends
// 'delivered', which store.PaidUndelivered's WHERE status='paid' can never
// see. So no backstop will EVER surface this: the credits are live on the
// key, the local stamp says otherwise, and a settle replay would apply them
// a SECOND time. The alert is the only trace. The final-status assertion
// below exists to keep that fact pinned, since the false claim drifted in
// unnoticed precisely because nothing asserted the status.
func TestApikeyRechargeAppliedStampFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "eeee5555eeee5555eeee5555eeee5555"
	const rcDes = "ffff6666ffff6666ffff6666ffff6666"
	seedKey(t, st, fk, keyDes, chat, "poolkey-stampfail", "sk-test-stampfail-0123456789", "bedrock", 1000, 1000, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if _, err := st.DB().Exec(`CREATE TRIGGER block_recharge_stamp BEFORE UPDATE ON apikey_recharges
		BEGIN SELECT RAISE(ABORT, 'forced test failure: blocked recharge-applied stamp'); END`); err != nil {
		t.Fatalf("install stamp-blocking trigger: %v", err)
	}

	if !b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("deliverApikeyRecharge must still report success — the top-up landed on Kiro-Go even though the local stamp failed")
	}
	rec, err := st.APIKeyRechargeForOrder(rcDes)
	if err != nil || rec == nil || rec.AppliedTS != nil {
		t.Errorf("applied_ts should remain NULL (the stamp write was blocked), got %+v (err=%v)", rec, err)
	}
	if n := countContains(f, "RECHARGE-APPLIED STAMP FAILED"); n != 1 {
		t.Errorf("owner alert count = %d, want 1", n)
	}
	// The order must end 'delivered', NOT 'paid'. This is the assertion whose
	// absence let a false comment claim the stuck-paid reconcile would cover
	// this site. store.PaidUndelivered selects WHERE status='paid', so a
	// 'delivered' row is invisible to it forever — which is precisely why the
	// alert above is the only trace this failure will ever leave. If this ever
	// flips to 'paid', the alert's justification changes and the comments
	// above it become wrong again.
	got, err := st.GetOrder(rcDes)
	if err != nil {
		t.Fatalf("GetOrder(%s): %v", rcDes, err)
	}
	if got.Status != "delivered" {
		t.Errorf("order status = %q, want \"delivered\" — the stamp failure must not block the paid->delivered claim, and the reconcile's WHERE status='paid' must not be able to see this row", got.Status)
	}
}
