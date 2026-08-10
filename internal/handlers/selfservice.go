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

	// No store wired (partial boot, or a unit-test Bot): there is nothing to
	// list and dereferencing would panic the update. Matches the explicit nil
	// guard chatOwnsAccount uses (otp.go:270).
	if b.store == nil {
		return
	}

	// Compute the since-timestamp: 30-day window applied to ALL orders including
	// apikey — matching Python @16814 which applies the same since filter to the
	// full query. Apikey orders older than 30 days are excluded at DB level (full
	// warranty does not mean infinite picker window).
	//
	// b.now() rather than time.Now(): every time-dependent handler reads the
	// injectable clock (dispatch.go:168) so a test can pin the window instead of
	// racing the wall clock.
	sinceTS := float64(b.now().Unix() - refundPickWindowS)

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

	// Build one button row per eligible order — label format mirrors @16828-16842.
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
		rows = append(rows, []tg.InlineKeyboardButton{
			{Text: truncateButtonLabel(label), CallbackData: "refreq:" + o.Des},
		})
	}

	b.sendMenu(ctx, chatID, c.RefundPickHeader.T(lang), rows)
}

// truncateButtonLabel caps an inline-button label at 60 CHARACTERS — Python's
// label[:60] (telegram_bot.py:16843), which slices a str and therefore counts
// characters, not bytes.
//
// Truncating by byte instead would be a silent, total failure: every label the
// picker builds is multi-byte (🛡️ alone is 7 bytes, 🔁/🔑/🟣 are 4 each, the
// "·" separators are 2, and tierLabel is localized into Vietnamese/Chinese), so
// a byte cut can land in the middle of a rune. The resulting string is invalid
// UTF-8, Telegram rejects the ENTIRE sendMessage rather than just that button,
// and the customer sees no picker at all — with no error surfaced, because
// sendMenu is fire-and-forget.
//
// Split out of showRefundOrderPicker so the boundary behaviour can be asserted
// directly; an inline slice expression is unreachable from a test.
func truncateButtonLabel(label string) string {
	// Telegram's hard limit is 64 bytes of button text; Python's 60-character
	// cap is the conservative budget this port keeps.
	if r := []rune(label); len(r) > 60 {
		return string(r[:60])
	}
	return label
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
// refund request — _file_refund_request (telegram_bot.py:16847-16896).
//
// For API-key orders: routes straight to apikeyRefundPreview (auto-approved
// self-service flow). For standard orders: validates ownership, status,
// warranty window and duplicate, then creates a pending request for admin review.
//
// DELIBERATE DEVIATION — ownership failure is a DEAD BUTTON (no message).
// Python does NOT do this: _file_refund_request (@16864-16866) calls
// _refund_ineligible_msg for EVERY failure reason, "not_owner" and "not_found"
// included, so it answers a tap on someone else's order with "⛔ This order
// isn't yours." That is an order-id enumeration oracle: a des is a 32-hex string
// that can be guessed or forwarded, and the distinct replies let a prober tell
// "exists but not yours" apart from "does not exist". Staying silent leaks
// nothing and costs a legitimate customer nothing, because a legitimate
// customer only ever reaches this from their OWN picker.
//
// Second, smaller deviation: the status and warranty branches answer with the
// generic Buy.OrderGone rather than Python's per-reason REFUND_INELIGIBLE
// strings ("not_refundable" @7083, "warranty_expired" @7089). The customer is
// told the order can no longer be refunded, just not precisely why. Only the
// duplicate branch has its own string (RefundIneligibleDuplicate), because
// there the generic notice would be actively misleading — see below.
func (b *Bot) fileRefundRequest(ctx context.Context, chatID int64, lang i18n.Lang, des string) {
	c := &i18n.C().Customer

	// No store wired (partial boot, or a unit-test Bot): fail CLOSED and drop
	// the tap rather than nil-deref. This gate decides whether money moves, so
	// "cannot check" must never mean "allow" — same rule as chatOwnsAccount
	// (otp.go:270).
	if b.store == nil {
		return
	}

	// Load the order; gate: must exist and belong to this chat. Silently drop
	// foreign-order taps — see the enumeration-oracle note above.
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

	// Standard order: warranty window gate — _refund_eligibility (@16800-16802):
	//
	//	window = _warranty_seconds(order.get("tier", ""))
	//	if window <= 0 or now > float(order.get("ts", 0) or 0) + window:
	//	    return (False, "warranty_expired")
	//
	// BOTH arms matter. window == 0 rejects KBH/unknown tiers (no warranty was
	// ever sold). The elapsed-time arm rejects an order whose warranty has since
	// run out — without it a 7-day-warranty order bought 20 days ago still files
	// a request, because the picker's window is 30 days (refundPickWindowS)
	// while real tiers are much shorter. That creates a refund_requests row
	// Python would have refused and puts a money decision in front of an admin
	// that policy already answered.
	//
	// b.now(), not time.Now(): the injectable clock (dispatch.go:168) is what
	// makes this gate testable. Python additionally caps its eligibility clock at
	// the product-suspend timestamp (@16859-16862) so a warranty that lapsed only
	// AFTER the product died stays eligible; the Go port has no product-suspend
	// tracking yet, so the cap is a no-op and plain now is the equivalent value.
	// M3.x adds product_suspended() and restores min(now, suspend_ts) here.
	win := stock.WarrantySeconds(o.Tier)
	if win == 0 || float64(b.now().Unix()) > o.TS+float64(win) {
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
		// An open request already exists — _refund_eligibility returns
		// (False, "duplicate") here (@16803-16804) and the caller answers with
		// REFUND_INELIGIBLE["duplicate"] (@7092).
		//
		// This MUST NOT reuse RefundRequestOK: that string says a request was
		// submitted, and nothing was. A customer who re-taps because they think
		// the first attempt was lost would get the identical confirmation twice
		// and still have no idea one is already pending.
		b.send(ctx, chatID, c.RefundIneligibleDuplicate.T(lang), nil)
		return
	}

	// Create the refund request.
	//
	// predatedSuspend / productSuspendTS now read the REAL product-suspend
	// state at filing time via productSuspendedAt (adminorders.go) — Python
	// computes `predated = 0 if susp else 1` and
	// `product_suspend_ts = susp["ts"] if susp else None` (@16879/16884).
	// predatedSuspend TRUE means "the customer filed while the product was
	// still ACTIVE" (they beat the admins to the problem); FALSE means the
	// product was already suspended when they filed, in which case the
	// frozen clock (refundRequestAsOf, adminorders.go) must cap the payout's
	// warranty window at THAT suspend ts, not whatever is live at payout
	// time. A ts of exactly 0 is treated as "not suspended" (falsy in
	// Python, `if susp.get("ts")`), matching refundAmount's own guard.
	susTS, suspOK := b.productSuspendedAt(o.Product)
	suspended := suspOK && susTS > 0
	var suspendTS *float64
	if suspended {
		suspendTS = &susTS
	}
	_, err = b.refundStore.CreateRequest(
		des, fmtInt(chatID),
		o.Product, o.Tier, o.Total, o.TS,
		!suspended, suspendTS,
	)
	if err != nil {
		log.Println("fileRefundRequest: CreateRequest:", err)
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}

	// Confirmation to the customer.
	b.send(ctx, chatID, i18n.F(c.RefundRequestOK.T(lang), "des", des), nil)

	// Best-effort admin notification — _notify_admins (telegram_bot.py:16978-16986).
	log.Printf("NEW REFUND REQUEST des=%s product=%s tier=%s total=%d chat=%d",
		des, o.Product, o.Tier, o.Total, chatID)
	// predated (telegram_bot.py:16879 `predated = 0 if susp else 1`): the
	// customer filed while the product was still ACTIVE when suspended is
	// false — matches the "!suspended" the CreateRequest call above already
	// derived, so this reuses it rather than recomputing anything.
	statusLine := "product already suspended"
	if !suspended {
		statusLine = "⚡ filed while product still ACTIVE"
	}
	b.notifyAdmins(ctx, fmt.Sprintf(
		"🔁 NEW REFUND REQUEST\n🧾 %s\n📦 %s (%s)\n💰 %s\n%s",
		des, o.Product, b.tierLabel(o.Tier, i18n.Lang("en")), b.usdtDisp(o.Total), statusLine))
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

	// No store wired (partial boot, or a unit-test Bot) — drop rather than
	// nil-deref (otp.go:270 idiom).
	if b.store == nil {
		return
	}

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

	// No store wired (partial boot, or a unit-test Bot) — drop rather than
	// nil-deref (otp.go:270 idiom).
	if b.store == nil {
		return
	}

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

	// Release any promo/referral hold the current code is occupying BEFORE
	// clearing the columns — _release_order_code (telegram_bot.py:16079). Bug
	// fix: previously this was not called here, so removing a promo leaked its
	// reserved use forever. The release must happen against the LOADED order (o)
	// which still carries the old code fields; UpdateOrderCode clears them next.
	b.releaseOrderCode(o)

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
