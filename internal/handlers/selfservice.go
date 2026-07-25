package handlers

// selfservice.go — M2.6 slice: the Self-Service inline submenu and all of its
// child flows.
//
// Ports:
//   - _show_self_service       (telegram_bot.py:4259)   → showSelfService
//   - _show_refund_order_picker(telegram_bot.py:16812)   → showRefundOrderPicker
//   - _file_refund_request     (telegram_bot.py:16847)   → fileRefundRequest
//   - _start_cust_reset        (telegram_bot.py:2343)    → startCustReset
//   - _start_cust_delete       (telegram_bot.py:2496)    → startCustDelete
//   - ss:* callback dispatch   (telegram_bot.py:12494)   → cbSelfService
//   - applycode: handler       (telegram_bot.py:16055)   → cbApplyCode
//   - rmcode: handler          (telegram_bot.py:16069)   → cbRemoveCode
//
// Domain note: entraSessionOk() is a STUB returning false until M3.4 wires the
// Entra/MSAL session. The password-reset and account-deletion flows will work
// end-to-end once M3.4 fills it in; the arming logic (pending state, prompts) is
// complete today so no downstream slice needs to add flow-entry code.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/tg"
)

// custDelMax is CUST_DEL_MAX (telegram_bot.py:7278): the maximum number of
// accounts a reseller/admin may delete in a single Self-Service batch.
const custDelMax = 10

// refundPickWindowS is REFUND_PICK_WINDOW_S (telegram_bot.py:16809): non-apikey
// orders older than 30 days are excluded from the picker (apikey orders are
// always shown — full-warranty policy).
const refundPickWindowS = 30 * 86400

// ---- entra session stub -------------------------------------------------------

// entraSessionOk reports whether the bot's Entra / MSAL session is live —
// _entra_session_ok (telegram_bot.py:2280). Returns false until M3.4 wires the
// actual session check; all flows that gate on it show "not available right now"
// and arm nothing, so the pending state stays clean.
func (b *Bot) entraSessionOk() bool {
	// TODO(M3.4): check _entra_session_ok / msal session liveness.
	return false
}

// ---- canDeleteAccounts -------------------------------------------------------

// canDeleteAccounts reports whether chatID may use the Self-Service account-
// deletion flow — _can_delete_accounts (telegram_bot.py:11432): admins and
// authorized resellers only.
func (b *Bot) canDeleteAccounts(chatID int64) bool {
	return b.isAdmin(chatID) || b.isAuthorizedReseller(chatID)
}

// ---- showSelfService ---------------------------------------------------------

// showSelfService renders the Self-Service inline submenu — port of
// _show_self_service (telegram_bot.py:4259-4280). Each row maps to an ss:*
// callback that re-dispatches to the matching original command handler, so
// consolidating these entries under one button changed only where they live, not
// how they behave.
//
// Replaces the M2.1 stub in command.go.
func (b *Bot) showSelfService(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Customer

	// Fixed rows shown to every customer (@4264-4267).
	rows := [][]tg.InlineKeyboardButton{
		{{Text: c.SSHistoryBtn.T(lang), CallbackData: "ss:history"}},
		{{Text: c.SSInfoBtn.T(lang), CallbackData: "ss:info"}},
		{{Text: c.SSCodeBtn.T(lang), CallbackData: "ss:getcode"}},
		{{Text: c.SSResetBtn.T(lang), CallbackData: "ss:reset"}},
	}

	// Delete Accounts is destructive: shown only when DELACCT_ENABLED (settings
	// key "delacct_enabled") AND the chat is an admin or authorized reseller —
	// mirrors the Python @4271-4273 guard.
	delAcctEnabled, _ := b.settings()["delacct_enabled"].(bool)
	if delAcctEnabled && b.canDeleteAccounts(chatID) {
		rows = append(rows, []tg.InlineKeyboardButton{
			{Text: c.SSDelAcctBtn.T(lang), CallbackData: "ss:delacct"},
		})
	}

	// My API Keys: always shown — harmless empty-state message keeps buyers
	// finding their keys even after the admin pauses new sales (@4276-4277).
	rows = append(rows,
		[]tg.InlineKeyboardButton{{Text: c.SSMyKeysBtn.T(lang), CallbackData: "ss:mykeys"}},
		[]tg.InlineKeyboardButton{{Text: c.SSRefundBtn.T(lang), CallbackData: "ss:refund"}},
		[]tg.InlineKeyboardButton{{Text: c.SSSupportBtn.T(lang), CallbackData: "ss:support"}},
	)

	b.sendMenu(ctx, chatID, c.SelfServiceHeader.T(lang), rows)
}

// ---- cbSelfService -----------------------------------------------------------

// cbSelfService dispatches all ss:* callbacks — the ss: branch of
// handle_callback (telegram_bot.py:12494-12515). Re-dispatches "history",
// "info", "getcode", "support" to handleCommand (same behaviour as before these
// moved off the main keyboard); routes the rest to their dedicated flows.
func (b *Bot) cbSelfService(cc *cbCtx) {
	action, _ := strings.CutPrefix(cc.Data, "ss:")
	lang := b.langFor(cc.ChatID, "en")

	switch action {
	case "reset":
		// Self-Service > Password reset.
		b.startCustReset(cc.Ctx, cc.ChatID, lang)
	case "delacct":
		// Self-Service > Delete accounts.
		b.startCustDelete(cc.Ctx, cc.ChatID, lang)
	case "refund":
		// Self-Service > Ask for refund — show picker.
		b.showRefundOrderPicker(cc.Ctx, cc.ChatID, lang)
	case "mykeys":
		// Self-Service > My API Keys: minted keys + live quota from Kiro-Go.
		b.showMyAPIKeys(cc.Ctx, cc.ChatID, lang)
	case "history", "info", "getcode", "support":
		// Re-dispatch to the original command handler — same behaviour as
		// before these moved off the main keyboard (@12509-12510).
		b.handleCommand(cc.Ctx, cc.ChatID, action, "")
	}
}

// ---- showRefundOrderPicker ---------------------------------------------------

// showRefundOrderPicker lists the chat's recent refund-eligible orders as
// buttons (refreq:<des>) — _show_refund_order_picker (telegram_bot.py:16812-16845).
// The window is 30 days for non-apikey orders; apikey orders have full warranty
// so they're always included.
func (b *Bot) showRefundOrderPicker(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Customer

	// Compute the since-timestamp: 30-day window applied to ALL orders including
	// apikey — matching Python @16814 which applies the same since filter to the
	// full query. Apikey orders older than 30 days are excluded at DB level (full
	// warranty does not mean infinite picker window).
	sinceTS := float64(time.Now().Unix() - refundPickWindowS)

	// Fetch the last 20 refundable paid/delivered orders for this chat.
	orders, err := b.store.OrdersForChat(
		fmtInt(chatID), 20,
		[]string{"delivered", "paid"},
		&sinceTS,
	)
	if err != nil {
		log.Println("showRefundOrderPicker: OrdersForChat:", err)
		b.send(ctx, chatID, c.RefundPickEmpty.T(lang), nil)
		return
	}

	// Filter: exclude deposits; keep apikey products unconditionally (full
	// warranty, no time window), keep standard products only if they carry a
	// non-zero warranty tier (@16816-16823).
	eligible := orders[:0]
	for _, o := range orders {
		if o.Product == depositProduct {
			continue
		}
		if isAPIKeyProduct(o.Product) {
			eligible = append(eligible, o)
			continue
		}
		if stock.WarrantySeconds(o.Tier) > 0 {
			eligible = append(eligible, o)
		}
	}

	if len(eligible) == 0 {
		b.send(ctx, chatID, c.RefundPickEmpty.T(lang), nil)
		return
	}

	// Build one button row per eligible order — label format mirrors @16828-16838.
	rows := make([][]tg.InlineKeyboardButton, 0, len(eligible))
	for _, o := range eligible {
		when := "—"
		if o.TS > 0 {
			when = fmtWhen(o.TS, lang)
		}
		var label string
		switch o.Product {
		case apikeyRechargeProduct:
			// Recharge — show credit amount with a 🔁 marker.
			label = fmt.Sprintf("🔁 %d credits · %s · %s", o.Qty, b.usdtDisp(o.Total), when)
		case apikeyProduct:
			// Bedrock API key sale.
			label = fmt.Sprintf("🔑 %d credits · %s · %s", o.Qty, b.usdtDisp(o.Total), when)
		case kiroApikeyProduct:
			// Kiro API key sale.
			label = fmt.Sprintf("🟣 %d Kiro credits · %s · %s", o.Qty, b.usdtDisp(o.Total), when)
		default:
			// Standard account order — show warranty tier label.
			label = fmt.Sprintf("🛡️ %s · %s · %s", b.tierLabel(o.Tier, lang), b.usdtDisp(o.Total), when)
		}
		// Telegram button text is limited to 64 bytes — truncate safely.
		if len(label) > 60 {
			label = label[:60]
		}
		rows = append(rows, []tg.InlineKeyboardButton{
			{Text: label, CallbackData: "refreq:" + o.Des},
		})
	}

	b.sendMenu(ctx, chatID, c.RefundPickHeader.T(lang), rows)
}

// ---- cbFileRefundReq ---------------------------------------------------------

// cbFileRefundReq handles the refreq:<des> callback — routes to
// fileRefundRequest after parsing the des from the callback data.
func (b *Bot) cbFileRefundReq(cc *cbCtx) {
	_, des, ok := strings.Cut(cc.Data, ":")
	if !ok || des == "" {
		return
	}
	lang := b.langFor(cc.ChatID, "en")
	b.fileRefundRequest(cc.Ctx, cc.ChatID, lang, des)
}

// fileRefundRequest eligibility-checks order des for chatID, then records a
// refund request — _file_refund_request (telegram_bot.py:16847-16889).
//
// For API-key orders: routes straight to apikeyRefundPreview (auto-approved
// self-service flow). For standard orders: validates ownership, status,
// warranty window and duplicate, then creates a pending request for admin review.
//
// Ownership failure is a DEAD BUTTON (no message): a des can be guessed/shared,
// so silently dropping the tap is correct — the Python original returns early
// with no response on non-ownership (@16851-16852 guard).
func (b *Bot) fileRefundRequest(ctx context.Context, chatID int64, lang i18n.Lang, des string) {
	c := &i18n.C().Customer

	// Load the order; gate: must exist and belong to this chat. Silently drop
	// foreign-order taps — the callback was for someone else's order (@16851).
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil || o.ChatID != fmtInt(chatID) {
		// Dead button — no message sent (ownership mismatch or order gone).
		return
	}

	// Status gate: only paid / delivered orders are refundable.
	if !isRefundableStatus(o.Status) {
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}

	// API-key family: skip admin queue, show self-service preview with pro-rated
	// payout (@16867-16869).
	if isAPIKeyProduct(o.Product) {
		b.apikeyRefundPreview(ctx, chatID, lang, o)
		return
	}

	// Standard order: warranty window gate — KBH tier is never refundable.
	if stock.WarrantySeconds(o.Tier) == 0 {
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}

	// Duplicate guard: only one open request per order.
	// Guard: refundStore is nil when not wired (unit tests without a DB).
	if b.refundStore == nil {
		log.Println("fileRefundRequest: refundStore not wired — dropping request for", des)
		return
	}
	existing, err := b.refundStore.OpenRequestForOrder(des)
	if err != nil {
		log.Println("fileRefundRequest: OpenRequestForOrder:", err)
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}
	if existing != nil {
		// A request already exists — silently show the success notice so the
		// customer sees their submission was recorded, without creating a duplicate.
		b.send(ctx, chatID, i18n.F(c.RefundRequestOK.T(lang), "des", des), nil)
		return
	}

	// Create the refund request. predatedSuspend = 0 (no suspend tracking in
	// this path yet — M3.x adds product-suspend awareness).
	_, err = b.refundStore.CreateRequest(
		des, fmtInt(chatID),
		o.Product, o.Tier, o.Total, o.TS,
		false, nil,
	)
	if err != nil {
		log.Println("fileRefundRequest: CreateRequest:", err)
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}

	// Confirmation to the customer.
	b.send(ctx, chatID, i18n.F(c.RefundRequestOK.T(lang), "des", des), nil)

	// Best-effort admin notification — log if no channel is wired yet.
	log.Printf("NEW REFUND REQUEST des=%s product=%s tier=%s total=%d chat=%d",
		des, o.Product, o.Tier, o.Total, chatID)
}

// ---- startCustReset ----------------------------------------------------------

// startCustReset begins the customer self-service password reset flow — port of
// _start_cust_reset (telegram_bot.py:2343-2355). Verifies Entra reachability,
// then arms pending CustReset so the next typed line is treated as the account
// email. Entra is a stub (M3.4) so this always shows the "not available" notice.
func (b *Bot) startCustReset(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Customer

	// Gate: Entra session must be live to perform resets (@2348-2351).
	if !b.entraSessionOk() {
		b.send(ctx, chatID, c.CustResetNoSSO.T(lang), nil)
		return
	}

	// Drop any half-finished sibling capture before arming this one (@2352).
	st := b.states.Get(chatID)
	st.ClearCustomerPending()

	// Arm the pending capture; the custreset capture stage (M3.4) consumes the
	// next typed message as the account email / email list (@2353-2355).
	st.mu.Lock()
	st.CustReset = &CustResetCtx{}
	st.mu.Unlock()

	b.send(ctx, chatID, c.CustResetPrompt.T(lang), nil)
}

// ---- startCustDelete ---------------------------------------------------------

// startCustDelete begins the customer self-service account deletion flow — port
// of _start_cust_delete (telegram_bot.py:2496-2516). Checks the kill-switch,
// authorization, and Entra reachability before arming the pending capture.
func (b *Bot) startCustDelete(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Customer

	// Kill-switch: DELACCT_ENABLED equivalent in settings (@2501-2503).
	delAcctEnabled, _ := b.settings()["delacct_enabled"].(bool)
	if !delAcctEnabled {
		b.send(ctx, chatID, c.CustDelDisabled.T(lang), nil)
		return
	}

	// Authorization: admins + authorized resellers only (@2505-2507).
	if !b.canDeleteAccounts(chatID) {
		b.send(ctx, chatID, c.CustDelNotAllowed.T(lang), nil)
		return
	}

	// Gate: Entra session must be live to perform deletions (@2509-2511).
	if !b.entraSessionOk() {
		b.send(ctx, chatID, c.CustDelNoSSO.T(lang), nil)
		return
	}

	// Drop sibling captures, arm this one (@2513-2514).
	st := b.states.Get(chatID)
	st.ClearCustomerPending()

	st.mu.Lock()
	st.CustDelete = &CustDelCtx{}
	st.mu.Unlock()

	// Prompt: send the email list to delete, one per line; {n} = custDelMax.
	b.send(ctx, chatID, i18n.F(c.CustDelPrompt.T(lang), "n", itoa(custDelMax)), nil)
}

// ---- cbApplyCode / cbRemoveCode ---------------------------------------------

// cbApplyCode handles the applycode:<des> callback — port of the applycode:
// branch of handle_callback (telegram_bot.py:16055-16068).
//
// Arms pending_ordercode so the customer's next typed message is treated as a
// promo/referral code to apply to the reserved order.
func (b *Bot) cbApplyCode(cc *cbCtx) {
	_, des, ok := strings.Cut(cc.Data, ":")
	if !ok || des == "" {
		return
	}
	lang := b.langFor(cc.ChatID, "en")

	// Ownership + status guard: only the owning chat may apply a code, and only
	// while the order is still reserved (@16059-16062).
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}

	// Drop stale sibling captures, then arm pending_ordercode (@16063-16065).
	st := b.states.Get(cc.ChatID)
	st.ClearCustomerPending()

	st.mu.Lock()
	st.OrderCode = &OrderCodeCtx{Des: des}
	st.mu.Unlock()

	b.send(cc.Ctx, cc.ChatID, i18n.C().Customer.EnterCode.T(lang), nil)
}

// cbRemoveCode handles the rmcode:<des> callback — port of the rmcode: branch
// of handle_callback (telegram_bot.py:16069-16091).
//
// Releases any applied promo/referral code on the reserved order and reverts the
// price to the volume-discount baseline, then re-renders the pay screen.
func (b *Bot) cbRemoveCode(cc *cbCtx) {
	_, des, ok := strings.Cut(cc.Data, ":")
	if !ok || des == "" {
		return
	}
	lang := b.langFor(cc.ChatID, "en")

	// Ownership + status guard (@16073-16076).
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}

	// Pop any pending_ordercode capture for this chat (@16078).
	st := b.states.Get(cc.ChatID)
	st.mu.Lock()
	st.OrderCode = nil
	st.mu.Unlock()

	// Revert the order price to the baseline (volume or reseller discount),
	// clearing the code fields — _release_order_code (telegram_bot.py:16079-16089).
	// Compute the volume/reseller baseline total (@16081-16089).
	resellerPct := b.resellerPctFor(cc.ChatID)
	var volTotal int64
	if isAPIKeyProduct(o.Product) {
		// API-key orders price off credits (@16083-16085). saleClass must be
		// derived from the product sentinel — NOT passed as the raw product string.
		total, _, _ := b.apikeyCodedTotal(o.Qty, resellerPct, apikeyClassForProduct(o.Product))
		volTotal = total
	} else {
		// Standard account orders (@16086-16089).
		unit := 0
		if b.stock != nil {
			unit, _ = b.stock.ProductPrice(o.Product, o.Tier)
		}
		total, _, _ := codedTotal(unit, int(o.Qty), resellerPct)
		volTotal = int64(total)
	}

	// Clear the code columns, apply the reverted total.
	if err := b.store.UpdateOrderCode(des, nil, nil, nil, volTotal); err != nil {
		log.Println("cbRemoveCode: UpdateOrderCode:", err)
	}

	// Re-render the pay screen with a "code removed" note (@16091).
	note := i18n.C().Customer.CodeRemoved.T(lang)
	b.sendPayScreen(cc.Ctx, cc.ChatID, des, note)
}

// ---- isAPIKeyProduct helper (for this file) ---------------------------------

// isAPIKeyProduct reports whether product is any api-key-family product
// (__apikey__, __kiroapi__, __apikeyrc__) — _APIKEY_PRODUCTS membership check
// (telegram_bot.py:3805). Kept package-private; the canonical one lives in
// apikey.go but is not exported; this mirrors it here to avoid a cross-file
// call from a non-method function.
//
// NOTE: this re-uses the identical logic from apikey.go isAPIKeyFamilyProduct.
// If that function is exported in a later slice, replace this with a call to it.
func isAPIKeyProduct(product string) bool {
	return product == apikeyProduct ||
		product == kiroApikeyProduct ||
		product == apikeyRechargeProduct
}
