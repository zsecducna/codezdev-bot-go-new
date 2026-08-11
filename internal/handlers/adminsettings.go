package handlers

// adminsettings.go — ⚙️ SETTINGS category + 💳 PAYMENT METHODS submenu
// (py_settings_spec.md is the exact behavioural/verbatim-string reference for
// every callback below, keyed off telegram_bot.py). All 22 callbacks in this
// file used to fall through cbAdmAction's default branch to the generic
// "not wired up yet" notice (admin.go:766, adminNotPorted) — this file
// replaces each stub with real behaviour.
//
// Permission model (py_settings_spec.md §3): only adm:togglesales and
// adm:toggleprivate are reachable by a standard (granted, non-super) admin
// — the toggle_sales capability class. Every other action here (every other
// toggle, every Kiro-Go setter, both on-chain address setters, all 3 payment
// toggles, and broadcast) is super-admin/owner/dev only, enforced by
// settingsSuperAdminOnly. Go has no per-capability grant table yet
// (admin.go's own header notes the std-admin filter isn't ported), so in
// practice "may run without the gate" just means the two toggle handlers
// below never call it.
//
// Env-poke note (py_settings_spec.md §1): Python pokes os.environ inline at
// save time for usd_to_vnd/kiro_go_*/bep20_address/tron_address. Go must
// NEVER mutate the process environment (CLAUDE.md). internal/config's
// USDToVND/BEP20Address/TRONAddress/KiroGoAdminKey/KiroGoBaseURL/
// KiroGoAccountID getters already prefer the settings.json value over the
// env snapshot (internal/config/settings.go) — confirmed by inspection, no
// getter changes were needed — so a plain SaveSettings call here IS the
// live-apply mechanism; no env write is needed or wanted.
import (
	"context"
	"errors"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/tg"
)

// settingsNotConfigured is a Go-only fallback string — no Python equivalent,
// since the original always has a loaded config. Shown instead of writing
// through a nil b.cfg (which would either no-op silently or, for a raw map
// write, panic on a nil map) so a unit test / unconfigured install still
// gets an explicit reply rather than a dropped tap.
const settingsNotConfigured = "⚠️ Settings not configured on this instance."

// settingsSaveFailed is shown when a wizard's own SaveSettings call fails
// (disk full, read-only FS, tmp-rename failure) — distinct from
// settingsNotConfigured (nil b.cfg): here a real store exists but the write
// itself did not land, so the reply must say plainly that nothing was saved
// rather than falling through to a ✅ success message the failed write never
// earned (H1 fix).
const settingsSaveFailed = "⚠️ Could not save — nothing was changed. Try again."

// settingsPermissionDenied is the refusal shown to a granted-but-not-super
// admin who taps a super-admin-only settings action (py_settings_spec.md
// §3/§7).
const settingsPermissionDenied = "⛔ Not permitted."

// settingsSuperAdminOnly is the shared refusal gate every super-admin-only
// handler in this file runs first. cbAdmAction has already confirmed
// b.isAdmin(cc.ChatID) before reaching any case here, so a false return means
// "an admin, but not a super-admin, tapped an action outside the two
// toggle_sales-capability exceptions" (adm:togglesales / adm:toggleprivate,
// which never call this gate at all).
//
// Delivery note (H2 fix — this used to fire a SECOND answerCallbackQuery
// alert here). command.go's handleCallback already sends one blank,
// unconditional answerCallbackQuery ahead of dispatchCB for EVERY callback
// (@12258), and Telegram allows exactly one answer per callback_query_id: a
// second AnswerCallbackQuery call from this gate is silently rejected by a
// real Telegram server and never reaches the admin — only the fake-Telegram
// test harness (which records every call regardless of Telegram's
// one-per-callback rule) made that look correct. adminusers.go's
// fullAdminOnly already solves the identical problem for its own
// super-admin gates by sending a CHAT MESSAGE instead of a second alert, so
// this gate now does the same rather than reordering the shared
// handleCallback pipeline (a bigger, cross-file change outside this slice's
// ownership).
func (b *Bot) settingsSuperAdminOnly(cc *cbCtx) bool {
	if b.isSuperAdmin(cc.ChatID) {
		return true
	}
	b.send(cc.Ctx, cc.ChatID, settingsPermissionDenied, admNav("admcat:settings"))
	return false
}

// admNavRetry is _adm_nav(back, next_step) WITH a next_step row present — the
// keyboard shape every "invalid input, retap to retry" reply in this file
// uses (py_settings_spec.md §7): [next_step button] then [↩️ Back]. Kept
// separate from admin.go's admNav (whose single-arg shape is used pervasively
// elsewhere) since the retry row is unique to this file's text-validation
// failures.
func admNavRetry(back, label, data string) *tg.InlineKeyboardMarkup {
	return &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		btnRow(label, data),
		btnRow("↩️ Back", back),
	}}
}

// stripThousandsSeps mirrors `text.strip().replace(",", "").replace(".", "")`
// — the max-qty and rate typed inputs both accept "10,000" / "10.000"
// grouping forms (py_settings_spec.md §4).
func stripThousandsSeps(text string) string {
	return strings.NewReplacer(",", "", ".", "").Replace(strings.TrimSpace(text))
}

// commaInt renders n with thousands separators — Python's f"{n:,}" format
// used in the rate setter's prompt/success/error text (py_settings_spec.md
// §4, e.g. "1,000", "200,000", "23,000").
func commaInt(n int) string {
	neg := n < 0
	s := itoa(n)
	if neg {
		s = s[1:]
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// ---- generic boolean toggle -------------------------------------------------

// errToggleSettingNotConfigured is toggleSetting's sentinel for the b.cfg ==
// nil case — a genuinely unconfigured install (or a unit test that wired no
// Config at all) — kept distinguishable from a real SaveSettings failure so
// a caller can pick the right reply string instead of collapsing both into
// one. See toggleSetting below.
var errToggleSettingNotConfigured = errors.New("adminsettings: no config wired")

// toggleSetting flips a boolean settings key with an inline default — the
// `s.get(key, default)` pattern every simple toggle in py_settings_spec.md §4
// shares — and persists it. err is errToggleSettingNotConfigured when no
// config is wired at all (a Go map write would panic on a nil map, unlike
// Python's dict.get default, so this must be checked before trusting next);
// otherwise err is the raw SaveSettings failure, a real config wired but the
// WRITE itself did not land. The two used to collapse into a single (false,
// false) return with no way for a caller to tell them apart, which meant
// every caller reported "not configured" on a save failure too — the same
// bug task #29 already fixed at four direct call sites elsewhere in this
// file, just one frame further down where a grep for the call-site shape
// couldn't see it. Callers must use errors.Is against
// errToggleSettingNotConfigured to choose settingsNotConfigured vs
// settingsSaveFailed.
//
// Task #13 Tier 5 (config-audit-spec.md §2): this is the SINGLE
// instrumentation point for every toggle that routes through it —
// cbToggleSimple's four bell toggles plus cbToggleMaint — rather than
// auditing each of the five wrappers separately. actorChat must already be
// formatted (fmtInt(cc.ChatID)) since this has no cbCtx of its own; a future
// toggle wired through this helper is audited by construction with zero
// extra code at its own call site.
func (b *Bot) toggleSetting(actorChat, key string, def bool) (next bool, err error) {
	if b.cfg == nil {
		return false, errToggleSettingNotConfigured
	}
	s := b.cfg.Settings()
	old := settingBool(s, key, def)
	next = !old
	s[key] = next
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		return false, err
	}
	b.auditConfigSet(actorChat, "config_toggle_"+key, key,
		strconv.FormatBool(old), strconv.FormatBool(next))
	return next, nil
}

// cbToggleSimple is the shared body for the four "bell" toggles that differ
// only in (settings key, default, verb-inflected prefix) —
// py_settings_spec.md §4 togglebcast/togglestockbcast/togglesalenotify/
// toggledepnotify. All four are super-admin only. prefix already carries the
// correct "is now"/"are now" inflection so the final text matches Python
// verbatim (e.g. "New-sale admin alerts ARE now ON 🔔.").
func (b *Bot) cbToggleSimple(cc *cbCtx, key string, def bool, prefix string) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	next, err := b.toggleSetting(fmtInt(cc.ChatID), key, def)
	if err != nil {
		text := settingsSaveFailed
		if errors.Is(err, errToggleSettingNotConfigured) {
			text = settingsNotConfigured
		}
		b.send(cc.Ctx, cc.ChatID, text, admNav("admcat:settings"))
		return
	}
	state := "OFF 🔕"
	if next {
		state = "ON 🔔"
	}
	b.send(cc.Ctx, cc.ChatID, prefix+" "+state+".", admNav("admcat:settings"))
}

// cbToggleBcast flips sale_broadcast — telegram_bot.py:13830.
func (b *Bot) cbToggleBcast(cc *cbCtx) {
	b.cbToggleSimple(cc, "sale_broadcast", true, "Sale broadcast on delivery is now")
}

// cbToggleStockBcast flips auto_stock_broadcast — telegram_bot.py:13839.
func (b *Bot) cbToggleStockBcast(cc *cbCtx) {
	b.cbToggleSimple(cc, "auto_stock_broadcast", true, "Auto restock alert is now")
}

// cbToggleSaleNotify flips admin_sale_notify — telegram_bot.py:13850.
func (b *Bot) cbToggleSaleNotify(cc *cbCtx) {
	b.cbToggleSimple(cc, "admin_sale_notify", true, "New-sale admin alerts are now")
}

// cbToggleDepNotify flips admin_deposit_notify — telegram_bot.py:13859.
func (b *Bot) cbToggleDepNotify(cc *cbCtx) {
	b.cbToggleSimple(cc, "admin_deposit_notify", true, "New-deposit admin alerts are now")
}

// cbToggleMaint flips maintenance — telegram_bot.py:13868. ON blocks
// non-admin customers with a maintenance notice (command.go's
// b.maintenance() gate); admin keeps full access.
func (b *Bot) cbToggleMaint(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	next, err := b.toggleSetting(fmtInt(cc.ChatID), "maintenance", false)
	if err != nil {
		text := settingsSaveFailed
		if errors.Is(err, errToggleSettingNotConfigured) {
			text = settingsNotConfigured
		}
		b.send(cc.Ctx, cc.ChatID, text, admNav("admcat:settings"))
		return
	}
	state := "OFF 🟢 (normal)"
	if next {
		state = "ON 🛠️ (customers blocked)"
	}
	b.send(cc.Ctx, cc.ChatID, "Maintenance mode is now "+state+".", admNav("admcat:settings"))
}

// cbToggleSales flips sales_paused — _toggle_sales (telegram_bot.py:5267),
// handler at :13878. No settingsSuperAdminOnly gate: this is one of the two
// toggle_sales-capability actions a standard (granted) admin may run
// (py_settings_spec.md §3). Turning sales back ON (unpausing) ALSO clears
// private_sale — a public sale supersedes a private one; pausing alone
// leaves private_sale untouched.
func (b *Bot) cbToggleSales(cc *cbCtx) {
	if b.cfg == nil {
		b.send(cc.Ctx, cc.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return
	}
	s := b.cfg.Settings()
	oldPaused := settingBool(s, "sales_paused", false)
	oldPrivate := settingBool(s, "private_sale", false)
	next := !oldPaused
	s["sales_paused"] = next
	if !next {
		s["private_sale"] = false
	}
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return
	}
	// Task #13 Tier 5, two-key site (spec §2): unpausing also clears
	// private_sale as a side effect, so ONE row must name BOTH transitions —
	// two rows would misrepresent one admin tap as two independent actions.
	newPrivate := oldPrivate
	if !next {
		newPrivate = false
	}
	if b.store != nil {
		b.store.LogAction(fmtInt(cc.ChatID), "admin", "config_toggle_sales_paused",
			"sales_paused,private_sale",
			"sales_paused: "+auditOldNew(strconv.FormatBool(oldPaused), strconv.FormatBool(next))+
				"; private_sale: "+auditOldNew(strconv.FormatBool(oldPrivate), strconv.FormatBool(newPrivate)),
			"")
	}
	state := "ON 🟢 (buying allowed)"
	if next {
		state = "PAUSED ⏸️ (no buying)"
	}
	b.send(cc.Ctx, cc.ChatID, "Sales are now "+state+".", admNav("admcat:settings"))
}

// cbTogglePrivate flips private_sale — _toggle_private_sale
// (telegram_bot.py:5254), handler at :13897. The other toggle_sales-
// capability action (no settingsSuperAdminOnly gate). Enabling ALSO
// force-pauses public sale (sales_paused=true) so "sale on + private on" is
// unreachable; disabling leaves sales_paused untouched.
func (b *Bot) cbTogglePrivate(cc *cbCtx) {
	if b.cfg == nil {
		b.send(cc.Ctx, cc.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return
	}
	s := b.cfg.Settings()
	oldPrivate := settingBool(s, "private_sale", false)
	oldPaused := settingBool(s, "sales_paused", false)
	next := !oldPrivate
	s["private_sale"] = next
	if next {
		s["sales_paused"] = true
	}
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return
	}
	// Task #13 Tier 5, two-key site (spec §2): same rationale as
	// cbToggleSales above — enabling private sale force-pauses public sale as
	// a side effect, so ONE row names both transitions.
	newPaused := oldPaused
	if next {
		newPaused = true
	}
	if b.store != nil {
		b.store.LogAction(fmtInt(cc.ChatID), "admin", "config_toggle_private_sale",
			"private_sale,sales_paused",
			"private_sale: "+auditOldNew(strconv.FormatBool(oldPrivate), strconv.FormatBool(next))+
				"; sales_paused: "+auditOldNew(strconv.FormatBool(oldPaused), strconv.FormatBool(newPaused)),
			"")
	}
	var text string
	if next {
		text = "Private sale is now ON 🔒 — public sale auto-paused. Resellers + " +
			itoa(len(b.whitelist())) + " whitelisted customer(s) can buy."
	} else {
		text = "Private sale is now OFF 🔓 — back to the normal Sale on/off rule."
	}
	b.send(cc.Ctx, cc.ChatID, text, admNav("admcat:settings"))
}

// cbToggleReferral flips referral_enabled — telegram_bot.py:13920. Super-
// admin only. The CURRENT effective value mirrors config.ReferralEnabled's
// own fallback chain (referral_enabled, else legacy promo_enabled, else
// false) so a pre-split instance's existing flag is read correctly before
// being flipped; the flip always writes the NEW key only.
func (b *Bot) cbToggleReferral(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	if b.cfg == nil {
		b.send(cc.Ctx, cc.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return
	}
	next := !b.cfg.ReferralEnabled()
	s := b.cfg.Settings()
	s["referral_enabled"] = next
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return
	}
	state := "OFF 🚫"
	if next {
		state = "ON 👥 (customers can refer)"
	}
	b.send(cc.Ctx, cc.ChatID, "Referral program is now "+state+".", admNav("admcat:settings"))
}

// ---- adm:setmaxqty (button + "setmaxqty" capture) --------------------------

// cbSetMaxQty arms the max-bulk wizard — telegram_bot.py:14209. Super-admin
// only.
func (b *Bot) cbSetMaxQty(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cur := "unlimited"
	if n := b.maxBulk(); n != 0 {
		cur = itoa(n)
	}
	cc.st.mu.Lock()
	cc.st.SetMaxQty = true
	cc.st.mu.Unlock()
	text := "🔢 Current max bulk buy: " + cur +
		"\nSend the new per-order limit (a positive number), or 0 for unlimited:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:settings"))
}

// capSetMaxQty is the "setmaxqty" capture (#23, POP semantics —
// telegram_bot.py:18661). An invalid value re-prompts only after the admin
// retaps the button (the slot is popped either way, matching Python's own
// pending_setmaxqty.pop-then-return-on-error shape).
func (b *Bot) capSetMaxQty(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetMaxQty
	c.st.SetMaxQty = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	raw := stripThousandsSeps(c.Text)
	n, err := strconv.Atoi(raw)
	if !isAllDigits(raw) || err != nil {
		b.send(c.Ctx, c.ChatID, "⚠️ Enter a whole number (0 = unlimited), e.g. 10.",
			admNavRetry("admcat:settings", "🔢 Max bulk buy", "adm:setmaxqty"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text,
	// which would otherwise tell the admin a value was saved when it wasn't.
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return true
	}
	// Task #13 Tier 4: capture the effective pre-change limit BEFORE it is
	// overwritten. 0 is a real saved value here (unlimited), never "never
	// configured", so it is passed through as the literal string "0" —
	// auditOldNew's "(unset)" rendering only fires on an EMPTY old string,
	// which a numeric setting like this one never produces.
	oldQty := b.maxBulk()
	s := b.cfg.Settings()
	s["max_qty"] = n
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return true
	}
	// Audit AFTER the save succeeds, never before (spec §4.3).
	b.auditConfigSet(fmtInt(c.ChatID), "config_set_max_qty", "max_qty", itoa(oldQty), itoa(n))
	text := "✅ Max bulk buy set to unlimited."
	if n != 0 {
		text = "✅ Max bulk buy set to " + itoa(n) + " accounts per order."
	}
	b.send(c.Ctx, c.ChatID, text, admNav("admcat:settings"))
	return true
}

// ---- adm:setrate (button + "setrate" capture) ------------------------------

// rateMin / rateMax mirror RATE_MIN, RATE_MAX (telegram_bot.py:5282).
//
// DUPLICATED, deliberately: config.RateMin / config.RateMax (internal/config/
// settings.go) hold the same two numbers and apply the same bound when READING
// a hand-edited settings.json, so a rate this wizard could never have written
// can never be used either. internal/handlers imports internal/config, so the
// reverse import would cycle — hence two copies rather than one. **If this
// range ever changes, change it there too**; that side carries the matching
// pointer back here.
const (
	rateMin = 1000
	rateMax = 200000
)

// cbSetRate arms the USD→VND rate wizard — telegram_bot.py:13953. Super-admin
// only.
func (b *Bot) cbSetRate(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetRate = true
	cc.st.mu.Unlock()
	text := "💱 Current USD→VND rate: " + b.rateStr() + "\nSend the new rate (VND per 1 USD), e.g. 23000:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:settings"))
}

// capSetRate is the "setrate" capture (#10, POP semantics —
// telegram_bot.py:18321). An out-of-range or non-numeric value requires a
// retap to retry.
func (b *Bot) capSetRate(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetRate
	c.st.SetRate = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	raw := stripThousandsSeps(c.Text)
	n, err := strconv.Atoi(raw)
	if !isAllDigits(raw) || err != nil || n < rateMin || n > rateMax {
		b.send(c.Ctx, c.ChatID,
			"⚠️ Enter a whole number between "+commaInt(rateMin)+" and "+commaInt(rateMax)+" (VND per 1 USD), e.g. 23000.",
			admNavRetry("admcat:settings", "💱 Set rate", "adm:setrate"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text.
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return true
	}
	s := b.cfg.Settings()
	s["usd_to_vnd"] = n
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return true
	}
	// Residual fix (Opus review of commit 11e033d): this used to say "New
	// orders price PayPal/Binance at this rate" — both rails are gone (the
	// USDT migration removed SePay+PayPal in this commit; Binance was cut
	// earlier). Today the rate feeds vndToUSD (usdt.go), which prices every
	// BEP20/TRON quote and every VND-denominated display conversion
	// (money.go, adminpricing.go, apikey.go, deposit.go) — not any specific
	// payment rail. The whole rate feature is slated to die in the USDT
	// migration's S5 once the ledger moves to micro-USDT; until then this
	// text must describe what the rate actually does today.
	b.send(c.Ctx, c.ChatID,
		"✅ USD→VND rate set to "+commaInt(n)+". New orders and USDT quotes price at this rate.",
		admNav("admcat:settings"))
	return true
}

// ---- adm:setkirourl (button + "setkirourl" capture) ------------------------

// kiroURLRE mirrors r"https?://[^\s/]+\S*" fullmatch (telegram_bot.py:18543):
// an absolute http(s) URL with no whitespace.
var kiroURLRE = regexp.MustCompile(`^https?://[^\s/]+\S*$`)

// kiroURLUserinfoRE strips `scheme://user:pass@` (or `scheme://user@`)
// userinfo from a Kiro-Go base URL before it may ever reach an audit row
// (task #13 Tier 2, .omc/artifacts/config-audit-spec.md §3): kiroURLRE above
// accepts userinfo in the host segment (it excludes only whitespace and "/",
// not "@"/":"), so an admin-typed URL can legitimately carry embedded
// credentials, and those must not survive into a row the audit viewer
// displays and /audit can substring-search.
//
// DIVERGES from internal/entra/redact.go:17's urlUserinfoRE
// (`://[^\s"']*?@` -> `://***@`) on purpose, not by oversight — reimplemented
// locally rather than imported since that identifier is unexported and
// handlers importing entra just for one regex would be a worse dependency
// than three lines of regex, but the PATTERN itself had to change with the
// requirement:
//   - entra's is non-greedy (`*?`) and unbounded, fine for redacting
//     free-form log lines where over-redaction costs nothing.
//   - here the HOST is load-bearing (it is the whole forensic reason this
//     field is audited — Tier 2 ranks it above the admin key precisely
//     because repointing this URL steals the key on the next call), so
//     over-redaction is a real defect, not a safe default. A non-greedy,
//     unbounded match stops at the FIRST "@" it sees, which is the query
//     string's "@" in "https://kiro.example.com/api?token=a@b" — that
//     consumes the host along with the token, leaving an audit row that
//     answers nothing about which server was configured.
//   - this version is GREEDY and slash-bounded (`[^\s"'/]*@`): the "/"
//     exclusion stops the match at the path boundary so a query-string "@"
//     is never reached, and greedy-to-slash-boundary consumes through the
//     LAST "@" in the authority, so an admin-pasted unencoded "@" inside the
//     userinfo itself (e.g. "u:p@ss@host", RFC 3986 says to percent-encode
//     that but a redactor exists precisely because admins don't always) is
//     fully redacted rather than leaving a password fragment ("ss") behind.
//
// Residual, accepted: a query string with NO path slash at all
// ("https://host?x=a@b") still mangles the host, since there is no "/" to
// bound the match before the query's "@". Full net/url parsing would close
// this but is more machinery than a Kiro-Go base URL (which always resolves
// to an origin, never a bare query) warrants.
var kiroURLUserinfoRE = regexp.MustCompile(`://[^\s"'/]*@`)

// redactKiroURL applies kiroURLUserinfoRE so a kiro_go_base_url audit row can
// never carry embedded userinfo credentials while still recording the host —
// the part that answers "which server did this admin point the bot at".
func redactKiroURL(url string) string {
	return kiroURLUserinfoRE.ReplaceAllString(url, "://***@")
}

// kiroKeyStateStr renders the Kiro-Go admin key's set/unset state using the
// plain "not set" wording py_settings_spec.md specifies for these wizard
// prompts (L7 fix). admin.go's shared setUnsetState renders "not set 🚫"
// instead — correct for its own menu-button label (admin.go:421, which this
// slice does not own and must not touch) — so this file-local variant is
// used only at the two call sites below.
func kiroKeyStateStr(isSet bool) string {
	if isSet {
		return "set ✅"
	}
	return "not set"
}

// cbSetKiroURL arms the Kiro-Go base-URL wizard — telegram_bot.py:14127.
// Super-admin only (double-gated in Python; here settingsSuperAdminOnly IS
// the gate, so no separate inline re-check is needed).
func (b *Bot) cbSetKiroURL(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetKiroURL = true
	cc.st.mu.Unlock()
	text := "🌐 KIRO-GO SERVER URL\nCurrent: " + setUnsetValue(b.kirogoURLStr()) +
		"\nAdmin key: " + kiroKeyStateStr(b.kirogoKeySet()) +
		"\n\nSend the new base URL (http:// or https://), e.g. http://localhost:8080\n" +
		"Note: customers see KIRO_GO_PUBLIC_URL from .env in their delivery instructions when it differs (e.g. this URL is localhost)."
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:settings"))
}

// capSetKiroURL is the "setkirourl" capture (#18, POP semantics —
// telegram_bot.py:18541).
func (b *Bot) capSetKiroURL(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetKiroURL
	c.st.SetKiroURL = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	url := strings.TrimRight(strings.TrimSpace(c.Text), "/")
	if !kiroURLRE.MatchString(url) {
		b.send(c.Ctx, c.ChatID, "⚠️ Not a valid URL. Send an http:// or https:// base URL, e.g. http://localhost:8080",
			admNavRetry("admcat:settings", "🌐 Kiro-Go URL", "adm:setkirourl"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text
	// (or run the connectivity probe against an unsaved URL).
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return true
	}
	// Task #13 Tier 2: capture the effective pre-change URL BEFORE it is
	// overwritten — same settings.json-over-env resolution as
	// capSetBep20/capSetTron's oldAddr, via the same getter cbSetKiroURL
	// showed the admin above.
	oldURL := b.cfg.KiroGoBaseURL()
	s := b.cfg.Settings()
	s["kiro_go_base_url"] = url
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return true
	}
	// Audit AFTER the save succeeds, never before (spec §4.3). Ranked ABOVE
	// the admin key itself (spec §2, Tier 2): repointing this URL hands the
	// admin key to whatever host is configured on the NEXT Kiro-Go call, so
	// an unaudited change here is credential exfiltration via config, not
	// merely a broken integration. Both sides go through redactKiroURL
	// first — a URL can carry user:pass@ userinfo, and the host (the
	// forensically useful part) survives the redaction.
	b.auditConfigSet(fmtInt(c.ChatID), "config_set_kiro_go_base_url", "kiro_go_base_url",
		redactKiroURL(oldURL), redactKiroURL(url))
	probe := b.kiroGoProbe(c.Ctx)
	b.send(c.Ctx, c.ChatID, "✅ Kiro-Go server URL set to "+url+"\n"+probe, admNav("admcat:settings"))
	return true
}

// kiroGoProbe is the best-effort "verified" / "could not verify"
// connectivity check py_settings_spec.md §4 runs after setkirourl/setkirokey
// save. internal/kirogo has no GET /admin/pool listing endpoint (the nearest
// Python equivalent, kiro_go_sellable_credits), so this reuses the admin
// Stats() call batchedKeyStats already relies on (apikey_keys.go) as the
// nearest "does this URL+key actually authenticate" probe.
func (b *Bot) kiroGoProbe(ctx context.Context) string {
	if b.cfg == nil || strings.TrimSpace(b.cfg.KiroGoAdminKey()) == "" {
		return "⚠️ admin key not set yet — set it via 🔑 Kiro-Go Admin Password."
	}
	client := b.kirogoClient()
	if client == nil {
		return "⚠️ could NOT verify against Kiro-Go (check the URL and admin key)."
	}
	if _, err := client.Stats(ctx, kirogo.StatsRequest{}); err != nil {
		return "⚠️ could NOT verify against Kiro-Go (check the URL and admin key)."
	}
	return "✅ verified against Kiro-Go."
}

// ---- adm:setkirokey (button + "setkirokey" capture) ------------------------

// cbSetKiroKey arms the Kiro-Go admin-password wizard — telegram_bot.py:14143.
// Super-admin only. The key is a SECRET (mirrors CLAUDE.md's upstream-key
// invariant): never echoed back or logged, only "set ✅ / not set" state.
func (b *Bot) cbSetKiroKey(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetKiroKey = true
	cc.st.mu.Unlock()
	urlDisp := b.kirogoURLStr()
	if urlDisp == "" {
		urlDisp = "— not set"
	}
	text := "🔑 KIRO-GO ADMIN PASSWORD\nCurrent: " + kiroKeyStateStr(b.kirogoKeySet()) +
		"\nServer URL (env KIRO_GO_BASE_URL): " + urlDisp +
		"\n\nSend the new admin key/password (it will be stored, never shown again):"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:settings"))
}

// capSetKiroKey is the "setkirokey" capture (#19, POP semantics —
// telegram_bot.py:18573). Only non-emptiness is validated.
func (b *Bot) capSetKiroKey(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetKiroKey
	c.st.SetKiroKey = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	// L12 known-parity note: the SetKiroKey slot is already popped above
	// BEFORE this "/"-prefix check, so a password beginning with "/" falls
	// through to the generic command router instead of being captured here.
	// This exactly matches Python's pop-then-check order
	// (py_settings_spec.md), so it is not a port defect — but it is a known,
	// accepted credential-routing quirk (the admin would need to retype a
	// key that happens to start with "/"), kept for behavioral parity rather
	// than silently diverging from the original.
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	key := strings.TrimSpace(c.Text)
	if key == "" {
		b.send(c.Ctx, c.ChatID, "⚠️ Empty key ignored. Send the Kiro-Go admin key/password.",
			admNavRetry("admcat:settings", "🔑 Kiro-Go key", "adm:setkirokey"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text.
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return true
	}
	s := b.cfg.Settings()
	// The key is written to settings and NEVER logged/echoed anywhere past
	// this point — CLAUDE.md's credential-handling invariant.
	s["kiro_go_admin_key"] = key
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return true
	}
	// Task #13 Tier 2 (spec §3.1): deliberately NOT auditConfigSet — that
	// helper's whole job is formatting an "old -> new" outcome string, and
	// for this ONE field no function of the value may ever reach a row in
	// either direction. Not the new key (a second store an admin can
	// substring-search via /audit would be a credential-leak regression
	// introduced by a security fix), not the old key (retaining a
	// superseded credential extends its blast radius past rotation — the
	// whole point of rotating is that the old value stops mattering), and
	// not a length or prefix (internal/tg/client.go:128-131's ruling,
	// adopted verbatim: "a partial token is still a partial token"). Outcome
	// is the set/unset boolean only, the same convention admin.go's own
	// kirogoKeySet already uses for its menu label — never the raw key.
	// key is non-empty here (the "" case returned above before reaching
	// this line), so the event is always "set", never "cleared". Guarded on
	// b.store != nil like every other direct LogAction call site in this
	// file (auditConfigSet's own nil guard isn't in play since this
	// deliberately bypasses that helper).
	if b.store != nil {
		b.store.LogAction(fmtInt(c.ChatID), "admin", "config_set_kiro_go_admin_key", "kiro_go_admin_key", "set", "")
	}
	probe := b.kiroGoProbe(c.Ctx)
	b.send(c.Ctx, c.ChatID, "✅ Kiro-Go admin password saved. "+probe, admNav("admcat:settings"))
	return true
}

// ---- adm:setkiroacct (button + "setkiroacct" capture) ----------------------

// kiroAcctUUIDRE mirrors the fullmatch UUID shape (telegram_bot.py:18601).
var kiroAcctUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

// cbSetKiroAcct arms the Kiro-Go account-id wizard — telegram_bot.py:14157.
// Super-admin only.
//
// Scope note (deliberate, reported to the user): Python offers a PICKER
// (kacct:<id> / kacctman / kacctv, backed by GET /admin/api/accounts plus a
// mint-then-revoke verify probe) before falling back to manual UUID entry —
// py_settings_spec.md §4. Those three callback prefixes sit outside this
// slice's 22 listed callbacks, and internal/kirogo has no accounts-listing
// endpoint to back a picker with, so this handler always takes the same
// "Kiro-Go unreachable" manual-entry branch Python itself falls back to when
// the picker can't be built.
func (b *Bot) cbSetKiroAcct(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetKiroAcct = true
	cc.st.mu.Unlock()
	text := "🆔 KIRO-GO ACCOUNT ID\nCurrent: " + setUnsetValue(b.kirogoAccountStr()) +
		"\n\nSend the account id manually:"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:settings"))
}

// capSetKiroAcct is the "setkiroacct" capture (#20, POP semantics —
// telegram_bot.py:18599, manual-entry branch).
func (b *Bot) capSetKiroAcct(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetKiroAcct
	c.st.SetKiroAcct = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	acct := strings.TrimSpace(c.Text)
	if !kiroAcctUUIDRE.MatchString(acct) {
		b.send(c.Ctx, c.ChatID, "⚠️ Not a valid account id. Expected a UUID, e.g. 22bb9057-183d-4ff6-89c4-7f230cb98b8e",
			admNavRetry("admcat:settings", "🆔 Kiro-Go Account", "adm:setkiroacct"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text.
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:settings"))
		return true
	}
	// Task #13 Tier 3: capture the effective pre-change account id BEFORE it
	// is overwritten — an identifier, not a credential (spec §3), so it is
	// logged verbatim like bep20_address/tron_address rather than
	// set/unset-only like kiro_go_admin_key.
	oldAcct := b.cfg.KiroGoAccountID()
	s := b.cfg.Settings()
	s["kiro_go_account_id"] = acct
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:settings"))
		return true
	}
	// Audit AFTER the save succeeds, never before (spec §4.3). Which
	// upstream account is billed — a wrong value mints against someone
	// else's quota, or silently fails every API-key sale.
	b.auditConfigSet(fmtInt(c.ChatID), "config_set_kiro_go_account_id", "kiro_go_account_id", oldAcct, acct)
	text := "✅ Kiro-Go account id set to " + acct + "\n"
	if b.kiroGoConfigured() {
		text += "API-key selling is now configured."
	} else {
		text += "⚠️ Still incomplete — set the Kiro-Go URL and admin key too."
	}
	b.send(c.Ctx, c.ChatID, text, admNav("admcat:settings"))
	return true
}

// ---- adm:togglepay:<method> -------------------------------------------------

// payMethodNames mirrors PAY_METHODS' method-name column (telegram_bot.py:5147).
// "qr" (SePay/VietQR), "paypal" and "binance" (Binance Pay) were removed by
// the USDT-only migration (S1-S3) — the shop takes USDT via BEP20/TRON (and
// the wallet balance) only.
var payMethodNames = []string{"balance", "bep20", "tron"}

// depositMethodNames is the subset of payMethodNames that can actually put
// NEW money into the system — a customer sends on-chain USDT and it lands in
// their wallet. "balance" is deliberately excluded: it SPENDS money already
// on deposit, it can never fund anything, so it must not count toward "the
// shop can still take money" (see cbTogglePay's guard below).
var depositMethodNames = []string{"bep20", "tron"}

// cbTogglePay is the shared handler for all 3 adm:togglepay:<method> buttons
// — telegram_bot.py:13931. Super-admin only. Refuses to leave zero WORKING
// deposit rails — a rail only counts as "working" when it is BOTH
// admin-enabled AND Configured() (see the WORKING-RAIL note below);
// disabling "balance" is never blocked, since balance alone cannot fund the
// shop either way.
//
// The guard used to be "at least one of all 3 methods must stay on", which
// "balance" alone satisfied — an admin could disable both bep20 and tron
// (balance still on) and the guard waved it through, reaching exactly the
// unfundable-shop state this is supposed to prevent, just via the admin
// panel instead of a bad restart default. See
// TestTogglePayRefusesLastDepositRail, which fails if this regresses to
// counting "balance" toward the guard again.
//
// WORKING-RAIL NOTE (2nd-round fix): the first version of this guard checked
// only payEnabled(m), matching every OTHER call site's pattern except the
// render sites (cmdDeposit, the admin payments menu, the buy screen), which
// all additionally gate on <rail>.Configured(). Since Fix 1 to settings.go
// made PayEnabled default every method ON, an unconfigured rail (no
// BEP20_ADDRESS/TRON_ADDRESS set, the common single-chain shop) now reads
// "enabled" while being invisible to every customer — so the old guard would
// count it as "another deposit rail" and wave through disabling the ONE
// rail that actually works. Both bep20Configured/tronConfigured must be
// consulted here too, or the two independently-correct fixes reopen the
// exact hole between them.
//
// EDGE CASE: the guard only fires when `method` ITSELF is a working rail
// today (enabled AND configured). If `method` is enabled-but-unconfigured,
// toggling it changes nothing about the shop's real fundability — the shop
// was already dark on that rail regardless of the setting — so blocking the
// toggle would be noise, not protection, and an admin cleaning up a
// half-configured instance couldn't turn the dead flag off. See
// TestTogglePayAllowsDisablingUnconfiguredDepositRail and
// TestTogglePayRefusesLastConfiguredDepositRail (the TRON-only-shop
// scenario this round's review reproduced).
//
// Unlike every other handler in this file, a successful toggle RE-RENDERS
// the payments submenu in place rather than sending a terminal message
// (py_settings_spec.md §4/§7).
func (b *Bot) cbTogglePay(cc *cbCtx, method string) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	if !slices.Contains(payMethodNames, method) {
		// Unknown method: a stale adm:togglepay:paypal/binance button tapped
		// from a menu rendered before the USDT migration (S1-S3) removed
		// those rails. admin.go:882's dispatcher already deleted the message
		// that held the tap before we got here (command.go:182), so silently
		// returning (the pre-fix behavior) left the super-admin staring at a
		// chat with the menu gone and no explanation — same defect class as
		// the dep: fall-through fixed for customers in deposit.go's cbDep.
		b.send(cc.Ctx, cc.ChatID,
			"⚠️ That payment method no longer exists.",
			admNav("admcat:payments"))
		return
	}
	if b.cfg == nil {
		b.send(cc.Ctx, cc.ChatID, settingsNotConfigured, admNav("admcat:payments"))
		return
	}
	// railConfigured mirrors every render site's payEnabled(m) &&
	// <rail>.Configured() gate — see the WORKING-RAIL note above. Only
	// bep20/tron have a Configured() concept; "balance" never reaches this
	// map since it's excluded from depositMethodNames below.
	//
	// Both lookups below go through the two-value form (f, ok := map[key])
	// rather than a bare map[key](), even though every CURRENT
	// depositMethodNames entry has a map entry by construction: a bare lookup
	// on a key the map doesn't hold returns a nil func value, and calling a
	// nil func panics the whole handler on an admin's tap. depositMethodNames
	// and this map are two separately-maintained literals a few lines apart —
	// a third on-chain rail added to one and not the other would panic here
	// instead of failing a test, which is the wrong way to find out.
	//
	// An unmapped rail (ok==false) is treated as "not working" in BOTH roles
	// below, which is asymmetric in effect: as an OTHER rail in the loop it's
	// conservative (undercounts working rails, so the guard is if anything
	// MORE likely to refuse a toggle — a false refusal, not a money-safety
	// gap); as the toggle TARGET itself, ok==false skips the guard entirely
	// and the toggle proceeds unblocked. That second case is intentional, not
	// an oversight: it's the same "an unmapped/unconfigured rail was never
	// counted as working, so blocking its toggle is noise" rationale the
	// EDGE CASE note above already documents for enabled-but-unconfigured
	// bep20/tron. It only matters in practice if depositMethodNames grows a
	// rail this map doesn't — a state the comment above already flags as a
	// bug to avoid shipping, not a state to design further defenses around.
	railConfigured := map[string]func() bool{"bep20": b.bep20Configured, "tron": b.tronConfigured}
	cur := b.payEnabled(method)
	methodWorking, ok := railConfigured[method]
	if cur && slices.Contains(depositMethodNames, method) && ok && methodWorking() {
		anyOtherWorking := false
		for _, m := range depositMethodNames {
			if f, ok := railConfigured[m]; m != method && ok && b.payEnabled(m) && f() {
				anyOtherWorking = true
				break
			}
		}
		if !anyOtherWorking {
			b.send(cc.Ctx, cc.ChatID,
				"⚠️ Can't disable the last deposit method — at least one must stay on so customers can fund their wallet.",
				backKB("admcat:payments"))
			return
		}
	}
	s := b.cfg.Settings()
	s["pay_"+method] = !cur
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:payments"))
		return
	}
	b.renderPaymentsMenu(cc.Ctx, cc.ChatID)
}

// renderPaymentsMenu RE-RENDERS the payments submenu in place — the ONLY
// handler in this whole file that does so; every other toggle/setter sends a
// fresh terminal message instead. The title is deliberately double-emoji:
// "🔑 " + "💳 PAYMENT METHODS" (verbatim f"🔑 {title}", py_settings_spec.md
// §4, matching cbAdmCat's own category-open rendering in admin.go).
func (b *Bot) renderPaymentsMenu(ctx context.Context, chatID int64) {
	title, rows, ok := b.adminCategoryMenu(chatID, "payments")
	if !ok {
		return
	}
	b.sendMenu(ctx, chatID, "🔑 "+title, rows)
}

// ---- adm:setbep20 (button + "setbep20" capture) ----------------------------

// cbSetBep20 arms the BEP20 address wizard — telegram_bot.py:14193.
// Super-admin only (via the upstream cap gate; no inline re-check in
// Python either, matching this handler's reliance on settingsSuperAdminOnly).
func (b *Bot) cbSetBep20(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetBep20 = true
	cc.st.mu.Unlock()
	text := "🟡 Current USDT BEP20 address:\n" + setUnsetValue(b.bep20AddrStr()) +
		"\n\nSend the new BSC (BEP20) receiving address (0x… 40 hex):"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:payments"))
}

// auditOldClipRunes bounds the UNVALIDATED "old" side of an audit outcome.
// newVal always passed bep20.ValidAddress/tron.ValidAddress before reaching
// auditOldNew, so it is capped at ~42 runes by construction — but old comes
// straight from BEP20_ADDRESS/TRON_ADDRESS (env, or a settings.json value a
// past admin typed), and nothing in this codebase validates its shape. An
// oversized old value could otherwise push the formatted outcome past
// LogAction's silent 500-rune clip (store/misc.go's truncRunes), cutting off
// the " → <new>" tail entirely — a row with no arrow and no destination
// address left in it, on the one field where getting the destination right
// matters most. 64 runes is generously past any realistic address/URL while
// still leaving the clip itself legible.
const auditOldClipRunes = 64

// auditOldNew formats a config mutator's "old → new" forensic payload for
// LogAction's outcome field (task #13 Tier 1, .omc/artifacts/config-audit-spec.md
// §4.1). A never-before-set field renders as "(unset)" rather than an empty
// string, so a first-ever /setbep20 or /settron doesn't read as "the address
// was blank" — it must read as "there was no address at all". old is clipped
// defensively per auditOldClipRunes above; newVal is not re-clipped here
// since every current caller already validated it before calling in.
func auditOldNew(old, newVal string) string {
	if old == "" {
		old = "(unset)"
	} else if r := []rune(old); len(r) > auditOldClipRunes {
		old = string(r[:auditOldClipRunes]) + "…"
	}
	return old + " → " + newVal
}

// auditConfigSet is the shared "config_set"-shaped audit row every Tier 2-5
// config mutator in this file and adminpricing.go reduces to (task #13,
// .omc/artifacts/config-audit-spec.md §4.1): actor + "admin", action, target,
// old→new via auditOldNew, empty detail. Tier 1 (capSetBep20/capSetTron) was
// written before this helper existed and is left untouched — its shape is
// identical, just inlined; not touching already-shipped, already-tested code
// for a pure refactor. b.store==nil (unit tests with no store wired) is a
// silent no-op, matching every existing LogAction call site's own nil guard.
//
// SECRET WARNING: never pass a secret as oldVal/newVal — outcome, unlike
// detail, IS displayed by the audit viewer. kiro_go_admin_key is the one
// Tier 2-5 field this applies to: it must be audited as a set/unset boolean
// only, via kirogoKeySet, the same convention admin.go:945 already uses for
// its own menu label, never routed through this helper with the raw key
// text.
func (b *Bot) auditConfigSet(actorChat, action, target, oldVal, newVal string) {
	if b.store == nil {
		return
	}
	b.store.LogAction(actorChat, "admin", action, target, auditOldNew(oldVal, newVal), "")
}

// capSetBep20 is the "setbep20" capture (#21, POP semantics —
// telegram_bot.py:18625).
func (b *Bot) capSetBep20(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetBep20
	c.st.SetBep20 = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	addr := strings.TrimSpace(c.Text)
	if !bep20.ValidAddress(addr) {
		b.send(c.Ctx, c.ChatID, "⚠️ Not a valid BSC/BEP20 address. Send a 0x… address (40 hex chars).",
			admNavRetry("admcat:payments", "🟡 Set address", "adm:setbep20"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text.
	// This is the exact scenario H1 calls out: an admin who types a new
	// BEP20 payout address must never see a success message when the write
	// didn't land, since every customer would keep paying the stale wallet.
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:payments"))
		return true
	}
	// Tier 1 (task #13): capture the effective pre-change value BEFORE it is
	// overwritten — cfg.BEP20Address() (settings.json override, falling back
	// to env), the SAME resolution cbSetBep20 already shows the admin as
	// "Current ... address" above. This deliberately deviates from spec
	// §4.4's literal "read from the s map already in hand": a bare map read
	// would report "(unset)" for a shop whose address was only ever supplied
	// via env, which is a wrong "old" value, not merely an imprecise one.
	// Task #17 (fixed): the customer pay screen (usdt.go's sendUSDTScreen) and
	// the by-hash verify path (b.bep20ByHash, dispatch.go) now both resolve
	// through b.bep20AddrStr() -> cfg.BEP20Address(), the SAME getter used
	// here — so this "old" value is not merely the shop's notional source of
	// truth, it is the actual live customer-facing address a write here
	// replaces.
	oldAddr := b.cfg.BEP20Address()
	s := b.cfg.Settings()
	s["bep20_address"] = addr
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:payments"))
		return true
	}
	// Audit AFTER the save succeeds, never before (spec §4.3): a row written
	// on the failure path would record a payout-address change that never
	// actually happened. This row proves WHO changed the CONFIGURED
	// bep20_address, FROM WHAT to WHAT, and WHEN — and, since task #17 closed
	// the config->pay-screen gap (see the oldAddr comment above), that
	// CONFIGURED address is now also the LIVE one every customer is shown.
	// Config mutators of this class are exactly why Tier 1 exists — an
	// unaudited change to the configured address is how a shop's incoming
	// BEP20 funds get silently redirected — so old→new (both PUBLIC, safe to
	// log verbatim per spec §3) is the row's whole payload, and is the only
	// record of which addresses were ever configured and when.
	if b.store != nil {
		b.store.LogAction(fmtInt(c.ChatID), "admin", "config_set_bep20_address",
			"bep20_address", auditOldNew(oldAddr, addr), "")
	}
	b.send(c.Ctx, c.ChatID,
		"✅ USDT BEP20 address set:\n"+addr+"\n\nEnable it under 💳 Payment methods to show it at checkout.",
		admNav("admcat:payments"))
	return true
}

// ---- adm:settron (button + "settron" capture) ------------------------------

// cbSetTron arms the TRON address wizard — telegram_bot.py:14201. Super-admin
// only.
func (b *Bot) cbSetTron(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.SetTron = true
	cc.st.mu.Unlock()
	text := "🔴 Current USDT TRC20 (TRON) address:\n" + setUnsetValue(b.tronAddrStr()) +
		"\n\nSend the new TRON (TRC20) receiving address (T… 34 chars):"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:payments"))
}

// capSetTron is the "settron" capture (#22, POP semantics —
// telegram_bot.py:18643).
func (b *Bot) capSetTron(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.SetTron
	c.st.SetTron = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	addr := strings.TrimSpace(c.Text)
	if !tron.ValidAddress(addr) {
		b.send(c.Ctx, c.ChatID, "⚠️ Not a valid TRON/TRC20 address. Send a T… address (34 base58 chars).",
			admNavRetry("admcat:payments", "🔴 Set address", "adm:settron"))
		return true
	}
	// H1 fix: a nil cfg or a failed SaveSettings must stop here with an
	// explicit failure reply — never fall through to the ✅ success text.
	if b.cfg == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:payments"))
		return true
	}
	// Tier 1 (task #13): capture the effective pre-change value BEFORE it is
	// overwritten — cfg.TRONAddress() (settings.json override, falling back
	// to env). Same deliberate §4.4 deviation as capSetBep20 above. Task #17
	// (fixed): this is also now the LIVE customer-facing address — see the
	// matching comment on capSetBep20's oldAddr above.
	oldAddr := b.cfg.TRONAddress()
	s := b.cfg.Settings()
	s["tron_address"] = addr
	if err := b.cfg.SaveSettings(s); err != nil {
		log.Println("adminsettings: save failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:payments"))
		return true
	}
	// Audit AFTER the save succeeds, never before (spec §4.3). Same reasoning
	// as capSetBep20 above: this row proves WHO changed the CONFIGURED
	// tron_address, FROM WHAT to WHAT, and WHEN, and — since #17 closed the
	// config->pay-screen gap — that address is also the live one every
	// customer is shown. Config mutators of this class are exactly why Tier 1
	// exists — an unaudited change to the configured address is how a shop's
	// incoming TRON funds get silently redirected — so old→new (both PUBLIC)
	// is the row's whole payload, and is the only record of every configured
	// address.
	if b.store != nil {
		b.store.LogAction(fmtInt(c.ChatID), "admin", "config_set_tron_address",
			"tron_address", auditOldNew(oldAddr, addr), "")
	}
	b.send(c.Ctx, c.ChatID,
		"✅ USDT TRC20 (TRON) address set:\n"+addr+"\n\nEnable it under 💳 Payment methods to show it at checkout.",
		admNav("admcat:payments"))
	return true
}

// ---- adm:broadcast: text-or-photo collector -> admdo:confirm/cancel -------

// cbBroadcastStart arms the one-field ADMIN_ACTIONS collector for
// action=="broadcast" — admin_start_action (telegram_bot.py:17234), entered
// via the adm: dispatch's ADMIN_ACTIONS branch (telegram_bot.py:14318).
// Super-admin only.
func (b *Bot) cbBroadcastStart(cc *cbCtx) {
	if !b.settingsSuperAdminOnly(cc) {
		return
	}
	cc.st.mu.Lock()
	cc.st.Admin = &AdminAction{Action: "broadcast", Step: "collecting"}
	cc.st.mu.Unlock()
	text := "🔧 Broadcast\nEnter the message to send to ALL customers — or send a photo (optional caption) to broadcast an image (super admin only):"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:settings"))
}

// capAdminInput is the step-9 "admininput" stage (CanonicalAdminTextOrder
// #10, admin.go) — the TEXT half of the generic ADMIN_ACTIONS collector
// (admin_collect_input, telegram_bot.py:17243-17273). "broadcast"
// (adminsettings.go) and "addbal" (adminusers.go, M3/M4 fix — folded in from
// its own former slot/executor) are both ported; a future ADMIN_ACTIONS
// member extends the switch on armed.Action the same way.
//
// M4 fix: Python's admin_collect_input is a MEMBERSHIP check
// (`pending_admin.get(chat_id)`), not a "only while collecting" gate — once
// armed, EVERY subsequent qualifying text is consumed, including while
// Step=="confirm" awaiting Confirm/Cancel (telegram_bot.py:17252-17255
// explicitly swallows rather than "indexing past the field list"). The old
// addbal-specific capAddBal instead went INERT at Step=="confirm" (`return
// false`), so a stray message fell through to whatever capture came next in
// the chain — e.g. a buy-qty prompt armed at the same time would swallow a
// typed "3" meant for nothing. This function now swallows (returns true) for
// ANY non-nil armed wizard once the nav/slash-command carve-out is cleared,
// regardless of Step.
func (b *Bot) capAdminInput(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Admin
	// M3 fix (entry-gate half): read Step WHILE holding the lock too —
	// capBroadcastPhoto can flip this same *AdminAction's Step to "confirm"
	// on a different pool worker at any time, so checking armed.Step after
	// unlocking would race exactly like the sendBroadcastConfirm read below
	// used to.
	step := ""
	if armed != nil {
		step = armed.Step
	}
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if step == "confirm" {
		return true // M4 fix: swallow — awaiting Confirm/Cancel, not more field input
	}
	switch armed.Action {
	case "broadcast":
		c.st.mu.Lock()
		if c.st.Admin == armed { // still the same wizard instance
			armed.Message = strings.TrimSpace(c.Text)
			armed.Step = "confirm"
		}
		// M3 fix: snapshot Message/PhotoID into locals WHILE holding the
		// lock. capBroadcastPhoto can write these same fields for this same
		// chat on a different pool worker; reading them off the shared
		// *AdminAction after unlocking (the old code passed `armed` itself
		// into sendBroadcastConfirm) races with that write under -race.
		msg, photoID := armed.Message, armed.PhotoID
		c.st.mu.Unlock()
		b.sendBroadcastConfirm(c.Ctx, c.ChatID, msg, photoID)
		return true
	case "addbal":
		return b.capAddBalStep(c, armed, step)
	default:
		return true // armed but an unrecognized action: still swallow (M4 rationale)
	}
}

// capBroadcastPhoto is a step-5 file intercept (b.fileIntercepts) — the PHOTO
// half of the broadcast collector (admin_collect_broadcast_photo,
// telegram_bot.py:17276). Runs for EVERY chat, before the step-9 admin gate
// (photo messages carry no text and never reach the admin-only text stages),
// so it re-checks super-admin itself rather than trusting an earlier gate —
// py_settings_spec.md §3/§5's explicit note that the photo path needs its
// own check since it skips the callback gate entirely.
//
// File intercepts run at step 5 of Process (dispatch.go), BEFORE c.st is
// populated (that happens at step 7+, past the text-only gate a photo
// message never reaches) — so this fetches the ChatState itself via
// b.states.Get rather than touching the nil c.st.
func (b *Bot) capBroadcastPhoto(c *Ctx) bool {
	if c.Msg == nil || len(c.Msg.Photo) == 0 {
		return false
	}
	st := b.states.Get(c.ChatID)
	st.mu.Lock()
	armed := st.Admin
	// M3 fix (entry-gate half, same rationale as capAdminInput above): read
	// Action/Step WHILE holding the lock so this doesn't race capAdminInput
	// flipping Step to "confirm" on a different pool worker.
	eligible := armed != nil && armed.Action == "broadcast" && armed.Step == "collecting"
	st.mu.Unlock()
	if !eligible {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		b.send(c.Ctx, c.ChatID, "⚠️ Image broadcast is super-admin only — send text instead.", backKB("admcat:settings"))
		return true
	}
	// Largest rendition is last in Telegram's photo size array.
	largest := c.Msg.Photo[len(c.Msg.Photo)-1]
	st.mu.Lock()
	if st.Admin == armed {
		armed.PhotoID = largest.FileID
		armed.Message = strings.TrimSpace(c.Msg.Caption)
		armed.Step = "confirm"
	}
	// M3 fix: snapshot under lock — see capAdminInput's matching comment.
	msg, photoID := armed.Message, armed.PhotoID
	st.mu.Unlock()
	b.sendBroadcastConfirm(c.Ctx, c.ChatID, msg, photoID)
	return true
}

// sendBroadcastConfirm renders the Confirm/Cancel summary shared by the text
// and photo collectors (telegram_bot.py:17243-17276, py_settings_spec.md §5
// steps 2-3). Takes plain message/photoID values rather than the shared
// *AdminAction pointer (M3 fix): both callers now snapshot these fields
// under st.mu before calling, so this function never reads memory another
// goroutine could be concurrently writing.
func (b *Bot) sendBroadcastConfirm(ctx context.Context, chatID int64, message, photoID string) {
	var summary string
	if photoID != "" {
		caption := message
		if caption == "" {
			caption = "(none)"
		}
		summary = "🔧 Broadcast — confirm:\n• photo: attached ✅\n• caption: " + caption
	} else {
		summary = "🔧 Broadcast — confirm:\n• message: " + message
	}
	rows := [][]tg.InlineKeyboardButton{
		btnRow("✅ Confirm", "admdo:confirm"),
		btnRow("❌ Cancel", "admdo:cancel"),
	}
	b.send(ctx, chatID, summary, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// cbAdminDoConfirm executes the armed ADMIN_ACTIONS wizard — admdo:confirm
// (telegram_bot.py:15613) -> admin_execute's per-action branch (§5 step 5).
// Gated by plain isAdmin, matching Python (py_settings_spec.md §3): a
// standard admin can never actually reach a confirmed "broadcast" wizard
// here anyway, since adm:broadcast itself is denied upstream by
// settingsSuperAdminOnly — same for "addbal" (adminusers.go's cbAddBal is
// gated by fullAdminOnly). M3/M4 fix: "addbal" used to have its OWN
// admdo:addbalconfirm route/handler; it now shares this literal
// admdo:confirm route and switch case, restoring Python's exact callback
// strings.
func (b *Bot) cbAdminDoConfirm(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.mu.Lock()
	action := cc.st.Admin
	// H2 fix (Opus review): only pop the wizard when it is actually AT the
	// confirm step. Nil-ing cc.st.Admin unconditionally here let a STALE
	// admdo:confirm tap (an abandoned flow's old inline keyboard) silently
	// destroy a freshly-armed, still-in-progress wizard with no message at
	// all — leaving it untouched when Step != "confirm" is what actually
	// prevents that, not reordering which line nils it.
	if action != nil && action.Step == "confirm" {
		cc.st.Admin = nil
	}
	cc.st.mu.Unlock()
	if action == nil || action.Step != "confirm" {
		return
	}
	switch action.Action {
	case "broadcast":
		b.execBroadcast(cc.Ctx, cc.ChatID, action)
	case "addbal":
		b.execAddBal(cc.Ctx, cc.ChatID, action.Target, action.Amount)
	}
}

// cbAdminDoCancel abandons the armed wizard — admdo:cancel
// (telegram_bot.py:15614). Goes all the way to admroot, not admcat:settings,
// matching Python's _adm_nav("admroot") on this specific reply.
func (b *Bot) cbAdminDoCancel(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.mu.Lock()
	cc.st.Admin = nil
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "❌ Cancelled.", admNav("admroot"))
}

// execBroadcast fans the confirmed message/photo out to every chat the store
// has ever seen — admin_execute's "broadcast" branch (telegram_bot.py:17356,
// py_settings_spec.md §5). Runs on the tracked worker registry with a FRESH
// cancellable context (workers.go's runOnStoppableCtx, itself modeled on the
// otp.go:415-427 / deposit.go:234 background-work pattern) so a slow fan-out
// to many chats can be interrupted by Shutdown() instead of leaking, and so
// the send loop never blocks the update goroutine that queued it.
func (b *Bot) execBroadcast(ctx context.Context, chatID int64, a *AdminAction) {
	if b.store == nil {
		b.send(ctx, chatID, "📣 Broadcast sent to 0 chats (0 failed).", admNav("admcat:settings"))
		return
	}
	ids, err := b.store.AllChatIDs()
	if err != nil {
		b.send(ctx, chatID, "⚠️ Could not read the recipient list.", admNav("admcat:settings"))
		return
	}
	msg, photo := a.Message, a.PhotoID
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			sent, fail := b.runBroadcast(fctx, ids, msg, photo)
			// Logged here, inside the tracked worker, because sent/fail are
			// only known once the fan-out returns — see auditBroadcast below
			// for why this row exists at all.
			b.auditBroadcast(chatID, sent, fail, photo != "", msg)
			b.send(fctx, chatID, "📣 Broadcast sent to "+itoa(sent)+" chats ("+itoa(fail)+" failed).", admNav("admcat:settings"))
		})
	})
}

// broadcastOutcomeExcerptRunes caps the free-text excerpt embedded in a
// broadcast's audit outcome. outcome is the field sendAuditLog actually
// prints (auditConfigSet's SECRET WARNING above notes the same split), and
// LogAction itself silently clips the whole outcome string to 500 runes — an
// uncapped admin message could alone eat that entire budget and crowd out
// the sent/fail counts that make the row useful at a glance. 200 runes is
// generous enough to recognize which broadcast a customer complaint is
// about while leaving headroom for the counts and photo marker ahead of it.
const broadcastOutcomeExcerptRunes = 200

// auditBroadcast records a broadcast fan-out (task #14). Before this, the
// single highest blast-radius action available to an admin — a message sent
// to every chat the store has ever seen — left NO record anywhere: not who
// sent it, not what it said, not how many people actually received it.
// auditConfigSet's shape (old → new) does not fit a broadcast, which is not
// a config transition, so this calls store.LogAction directly, the same
// primitive auditConfigSet itself wraps.
//
// The body is split deliberately, not arbitrarily: outcome is the field
// sendAuditLog actually displays (confirmed by reading it — the loop only
// ever prints r.Outcome), so it carries the sent/fail counts first — the
// cheapest, always-useful signal, even for a caption-less photo broadcast —
// followed by a short excerpt so an investigator can identify which
// broadcast a complaint concerns without opening the DB directly. detail is
// NOT display-exempt, though: RecentAudit's WHERE clause matches
// actor_chat, target, action, detail, actor_kind, and outcome alike
// (misc.go:51-53), so it is searched even though it is never shown. The
// practical cost of that: an /audit <query> for a term with no relation to
// broadcasts can still match text buried inside a stored broadcast body and
// surface the broadcast row as a false positive in an investigator's
// filter — noise, not a leak. detail still carries the FULL body (still
// bounded by LogAction's own 2000-rune clip), because storing the complete
// text there loses nothing visible today and preserves it for a future
// export/inspection path instead of discarding it a second time on top of
// outcome's 200-rune excerpt. No redaction is applied: an admin-authored
// broadcast body is not a secret like kiro_go_admin_key (auditConfigSet's
// SECRET WARNING) — it is text already deliberately sent to every customer,
// so nothing about it is hidden by also keeping it in this row.
func (b *Bot) auditBroadcast(actorChat int64, sent, fail int, hasPhoto bool, body string) {
	outcome := "sent=" + itoa(sent) + " fail=" + itoa(fail)
	if hasPhoto {
		outcome += " photo"
	}
	if body != "" {
		excerpt := body
		if r := []rune(excerpt); len(r) > broadcastOutcomeExcerptRunes {
			excerpt = string(r[:broadcastOutcomeExcerptRunes]) + "…"
		}
		outcome += " · " + excerpt
	}
	b.store.LogAction(fmtInt(actorChat), "admin", "broadcast_send", "all_chats", outcome, body)
}

// runBroadcast is the per-recipient send loop itself — no rate limit, no
// progress updates, a per-recipient failure swallowed and counted rather than
// aborting the batch (telegram_bot.py's broadcast try/except, §5). Photo
// re-sends reuse the Telegram file_id directly (no re-upload per recipient),
// via SendPhotoURLOpts — Telegram's sendPhoto "photo" field accepts a
// file_id exactly as it accepts a URL, so the same client method serves both.
func (b *Bot) runBroadcast(ctx context.Context, ids []string, msg, photo string) (sent, fail int) {
	for _, idStr := range ids {
		cid, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			fail++
			continue
		}
		var sendErr error
		if photo != "" {
			p := tg.SendPhotoURLParams{ChatID: cid, Photo: photo}
			if msg != "" {
				p.Caption = msg
			}
			_, sendErr = b.tg.SendPhotoURLOpts(ctx, p)
		} else {
			_, sendErr = b.tg.SendMessage(ctx, tg.SendMessageParams{ChatID: cid, Text: msg})
		}
		if sendErr != nil {
			fail++
		} else {
			sent++
		}
	}
	return sent, fail
}

// ---- registration ------------------------------------------------------------

// registerAdminSettings wires this file's 7 captures + the step-9 broadcast
// text collector + the step-5 broadcast photo intercept + the admdo:
// confirm/cancel routes into the ordered dispatch tables. Called from New
// (dispatch.go) AFTER registerCodeCapture, so insertCapturesBefore("code", …)
// finds "code" already spliced into the chain and lands this file's 7
// captures immediately ahead of it: their canonical positions (setrate #10
// through setmaxqty #23, captures.go) are all earlier than code's (#38-ish),
// so the resulting order stays an in-order subsequence of
// CanonicalCaptureOrder (handlers-go.md §7 / go_structure.md §2).
func (b *Bot) registerAdminSettings() {
	b.insertCapturesBefore("code",
		stage{name: "setrate", fn: (*Bot).capSetRate},
		stage{name: "setkirourl", fn: (*Bot).capSetKiroURL},
		stage{name: "setkirokey", fn: (*Bot).capSetKiroKey},
		stage{name: "setkiroacct", fn: (*Bot).capSetKiroAcct},
		stage{name: "setbep20", fn: (*Bot).capSetBep20},
		stage{name: "settron", fn: (*Bot).capSetTron},
		stage{name: "setmaxqty", fn: (*Bot).capSetMaxQty},
	)
	b.adminText = append(b.adminText,
		stage{name: "admininput", fn: (*Bot).capAdminInput}, // #10 (last) @17213
	)
	b.fileIntercepts = append(b.fileIntercepts,
		stage{name: "broadcastphoto", fn: (*Bot).capBroadcastPhoto},
	)
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "admdo:confirm", exact: true, fn: (*Bot).cbAdminDoConfirm}, // @15613
		cbRoute{key: "admdo:cancel", exact: true, fn: (*Bot).cbAdminDoCancel},   // @15614
	)
}
