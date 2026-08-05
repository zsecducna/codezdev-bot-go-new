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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withDiscountsFile points the package-level discountsFilePath at a fresh
// temp file for the duration of the test, restoring it afterwards — the
// var exists so codedTotalFull/discountFor's existing "no bot" unit tests
// (code_test.go) keep working untouched, but that means every test that
// exercises discounts.json must isolate its own path or risk polluting the
// real repo cwd / racing other tests.
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

func TestAdminUsdtToVnd(t *testing.T) {
	b, _ := newTestBot("1", nil) // b.cfg == nil -> compiled default rate 25000
	cases := []struct {
		text    string
		wantVnd int64
		wantOK  bool
	}{
		{"5", 125000, true},
		{"5.5", 137500, true},
		{"1,234.5", 30862500, true}, // comma stripped as a thousands separator
		{" 5 ", 125000, true},       // trimmed
		{"-3", -75000, true},        // sign accepted here; callers reject <=0 themselves
		{"abc", 0, false},
		{"", 0, false},
		{"5,,5", 0, false}, // stripping commas leaves "55" -- NOT this case; see below
	}
	for _, tc := range cases {
		vnd, ok := b.adminUsdtToVnd(tc.text)
		if tc.text == "5,,5" {
			// "5,,5" comma-stripped is "55", a VALID digit string -> 55*25000.
			if !ok || vnd != 1375000 {
				t.Errorf("adminUsdtToVnd(%q) = (%d,%v), want (1375000,true)", tc.text, vnd, ok)
			}
			continue
		}
		if ok != tc.wantOK || (ok && vnd != tc.wantVnd) {
			t.Errorf("adminUsdtToVnd(%q) = (%d,%v), want (%d,%v)", tc.text, vnd, ok, tc.wantVnd, tc.wantOK)
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
	if got, ok := stk.ProductPrice("prod1", "BH30D"); !ok || got != 225000 { // 9 USDT * 25000
		t.Fatalf("ProductPrice(BH30D) = (%d,%v), want (225000,true)", got, ok)
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
	if got, ok := stk.ProductPrice("prod1", "BH30D"); !ok || got != 125000 {
		t.Fatalf("ProductPrice(BH30D) after edit = (%d,%v), want (125000,true)", got, ok)
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
