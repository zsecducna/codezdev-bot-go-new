package handlers

import (
	"context"
	"strings"
	"testing"

	"codezdev-bot/internal/i18n"
)

// TestInfoRender: /info shows the balance card (name, id, USDT balance); the
// referral line is absent while the program is off (default).
func TestInfoRender(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "999")
	const chat = 100
	b.chatNamesMu.Lock()
	b.chatNames[chat] = "Bob"
	b.chatNamesMu.Unlock()
	if _, err := st.CreditBalance("100", 250000); err != nil {
		t.Fatal(err)
	}

	b.cmdInfo(ctx, chat, i18n.EN)
	got := f.Sends[len(f.Sends)-1].Text
	if !strings.Contains(got, "Bob") || !strings.Contains(got, "100") {
		t.Errorf("info card = %q, want name + chat id", got)
	}
	if !strings.Contains(got, b.usdtDisp(250000)) {
		t.Errorf("info card = %q, want balance %s", got, b.usdtDisp(250000))
	}
	if strings.Contains(got, "referral") || strings.Contains(got, "🎟") {
		t.Errorf("referral line must be hidden while the program is off: %q", got)
	}
}

// TestHistorySummaryDetailRedl walks the /history summary -> detail toggle and
// the per-order re-download for a delivered order.
func TestHistorySummaryDetailRedl(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const chat = 100
	// Deliver one order via the balance settle path (M2.2).
	b.finalizePurchase(ctx, chat, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat("100")
	if _, err := st.CreditBalance("100", 1_000_000); err != nil {
		t.Fatal(err)
	}
	b.Process(ctx, callbackUpdate(chat, "paybal:"+o.Des))
	if g, _ := st.GetOrder(o.Des); g == nil || g.Status != "delivered" {
		t.Fatalf("setup: order not delivered: %+v", g)
	}
	f.Docs = nil // reset so the redl assertion counts only the re-download

	// 1) /history summary: per-product line + a details toggle.
	b.cmdHistory(ctx, chat, i18n.EN)
	sum := f.Sends[len(f.Sends)-1]
	if !strings.Contains(sum.Text, "prodA") {
		t.Errorf("summary = %q, want a prodA line", sum.Text)
	}
	if _, ok := findCB(inlineRows(sum), "hist:5"); !ok {
		t.Errorf("summary missing the details toggle: %+v", inlineRows(sum))
	}

	// 2) hist:5 detailed list: an order block with the des + a redl button back to
	// the summary.
	b.Process(ctx, callbackUpdate(chat, "hist:5"))
	det := f.Sends[len(f.Sends)-1]
	if !strings.Contains(det.Text, o.Des) {
		t.Errorf("detail view = %q, want the order id %s", det.Text, o.Des)
	}
	rbtn, ok := findCB(inlineRows(det), "redl:")
	if !ok || rbtn.CallbackData != "redl:"+o.Des {
		t.Fatalf("detail view missing redl button: %+v", inlineRows(det))
	}
	if _, ok := findCB(inlineRows(det), "hist:summary"); !ok {
		t.Errorf("detail view missing the Summary button")
	}

	// 3) redl:<des> re-sends the order file.
	b.Process(ctx, callbackUpdate(chat, "redl:"+o.Des))
	if len(f.Docs) != 1 {
		t.Errorf("redl delivered %d files, want 1", len(f.Docs))
	}
	if last := lastSend(f); last != i18n.C().History.RedlSending.EN {
		// RedlSending is sent before the document; the very last Send is it.
		if !strings.Contains(last, "Resending") {
			t.Errorf("redl should announce the re-send, last=%q", last)
		}
	}
}

// TestHistoryEmpty: a chat with no orders gets the empty-state and no toggle.
func TestHistoryEmpty(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999")
	const chat = 100
	b.cmdHistory(ctx, chat, i18n.EN)
	got := f.Sends[len(f.Sends)-1]
	if got.Text != i18n.C().History.Empty.EN {
		t.Errorf("empty history = %q, want HISTORY_EMPTY", got.Text)
	}
	if len(inlineRows(got)) != 0 {
		t.Errorf("empty history must have no buttons: %+v", inlineRows(got))
	}
}

// TestRedlOwnershipGuard: another chat cannot re-download this chat's order.
func TestRedlOwnershipGuard(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "999")
	seedProduct(t, dir, "prodA", 250000, 5)
	const owner = 100
	b.finalizePurchase(ctx, owner, i18n.EN, "prodA", "KBH", 1)
	o, _ := st.ReservedOrderForChat("100")
	st.CreditBalance("100", 1_000_000)
	b.Process(ctx, callbackUpdate(owner, "paybal:"+o.Des))
	f.Docs = nil

	// A different chat taps redl for the owner's des -> RedlNone, no file.
	const intruder = 200
	b.Process(ctx, callbackUpdate(intruder, "redl:"+o.Des))
	if len(f.Docs) != 0 {
		t.Errorf("intruder must not re-download, got %d files", len(f.Docs))
	}
	if last := lastSend(f); last != i18n.C().History.RedlNone.EN {
		t.Errorf("intruder redl = %q, want RedlNone", last)
	}
}
