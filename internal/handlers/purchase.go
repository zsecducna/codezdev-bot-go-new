package handlers

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/binance"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// getBalance reads a chat's wallet balance, tolerating a nil store (unit tests).
func (b *Bot) getBalance(chatID int64) (int64, error) {
	if b.store == nil {
		return 0, nil
	}
	return b.store.GetBalance(fmtInt(chatID))
}

// pendingProductDisplay renders a friendly product name for the pending-order
// warning (telegram_bot.py:10699-10702): the API-key sentinels get a readable
// label; a real product shows its own name.
func pendingProductDisplay(product string) string {
	switch product {
	case apikeyProduct:
		return "🔑 API credits"
	case kiroApikeyProduct:
		return "🟣 Kiro API credits"
	case apikeyRechargeProduct:
		return "🔁 API credits top-up"
	}
	return product
}

// pendingGuard shows the confirm-cancel prompt when this chat already has an
// unpaid reserved order, and returns true (caller must STOP) — pending_guard
// (telegram_bot.py:10682-10706). Deposits are excluded by
// ReservedOrderForChat, so topping up to pay a pending order stays free.
func (b *Bot) pendingGuard(ctx context.Context, chatID int64, lang i18n.Lang, resumeCmd string) bool {
	if b.store == nil {
		return false
	}
	o, err := b.store.ReservedOrderForChat(fmtInt(chatID))
	if err != nil {
		log.Println("reserved_order_for_chat failed:", err)
		return false
	}
	if o == nil {
		return false
	}
	c := &i18n.C().Buy
	kb := &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		{{Text: c.PendingCancelBtn.T(lang), CallbackData: cbData("cancelord", o.Des, resumeCmd)}},
		{{Text: c.PendingKeepBtn.T(lang), CallbackData: "keepord"}},
	}}
	b.send(ctx, chatID, i18n.F(c.PendingWarn.T(lang),
		"qty", itoa64(o.Qty), "product", pendingProductDisplay(o.Product),
		"total", b.usdtDisp(o.Total)), kb)
	return true
}

// finalizePurchase reserves stock, persists the order, starts the expiry timer,
// and shows the pay screen — finalize_purchase (telegram_bot.py:10360-10408).
// The gates re-run here so no NEW order is created while sales are paused / a
// product is suspended or private, even from a stale menu.
func (b *Bot) finalizePurchase(ctx context.Context, chatID int64, lang i18n.Lang, product, tier string, qty int) {
	c := &i18n.C().Buy
	if b.salesBlocked(ctx, chatID, lang, nil) {
		return
	}
	if b.productSuspended(product) {
		b.send(ctx, chatID, c.ProductSuspended.T(lang), nil)
		return
	}
	if !b.productVisibleTo(chatID, product) {
		b.send(ctx, chatID, c.OutOfStock.T(lang), nil)
		return
	}
	unit, ok := b.stock.ProductPrice(product, tier)
	if !ok {
		b.send(ctx, chatID, c.NoPrice.T(lang), nil)
		return
	}
	total, _, _ := codedTotal(unit, qty, b.resellerPctFor(chatID))
	des := store.NewDes()
	// Reserve the accounts up front (all-or-nothing). No lock -> no payment.
	reserved, err := b.stock.Reserve(product, qty, des, b.reserveTTL)
	if err != nil {
		log.Println("reserve failed:", err)
		return
	}
	if len(reserved) < qty {
		b.send(ctx, chatID, c.SoldOut.T(lang), nil)
		return
	}
	// Persist the order (survives restart) and start the hard-limit expiry timer.
	if err := b.store.CreateOrder(des, fmtInt(chatID), string(lang), product, tier,
		int64(qty), int64(total)); err != nil {
		log.Println("create_order failed:", err)
		return
	}
	b.spawnOrderExpiry(des)
	b.sendPayScreen(ctx, chatID, des, "")
}

// spawnOrderExpiry starts the per-order hard-limit timer — order_expiry_worker
// (telegram_bot.py:8974-8992). At DEPOSIT_TIMEOUT_S it claims reserved->expired;
// if it wins, it releases the rows and tells the customer the window closed. If
// it loses (already paid), it does nothing — a paid order is never expired out
// from under delivery. Tracked by the worker registry so Shutdown drains it.
//
// DEFERRED (M2.3 daemons slice): this in-process timer is volatile — a restart
// (or a graceful Shutdown, which cancels pending timers via the stop channel
// WITHOUT expiring) leaves the DB row stuck 'reserved'. Python's restart-safe
// backstop is a 120s _expiry_sweep_once loop (telegram_bot.py:9313) over
// store.ReservedOrdersBefore(now-DEPOSIT_TIMEOUT_S); that boot + periodic sweep
// lands with the SePay/sweeper slice (store.ReservedOrdersBefore already exists,
// awaiting its caller). Until then a leaked reserved row costs no money/stock
// (folder stock self-frees at RESERVE_TTL_S), but pending_guard blocks that
// customer's new purchases until they tap Cancel.
func (b *Bot) spawnOrderExpiry(des string) {
	b.workers.Go(func(stop <-chan struct{}) {
		select {
		case <-stop:
			return // shutdown before the window elapsed
		case <-time.After(b.expiryTimeout):
		}
		ctx := context.Background() // the request ctx is long gone by now
		order, err := b.store.ClaimOrder(des, "reserved", "expired")
		if err != nil {
			log.Println("expiry claim failed:", err)
			return
		}
		if order == nil {
			return // already paid/handled
		}
		// Virtual products reserve no folder stock (releasing would touch a
		// nonexistent ./accounts/<sentinel> folder).
		if !isNoStockProduct(order.Product) {
			if _, err := b.stock.Release(order.Product, des); err != nil {
				log.Println("expiry release failed:", err)
			}
		}
		// Release any promo/referral hold on the expired order, giving back the
		// reserved use so others can apply the code (telegram_bot.py:8996).
		b.releaseOrderCode(order)
		chatID, _ := strconv.ParseInt(order.ChatID, 10, 64)
		b.send(ctx, chatID, i18n.C().Buy.DepositTimeout.T(langSafe(order)), nil)
	})
}

// sendPayScreen (re)renders the payment screen for a reserved order from its
// stored state — _send_pay_screen (telegram_bot.py:10154-10246). Reads the order
// fresh so the shown total always reflects the stored (code-adjusted) price. Only
// admin-enabled methods render (the on-chain handlers land in later slices).
func (b *Bot) sendPayScreen(ctx context.Context, chatID int64, des, note string) {
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil || o.Status != "reserved" {
		return
	}
	c := &i18n.C().Buy
	lang := langSafe(o)
	product, qty, total := o.Product, o.Qty, o.Total
	// API-key orders (new key OR recharge): qty holds CREDITS and there is no
	// folder price file, so the generic unit×qty maths is skipped and a
	// dedicated prompt renders per kind (@10166-10176).
	isAPIKey := isApikeyFamily(product)
	unit := 0
	if !isAPIKey {
		if up, ok := b.stock.ProductPrice(product, o.Tier); ok {
			unit = up
		}
	}
	base := int64(unit) * qty
	if isAPIKey {
		// SKU-correct undiscounted base for the discount % line: a kiro order's
		// list price is the KIRO rate (recharges inherit the target key's class).
		base = b.apikeyPriceVND(qty, b.orderSaleClass(o))
	}
	pct := 0
	if base > 0 {
		pct = int(math.Round((1 - float64(total)/float64(base)) * 100))
	}
	balance, _ := b.getBalance(chatID)
	var rows [][]tg.InlineKeyboardButton
	if b.payEnabled("balance") {
		rows = append(rows, btnRow(c.PayBtnBalance.T(lang), cbData("paybal", des)))
	}
	if b.payEnabled("paypal") {
		rows = append(rows, btnRow(c.PayBtnPayPal.T(lang), cbData("paypp", des)))
	}
	if b.payEnabled("binance") && binance.Configured() {
		rows = append(rows, btnRow(c.PayBtnBinance.T(lang), cbData("paybnb", des)))
	}
	if b.payEnabled("bep20") && bep20.Configured() {
		rows = append(rows, styledBtnRow(c.PayBtnBEP20.T(lang), cbData("paybsc", des), "primary"))
	}
	if b.payEnabled("tron") && tron.Configured() {
		rows = append(rows, styledBtnRow(c.PayBtnTRON.T(lang), cbData("paytrx", des), "danger"))
	}
	// Apply- or remove-code button (@10197-10206). The apply/remove handlers are
	// M2.6; the button renders now so the pay screen matches the Python layout.
	if o.Code != nil && *o.Code != "" {
		rows = append(rows, btnRow(i18n.F(c.PayBtnRmCode.T(lang), "code", *o.Code), cbData("rmcode", des)))
	} else {
		rows = append(rows, btnRow(c.PayBtnApplyCode.T(lang), cbData("applycode", des)))
	}
	text := i18n.F(c.PayPrompt.T(lang),
		"product", product, "unit", b.usdtDisp(int64(unit)), "qty", itoa64(qty),
		"pct", itoa(pct), "total", b.usdtDisp(total), "balance", b.usdtDisp(balance))
	if isAPIKey {
		text = b.apikeyPayScreenText(o, lang, balance)
	}
	if note != "" {
		text = note + "\n\n" + text
	}
	b.send(ctx, chatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// cbPayBalance pays a reserved order from the wallet balance — paybal:
// (telegram_bot.py:15819-15850). Claims reserved->paid FIRST (closing the
// apply-code TOCTOU), then debits the LOCKED row's total; on an insufficient
// balance it reverts to reserved so other methods stay available.
func (b *Bot) cbPayBalance(cc *cbCtx) {
	c := &i18n.C().Buy
	des := strings.TrimPrefix(cc.Data, "paybal:")
	o, err := b.store.GetOrder(des)
	// Ownership guard: a des can leak (shown in QR content), so a payment button
	// must only act for the chat that owns the order (@15456-15461).
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, c.OrderGone.T(b.langFor(cc.ChatID, "en")), nil)
		return
	}
	lang := langSafe(o)
	if !b.payEnabled("balance") {
		b.send(cc.Ctx, cc.ChatID, c.PayMethodOff.T(lang), nil)
		return
	}
	won, err := b.store.ClaimOrder(des, "reserved", "paid")
	if err != nil {
		log.Println("paybal claim failed:", err)
		return
	}
	if won == nil {
		return // lost the claim — another path owns it
	}
	ok, _, err := b.store.Debit(won.ChatID, won.Total)
	if err != nil {
		// Debit ERRORED (distinct from "insufficient"): store.Debit runs in one
		// transaction whose deferred Rollback fires on every error path, so NO
		// money moved — but this order is already 'paid'. Leaving it 'paid'
		// would break the money invariant every downstream consumer relies on
		// ('paid' ⇒ money confirmed): the stuck-paid reconcile / any manual
		// re-drive would later deliver or refund against money that was never
		// taken. Revert to 'reserved' (the same compensation the
		// insufficient-funds branch below uses) so the order stays payable and
		// 'paid' keeps meaning money-in. (Python had the same latent hole — a
		// debit exception aborted the handler leaving the row 'paid'; closing
		// it here is strictly safer and costs nothing.)
		log.Printf("CRITICAL paybal debit errored [des=%s chat=%s total=%d err=%v] — reverting to 'reserved' (no money moved)",
			des, won.ChatID, won.Total, err)
		if _, rerr := b.store.ClaimOrder(des, "paid", "reserved"); rerr != nil {
			// Revert ALSO failed: order stuck 'paid' with NOTHING debited.
			// Flag loudly — whoever reconciles stuck-paid orders must check the
			// wallet ledger before re-driving this des.
			log.Printf("CRITICAL paybal revert ALSO failed [des=%s err=%v] — order stuck 'paid' with NO debit; do NOT auto-redeliver without checking the ledger",
				des, rerr)
		}
		return
	}
	if !ok {
		// Can't cover it -> revert to reserved so QR/PayPal stay available.
		b.store.ClaimOrder(des, "paid", "reserved")
		b.send(cc.Ctx, cc.ChatID, c.Insufficient.T(lang), nil)
		return
	}
	b.deliverPaid(cc.Ctx, won, des)
}

// deliverPaid finalizes a PAID order's stock into a delivery, or refunds to
// balance if the reserved rows are gone — _deliver_paid (telegram_bot.py:
// 7823-7943). The paid->delivered / paid->refunded transitions are the atomic
// double-delivery guard. Assumes the caller already won reserved->paid, so this
// runs exactly once per order.
func (b *Bot) deliverPaid(ctx context.Context, order *store.Order, des string) bool {
	c := &i18n.C().Buy
	chatIDStr := order.ChatID
	// API-key orders (either SKU) have no folder stock to finalize: route to the
	// Kiro-Go minting path, which owns its own idempotency + refund-on-failure
	// and reads the SKU off the product sentinel. Every payment rail (balance,
	// QR, PayPal, Binance, on-chain) fulfills the virtual product through this
	// ONE branch (telegram_bot.py:7834-7844).
	if isApikeyBuyProduct(order.Product) {
		return b.deliverApikey(ctx, order, des)
	}
	// Recharge orders route to the top-up path (same idempotency +
	// refund-on-failure contract as the mint, against /admin/recharge_api_key).
	if order.Product == apikeyRechargeProduct {
		return b.deliverApikeyRecharge(ctx, order, des)
	}
	chatID, _ := strconv.ParseInt(chatIDStr, 10, 64)
	lang := langSafe(order)
	product, qty, total := order.Product, order.Qty, order.Total

	warranty := ""
	if order.Tier != "" {
		warranty = b.tierLabel(order.Tier, lang)
	}
	claimed, err := b.stock.Finalize(product, des, warranty, string(lang))
	if err != nil {
		log.Println("finalize failed:", err)
	}
	if len(claimed) == 0 {
		// Reserved rows gone -> refund the confirmed money to balance, never drop
		// it (@7844-7851). The paid->refunded CAS must be WON before crediting:
		// crediting after a failed claim would leave the order 'paid' (so the
		// stuck-paid reconcile re-drives it later) with the money ALREADY
		// credited -> double refund. On a claim error nothing moves — the order
		// stays 'paid' and surfaces in the PaidOrders reconcile for manual review
		// (Python got this for free: a claim_order exception aborted before the
		// credit ran).
		won, err := b.store.ClaimOrder(des, "paid", "refunded")
		if err != nil || won == nil {
			log.Printf("CRITICAL refund claim failed [des=%s chat=%s total=%d won=%t err=%v] — order left 'paid' for reconcile, nothing credited",
				des, chatIDStr, total, won != nil, err)
			return false
		}
		newBal, cerr := b.store.CreditBalance(chatIDStr, total)
		if cerr != nil {
			// Credit failed AFTER the refunded claim committed. Left as-is the
			// money would vanish: status 'refunded' looks settled and hides the
			// order from the stuck-paid reconcile. Revert to 'paid' so the
			// reconcile surfaces it and an admin re-drives the refund by hand
			// (money must NEVER be silently dropped — repo invariant).
			log.Printf("CRITICAL refund credit failed [des=%s chat=%s total=%d err=%v] — reverting to 'paid' for reconcile",
				des, chatIDStr, total, cerr)
			if _, rerr := b.store.ClaimOrder(des, "refunded", "paid"); rerr != nil {
				log.Printf("CRITICAL refund revert ALSO failed [des=%s err=%v] — MANUAL FIX REQUIRED: customer paid %d, not credited",
					des, rerr, total)
			}
			return false
		}
		b.send(ctx, chatID, i18n.F(c.PaidNoStock.T(lang),
			"amount", b.usdtDisp(total), "balance", b.usdtDisp(newBal)), nil)
		return false
	}
	if int64(len(claimed)) < qty {
		// Partial finalize: a corrupt credential file was skipped. Audit it for a
		// manual top-up/partial refund (admin push-notify is M3).
		b.store.LogAction(chatIDStr, "system", "partial_delivery", des,
			fmt.Sprintf("%d/%d delivered", len(claimed), qty), "product="+product)
	}
	// Close the order. The result is unused this slice (Python only used it to
	// gate the referral payout, M2.6) but an ERROR must not pass silently: a
	// failed paid->delivered claim leaves a DELIVERED order looking 'paid', and
	// anything that later re-drives stuck-paid orders would double-deliver or
	// double-refund it. Log it for the reconcile/audit trail.
	won2, claimErr := b.store.ClaimOrder(des, "paid", "delivered")
	if claimErr != nil {
		log.Printf("CRITICAL delivered-claim failed [des=%s chat=%s]: %v — goods delivered but order left 'paid'; reconcile must NOT re-drive this des",
			des, chatIDStr, claimErr)
	}
	// Referral payout: if this order carried a referral code, credit the referrer
	// REFERRAL_PCT% of the delivered total (_pay_referrer telegram_bot.py:7872).
	// Use the won2 row (which has the referrer_chat and total), or fall back to
	// order if the claim failed but goods were still delivered.
	referralOrder := won2
	if referralOrder == nil {
		referralOrder = order
	}
	b.payReferrer(ctx, referralOrder)

	// Delivery instructions template from what was ACTUALLY delivered.
	isJSON := false
	for _, a := range claimed {
		if a.Kind == stock.KindJSON {
			isJSON = true
			break
		}
	}
	isToken := isJSON
	if !isToken {
		for _, a := range claimed {
			if a.Kind == stock.KindToken {
				isToken = true
				break
			}
		}
	}
	tmpl := c.PurchaseOK
	if isToken {
		tmpl = c.PurchaseOKToken
	}
	msg := i18n.F(tmpl.T(lang), "total", b.usdtDisp(total), "qty", itoa64(qty), "product", product)
	if note := b.stock.ProductDelivery(product); note != "" {
		msg = msg + "\n\n" + note
	}
	b.send(ctx, chatID, msg, nil)

	// Deliver the order file(s). JSON-account zip bundling (build_order_zip @7387)
	// lands with the JSON-account slice; until then every kind ships its raw order
	// files — the same fallback Python uses when zipping fails (@7906-7914) — so
	// the customer is never left empty-handed.
	sentOK := false
	for _, fp := range b.stock.OrderFiles(des) {
		if _, err := b.tg.SendDocument(ctx, chatID, fp, ""); err == nil {
			sentOK = true
		} else {
			log.Printf("send order file error [des=%s chat=%d]: %v", des, chatID, err)
		}
	}
	if !sentOK {
		// Nothing reached the buyer even after retries. The admin re-send alert is
		// M3; the buyer can also self-fetch via /history (redl:, M2.3).
		log.Printf("DELIVERY FAILED [des=%s chat=%d] — no file reached the buyer", des, chatID)
	}
	return true
}

// cbCancelOrd confirm-cancels a pending unpaid order then resumes the requested
// command — cancelord: (telegram_bot.py:15345-15365). Format cancelord:<des>:<cmd>.
func (b *Bot) cbCancelOrd(cc *cbCtx) {
	c := &i18n.C().Buy
	parts := strings.Split(cc.Data, ":")
	if len(parts) != 3 {
		return
	}
	des, resumeCmd := parts[1], parts[2]
	lang := b.langFor(cc.ChatID, "en")
	// Only release if still reserved (atomic): a payment may have landed.
	won, err := b.store.ClaimOrder(des, "reserved", "expired")
	if err != nil {
		log.Println("cancelord claim failed:", err)
	}
	if won != nil {
		if !isNoStockProduct(won.Product) {
			b.stock.Release(won.Product, des)
		}
		// Release any promo/referral hold on the cancelled order
		// (telegram_bot.py:15634 _release_order_code).
		b.releaseOrderCode(won)
		b.send(cc.Ctx, cc.ChatID, c.PendingCancelled.T(lang), nil)
	}
	// Whether or not we won the claim, run the command they wanted (@14993-14996).
	b.handleCommand(cc.Ctx, cc.ChatID, resumeCmd, "en")
}

// cbKeepOrd keeps the pending order and reminds the customer to pay — keepord
// (telegram_bot.py:15367-15370).
func (b *Bot) cbKeepOrd(cc *cbCtx) {
	b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PendingKept.T(b.langFor(cc.ChatID, "en")), nil)
}
