package handlers

// adminpricing_test.go — tests for the 🏷️ PRICING & DISCOUNTS category
// (adminpricing.go). Covers: tier-code + price validators, the
// discounts.json missing-vs-empty distinction, promo create/list/delete
// through the handler layer, min/max cross-validation for both SKUs, the
// abort-(pop) vs rearm-(peek) asymmetry per handler, and the kirotoggle
// in-place re-render. store.go's TestPromoReserveUseConcurrent covers the
// reserve-use oversell guard under concurrency at the data layer.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
)

// withDiscountsFile points the package-level discountsFilePath at a fresh
// temp file for the duration of the test, restoring it afterwards — the
// var exists so codedTotalFull/discountFor's existing "no bot" unit tests
// (code_test.go) keep working untouched, but that means every test that
// exercises discounts.json must isolate its own path or risk polluting the
// real repo cwd / racing other tests.
// armAndCapture drives a capture handler's arming callback through the real
// Process path (so the exact wizard-arming logic in cbApiMax/cbApiCap/etc.
// is never duplicated here), then invokes the capture directly against a
// freshly built Ctx carrying text — bypassing Process for that second step
// is the only way to observe the capture's own bool return value, which
// Process discards (first-match-wins dispatch only cares whether an update
// was consumed). Used by the save-failure tests below, which the review
// that opened task #29 flagged for asserting neither the reply text nor
// this return value.
func armAndCapture(t *testing.T, b *Bot, chatID int64, armCB, text string, capture func(*Ctx) bool) bool {
	t.Helper()
	b.Process(context.Background(), callbackUpdate(chatID, armCB))
	c := &Ctx{Ctx: context.Background(), ChatID: chatID, Lang: i18n.EN, Text: text, st: b.states.Get(chatID)}
	return capture(c)
}

func withDiscountsFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := discountsFilePath
	discountsFilePath = filepath.Join(dir, "discounts.json")
	t.Cleanup(func() { discountsFilePath = old })
	return dir
}

// ---- validators --------------------------------------------------------

func TestValidTierCode(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"KBH", true},
		{"BH30D", true},
		{"BH12H", true},
		{"BH1D", true},
		{"KBH2", false},
		{"BH30", false},
		{"BHD", false},
		{"BH30X", false},
		{"", false},
		{"bh30d", false}, // caller upper-cases before calling; this checks the exact match
	}
	for _, tc := range cases {
		if got := validTierCode(tc.code); got != tc.want {
			t.Errorf("validTierCode(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestAdminUsdtToMicro(t *testing.T) {
	b, _ := newTestBot("1", nil) // b.cfg == nil; adminUsdtToMicro is rate-free (scales by 1e6)
	cases := []struct {
		text      string
		wantMicro int64
		wantOK    bool
	}{
		{"5", 5_000_000, true},
		{"5.5", 5_500_000, true},
		{"1,234.5", 1_234_500_000, true}, // comma stripped as a thousands separator
		{" 5 ", 5_000_000, true},         // trimmed
		{"-3", -3_000_000, true},         // sign accepted here; callers reject <=0 themselves
		{"abc", 0, false},
		{"", 0, false},
		{"5,,5", 0, false}, // stripping commas leaves "55" -- NOT this case; see below
	}
	for _, tc := range cases {
		micro, ok := b.adminUsdtToMicro(tc.text)
		if tc.text == "5,,5" {
			// "5,,5" comma-stripped is "55", a VALID digit string -> 55 * 1e6.
			if !ok || micro != 55_000_000 {
				t.Errorf("adminUsdtToMicro(%q) = (%d,%v), want (55000000,true)", tc.text, micro, ok)
			}
			continue
		}
		if ok != tc.wantOK || (ok && micro != tc.wantMicro) {
			t.Errorf("adminUsdtToMicro(%q) = (%d,%v), want (%d,%v)", tc.text, micro, ok, tc.wantMicro, tc.wantOK)
		}
	}
}

// ---- discounts.json missing-vs-empty ------------------------------------

func TestLoadDiscountsMissingVsEmpty(t *testing.T) {
	withDiscountsFile(t)
	defaultTiers := []discountTier{{50, 0.30}, {20, 0.20}, {10, 0.10}}

	if got := loadDiscounts(); !reflect.DeepEqual(got, defaultTiers) {
		t.Fatalf("missing file: loadDiscounts() = %+v, want default %+v", got, defaultTiers)
	}

	// The admin's "send 0 to clear" path persists an EMPTY list — must NOT
	// fall back to the default.
	if err := saveDiscounts(nil); err != nil {
		t.Fatalf("saveDiscounts(nil): %v", err)
	}
	if got := loadDiscounts(); len(got) != 0 {
		t.Fatalf("cleared file: loadDiscounts() = %+v, want empty", got)
	}

	// A corrupt file falls back to the default (Python's bare except).
	if err := os.WriteFile(discountsFilePath, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadDiscounts(); !reflect.DeepEqual(got, defaultTiers) {
		t.Fatalf("corrupt file: loadDiscounts() = %+v, want default %+v", got, defaultTiers)
	}

	// A real saved list round-trips sorted descending regardless of input order.
	if err := saveDiscounts([]discountTier{{10, 0.10}, {50, 0.30}, {20, 0.20}}); err != nil {
		t.Fatalf("saveDiscounts: %v", err)
	}
	if got := loadDiscounts(); !reflect.DeepEqual(got, defaultTiers) {
		t.Fatalf("round trip: loadDiscounts() = %+v, want %+v", got, defaultTiers)
	}
}

// TestDiscountForLiveEdit proves discountFor (catalog.go) re-reads
// discounts.json on every call, so an admin's edit changes the very next
// quote — no cache to invalidate (py_pricing_spec.md §2 "Side effects").
func TestDiscountForLiveEdit(t *testing.T) {
	withDiscountsFile(t)
	if got := discountFor(50); got != 0.30 {
		t.Errorf("default discountFor(50) = %v, want 0.30", got)
	}
	if err := saveDiscounts([]discountTier{{5, 0.50}}); err != nil {
		t.Fatal(err)
	}
	if got := discountFor(5); got != 0.50 {
		t.Errorf("after admin edit, discountFor(5) = %v, want 0.50", got)
	}
	if got := discountFor(1); got != 0 {
		t.Errorf("below every threshold, discountFor(1) = %v, want 0", got)
	}
}

// ---- adm:setdiscount handler + capture ----------------------------------

func TestSetDiscountFlow(t *testing.T) {
	withDiscountsFile(t)
	ctx := context.Background()
	b, f := newTestBot("1", nil)

	b.Process(ctx, callbackUpdate(1, "adm:setdiscount"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Volume discounts") {
		t.Fatalf("adm:setdiscount prompt missing: %+v", last)
	}

	b.Process(ctx, textUpdate(1, "10:5, 20:15, 50:25"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "updated") {
		t.Fatalf("expected success confirmation, got %+v", last)
	}
	got := loadDiscounts()
	want := []discountTier{{50, 0.25}, {20, 0.15}, {10, 0.05}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved discounts = %+v, want %+v", got, want)
	}

	// "0" clears everything.
	b.Process(ctx, callbackUpdate(1, "adm:setdiscount"))
	b.Process(ctx, textUpdate(1, "0"))
	if got := loadDiscounts(); len(got) != 0 {
		t.Fatalf("after clear, loadDiscounts() = %+v, want empty", got)
	}
}

// ---- promo handlers (create/list/delete) --------------------------------

func TestPromoHandlersLifecycle(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:promocodes"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "No promo codes yet") {
		t.Fatalf("expected empty promo list, got %+v", last)
	}

	b.Process(ctx, callbackUpdate(1, "adm:promonew"))
	b.Process(ctx, textUpdate(1, "20 100 off"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "Promo code created") || !strings.Contains(last.Text, "REPLACES") {
		t.Fatalf("expected promo-created confirmation, got %+v", last)
	}

	b.Process(ctx, callbackUpdate(1, "adm:promocodes"))
	last = lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "PROMO CODES") || !strings.Contains(last.Text, "20%") {
		t.Fatalf("expected the new promo in the list, got %+v", last)
	}
	dataBtns := buttonData(last.ReplyMarkup)
	var delBtn string
	for _, d := range dataBtns {
		if strings.HasPrefix(d, "promodel:") {
			delBtn = d
		}
	}
	if delBtn == "" {
		t.Fatalf("no promodel: button rendered, buttons=%v", dataBtns)
	}

	b.Process(ctx, callbackUpdate(1, delBtn))
	last = lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "No promo codes yet") {
		t.Fatalf("promo should be gone after delete, got %+v", last)
	}
}

// TestPromoNewStack proves the stack flag parses on/off correctly and the
// mode note matches.
func TestPromoNewStack(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:promonew"))
	b.Process(ctx, textUpdate(1, "10 5 on"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "stacks ON TOP") {
		t.Fatalf("expected stack-on confirmation, got %+v", last)
	}
}

// ---- min/max cross-validation, both SKUs --------------------------------

func TestApikeyCreditsCrossValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("bedrock", func(t *testing.T) {
		b, f, _, _, _ := newBuyBot(t, "1")
		st := b.states.Get(1)

		st.mu.Lock()
		st.SetApiMax = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "500"))
		if got := b.apikeyMaxCredits(saleClassBedrock); got != 500 {
			t.Fatalf("apikeyMaxCredits = %d, want 500", got)
		}

		// min ABOVE the current max is rejected, routed at the max screen.
		// Capture the live setting BEFORE the rejecting Process call: the
		// NEXT step below sets a real min (100), so asserting only the
		// message text would let a handler that wrongly persisted 501 pass
		// green — the overwrite would hide it.
		beforeMin := b.apikeyMinCredits(saleClassBedrock)
		st.mu.Lock()
		st.SetApiMin = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "501"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "above the current maximum") {
			t.Fatalf("min above max should be rejected, got %+v", last)
		}
		if !containsStr(buttonData(last.ReplyMarkup), "adm:apimax") {
			t.Errorf("rejection should route to adm:apimax, buttons=%v", buttonData(last.ReplyMarkup))
		}
		if got := b.apikeyMinCredits(saleClassBedrock); got != beforeMin {
			t.Errorf("rejected min=501 must not persist: apikeyMinCredits = %d, want unchanged %d", got, beforeMin)
		}

		st.mu.Lock()
		st.SetApiMin = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "100"))
		if got := b.apikeyMinCredits(saleClassBedrock); got != 100 {
			t.Fatalf("apikeyMinCredits = %d, want 100", got)
		}

		// max BELOW the current min is rejected.
		beforeMax := b.apikeyMaxCredits(saleClassBedrock)
		st.mu.Lock()
		st.SetApiMax = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "50"))
		last = lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "the per-order minimum") {
			t.Fatalf("max below min should be rejected, got %+v", last)
		}
		if got := b.apikeyMaxCredits(saleClassBedrock); got != beforeMax {
			t.Errorf("rejected max=50 must not persist: apikeyMaxCredits = %d, want unchanged %d", got, beforeMax)
		}
	})

	t.Run("kiro", func(t *testing.T) {
		b, f, _, _, _ := newBuyBot(t, "1")
		st := b.states.Get(1)

		st.mu.Lock()
		st.SetKiroMax = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "2000"))
		if got := b.apikeyMaxCredits(saleClassKiro); got != 2000 {
			t.Fatalf("kiro apikeyMaxCredits = %d, want 2000", got)
		}

		beforeKiroMin := b.apikeyMinCredits(saleClassKiro)
		st.mu.Lock()
		st.SetKiroMin = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "2001"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "above the current maximum") {
			t.Fatalf("kiro min above max should be rejected, got %+v", last)
		}
		if !containsStr(buttonData(last.ReplyMarkup), "adm:kiromax") {
			t.Errorf("kiro rejection should route to adm:kiromax, buttons=%v", buttonData(last.ReplyMarkup))
		}
		if got := b.apikeyMinCredits(saleClassKiro); got != beforeKiroMin {
			t.Errorf("rejected kiro min=2001 must not persist: apikeyMinCredits = %d, want unchanged %d", got, beforeKiroMin)
		}

		st.mu.Lock()
		st.SetKiroMin = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "600"))
		if got := b.apikeyMinCredits(saleClassKiro); got != 600 {
			t.Fatalf("kiro apikeyMinCredits = %d, want 600", got)
		}

		beforeKiroMax := b.apikeyMaxCredits(saleClassKiro)
		st.mu.Lock()
		st.SetKiroMax = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "500"))
		last = lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "the per-order minimum") {
			t.Fatalf("kiro max below min should be rejected, got %+v", last)
		}
		if got := b.apikeyMaxCredits(saleClassKiro); got != beforeKiroMax {
			t.Errorf("rejected kiro max=500 must not persist: apikeyMaxCredits = %d, want unchanged %d", got, beforeKiroMax)
		}

		// Bedrock and Kiro bounds must not cross-contaminate.
		if got := b.apikeyMaxCredits(saleClassBedrock); got == 2000 {
			t.Errorf("bedrock max should be unaffected by the kiro flow, got %d", got)
		}
	})
}

// ---- abort (pop) vs rearm (peek) semantics -------------------------------

func TestAbortVsRearmSemantics(t *testing.T) {
	ctx := context.Background()

	t.Run("discount pop aborts on invalid", func(t *testing.T) {
		withDiscountsFile(t)
		b, f := newTestBot("1", nil)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Discount = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "not valid input"))
		st.mu.Lock()
		armed := st.Discount
		st.mu.Unlock()
		if armed {
			t.Error("capDiscount must POP the slot even on invalid input (abort semantics)")
		}
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Invalid") {
			t.Errorf("expected the invalid-input reply, got %+v", last)
		}
	})

	t.Run("promo pop aborts on invalid", func(t *testing.T) {
		b, f := newTestBot("1", nil)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Promo = true
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "not enough fields"))
		st.mu.Lock()
		armed := st.Promo
		st.mu.Unlock()
		if armed {
			t.Error("capPromo must POP the slot even on invalid input (abort semantics)")
		}
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Invalid") {
			t.Errorf("expected the invalid-input reply, got %+v", last)
		}
	})

	t.Run("tier peek re-arms on invalid", func(t *testing.T) {
		b, f := newTestBot("1", nil)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Tier = "p1"
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "BADCODE notanumber"))
		st.mu.Lock()
		product := st.Tier
		st.mu.Unlock()
		if product != "p1" {
			t.Error("capTier must leave the slot ARMED on invalid input (rearm semantics)")
		}
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "CODE PRICE") {
			t.Errorf("expected the re-prompt, got %+v", last)
		}
	})

	t.Run("price peek re-arms on invalid", func(t *testing.T) {
		b, f := newTestBot("1", nil)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Price = &PriceCtx{Product: "p1", Code: "KBH"}
		st.mu.Unlock()
		b.Process(ctx, textUpdate(1, "not a number"))
		st.mu.Lock()
		armed := st.Price
		st.mu.Unlock()
		if armed == nil {
			t.Error("capPrice must leave the slot ARMED on invalid input (rearm semantics)")
		}
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Invalid price") {
			t.Errorf("expected the re-prompt, got %+v", last)
		}
	})

	t.Run("setapiprice pop aborts on invalid (one of the 8 redundant-gate handlers)", func(t *testing.T) {
		b, f, _, _, _ := newBuyBot(t, "1")
		st := b.states.Get(1)
		beforeRate := b.apikeyRateUSDT(saleClassBedrock)
		st.mu.Lock()
		st.SetApiPrice = true
		st.mu.Unlock()
		b.Process(context.Background(), textUpdate(1, "99999999"))
		st.mu.Lock()
		armed := st.SetApiPrice
		st.mu.Unlock()
		if armed {
			t.Error("capSetApiPrice must POP the slot even on invalid input")
		}
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "USDT per 100 credits") {
			t.Errorf("expected the invalid-input reply, got %+v", last)
		}
		if got := b.apikeyRateUSDT(saleClassBedrock); got != beforeRate {
			t.Errorf("rejected rate=99999999 must not persist: apikeyRateUSDT = %v, want unchanged %v", got, beforeRate)
		}
	})
}

// ---- full tier lifecycle through the real stock machine ------------------

func TestPriceTierLifecycle(t *testing.T) {
	ctx := context.Background()
	b, f, _, stk, dir := newBuyBot(t, "1")
	seedProduct(t, dir, "prod1", 90000, 1)

	// adm:setprice lists the product.
	b.Process(ctx, callbackUpdate(1, "adm:setprice"))
	last := lastSendMsg(f)
	if last == nil || !containsStr(buttonData(last.ReplyMarkup), "priceprod:0") {
		t.Fatalf("expected priceprod:0 button, got %+v", last)
	}

	// priceprod:0 -> the tier list for prod1 (seeded with a KBH price).
	b.Process(ctx, callbackUpdate(1, "priceprod:0"))
	last = lastSendMsg(f)
	if last == nil || !containsStr(buttonData(last.ReplyMarkup), "tieradd:0") {
		t.Fatalf("expected tieradd:0 button, got %+v", last)
	}

	// tieradd:0 -> arm, then type "BH30D 9" to add a new tier.
	b.Process(ctx, callbackUpdate(1, "tieradd:0"))
	b.Process(ctx, textUpdate(1, "BH30D 9"))
	last = lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "Added tier") {
		t.Fatalf("expected an Added-tier confirmation, got %+v", last)
	}
	if got, ok := stk.ProductPrice("prod1", "BH30D"); !ok || got != 9_000_000 { // 9 USDT * 1e6 micro
		t.Fatalf("ProductPrice(BH30D) = (%d,%v), want (9000000,true)", got, ok)
	}

	// pricetier:0:BH30D -> edit its price to 5 USDT.
	b.Process(ctx, callbackUpdate(1, "pricetier:0:BH30D"))
	last = lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "Current:") {
		t.Fatalf("expected a Current: price line, got %+v", last)
	}
	b.Process(ctx, textUpdate(1, "5"))
	last = lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "set to") {
		t.Fatalf("expected a set-to confirmation, got %+v", last)
	}
	if got, ok := stk.ProductPrice("prod1", "BH30D"); !ok || got != 5_000_000 {
		t.Fatalf("ProductPrice(BH30D) after edit = (%d,%v), want (5000000,true)", got, ok)
	}

	// tierdel:0 -> lists tap-to-delete buttons; tierdo:0:BH30D removes it.
	b.Process(ctx, callbackUpdate(1, "tierdel:0"))
	last = lastSendMsg(f)
	var delBtn string
	for _, d := range buttonData(last.ReplyMarkup) {
		if strings.HasPrefix(d, "tierdo:0:BH30D") {
			delBtn = d
		}
	}
	if delBtn == "" {
		t.Fatalf("no tierdo:0:BH30D button rendered, got %+v", buttonData(last.ReplyMarkup))
	}
	b.Process(ctx, callbackUpdate(1, delBtn))
	last = lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "Removed tier") {
		t.Fatalf("expected a Removed-tier confirmation, got %+v", last)
	}
	if _, ok := stk.ProductPrice("prod1", "BH30D"); ok {
		t.Error("BH30D should be gone after tierdo:")
	}
}

// TestTierDelNoTiers proves the "no tiers to remove" early exit — deleting
// a product's LAST tier via tierdo: then reopening tierdel: must not offer
// an empty delete list.
func TestTierDelNoTiers(t *testing.T) {
	ctx := context.Background()
	b, f, _, stk, dir := newBuyBot(t, "1")
	_ = stk
	// Seed a product with NO price file at all (0 tiers).
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "bare", "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.Process(ctx, callbackUpdate(1, "priceprod:0"))
	b.Process(ctx, callbackUpdate(1, "tierdel:0"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "No tiers to remove") {
		t.Fatalf("expected the no-tiers warning, got %+v", last)
	}
}

// ---- kirotoggle re-render -------------------------------------------------

// TestKiroToggleRerendersSubmenu proves kirotoggle is the ONE handler in
// this category that re-renders the pricing submenu in place after acting
// (py_pricing_spec.md §5) — every other action sends a plain confirmation.
func TestKiroToggleRerendersSubmenu(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1")

	before := f.sendCount()
	b.Process(ctx, callbackUpdate(1, "adm:kirotoggle"))
	after := f.sendCount()
	// TWO sends: the advisory note, then the re-rendered submenu.
	if after-before != 2 {
		t.Fatalf("kirotoggle sent %d messages, want 2 (note + re-render)", after-before)
	}
	f.mu.Lock()
	noteMsg := f.Sends[len(f.Sends)-2]
	menuMsg := f.Sends[len(f.Sends)-1]
	f.mu.Unlock()
	if !strings.Contains(noteMsg.Text, "Kiro Credits SKU: ON") {
		t.Errorf("first send should be the toggle note, got %q", noteMsg.Text)
	}
	if !strings.Contains(menuMsg.Text, "PRICING & DISCOUNTS") {
		t.Errorf("second send should be the re-rendered pricing submenu, got %q", menuMsg.Text)
	}
	if !containsStr(buttonData(menuMsg.ReplyMarkup), "adm:kiroprice") {
		t.Errorf("re-rendered submenu missing adm:kiroprice, buttons=%v", buttonData(menuMsg.ReplyMarkup))
	}
	if !b.kiroApikeyEnabled() {
		t.Error("kiro_apikey_enabled should now be true")
	}

	// Toggling again flips it back OFF, no advisory warnings this time.
	b.Process(ctx, callbackUpdate(1, "adm:kirotoggle"))
	if b.kiroApikeyEnabled() {
		t.Error("second toggle should turn it back OFF")
	}
}

// ---- super-admin gate ------------------------------------------------------

// TestPricingCategorySuperAdminOnly proves a standard (granted, non-super)
// admin is refused every callback in this category, matching
// py_pricing_spec.md §0's "no capability key unlocks this category" rule.
//
// H1 fix: none of these callbacks appear in admincap.go's stdAdminCbCap
// table (pricing has no capability at all, matching Python's _CAT_CAPS,
// which omits "pricing" entirely), so command.go's central chokepoint now
// denies every one of them via a pre-ack ALERT before settingsSuperAdminOnly
// ever runs — the security outcome (denied) is unchanged, but the assertion
// moves from lastSendMsg (a chat message) to wantAdminCapDenied (an alert;
// see that helper's doc comment in admin_test.go for why the two mechanisms
// aren't interchangeable).
func TestPricingCategorySuperAdminOnly(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil) // super admin is chat 1
	b.adminsMu.Lock()
	b.admins["7"] = true // chat 7 is a STANDARD admin (isAdmin true, isSuperAdmin false)
	b.adminsMu.Unlock()

	for _, data := range []string{
		"adm:setprice", "adm:setdiscount", "adm:promocodes", "adm:promonew",
		"adm:apiprice", "adm:apimin", "adm:apimax", "adm:apicap",
		"adm:kirotoggle", "adm:kiroprice", "adm:kiromin", "adm:kiromax",
		// The Go-only Fixed Pricing SKU rows (adminpricing.go §6).
		"adm:fixedtoggle", "adm:fixedprice", "adm:fixedmin", "adm:fixedmax",
		"adm:fixedname",
		// The Go-only Fixed Pricing rate-card entries (adminpricing.go §4c) —
		// adm:modelprices in particular is the only READ-ONLY action in this
		// category, so its gate is easy to forget.
		"adm:setmodelprice", "adm:modelprices",
		// The 6 standalone tier/promo callback prefixes (cbRoutes, not
		// captures) each start with their own settingsSuperAdminOnly check —
		// prove the gate fires there too, not just on the 11 capture-armed
		// entry points above.
		"priceprod:0", "pricetier:0:KBH", "tieradd:0", "tierdel:0", "tierdo:0:KBH", "promodel:SOMECODE",
	} {
		f.Sends, f.Answers = nil, nil
		b.Process(ctx, callbackUpdate(7, data))
		wantAdminCapDenied(t, f, data)
	}
}

// ---- Task #13 Tiers 3-4: pricing/discount/promo audit logging -------------
// .omc/artifacts/config-audit-spec.md — the money-defining set: product
// prices, apikey pricing, credit floors/ceilings/caps, volume discounts,
// promo codes, max_qty (adminsettings.go), the Kiro SKU toggle. capSetRate is
// deliberately NOT audited — the USDT migration's S5 deletes it outright.

// newReadOnlyCfgAuditBot builds a Bot with cfg rooted in a directory with no
// write permission, so every SaveSettings call fails — the shared setup this
// file's several "no audit row on a failed save" tests (spec §4.3/§6.2) need,
// factored out once rather than duplicated at each call site (identical
// boilerplate to adminsettings_test.go's newAuditTestBot, minus stock, which
// none of these handlers touch).
func newReadOnlyCfgAuditBot(t *testing.T) (*Bot, *fakeTG, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	cfg, err := config.Load(roDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(b.Shutdown)
	return b, f, st
}

// newQueryOnlyStoreBot builds a Bot wired to a real, schema-initialized
// *store.Store, then injects a targeted write failure for exactly the tables
// named in faultTables — a BEFORE INSERT and a BEFORE DELETE trigger per
// table, each unconditionally RAISE(ABORT, ...) — the fault injection
// capPromo/cbPromoDel/execAddBal need, since none of them fails on a config
// write (there is none in their path): each calls a plain store.* write
// directly (CreatePromo/DeletePromo/CreditBalance), and store.Store wraps a
// concrete *sql.DB with no interface seam to substitute a fake behind. The
// newReadOnlyCfgAuditBot technique above (chmod a directory read-only
// before Load) does NOT transfer here — verified empirically before writing
// this helper: store.Open keeps ONE already-open, already-writable file
// descriptor alive for the whole test (SetMaxOpenConns(1)), and POSIX
// permission bits are checked at open(2), not at each write(2), so
// chmod-after-Open on the db file/directory left every write still
// succeeding.
//
// THE TRAP an earlier version of this helper fell into, for the next site
// that reaches for fault injection here: that version flipped the WHOLE
// connection read-only with `PRAGMA query_only=ON`, which blocks every
// write — including the audit trail's own INSERT. Every "no audit row was
// written" test built on it was vacuous either way: guard present -> the
// audited call is never reached -> no row; guard DELETED -> the audited
// call IS reached, but its INSERT fails on the same read-only connection ->
// still no row (store.LogAction swallows that error by contract — see its
// own doc comment). `latestAuditRow(...) == nil` passed regardless of
// whether the guard existed; it was only proving the fault injection itself
// was switched on (task #32). This is the EXACT OPPOSITE of
// newReadOnlyCfgAuditBot above: that helper's chmod'd directory breaks only
// config SAVES, leaving the database — audit table included — fully
// healthy, so a deleted guard there really does let a row through and the
// assertion is real. Same-shaped `latestAuditRow` line, opposite truth
// value, depending on which thing the harness broke underneath it — not
// something grep can tell apart. Table-scoped triggers restore the
// property here: the target table's write fails exactly as before, but
// `audit` is left writable, so "no audit row" once again means the guard
// held.
//
// seed, if non-nil, runs BEFORE the triggers are installed, so a test
// needing an existing row in a table it's about to fault-inject (e.g. a
// promo code for the delete-failure path) can create it while writes still
// work.
func newQueryOnlyStoreBot(t *testing.T, seed func(*store.Store), faultTables ...string) (*Bot, *fakeTG, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if seed != nil {
		seed(st)
	}
	for _, table := range faultTables {
		for _, verb := range []string{"INSERT", "DELETE"} {
			trig := fmt.Sprintf(
				"CREATE TRIGGER fault_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'injected write failure'); END",
				strings.ToLower(verb), table, verb, table)
			if _, err := st.DB().Exec(trig); err != nil {
				t.Fatalf("create fault trigger on %s/%s: %v", table, verb, err)
			}
		}
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, AdminID: "1"})
	t.Cleanup(b.Shutdown)
	return b, f, st
}

// ---- product price (capTier / capPrice): SAME underlying stock mutation ---

// TestCapTierAndCapPriceAuditProductPriceChanges covers BOTH code paths that
// funnel into stock.SetProductPrice + the shared audit call: capTier
// (tieradd:, brand-new tier -> old side is "(unset)") and capPrice
// (pricetier:, editing an existing tier -> old side is the real prior
// price). One test, since the fixture setup (a seeded product) is shared and
// the scenario is a natural lifecycle, matching TestPriceTierLifecycle's own
// add-then-edit shape.
func TestCapTierAndCapPriceAuditProductPriceChanges(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "1")
	seedProduct(t, dir, "prod1", 90000, 1)

	// tieradd:0 -> "BH30D 9": a BRAND NEW tier, so old must read "(unset)".
	b.Process(ctx, callbackUpdate(1, "priceprod:0"))
	b.Process(ctx, callbackUpdate(1, "tieradd:0"))
	b.Process(ctx, textUpdate(1, "BH30D 9"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Added tier") {
		t.Fatalf("expected an Added-tier confirmation, got %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_product_price")
	if row1 == nil {
		t.Fatal("no audit row written for new tier creation")
	}
	if row1.Target != "prod1/BH30D" {
		t.Errorf("Target = %q, want prod1/BH30D", row1.Target)
	}
	if want := "(unset) → " + b.usdtDisp(9_000_000); row1.Outcome != want { // 9 USDT * 1e6 micro
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	// pricetier:0:BH30D -> "5": editing the tier just created, so old must be
	// the REAL prior price (9,000,000 micro = 9 USDT), not "(unset)" again.
	b.Process(ctx, callbackUpdate(1, "pricetier:0:BH30D"))
	b.Process(ctx, textUpdate(1, "5"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "set to") {
		t.Fatalf("expected a set-to confirmation, got %+v", last)
	}
	row2 := latestAuditRow(t, st, "config_set_product_price")
	if row2 == nil {
		t.Fatal("no audit row written for tier edit")
	}
	if want := b.usdtDisp(9_000_000) + " → " + b.usdtDisp(5_000_000); row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestCapTierNoAuditOnSaveFailure proves the audit row is emitted only AFTER
// stock.SetProductPrice succeeds (spec §4.3). Unlike the cfg-based sites, a
// product price write fails via a read-only PRODUCT directory (SetProductPrice
// writes a plain file, not settings.json), so this can't reuse
// newReadOnlyCfgAuditBot above.
func TestCapTierNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, dir := newBuyBot(t, "1")
	seedProduct(t, dir, "prod1", 90000, 1)

	productDir := filepath.Join(dir, "accounts", "prod1")
	if err := os.Chmod(productDir, 0o500); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(productDir, 0o700) })

	b.Process(ctx, callbackUpdate(1, "priceprod:0"))
	b.Process(ctx, callbackUpdate(1, "tieradd:0"))
	b.Process(ctx, textUpdate(1, "BH30D 9"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Failed to write price") {
		t.Fatalf("expected a write-failure reply, got %+v", last)
	}
	if row := latestAuditRow(t, st, "config_set_product_price"); row != nil {
		t.Fatalf("audit row written despite a failed price write: %+v", *row)
	}
}

// ---- volume discounts (capDiscount) ---------------------------------------

// TestCapDiscountAuditsSetAndClear covers BOTH branches capDiscount can take
// — a real tier set and the "0 clears everything" branch — asserting the
// compact fmtDiscountTiers rendering on both sides of each row.
func TestCapDiscountAuditsSetAndClear(t *testing.T) {
	ctx := context.Background()
	withDiscountsFile(t)
	b, f, st := newAuditTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:setdiscount"))
	b.Process(ctx, textUpdate(1, "10:5, 20:15, 50:25"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "updated") {
		t.Fatalf("expected success confirmation, got %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_discounts")
	if row1 == nil {
		t.Fatal("no audit row written for discount set")
	}
	if row1.Target != "discounts.json" {
		t.Errorf("Target = %q, want discounts.json", row1.Target)
	}
	// withDiscountsFile points at a fresh, nonexistent file -> loadDiscounts
	// falls back to discountsDefaultTiers (spec: file absent is a real
	// default state, not "(unset)").
	if want := "50:30,20:20,10:10 → 50:25,20:15,10:5"; row1.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	b.Process(ctx, callbackUpdate(1, "adm:setdiscount"))
	b.Process(ctx, textUpdate(1, "0"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "cleared") {
		t.Fatalf("expected clear confirmation, got %+v", last)
	}
	row2 := latestAuditRow(t, st, "config_set_discounts")
	if row2 == nil {
		t.Fatal("no audit row written for discount clear")
	}
	if want := "50:25,20:15,10:5 → (none)"; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestCapDiscountNoAuditOnSaveFailure proves the audit row is emitted only
// AFTER saveDiscounts succeeds (spec §4.3) — capDiscount already has the H1
// stop-on-failure shape (settingsSaveFailed + return true), unlike the
// apikey/kiro pricing setters below.
func TestCapDiscountNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	dir := withDiscountsFile(t)
	if err := os.Chmod(dir, 0o500); err != nil { // discountsFilePath.tmp can't be created here
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	b, f, st := newAuditTestBot(t, "1")
	b.Process(ctx, callbackUpdate(1, "adm:setdiscount"))
	b.Process(ctx, textUpdate(1, "10:5"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_discounts"); row != nil {
		t.Fatalf("audit row written despite failed saveDiscounts: %+v", *row)
	}
}

// ---- promo codes (capPromo / cbPromoDel) -----------------------------------

// TestCapPromoAuditsCreateAndDelete covers both promo code paths. A promo's
// code is auto-generated (genUniqueCode) rather than admin-typed, so the
// created code is read back off the audit row's OWN Target field — no
// separate list-rendering round-trip needed to learn it.
//
// No-audit-on-failure case deliberately NOT added for either path: CreatePromo
// only fails on a genuine DB error (its inputs are validated by capPromo
// before the call), and DeletePromo's DELETE is a no-op success on a
// nonexistent code rather than an error — neither has a way to force a
// deterministic failure through the bot's own validated call surface without
// reaching into store internals, unlike the read-only-directory trick that
// works for every settings.json/file-based site in this package.
func TestCapPromoAuditsCreateAndDelete(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:promonew"))
	b.Process(ctx, textUpdate(1, "20 100 off"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Promo code created") {
		t.Fatalf("expected promo-created confirmation, got %+v", last)
	}
	rowCreate := latestAuditRow(t, st, "config_set_promo_create")
	if rowCreate == nil {
		t.Fatal("no audit row written for promo create")
	}
	code := rowCreate.Target
	if code == "" {
		t.Fatal("audit row Target must be the new promo's code")
	}
	if want := "(unset) → 20% / 100 uses / stack=off"; rowCreate.Outcome != want {
		t.Errorf("create Outcome = %q, want %q", rowCreate.Outcome, want)
	}

	b.Process(ctx, callbackUpdate(1, cbData("promodel", code)))
	rowDelete := latestAuditRow(t, st, "config_set_promo_delete")
	if rowDelete == nil {
		t.Fatal("no audit row written for promo delete")
	}
	if rowDelete.Target != code {
		t.Errorf("delete Target = %q, want %q", rowDelete.Target, code)
	}
	if want := "20% / 100 uses / stack=off → (deleted)"; rowDelete.Outcome != want {
		t.Errorf("delete Outcome = %q, want %q", rowDelete.Outcome, want)
	}
}

// ---- Kiro SKU toggle (cbKiroToggle) -----------------------------------------

// TestKiroToggleAuditsChange proves cbKiroToggle's boolean flip is audited
// with the "config_toggle_" action prefix (the established convention for
// every toggle in this codebase — toggleSetting, cbToggleSales,
// cbTogglePrivate), not "config_set_".
func TestKiroToggleAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:kirotoggle")) // fresh config: OFF -> ON
	row := latestAuditRow(t, st, "config_toggle_kiro_apikey_enabled")
	if row == nil {
		t.Fatal("no audit row written for kiro_apikey_enabled toggle")
	}
	if row.Target != "kiro_apikey_enabled" {
		t.Errorf("Target = %q, want kiro_apikey_enabled", row.Target)
	}
	if want := "false → true"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}

	b.Process(ctx, callbackUpdate(1, "adm:kirotoggle")) // ON -> OFF
	row2 := latestAuditRow(t, st, "config_toggle_kiro_apikey_enabled")
	if row2 == nil {
		t.Fatal("no audit row written for second kiro_apikey_enabled toggle")
	}
	if want := "true → false"; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestKiroToggleNoAuditOnSaveFailure proves cbKiroToggle's audit row is
// emitted only after SaveSettings succeeds — this handler DOES have the H1
// stop-on-failure shape (returns early on a save error), unlike the pricing
// setters below. It also pins task #29's error-text fix: cbKiroToggle used
// to send settingsNotConfigured ("Settings not configured on this
// instance") on a save failure, even though b.cfg is non-nil and settings
// ARE configured here — only the disk write failed. That text sent the
// admin toward re-checking setup instead of toward the real fault, and this
// test used to assert that WRONG string as the expected reply (a fix
// pinned as a bug). The correct reply is settingsSaveFailed, the same text
// every sibling setter in this file already sends on the identical
// failure.
func TestKiroToggleNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	b, f, st := newReadOnlyCfgAuditBot(t)

	b.Process(ctx, callbackUpdate(1, "adm:kirotoggle"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_toggle_kiro_apikey_enabled"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// ---- apikey/kiro rate + credit-bound setters -------------------------------
//
// capSetApiPrice/capSetApiMin/capSetApiMax/capSetApiCap/capSetKiroPrice/
// capSetKiroMin/capSetKiroMax all now follow the H1 stop-on-failure shape:
// on a SaveSettings error they log, send the shared settingsSaveFailed reply,
// and return early — never falling through to the ✅ success text. The audit
// row is emitted only AFTER the save succeeds (spec §4.3/§6.2), so it now
// sits on the straight-line path after the early-return gate rather than in
// an `else` branch. This matches capDiscount's own two branches exactly.
//
// Task #29 review finding: this file used to claim the failure-path gate on
// the five siblings below (capSetApiMax/Cap, capSetKiroPrice/Min/Max) was
// "covered by construction" from only capSetApiPrice's and capSetApiMin's
// two tests — identical code at a site is not evidence the code at that
// site was ever exercised, and this repo's own rule rejects that reasoning
// ("a fix is not done when it is correct; it is done when deleting it turns
// something red"). Every one of the seven setters — plus capPromo/
// cbPromoDel below, whose failure mode is a store write, not a config
// write — now has its own dedicated NoAuditOnSaveFailure/NoAuditOnFailure
// test, each mutation-verified by deleting that setter's early-return guard
// and confirming its OWN test (not a neighbor's) goes red.

// TestCapSetApiPriceAuditsChange is the representative test for the
// "float rate + legacy twin" code path (capSetApiPrice only — capSetKiroPrice
// is the sibling shape minus the twin, covered separately below).
func TestCapSetApiPriceAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:apiprice"))
	b.Process(ctx, textUpdate(1, "4"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "API-key price set to 4") {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_apikey_usdt_per_100")
	if row1 == nil {
		t.Fatal("no audit row written for first apikey_usdt_per_100 change")
	}
	if want := "0 → 4"; row1.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	b.Process(ctx, callbackUpdate(1, "adm:apiprice"))
	b.Process(ctx, textUpdate(1, "6"))
	row2 := latestAuditRow(t, st, "config_set_apikey_usdt_per_100")
	if row2 == nil {
		t.Fatal("no audit row written for second apikey_usdt_per_100 change")
	}
	if want := "4 → 6"; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestCapSetApiPriceNoAuditOnSaveFailure proves the H1 stop-on-failure shape
// for capSetApiPrice: on a failed SaveSettings the audit row is NOT emitted,
// the reply is the shared settingsSaveFailed text (not the ✅ success text a
// real save would have earned), and the capture itself still reports the
// input as CONSUMED (returns true) — an admin's retyped number must not
// fall through to a lower-priority dispatch stage just because the save
// underneath it failed. The six sibling pricing setters each get their own
// dedicated test below/nearby rather than relying on this one by
// construction (task #29 review finding — see the block comment above).
//
// Falsification: dropping the `return true` on save error (so the handler
// falls through to the ✅ text) makes the settingsSaveFailed assertion fail;
// hoisting the auditConfigSet call out of the post-save path (auditing
// unconditionally) makes the audit-row assertion fail — both verified by
// mutation against this test.
func TestCapSetApiPriceNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:apiprice", "4", b.capSetApiPrice)
	if !got {
		t.Errorf("capSetApiPrice returned %v, want true (input consumed even on save failure)", got)
	}
	// H1 fix: a failed SaveSettings now surfaces as settingsSaveFailed rather
	// than the ✅ "API-key price set to ..." success text a silent failure
	// used to produce.
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_apikey_usdt_per_100"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetKiroPriceAuditsChange is capSetApiPrice's sibling for the Kiro
// SKU — same rate-setter shape, minus the legacy per-1k twin.
func TestCapSetKiroPriceAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:kiroprice"))
	b.Process(ctx, textUpdate(1, "0.7"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Kiro-credit price set to 0.7") {
		t.Fatalf("valid reply = %+v", last)
	}
	row := latestAuditRow(t, st, "config_set_kiro_apikey_usdt_per_100")
	if row == nil {
		t.Fatal("no audit row written for kiro_apikey_usdt_per_100 change")
	}
	if want := "0 → 0.7"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}
}

// TestCapSetApiMinAuditsChange is the representative test for the
// "cross-validated int credit-bound setter" code path shared by
// capSetApiMin/capSetApiMax/capSetApiCap/capSetKiroMin/capSetKiroMax — same
// (read effective old via the existing accessor, write, gate audit on
// err == nil) shape with only the settings key and accessor varying.
// TestCapSetKiroMinAuditsChange below proves the shape also holds across the
// bedrock/kiro saleClass boundary; capSetApiMax/capSetApiCap/capSetKiroMax
// are covered by construction (identical code added at each site) rather
// than retested individually — spec review guidance: test at the
// granularity of the code path, not per setting.
func TestCapSetApiMinAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:apimin"))
	b.Process(ctx, textUpdate(1, "200"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Min API credits per order set to 200") {
		t.Fatalf("valid reply = %+v", last)
	}
	row1 := latestAuditRow(t, st, "config_set_apikey_min_credits")
	if row1 == nil {
		t.Fatal("no audit row written for first apikey_min_credits change")
	}
	if want := "100 → 200"; row1.Outcome != want { // apikeyMinCredits' env-less default is 100
		t.Errorf("Outcome = %q, want %q", row1.Outcome, want)
	}

	b.Process(ctx, callbackUpdate(1, "adm:apimin"))
	b.Process(ctx, textUpdate(1, "300"))
	row2 := latestAuditRow(t, st, "config_set_apikey_min_credits")
	if row2 == nil {
		t.Fatal("no audit row written for second apikey_min_credits change")
	}
	if want := "200 → 300"; row2.Outcome != want {
		t.Errorf("second Outcome = %q, want %q", row2.Outcome, want)
	}
}

// TestCapSetApiMinNoAuditOnSaveFailure proves the same H1 stop-on-failure
// shape (documented above capSetApiPrice's tests) holds for the int-setter
// path too — a second, independent instance of the mechanism, not just the
// rate-setter one. The settingsSaveFailed reply, the missing audit row, AND
// the capture's own true return (input consumed) are all asserted.
func TestCapSetApiMinNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:apimin", "200", b.capSetApiMin)
	if !got {
		t.Errorf("capSetApiMin returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_apikey_min_credits"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetKiroMinAuditsChange proves the int-setter audit shape transfers
// correctly to the kiro_ prefixed keys / saleClassKiro accessor — the one
// genuine boundary capSetApiMin's coverage above doesn't reach.
func TestCapSetKiroMinAuditsChange(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:kiromin"))
	b.Process(ctx, textUpdate(1, "1000"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Min KIRO credits per order set to 1,000") {
		t.Fatalf("valid reply = %+v", last)
	}
	row := latestAuditRow(t, st, "config_set_kiro_apikey_min_credits")
	if row == nil {
		t.Fatal("no audit row written for kiro_apikey_min_credits change")
	}
	if want := "500 → 1000"; row.Outcome != want { // apikeyMinCredits' kiro env-less default is 500
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}
}

// ---- task #29: the five remaining setters, individually pinned ------------
//
// TestCapSetApiPriceNoAuditOnSaveFailure/TestCapSetApiMinNoAuditOnSaveFailure
// above were the only two of nine H1 stop-on-failure setters with their own
// failure-path test; the rest were "covered by construction". Each test
// below is mutation-verified on its OWN setter (delete that setter's
// early-return guard, confirm THIS test — not a neighbor's — goes red,
// restore) so a future edit that drops the guard at exactly one site can no
// longer hide behind a passing suite.

// TestCapSetApiMaxNoAuditOnSaveFailure pins capSetApiMax (adminpricing.go).
func TestCapSetApiMaxNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:apimax", "20000", b.capSetApiMax)
	if !got {
		t.Errorf("capSetApiMax returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_apikey_max_credits"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetApiCapNoAuditOnSaveFailure pins capSetApiCap (adminpricing.go).
func TestCapSetApiCapNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:apicap", "3", b.capSetApiCap)
	if !got {
		t.Errorf("capSetApiCap returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_apikey_max_per_customer"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetKiroPriceNoAuditOnSaveFailure pins capSetKiroPrice
// (adminpricing.go).
func TestCapSetKiroPriceNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:kiroprice", "0.7", b.capSetKiroPrice)
	if !got {
		t.Errorf("capSetKiroPrice returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_kiro_apikey_usdt_per_100"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetKiroMinNoAuditOnSaveFailure pins capSetKiroMin (adminpricing.go).
func TestCapSetKiroMinNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:kiromin", "1000", b.capSetKiroMin)
	if !got {
		t.Errorf("capSetKiroMin returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_kiro_apikey_min_credits"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetKiroMaxNoAuditOnSaveFailure pins capSetKiroMax (adminpricing.go).
func TestCapSetKiroMaxNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:kiromax", "100000", b.capSetKiroMax)
	if !got {
		t.Errorf("capSetKiroMax returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_kiro_apikey_max_credits"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// ---- capPromo / cbPromoDel: store-write failures, not config-write --------
//
// capPromo/cbPromoDel don't touch b.cfg.SaveSettings at all — their H1
// guard is against b.store.CreatePromo/DeletePromo failing instead
// (adminpricing.go's own "H1 stop-on-failure" comments on both call sites).
// newReadOnlyCfgAuditBot's directory-permission trick has no purchase here
// (config writes never happen on this path), so these two use
// newQueryOnlyStoreBot instead — see that helper's doc comment for why the
// simpler filesystem trick doesn't transfer to a live SQLite connection.

// TestCapPromoNoAuditOnSaveFailure pins capPromo's CreatePromo failure path
// — was structurally unpinnable before newQueryOnlyStoreBot existed (no
// fault-injecting store fake in this package).
func TestCapPromoNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newQueryOnlyStoreBot(t, nil, "promo_codes")

	got := armAndCapture(t, b, 1, "adm:promonew", "20 100 off", b.capPromo)
	if !got {
		t.Errorf("capPromo returned %v, want true (input consumed even on a failed create)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_promo_create"); row != nil {
		t.Fatalf("audit row written despite failed CreatePromo: %+v", *row)
	}
}

// TestCbPromoDelNoAuditOnSaveFailure pins cbPromoDel's DeletePromo failure
// path. cbPromoDel is a plain callback (cbCtx), not a capture, so it has no
// bool to assert. The real guard-deletion detector is the audit-row check
// below (now load-bearing since newQueryOnlyStoreBot scopes the fault to
// promo_codes and leaves audit writable — task #32). The promo-still-exists
// check below is NOT a substitute for that, despite an earlier version of
// this comment claiming it stood in for the bool assertions on the
// capture-shaped setters above: with DeletePromo's write genuinely failing
// (that's the fault injection, not the guard), the promo persists whether or
// not the guard runs — the guard's only observable effect is whether the
// code stops before ever reaching auditConfigSet or falls through to it.
// Kept anyway because "the failed delete truly didn't land" is still worth
// pinning on its own, just not as guard-deletion evidence.
func TestCbPromoDelNoAuditOnSaveFailure(t *testing.T) {
	ctx := context.Background()
	const code = "DELFAIL"
	b, f, st := newQueryOnlyStoreBot(t, func(st *store.Store) {
		if err := st.CreatePromo(code, 10, 5, false); err != nil {
			t.Fatalf("seed promo: %v", err)
		}
	}, "promo_codes")

	b.Process(ctx, callbackUpdate(1, cbData("promodel", code)))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_promo_delete"); row != nil {
		t.Fatalf("audit row written despite failed DeletePromo: %+v", *row)
	}
	promos, err := st.ListPromos()
	if err != nil {
		t.Fatalf("ListPromos: %v", err)
	}
	found := false
	for _, p := range promos {
		if p.Code == code {
			found = true
		}
	}
	if !found {
		t.Error("promo must still exist after a failed delete — the failure was not actually applied")
	}
}

// ---- SKU display rename (adm:apiname / adm:kironame) -----------------------
//
// The wizard is a copy of the cbApiMin/capSetApiMin shape (super-admin gate,
// armed ChatState flag, POP capture, save-then-audit), so these tests target
// what is genuinely NEW: the settings.json sub-map write, the "-" restore
// path, the input validation, and the fact that renaming one SKU leaves the
// other alone.

// apikeyNamesFrom reads the live apikey_names sub-map out of a bot's config,
// returning nil when the key is absent — the shape assertions below all go
// through this rather than through apikeyProductName, so they observe what was
// actually PERSISTED, not what the accessor would have defaulted to.
func apikeyNamesFrom(t *testing.T, b *Bot) map[string]any {
	t.Helper()
	if b.cfg == nil {
		t.Fatal("bot has no config")
	}
	m, _ := b.cfg.Settings()["apikey_names"].(map[string]any)
	return m
}

// TestCapSetApiNameSavesAndAudits: a valid name lands in
// settings.json apikey_names.bedrock, the reply confirms it, the audit row
// carries old → new, and the kiro SKU is untouched.
func TestCapSetApiNameSavesAndAudits(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:apiname"))
	b.Process(ctx, textUpdate(1, "  My Custom Product Name  "))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "My Custom Product Name") {
		t.Fatalf("valid reply = %+v, want the rename confirmation", last)
	}
	names := apikeyNamesFrom(t, b)
	if got := names["bedrock"]; got != "My Custom Product Name" {
		t.Errorf("apikey_names.bedrock = %v, want the trimmed name", got)
	}
	if _, present := names["kiro"]; present {
		t.Errorf("apikey_names.kiro = %v, want the other SKU left absent", names["kiro"])
	}
	if got := b.apikeyProductName(saleClassKiro); got != apikeyDefaultNameKiro {
		t.Errorf("kiro name = %q, want the untouched default", got)
	}
	row := latestAuditRow(t, st, "config_set_apikey_name")
	if row == nil {
		t.Fatal("no audit row written for the rename")
	}
	if row.Target != "apikey_names.bedrock" {
		t.Errorf("audit target = %q, want apikey_names.bedrock", row.Target)
	}
	if want := apikeyDefaultNameBedrock + " → My Custom Product Name"; row.Outcome != want {
		t.Errorf("audit outcome = %q, want %q", row.Outcome, want)
	}
}

// TestCapSetKiroNameSavesUnderItsOwnKey proves the shared body keys the kiro
// wizard off "kiro" (not "bedrock") — the one boundary the bedrock test above
// cannot reach.
func TestCapSetKiroNameSavesUnderItsOwnKey(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:kironame"))
	b.Process(ctx, textUpdate(1, "Purple Credits"))
	names := apikeyNamesFrom(t, b)
	if got := names["kiro"]; got != "Purple Credits" {
		t.Errorf("apikey_names.kiro = %v, want the typed name", got)
	}
	if _, present := names["bedrock"]; present {
		t.Errorf("apikey_names.bedrock = %v, want the other SKU left absent", names["bedrock"])
	}
	if row := latestAuditRow(t, st, "config_set_apikey_name"); row == nil || row.Target != "apikey_names.kiro" {
		t.Errorf("audit row = %+v, want target apikey_names.kiro", row)
	}
}

// TestCapSetApiNameDashRestoresDefault: "-" DELETES the override rather than
// storing a literal "-" — the only way back to the built-in name, since an
// empty message can't be sent to a bot.
func TestCapSetApiNameDashRestoresDefault(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:apiname"))
	b.Process(ctx, textUpdate(1, "Temporary Name"))
	if got := b.apikeyProductName(saleClassBedrock); got != "Temporary Name" {
		t.Fatalf("name = %q, want the override applied before the restore", got)
	}

	b.Process(ctx, callbackUpdate(1, "adm:apiname"))
	b.Process(ctx, textUpdate(1, "-"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "restored to the default") {
		t.Fatalf("restore reply = %+v, want the restore confirmation", last)
	}
	if _, present := apikeyNamesFrom(t, b)["bedrock"]; present {
		t.Error("apikey_names.bedrock must be DELETED by \"-\", not set to a literal dash")
	}
	// Restoring the LAST override drops the whole sub-map, so settings.json
	// ends up as it was before anyone renamed anything.
	if _, present := b.cfg.Settings()["apikey_names"]; present {
		t.Error("apikey_names must be removed entirely once its last override is restored")
	}
	if got := b.apikeyProductName(saleClassBedrock); got != apikeyDefaultNameBedrock {
		t.Errorf("name after restore = %q, want %q", got, apikeyDefaultNameBedrock)
	}
}

// TestCapSetApiNameRejectsBadInput: a newline-bearing or over-long name is
// refused and NOTHING is persisted — the value ends up inside inline-button
// labels and an HTML delivery message, so this is a trust boundary, not a
// cosmetic check.
func TestCapSetApiNameRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, input string }{
		{"newline", "Evil\nName"},
		{"carriage return", "Evil\rName"},
		{"too long", strings.Repeat("x", apikeyNameMaxRunes+1)},
		// Whitespace-only collapses to "" after TrimSpace, which apikeyNameOK
		// rejects (s != ""). Must not be confused with the "-" restore
		// sentinel, and must not silently persist a blank display name.
		{"whitespace only", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, st, _, _ := newBuyBot(t, "1")
			b.Process(ctx, callbackUpdate(1, "adm:apiname"))
			b.Process(ctx, textUpdate(1, tc.input))
			if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "⚠️") {
				t.Fatalf("reply = %+v, want the rejection notice", last)
			}
			if m := apikeyNamesFrom(t, b); len(m) != 0 {
				t.Errorf("apikey_names = %v, want nothing persisted on a rejected value", m)
			}
			if got := b.apikeyProductName(saleClassBedrock); got != apikeyDefaultNameBedrock {
				t.Errorf("name = %q, want the default still live", got)
			}
			if row := latestAuditRow(t, st, "config_set_apikey_name"); row != nil {
				t.Errorf("audit row written for a rejected value: %+v", *row)
			}
		})
	}
	// A name of exactly the cap is ACCEPTED — pins the boundary as <=, so a
	// future off-by-one that rejects a legal name goes red here.
	b, _, _, _, _ := newBuyBot(t, "1")
	atCap := strings.Repeat("x", apikeyNameMaxRunes)
	b.Process(ctx, callbackUpdate(1, "adm:apiname"))
	b.Process(ctx, textUpdate(1, atCap))
	if got := b.apikeyProductName(saleClassBedrock); got != atCap {
		t.Errorf("name at the %d-rune cap = %q, want it accepted", apikeyNameMaxRunes, got)
	}
}

// TestCapSetApiNameNoAuditOnSaveFailure is the H1 stop-on-failure pin for the
// rename capture: on a failed SaveSettings the reply is settingsSaveFailed
// (never the ✅ rename text), no audit row is written, and the capture still
// reports the input as CONSUMED.
func TestCapSetApiNameNoAuditOnSaveFailure(t *testing.T) {
	b, f, st := newReadOnlyCfgAuditBot(t)

	got := armAndCapture(t, b, 1, "adm:apiname", "Nebula Credits", b.capSetApiName)
	if !got {
		t.Errorf("capSetApiName returned %v, want true (input consumed even on save failure)", got)
	}
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on save failure = %+v, want %q", last, settingsSaveFailed)
	}
	if row := latestAuditRow(t, st, "config_set_apikey_name"); row != nil {
		t.Fatalf("audit row written despite failed SaveSettings: %+v", *row)
	}
}

// TestCapSetApiNameNilCfgEchoesTypedName pins the b.cfg == nil case, which
// TestCapSetApiNameSavesAndAudits above cannot reach — with a real config,
// the just-applied write and a fresh apikeyProductName read return the same
// string, so a handler that re-resolved the name instead of echoing what it
// just applied would still pass that test. Here b.cfg is nil, so the entire
// save block is skipped and apikeyProductName can only fall back to the
// built-in default: the ✅ reply must still show the TYPED name, proving the
// text comes from the local variable, not from re-reading storage.
func TestCapSetApiNameNilCfgEchoesTypedName(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil) // nil cfg (and nil store) — save block never runs

	b.Process(ctx, callbackUpdate(1, "adm:apiname"))
	b.Process(ctx, textUpdate(1, "Nebula Credits"))

	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "Nebula Credits") {
		t.Fatalf("reply = %+v, want the typed name even though nothing was saved", last)
	}
	if strings.Contains(last.Text, apikeyDefaultNameBedrock) {
		t.Errorf("reply = %q, want NOT the built-in default — a nil cfg must not make the ✅ text re-resolve through apikeyProductName", last.Text)
	}
}

// TestCapSetApikeyNamePeeksEscapes: the two rename captures must NOT swallow a
// /command or a reply-keyboard label typed while armed — same peek-escape the
// sibling pricing captures have, so an admin who changes their mind is not
// trapped in the wizard.
func TestCapSetApikeyNamePeeksEscapes(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "1")
	for _, tc := range []struct {
		name string
		arm  string
		cap  func(*Ctx) bool
	}{
		{"bedrock", "adm:apiname", b.capSetApiName},
		{"kiro", "adm:kironame", b.capSetKiroName},
		{"fixed", "adm:fixedname", b.capSetFixedName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := armAndCapture(t, b, 1, tc.arm, "/admin", tc.cap); got {
				t.Error("a /command typed while armed must fall through, not be eaten as a name")
			}
			if m := apikeyNamesFrom(t, b); len(m) != 0 {
				t.Errorf("apikey_names = %v, want nothing persisted by an escape", m)
			}
		})
	}
}

// TestCapSetApikeyNameRefusesNonSuperAdmin pins the CAPTURE's own isSuperAdmin
// check (adminpricing.go capSetApikeyName), not just the arming callback's
// settingsSuperAdminOnly gate — defense in depth for a case the callback gate
// alone cannot catch: an admin armed the wizard while still a super admin,
// then was demoted (RmAdmin / admins.json edit) before typing the name, and
// the armed flag survives the demotion in ChatState. The flag is set directly
// here (bypassing the arm callback) to simulate exactly that state, on a chat
// that is NOT super admin — input must be refused and nothing written.
func TestCapSetApikeyNameRefusesNonSuperAdmin(t *testing.T) {
	const chat = 7 // not registered as any kind of admin
	for _, tc := range []struct {
		name string
		arm  func(*ChatState)
		cap  func(*Bot) func(*Ctx) bool
	}{
		{"bedrock", func(st *ChatState) { st.SetApiName = true }, func(b *Bot) func(*Ctx) bool { return b.capSetApiName }},
		{"kiro", func(st *ChatState) { st.SetKiroName = true }, func(b *Bot) func(*Ctx) bool { return b.capSetKiroName }},
		{"fixed", func(st *ChatState) { st.SetFixedName = true }, func(b *Bot) func(*Ctx) bool { return b.capSetFixedName }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, _, _, _ := newBuyBot(t, "1") // chat 1 is the only super admin
			st := b.states.Get(chat)
			st.mu.Lock()
			tc.arm(st)
			st.mu.Unlock()

			c := &Ctx{Ctx: context.Background(), ChatID: chat, Lang: i18n.EN, Text: "Hijacked Name", st: st}
			if got := tc.cap(b)(c); !got {
				t.Errorf("capture returned %v, want true (input consumed, not fallen through)", got)
			}
			if last := lastSendMsg(f); last != nil {
				t.Errorf("reply sent to a refused non-super-admin capture: %+v, want none", last)
			}
			if m := apikeyNamesFrom(t, b); len(m) != 0 {
				t.Errorf("apikey_names = %v, want nothing persisted by a non-super-admin", m)
			}
		})
	}
}

// TestCapSetApikeyNameSecondArmDisarmsFirst is the regression test for the
// dual-arm bug: the pricing menu carries BOTH rename buttons in one message
// and old copies stay tappable, so an admin can arm bedrock, scroll up, and
// arm kiro without the first flag ever clearing. Before the fix, both
// SetApiName and SetKiroName ended up armed and the FIRST capture stage to
// run (setapiname, per CanonicalCaptureOrder) silently consumed the typed
// line — renaming bedrock, then leaving SetKiroName armed to steal the
// admin's next unrelated plain-text message. After the fix, arming kiro
// second disarms bedrock, so the one typed line reaches ONLY the kiro
// capture and the bedrock override stays unset.
func TestCapSetApikeyNameSecondArmDisarmsFirst(t *testing.T) {
	ctx := context.Background()
	b, _, _, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:apiname"))  // arm bedrock
	b.Process(ctx, callbackUpdate(1, "adm:kironame")) // arm kiro — must disarm bedrock
	b.Process(ctx, textUpdate(1, "Aurora Credits"))   // one typed line

	names := apikeyNamesFrom(t, b)
	if got := names["kiro"]; got != "Aurora Credits" {
		t.Errorf("apikey_names.kiro = %v, want the typed name (kiro was the live arm)", got)
	}
	if _, present := names["bedrock"]; present {
		t.Errorf("apikey_names.bedrock = %v, want unset — the first arm must have been disarmed by the second", names["bedrock"])
	}
	if got := b.apikeyProductName(saleClassBedrock); got != apikeyDefaultNameBedrock {
		t.Errorf("bedrock name = %q, want the untouched default %q", got, apikeyDefaultNameBedrock)
	}
}

// ---- 4c. Fixed Pricing flat prices (Go-only) -------------------------------
//
// adm:setmodelprice / adm:modelprices talk to the POOL over HTTP rather than to
// settings.json, so these tests run against the httptest Kiro-Go double from
// apikey_test.go and assert on the recorded request bodies. The money-critical
// pins are the ones proving a rejected price never leaves the process at all:
// perRequest is a POINTER precisely so an omitted price fails loudly once
// instead of pricing a model free on every request from then on.

// newModelPriceBot wires a Bot to a temp store/config plus the fake Kiro-Go,
// with chat 1 as the super admin (newAPIKeyBot's 999 would fail every
// super-admin gate these handlers run first).
func newModelPriceBot(t *testing.T) (*Bot, *fakeTG, *fakeKiro) {
	t.Helper()
	dir := t.TempDir()
	fk := newFakeKiro(t)
	fl := newFakeLlmgate(t)
	writeSettings(t, dir, apikeySettings(fk, fl))
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	return b, f, fk
}

// TestCapSetModelPriceMissingPriceRejectedNoCall is THE money-critical pin: a
// model name with no price must be refused outright, and — the half a
// reply-text assertion alone cannot prove — must never reach the pool. Sending
// a bare model would leave perRequest nil, which the client rejects, but a
// future edit that "helpfully" defaulted it to 0 would price that model FREE on
// every request afterwards instead of failing once, here.
func TestCapSetModelPriceMissingPriceRejectedNoCall(t *testing.T) {
	ctx := context.Background()
	b, f, fk := newModelPriceBot(t)

	b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
	b.Process(ctx, textUpdate(1, "claude-sonnet-4"))

	last := lastSendMsg(f)
	if last == nil || !strings.HasPrefix(last.Text, "⚠️") || !strings.Contains(last.Text, "REQUIRED") {
		t.Fatalf("reply = %+v, want the explicit price-is-required refusal", last)
	}
	if n := len(fk.modelPriceSets()); n != 0 {
		t.Errorf("set_model_price called %d time(s) for a missing price, want 0 — a rejected value must never reach the pool", n)
	}
}

// TestCapSetModelPriceZeroIsAccepted pins the OTHER half of the pointer
// contract: an explicit 0 is a legitimate price (that model becomes free) and
// must pass through as a non-nil *float64. The proof is that the HTTP call
// happened at all — a nil pointer would have been rejected client-side by
// kirogo.ErrPerRequestRequired with no request sent — plus perRequest being
// PRESENT in the body with value 0.
func TestCapSetModelPriceZeroIsAccepted(t *testing.T) {
	ctx := context.Background()
	b, f, fk := newModelPriceBot(t)

	b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
	b.Process(ctx, textUpdate(1, "claude-haiku-4 0"))

	sets := fk.modelPriceSets()
	if len(sets) != 1 {
		t.Fatalf("set_model_price called %d time(s), want exactly 1 — an explicit 0 is a legal price", len(sets))
	}
	got, present := sets[0]["perRequest"]
	if !present {
		t.Fatal("perRequest absent from the body — an explicit 0 must be sent, not omitted")
	}
	if got != float64(0) {
		t.Errorf("perRequest = %v, want 0", got)
	}
	if sets[0]["model"] != "claude-haiku-4" {
		t.Errorf("model = %v, want claude-haiku-4", sets[0]["model"])
	}
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅") {
		t.Fatalf("reply = %+v, want the success confirmation", last)
	}
}

// TestCapSetModelPriceRejectsBadInputBeforeCall: every malformed price is
// refused BEFORE the call, never after (py_pricing_spec.md §1 — a rejected
// value must never be persisted).
//
// Each case asserts the GUARD-SPECIFIC message, not merely a ⚠️ prefix plus
// zero HTTP calls. That distinction is the whole test: two backstops downstream
// (kirogo's ErrPerRequestNegative, and json.Marshal refusing a non-finite
// float) also reject these inputs before any request goes out and also produce
// a ⚠️ reply with zero recorded calls — so a prefix-only assertion passes
// identically with the handler's own pre-call guard DELETED, which a mutation
// run confirmed. Matching the guard's own wording is what makes these cases
// falsify a regression that removes it.
func TestCapSetModelPriceRejectsBadInputBeforeCall(t *testing.T) {
	ctx := context.Background()
	const (
		wantFinite   = "must be a finite number of credits" // the parse/finite/non-negative guard
		wantRequired = "A price is REQUIRED"                // the exactly-two-tokens guard
		wantModelCap = "model name must be at most"         // the model-token length guard
	)
	for _, tc := range []struct{ name, input, want string }{
		{"negative", "gpt-5 -1", wantFinite},
		{"negative fraction", "gpt-5 -0.5", wantFinite},
		{"unparseable", "gpt-5 cheap", wantFinite},
		{"NaN", "gpt-5 NaN", wantFinite},
		{"positive infinity", "gpt-5 Inf", wantFinite},
		{"negative infinity", "gpt-5 -Inf", wantFinite},
		// Three tokens is not "<model> <perRequest>": which of the two trailing
		// tokens is the price is a guess, and guessing about a price is exactly
		// what this wizard must never do.
		{"extra token", "gpt-5 1.5 please", wantRequired},
		{"empty line", "   ", wantRequired},
		// An unbounded model name would blow Telegram's 4096-char limit in the
		// confirmation and in every later listing render, and b.send does not
		// chunk — so the reply would silently not arrive.
		{"model name too long", strings.Repeat("m", modelNameMaxRunes+1) + " 1", wantModelCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, fk := newModelPriceBot(t)
			b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
			b.Process(ctx, textUpdate(1, tc.input))

			last := lastSendMsg(f)
			if last == nil || !strings.HasPrefix(last.Text, "⚠️") {
				t.Fatalf("reply = %+v, want the rejection notice", last)
			}
			if !strings.Contains(last.Text, tc.want) {
				t.Errorf("reply = %q, want the handler's own guard message containing %q — a downstream rejection is NOT the same as a pre-call one", last.Text, tc.want)
			}
			if n := len(fk.modelPriceSets()); n != 0 {
				t.Errorf("set_model_price called %d time(s) for %q, want 0 — validation must run BEFORE the call", n, tc.input)
			}
		})
	}
	// A model name of exactly the cap is ACCEPTED — pins the bound as <=, so a
	// future off-by-one that rejects a legal name goes red here.
	b, _, fk := newModelPriceBot(t)
	atCap := strings.Repeat("m", modelNameMaxRunes)
	b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
	b.Process(ctx, textUpdate(1, atCap+" 1"))
	if sets := fk.modelPriceSets(); len(sets) != 1 || sets[0]["model"] != atCap {
		t.Errorf("bodies = %v, want a model name at the %d-rune cap accepted", sets, modelNameMaxRunes)
	}
}

// TestCapSetModelPriceEchoesPoolValue proves the confirmation reports what the
// POOL stored, not what the admin typed — the two differ whenever the pool
// clamps or rounds, and showing the typed number would tell the operator a
// price is live that never was.
func TestCapSetModelPriceEchoesPoolValue(t *testing.T) {
	ctx := context.Background()
	b, f, fk := newModelPriceBot(t)
	stored := 2.5
	fk.ModelPriceEcho = &stored

	b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
	b.Process(ctx, textUpdate(1, "claude-opus-5 1.5"))

	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "2.5 credits/request") {
		t.Fatalf("reply = %+v, want the pool's echoed 2.5, not the typed 1.5", last)
	}
	if strings.Contains(last.Text, "1.5") {
		t.Errorf("reply = %q, want the typed value NOT reported as stored", last.Text)
	}
	if sets := fk.modelPriceSets(); len(sets) != 1 || sets[0]["perRequest"] != 1.5 {
		t.Errorf("body = %v, want the typed 1.5 actually sent", sets)
	}
}

// TestCapSetModelPriceServerErrorReported: a non-2xx from the pool must say
// nothing changed rather than showing a ✅ the write never earned — the same
// H1 stop-on-failure rule the settings.json setters follow.
func TestCapSetModelPriceServerErrorReported(t *testing.T) {
	ctx := context.Background()
	b, f, fk := newModelPriceBot(t)
	fk.SetModelPriceStatus = 400

	b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
	b.Process(ctx, textUpdate(1, "gpt-5 1"))

	last := lastSendMsg(f)
	if last == nil || !strings.HasPrefix(last.Text, "⚠️") || !strings.Contains(last.Text, "nothing was changed") {
		t.Fatalf("reply = %+v, want the failure notice", last)
	}
}

// TestCapSetModelPriceUnconfigured: with no Kiro-Go server there is nothing to
// call, so the wizard says so and changes nothing (b.cfg is nil here, which is
// a strictly harsher case than a configured-but-blank URL).
func TestCapSetModelPriceUnconfigured(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil) // nil cfg -> kirogoClient() is nil

	b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
	b.Process(ctx, textUpdate(1, "gpt-5 1"))

	if last := lastSendMsg(f); last == nil || last.Text != kirogoNotConfiguredNote {
		t.Fatalf("reply = %+v, want %q", last, kirogoNotConfiguredNote)
	}
}

// TestCapSetModelPricePeeksEscapes: a /command typed while armed must fall
// through to the command router, not be eaten as a model name — same escape
// the sibling pricing captures give an admin who changes their mind.
func TestCapSetModelPricePeeksEscapes(t *testing.T) {
	b, _, fk := newModelPriceBot(t)
	if got := armAndCapture(t, b, 1, "adm:setmodelprice", "/admin", b.capSetModelPrice); got {
		t.Error("a /command typed while armed must fall through, not be consumed")
	}
	if n := len(fk.modelPriceSets()); n != 0 {
		t.Errorf("set_model_price called %d time(s) by an escape, want 0", n)
	}
}

// TestCapSetModelPriceRefusesNonSuperAdmin pins the CAPTURE's own isSuperAdmin
// recheck, not just the arming callback's gate — the case the callback gate
// cannot catch is an admin who armed the wizard while still super, then was
// demoted before typing, since the armed flag survives the demotion in
// ChatState. The flag is set directly here to simulate exactly that.
func TestCapSetModelPriceRefusesNonSuperAdmin(t *testing.T) {
	const chat = 7 // not registered as any kind of admin
	b, f, fk := newModelPriceBot(t)
	st := b.states.Get(chat)
	st.mu.Lock()
	st.SetModelPrice = true
	st.mu.Unlock()

	c := &Ctx{Ctx: context.Background(), ChatID: chat, Lang: i18n.EN, Text: "gpt-5 1", st: st}
	if got := b.capSetModelPrice(c); !got {
		t.Errorf("capture returned %v, want true (input consumed, not fallen through)", got)
	}
	if last := lastSendMsg(f); last != nil {
		t.Errorf("reply sent to a refused non-super-admin capture: %+v, want none", last)
	}
	if n := len(fk.modelPriceSets()); n != 0 {
		t.Errorf("set_model_price called %d time(s) by a non-super-admin, want 0", n)
	}
}

// TestPricingFreeTextWizardsAreMutuallyExclusive is the regression test for the
// stale-arm bug armPricingFreeText exists to prevent: the pricing menu carries
// all four free-text buttons in ONE message and old copies stay tappable, so
// an admin can arm one and then another without the first flag clearing. A
// stale armed flag would silently eat the admin's next unrelated line —
// repricing a model, or publishing it as a SKU's customer-visible name. Both
// directions are pinned, because a helper that only cleared one way would still
// pass a single-direction test.
func TestPricingFreeTextWizardsAreMutuallyExclusive(t *testing.T) {
	ctx := context.Background()

	t.Run("rename arm disarms the price wizard", func(t *testing.T) {
		b, _, fk := newModelPriceBot(t)
		b.Process(ctx, callbackUpdate(1, "adm:setmodelprice"))
		b.Process(ctx, callbackUpdate(1, "adm:apiname")) // must disarm setmodelprice
		b.Process(ctx, textUpdate(1, "Nebula Credits"))

		if got := b.apikeyProductName(saleClassBedrock); got != "Nebula Credits" {
			t.Errorf("bedrock name = %q, want the typed line consumed by the LIVE arm", got)
		}
		// A SECOND, unrelated line is what actually exposes a stale flag in THIS
		// direction, and the first line cannot: "setapiname" sits ahead of
		// "setmodelprice" in the capture chain, so capSetApiName consumes the
		// rename and first-match dispatch stops before capSetModelPrice is ever
		// reached — the two assertions above hold with or without the disarm. By
		// now no rename is armed, so this line falls through to whatever else
		// still is. (Mutation-verified: a one-way armPricingFreeText that only
		// ever SETS its own flag reprices model "hello" here, and passes the
		// subtest without this second line.)
		b.Process(ctx, textUpdate(1, "hello 5"))
		if n := len(fk.modelPriceSets()); n != 0 {
			t.Errorf("set_model_price called %d time(s) by a later unrelated line, want 0 — arming the rename must have disarmed the price wizard", n)
		}
	})

	t.Run("price arm disarms the rename wizards", func(t *testing.T) {
		b, _, fk := newModelPriceBot(t)
		b.Process(ctx, callbackUpdate(1, "adm:apiname"))
		b.Process(ctx, callbackUpdate(1, "adm:kironame"))
		b.Process(ctx, callbackUpdate(1, "adm:fixedname"))
		b.Process(ctx, callbackUpdate(1, "adm:setmodelprice")) // must disarm all three renames
		b.Process(ctx, textUpdate(1, "gpt-5 3"))

		if sets := fk.modelPriceSets(); len(sets) != 1 || sets[0]["model"] != "gpt-5" {
			t.Fatalf("set_model_price bodies = %v, want exactly the one live arm's", sets)
		}
		if m := apikeyNamesFrom(t, b); len(m) != 0 {
			t.Errorf("apikey_names = %v, want nothing renamed — every rename arm must have been cleared", m)
		}
	})
}

// TestCbModelPricesRendersRows: the listing renders one "model — N
// credits/request" row per priced model.
func TestCbModelPricesRendersRows(t *testing.T) {
	ctx := context.Background()
	b, f, fk := newModelPriceBot(t)
	fk.ModelPriceList = []map[string]any{
		{"model": "claude-opus-5", "perRequest": 4.0},
		{"model": "claude-haiku-4", "perRequest": 0.25},
	}

	b.Process(ctx, callbackUpdate(1, "adm:modelprices"))

	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("no reply")
	}
	for _, want := range []string{
		"claude-opus-5 — 4 credits/request",
		"claude-haiku-4 — 0.25 credits/request",
	} {
		if !strings.Contains(last.Text, want) {
			t.Errorf("listing = %q, want a row %q", last.Text, want)
		}
	}
}

// TestCbModelPricesEmptyList: no priced models is a NORMAL state (nobody has
// set one yet), so it gets a plain explanatory message, not an error.
func TestCbModelPricesEmptyList(t *testing.T) {
	ctx := context.Background()
	b, f, _ := newModelPriceBot(t)

	b.Process(ctx, callbackUpdate(1, "adm:modelprices"))

	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "No models carry a flat price yet") {
		t.Fatalf("reply = %+v, want the empty-list explanation", last)
	}
	if strings.HasPrefix(last.Text, "⚠️") {
		t.Errorf("reply = %q, want an empty list treated as normal, not as an error", last.Text)
	}
}

// TestCbModelPricesServerError / unconfigured: both must produce a reply (a
// silent handler is a dead button) and neither may claim a price list it does
// not have.
func TestCbModelPricesFailurePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("server error", func(t *testing.T) {
		b, f, fk := newModelPriceBot(t)
		// 4xx, not 5xx: internal/kirogo retries a 5xx, which would make this
		// test sleep through the backoff for no extra coverage (the same reason
		// fakeKiro's status knobs are documented as 4xx-only).
		fk.ModelPricesStatus = 400
		b.Process(ctx, callbackUpdate(1, "adm:modelprices"))
		if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "⚠️") {
			t.Fatalf("reply = %+v, want the read-failure notice", last)
		}
	})

	t.Run("kiro-go unconfigured", func(t *testing.T) {
		b, f := newTestBot("1", nil) // nil cfg -> kirogoClient() is nil
		b.Process(ctx, callbackUpdate(1, "adm:modelprices"))
		if last := lastSendMsg(f); last == nil || last.Text != kirogoNotConfiguredNote {
			t.Fatalf("reply = %+v, want %q", last, kirogoNotConfiguredNote)
		}
	})
}

// ---- §6: the Fixed Pricing SKU's five admin wizards -----------------------

// TestFixedPricingWizardsPersistValidValues walks the four value-setting
// wizards end to end through the real dispatch chain: arm the callback, type a
// value, assert the LIVE accessor moved. Reading through the accessors (not the
// raw settings map) is deliberate — it proves the wizard writes the key the
// read path actually consults, which a raw-map assertion would not.
func TestFixedPricingWizardsPersistValidValues(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1") // chat 1 is the super admin

	b.Process(ctx, callbackUpdate(1, "adm:fixedprice"))
	b.Process(ctx, textUpdate(1, "1.5"))
	if got := b.apikeyRateUSDT(saleClassFixed); got != 1.5 {
		t.Errorf("fixed rate = %v, want 1.5", got)
	}
	// The bedrock rate must not have moved — the SKUs own separate keys.
	if got := b.apikeyRateUSDT(saleClassBedrock); got != 0 {
		t.Errorf("bedrock rate = %v, want 0 (untouched)", got)
	}

	b.Process(ctx, callbackUpdate(1, "adm:fixedmax"))
	b.Process(ctx, textUpdate(1, "5000"))
	if got := b.apikeyMaxCredits(saleClassFixed); got != 5000 {
		t.Errorf("fixed max = %d, want 5000", got)
	}
	b.Process(ctx, callbackUpdate(1, "adm:fixedmin"))
	b.Process(ctx, textUpdate(1, "250"))
	if got := b.apikeyMinCredits(saleClassFixed); got != 250 {
		t.Errorf("fixed min = %d, want 250", got)
	}

	b.Process(ctx, callbackUpdate(1, "adm:fixedname"))
	b.Process(ctx, textUpdate(1, "  Flat Rate Credits  "))
	if got := b.apikeyProductName(saleClassFixed); got != "Flat Rate Credits" {
		t.Errorf("fixed name = %q, want the trimmed typed name", got)
	}
	// A lone "-" restores the built-in default.
	b.Process(ctx, callbackUpdate(1, "adm:fixedname"))
	b.Process(ctx, textUpdate(1, "-"))
	if got := b.apikeyProductName(saleClassFixed); got != apikeyDefaultNameFixed {
		t.Errorf("fixed name after restore = %q, want %q", got, apikeyDefaultNameFixed)
	}
	if last := lastSendMsg(f); last == nil {
		t.Fatal("the wizards must always answer")
	}
}

// TestFixedPricingWizardsRejectWithoutPersisting: a rejected value must never
// be written. Asserting the LIVE value is unchanged (not merely that the reply
// says "⚠️") is the point — a handler that saved first and validated after
// would pass a message-only assertion.
func TestFixedPricingWizardsRejectWithoutPersisting(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1")

	// Baseline: a valid range to reject against.
	b.Process(ctx, callbackUpdate(1, "adm:fixedprice"))
	b.Process(ctx, textUpdate(1, "1.5"))
	b.Process(ctx, callbackUpdate(1, "adm:fixedmax"))
	b.Process(ctx, textUpdate(1, "5000"))
	b.Process(ctx, callbackUpdate(1, "adm:fixedmin"))
	b.Process(ctx, textUpdate(1, "250"))

	for _, tc := range []struct {
		name, arm, text, wantMsg string
		read                     func() string
	}{
		// NaN must not slip past the range check — it compares false against
		// every bound, so a bare `rate < 0 || rate > 10000` would accept it.
		{"price NaN", "adm:fixedprice", "NaN", "USDT per 100 credits",
			func() string { return fmtRate(b.apikeyRateUSDT(saleClassFixed)) }},
		{"price negative", "adm:fixedprice", "-1", "USDT per 100 credits",
			func() string { return fmtRate(b.apikeyRateUSDT(saleClassFixed)) }},
		{"price out of range", "adm:fixedprice", "10001", "USDT per 100 credits",
			func() string { return fmtRate(b.apikeyRateUSDT(saleClassFixed)) }},
		{"price not a number", "adm:fixedprice", "cheap", "USDT per 100 credits",
			func() string { return fmtRate(b.apikeyRateUSDT(saleClassFixed)) }},
		{"min zero", "adm:fixedmin", "0", "whole number ≥ 1",
			func() string { return itoa64(b.apikeyMinCredits(saleClassFixed)) }},
		{"min above max", "adm:fixedmin", "5001", "above the current maximum",
			func() string { return itoa64(b.apikeyMinCredits(saleClassFixed)) }},
		{"max below min", "adm:fixedmax", "10", "the per-order minimum",
			func() string { return itoa64(b.apikeyMaxCredits(saleClassFixed)) }},
		{"name multi-line", "adm:fixedname", "Evil\nName", "single-line name",
			func() string { return b.apikeyProductName(saleClassFixed) }},
		{"name over-long", "adm:fixedname", strings.Repeat("x", apikeyNameMaxRunes+1), "single-line name",
			func() string { return b.apikeyProductName(saleClassFixed) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.read()
			f.Sends = nil
			b.Process(ctx, callbackUpdate(1, tc.arm))
			b.Process(ctx, textUpdate(1, tc.text))
			last := lastSendMsg(f)
			if last == nil || !strings.Contains(last.Text, tc.wantMsg) {
				t.Fatalf("reply = %+v, want the rejection mentioning %q", last, tc.wantMsg)
			}
			if got := tc.read(); got != before {
				t.Errorf("a rejected value was persisted: now %q, want unchanged %q", got, before)
			}
		})
	}
}

// TestFixedToggleFlipsAndWarns: the master toggle flips both ways, audits the
// change, and warns (advisory only, never blocking) while the SKU still cannot
// actually sell.
func TestFixedToggleFlipsAndWarns(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "1")

	if b.fixedApikeyEnabled() {
		t.Fatal("the Fixed SKU must default OFF")
	}
	b.Process(ctx, callbackUpdate(1, "adm:fixedtoggle"))
	if !b.fixedApikeyEnabled() {
		t.Fatal("the toggle must flip ON")
	}
	// Nothing else is configured here, so both advisories must fire — the
	// toggle is a rollout gate, not a validator, so it flips regardless.
	var warned string
	for _, m := range f.Sends {
		if strings.Contains(m.Text, "Fixed Pricing SKU: ON") {
			warned = m.Text
		}
	}
	if warned == "" {
		t.Fatal("no ON confirmation was sent")
	}
	for _, want := range []string{"URL/admin key not set", "Fixed pricing is 0"} {
		if !strings.Contains(warned, want) {
			t.Errorf("ON notice = %q, want the advisory %q", warned, want)
		}
	}
	// The advisory must NOT name the account id: a fixed mint sends none, so
	// pointing the admin at it would send them to configure the wrong thing.
	if strings.Contains(warned, "account id") || strings.Contains(warned, "Account") {
		t.Errorf("ON notice = %q, must not mention an account id for the fixed SKU", warned)
	}
	b.Process(ctx, callbackUpdate(1, "adm:fixedtoggle"))
	if b.fixedApikeyEnabled() {
		t.Fatal("the toggle must flip back OFF")
	}
}

// TestPricingMenuCarriesFixedRows: every fixed wizard must be reachable from
// the pricing submenu. A handler wired in the adm: switch but missing from the
// menu is a dead feature no other test would catch.
func TestPricingMenuCarriesFixedRows(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "1")
	_, rows, ok := b.adminCategoryMenu(1, "pricing")
	if !ok {
		t.Fatal("no pricing menu")
	}
	var data []string
	for _, row := range rows {
		for _, btn := range row {
			data = append(data, btn.CallbackData)
		}
	}
	for _, want := range []string{
		"adm:fixedtoggle", "adm:fixedprice", "adm:fixedmin", "adm:fixedmax", "adm:fixedname",
	} {
		if !containsStr(data, want) {
			t.Errorf("pricing menu is missing %q, buttons=%v", want, data)
		}
	}
}

// TestNumericPricingWizardsDisarmFreeText is the regression test for the
// asymmetric-arming hole: only the free-text wizards routed through
// armPricingFreeText, so arming a NUMERIC one left any standing rename /
// model-price arm untouched.
//
// That is not benign. All of these buttons live in one pricing-menu message and
// old copies stay tappable, so an admin can tap "⚡ Rename" (arming SetFixedName),
// scroll up, tap "Min credits" (arming SetFixedMin), and type a number. The
// capture chain is ORDERED (captures.go CanonicalCaptureOrder puts setfixedmin
// ahead of setfixedname) and the first stage that consumes the line ENDS the
// chain — so the number is eaten by capSetFixedMin and SetFixedName is still
// armed, ready to publish the admin's next unrelated plain-text line as a
// customer-visible SKU name on the buy button, pay screen and delivery message.
//
// PRE-EXISTING and symmetric across all ten numeric wizards, which is why the
// disarm lives in armPricingFreeText and every one of them is asserted here:
// dropping the armPricingFreeText call from ANY single row fails this test.
//
// (Go-only; no Python original — Python has no rename or model-price wizard.)
func TestNumericPricingWizardsDisarmFreeText(t *testing.T) {
	for _, tc := range []struct {
		data  string                // the adm: callback that arms it
		armed func(*ChatState) bool // its own numeric flag, which must end up set
	}{
		{"adm:apiprice", func(st *ChatState) bool { return st.SetApiPrice }},
		{"adm:apimin", func(st *ChatState) bool { return st.SetApiMin }},
		{"adm:apimax", func(st *ChatState) bool { return st.SetApiMax }},
		{"adm:apicap", func(st *ChatState) bool { return st.SetApiCap }},
		{"adm:kiroprice", func(st *ChatState) bool { return st.SetKiroPrice }},
		{"adm:kiromin", func(st *ChatState) bool { return st.SetKiroMin }},
		{"adm:kiromax", func(st *ChatState) bool { return st.SetKiroMax }},
		{"adm:fixedprice", func(st *ChatState) bool { return st.SetFixedPrice }},
		{"adm:fixedmin", func(st *ChatState) bool { return st.SetFixedMin }},
		{"adm:fixedmax", func(st *ChatState) bool { return st.SetFixedMax }},
	} {
		t.Run(tc.data, func(t *testing.T) {
			b, _, _, _, _ := newBuyBot(t, "1") // chat 1 is the only super admin
			st := b.states.Get(1)
			// Every free-text wizard armed at once — the worst case an admin can
			// reach by tapping stale copies of the pricing menu.
			st.mu.Lock()
			st.SetApiName, st.SetKiroName, st.SetFixedName, st.SetModelPrice = true, true, true, true
			st.mu.Unlock()

			b.Process(context.Background(), callbackUpdate(1, tc.data))

			st.mu.Lock()
			defer st.mu.Unlock()
			for _, ft := range []struct {
				name  string
				armed bool
			}{
				{"SetApiName", st.SetApiName},
				{"SetKiroName", st.SetKiroName},
				{"SetFixedName", st.SetFixedName},
				{"SetModelPrice", st.SetModelPrice},
			} {
				if ft.armed {
					t.Errorf("%s still armed after %s — it will steal the admin's next "+
						"plain-text line once this numeric capture ends the chain", ft.name, tc.data)
				}
			}
			if !tc.armed(st) {
				t.Errorf("%s did not arm its own wizard flag", tc.data)
			}
		})
	}
}
