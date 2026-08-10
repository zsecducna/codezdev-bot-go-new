package handlers

// notify.go — the M3.1 owner/admin alert channel. Every deferred-alert
// marker left across the handler package (apikey_deliver.go,
// apikey_refund.go, selfservice.go, reconcile.go) pointed at this file; it
// is also what makes
// the admin_sale_notify / admin_deposit_notify toggles (Settings → New-sale
// alerts / New-deposit alerts) actually do something instead of just
// flipping a boolean nobody reads.
//
// Ports _notify_owners / _notify_admins (telegram_bot.py:10820-10859) and
// their two settings-gated wrappers notify_admin_sale / notify_admin_deposit
// (telegram_bot.py:8079-8137), plus _buyer_mention / _resolve_buyer_handle
// (telegram_bot.py:8037-8076).
//
// RECIPIENT POLICY (read off the Python source, not guessed):
//
//   - OWNERS = DEV_IDS ∪ {ADMIN_ID}  (b.devIDs ∪ {b.adminID}). Used for every
//     money-critical operational alarm this port carries (mint/recharge
//     auto-refund, upstream-not-revoked, refund-revoke failure, the stuck-
//     paid reconcile) AND for the sale/deposit notices. notify_admin_sale's
//     own docstring is explicit: "SALE alerts are owner-only by policy:
//     granted standard admins must never see revenue events"
//     (telegram_bot.py:8083-8085) — notify_admin_deposit mirrors it
//     (telegram_bot.py:8115-8116). Both go through _notify_owners, never
//     _notify_admins.
//   - ADMINS = {ADMIN_ID} ∪ admins.json (b.admins, the granted "standard"
//     admin set). Used for events a granted admin IS allowed to see: a new
//     customer refund request filed, and the self-service API-key refund
//     payout record.
//
// FAILURE POLICY: every helper here is best-effort per recipient (one
// unreachable chat never blocks the others, matching Python's per-cid
// try/except) and never returns an error — b.send/b.sendHTML already log a
// send failure and stop there, so nothing in this file can unwind or block
// the flow it is reporting on. That is load-bearing: these call sites fire
// AFTER a refund/credit/delivery has already committed, so a Telegram outage
// must never look like the flow itself failed.

import (
	"context"
	"fmt"
	"html"
	"strconv"

	"codezdev-bot/internal/i18n"
)

// ownerRecipients returns the de-duped OWNER chat-id set — DEV_IDS ∪
// {ADMIN_ID} (telegram_bot.py:10849's `set(DEV_IDS) | {ADMIN_ID}`).
// Recomputed per call: both inputs are immutable after New(), so this is
// cheap and always current.
func (b *Bot) ownerRecipients() []int64 {
	set := make(map[string]bool, len(b.devIDs)+1)
	for id := range b.devIDs {
		set[id] = true
	}
	if b.adminID != "" {
		set[b.adminID] = true
	}
	return parseChatIDs(set)
}

// adminRecipients returns the de-duped ADMIN chat-id set — {ADMIN_ID} ∪
// admins.json (telegram_bot.py:10827-10829's `{ADMIN_ID} | list_admins_snapshot()`).
// Deliberately excludes devIDs: only _notify_owners unions DEV_IDS in; a dev
// override is not automatically a granted standard admin.
func (b *Bot) adminRecipients() []int64 {
	set := map[string]bool{}
	if b.adminID != "" {
		set[b.adminID] = true
	}
	b.adminsMu.RLock()
	for id, on := range b.admins {
		if on {
			set[id] = true
		}
	}
	b.adminsMu.RUnlock()
	return parseChatIDs(set)
}

// parseChatIDs converts a set of string chat ids to int64s, silently
// dropping anything non-numeric (a Telegram chat_id is always numeric; a
// malformed ADMIN_ID/admins.json entry must not crash the alert path).
func parseChatIDs(set map[string]bool) []int64 {
	ids := make([]int64, 0, len(set))
	for s := range set {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// notifyOwners DMs every owner, best-effort per recipient — _notify_owners
// (telegram_bot.py:10844-10859).
func (b *Bot) notifyOwners(ctx context.Context, text string) {
	for _, id := range b.ownerRecipients() {
		b.send(ctx, id, text, nil)
	}
}

// notifyOwnersHTML is notifyOwners with parse_mode=HTML — the shape the sale
// and deposit notices need for their tappable buyer mention
// (telegram_bot.py:8104-8109 / 8130-8136, both `parse_mode="HTML"`).
func (b *Bot) notifyOwnersHTML(ctx context.Context, text string) {
	for _, id := range b.ownerRecipients() {
		b.sendHTML(ctx, id, text)
	}
}

// notifyAdmins DMs every granted admin, best-effort per recipient —
// _notify_admins (telegram_bot.py:10820-10841).
func (b *Bot) notifyAdmins(ctx context.Context, text string) {
	for _, id := range b.adminRecipients() {
		b.send(ctx, id, text, nil)
	}
}

// ---- buyer mention (parse_mode=HTML sale/deposit notices) ------------------

// resolveBuyerHandle is a best-effort display handle for a buyer chat: the
// runtime @handle cache, else the runtime display-name cache, else a live
// Telegram getChat lookup (cached on success) — _resolve_buyer_handle
// (telegram_bot.py:8037-8062). Returns "" when nothing is resolvable; the
// caller (buyerMention) falls back to the bare chat id, exactly like Python
// falls back to `str(buyer_chat_id)`.
//
// Python's getChat fallback also reads first_name/last_name; this port's
// tg.Chat only decodes Username (internal/tg/types.go) — a live lookup here
// can resolve an @handle but not a bare display name, a narrower surface
// than Python's, not a behavioral bug (the id fallback still applies).
func (b *Bot) resolveBuyerHandle(ctx context.Context, chatID int64) string {
	if h := b.handleForChat(chatID); h != "" {
		return h
	}
	if nm := b.nameFor(chatID); nm != "" {
		return nm
	}
	if b.tg == nil {
		return ""
	}
	chat, err := b.tg.GetChat(ctx, chatID)
	if err != nil || chat == nil || chat.Username == "" {
		return ""
	}
	handle := "@" + chat.Username
	// Cache + persist so later notices/features reuse it without another
	// getChat round trip — mirrors chat_handles[key]=handle / store.set_handle
	// (telegram_bot.py:8057-8059).
	b.handlesMu.Lock()
	b.handles[chatID] = handle
	b.handlesMu.Unlock()
	if b.store != nil {
		_ = b.store.SetHandle(fmtInt(chatID), handle) // best-effort cache warm; never blocks the alert
	}
	return handle
}

// buyerMention renders a tappable HTML mention for a buyer chat, for use in
// parse_mode=HTML admin alerts — _buyer_mention (telegram_bot.py:8066-8076).
func (b *Bot) buyerMention(ctx context.Context, chatID int64) string {
	disp := html.EscapeString(b.resolveBuyerHandle(ctx, chatID))
	if disp == "" {
		disp = fmtInt(chatID)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a> (%d)`, chatID, disp, chatID)
}

// ---- sale / deposit admin notices (#36) -------------------------------------

// notifyAdminSale sends the owner-only "new sale" DM — notify_admin_sale
// (telegram_bot.py:8079-8110). Gated on the admin_sale_notify toggle
// (default true, matching load_settings().get("admin_sale_notify", True));
// OFF means silent sales, exactly like Python. Both of this port's call
// sites (API-key mint + recharge delivery) pass tierCode "API", which
// renders the "N credits — product" package line Python reserves for it;
// anything else falls back to the generic "qty× product (tier label)" shape
// Python uses for folder-product sales, kept for parity even though no
// folder-product call site exists yet in this port.
func (b *Bot) notifyAdminSale(ctx context.Context, buyerChatID int64, product, tierCode string, qty, total int64, des string) {
	if !settingBool(b.settings(), "admin_sale_notify", true) {
		return
	}
	mention := b.buyerMention(ctx, buyerChatID)
	var pkg string
	if tierCode == "API" {
		pkg = fmt.Sprintf("%d credits — %s", qty, html.EscapeString(product))
	} else {
		pkg = fmt.Sprintf("%d× %s (%s)", qty, html.EscapeString(product), html.EscapeString(b.tierLabel(tierCode, i18n.Lang("en"))))
	}
	b.notifyOwnersHTML(ctx, fmt.Sprintf(
		"💸 NEW SALE\n👤 %s\n📦 %s\n💰 %s\n🧾 %s",
		mention, pkg, b.usdtDisp(total), html.EscapeString(des)))
}

// notifyAdminDeposit sends the owner-only "new deposit" DM —
// notify_admin_deposit (telegram_bot.py:8113-8137). Gated on the
// admin_deposit_notify toggle (default true); OFF means silent deposits.
//
// Runs AFTER the wallet credit has already committed at every call site —
// deposit.go's two settlePaid branches and usdt.go's on-chain deposit
// settle all call this only once CreditBalance/Credit has succeeded, so a
// send failure here can only cost a notification, never the money: it must
// never be allowed to unwind the credit, and (per this file's doc comment)
// nothing it calls can propagate a failure back up to do that.
//
// method is the funding rail label ("USDT BEP20" / "USDT TRC20" / …) —
// callers pass the order's stored Tier, falling back to "Deposit" exactly
// like Python's `won.get("tier") or "Deposit"` (telegram_bot.py:9034).
func (b *Bot) notifyAdminDeposit(ctx context.Context, buyerChatID int64, amountVND, balanceVND int64, method string) {
	if !settingBool(b.settings(), "admin_deposit_notify", true) {
		return
	}
	mention := b.buyerMention(ctx, buyerChatID)
	b.notifyOwnersHTML(ctx, fmt.Sprintf(
		"💰 NEW DEPOSIT\n👤 %s\n💵 %s\n💳 %s\n👛 New balance: %s",
		mention, b.usdtDisp(amountVND), html.EscapeString(method), b.usdtDisp(balanceVND)))
}
