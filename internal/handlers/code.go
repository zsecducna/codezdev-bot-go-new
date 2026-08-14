package handlers

// code.go — M2.6 promo/referral code engine.
//
// Ports:
//   - _coded_total            telegram_bot.py:10107-10135 → codedTotalFull
//   - _apikey_coded_total     telegram_bot.py:6237-6269   → (b *Bot).apikeyCodedTotalFull
//   - _release_order_code     telegram_bot.py:10138-10151 → (b *Bot).releaseOrderCode
//   - _apply_code_to_order    telegram_bot.py:10249-10336 → (b *Bot).applyCodeToOrder
//   - _pay_referrer           telegram_bot.py:7438-7455   → (b *Bot).payReferrer
//   - pending_ordercode       telegram_bot.py:19382-19391 → capOrderCode (#47)
//   - registerCodeCapture     wires capOrderCode into capture chain

import (
	"context"
	"log"
	"math"
	"strconv"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
)

// referralPctConst is REFERRAL_PCT (telegram_bot.py:1574): the percentage of
// an order's total paid both to the buyer as a discount and to the referrer as
// a reward. Kept as a named const so changing the single Python value flows to
// all three call sites (discount, reward, UI display).
//
// NOTE: info.go already has `const referralPct = 10` for the /info display.
// That const is in the same package, so we reference it directly here instead
// of declaring a duplicate. Both must stay == 10 (REFERRAL_PCT @1574).

// ---- codedTotalFull ----------------------------------------------------------

// codedTotalFull is the FULL _coded_total (telegram_bot.py:10107-10135) with
// kind/percent/stack support. The code-free helper codedTotal in catalog.go
// now delegates here; all callers remain consistent.
//
// Return values:
//
//	total    — the charge in VND after discount
//	effPct   — effective discount % (what the customer sees)
//	volPct   — the baseline volume-tier % before any code (useful for stack notes)
//
// Rules (telegram_bot.py:10107-10135):
//
//	kind=""          baseline: reseller flat rate replaces volume tier when
//	                 resellerPct>0; otherwise volume tier applies.
//	kind="referral"  referralPct, REPLACES volume discount.
//	kind="promo"     stack=false replaces volume with percent;
//	                 stack=true adds percent ON TOP of volume (capped).
//	Any code (kind set) bypasses resellerPct — code wins.
//	Effective fraction clamped to [0, 0.95] — no code can zero an order.
func codedTotalFull(unit, qty int, kind string, percent float64, stack bool, resellerPct int) (total, effPct, volPct int) {
	base := max(unit*qty, 0)
	vol := discountFor(qty)
	var eff float64
	switch kind {
	case "referral":
		// REFERRAL_PCT / 100.0 — REPLACES volume discount (telegram_bot.py:10115).
		eff = float64(referralPct) / 100.0
	case "promo":
		p := percent / 100.0
		if stack {
			// stack=true: ADD percent on top of the volume tier (telegram_bot.py:10119).
			eff = vol + p
		} else {
			// stack=false: REPLACE the volume tier with percent (telegram_bot.py:10121).
			eff = p
		}
	default:
		// Baseline: reseller flat rate replaces the volume tier; a zero resellerPct
		// (not a reseller, or feature off) falls back to the normal volume tier so
		// resellers never LOSE the volume discount when the rate is unset
		// (telegram_bot.py:10123-10129).
		if resellerPct != 0 {
			eff = float64(resellerPct) / 100.0
		} else {
			eff = vol
		}
	}
	// Clamp effective fraction to [0, 0.95] (telegram_bot.py:10131).
	eff = math.Max(0.0, math.Min(0.95, eff))
	total = int(math.Round(float64(base) * (1 - eff)))
	return total, int(math.Round(eff * 100)), int(math.Round(vol * 100))
}

// ---- apikeyCodedTotalFull ----------------------------------------------------

// apikeyCodedTotalFull is the FULL _apikey_coded_total
// (telegram_bot.py:6237-6269) with kind/percent/stack support. Extends the
// code-free apikeyCodedTotal that was sufficient for M2.2-M2.5.
//
// Deliberate differences from codedTotalFull, both forced by the product
// (telegram_bot.py:6237-6247):
//   - base is apikeyPriceMicro — a virtual product has no folder price file;
//   - there is no volume tier (qty is CREDITS, not units), so volumePct is
//     always 0 and stack is a no-op: a stacking promo adds its percent to a
//     0% volume discount, which is just the promo percent.
func (b *Bot) apikeyCodedTotalFull(credits int64, kind string, percent float64, stack bool, resellerPct int, saleClass string) (total int64, effPct, volPct int) {
	base := b.apikeyPriceMicro(credits, saleClass)
	if base <= 0 {
		return base, 0, 0
	}
	var eff float64
	switch kind {
	case "referral":
		// REFERRAL_PCT / 100.0 (telegram_bot.py:6253).
		eff = float64(referralPct) / 100.0
	case "promo":
		// No volume tier, so stack is a no-op — just the promo percent
		// (telegram_bot.py:6255-6256).
		eff = percent / 100.0
	default:
		// Baseline: reseller flat rate (telegram_bot.py:6258-6261).
		if resellerPct != 0 {
			eff = float64(resellerPct) / 100.0
		}
	}
	// Clamp [0, 0.95] (telegram_bot.py:6262).
	eff = math.Max(0.0, math.Min(0.95, eff))
	return int64(math.Round(float64(base) * (1 - eff))), int(math.Round(eff * 100)), 0
}

// ---- releaseOrderCode --------------------------------------------------------

// releaseOrderCode returns any reserved hold from the code currently on order o
// — _release_order_code (telegram_bot.py:10138-10151).
//
// A promo gives back its reserved use; a referral deletes the redeemer's
// one-time reservation scoped to THIS order so we never wipe a redemption
// another order owns. No-op when the order has no code.
//
// Called on: code-replace, code-remove (cbRemoveCode), expiry, cancel, and
// non-delivery (apikey mint-fail / recharge-fail) paths.
func (b *Bot) releaseOrderCode(o *store.Order) {
	if o == nil {
		return
	}
	kind := ""
	if o.CodeKind != nil {
		kind = *o.CodeKind
	}
	code := ""
	if o.Code != nil {
		code = *o.Code
	}
	switch kind {
	case "promo":
		// Give back one reserved use (telegram_bot.py:10146).
		if code != "" && b.store != nil {
			if err := b.store.PromoReleaseUse(code); err != nil {
				log.Printf("releaseOrderCode: PromoReleaseUse(%s) [des=%s]: %v", code, o.Des, err)
			}
		}
	case "referral":
		// Scope release to THIS order so we never wipe a different order's
		// redemption (telegram_bot.py:10148-10150).
		if b.store != nil {
			des := o.Des
			if err := b.store.ReferralRedeemRelease(o.ChatID, &des); err != nil {
				log.Printf("releaseOrderCode: ReferralRedeemRelease [des=%s chat=%s]: %v", o.Des, o.ChatID, err)
			}
		}
	}
}

// ---- applyCodeToOrder --------------------------------------------------------

// applyCodeToOrder validates `code` and (re)applies it to reserved order o,
// then re-renders the pay screen — _apply_code_to_order
// (telegram_bot.py:10249-10336).
//
// One code per order: any previously applied code is released and the order
// is reset to the plain volume/reseller-baseline price BEFORE evaluating the
// new one, so a REJECTED code leaves the order on its baseline price (never
// on the old code's price).
//
// Ownership guard is enforced by the caller (capOrderCode checks status and
// compares ChatID) before calling here; a second check is inside as
// defence-in-depth (telegram_bot.py:10255-10257).
func (b *Bot) applyCodeToOrder(ctx *Ctx, o *store.Order, code string) {
	c := &i18n.C().Customer
	lang := ctx.Lang
	des := o.Des
	chatID := ctx.ChatID

	// Defence-in-depth ownership guard (telegram_bot.py:10255-10257): never
	// mutate another chat's order even if called directly.
	if o.ChatID != fmtInt(chatID) {
		return
	}

	// Determine whether this is an API-key-family order so we route all
	// pricing through the correct twin (telegram_bot.py:10261-10266).
	isAPIKey := isAPIKeyProduct(o.Product)
	// Resolve the sale class once: codes re-price off the SAME base rate the
	// order was created with (kiro vs bedrock), so applying/removing a code
	// never silently re-quotes a kiro order at bedrock prices
	// (telegram_bot.py:10263-10266).
	akClass := ""
	if isAPIKey {
		akClass = b.orderSaleClass(o)
	}

	// Helper: (total, effPct, volPct) using the right pricing twin.
	totalFor := func(kind string, percent float64, stack bool, resellerPct int) (int64, int, int) {
		if isAPIKey {
			t, e, v := b.apikeyCodedTotalFull(o.Qty, kind, percent, stack, resellerPct, akClass)
			return t, e, v
		}
		unit := 0
		if b.stock != nil {
			u, _ := b.stock.ProductPrice(o.Product, o.Tier)
			unit = u
		}
		t, e, v := codedTotalFull(unit, int(o.Qty), kind, percent, stack, resellerPct)
		return int64(t), e, v
	}

	// 1. Drop the previous code's hold and reset to the baseline price.
	// A rejected/unknown code then leaves the order on this baseline
	// (telegram_bot.py:10280-10284).
	b.releaseOrderCode(o)
	resellerPct := b.resellerPctFor(chatID)
	volTotal, _, _ := totalFor("", 0, false, resellerPct)
	if err := b.store.UpdateOrderCode(des, nil, nil, nil, volTotal); err != nil {
		log.Printf("applyCodeToOrder: reset UpdateOrderCode [des=%s]: %v", des, err)
	}

	// 2. Try admin promo (telegram_bot.py:10286-10316).
	promo, err := b.store.GetPromo(code)
	if err != nil {
		log.Printf("applyCodeToOrder: GetPromo [des=%s code=%s]: %v", des, code, err)
	}
	if promo != nil {
		if !promo.Active {
			// Promo disabled — show CODE_USED_UP (telegram_bot.py:10290-10292).
			b.sendPayScreen(ctx.Ctx, chatID, des,
				i18n.F(c.CodeUsedUp.T(lang), "code", code))
			return
		}
		reserved, err := b.store.PromoReserveUse(code)
		if err != nil {
			log.Printf("applyCodeToOrder: PromoReserveUse [des=%s code=%s]: %v", des, code, err)
		}
		if reserved == nil {
			// Exhausted — also CODE_USED_UP (telegram_bot.py:10294-10296).
			b.sendPayScreen(ctx.Ctx, chatID, des,
				i18n.F(c.CodeUsedUp.T(lang), "code", code))
			return
		}
		stackPromo := reserved.Stack
		total, effPct, volPct := totalFor("promo", reserved.Percent, stackPromo, 0)
		kind := "promo"
		if err := b.store.UpdateOrderCode(des, &code, &kind, nil, total); err != nil {
			// If the persist fails, give the slot back to avoid a phantom reservation.
			log.Printf("applyCodeToOrder: promo UpdateOrderCode [des=%s]: %v", des, err)
			_ = b.store.PromoReleaseUse(code)
			return
		}
		// API-key orders have no volume tier — a stacking promo's "stack" note
		// would be nonsense (telegram_bot.py:10298-10307).
		pctStr := itoa(int(math.Round(reserved.Percent)))
		var note string
		if stackPromo && !isAPIKey {
			note = i18n.F(c.PromoAppliedStack.T(lang),
				"code", code,
				"pct", pctStr,
				"vol", itoa(volPct),
				"eff", itoa(effPct))
		} else {
			note = i18n.F(c.PromoAppliedReplace.T(lang),
				"code", code,
				"pct", pctStr)
		}
		b.sendPayScreen(ctx.Ctx, chatID, des, note)
		return
	}

	// 3. Try referral code (only when the referral program is on; once per
	// customer for life; not your own code) (telegram_bot.py:10318-10334).
	var owner string
	if b.referralEnabled() {
		owner, err = b.store.ReferralOwner(code)
		if err != nil {
			log.Printf("applyCodeToOrder: ReferralOwner [des=%s code=%s]: %v", des, code, err)
		}
	}
	if owner != "" {
		if owner == fmtInt(chatID) {
			// Can't use your own referral code (telegram_bot.py:10322-10324).
			b.sendPayScreen(ctx.Ctx, chatID, des, c.RefOwnCode.T(lang))
			return
		}
		// Fast pre-check (telegram_bot.py:10325-10327): has_redeemed_referral is
		// not the real gate but avoids the INSERT race on the common path.
		redeemed, err := b.store.HasRedeemedReferral(fmtInt(chatID))
		if err != nil {
			log.Printf("applyCodeToOrder: HasRedeemedReferral [des=%s]: %v", des, err)
		}
		if redeemed {
			b.sendPayScreen(ctx.Ctx, chatID, des, c.RefAlready.T(lang))
			return
		}
		// Real atomic gate: referral_redeem_reserve fails when a concurrent apply
		// already took the slot (telegram_bot.py:10328-10330).
		won, err := b.store.ReferralRedeemReserve(fmtInt(chatID), code, owner, des)
		if err != nil {
			log.Printf("applyCodeToOrder: ReferralRedeemReserve [des=%s]: %v", des, err)
		}
		if !won {
			b.sendPayScreen(ctx.Ctx, chatID, des, c.RefAlready.T(lang))
			return
		}
		total, _, _ := totalFor("referral", 0, false, 0)
		kind := "referral"
		if err := b.store.UpdateOrderCode(des, &code, &kind, &owner, total); err != nil {
			log.Printf("applyCodeToOrder: referral UpdateOrderCode [des=%s]: %v", des, err)
			// Persist failed — roll back the reservation so the customer can retry.
			_ = b.store.ReferralRedeemRelease(fmtInt(chatID), &des)
			return
		}
		b.sendPayScreen(ctx.Ctx, chatID, des,
			i18n.F(c.RefApplied.T(lang), "code", code, "pct", itoa(referralPct)))
		return
	}

	// 4. Unknown code — order is already on baseline price from step 1
	// (telegram_bot.py:10335-10336).
	b.sendPayScreen(ctx.Ctx, chatID, des,
		i18n.F(c.CodeInvalid.T(lang), "code", code))
}

// ---- payReferrer -------------------------------------------------------------

// payReferrer credits the referrer REFERRAL_PCT% of a delivered order's total
// and DMs them — _pay_referrer (telegram_bot.py:7438-7455). Called once, only
// after an order with a referral code reaches 'delivered'. The
// referral_redemptions row is kept (never released) so the redeeming
// customer's one-time use stays consumed for life.
//
// No-op when the order has no referrer_chat or the reward rounds to zero.
func (b *Bot) payReferrer(ctx context.Context, o *store.Order) {
	if o == nil || o.ReferrerChat == nil || *o.ReferrerChat == "" {
		return
	}
	ref := *o.ReferrerChat
	if b.store == nil {
		return
	}
	reward := int64(math.Round(float64(o.Total) * float64(referralPct) / 100.0))
	if reward <= 0 {
		return
	}
	newBal, err := b.store.CreditBalance(ref, reward)
	if err != nil {
		log.Printf("payReferrer: CreditBalance [des=%s ref=%s reward=%d]: %v",
			o.Des, ref, reward, err)
		return
	}
	// DM the referrer in their language (telegram_bot.py:7449-7454). Python's
	// lang_for falls back to "vi" for a non-numeric id; here a bad id means we
	// cannot address the chat at all, so log and stop after the credit (the
	// money is already theirs — only the notification is lost).
	refChatID, perr := strconv.ParseInt(ref, 10, 64)
	if perr != nil {
		log.Printf("payReferrer: credited %d but referrer_chat %q is not a chat id [des=%s]",
			reward, ref, o.Des)
		return
	}
	rlang := b.langFor(refChatID, "vi")
	c := &i18n.C().Customer
	code := ""
	if o.Code != nil {
		code = *o.Code
	}
	b.send(ctx, refChatID,
		i18n.F(c.ReferrerEarned.T(rlang),
			"code", code,
			"amount", b.usdtDisp(reward),
			"balance", b.usdtDisp(newBal)),
		nil)
}

// ---- capOrderCode ------------------------------------------------------------

// capOrderCode is capture-chain entry #47, pending_ordercode — the promo /
// referral code free-text capture (telegram_bot.py:19382-19391).
//
// PEEK semantics — handlers-go.md §2.2 lists #47 pending_ordercode as `peek`,
// and the Python source confirms it. The `.pop` on @19382 is the FIRST STATEMENT
// OF THE `if` BODY, not part of the condition:
//
//	if chat_id in pending_ordercode and not text.startswith("/") \
//	        and label_to_cmd(text) is None:      # @19381 — guard
//	    des = pending_ordercode.pop(chat_id)     # @19382 — body
//
// So an escaping text (a "/" command or a keyboard label) fails the GUARD, the
// body never runs, and the slot stays ARMED — the customer can tap /help, read
// it, and then type their code with the prompt still live. Only a plain,
// non-escaping text reaches the pop and consumes the slot.
//
// (Contrast a genuine pop-capture such as pending_entra @18013, where the
// `.pop(chat_id, None)` is evaluated as part of the branch condition itself and
// an escape therefore kills the flow.)
//
// Uppercase the raw text before passing to applyCodeToOrder (telegram_bot.py:19388).
func (b *Bot) capOrderCode(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.OrderCode
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	// Peek escape (telegram_bot.py:19381 guard): slot stays armed, router keeps
	// going so the "/command" or tapped label is handled by its own stage.
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if b.store == nil {
		return true
	}
	// Atomically claim the slot (telegram_bot.py:19382 `.pop`) — a single lock
	// hold over the check-and-clear so a double-submit from two goroutines can
	// only apply the code once (same pattern as capAPIKey/capPurchase).
	c.st.mu.Lock()
	claimed := c.st.OrderCode
	c.st.OrderCode = nil
	c.st.mu.Unlock()
	if claimed == nil {
		return true // lost the race — the other goroutine owns this code entry
	}

	// Status gate: only a reserved order can have a code applied
	// (telegram_bot.py:19384-19387).
	o, err := b.store.GetOrder(claimed.Des)
	if err != nil || o == nil || o.Status != "reserved" {
		b.send(c.Ctx, c.ChatID, i18n.C().Buy.OrderGone.T(c.Lang), nil)
		return true
	}
	// Ownership guard (telegram_bot.py:10255-10257 inside applyCodeToOrder;
	// also enforced here before the call).
	if o.ChatID != fmtInt(c.ChatID) {
		b.send(c.Ctx, c.ChatID, i18n.C().Buy.OrderGone.T(c.Lang), nil)
		return true
	}
	// Uppercase the typed code (telegram_bot.py:19388).
	code := strings.ToUpper(strings.TrimSpace(c.Text))
	b.applyCodeToOrder(c, o, code)
	return true
}

// ---- registerCodeCapture -----------------------------------------------------

// registerCodeCapture wires capOrderCode into the capture chain at position #47
// — between apikeyrc (#46) and purchase (#48) — exactly matching the Python
// priority comment @3632 and the canonical order in captures.go.
//
// Uses insertCapturesBefore("purchase") so the splice keeps the chain an
// in-order subsequence of CanonicalCaptureOrder (handlers-go.md §7).
func (b *Bot) registerCodeCapture() {
	b.insertCapturesBefore("purchase",
		stage{name: "ordercode", fn: (*Bot).capOrderCode}, // #47 @19382 peek
	)
}
