package handlers

// selfservice_test.go — store-backed tests for the M2.6 Self-Service slice.
//
// The refund path is the money path: fileRefundRequest decides whether a
// customer-visible refund_requests row is created, and every one of its gates
// (ownership, status, warranty window, duplicate) had shipped untested. The
// eligibility matrix below is the direct Go analogue of Python's
// _refund_eligibility (telegram_bot.py:16775-16805) and exists so a regression
// in any single arm fails loudly rather than silently widening who can file.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/refund"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// ============================================================
// harness
// ============================================================

// ssFixedNow is the pinned wall clock every test below runs against. The
// warranty gate compares b.now() to order.ts + window, so the clock must be
// deterministic or the "expired by one second" case is a coin flip.
var ssFixedNow = time.Unix(1_700_000_000, 0).UTC()

// newSelfServiceBot wires a Bot to a temp store + refund store + config, with
// the clock pinned to ssFixedNow. settings carries the delacct_enabled value
// under test (showSelfService / startCustDelete both read it).
func newSelfServiceBot(t *testing.T, delAcctEnabled bool) (*Bot, *fakeTG, *store.Store, *refund.Store) {
	t.Helper()
	dir := t.TempDir()

	raw, _ := json.Marshal(map[string]any{"delacct_enabled": delAcctEnabled})
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	// The refund store deliberately shares store.db (refund_store.py:32-33).
	rst, err := refund.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("refund store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}

	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, RefundStore: rst, Config: cfg, AdminID: "999"})
	b.now = func() time.Time { return ssFixedNow }
	t.Cleanup(func() { b.Shutdown(); rst.Close(); st.Close() })
	return b, f, st, rst
}

// seedOrder creates an order and forces its status + creation timestamp, which
// CreateOrder alone cannot do (it always writes status='reserved' and ts=now).
// ageSeconds is how long ago, relative to ssFixedNow, the order was placed.
func seedSSOrder(t *testing.T, st *store.Store, des, chatID, product, tier, status string, ageSeconds int64) {
	t.Helper()
	if err := st.CreateOrder(des, chatID, "en", product, tier, 1, 100_000); err != nil {
		t.Fatalf("create order %s: %v", des, err)
	}
	ts := float64(ssFixedNow.Unix() - ageSeconds)
	if _, err := st.DB().Exec("UPDATE orders SET status=?, ts=? WHERE des=?", status, ts, des); err != nil {
		t.Fatalf("backdate order %s: %v", des, err)
	}
}

// lastSendText returns the text of the most recent sendMessage, or "" when the
// handler sent nothing (the dead-button case).
func lastSendText(f *fakeTG) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sends) == 0 {
		return ""
	}
	return f.Sends[len(f.Sends)-1].Text
}

// cbFor builds the callback context the ss:/refreq: handlers take.
func cbFor(b *Bot, chatID int64, data string) *cbCtx {
	return &cbCtx{
		Ctx:    context.Background(),
		ChatID: chatID,
		Data:   data,
		CB:     &tg.CallbackQuery{ID: "cbid", From: tg.User{ID: chatID}, Data: data},
		st:     b.states.Get(chatID),
	}
}

// ============================================================
// fileRefundRequest — the eligibility matrix
// ============================================================

// TestFileRefundRequestEligibility is the table Python's _refund_eligibility
// (telegram_bot.py:16775-16805) describes, one row per rejection reason plus
// the happy path. Each case asserts BOTH the customer-visible reply and the
// EXACT refund_requests row count afterwards — the two have diverged before (a
// duplicate tap used to answer with the success string while filing nothing).
func TestFileRefundRequestEligibility(t *testing.T) {
	const owner int64 = 1001
	const stranger int64 = 2002

	c := &i18n.C().Customer
	orderGone := i18n.C().Buy.OrderGone.T(i18n.EN)

	cases := []struct {
		name string
		des  string
		// order fixture
		product string
		tier    string
		status  string
		age     int64 // seconds before ssFixedNow the order was placed
		// who taps
		caller int64
		// pre-existing open request for the same order?
		preExisting bool

		wantText string // "" = nothing sent (dead button)
		// wantRows is the EXACT number of refund_requests rows for this order
		// afterwards. Counting (rather than "is there an open one?") is what
		// makes the duplicate case meaningful: it already has one row going in,
		// so only an exact count proves a second was not inserted.
		wantRows int
	}{
		{
			// BH7D bought 1 day ago: comfortably inside the warranty window.
			name: "within warranty files a request", des: "ord-within",
			product: "prod-a", tier: "BH7D", status: "delivered", age: 86_400,
			caller:   owner,
			wantText: i18n.F(c.RefundRequestOK.T(i18n.EN), "des", "ord-within"),
			wantRows: 1,
		},
		{
			// BH7D bought 20 days ago. The picker's window is 30 days
			// (refundPickWindowS) so this order is still SHOWN, but the
			// warranty lapsed 13 days ago — Python's `now > ts + window` arm
			// (@16801) rejects it. This is the HIGH-1 regression pin.
			name: "expired warranty is rejected", des: "ord-expired",
			product: "prod-a", tier: "BH7D", status: "delivered", age: 20 * 86_400,
			caller:   owner,
			wantText: orderGone,
			wantRows: 0,
		},
		{
			// One second past the window: the boundary must be exclusive the
			// same way Python's strict `>` is.
			name: "warranty expired by one second is rejected", des: "ord-off-by-one",
			product: "prod-a", tier: "BH24H", status: "delivered", age: 86_401,
			caller:   owner,
			wantText: orderGone,
			wantRows: 0,
		},
		{
			// Exactly at the boundary: now == ts + window is still eligible
			// (Python rejects only when now is strictly greater).
			name: "warranty boundary exactly reached is still eligible", des: "ord-boundary",
			product: "prod-a", tier: "BH24H", status: "delivered", age: 86_400,
			caller:   owner,
			wantText: i18n.F(c.RefundRequestOK.T(i18n.EN), "des", "ord-boundary"),
			wantRows: 1,
		},
		{
			// KBH = no warranty sold; WarrantySeconds is 0 and the order is
			// never refundable regardless of age.
			name: "KBH tier (zero warranty) is rejected", des: "ord-kbh",
			product: "prod-a", tier: "KBH", status: "delivered", age: 60,
			caller:   owner,
			wantText: orderGone,
			wantRows: 0,
		},
		{
			// Someone else's des: DELIBERATE deviation from Python, which
			// answers "⛔ This order isn't yours." Go stays silent so the
			// callback cannot be used to probe which order ids exist.
			name: "foreign order is a silent dead button", des: "ord-foreign",
			product: "prod-a", tier: "BH7D", status: "delivered", age: 60,
			caller:   stranger,
			wantText: "",
			wantRows: 0,
		},
		{
			// 'reserved' is not in REFUNDABLE_STATUSES (telegram_bot.py:3649):
			// no money is confirmed in yet, so there is nothing to give back.
			name: "non-refundable status is rejected", des: "ord-reserved",
			product: "prod-a", tier: "BH7D", status: "reserved", age: 60,
			caller:   owner,
			wantText: orderGone,
			wantRows: 0,
		},
		{
			// A second tap must NOT answer with the success string: nothing new
			// was filed, and telling the customer otherwise hides the fact that
			// one request is already pending (@16803 -> REFUND_INELIGIBLE
			// "duplicate" @7092). wantRows stays 1: the seeded row, and no more.
			name: "duplicate open request gets the duplicate notice, not success", des: "ord-dup",
			product: "prod-a", tier: "BH7D", status: "delivered", age: 60,
			caller: owner, preExisting: true,
			wantText: c.RefundIneligibleDuplicate.T(i18n.EN),
			wantRows: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f, st, rst := newSelfServiceBot(t, false)
			seedSSOrder(t, st, tc.des, fmtInt(owner), tc.product, tc.tier, tc.status, tc.age)

			if tc.preExisting {
				if _, err := rst.CreateRequest(tc.des, fmtInt(owner), tc.product, tc.tier,
					100_000, float64(ssFixedNow.Unix()), true, nil); err != nil {
					t.Fatalf("seed pre-existing request: %v", err)
				}
			}

			b.fileRefundRequest(context.Background(), tc.caller, i18n.EN, tc.des)

			// Scoped to the customer's own chat, not lastSendText's "most
			// recent send overall": a filed request also fires notifyAdmins
			// (notify.go) to the admin chat (id "999" here, never == owner),
			// which would otherwise be mistaken for the customer's reply.
			got := ""
			if m := lastSendMsgTo(f, tc.caller); m != nil {
				got = m.Text
			}
			if got != tc.wantText {
				t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, tc.wantText)
			}
			if got := countRefundRows(t, st, tc.des); got != tc.wantRows {
				t.Fatalf("refund_requests rows for %s = %d, want %d", tc.des, got, tc.wantRows)
			}
			// #34/notify.go: a request that actually got FILED (a fresh row
			// landed) must alert the admins — _notify_admins
			// (telegram_bot.py:16978-16986). The ineligible/duplicate/
			// non-refundable cases file nothing, so they must stay silent too.
			adminAlert := lastSendMsgTo(f, 999)
			filedNow := tc.wantRows > 0 && !tc.preExisting
			gotAlert := adminAlert != nil && strings.Contains(adminAlert.Text, "NEW REFUND REQUEST")
			if filedNow && !gotAlert {
				t.Errorf("case %q filed a request but no admin NEW REFUND REQUEST alert was sent", tc.name)
			}
			if !filedNow && gotAlert {
				t.Errorf("case %q filed nothing but an admin alert was sent anyway", tc.name)
			}
		})
	}
}

// countRefundRows returns the exact number of refund_requests rows for an
// order. The refund store shares store.db (refund_store.py:32-33), so this can
// query straight through the main store's handle.
func countRefundRows(t *testing.T, st *store.Store, des string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM refund_requests WHERE des=?", des).Scan(&n); err != nil {
		t.Fatalf("count refund_requests: %v", err)
	}
	return n
}

// TestFileRefundRequestPredatedSuspendIsTrue pins the M2.6 fix for the inverted
// flag. Python writes `predated = 0 if susp else 1` (@16879); the Go port has no
// product-suspend tracking, so susp is always absent and every request filed
// here is by definition "filed while the product was still ACTIVE" = true. The
// column feeds the admin refund view and the M3.x payout maths, so recording
// its inverse silently mislabels every request in the queue.
func TestFileRefundRequestPredatedSuspendIsTrue(t *testing.T) {
	const owner int64 = 1001
	b, _, st, rst := newSelfServiceBot(t, false)
	seedSSOrder(t, st, "ord-pred", fmtInt(owner), "prod-a", "BH7D", "delivered", 3600)

	b.fileRefundRequest(context.Background(), owner, i18n.EN, "ord-pred")

	req, err := rst.OpenRequestForOrder("ord-pred")
	if err != nil || req == nil {
		t.Fatalf("expected a filed request, got req=%v err=%v", req, err)
	}
	if !req.PredatedSuspend {
		t.Fatal("predated_suspend must be TRUE (no suspend tracking exists, so the " +
			"product was still active at filing time — @16879 `0 if susp else 1`)")
	}
}

// TestFileRefundRequestWithoutRefundStoreDegrades pins the safe-degrade path: a
// Bot booted without a refund store must drop the tap (log + return) rather
// than nil-deref, and must not tell the customer a request was filed.
func TestFileRefundRequestWithoutRefundStoreDegrades(t *testing.T) {
	const owner int64 = 1001
	b, f, st, _ := newSelfServiceBot(t, false)
	seedSSOrder(t, st, "ord-nostore", fmtInt(owner), "prod-a", "BH7D", "delivered", 3600)
	b.refundStore = nil // simulate a partial boot

	b.fileRefundRequest(context.Background(), owner, i18n.EN, "ord-nostore")

	if n := f.sendCount(); n != 0 {
		t.Fatalf("expected no reply when the refund store is unwired, got %d send(s): %q",
			n, lastSendText(f))
	}
}

// TestCbFileRefundReqParsesDes covers the callback wrapper: a malformed payload
// is a no-op, and a well-formed one reaches fileRefundRequest (proved by the
// eligibility reply coming back).
func TestCbFileRefundReqParsesDes(t *testing.T) {
	const owner int64 = 1001
	b, f, st, _ := newSelfServiceBot(t, false)
	seedSSOrder(t, st, "ord-cb", fmtInt(owner), "prod-a", "KBH", "delivered", 60)

	// No colon, and an empty des: both must be dropped without a send.
	b.cbFileRefundReq(cbFor(b, owner, "refreq"))
	b.cbFileRefundReq(cbFor(b, owner, "refreq:"))
	if n := f.sendCount(); n != 0 {
		t.Fatalf("malformed refreq payloads must be no-ops, got %d send(s)", n)
	}

	// Well-formed: KBH is ineligible, so the ineligibility reply proves the
	// des was parsed and handed to fileRefundRequest.
	b.cbFileRefundReq(cbFor(b, owner, "refreq:ord-cb"))
	if got, want := lastSendText(f), i18n.C().Buy.OrderGone.T(i18n.EN); got != want {
		t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, want)
	}
}

// ============================================================
// showRefundOrderPicker
// ============================================================

// TestTruncateButtonLabel is the M1 regression pin, asserted directly on the
// boundary rather than through a fixture.
//
// The defect it guards: labels used to be cut with label[:60], a BYTE slice,
// while Python's label[:60] (@16843) slices characters. Each case below states
// what the byte cut would have produced, and the test proves the current code
// does not produce it — an over-60-byte input is the ONLY interesting input, so
// making it explicit beats hoping a seeded order happens to be long enough.
func TestTruncateButtonLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// 61 CJK runes = 183 bytes. The old byte cut kept 60 bytes = 20
			// whole runes; 60 is a multiple of 3 so that one stayed valid, but
			// it silently dropped two thirds of the label.
			name: "long CJK truncates to 60 runes",
			in:   strings.Repeat("保", 61),
			want: strings.Repeat("保", 60),
		},
		{
			// The dangerous shape: a rune straddling byte 60. 保 is 3 bytes,
			// and the single leading ASCII char shifts the run by one, so byte
			// 60 = 1 + 19 whole runes (57 B) + 2 bytes into the 20th — a cut
			// there lands INSIDE that rune. label[:60] yields invalid UTF-8 and
			// Telegram rejects the entire sendMessage.
			name: "byte-60 landing mid-rune still yields valid UTF-8",
			in:   "x" + strings.Repeat("保", 61),
			want: "x" + strings.Repeat("保", 59),
		},
		{
			// Emoji + variation selector: 🛡️ is TWO runes (U+1F6E1 U+FE0F),
			// 7 bytes. 30 of them = 60 runes, exactly at the cap.
			name: "exactly 60 runes is left alone",
			in:   strings.Repeat("🛡️", 30),
			want: strings.Repeat("🛡️", 30),
		},
		{
			name: "short label is returned unchanged",
			in:   "🟣 500 Kiro credits · 4.00 USDT · today",
			want: "🟣 500 Kiro credits · 4.00 USDT · today",
		},
		{
			name: "empty label is returned unchanged",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateButtonLabel(tc.in)
			if got != tc.want {
				t.Fatalf("truncateButtonLabel mismatch\n got:  %q\n want: %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
			if n := utf8.RuneCountInString(got); n > 60 {
				t.Fatalf("result exceeds the 60-rune cap (%d runes)", n)
			}
		})
	}

	// Explicitly demonstrate the defect this function exists to prevent, so a
	// future reader can see WHY the rune conversion is not gratuitous.
	buggy := ("x" + strings.Repeat("保", 61))[:60]
	if utf8.ValidString(buggy) {
		t.Fatal("the mid-rune fixture stopped producing invalid UTF-8 under a byte " +
			"slice — it no longer demonstrates the defect being guarded")
	}
}

// TestRefundPickerLabelsStayValidUTF8 is the integration-level companion to
// TestTruncateButtonLabel: it checks that what the picker actually puts on the
// wire is valid UTF-8 and within the cap, across every label branch (the three
// api-key markers plus the localized 🛡️ tier branch).
func TestRefundPickerLabelsStayValidUTF8(t *testing.T) {
	const owner int64 = 1001
	b, f, st, _ := newSelfServiceBot(t, false)

	// Kiro api-key orders: label = "🟣 %d Kiro credits · %s · %s", and api-key
	// products are pickable regardless of tier.
	qty := int64(1)
	for i := range 10 {
		des := "ord-utf8-" + fmtInt(int64(i))
		if err := st.CreateOrder(des, fmtInt(owner), "zh", kiroApikeyProduct, "API", qty, 100_000); err != nil {
			t.Fatalf("create order: %v", err)
		}
		if _, err := st.DB().Exec("UPDATE orders SET status='delivered', ts=? WHERE des=?",
			float64(ssFixedNow.Unix()-3600), des); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		qty *= 10
	}
	// Plus one standard order using the 🛡️ + localized-tier branch.
	seedSSOrder(t, st, "ord-utf8-std", fmtInt(owner), "prod-a", "BH30D", "delivered", 3600)

	b.showRefundOrderPicker(context.Background(), owner, i18n.ZH)

	rows := pickerRows(t, f)
	if len(rows) == 0 {
		t.Fatal("expected picker rows, got none")
	}

	for _, row := range rows {
		for _, btn := range row {
			if !utf8.ValidString(btn.Text) {
				t.Fatalf("button label is not valid UTF-8: %q", btn.Text)
			}
			if n := utf8.RuneCountInString(btn.Text); n > 60 {
				t.Fatalf("button label exceeds the 60-rune cap (%d runes): %q", n, btn.Text)
			}
			if btn.Text == "" {
				t.Fatal("button label is empty — Telegram rejects a blank button")
			}
		}
	}
}

// TestRefundPickerFiltersOrders pins the picker's inclusion rules
// (@16816-16823): deposits are always excluded, api-key orders are always in,
// and standard orders need a non-zero warranty tier.
func TestRefundPickerFiltersOrders(t *testing.T) {
	const owner int64 = 1001
	b, f, st, _ := newSelfServiceBot(t, false)

	seedSSOrder(t, st, "ord-dep", fmtInt(owner), depositProduct, "SePay", "delivered", 3600)
	seedSSOrder(t, st, "ord-kbh", fmtInt(owner), "prod-a", "KBH", "delivered", 3600)
	seedSSOrder(t, st, "ord-bh", fmtInt(owner), "prod-a", "BH7D", "delivered", 3600)
	seedSSOrder(t, st, "ord-key", fmtInt(owner), apikeyProduct, "API", "delivered", 3600)

	b.showRefundOrderPicker(context.Background(), owner, i18n.EN)

	got := map[string]bool{}
	for _, row := range pickerRows(t, f) {
		for _, btn := range row {
			got[btn.CallbackData] = true
		}
	}
	for _, want := range []string{"refreq:ord-bh", "refreq:ord-key"} {
		if !got[want] {
			t.Fatalf("expected %q in the picker, got %v", want, got)
		}
	}
	for _, unwanted := range []string{"refreq:ord-dep", "refreq:ord-kbh"} {
		if got[unwanted] {
			t.Fatalf("%q must not be pickable, got %v", unwanted, got)
		}
	}
}

// TestRefundPickerWindowUsesInjectedClock pins the b.now() swap (the picker used
// time.Now(), so its 30-day window could not be tested deterministically).
//
// It asserts a BOUNDARY PAIR on purpose: a 29-day-old order must be listed and a
// 31-day-old one must not. Asserting only the exclusion would pass even with a
// broken clock, because every fixture here is backdated relative to ssFixedNow —
// under a live time.Now() the whole window misses and "not listed" becomes the
// trivial default. Requiring the inclusion is what makes the clock observable.
func TestRefundPickerWindowUsesInjectedClock(t *testing.T) {
	const owner int64 = 1001
	b, f, st, _ := newSelfServiceBot(t, false)
	seedSSOrder(t, st, "ord-inside", fmtInt(owner), "prod-a", "BH30D", "delivered", 29*86_400)
	seedSSOrder(t, st, "ord-outside", fmtInt(owner), "prod-a", "BH30D", "delivered", 31*86_400)

	b.showRefundOrderPicker(context.Background(), owner, i18n.EN)

	// An empty picker here means the window was computed off a clock other than
	// b.now(), so both fixtures fell outside it. Say so explicitly — the generic
	// "no inline keyboard" failure from pickerRows would not point at the clock.
	if got := lastSendText(f); got == i18n.C().Customer.RefundPickEmpty.T(i18n.EN) {
		t.Fatal("picker was empty: the 30-day window is not being measured from " +
			"b.now(), so every backdated fixture fell outside it")
	}

	got := map[string]bool{}
	for _, row := range pickerRows(t, f) {
		for _, btn := range row {
			got[btn.CallbackData] = true
		}
	}
	if !got["refreq:ord-inside"] {
		t.Fatalf("29-day-old order must be inside the %d-day window, got %v",
			refundPickWindowS/86_400, got)
	}
	if got["refreq:ord-outside"] {
		t.Fatalf("31-day-old order must be outside the %d-day window, got %v",
			refundPickWindowS/86_400, got)
	}
}

// TestRefundPickerEmptyWhenNothingEligible covers the other side: with every
// order out of window the customer gets the empty-picker notice, not silence.
func TestRefundPickerEmptyWhenNothingEligible(t *testing.T) {
	const owner int64 = 1001
	b, f, st, _ := newSelfServiceBot(t, false)
	seedSSOrder(t, st, "ord-old", fmtInt(owner), "prod-a", "BH30D", "delivered", 31*86_400)

	b.showRefundOrderPicker(context.Background(), owner, i18n.EN)

	if got, want := lastSendText(f), i18n.C().Customer.RefundPickEmpty.T(i18n.EN); got != want {
		t.Fatalf("expected the empty-picker notice\n got:  %q\n want: %q", got, want)
	}
}

// pickerRows extracts the inline keyboard from the last recorded sendMessage.
func pickerRows(t *testing.T, f *fakeTG) [][]tg.InlineKeyboardButton {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sends) == 0 {
		t.Fatal("no sendMessage recorded")
	}
	markup, ok := f.Sends[len(f.Sends)-1].ReplyMarkup.(*tg.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("last send carried no inline keyboard: %#v", f.Sends[len(f.Sends)-1].ReplyMarkup)
	}
	return markup.InlineKeyboard
}

// ============================================================
// showSelfService
// ============================================================

// TestShowSelfServiceDelAcctVisibility pins the destructive-button gate
// (@4271-4273): "Delete accounts" appears only when the kill-switch is ON *and*
// the chat is an admin or authorized reseller.
func TestShowSelfServiceDelAcctVisibility(t *testing.T) {
	const admin int64 = 999
	const customer int64 = 1001

	cases := []struct {
		name    string
		enabled bool
		chatID  int64
		want    bool
	}{
		{"enabled + admin shows the button", true, admin, true},
		{"enabled + plain customer hides it", true, customer, false},
		{"disabled + admin hides it", false, admin, false},
		{"disabled + plain customer hides it", false, customer, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f, _, _ := newSelfServiceBot(t, tc.enabled)

			b.showSelfService(context.Background(), tc.chatID, i18n.EN)

			found := false
			for _, row := range pickerRows(t, f) {
				for _, btn := range row {
					if btn.CallbackData == "ss:delacct" {
						found = true
					}
				}
			}
			if found != tc.want {
				t.Fatalf("ss:delacct visible = %v, want %v", found, tc.want)
			}
		})
	}
}

// TestShowSelfServiceAlwaysOffersCoreRows pins the fixed rows every customer
// gets (@4264-4267 + @4276-4277) so a future edit cannot quietly drop one.
func TestShowSelfServiceAlwaysOffersCoreRows(t *testing.T) {
	b, f, _, _ := newSelfServiceBot(t, false)

	b.showSelfService(context.Background(), 1001, i18n.EN)

	got := map[string]bool{}
	for _, row := range pickerRows(t, f) {
		for _, btn := range row {
			got[btn.CallbackData] = true
		}
	}
	for _, want := range []string{
		"ss:history", "ss:info", "ss:getcode", "ss:reset",
		"ss:mykeys", "ss:refund", "ss:support",
	} {
		if !got[want] {
			t.Fatalf("missing %q from the Self-Service menu, got %v", want, got)
		}
	}
}

// ============================================================
// startCustReset / startCustDelete
// ============================================================

// TestStartCustResetBlockedByEntraStub pins invariant 8: entraSessionOk() is a
// stub returning false until M3.4, so the flow must show the "not available"
// notice and arm NOTHING. An armed capture with no registered consumer would
// swallow the customer's next message.
func TestStartCustResetBlockedByEntraStub(t *testing.T) {
	const chatID int64 = 1001
	b, f, _, _ := newSelfServiceBot(t, false)

	b.startCustReset(context.Background(), chatID, i18n.EN)

	if got, want := lastSendText(f), i18n.C().Customer.CustResetNoSSO.T(i18n.EN); got != want {
		t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, want)
	}
	assertNothingArmed(t, b, chatID)
}

// TestStartCustDeleteAuthorization pins the three server-side gates in order
// (@2501-2511): kill-switch, then authorization, then the Entra session. The
// authorization re-check matters even though showSelfService already hides the
// button — a callback payload is client-supplied and can be replayed.
func TestStartCustDeleteAuthorization(t *testing.T) {
	const admin int64 = 999
	const customer int64 = 1001
	c := &i18n.C().Customer

	cases := []struct {
		name    string
		enabled bool
		chatID  int64
		want    string
	}{
		{"kill-switch off refuses even an admin", false, admin, c.CustDelDisabled.T(i18n.EN)},
		{"kill-switch off refuses a customer", false, customer, c.CustDelDisabled.T(i18n.EN)},
		{"enabled but unauthorized chat is refused", true, customer, c.CustDelNotAllowed.T(i18n.EN)},
		// Admin passes both gates and is stopped only by the Entra stub —
		// which is what proves the ordering above is real.
		{"enabled admin stops at the Entra stub", true, admin, c.CustDelNoSSO.T(i18n.EN)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f, _, _ := newSelfServiceBot(t, tc.enabled)

			b.startCustDelete(context.Background(), tc.chatID, i18n.EN)

			if got := lastSendText(f); got != tc.want {
				t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, tc.want)
			}
			// No path may arm the capture while the stub is in place.
			assertNothingArmed(t, b, tc.chatID)
		})
	}
}

// assertNothingArmed fails when either customer self-service capture is armed.
// Both custreset (#37) and custdelete (#38) are absent from captureChain today,
// so an armed capture would make the customer's next typed line vanish.
func assertNothingArmed(t *testing.T, b *Bot, chatID int64) {
	t.Helper()
	st := b.states.Get(chatID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.CustReset != nil {
		t.Fatal("pending CustReset must not be armed while entraSessionOk() is a stub")
	}
	if st.CustDelete != nil {
		t.Fatal("pending CustDelete must not be armed while entraSessionOk() is a stub")
	}
}

// ============================================================
// nil-store guards
// ============================================================

// TestSelfServiceHandlersSurviveNilStore pins the b.store nil guards added to
// every M2.6 DB-touching entry point. A partial boot (or a unit-test Bot) leaves
// b.store nil; without the guards these nil-deref, and while cmd/bot/main.go's
// per-update recover keeps the process alive, the update is silently dropped.
//
// Each handler is exercised separately so a panic names the one that regressed —
// a single combined test would stop at the first failure and hide the rest.
func TestSelfServiceHandlersSurviveNilStore(t *testing.T) {
	const chatID int64 = 1001

	cases := []struct {
		name string
		run  func(b *Bot)
	}{
		{"showRefundOrderPicker", func(b *Bot) {
			b.showRefundOrderPicker(context.Background(), chatID, i18n.EN)
		}},
		{"fileRefundRequest", func(b *Bot) {
			b.fileRefundRequest(context.Background(), chatID, i18n.EN, "ord-x")
		}},
		{"cbApplyCode", func(b *Bot) {
			b.cbApplyCode(cbFor(b, chatID, "applycode:ord-x"))
		}},
		{"cbRemoveCode", func(b *Bot) {
			b.cbRemoveCode(cbFor(b, chatID, "rmcode:ord-x"))
		}},
		{"cbFileRefundReq", func(b *Bot) {
			b.cbFileRefundReq(cbFor(b, chatID, "refreq:ord-x"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f := newTestBot("999", nil) // Store and RefundStore are both nil here
			t.Cleanup(b.Shutdown)

			// A panic here fails the sub-test rather than the whole binary.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s panicked with no store wired: %v", tc.name, r)
				}
			}()
			tc.run(b)

			if n := f.sendCount(); n != 0 {
				t.Fatalf("%s must stay silent with no store wired, got %d send(s): %q",
					tc.name, n, lastSendText(f))
			}
		})
	}
}

// ============================================================
// cbSelfService routing
// ============================================================

// TestCbSelfServiceRoutesActions pins the ss:* switch, including its
// default-deny shape: the switch has no default branch, so an unknown action is
// a silent no-op rather than a fallthrough into some other handler.
func TestCbSelfServiceRoutesActions(t *testing.T) {
	const admin int64 = 999
	c := &i18n.C().Customer

	t.Run("reset routes to startCustReset", func(t *testing.T) {
		b, f, _, _ := newSelfServiceBot(t, false)
		b.cbSelfService(cbFor(b, admin, "ss:reset"))
		if got, want := lastSendText(f), c.CustResetNoSSO.T(i18n.EN); got != want {
			t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, want)
		}
	})

	t.Run("delacct routes to startCustDelete", func(t *testing.T) {
		b, f, _, _ := newSelfServiceBot(t, true)
		b.cbSelfService(cbFor(b, admin, "ss:delacct"))
		if got, want := lastSendText(f), c.CustDelNoSSO.T(i18n.EN); got != want {
			t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, want)
		}
	})

	t.Run("refund routes to the order picker", func(t *testing.T) {
		b, f, _, _ := newSelfServiceBot(t, false)
		b.cbSelfService(cbFor(b, admin, "ss:refund"))
		// No eligible orders seeded -> the empty-picker notice.
		if got, want := lastSendText(f), c.RefundPickEmpty.T(i18n.EN); got != want {
			t.Fatalf("reply mismatch\n got:  %q\n want: %q", got, want)
		}
	})

	t.Run("unknown action is a silent no-op", func(t *testing.T) {
		b, f, _, _ := newSelfServiceBot(t, true)
		for _, data := range []string{"ss:", "ss:bogus", "ss:delacct2", "ss:DELACCT"} {
			b.cbSelfService(cbFor(b, admin, data))
		}
		if n := f.sendCount(); n != 0 {
			t.Fatalf("unknown ss:* actions must be no-ops, got %d send(s): %q",
				n, lastSendText(f))
		}
	})
}
