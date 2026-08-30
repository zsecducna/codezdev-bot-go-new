package handlers

// apikey_fixed_test.go — the third mint SKU, "fixed" sale class (product
// sentinel __fixedapi__).
//
// Two of these tests are REGRESSION GUARDS for bugs that made the SKU
// unsellable the first time it was wired up, and they are written so that
// restoring the old shape fails them:
//
//   - the mint path gated the accountId payload on `saleClass != saleClassKiro`,
//     so every FIXED mint sent an accountId, the kirogo client refused it with
//     ErrAccountIDForbiddenForFixed before the request left the process, and the
//     buyer was auto-refunded (TestFixedMintSendsNoAccountID);
//   - kirogo.UpstreamStatusAnomalous treated only kiro as skip-expected, so the
//     "skipped" every fixed mint returns read as an anomaly and DM'd the owners
//     an "API-KEY SOLD WITHOUT UPSTREAM BINDING" alert on every normal sale
//     (TestFixedMintDoesNotAlertOwners).

import (
	"context"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/store"
)

// fixedSettings is apikeySettings plus a live, enabled Fixed SKU priced at
// 1.5 USDT / 100 credits.
func fixedSettings(fk *fakeKiro) map[string]any {
	s := apikeySettings(fk)
	s["fixed_apikey_enabled"] = true
	s["fixed_apikey_usdt_per_100"] = 1.5
	return s
}

// newFixedBot is newAPIKeyBot with the Fixed SKU turned on and the server
// echoing the sale class back, the way the real Kiro-Go does (the echoed class
// is what gets persisted on the api_keys row).
func newFixedBot(t *testing.T) (*Bot, *fakeTG, *store.Store, *fakeKiro, string) {
	t.Helper()
	b, f, st, fk, dir := newAPIKeyBot(t)
	fk.EchoSaleClass = true
	fk.UpstreamStatus = "skipped" // the NORMAL terminal status for a fixed mint
	writeSettings(t, dir, fixedSettings(fk))
	return b, f, st, fk, dir
}

// ---- BUG 1: the mint payload ------------------------------------------------

// TestFixedMintSendsNoAccountID: a fixed mint must carry saleClass "fixed",
// NO accountId at all, and must complete the sale.
//
// This is the guard for the `!= saleClassKiro` shape: with that branch back,
// mintAPIKey sets req.AccountID, kirogo returns ErrAccountIDForbiddenForFixed
// without making a request, and deliverApikey auto-refunds — so the delivery
// assertion, the provision count and the refund check all fail, not just the
// payload one.
func TestFixedMintSendsNoAccountID(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newFixedBot(t)
	const chat = 100
	const des = "f1f10000f1f10000f1f10000f1f10000"
	o := seedOrder(t, st, des, chat, fixedApikeyProduct, 1000, 1500000, "paid")

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("a fixed mint must deliver (an accountId on the payload fails it client-side)")
	}
	if n := fk.provisionCount(); n != 1 {
		t.Fatalf("provision called %d times, want exactly 1", n)
	}
	p := fk.lastProvision()
	if _, present := p["accountId"]; present {
		t.Errorf("fixed mint must send NO accountId, got %v", p["accountId"])
	}
	if p["saleClass"] != "fixed" {
		t.Errorf("saleClass = %v, want fixed", p["saleClass"])
	}
	if p["skipUpstream"] != true {
		t.Errorf("skipUpstream = %v, want true for fixed", p["skipUpstream"])
	}
	if got, _ := st.GetOrder(des); got.Status != "delivered" {
		t.Errorf("order = %q, want delivered", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("balance = %d, want 0 — a working fixed sale must not auto-refund", bal)
	}
	// The SERVER'S echoed class is what arms the recharge guard later.
	row, _ := st.APIKeyForOrder(des)
	if row == nil || row.SaleClass != saleClassFixed {
		t.Fatalf("stored sale_class = %+v, want fixed", row)
	}
}

// TestFixedMintDeliversFixedTemplate: the delivery message must be the fixed
// SKU's own text (flat pricing wording + its display name), not the bedrock or
// kiro one, and must leak no unsubstituted placeholder.
func TestFixedMintDeliversFixedTemplate(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newFixedBot(t)
	const chat = 100
	const des = "f2f20000f2f20000f2f20000f2f20000"
	o := seedOrder(t, st, des, chat, fixedApikeyProduct, 1000, 1500000, "paid")
	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("fixed mint must deliver")
	}
	if countContains(f, apikeyDefaultNameFixed) == 0 {
		t.Errorf("delivery must name the fixed SKU %q", apikeyDefaultNameFixed)
	}
	if countContains(f, "{name}") != 0 || countContains(f, "{credits}") != 0 {
		t.Error("an unsubstituted placeholder leaked into the delivery message")
	}
	// The 👁 re-reveal must reuse the SAME template (apikeyDeliveredTemplate).
	before := countContains(f, apikeyDefaultNameFixed)
	b.Process(ctx, callbackUpdate(chat, "apikeyrev:"+des))
	if countContains(f, apikeyDefaultNameFixed) != before+1 {
		t.Error("the re-reveal must re-send the fixed delivery message")
	}
}

// ---- BUG 2: the owner alert -------------------------------------------------

// TestFixedMintDoesNotAlertOwners: "skipped" is the NORMAL terminal upstream
// status for a fixed mint, so a normal sale must page nobody. The bedrock case
// is asserted alongside it so the alert is proven still wired — a guard that
// silenced it for everyone would otherwise pass.
func TestFixedMintDoesNotAlertOwners(t *testing.T) {
	ctx := context.Background()
	const alert = "SOLD WITHOUT UPSTREAM BINDING"
	for _, tc := range []struct {
		name      string
		product   string
		wantAlert int
	}{
		{"fixed/skipped", fixedApikeyProduct, 0},
		{"kiro/skipped", kiroApikeyProduct, 0},
		// A BEDROCK mint that skips binding really IS anomalous: it should
		// bind or defer-bind, so the owners are told.
		{"bedrock/skipped", apikeyProduct, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, st, fk, dir := newFixedBot(t)
			s := fixedSettings(fk)
			s["kiro_apikey_enabled"] = true
			s["kiro_apikey_usdt_per_100"] = 1.0
			writeSettings(t, dir, s)
			const chat = 100
			const des = "f3f30000f3f30000f3f30000f3f30000"
			o := seedOrder(t, st, des, chat, tc.product, 1000, 1500000, "paid")

			if !b.deliverApikey(ctx, o, des) {
				t.Fatal("mint must deliver")
			}
			if got := countContains(f, alert); got != tc.wantAlert {
				t.Errorf("owner alerts = %d, want %d", got, tc.wantAlert)
			}
		})
	}
}

// ---- sale gate --------------------------------------------------------------

// TestFixedForSaleGate walks the three inputs of apikeyForSale(fixed): the
// master toggle, a positive rate, and the BASE Kiro-Go config.
//
// The "no account id" row is the load-bearing one: a fixed mint sends no
// accountId at all, so gating on kiroGoConfigured() (which requires one) would
// hide a perfectly sellable product. Bedrock is asserted hidden in that same
// row to prove the two gates really are different checks.
func TestFixedForSaleGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(map[string]any)
		wantFixed bool
		wantBed   bool
	}{
		{"fully configured", func(map[string]any) {}, true, true},
		{"toggle off", func(s map[string]any) { s["fixed_apikey_enabled"] = false }, false, true},
		{"toggle absent", func(s map[string]any) { delete(s, "fixed_apikey_enabled") }, false, true},
		{"rate 0", func(s map[string]any) { s["fixed_apikey_usdt_per_100"] = 0.0 }, false, true},
		{"rate absent", func(s map[string]any) { delete(s, "fixed_apikey_usdt_per_100") }, false, true},
		{"no base url", func(s map[string]any) { delete(s, "kiro_go_base_url") }, false, false},
		{"no admin key", func(s map[string]any) { delete(s, "kiro_go_admin_key") }, false, false},
		// Fixed needs NO account id; bedrock does.
		{"no account id", func(s map[string]any) { delete(s, "kiro_go_account_id") }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _, fk, dir := newAPIKeyBot(t)
			s := fixedSettings(fk)
			tc.mutate(s)
			writeSettings(t, dir, s)

			if got := b.apikeyForSale(saleClassFixed); got != tc.wantFixed {
				t.Errorf("apikeyForSale(fixed) = %v, want %v", got, tc.wantFixed)
			}
			if got := b.apikeyForSale(saleClassBedrock); got != tc.wantBed {
				t.Errorf("apikeyForSale(bedrock) = %v, want %v", got, tc.wantBed)
			}
			// The buy menu must agree with the gate.
			rows, _ := b.apikeyBuyRows(100, i18n.EN)
			var hasFixed bool
			for _, r := range rows {
				for _, btn := range r {
					if btn.CallbackData == "fixedapi:start" {
						hasFixed = true
					}
				}
			}
			if hasFixed != tc.wantFixed {
				t.Errorf("fixed buy row present = %v, want %v (rows=%v)", hasFixed, tc.wantFixed, rows)
			}
		})
	}
}

// TestFixedBuyFlowEndToEnd drives the real customer path: the /buy row, the
// amount prompt (with its own quick-pick prefix), a quick-pick tap, and the
// resulting order — which must carry the FIXED sentinel, not the bedrock one.
func TestFixedBuyFlowEndToEnd(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newFixedBot(t)
	const chat = 100

	b.Process(ctx, callbackUpdate(chat, "fixedapi:start"))
	prompt := lastMsg(t, f)
	if !strings.Contains(prompt.Text, strings.ToUpper(apikeyDefaultNameFixed)) {
		t.Errorf("prompt = %q, want the uppercased fixed SKU name", prompt.Text)
	}
	if !containsStr(buttonData(prompt.ReplyMarkup), "fixedapiqty:1000") {
		t.Fatalf("quick picks = %v, want a fixedapiqty: prefix", buttonData(prompt.ReplyMarkup))
	}
	// A stale BEDROCK quick pick must not buy against the armed fixed prompt.
	b.Process(ctx, callbackUpdate(chat, "apikeyqty:1000"))
	if o, _ := st.ReservedOrderForChat(fmtInt(chat)); o != nil {
		t.Fatalf("a cross-SKU quick pick must not create an order, got %+v", o)
	}

	b.Process(ctx, callbackUpdate(chat, "fixedapiqty:1000"))
	o, err := st.ReservedOrderForChat(fmtInt(chat))
	if err != nil || o == nil {
		t.Fatalf("no reserved order after the quick pick: %v", err)
	}
	if o.Product != fixedApikeyProduct {
		t.Fatalf("order product = %q, want %q", o.Product, fixedApikeyProduct)
	}
	if o.Qty != 1000 {
		t.Errorf("order qty = %d, want 1000 credits", o.Qty)
	}
	// 1000 credits at 1.5 USDT / 100 = 15 USDT = 15_000_000 micro.
	if o.Total != 15_000_000 {
		t.Errorf("order total = %d micro-USDT, want 15000000", o.Total)
	}
	if got := b.orderSaleClass(o); got != saleClassFixed {
		t.Errorf("orderSaleClass = %q, want fixed", got)
	}
	// The pay screen must render the fixed template, not bedrock's.
	if text := b.apikeyPayScreenText(o, i18n.EN, 0); !strings.Contains(text, apikeyDefaultNameFixed) ||
		strings.Contains(text, "{name}") {
		t.Errorf("pay screen = %q, want the fixed SKU name and no leaked placeholder", text)
	}
}

// ---- class resolution -------------------------------------------------------

// TestFixedKeyClassDoesNotCollapseToBedrock: a stored fixed row must resolve as
// fixed everywhere pricing, bounds and labels are read from it — the previous
// two-arm shape silently priced it at the bedrock rate card.
func TestFixedKeyClassDoesNotCollapseToBedrock(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   string
	}{
		{saleClassFixed, saleClassFixed},
		{saleClassKiro, saleClassKiro},
		{saleClassBedrock, saleClassBedrock},
		{"", saleClassBedrock},               // legacy pre-split row
		{"something-else", saleClassBedrock}, // unknown class collapses, as before
	} {
		if got := apikeyKeyClass(&store.APIKey{SaleClass: tc.stored}); got != tc.want {
			t.Errorf("apikeyKeyClass(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
	if got := apikeyKeyClass(nil); got != saleClassBedrock {
		t.Errorf("apikeyKeyClass(nil) = %q, want bedrock", got)
	}
	// The buy sentinel maps to the class and back.
	if got := apikeyClassForProduct(fixedApikeyProduct); got != saleClassFixed {
		t.Errorf("apikeyClassForProduct(__fixedapi__) = %q, want fixed", got)
	}
	if !isApikeyBuyProduct(fixedApikeyProduct) || !isApikeyFamily(fixedApikeyProduct) ||
		!isAPIKeyProduct(fixedApikeyProduct) || !isNoStockProduct(fixedApikeyProduct) {
		t.Error("__fixedapi__ must be recognised by every product-family predicate")
	}
}

// TestApikeyBindsUpstream pins the shared predicate the three mint-path sites
// route through. Only bedrock and the legacy "" bind a per-customer upstream
// key; kiro and fixed skip binding by construction.
func TestApikeyBindsUpstream(t *testing.T) {
	for cls, want := range map[string]bool{
		saleClassBedrock: true,
		"":               true,
		saleClassKiro:    false,
		saleClassFixed:   false,
	} {
		if got := apikeyBindsUpstream(cls); got != want {
			t.Errorf("apikeyBindsUpstream(%q) = %v, want %v", cls, got, want)
		}
	}
}

// ---- recharge ---------------------------------------------------------------

// TestFixedKeyRecharge: a fixed key must be rechargeable, its top-up order must
// resolve back to the fixed class for pricing/bounds, and the recharge payload
// must carry the RAW stored class so the server's own class guard can fire.
func TestFixedKeyRecharge(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newFixedBot(t)
	const chat = 100
	const des = "f4f40000f4f40000f4f40000f4f40000"
	o := seedOrder(t, st, des, chat, fixedApikeyProduct, 1000, 1500000, "paid")
	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("fixed mint must deliver")
	}

	const rcDes = "f5f50000f5f50000f5f50000f5f50000"
	if _, err := st.AddAPIKeyRecharge(rcDes, des, chat, 500); err != nil {
		t.Fatal(err)
	}
	rc := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 750000, "paid")
	// The recharge order inherits the TARGET key's class through the binding.
	if got := b.orderSaleClass(rc); got != saleClassFixed {
		t.Fatalf("orderSaleClass(recharge) = %q, want fixed", got)
	}
	if !b.deliverApikeyRecharge(ctx, rc, rcDes) {
		t.Fatal("a fixed key must be rechargeable")
	}
	if got := fk.lastRecharge()["saleClass"]; got != "fixed" {
		t.Errorf("recharge saleClass = %v, want the raw stored fixed", got)
	}
}

// ---- bounds / pricing knobs -------------------------------------------------

// TestFixedBoundsAreItsOwn: the fixed SKU reads its own settings/env knobs and
// its defaults hold with nothing configured — a shared read would let a bedrock
// change move the fixed SKU's floor or ceiling.
func TestFixedBoundsAreItsOwn(t *testing.T) {
	b, _, _, fk, dir := newAPIKeyBot(t)
	s := fixedSettings(fk)
	s["apikey_min_credits"] = 250
	s["apikey_max_credits"] = 900
	s["kiro_apikey_min_credits"] = 700
	writeSettings(t, dir, s)

	// Nothing fixed-specific set -> the built-in defaults.
	if got := b.apikeyMinCredits(saleClassFixed); got != 100 {
		t.Errorf("fixed min = %d, want the 100 default (not bedrock's 250 / kiro's 700)", got)
	}
	if got := b.apikeyMaxCredits(saleClassFixed); got != 1000000 {
		t.Errorf("fixed max = %d, want the 1000000 default (not bedrock's 900)", got)
	}

	s["fixed_apikey_min_credits"] = 300
	s["fixed_apikey_max_credits"] = 4000
	writeSettings(t, dir, s)
	if got := b.apikeyMinCredits(saleClassFixed); got != 300 {
		t.Errorf("fixed min = %d, want 300", got)
	}
	if got := b.apikeyMaxCredits(saleClassFixed); got != 4000 {
		t.Errorf("fixed max = %d, want 4000", got)
	}
	// The siblings must not have moved.
	if got := b.apikeyMinCredits(saleClassBedrock); got != 250 {
		t.Errorf("bedrock min = %d, want 250", got)
	}
	if got := b.apikeyMinCredits(saleClassKiro); got != 700 {
		t.Errorf("kiro min = %d, want 700", got)
	}
	// Quick picks come from the fixed SKU's own slice, filtered to the range.
	got := qtyChoicesFor(saleClassFixed, 300, 4000)
	want := []int64{500, 1000, 2000}
	if len(got) != len(want) {
		t.Fatalf("fixed quick picks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fixed quick picks = %v, want %v", got, want)
		}
	}
}

// TestFixedRateIsItsOwnSetting: the fixed rate reads fixed_apikey_usdt_per_100
// and has NO legacy per-1k fallback (the SKU postdates that migration).
func TestFixedRateIsItsOwnSetting(t *testing.T) {
	b, _, _, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["apikey_usdt_per_1k"] = 40.0
	writeSettings(t, dir, s)
	if got := b.apikeyRateUSDT(saleClassFixed); got != 0 {
		t.Errorf("fixed rate = %v, want 0 — there is no legacy per-1k fallback for this SKU", got)
	}
	s["fixed_apikey_usdt_per_100"] = 1.5
	writeSettings(t, dir, s)
	if got := b.apikeyRateUSDT(saleClassFixed); got != 1.5 {
		t.Errorf("fixed rate = %v, want 1.5", got)
	}
	if got := b.apikeyRateUSDT(saleClassBedrock); got != 4.0 {
		t.Errorf("bedrock rate = %v, want its own 4.0", got)
	}
}

// ---- removing a code from a RECHARGE order ----------------------------------

// TestRemoveCodeRepricesRechargeAtTargetKeyClass is the money-path guard for
// cbRemoveCode (selfservice.go). Removing a promo re-computes the order's
// baseline total, and for an API-key-family order that computation needs the
// sale class. The recharge sentinel __apikeyrc__ passes isAPIKeyProduct, but
// its class lives on the TARGET key's stored sale_class — apikeyClassForProduct
// maps only the three MINT sentinels and falls through to bedrock, so deriving
// the class from the product sentinel re-quoted a fixed recharge at the bedrock
// rate card (1.5 vs 4.0 USDT per 100 credits, a 2.67x overcharge) and
// UpdateOrderCode persisted that total for the buyer to pay.
//
// PRE-EXISTING before the Fixed SKU: kiro recharges have hit the same fall-
// through since the kiro SKU shipped. The fix routes through orderSaleClass,
// which is what the sibling apply path (applyCodeToOrder, code.go) already did.
//
// The test is written so restoring the old shape fails it: the asserted total
// is the FIXED baseline, and the bedrock baseline the bug produced is asserted
// to be a different number, so the two can never be satisfied at once.
func TestRemoveCodeRepricesRechargeAtTargetKeyClass(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newFixedBot(t)
	const (
		chat      = int64(100)
		targetDes = "aaaa1111aaaa1111aaaa1111aaaa1111"
		rcDes     = "bbbb2222bbbb2222bbbb2222bbbb2222"
		credits   = int64(1000)
		// 1000 credits at the FIXED rate (1.5 USDT / 100) = 15 USDT.
		fixedBaseline = int64(15_000_000)
		// The same 1000 credits at the BEDROCK rate (4.0 USDT / 100) = 40 USDT
		// — what the bug charged.
		bedrockBaseline = int64(40_000_000)
	)

	// A delivered FIXED key, and a reserved recharge order pointing at it. The
	// recharge order's own product carries no class; only this binding does.
	seedOrder(t, st, targetDes, chat, fixedApikeyProduct, credits, fixedBaseline, "delivered")
	if _, err := st.AddAPIKey(targetDes, chat, "kid-fixed", "sk-fixed-abc", float64(credits), saleClassFixed); err != nil {
		t.Fatalf("AddAPIKey: %v", err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, credits, fixedBaseline, "reserved")
	if _, err := st.AddAPIKeyRecharge(rcDes, targetDes, chat, float64(credits)); err != nil {
		t.Fatalf("AddAPIKeyRecharge: %v", err)
	}
	// Sanity: the binding really does resolve to fixed, so a failure below is
	// about cbRemoveCode's class lookup and not about the seeding.
	if got := b.orderSaleClass(o); got != saleClassFixed {
		t.Fatalf("orderSaleClass = %q, want fixed — seeding is wrong, not the handler", got)
	}

	// Apply a promo (the apply path already resolves the class correctly), then
	// remove it — the remove path is what is under test.
	if err := st.CreatePromo("RCFIX", 20.0, 5, false); err != nil {
		t.Fatalf("CreatePromo: %v", err)
	}
	b.applyCodeToOrder(ctxFor(b, chat), o, "RCFIX")
	if discounted := currentTotal(t, st, rcDes); discounted >= fixedBaseline {
		t.Fatalf("total after applying the promo = %d, want below the %d baseline", discounted, fixedBaseline)
	}

	b.Process(ctx, callbackUpdate(chat, cbData("rmcode", rcDes)))

	got := currentTotal(t, st, rcDes)
	if got == bedrockBaseline {
		t.Fatalf("total after rmcode = %d — re-priced at the BEDROCK rate card; "+
			"the sale class must come from orderSaleClass, not the product sentinel", got)
	}
	if got != fixedBaseline {
		t.Errorf("total after rmcode = %d, want the fixed baseline %d", got, fixedBaseline)
	}
}

// ---- the SKU tag actually renders -------------------------------------------

// TestMyKeysRendersFixedTag drives the real My-Keys list (showMyAPIKeys,
// apikey_keys.go) with a stored fixed row and asserts the ⚡ Fixed tag reaches
// the message.
//
// The i18n tests prove SkuTag["fixed"] EXISTS in all three languages; nothing
// proved a fixed key ever reaches it. showMyAPIKeys collapses anything it does
// not recognise onto bedrock before indexing SkuTag (so an unknown class can
// never render an EMPTY tag), and "fixed" had to be added to that recognised
// set by hand — dropping it from the condition makes every fixed key silently
// display as "🔷 Bedrock" with no test failing.
//
// This must drive showMyAPIKeys specifically: the recharge picker (apikey.go)
// resolves its tag through apikeyKeyClass, a different code path that the
// collapse condition here does not touch — a picker-based test would pass
// either way.
func TestMyKeysRendersFixedTag(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newFixedBot(t)
	const (
		chat     = int64(100)
		fixedDes = "cccc3333cccc3333cccc3333cccc3333"
		kiroDes  = "dddd4444dddd4444dddd4444dddd4444"
	)
	// A fixed key and a kiro one side by side: the kiro row proves the tag
	// column is really per-row, so a change that hardcoded one tag for
	// everything would not slip through.
	if _, err := st.AddAPIKey(fixedDes, chat, "kid-fixed", "sk-fixed-abc", 1000, saleClassFixed); err != nil {
		t.Fatalf("AddAPIKey fixed: %v", err)
	}
	if _, err := st.AddAPIKey(kiroDes, chat, "kid-kiro", "sk-kiro-xyz", 1000, saleClassKiro); err != nil {
		t.Fatalf("AddAPIKey kiro: %v", err)
	}

	b.showMyAPIKeys(ctx, chat, i18n.EN)

	text := lastMsg(t, f).Text
	fixedTag := i18n.C().APIKey.SkuTag[saleClassFixed].T(i18n.EN)
	if fixedTag == "" {
		t.Fatal("SkuTag[fixed] is empty — the catalog entry is missing")
	}
	if !strings.Contains(text, fixedTag) {
		t.Errorf("My-Keys list = %q, want the %q tag — a fixed key is rendering under "+
			"another SKU's label", text, fixedTag)
	}
	if kiroTag := i18n.C().APIKey.SkuTag[saleClassKiro].T(i18n.EN); !strings.Contains(text, kiroTag) {
		t.Errorf("My-Keys list = %q, want the sibling %q tag too", text, kiroTag)
	}
}

// ---- the live per-request rate card on the amount prompt --------------------
//
// The Fixed Pricing amount prompt shows the pool's current per-request price
// card, so a customer knows which models cost how many credits before they
// type an amount. The card is fetched live from Kiro-Go on the button tap, so
// the tests below care as much about what happens when that fetch DOESN'T work
// as about what it renders when it does: the card is decoration on the prompt
// and must never become a gate in front of the sale.

// fixedPriceCardBot is newFixedBot with a stocked flat-price card.
func fixedPriceCardBot(t *testing.T) (*Bot, *fakeTG, *fakeKiro) {
	t.Helper()
	b, f, _, fk, _ := newFixedBot(t)
	fk.ModelPriceList = []map[string]any{
		{"model": "claude-opus-5", "perRequest": 4.0},
		{"model": "claude-haiku-4", "perRequest": 0.25},
	}
	return b, f, fk
}

// TestFixedPromptShowsPriceCard: tapping Fixed Pricing renders one
// "<model> — <n> credits/request" row per priced model, inside the amount
// prompt, above the "type how many credits" line.
func TestFixedPromptShowsPriceCard(t *testing.T) {
	ctx := context.Background()
	b, f, _ := fixedPriceCardBot(t)

	b.Process(ctx, callbackUpdate(100, "fixedapi:start"))

	got := lastMsg(t, f).Text
	for _, want := range []string{
		"claude-opus-5 — 4 credits/request",
		"claude-haiku-4 — 0.25 credits/request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fixed prompt = %q, want a price row %q", got, want)
		}
	}
	// The card must land INSIDE the prompt, not replace it: the amount question
	// is the whole point of the screen.
	if !strings.Contains(got, "Type how many credits you want") {
		t.Errorf("fixed prompt = %q, want the amount question still present", got)
	}
	if strings.Contains(got, "{prices}") {
		t.Errorf("fixed prompt = %q, want no stray placeholder", got)
	}
}

// TestFixedPromptSurvivesPriceCardFailure: every way the fetch can fail must
// still produce a sendable, buyable prompt — and must never put an upstream
// error string in front of a customer.
func TestFixedPromptSurvivesPriceCardFailure(t *testing.T) {
	ctx := context.Background()

	// assertBuyablePrompt is the shared bar for all three failure modes: the
	// prompt still went out, it still asks for an amount, and it carries
	// neither a price row nor any leaked upstream/plumbing text.
	assertBuyablePrompt := func(t *testing.T, got string) {
		t.Helper()
		if !strings.Contains(got, "Type how many credits you want") {
			t.Fatalf("prompt = %q, want the amount question — the sale must not be blocked "+
				"by an unavailable price card", got)
		}
		if strings.Contains(got, "credits/request") {
			t.Errorf("prompt = %q, want no price rows when the card is unavailable", got)
		}
		if strings.Contains(got, "{prices}") {
			t.Errorf("prompt = %q, want no stray placeholder", got)
		}
		// Byte-exact: with no card, the explanatory paragraph must run straight
		// into the amount question exactly as it did before the card existed.
		// A dangling header, a stray blank line or a lone newline left behind by
		// a half-rendered card would all break this.
		if !strings.Contains(got, "answer is.\n\nType how many credits you want") {
			t.Errorf("prompt = %q, want the pre-card wording with no leftover artifact "+
				"where the card would have gone", got)
		}
		// The fake's 4xx body is "model_prices rejected" and kirogo wraps its
		// failures with a "kirogo:" prefix — either surfacing in a customer's
		// chat is the leak this checks for.
		for _, leak := range []string{"model_prices rejected", "kirogo", "⚠️"} {
			if strings.Contains(got, leak) {
				t.Errorf("prompt = %q, want no upstream error text (%q) shown to a customer", got, leak)
			}
		}
	}

	t.Run("server rejects the call", func(t *testing.T) {
		b, f, fk := fixedPriceCardBot(t)
		fk.ModelPricesStatus = 404 // 4xx: kirogo never retries, so this is instant

		b.Process(ctx, callbackUpdate(100, "fixedapi:start"))

		assertBuyablePrompt(t, lastMsg(t, f).Text)
	})

	t.Run("no model carries a flat price yet", func(t *testing.T) {
		b, f, fk := fixedPriceCardBot(t)
		fk.ModelPriceList = nil // a NORMAL state, not an error

		b.Process(ctx, callbackUpdate(100, "fixedapi:start"))

		assertBuyablePrompt(t, lastMsg(t, f).Text)
	})

	t.Run("no row fits the budget", func(t *testing.T) {
		b, f, fk := fixedPriceCardBot(t)
		// One model whose name alone blows the card's byte budget. Nothing can
		// be shown, so the whole block must be dropped — rendering the helper's
		// bare "… and N more (not shown)." would advertise a list the customer
		// never saw.
		fk.ModelPriceList = []map[string]any{
			{"model": strings.Repeat("m", fixedPriceCardMaxBytes+1), "perRequest": 4.0},
		}

		b.Process(ctx, callbackUpdate(100, "fixedapi:start"))

		got := lastMsg(t, f).Text
		assertBuyablePrompt(t, got)
		if strings.Contains(got, "more (not shown)") {
			t.Errorf("prompt = %q, want no partial-list suffix when no row was shown", got)
		}
	})

	t.Run("pool is reachable but slow", func(t *testing.T) {
		b, f, fk := fixedPriceCardBot(t)
		fk.ModelPricesDelay = 300 * time.Millisecond
		// Shrink the customer-tap bound so the test can prove the bound is what
		// ends the wait. Restored immediately: it is a package-level var.
		restore := fixedPricesTimeout
		fixedPricesTimeout = 30 * time.Millisecond
		defer func() { fixedPricesTimeout = restore }()

		start := time.Now()
		b.Process(ctx, callbackUpdate(100, "fixedapi:start"))
		elapsed := time.Since(start)

		assertBuyablePrompt(t, lastMsg(t, f).Text)
		// The fetch must actually have been attempted, or this subtest would
		// pass vacuously on any future change that skipped the call entirely
		// (an unconfigured client returns instantly and satisfies everything
		// else here). The fake counts the hit before it starts stalling.
		if n := fk.modelPricesHitCount(); n < 1 {
			t.Fatalf("model_prices fetched %d time(s) — the timeout path was never exercised", n)
		}
		// Bounded by fixedPricesTimeout, not by the pool: a generous 10x the
		// bound still lands far under the fake's stall, so if the prompt had
		// waited for the slow answer (or for kirogo's own 20s-per-attempt
		// retries) this would blow past it.
		if want := 10 * fixedPricesTimeout; elapsed >= want {
			t.Errorf("prompt took %v against a %v bound (pool stalls %v) — the fetch must be "+
				"bounded, not left to hang a customer's tap",
				elapsed, fixedPricesTimeout, fk.ModelPricesDelay)
		}
	})
}

// TestBedrockAndKiroPromptsHaveNoPriceCard: the other two SKUs bill by
// rate-card burn, so a per-request card would be meaningless on their prompts —
// and they must not pay an admin-API round trip for it either.
func TestBedrockAndKiroPromptsHaveNoPriceCard(t *testing.T) {
	ctx := context.Background()
	b, f, _, fk, dir := newFixedBot(t)
	fk.ModelPriceList = []map[string]any{{"model": "claude-opus-5", "perRequest": 4.0}}
	s := fixedSettings(fk)
	s["kiro_apikey_enabled"] = true
	s["kiro_apikey_usdt_per_100"] = 1.0
	writeSettings(t, dir, s)

	for _, tc := range []struct{ name, data string }{
		{"bedrock", "apikey:start"},
		{"kiro", "kiroapi:start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b.Process(ctx, callbackUpdate(100, tc.data))
			got := lastMsg(t, f).Text
			if strings.Contains(got, "credits/request") || strings.Contains(got, "claude-opus-5") {
				t.Errorf("%s prompt = %q, want no per-request price card", tc.name, got)
			}
			if strings.Contains(got, "{prices}") {
				t.Errorf("%s prompt = %q, want no stray placeholder", tc.name, got)
			}
		})
	}
	// Nothing above should have cost a fetch: the card is computed for the
	// fixed SKU alone, so these two taps must not touch Kiro-Go at all.
	if n := fk.modelPricesHitCount(); n != 0 {
		t.Errorf("model_prices fetched %d time(s) for the bedrock/kiro prompts, want 0", n)
	}
}

// TestAppendModelPriceRowsCaps pins the shared renderer's two bounds at their
// boundaries. It is a unit test because both call sites depend on the same
// arithmetic: the admin listing (cbModelPrices) relies on the byte budget
// counting its already-written header, and the customer prompt relies on the
// row cap plus the "… and N more" suffix that tells a reader the list is
// partial.
func TestAppendModelPriceRowsCaps(t *testing.T) {
	// prices builds n identical-length rows, so the byte arithmetic below is
	// exact without hardcoding the width of a row.
	prices := func(n int) []kirogo.ModelPrice {
		out := make([]kirogo.ModelPrice, n)
		for i := range out {
			out[i] = kirogo.ModelPrice{Model: "model-x", PerRequest: 2}
		}
		return out
	}
	render := func(seed string, p []kirogo.ModelPrice, maxRows, maxBytes int) string {
		var sb strings.Builder
		sb.WriteString(seed)
		appendModelPriceRows(&sb, p, maxRows, maxBytes)
		return sb.String()
	}
	// One row's byte width, measured rather than assumed.
	rowLen := len(render("", prices(1), 0, 1<<20))
	const huge = 1 << 20

	t.Run("row cap trims and says how many are hidden", func(t *testing.T) {
		got := render("", prices(7), 6, huge)
		if n := strings.Count(got, "credits/request"); n != 6 {
			t.Errorf("rendered %d rows, want the cap's 6: %q", n, got)
		}
		if !strings.Contains(got, "… and 1 more (not shown).") {
			t.Errorf("render = %q, want the partial-list suffix", got)
		}
	})

	t.Run("exactly at the row cap says nothing is hidden", func(t *testing.T) {
		got := render("", prices(6), 6, huge)
		if n := strings.Count(got, "credits/request"); n != 6 {
			t.Errorf("rendered %d rows, want all 6: %q", n, got)
		}
		if strings.Contains(got, "more (not shown)") {
			t.Errorf("render = %q, want no suffix when the whole list fit", got)
		}
	})

	t.Run("byte budget stops before the row that would breach it", func(t *testing.T) {
		// Room for exactly two rows: the third must not be written.
		got := render("", prices(3), 0, 2*rowLen)
		if n := strings.Count(got, "credits/request"); n != 2 {
			t.Errorf("rendered %d rows under a 2-row budget, want 2: %q", n, got)
		}
		// One byte short of two rows: only the first fits.
		got = render("", prices(3), 0, 2*rowLen-1)
		if n := strings.Count(got, "credits/request"); n != 1 {
			t.Errorf("rendered %d rows one byte under a 2-row budget, want 1: %q", n, got)
		}
	})

	t.Run("a caller's existing header counts against the budget", func(t *testing.T) {
		// cbModelPrices writes its "FLAT PRICE LIST (n)" header before calling
		// in, and that header has always eaten into the same budget. A seed as
		// long as one row must therefore cost one row.
		got := render(strings.Repeat("h", rowLen), prices(3), 0, 2*rowLen)
		if n := strings.Count(got, "credits/request"); n != 1 {
			t.Errorf("rendered %d rows after a one-row-long header, want 1: %q", n, got)
		}
	})
}
