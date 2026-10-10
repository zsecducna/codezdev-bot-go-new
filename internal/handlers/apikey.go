package handlers

// apikey.go — M2.5 slice, part 1: the API-key product's CONFIG/GATING/PRICING
// layer plus the customer-facing purchase + recharge dialogs.
//
// Ports (telegram_bot.py):
//   - kiro_go_configured @5822 / kiro_go_configured_base @5835 / kiro_go_public_url @5845,
//   - apikey_rate_usdt @6163, kiro_apikey_enabled @6196, apikey_for_sale @6207,
//     apikey_price_vnd @6218, _apikey_coded_total @6237, _apikey_price_for @6272,
//     _apikey_rate_disp_for @6285, _apikey_qty_rows @6292,
//   - apikey_min_credits @6302 / apikey_max_credits @6321 / apikey_max_per_customer @6336,
//   - _apikey_key_class @6347 / _order_sale_class @6357 / _apikey_rc_gate @6373,
//   - _start_apikey_purchase @10417 / _start_apikey_recharge @10471,
//   - the apikey:start / kiroapi:start / apikeyqty: / kiroapiqty: / apikeyrc:start /
//     apikeyrcpick: / apikeyrcqty: callbacks @15405-15570,
//   - the pending_apikey (#45) / pending_apikeyrc (#46) text captures @19058-19103.
//
// Delivery (mint + top-up) lives in apikey_deliver.go, the My-Keys/reveal views
// in apikey_keys.go, and the group refund in apikey_refund.go.

import (
	"context"
	"html"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/llmgate"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// registerAPIKey attaches the M2.5 captures + callback routes. Called once from
// New, AFTER registerCatalog/registerDeposit.
//
// CAPTURE ORDER IS THE CONTRACT (handlers-go.md §2.2): pending_apikey is #45 and
// pending_apikeyrc #46, i.e. after pending_deposit (#44) and before
// pending_purchase (#48) — the customer priority comment at telegram_bot.py:3639
// (code > sell > deposit > apikey > apikeyrc > ordercode > purchase). They are
// therefore INSERTED before the "purchase" stage rather than appended.
//
// Callback routes are appended: none of the apikey prefixes overlap any
// already-registered prefix, so first-match dispatch is order-independent for
// them (their Python positions are @15405-15592 and @12371-12379).
func (b *Bot) registerAPIKey() {
	b.insertCapturesBefore("purchase",
		stage{name: "apikey", fn: (*Bot).capAPIKey},     // #45 @19058 peek
		stage{name: "apikeyrc", fn: (*Bot).capAPIKeyRC}, // #46 @19084 peek
	)
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "apikey:start", exact: true, fn: (*Bot).cbApikeyStart},  // @15405
		cbRoute{key: "kiroapi:start", exact: true, fn: (*Bot).cbApikeyStart}, // @15405
		// The Fixed Pricing SKU's twins. Go-only (the "fixed" sale class
		// postdates telegram_bot.py), so there is no @line to cite — same
		// precedent as captures.go's "provpool".
		cbRoute{key: "fixedapi:start", exact: true, fn: (*Bot).cbApikeyStart},
		cbRoute{key: "apikeyqty:", fn: (*Bot).cbApikeyQty},                      // @15448
		cbRoute{key: "kiroapiqty:", fn: (*Bot).cbApikeyQty},                     // @15448
		cbRoute{key: "fixedapiqty:", fn: (*Bot).cbApikeyQty},                    // (Go-only)
		cbRoute{key: "apikeyrc:start", exact: true, fn: (*Bot).cbApikeyRCStart}, // @15467
		cbRoute{key: "apikeyrcpick:", fn: (*Bot).cbApikeyRCPick},                // @15502
		cbRoute{key: "apikeyrcqty:", fn: (*Bot).cbApikeyRCQty},                  // @15549
		cbRoute{key: "apikeyrev:", fn: (*Bot).cbApikeyRev},                      // @15571
		cbRoute{key: "apikeyrefok:", fn: (*Bot).cbApikeyRefOK},                  // @12371
		cbRoute{key: "apikeyrefno", exact: true, fn: (*Bot).cbApikeyRefNo},      // @12375
		// M2.5's provisional self-service entry points were folded into the M2.6
		// ss:/refreq: routers (selfservice.go): registerCatalog runs first
		// (dispatch.go:396) and its ss:/refreq: PREFIX routes matched before
		// these exact/prefix routes could, and dispatchCB is first-match — so
		// they were unreachable, not merely redundant. Both routes AND their
		// handlers (cbSSMyKeys, cbRefReqAPIKey) are deleted; the behaviour lives
		// on in cbSelfService("mykeys") -> showMyAPIKeys and in
		// cbFileRefundReq -> fileRefundRequest, which routes api-key-family
		// orders to apikeyRefundPreview itself.
	)
}

// insertCapturesBefore splices stages into the capture chain immediately before
// the named stage, keeping the chain an in-order subsequence of
// CanonicalCaptureOrder (handlers-go.md §7: slices land independently, the
// REMAINING entries must still match the golden list). Appends when the anchor
// is not registered yet.
func (b *Bot) insertCapturesBefore(anchor string, stages ...stage) {
	at := len(b.captureChain)
	for i, s := range b.captureChain {
		if s.name == anchor {
			at = i
			break
		}
	}
	out := make([]stage, 0, len(b.captureChain)+len(stages))
	out = append(out, b.captureChain[:at]...)
	out = append(out, stages...)
	out = append(out, b.captureChain[at:]...)
	b.captureChain = out
}

// ---- SKU model -------------------------------------------------------------

// The three Kiro-Go sale classes. Re-exported from internal/kirogo so this file
// reads like the Python source ("bedrock" / "kiro") while the wire values stay
// owned by the client package (telegram_bot.py:3801 _APIKEY_CLASS). "fixed" is
// Go-only — the class postdates telegram_bot.py, so it has no _APIKEY_CLASS
// entry to cite.
const (
	saleClassBedrock = kirogo.SaleClassBedrock
	saleClassKiro    = kirogo.SaleClassKiro
	saleClassFixed   = kirogo.SaleClassFixed
)

// apikeyBindsUpstream reports whether a sale class binds a PER-CUSTOMER
// upstream (llmgate) key at mint time. True only for bedrock and the legacy
// "" of pre-split rows, which billed as bedrock and bound the same way.
//
// Kiro and Fixed both mint against a pooled account that fronts its own
// credentials, so ProvisionCustomer sends skipUpstream:true for them and the
// server answers upstreamStatus:"skipped" — the NORMAL terminal state for
// those classes, not an anomaly.
//
// This exists as ONE predicate because the three sites that need it
// (accountId on the mint payload, the not-bound log line, the owner alert)
// were each written as their own `!= saleClassKiro` literal. That shape is
// how the Fixed SKU shipped broken twice over: every mint failed with
// ErrAccountIDForbiddenForFixed, and every sale that did go through paged the
// owners. Route new classes through here instead of adding a fourth literal.
func apikeyBindsUpstream(saleClass string) bool {
	return saleClass == saleClassBedrock || saleClass == ""
}

// apikeyClassForProduct maps a BUY product sentinel to its Kiro-Go saleClass —
// _APIKEY_CLASS (telegram_bot.py:3801). The recharge sentinel is deliberately
// absent: a top-up's class comes from the TARGET key's stored sale_class, never
// from its own product (see orderSaleClass).
func apikeyClassForProduct(product string) string {
	switch product {
	case kiroApikeyProduct:
		return saleClassKiro
	case fixedApikeyProduct:
		return saleClassFixed
	}
	return saleClassBedrock
}

// isApikeyBuyProduct reports a NEW-KEY (mint) order — _APIKEY_BUY_PRODUCTS
// membership (telegram_bot.py:3798). Recharges are excluded.
func isApikeyBuyProduct(product string) bool {
	return product == apikeyProduct || product == kiroApikeyProduct ||
		product == fixedApikeyProduct
}

// ---- env/settings bounds (telegram_bot.py:3814-3842) -----------------------

// Quick-pick credit buttons on the amount prompts — APIKEY_QTY_CHOICES @3828 /
// KIRO_APIKEY_QTY_CHOICES @3842. Buttons outside the live [min, ceiling] range
// are hidden at render time, so a stale constant can never offer an unsellable
// amount.
var (
	apikeyQtyChoices     = []int64{100, 200, 500, 1000, 2000, 5000, 10000}
	kiroApikeyQtyChoices = []int64{500, 1000, 2000, 5000, 10000, 20000, 50000}
	// Fixed Pricing bills a flat credit price PER REQUEST rather than by
	// rate-card burn, so its credit unit is closest to bedrock's — it reuses
	// the same quick picks rather than kiro's ~5.7x-smaller ladder. Its own
	// slice (not a shared reference) so retuning one SKU can never move the
	// other's buttons.
	fixedApikeyQtyChoices = []int64{100, 200, 500, 1000, 2000, 5000, 10000}
)

// apikeyQtyPerRow is APIKEY_QTY_PER_ROW (telegram_bot.py:3831): seven quick
// picks on one row are unreadably narrow on a phone, so they wrap.
const apikeyQtyPerRow = 4

// settingsNumber coerces a settings value to a float the way Python's
// `float(v or 0)` inside a try/except (TypeError, ValueError) does. ok is false
// when the key is absent, empty, or not coercible — the callers below need that
// distinction to fall back to a legacy key or an env default.
func settingsNumber(s map[string]any, key string) (float64, bool) {
	raw, present := s[key]
	if !present || raw == nil {
		return 0, false
	}
	switch t := raw.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		v := strings.TrimSpace(t)
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// envInt64 reads an int64 env var through config with a fallback (the
// APIKEY_* / KIRO_APIKEY_* module constants at telegram_bot.py:3814-3842 are
// env-backed, so they are read here, not compiled in).
func (b *Bot) envInt64(key string, def int64) int64 {
	return int64(b.envInt(key, int(def)))
}

// The built-in DISPLAY names of the two mint SKUs. Declared as constants (not
// inline literals) because the i18n catalog's default rendering and
// apikeyProductName's fallback must be the SAME text: with an admin override
// absent, the rendered button/prompt/delivery text has to stay byte-identical
// to what shipped before the rename feature existed, and two independent
// literals would eventually drift apart.
//
// These are DISPLAY strings only. They are NEVER the product sentinels
// (__apikey__ / __kiroapi__ / __apikeyrc__) — orders, api_keys rows, suspension
// keys and private-product gating all key off those, so a display rename must
// never reach them.
const (
	apikeyDefaultNameBedrock = "Bedrock API Credits"
	apikeyDefaultNameKiro    = "Kiro Credits API"
	apikeyDefaultNameFixed   = "Fixed Pricing API Credits"
)

// apikeyProductName is the admin-renameable DISPLAY name of one mint SKU —
// settings.json `apikey_names` = {"bedrock": "...", "kiro": "..."} (Go-only:
// the Python original has no rename feature, so there is no line to cite).
// Absent key, absent SKU entry, a wrong-typed value or an empty-after-trim
// value all fall back to the built-in default above.
//
// Legacy (” sale_class) rows resolve to the BEDROCK entry, override included —
// every render site collapses an unrecognised class onto the bedrock template
// (apikeyKeyClass maps only "kiro"/"fixed" to themselves), so resolving the
// name any other way would print the old name on a re-reveal of a legacy key
// while the surrounding template is the renamed bedrock one.
//
// Nil-safe exactly like productSuspended (catalog.go): b.settings() returns nil
// with no config wired (unit tests) and indexing a nil map is legal in Go.
//
// The shape guard (single line, ≤ apikeyNameMaxRunes) is enforced HERE, on the
// read, not only in the admin wizard that writes it: settings.json is also
// hand-editable, and this name is concatenated into inline-button labels and an
// all-caps prompt header, where an embedded newline breaks the layout. One
// guard at the single point every caller routes through beats trusting that
// every writer validated first; a value that fails it falls back to the
// built-in default rather than rendering broken.
func (b *Bot) apikeyProductName(saleClass string) string {
	def := apikeyDefaultNameBedrock
	key := saleClassBedrock
	switch saleClass {
	case saleClassKiro:
		def, key = apikeyDefaultNameKiro, saleClassKiro
	case saleClassFixed:
		def, key = apikeyDefaultNameFixed, saleClassFixed
	}
	names, _ := b.settings()["apikey_names"].(map[string]any)
	if v, ok := names[key]; ok {
		if s := strings.TrimSpace(asStr(v)); apikeyNameOK(s) {
			return s
		}
	}
	return def
}

// apikeyDeliveredTemplate picks the delivery/reveal template for a key's
// effective class AND backend. Shared by the first delivery (deliverApikey)
// and the 👁 re-reveal (cbApikeyRev) so the two can never show a buyer one
// SKU's wording on delivery and another's on a re-read of the same key.
//
// Backend first: a BackendLlmgate row gets DeliveredLlmgate whatever its sale
// class — llmgate keys are minted bedrock-only, and their message carries the
// TWO base URLs ({apibase} + the shop's {base}) the pool templates don't.
// Anything not recognised by class (bedrock, or the legacy "" of pre-split
// rows) keeps the bedrock template, matching apikeyKeyClass's own collapse.
func apikeyDeliveredTemplate(c *i18n.APIKeyCatalog, keyClass, backend string) i18n.Str {
	if backend == store.BackendLlmgate {
		return c.DeliveredLlmgate
	}
	switch keyClass {
	case saleClassKiro:
		return c.KiroDelivered
	case saleClassFixed:
		return c.FixedDelivered
	}
	return c.Delivered
}

// llmgateAPIBaseDisp is the {apibase} substitution for the llmgate delivery /
// reveal template: the PUBLIC completions base buyers paste into their
// tooling, right-trimmed of '/' and HTML-escaped — it lands inside a
// parse_mode=HTML message, and a configured URL with a stray "&" must not
// make Telegram reject the one message the buyer must receive. Distinct from
// LlmgateBaseURL (the control plane the bot logs into) on purpose; see
// config.LlmgateAPIBase. A nil cfg (unit tests with no config wired) falls
// back to llmgate.DefaultBaseURL so the template still renders a sane URL
// instead of a bare "{apibase}" token.
func (b *Bot) llmgateAPIBaseDisp() string {
	u := llmgate.DefaultBaseURL
	if b.cfg != nil {
		u = b.cfg.LlmgateAPIBase()
	}
	return html.EscapeString(strings.TrimRight(u, "/"))
}

// apikeyNameOK is the shared shape check for a SKU display name — non-empty,
// single-line, at most apikeyNameMaxRunes runes. Used by both the read path
// above and the admin capture that writes it (capSetApikeyName,
// adminpricing.go) so the two can never disagree about what is acceptable.
// Runes, not bytes, so a CJK name is not silently cut shorter than a Latin one.
func apikeyNameOK(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\n\r") && len([]rune(s)) <= apikeyNameMaxRunes
}

// apikeyMinCredits is the per-order credit FLOOR for one SKU — apikey_min_credits
// (telegram_bot.py:6302-6318). Admin setting wins, else that SKU's env default,
// never below 1 (a floor of 0 would let a customer order 0 credits, which
// provisioning rejects with a 400 AFTER the money is taken).
//
// Read through this on every buy path; never read the env constants directly,
// or an admin change would apply to some checks and not others.
func (b *Bot) apikeyMinCredits(saleClass string) int64 {
	key, envKey, envDef := "apikey_min_credits", "APIKEY_MIN_CREDITS", int64(100)
	switch saleClass {
	case saleClassKiro:
		key, envKey, envDef = "kiro_apikey_min_credits", "KIRO_APIKEY_MIN_CREDITS", 500
	case saleClassFixed:
		key, envKey, envDef = "fixed_apikey_min_credits", "FIXED_APIKEY_MIN_CREDITS", 100
	}
	v := int64(0)
	if f, ok := settingsNumber(b.settings(), key); ok {
		v = int64(f)
	}
	if v <= 0 {
		v = b.envInt64(envKey, envDef)
	}
	return max(1, v)
}

// apikeyMaxCredits is the per-order credit CEILING for one SKU —
// apikey_max_credits (telegram_bot.py:6321-6333). Never below the live min, so
// a max under the min can never make the sellable range empty.
func (b *Bot) apikeyMaxCredits(saleClass string) int64 {
	key, envKey := "apikey_max_credits", "APIKEY_MAX_CREDITS"
	switch saleClass {
	case saleClassKiro:
		key, envKey = "kiro_apikey_max_credits", "KIRO_APIKEY_MAX_CREDITS"
	case saleClassFixed:
		key, envKey = "fixed_apikey_max_credits", "FIXED_APIKEY_MAX_CREDITS"
	}
	v := int64(0)
	if f, ok := settingsNumber(b.settings(), key); ok {
		v = int64(f)
	}
	if v <= 0 {
		v = b.envInt64(envKey, 1000000)
	}
	return max(b.apikeyMinCredits(saleClass), v)
}

// apikeyMaxPerCustomer is the max number of LIVE keys one customer may hold —
// apikey_max_per_customer (telegram_bot.py:6336-6344). 0 = unlimited. The cap
// is deliberately SHARED across both SKUs (it caps support surface per
// customer, which doesn't depend on which pool backs the key).
func (b *Bot) apikeyMaxPerCustomer() int64 {
	if f, ok := settingsNumber(b.settings(), "apikey_max_per_customer"); ok {
		return max(0, int64(f))
	}
	return max(0, b.envInt64("APIKEY_MAX_PER_CUSTOMER", 0))
}

// apikeyOpusTokensPer100 is APIKEY_OPUS_TOKENS_PER_100 (Kiro commit 3069329):
// the observed Opus 5 token burn per 100 BEDROCK rate-card credits (~80M
// tokens), used only to render the "how many tokens does this actually buy"
// estimate on the bedrock buy screen. It is an OBSERVATION, not a guarantee:
// real burn moves with the model's live rate card and the caller's
// input/output mix, so every string built from it is prefixed with "≈" and
// labelled an estimate (APIKeyCatalog.OpusEst). Env-overridable so the number
// can be re-tuned without a code change. Deliberately NOT applied to the kiro
// SKU — a Kiro credit is a ~5.7x smaller unit, so this constant would
// overstate its token value by that factor (apikeyOpusEstFor only ever prices
// the bedrock class).
func (b *Bot) apikeyOpusTokensPer100() int64 {
	return b.envInt64("APIKEY_OPUS_TOKENS_PER_100", 80000000)
}

// commaFloat1 renders f with thousands separators and one decimal place —
// Python's f"{x:,.1f}" used by _apikey_opus_est to print the tokens-per-USDT
// figure in millions (e.g. "20.0", "1,234.5"). Reuses commaInt's digit
// grouping (adminsettings.go) for the integer part instead of a second
// grouping implementation; only the "split at the decimal point" step is new.
func commaFloat1(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	s := strconv.FormatFloat(f, 'f', 1, 64) // e.g. "1234.5"
	intPart, decPart, _ := strings.Cut(s, ".")
	n, _ := strconv.Atoi(intPart)
	out := commaInt(n) + "." + decPart
	if neg {
		out = "-" + out
	}
	return out
}

// usdtStrCfg has been removed: the cent-rounded USDT quote it computed from a
// VND amount via the USD→VND rate is now exactly what usdtStr (usdt.go) returns
// for a micro-USDT amount. Its single caller (apikeyOpusEst) routes through
// usdtStr directly — see that function's doc comment.

// apikeyOpusEst is the rendered Opus-5 token-value estimate for a BEDROCK
// price of 100 credits — _apikey_opus_est (Kiro commit 3069329). Pure over
// its (microPer100, lang) inputs on purpose (micro-USDT in, string out — no
// chat_id read directly) so the arithmetic and its failure paths are directly
// testable. Given R = the USDT price of 100 credits and T =
// apikeyOpusTokensPer100():
//
//	tokens per 1 USDT   = T / R
//	USDT per 1M tokens  = R / (T / 1e6)
//
// Returns "" — NOT a zero or a placeholder number — whenever the estimate
// can't be trusted: no price, or a price the tokens-per-USDT math rejects.
// An empty block simply drops the lines from the screen (i18n.F leaves an
// absent {opus} substitution as an empty string), which is the correct
// behaviour for a number we can't stand behind; a bogus "0 tokens per USDT"
// on a purchase screen would be worse than silence.
func (b *Bot) apikeyOpusEst(microPer100 int64, lang i18n.Lang) string {
	tokensPer100 := b.apikeyOpusTokensPer100()
	if microPer100 <= 0 || tokensPer100 <= 0 {
		return ""
	}
	// usdtStr renders the cent-rounded USDT price of 100 credits from the
	// stored micro amount. It never returns "" (micro is always valid), so the
	// rate<=0 guard below only fires on a genuinely non-positive price — same
	// net effect as Python's `float(usdt_str(vnd) or 0)` + `if rate <= 0`.
	rate, err := strconv.ParseFloat(b.usdtStr(microPer100), 64)
	if err != nil || rate <= 0 {
		return ""
	}
	tokensPerUSDT := float64(tokensPer100) / rate
	usdtPer1M := rate / (float64(tokensPer100) / 1_000_000.0)
	// Below 100k tokens/USDT the "M" rendering floors to a misleading "0.0M"
	// (needs a ~1,600 USDT per-100-credits price, i.e. a fat-fingered admin
	// rate). Drop the block instead — same rule as every other untrustworthy
	// input here: no estimate beats a wrong one on a purchase screen.
	if tokensPerUSDT < 100_000 {
		return ""
	}
	// Millions is the only readable unit here (raw counts run to 9 digits);
	// the per-1M price uses 4 significant digits (Go's 'g' verb at precision
	// 4, the exact analogue of Python's f"{x:.4g}") so a sub-cent rate — e.g.
	// 0.0125 USDT — is not rounded away to "0.01".
	return i18n.F(i18n.C().APIKey.OpusEst.T(lang),
		"tokens", commaFloat1(tokensPerUSDT/1_000_000.0)+"M",
		"per1m", strconv.FormatFloat(usdtPer1M, 'g', 4, 64))
}

// ---- Kiro-Go configuration gates -------------------------------------------

// kiroGoConfigured is kiro_go_configured (telegram_bot.py:5822-5832): base URL,
// admin key AND account id all present — the gate for the BEDROCK product. The
// account id is part of the gate ON PURPOSE: without it every mint would fail
// AFTER the customer paid and bounce through the auto-refund path. Failing at
// the menu is the cheap failure; failing at the mint is the expensive one.
func (b *Bot) kiroGoConfigured() bool {
	return b.kiroGoConfig().Configured()
}

// kiroGoConfiguredBase is kiro_go_configured_base (telegram_bot.py:5835-5842):
// URL + admin key only — the gate for the KIRO-credit SKU, whose mints send no
// accountId at all (requiring one would hide a fully sellable product behind a
// setting its mints never read).
func (b *Bot) kiroGoConfiguredBase() bool {
	return b.kiroGoConfig().ConfiguredBase()
}

// kiroGoConfig snapshots the LIVE Kiro-Go connection settings (settings.json
// overriding .env, per config.KiroGo*). Read fresh per call so an admin who
// pastes a new key/URL affects the very next gate check, exactly like Python's
// per-call os.environ reads.
func (b *Bot) kiroGoConfig() kirogo.Config {
	if b.cfg == nil {
		return kirogo.Config{}
	}
	return kirogo.Config{
		BaseURL:   strings.TrimSpace(b.cfg.KiroGoBaseURL()),
		AdminKey:  strings.TrimSpace(b.cfg.KiroGoAdminKey()),
		AccountID: strings.TrimSpace(b.cfg.KiroGoAccountID()),
	}
}

// kirogoClient builds a Kiro-Go client from the live config, or nil when the
// base URL / admin key are missing (no admin call can be attempted at all).
// Cheap: *http.Client with a nil Transport reuses http.DefaultTransport's
// shared connection pool, so a per-call client costs no new sockets.
func (b *Bot) kirogoClient() *kirogo.Client {
	cfg := b.kiroGoConfig()
	if !cfg.ConfiguredBase() {
		return nil
	}
	return kirogo.New(cfg, nil)
}

// kiroGoPublicURL is the base URL CUSTOMERS should call, for the delivery
// instructions — kiro_go_public_url (telegram_bot.py:5845-5850). Falls back to
// the admin base URL when no separate public URL is set (they usually match;
// they differ when the bot reaches Kiro-Go via localhost).
func (b *Bot) kiroGoPublicURL() string {
	if b.cfg == nil {
		return ""
	}
	u := strings.TrimSpace(b.cfg.Env("KIRO_GO_PUBLIC_URL"))
	if u == "" {
		u = strings.TrimSpace(b.cfg.KiroGoBaseURL())
	}
	return strings.TrimRight(u, "/")
}

// ---- pricing ---------------------------------------------------------------

// apikeyRateUSDT is the admin-set USDT price per 100 CREDITS of one SKU —
// apikey_rate_usdt (telegram_bot.py:6163-6193). 0 / unset / invalid = that SKU
// DISABLED (buy-menu row hidden, any purchase attempt refused).
//
// LEGACY MIGRATION (bedrock only): pricing used to be quoted per 1000 credits.
// A shop configured before that change still has only `apikey_usdt_per_1k`, so
// it is read as a FALLBACK and divided by 10 — the effective price per credit
// is IDENTICAL and nobody's prices move because the display unit changed. The
// kiro SKU postdates the per-100 change and has no legacy fallback.
func (b *Bot) apikeyRateUSDT(saleClass string) float64 {
	s := b.settings()
	switch saleClass {
	case saleClassKiro:
		f, _ := settingsNumber(s, "kiro_apikey_usdt_per_100")
		return math.Max(0, f)
	case saleClassFixed:
		// Like kiro, this SKU postdates the per-1k -> per-100 migration, so
		// there is no legacy key to fall back to. NOT to be confused with the
		// pool's per-model flat price card (adm:setmodelprice): that is what a
		// REQUEST costs the key in pool credits; this is what the shop charges
		// in USDT for 100 of those credits.
		f, _ := settingsNumber(s, "fixed_apikey_usdt_per_100")
		return math.Max(0, f)
	}
	// Python parity (telegram_bot.py:6180-6188): the legacy per-1k fallback
	// applies ONLY when `apikey_usdt_per_100` is ABSENT, nil or "" — Python's
	// `if v is None or v == ""`. A PRESENT-but-invalid value (a corrupt "abc",
	// a list, …) hits Python's float() ValueError path and returns 0 (SKU
	// DISABLED); it must NOT silently fall through to the legacy key and keep
	// selling at a rate the admin can no longer see or edit.
	if raw, present := s["apikey_usdt_per_100"]; present && raw != nil {
		str, isStr := raw.(string)
		if !isStr || strings.TrimSpace(str) != "" {
			f, ok := settingsNumber(s, "apikey_usdt_per_100")
			if !ok {
				return 0 // present but not coercible -> disabled, never legacy
			}
			return math.Max(0, f)
		}
	}
	legacy, _ := settingsNumber(s, "apikey_usdt_per_1k")
	return math.Max(0, legacy/10.0)
}

// kiroApikeyEnabled is the KIRO-SKU master toggle — kiro_apikey_enabled
// (telegram_bot.py:6196-6204), default OFF. Separate from the price so the
// rollout order is safe: the bot can be priced up while the toggle stays OFF,
// the saleClass-aware Kiro-Go is deployed, THEN the toggle flips. Enabling
// against a pre-saleClass server would mint UNCLASSED keys billed by the
// bedrock rate card — a ~5.7x giveaway.
func (b *Bot) kiroApikeyEnabled() bool {
	v, _ := b.settings()["kiro_apikey_enabled"].(bool)
	return v
}

// fixedApikeyEnabled is the FIXED-SKU master toggle, default OFF — the twin of
// kiroApikeyEnabled (Go-only: the class postdates telegram_bot.py, so there is
// no line to cite). Separate from the price for the same rollout reason: the
// shop can be priced up while the toggle stays OFF until the pool actually has
// enabled fixed-class capacity and a per-model flat price card, and enabling
// it before that would sell keys every request of which the pool refuses.
func (b *Bot) fixedApikeyEnabled() bool {
	v, _ := b.settings()["fixed_apikey_enabled"].(bool)
	return v
}

// apikeyForSale reports whether a SKU is actually sellable — apikey_for_sale
// (telegram_bot.py:6207-6215): a positive admin rate AND a reachable-in-
// principle backend config. The product HIDES rather than selling and
// auto-refunding (repo CLAUDE.md domain invariant).
func (b *Bot) apikeyForSale(saleClass string) bool {
	switch saleClass {
	case saleClassKiro:
		return b.kiroApikeyEnabled() && b.apikeyRateUSDT(saleClassKiro) > 0 && b.kiroGoConfiguredBase()
	case saleClassFixed:
		// BASE config (URL + admin key), never kiroGoConfigured(): a fixed mint
		// sends NO accountId at all (the client REJECTS one —
		// kirogo.ErrAccountIDForbiddenForFixed), so requiring a configured
		// account id here would hide a perfectly sellable product.
		return b.fixedApikeyEnabled() && b.apikeyRateUSDT(saleClassFixed) > 0 && b.kiroGoConfiguredBase()
	}
	// BEDROCK mints DIRECTLY on llmgate since the cutover (deliverApikey's
	// mintLlmgateKey path), so its gate is the llmgate client being configured
	// — NOT kiroGoConfigured(). The pool's account id is irrelevant to a mint
	// that never touches the pool: gating on it would hide a sellable product
	// behind a setting its mints never read (the same reasoning that keeps the
	// kiro/fixed arms on ConfiguredBase). The pool gate stays in place for the
	// kiro/fixed arms above and for OLD pool keys' recharge/stats/revoke paths,
	// which route per-row on api_keys.backend, not on the SKU.
	return b.apikeyRateUSDT(saleClassBedrock) > 0 && b.llmgateClient() != nil
}

// apikeyPriceMicro is the micro-USDT total for `credits` at the SKU's admin
// USDT-per-100 rate — apikey_price_vnd (telegram_bot.py:6218-6226). 0 means
// "not for sale" (rate unset/invalid or non-positive credits).
func (b *Bot) apikeyPriceMicro(credits int64, saleClass string) int64 {
	rate := b.apikeyRateUSDT(saleClass)
	if rate <= 0 || credits <= 0 {
		return 0
	}
	return b.usdtToMicro(float64(credits) * rate / 100.0)
}

// apikeyCodedTotal is (total, effectivePct, volumePct) for an API-key-family
// order at the baseline (NO-code) pricing — kind=None branch of
// _apikey_coded_total (telegram_bot.py:6237-6269).
//
// M2.6 note: this is now a thin wrapper over apikeyCodedTotalFull (code.go),
// which carries the kind/percent/stack branches. ONE implementation means a
// coded and uncoded quote can never drift apart.
func (b *Bot) apikeyCodedTotal(credits int64, resellerPct int, saleClass string) (total int64, effPct, volPct int) {
	return b.apikeyCodedTotalFull(credits, "", 0, false, resellerPct, saleClass)
}

// apikeyPriceFor is the API-key micro-USDT total for `credits` WITH this
// buyer's flat reseller discount — _apikey_price_for
// (telegram_bot.py:6272-6282). The quoted rate and the charged total both
// flow through apikeyCodedTotal so they can never drift apart.
func (b *Bot) apikeyPriceFor(chatID int64, credits int64, saleClass string) int64 {
	total, _, _ := b.apikeyCodedTotal(credits, b.resellerPctFor(chatID), saleClass)
	return total
}

// apikeyRateDisp is the ADVERTISED (undiscounted) per-100-credits price string
// — apikey_rate_disp (telegram_bot.py:6229-6234). Buy-menu / general display.
func (b *Bot) apikeyRateDisp(saleClass string) string {
	return b.usdtDisp(b.apikeyPriceMicro(100, saleClass))
}

// apikeyRateDispFor is the per-100-credits price for THIS buyer (reseller-
// discounted) — _apikey_rate_disp_for (telegram_bot.py:6285-6289). Kept
// consistent with the charged total: both flow through apikeyPriceFor.
func (b *Bot) apikeyRateDispFor(chatID int64, saleClass string) string {
	return b.usdtDisp(b.apikeyPriceFor(chatID, 100, saleClass))
}

// apikeyOpusEstFor is the buyer-specific {opus} block for the bedrock amount
// prompt — _apikey_opus_est_for (Kiro commit 3069329). Prices 100 credits
// through apikeyPriceFor — the SAME path as apikeyRateDispFor's "Price: X /
// 100 credits" line — so a reseller's discount is reflected in the token
// estimate and the two lines can never disagree. Bedrock only by
// construction: cbApikeyStart never calls this for the kiro SKU.
func (b *Bot) apikeyOpusEstFor(chatID int64, lang i18n.Lang) string {
	return b.apikeyOpusEst(b.apikeyPriceFor(chatID, 100, saleClassBedrock), lang)
}

// ---- Fixed Pricing rate card (customer side) --------------------------------

// fixedPricesTimeout bounds the live rate-card fetch behind the Fixed Pricing
// amount prompt. That fetch happens on a CUSTOMER's button tap, and
// internal/kirogo allows 20s per attempt across up to 3 attempts — so an
// unbounded call could leave a customer staring at nothing for a minute.
// Three seconds comfortably covers one healthy round trip to the pool while
// keeping the worst case inside what a person will wait through; the parent
// bound covers the WHOLE call, so the client's internal retries cannot push
// past it.
//
// A var rather than a const purely so a test can shrink it and prove the
// timeout path degrades to an empty block instead of blocking the sale.
var fixedPricesTimeout = 3 * time.Second

// Caps for the customer copy of the rate card. The prompt already carries a
// price line, a minimum, a balance and an explanatory paragraph, so the card
// gets six rows rather than the admin listing's byte-bounded everything.
//
// The byte budget is the belt to that suspenders: a model name comes from the
// pool operator, so a row cap alone is NOT a bound on length, and Telegram
// hard-rejects a message over 4096 characters (b.send does not chunk, so an
// oversized prompt would send NOTHING — the customer could not buy at all).
// The worst case with these caps is comfortably clear of that ceiling: the
// rendered FixedPrompt templates are 275-306 bytes, {name} is capped at
// apikeyNameMaxRunes (60) and the rate/min/balance numbers are tens of bytes,
// so template + fields + 700 lands around 1.3 KB. (Telegram counts UTF-16 code
// units, which for any string are at most its UTF-8 byte count, so bounding
// bytes always bounds the real limit — same reasoning as
// modelPriceListMaxBytes.) That leaves well over the ~40 bytes by which
// appendModelPriceRows' suffix may exceed the budget, as its doc requires.
const (
	fixedPriceCardMaxRows  = 6
	fixedPriceCardMaxBytes = 700
)

// apikeyFixedPriceCard renders the live per-request rate card shown on the
// Fixed Pricing amount prompt, so a customer can see which models cost how
// many credits per request BEFORE typing an amount. Returns "" when the card
// cannot be had.
//
// Degrades SILENTLY on every failure — Kiro-Go unconfigured, unreachable,
// slow, answering nil, or carrying no flat prices at all. The card is a
// nice-to-have on the prompt and must never become a gate in front of the
// sale: a customer has to be able to buy even when the pool is down. Failures
// are logged server-side only; no upstream error text ever reaches a chat.
//
// ponytail: no caching — each Fixed Pricing tap costs one admin-API round
// trip. If product taps ever get heavy enough for that to matter, a short TTL
// cache around ModelPrices is the upgrade path. There is no evidence one is
// needed today, so none is built.
func (b *Bot) apikeyFixedPriceCard(ctx context.Context) string {
	client := b.kirogoClient()
	if client == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, fixedPricesTimeout)
	defer cancel()
	res, err := client.ModelPrices(ctx)
	if err != nil {
		log.Println("apikey: fixed price card unavailable:", err)
		return ""
	}
	// An empty list is a normal state (no model carries a flat price yet), not
	// an error — same reading as cbModelPrices makes of it. Either way the
	// prompt simply goes out without the block.
	if res == nil || len(res.Prices) == 0 {
		return ""
	}
	// The prompt goes out through b.send, which sets no parse_mode at all
	// (command.go) — PLAIN TEXT, unlike the delivery path's sendHTML. So the
	// pool operator's model names need no escaping and are written verbatim.
	// If this prompt is ever switched to HTML, these names must be escaped the
	// way apikey_deliver.go escapes {name}.
	//
	// The leading newline makes the card its own paragraph under the template's
	// explanatory text (each row then supplies its own "\n"). It is written
	// into the builder rather than prepended afterwards so it counts against
	// the byte budget like everything else.
	var sb strings.Builder
	sb.WriteString("\n")
	// Zero rows written means not even the first one fit the budget (one model
	// name long enough to blow 700 bytes on its own). Rendering the bare
	// "… and N more (not shown)." that the helper then appends would advertise
	// a list the customer was never shown — drop the whole block instead.
	if appendModelPriceRows(&sb, res.Prices, fixedPriceCardMaxRows, fixedPriceCardMaxBytes) == 0 {
		return ""
	}
	return sb.String()
}

// apikeyQtyRows wraps quick-pick credit buttons into rows of apikeyQtyPerRow —
// _apikey_qty_rows (telegram_bot.py:6292-6299). Labelled "<n> cr": a bare
// number sits directly under a rate line reading "4.00 USDT / 100 credits",
// where it is easy to misread as a price.
func apikeyQtyRows(choices []int64, cbPrefix string) [][]tg.InlineKeyboardButton {
	var rows [][]tg.InlineKeyboardButton
	for i := 0; i < len(choices); i += apikeyQtyPerRow {
		end := min(i+apikeyQtyPerRow, len(choices))
		row := make([]tg.InlineKeyboardButton, 0, end-i)
		for _, n := range choices[i:end] {
			row = append(row, tg.InlineKeyboardButton{
				Text:         itoa64(n) + " cr",
				CallbackData: cbData(cbPrefix, itoa64(n)),
			})
		}
		rows = append(rows, row)
	}
	return rows
}

// qtyChoicesFor returns the SKU's quick-pick list filtered to [lo, hi].
func qtyChoicesFor(saleClass string, lo, hi int64) []int64 {
	src := apikeyQtyChoices
	switch saleClass {
	case saleClassKiro:
		src = kiroApikeyQtyChoices
	case saleClassFixed:
		src = fixedApikeyQtyChoices
	}
	var out []int64
	for _, n := range src {
		if n >= lo && n <= hi {
			out = append(out, n)
		}
	}
	return out
}

// ---- key-class resolution ---------------------------------------------------

// apikeyKeyClass is the effective SKU of a stored api_keys row for pricing/
// labels/bounds — _apikey_key_class (telegram_bot.py:6347-6354). "kiro" or
// "fixed" only when the row was minted as that class; anything else (bedrock,
// or the legacy "" of pre-split rows) prices and labels as bedrock, because
// legacy keys WERE sold at the bedrock rate card.
//
// Recharge PAYLOADS must NOT use this: they read the RAW stored value so a
// legacy "" row omits saleClass entirely (see deliverApikeyRecharge).
func apikeyKeyClass(row *store.APIKey) string {
	if row != nil {
		switch row.SaleClass {
		case saleClassKiro:
			return saleClassKiro
		case saleClassFixed:
			return saleClassFixed
		}
	}
	return saleClassBedrock
}

// orderSaleClass is the SKU of any API-key-family order, for pricing/labels —
// _order_sale_class (telegram_bot.py:6357-6370). A buy order carries it in its
// product sentinel (all three mint SKUs, via apikeyClassForProduct); a
// RECHARGE order inherits the TARGET key's class via the
// order→key binding. Unknown/broken bindings fall back to bedrock, matching
// every legacy row's real billing.
func (b *Bot) orderSaleClass(o *store.Order) string {
	if o == nil {
		return saleClassBedrock
	}
	if isApikeyBuyProduct(o.Product) {
		return apikeyClassForProduct(o.Product)
	}
	if o.Product == apikeyRechargeProduct {
		rec, err := b.store.APIKeyRechargeForOrder(o.Des)
		if err != nil || rec == nil {
			return saleClassBedrock
		}
		row, err := b.store.APIKeyForOrder(rec.TargetOrderID)
		if err != nil {
			return saleClassBedrock
		}
		return apikeyKeyClass(row)
	}
	return saleClassBedrock
}

// ---- live Kiro-Go stats -----------------------------------------------------

// statsTimeout bounds the customer/admin-facing DISPLAY calls that read live
// key STATS from Kiro-Go. Python passed timeout=5, attempts=1 on these paths so
// a hung proxy degrades the display instead of pinning one of the 16 shared
// workers (telegram_bot.py:5916-5918, 4509-4511). internal/kirogo's retry
// budget is fixed at 3 attempts, so the equivalent bound here is a context
// deadline: it aborts the retry sleep (sleepOrDone) and the in-flight request
// together.
//
// It is not the only such bound: fixedPricesTimeout applies the same technique,
// with a tighter 3s, to the rate-card fetch on the Fixed Pricing amount prompt.
// That one is deliberately shorter because it sits on a plain button tap with
// nothing yet on screen, whereas a stats read backs a screen the customer has
// already committed to. Any new display call needs a bound of its own — pick
// one of these two rather than adding a third number.
const statsTimeout = 6 * time.Second

// keyLiveStats — the backend-agnostic live view of one sold key: both pool
// (kirogo /admin/stats) and llmgate (KeyStats with the customer's own key)
// collapse into this. status: "active"|"exhausted"|"disabled"; unlimited:
// credit side is unlimited (render ∞, refuse recharges); remaining may be
// negative (overspend) — do not clamp; used mirrors the backend's meter.
type keyLiveStats struct {
	Status    string
	Remaining float64
	Used      float64
	Unlimited bool
	SaleClass string // pool rows only (legacy '' display fallback); "" for llmgate
}

// apikeyLiveStats returns the live per-key stats for a minted key on WHATEVER
// backend its row names (api_keys.backend), or nil when the call fails / the
// key can't be found — _apikey_live_stats (telegram_bot.py:4498-4516), made
// backend-routing by the llmgate cutover. Callers MUST degrade gracefully.
//
// Backend split: a BackendLlmgate row reads GET /api/v1/apikey/stats
// authenticated AS the customer's own sk-llmgate-… key (the KEY realm — a 401
// means the key itself is bad, so internal/llmgate never re-login-retries it);
// every other row is a pool key read through the batched /admin/stats shape as
// before. Both map into keyLiveStats so no caller needs to know which gateway
// answered.
//
// This used to carry a note claiming the opposite of the truth: that the
// deployed server sends the documented "keys" field, so dropping Python's
// acceptance of the original deploy's "apiKeys" spelling was safe. The
// deployed server sends "apiKeys". The doc (KIRO_GO_API.md:111-124) says
// "keys", the code matched the doc, and every key read came back empty --
// which is what the comment was asserting could not happen. internal/kirogo
// now decodes BOTH, preferring "keys", exactly as Python does
// (telegram_bot.py:4346-4349, 4530-4545).
//
// Worth knowing when this returns nil: it does so SILENTLY on zero keys --
// only a transport error is logged. A server that answers 200 with nothing
// the decoder recognises therefore leaves no trace at all, and the customer
// simply sees "couldn't check this key's credits". Do not read an empty log
// as evidence the call was never made.
func (b *Bot) apikeyLiveStats(ctx context.Context, row *store.APIKey) *keyLiveStats {
	if row == nil {
		return nil
	}
	if row.Backend == store.BackendLlmgate {
		u := b.llmgateKeyUsage(ctx, row)
		if u == nil {
			return nil
		}
		remaining, limited := u.Remaining()
		return &keyLiveStats{
			Status:    u.Status(),
			Remaining: remaining,
			Used:      u.Used(),
			Unlimited: !limited,
			// SaleClass stays "": llmgate keys carry no server-side sale class,
			// and every llmgate row was minted bedrock anyway (the only class
			// the cutover moved).
		}
	}
	// Pool arm: the same row→union mapping the My-Keys batch path uses
	// (poolStatToLive, apikey_keys.go), so a single mapping defines what a
	// pool KeyStat means in the backend-agnostic shape.
	return poolStatToLive(b.poolKeyLiveStats(ctx, row))
}

// llmgateKeyUsage reads one llmgate key's record + usage through the KEY realm
// (customer's own key as Bearer), bounded by statsTimeout like every other
// display-path stats call. nil on ANY failure, logging the ORDER id only —
// never the key (invariant 2; the llmgate client itself never logs the token
// either). Split out of apikeyLiveStats because the recharge path needs the
// RAW KeyUsage (Info.Name / Info.CreditLimit) for its read-modify-write, not
// the collapsed display shape.
func (b *Bot) llmgateKeyUsage(ctx context.Context, row *store.APIKey) *llmgate.KeyUsage {
	client := b.llmgateClient()
	if client == nil || row == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	u, err := client.KeyStats(cctx, row.APIKey)
	if err != nil {
		log.Println("llmgate stats failed for order", row.OrderID, ":", err)
		return nil
	}
	return u
}

// poolKeyLiveStats is the pre-cutover body of apikeyLiveStats: one pool key's
// live row off kirogo /admin/stats, matched by key id with a fallback to the
// deterministic mint name for legacy rows stored without an id. nil when the
// pool is unconfigured, the call fails, or the key isn't in the answer.
func (b *Bot) poolKeyLiveStats(ctx context.Context, row *store.APIKey) *kirogo.KeyStat {
	client := b.kirogoClient()
	if client == nil {
		return nil
	}
	req := kirogo.StatsRequest{}
	if row.KeyID != "" {
		req.ID = row.KeyID
	} else {
		req.APIKey = row.APIKey
	}
	cctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	res, err := client.Stats(cctx, req)
	if err != nil {
		// Never log the response body or the key — only the order id (invariant 2).
		log.Println("kiro-go stats failed for order", row.OrderID, ":", err)
		return nil
	}
	if len(res.Keys) == 0 {
		return nil
	}
	return &res.Keys[0]
}

// apikeyRemainingCredits is the live creditsRemaining for a minted key, or
// (0, false) when unreadable — _apikey_remaining_credits
// (telegram_bot.py:4519-4528). Refund maths falls back to the deliberate
// force-full prompt on false; never guess a payout from stale data.
//
// UNLIMITED keys report the pool-shaped -1 sentinel: the refund maths reading
// this (< 0 = unlimited = treat as fully-unused, store.py semantics) predates
// the cutover and is shared by both backends, so an llmgate unlimited key
// (whose Remaining() is (0,false) — zero reads as "nothing left" to that
// maths) must be translated here or a full refund would silently pro-rate to
// nothing.
func (b *Bot) apikeyRemainingCredits(ctx context.Context, row *store.APIKey) (float64, bool) {
	st := b.apikeyLiveStats(ctx, row)
	if st == nil {
		return 0, false
	}
	// Unlimited reports the pool-shaped -1 sentinel so the shared refund maths
	// (< 0 = unlimited = fully-unused) keeps meaning what it always meant.
	if st.Unlimited {
		return -1, true
	}
	// A LIMITED key's negative remaining is OVERSPEND, not the -1 sentinel —
	// llmgate returns CreditLimit − Used unclamped, so a burnt-past-the-limit
	// key reads e.g. -2.5. Passed through raw it would fall into the refund
	// maths' unlimited branch and pay a FULL refund for a fully-used key
	// (money out for credits already spent). Clamp to 0: an overspent key has
	// nothing left to return. keyLiveStats.Remaining itself stays unclamped —
	// the RC gate's headroom arithmetic needs the true figure.
	if st.Remaining < 0 {
		return 0, true
	}
	return st.Remaining, true
}

// ---- recharge eligibility gate ---------------------------------------------

// rcGate is the result of apikeyRCGate — the shared eligibility verdict for a
// recharge, re-run at EVERY step (picker tap, typed amount, order creation) so
// a stale prompt or forged callback can never bypass a check.
type rcGate struct {
	Row       *store.APIKey // nil on "gone"
	Remaining float64       // live creditsRemaining, clamped >= 0
	Headroom  int64         // credits still addable under the per-key cap
	// Err is "" when rechargeable, else one of:
	//   "gone"      unknown order / not this chat's key / already refunded,
	//   "stats"     live stats unreadable -> FAIL CLOSED (the cap can't be
	//               verified without the live remaining balance; unlike the
	//               mint-time auto-refund, unknown here must BLOCK the sale),
	//   "disabled"  key manually disabled — a recharge would RE-ENABLE it
	//               server-side, so it is refused ("exhausted" keys pass:
	//               re-enabling those is the whole point),
	//   "unlimited" creditsRemaining == -1 (Kiro-Go 400s credit top-ups on
	//               unlimited keys),
	//   "full"      headroom below the minimum order size.
	Err string
}

// apikeyRCGate ports _apikey_rc_gate (telegram_bot.py:6373-6428).
func (b *Bot) apikeyRCGate(ctx context.Context, chatID int64, targetDes string) rcGate {
	row, err := b.store.APIKeyForOrder(targetDes)
	if err != nil {
		log.Println("api_key_for_order failed:", err)
		return rcGate{Err: "gone"}
	}
	// Ownership + liveness: only the buyer's own, non-refunded key. A refunded
	// key was revoked on Kiro-Go — recharging it would resurrect a paid-back key.
	if row == nil || row.ChatID != chatID || row.RefundedTS != nil {
		return rcGate{Err: "gone"}
	}
	st := b.apikeyLiveStats(ctx, row)
	if st == nil {
		return rcGate{Row: row, Err: "stats"}
	}
	// ONLY the tri-state "status" distinguishes a MANUAL disable from plain
	// exhaustion — an exhausted key also has enabled=false server-side, and
	// exhausted keys MUST stay rechargeable, so never gate on the bare
	// `enabled` flag here. Backend-agnostic since the cutover: keyLiveStats
	// carries the same tri-state for pool and llmgate rows alike (llmgate's
	// Status() derives it from IsActive + remaining the same way).
	if st.Status == "disabled" {
		return rcGate{Row: row, Err: "disabled"}
	}
	if st.Unlimited {
		// Pool: creditsRemaining -1. llmgate: credit_limit 0. Both backends
		// reject a credit top-up on an unlimited key, so refuse identically.
		return rcGate{Row: row, Err: "unlimited"}
	}
	remaining := st.Remaining
	// Per-key cap: total UNUSED credits after the top-up must stay within the
	// per-order/per-key ceiling, so the sellable headroom is
	// cap − remaining − any IN-FLIGHT (reserved/paid, not yet applied) recharge
	// credits for this same key: live creditsRemaining can't see those yet, and
	// two overlapping checkouts must not jointly breach the cap. Bounds are the
	// KEY'S OWN SKU's (a kiro key's headroom measured against the bedrock cap
	// would be off by ~5.7x in either direction).
	cls := apikeyKeyClass(row)
	inflight, err := b.store.PendingRechargeCreditsForTarget(targetDes)
	if err != nil {
		log.Println("pending_recharge_credits_for_target failed:", err)
		return rcGate{Row: row, Remaining: remaining, Err: "stats"}
	}
	headroom := int64(float64(b.apikeyMaxCredits(cls)) - remaining - float64(inflight))
	if headroom < b.apikeyMinCredits(cls) {
		return rcGate{Row: row, Remaining: remaining, Headroom: max(0, headroom), Err: "full"}
	}
	return rcGate{Row: row, Remaining: remaining, Headroom: headroom}
}

// rcGateMessage maps a gate error to its customer-facing notice, so the SAME
// explanation is shown at picker tap, typed amount and order creation
// (telegram_bot.py:15520-15528 and 10507-10526).
func (b *Bot) rcGateMessage(g rcGate, lang i18n.Lang, cls string) string {
	c := &i18n.C().APIKey
	switch g.Err {
	case "gone":
		return c.RCNone.T(lang)
	case "stats":
		return c.RCStatsNA.T(lang)
	case "disabled", "unlimited":
		// Both are permanent "this key can't take a credit top-up" states
		// (a manual disable would be re-enabled by a recharge; an unlimited key
		// is rejected by Kiro-Go with a 400) — one customer-facing message.
		return c.RCDisabled.T(lang)
	case "full":
		// Round, don't truncate: g.Remaining is fed straight from
		// keyLiveStats.Remaining, which is fractional on live keys of BOTH
		// backends — int64(...) would floor 1999.95 down to 1999 in this
		// notice. It is deliberately NOT clamped at 0 first: an overspent
		// llmgate key reports a NEGATIVE remaining, and headroom computed off
		// it is still the honest cap − remaining arithmetic.
		return i18n.F(c.RCFull.T(lang), "remaining", itoa64(roundCredits(g.Remaining)),
			"cap", itoa64(b.apikeyMaxCredits(cls)))
	}
	return ""
}

// ---- order creation ---------------------------------------------------------

// startApikeyPurchase creates a reserved order for `credits` of one API SKU and
// shows the payment screen — _start_apikey_purchase (telegram_bot.py:10417-10468).
// The API-key twin of finalizePurchase: same gates, same order pipeline, but NO
// stock.Reserve (the "stock" is the Kiro-Go pool, enforced for real at mint time).
//
// Order shape: product = the SKU sentinel (the product IS the SKU record on the
// order), tier = "API" (fixed marker, warranty 0), qty = CREDITS, total = VND
// at that SKU's admin per-100 rate. Settlement routes through settlePaid ->
// deliverPaid -> deliverApikey, which reads the class back off the product.
//
// NO pool-headroom gate by design (telegram_bot.py:10454-10461): selling is
// bounded by the ADMIN caps that run below — per-order max credits and the
// per-customer key cap — not by Kiro-Go's reported pool figure, which goes
// negative whenever the shop sells ahead of restocking (a normal trading
// position). Gating on it silently froze ALL sales. A mint the pool truly
// cannot back still fails safe at mint time and auto-refunds the buyer.
func (b *Bot) startApikeyPurchase(ctx context.Context, chatID int64, lang i18n.Lang, credits int64, saleClass string) {
	c := &i18n.C().APIKey
	// Same hard backstop as finalizePurchase: no NEW order while sales are
	// paused, even from a stale prompt/button.
	if b.salesBlocked(ctx, chatID, lang, nil) {
		return
	}
	// Rate/config re-checked at ORDER time (the admin may have zeroed the price,
	// turned the SKU off, or wiped the Kiro-Go key since the prompt was shown).
	total := b.apikeyPriceFor(chatID, credits, saleClass) // reseller flat discount applied
	if !b.apikeyForSale(saleClass) || total <= 0 {
		b.send(ctx, chatID, c.Off.T(lang), nil)
		return
	}
	// Per-order credit bounds re-checked here so a stale prompt / forged
	// callback can't exceed the current cap.
	if credits < b.apikeyMinCredits(saleClass) || credits > b.apikeyMaxCredits(saleClass) {
		b.send(ctx, chatID, i18n.F(c.BadAmount.T(lang),
			"min", itoa64(b.apikeyMinCredits(saleClass)),
			"max", itoa64(b.apikeyMaxCredits(saleClass))), nil)
		return
	}
	// Per-customer key cap (0 = unlimited). Checked before the per-order cap in
	// Python's ordering so a capped customer gets the right message.
	if cap := b.apikeyMaxPerCustomer(); cap > 0 {
		n, err := b.store.CountAPIKeysForChat(chatID)
		if err != nil {
			log.Println("count_api_keys_for_chat failed:", err)
			return
		}
		if n >= cap {
			b.send(ctx, chatID, i18n.F(c.CustCap.T(lang), "cap", itoa64(cap)), nil)
			return
		}
	}
	des := store.NewDes()
	// The order's product sentinel IS its SKU record — deliverApikey reads the
	// mint class straight back off it. Picking the wrong one here would settle
	// a paid fixed order as a bedrock mint.
	product := apikeyProduct
	switch saleClass {
	case saleClassKiro:
		product = kiroApikeyProduct
	case saleClassFixed:
		product = fixedApikeyProduct
	}
	if err := b.store.CreateOrder(des, fmtInt(chatID), string(lang), product, "API",
		credits, total); err != nil {
		log.Println("create_order (apikey) failed:", err)
		return
	}
	b.spawnOrderExpiry(des)
	b.sendPayScreen(ctx, chatID, des, "")
}

// startApikeyRecharge creates a reserved RECHARGE order — _start_apikey_recharge
// (telegram_bot.py:10471-10545). The recharge twin of startApikeyPurchase: same
// order pipeline, but the per-CUSTOMER cap does NOT apply (no new key is
// created) — the per-KEY cap is enforced instead via apikeyRCGate's live
// headroom, which FAILS CLOSED when stats are unreadable (unlike a mint
// failure, which auto-refunds: Kiro-Go happily adds past the cap, so an
// unverifiable cap must block the sale).
func (b *Bot) startApikeyRecharge(ctx context.Context, chatID int64, lang i18n.Lang, targetDes string, credits int64) {
	c := &i18n.C().APIKey
	if b.salesBlocked(ctx, chatID, lang, nil) {
		return
	}
	// The TARGET key's SKU drives every rate/bound below. Read it BEFORE the
	// full gate: pricing must never mix classes even for one message render.
	// (Ownership is enforced by apikeyRCGate; a forged target_des at worst reads
	// a class here and then dies on "gone".)
	targetRow, _ := b.store.APIKeyForOrder(targetDes)
	cls := apikeyKeyClass(targetRow)
	total := b.apikeyPriceFor(chatID, credits, cls) // reseller flat discount applied
	if !b.apikeyForSale(cls) || total <= 0 {
		b.send(ctx, chatID, c.Off.T(lang), nil)
		return
	}
	// Absolute bounds first — a forged callback can't smuggle a 1-credit or
	// 10M-credit order past the prompt.
	if credits < b.apikeyMinCredits(cls) || credits > b.apikeyMaxCredits(cls) {
		b.send(ctx, chatID, i18n.F(c.BadAmount.T(lang),
			"min", itoa64(b.apikeyMinCredits(cls)), "max", itoa64(b.apikeyMaxCredits(cls))), nil)
		return
	}
	// Full eligibility re-check at ORDER time (ownership, live stats,
	// disabled/unlimited key, per-key cap headroom).
	g := b.apikeyRCGate(ctx, chatID, targetDes)
	if g.Err != "" {
		b.send(ctx, chatID, b.rcGateMessage(g, lang, cls), nil)
		return
	}
	// Per-key cap: unused credits after the top-up must not exceed the cap.
	if credits > g.Headroom {
		b.send(ctx, chatID, i18n.F(c.RCTooBig.T(lang), "headroom", itoa64(g.Headroom)), nil)
		return
	}
	des := store.NewDes()
	// Bind order -> target key BEFORE creating the order row: if the process
	// dies between the two, an orphan binding for a des that never became an
	// order is harmless, whereas a PAID order with NO binding could only be
	// auto-refunded. Both rows must exist before the pay screen is shown.
	if _, err := b.store.AddAPIKeyRecharge(des, targetDes, chatID, float64(credits)); err != nil {
		log.Println("add_apikey_recharge failed:", err)
		return
	}
	if err := b.store.CreateOrder(des, fmtInt(chatID), string(lang), apikeyRechargeProduct,
		"API", credits, total); err != nil {
		log.Println("create_order (apikey recharge) failed:", err)
		return
	}
	b.spawnOrderExpiry(des)
	b.sendPayScreen(ctx, chatID, des, "")
}

// ---- callbacks --------------------------------------------------------------

// cbApikeyStart handles apikey:start / kiroapi:start — the virtual API-key
// product tapped on the buy menu (telegram_bot.py:15405-15447). Same
// order-creating gates as buy:/buygo: (sales pause + the confirm-first
// pending-order gate). Every gate/bound/string below is the TAPPED SKU's own.
func (b *Bot) cbApikeyStart(cc *cbCtx) {
	c := &i18n.C().APIKey
	cls := saleClassBedrock
	switch cc.Data {
	case "kiroapi:start":
		cls = saleClassKiro
	case "fixedapi:start":
		cls = saleClassFixed
	}
	lang := b.langFor(cc.ChatID, "en")
	if b.salesBlocked(cc.Ctx, cc.ChatID, lang, nil) {
		return
	}
	if b.pendingGuard(cc.Ctx, cc.ChatID, lang, "buy") {
		return
	}
	if !b.apikeyForSale(cls) { // stale menu / admin turned it off
		b.send(cc.Ctx, cc.ChatID, c.Off.T(lang), nil)
		return
	}
	// Per-customer key cap: a capped customer can't even open the amount
	// prompt. Counts keys of BOTH SKUs — the cap is per customer, not per SKU.
	if cap := b.apikeyMaxPerCustomer(); cap > 0 {
		n, err := b.store.CountAPIKeysForChat(cc.ChatID)
		if err != nil {
			log.Println("count_api_keys_for_chat failed:", err)
			return
		}
		if n >= cap {
			b.send(cc.Ctx, cc.ChatID, i18n.F(c.CustCap.T(lang), "cap", itoa64(cap)), nil)
			return
		}
	}
	// Arm the typed-amount capture FOR THIS SKU; drop stale sibling captures
	// first so an abandoned deposit/code prompt can't eat the number (mirrors
	// tier:). The stored class keeps the typed amount and the quick buttons
	// validating against the right bounds.
	cc.st.ClearCustomerPending()
	cc.st.mu.Lock()
	cc.st.APIKey = &APIKeyCtx{SaleClass: cls}
	cc.st.mu.Unlock()

	mn, mx := b.apikeyMinCredits(cls), b.apikeyMaxCredits(cls)
	qtyPrefix, prompt := "apikeyqty", c.Prompt
	switch cls {
	case saleClassKiro:
		qtyPrefix, prompt = "kiroapiqty", c.KiroPrompt
	case saleClassFixed:
		qtyPrefix, prompt = "fixedapiqty", c.FixedPrompt
	}
	var markup *tg.InlineKeyboardMarkup
	if choices := qtyChoicesFor(cls, mn, mx); len(choices) > 0 {
		markup = &tg.InlineKeyboardMarkup{InlineKeyboard: apikeyQtyRows(choices, qtyPrefix)}
	}
	balance, _ := b.getBalance(cc.ChatID)
	// "opus" is always passed but only c.Prompt (bedrock) carries the {opus}
	// placeholder — i18n.F is a strings.Replacer pass, so a kv pair whose
	// "{opus}" token isn't present in the target string (KiroPrompt) is simply
	// a no-op, keeping the "one F() call feeds either dict" invariant intact
	// (mirrors Python's str.format silently ignoring a surplus kwarg). It is
	// computed only for bedrock: the estimate is denominated in rate-card
	// credits and would be ~5.7x too generous for kiro credits.
	opus := ""
	if cls == saleClassBedrock {
		opus = b.apikeyOpusEstFor(cc.ChatID, lang)
	}
	// "prices" rides the exact same arrangement as "opus": always passed, but
	// only FixedPrompt carries a {prices} placeholder, so for the other two
	// SKUs the kv pair is a silent no-op. It is fetched only for fixed because
	// a per-request rate card describes how a FIXED key bills and says nothing
	// about a rate-card-burn SKU — and because a customer tapping bedrock or
	// kiro must not pay the latency of an admin-API call they gain nothing
	// from. An unreachable pool yields "" and the prompt sends without it.
	prices := ""
	if cls == saleClassFixed {
		prices = b.apikeyFixedPriceCard(cc.Ctx)
	}
	// "name" is the admin-renameable SKU display name. UPPERCASED here because
	// both prompt templates are all-caps headers ("🔑 BUY {name}") — the
	// default name renders byte-identically to the pre-rename literal.
	text := i18n.F(prompt.T(lang),
		"name", strings.ToUpper(b.apikeyProductName(cls)),
		"rate", b.apikeyRateDispFor(cc.ChatID, cls),
		"min", itoa64(mn), "balance", b.usdtDisp(balance), "opus", opus,
		"prices", prices)
	if markup != nil {
		b.send(cc.Ctx, cc.ChatID, text, markup)
	} else {
		b.send(cc.Ctx, cc.ChatID, text, nil)
	}
}

// cbApikeyQty handles a quick credit-amount button on either mint prompt —
// apikeyqty: / kiroapiqty: (telegram_bot.py:15448-15466). Atomically claims the
// pending prompt so a rapid double-tap can't create several reserved orders
// (mirrors buyqty:). The button's SKU must MATCH the armed one: a stale bedrock
// button after re-arming the kiro prompt (or vice versa) must not buy the wrong
// product at the wrong rate.
func (b *Bot) cbApikeyQty(cc *cbCtx) {
	cls := saleClassBedrock
	switch {
	case strings.HasPrefix(cc.Data, "kiroapiqty:"):
		cls = saleClassKiro
	case strings.HasPrefix(cc.Data, "fixedapiqty:"):
		cls = saleClassFixed
	}
	lang := b.langFor(cc.ChatID, "en")
	_, raw, _ := strings.Cut(cc.Data, ":")
	credits, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return
	}
	if credits < b.apikeyMinCredits(cls) || credits > b.apikeyMaxCredits(cls) {
		return // forged/stale button
	}
	// Peek the armed SKU, then claim atomically (the pop IS the double-tap guard).
	cc.st.mu.Lock()
	armed := cc.st.APIKey
	if armed == nil || armed.SaleClass != cls {
		cc.st.mu.Unlock()
		return // nothing armed / stale cross-SKU button
	}
	cc.st.APIKey = nil
	cc.st.mu.Unlock()
	b.startApikeyPurchase(cc.Ctx, cc.ChatID, lang, credits, cls)
}

// cbApikeyRCStart handles apikeyrc:start — the recharge row tapped on the buy
// menu (telegram_bot.py:15467-15501): lists this chat's LIVE keys as pick
// buttons. Same order-creating gates as apikey:start.
func (b *Bot) cbApikeyRCStart(cc *cbCtx) {
	c := &i18n.C().APIKey
	lang := b.langFor(cc.ChatID, "en")
	if b.salesBlocked(cc.Ctx, cc.ChatID, lang, nil) {
		return
	}
	if b.pendingGuard(cc.Ctx, cc.ChatID, lang, "buy") {
		return
	}
	// ANY mint SKU sellable keeps the picker open; the picked key's own SKU is
	// re-gated at pick/order time (a bedrock key can't be topped up while only
	// the kiro SKU is on sale, and vice versa).
	if !b.apikeyForSale(saleClassBedrock) && !b.apikeyForSale(saleClassKiro) &&
		!b.apikeyForSale(saleClassFixed) {
		b.send(cc.Ctx, cc.ChatID, c.Off.T(lang), nil)
		return
	}
	// Live keys only, filtered IN SQL: a refunded key was revoked on Kiro-Go and
	// must never be offered for a top-up (a recharge would resurrect it
	// server-side) — and a Go post-filter over a capped page could hide an older
	// live key behind newer refunded rows.
	live, err := b.store.APIKeysForChat(cc.ChatID, 10, true)
	if err != nil {
		log.Println("api_keys_for_chat failed:", err)
		return
	}
	if len(live) == 0 {
		b.send(cc.Ctx, cc.ChatID, c.RCNone.T(lang), nil)
		return
	}
	// One button per key: SKU tag + masked value + originally bought credits, so
	// a two-SKU customer picks the right kind (the units are ~5.7x apart — a
	// mispick would read as wild over/under-pricing). The per-key LIVE stats
	// fetch happens at PICK time (one key, one call) — never one round-trip per
	// key inside this listing.
	var rows [][]tg.InlineKeyboardButton
	for _, k := range live {
		tag := c.SkuTag[apikeyKeyClass(k)].T(lang)
		label := tag + " · " + maskAPIKey(k.APIKey) + " · " +
			itoa64(int64(math.Round(k.Credits))) + " credits"
		rows = append(rows, btnRow(label, cbData("apikeyrcpick", k.OrderID)))
	}
	b.sendMenu(cc.Ctx, cc.ChatID, c.RCPick.T(lang), rows)
}

// cbApikeyRCPick handles apikeyrcpick:<target des> — a key chosen for a
// recharge (telegram_bot.py:15502-15548): run the full eligibility gate, then
// arm the typed-amount capture and show the amount prompt.
func (b *Bot) cbApikeyRCPick(cc *cbCtx) {
	c := &i18n.C().APIKey
	lang := b.langFor(cc.ChatID, "en")
	if b.salesBlocked(cc.Ctx, cc.ChatID, lang, nil) {
		return
	}
	if b.pendingGuard(cc.Ctx, cc.ChatID, lang, "buy") {
		return
	}
	target := strings.TrimPrefix(cc.Data, "apikeyrcpick:")
	g := b.apikeyRCGate(cc.Ctx, cc.ChatID, target)
	// The PICKED key's own SKU gates the sale from here on: rate, bounds and the
	// for-sale switch are all that class's. (The gate already verified
	// ownership, so reading the class off g.Row is safe.)
	cls := apikeyKeyClass(g.Row)
	if !b.apikeyForSale(cls) {
		b.send(cc.Ctx, cc.ChatID, c.Off.T(lang), nil)
		return
	}
	if g.Err != "" {
		b.send(cc.Ctx, cc.ChatID, b.rcGateMessage(g, lang, cls), nil)
		return
	}
	// Arm the typed-amount capture with the TARGET des; drop stale sibling
	// captures first so an abandoned prompt can't eat the number.
	cc.st.ClearCustomerPending()
	cc.st.mu.Lock()
	cc.st.APIKeyRC = &APIKeyRCCtx{TargetOrderID: target}
	cc.st.mu.Unlock()

	mn := b.apikeyMinCredits(cls)
	var markup *tg.InlineKeyboardMarkup
	// Quick buttons: bounded by the per-key HEADROOM (not the per-order cap), in
	// the key's own SKU denominations.
	if choices := qtyChoicesFor(cls, mn, g.Headroom); len(choices) > 0 {
		markup = &tg.InlineKeyboardMarkup{InlineKeyboard: apikeyQtyRows(choices, "apikeyrcqty")}
	}
	balance, _ := b.getBalance(cc.ChatID)
	text := i18n.F(c.RCPrompt.T(lang),
		"masked", maskAPIKey(g.Row.APIKey),
		"remaining", itoa64(int64(g.Remaining)),
		"rate", b.apikeyRateDispFor(cc.ChatID, cls),
		"min", itoa64(mn), "headroom", itoa64(g.Headroom),
		"balance", b.usdtDisp(balance))
	if markup != nil {
		b.send(cc.Ctx, cc.ChatID, text, markup)
	} else {
		b.send(cc.Ctx, cc.ChatID, text, nil)
	}
}

// cbApikeyRCQty handles a quick credit-amount button on the recharge prompt —
// apikeyrcqty: (telegram_bot.py:15549-15570). Atomically claims the pending
// capture (which carries the target des) so a rapid double-tap can't create
// several reserved orders.
func (b *Bot) cbApikeyRCQty(cc *cbCtx) {
	lang := b.langFor(cc.ChatID, "en")
	_, raw, _ := strings.Cut(cc.Data, ":")
	credits, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return
	}
	// Bounds in the TARGET key's own SKU (peek the armed target — the atomic pop
	// below stays the double-submit guard). A missing capture dies here exactly
	// as it would at the pop.
	cc.st.mu.Lock()
	armed := cc.st.APIKeyRC
	cc.st.mu.Unlock()
	if armed == nil {
		return // stale button, nothing armed
	}
	row, _ := b.store.APIKeyForOrder(armed.TargetOrderID)
	cls := apikeyKeyClass(row)
	if credits < b.apikeyMinCredits(cls) || credits > b.apikeyMaxCredits(cls) {
		return // forged/stale button
	}
	cc.st.mu.Lock()
	claimed := cc.st.APIKeyRC
	cc.st.APIKeyRC = nil
	cc.st.mu.Unlock()
	if claimed == nil {
		return // double-tap loser
	}
	b.startApikeyRecharge(cc.Ctx, cc.ChatID, lang, claimed.TargetOrderID, credits)
}

// ---- text captures ----------------------------------------------------------

// apikeyParseCredits normalizes a typed credit amount the way Python does
// (telegram_bot.py:19064): strip thousands separators written as either ","
// or "." so 7500 / 7,500 / 7.500 all mean 7500, then require plain digits
// (_digits: non-empty, <= 12 chars, ASCII decimal only).
func apikeyParseCredits(text string) (int64, bool) {
	raw := strings.NewReplacer(",", "", ".", "").Replace(strings.TrimSpace(text))
	if !digitsOK(raw) {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// capAPIKey is capture-chain entry #45, pending_apikey — the API-key credit
// amount free-text capture (telegram_bot.py:19058-19078). PEEK semantics: an
// escaping text (a "/command" or a keyboard label) leaves the slot ARMED and is
// NOT consumed; an out-of-bounds/invalid amount is answered and the slot STAYS
// armed for a retry.
func (b *Bot) capAPIKey(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.APIKey
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false // peek escape: slot stays armed, router keeps going
	}
	// The armed value IS the SKU — validate the typed number against THAT
	// class's bounds (peek; the atomic pop below stays the double-submit guard).
	cls := armed.SaleClass
	if cls == "" {
		cls = saleClassBedrock
	}
	cat := &i18n.C().APIKey
	mn, mx := b.apikeyMinCredits(cls), b.apikeyMaxCredits(cls)
	credits, ok := apikeyParseCredits(c.Text)
	if !ok || credits < mn || credits > mx {
		b.send(c.Ctx, c.ChatID, i18n.F(cat.BadAmount.T(c.Lang),
			"min", itoa64(mn), "max", itoa64(mx)), nil)
		return true // invalid -> keep pending for retry
	}
	// Atomically claim the prompt so a double-submit can't order twice.
	c.st.mu.Lock()
	claimed := c.st.APIKey
	c.st.APIKey = nil
	c.st.mu.Unlock()
	if claimed == nil {
		return true
	}
	b.startApikeyPurchase(c.Ctx, c.ChatID, c.Lang, credits, cls)
	return true
}

// capAPIKeyRC is capture-chain entry #46, pending_apikeyrc — the recharge
// amount free-text capture (telegram_bot.py:19084-19103). Mirrors capAPIKey;
// the deeper gates (headroom / live stats) re-run inside startApikeyRecharge,
// so this only pre-validates the basic bounds in the TARGET key's own SKU.
func (b *Bot) capAPIKeyRC(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.APIKeyRC
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false // peek escape
	}
	row, _ := b.store.APIKeyForOrder(armed.TargetOrderID)
	cls := apikeyKeyClass(row)
	cat := &i18n.C().APIKey
	mn, mx := b.apikeyMinCredits(cls), b.apikeyMaxCredits(cls)
	credits, ok := apikeyParseCredits(c.Text)
	if !ok || credits < mn || credits > mx {
		b.send(c.Ctx, c.ChatID, i18n.F(cat.BadAmount.T(c.Lang),
			"min", itoa64(mn), "max", itoa64(mx)), nil)
		return true // invalid -> keep pending for retry
	}
	c.st.mu.Lock()
	claimed := c.st.APIKeyRC
	c.st.APIKeyRC = nil
	c.st.mu.Unlock()
	if claimed == nil {
		return true
	}
	b.startApikeyRecharge(c.Ctx, c.ChatID, c.Lang, claimed.TargetOrderID, credits)
	return true
}

// ---- pay screen -------------------------------------------------------------

// apikeyPayScreenText renders the API-key branch of the payment screen —
// telegram_bot.py:10213-10239. Called only for an api-key-family order.
//
// The per-100 rate is derived from the STORED total so rate × credits always
// reconciles with what the customer is charged. It MUST match the "/ 100
// credits" unit in the templates: dividing per 1000 while the label says per
// 100 would print a rate 10x the real one directly above the correct total, on
// the payment screen itself. The buyer-specific apikeyRateDispFor is NOT usable
// here — it reflects only the reseller rate and knows nothing about an applied
// code, so it would print "Price: $1.00 / 100" above a code-discounted
// "Total: $40.00" for 5000 credits: arithmetic the customer can see does not
// add up.
func (b *Bot) apikeyPayScreenText(o *store.Order, lang i18n.Lang, balance int64) string {
	c := &i18n.C().APIKey
	rateDisp := b.usdtDisp(0)
	if o.Qty != 0 {
		rateDisp = b.usdtDisp(int64(math.Round(float64(o.Total) * 100.0 / float64(o.Qty))))
	}
	if o.Product == apikeyRechargeProduct {
		// Recharge order: show WHICH key gets topped up (masked). The binding +
		// key rows exist from order creation; degrade to a bare mask if either is
		// somehow gone — the pay screen must always render.
		masked := ""
		if rec, err := b.store.APIKeyRechargeForOrder(o.Des); err == nil && rec != nil {
			if krow, err := b.store.APIKeyForOrder(rec.TargetOrderID); err == nil && krow != nil {
				masked = maskAPIKey(krow.APIKey)
			}
		}
		return i18n.F(c.RCPayPrompt.T(lang),
			"credits", itoa64(o.Qty), "masked", masked, "rate", rateDisp,
			"total", b.usdtDisp(o.Total), "balance", b.usdtDisp(balance))
	}
	// SKU-correct product naming on the pay screen (same shape either way). The
	// class drives BOTH the template and the display name, so a renamed SKU
	// can never show one SKU's name above the other SKU's units.
	cls, tmpl := saleClassBedrock, c.PayPrompt
	switch o.Product {
	case kiroApikeyProduct:
		cls, tmpl = saleClassKiro, c.KiroPayPrompt
	case fixedApikeyProduct:
		cls, tmpl = saleClassFixed, c.FixedPayPrompt
	}
	return i18n.F(tmpl.T(lang),
		"name", b.apikeyProductName(cls),
		"credits", itoa64(o.Qty), "rate", rateDisp,
		"total", b.usdtDisp(o.Total), "balance", b.usdtDisp(balance))
}

// ---- buy-menu rows ----------------------------------------------------------

// apikeyBuyRows returns the virtual API-key rows for the /buy menu and whether
// ANY key SKU is sellable — the port of telegram_bot.py:11737-11757. Shown
// whenever the admin has set that SKU's price and Kiro-Go is configured (their
// "stock" is the proxy pool, checked at the amount prompt + at mint, so there is
// no per-render pool call in this hot path).
func (b *Bot) apikeyBuyRows(chatID int64, lang i18n.Lang) (rows [][]tg.InlineKeyboardButton, anySellable bool) {
	c := &i18n.C().APIKey
	bedrockOn := b.apikeyForSale(saleClassBedrock)
	kiroOn := b.apikeyForSale(saleClassKiro)
	fixedOn := b.apikeyForSale(saleClassFixed)
	// "name" is the admin-renameable display name (plain, not uppercased: these
	// are title-case button labels, not headers).
	if bedrockOn {
		rows = append(rows, styledBtnRow(
			i18n.F(c.Btn.T(lang), "name", b.apikeyProductName(saleClassBedrock),
				"rate", b.apikeyRateDisp(saleClassBedrock)),
			"apikey:start", "primary"))
	}
	if kiroOn {
		rows = append(rows, styledBtnRow(
			i18n.F(c.KiroBtn.T(lang), "name", b.apikeyProductName(saleClassKiro),
				"rate", b.apikeyRateDisp(saleClassKiro)),
			"kiroapi:start", "primary"))
	}
	if fixedOn {
		rows = append(rows, styledBtnRow(
			i18n.F(c.FixedBtn.T(lang), "name", b.apikeyProductName(saleClassFixed),
				"rate", b.apikeyRateDisp(saleClassFixed)),
			"fixedapi:start", "primary"))
	}
	if !bedrockOn && !kiroOn && !fixedOn {
		return nil, false
	}
	// Recharge row: only for customers who already hold a live (non-refunded)
	// key — everyone else has nothing to top up. Local DB count only; no Kiro-Go
	// round-trip in this hot menu path. ONE shared row for both SKUs: the picker
	// lists the customer's keys and each key knows its own class.
	if n, err := b.store.CountAPIKeysForChat(chatID); err == nil && n > 0 {
		rows = append(rows, btnRow(c.RCBtn.T(lang), "apikeyrc:start"))
	}
	return rows, true
}
