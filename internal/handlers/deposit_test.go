package handlers

// deposit_test.go — the pending_deposit amount-capture test plus small shared
// test helpers used across the handlers test package (waitFor/lastSend/
// docCount/countContains). The SePay/VietQR deposit + order-pay-by-QR tests
// (TestDepositCreditOnce, TestDepositQRRender, TestDepositTimeout,
// TestOrderPayUnderpayThenFull, TestOrderPayFullNoBalanceCredit) and their
// fakeSepay/txnRow harness were removed along with the SePay rail itself —
// USDT-only migration, S1 (2026-08-06).

import (
	"context"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
)

// waitFor polls cond up to 2s (fast tick) so worker-goroutine tests aren't flaky.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(3 * time.Millisecond)
	}
	return cond()
}

// TestDepositWindowIsOneHour pins the customer-facing deposit expiry: a
// deposit must stay claimable long enough for a customer to leave Telegram,
// open a wallet app, and send — ten minutes was not enough (204 of 426
// deposit orders expired). Pinned as a constant expectation so a future edit
// to the default is a deliberate, visible change.
func TestDepositWindowIsOneHour(t *testing.T) {
	b, _ := newTestBot("999", nil)
	if got, want := b.expiryTimeout, time.Hour; got != want {
		t.Fatalf("expiryTimeout = %v, want %v", got, want)
	}
}

// TestDepositAmountBounds pins the pending_deposit capture's peek + bounds: an
// out-of-range amount re-prompts and keeps the slot armed; a slash/label escape
// leaves it armed; a valid amount consumes it.
func TestDepositAmountBounds(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999")
	const chat = 100
	primeKB(b, chat)
	arm := func() {
		st := b.states.Get(chat)
		st.mu.Lock()
		st.Deposit = &DepositCtx{Method: "pp"}
		st.mu.Unlock()
	}

	// Escape via slash command: slot stays armed.
	arm()
	b.Process(ctx, textUpdate(chat, "/start"))
	if b.states.Get(chat).Deposit == nil {
		t.Error("slash escape must leave pending_deposit ARMED")
	}

	// Below the minimum (0.5 < 1): BAD_AMOUNT, slot stays armed.
	b.Process(ctx, textUpdate(chat, "0.5"))
	if !strings.HasPrefix(lastSend(f), "⚠️") {
		t.Errorf("below-min amount reply = %q, want DEPOSIT_BAD_AMOUNT", lastSend(f))
	}
	if b.states.Get(chat).Deposit == nil {
		t.Error("out-of-bounds amount must keep pending_deposit armed for retry")
	}

	// Valid amount consumes the slot (routes to the method stub).
	b.Process(ctx, textUpdate(chat, "5"))
	if b.states.Get(chat).Deposit != nil {
		t.Error("valid amount must consume pending_deposit")
	}
}

// TestCbDepStaleMethodAnswersPayMethodOff is the regression test for the
// residual fix (Opus review of 11e033d): cbDep's switch used to have no
// default case, so a stale "dep:qr"/"dep:pp"/"dep:bnb" tap — left over from a
// deposit picker rendered before the SePay/PayPal/Binance removal (S1-S3) —
// fell through silently. command.go unconditionally deletes the tapped
// message BEFORE dispatch, so that silent fall-through left the customer with
// NOTHING: no menu, no error, no way forward. Every dep: tap must now answer.
func TestCbDepStaleMethodAnswersPayMethodOff(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999")
	const chat = 100
	primeKB(b, chat)

	for _, stale := range []string{"pp", "bnb", "qr", "sepay"} {
		f.Sends = nil
		b.Process(ctx, callbackUpdate(chat, "dep:"+stale))
		last := lastSendMsg(f)
		want := i18n.C().Buy.PayMethodOff.T(i18n.EN)
		if last == nil {
			t.Errorf("dep:%s produced no reply — the customer is left with nothing", stale)
			continue
		}
		if last.Text != want {
			t.Errorf("dep:%s reply = %q, want %q", stale, last.Text, want)
		}
	}
}

// TestSettlePaidLateExpiredClaimKindByProduct is the HANDLER half of the
// review-round fix on this slice: settlePaid's `o.Status == "expired"` tail
// (a payment that lands AFTER the checkout window closed) already branched on
// o.Product BEFORE this refund-kind work existed — a __deposit__ order gets
// Deposit.OK, any other product gets Buy.PaidNoStock (deposit.go, both below
// the ClaimOrderKind call). The first version of this slice tagged BOTH
// branches late_settled; that made a real paid-not-delivered event (money
// back, nothing shipped, on the MAIN product path) invisible on every
// refund-kind dashboard bucket — reviewed and fixed to branch the KIND the
// same way the MESSAGE already branches.
//
// No test previously exercised this code path at all: reverting the kind
// selection back to always RefundKindLateSettled passes the full suite
// without this test (verified before writing it, then reverted).
//
// Falsification: reverting deposit.go's `kind := ...; if o.Product !=
// depositProduct { kind = store.RefundKindOutOfStock }` back to the
// single-kind form makes the "product order" half of this test fail (its
// RefundKind reads late_settled instead of out_of_stock); the deposit half
// stays green either way, which is exactly why a single-branch test would not
// have caught the bug.
func TestSettlePaidLateExpiredClaimKindByProduct(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newBuyBot(t, "999")

	// A __deposit__ order sitting 'expired': a late top-up landing after the
	// window is a COMPLETED SUCCESS (Deposit.OK is sent) wearing a 'refunded'
	// status — late_settled.
	depDes := store.NewDes()
	if err := st.CreateOrder(depDes, "100", "en", depositProduct, "", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(dep): %v", err)
	}
	if _, err := st.ClaimOrder(depDes, "reserved", "expired"); err != nil {
		t.Fatalf("claim dep -> expired: %v", err)
	}
	if _, err := b.settlePaid(ctx, depDes); err != nil {
		t.Fatalf("settlePaid(dep): %v", err)
	}
	depOrder, err := st.GetOrder(depDes)
	if err != nil || depOrder == nil {
		t.Fatalf("GetOrder(dep): order=%v err=%v", depOrder, err)
	}
	if depOrder.Status != "refunded" {
		t.Fatalf("dep status = %q, want refunded", depOrder.Status)
	}
	if depOrder.RefundKind == nil || *depOrder.RefundKind != store.RefundKindLateSettled {
		t.Errorf("dep RefundKind = %s, want %q", kindStr(depOrder.RefundKind), store.RefundKindLateSettled)
	}

	// A REGULAR product order sitting 'expired': spawnOrderExpiry already
	// released the reserved folder-stock rows back at expiry, so a payment
	// landing after that has NOTHING to deliver against — money back,
	// Buy.PaidNoStock sent. Identical in kind to purchase.go's out_of_stock
	// path (customer paid, stock gone, wallet credited), not a success.
	prodDes := store.NewDes()
	if err := st.CreateOrder(prodDes, "101", "en", "kiro-pro", "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(prod): %v", err)
	}
	if _, err := st.ClaimOrder(prodDes, "reserved", "expired"); err != nil {
		t.Fatalf("claim prod -> expired: %v", err)
	}
	if _, err := b.settlePaid(ctx, prodDes); err != nil {
		t.Fatalf("settlePaid(prod): %v", err)
	}
	prodOrder, err := st.GetOrder(prodDes)
	if err != nil || prodOrder == nil {
		t.Fatalf("GetOrder(prod): order=%v err=%v", prodOrder, err)
	}
	if prodOrder.Status != "refunded" {
		t.Fatalf("prod status = %q, want refunded", prodOrder.Status)
	}
	if prodOrder.RefundKind == nil || *prodOrder.RefundKind != store.RefundKindOutOfStock {
		t.Errorf("prod RefundKind = %s, want %q — a late payment on a non-deposit "+
			"expired order must NOT be tagged late_settled, or it becomes invisible "+
			"on every refund-kind dashboard bucket",
			kindStr(prodOrder.RefundKind), store.RefundKindOutOfStock)
	}
}

// TestSettlePaidFreshDepositNotifiesAdminOwner covers deposit.go:240 (the
// fresh reserved->paid claim branch) — notify_admin_deposit
// (telegram_bot.py:9028-9034). AdminID "999" here is always distinct from
// the buyer chat, so a message landing there is unambiguously the owner
// alert.
func TestSettlePaidFreshDepositNotifiesAdminOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "999")
	const chat = 200

	des := store.NewDes()
	if err := st.CreateOrder(des, fmtInt(chat), "en", depositProduct, "USDT BEP20", 0, 500000); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	settled, err := b.settlePaid(ctx, des)
	if err != nil || !settled {
		t.Fatalf("settlePaid: settled=%v err=%v", settled, err)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "💰 NEW DEPOSIT") {
		t.Fatalf("owner alert = %v, want a 💰 NEW DEPOSIT message", alert)
	}
	if !strings.Contains(alert.Text, "USDT BEP20") {
		t.Errorf("deposit alert = %q, want the order's Tier as the funding-rail label", alert.Text)
	}
}

// TestSettlePaidDepositReplayDoesNotDoubleNotify pins the CARE requirement:
// deposits are idempotent by des/status at the storage layer, and the alert
// must not undo that — a replayed settle on an already-delivered deposit
// order must not produce a second "NEW DEPOSIT" alert.
func TestSettlePaidDepositReplayDoesNotDoubleNotify(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "999")
	const chat = 201

	des := store.NewDes()
	if err := st.CreateOrder(des, fmtInt(chat), "en", depositProduct, "USDT TRC20", 0, 300000); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if settled, err := b.settlePaid(ctx, des); err != nil || !settled {
		t.Fatalf("first settlePaid: settled=%v err=%v", settled, err)
	}
	if n := countContains(f, "💰 NEW DEPOSIT"); n != 1 {
		t.Fatalf("after first settle: NEW DEPOSIT count = %d, want 1", n)
	}

	// Replay: the order is already 'delivered', so ClaimOrder(reserved->paid)
	// loses and settlePaid falls through to its "already handled" no-op tail.
	if settled, err := b.settlePaid(ctx, des); err != nil || settled {
		t.Fatalf("replay settlePaid: settled=%v err=%v, want settled=false (already handled)", settled, err)
	}
	if n := countContains(f, "💰 NEW DEPOSIT"); n != 1 {
		t.Errorf("after replay: NEW DEPOSIT count = %d, want still 1 (no double-alert on a settled replay)", n)
	}
}

// TestSettlePaidLateExpiredDepositNotifiesAdminOwner covers deposit.go:288
// (the late-settled, post-expiry claim branch) — notify_admin_deposit
// (telegram_bot.py:9048-9053).
func TestSettlePaidLateExpiredDepositNotifiesAdminOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "999")
	const chat = 202

	des := store.NewDes()
	if err := st.CreateOrder(des, fmtInt(chat), "en", depositProduct, "USDT BEP20", 0, 400000); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "expired"); err != nil {
		t.Fatalf("claim -> expired: %v", err)
	}
	// settlePaid's `settled` return only reflects the FRESH reserved->paid win
	// (deposit.go:234/249) — the late-expired refund-credit branch always
	// returns (false, nil) regardless of outcome (deposit.go's own "already
	// handled, dedupe" tail), so this path is checked via order status, not
	// the return value.
	if _, err := b.settlePaid(ctx, des); err != nil {
		t.Fatalf("settlePaid(late): err=%v", err)
	}
	if got, _ := st.GetOrder(des); got.Status != "refunded" {
		t.Fatalf("order = %q, want refunded (the late-settled deposit path)", got.Status)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "💰 NEW DEPOSIT") {
		t.Fatalf("owner alert = %v, want a 💰 NEW DEPOSIT message", alert)
	}
}

// TestSettlePaidDepositSendFailureDoesNotBlockCredit is the CARE pin for the
// deposit rails: a Telegram outage on the owner alert (or the customer's own
// Deposit.OK) must never unwind the wallet credit that already committed.
func TestSettlePaidDepositSendFailureDoesNotBlockCredit(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "999")
	f.sendErr = context.DeadlineExceeded
	const chat = 203

	des := store.NewDes()
	if err := st.CreateOrder(des, fmtInt(chat), "en", depositProduct, "USDT TRC20", 0, 250000); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	settled, err := b.settlePaid(ctx, des)
	if err != nil || !settled {
		t.Fatalf("settlePaid under a Telegram outage: settled=%v err=%v, want settled=true (credit must complete regardless)", settled, err)
	}
	if got, _ := st.GetOrder(des); got.Status != "delivered" {
		t.Errorf("order = %q, want delivered — a send failure must not roll back the deposit close", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 250000 {
		t.Errorf("balance = %d, want 250000 credited despite the send failure", bal)
	}
}

// kindStr renders a *string failure-message argument safely: RefundKind is a
// *string, and %v on a pointer prints its ADDRESS, not the value it points
// to — a diagnostic that degrades to noise exactly when the regression it
// guards actually fires (carried-over item from the stats-fixes review).
func kindStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// ---- small test helpers shared across the handlers test package ------------

func lastSend(f *fakeTG) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sends) == 0 {
		return ""
	}
	return f.Sends[len(f.Sends)-1].Text
}

// docCount returns the number of recorded sendDocument calls under the fake's
// lock (a worker goroutine may append concurrently).
func docCount(f *fakeTG) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Docs)
}

// countContains counts sent messages whose text contains sub.
func countContains(f *fakeTG, sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.Sends {
		if strings.Contains(s.Text, sub) {
			n++
		}
	}
	return n
}
