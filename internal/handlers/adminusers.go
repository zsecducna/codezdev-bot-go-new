package handlers

// adminusers.go — 👥 CUSTOMERS category, 🛡️ ADMIN ROLES category, the
// private-sale WHITELIST submenu, and the authorized RESELLERS submenu
// (py_customers_spec.md is the exact behavioural/verbatim-string reference for
// every callback below, keyed off telegram_bot.py). All these callbacks used
// to fall through cbAdmAction's default branch (or cbAdmTodo for the
// standalone resmng:/resdisc:/rmreseller:/rmwl: prefixes) to the generic
// "not wired up yet" notice — this file replaces each stub with real
// behaviour.
//
// M3/M4 fix (was a task-specific file-ownership note, no longer applicable —
// this task's instructions now grant ownership of the whole handlers
// package): Python's adm:addbal reuses the SAME generic pending_admin /
// admin_collect_input / admdo:confirm mechanism as adm:broadcast
// (adminsettings.go's capAdminInput / cbAdminDoConfirm). addbal used to get
// its OWN two-field collector (ChatState.AddBal) and its OWN confirm/cancel
// callback_data — admdo:addbalconfirm / admdo:addbalcancel — specifically
// because an earlier task's file-ownership split forbade extending that
// shared executor. That constraint is gone: cbAddBal now arms the shared
// cc.st.Admin slot (AdminAction, pending.go) and capAddBalStep (below) plugs
// into adminsettings.go's capAdminInput/cbAdminDoConfirm switch, so
// admdo:confirm/admdo:cancel are Python's LITERAL callback strings again.
//
// Permission model (py_customers_spec.md §0): adm:custlookup needs capability
// customers_view; adm:addwl / rmwl: need toggle_sales — both are grantable to
// a standard admin in principle. H1 fix (admincap.go): a standard admin's
// fixed default capability set is {stock_read, hold_restore, audit,
// toggle_sales} — so adm:addwl/rmwl: (toggle_sales) ARE reachable by a
// standard admin (matching adm:togglesales/adm:toggleprivate), while
// adm:custlookup (customers_view) is NOT, and is now actually enforced by the
// central command.go chokepoint before this file's own handler ever runs.
// EVERYTHING ELSE here (addbal, all three admin-roles actions, the entire
// resellers subtree) is full-admin/super-admin only, gated by fullAdminOnly
// below (and, independently, by the same chokepoint).
import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// errCfgNotConfigured marks a whitelist/reseller write attempted with no
// *config.Config wired (b.cfg == nil) — unit tests, or a partially set up
// install. Distinguished from a SaveSettings failure so the admin sees the
// right message (settingsNotConfigured vs settingsSaveFailed, both reused
// from adminsettings.go — same package, no edit needed there).
var errCfgNotConfigured = errors.New("adminusers: no config wired")

// whitelistMax / resellerMax mirror WHITELIST_MAX (telegram_bot.py:11539) and
// RESELLER_MAX (telegram_bot.py:11289) — the list-length caps enforced by
// addWhitelist/addReseller below.
const (
	whitelistMax = 100
	resellerMax  = 20
)

// fullAdminOnly is this file's own twin of adminsettings.go's
// settingsSuperAdminOnly gate (see file header: that file is owned by a
// concurrent slice per this task, so it is a parallel implementation, not a
// shared call), parametrized by the category's own Back destination so a
// denial always lands the admin back where they tapped from. Two-tier,
// mirroring Python's layered checks: a plain non-admin gets SILENCE (the
// resmng:/resdisc:/rmreseller:/admdo:addbal* routes sit OUTSIDE the adm:
// prefix's blanket isAdmin gate, so they need their own — telegram_bot.py's
// `if not is_admin(chat_id): return`); a standard (granted-but-not-super)
// admin gets the loud "⛔ Not permitted." refusal — the actual capability
// denial (telegram_bot.py:12411-12415).
func (b *Bot) fullAdminOnly(cc *cbCtx, back string) bool {
	if !b.isAdmin(cc.ChatID) {
		return false
	}
	if b.isSuperAdmin(cc.ChatID) {
		return true
	}
	b.send(cc.Ctx, cc.ChatID, settingsPermissionDenied, admNav(back))
	return false
}

// resolveCustomerTarget resolves a typed chat_id-or-@username to a bare
// chat_id string, or "" if it can't be resolved — the shared target-lookup
// logic custlookup (telegram_bot.py:18208-18213) and addbal
// (telegram_bot.py:17306-17311) both use verbatim. A leading run of '-'
// (negative group-chat ids) is tolerated before the digit check, matching
// Python's `target.lstrip("-").isdigit()`.
func (b *Bot) resolveCustomerTarget(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	if isDigits(strings.TrimLeft(t, "-")) {
		return t
	}
	if b.store == nil {
		return ""
	}
	resolved, err := b.store.ChatIDForHandle(t)
	if err != nil {
		return ""
	}
	return resolved
}

// ============================================================================
// 1. adm:custlookup — 🔎 Look up customer (customers_view; open to any admin)
// ============================================================================

// cbCustLookup arms the customer-lookup capture — adm:custlookup
// (telegram_bot.py:14275-14281). READ-ONLY: the profile screen this produces
// has no action buttons beyond Back (py_customers_spec.md §1) — do not add
// any here.
func (b *Bot) cbCustLookup(cc *cbCtx) {
	cc.st.mu.Lock()
	cc.st.CustLookup = true
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "🔎 Send the customer's chat_id or @username to look up:",
		backKB("admcat:customers"))
}

// capCustLookup is the "custlookup" capture (#5, POP semantics —
// telegram_bot.py:18205-18224): abort-on-attempt, not stay-armed — any
// qualifying text (including one that fails to resolve) consumes the slot, so
// a second line after a failed lookup is NOT retried automatically; the admin
// must retap 🔎 Look up customer.
//
// M5 fix: re-checks isAdmin after popping the slot (matching capAddAdmin/
// capRmAdmin's precedent) — captureChain (step 10) runs for EVERY chat, not
// just admins, so an armed slot alone doesn't prove the tapper is still an
// admin: a super-admin can revoke a standard admin's grant AFTER they armed
// custlookup but BEFORE they typed the target, and this stage must not then
// hand back a wallet balance + order history to a now-plain chat. customers_
// view is a grantable capability (admincap.go), so the check is isAdmin, not
// isSuperAdmin — a standard admin who still holds the capability keeps using
// it.
func (b *Bot) capCustLookup(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.CustLookup
	c.st.CustLookup = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isAdmin(c.ChatID) {
		return true
	}
	target := strings.TrimSpace(c.Text)
	resolved := b.resolveCustomerTarget(target)
	if resolved == "" {
		b.send(c.Ctx, c.ChatID,
			"⚠️ No customer found for “"+target+"”.\nEnter a numeric chat_id (e.g. 7302701741) or an @username the bot has already seen.",
			admNavRetry("admcat:customers", "🔎 Look up customer", "adm:custlookup"))
		return true
	}
	b.send(c.Ctx, c.ChatID, b.customerLookupText(resolved), admNav("admcat:customers"))
	return true
}

// ownedAccount is one delivered account attributed to a customer — the tuple
// _owned_accounts_for_chat yields (telegram_bot.py:17095-17107).
type ownedAccount struct{ Username, Product, Des string }

// ownedAccountsForChat lists every account delivered to chatID across its
// delivered/paid orders, newest order first — _owned_accounts_for_chat
// (telegram_bot.py:17095-17107). nil (not an error) when store/stock aren't
// wired, so the customer-lookup report degrades to "none" rather than
// crashing (handlers must survive nil b.store/b.stock).
func (b *Bot) ownedAccountsForChat(chatID string) []ownedAccount {
	if b.store == nil || b.stock == nil {
		return nil
	}
	orders, err := b.store.OrdersForChat(chatID, 200, []string{"delivered", "paid"}, nil)
	if err != nil {
		return nil
	}
	var out []ownedAccount
	for _, o := range orders {
		for _, u := range b.stock.OrderUsernames(o.Des) {
			out = append(out, ownedAccount{Username: u, Product: o.Product, Des: o.Des})
		}
	}
	return out
}

// customerLookupText builds the admin customer-lookup report for a resolved
// chat_id — _customer_lookup_text (telegram_bot.py:17110-17170), FULL PARITY:
// name+handle, balance, up to 10 most-recent orders (wallet top-ups labelled
// specially), up to 50 owned accounts with a "…(+N more)" line, and the
// 3900-char trim with the "truncated" marker so the report always fits under
// Telegram's 4096-char message cap. Read-only; no writes, no notification.
func (b *Bot) customerLookupText(chatID string) string {
	var name, handle string
	if cid, err := strconv.ParseInt(chatID, 10, 64); err == nil {
		name = b.nameFor(cid)
		handle = b.handleForChat(cid)
	}
	if name == "" {
		name = "—"
	}
	var bal int64
	if b.store != nil {
		bal, _ = b.store.GetBalance(chatID)
	}
	nameLine := "Name: " + name
	if handle != "" {
		nameLine += " " + handle
	}
	lines := []string{
		"👤 CUSTOMER " + chatID,
		nameLine,
		"💰 Balance: " + b.usdtDisp(bal),
		strings.Repeat("─", 24),
	}

	var orders []*store.Order
	if b.store != nil {
		orders, _ = b.store.OrdersForChat(chatID, 10, nil, nil)
	}
	if len(orders) == 0 {
		lines = append(lines, "🧾 No orders.")
	} else {
		lines = append(lines, "🧾 Recent orders ("+itoa(len(orders))+", full detail):")
		emoji := map[string]string{
			"delivered": "✅", "paid": "💸", "reserved": "⏳", "expired": "⌛", "refunded": "↩️",
		}
		for _, o := range orders {
			e := emoji[o.Status]
			if e == "" {
				e = "•"
			}
			when := fmtWhen(o.TS, i18n.EN)
			pay := "—"
			if o.PaypalOrderID != nil && *o.PaypalOrderID != "" {
				pay = *o.PaypalOrderID
			}
			olang := o.Lang
			if olang == "" {
				olang = "—"
			}
			lines = append(lines, "")
			if o.Product == depositProduct {
				lines = append(lines, e+" 💳 Wallet top-up · "+o.Status)
				lines = append(lines, "   💰 "+b.usdtDisp(o.Total))
			} else {
				tier := o.Tier
				if tier == "" {
					tier = "—"
				}
				lines = append(lines, e+" "+o.Product+" · "+o.Status)
				lines = append(lines, "   📦 "+itoa64(o.Qty)+"× · tier "+tier)
				lines = append(lines, "   💰 "+b.usdtDisp(o.Total))
			}
			lines = append(lines, "   🆔 "+o.Des)
			lines = append(lines, "   🗓️ "+when+" · 🌐 "+olang)
			lines = append(lines, "   💳 PayPal: "+pay)
		}
	}

	owned := b.ownedAccountsForChat(chatID)
	lines = append(lines, strings.Repeat("─", 24))
	if len(owned) > 0 {
		lines = append(lines, "🔑 Owned accounts ("+itoa(len(owned))+"):")
		limit := len(owned)
		if limit > 50 {
			limit = 50
		}
		for _, o := range owned[:limit] {
			lines = append(lines, "   • "+o.Username+" ("+o.Product+")")
		}
		if len(owned) > 50 {
			lines = append(lines, "   … (+"+itoa(len(owned)-50)+" more)")
		}
	} else {
		lines = append(lines, "🔑 Owned accounts: none.")
	}

	// Telegram's ~4096-char message cap: trim trailing (least-critical) lines
	// until the report fits, then append the truncation marker once — mirrors
	// the exact pop-until-fits loop at telegram_bot.py:17158-17162.
	const limit = 3900
	if len(strings.Join(lines, "\n")) > limit {
		for len(lines) > 0 && len(strings.Join(lines, "\n")) > limit {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "… (truncated — too many accounts to show)")
	}
	return strings.Join(lines, "\n")
}

// ============================================================================
// 2. adm:addbal — 💰 Update balance (+/−) (full-admin-only)
// ============================================================================

// cbAddBal arms the two-field balance-adjust collector — adm:addbal
// (telegram_bot.py:17223-17240, admin_start_action). Full-admin-only: addbal
// is absent from _std_admin_cb_cap, so it falls to full-admin-only
// (py_customers_spec.md §2).
//
// M3/M4 fix: this now arms the SHARED cc.st.Admin slot (adminsettings.go's
// capAdminInput / cbAdminDoConfirm executor) instead of its own AddBalCtx
// slot — see AdminAction's own doc comment (pending.go) for why. Step starts
// at "collecting_chatid"; capAddBalStep (below) walks it to
// "collecting_amount" then "confirm".
func (b *Bot) cbAddBal(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:customers") {
		return
	}
	cc.st.mu.Lock()
	cc.st.Admin = &AdminAction{Action: "addbal", Step: "collecting_chatid"}
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "🔧 Update balance\nEnter the customer's chat_id:", backKB("admcat:customers"))
}

// capAddBalStep advances the addbal wizard — called from adminsettings.go's
// capAdminInput once it has already confirmed armed != nil, the text isn't a
// nav/slash command, and Step != "confirm" (that terminal case is handled
// generically by the caller, M4 fix). Collects field 1 (chat_id) then field 2
// (amount) with NO validation at this stage (telegram_bot.py:17243-17273:
// admin_collect_input stores raw text — validation happens only in
// admin_execute, i.e. execAddBal below), then renders the Confirm/Cancel
// summary using Python's LITERAL admdo:confirm/admdo:cancel callback strings
// (restored by the M3/M4 merge — the old standalone slot invented
// admdo:addbalconfirm/admdo:addbalcancel specifically to avoid colliding with
// this same executor, a constraint that no longer applies).
func (b *Bot) capAddBalStep(c *Ctx, armed *AdminAction, step string) bool {
	switch step {
	case "collecting_chatid":
		c.st.mu.Lock()
		if c.st.Admin == armed {
			armed.Target = strings.TrimSpace(c.Text)
			armed.Step = "collecting_amount"
		}
		c.st.mu.Unlock()
		b.send(c.Ctx, c.ChatID,
			"Enter amount in USDT — positive to add, negative to deduct (e.g. 2.5 or -1):",
			backKB("admcat:customers"))
		return true
	case "collecting_amount":
		c.st.mu.Lock()
		if c.st.Admin == armed {
			armed.Amount = strings.TrimSpace(c.Text)
			armed.Step = "confirm"
		}
		target, amount := armed.Target, armed.Amount
		c.st.mu.Unlock()
		b.send(c.Ctx, c.ChatID,
			"🔧 Update balance — confirm:\n• chat_id: "+target+"\n• amount: "+amount,
			&tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
				btnRow("✅ Confirm", "admdo:confirm"),
				btnRow("❌ Cancel", "admdo:cancel"),
			}})
		return true
	}
	// Unreached in practice (Step is always one of the three values above),
	// but armed is non-nil and past the nav/slash-command carve-out, so this
	// still swallows rather than falling through (M4 rationale).
	return true
}

// execAddBal runs the confirmed balance adjustment — admin_execute's
// "addbal" branch (telegram_bot.py:17305-17348, FULL parity). Credit and
// debit are asymmetric on purpose (store.py:838-869): debit is a single
// atomic guarded UPDATE that refuses to overdraw; credit is a plain additive
// upsert.
//
// ⚠️ MONEY-SAFETY HAZARD, ported faithfully from Python, NOT fixed here (task
// instruction: flag, don't silently change): the CREDIT path
// (store.CreditBalance) writes NO ledger/txn row and is NOT idempotent.
// Python has the identical hazard (store.py:838-851's own docstring: "no txn
// dedupe"). Reported to the user in the final task summary so an idempotency
// key can be added later if wanted; this port does not invent one
// unilaterally.
//
// L10 fix: a PREVIOUS version of this comment claimed "a double-tapped ✅
// Confirm... credits the customer TWICE" — that is WRONG and has been
// removed. cbAdminDoConfirm (adminsettings.go) pops cc.st.Admin to nil under
// st.mu BEFORE this function ever runs, so a second, near-simultaneous
// admdo:confirm tap finds a nil wizard and returns immediately — it never
// reaches execAddBal a second time. The real residual hazard is narrower: an
// admin who manually re-arms adm:addbal and re-enters the SAME chat_id/amount
// (a genuinely new admin action, not a double-tap of one) gets a second,
// equally-real credit — exactly as Python would for the same manual repeat.
func (b *Bot) execAddBal(ctx context.Context, chatID int64, rawTarget, rawAmount string) {
	target := b.resolveCustomerTarget(rawTarget)
	if target == "" {
		b.send(ctx, chatID,
			"⚠️ No customer found for “"+strings.TrimSpace(rawTarget)+"”.\nEnter a numeric chat_id or an @username the bot has already seen.",
			admNavRetry("admcat:customers", "💰 Update balance", "adm:addbal"))
		return
	}
	amount, ok := b.adminUsdtToVnd(rawAmount)
	if !ok {
		b.send(ctx, chatID, "❌ Amount must be a number, e.g. 2.5 or -1.",
			admNavRetry("admcat:customers", "💰 Update balance", "adm:addbal"))
		return
	}
	if amount == 0 {
		b.send(ctx, chatID, "❌ Amount can't be 0.",
			admNavRetry("admcat:customers", "💰 Update balance", "adm:addbal"))
		return
	}
	if b.store == nil {
		b.send(ctx, chatID, settingsNotConfigured, admNav("admcat:customers"))
		return
	}

	var custMsg string
	if amount > 0 {
		// See the money-safety hazard comment above this function: this path
		// is NOT idempotent by design (ported from Python as-is).
		newBal, err := b.store.CreditBalance(target, amount)
		if err != nil {
			log.Println("adminusers: credit balance failed:", err)
			b.send(ctx, chatID, settingsSaveFailed, admNav("admcat:customers"))
			return
		}
		b.send(ctx, chatID,
			"✅ Credited "+b.usdtDisp(amount)+" to "+target+". New balance: "+b.usdtDisp(newBal),
			admNav("admcat:customers"))
		custMsg = "💰 " + b.usdtDisp(amount) + " added to your balance. New balance: " + b.usdtDisp(newBal)
	} else {
		debited, newBal, err := b.store.Debit(target, -amount)
		if err != nil {
			log.Println("adminusers: debit balance failed:", err)
			b.send(ctx, chatID, settingsSaveFailed, admNav("admcat:customers"))
			return
		}
		if !debited {
			b.send(ctx, chatID,
				"❌ Can't deduct "+b.usdtDisp(-amount)+" from "+target+" — balance is only "+b.usdtDisp(newBal)+".",
				admNavRetry("admcat:customers", "💰 Update balance", "adm:addbal"))
			return
		}
		b.send(ctx, chatID,
			"✅ Deducted "+b.usdtDisp(-amount)+" from "+target+". New balance: "+b.usdtDisp(newBal),
			admNav("admcat:customers"))
		custMsg = "💰 " + b.usdtDisp(-amount) + " deducted from your balance. New balance: " + b.usdtDisp(newBal)
	}
	// Best-effort customer DM; b.send already logs and swallows delivery
	// failure (telegram_bot.py's try/except Exception: pass, :17345-17348).
	if tid, err := strconv.ParseInt(target, 10, 64); err == nil {
		b.send(ctx, tid, custMsg, nil)
	}
}

// ============================================================================
// 3-5. 🛡️ ADMIN ROLES: listadmins / addadmin / rmadmin (all full-admin-only)
// ============================================================================

// adminsFilePath is admins.json's location. Python keeps it NEXT TO THE
// SCRIPT FILE (ADMINS_PATH, telegram_bot.py:100); this Go port follows the
// SAME cwd-relative convention adminpricing.go's discountsFilePath and
// internal/config's settings.json already use (the bot is always run from
// its data directory, per that file's own comment) rather than resolving
// os.Args[0]'s directory — a deliberate, low-risk convention match, not a
// behavioural gap. A package-level var (not a Bot field) so tests can
// redirect it to an isolated temp path.
var adminsFilePath = "admins.json"

// loadAdminsFile reads admins.json into a set — load_admins
// (telegram_bot.py:103-112): a bare JSON array of chat-id strings. Any read
// or parse error (missing file, corrupt JSON) yields an EMPTY set, matching
// Python's bare except — a corrupt admins.json must never crash the bot, it
// just means "no standard admins configured".
func loadAdminsFile() map[string]bool {
	raw, err := os.ReadFile(adminsFilePath)
	if err != nil {
		return map[string]bool{}
	}
	// L12 fix: unmarshal into []any, not []string. A hand-edited admins.json
	// with bare numeric entries (`[111, 222]`) is exactly what Python
	// accepts too (str(x) on load_admins — telegram_bot.py:103-112 has no
	// separate int/string admin-id type), but json.Unmarshal into []string
	// rejects a JSON number outright — the whole file failed to parse and
	// EVERY existing grant silently vanished, with no log line explaining
	// why. Fail-closed (still safe: worst case is "no standard admins", same
	// as before) but no longer silent.
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		log.Println("adminusers: admins.json is not valid JSON, ignoring:", err)
		return map[string]bool{}
	}
	out := make(map[string]bool, len(arr))
	for _, v := range arr {
		switch t := v.(type) {
		case string:
			out[t] = true
		case float64: // encoding/json decodes a bare JSON number as float64
			out[strconv.FormatInt(int64(t), 10)] = true
		default:
			log.Printf("adminusers: admins.json entry %v is neither a string nor a number, skipping", v)
		}
	}
	return out
}

// saveAdminsFile persists the FULL admin set as a sorted JSON array via
// tmp+rename — save_admins (telegram_bot.py:115-120): a full-snapshot
// overwrite every time (no per-entry diffing), atomic so a reader can never
// observe a half-written file. Caller must hold b.adminsMu.
func saveAdminsFile(m map[string]bool) error {
	arr := make([]string, 0, len(m))
	for k := range m {
		arr = append(arr, k)
	}
	sort.Strings(arr)
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	tmp := adminsFilePath + ".tmp"
	// L14 fix: admins.json holds only chat ids (no secrets) so 0644 was not
	// a leak, but nothing else needs to read this file — tighten to 0600
	// anyway, matching the rest of this package's config-write discipline.
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, adminsFilePath); err != nil {
		// L13 fix: a failed rename (cross-device tmp dir, permissions, full
		// disk on the target) used to leave admins.json.tmp behind forever.
		// Clean it up on this path; on a SUCCESSFUL rename the tmp path is
		// already gone, so this call is never reached there.
		os.Remove(tmp)
		return err
	}
	return nil
}

// addAdmin grants admin access to chatID — add_admin (telegram_bot.py:
// 150-160). granted is false with a nil err when chatID is the super admin, a
// dev override, or already a standard admin (no write is even attempted,
// matching Python's plain False return); a non-nil err means the admins.json
// write itself failed, in which case the in-memory grant is ROLLED BACK so
// the caller never reports success for an unpersisted grant (H1 parity,
// adminsettings.go's save-failure discipline) — the two failure modes are
// deliberately distinguishable so the admin sees the right message.
func (b *Bot) addAdmin(chatID string) (granted bool, err error) {
	if chatID == b.adminID || b.devIDs[chatID] {
		return false, nil
	}
	b.adminsMu.Lock()
	defer b.adminsMu.Unlock()
	if b.admins[chatID] {
		return false, nil
	}
	b.admins[chatID] = true
	if saveErr := saveAdminsFile(b.admins); saveErr != nil {
		delete(b.admins, chatID)
		return false, saveErr
	}
	return true, nil
}

// removeAdmin revokes a granted admin — remove_admin (telegram_bot.py:
// 163-172). The super admin can never be removed because it was never stored
// in b.admins in the first place (addAdmin's own guard) — removed is simply
// false for that id, same as "not a granted admin".
func (b *Bot) removeAdmin(chatID string) (removed bool, err error) {
	b.adminsMu.Lock()
	defer b.adminsMu.Unlock()
	if !b.admins[chatID] {
		return false, nil
	}
	delete(b.admins, chatID)
	if saveErr := saveAdminsFile(b.admins); saveErr != nil {
		b.admins[chatID] = true
		return false, saveErr
	}
	return true, nil
}

// listAdminsSnapshot returns a sorted copy of the granted-admin ids —
// list_admins_snapshot (telegram_bot.py:349-352).
func (b *Bot) listAdminsSnapshot() []string {
	b.adminsMu.RLock()
	defer b.adminsMu.RUnlock()
	out := make([]string, 0, len(b.admins))
	for k := range b.admins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// adminsText builds the 🛡️ ADMIN ROLES roster — _admins_text
// (telegram_bot.py:17173-17185).
func (b *Bot) adminsText() string {
	lines := []string{"🛡️ ADMIN ROLES", strings.Repeat("─", 24), "👑 Super admin: " + b.adminID}
	extra := b.listAdminsSnapshot()
	if len(extra) == 0 {
		lines = append(lines, "🔑 Admins: none granted.")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "🔑 Admins ("+itoa(len(extra))+"):")
	for _, a := range extra {
		var nm string
		if cid, err := strconv.ParseInt(a, 10, 64); err == nil {
			nm = b.nameFor(cid)
		}
		line := "   • " + a
		if nm != "" {
			line += " — " + nm
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// cbListAdmins renders the roster — adm:listadmins (telegram_bot.py:
// 14282-14287). Super-admin only.
func (b *Bot) cbListAdmins(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:roles") {
		return
	}
	b.send(cc.Ctx, cc.ChatID, b.adminsText(), admNav("admcat:roles"))
}

// cbAddAdmin arms the grant-admin capture — adm:addadmin (telegram_bot.py:
// 14288-14295). Super-admin only.
func (b *Bot) cbAddAdmin(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:roles") {
		return
	}
	cc.st.mu.Lock()
	cc.st.AddAdmin = true
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "➕ Send the chat_id to GRANT admin access to:", backKB("admcat:roles"))
}

// capAddAdmin is the "addadmin" capture (#7, POP semantics — telegram_bot.py:
// 18226-18249). Numeric-only, unlike custlookup/addbal — a non-digit reply is
// rejected outright (no @username form here). The isSuperAdmin re-check is
// redundant defence-in-depth for the callback-armed path but IS load-bearing
// for this text-capture path (a different dispatch route than handle_callback
// in Python, and correspondingly here: any admin text could in theory reach
// this stage if state leaked, so it re-checks).
func (b *Bot) capAddAdmin(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.AddAdmin
	c.st.AddAdmin = false
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
	target := strings.TrimSpace(c.Text)
	if !isDigits(strings.TrimLeft(target, "-")) {
		b.send(c.Ctx, c.ChatID, "⚠️ Enter a numeric chat_id, e.g. 7302701741.",
			admNavRetry("admcat:roles", "➕ Add admin", "adm:addadmin"))
		return true
	}
	granted, err := b.addAdmin(target)
	if err != nil {
		log.Println("adminusers: save admins.json failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:roles"))
		return true
	}
	if granted {
		b.send(c.Ctx, c.ChatID, "✅ Granted admin access to "+target+".", admNav("admcat:roles"))
		if tid, perr := strconv.ParseInt(target, 10, 64); perr == nil {
			b.send(c.Ctx, tid, "🔑 You have been granted admin access. Send /admin to open the panel.", nil)
		}
	} else {
		b.send(c.Ctx, c.ChatID, "ℹ️ "+target+" is already an admin (or is the super admin).", admNav("admcat:roles"))
	}
	return true
}

// cbRmAdmin arms the revoke-admin capture — adm:rmadmin (telegram_bot.py:
// 14296-14303). Super-admin only.
func (b *Bot) cbRmAdmin(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:roles") {
		return
	}
	cc.st.mu.Lock()
	cc.st.RmAdmin = true
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "➖ Send the chat_id to REVOKE admin access from:", backKB("admcat:roles"))
}

// capRmAdmin is the "rmadmin" capture (#10, POP semantics — telegram_bot.py:
// 18300-18318). NO digit pre-validation (unlike addadmin) — any garbage
// string just fails to match inside removeAdmin, self-validating via the
// "not a granted admin" branch.
func (b *Bot) capRmAdmin(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.RmAdmin
	c.st.RmAdmin = false
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
	target := strings.TrimSpace(c.Text)
	removed, err := b.removeAdmin(target)
	if err != nil {
		log.Println("adminusers: save admins.json failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:roles"))
		return true
	}
	if removed {
		b.send(c.Ctx, c.ChatID, "✅ Revoked admin access from "+target+".", admNav("admcat:roles"))
		if tid, perr := strconv.ParseInt(target, 10, 64); perr == nil {
			b.send(c.Ctx, tid, "🔒 Your admin access has been revoked.", nil)
		}
	} else {
		b.send(c.Ctx, c.ChatID, "ℹ️ "+target+" is not a granted admin (the super admin can't be removed).", admNav("admcat:roles"))
	}
	return true
}

// ============================================================================
// 6. adm:addwl / rmwl: — private-sale whitelist (toggle_sales; open to any admin)
// ============================================================================
//
// NAMING NOTE (task-specified, deliberate divergence FROM Python, kept as
// instructed): Python's literal callback_data is "adm:wladd", but this Go
// repo's menu already emits "adm:addwl" (admin.go's adminCategoryMenu,
// "whitelist" case). KEEP "adm:addwl" — do not "fix" it to match Python;
// changing it would break buttons already sitting in admins' chat history.

// coerceWLValue extracts the bare id-or-username from one raw settings.json
// whitelist entry (string OR {"v","note"} map) — the same per-item
// normalization catalog.go's whitelist() uses, reused here so
// removeWhitelist's identity match agrees with the read path exactly.
func coerceWLValue(it any) string {
	switch t := it.(type) {
	case string:
		return normWLValue(t)
	case map[string]any:
		return normWLValue(asStr(t["v"]))
	}
	return ""
}

// addWhitelist adds a whitelist entry — add_whitelist (telegram_bot.py:
// 11579-11597). reason is one of "bad"/"dup"/"full" when !ok && err == nil;
// err distinguishes a config-not-wired/write-failure from a validation
// rejection so the caller shows the right message.
func (b *Bot) addWhitelist(rawValue, rawNote string) (ok bool, val, reason string, err error) {
	v := normWLValue(rawValue)
	if v == "" {
		return false, "", "bad", nil
	}
	cur := b.whitelist()
	for _, w := range cur {
		if strings.EqualFold(w.V, v) {
			return false, "", "dup", nil
		}
	}
	if len(cur) >= whitelistMax {
		return false, "", "full", nil
	}
	if b.cfg == nil {
		return false, "", "", errCfgNotConfigured
	}
	s := b.cfg.Settings()
	raw, _ := s["whitelist"].([]any)
	s["whitelist"] = append(raw, map[string]any{"v": v, "note": cleanNote(rawNote)})
	if saveErr := b.cfg.SaveSettings(s); saveErr != nil {
		return false, "", "", saveErr
	}
	return true, v, "", nil
}

// removeWhitelist removes a whitelist entry BY VALUE (case-insensitive
// identity, not list position) — remove_whitelist (telegram_bot.py:
// 11600-11615). removed=="" with a nil err means "not found" (a stale
// button); the caller re-renders the menu either way.
func (b *Bot) removeWhitelist(rawValue string) (removed string, err error) {
	if b.cfg == nil {
		return "", errCfgNotConfigured
	}
	target := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(rawValue), "@")))
	if target == "" {
		return "", nil
	}
	s := b.cfg.Settings()
	raw, _ := s["whitelist"].([]any)
	for i, it := range raw {
		v := coerceWLValue(it)
		if v == "" || !strings.EqualFold(v, target) {
			continue
		}
		s["whitelist"] = append(append([]any{}, raw[:i]...), raw[i+1:]...)
		if saveErr := b.cfg.SaveSettings(s); saveErr != nil {
			return "", saveErr
		}
		return v, nil
	}
	return "", nil
}

// cbAddWL arms the add-whitelist-entry capture — adm:addwl (telegram_bot.py's
// adm:wladd, :13910-13919 — see the naming note above). toggle_sales
// capability: no fullAdminOnly gate, any admin may run it (cbAdmAction's
// isAdmin check already ran).
func (b *Bot) cbAddWL(cc *cbCtx) {
	cc.st.mu.Lock()
	cc.st.AddWL = true
	cc.st.mu.Unlock()
	text := "👤 Send the customer's Telegram numeric ID or @username, optionally followed by | and a short note.\n" +
		"Examples:\n  7302701741 | VIP\n  @vipuser\n" +
		"Numeric ID works even if they have no @username (get it from 👥 Look up customers)."
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:whitelist"))
}

// capAddWL is the "addwl" capture (#9, POP semantics — telegram_bot.py:
// 18274-18298).
//
// M5 fix: re-checks isAdmin after popping (Python's own inline check,
// telegram_bot.py:18277 — `if not is_admin(chat_id): return`). Concrete
// failure this closes: admin A arms Add customer; the super-admin revokes A
// mid-flow; A then types "@vip | x" — without this check the whitelist entry
// still got written, because captureChain (step 10) runs for every chat, not
// just admins, so a popped-but-stale armed slot alone doesn't prove A is
// still an admin. toggle_sales is grantable (admincap.go), so isAdmin (not
// isSuperAdmin) is the right check — a standard admin keeps using it.
func (b *Bot) capAddWL(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.AddWL
	c.st.AddWL = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isAdmin(c.ChatID) {
		return true
	}
	value, note, _ := strings.Cut(c.Text, "|")
	ok, val, reason, err := b.addWhitelist(value, note)
	if err != nil {
		log.Println("adminusers: save whitelist failed:", err)
		msg := settingsSaveFailed
		if errors.Is(err, errCfgNotConfigured) {
			msg = settingsNotConfigured
		}
		b.send(c.Ctx, c.ChatID, msg, admNav("admcat:whitelist"))
		return true
	}
	var msg string
	switch {
	case ok:
		shown := val
		if !isDigits(val) {
			shown = "@" + val
		}
		msg = "✅ Added " + shown + " to the private-sale whitelist"
		if extra := cleanNote(note); extra != "" {
			msg += " — " + extra
		}
		msg += ". They can buy while sale is paused + Private Sale is ON."
	case reason == "dup":
		msg = "ℹ️ That customer is already whitelisted."
	case reason == "full":
		msg = "⚠️ Whitelist is full (max " + itoa(whitelistMax) + "). Remove one before adding another."
	default:
		msg = "⚠️ Not a valid Telegram numeric ID (5–15 digits) or @username (5–32 letters/digits/_, starts with a letter). Example: 7302701741 or @vipuser"
	}
	b.send(c.Ctx, c.ChatID, msg, admNavRetry("admcat:whitelist", "👥 Private-sale customers", "admcat:whitelist"))
	return true
}

// cbRmWL removes a whitelist entry then re-renders the menu — rmwl:<value>
// (telegram_bot.py:12932-12942). A standalone top-level route (NOT under
// adm:), so it re-checks isAdmin itself, matching Python's inline check.
// toggle_sales capability: no fullAdminOnly super-admin gate.
func (b *Bot) cbRmWL(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	value := strings.TrimPrefix(cc.Data, "rmwl:")
	removed, err := b.removeWhitelist(value)
	// L9 fix: a save failure used to be swallowed silently here — the entry
	// visibly stayed in the re-rendered menu below with NO explanation at
	// all (not even a false "success" — but no message either). Tell the
	// admin the write didn't land, same discipline every other save-failure
	// path in this package now follows.
	if err != nil {
		log.Println("adminusers: save whitelist failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, nil)
	} else if removed != "" {
		shown := removed
		if !isDigits(removed) {
			shown = "@" + removed
		}
		b.send(cc.Ctx, cc.ChatID, "➖ Removed "+shown+".", nil)
	}
	title, rows, ok := b.adminCategoryMenu(cc.ChatID, "whitelist")
	if ok {
		b.sendMenu(cc.Ctx, cc.ChatID, "🔑 "+title, rows)
	}
}

// ============================================================================
// 7. adm:addreseller, resmng:, resdisc:, rmreseller: — authorized resellers
//    (full-admin-only for the WHOLE subtree)
// ============================================================================

// normReseller normalizes a typed reseller handle to a bare username, or ""
// — _norm_reseller (telegram_bot.py:11295-11305): accepts "@name", "name", or
// a t.me/name URL (query/fragment dropped). Reseller-only twin of
// normWLValue (catalog.go) — no digit branch, since a reseller is ALWAYS an
// @username, never a bare chat-id.
func normReseller(s string) string {
	u := strings.TrimSpace(s)
	if u == "" {
		return ""
	}
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimSpace(strings.TrimLeft(u, "@"))
	if resellerRe.MatchString(u) {
		return u
	}
	return ""
}

// findReseller looks up one reseller entry by @username, case-insensitively
// — the shared lookup resmng:/resdisc: both use (telegram_bot.py:12885,
// 12909). nil for a stale/removed username.
func (b *Bot) findReseller(username string) *reseller {
	target := strings.ToLower(username)
	for _, r := range b.resellerList() {
		if strings.ToLower(r.U) == target {
			rr := r
			return &rr
		}
	}
	return nil
}

// addReseller adds an authorized reseller — add_reseller (telegram_bot.py:
// 11447-11465). New entries always start disc:0 (no flat discount until
// resdisc: is used). reason mirrors addWhitelist's contract.
func (b *Bot) addReseller(rawHandle, rawNote string) (ok bool, val, reason string, err error) {
	u := normReseller(rawHandle)
	if u == "" {
		return false, "", "bad", nil
	}
	cur := b.resellerList()
	for _, r := range cur {
		if strings.EqualFold(r.U, u) {
			return false, "", "dup", nil
		}
	}
	if len(cur) >= resellerMax {
		return false, "", "full", nil
	}
	if b.cfg == nil {
		return false, "", "", errCfgNotConfigured
	}
	s := b.cfg.Settings()
	raw, _ := s["resellers"].([]any)
	s["resellers"] = append(raw, map[string]any{"u": u, "note": cleanNote(rawNote), "disc": 0})
	if saveErr := b.cfg.SaveSettings(s); saveErr != nil {
		return false, "", "", saveErr
	}
	return true, u, "", nil
}

// removeReseller removes an authorized reseller BY IDENTITY (case-insensitive
// username, not list position) — remove_reseller (telegram_bot.py:
// 11468-11485).
func (b *Bot) removeReseller(rawUsername string) (removed string, err error) {
	if b.cfg == nil {
		return "", errCfgNotConfigured
	}
	target := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(rawUsername), "@")))
	if target == "" {
		return "", nil
	}
	s := b.cfg.Settings()
	raw, _ := s["resellers"].([]any)
	for i, it := range raw {
		r := coerceReseller(it)
		if r == nil || !strings.EqualFold(r.U, target) {
			continue
		}
		s["resellers"] = append(append([]any{}, raw[:i]...), raw[i+1:]...)
		if saveErr := b.cfg.SaveSettings(s); saveErr != nil {
			return "", saveErr
		}
		return r.U, nil
	}
	return "", nil
}

// setResellerDiscount sets the per-reseller flat discount % [0,95] (0==off)
// matched BY IDENTITY, rewriting the entry to its canonical dict form (a
// legacy bare-string entry gets a home for its disc field) — the coerce+
// rewrite mirrors set_reseller_discount (telegram_bot.py:11488-11511) exactly.
func (b *Bot) setResellerDiscount(username string, pct int) (saved string, ok bool, err error) {
	if b.cfg == nil {
		return "", false, errCfgNotConfigured
	}
	target := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(username), "@")))
	if target == "" {
		return "", false, nil
	}
	p := pct
	if p < 0 {
		p = 0
	}
	if p > 95 {
		p = 95
	}
	s := b.cfg.Settings()
	raw, _ := s["resellers"].([]any)
	for i, it := range raw {
		r := coerceReseller(it)
		if r == nil || !strings.EqualFold(r.U, target) {
			continue
		}
		raw[i] = map[string]any{"u": r.U, "note": r.Note, "disc": p}
		s["resellers"] = raw
		if saveErr := b.cfg.SaveSettings(s); saveErr != nil {
			return "", false, saveErr
		}
		return r.U, true, nil
	}
	return "", false, nil
}

// cbAddReseller arms the add-reseller capture — adm:addreseller
// (telegram_bot.py:13888-13896). Full-admin-only (absent from
// _std_admin_cb_cap entirely).
func (b *Bot) cbAddReseller(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:resellers") {
		return
	}
	cc.st.mu.Lock()
	cc.st.AddReseller = true
	cc.st.mu.Unlock()
	text := "🤝 Send the reseller's Telegram @username (a bot or a person), optionally followed by | and a short note.\n" +
		"Examples:\n  @authorized_reseller_bot\n  @reseller_bot | USDT only, min buy 10\n" +
		"Shown to customers on the Buy screen as a link to https://t.me/<username>."
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:resellers"))
}

// capAddReseller is the "addreseller" capture (#8, POP semantics —
// telegram_bot.py:18251-18272).
//
// M5 fix: re-checks isSuperAdmin after popping (Python's own inline check,
// telegram_bot.py:18253 — `if not is_admin(chat_id): return`; this Go port
// uses isSuperAdmin, not isAdmin, because the WHOLE resellers subtree is
// full-admin-only — cbAddReseller's own fullAdminOnly gate is the only way
// this slot ever arms, matching capAddAdmin/capRmAdmin's identical pattern
// for the equally full-admin-only roles subtree). Concrete failure this
// closes: captureChain (step 10) runs for every chat, not just admins, so an
// armed-but-stale slot alone doesn't prove the tapper is still a super
// admin if they were demoted mid-flow.
func (b *Bot) capAddReseller(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.AddReseller
	c.st.AddReseller = false
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
	handle, note, _ := strings.Cut(c.Text, "|")
	ok, val, reason, err := b.addReseller(handle, note)
	if err != nil {
		log.Println("adminusers: save resellers failed:", err)
		msg := settingsSaveFailed
		if errors.Is(err, errCfgNotConfigured) {
			msg = settingsNotConfigured
		}
		b.send(c.Ctx, c.ChatID, msg, admNav("admcat:resellers"))
		return true
	}
	var msg string
	switch {
	case ok:
		shown := cleanNote(note)
		msg = "✅ Added reseller @" + val
		if shown != "" {
			msg += " — " + shown
		}
		msg += ". Customers now see it on the Buy screen."
	case reason == "dup":
		msg = "ℹ️ That reseller is already in the list."
	case reason == "full":
		msg = "⚠️ Reseller list is full (max " + itoa(resellerMax) + "). Remove one before adding another."
	default:
		msg = "⚠️ That's not a valid Telegram @username (5–32 letters/digits/_, starts with a letter). Example: @authorized_reseller_bot"
	}
	b.send(c.Ctx, c.ChatID, msg, admNavRetry("admcat:resellers", "🤝 Authorized resellers", "admcat:resellers"))
	return true
}

// cbResMng renders the per-reseller manage submenu — resmng:<username>
// (telegram_bot.py:12880-12901). A stale username (removed since the menu was
// rendered) silently falls back to re-rendering the resellers list.
// Full-admin-only.
func (b *Bot) cbResMng(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:resellers") {
		return
	}
	u := strings.TrimPrefix(cc.Data, "resmng:")
	r := b.findReseller(u)
	if r == nil {
		title, rows, ok := b.adminCategoryMenu(cc.ChatID, "resellers")
		if ok {
			b.sendMenu(cc.Ctx, cc.ChatID, "🔑 "+title, rows)
		}
		return
	}
	discLbl := "💸 Reselling discount: off (tap to set)"
	if r.Disc > 0 {
		discLbl = "💸 Reselling discount: " + itoa(r.Disc) + "% (tap to change)"
	}
	rows := [][]tg.InlineKeyboardButton{
		btnRow(discLbl, "resdisc:"+r.U),
		btnRow("➖ Remove reseller", "rmreseller:"+r.U),
		btnRow("↩️ Back", "admcat:resellers"),
	}
	note := ""
	if r.Note != "" {
		note = " — " + r.Note
	}
	b.sendMenu(cc.Ctx, cc.ChatID,
		"🤝 @"+r.U+note+"\nReselling discount applies to their own purchases; a promo/referral code overrides it.",
		rows)
}

// cbResDisc arms the discount-percent capture for one reseller —
// resdisc:<username> (telegram_bot.py:12903-12921). The pending value stores
// the target USERNAME (not a bool) so capResellerDiscount knows which entry
// to apply to. Full-admin-only.
func (b *Bot) cbResDisc(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:resellers") {
		return
	}
	u := strings.TrimPrefix(cc.Data, "resdisc:")
	r := b.findReseller(u)
	if r == nil {
		title, rows, ok := b.adminCategoryMenu(cc.ChatID, "resellers")
		if ok {
			b.sendMenu(cc.Ctx, cc.ChatID, "🔑 "+title, rows)
		}
		return
	}
	cc.st.mu.Lock()
	cc.st.ResellerDiscount = r.U
	cc.st.mu.Unlock()
	text := "💸 Reselling discount for @" + r.U + " (now " + itoa(r.Disc) + "%).\n" +
		"Send a whole number 0–95 (0 = off → they get the normal volume discount).\nExample:  15"
	b.send(cc.Ctx, cc.ChatID, text, backKB("admcat:resellers"))
}

// capResellerDiscount is the "reseller_discount" capture (#36 — telegram_bot.py:
// 19003-19025). Membership-check-then-pop idiom (per the spec: the pending
// value carries the target USERNAME, not a plain bool, so the guard checks
// presence before popping) — behaviourally still abort-on-attempt like every
// sibling capture in this file.
func (b *Bot) capResellerDiscount(c *Ctx) bool {
	c.st.mu.Lock()
	target := c.st.ResellerDiscount
	c.st.mu.Unlock()
	if target == "" {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	c.st.mu.Lock()
	c.st.ResellerDiscount = ""
	c.st.mu.Unlock()
	body := strings.TrimSpace(c.Text)
	n, convErr := strconv.Atoi(body)
	if !isDigits(body) || convErr != nil || n > 95 {
		b.send(c.Ctx, c.ChatID, "⚠️ Invalid. Send a whole number 0–95 (0 = off).",
			admNavRetry("admcat:resellers", "🤝 Authorized resellers", "admcat:resellers"))
		return true
	}
	saved, ok, err := b.setResellerDiscount(target, n)
	if err != nil {
		log.Println("adminusers: save reseller discount failed:", err)
		msg := settingsSaveFailed
		if errors.Is(err, errCfgNotConfigured) {
			msg = settingsNotConfigured
		}
		b.send(c.Ctx, c.ChatID, msg, admNav("admcat:resellers"))
		return true
	}
	if !ok {
		b.send(c.Ctx, c.ChatID, "⚠️ @"+target+" is no longer in the reseller list.",
			admNavRetry("admcat:resellers", "🤝 Authorized resellers", "admcat:resellers"))
		return true
	}
	var msg string
	if n == 0 {
		msg = "✅ Reselling discount for @" + saved + " turned OFF (they get the normal volume discount)."
	} else {
		msg = "✅ Reselling discount for @" + saved + " set to " + itoa(n) + "% (ignored when they use a promo/referral code)."
	}
	b.send(c.Ctx, c.ChatID, msg, admNavRetry("admcat:resellers", "🤝 Authorized resellers", "admcat:resellers"))
	return true
}

// cbRmReseller removes a reseller then re-renders the menu —
// rmreseller:<username> (telegram_bot.py:12922-12930). Full-admin-only, no
// confirm step (mirrors tierdo:'s directness).
func (b *Bot) cbRmReseller(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:resellers") {
		return
	}
	username := strings.TrimPrefix(cc.Data, "rmreseller:")
	removed, err := b.removeReseller(username)
	// L9 fix: same silent-save-failure gap as cbRmWL above — say so instead
	// of leaving the admin to guess why the reseller is still listed below.
	if err != nil {
		log.Println("adminusers: save resellers failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, nil)
	} else if removed != "" {
		b.send(cc.Ctx, cc.ChatID, "➖ Removed @"+removed+".", nil)
	}
	title, rows, ok := b.adminCategoryMenu(cc.ChatID, "resellers")
	if ok {
		b.sendMenu(cc.Ctx, cc.ChatID, "🔑 "+title, rows)
	}
}

// ============================================================================
// registration
// ============================================================================

// registerAdminUsers wires this file's 6 captures into the ordered dispatch
// tables. Called LAST from New (dispatch.go), after registerAdminSettings
// ("setrate") and registerAdminPricing ("promo") have both already landed
// their own captures — insertCapturesBefore needs those anchors present to
// splice ahead of them rather than falling back to appending at the very end.
//
// addbal is NOT one of the 6 (M3/M4 fix): it moved out of the captureChain
// entirely into the shared step-9 "admininput" stage (adminsettings.go's
// capAdminInput, already registered by registerAdminSettings) — matching
// Python's real structure, where admin_collect_input for EVERY ADMIN_ACTIONS
// member (broadcast, addbal) runs from the same step-9 pending_admin bucket
// (CanonicalAdminTextOrder #10), not the step-10 text-capture chain.
//
// resmng:/resdisc:/rmreseller:/rmwl: are standalone top-level cbRoutes,
// already scaffolded in admin.go's registerAdmin (previously pointed at the
// generic cbAdmTodo stub) — this function does not re-register them. The
// admdo:confirm/admdo:cancel routes addbal now shares are already
// registered by registerAdminSettings for "broadcast" — nothing new needed
// here either.
func (b *Bot) registerAdminUsers() {
	b.insertCapturesBefore("setrate",
		stage{name: "custlookup", fn: (*Bot).capCustLookup},
		stage{name: "addadmin", fn: (*Bot).capAddAdmin},
		stage{name: "addreseller", fn: (*Bot).capAddReseller},
		stage{name: "addwl", fn: (*Bot).capAddWL},
		stage{name: "rmadmin", fn: (*Bot).capRmAdmin},
	)
	b.insertCapturesBefore("promo",
		stage{name: "reseller_discount", fn: (*Bot).capResellerDiscount},
	)
}
