package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// newBuyBot wires a Bot to a temp SQLite store, a temp folder-state stock
// machine and a config rooted at the same dir (so USD_TO_VND falls back to the
// compiled default => a deterministic USDT rate). Cleanup closes the store and
// drains the expiry workers.
func newBuyBot(t *testing.T, adminID string) (*Bot, *fakeTG, *store.Store, *stock.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: adminID})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	return b, f, st, stk, dir
}

// seedProduct writes a product folder with `n` active password accounts and a
// KBH (no-warranty) price file. priceMicro is the unit price in MICRO-USDT
// (the same unit ProductPrice reads since the VND->micro flip); the tests use
// it as a token amount asserted arithmetically (total = unit x qty), so its
// real-world magnitude does not matter.
func seedProduct(t *testing.T, dir, name string, priceMicro, n int) {
	t.Helper()
	active := filepath.Join(dir, "accounts", name, "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		u := name + "-u" + strconv.Itoa(i)
		body := "username: " + u + "\npassword: pw-" + u + "\n"
		if err := os.WriteFile(filepath.Join(active, u+".txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts", name, "price_KBH_USDT"),
		[]byte(strconv.Itoa(priceMicro)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// primeKB pre-records the keyboard version so a plain-text /buy doesn't emit a
// refresh note ahead of the tested reply.
func primeKB(b *Bot, chatID int64) { b.states.Get(chatID).KBVersionSent = KeyboardVersion }

func inlineRows(p tg.SendMessageParams) [][]tg.InlineKeyboardButton {
	m, ok := p.ReplyMarkup.(*tg.InlineKeyboardMarkup)
	if !ok {
		return nil
	}
	return m.InlineKeyboard
}

// findCB returns the first button whose callback_data has the given prefix.
func findCB(rows [][]tg.InlineKeyboardButton, prefix string) (tg.InlineKeyboardButton, bool) {
	for _, r := range rows {
		for _, btn := range r {
			if strings.HasPrefix(btn.CallbackData, prefix) {
				return btn, true
			}
		}
	}
	return tg.InlineKeyboardButton{}, false
}

// TestBuyE2E walks the whole browse->tier->qty->pay-screen flow through Process
// with the fake Telegram harness.
func TestBuyE2E(t *testing.T) {
	ctx := context.Background()
	b, f, st, stk, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	primeKB(b, chat)

	// 1) /buy -> product list with an index-based buy:0 button.
	b.Process(ctx, textUpdate(chat, "/buy"))
	rows := inlineRows(f.Sends[len(f.Sends)-1])
	btn, ok := findCB(rows, "buy:")
	if !ok || btn.CallbackData != "buy:0" {
		t.Fatalf("product button = %+v (all rows %+v)", btn, rows)
	}
	if !strings.Contains(btn.Text, "prodA (5)") {
		t.Errorf("product label = %q, want stock count", btn.Text)
	}

	// 2) tap buy:0 -> tier menu with tier:0:KBH.
	b.Process(ctx, callbackUpdate(chat, "buy:0"))
	trow := inlineRows(f.Sends[len(f.Sends)-1])
	tbtn, ok := findCB(trow, "tier:")
	if !ok || tbtn.CallbackData != "tier:0:KBH" {
		t.Fatalf("tier button = %+v", tbtn)
	}

	// 3) tap tier:0:KBH -> qty prompt (arms pending_purchase) + buyqty buttons.
	b.Process(ctx, callbackUpdate(chat, "tier:0:KBH"))
	if p := b.states.Get(chat).Purchase; p == nil || p.Product != "prodA" || p.Tier != "KBH" {
		t.Fatalf("pending purchase = %+v", b.states.Get(chat).Purchase)
	}
	if _, ok := findCB(inlineRows(f.Sends[len(f.Sends)-1]), "buyqty:"); !ok {
		t.Fatalf("no quick-amount buttons on the qty prompt")
	}

	// 4) tap buyqty:2 -> finalize: reserve 2, order reserved, pay screen.
	b.Process(ctx, callbackUpdate(chat, "buyqty:2"))
	o, _ := st.ReservedOrderForChat("100")
	if o == nil || o.Qty != 2 || o.Total != 500000 || o.Product != "prodA" {
		t.Fatalf("reserved order = %+v", o)
	}
	if avail, _ := stk.CountAvailable("prodA"); avail != 3 {
		t.Errorf("available after reserve = %d, want 3", avail)
	}
	pay := f.Sends[len(f.Sends)-1]
	if pbtn, ok := findCB(inlineRows(pay), "paybal:"); !ok || pbtn.CallbackData != "paybal:"+o.Des {
		t.Fatalf("pay screen missing paybal button: %+v", inlineRows(pay))
	}
	if !strings.Contains(pay.Text, b.usdtDisp(500000)) {
		t.Errorf("pay screen total = %q, want %s", pay.Text, b.usdtDisp(500000))
	}
	// Purchase slot consumed by the quick-amount claim.
	if b.states.Get(chat).Purchase != nil {
		t.Errorf("pending purchase not cleared after finalize")
	}
}

// TestPendingGuard blocks a second purchase while one is reserved, but a pending
// DEPOSIT order never trips the guard (deposits stay ungated).
func TestPendingGuard(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100

	// A real reserved order -> the guard fires with a cancel/keep prompt.
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat("100")
	if o == nil {
		t.Fatal("expected a reserved order")
	}
	n0 := len(f.Sends)
	if !b.pendingGuard(ctx, chat, i18n.EN, "buy") {
		t.Fatal("pendingGuard should block a second purchase")
	}
	warn := f.Sends[len(f.Sends)-1]
	if len(f.Sends) != n0+1 {
		t.Fatalf("guard should send exactly one prompt")
	}
	if cbtn, ok := findCB(inlineRows(warn), "cancelord:"); !ok || cbtn.CallbackData != "cancelord:"+o.Des+":buy" {
		t.Fatalf("guard cancel button = %+v", inlineRows(warn))
	}
	if _, ok := findCB(inlineRows(warn), "keepord"); !ok {
		t.Errorf("guard missing keep button")
	}

	// A pending DEPOSIT order for a DIFFERENT chat's-style sentinel does NOT trip
	// the guard: ReservedOrderForChat excludes __deposit__.
	dep := store.NewDes()
	if err := st.CreateOrder(dep, "200", "en", store.DepositProduct, "PayPal", 0, 50000); err != nil {
		t.Fatal(err)
	}
	if b.pendingGuard(ctx, 200, i18n.EN, "buy") {
		t.Error("a pending deposit must NOT block a purchase")
	}
}

// TestQtyCaptureEscape pins the peek escape semantics of pending_purchase: a
// slash command or keyboard label leaves the slot armed; an invalid number keeps
// it armed with a BAD_QTY reply; a valid number consumes it and finalizes.
func TestQtyCaptureEscape(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	primeKB(b, chat)

	arm := func() {
		b.states.Get(chat).mu.Lock()
		b.states.Get(chat).Purchase = &PurchaseCtx{Product: "prodA", Tier: "KBH"}
		b.states.Get(chat).mu.Unlock()
	}

	// Escape via a slash command: slot stays armed, /start still runs.
	arm()
	b.Process(ctx, textUpdate(chat, "/start"))
	if b.states.Get(chat).Purchase == nil {
		t.Error("slash escape must leave pending_purchase ARMED")
	}

	// Escape via a keyboard label: slot stays armed, label routes onward.
	label := i18n.C().DescFor("buy").EN
	b.Process(ctx, textUpdate(chat, label))
	if b.states.Get(chat).Purchase == nil {
		t.Error("label escape must leave pending_purchase ARMED")
	}

	// Invalid number: BAD_QTY, slot stays armed.
	b.Process(ctx, textUpdate(chat, "abc"))
	if got := f.Sends[len(f.Sends)-1].Text; got != i18n.C().Buy.BadQty.T(i18n.EN) {
		t.Errorf("invalid qty reply = %q, want BAD_QTY", got)
	}
	if b.states.Get(chat).Purchase == nil {
		t.Error("invalid qty must keep pending_purchase armed for retry")
	}

	// Valid number: consumes the slot and reserves an order.
	b.Process(ctx, textUpdate(chat, "3"))
	if b.states.Get(chat).Purchase != nil {
		t.Error("valid qty must consume pending_purchase")
	}
	if o, _ := st.ReservedOrderForChat("100"); o == nil || o.Qty != 3 {
		t.Fatalf("valid qty did not finalize a 3-qty order: %+v", o)
	}
}

// TestPayByBalance covers the balance settle path and the insufficient-balance
// revert.
func TestPayByBalance(t *testing.T) {
	ctx := context.Background()

	t.Run("settle delivers", func(t *testing.T) {
		b, f, st, stk, dir := newBuyBot(t, "999")
		seedProduct(t, dir, "prodA", 250000, 5)
		const chat = 100
		b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
		o, _ := st.ReservedOrderForChat("100")
		if _, err := st.CreditBalance("100", 1_000_000); err != nil {
			t.Fatal(err)
		}
		b.Process(ctx, callbackUpdate(chat, "paybal:"+o.Des))

		got, _ := st.GetOrder(o.Des)
		if got.Status != "delivered" {
			t.Fatalf("order status = %s, want delivered", got.Status)
		}
		if bal, _ := st.GetBalance("100"); bal != 750000 {
			t.Errorf("balance after pay = %d, want 750000", bal)
		}
		if sold := stk.CountSold("prodA"); sold != 1 {
			t.Errorf("sold count = %d, want 1", sold)
		}
		if len(f.Docs) != 1 {
			t.Errorf("order file deliveries = %d, want 1", len(f.Docs))
		}
	})

	t.Run("insufficient balance reverts", func(t *testing.T) {
		b, f, st, _, dir := newBuyBot(t, "999")
		seedProduct(t, dir, "prodA", 250000, 5)
		const chat = 100
		b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1) // total 250000
		o, _ := st.ReservedOrderForChat("100")
		b.Process(ctx, callbackUpdate(chat, "paybal:"+o.Des))

		got, _ := st.GetOrder(o.Des)
		if got.Status != "reserved" {
			t.Fatalf("order status = %s, want reserved (reverted)", got.Status)
		}
		if f.Sends[len(f.Sends)-1].Text != i18n.C().Buy.Insufficient.T(i18n.EN) {
			t.Errorf("expected INSUFFICIENT reply")
		}
		if len(f.Docs) != 0 {
			t.Errorf("no delivery on insufficient balance, got %d", len(f.Docs))
		}
	})
}

// TestExpiryWorker: the per-order timer claims reserved->expired and releases
// the reserved stock when the payment window elapses.
func TestExpiryWorker(t *testing.T) {
	ctx := context.Background()
	b, _, st, stk, dir := newBuyBot(t, "999")
	b.expiryTimeout = 20 * time.Millisecond // fast expiry for the test
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100

	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 2)
	o, _ := st.ReservedOrderForChat("100")
	if o == nil {
		t.Fatal("expected reserved order")
	}
	// Poll until the worker fires (claims expired + releases stock).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := st.GetOrder(o.Des); got != nil && got.Status == "expired" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := st.GetOrder(o.Des)
	if got == nil || got.Status != "expired" {
		t.Fatalf("order status = %v, want expired", got)
	}
	if avail, _ := stk.CountAvailable("prodA"); avail != 5 {
		t.Errorf("stock not released on expiry: available = %d, want 5", avail)
	}
}

// TestDoubleSettleIdempotency: the ClaimOrder CAS makes a second settle a no-op,
// so a leaked des tapped twice never double-delivers.
func TestDoubleSettleIdempotency(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat("100")
	st.CreditBalance("100", 1_000_000)

	b.Process(ctx, callbackUpdate(chat, "paybal:"+o.Des)) // wins the claim, delivers
	b.Process(ctx, callbackUpdate(chat, "paybal:"+o.Des)) // order no longer reserved -> no-op

	if len(f.Docs) != 1 {
		t.Errorf("double settle delivered %d times, want 1", len(f.Docs))
	}
	if bal, _ := st.GetBalance("100"); bal != 750000 {
		t.Errorf("balance = %d, want 750000 (debited exactly once)", bal)
	}
	// A third manual claim attempt must also lose.
	if won, _ := st.ClaimOrder(o.Des, "reserved", "paid"); won != nil {
		t.Errorf("claim on a delivered order should return nil")
	}
}

// TestRowsGoneRefund: money confirmed but the reserved rows vanished -> refund to
// balance, never a silent drop and never a delivery.
func TestRowsGoneRefund(t *testing.T) {
	ctx := context.Background()
	b, f, st, stk, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat("100")
	st.CreditBalance("100", 1_000_000)
	// Simulate the reservation being swept back (rows gone before delivery).
	if _, err := stk.Release("prodA", o.Des); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(chat, "paybal:"+o.Des))

	got, _ := st.GetOrder(o.Des)
	if got.Status != "refunded" {
		t.Fatalf("order status = %s, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance("100"); bal != 1_000_000 {
		t.Errorf("balance = %d, want 1000000 (debit then refund cancel out)", bal)
	}
	if len(f.Docs) != 0 {
		t.Errorf("no delivery when rows are gone, got %d", len(f.Docs))
	}
	if last := f.Sends[len(f.Sends)-1].Text; !strings.HasPrefix(last, "⚠️") {
		t.Errorf("expected PAID_NO_STOCK refund notice, got %q", last)
	}
	if sold := stk.CountSold("prodA"); sold != 0 {
		t.Errorf("nothing should be sold on a refund, sold = %d", sold)
	}
}

// TestCallbackDataLen — handlers-go.md test contract #7: every buy-pipeline
// callback builder stays within Telegram's 64-byte hard limit, including the
// widest compound (cancelord:<32-hex-des>:buy).
func TestCallbackDataLen(t *testing.T) {
	des := store.NewDes() // 32 hex chars
	cases := []string{
		cbData("buy", "9999"),
		cbData("buyx", "9999"),
		cbData("buygo", "9999"),
		cbData("tier", "9999", "BH365D"),
		cbData("buyqty", "100000"),
		cbData("paybal", des),
		cbData("payqr", des),
		cbData("applycode", des),
		cbData("rmcode", des),
		cbData("cancelord", des, "buy"),
		"keepord",
		"back:products",
	}
	for _, d := range cases {
		if !cbDataLen(d) {
			t.Errorf("callback data %q = %d bytes, exceeds 64", d, len(d))
		}
	}
}

// TestVisibilityGatesHideProducts: a private product is invisible to a customer
// outside its allow-list, visible to the admin, and visible to a whitelisted
// customer.
func TestVisibilityGatesHideProducts(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "pubProd", 250000, 3)
	seedProduct(t, dir, "privProd", 250000, 3)

	// Mark privProd PRIVATE, allow-list a 7-digit whitelisted chat id.
	s := b.cfg.Settings()
	s["private_products"] = map[string]any{"privProd": []any{"1000001"}}
	s["whitelist"] = []any{map[string]any{"v": "1000001", "note": ""}}
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	buyRows := func(chat int64) [][]tg.InlineKeyboardButton {
		f.Sends = nil
		primeKB(b, chat)
		b.Process(ctx, textUpdate(chat, "/buy"))
		return inlineRows(f.Sends[len(f.Sends)-1])
	}
	labelsOf := func(rows [][]tg.InlineKeyboardButton) string {
		var sb strings.Builder
		for _, r := range rows {
			for _, btn := range r {
				sb.WriteString(btn.Text)
				sb.WriteByte('|')
			}
		}
		return sb.String()
	}

	// Outsider (chat 55555): sees pubProd, NOT privProd.
	out := labelsOf(buyRows(55555))
	if !strings.Contains(out, "pubProd") || strings.Contains(out, "privProd") {
		t.Errorf("outsider buttons = %q, want pubProd only", out)
	}
	// Whitelisted customer (chat 1000001): sees both.
	wl := labelsOf(buyRows(1000001))
	if !strings.Contains(wl, "pubProd") || !strings.Contains(wl, "privProd") {
		t.Errorf("whitelisted buttons = %q, want both", wl)
	}
	// Admin (chat 999): sees both regardless of the allow-list.
	adm := labelsOf(buyRows(999))
	if !strings.Contains(adm, "pubProd") || !strings.Contains(adm, "privProd") {
		t.Errorf("admin buttons = %q, want both", adm)
	}
}

// TestProductClickRateLimit: past the per-chat tap cap, buy: taps are refused
// with an alert (no chat message) and the handler goes no further.
func TestProductClickRateLimit(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, dir := newBuyBot(t, "999")
	b.clickLimit = 2 // tiny cap for the test
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100

	// First two taps are allowed (they emit the tier menu / a chat reply).
	b.Process(ctx, callbackUpdate(chat, "buy:0"))
	b.Process(ctx, callbackUpdate(chat, "buy:0"))
	answersBefore := len(f.Answers)
	sendsBefore := len(f.Sends)

	// Third tap trips the cooldown: alert only, no new chat message.
	b.Process(ctx, callbackUpdate(chat, "buy:0"))
	if len(f.Sends) != sendsBefore {
		t.Errorf("rate-limited tap must not send a chat message")
	}
	if len(f.Answers) != answersBefore+1 {
		t.Fatalf("rate-limited tap must answer with an alert")
	}
	if !f.Answers[len(f.Answers)-1].ShowAlert {
		t.Errorf("rate-limit answer must be an alert")
	}
}
