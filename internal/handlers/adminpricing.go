package handlers

// adminpricing.go — 🏷️ PRICING & DISCOUNTS category (py_pricing_spec.md is the
// exact behavioural/verbatim-string reference for every callback below, keyed
// off telegram_bot.py). All 11 callbacks in this file used to fall through
// cbAdmAction's default branch to the generic "not wired up yet" notice
// (admin.go:76, adminNotPorted) — this file replaces each stub with real
// behaviour.
//
// Permission model (py_pricing_spec.md §0): the WHOLE category is
// full-admin (super-admin) only — it appears in no capability table, so no
// standard-admin grant ever unlocks any of these 11 callbacks. Python
// enforces this with an OUTER gate in handle_callback (before an adm:
// callback is even dispatched) plus a REDUNDANT inline is_super_admin
// re-check inside 8 of the 11 handlers. Go has not ported that outer
// capability-table gate yet (admin.go's own header, and adminsettings.go's:
// "Go has no per-capability grant table yet") — cbAdmAction only requires
// isAdmin, which today happens to coincide with isSuperAdmin only because no
// standard admin has been granted, not because of any structural guarantee.
// So EVERY handler in this file (not just 8 of 11) calls
// settingsSuperAdminOnly (adminsettings.go) as its own first line — the
// single enforcement point this build has for "full-admin only", reused
// rather than duplicated.
import (
	"context"
	"encoding/json"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// ---- registration -----------------------------------------------------------

// registerAdminPricing wires this file's 11 captures + the 6 standalone
// tier/promo callback prefixes into the ordered dispatch tables. Called from
// New (dispatch.go) AFTER registerAdminSettings, so insertCapturesBefore
// finds "setkirourl"/"code"/"txid" already spliced into the chain:
//   - setapiprice/setapimin/setapimax/setkiroprice/setkiromin/setkiromax/
//     setapicap (canonical #11-#17) land ahead of "setkirourl" (#18, already
//     registered by registerAdminSettings) — setrate (#10) stays first.
//   - discount/promo (#34/#36) land ahead of "code" (#37, already registered
//     by registerOTP).
//   - tier/price (#41/#42) land ahead of "txid" (#44, already registered by
//     registerUSDT).
//
// Together the result stays an in-order subsequence of CanonicalCaptureOrder
// (captures.go / handlers-go.md §7).
func (b *Bot) registerAdminPricing() {
	b.insertCapturesBefore("setkirourl",
		stage{name: "setapiprice", fn: (*Bot).capSetApiPrice},
		stage{name: "setapimin", fn: (*Bot).capSetApiMin},
		stage{name: "setapimax", fn: (*Bot).capSetApiMax},
		stage{name: "setkiroprice", fn: (*Bot).capSetKiroPrice},
		stage{name: "setkiromin", fn: (*Bot).capSetKiroMin},
		stage{name: "setkiromax", fn: (*Bot).capSetKiroMax},
		stage{name: "setapicap", fn: (*Bot).capSetApiCap},
	)
	b.insertCapturesBefore("code",
		stage{name: "discount", fn: (*Bot).capDiscount},
		stage{name: "promo", fn: (*Bot).capPromo},
	)
	b.insertCapturesBefore("txid",
		stage{name: "tier", fn: (*Bot).capTier},
		stage{name: "price", fn: (*Bot).capPrice},
	)
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "priceprod:", fn: (*Bot).cbPriceProd}, // @14794
		cbRoute{key: "pricetier:", fn: (*Bot).cbPriceTier}, // @14878
		cbRoute{key: "tieradd:", fn: (*Bot).cbTierAdd},     // @14818
		cbRoute{key: "tierdel:", fn: (*Bot).cbTierDel},     // @14835
		cbRoute{key: "tierdo:", fn: (*Bot).cbTierDo},       // @14854
		cbRoute{key: "promodel:", fn: (*Bot).cbPromoDel},   // store.py DeletePromo
	)
}

// ---- shared money/validation helpers ----------------------------------------

// adminUsdtRe is the signed-decimal shape _admin_usdt_to_vnd accepts
// (telegram_bot.py:6463-6477): an optional sign, digits, an optional decimal
// part. Commas are stripped as thousands separators BEFORE this match, so
// "1,234.5" is accepted the same as "1234.5".
var adminUsdtRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// adminUsdtToVnd converts an admin-typed USDT amount into stored VND —
// _admin_usdt_to_vnd (telegram_bot.py:6463-6477): comma-stripped, matched
// against a plain signed-decimal shape, then rounded at the live USD→VND
// rate. ok is false on ANY parse failure OR an unset/invalid rate — either
// way the caller must reject the input rather than persist a bogus price
// (a price write is money: CLAUDE.md/py_pricing_spec.md §1's "a rejected
// value must never be persisted").
func (b *Bot) adminUsdtToVnd(text string) (int64, bool) {
	s := strings.ReplaceAll(strings.TrimSpace(text), ",", "")
	if !adminUsdtRe.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	rateStr := config.DefaultUSDToVND
	if b.cfg != nil {
		rateStr = b.cfg.USDToVND()
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || rate <= 0 {
		return 0, false
	}
	return int64(math.Round(f * rate)), true
}

// fmtRate renders a USDT rate the way Python's :g format spec does: 6
// significant digits, no trailing zeros, e.g. 4 -> "4", 0.5 -> "0.5",
// 0.123456789 -> "0.123457" (apiPricingLabel in admin.go does the same for
// the menu labels; this is the twin for confirmation text). Precision -1
// (shortest round-trippable repr) was wrong here — it would print
// 0.123456789 back verbatim instead of rounding to 6 sig figs like Python.
func fmtRate(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }

// tierCodeRe matches a valid warranty tier code shape (input already
// upper-cased before this match) — BH<N>D (days) or BH<N>H (hours); the bare
// "KBH" no-warranty code is checked separately since it has no digits.
var tierCodeRe = regexp.MustCompile(`^BH\d+D$|^BH\d+H$`)

// validTierCode reports whether code (already upper-cased) is a legal
// warranty tier code — the tieradd: validator's code half
// (telegram_bot.py:19179-19213): KBH exactly, or BH<N>D, or BH<N>H.
func validTierCode(code string) bool {
	return code == "KBH" || tierCodeRe.MatchString(code)
}

// rawApikeyMaxCredits reads the RAW stored/env max-credits setting for a SKU
// with NO cross-field flooring — deliberately NOT apikeyMaxCredits()
// (apikey.go), whose `max(min, v)` floor would make a min-raise validator
// unable to ever reject a min above the (auto-floored) max. Python's own
// apimin/kiromin consumption blocks read this same raw value (comment: "RAW
// stored max", telegram_bot.py:18404).
func (b *Bot) rawApikeyMaxCredits(saleClass string) int64 {
	key, envKey, envDef := "apikey_max_credits", "APIKEY_MAX_CREDITS", int64(1000000)
	if saleClass == saleClassKiro {
		key, envKey = "kiro_apikey_max_credits", "KIRO_APIKEY_MAX_CREDITS"
	}
	v := int64(0)
	if f, ok := settingsNumber(b.settings(), key); ok {
		v = int64(f)
	}
	if v <= 0 {
		v = b.envInt64(envKey, envDef)
	}
	return v
}

// productAt resolves a bare index string (the trailing segment of
// priceprod:/pricetier:/tieradd:/tierdel:/tierdo:) into a product name — the
// same INDEX-based resolution buy.go's cbBuy uses for buy:<idx> (Telegram's
// 64-byte callback_data limit is why product refs are indices, not names).
// ok is false for a stale/out-of-range index (a product removed after the
// menu was rendered) or when no stock machine is wired (b.stock nil ->
// listProducts() is empty -> every index is out of range), so callers never
// need a separate nil check on b.stock before using the returned product.
func (b *Bot) productAt(idxStr string) (product string, ok bool) {
	idx, err := strconv.Atoi(idxStr)
	products := b.listProducts()
	if err != nil || idx < 0 || idx >= len(products) {
		return "", false
	}
	return products[idx], true
}

// ---- 1. adm:setprice — prices & warranty tiers editor -----------------------

// cbSetPrice opens the product picker for 🏷️ Prices & warranty tiers —
// adm:setprice (telegram_bot.py:13821-13829).
func (b *Bot) cbSetPrice(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	products := b.listProducts()
	if len(products) == 0 {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.NoProducts.T(i18n.EN), admNav("admcat:pricing"))
		return
	}
	rows := make([][]tg.InlineKeyboardButton, 0, len(products)+1)
	for i, p := range products {
		rows = append(rows, btnRow(p, cbData("priceprod", itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:pricing"))
	b.sendMenu(cc.Ctx, cc.ChatID, "🏷️ Prices & tiers — which product?", rows)
}

// cbPriceProd lists one product's discovered tiers — priceprod:<idx>
// (telegram_bot.py:14794-14817). Clears any half-typed price/tier prompt
// FIRST (Python's explicit "_clear_admin_pending" note on this handler) so a
// stale tieradd/pricetier armed from a different product can't eat the next
// plain-text line.
func (b *Bot) cbPriceProd(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.ClearAdminPending()
	idxStr := strings.TrimPrefix(cc.Data, "priceprod:")
	product, ok := b.productAt(idxStr)
	if !ok {
		return
	}
	b.renderPriceProdMenu(cc.Ctx, cc.ChatID, idxStr, product)
}

// renderPriceProdMenu renders {product}'s tier list, one button per
// discovered tier (pricetier:<idx>:<code>) plus Add/Remove — the same
// screen priceprod: renders and tierdo:'s success path returns to
// (telegram_bot.py:14794-14817). Safe to call with b.stock non-nil: every
// caller resolves idxStr/product via productAt first, which only returns
// ok=true when listProducts() was non-empty — that requires b.stock != nil.
func (b *Bot) renderPriceProdMenu(ctx context.Context, chatID int64, idxStr, product string) {
	tiers := b.stock.ProductTiers(product)
	rows := make([][]tg.InlineKeyboardButton, 0, len(tiers)+2)
	for _, t := range tiers {
		label := b.tierLabel(t.Code, i18n.EN) + " — " + b.usdtDisp(int64(t.Price))
		rows = append(rows, btnRow(label, cbData("pricetier", idxStr, t.Code)))
	}
	rows = append(rows, btnRow("➕ Add tier", cbData("tieradd", idxStr)))
	if len(tiers) > 0 {
		rows = append(rows, btnRow("🗑 Remove tier", cbData("tierdel", idxStr)))
	}
	rows = append(rows, btnRow("↩️ Back", "adm:setprice"))
	b.sendMenu(ctx, chatID, "🏷️ "+product+"\nTap a tier to set its price, or add/remove a tier:", rows)
}

// cbPriceTier arms the edit-existing-tier price wizard — pricetier:<idx>:<code>
// (telegram_bot.py:14878-14899).
func (b *Bot) cbPriceTier(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "pricetier:")
	idxStr, code, found := strings.Cut(rest, ":")
	if !found {
		return
	}
	product, ok := b.productAt(idxStr)
	if !ok {
		return
	}
	code = strings.ToUpper(code)
	cc.st.mu.Lock()
	cc.st.Price = &PriceCtx{Product: product, Code: code}
	cc.st.mu.Unlock()
	text := "🏷️ " + product + " — " + b.tierLabel(code, i18n.EN) + "\n"
	if cur, ok := b.stock.ProductPrice(product, code); ok {
		text += "Current: " + b.usdtDisp(int64(cur)) + "\n"
	}
	text += "Enter the new price in USDT (e.g. 5):"
	b.send(cc.Ctx, cc.ChatID, text, backKB(cbData("priceprod", idxStr)))
}

// cbTierAdd arms the add-new-tier wizard — tieradd:<idx>
// (telegram_bot.py:14818-14834).
func (b *Bot) cbTierAdd(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "tieradd:")
	product, ok := b.productAt(idxStr)
	if !ok {
		return
	}
	cc.st.mu.Lock()
	cc.st.Tier = product
	cc.st.mu.Unlock()
	text := "➕ Add a warranty tier to " + product + "\n\n" +
		"Send  CODE PRICE  (code then price in USDT).\n" +
		"Codes:  KBH = no warranty,  BH<N>D = N-day,  BH<N>H = N-hour warranty.\n" +
		"Examples:  BH30D 9   ·   BH12H 7   ·   KBH 3.5"
	b.send(cc.Ctx, cc.ChatID, text, backKB(cbData("priceprod", idxStr)))
}

// cbTierDel lists tiers as tap-to-delete buttons — tierdel:<idx>
// (telegram_bot.py:14835-14853). No tiers -> a plain warning + Back to
// adm:setprice, matching Python's `if not tiers: ...` early return.
func (b *Bot) cbTierDel(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "tierdel:")
	product, ok := b.productAt(idxStr)
	if !ok {
		return
	}
	tiers := b.stock.ProductTiers(product)
	if len(tiers) == 0 {
		b.send(cc.Ctx, cc.ChatID, "⚠️ No tiers to remove.", admNav("adm:setprice"))
		return
	}
	rows := make([][]tg.InlineKeyboardButton, 0, len(tiers)+1)
	for _, t := range tiers {
		rows = append(rows, btnRow(b.tierLabel(t.Code, i18n.EN), cbData("tierdo", idxStr, t.Code)))
	}
	rows = append(rows, btnRow("↩️ Back", cbData("priceprod", idxStr)))
	b.sendMenu(cc.Ctx, cc.ChatID, "🗑 "+product+"\nTap a tier to DELETE it:", rows)
}

// cbTierDo deletes one tier's price file IMMEDIATELY, NO confirmation step
// — tierdo:<idx>:<code> (telegram_bot.py:14854-14877). The confirm step
// already happened one screen up (tapping into the tierdel: list).
func (b *Bot) cbTierDo(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "tierdo:")
	idxStr, code, found := strings.Cut(rest, ":")
	if !found {
		return
	}
	product, ok := b.productAt(idxStr)
	if !ok {
		return
	}
	code = strings.ToUpper(code)
	if err := b.stock.DeleteProductTier(product, code); err != nil {
		b.send(cc.Ctx, cc.ChatID, "❌ Couldn't remove tier: "+err.Error(), admNav("adm:setprice"))
		return
	}
	b.send(cc.Ctx, cc.ChatID, "🗑 Removed tier "+b.tierLabel(code, i18n.EN)+" from "+product+".", admNav("adm:setprice"))
}

// capTier is the "tier" capture (#41, PEEK semantics — telegram_bot.py:
// 19179-19213): armed by tieradd:. An invalid CODE/PRICE line does NOT
// clear the slot — the admin's very next message is retried against the
// SAME product — the deliberate asymmetry with capDiscount/capPromo above,
// which POP before validating and so ABORT on bad input.
func (b *Bot) capTier(c *Ctx) bool {
	c.st.mu.Lock()
	product := c.st.Tier
	c.st.mu.Unlock()
	if product == "" {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	bits := strings.Fields(c.Text)
	code := ""
	if len(bits) > 0 {
		code = strings.ToUpper(bits[0])
	}
	var vnd int64
	var priceOK bool
	if len(bits) > 1 {
		vnd, priceOK = b.adminUsdtToVnd(bits[1])
	}
	if !validTierCode(code) || !priceOK || vnd <= 0 {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Send  CODE PRICE.\n"+
				"Code must be KBH, BH<N>D (days, e.g. BH30D) or BH<N>H "+
				"(hours, e.g. BH12H); price a positive USDT amount.\n"+
				"Example:  BH30D 5   ·   BH12H 7",
			admNav("admcat:pricing"))
		return true // consumed, but the slot stays ARMED (see captures.go: peek)
	}
	c.st.mu.Lock()
	c.st.Tier = ""
	c.st.mu.Unlock()
	// Task #13 Tier 3: capture the prior price (if any) BEFORE it is
	// overwritten — "" when the tier never existed, so auditOldNew renders
	// "(unset) -> <price>" rather than a misleading blank price.
	oldVnd, hadOld := b.stock.ProductPrice(product, code)
	existed, err := b.stock.SetProductPrice(product, code, int(vnd))
	if err != nil {
		b.send(c.Ctx, c.ChatID, "❌ Failed to write price: "+err.Error(), admNav("admcat:pricing"))
		return true
	}
	// Audit AFTER the write succeeds, never before (spec §4.3). The literal
	// sale price — an unaudited change under-prices stock (loss) or
	// over-prices it (lost sales). Target is product/tier-code, not a
	// settings.json key, since this writes a price file (spec §2 Tier 3).
	oldStr := ""
	if hadOld {
		oldStr = b.usdtDisp(int64(oldVnd))
	}
	b.auditConfigSet(fmtInt(c.ChatID), "config_set_product_price", product+"/"+code, oldStr, b.usdtDisp(vnd))
	verb := "Added"
	if existed {
		verb = "Updated"
	}
	b.send(c.Ctx, c.ChatID,
		"✅ "+verb+" tier "+b.tierLabel(code, i18n.EN)+" = "+b.usdtDisp(vnd)+" on "+product,
		admNavRetry("admcat:pricing", "🏷️ Prices & tiers", "adm:setprice"))
	return true
}

// capPrice is the "price" capture (#42, PEEK semantics — telegram_bot.py:
// 19215-19239): armed by pricetier:. Same asymmetry as capTier — an
// invalid price re-prompts against the SAME product+code rather than
// aborting.
func (b *Bot) capPrice(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Price
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	vnd, ok := b.adminUsdtToVnd(c.Text)
	if !ok || vnd <= 0 {
		b.send(c.Ctx, c.ChatID, "⚠️ Invalid price. Enter a positive USDT amount, e.g. 5.", admNav("admcat:pricing"))
		return true // slot stays ARMED
	}
	c.st.mu.Lock()
	c.st.Price = nil
	c.st.mu.Unlock()
	// Task #13 Tier 3: same before/after capture as capTier above — this is
	// the SAME underlying stock.SetProductPrice mutation, reached via the
	// edit-existing-tier wizard (pricetier:) instead of add-new (tieradd:).
	oldVnd, hadOld := b.stock.ProductPrice(armed.Product, armed.Code)
	if _, err := b.stock.SetProductPrice(armed.Product, armed.Code, int(vnd)); err != nil {
		b.send(c.Ctx, c.ChatID, "❌ Failed to write price: "+err.Error(), admNav("admcat:pricing"))
		return true
	}
	oldStr := ""
	if hadOld {
		oldStr = b.usdtDisp(int64(oldVnd))
	}
	b.auditConfigSet(fmtInt(c.ChatID), "config_set_product_price", armed.Product+"/"+armed.Code, oldStr, b.usdtDisp(vnd))
	b.send(c.Ctx, c.ChatID,
		"✅ "+armed.Product+" — "+b.tierLabel(armed.Code, i18n.EN)+" set to "+b.usdtDisp(vnd),
		admNavRetry("admcat:pricing", "🏷️ Prices & tiers", "adm:setprice"))
	return true
}

// ---- 2. adm:setdiscount — volume-discount editor ----------------------------

// discountTier is one volume-discount rule read from / written to
// discounts.json — MinQty is the purchase-quantity threshold, Frac the
// fraction off (0.0-1.0), e.g. {50, 0.30} = "50+ units -> 30% off".
type discountTier struct {
	MinQty int
	Frac   float64
}

// discountsDefaultTiers is _DISCOUNT_DEFAULT (telegram_bot.py:6474): used
// ONLY when discounts.json is ABSENT (a fresh install, or one that has never
// touched 📉 Volume discounts). Checked high-to-low, matching loadDiscounts'
// descending sort.
var discountsDefaultTiers = []discountTier{{50, 0.30}, {20, 0.20}, {10, 0.10}}

// discountsFilePath is discounts.json's location — DISCOUNTS_PATH
// (telegram_bot.py:6470): a plain file at the repo root, deliberately NOT
// settings.json and NOT SQLite (py_pricing_spec.md §2). A relative name,
// matching config.Load("")'s own cwd-relative convention for settings.json —
// this bot is always run from its data directory. A package-level var (not
// a Bot field) so codedTotalFull/codedTotal (code.go/catalog.go) and their
// existing unit tests (code_test.go's "no store, no bot" TestCodedTotalFull)
// keep working unmodified; tests needing a different path override this var
// and restore it via t.Cleanup (see adminpricing_test.go).
var discountsFilePath = "discounts.json"

// loadDiscounts reads discounts.json, sorted descending by MinQty ("checked
// high-to-low", telegram_bot.py:6480-6497). Three cases, in order:
//   - file absent               -> discountsDefaultTiers (fresh install)
//   - file present, parses to []  -> empty (admin deliberately cleared
//     every discount via the "send 0 to clear" path — must NOT fall back
//     to the default, or a clear could never stick)
//   - file present, unparsable   -> discountsDefaultTiers (Python's bare
//     except -> default; a corrupt file must never brick pricing)
//
// Re-read on every call, matching Python's own no-cache load_discounts —
// discounts.json is a rarely-touched admin file, so the tiny disk hit buys
// "an admin's edit takes effect on the very next quote" for free
// (py_pricing_spec.md §2 "Side effects").
//
// Unmarshalling into [][]float64 (not [2]float64) lets a wrong-arity pair
// (e.g. [[50]] or [[50,30,1]]) be DETECTED instead of silently truncated —
// Python's own unpack (`q, p = pair`) raises ValueError on exactly that
// shape, and the whole file falls back to the default rather than trusting
// a partially-decoded tier. Each pair is also guarded against
// `int(q) > 0 and float(p) >= 0` (telegram_bot.py:6480-6497): a corrupt or
// hand-edited tier like [0,50] would otherwise match every order (qty>=0)
// and discount 50% off everything.
func loadDiscounts() []discountTier {
	raw, err := os.ReadFile(discountsFilePath)
	if err != nil {
		return append([]discountTier(nil), discountsDefaultTiers...)
	}
	var pairs [][]float64
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return append([]discountTier(nil), discountsDefaultTiers...)
	}
	out := make([]discountTier, 0, len(pairs))
	for _, p := range pairs {
		if len(p) != 2 {
			// Wrong arity anywhere in the file is a corrupt file, not a
			// corrupt entry — fall back to the default wholesale, matching
			// Python's ValueError-on-unpack -> bare except -> default path.
			return append([]discountTier(nil), discountsDefaultTiers...)
		}
		if p[0] <= 0 || p[1] < 0 {
			continue // skip a nonsense tier rather than let it discount every order
		}
		out = append(out, discountTier{MinQty: int(p[0]), Frac: p[1] / 100.0})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MinQty > out[j].MinQty })
	return out
}

// saveDiscounts persists tiers to discounts.json as a JSON list of
// [minQty, percent] pairs (percent 0-100, NOT a fraction —
// py_pricing_spec.md §2's on-disk shape), sorted descending by MinQty, via
// tmp+rename so a reader can never observe a half-written file
// (save_discounts, telegram_bot.py:6501-6507). A nil/empty tiers persists as
// "[]" — the deliberate "admin cleared everything" marker loadDiscounts
// checks for, never as a missing file (which would mean "never configured").
func saveDiscounts(tiers []discountTier) error {
	// Stable, not just sorted: a duplicate MinQty (e.g. "10:10, 10:30",
	// which parseDiscountInput's validator accepts) must keep the
	// first-listed tier winning, matching Python's Timsort stability —
	// sort.Slice offers no such guarantee and could silently flip which
	// duplicate wins between runs.
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].MinQty > tiers[j].MinQty })
	pairs := make([][2]float64, len(tiers))
	for i, t := range tiers {
		pairs[i] = [2]float64{float64(t.MinQty), math.Round(t.Frac*100*10000) / 10000}
	}
	raw, err := json.Marshal(pairs)
	if err != nil {
		return err
	}
	tmp := discountsFilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, discountsFilePath)
}

// fmtDiscountTiers renders a tier set compactly for an audit row (task #13
// Tier 3, spec §3: "10:5,50:10") — minQty:percent pairs, comma-separated, in
// whatever order tiers is already in (both loadDiscounts and saveDiscounts
// leave it sorted descending by MinQty). An empty/nil set renders "(none)",
// not auditOldNew's "(unset)" — a shop can genuinely have zero discounts
// configured (the admin's own "send 0 to clear" state), which is a real
// value, not "never touched this setting".
func fmtDiscountTiers(tiers []discountTier) string {
	if len(tiers) == 0 {
		return "(none)"
	}
	parts := make([]string, len(tiers))
	for i, t := range tiers {
		parts[i] = itoa(t.MinQty) + ":" + itoa(int(math.Round(t.Frac*100)))
	}
	return strings.Join(parts, ",")
}

// parseDiscountInput parses "minQty:percent" comma/semicolon-separated pairs
// — the setdiscount validator (telegram_bot.py:18967-19001). ok is false on
// ANY malformed pair (not exactly 2 digit-string parts split on ':', a
// minQty<=0, or a percent outside [0,100]) — the caller aborts the WHOLE
// input rather than partially applying it. Empty segments from stray commas
// are silently skipped, matching Python's `if not pair: continue`.
func parseDiscountInput(body string) (tiers []discountTier, ok bool) {
	body = strings.NewReplacer(";", ",").Replace(body)
	for _, pair := range strings.Split(body, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.Split(pair, ":")
		if len(parts) != 2 {
			return nil, false
		}
		qStr, pStr := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !isAllDigits(qStr) || !isAllDigits(pStr) {
			return nil, false
		}
		q, _ := strconv.Atoi(qStr)
		pct, _ := strconv.Atoi(pStr)
		if q <= 0 || pct < 0 || pct > 100 {
			return nil, false
		}
		tiers = append(tiers, discountTier{MinQty: q, Frac: float64(pct) / 100.0})
	}
	return tiers, true
}

// cbSetDiscount shows the current tiers and arms the full-replace text
// prompt — adm:setdiscount (telegram_bot.py:14243-14253). Single-shot: no
// add/remove-individual-rule UI, the admin retypes the WHOLE new set.
func (b *Bot) cbSetDiscount(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cur := loadDiscounts()
	curtxt := "  (none)"
	if len(cur) > 0 {
		lines := make([]string, 0, len(cur))
		for _, t := range cur {
			lines = append(lines, "  • "+itoa(t.MinQty)+"+ → "+itoa(int(math.Round(t.Frac*100)))+"%")
		}
		curtxt = strings.Join(lines, "\n")
	}
	cc.st.mu.Lock()
	cc.st.Discount = true
	cc.st.mu.Unlock()
	text := "📉 Volume discounts (current):\n" + curtxt +
		"\n\nEnter new tiers as  minQty:percent  pairs, comma-separated.\n" +
		"Example:  10:10, 20:20, 50:30\n" +
		"(Send  0  to clear all discounts.)"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capDiscount is the "discount" capture (#34, POP semantics —
// telegram_bot.py:18967-19001): the slot is cleared UNCONDITIONALLY before
// validation, so an invalid submission ABORTS the whole flow instead of
// re-arming it — the admin must retap adm:setdiscount to try again.
// Deliberate asymmetry with capTier/capPrice below, which stay ARMED on
// invalid input (captures.go documents each entry's mode; both behaviours
// are ported faithfully from Python, not accidental).
//
// The slash/label guard runs BEFORE the pop, not after: Python's `del` sits
// INSIDE `if ... and not text.startswith("/") and label_to_cmd(text) is
// None:` (telegram_bot.py:18967), so a slash command or reply-keyboard
// label does NOT disarm the prompt — it falls through untouched for the
// real command/button handler to process, and the volume-discount prompt
// stays armed for the admin's next message. Popping first (as this used to)
// would silently disarm on exactly that input, losing the flow with no
// error shown.
func (b *Bot) capDiscount(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Discount
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false // prompt stays ARMED; let the real command/button handler run
	}
	c.st.mu.Lock()
	c.st.Discount = false
	c.st.mu.Unlock()
	// Task #13 Tier 3: capture the prior tier set BEFORE either branch below
	// overwrites discounts.json, so the audit row's old side reflects what
	// was actually replaced regardless of which branch runs.
	oldTiers := loadDiscounts()
	body := strings.TrimSpace(c.Text)
	if body == "0" {
		// A save failure here (read-only dir, full disk) must never be
		// reported as the "✅ All volume discounts cleared." success text —
		// that would tell the admin discounting stopped while the old
		// tiers stay live and mispricing continues invisibly.
		if err := saveDiscounts(nil); err != nil {
			log.Println("adminpricing: save discounts failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3). A
		// percentage applied to every order at or above a threshold — a bad
		// tier discounts everything.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_discounts", "discounts.json",
			fmtDiscountTiers(oldTiers), fmtDiscountTiers(nil))
		b.send(c.Ctx, c.ChatID, "✅ All volume discounts cleared.", admNav("admcat:pricing"))
		return true
	}
	tiers, ok := parseDiscountInput(body)
	if !ok || len(tiers) == 0 {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Invalid. Use  minQty:percent  pairs, e.g. 10:10, 20:20, 50:30 (or 0 to clear).",
			admNavRetry("admcat:pricing", "📉 Volume discounts", "adm:setdiscount"))
		return true
	}
	// Same rule as the clear-all branch above: a failed write must stop
	// here with an explicit failure reply, never fall through to the
	// "✅ ... updated" success text.
	if err := saveDiscounts(tiers); err != nil { // sorts tiers in place, descending
		log.Println("adminpricing: save discounts failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
		return true
	}
	// Audit AFTER the save succeeds, never before (spec §4.3). tiers is
	// already sorted descending by saveDiscounts above, matching oldTiers'
	// own sort order from loadDiscounts.
	b.auditConfigSet(fmtInt(c.ChatID), "config_set_discounts", "discounts.json",
		fmtDiscountTiers(oldTiers), fmtDiscountTiers(tiers))
	shown := make([]string, 0, len(tiers))
	for _, t := range tiers {
		shown = append(shown, "  • "+itoa(t.MinQty)+"+ → "+itoa(int(math.Round(t.Frac*100)))+"%")
	}
	b.send(c.Ctx, c.ChatID, "✅ Volume discounts updated:\n"+strings.Join(shown, "\n"), admNav("admcat:pricing"))
	return true
}

// ---- 3. adm:promocodes — promo code admin (list/create/delete) -------------

// renderPromoList renders the 🎟️ PROMO CODES list —
// _show_promo_codes (telegram_bot.py:10339-10363). Shared by cbPromoCodes
// (initial open) and cbPromoDel (re-render after a delete), matching
// Python's own reuse.
func (b *Bot) renderPromoList(ctx context.Context, chatID int64) {
	var promos []*store.Promo
	if b.store != nil {
		if ps, err := b.store.ListPromos(); err == nil {
			promos = ps
		}
	}
	body := "🎟️ No promo codes yet."
	if len(promos) > 0 {
		lines := make([]string, 0, len(promos)+1)
		lines = append(lines, "🎟️ PROMO CODES:")
		for _, p := range promos {
			left := p.MaxUses - p.UsedCount
			if left < 0 {
				left = 0
			}
			var flags []string
			if !p.Active {
				flags = append(flags, "OFF")
			}
			if p.Stack {
				flags = append(flags, "stack")
			}
			tag := ""
			if len(flags) > 0 {
				tag = " [" + strings.Join(flags, ", ") + "]"
			}
			lines = append(lines, "• "+p.Code+" — "+itoa(int(math.Round(p.Percent)))+"% — "+
				itoa64(p.UsedCount)+"/"+itoa64(p.MaxUses)+" used ("+itoa64(left)+" left)"+tag)
		}
		body = strings.Join(lines, "\n")
	}
	rows := [][]tg.InlineKeyboardButton{btnRow("➕ New promo code", "adm:promonew")}
	for _, p := range promos {
		rows = append(rows, btnRow("🗑 Delete "+p.Code, cbData("promodel", p.Code)))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:pricing"))
	b.sendMenu(ctx, chatID, body, rows)
}

// cbPromoCodes opens the promo list — adm:promocodes (telegram_bot.py:14254-14256).
func (b *Bot) cbPromoCodes(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	b.renderPromoList(cc.Ctx, cc.ChatID)
}

// cbPromoNew arms the one-line promo-code creation prompt — adm:promonew
// (telegram_bot.py:14257-14268). The CODE itself is never typed — it is
// always auto-generated (genUniqueCode, info.go) on success.
func (b *Bot) cbPromoNew(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.Promo = true
	cc.st.mu.Unlock()
	text := "🎟️ New promo code.\n" +
		"Send:  <percent> <max_uses> <stack on/off>\n" +
		"Example:  20 100 off\n\n" +
		"• percent  = discount %\n" +
		"• max_uses = how many times it can be redeemed\n" +
		"• stack    = 'on' to ADD on top of the volume discount,\n" +
		"             'off' to REPLACE it (default off)"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// promoAuditDesc renders a promo's percent/uses/stack as one compact string
// for an audit row (task #13 Tier 4, spec §3) — shared by capPromo (new) and
// cbPromoDel (deleted) below so both sides of a promo's audit trail use the
// identical format.
func promoAuditDesc(percent float64, maxUses int64, stack bool) string {
	stackStr := "off"
	if stack {
		stackStr = "on"
	}
	return itoa(int(math.Round(percent))) + "% / " + itoa64(maxUses) + " uses / stack=" + stackStr
}

// isStackOn reports whether a typed 3rd word means "stack" — the
// on/stack/yes/true/1 (case-insensitive) recognizer, anything else (or
// absent) defaulting to false (telegram_bot.py:19040-19042).
func isStackOn(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "stack", "yes", "true", "1":
		return true
	}
	return false
}

// capPromo is the "promo" capture (#36, POP semantics —
// telegram_bot.py:19027-19050): popped BEFORE validation, so an invalid
// submission ABORTS (must reopen adm:promonew to retry) — same
// pop-then-validate abort shape as capDiscount above.
//
// Same guard-before-pop ordering as capDiscount: Python's `del` sits INSIDE
// the not-slash-and-not-label check, so a slash command or reply-keyboard
// label leaves the promo prompt ARMED instead of silently disarming it.
func (b *Bot) capPromo(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Promo
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false // prompt stays ARMED; let the real command/button handler run
	}
	c.st.mu.Lock()
	c.st.Promo = false
	c.st.mu.Unlock()
	parts := strings.Fields(c.Text)
	ok := len(parts) >= 2 && isAllDigits(parts[0]) && isAllDigits(parts[1])
	pct, uses := 0, 0
	if ok {
		pct, _ = strconv.Atoi(parts[0])
		uses, _ = strconv.Atoi(parts[1])
	}
	stack := ok && len(parts) >= 3 && isStackOn(parts[2])
	if !ok || pct <= 0 || pct > 95 || uses <= 0 {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Invalid. Send:  <percent> <max_uses> <stack on/off>\n"+
				"e.g. 20 100 off   (percent 1-95, uses ≥ 1).",
			admNavRetry("admcat:pricing", "🎟️ Promo codes", "adm:promocodes"))
		return true
	}
	code := b.genUniqueCode()
	if b.store != nil {
		// H1 stop-on-failure: a CreatePromo error (genuine DB/constraint
		// failure) must never be reported as the "✅ Promo code created:"
		// success text — that would tell the admin a discount is live while
		// no code was inserted. Stop here with the shared save-failed reply,
		// exactly like the SaveSettings sites below.
		if err := b.store.CreatePromo(code, float64(pct), int64(uses), stack); err != nil {
			log.Println("adminpricing: create promo failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Task #13 Tier 4: audit AFTER the create succeeds, never before
		// (spec §4.3). A promo code is always FRESH (genUniqueCode above),
		// so old is always "(unset)" — there is no prior state for a code
		// that didn't exist a moment ago. A promo is a discount bounded by
		// max_uses (spec §2 Tier 4).
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_promo_create", code,
			"", promoAuditDesc(float64(pct), int64(uses), stack))
	}
	mode := "REPLACES the volume discount"
	if stack {
		mode = "stacks ON TOP of the volume discount"
	}
	b.send(c.Ctx, c.ChatID,
		"✅ Promo code created:\n\n🎟️ "+code+"\n"+
			"• "+itoa(pct)+"% off\n• "+itoa(uses)+" use(s)\n• "+mode,
		admNavRetry("admcat:pricing", "🎟️ Promo codes", "adm:promocodes"))
	return true
}

// cbPromoDel deletes a promo code directly, NO confirmation step (mirrors
// tierdo:'s directness) — promodel:<code>, then re-renders the same list.
func (b *Bot) cbPromoDel(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	code := strings.TrimPrefix(cc.Data, "promodel:")
	if b.store != nil {
		// Task #13 Tier 4: look up the promo's live state BEFORE deleting it
		// — once DeletePromo succeeds there is nothing left to read. A
		// lookup miss (stale button, already-deleted code) leaves oldDesc
		// "(unset)" rather than fabricating a description for a promo that
		// no longer existed to begin with.
		oldDesc := ""
		if promos, err := b.store.ListPromos(); err == nil {
			for _, p := range promos {
				if p.Code == code {
					oldDesc = promoAuditDesc(p.Percent, p.MaxUses, p.Stack)
					break
				}
			}
		}
		// H1 stop-on-failure: a DeletePromo error (genuine DB failure, not a
		// missing-code no-op which DeletePromo reports as success) must never
		// fall through to re-rendering the list as if the delete landed. Stop
		// here with the shared save-failed reply instead of renderPromoList,
		// mirroring capPromo's create path above and the SaveSettings sites.
		if err := b.store.DeletePromo(code); err != nil {
			log.Println("adminpricing: delete promo failed:", err)
			b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return
		}
		// Audit AFTER the delete succeeds, never before (spec §4.3) — same
		// post-success audit shape as capPromo's create above.
		b.auditConfigSet(fmtInt(cc.ChatID), "config_set_promo_delete", code, oldDesc, "(deleted)")
	}
	b.renderPromoList(cc.Ctx, cc.ChatID)
}

// ---- 4. API-key pricing model: apiprice/apimin/apimax/apicap ---------------
//
// A flat rate card (not tiers): one USDT-per-100-credits rate, one per-order
// credit floor, one per-order credit ceiling, one per-customer key-count cap.
// Every READ helper (apikeyRateUSDT, apikeyMinCredits, apikeyMaxCredits,
// apikeyMaxPerCustomer, kiroApikeyEnabled) already exists in apikey.go; this
// file only adds the admin WRITE side.

// cbApiPrice arms the bedrock API-key rate wizard — adm:apiprice
// (telegram_bot.py:13961-13983).
//
// Scope note (deliberate, reported to the user): Python's prompt also shows
// an informational "Pool headroom: N credits" line from
// kiro_go_sellable_credits() — internal/kirogo has no pool-listing endpoint
// to back that with (the same gap adminsettings.go's cbSetKiroAcct already
// notes for the accounts picker), so that line is omitted; it never gated
// anything, only informed.
func (b *Bot) cbApiPrice(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetApiPrice = true
	cc.st.mu.Unlock()
	priceLine := "OFF (not for sale)"
	if cur := b.apikeyRateUSDT(saleClassBedrock); cur > 0 {
		priceLine = fmtRate(cur) + " USDT per 100 credits"
	}
	kg := "NOT configured"
	if b.kiroGoConfigured() {
		kg = "configured ✅"
	}
	text := "🔑 API PRICING\nCurrent: " + priceLine + "\nKiro-Go: " + kg + "\n\n" +
		"Send the new USDT price per 100 credits (e.g. 4 or 0.5), or 0 to stop selling API keys:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetApiPrice is the "setapiprice" capture (#11, POP semantics —
// telegram_bot.py:18342-18372). The inline is_super_admin re-check mirrors
// Python's own "defence in depth" comment on this consumption block — a
// silent no-reply return, exactly like Python's bare `return`.
func (b *Bot) capSetApiPrice(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetApiPrice
	c.st.SetApiPrice = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := strings.ReplaceAll(strings.TrimSpace(c.Text), ",", ".")
	rate, err := strconv.ParseFloat(raw, 64)
	// strconv.ParseFloat happily parses "NaN"/"Inf" and both pass a bare
	// `< 0 || > 10000` range check (NaN compares false against everything).
	// Python's `not (0 <= rate <= 10000)` correctly rejects NaN; match that
	// here explicitly so a stray "NaN" can never reach json.Marshal (which
	// errors on NaN) and get silently logged-and-dropped while the admin is
	// told the price was set.
	if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = -1.0
	}
	if rate < 0 || rate > 10000 {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Enter a price in USDT per 100 credits (e.g. 4 or 0.5), or 0 to disable.",
			admNavRetry("admcat:pricing", "🔑 API pricing", "adm:apiprice"))
		return true
	}
	if b.cfg != nil {
		// Task #13 Tier 3: capture the effective pre-change rate BEFORE it is
		// overwritten (apikeyRateUSDT already resolves the settings-vs-legacy-
		// twin fallback, so this is the SAME rate the admin was just shown as
		// "Current" above).
		old := b.apikeyRateUSDT(saleClassBedrock)
		s := b.cfg.Settings()
		s["apikey_usdt_per_100"] = rate
		// Legacy twin kept in lockstep, WRITE-ONLY — never authoritative once
		// apikey_usdt_per_100 is set (apikeyRateUSDT, apikey.go). Not audited
		// separately: it carries no information the primary rate doesn't
		// already convey.
		s["apikey_usdt_per_1k"] = rate * 10
		// H1 stop-on-failure: a SaveSettings error (read-only dir, full disk)
		// must never be reported as the ✅ "API-key price set to ..." success
		// text — that would tell the admin the literal sale price of the SKU
		// changed while the old rate stayed live. Stop here with the shared
		// save-failed reply, exactly like capDiscount's own branches.
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3). The
		// literal sale price of the SKU — an unaudited change under-prices
		// it (loss) or over-prices it (lost sales).
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_apikey_usdt_per_100", "apikey_usdt_per_100",
			fmtRate(old), fmtRate(rate))
	}
	text := "✅ API-key price set to " + fmtRate(rate) + " USDT per 100 credits (" + fmtRate(rate*10) + " per 1000)."
	if rate == 0 {
		text = "✅ API-key selling turned OFF (price 0)."
	}
	if rate > 0 && !b.kiroGoConfigured() {
		text += "\n⚠️ Kiro-Go is not configured — the product stays hidden until it is."
	}
	b.send(c.Ctx, c.ChatID, text, admNav("admcat:pricing"))
	return true
}

// cbApiMin arms the bedrock per-order credit-floor wizard — adm:apimin
// (telegram_bot.py:13996-14007).
func (b *Bot) cbApiMin(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetApiMin = true
	cc.st.mu.Unlock()
	text := "🔽 MIN CREDITS PER API KEY\nCurrent: " + commaInt(int(b.apikeyMinCredits(saleClassBedrock))) +
		"\n(The smallest key a customer may buy. Must stay below the maximum, currently " +
		commaInt(int(b.apikeyMaxCredits(saleClassBedrock))) + ".)\n\n" +
		"Send the new minimum (a whole number of credits), e.g. 100:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetApiMin is the "setapimin" capture (#12, POP semantics —
// telegram_bot.py:18376-18412). Cross-validates against the RAW stored max
// (rawApikeyMaxCredits), not the min-floored apikeyMaxCredits(), so a min
// raise above the real max is correctly rejected rather than silently
// accepted against a max that would auto-float upward with it.
func (b *Bot) capSetApiMin(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetApiMin
	c.st.SetApiMin = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := stripThousandsSeps(c.Text)
	n, err := strconv.Atoi(raw)
	if !isAllDigits(raw) || err != nil || n < 1 {
		b.send(c.Ctx, c.ChatID, "⚠️ Enter a whole number ≥ 1, e.g. 100.",
			admNavRetry("admcat:pricing", "🔽 API min credits", "adm:apimin"))
		return true
	}
	mx := b.rawApikeyMaxCredits(saleClassBedrock)
	if int64(n) > mx {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Minimum "+commaInt(n)+" is above the current maximum "+commaInt(int(mx))+
				" — nothing would be buyable. Raise the maximum first, or pick a smaller minimum.",
			admNavRetry("admcat:pricing", "🔢 API max credits", "adm:apimax"))
		return true
	}
	if b.cfg != nil {
		// Task #13 Tier 4: capture the effective pre-change floor BEFORE it
		// is overwritten.
		old := b.apikeyMinCredits(saleClassBedrock)
		s := b.cfg.Settings()
		s["apikey_min_credits"] = n
		// H1 stop-on-failure: a SaveSettings error must never be reported
		// as the ✅ "Min API credits per order set to ..." success text —
		// the floor is the minimum a customer must spend to buy this SKU,
		// so a silent failure would leave the old floor live while the
		// admin believes they changed it. Stop here with the shared
		// save-failed reply (same shape as capSetApiPrice above).
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3). Bounds
		// exposure per order — raising the floor changes what a customer
		// must spend to buy this SKU at all.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_apikey_min_credits", "apikey_min_credits",
			itoa64(old), itoa(n))
	}
	b.send(c.Ctx, c.ChatID,
		"✅ Min API credits per order set to "+commaInt(n)+".\nBuyable range is now "+
			commaInt(n)+" – "+commaInt(int(mx))+" credits.",
		admNav("admcat:pricing"))
	return true
}

// cbApiMax arms the bedrock per-order credit-ceiling wizard — adm:apimax
// (telegram_bot.py:13984-13995).
func (b *Bot) cbApiMax(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetApiMax = true
	cc.st.mu.Unlock()
	text := "🔢 MAX CREDITS PER API KEY\nCurrent: " + commaInt(int(b.apikeyMaxCredits(saleClassBedrock))) +
		"\n(One purchase mints one key, so this is the biggest key a customer can buy. " +
		"Minimum is " + itoa64(b.apikeyMinCredits(saleClassBedrock)) + ".)\n\n" +
		"Send the new maximum (a whole number of credits), e.g. 20000:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetApiMax is the "setapimax" capture (#13, POP semantics —
// telegram_bot.py:18416-18431). Floors against the LIVE (already-floored)
// apikeyMinCredits — unlike the min setter, a max validator floors against
// the effective min, matching Python's own apikey_min_credits() call here.
func (b *Bot) capSetApiMax(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetApiMax
	c.st.SetApiMax = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := stripThousandsSeps(c.Text)
	n, err := strconv.Atoi(raw)
	minC := b.apikeyMinCredits(saleClassBedrock)
	if !isAllDigits(raw) || err != nil || int64(n) < minC {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Enter a whole number ≥ "+itoa64(minC)+" (the per-order minimum).",
			admNavRetry("admcat:pricing", "🔢 API max credits", "adm:apimax"))
		return true
	}
	if b.cfg != nil {
		// Task #13 Tier 4: capture the effective pre-change ceiling BEFORE
		// it is overwritten.
		old := b.apikeyMaxCredits(saleClassBedrock)
		s := b.cfg.Settings()
		s["apikey_max_credits"] = n
		// H1 stop-on-failure: a SaveSettings error must never be reported
		// as the ✅ "Max API credits per order set to ..." success text —
		// raising the ceiling raises the loss ceiling of any other
		// pricing error, so a silent failure would leave the admin
		// believing the old (lower) cap is gone while it still holds.
		// Stop here with the shared save-failed reply (same shape as
		// capSetApiPrice above).
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3). Raising
		// the ceiling raises the loss ceiling of any other pricing error.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_apikey_max_credits", "apikey_max_credits",
			itoa64(old), itoa(n))
	}
	b.send(c.Ctx, c.ChatID, "✅ Max API credits per order set to "+commaInt(n)+".", admNav("admcat:pricing"))
	return true
}

// cbApiCap arms the per-customer key-count cap wizard — adm:apicap
// (telegram_bot.py:14080-14091). Shared across BOTH SKUs (one cap counts
// every live key regardless of sale_class — apikeyMaxPerCustomer, apikey.go).
func (b *Bot) cbApiCap(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetApiCap = true
	cc.st.mu.Unlock()
	cur := "unlimited"
	if n := b.apikeyMaxPerCustomer(); n > 0 {
		cur = itoa64(n)
	}
	text := "👤 API KEYS PER CUSTOMER\nCurrent: " + cur + "\n\n" +
		"Send the max number of API keys one customer may hold, or 0 for unlimited:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetApiCap is the "setapicap" capture (#17, POP semantics —
// telegram_bot.py:18520-18537). No upper bound — any non-negative whole
// number is accepted, 0 meaning unlimited.
func (b *Bot) capSetApiCap(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetApiCap
	c.st.SetApiCap = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := stripThousandsSeps(c.Text)
	if !isAllDigits(raw) {
		b.send(c.Ctx, c.ChatID, "⚠️ Enter a whole number (0 = unlimited), e.g. 3.",
			admNavRetry("admcat:pricing", "👤 Keys/customer", "adm:apicap"))
		return true
	}
	n, _ := strconv.Atoi(raw)
	if b.cfg != nil {
		// Task #13 Tier 4: capture the effective pre-change cap BEFORE it is
		// overwritten. 0 (unlimited) is a real value here, never "(unset)" —
		// itoa64 never returns an empty string, so auditOldNew's "(unset)"
		// rendering can't misfire on it.
		old := b.apikeyMaxPerCustomer()
		s := b.cfg.Settings()
		s["apikey_max_per_customer"] = n
		// H1 stop-on-failure: a SaveSettings error must never be reported
		// as the ✅ "Max API keys per customer set to ..." success text —
		// raising the cap raises the support surface and the loss ceiling
		// of any other pricing error, so a silent failure would leave the
		// admin believing the old cap is gone while it still holds. Stop
		// here with the shared save-failed reply (same shape as
		// capSetApiPrice above).
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3). Raising
		// the cap raises the loss ceiling of any other pricing error.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_apikey_max_per_customer", "apikey_max_per_customer",
			itoa64(old), itoa(n))
	}
	text := "✅ API keys per customer set to unlimited."
	if n != 0 {
		text = "✅ Max API keys per customer set to " + itoa(n) + "."
	}
	b.send(c.Ctx, c.ChatID, text, admNav("admcat:pricing"))
	return true
}

// ---- 5. Kiro SKU variants: kirotoggle/kiroprice/kiromin/kiromax ------------

// cbKiroToggle flips the Kiro Credits SKU master switch — adm:kirotoggle
// (telegram_bot.py:14008-14035). The warnings are ADVISORY only (never
// block the toggle) — this IS the rollout safety gate: an old,
// pre-saleClass Kiro-Go server would ignore saleClass and mint an unclassed
// key billed at the ~5.7x-higher bedrock rate, so the warning fires every
// time the toggle flips ON while a prerequisite looks unmet.
func (b *Bot) cbKiroToggle(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	if b.cfg == nil {
		b.send(cc.Ctx, cc.ChatID, settingsNotConfigured, admNav("admcat:pricing"))
		return
	}
	s := b.cfg.Settings()
	turningOn := !settingBool(s, "kiro_apikey_enabled", false)
	s["kiro_apikey_enabled"] = turningOn
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminpricing: save failed:", err)
		// settingsNotConfigured ("Settings not configured on this instance")
		// is the WRONG string here: b.cfg is non-nil (checked above) and
		// SETTINGS ARE configured — only the disk WRITE just failed. Sending
		// the "not configured" text misdirects the admin toward re-checking
		// setup when the real fault is a failed save, the exact mix-up every
		// sibling setter in this file avoids by sending settingsSaveFailed
		// instead.
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
		return
	}
	// Task #13 Tier 4: audit AFTER the save succeeds, never before (spec
	// §4.3). Enabling this SKU against a pre-saleClass Kiro-Go server is a
	// ~5.7x giveaway (this function's own doc above) — a money event, so it
	// gets a row like every other Tier 4 toggle.
	b.auditConfigSet(fmtInt(cc.ChatID), "config_toggle_kiro_apikey_enabled", "kiro_apikey_enabled",
		strconv.FormatBool(!turningOn), strconv.FormatBool(turningOn))
	state := "OFF"
	if turningOn {
		state = "ON"
	}
	note := "🟣 Kiro Credits SKU: " + state
	if turningOn {
		if !b.kiroGoConfiguredBase() {
			note += "\n⚠️ Kiro-Go URL/admin key not set — the product stays hidden until they are."
		}
		if b.apikeyRateUSDT(saleClassKiro) <= 0 {
			note += "\n⚠️ Kiro price is 0 — set 🟣 Kiro pricing or the product stays hidden."
		}
		note += "\nℹ️ Only flip this ON after the saleClass-aware Kiro-Go build is deployed AND " +
			"Kiro-credit accounts are tagged, or keys will misroute/misbill."
	}
	b.send(cc.Ctx, cc.ChatID, note, nil)
	// RE-RENDERS the pricing submenu in place — the ONLY handler in this
	// whole category that does so (py_pricing_spec.md §5); every other
	// action here sends a plain confirmation with a Back button instead.
	title, rows, ok := b.adminCategoryMenu(cc.ChatID, "pricing")
	if ok {
		b.sendMenu(cc.Ctx, cc.ChatID, "🔑 "+title, rows)
	}
}

// cbKiroPrice arms the Kiro-credit rate wizard — adm:kiroprice
// (telegram_bot.py:14036-14055).
func (b *Bot) cbKiroPrice(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetKiroPrice = true
	cc.st.mu.Unlock()
	priceLine := "OFF (not for sale)"
	if cur := b.apikeyRateUSDT(saleClassKiro); cur > 0 {
		priceLine = fmtRate(cur) + " USDT per 100 Kiro credits"
	}
	skuState := "OFF"
	if b.kiroApikeyEnabled() {
		skuState = "ON"
	}
	parity := ""
	if bed := b.apikeyRateUSDT(saleClassBedrock); bed > 0 {
		parity = "\nParity guide: bedrock is " + fmtRate(bed) + "/100cr → same token value ≈ " +
			strconv.FormatFloat(bed/5.7, 'f', 3, 64) + "/100 Kiro credits."
	}
	text := "🟣 KIRO CREDITS PRICING\nCurrent: " + priceLine + "\nSKU toggle: " + skuState + parity + "\n\n" +
		"Send the new USDT price per 100 KIRO credits (e.g. 0.7), or 0 to stop selling the Kiro SKU:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetKiroPrice is the "setkiroprice" capture (#14, POP semantics —
// telegram_bot.py:18438-18464). No legacy per-1k twin here — the Kiro SKU
// postdates that migration, so there is no old build to stay in lockstep
// with (unlike capSetApiPrice's apikey_usdt_per_1k mirror).
func (b *Bot) capSetKiroPrice(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetKiroPrice
	c.st.SetKiroPrice = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := strings.ReplaceAll(strings.TrimSpace(c.Text), ",", ".")
	rate, err := strconv.ParseFloat(raw, 64)
	// Same NaN/Inf guard as capSetApiPrice above — a bare range check lets
	// "NaN" slip through since NaN compares false against every bound.
	if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = -1.0
	}
	if rate < 0 || rate > 10000 {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Enter a price in USDT per 100 KIRO credits (e.g. 0.7), or 0 to disable.",
			admNavRetry("admcat:pricing", "🟣 Kiro pricing", "adm:kiroprice"))
		return true
	}
	if b.cfg != nil {
		// Task #13 Tier 3: same effective-rate capture as capSetApiPrice above.
		old := b.apikeyRateUSDT(saleClassKiro)
		s := b.cfg.Settings()
		s["kiro_apikey_usdt_per_100"] = rate
		// H1 stop-on-failure: a SaveSettings error must never be reported
		// as the ✅ "Kiro-credit price set to ..." success text — that
		// would tell the admin the Kiro SKU's literal sale price changed
		// while the old rate stayed live. Stop here with the shared
		// save-failed reply (same shape as capSetApiPrice above).
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3) — same
		// post-success audit shape as capSetApiPrice above.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_kiro_apikey_usdt_per_100", "kiro_apikey_usdt_per_100",
			fmtRate(old), fmtRate(rate))
	}
	text := "✅ Kiro-credit price set to " + fmtRate(rate) + " USDT per 100 Kiro credits."
	if rate == 0 {
		text = "✅ Kiro-credit price turned OFF (price 0)."
	}
	if rate > 0 && !b.kiroApikeyEnabled() {
		text += "\nℹ️ The 🟣 Kiro SKU toggle is still OFF — flip 🟣 Kiro SKU when ready."
	}
	if rate > 0 && !b.kiroGoConfiguredBase() {
		text += "\n⚠️ Kiro-Go is not configured — set the URL and admin key too."
	}
	b.send(c.Ctx, c.ChatID, text, admNav("admcat:pricing"))
	return true
}

// cbKiroMin arms the Kiro-credit per-order floor wizard — adm:kiromin
// (telegram_bot.py:14056-14067).
func (b *Bot) cbKiroMin(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetKiroMin = true
	cc.st.mu.Unlock()
	text := "🟣 KIRO MIN CREDITS PER API KEY\nCurrent: " + commaInt(int(b.apikeyMinCredits(saleClassKiro))) +
		"\n(The smallest Kiro-credit key a customer may buy. Must stay below the maximum, currently " +
		commaInt(int(b.apikeyMaxCredits(saleClassKiro))) + ".)\n\n" +
		"Send the new minimum (a whole number of KIRO credits), e.g. 500:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetKiroMin is the "setkiromin" capture (#15, POP semantics —
// telegram_bot.py:18468-18516's mirror of apimin), floored against the RAW
// Kiro max (rawApikeyMaxCredits(saleClassKiro)) for the same reason
// capSetApiMin uses the raw bedrock max.
func (b *Bot) capSetKiroMin(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetKiroMin
	c.st.SetKiroMin = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := stripThousandsSeps(c.Text)
	n, err := strconv.Atoi(raw)
	if !isAllDigits(raw) || err != nil || n < 1 {
		b.send(c.Ctx, c.ChatID, "⚠️ Enter a whole number ≥ 1, e.g. 500.",
			admNavRetry("admcat:pricing", "🟣 Kiro min credits", "adm:kiromin"))
		return true
	}
	mx := b.rawApikeyMaxCredits(saleClassKiro)
	if int64(n) > mx {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Minimum "+commaInt(n)+" is above the current maximum "+commaInt(int(mx))+
				" — nothing would be buyable. Raise the maximum first, or pick a smaller minimum.",
			admNavRetry("admcat:pricing", "🟣 Kiro max credits", "adm:kiromax"))
		return true
	}
	if b.cfg != nil {
		// Task #13 Tier 4: capture the effective pre-change floor BEFORE it
		// is overwritten.
		old := b.apikeyMinCredits(saleClassKiro)
		s := b.cfg.Settings()
		s["kiro_apikey_min_credits"] = n
		// H1 stop-on-failure: a SaveSettings error must never be reported
		// as the ✅ "Min KIRO credits per order set to ..." success text —
		// the floor is the minimum a customer must spend to buy the Kiro
		// SKU, so a silent failure would leave the old floor live while
		// the admin believes they changed it. Stop here with the shared
		// save-failed reply (same shape as capSetApiPrice above).
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3) — same
		// post-success audit shape as capSetApiPrice above.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_kiro_apikey_min_credits", "kiro_apikey_min_credits",
			itoa64(old), itoa(n))
	}
	b.send(c.Ctx, c.ChatID,
		"✅ Min KIRO credits per order set to "+commaInt(n)+".\nBuyable range is now "+
			commaInt(n)+" – "+commaInt(int(mx))+" credits.",
		admNav("admcat:pricing"))
	return true
}

// cbKiroMax arms the Kiro-credit per-order ceiling wizard — adm:kiromax
// (telegram_bot.py:14068-14079).
func (b *Bot) cbKiroMax(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetKiroMax = true
	cc.st.mu.Unlock()
	text := "🟣 KIRO MAX CREDITS PER API KEY\nCurrent: " + commaInt(int(b.apikeyMaxCredits(saleClassKiro))) +
		"\n(The biggest Kiro-credit key a customer can buy. Minimum is " +
		itoa64(b.apikeyMinCredits(saleClassKiro)) + ".)\n\n" +
		"Send the new maximum (a whole number of KIRO credits), e.g. 100000:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:pricing"))
}

// capSetKiroMax is the "setkiromax" capture (#16, POP semantics —
// telegram_bot.py:18468-18516's mirror of apimax), floored against the LIVE
// (already-floored) apikeyMinCredits(saleClassKiro).
func (b *Bot) capSetKiroMax(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetKiroMax
	c.st.SetKiroMax = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	raw := stripThousandsSeps(c.Text)
	n, err := strconv.Atoi(raw)
	minC := b.apikeyMinCredits(saleClassKiro)
	if !isAllDigits(raw) || err != nil || int64(n) < minC {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Enter a whole number ≥ "+itoa64(minC)+" (the per-order minimum).",
			admNavRetry("admcat:pricing", "🟣 Kiro max credits", "adm:kiromax"))
		return true
	}
	if b.cfg != nil {
		// Task #13 Tier 4: capture the effective pre-change ceiling BEFORE
		// it is overwritten.
		old := b.apikeyMaxCredits(saleClassKiro)
		s := b.cfg.Settings()
		s["kiro_apikey_max_credits"] = n
		// H1 stop-on-failure: a SaveSettings error must never be reported
		// as the ✅ "Max KIRO credits per order set to ..." success text —
		// raising the ceiling raises the loss ceiling of any other pricing
		// error, so a silent failure would leave the admin believing the
		// old (lower) cap is gone while it still holds. Stop here with the
		// shared save-failed reply (same shape as capSetApiPrice above).
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminpricing: save failed:", err)
			b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:pricing"))
			return true
		}
		// Audit AFTER the save succeeds, never before (spec §4.3) — same
		// post-success audit shape as capSetApiPrice above.
		b.auditConfigSet(fmtInt(c.ChatID), "config_set_kiro_apikey_max_credits", "kiro_apikey_max_credits",
			itoa64(old), itoa(n))
	}
	b.send(c.Ctx, c.ChatID, "✅ Max KIRO credits per order set to "+commaInt(n)+".", admNav("admcat:pricing"))
	return true
}
