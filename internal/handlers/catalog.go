package handlers

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// ---- virtual-product sentinels (telegram_bot.py:3752-3803) -----------------
//
// These products reserve no ./accounts folder stock, so the buy/expiry/cancel
// paths must NOT call stock.Release/Finalize against them. The buy flow this
// slice ports only ever creates real-product orders; the sentinels are matched
// here so pending_guard + the expiry/cancel workers behave correctly when a
// later slice's deposit/API-key order is the chat's pending order.
const (
	depositProduct        = store.DepositProduct // "__deposit__"
	apikeyProduct         = "__apikey__"
	kiroApikeyProduct     = "__kiroapi__"
	apikeyRechargeProduct = "__apikeyrc__"
)

// isNoStockProduct reports whether a product reserves no folder stock — the Go
// twin of _NO_STOCK_PRODUCTS (telegram_bot.py:3803).
func isNoStockProduct(product string) bool {
	switch product {
	case depositProduct, apikeyProduct, kiroApikeyProduct, apikeyRechargeProduct:
		return true
	}
	return false
}

// ---- settings-backed sale gates --------------------------------------------

// settings returns the live merged settings, or nil when no config is wired
// (unit tests). Reading a nil map is safe in Go, so every gate below degrades to
// its default (sales open, nothing suspended/private) with no config.
func (b *Bot) settings() map[string]any {
	if b.cfg == nil {
		return nil
	}
	return b.cfg.Settings()
}

// payEnabled reports whether a payment method is admin-enabled — pay_enabled
// (telegram_bot.py:5150-5153). bep20/tron default OFF; the rest ON.
func (b *Bot) payEnabled(method string) bool {
	if b.cfg == nil {
		return method != "bep20" && method != "tron"
	}
	return b.cfg.PayEnabled(method)
}

// maxBulk is the admin per-order account cap (0 = unlimited) — max_bulk
// (telegram_bot.py:5156-5161).
func (b *Bot) maxBulk() int {
	if b.cfg == nil {
		return 0
	}
	return b.cfg.MaxBulk()
}

// productSuspended reports whether an admin has suspended this product —
// product_suspended (telegram_bot.py:5179-5184). settings.json "suspended"
// holds {product: {ts, by}}; presence of the key means suspended.
func (b *Bot) productSuspended(product string) bool {
	susp, _ := b.settings()["suspended"].(map[string]any)
	_, ok := susp[product]
	return ok
}

// productPrivateAllowed returns the private-sale whitelist for a product and
// whether it is PRIVATE at all — product_private_allowed (telegram_bot.py:
// 5216-5224). isPrivate is true even for an empty allow-list (admin-only).
func (b *Bot) productPrivateAllowed(product string) (allowed []string, isPrivate bool) {
	pp, ok := b.settings()["private_products"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := pp[product]
	if !ok {
		return nil, false
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, asStr(x))
	}
	return out, true
}

// productVisibleTo is the pure per-product visibility predicate —
// _product_visible_to (telegram_bot.py:11375-11404). Public product -> everyone;
// admin -> always; private product -> only allow-list entries that are ALSO
// still in the global whitelist (removing a customer from the whitelist revokes
// every private grant, no stale access).
func (b *Bot) productVisibleTo(chatID int64, product string) bool {
	allowed, isPrivate := b.productPrivateAllowed(product)
	if !isPrivate {
		return true
	}
	if b.isAdmin(chatID) {
		return true
	}
	wlVals := map[string]bool{}
	for _, e := range b.whitelist() {
		wlVals[strings.ToLower(e.V)] = true
	}
	cid := fmtInt(chatID)
	h := normHandle(b.handleForChat(chatID))
	for _, v := range allowed {
		if !wlVals[strings.ToLower(v)] {
			continue // dropped from the global list -> revoked
		}
		if isDigits(v) {
			if v == cid {
				return true
			}
		} else if h != "" && strings.ToLower(v) == h {
			return true
		}
	}
	return false
}

// saleBlockedFor is the pure buy-gate decision — _sale_blocked_for
// (telegram_bot.py:11407-11423). admin -> not blocked; sales ON -> not blocked;
// sales OFF + private ON + (reseller|whitelisted) -> not blocked; else blocked.
func (b *Bot) saleBlockedFor(chatID int64) bool {
	if b.isAdmin(chatID) {
		return false
	}
	s := b.settings()
	if paused, _ := s["sales_paused"].(bool); !paused {
		return false
	}
	if priv, _ := s["private_sale"].(bool); priv &&
		(b.isAuthorizedReseller(chatID) || b.isWhitelisted(chatID)) {
		return false
	}
	return true
}

// salesBlocked sends the SALES_PAUSED notice (with the reseller rows appended
// for the /buy entry) and returns true when the chat may not start a new order —
// _sales_blocked (telegram_bot.py:11253-11270). Caller stops on true.
func (b *Bot) salesBlocked(ctx context.Context, chatID int64, lang i18n.Lang, extraRows [][]tg.InlineKeyboardButton) bool {
	if !b.saleBlockedFor(chatID) {
		return false
	}
	c := &i18n.C().Buy
	text := c.SalesPaused.T(lang)
	if len(extraRows) > 0 {
		text += "\n\n" + c.ResellerNote.T(lang)
		b.send(ctx, chatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: extraRows})
	} else {
		b.send(ctx, chatID, text, nil)
	}
	return true
}

// ---- authorized resellers + private-sale whitelist -------------------------

// reseller is one authorized-reseller entry: bare @username, optional note, and
// the per-reseller flat discount % (0 == off) (telegram_bot.py:11318-11341).
type reseller struct {
	U    string
	Note string
	Disc int
}

// resellerRe is the Telegram username rule (5–32 chars, letters/digits/_,
// starts with a letter) — _RESELLER_RE (telegram_bot.py:11280).
var resellerRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{4,31}$`)

// resellerList parses settings.json "resellers" — reseller_list
// (telegram_bot.py:11334-11341). Tolerates legacy bare-string entries.
func (b *Bot) resellerList() []reseller {
	raw, _ := b.settings()["resellers"].([]any)
	var out []reseller
	for _, it := range raw {
		if r := coerceReseller(it); r != nil {
			out = append(out, *r)
		}
	}
	return out
}

func coerceReseller(it any) *reseller {
	switch t := it.(type) {
	case string:
		u := strings.TrimLeft(strings.TrimSpace(t), "@")
		if u == "" {
			return nil
		}
		return &reseller{U: u}
	case map[string]any:
		u := strings.TrimLeft(strings.TrimSpace(asStr(t["u"])), "@")
		if u == "" {
			return nil
		}
		return &reseller{U: u, Note: cleanNote(asStr(t["note"])), Disc: coerceDisc(t["disc"])}
	}
	return nil
}

// resellerButtonRows builds the deep-link reseller rows for the Buy screen —
// _reseller_button_rows (telegram_bot.py:11612-11624). Capped at RESELLER_MAX=20.
func (b *Bot) resellerButtonRows() [][]tg.InlineKeyboardButton {
	list := b.resellerList()
	if len(list) > 20 {
		list = list[:20]
	}
	var rows [][]tg.InlineKeyboardButton
	for _, r := range list {
		label := "👑 @" + r.U
		if r.Note != "" {
			label += " — " + r.Note
		}
		rows = append(rows, []tg.InlineKeyboardButton{{Text: label, URL: "https://t.me/" + r.U}})
	}
	return rows
}

// resellerPctFor is this buyer's per-reseller baseline discount % (0 for a
// non-reseller / no discount) — _reseller_pct_for (telegram_bot.py:11508-11522).
func (b *Bot) resellerPctFor(chatID int64) int {
	h := normHandle(b.handleForChat(chatID))
	if h == "" {
		return 0
	}
	for _, r := range b.resellerList() {
		if strings.ToLower(r.U) == h {
			return r.Disc
		}
	}
	return 0
}

// isAuthorizedReseller reports whether this chat's @handle is an authorized
// reseller — _is_authorized_reseller (telegram_bot.py:11344-11353).
func (b *Bot) isAuthorizedReseller(chatID int64) bool {
	h := normHandle(b.handleForChat(chatID))
	if h == "" {
		return false
	}
	for _, r := range b.resellerList() {
		if strings.ToLower(r.U) == h {
			return true
		}
	}
	return false
}

// wlEntry is one private-sale whitelist entry (bare id or @username + note).
type wlEntry struct{ V, Note string }

// whitelist parses settings.json "whitelist" — whitelist (telegram_bot.py:
// 11564-11570). Tolerates legacy bare strings; drops invalid values.
func (b *Bot) whitelist() []wlEntry {
	raw, _ := b.settings()["whitelist"].([]any)
	var out []wlEntry
	for _, it := range raw {
		switch t := it.(type) {
		case string:
			if v := normWLValue(t); v != "" {
				out = append(out, wlEntry{V: v})
			}
		case map[string]any:
			if v := normWLValue(asStr(t["v"])); v != "" {
				out = append(out, wlEntry{V: v, Note: cleanNote(asStr(t["note"]))})
			}
		}
	}
	return out
}

// isWhitelisted matches this chat against the whitelist by numeric id or @handle
// — _is_whitelisted (telegram_bot.py:11356-11372).
func (b *Bot) isWhitelisted(chatID int64) bool {
	cid := fmtInt(chatID)
	h := normHandle(b.handleForChat(chatID))
	for _, e := range b.whitelist() {
		if isDigits(e.V) {
			if e.V == cid {
				return true
			}
		} else if h != "" && strings.ToLower(e.V) == h {
			return true
		}
	}
	return false
}

// handleForChat returns this chat's @handle from the runtime cache, else the
// persisted directory, else "" — the source used by every reseller/whitelist
// match (telegram_bot.py:11349, 11363).
func (b *Bot) handleForChat(chatID int64) string {
	b.handlesMu.Lock()
	h := b.handles[chatID]
	b.handlesMu.Unlock()
	if h != "" {
		return h
	}
	if b.store != nil {
		if hs, err := b.store.HandleForChat(fmtInt(chatID)); err == nil && hs != "" {
			return hs
		}
	}
	return ""
}

// ---- pricing (volume discount only; codes are M2.6) ------------------------

// defaultDiscountTiers is _DISCOUNT_DEFAULT (telegram_bot.py:5078), checked
// high-to-low. Used when discounts.json is absent — the admin-editable
// discounts.json loader lands with the pricing-admin slice (M3.3); until then
// this is the fresh-install behaviour, exactly as Python (load_discounts @6474).
var defaultDiscountTiers = []struct {
	Min  int
	Frac float64
}{{50, 0.30}, {20, 0.20}, {10, 0.10}}

// discountFor returns the volume-discount fraction for a quantity — discount_for
// (telegram_bot.py:6504-6509).
func discountFor(qty int) float64 {
	for _, t := range defaultDiscountTiers {
		if qty >= t.Min {
			return t.Frac
		}
	}
	return 0
}

// codedTotal computes (total, effectivePct, volumePct) for unit×qty under the
// baseline (NO-code) pricing — the kind=None branch of _coded_total
// (telegram_bot.py:10107-10135). A reseller flat rate REPLACES the volume tier
// when resellerPct>0; else the volume tier applies.
//
// M2.6 note: this is now a thin wrapper over codedTotalFull (code.go), which
// carries the kind/percent/stack branches. ONE implementation of the discount
// maths means a coded and an uncoded quote for the same order can never drift.
func codedTotal(unit, qty, resellerPct int) (total, effPct, volPct int) {
	return codedTotalFull(unit, qty, "", 0, false, resellerPct)
}

// discountBlock renders the current volume-discount tiers as a human list —
// discount_block (telegram_bot.py:6517-6536). Built from the default tiers
// (ascending min_qty) until discounts.json wiring lands (M3.3).
func discountBlock(lang i18n.Lang) string {
	tiers := []struct {
		Min  int
		Frac float64
	}{{10, 0.10}, {20, 0.20}, {50, 0.30}}
	c := &i18n.C().Buy
	unit := c.DiscountUnit.T(lang)
	lines := []string{c.DiscountHeader.T(lang)}
	for i, t := range tiers {
		var rng string
		if i+1 < len(tiers) {
			rng = fmt.Sprintf("%d-%d", t.Min, tiers[i+1].Min-1)
		} else {
			rng = fmt.Sprintf("%d+", t.Min)
		}
		lines = append(lines, fmt.Sprintf("=> %s %s  -%d%%", rng, unit, int(math.Round(t.Frac*100))))
	}
	return strings.Join(lines, "\n")
}

// tierDayRe / tierHourRe match the two duration-bearing tier codes for the
// localized label (telegram_bot.py:4664-4671).
var (
	tierDayRe  = regexp.MustCompile(`^BH(\d+)D$`)
	tierHourRe = regexp.MustCompile(`^BH(\d+)H$`)
)

// tierLabel renders a warranty code into a localized policy label — tier_label
// (telegram_bot.py:4659-4672). Unknown formats show the raw code.
func (b *Bot) tierLabel(code string, lang i18n.Lang) string {
	cu := strings.ToUpper(code)
	c := &i18n.C().Buy
	if cu == "KBH" {
		return c.TierNoWarranty.T(lang)
	}
	if m := tierDayRe.FindStringSubmatch(cu); m != nil {
		return i18n.F(c.TierDaysWarranty.T(lang), "n", m[1])
	}
	if m := tierHourRe.FindStringSubmatch(cu); m != nil {
		return i18n.F(c.TierHoursWarranty.T(lang), "n", m[1])
	}
	return code
}

// ---- product-click anti-spam rate limiter ----------------------------------

// productClickAllowed is the per-chat anti-spam gate for product-name taps —
// _product_click_allowed (telegram_bot.py:12191-12218). Returns (allowed,
// retryAfterSec). Allows up to clickLimit taps per rolling clickWindow; the tap
// that would exceed the cap starts a clickCooldown lockout (every tap refused
// until it elapses). Per-chat state (ProductClicks/ProductCooldownUntil) lives
// on ChatState under st.mu (handlers-go.md §3), replacing Python's global maps.
func (b *Bot) productClickAllowed(st *ChatState) (bool, int) {
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ProductCooldownUntil.IsZero() {
		if now.Before(st.ProductCooldownUntil) {
			return false, int(st.ProductCooldownUntil.Sub(now).Seconds()) + 1 // still locked out
		}
		st.ProductCooldownUntil = time.Time{} // lockout elapsed -> reset
	}
	// Prune taps outside the rolling window.
	var log []time.Time
	for _, t := range st.ProductClicks {
		if now.Sub(t) < b.clickWindow {
			log = append(log, t)
		}
	}
	if len(log) >= b.clickLimit { // this tap would exceed the cap
		st.ProductCooldownUntil = now.Add(b.clickCooldown)
		st.ProductClicks = nil // cooldown now owns the throttle
		return false, int(b.clickCooldown.Seconds())
	}
	log = append(log, now)
	st.ProductClicks = log
	return true, 0
}

// ---- rendering + small helpers ---------------------------------------------

// sendMenu sends an inline-button menu, first deleting this chat's previous menu
// — send_menu (telegram_bot.py:4233-4250). The single choke point for
// user-facing button menus: tracks the new message so the next send_menu (or a
// button tap, handled in handleCallback) can clear it, keeping stale menus from
// piling up.
func (b *Bot) sendMenu(ctx context.Context, chatID int64, text string, rows [][]tg.InlineKeyboardButton) {
	st := b.states.Get(chatID)
	st.mu.Lock()
	prev := st.LastMenu
	st.LastMenu = 0
	st.mu.Unlock()
	if prev != 0 {
		_ = b.tg.DeleteMessage(ctx, chatID, prev) // best-effort (@4243)
	}
	m, err := b.tg.SendMessage(ctx, tg.SendMessageParams{
		ChatID: chatID, Text: text,
		ReplyMarkup: &tg.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
	if err != nil {
		log.Println("send_menu failed:", err)
		return
	}
	if m != nil && m.MessageID != 0 {
		st.mu.Lock()
		st.LastMenu = m.MessageID
		st.mu.Unlock()
	}
}

// langSafe returns an order's language with an "en" fallback — lang_safe
// (telegram_bot.py:8969-8971).
func langSafe(o *store.Order) i18n.Lang {
	if o == nil || o.Lang == "" {
		return i18n.EN
	}
	return i18n.Lang(o.Lang)
}

// fmtInt formats a chat id as the string form the store/settings use for keys.
func fmtInt(chatID int64) string { return strconv.FormatInt(chatID, 10) }

// normHandle strips a leading @, trims, and lowercases a handle for matching.
func normHandle(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimLeft(h, "@")))
}

// isDigits reports whether s is all ASCII decimal digits (a chat-id, not a
// username) — the digit branch of _norm_wl_value (telegram_bot.py:11547).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// digitsOK ports _digits (telegram_bot.py:6768-6777): non-empty, ≤12 chars, all
// ASCII decimal digits (a safe int() pre-check).
func digitsOK(s string) bool {
	return s != "" && len(s) <= 12 && isDigits(s)
}

// asStr coerces a JSON-decoded settings value to its string form — the Go twin
// of Python str(x). A float64 (JSON number) that is a whole number renders as an
// integer so a numeric chat-id doesn't become "7.3e+09".
func asStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// cleanNote sanitizes a reseller/whitelist note for a single-line button label —
// _clean_note (telegram_bot.py:11302-11306): collapse whitespace, cap at 40
// runes.
func cleanNote(s string) string {
	j := strings.Join(strings.Fields(s), " ")
	if r := []rune(j); len(r) > 40 {
		j = string(r[:40])
	}
	return strings.TrimSpace(j)
}

// coerceDisc clamps a stored per-reseller discount to an int percent in [0, 95]
// — _coerce_disc (telegram_bot.py:11309-11315).
func coerceDisc(v any) int {
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case int:
		n = t
	case string:
		p, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0
		}
		n = p
	default:
		return 0
	}
	if n < 0 {
		return 0
	}
	if n > 95 {
		return 95
	}
	return n
}

// normWLValue normalizes a typed whitelist value to a bare id-or-username, or ""
// — _norm_wl_value (telegram_bot.py:11536-11549).
func normWLValue(s string) string {
	u := strings.TrimSpace(s)
	if u == "" {
		return ""
	}
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:] // strip any t.me/ prefix
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i] // drop ?query / #fragment
	}
	u = strings.TrimSpace(strings.TrimLeft(u, "@"))
	if isDigits(u) {
		if len(u) >= 5 && len(u) <= 15 {
			return u
		}
		return ""
	}
	if resellerRe.MatchString(u) {
		return u
	}
	return ""
}
