package handlers

// apikey_keys.go — M2.5 slice, part 3: the buyer-facing key views.
//
// Ports _mask_api_key (telegram_bot.py:4220-4224), _show_my_api_keys
// (telegram_bot.py:4293-4369) and the apikeyrev: reveal callback
// (telegram_bot.py:15571-15592).
//
// SECURITY posture of this file: the cleartext key is the BUYER'S OWN secret,
// so re-sending it on demand is fine — but only ever to the chat that owns the
// row, and never for a REFUNDED key (the money went back and the key was
// revoked server-side). Lists and button labels show the masked form only.

import (
	"context"
	"html"
	"log"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/tg"
)

// maskAPIKey is the shortened display form of an sk-… key (prefix + tail only)
// for lists and button labels — _mask_api_key (telegram_bot.py:4220-4224). The
// full cleartext is only ever sent via the reveal/delivery path.
func maskAPIKey(k string) string {
	if len([]rune(k)) > 14 {
		r := []rune(k)
		return string(r[:7]) + "…" + string(r[len(r)-4:])
	}
	return k
}

// tgSendHTML builds the parse_mode=HTML, no-link-preview send params the
// delivery/reveal messages use (telegram_bot.py:7646-7654, 15586-15592).
func tgSendHTML(chatID int64, text string) tg.SendMessageParams {
	return tg.SendMessageParams{
		ChatID:                chatID,
		Text:                  text,
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	}
}

// sendHTMLKB sends a parse_mode=HTML admin message with an optional inline
// keyboard — for screens whose body uses <b>/<code> markup and also needs a
// reply_markup, which neither tgSendHTML nor apikey_deliver.go's sendHTML
// carries. Originally added for the AWS IDC provisioning admin screens (since
// removed from this repo); kept here as a shared helper because adminstock.go's
// 🌐 Provision Kiro pool screen needs the exact same shape.
func (b *Bot) sendHTMLKB(ctx context.Context, chatID int64, text string, markup any) {
	p := tg.SendMessageParams{ChatID: chatID, Text: text, ParseMode: "HTML"}
	if markup != nil {
		p.ReplyMarkup = markup
	}
	if _, err := b.tg.SendMessage(ctx, p); err != nil {
		log.Println("sendHTMLKB: sendMessage failed:", err)
	}
}

// showMyAPIKeys renders Self-Service → My API Keys: this buyer's minted keys,
// newest first — _show_my_api_keys (telegram_bot.py:4293-4369).
//
// Per key: masked key, SKU tag, live status + used/remaining from Kiro-Go
// /admin/stats, purchase date — degrading to the locally stored info with a
// "stats unavailable" note when the stats call fails. ONE batched stats call
// for ALL keys (empty filter = every key), matched locally by key id / mint
// name — never one HTTP round-trip per key: this is a customer-facing handler
// on the shared worker pool, so it makes a single bounded attempt and degrades
// rather than pinning a worker.
func (b *Bot) showMyAPIKeys(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().APIKey
	keys, err := b.store.APIKeysForChat(chatID, 10, false)
	if err != nil {
		log.Println("api_keys_for_chat failed:", err)
		return
	}
	if len(keys) == 0 {
		b.send(ctx, chatID, c.MyKeysEmpty.T(lang), nil)
		return
	}
	statsByID, statsByName, statsFailed := b.batchedKeyStats(ctx)

	lines := []string{c.MyKeysHeader.T(lang)}
	var rows [][]tg.InlineKeyboardButton
	for _, k := range keys {
		masked := maskAPIKey(k.APIKey)
		bought := roundCredits(k.Credits)
		when := "—"
		if k.CreatedAt != 0 {
			when = fmtWhen(float64(k.CreatedAt), lang)
		}
		// Match this row's live stats by Kiro-Go key id, falling back to the
		// deterministic mint name for legacy rows stored without an id.
		stat := statsByID[k.KeyID]
		if stat == nil {
			stat = statsByName["tg-"+fmtInt(k.ChatID)+"-order-"+k.OrderID]
		}
		// SKU tag: the STORED class wins; a legacy '' row falls back to the LIVE
		// stats row's class, and only then to the bedrock default — so an
		// operator who classed a legacy key kiro server-side still sees it
		// labelled (and unit-labelled) correctly here (telegram_bot.py:4340-4342).
		// DISPLAY ONLY: the recharge guard must keep reading the raw stored value.
		rowCls := k.SaleClass
		if rowCls == "" && stat != nil {
			rowCls = stat.SaleClass
		}
		if rowCls != saleClassKiro {
			rowCls = saleClassBedrock
		}
		tag := c.SkuTag[rowCls].T(lang)
		// A REFUNDED key is never re-served: show it struck from the list (no
		// live stats, no reveal button) so the buyer sees why it disappeared.
		if k.RefundedTS != nil {
			lines = append(lines, "\n🔑 "+masked+" · "+tag+" — ↩️ refunded\n   "+
				i18n.F(c.MyKeysLineLocal.T(lang), "bought", itoa64(bought), "when", when))
			continue
		}
		if stat != nil {
			status := strings.ToLower(stat.Status)
			if status == "" {
				status = "active"
			}
			label := status
			if s, ok := c.Status[status]; ok {
				label = s.T(lang)
			}
			// -1 = unlimited (defensive; sold keys always carry a quota).
			// CreditsRemaining/CreditsUsed are fractional now (kirogo.Remaining
			// and KeyStat.CreditsUsed are float64-backed — live keys accrue
			// sub-1-credit usage), so round to the nearest whole credit for
			// display via roundCredits rather than truncating with int64(...)
			// (would show 1999 instead of 2000 for a 1999.95 balance) or
			// printing the float directly (would show "1999.950000").
			left := "∞"
			if !stat.CreditsRemaining.IsUnlimited() && stat.CreditsRemaining >= 0 {
				left = itoa64(roundCredits(float64(stat.CreditsRemaining)))
			}
			detail := i18n.F(c.MyKeysLineStats.T(lang),
				"used", itoa64(roundCredits(stat.CreditsUsed)), "bought", itoa64(bought),
				"left", left, "when", when)
			lines = append(lines, "\n🔑 "+masked+" · "+tag+" — "+label+"\n   "+detail)
		} else {
			statsFailed = true
			lines = append(lines, "\n🔑 "+masked+" · "+tag+"\n   "+
				i18n.F(c.MyKeysLineLocal.T(lang), "bought", itoa64(bought), "when", when))
		}
		// Reveal button re-sends this key's delivery message (cleartext key + the
		// web-app guidance links); the callback carries the order id and
		// ownership is re-checked in the handler.
		rows = append(rows, btnRow("👁 "+masked, cbData("apikeyrev", k.OrderID)))
	}
	if statsFailed {
		lines = append(lines, "\n"+c.MyKeysStatsNA.T(lang))
	}
	b.sendMenu(ctx, chatID, strings.Join(lines, "\n"), rows)
}

// batchedKeyStats fetches EVERY key's live stats in one /admin/stats call and
// indexes them by id and by mint name (telegram_bot.py:4312-4324). failed
// reports that the call itself died, so the caller can append the
// "stats unavailable" note.
func (b *Bot) batchedKeyStats(ctx context.Context) (byID, byName map[string]*kirogo.KeyStat, failed bool) {
	byID, byName = map[string]*kirogo.KeyStat{}, map[string]*kirogo.KeyStat{}
	client := b.kirogoClient()
	if client == nil {
		return byID, byName, true
	}
	// Bounded like every other display-path stats call (see statsTimeout): a
	// hung Kiro-Go must degrade the view, never pin a shared worker.
	cctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	// An empty filter returns EVERY key, masked (KIRO_GO_API.md:106).
	res, err := client.Stats(cctx, kirogo.StatsRequest{})
	if err != nil {
		// Body never logged (invariant 2) — the error text alone.
		log.Println("mykeys stats failed:", err)
		return byID, byName, true
	}
	for i := range res.Keys {
		s := &res.Keys[i]
		if s.ID != "" {
			byID[s.ID] = s
		}
		if s.Name != "" {
			byName[s.Name] = s
		}
	}
	return byID, byName, false
}

// cbApikeyRev handles apikeyrev:<order des> — the per-key 👁 reveal from My API
// Keys and the /history API-key row (telegram_bot.py:15571-15592). Re-sends the
// delivery message (cleartext key + guidance links).
//
// Ownership guard: the key row's chat must match, so a guessed/forged order id
// reveals nothing. A REFUNDED key behaves like a dead button — the money went
// back and the key was revoked server-side.
func (b *Bot) cbApikeyRev(cc *cbCtx) {
	c := &i18n.C().APIKey
	lang := b.langFor(cc.ChatID, "en")
	des := strings.TrimPrefix(cc.Data, "apikeyrev:")
	row, err := b.store.APIKeyForOrder(des)
	if err != nil || row == nil || row.ChatID != cc.ChatID {
		return
	}
	if row.RefundedTS != nil {
		return
	}
	// SKU-correct wording/units: a kiro key re-reveals with the kiro delivery
	// message, everything else (bedrock + legacy '') the bedrock one.
	tmpl := c.Delivered
	if apikeyKeyClass(row) == saleClassKiro {
		tmpl = c.KiroDelivered
	}
	b.sendHTML(cc.Ctx, cc.ChatID, i18n.F(tmpl.T(lang),
		"credits", itoa64(roundCredits(row.Credits)),
		"key", html.EscapeString(row.APIKey),
		"base", html.EscapeString(b.kiroGoPublicURL())))
}
