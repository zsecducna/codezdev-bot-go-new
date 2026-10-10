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
	"sync"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// maskAPIKey is the shortened display form of an sk-… key (prefix + tail only)
// for lists and button labels — _mask_api_key (telegram_bot.py:4220-4224). The
// full cleartext is only ever sent via the reveal/delivery path.
//
// Real keys always exceed the 14-rune threshold below (the server's own
// masking scheme is a 6-char prefix + 4-char tail + a middle worth hiding, so
// anything real is longer), so the >14 branch is the one that actually fires
// in production. The <=14 branch is hardening, not a fix for an observed
// leak: keyShapeRe (internal/kirogo) accepts "sk-"/"ksk_" + 8 chars — 11
// runes — as a well-formed key shape, so the grammar alone permits a value
// short enough to have rendered verbatim here; nothing in this repo has ever
// measured a real key that short. Masking unconditionally below the
// threshold closes that gap for free rather than leaving it to inference.
func maskAPIKey(k string) string {
	if len([]rune(k)) > 14 {
		r := []rune(k)
		return string(r[:7]) + "…" + string(r[len(r)-4:])
	}
	return strings.Repeat("•", len([]rune(k)))
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
// Per key: masked key, SKU tag, live status + used/remaining, purchase date —
// degrading to the locally stored info with a "stats unavailable" note when
// the stats fetch fails. TWO fetch shapes since the llmgate cutover, split by
// row backend: POOL rows keep the ONE batched /admin/stats call for ALL their
// keys (empty filter = every key, matched locally by key id / mint name —
// never one HTTP round-trip per key); LLMGATE rows have no batch endpoint, so
// each is a per-key KeyStats read, run in parallel but bounded (see
// llmgateKeyStatsParallel). Either way this is a customer-facing handler on
// the shared worker pool, so it makes bounded attempts and degrades rather
// than pinning a worker.
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
	// The batched pool call runs only when the page actually holds a pool row:
	// with llmgate-only inventory and no pool configured it would return
	// failed=true unconditionally (kirogoClient == nil) and pin a bogus
	// "stats unavailable" note under keys whose stats are about to load fine.
	anyPool := false
	for _, k := range keys {
		if k.Backend != store.BackendLlmgate {
			anyPool = true
			break
		}
	}
	var statsByID, statsByName map[string]*kirogo.KeyStat
	statsFailed := false
	if anyPool {
		statsByID, statsByName, statsFailed = b.batchedKeyStats(ctx)
	}
	lgStats := b.llmgateKeyStatsParallel(ctx, keys)

	lines := []string{c.MyKeysHeader.T(lang)}
	var rows [][]tg.InlineKeyboardButton
	for _, k := range keys {
		masked := maskAPIKey(k.APIKey)
		bought := roundCredits(k.Credits)
		when := "—"
		if k.CreatedAt != 0 {
			when = fmtWhen(float64(k.CreatedAt), lang)
		}
		// This row's live stats, by backend: llmgate rows read their per-key
		// parallel fetch; pool rows match the batched call by Kiro-Go key id,
		// falling back to the deterministic mint name for legacy rows stored
		// without an id.
		var stat *keyLiveStats
		if k.Backend == store.BackendLlmgate {
			stat = lgStats[k.OrderID]
		} else {
			ps := statsByID[k.KeyID]
			if ps == nil {
				ps = statsByName["tg-"+fmtInt(k.ChatID)+"-order-"+k.OrderID]
			}
			if ps != nil {
				stat = poolStatToLive(ps)
			}
		}
		// SKU tag: llmgate rows are always bedrock (the only class the cutover
		// moved). For pool rows the STORED class wins; a legacy '' row falls
		// back to the LIVE stats row's class, and only then to the bedrock
		// default — so an operator who classed a legacy key kiro/fixed
		// server-side still sees it labelled (and unit-labelled) correctly here
		// (telegram_bot.py:4340-4342). DISPLAY ONLY: the recharge guard must
		// keep reading the raw stored value.
		rowCls := k.SaleClass
		if k.Backend == store.BackendLlmgate {
			rowCls = saleClassBedrock
		} else if rowCls == "" && stat != nil {
			rowCls = stat.SaleClass
		}
		// Collapse anything unrecognised onto bedrock, exactly as
		// apikeyKeyClass does — an unknown class would otherwise index SkuTag
		// with a missing key and render an EMPTY tag.
		if rowCls != saleClassKiro && rowCls != saleClassFixed {
			rowCls = saleClassBedrock
		}
		tag := c.SkuTag[rowCls].T(lang)
		// A REFUNDED key is never re-served: show it struck from the list (no
		// live stats, no reveal button) so the buyer sees why it disappeared.
		// DEAD keys are OMITTED entirely (operator 2026-10-10): EXPIRED
		// (past the 30-day window, unrechargeable) and EXHAUSTED (balance
		// spent) alike — a dead key on this screen is noise and only invites
		// "why is this here" questions. Only a POSITIVE live verdict hides
		// the row; unreadable stats keep it visible (fail-open display, same
		// reasoning as the statsFailed note). An exhausted key stays
		// rechargeable through the BUY menu's Recharge flow regardless —
		// this is display-only. Refunded-and-dead hides too.
		if stat != nil && (stat.Expired || stat.Status == "exhausted") {
			continue
		}
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
			// Unlimited renders ∞ (llmgate credit_limit 0, pool -1 — the same
			// keyLiveStats.Unlimited flag either way). Remaining/Used are
			// fractional live numbers on BOTH backends, so round to the nearest
			// whole credit for display via roundCredits rather than truncating
			// with int64(...) (would show 1999 instead of 2000 for a 1999.95
			// balance) or printing the float directly (would show
			// "1999.950000"). A negative remaining on a LIMITED key keeps the
			// pre-cutover pool rendering (∞) — the status line already says
			// exhausted, and this branch is defensive for sold keys.
			left := "∞"
			if !stat.Unlimited && stat.Remaining >= 0 {
				left = itoa64(roundCredits(stat.Remaining))
			}
			detail := i18n.F(c.MyKeysLineStats.T(lang),
				"used", itoa64(roundCredits(stat.Used)), "bought", itoa64(bought),
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

// poolStatToLive maps one pool /admin/stats row into the backend-agnostic
// keyLiveStats shape — the My-Keys view's half of the union apikeyLiveStats
// performs for single-key reads, kept as its own mapper because this call
// site starts from an ALREADY-FETCHED batched row and must not pay another
// round trip per key.
func poolStatToLive(st *kirogo.KeyStat) *keyLiveStats {
	if st == nil {
		return nil
	}
	return &keyLiveStats{
		Status:    st.Status,
		Remaining: float64(st.CreditsRemaining),
		Used:      st.CreditsUsed,
		Unlimited: st.CreditsRemaining.IsUnlimited() || st.CreditsRemaining < 0,
		SaleClass: st.SaleClass,
	}
}

// llmgateKeyStatsMaxConcurrent bounds the parallel per-key llmgate stats fetch
// on the My-Keys view. llmgate has no batch endpoint, so a 10-key page is 10
// KeyStats reads; 4 at a time keeps the view's latency near 3 sequential
// round trips instead of 10, without opening 10 simultaneous connections to
// the API on every Self-Service tap.
const llmgateKeyStatsMaxConcurrent = 4

// llmgateKeyStatsParallel fetches live stats for every LLMGATE-backend row of
// the My-Keys page, in parallel (bounded by llmgateKeyStatsMaxConcurrent)
// under ONE overall statsTimeout budget, keyed by order id. Refunded rows are
// skipped (they render from local state) and every failure is silent — the
// caller degrades that key to its local row plus the statsFailed note, the
// same contract a failed batched pool call has. Returns an empty map (never
// nil) when there is nothing to fetch or llmgate is unconfigured, so callers
// can index it unconditionally.
func (b *Bot) llmgateKeyStatsParallel(ctx context.Context, keys []*store.APIKey) map[string]*keyLiveStats {
	out := map[string]*keyLiveStats{}
	if b.llmgateClient() == nil {
		return out
	}
	var lg []*store.APIKey
	for _, k := range keys {
		if k.Backend == store.BackendLlmgate && k.RefundedTS == nil {
			lg = append(lg, k)
		}
	}
	if len(lg) == 0 {
		return out
	}
	// ONE parent deadline for the whole page: each per-key read applies its own
	// statsTimeout too, but as a CHILD of this context the effective deadline
	// is the tighter of the two, so a slow tail key can never stretch the view
	// past the budget a single-key read would have had.
	pctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	sem := make(chan struct{}, llmgateKeyStatsMaxConcurrent)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, k := range lg {
		wg.Add(1)
		go func(row *store.APIKey) {
			defer wg.Done()
			// Acquire a slot or die with the page budget — a queued 5th key
			// must not hold the worker past the deadline waiting for its turn.
			select {
			case sem <- struct{}{}:
			case <-pctx.Done():
				return
			}
			defer func() { <-sem }()
			st := b.apikeyLiveStats(pctx, row)
			if st == nil {
				return // silent: caller marks this key statsFailed
			}
			mu.Lock()
			out[row.OrderID] = st
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	return out
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
	// SKU-correct wording/units: a kiro/fixed key re-reveals with its own
	// delivery message, everything else (bedrock + legacy '') the bedrock one;
	// an llmgate-backend row gets the llmgate wording whatever its class.
	// Shared with the first delivery via apikeyDeliveredTemplate so the
	// two renders of one key can never disagree.
	keyCls := apikeyKeyClass(row)
	tmpl := apikeyDeliveredTemplate(c, keyCls, row.Backend)
	// "name" (admin-renameable display name) must be passed here too, not only
	// on the first delivery: this re-send uses the SAME template, so omitting
	// it would leak a literal "{name}" into the customer's message. HTML-escaped
	// because this goes out with parse_mode=HTML. "apibase"/"pubbase" fill only
	// the llmgate template (silent no-ops elsewhere, same as {opus} on prompts);
	// {pubbase} is the shop's web-pages host the guidance links point at.
	b.sendHTML(cc.Ctx, cc.ChatID, i18n.F(tmpl.T(lang),
		"name", html.EscapeString(b.apikeyProductName(keyCls)),
		"credits", itoa64(roundCredits(row.Credits)),
		"key", html.EscapeString(row.APIKey),
		"base", html.EscapeString(b.kiroGoPublicURL()),
		"apibase", b.llmgateAPIBaseDisp(),
		"pubbase", b.llmgatePublicURLDisp()))
}
