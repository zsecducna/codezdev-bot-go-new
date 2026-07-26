package handlers

// history.go — the /history views (per-product summary + detailed per-order
// list) and the self-serve re-download of a delivered order file. Ports:
//   - _send_history @16532, _history_summary_text @16490, _history_text @16211,
//   - the hist: range toggle @12293 and redl: re-download @12299.
//
// API-key-family history rows (APIKEY_HIST_LABEL etc. and apikeyrev: reveal
// buttons) are M2.5: no API-key orders exist until then, so the summary/detail
// render every row generically and the redl loop skips virtual products.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// History paging constants (telegram_bot.py:530/560/576/580).
const (
	historyDefaultN  = 5    // HISTORY_DEFAULT_N @530
	redlMaxBtns      = 8    // REDL_MAX_BTNS @560
	historyMaxRows   = 50   // HISTORY_MAX_ROWS @576 — per-chat line/row cap
	historyMsgBudget = 3800 // HISTORY_MSG_BUDGET @580 — char budget (Telegram <4096)
)

// cmdHistory renders the default /history view — the cmd=="history" branch
// (telegram_bot.py:11681-11685): the per-product purchase summary.
func (b *Bot) cmdHistory(ctx context.Context, chatID int64, lang i18n.Lang) {
	b.sendHistory(ctx, chatID, lang, "summary")
}

// cbHist re-sends history for the tapped range/view — hist: (telegram_bot.py:
// 12293-12296).
func (b *Bot) cbHist(cc *cbCtx) {
	b.sendHistory(cc.Ctx, cc.ChatID, b.langFor(cc.ChatID, "en"), strings.TrimPrefix(cc.Data, "hist:"))
}

// sendHistory renders one history view — _send_history (telegram_bot.py:
// 16532-16594). "summary" (default) is the per-product purchase summary with a
// detail toggle; "5"/"all" are the detailed per-order lists with re-download
// buttons, a range toggle and a Summary button back.
func (b *Bot) sendHistory(ctx context.Context, chatID int64, lang i18n.Lang, key string) {
	h := &i18n.C().History
	if key == "summary" {
		text, hasPurchases := b.historySummaryText(chatID, lang)
		var rows [][]tg.InlineKeyboardButton
		// Offer the detail toggle whenever the chat has ANY order (even non-
		// delivered ones the summary doesn't count); hide it only for truly none.
		if hasPurchases || b.hasAnyOrder(chatID) {
			rows = append(rows, btnRow(h.BtnDetails.T(lang), "hist:5"))
		}
		if len(rows) > 0 {
			b.send(ctx, chatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
		} else {
			b.send(ctx, chatID, text, nil)
		}
		return
	}
	if key != "all" { // normalize; only two real ranges
		key = "5"
	}
	text, toggle := b.historyText(chatID, lang, key)
	var rows [][]tg.InlineKeyboardButton
	// Re-download buttons mirror the text's range: recent delivered, real
	// (non-deposit) order files, newest first, capped to N buttons.
	nBtns := historyDefaultN
	if key == "all" {
		nBtns = redlMaxBtns
	}
	delivered, err := b.store.OrdersForChat(fmtInt(chatID), historyMaxRows, []string{"delivered"}, nil)
	if err != nil {
		log.Println("history delivered query failed:", err)
	}
	// Deposits are filtered out BEFORE the N-button slice (they buy no file), so
	// they never consume a slot; every other delivered order DOES consume one —
	// including the API-key rows below, which either swap in a reveal button or
	// (recharges) render nothing at all. That ordering is Python's
	// (telegram_bot.py:16786-16789: filter, then `delivered[:n_btns]`) and it is
	// load-bearing: skipping an apikey row without consuming its slot would push
	// OLDER orders into the button list than the text above ever shows.
	var eligible []*store.Order
	for _, o := range delivered {
		if o.Product != depositProduct {
			eligible = append(eligible, o)
		}
	}
	eligible = eligible[:min(nBtns, len(eligible))]
	for _, o := range eligible {
		when := localizeTime(o.TS, lang).Format("01-02") // compact %m-%d
		// API-key orders have no order FILE: their button re-sends the key via
		// the ownership-gated reveal path instead of a dead redl: tap (and shows
		// a friendly label, not the raw "__apikey__" sentinel) — @16793-16799.
		if isApikeyBuyProduct(o.Product) {
			icon := "🔑"
			if o.Product == kiroApikeyProduct {
				icon = "🟣"
			}
			rows = append(rows, btnRow(icon+" API key "+when, cbData("apikeyrev", o.Des)))
			continue
		}
		// Recharge orders have neither a file nor an own key to reveal (the
		// credits merged into the target key) — no button at all (@16801-16804).
		if o.Product == apikeyRechargeProduct {
			continue
		}
		label := cut(o.Product, 24) + " " + when
		rows = append(rows, btnRow(i18n.F(h.RedlBtn.T(lang), "label", label), cbData("redl", o.Des)))
	}
	switch toggle {
	case "5":
		rows = append(rows, btnRow(h.BtnAll.T(lang), "hist:all"))
	case "all":
		rows = append(rows, btnRow(h.BtnRecent.T(lang), "hist:5"))
	}
	rows = append(rows, btnRow(h.BtnSummary.T(lang), "hist:summary")) // back to summary
	b.send(ctx, chatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// historySummaryText builds the per-product purchase summary — _history_summary_text
// (telegram_bot.py:16490-16529). Σ accounts excludes API-key-family rows (their
// qty is credits); Σ paid is complete. Returns (text, hasPurchases).
func (b *Bot) historySummaryText(chatID int64, lang i18n.Lang) (string, bool) {
	h := &i18n.C().History
	items, err := b.store.DeliveredProductSummary(fmtInt(chatID))
	if err != nil {
		log.Println("delivered_product_summary failed:", err)
	}
	if len(items) == 0 {
		return h.Empty.T(lang), false
	}
	var totAcc, totPaid int64
	for _, r := range items {
		if !isApikeyFamily(r.Product) { // apikey qty=credits would inflate the count
			totAcc += r.Accounts
		}
		totPaid += r.Paid
	}
	const maxProducts = 40
	ak := &i18n.C().APIKey
	lines := []string{h.SummaryHeader.T(lang), ""}
	for i, r := range items {
		if i >= maxProducts {
			break
		}
		// API-key rows render with the credits label instead of the raw
		// "__apikey__"/"__kiroapi__"/"__apikeyrc__" sentinel and the "accounts"
		// wording (telegram_bot.py:16733-16746).
		var label i18n.Str
		switch r.Product {
		case apikeyProduct:
			label = ak.HistLabel
		case kiroApikeyProduct:
			label = ak.KiroHistLabel
		case apikeyRechargeProduct:
			label = ak.RCHistLabel
		}
		if label != (i18n.Str{}) {
			lines = append(lines, i18n.F(label.T(lang), "credits", itoa64(r.Accounts))+
				" · "+b.usdtDisp(r.Paid))
			continue
		}
		lines = append(lines, i18n.F(h.SummaryLine.T(lang),
			"product", cut(r.Product, 32), "n", itoa64(r.Accounts), "amount", b.usdtDisp(r.Paid)))
	}
	if len(items) > maxProducts {
		lines = append(lines, i18n.F(h.SummaryMore.T(lang), "n", itoa(len(items)-maxProducts)))
	}
	lines = append(lines, "") // Σ over ALL products, not just shown
	lines = append(lines, i18n.F(h.SummaryTotal.T(lang), "n", itoa64(totAcc), "amount", b.usdtDisp(totPaid)))
	return strings.Join(lines, "\n"), true
}

// historyText builds the detailed per-order list for range key ('5'/'all') —
// _history_text (telegram_bot.py:16211-16274). '5' shows the last N by count with
// a "show all" toggle when older orders exist; 'all' is unbounded but capped to
// HISTORY_MAX_ROWS rows and the char budget. Returns (text, toggleKey) where
// toggleKey is '5'/'all'/'' (nothing more to toggle to).
func (b *Bot) historyText(chatID int64, lang i18n.Lang, key string) (string, string) {
	h := &i18n.C().History
	var orders []*store.Order
	var toggle string
	if key == "all" {
		orders, _ = b.store.OrdersForChat(fmtInt(chatID), historyMaxRows, nil, nil)
		toggle = "all"
	} else {
		// Fetch one extra so we know whether older orders exist (only then is the
		// "show all" toggle worth offering).
		rows, _ := b.store.OrdersForChat(fmtInt(chatID), historyDefaultN+1, nil, nil)
		if len(rows) > historyDefaultN {
			orders, toggle = rows[:historyDefaultN], "5"
		} else {
			orders = rows
		}
		key = "5" // normalize any non-'all' key
	}
	if len(orders) == 0 {
		return h.Empty.T(lang), ""
	}
	note := i18n.F(h.RangeNote[key].T(lang), "n", itoa(historyDefaultN))
	lines := []string{h.Header.T(lang) + " " + note, ""}
	used := 0
	for _, s := range lines {
		used += len(s) + 1
	}
	shown := 0
	truncated := false
	for _, o := range orders {
		status := statusLabel(o.Status, lang)
		when := fmtWhen(o.TS, lang)
		var head string
		ak := &i18n.C().APIKey
		switch o.Product {
		case depositProduct:
			head = status + " · " + i18n.C().Deposit.Label.T(lang) // DEPOSIT_LABEL wallet top-up row
		case apikeyProduct:
			// API-key order: qty holds the CREDITS bought; no warranty tier (@16474).
			head = status + " · " + i18n.F(ak.HistLabel.T(lang), "credits", itoa64(o.Qty))
		case kiroApikeyProduct:
			// Kiro-SKU key order: same shape, Kiro-credit wording (@16477).
			head = status + " · " + i18n.F(ak.KiroHistLabel.T(lang), "credits", itoa64(o.Qty))
		case apikeyRechargeProduct:
			// Recharge order: qty is the topped-up credits; own 🔁 label (@16480).
			head = status + " · " + i18n.F(ak.RCHistLabel.T(lang), "credits", itoa64(o.Qty))
		default:
			head = status + " · " + itoa64(o.Qty) + "× " + o.Product + " (" + b.tierLabel(o.Tier, lang) + ")"
		}
		block := head + "\n   💰 " + b.usdtDisp(o.Total) + " · " + when + "\n   🆔 " + o.Des
		if used+len(block)+1 > historyMsgBudget {
			truncated = true
			break
		}
		lines = append(lines, block)
		used += len(block) + 1
		shown++
	}
	if truncated || (key == "all" && shown < len(orders)) {
		lines = append(lines, "")
		lines = append(lines, i18n.F(h.Truncated.T(lang), "n", itoa(shown)))
	}
	return strings.Join(lines, "\n"), toggle
}

// cbRedl re-sends a delivered order's file(s) — redl: (telegram_bot.py:
// 12299-12337). Ownership + state gated: only the buyer, only a delivered real-
// account order (virtual products have no file). Sends the .zip if present, else
// the raw order files.
func (b *Bot) cbRedl(cc *cbCtx) {
	h := &i18n.C().History
	lang := b.langFor(cc.ChatID, "en")
	des := strings.TrimPrefix(cc.Data, "redl:")
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil || o.ChatID != fmtInt(cc.ChatID) ||
		o.Status != "delivered" || isNoStockProduct(o.Product) {
		b.send(cc.Ctx, cc.ChatID, h.RedlNone.T(lang), nil)
		return
	}
	paths := b.orderRedlPaths(des)
	if len(paths) == 0 {
		b.send(cc.Ctx, cc.ChatID, h.RedlNone.T(lang), nil)
		return
	}
	b.send(cc.Ctx, cc.ChatID, h.RedlSending.T(lang), nil)
	sentAny := false
	for _, fp := range paths {
		if _, err := b.tg.SendDocument(cc.Ctx, cc.ChatID, fp, ""); err == nil {
			sentAny = true
		} else {
			log.Println("redl send file error:", err)
		}
	}
	// If every send failed, tell the customer so they know to tap again (@12334).
	if !sentAny {
		b.send(cc.Ctx, cc.ChatID, h.RedlNone.T(lang), nil)
	}
}

// orderRedlPaths returns the delivery artifact(s) to re-send: the .zip for JSON
// orders if present, otherwise the raw order files — the same selection redl:
// uses (telegram_bot.py:12317-12318).
func (b *Bot) orderRedlPaths(des string) []string {
	if b.stock == nil {
		return nil
	}
	zip := filepath.Join(b.stock.OrdersDir, des+".zip")
	if _, err := os.Stat(zip); err == nil {
		return []string{zip}
	}
	return b.stock.OrderFiles(des)
}

// hasAnyOrder reports whether the chat has at least one order of any kind — the
// `store.orders_for_chat(chat_id, limit=1)` guard (@16547).
func (b *Bot) hasAnyOrder(chatID int64) bool {
	rows, err := b.store.OrdersForChat(fmtInt(chatID), 1, nil, nil)
	return err == nil && len(rows) > 0
}

// isApikeyFamily reports an API-key-family sentinel product (sale or recharge) —
// _APIKEY_PRODUCTS membership (telegram_bot.py:3805). Their qty column holds
// CREDITS, so the summary excludes them from the "accounts bought" total.
func isApikeyFamily(product string) bool {
	switch product {
	case apikeyProduct, kiroApikeyProduct, apikeyRechargeProduct:
		return true
	}
	return false
}

// cut truncates s to at most n runes (Python's str[:n]) without splitting a
// multi-byte rune.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
