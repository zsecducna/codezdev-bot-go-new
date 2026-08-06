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
