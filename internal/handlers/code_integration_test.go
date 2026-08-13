package handlers

// code_integration_test.go — store-backed tests for the code engine.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
)

// ============================================================
// harness helpers
// ============================================================

// newCodeBot wires a Bot to temp store + stock + config with referral_enabled.
func newCodeBot(t *testing.T) (*Bot, *fakeTG, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"referral_enabled": true})
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o644)
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
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "999"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	return b, f, st, dir
}

// seedProduct100 creates product "prod-a" priced at 100 VND/unit with tier KBH.
func seedProduct100(t *testing.T, dir string) {
	t.Helper()
	pdir := filepath.Join(dir, "accounts", "prod-a")
	if err := os.MkdirAll(filepath.Join(pdir, "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "price_KBH_VND"), []byte("100"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedReservedOrder creates a reserved order owned by chatID with tier KBH.
func seedReservedOrder(t *testing.T, st *store.Store, des, chatID string, qty, total int64) {
	t.Helper()
	if err := st.CreateOrder(des, chatID, "en", "prod-a", "KBH", qty, total); err != nil {
		t.Fatalf("create order: %v", err)
	}
}

// ctxFor builds a minimal dispatch Ctx (enough for applyCodeToOrder).
func ctxFor(b *Bot, chatID int64) *Ctx {
	return &Ctx{
		Ctx:    context.Background(),
		ChatID: chatID,
		Lang:   i18n.EN,
		st:     b.states.Get(chatID),
	}
}

// currentTotal reads the stored total from the order.
func currentTotal(t *testing.T, st *store.Store, des string) int64 {
	t.Helper()
	o, err := st.GetOrder(des)
	if err != nil || o == nil {
		t.Fatalf("getOrder(%s): %v", des, err)
	}
	return o.Total
}

// currentCode reads code/kind from the stored order.
func currentCode(t *testing.T, st *store.Store, des string) (code, kind string) {
	t.Helper()
	o, err := st.GetOrder(des)
	if err != nil || o == nil {
		t.Fatalf("getOrder(%s): %v", des, err)
	}
	if o.Code != nil {
		code = *o.Code
	}
	if o.CodeKind != nil {
		kind = *o.CodeKind
	}
	return
}

// promoUsed returns the stored used_count for a promo code.
func promoUsed(t *testing.T, st *store.Store, code string) int64 {
	t.Helper()
	p, err := st.GetPromo(code)
	if err != nil || p == nil {
		t.Fatalf("getPromo(%s): %v", code, err)
	}
	return p.UsedCount
}

// ============================================================
// TestApplyCodePromo
// ============================================================

func TestApplyCodePromo(t *testing.T) {
	b, _, st, dir := newCodeBot(t)
	seedProduct100(t, dir)
	const (
		chatID = int64(42)
		des    = "des-promo-1"
		unit   = 100
		qty    = int64(5)
		base   = int64(unit * 5) // 500
	)
	seedReservedOrder(t, st, des, "42", qty, base)

	// Create a promo code: 20%, 2 uses, stack=false.
	if err := st.CreatePromo("SAVE20", 20.0, 2, false); err != nil {
		t.Fatalf("createPromo: %v", err)
	}
	ctx := ctxFor(b, chatID)

	t.Run("happy_path", func(t *testing.T) {
		o, _ := st.GetOrder(des)
		b.applyCodeToOrder(ctx, o, "SAVE20")

		// Total must be 500 * 0.80 = 400.
		if got := currentTotal(t, st, des); got != 400 {
			t.Errorf("total=%d, want 400", got)
		}
		code, kind := currentCode(t, st, des)
		if code != "SAVE20" || kind != "promo" {
			t.Errorf("code=%q kind=%q, want SAVE20/promo", code, kind)
		}
		if promoUsed(t, st, "SAVE20") != 1 {
			t.Errorf("used_count want 1")
		}
	})

	t.Run("inactive_code", func(t *testing.T) {
		// Create and deactivate a promo.
		_ = st.CreatePromo("OFF", 10.0, 10, false)
		_ = st.SetPromoActive("OFF", false)
		o, _ := st.GetOrder(des)
		b.applyCodeToOrder(ctx, o, "OFF")
		// Total should revert to baseline (no vol discount at qty=5) = 500.
		if got := currentTotal(t, st, des); got != 500 {
			t.Errorf("total=%d, want 500 (baseline, inactive code rejected)", got)
		}
		// Promo that was applied (SAVE20) is now released + order reset.
		if promoUsed(t, st, "SAVE20") != 0 {
			t.Errorf("prior promo use should be released")
		}
	})

	t.Run("exhausted_code", func(t *testing.T) {
		// SAVE20 has 2 uses; burn them both.
		_ = st.CreatePromo("XHAUST", 5.0, 1, false)
		_, _ = st.PromoReserveUse("XHAUST")
		// Now it's exhausted (max_uses=1, used=1). Applying it should leave order at baseline.
		o, _ := st.GetOrder(des)
		b.applyCodeToOrder(ctx, o, "XHAUST")
		if got := currentTotal(t, st, des); got != 500 {
			t.Errorf("total=%d, want 500 (baseline, exhausted code rejected)", got)
		}
	})
}

// ============================================================
// TestApplyCodeReferral
// ============================================================

func TestApplyCodeReferral(t *testing.T) {
	b, _, st, dir := newCodeBot(t)
	seedProduct100(t, dir)
	const (
		buyerID   = int64(10)
		ownerID   = int64(20)
		des       = "des-ref-1"
		qty       = int64(1)
		totalBase = int64(100)
	)
	seedReservedOrder(t, st, des, "10", qty, totalBase)

	// Give ownerID a referral code "REFCODE".
	refCode, err := st.SetReferralCode("20", "REFCODE")
	if err != nil || refCode != "REFCODE" {
		t.Fatalf("set referral code: %v", err)
	}

	ctx := ctxFor(b, buyerID)

	t.Run("happy_path", func(t *testing.T) {
		o, _ := st.GetOrder(des)
		b.applyCodeToOrder(ctx, o, "REFCODE")

		// Referral is 10% off: 100 * 0.90 = 90.
		if got := currentTotal(t, st, des); got != 90 {
			t.Errorf("total=%d, want 90", got)
		}
		code, kind := currentCode(t, st, des)
		if code != "REFCODE" || kind != "referral" {
			t.Errorf("code=%q kind=%q, want REFCODE/referral", code, kind)
		}
	})

	t.Run("own_code_rejected", func(t *testing.T) {
		// ownerID tries to use their own code.
		ownerCtx := ctxFor(b, ownerID)
		// Create a fresh order for ownerID.
		const ownerDes = "des-ref-own"
		seedReservedOrder(t, st, ownerDes, "20", qty, totalBase)
		o, _ := st.GetOrder(ownerDes)
		b.applyCodeToOrder(ownerCtx, o, "REFCODE")
		// Total must remain at baseline (no discount applied).
		if got := currentTotal(t, st, ownerDes); got != totalBase {
			t.Errorf("total=%d, want %d (own code rejected)", got, totalBase)
		}
	})

	t.Run("already_redeemed", func(t *testing.T) {
		// buyerID already redeemed REFCODE — a second order should be rejected.
		const des2 = "des-ref-2"
		seedReservedOrder(t, st, des2, "10", qty, totalBase)
		o, _ := st.GetOrder(des2)
		b.applyCodeToOrder(ctx, o, "REFCODE")
		// Already has a redemption row — should be at baseline.
		if got := currentTotal(t, st, des2); got != totalBase {
			t.Errorf("total=%d, want %d (already redeemed)", got, totalBase)
		}
	})
}

// ============================================================
// TestUnknownCodeBaseline
// ============================================================

// TestUnknownCodeBaseline verifies the full invariant from
// telegram_bot.py:10250-10256 _apply_code_to_order docstring:
// "One code per order: any previously applied code is released and the order
// is reset to the plain volume-discount total BEFORE the new code is
// evaluated, so a REJECTED code leaves the order on its volume price."
//
// Two subtests:
//
//	no_prior_code   — original weak case; still valid as a sanity check.
//	prior_promo_discarded — the case that proved the original test was
//	  insufficient (prior SAVE30 at 630 VND; BADCODE applied; must land at
//	  volume baseline 900, code cleared, promo use released).
func TestUnknownCodeBaseline(t *testing.T) {
	const (
		chatID    = int64(50)
		qty       = int64(10)
		totalBase = int64(900) // unit=100 * qty=10 * (1-10% vol) = 900
	)

	t.Run("no_prior_code", func(t *testing.T) {
		// Original case: order starts at baseline already; bad code leaves it there.
		b, _, st, dir := newCodeBot(t)
		seedProduct100(t, dir)
		const des = "des-unknown-no-prior"
		seedReservedOrder(t, st, des, "50", qty, totalBase)
		o, _ := st.GetOrder(des)
		ctx := ctxFor(b, chatID)

		b.applyCodeToOrder(ctx, o, "BADCODE")

		if got := currentTotal(t, st, des); got != totalBase {
			t.Errorf("total=%d, want %d (baseline after unknown code, no prior)", got, totalBase)
		}
		code, _ := currentCode(t, st, des)
		if code != "" {
			t.Errorf("code=%q, want empty after unknown code", code)
		}
	})

	t.Run("prior_promo_discarded", func(t *testing.T) {
		// Binding case: order already carries a promo code (30% off → 630 VND
		// stored total) when the customer types an unknown code.
		// Invariant: stored total MUST revert to volume baseline (900), code
		// column MUST be cleared, and the prior promo's used_count MUST go back
		// to 0.  Without the baseline-reset call this test catches the mutation.
		b, _, st, dir := newCodeBot(t)
		seedProduct100(t, dir)
		const des = "des-unknown-prior-promo"
		// Seed at the DISCOUNTED total (promo already applied before we begin).
		const discountedTotal = int64(630) // 100*10*(1-0.37) rounded → 630
		seedReservedOrder(t, st, des, "50", qty, discountedTotal)

		// Create and apply a promo code so the order truly carries a hold.
		if err := st.CreatePromo("SAVE30", 30.0, 5, false); err != nil {
			t.Fatalf("CreatePromo: %v", err)
		}
		// Manually seed the order into the discounted state that applyCodeToOrder
		// would leave it in: reserve a use and stamp code/kind/total directly.
		reserved, err := st.PromoReserveUse("SAVE30")
		if err != nil || reserved == nil {
			t.Fatalf("PromoReserveUse: %v", err)
		}
		promoKind := "promo"
		promoCode := "SAVE30"
		if err := st.UpdateOrderCode(des, &promoCode, &promoKind, nil, discountedTotal); err != nil {
			t.Fatalf("UpdateOrderCode seed: %v", err)
		}
		// Confirm setup: used_count == 1 and stored total is the discounted value.
		if got := promoUsed(t, st, "SAVE30"); got != 1 {
			t.Fatalf("setup: want used_count=1 after seed, got %d", got)
		}
		if got := currentTotal(t, st, des); got != discountedTotal {
			t.Fatalf("setup: want total=%d, got %d", discountedTotal, got)
		}

		// Now apply an UNKNOWN code — must trigger the full reset path.
		o, _ := st.GetOrder(des)
		ctx := ctxFor(b, chatID)
		b.applyCodeToOrder(ctx, o, "BADCODE")

		// 1. Stored total must revert to the volume-discount baseline, NOT stay
		//    at the old promo's discounted value.
		if got := currentTotal(t, st, des); got != totalBase {
			t.Errorf("total=%d, want %d (volume baseline after unknown code replaces promo)",
				got, totalBase)
		}
		// 2. Code column must be cleared.
		code, kind := currentCode(t, st, des)
		if code != "" || kind != "" {
			t.Errorf("code=%q kind=%q, want both empty after unknown code", code, kind)
		}
		// 3. Prior promo's hold must be released back to 0.
		if got := promoUsed(t, st, "SAVE30"); got != 0 {
			t.Errorf("SAVE30 used_count=%d, want 0 (hold must be released)", got)
		}
	})
}

// ============================================================
// TestReApplyReleasesOldHold
// ============================================================

func TestReApplyReleasesOldHold(t *testing.T) {
	b, _, st, dir := newCodeBot(t)
	seedProduct100(t, dir)
	const (
		chatID    = int64(60)
		des       = "des-reapply-1"
		qty       = int64(1)
		totalBase = int64(100)
	)
	seedReservedOrder(t, st, des, "60", qty, totalBase)
	_ = st.CreatePromo("FIRST", 10.0, 5, false)
	_ = st.CreatePromo("SECOND", 20.0, 5, false)
	ctx := ctxFor(b, chatID)

	// Apply first promo.
	o, _ := st.GetOrder(des)
	b.applyCodeToOrder(ctx, o, "FIRST")
	if promoUsed(t, st, "FIRST") != 1 {
		t.Fatal("FIRST promo should be reserved after first apply")
	}

	// Re-apply with second promo — must release FIRST's hold.
	o, _ = st.GetOrder(des)
	b.applyCodeToOrder(ctx, o, "SECOND")
	if promoUsed(t, st, "FIRST") != 0 {
		t.Errorf("FIRST promo used_count should be released after replacement, got %d",
			promoUsed(t, st, "FIRST"))
	}
	if promoUsed(t, st, "SECOND") != 1 {
		t.Errorf("SECOND promo should be reserved")
	}
	// Total: 100 * 0.80 = 80.
	if got := currentTotal(t, st, des); got != 80 {
		t.Errorf("total=%d, want 80 after SECOND promo", got)
	}
}

// ============================================================
// TestRmCodeReleasesHold
// ============================================================

func TestRmCodeReleasesHold(t *testing.T) {
	b, f, st, dir := newCodeBot(t)
	seedProduct100(t, dir)
	const (
		chatID    = int64(70)
		des       = "des-rmcode-1"
		qty       = int64(1)
		totalBase = int64(100)
	)
	seedReservedOrder(t, st, des, "70", qty, totalBase)
	_ = st.CreatePromo("RMPROM", 20.0, 5, false)

	ctx := ctxFor(b, chatID)
	// Apply promo first.
	o, _ := st.GetOrder(des)
	b.applyCodeToOrder(ctx, o, "RMPROM")
	if promoUsed(t, st, "RMPROM") != 1 {
		t.Fatal("promo should be reserved")
	}

	// Tap the "Remove code" callback.
	f.Sends = nil
	b.Process(context.Background(), callbackUpdate(chatID, cbData("rmcode", des)))

	if promoUsed(t, st, "RMPROM") != 0 {
		t.Errorf("promo used_count should be 0 after rmcode, got %d", promoUsed(t, st, "RMPROM"))
	}
	// Order total should revert to baseline.
	if got := currentTotal(t, st, des); got != totalBase {
		t.Errorf("total=%d, want %d after rmcode", got, totalBase)
	}
}

// ============================================================
// TestExpiryReleasesHold
// ============================================================

func TestExpiryReleasesHold(t *testing.T) {
	b, _, st, dir := newCodeBot(t)
	seedProduct100(t, dir)
	// Very short expiry so we don't wait long.
	b.expiryTimeout = 10 * time.Millisecond

	const (
		chatID    = int64(80)
		des       = "des-expiry-1"
		qty       = int64(1)
		totalBase = int64(100)
	)
	seedReservedOrder(t, st, des, "80", qty, totalBase)
	_ = st.CreatePromo("EXPPROM", 20.0, 5, false)

	ctx := ctxFor(b, chatID)
	o, _ := st.GetOrder(des)
	b.applyCodeToOrder(ctx, o, "EXPPROM")
	if promoUsed(t, st, "EXPPROM") != 1 {
		t.Fatal("promo should be reserved after apply")
	}

	// Spawn expiry timer directly.
	b.spawnOrderExpiry(des)
	// Wait longer than the timer.
	time.Sleep(150 * time.Millisecond)
	b.Shutdown() // drain workers

	if promoUsed(t, st, "EXPPROM") != 0 {
		t.Errorf("expiry should release promo hold, used_count=%d", promoUsed(t, st, "EXPPROM"))
	}
}

// ============================================================
// TestCancelReleasesHold
// ============================================================

func TestCancelReleasesHold(t *testing.T) {
	b, _, st, dir := newCodeBot(t)
	seedProduct100(t, dir)

	const (
		chatID    = int64(90)
		des       = "des-cancel-1"
		qty       = int64(1)
		totalBase = int64(100)
	)
	seedReservedOrder(t, st, des, "90", qty, totalBase)
	_ = st.CreatePromo("CANPROM", 20.0, 5, false)

	ctx := ctxFor(b, chatID)
	o, _ := st.GetOrder(des)
	b.applyCodeToOrder(ctx, o, "CANPROM")
	if promoUsed(t, st, "CANPROM") != 1 {
		t.Fatal("promo should be reserved after apply")
	}

	// cancelord:<des>:<resumeCmd>
	b.Process(context.Background(), callbackUpdate(chatID, cbData("cancelord", des, "buy")))

	if promoUsed(t, st, "CANPROM") != 0 {
		t.Errorf("cancel should release promo hold, used_count=%d", promoUsed(t, st, "CANPROM"))
	}
}

// ============================================================
// TestPayReferrer
// ============================================================

func TestPayReferrer(t *testing.T) {
	b, f, st, _ := newCodeBot(t)

	// Give buyer and referrer some initial balance (0).
	const (
		buyerID    = int64(100)
		referrer   = "200"
		des        = "des-payref-1"
		orderTotal = int64(1000)
		wantReward = int64(100) // 10% of 1000
	)

	t.Run("credits_referrer", func(t *testing.T) {
		ref := referrer
		o := &store.Order{
			Des:          des,
			ChatID:       "100",
			Total:        orderTotal,
			Code:         strPtr("REFCODE"),
			ReferrerChat: &ref,
		}
		f.Sends = nil
		b.payReferrer(context.Background(), o)

		bal, err := st.GetBalance(referrer)
		if err != nil {
			t.Fatalf("getBalance: %v", err)
		}
		if bal != wantReward {
			t.Errorf("referrer balance=%d, want %d", bal, wantReward)
		}
		// Should have sent a DM to the referrer.
		if len(f.Sends) != 1 {
			t.Fatalf("payReferrer should send 1 DM, got %d", len(f.Sends))
		}
		if f.Sends[0].ChatID != 200 {
			t.Errorf("DM sent to %d, want 200", f.Sends[0].ChatID)
		}
		if !strings.Contains(f.Sends[0].Text, "REFCODE") {
			t.Errorf("DM should mention code, got %q", f.Sends[0].Text)
		}
	})

	t.Run("skips_when_no_referrer", func(t *testing.T) {
		f.Sends = nil
		o := &store.Order{Des: "no-ref", ChatID: "100", Total: 1000}
		b.payReferrer(context.Background(), o)
		if len(f.Sends) != 0 {
			t.Errorf("payReferrer with no referrer should send nothing")
		}
	})

	t.Run("skips_when_reward_rounds_to_zero", func(t *testing.T) {
		f.Sends = nil
		ref := referrer
		o := &store.Order{Des: "tiny", ChatID: "100", Total: 4, // 10% rounds to 0
			ReferrerChat: &ref}
		b.payReferrer(context.Background(), o)
		if len(f.Sends) != 0 {
			t.Errorf("payReferrer with zero reward should send nothing")
		}
	})
}

// ============================================================
// TestCapOrderCodePopSemantics
// ============================================================

func TestCapOrderCodePopSemantics(t *testing.T) {
	// POP semantics: an escaping text CONSUMES the slot (telegram_bot.py:19383).
	// The slot is gone after an escape — unlike PEEK where it stays armed.
	b, _, st, _ := newCodeBot(t)
	const (
		chatID = int64(110)
		des    = "des-cap-pop"
	)
	seedReservedOrder(t, st, des, "110", 1, 100)

	t.Run("escape_leaves_slot_armed", func(t *testing.T) {
		// PEEK semantics (telegram_bot.py:19381 guard): an escaping text leaves
		// the slot armed — the customer can type /help and then come back to
		// enter their code. This matches handlers-go.md §2.2 (#47 peek).
		st2 := b.states.Get(chatID)
		st2.mu.Lock()
		st2.OrderCode = &OrderCodeCtx{Des: des}
		st2.mu.Unlock()

		// /buy is an escaping text (starts with /).
		b.Process(context.Background(), textUpdate(chatID, "/buy"))

		// Slot must still be armed (peek, not pop).
		st2.mu.Lock()
		armed := st2.OrderCode
		st2.mu.Unlock()
		if armed == nil {
			t.Errorf("peek: escaping text must leave the slot armed; got nil")
		}
	})

	t.Run("plain_text_applies_code", func(t *testing.T) {
		_ = st.CreatePromo("POPTEST", 10.0, 5, false)
		st2 := b.states.Get(chatID)
		st2.mu.Lock()
		st2.OrderCode = &OrderCodeCtx{Des: des}
		st2.mu.Unlock()

		// Re-seed reserved status after previous test may have expired it.
		// Create a fresh order under a new des.
		const des2 = "des-cap-pop-2"
		seedReservedOrder(t, st, des2, "110", 1, 100)
		st2.mu.Lock()
		st2.OrderCode = &OrderCodeCtx{Des: des2}
		st2.mu.Unlock()

		b.Process(context.Background(), textUpdate(chatID, "poptest"))

		// Slot consumed.
		st2.mu.Lock()
		armed := st2.OrderCode
		st2.mu.Unlock()
		if armed != nil {
			t.Errorf("pop: plain text should consume the slot")
		}
		// Code should be uppercased and applied.
		code, kind := currentCode(t, st, des2)
		if code != "POPTEST" || kind != "promo" {
			t.Errorf("code=%q kind=%q after apply, want POPTEST/promo", code, kind)
		}
	})
}

// ============================================================
// TestCaptureChainOrderWithCode
// ============================================================

// TestCaptureChainOrderWithCode verifies that after registerCodeCapture, the
// chain contains "ordercode" between "apikeyrc" and "purchase".
func TestCaptureChainOrderWithCode(t *testing.T) {
	b, _ := newTestBot("1", nil)
	names := b.captureNames()

	// adminsettings.go's 7 captures land ahead of "code", and
	// adminpricing.go's 11 captures land ahead of "setkirourl"/"code"/"deposit"
	// (dispatch_test.go's TestCaptureChainOrder documents the canonical
	// positions). adminusers.go's captures land next-to-last: custlookup/
	// addadmin/addreseller/addwl/rmadmin ahead of "setrate"; reseller_discount
	// ahead of "promo". addbal is NOT in this chain (M3/M4 fix) — it lives in
	// the step-9 adminText stages instead, alongside "broadcast".
	// adminorders.go registers LAST: "getord" ahead of "custlookup",
	// "refundreject" and "refund" (in that order) both ahead of "discount".
	// adminstock.go registers last of all — see dispatch_test.go's
	// TestCaptureChainOrder for the full anchor-by-anchor rationale.
	want := []string{
		"upstock",
		"getord",
		"custlookup", "addadmin", "addreseller", "addwl", "rmadmin",
		"setrate",
		"setapiprice", "setapimin", "setapimax", "setkiroprice", "setkiromin", "setkiromax", "setapicap",
		"setkirourl", "setkirokey", "setkiroacct", "setbep20", "settron", "setmaxqty",
		"refundreject",
		"newprod", "renprod", "provpool",
		"setnotice", "setdelivery", "editfile",
		"refund",
		"discount", "reseller_discount", "promo", "code",
		"hold",
		"tier", "price", "deposit", "apikey", "apikeyrc",
		"ordercode", "purchase",
	}
	if len(names) != len(want) {
		t.Fatalf("capture chain = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("capture chain[%d] = %q, want %q", i, names[i], n)
		}
	}
}
