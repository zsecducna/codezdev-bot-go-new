package handlers

// apikey_refund_test.go — owner-alert regression coverage for the #39 site
// in apikey_refund.go: doRefundApikeyGroup's CreditBalance failure. The
// group claims already committed (every refunded order in the group IS
// 'refunded'), so this is the group-refund twin of adminorders.go's
// TestDoRefundCreditFailedNotifiesOwner.

import (
	"context"
	"strings"
	"testing"
)

// TestDoRefundApikeyGroupCreditFailedNotifiesOwner covers
// apikey_refund.go:353 (#39 site 5): CreditBalance errors AFTER the group
// claim already committed — every refunded order stays 'refunded' with
// nothing credited, and no automatic path will ever retry a 'refunded'
// order. Before #39 the CRITICAL log line was the only trace. Deleting the
// notifyOwners call in doRefundApikeyGroup must turn this test red.
func TestDoRefundApikeyGroupCreditFailedNotifiesOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "1111aaaa1111aaaa1111aaaa1111aaaa"
	seedKey(t, st, fk, keyDes, chat, "poolkey-credfail", "sk-test-credfail-0123456789a", "bedrock", 1000, 1000, 200)

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	o, _ := st.GetOrder(keyDes)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil || !res.CreditFailed {
		t.Fatalf("res = %+v, want a claimed group refund with CreditFailed=true", res)
	}
	got, _ := st.GetOrder(keyDes)
	if got.Status != "refunded" {
		t.Errorf("order status = %q, want refunded (the claim committed even though the credit failed)", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "REFUND CREDIT FAILED") {
		t.Fatalf("owner alert = %v, want a REFUND CREDIT FAILED message", alert)
	}
	if !strings.Contains(alert.Text, keyDes) {
		t.Errorf("owner alert missing the order des: %q", alert.Text)
	}
}

// TestCbApikeyRefOKSkipsAdminRefundedNoticeOnCreditFailed covers a
// pre-existing bug found alongside task #43's HIGH fixes: cbApikeyRefOK (the
// customer's self-service refund confirm) always hardcodes method="balance"
// and, before this fix, sent its "refunded X to wallet" admin notice
// UNCONDITIONALLY — even when doRefundApikeyGroup's CreditBalance call above
// it errored and res.CreditFailed is true. That claimed a wallet credit had
// happened directly alongside (and in flat contradiction of) the loud
// REFUND CREDIT FAILED alert doRefundApikeyGroup itself already sent from
// inside the failure branch. Deleting the `if res.CreditFailed { return }`
// guard in cbApikeyRefOK turns this test red.
func TestCbApikeyRefOKSkipsAdminRefundedNoticeOnCreditFailed(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "ffff6666ffff6666ffff6666ffff6666"
	seedKey(t, st, fk, keyDes, chat, "poolkey-cbcredfail", "sk-test-cbcredfail-0123456789", "bedrock", 1000, 1000, 200)

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(chat, "apikeyrefok:"+keyDes))

	got, _ := st.GetOrder(keyDes)
	if got.Status != "refunded" {
		t.Errorf("order status = %q, want refunded (the claim commits before CreditBalance runs)", got.Status)
	}
	sawCreditFailedAlert := false
	for _, s := range f.Sends {
		if strings.Contains(s.Text, "to wallet") {
			t.Errorf("a message claimed a wallet credit that never happened: %q", s.Text)
		}
		if s.ChatID == 999 && strings.Contains(s.Text, "REFUND CREDIT FAILED") {
			sawCreditFailedAlert = true
		}
	}
	if !sawCreditFailedAlert {
		t.Error("owner never received the REFUND CREDIT FAILED alert from doRefundApikeyGroup")
	}
	if buyerMsg := lastSendMsgTo(f, chat); buyerMsg != nil {
		t.Errorf("buyer chat %d received a message, want NONE (nothing was credited), got: %q", chat, buyerMsg.Text)
	}
}

// ============================================================================
// refunded_amount — S2 bookkeeping (task #20), the group-refund split
// ============================================================================

// TestSplitRefundedAmountExactSum is a direct unit test of the
// largest-paid-member split: SUM(shares) must equal amountGroup EXACTLY even
// when independent per-member rounding would drift from it (here, two equal
// 500/500 totals splitting an odd 1, where round(0.5)+round(0.5)=2 != 1).
// This is the property that makes refunded_amount trustworthy for a group
// refund — a stats query can SUM() this column and get the real payout back.
func TestSplitRefundedAmountExactSum(t *testing.T) {
	won := []apikeyRefundMember{{Des: "a", Total: 500}, {Des: "b", Total: 500}}
	shares := splitRefundedAmount(won, 1000, 1)
	var sum int64
	for _, v := range shares {
		sum += v
	}
	if sum != 1 {
		t.Fatalf("sum(shares) = %d, want 1 (== amountGroup)", sum)
	}
	// The residual lands on the first member when totals tie (index 0 never
	// loses its "largest" slot to an equal, not greater, total) — pin the
	// exact split so a future reshuffle of the algorithm is caught, not just
	// its sum property.
	if shares["a"] != 0 || shares["b"] != 1 {
		t.Errorf("shares = %v, want {a:0, b:1}", shares)
	}
}

// TestDoRefundApikeyGroupRecordsRefundedAmountAcceptance is THE acceptance
// test for the group split: refund a real two-member group (original key
// order + one applied recharge) and prove, by reading refunded_amount back
// from the DB, that SUM(refunded_amount) over the group EXACTLY equals the
// single aggregate CreditBalance amount. If that ever drifts, the column is
// lying about revenue for every multi-order API-key group refund — the
// larger overstatement this stage exists to fix. Deleting either
// recordGroupRefundedAmounts call or the SetRefundedAmount calls inside it
// turns this test red.
func TestDoRefundApikeyGroupRecordsRefundedAmountAcceptance(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "aaaa1111aaaa1111aaaa1111aaaa1111"
	const rcDes = "bbbb2222bbbb2222bbbb2222bbbb2222"
	// Root: 1000 credits bought, 1,000,000 paid, 200 used live (800 remaining).
	seedKey(t, st, fk, keyDes, chat, "poolkey-acc", "sk-test-acc-0123456789abcdef", "bedrock", 1000, 1000, 200)
	// One applied recharge: 500 credits, 500,000 paid.
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOrder(rcDes, itoa64(chat), "en", apikeyRechargeProduct, "API", 500, 500000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(rcDes, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAPIKeyRechargeApplied(rcDes); err != nil {
		t.Fatal(err)
	}

	o, _ := st.GetOrder(keyDes)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil || res.CreditFailed {
		t.Fatalf("res = %+v, want a successful group refund", res)
	}
	if res.GroupN != 2 {
		t.Fatalf("GroupN = %d, want 2 (root + 1 applied recharge)", res.GroupN)
	}
	// bought=1500, groupPaid=1,500,000, remaining=800 -> amount =
	// round(1,500,000 * 800/1500) = 800,000, split exactly (no residual):
	// root 1,000,000/1,500,000 * 800,000 = 533,333 (round-half-up of .333.. down)
	// rc     500,000/1,500,000 * 800,000 = 266,667
	if res.Amount != 800000 {
		t.Fatalf("res.Amount = %d, want 800000", res.Amount)
	}
	rootShare, ok1 := refundedAmountFor(t, st, keyDes)
	rcShare, ok2 := refundedAmountFor(t, st, rcDes)
	if !ok1 || !ok2 {
		t.Fatalf("refunded_amount left NULL: root ok=%v rc ok=%v", ok1, ok2)
	}
	if sum := rootShare + rcShare; sum != res.Amount {
		t.Errorf("SUM(refunded_amount) = %d, want %d (== the single CreditBalance amount)", sum, res.Amount)
	}
	if rootShare != 533333 || rcShare != 266667 {
		t.Errorf("split = root:%d rc:%d, want root:533333 rc:266667", rootShare, rcShare)
	}
}

// TestDoRefundApikeyGroupCreditFailedRecordsZero is the group-refund twin of
// TestDoRefundCreditFailedRecordsZeroRefundedAmount: CreditBalance errors
// AFTER every claim in the group already committed 'refunded', so ZERO money
// moved for the WHOLE group — every member must record 0, not a would-be
// split and not NULL.
func TestDoRefundApikeyGroupCreditFailedRecordsZero(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "cccc3333cccc3333cccc3333cccc3333"
	seedKey(t, st, fk, keyDes, chat, "poolkey-zero", "sk-test-zero-0123456789abcdef", "bedrock", 1000, 1000, 200)

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	o, _ := st.GetOrder(keyDes)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil || !res.CreditFailed {
		t.Fatalf("res = %+v, want a claimed group refund with CreditFailed=true", res)
	}
	got, ok := refundedAmountFor(t, st, keyDes)
	if !ok || got != 0 {
		t.Errorf("refunded_amount = (valid=%v, %d), want (true, 0) — nothing was actually credited", ok, got)
	}
}

// TestDoRefundApikeyGroupManualRecordsRefundedAmount proves the "manual"
// payout method (no CreditBalance call) still records refunded_amount for a
// group refund, same as the single-order path.
func TestDoRefundApikeyGroupManualRecordsRefundedAmount(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "dddd4444dddd4444dddd4444dddd4444"
	seedKey(t, st, fk, keyDes, chat, "poolkey-manual", "sk-test-manual-0123456789abcd", "bedrock", 1000, 1000, 200)

	o, _ := st.GetOrder(keyDes)
	res := b.doRefundApikeyGroup(ctx, o, "manual", false)
	if res == nil {
		t.Fatal("doRefundApikeyGroup returned nil, want a result")
	}
	if bal, _ := st.GetBalance(itoa64(chat)); bal != 0 {
		t.Errorf("balance = %d, want 0 — manual payout must never call CreditBalance", bal)
	}
	got, ok := refundedAmountFor(t, st, keyDes)
	if !ok || got != res.Amount {
		t.Errorf("refunded_amount = (valid=%v, %d), want (true, %d)", ok, got, res.Amount)
	}
}

// TestDoRefundApikeyGroupUnknownMethodAlertsOwner is the group-refund twin of
// adminorders.go's TestDoRefundUnknownMethodAlertsOwner (task #43, L3): a
// payout method matching neither "balance" nor "manual" has no branch that
// knows how to credit the group or record refunded_amount for any of its
// members, so it must alert the owner loudly by name. Deleting the default
// branch's notifyOwners call in doRefundApikeyGroup turns this test red.
//
// Seeds all THREE refunded_amount populations the default branch must zero:
// the root key order, an APPLIED recharge (a normal group member, paid via
// the pro-rated split), and a STUCK never-applied recharge (paid at face
// value outside the group maths, refunded via the same call by acting on the
// stuck order itself — apikeyRefundGroup resolves back to the same root).
func TestDoRefundApikeyGroupUnknownMethodAlertsOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "eeee5555eeee5555eeee5555eeee5555"
	const rcDes = "eeee5555eeee5555eeee5555eeee5551"
	const stuckDes = "eeee5555eeee5555eeee5555eeee5552"
	seedKey(t, st, fk, keyDes, chat, "poolkey-unknownmethod", "sk-test-unknownmethod-0123456789", "bedrock", 1000, 1000, 200)
	// Applied recharge: a normal group member.
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOrder(rcDes, itoa64(chat), "en", apikeyRechargeProduct, "API", 500, 500000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(rcDes, "reserved", "delivered"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAPIKeyRechargeApplied(rcDes); err != nil {
		t.Fatal(err)
	}
	// Stuck recharge: paid but never applied (Kiro-Go down between payment
	// and fulfillment) — refunds at face value outside the group maths.
	if _, err := st.AddAPIKeyRecharge(stuckDes, keyDes, chat, 300); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOrder(stuckDes, itoa64(chat), "en", apikeyRechargeProduct, "API", 300, 300000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrder(stuckDes, "reserved", "paid"); err != nil {
		t.Fatal(err)
	}

	// Act on the stuck order itself — apikeyRefundGroup resolves it back to
	// the same root/group, so this single call refunds all three orders.
	oStuck, _ := st.GetOrder(stuckDes)
	res := b.doRefundApikeyGroup(ctx, oStuck, "carrier_pigeon", false)
	if res == nil {
		t.Fatal("doRefundApikeyGroup returned nil — the group claim must still win even though the method is unrecognised")
	}
	// Nothing was actually credited (no CreditBalance call, no manual payout
	// recorded), so this must read exactly like a CreditBalance failure:
	// CreditFailed=true, and the buyer must NEVER hear "refunded" — they
	// would be told they were paid while nothing moved (and, when the key
	// was revoked above — gated on len(won)>0, not always true for a
	// stuck-only claim — while their key is gone too). Review finding (task
	// #43 HIGH): without CreditFailed set here, the customer DM guard below
	// it (and cbApikeyRefOK's admin "refunded X to wallet" notice) would
	// both fire as if the payout had actually succeeded. (This switch is
	// unreachable in the current build — every caller passes "balance" or
	// "manual" — so this has not actually happened; this pins the guarantee
	// for whenever that stops being true.)
	if !res.CreditFailed {
		t.Error("res.CreditFailed = false, want true — nothing was credited for an unrecognised payout method")
	}
	if res.Notified {
		t.Error("res.Notified = true — no customer DM should have been attempted")
	}
	for _, d := range []string{keyDes, rcDes, stuckDes} {
		if got, _ := st.GetOrder(d); got.Status != "refunded" {
			t.Errorf("order %s status = %q, want refunded (the claim commits before the payout-method switch runs)", d, got.Status)
		}
	}
	if buyerMsg := lastSendMsgTo(f, chat); buyerMsg != nil {
		t.Errorf("buyer chat %d received a message, want NONE (nothing was credited, so no refund DM must be sent), got: %q", chat, buyerMsg.Text)
	}
	// The headline decision this default branch makes: record 0, not NULL,
	// for EVERY member of the group — root, applied recharge, and stuck
	// order alike — not just the one order the call was invoked from.
	// Deleting either SetRefundedAmount(_, 0) loop/call in the default
	// branch turns one or more of these red.
	for _, d := range []string{keyDes, rcDes, stuckDes} {
		if got, ok := refundedAmountFor(t, st, d); !ok || got != 0 {
			t.Errorf("refunded_amount(%s) = (valid=%v, %d), want (true, 0) — nothing was actually credited", d, ok, got)
		}
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "UNKNOWN PAYOUT METHOD") {
		t.Fatalf("owner alert = %v, want an UNKNOWN PAYOUT METHOD message", alert)
	}
	if !strings.Contains(alert.Text, "carrier_pigeon") {
		t.Errorf("owner alert missing the unrecognised method name, got: %q", alert.Text)
	}
	if !strings.Contains(alert.Text, stuckDes) {
		t.Errorf("owner alert missing the acted-on order des: %q", alert.Text)
	}
	if !strings.Contains(alert.Text, keyDes) || !strings.Contains(alert.Text, rcDes) {
		t.Errorf("owner alert missing group members (root/recharge) from the members list: %q", alert.Text)
	}
}
