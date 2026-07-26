package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// qtyChoices is QTY_CHOICES (telegram_bot.py:800): the quick-amount buttons at
// the quantity step. A customer may also type any positive number.
var qtyChoices = []int{1, 2, 5, 10}

// btnRow / styledBtnRow build one inline-keyboard ROW holding a single button.
func btnRow(text, data string) []tg.InlineKeyboardButton {
	return []tg.InlineKeyboardButton{{Text: text, CallbackData: data}}
}

func styledBtnRow(text, data, style string) []tg.InlineKeyboardButton {
	return []tg.InlineKeyboardButton{{Text: text, CallbackData: data, Style: style}}
}

// listProducts returns the catalog folder names, or nil when no stock machine is
// wired (defensive for unit tests that exercise only the dispatch/keyboard core).
func (b *Bot) listProducts() []string {
	if b.stock == nil {
		return nil
	}
	return b.stock.ListProducts()
}

// indexOf returns the position of name in xs, or -1.
func indexOf(xs []string, name string) int {
	for i, x := range xs {
		if x == name {
			return i
		}
	}
	return -1
}

// cmdBuy renders the /buy product list — the "buy" branch of handle_command
// (telegram_bot.py:11680-11758). Authorized resellers are surfaced on EVERY Buy
// outcome (paused, no products, or alongside the list); button callback_data is
// INDEX-based (buy:N / buyx:N) to stay under Telegram's 64-byte limit and avoid
// escaping folder names. The virtual API-key product rows (@11737-11757) are
// appended by apikeyBuyRows (M2.5) below the folder products.
func (b *Bot) cmdBuy(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Buy
	resRows := b.resellerButtonRows()
	if b.salesBlocked(ctx, chatID, lang, resRows) { // paused -> notice + resellers
		return
	}
	// Confirm-first gate: one unpaid reserved order blocks a NEW purchase
	// (telegram_bot.py:11697; deposits stay ungated).
	if b.pendingGuard(ctx, chatID, lang, "buy") {
		return
	}
	products := b.listProducts()
	var rows [][]tg.InlineKeyboardButton
	var header string
	if len(products) > 0 {
		for i, name := range products {
			// A private product simply does not exist for a customer outside its
			// allow-list — no button, no SOLD OUT hint. The index i stays aligned
			// with the full list so buy:/buyx: keep resolving (@11712-11717).
			if !b.productVisibleTo(chatID, name) {
				continue
			}
			avail, _ := b.stock.CountAvailable(name)
			switch {
			case b.productSuspended(name):
				rows = append(rows, styledBtnRow(name+" — ⏸️ SUSPENDED", cbData("buyx", itoa(i)), "danger"))
			case avail > 0:
				rows = append(rows, styledBtnRow(fmt.Sprintf("%s (%d)", name, avail), cbData("buy", itoa(i)), "success"))
			default:
				rows = append(rows, styledBtnRow(name+" — ❌ SOLD OUT", cbData("buyx", itoa(i)), "danger"))
			}
		}
		header = c.ProductsHeader.T(lang)
	} else {
		header = c.NoProducts.T(lang)
	}
	// Virtual API-key rows, one per sellable SKU (+ the shared recharge row when
	// this customer already holds a live key) — telegram_bot.py:11737-11757.
	if apiRows, anySellable := b.apikeyBuyRows(chatID, lang); anySellable {
		rows = append(rows, apiRows...)
		header = c.ProductsHeader.T(lang) // keys sellable -> never "no products"
	}
	if len(resRows) > 0 { // append the reseller links + note
		header += "\n\n" + c.ResellerNote.T(lang)
		rows = append(rows, resRows...)
	}
	if len(rows) > 0 {
		b.sendMenu(ctx, chatID, header, rows)
	} else {
		b.send(ctx, chatID, header, nil)
	}
}

// showProductTiers lists a product's warranty tiers as buy buttons — the port of
// _show_product_tiers (telegram_bot.py:12060-12080). Shared by the buy:N and
// buygo:N (post-notice Continue) paths.
func (b *Bot) showProductTiers(ctx context.Context, chatID int64, lang i18n.Lang, name string) {
	c := &i18n.C().Buy
	products := b.listProducts()
	pidx := indexOf(products, name)
	if pidx < 0 {
		return // product removed since the tap
	}
	avail, _ := b.stock.CountAvailable(name)
	if avail <= 0 {
		b.send(ctx, chatID, c.OutOfStock.T(lang), nil)
		return
	}
	var rows [][]tg.InlineKeyboardButton
	for _, t := range b.stock.ProductTiers(name) {
		rows = append(rows, btnRow(
			fmt.Sprintf("%s — %s", b.tierLabel(t.Code, lang), b.usdtDisp(int64(t.Price))),
			cbData("tier", itoa(pidx), t.Code)))
	}
	if len(rows) == 0 {
		b.send(ctx, chatID, c.NoPrice.T(lang), nil)
		return
	}
	rows = append(rows, btnRow(c.BackBtn.T(lang), "back:products"))
	b.sendMenu(ctx, chatID, i18n.F(c.TierHeader.T(lang), "product", name, "stock", itoa(avail)), rows)
}

// cbBackProducts re-runs the buy entry from the tier menu — back:products
// (telegram_bot.py:15019-15022).
func (b *Bot) cbBackProducts(cc *cbCtx) {
	b.cmdBuy(cc.Ctx, cc.ChatID, b.langFor(cc.ChatID, "en"))
}

// cbBuyX handles a tap on a SOLD-OUT / SUSPENDED product button — buyx:
// (telegram_bot.py:15032-15035): just inform, don't enter the flow.
func (b *Bot) cbBuyX(cc *cbCtx) {
	b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OutOfStock.T(b.langFor(cc.ChatID, "en")), nil)
}

// cbBuy resolves a product index and either shows its buy-notice gate or the
// tiers — buy: (telegram_bot.py:15224-15261).
func (b *Bot) cbBuy(cc *cbCtx) {
	c := &i18n.C().Buy
	lang := b.langFor(cc.ChatID, "en")
	if b.salesBlocked(cc.Ctx, cc.ChatID, lang, nil) { // sales paused (stale menu tap)
		return
	}
	if b.pendingGuard(cc.Ctx, cc.ChatID, lang, "buy") {
		return
	}
	products := b.listProducts()
	idx, err := strconv.Atoi(strings.TrimPrefix(cc.Data, "buy:"))
	if err != nil || idx < 0 || idx >= len(products) {
		return // stale button / product removed
	}
	name := products[idx]
	// PRIVATE product this customer can't see (stale/forged button): respond
	// exactly like a dead button — silent — so nothing leaks (@15240-15243).
	if !b.productVisibleTo(cc.ChatID, name) {
		return
	}
	if avail, _ := b.stock.CountAvailable(name); avail <= 0 {
		b.send(cc.Ctx, cc.ChatID, c.OutOfStock.T(lang), nil)
		return
	}
	if b.productSuspended(name) {
		b.send(cc.Ctx, cc.ChatID, c.ProductSuspended.T(lang), nil)
		return
	}
	// Buy notice behind a ✅ Continue gate, when the admin set one (@15252-15260).
	if notice := b.stock.ProductNotice(name); notice != "" {
		rows := [][]tg.InlineKeyboardButton{
			{{Text: c.ProductNoticeContinue.T(lang), CallbackData: cbData("buygo", itoa(idx))}},
			{{Text: c.BackBtn.T(lang), CallbackData: "back:products"}},
		}
		b.sendMenu(cc.Ctx, cc.ChatID, notice, rows)
		return
	}
	b.showProductTiers(cc.Ctx, cc.ChatID, lang, name)
}

// cbBuyGo continues past a product's buy notice to the tiers — buygo:
// (telegram_bot.py:15262-15280).
func (b *Bot) cbBuyGo(cc *cbCtx) {
	lang := b.langFor(cc.ChatID, "en")
	if b.salesBlocked(cc.Ctx, cc.ChatID, lang, nil) {
		return
	}
	if b.pendingGuard(cc.Ctx, cc.ChatID, lang, "buy") {
		return
	}
	products := b.listProducts()
	idx, err := strconv.Atoi(strings.TrimPrefix(cc.Data, "buygo:"))
	if err != nil || idx < 0 || idx >= len(products) {
		return
	}
	name := products[idx]
	if !b.productVisibleTo(cc.ChatID, name) {
		return
	}
	b.showProductTiers(cc.Ctx, cc.ChatID, lang, name)
}

// cbTier stashes the chosen (product, tier) and prompts for a quantity — tier:
// (telegram_bot.py:15281-15329). Format tier:<pidx>:<CODE>.
func (b *Bot) cbTier(cc *cbCtx) {
	c := &i18n.C().Buy
	lang := b.langFor(cc.ChatID, "en")
	parts := strings.Split(cc.Data, ":")
	if len(parts) != 3 {
		return
	}
	// Last line of defense on the order-creating path: a tier tap on an OLD menu
	// must hit the same confirm-first gate as buy:/buygo: (@15288-15292).
	if b.pendingGuard(cc.Ctx, cc.ChatID, lang, "buy") {
		return
	}
	products := b.listProducts()
	idx, err := strconv.Atoi(parts[1])
	if err != nil || idx < 0 || idx >= len(products) {
		return // stale button
	}
	name := products[idx]
	if !b.productVisibleTo(cc.ChatID, name) {
		return // silent private-product gate (@15298-15302)
	}
	code := parts[2]
	unit, ok := b.stock.ProductPrice(name, code)
	if !ok {
		return // unknown / unpriced tier
	}
	stockN, _ := b.stock.CountAvailable(name)
	if stockN <= 0 {
		b.send(cc.Ctx, cc.ChatID, c.SoldOut.T(lang), nil)
		return
	}
	// Drop any abandoned sibling capture so the qty typed next isn't shadowed by
	// a higher-priority stale prompt in the text router (@15312-15315).
	cc.st.ClearCustomerPending()
	cc.st.mu.Lock()
	cc.st.Purchase = &PurchaseCtx{Product: name, Tier: code}
	cc.st.mu.Unlock()
	balance, _ := b.getBalance(cc.ChatID)
	// Quick-amount buttons; hide choices above the admin cap (0 == unlimited).
	cap := b.maxBulk()
	var qtyBtns []tg.InlineKeyboardButton
	for _, n := range qtyChoices {
		if cap == 0 || n <= cap {
			qtyBtns = append(qtyBtns, tg.InlineKeyboardButton{Text: itoa(n), CallbackData: cbData("buyqty", itoa(n))})
		}
	}
	text := i18n.F(c.VolumePrompt.T(lang),
		"product", name, "tier", b.tierLabel(code, lang),
		"unit", b.usdtDisp(int64(unit)), "stock", itoa(stockN),
		"balance", b.usdtDisp(balance), "discounts", discountBlock(lang))
	b.send(cc.Ctx, cc.ChatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{qtyBtns}})
}

// cbBuyQty finalizes on a quick-amount button — buyqty: (telegram_bot.py:
// 15330-15348). Atomically claims the pending purchase so a rapid double-tap
// can't reserve stock twice.
func (b *Bot) cbBuyQty(cc *cbCtx) {
	c := &i18n.C().Buy
	lang := b.langFor(cc.ChatID, "en")
	n, err := strconv.Atoi(strings.TrimPrefix(cc.Data, "buyqty:"))
	if err != nil {
		return
	}
	cap := b.maxBulk()
	if n < 1 || (cap != 0 && n > cap) {
		b.send(cc.Ctx, cc.ChatID, i18n.F(c.QtyTooMany.T(lang), "max", itoa(cap)), nil)
		return
	}
	cc.st.mu.Lock()
	pend := cc.st.Purchase
	cc.st.Purchase = nil
	cc.st.mu.Unlock()
	if pend == nil {
		return // stale prompt or already claimed
	}
	b.finalizePurchase(cc.Ctx, cc.ChatID, lang, pend.Product, pend.Tier, n)
}

// capPurchase is capture-chain entry #48, pending_purchase — the buy-quantity
// free-text capture (telegram_bot.py:18676-18693). PEEK semantics: an escaping
// text (a "/command" or a keyboard label) leaves the slot ARMED and is NOT
// consumed (returns false so the router keeps going); an invalid/over-cap number
// is answered but the slot STAYS armed for a retry (returns true, consumed).
func (b *Bot) capPurchase(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Purchase != nil
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	text := c.Text
	// peek escape: a slash command or a tapped keyboard label leaves the slot
	// armed and falls through to the command router (@18676's `and` guards).
	if strings.HasPrefix(text, "/") || labelToCmd(text) != "" {
		return false
	}
	cat := &i18n.C().Buy
	if !(digitsOK(text)) {
		b.send(c.Ctx, c.ChatID, cat.BadQty.T(c.Lang), nil)
		return true // invalid -> keep pending for retry
	}
	qty, _ := strconv.Atoi(text)
	if qty <= 0 {
		b.send(c.Ctx, c.ChatID, cat.BadQty.T(c.Lang), nil)
		return true
	}
	cap := b.maxBulk()
	if cap != 0 && qty > cap {
		b.send(c.Ctx, c.ChatID, i18n.F(cat.QtyTooMany.T(c.Lang), "max", itoa(cap)), nil)
		return true // over cap -> keep pending for retry
	}
	// Atomically claim the pending purchase so a double-submit (two fast messages
	// racing in the pool) can't reserve stock twice (@18687-18691).
	c.st.mu.Lock()
	pend := c.st.Purchase
	c.st.Purchase = nil
	c.st.mu.Unlock()
	if pend == nil {
		return true
	}
	b.finalizePurchase(c.Ctx, c.ChatID, c.Lang, pend.Product, pend.Tier, qty)
	return true
}
