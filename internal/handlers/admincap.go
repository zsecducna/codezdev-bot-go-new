package handlers

// admincap.go — the STANDARD-ADMIN CAPABILITY GATE (H1 fix; py_customers_spec.md
// §0, telegram_bot.py:204-346).
//
// Why this file exists: dispatch.go's New() now does `admins: loadAdminsFile()`,
// so a "standard" admin (granted via admins.json — NOT the super admin / owner
// / dev override) can exist for real. Until this file landed, EVERY admin
// slice's permission gate was hand-rolled per handler: fullAdminOnly
// (adminusers.go), settingsSuperAdminOnly (adminsettings.go), an ad-hoc
// isSuperAdmin check (adminpricing.go) — or, for adm:custlookup,
// adm:orders, adm:revenue, and the whole 👥 CUSTOMERS category-open path, NO
// gate at all. That last gap was a LIVE privilege escalation the moment a
// standard admin could exist: a granted-but-not-super admin could open
// Customers -> Look up customer and read any chat's wallet balance, full order
// history (with PayPal order ids), and every owned-account username — none of
// which Python allows a standard admin to see by default.
//
// Port shape: Python's chokepoint — the standard-admin gate inside
// handle_callback (telegram_bot.py `_is_admin_cb`/`_cb_allowed_for` call
// site, ≈:12496-12497 @3069329; cite by symbol, not just line — this
// chokepoint has already moved twice across upstream commits, 12258 ->
// 12412 -> 12497, so a line-only cite silently rots the next time
// telegram_bot.py changes above it. This is the ONE place that history is
// spelled out — command.go and adminusers_test.go cite the same symbol +
// line + revision but point back here for the "why a line number alone
// isn't enough" rationale, so a third move doesn't require editing three
// copies of this paragraph, only this one) — classifies EVERY inline-button tap via
// _is_admin_cb + _cb_allowed_for, which in turn call _std_admin_cb_cap +
// _CAT_CAPS (telegram_bot.py `_std_admin_cb_cap`/`_CAT_CAPS`, ≈:243-318
// @3069329) — ONE central classification that runs BEFORE any
// adm:/admcat:/resmng:/resdisc:/rmreseller:/rmwl: branch executes, and
// BEFORE Telegram's answerCallbackQuery ack. That ordering
// matters: the refusal alert IS that update's one and only callback answer
// (Telegram allows exactly one answerCallbackQuery per callback_query_id; a
// second call is silently dropped by a real server — which is exactly why
// adminsettings.go's settingsSuperAdminOnly deliberately sends a CHAT MESSAGE
// instead of a second alert: that gate runs deep inside dispatch, AFTER the
// unconditional ack has already gone out). This file's gate is wired into
// command.go's handleCallback at the SAME pre-ack position Python uses, so it
// is that update's first (and only) answer, never colliding with the
// one-answer rule, and every existing per-handler gate downstream becomes
// redundant defense-in-depth for the callback path — exactly as it is in
// Python (py_customers_spec.md §0) — rather than something this file needs to
// rip out.
//
// Capability model: Go has no per-admin capability GRANT UI yet (only a
// binary "is this chat id in admins.json" flag), so every standard admin is
// treated as holding exactly Python's DEFAULT_STD_CAPS and nothing else —
// stock_read, hold_restore, audit, toggle_sales — until a real grant UI
// lands. customers_view, orders_view, revenue, password_reset, and pool_view
// are default-OFF, matching Python's own fallback (std_admin_caps(),
// telegram_bot.py:219-226) for an instance whose settings.json has never
// configured "std_admin_caps".
import (
	"strings"

	"codezdev-bot/internal/tg"
)

// defaultStdCaps is Go's fixed twin of Python's DEFAULT_STD_CAPS
// (telegram_bot.py:204) — the fallback std_admin_caps() returns for an
// instance that has never configured a per-admin grant. Go has no grant UI
// yet (admin.go / adminsettings.go's own file headers already note this), so
// every standard admin holds EXACTLY this set, always — a flat default, not
// a per-admin bitmap.
var defaultStdCaps = map[string]bool{
	"stock_read":   true,
	"hold_restore": true,
	"audit":        true,
	"toggle_sales": true,
}

// stdAdminCaps returns the capability set a standard (non-super) admin
// currently holds — std_admin_caps() (telegram_bot.py:219-226) with no
// per-admin override path implemented yet, so this is the same fixed set for
// every standard admin.
func (b *Bot) stdAdminCaps() map[string]bool { return defaultStdCaps }

// adminCan reports whether chatID may use capability cap — admin_can
// (telegram_bot.py:229-237). A super admin can do anything; a standard admin
// only its granted (currently: default) capabilities; anyone else nothing.
func (b *Bot) adminCan(chatID int64, cap string) bool {
	if b.isSuperAdmin(chatID) {
		return true
	}
	if !b.isAdmin(chatID) {
		return false
	}
	return b.stdAdminCaps()[cap]
}

// stdCmdCap is _STD_CMD_CAP (telegram_bot.py:211-216) — the slash-command twin
// of stdAdminCbCap below. H1 fix (Opus review of this file): the callback
// surface was fully policed but the SLASH-COMMAND surface (capAdminCmd,
// admin.go) had no gate at all — step 9 only checked isAdmin (dispatch.go),
// so `/orders 50` handed a standard admin the full orders_view report
// (customer chat ids, product, tier, total, des) even though the identical
// `adm:orders`/`admcat:orders` callbacks were correctly denied. A command
// whose word is absent from this map is full-admin-only by the same
// default-deny rule as stdAdminCbCap's `return "", false` — that is
// deliberate for "addbal"/"broadcast"/"refund"/"getord" (none of them
// appear here, exactly mirroring stdAdminCbCap's omission of
// adm:refund/adm:getord), not an oversight.
var stdCmdCap = map[string]string{
	"admin": "", "stock": "stock_read", "orders": "orders_view", "audit": "audit",
	"suspend": "toggle_sales", "reset": "password_reset", "pool": "pool_view",
}

// catCaps is _CAT_CAPS (telegram_bot.py:243-250): the categories a standard
// admin may OPEN, and the capability set that unlocks each (holding any ONE
// member is enough — the category menu is then filtered via filterAdminRows
// to only the buttons that capability actually grants). "roles", "resellers",
// "stdperms", "pricing", "payments", and "entra" are deliberately ABSENT —
// those categories stay super-admin-only no matter what a standard admin
// holds, exactly like Python (whose _CAT_CAPS omits them too).
var catCaps = map[string]map[string]bool{
	"stock":     {"stock_read": true, "hold_restore": true},
	"orders":    {"orders_view": true},
	"refundreq": {"orders_view": true},
	"customers": {"customers_view": true},
	"settings":  {"toggle_sales": true},
	"whitelist": {"toggle_sales": true},
}

// stdAdminCbCap classifies an admin callback for a STANDARD (non-super)
// admin — the sole port of _std_admin_cb_cap + its _CAT_CAPS lookup
// (telegram_bot.py:243-250, 276-318), kept as ONE function so the rule lives
// in exactly one place (H1's own instruction). governed=false means data is
// not in this table at all -> full-admin-only, deny (Python's `return
// None`). governed=true with cap=="" means always-allowed navigation;
// governed=true with a non-empty cap means the tapper must hold that
// capability (adminCan) to pass.
//
// "adm:addwl" (not Python's literal "adm:wladd") is THIS repo's own callback
// string for the same button — adminusers.go's documented, deliberate naming
// divergence — so the case below gates the Go string, not the Python one.
func stdAdminCbCap(data string) (cap string, governed bool) {
	if data == "admroot" || data == "admin" {
		return "", true // panel navigation, always allowed
	}
	if rest, ok := strings.CutPrefix(data, "admcat:"); ok {
		need := catCaps[rest]
		if need == nil {
			return "", false
		}
		for c := range need {
			if defaultStdCaps[c] {
				return "", true
			}
		}
		return "", false
	}
	if data == "adm:stock" {
		return "stock_read", true
	}
	if data == "adm:holdstock" || strings.HasPrefix(data, "holdprod:") || strings.HasPrefix(data, "holdact:") {
		return "hold_restore", true
	}
	if data == "adm:audit" {
		return "audit", true
	}
	if data == "adm:togglesales" {
		return "toggle_sales", true
	}
	if data == "adm:toggleprivate" || data == "adm:addwl" || strings.HasPrefix(data, "rmwl:") {
		return "toggle_sales", true
	}
	if data == "adm:orders" || strings.HasPrefix(data, "admord:") {
		return "orders_view", true
	}
	if data == "adm:revenue" || strings.HasPrefix(data, "admrev:") ||
		strings.HasPrefix(data, "admrevmore:") || strings.HasPrefix(data, "admrevp:") {
		return "revenue", true
	}
	if data == "adm:custlookup" {
		return "customers_view", true
	}
	if data == "adm:pool" || data == "admpoolr" {
		return "pool_view", true
	}
	if strings.HasPrefix(data, "refrev:") || strings.HasPrefix(data, "refrej:") {
		return "orders_view", true
	}
	// adminstock.go fix (py_stock_spec.md §0): "adm:upstock", "adm:upstockstore",
	// "adm:mngprod" (+ every "prod*:"/"upstock*:" sub-callback), "adm:stockbroadcast"
	// and "adm:provpool" are deliberately ABSENT here — Python's own
	// _std_admin_cb_cap has no branch for any of them, so they fall through to
	// `return None` (full-admin/super-admin-only), same as this function's own
	// default `return "", false` below. A prior revision of this file had an
	// invented "prodsusp:" -> toggle_sales grant that predated the mngprod
	// implementation and contradicted this — removed as part of landing the
	// real feature: an unclassified string denies (safe), but a grant this
	// table never earned is the exact privilege-escalation bug this file's own
	// header warns against.
	return "", false // full-admin-only
}

// customerCBPrefixes is _CUSTOMER_CB_PREFIXES (telegram_bot.py:255-263)
// verbatim: every callback prefix the CUSTOMER surface emits. A standard
// admin is ALSO a customer, so these always pass the capability gate
// untouched — anything NOT matching one of these is treated as a
// privileged/admin callback and IS policed (default-deny safety: better to
// over-restrict a granted admin than to leak a privileged action).
//
// The AWS-sell prefixes ("sell:", "sellnew", "sellok:", "sellno:",
// "sellpay:") that used to be ported here as forward-compatible placeholders
// are gone: the whole AWS-sell feature (internal/awssell, internal/awsval,
// the awsadm: admin tree) was removed — user decision, full rip-out, never
// shipped past the store layer. The on-chain product-pay prefixes ("paybsc",
// "paytrx") and the deposit "I've paid" button ("usdtpaid:") went with the
// tx-hash credit path: products pay from wallet balance and deposits credit
// hands-free via the watcher, so no customer button emits them any more.
var customerCBPrefixes = []string{
	"buy:", "buyx:", "buygo:", "buyqty:", "tier:", "back:", "setlang:",
	"apikey", "kiroapi",
	"ss:", "refreq:", "hist", "redl:", "cancelord:", "keepord", "applycode:",
	"otpkeep", "otpstop:", "dep:", "paybal", "paybnb",
}

// isAdminCB reports whether data is a privileged/admin callback the
// standard-admin capability gate must police — _is_admin_cb
// (telegram_bot.py:266-273). Default-deny: anything not clearly
// customer-facing is treated as admin.
func isAdminCB(data string) bool {
	if data == "" {
		return false
	}
	for _, p := range customerCBPrefixes {
		if strings.HasPrefix(data, p) {
			return false
		}
	}
	return true
}

// cbAllowedFor reports whether chatID may use admin callback data —
// _cb_allowed_for (telegram_bot.py:321-328). A super admin always may; a
// standard admin only when stdAdminCbCap classifies data as always-allowed
// nav or a held capability confirms it.
func (b *Bot) cbAllowedFor(chatID int64, data string) bool {
	if b.isSuperAdmin(chatID) {
		return true
	}
	cap, governed := stdAdminCbCap(data)
	if !governed {
		return false
	}
	return cap == "" || b.adminCan(chatID, cap)
}

// filterAdminRows drops inline buttons a standard admin can't use — the Go
// port of _filter_admin_rows (telegram_bot.py:331-346). A super admin (or a
// non-admin — filtering only ever matters for a rendered admin menu, which a
// non-admin never reaches) keeps every row untouched (no-op). A row a
// standard admin has NO usable button left in is dropped entirely, so the
// menu stays clean rather than rendering a dead blank row.
func (b *Bot) filterAdminRows(chatID int64, rows [][]tg.InlineKeyboardButton) [][]tg.InlineKeyboardButton {
	if b.isSuperAdmin(chatID) {
		return rows
	}
	out := make([][]tg.InlineKeyboardButton, 0, len(rows))
	for _, row := range rows {
		var kept []tg.InlineKeyboardButton
		for _, btn := range row {
			if !isAdminCB(btn.CallbackData) || b.cbAllowedFor(chatID, btn.CallbackData) {
				kept = append(kept, btn)
			}
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}
