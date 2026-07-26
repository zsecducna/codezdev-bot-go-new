package handlers

// paypal.go — the M2.4 PayPal rail (international / USD payments). Ports:
//   - start_paypal_deposit @10661  (dep:pp — wallet top-up via PayPal),
//   - the paypp: callback @15869   (order pay via PayPal),
//   - _handle_paypal_event @19166  (webhook event -> capture -> amount gate -> settle),
//   - start_paypal_webhook_server @19242 + its boot gate @19437.
//
// Money invariants this file must hold (CLAUDE.md, analysis-features.md §6.6):
//   1. The webhook signature is verified BEFORE anything in the body is trusted,
//      and verification FAILS CLOSED (an unset PAYPAL_WEBHOOK_ID verifies nothing).
//   2. The internal order is resolved from OUR persisted orders.paypal_order_id
//      map — never from the event's attacker-influenceable custom_id.
//   3. The captured USD must cover the order's expected USD before we settle, so
//      neither an underpayment nor a replayed cross-order event can deliver.
//   4. Settlement goes through the ONE shared chokepoint (settlePaid -> the
//      reserved->paid CAS + deliverPaid, or the __deposit__ wallet-credit branch),
//      so a replayed webhook can never double-deliver or double-credit.
//
// Binance / BEP20 / TRON deposit + order-pay rails are a separate slice; their
// stubs in deposit.go / callback.go are untouched here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
)

// webhookPath is the URL path PayPal POSTs events to — hardcoded exactly like
// the Python listener (telegram_bot.py:19260 `self.path.rstrip("/") != "/paypal/webhook"`).
const webhookPath = "/paypal/webhook"

// maxWebhookBody caps how much of a webhook POST we read. Python read exactly
// Content-Length bytes from an unauthenticated socket; a Go LimitReader adds a
// hard ceiling so a hostile POST can't balloon memory before signature checking.
// PayPal events are a few KB at most.
const maxWebhookBody = 1 << 20 // 1 MiB

// webhookShutdownGrace bounds how long a graceful listener Shutdown waits for
// in-flight requests. Our handler acks in milliseconds (the capture round-trip
// runs on a separate tracked goroutine), so this only ever covers a slow socket.
const webhookShutdownGrace = 5 * time.Second

// ---- dep:pp — PayPal wallet top-up (start_paypal_deposit @10661) -----------

// startPaypalDeposit creates a PayPal order that CREDITS the wallet by `vnd` on
// completion — start_paypal_deposit (telegram_bot.py:10661-10685).
//
// Persisted as an order with product=__deposit__ and status 'reserved' so the
// existing PayPal webhook path finds it; settlePaid routes the sentinel product
// to CreditBalance instead of delivering accounts. No stock is reserved (a
// deposit buys no accounts), which is why isNoStockProduct covers __deposit__.
func (b *Bot) startPaypalDeposit(ctx context.Context, chatID int64, lang i18n.Lang, vnd int64) {
	des := store.NewDes()
	if err := b.store.CreateOrder(des, fmtInt(chatID), string(lang), depositProduct, "PayPal", 0, vnd); err != nil {
		log.Println("paypal deposit create_order (db) error:", err)
		b.send(ctx, chatID, i18n.C().Buy.PaypalError.T(lang), nil)
		return
	}
	// Deviation (safe): Python computes the display USD only AFTER the PayPal
	// order exists (@10683). VndToUSD is a pure function of (vnd, USD_TO_VND) and
	// paypal.CreateOrder calls it internally FIRST, so a bad/missing rate makes
	// both fail identically — computing it up front just turns an unreachable
	// post-create error into the same PAYPAL_ERROR, with no PayPal order minted.
	usd, err := b.paypalVndToUSD(vnd)
	if err != nil {
		log.Println("paypal deposit rate error:", err)
		b.abandonPaypalStub(des)
		b.send(ctx, chatID, i18n.C().Buy.PaypalError.T(lang), nil)
		return
	}
	ppOrderID, approve, err := b.paypalCreateOrder(vnd, des, "Wallet top-up")
	if err != nil {
		log.Println("paypal deposit create_order error:", err)
		b.abandonPaypalStub(des) // @10674: abandon the stub order
		b.send(ctx, chatID, i18n.C().Buy.PaypalError.T(lang), nil)
		return
	}
	if approve == "" { // no approve link -> the customer has nothing to tap (@10678)
		b.abandonPaypalStub(des)
		b.send(ctx, chatID, i18n.C().Buy.PaypalError.T(lang), nil)
		return
	}
	// Persisted reverse-map: the webhook resolves `des` from this, never from the
	// event's custom_id (invariant 2).
	if err := b.store.SetPaypalOrder(des, ppOrderID); err != nil {
		// Without the map a completed payment could not be tied back to this
		// order, so refuse to hand out a payable link at all.
		log.Println("paypal deposit set_paypal_order failed:", err)
		b.abandonPaypalStub(des)
		b.send(ctx, chatID, i18n.C().Buy.PaypalError.T(lang), nil)
		return
	}
	b.send(ctx, chatID, i18n.F(i18n.C().Deposit.PPLink.T(lang), "usdt", usd, "url", approve), nil)
}

// abandonPaypalStub releases a deposit stub order whose PayPal side failed —
// `store.claim_order(des, "reserved", "expired")` (telegram_bot.py:10674). A
// deposit reserves no stock, so expiring the row is the whole cleanup; it also
// clears pending_guard for the customer's next attempt.
func (b *Bot) abandonPaypalStub(des string) {
	if _, err := b.store.ClaimOrder(des, "reserved", "expired"); err != nil {
		log.Printf("paypal deposit stub abandon failed [des=%s]: %v", des, err)
	}
}

// ---- paypp: — PayPal order pay (telegram_bot.py:15869-15897) ---------------

// cbPayPP creates a PayPal order for a reserved PRODUCT order and sends the
// approval link — the paypp: branch (telegram_bot.py:15869-15897). The webhook
// captures + settles through settlePaid, so nothing is delivered here.
//
// Unlike the deposit path this NEVER abandons the order on failure: the customer
// still owns a reserved order they can pay by balance/QR, so a PayPal outage must
// not cancel their checkout (Python parity @15887-15892).
func (b *Bot) cbPayPP(cc *cbCtx) {
	c := &i18n.C().Buy
	des := strings.TrimPrefix(cc.Data, "paypp:")
	o, err := b.store.GetOrder(des)
	// Ownership guard: a `des` can leak (it is shown in QR transfer content), so a
	// payment button must only act for the chat that actually owns the order
	// (@15875-15879).
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, c.OrderGone.T(b.langFor(cc.ChatID, "en")), nil)
		return
	}
	lang := langSafe(o)
	if !b.payEnabled("paypal") { // guard a stale button (@15881)
		b.send(cc.Ctx, cc.ChatID, c.PayMethodOff.T(lang), nil)
		return
	}
	// Same up-front-rate note as startPaypalDeposit: pure function, identical
	// failure surface, no PayPal order minted on a bad rate.
	usd, err := b.paypalVndToUSD(o.Total)
	if err != nil {
		log.Println("paypal rate error:", err)
		b.send(cc.Ctx, cc.ChatID, c.PaypalError.T(lang), nil)
		return
	}
	desc := fmt.Sprintf("%dx %s (%s)", o.Qty, o.Product, o.Tier) // @15885
	ppOrderID, approve, err := b.paypalCreateOrder(o.Total, des, desc)
	if err != nil {
		log.Println("paypal create_order error:", err)
		b.send(cc.Ctx, cc.ChatID, c.PaypalError.T(lang), nil)
		return
	}
	if approve == "" {
		b.send(cc.Ctx, cc.ChatID, c.PaypalError.T(lang), nil)
		return
	}
	if err := b.store.SetPaypalOrder(des, ppOrderID); err != nil { // @15894 persisted reverse-map
		log.Println("paypal set_paypal_order failed:", err)
		b.send(cc.Ctx, cc.ChatID, c.PaypalError.T(lang), nil)
		return
	}
	b.send(cc.Ctx, cc.ChatID, i18n.F(c.PaypalLink.T(lang), "usd", usd, "url", approve), nil)
}

// ---- webhook event handling (_handle_paypal_event @19166) ------------------

// usdCents parses a PayPal USD amount string ("10.00") into integer cents,
// reporting ok=false for anything it cannot read exactly. Integer cents replace
// Python's Decimal comparison (@19232) — binary floats must never decide whether
// a payment covers a price.
//
// Fails CLOSED: an unparseable amount returns ok=false and the caller refuses to
// settle. More than two decimal places are TRUNCATED (never rounded up), so a
// crafted "9.999" reads as 9.99 and still has to clear the gate.
func usdCents(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") {
		return 0, false // negative/absent amounts can never cover a price
	}
	s = strings.TrimPrefix(s, "+")
	intPart, frac, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	// Pad/truncate the fraction to exactly two digits (cents).
	if len(frac) > 2 {
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	digits := intPart + frac
	var cents int64
	for i := 0; i < len(digits); i++ {
		d := digits[i]
		if d < '0' || d > '9' {
			return 0, false
		}
		cents = cents*10 + int64(d-'0')
		if cents > 1<<50 { // absurd magnitude -> refuse rather than overflow
			return 0, false
		}
	}
	return cents, true
}

// paypalEventFacts is what one webhook event yields once its shape is decoded:
// the amount actually captured, its currency, the des the EVENT claims
// (attacker-influenceable) and the des OUR map returns for the referenced PayPal
// order (trusted).
type paypalEventFacts struct {
	paidUSD  string
	currency string
	claimed  string // custom_id asserted by the event
	mapped   string // des from OUR persisted orders.paypal_order_id map
}

// handlePaypalEvent processes a SIGNATURE-VERIFIED PayPal webhook event: capture
// (when needed), bind to our trusted order map, gate on the captured amount, then
// settle through the shared chokepoint — _handle_paypal_event (telegram_bot.py:
// 19166-19240).
//
// settlePaid is idempotent (reserved->paid CAS: deliver, credit a __deposit__
// wallet top-up, or refund if the 600s window already released the rows), so a
// replayed event settles at most once.
func (b *Bot) handlePaypalEvent(ctx context.Context, event map[string]any) {
	etype, _ := event["event_type"].(string)
	resource, _ := event["resource"].(map[string]any)
	resID, _ := resource["id"].(string)

	var f paypalEventFacts
	f.currency = "USD"

	switch {
	case etype == "CHECKOUT.ORDER.APPROVED" && resID != "":
		// res_id is the ORDER id. Capture, then trust our map over the event
		// (@19186-19198).
		cap, err := b.paypalCaptureOrder(resID)
		if err != nil {
			log.Println("paypal capture error:", err)
			return
		}
		if cap.Status != "COMPLETED" {
			log.Println("paypal capture not completed:", cap.Status)
			return
		}
		mapped, err := b.store.OrderDesForPaypal(resID)
		if err != nil {
			log.Println("paypal order_des_for_paypal error:", err)
			return
		}
		f.mapped, f.claimed, f.paidUSD = mapped, cap.CustomID, cap.AmountUSD
	case etype == "PAYMENT.CAPTURE.COMPLETED":
		// resource is a CAPTURE (res_id = capture id, not order id). It carries
		// amount + custom_id directly; the order id (if present) lives under
		// supplementary_data.related_ids (@19199-19207).
		amt, _ := resource["amount"].(map[string]any)
		f.paidUSD, _ = amt["value"].(string)
		cur, _ := amt["currency_code"].(string)
		f.currency = strings.ToUpper(cur)
		f.claimed, _ = resource["custom_id"].(string)
		if oid := relatedOrderID(resource); oid != "" {
			mapped, err := b.store.OrderDesForPaypal(oid)
			if err != nil {
				log.Println("paypal order_des_for_paypal error:", err)
				return
			}
			f.mapped = mapped
		}
	default:
		return // an event we don't act on (@19208)
	}

	// Bind to the trusted side; reject a custom_id that disagrees with our map, so
	// a replayed/edited event can't point a cheap payment at another order
	// (@19210-19216).
	if f.mapped != "" && f.claimed != "" && f.claimed != f.mapped {
		log.Printf("paypal custom_id mismatch: claimed=%s mapped=%s", f.claimed, f.mapped)
		return
	}
	des := f.mapped
	if des == "" {
		des = f.claimed
	}
	if des == "" {
		return
	}

	// AMOUNT GATE (@19221-19239): settle only if the money actually received
	// covers the order's expected USD, recomputed from its stored VND total via
	// the SAME converter used at create time. Blocks underpayment and
	// wrong-order settlement.
	o, err := b.store.GetOrder(des)
	if err != nil {
		log.Println("paypal get_order error:", err)
		return
	}
	if o == nil {
		log.Println("paypal: no order for des", des)
		return
	}
	if f.currency != "USD" {
		log.Printf("paypal: non-USD capture %s for %s", f.currency, des)
		return
	}
	expectedStr, err := b.paypalVndToUSD(o.Total)
	if err != nil {
		log.Println("paypal amount check error:", err)
		return
	}
	expected, okExp := usdCents(expectedStr)
	paid, okPaid := usdCents(f.paidUSD)
	// Python: `paid_usd is None or Decimal(paid) < Decimal(expected) - Decimal("0.01")`
	// — a one-cent tolerance for rounding on PayPal's side, and nothing else.
	if !okExp || !okPaid || paid < expected-1 {
		log.Printf("paypal underpaid des=%s paid=%q expected=%q", des, f.paidUSD, expectedStr)
		return
	}
	// The ONE shared settle chokepoint (idempotent: deliver, credit, or refund).
	if _, err := b.settlePaid(ctx, des); err != nil {
		log.Printf("paypal settle failed [des=%s]: %v", des, err)
	}
}

// relatedOrderID digs the PayPal ORDER id out of a capture resource's
// supplementary_data.related_ids.order_id (telegram_bot.py:19206), returning ""
// when any hop is missing or the wrong shape.
func relatedOrderID(resource map[string]any) string {
	supp, _ := resource["supplementary_data"].(map[string]any)
	related, _ := supp["related_ids"].(map[string]any)
	oid, _ := related["order_id"].(string)
	return oid
}

// ---- webhook listener (start_paypal_webhook_server @19242) -----------------

// paypalWebhookHandler is the HTTP entry point for a PayPal webhook POST — the
// do_POST body of the Python listener (telegram_bot.py:19259-19281).
//
// Order of operations is the security contract and must not be reshuffled:
//  1. path + method check,
//  2. read the raw body (bounded),
//  3. VERIFY THE SIGNATURE before trusting a single field of it — a failed or
//     erroring verification answers 400 and does nothing else,
//  4. ack 200 IMMEDIATELY (PayPal retries on non-2xx),
//  5. only then decode + process asynchronously, so the capture round-trip never
//     holds the connection open.
func (b *Bot) paypalWebhookHandler(w http.ResponseWriter, r *http.Request) {
	// Python compares self.path.rstrip("/") — same normalization here, so
	// "/paypal/webhook/" is accepted but "/paypal/webhook/anything" is not.
	if strings.TrimRight(r.URL.Path, "/") != webhookPath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		// Python's BaseHTTPRequestHandler only implements do_POST; every other
		// verb is refused. 405 is the accurate Go equivalent.
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Verify signature BEFORE trusting anything in the body. A verification ERROR
	// is treated exactly like a failed verification (fail closed) — as is a
	// missing PAYPAL_WEBHOOK_ID, which paypal.VerifyWebhook reports as (false, nil).
	ok, verr := b.paypalVerifyWebhook(r.Header, raw)
	if verr != nil {
		log.Println("paypal verify error:", verr)
		ok = false
	}
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		// Verified but unreadable body: nothing to act on. Ack anyway (Python
		// parity @19278-19280) — a retry would redeliver the same bytes.
		w.WriteHeader(http.StatusOK)
		return
	}
	// Hand the event to a registry-tracked goroutine (Python used a bare daemon
	// thread) so Shutdown DRAINS an in-flight capture+settle instead of losing it
	// at process death. The request ctx dies with the response, so settle on
	// Background.
	//
	// Enqueue BEFORE acking, and refuse with 503 if the registry is already
	// shutting down: a 200 tells PayPal the event is handled and stops its
	// retries, so acking an event we will never process would permanently lose an
	// already-CAPTURED payment. A non-2xx makes PayPal redeliver it to the next
	// process — the one case where Python's fire-and-forget thread silently
	// dropped money.
	if !b.workers.TryGo(func(_ <-chan struct{}) {
		b.handlePaypalEvent(context.Background(), event)
	}) {
		log.Println("paypal webhook: shutting down, refusing event so PayPal retries it")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// Ack immediately; the capture round-trip runs on the goroutine above and
	// never holds this connection open (@19274-19276).
	w.WriteHeader(http.StatusOK)
}

// StartPaypalWebhook starts the background HTTP listener that receives PayPal
// webhooks — start_paypal_webhook_server (telegram_bot.py:19242-19284) plus its
// boot gate (@19437-19443).
//
// Listens on PAYPAL_WEBHOOK_PORT (default 8080) at /paypal/webhook, bound to
// PAYPAL_WEBHOOK_HOST (default 127.0.0.1 so a reverse proxy — nginx — fronts it;
// set 0.0.0.0 only for direct exposure / tunnels). Register the public URL as the
// webhook in the PayPal dashboard.
//
// Returns (nil, nil) when PayPal is NOT configured: a reseller instance without
// PayPal keys must bind no port at all, so several instances on one host never
// clash over 8080. Otherwise returns the bound address (tests use port 0).
// The listener stops on ctx cancellation OR Bot.Shutdown, whichever comes first.
func (b *Bot) StartPaypalWebhook(ctx context.Context) (net.Addr, error) {
	if !b.paypalConfigured() {
		log.Println("PayPal not configured -> webhook listener skipped.")
		return nil, nil
	}
	addr := net.JoinHostPort(b.paypalWebhookHost, b.paypalWebhookPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("paypal webhook listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(b.paypalWebhookHandler),
		// An unauthenticated public endpoint must not let a slow-loris hold a
		// connection open forever; Python's ThreadingHTTPServer had no such
		// bound (each request got its own thread).
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	b.workers.Go(func(stop <-chan struct{}) {
		served := make(chan struct{})
		// Closer goroutine: whichever of ctx / Shutdown / server-exit fires first
		// releases it, so it can never outlive the server (no goroutine leak).
		go func() {
			select {
			case <-ctx.Done():
			case <-stop:
			case <-served:
				return
			}
			sctx, cancel := context.WithTimeout(context.Background(), webhookShutdownGrace)
			defer cancel()
			if err := srv.Shutdown(sctx); err != nil {
				log.Println("paypal webhook shutdown:", err)
			}
		}()
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Println("paypal webhook server:", err)
		}
		close(served)
	})
	log.Printf("PayPal webhook listener on %s%s", ln.Addr().String(), webhookPath)
	return ln.Addr(), nil
}
