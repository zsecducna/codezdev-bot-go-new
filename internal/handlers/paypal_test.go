package handlers

// paypal_test.go — the M2.4 PayPal rail tests. The security-relevant pins port
// tests/test_payment_amount.py::test_paypal_amount_and_custom_id_gate plus the
// listener rules from analysis-features.md §6 item 6:
//
//   - a signature-invalid POST is rejected and settles NOTHING,
//   - a signature-valid, sufficient capture settles exactly once,
//   - a REPLAYED webhook cannot double-deliver (the reserved->paid CAS),
//   - an insufficient captured USD never settles,
//   - an event custom_id that disagrees with OUR paypal_order_id map is ignored,
//   - PayPal unconfigured -> no port is bound at all,
//   - a __deposit__ order credits the wallet and delivers nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/paypal"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
)

// fakePaypal replaces the live PayPal client seams on a Bot. verifyOK decides
// the webhook signature verdict; capture is what CaptureOrder returns. Counters
// let a test assert "captured exactly once" across a replay.
type fakePaypal struct {
	verifyOK    atomic.Bool
	verifyErr   error
	capture     paypal.CaptureResult
	captureErr  error
	createErr   error
	captureHits atomic.Int32
	createHits  atomic.Int32

	mu       sync.Mutex
	lastDes  string // custom_id passed to CreateOrder
	lastDesc string
}

// newPaypalBot builds a test Bot with the PayPal seams faked and the USD rate
// pinned to 23000 — so 230000 VND is exactly "10.00", the numbers
// test_payment_amount.py pins.
//
// The t.Setenv MUST happen before newBuyBot: config.Load snapshots the process
// env once, and BOTH rate readers have to agree — b.usdtToVND (config-backed,
// converts the typed deposit amount) and paypal.VndToUSD (process-env-backed,
// used by CreateOrder to charge and by the webhook gate to check).
func newPaypalBot(t *testing.T) (*Bot, *fakeTG, *store.Store, *stock.Store, string, *fakePaypal) {
	t.Helper()
	t.Setenv("USD_TO_VND", "23000")
	b, f, st, stk, dir := newBuyBot(t, "999")
	return b, f, st, stk, dir, armPaypal(t, b)
}

// armPaypal installs the fake PayPal client seams on b and returns the fake.
// paypalVndToUSD is deliberately NOT faked: the real converter is what decides
// both the charge and the amount gate, so the tests exercise it.
func armPaypal(t *testing.T, b *Bot) *fakePaypal {
	t.Helper()
	fp := &fakePaypal{}
	fp.verifyOK.Store(true)
	b.paypalConfigured = func() bool { return true }
	b.paypalCreateOrder = func(vndTotal int64, des, description string) (string, string, error) {
		fp.createHits.Add(1)
		fp.mu.Lock()
		fp.lastDes, fp.lastDesc = des, description
		fp.mu.Unlock()
		if fp.createErr != nil {
			return "", "", fp.createErr
		}
		return "PPORDER", "https://paypal.example/approve/PPORDER", nil
	}
	b.paypalCaptureOrder = func(string) (paypal.CaptureResult, error) {
		fp.captureHits.Add(1)
		return fp.capture, fp.captureErr
	}
	b.paypalVerifyWebhook = func(http.Header, []byte) (bool, error) {
		return fp.verifyOK.Load(), fp.verifyErr
	}
	return fp
}

// approvedEvent is the CHECKOUT.ORDER.APPROVED body PayPal posts for order id.
func approvedEvent(orderID string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"event_type": "CHECKOUT.ORDER.APPROVED",
		"resource":   map[string]any{"id": orderID},
	})
	return raw
}

// postWebhook drives the REAL handler over a real HTTP round trip (httptest), so
// the ack-then-process ordering and the status codes are exercised end to end.
func postWebhook(t *testing.T, b *Bot, body []byte) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.paypalWebhookHandler))
	defer srv.Close()
	resp, err := http.Post(srv.URL+webhookPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("webhook POST: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// reservedOrder seeds a product and a reserved order for `chat`, returning its
// des. Total is 230000 VND == $10.00 at the pinned rate.
func reservedOrder(t *testing.T, b *Bot, st *store.Store, dir string, chat int64) string {
	t.Helper()
	seedProduct(t, dir, "prodA", 230000, 5)
	b.finalizePurchase(context.Background(), chat, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat(fmtInt(chat))
	if o == nil {
		t.Fatal("expected a reserved order")
	}
	return o.Des
}

// ---- order pay (paypp:) ----------------------------------------------------

// TestPaypalOrderPayLink: paypp: creates the PayPal order with custom_id = des,
// PERSISTS the reverse map, and sends PAYPAL_LINK with the USD amount + approve
// URL (telegram_bot.py:15869-15897).
func TestPaypalOrderPayLink(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)

	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	if fp.createHits.Load() != 1 {
		t.Fatalf("CreateOrder called %d times, want 1", fp.createHits.Load())
	}
	fp.mu.Lock()
	gotDes, gotDesc := fp.lastDes, fp.lastDesc
	fp.mu.Unlock()
	if gotDes != des {
		t.Errorf("custom_id = %q, want the order des %q", gotDes, des)
	}
	if gotDesc != "1x prodA (KBH)" {
		t.Errorf("description = %q, want %q", gotDesc, "1x prodA (KBH)")
	}
	// The reverse map is what the webhook trusts — it must be persisted.
	mapped, err := st.OrderDesForPaypal("PPORDER")
	if err != nil || mapped != des {
		t.Errorf("OrderDesForPaypal = (%q,%v), want %q", mapped, err, des)
	}
	last := lastSend(f)
	if !strings.Contains(last, "10.00") || !strings.Contains(last, "https://paypal.example/approve/PPORDER") {
		t.Errorf("PAYPAL_LINK message = %q, want the USD amount + approve URL", last)
	}
}

// TestPaypalOrderPayForeignChat: the ownership guard — another chat tapping a
// leaked des gets ORDER_GONE and no PayPal order is created (@15875-15879).
func TestPaypalOrderPayForeignChat(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	des := reservedOrder(t, b, st, dir, 100)

	b.Process(ctx, callbackUpdate(200, "paypp:"+des)) // a DIFFERENT chat

	if fp.createHits.Load() != 0 {
		t.Errorf("a foreign chat must not create a PayPal order (hits=%d)", fp.createHits.Load())
	}
	if lastSend(f) != i18n.C().Buy.OrderGone.EN {
		t.Errorf("reply = %q, want ORDER_GONE", lastSend(f))
	}
}

// TestPaypalOrderPayCreateFails: a PayPal API failure shows PAYPAL_ERROR and,
// unlike the deposit stub, LEAVES the order reserved so balance/QR still work.
func TestPaypalOrderPayCreateFails(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	fp.createErr = errors.New("paypal down")
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)

	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	if lastSend(f) != i18n.C().Buy.PaypalError.EN {
		t.Errorf("reply = %q, want PAYPAL_ERROR", lastSend(f))
	}
	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("order must stay reserved after a PayPal failure, got %+v", o)
	}
}

// ---- webhook gates ---------------------------------------------------------

// TestPaypalWebhookInvalidSignature: an unverified POST is answered 400, the
// body is never acted on, and nothing settles. This is the fail-closed rule —
// it also covers an unset PAYPAL_WEBHOOK_ID, which VerifyWebhook reports as
// (false, nil).
func TestPaypalWebhookInvalidSignature(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", CustomID: "", AmountUSD: "10.00"}
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	fp.verifyOK.Store(false)
	if code := postWebhook(t, b, approvedEvent("PPORDER")); code != http.StatusBadRequest {
		t.Fatalf("unverified webhook status = %d, want 400", code)
	}
	b.Shutdown() // drain anything that (wrongly) got spawned
	if fp.captureHits.Load() != 0 {
		t.Errorf("an unverified event must never reach capture (hits=%d)", fp.captureHits.Load())
	}
	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("unverified webhook settled the order: %+v", o)
	}
	if docCount(f) != 0 {
		t.Errorf("unverified webhook delivered %d files, want 0", docCount(f))
	}
}

// TestPaypalWebhookVerifyErrorFailsClosed: a verification that ERRORS (PayPal
// unreachable) is treated exactly like a failed verification.
func TestPaypalWebhookVerifyErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, dir, fp := newPaypalBot(t)
	fp.verifyErr = errors.New("verify endpoint down")
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	if code := postWebhook(t, b, approvedEvent("PPORDER")); code != http.StatusBadRequest {
		t.Fatalf("verify-error webhook status = %d, want 400", code)
	}
	b.Shutdown()
	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("verify-error webhook settled the order: %+v", o)
	}
}

// TestPaypalWebhookSettlesOnceAndReplayIsSafe: the happy path settles the order
// exactly once, and a REPLAYED identical webhook does not double-deliver — the
// reserved->paid CAS in settlePaid is the only guard needed.
func TestPaypalWebhookSettlesOnceAndReplayIsSafe(t *testing.T) {
	ctx := context.Background()
	b, f, st, stk, dir, fp := newPaypalBot(t)
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", CustomID: "", AmountUSD: "10.00"}
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	if code := postWebhook(t, b, approvedEvent("PPORDER")); code != http.StatusOK {
		t.Fatalf("verified webhook status = %d, want 200 (ack before processing)", code)
	}
	if !waitFor(t, func() bool { return docCount(f) == 1 }) {
		o, _ := st.GetOrder(des)
		t.Fatalf("order not delivered; status=%+v docs=%d", o, docCount(f))
	}
	// Replay the exact same event.
	if code := postWebhook(t, b, approvedEvent("PPORDER")); code != http.StatusOK {
		t.Fatalf("replayed webhook status = %d, want 200", code)
	}
	b.Shutdown() // drain both async handlers before asserting

	if o, _ := st.GetOrder(des); o == nil || o.Status != "delivered" {
		t.Fatalf("order status = %+v, want delivered", o)
	}
	if docCount(f) != 1 {
		t.Errorf("delivered %d times, want exactly 1 (replay must not re-deliver)", docCount(f))
	}
	if sold := stk.CountSold("prodA"); sold != 1 {
		t.Errorf("sold count = %d, want 1", sold)
	}
	// A PayPal ORDER pay is not a deposit: the wallet must stay untouched.
	if bal, _ := st.GetBalance("100"); bal != 0 {
		t.Errorf("order pay must NOT credit the wallet, balance = %d", bal)
	}
}

// TestPaypalWebhookUnderpaidNeverSettles is the ported amount pin: a capture of
// $5.00 against a $10.00 order delivers nothing and leaves the order reserved.
func TestPaypalWebhookUnderpaidNeverSettles(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", CustomID: "DESpp", AmountUSD: "5.00"}
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))
	// The capture's custom_id must agree with our map, so the ONLY thing under
	// test here is the amount gate.
	fp.capture.CustomID = des

	postWebhook(t, b, approvedEvent("PPORDER"))
	b.Shutdown()

	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("underpaid capture settled the order: %+v", o)
	}
	if docCount(f) != 0 {
		t.Errorf("underpaid capture delivered %d files, want 0", docCount(f))
	}
}

// TestPaypalWebhookCustomIDSpoofIgnored: an event whose custom_id names ANOTHER
// order than our persisted map settles nothing — our map is the trusted side.
func TestPaypalWebhookCustomIDSpoofIgnored(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))
	// Sufficient money, but the capture claims a different internal order.
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", CustomID: "SOMEONE-ELSES-DES", AmountUSD: "10.00"}

	postWebhook(t, b, approvedEvent("PPORDER"))
	b.Shutdown()

	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("custom_id mismatch settled the order: %+v", o)
	}
	if docCount(f) != 0 {
		t.Errorf("custom_id mismatch delivered %d files, want 0", docCount(f))
	}
}

// TestPaypalWebhookNotCompletedNeverSettles: a non-COMPLETED capture (declined /
// pending) settles nothing (the fourth Python pin).
func TestPaypalWebhookNotCompletedNeverSettles(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, dir, fp := newPaypalBot(t)
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))
	fp.capture = paypal.CaptureResult{Status: "DECLINED", CustomID: des, AmountUSD: "10.00"}

	postWebhook(t, b, approvedEvent("PPORDER"))
	b.Shutdown()

	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("DECLINED capture settled the order: %+v", o)
	}
}

// TestPaypalWebhookUnknownOrderIgnored: a verified event for a PayPal order id
// we never issued (and whose custom_id we don't know) settles nothing.
func TestPaypalWebhookUnknownOrderIgnored(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", CustomID: "", AmountUSD: "10.00"}

	postWebhook(t, b, approvedEvent("NOT-OUR-ORDER"))
	b.Shutdown()

	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("unknown PayPal order settled something: %+v", o)
	}
	if docCount(f) != 0 {
		t.Errorf("unknown PayPal order delivered %d files, want 0", docCount(f))
	}
}

// TestPaypalCaptureCompletedEventShape covers the second event type
// (PAYMENT.CAPTURE.COMPLETED): amount + custom_id come off the resource and the
// order id off supplementary_data.related_ids — no capture call is made.
func TestPaypalCaptureCompletedEventShape(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir, fp := newPaypalBot(t)
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	raw, _ := json.Marshal(map[string]any{
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": map[string]any{
			"id":        "CAPTUREID",
			"amount":    map[string]any{"value": "10.00", "currency_code": "usd"},
			"custom_id": des,
			"supplementary_data": map[string]any{
				"related_ids": map[string]any{"order_id": "PPORDER"},
			},
		},
	})
	postWebhook(t, b, raw)

	if !waitFor(t, func() bool { return docCount(f) == 1 }) {
		o, _ := st.GetOrder(des)
		t.Fatalf("capture-completed event did not deliver; status=%+v", o)
	}
	b.Shutdown()
	if fp.captureHits.Load() != 0 {
		t.Errorf("PAYMENT.CAPTURE.COMPLETED must not re-capture (hits=%d)", fp.captureHits.Load())
	}
}

// TestPaypalWebhookNonUSDRejected: a capture in another currency never settles
// (the VND total is only ever converted to USD).
func TestPaypalWebhookNonUSDRejected(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, dir, _ := newPaypalBot(t)
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	raw, _ := json.Marshal(map[string]any{
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": map[string]any{
			"amount":    map[string]any{"value": "10.00", "currency_code": "EUR"},
			"custom_id": des,
			"supplementary_data": map[string]any{
				"related_ids": map[string]any{"order_id": "PPORDER"},
			},
		},
	})
	postWebhook(t, b, raw)
	b.Shutdown()

	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("non-USD capture settled the order: %+v", o)
	}
}

// ---- deposit rail (dep:pp) --------------------------------------------------

// TestPaypalDepositCreditsWalletWithoutDelivering: the full wallet top-up rail —
// dep:pp, the amount capture, the __deposit__ stub order, then a verified
// webhook that CREDITS the balance and delivers nothing.
func TestPaypalDepositCreditsWalletWithoutDelivering(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _, fp := newPaypalBot(t)
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", AmountUSD: "10.00"}
	const chat = 100
	primeKB(b, chat)

	b.Process(ctx, callbackUpdate(chat, "dep:pp")) // arms the amount prompt
	b.Process(ctx, textUpdate(chat, "10"))         // 10 USDT -> 230000 VND
	if fp.createHits.Load() != 1 {
		t.Fatalf("deposit must create exactly one PayPal order (hits=%d)", fp.createHits.Load())
	}
	fp.mu.Lock()
	des, desc := fp.lastDes, fp.lastDesc
	fp.mu.Unlock()
	if desc != "Wallet top-up" {
		t.Errorf("deposit description = %q, want %q", desc, "Wallet top-up")
	}
	o, _ := st.GetOrder(des)
	if o == nil || o.Product != depositProduct || o.Status != "reserved" || o.Total != 230000 {
		t.Fatalf("deposit stub order = %+v, want a reserved __deposit__ order of 230000", o)
	}
	if last := lastSend(f); !strings.Contains(last, "deposit 10.00 USDT") {
		t.Errorf("DEPOSIT_PP_LINK message = %q", last)
	}

	postWebhook(t, b, approvedEvent("PPORDER"))
	if !waitFor(t, func() bool { bal, _ := st.GetBalance("100"); return bal == 230000 }) {
		bal, _ := st.GetBalance("100")
		t.Fatalf("balance after PayPal deposit = %d, want 230000", bal)
	}
	// Replay the SAME event while the bot is still running — after Shutdown the
	// worker registry refuses new work, which would make this assertion vacuous.
	// The second capture must lose the reserved->paid CAS and credit nothing.
	if code := postWebhook(t, b, approvedEvent("PPORDER")); code != http.StatusOK {
		t.Fatalf("replayed webhook status = %d, want 200", code)
	}
	if !waitFor(t, func() bool { return fp.captureHits.Load() == 2 }) {
		t.Fatalf("replay never reached capture (hits=%d) — the assertion below would be vacuous",
			fp.captureHits.Load())
	}
	b.Shutdown() // drains BOTH event goroutines before asserting

	if bal, _ := st.GetBalance("100"); bal != 230000 {
		t.Errorf("replayed deposit webhook credited twice: balance = %d", bal)
	}
	if docCount(f) != 0 {
		t.Errorf("a deposit must deliver no accounts, got %d files", docCount(f))
	}
	if g, _ := st.GetOrder(des); g == nil || g.Status != "delivered" {
		t.Errorf("deposit order status = %+v, want delivered (closed)", g)
	}
}

// TestPaypalWebhookRefusesEventWhileShuttingDown: once the worker registry is
// drained, a verified event must NOT be acked 200 — a 2xx stops PayPal retrying
// and would permanently lose an already-captured payment. It must answer 503 so
// the next process receives the redelivery.
func TestPaypalWebhookRefusesEventWhileShuttingDown(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, dir, fp := newPaypalBot(t)
	fp.capture = paypal.CaptureResult{Status: "COMPLETED", AmountUSD: "10.00"}
	const chat = 100
	des := reservedOrder(t, b, st, dir, chat)
	b.Process(ctx, callbackUpdate(chat, "paypp:"+des))

	b.Shutdown() // the process is going down

	if code := postWebhook(t, b, approvedEvent("PPORDER")); code != http.StatusServiceUnavailable {
		t.Fatalf("webhook during shutdown = %d, want 503 so PayPal retries", code)
	}
	if fp.captureHits.Load() != 0 {
		t.Errorf("refused event must not be processed (capture hits=%d)", fp.captureHits.Load())
	}
	if o, _ := st.GetOrder(des); o == nil || o.Status != "reserved" {
		t.Errorf("refused event settled the order: %+v", o)
	}
}

// TestPaypalDepositCreateFailureAbandonsStub: when PayPal refuses, the stub
// order is expired (so pending_guard doesn't wedge the customer) and
// PAYPAL_ERROR is shown (telegram_bot.py:10673-10677).
func TestPaypalDepositCreateFailureAbandonsStub(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _, fp := newPaypalBot(t)
	fp.createErr = errors.New("paypal down")
	const chat = 100
	primeKB(b, chat)

	b.Process(ctx, callbackUpdate(chat, "dep:pp"))
	b.Process(ctx, textUpdate(chat, "10"))

	if lastSend(f) != i18n.C().Buy.PaypalError.EN {
		t.Errorf("reply = %q, want PAYPAL_ERROR", lastSend(f))
	}
	fp.mu.Lock()
	des := fp.lastDes
	fp.mu.Unlock()
	if o, _ := st.GetOrder(des); o == nil || o.Status != "expired" {
		t.Errorf("failed deposit stub = %+v, want status expired", o)
	}
}

// ---- listener lifecycle ------------------------------------------------------

// TestPaypalWebhookServerSkippedWhenUnconfigured: a reseller instance with no
// PayPal keys must bind NO port (no 8080 clash across instances).
func TestPaypalWebhookServerSkippedWhenUnconfigured(t *testing.T) {
	b, _, _, _, _, _ := newPaypalBot(t)
	b.paypalConfigured = func() bool { return false }
	b.paypalWebhookPort = "0"

	addr, err := b.StartPaypalWebhook(context.Background())
	if err != nil {
		t.Fatalf("unconfigured start returned an error: %v", err)
	}
	if addr != nil {
		t.Fatalf("unconfigured PayPal must bind no port, got %v", addr)
	}
}

// TestPaypalWebhookServerLifecycle: when configured, the listener binds, serves
// the real handler at /paypal/webhook (404 elsewhere, 405 on GET), and stops on
// context cancellation.
func TestPaypalWebhookServerLifecycle(t *testing.T) {
	b, _, _, _, _, fp := newPaypalBot(t)
	fp.verifyOK.Store(false) // reject bodies; this test is about the listener
	b.paypalWebhookHost = "127.0.0.1"
	b.paypalWebhookPort = "0" // ephemeral

	ctx, cancel := context.WithCancel(context.Background())
	addr, err := b.StartPaypalWebhook(ctx)
	if err != nil {
		t.Fatalf("StartPaypalWebhook: %v", err)
	}
	if addr == nil {
		t.Fatal("configured PayPal must bind a port")
	}
	base := "http://" + addr.String()

	resp, err := http.Post(base+webhookPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unverified POST = %d, want 400", resp.StatusCode)
	}
	// Wrong path -> 404 (Python's rstrip("/") path check).
	if r, err := http.Post(base+"/nope", "application/json", strings.NewReader("{}")); err == nil {
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("wrong path = %d, want 404", r.StatusCode)
		}
	}
	// GET on the right path -> 405 (the Python listener only implements POST).
	if r, err := http.Get(base + webhookPath); err == nil {
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET = %d, want 405", r.StatusCode)
		}
	}

	cancel()
	// The listener must stop accepting; b.Shutdown() then drains its goroutine.
	if !waitFor(t, func() bool {
		c, derr := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
		if derr != nil {
			return true
		}
		c.Close()
		return false
	}) {
		t.Error("listener still accepting after context cancellation")
	}
	b.Shutdown() // must not hang
}

// ---- amount parsing ----------------------------------------------------------

// TestUSDCents pins the integer-cent parser that replaced Python's Decimal: it
// must be exact, must truncate (never round up) extra precision, and must fail
// CLOSED on anything it can't read.
func TestUSDCents(t *testing.T) {
	ok := map[string]int64{
		"0.00": 0, "10.00": 1000, "9.99": 999, "10": 1000,
		".5": 50, "0.999": 99, "+3.50": 350, " 10.00 ": 1000,
	}
	for in, want := range ok {
		got, valid := usdCents(in)
		if !valid || got != want {
			t.Errorf("usdCents(%q) = (%d,%t), want (%d,true)", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "abc", "1O.00", "-5.00", "10,00", "1e3"} {
		if got, valid := usdCents(in); valid {
			t.Errorf("usdCents(%q) = (%d,true), want invalid (fail closed)", in, got)
		}
	}
}
