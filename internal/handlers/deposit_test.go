package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/sepay"
)

// fakeSepay stands up an httptest server that mimics SePay's transactions/list
// endpoint and points sepay.TxnListURL at it for the duration of the test, so the
// deposit/order-pay workers exercise the REAL sepay.FindMatchingTxn path without
// touching the live API. txnsFor(reqN) returns the transaction rows for the Nth
// poll (1-based), letting a test script an underpay-then-full sequence. NOT
// parallel-safe (mutates a package global) — callers must not t.Parallel().
func fakeSepay(t *testing.T, txnsFor func(reqN int) []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	req := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		req++
		n := req
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages":     map[string]any{"success": true},
			"transactions": txnsFor(n),
		})
	}))
	old := sepay.TxnListURL
	sepay.TxnListURL = ts.URL
	t.Cleanup(func() { sepay.TxnListURL = old; ts.Close() })
}

// txnRow builds one SePay transaction row (id as a JSON number, content carrying
// the des, amount_in as SePay's numeric string).
func txnRow(id int, content, amountIn string) map[string]any {
	return map[string]any{"id": id, "transaction_content": content, "amount_in": amountIn}
}

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

// TestDepositCreditOnce: the SePay poll credits the wallet exactly once and a
// re-run over the SAME txn never double-credits (store dedupe by txn id). Also
// pins that a QR deposit renders the VietQR image and NO order row is created.
func TestDepositCreditOnce(t *testing.T) {
	b, f, st, _, _ := newBuyBot(t, "999")
	b.depositPoll = 2 * time.Millisecond
	b.expiryTimeout = 2 * time.Second
	const chat = 100
	const des = "deadbeefdeadbeefdeadbeefdeadbeef"
	// Every poll reports the same matching transfer of 50000 VND. polls counts
	// served requests so the second phase can wait for a real poll instead of
	// guessing a sleep.
	var pmu sync.Mutex
	polls := 0
	pollsSeen := func() int { pmu.Lock(); defer pmu.Unlock(); return polls }
	fakeSepay(t, func(int) []map[string]any {
		pmu.Lock()
		polls++
		pmu.Unlock()
		return []map[string]any{txnRow(1, "NAP TIEN "+des, "50000")}
	})

	// Drive the worker on a FIXED des so a re-run hits the same txn id (the
	// duplicate-credit guard).
	b.depositWorker(chat, i18n.EN, des)

	// Wait on the CONFIRMATION, not the balance: depositWorker credits first and
	// sends after (never announce money that isn't persisted), so a balance-only
	// wait races the send and can sample the gap between the two.
	if !waitFor(t, func() bool { return countContains(f, "Deposit successful") == 1 }) {
		t.Fatalf("DEPOSIT_OK sent %d times, want 1", countContains(f, "Deposit successful"))
	}
	// Send implies the credit already landed.
	if bal, _ := st.GetBalance("100"); bal != 50000 {
		t.Fatalf("balance after deposit = %d, want 50000", bal)
	}

	// Re-run the worker over the SAME txn: Credit dedupes -> no second credit.
	sendsBefore := f.sendCount()
	pollsBefore := pollsSeen()
	b.depositWorker(chat, i18n.EN, des)
	// Wait until the re-run actually polled and had time to attempt the dup
	// credit, rather than sleeping a fixed guess.
	if !waitFor(t, func() bool { return pollsSeen() > pollsBefore }) {
		t.Fatalf("re-run never polled SePay")
	}
	// The poll returned a match, so the worker is now past its stop-channel
	// select and committed to Credit-then-return; draining the registry joins it
	// deterministically instead of sleeping a guess.
	b.workers.Shutdown()
	if bal, _ := st.GetBalance("100"); bal != 50000 {
		t.Errorf("balance after duplicate txn = %d, want 50000 (credited once)", bal)
	}
	if f.sendCount() != sendsBefore {
		t.Errorf("duplicate txn must not re-send DEPOSIT_OK")
	}
	// No order row for a QR deposit (Python parity).
	if o, _ := st.GetOrder(des); o != nil {
		t.Errorf("QR deposit must not create an order row, got %+v", o)
	}
}

// TestDepositQRRender: dep:qr renders the VietQR image with the des in its
// caption and starts a background poll (no order row created).
func TestDepositQRRender(t *testing.T) {
	b, f, st, _, _ := newBuyBot(t, "999")
	b.depositPoll = 2 * time.Millisecond
	b.expiryTimeout = 20 * time.Millisecond
	const chat = 100
	fakeSepay(t, func(int) []map[string]any { return nil }) // never matches -> times out

	b.startDepositQR(context.Background(), chat, i18n.EN)

	if len(f.Photos) != 1 {
		t.Fatalf("QR deposit must render exactly one VietQR photo, got %d", len(f.Photos))
	}
	if !strings.Contains(f.Photos[0].Caption, "Transfer content") {
		t.Errorf("QR caption = %q, want the DEPOSIT_QR body", f.Photos[0].Caption)
	}
	// Drain the poll worker (it will time out fast) and confirm nothing credited.
	waitFor(t, func() bool { return f.sendCount() > 0 })
	if bal, _ := st.GetBalance("100"); bal != 0 {
		t.Errorf("no credit before payment, balance = %d", bal)
	}
}

// TestDepositTimeout: an exhausted poll window sends the support-contact notice
// and credits nothing.
func TestDepositTimeout(t *testing.T) {
	b, f, st, _, _ := newBuyBot(t, "999")
	b.depositPoll = 2 * time.Millisecond
	b.expiryTimeout = 30 * time.Millisecond
	const chat = 100
	fakeSepay(t, func(int) []map[string]any { return nil }) // never matches

	b.depositWorker(chat, i18n.EN, "cafecafecafecafecafecafecafecafe")

	if !waitFor(t, func() bool {
		return f.sendCount() > 0 && lastSend(f) == i18n.C().Buy.DepositTimeout.EN
	}) {
		t.Fatalf("timeout notice not sent; last=%q", lastSend(f))
	}
	if bal, _ := st.GetBalance("100"); bal != 0 {
		t.Errorf("no credit on timeout, balance = %d", bal)
	}
}

// TestOrderPayUnderpayThenFull is the pinned test (port of test_payment_amount.py):
// an UNDERPAID transfer is banked to the wallet and never delivers; a later FULL
// transfer delivers exactly once; the full payment does NOT touch the wallet.
func TestOrderPayUnderpayThenFull(t *testing.T) {
	ctx := context.Background()
	b, f, st, stk, dir := newBuyBot(t, "999")
	b.depositPoll = 2 * time.Millisecond
	b.expiryTimeout = 2 * time.Second
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1) // total 250000
	o, _ := st.ReservedOrderForChat("100")
	if o == nil {
		t.Fatal("expected a reserved order")
	}
	des := o.Des

	// Poll 1 shows an underpayment (100000 < 250000). Later polls show BOTH the
	// (now-consumed) underpay and a sufficient transfer; FindMatchingTxn skips the
	// used underpay and settles on the full one.
	fakeSepay(t, func(n int) []map[string]any {
		if n <= 1 {
			return []map[string]any{txnRow(11, "TT "+des, "100000")}
		}
		return []map[string]any{
			txnRow(22, "TT "+des, "250000"),
			txnRow(11, "TT "+des, "100000"),
		}
	})

	b.Process(ctx, callbackUpdate(chat, "payqr:"+des)) // renders QR + starts worker

	// Wait for the delivery FILE (sent after the status flip); then drain the
	// worker so every field is stable before asserting.
	if !waitFor(t, func() bool { return docCount(f) == 1 }) {
		g, _ := st.GetOrder(des)
		t.Fatalf("order not delivered; status=%v docs=%d", g, docCount(f))
	}
	b.Shutdown()
	if g, _ := st.GetOrder(des); g == nil || g.Status != "delivered" {
		t.Fatalf("order not delivered; status=%v", g)
	}
	// Delivered exactly once (one order-file send).
	if docCount(f) != 1 {
		t.Errorf("order delivered %d times, want exactly 1", docCount(f))
	}
	if sold := stk.CountSold("prodA"); sold != 1 {
		t.Errorf("sold count = %d, want 1", sold)
	}
	// The wallet holds ONLY the banked underpayment (100000) — the full order-pay
	// never credits the wallet.
	if bal, _ := st.GetBalance("100"); bal != 100000 {
		t.Errorf("balance = %d, want 100000 (underpay banked, full pay NOT credited)", bal)
	}
	// QR_UNDERPAID sent once; QR image rendered.
	if c := countContains(f, "100,000"); c != 1 {
		t.Errorf("QR_UNDERPAID sent %d times, want 1", c)
	}
	if len(f.Photos) != 1 {
		t.Errorf("order-pay must render the amount-pinned QR once, got %d", len(f.Photos))
	}
}

// TestOrderPayFullNoBalanceCredit: a single full transfer delivers and leaves the
// wallet untouched (order-pay is not a deposit).
func TestOrderPayFullNoBalanceCredit(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	b.depositPoll = 2 * time.Millisecond
	b.expiryTimeout = 2 * time.Second
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat("100")
	des := o.Des
	fakeSepay(t, func(int) []map[string]any {
		return []map[string]any{txnRow(33, "PAY "+des, "250000")}
	})

	b.Process(ctx, callbackUpdate(chat, "payqr:"+des))

	if !waitFor(t, func() bool { return docCount(f) == 1 }) {
		g, _ := st.GetOrder(des)
		t.Fatalf("order not delivered; status=%v docs=%d", g, docCount(f))
	}
	b.Shutdown() // drain the worker before reading shared state
	if g, _ := st.GetOrder(des); g == nil || g.Status != "delivered" {
		t.Fatalf("order not delivered; status=%v", g)
	}
	if docCount(f) != 1 {
		t.Errorf("delivered %d times, want 1", docCount(f))
	}
	if bal, _ := st.GetBalance("100"); bal != 0 {
		t.Errorf("order-pay must NOT credit the wallet, balance = %d", bal)
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

	// Valid amount consumes the slot (routes to the M2.4 method stub).
	b.Process(ctx, textUpdate(chat, "5"))
	if b.states.Get(chat).Deposit != nil {
		t.Error("valid amount must consume pending_deposit")
	}
}

// ---- small test helpers ----------------------------------------------------

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
