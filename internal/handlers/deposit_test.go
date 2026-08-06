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
		t.Errorf("dep RefundKind = %v, want %q", depOrder.RefundKind, store.RefundKindLateSettled)
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
		t.Errorf("prod RefundKind = %v, want %q — a late payment on a non-deposit "+
			"expired order must NOT be tagged late_settled, or it becomes invisible "+
			"on every refund-kind dashboard bucket",
			prodOrder.RefundKind, store.RefundKindOutOfStock)
	}
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
