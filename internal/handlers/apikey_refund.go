package handlers

// apikey_refund.go — M2.5 slice, part 4: the GROUP refund of an API key.
//
// Ports _apikey_order_root (telegram_bot.py:4531), _apikey_refund_group
// (telegram_bot.py:4542), _apikey_refund_amount (telegram_bot.py:4570),
// _do_refund_apikey_group (telegram_bot.py:4764) and the customer-facing
// self-service preview/confirm (_apikey_refund_preview @16625 /
// _apikey_refund_confirm @16678, callbacks apikeyrefok: / apikeyrefno @12371).
//
// WHY A GROUP: after a top-up the key's remaining credits can't be attributed
// to individual orders, so an API key refunds as ONE unit — the original key
// order PLUS every APPLIED recharge. One payout, one revoke.
//
// The maths is by CREDITS remaining (live from Kiro-Go), never by warranty
// time: API-key sales are "full warranty" (always refund-eligible, any age),
// the customer just doesn't get money back for credits already burned.

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// refundableStatuses is REFUNDABLE_STATUSES (telegram_bot.py:3649): only money
// that is confirmed in (paid) or already fulfilled (delivered) can be returned.
var refundableStatuses = []string{"delivered", "paid"}

func isRefundableStatus(s string) bool {
	for _, x := range refundableStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// apikeyRefundMember is one order of the refund group, normalized across the
// two row shapes the group is built from (the root *store.Order and the joined
// store.RechargeOrder rows).
type apikeyRefundMember struct {
	Des    string
	Status string
	Total  int64
	Qty    int64 // CREDITS — every api-key-family order stores credits in qty
}

// apikeyOrderRoot returns the des of the ORIGINAL key order behind any
// api-key-family order — _apikey_order_root (telegram_bot.py:4531-4539): the
// order itself for a key sale, the bound target for a recharge (via the
// apikey_recharges binding written at order creation). "" when a recharge order
// somehow has no binding (nothing to resolve against).
func (b *Bot) apikeyOrderRoot(o *store.Order) string {
	if o == nil {
		return ""
	}
	if o.Product == apikeyRechargeProduct {
		rec, err := b.store.APIKeyRechargeForOrder(o.Des)
		if err != nil || rec == nil {
			return ""
		}
		return rec.TargetOrderID
	}
	return o.Des
}

// apikeyRefundGroup resolves the WHOLE-KEY refund group — _apikey_refund_group
// (telegram_bot.py:4542-4567). Members are the still-refundable (paid/delivered)
// orders of the group, root first then recharges oldest-first;
// already-refunded members are excluded (their money was paid back before), and
// so are UN-APPLIED recharges (their credits never reached the key, so they are
// not part of its quota and must not join the payout).
func (b *Bot) apikeyRefundGroup(o *store.Order) (rootDes string, members []apikeyRefundMember) {
	rootDes = b.apikeyOrderRoot(o)
	if rootDes == "" {
		return "", nil
	}
	// The caller already holds the full root row when it IS the root — reuse it
	// (saves a read and keeps callers passing a not-yet-committed row working).
	root := o
	if rootDes != o.Des {
		r, err := b.store.GetOrder(rootDes)
		if err != nil {
			log.Println("apikey refund group: get_order failed:", err)
			return rootDes, nil
		}
		root = r
	}
	if root != nil && isRefundableStatus(root.Status) {
		members = append(members, apikeyRefundMember{
			Des: root.Des, Status: root.Status, Total: root.Total, Qty: root.Qty})
	}
	ros, err := b.store.RechargeOrdersForTarget(rootDes)
	if err != nil {
		log.Println("recharge_orders_for_target failed:", err)
		return rootDes, members
	}
	for _, r := range ros {
		if isRefundableStatus(r.Status) {
			members = append(members, apikeyRefundMember{
				Des: r.Des, Status: r.Status, Total: r.Total, Qty: r.Qty})
		}
	}
	return rootDes, members
}

// apikeyStuckRecharge reports whether the acted-on order is a PAID-but-never-
// APPLIED recharge sitting OUTSIDE the group (a stuck settle: Kiro-Go was down
// between payment and fulfillment). Its credits never reached the key, so it is
// not a group member — but its money is real, and the refund button is the
// manual remedy: it refunds at FULL face value on top of the group proration
// (telegram_bot.py:4791-4803, mirrored in the preview at 4607-4612).
func (b *Bot) apikeyStuckRecharge(o *store.Order, members []apikeyRefundMember) bool {
	if o == nil || o.Product != apikeyRechargeProduct || !isRefundableStatus(o.Status) {
		return false
	}
	for _, m := range members {
		if m.Des == o.Des {
			return false // it IS a group member
		}
	}
	rec, err := b.store.APIKeyRechargeForOrder(o.Des)
	if err != nil {
		return false
	}
	return rec == nil || rec.AppliedTS == nil
}

// apikeyRefundAmount is the refund maths for an API-key-family order —
// _apikey_refund_amount (telegram_bot.py:4570-4613). Returns
// (amount, unitsTotal, unitNum) shaped like the generic _refund_amount so the
// preview/receipt render unchanged: unitsTotal = group credits BOUGHT,
// unitNum = credits USED, so the shared "(N−used)/N × total" line is exactly
// the maths applied.
//
// Degrades to (0, 0, 0) — which routes the admin to the deliberate force-full
// prompt and the customer to the "try again" notice — when nothing was minted
// (pre-mint refund) or live stats are unavailable: NEVER guess a payout from
// stale data.
func (b *Bot) apikeyRefundAmount(ctx context.Context, o *store.Order) (amount, unitsTotal, unitNum int64) {
	rootDes, members := b.apikeyRefundGroup(o)
	var row *store.APIKey
	if rootDes != "" {
		row, _ = b.store.APIKeyForOrder(rootDes)
	}
	if row == nil || len(members) == 0 {
		return 0, 0, 0
	}
	// qty holds CREDITS for every api-key-family order, so the group's credit
	// base is Σ qty and its payout base Σ total over the refundable members.
	var bought float64
	var groupPaid int64
	for _, m := range members {
		bought += float64(m.Qty)
		groupPaid += m.Total
	}
	remaining, ok := b.apikeyRemainingCredits(ctx, row)
	if bought <= 0 || !ok {
		return 0, 0, 0
	}
	// creditsRemaining -1 means unlimited (shouldn't happen for sold keys);
	// clamp into [0, bought] so the payout can never exceed the paid total.
	if remaining >= 0 {
		remaining = math.Max(0, math.Min(remaining, bought))
	} else {
		remaining = bought
	}
	used := bought - remaining
	// RoundToEven-to-nearest-unit is correct TODAY because groupPaid is VND.
	// After the ledger migration (task #11) groupPaid becomes micro-USDT and
	// orders.total/refunded_amount are bucketCentRounded — rounding to the
	// nearest MICRO here would land off a whole-cent (10,000 micro) boundary,
	// silently producing a value the migration's own whole-cent invariant
	// (cmd/migrate-usdt/migrate.go's checkWholeCentInvariant) exists to
	// reject. Post-cutover this must round to the nearest 10,000 micro, not
	// the nearest micro.
	amount = min(int64(math.RoundToEven(float64(groupPaid)*remaining/bought)), groupPaid)
	// A stuck never-applied recharge refunds its FULL face value on top of the
	// group proration, so the preview must quote the same figure (preview ==
	// payout).
	if b.apikeyStuckRecharge(o, members) {
		amount += o.Total
	}
	return amount, int64(math.Round(bought)), int64(math.Round(used))
}

// apikeyRefundResult is the _do_refund-shaped outcome of a group refund
// (telegram_bot.py:4906-4911).
type apikeyRefundResult struct {
	Amount       int64
	UnitsTotal   int64 // group credits bought
	UnitNum      int64 // group credits used
	Balance      int64 // wallet balance after a "balance" payout
	Des          string
	Method       string
	Notified     bool
	GroupN       int   // how many orders were refunded together
	PaidTotal    int64 // Σ actually paid across everything refunded here
	CreditFailed bool  // CreditBalance errored AFTER the claims already committed
}

// doRefundApikeyGroup refunds an API key as ONE unit — _do_refund_apikey_group
// (telegram_bot.py:4764-4911): claim the whole group (original order + every
// applied recharge), pay out once, revoke the key once. Returns nil when
// nothing was claimed.
//
// CONCURRENCY: each member order flips via its own atomic ClaimOrder, and the
// payout is computed from exactly the claims THIS call won — so two racing
// refunds (even entered from different member orders) can never pay for the
// same order twice; the loser of every claim gets nil back.
//
// ABORT CONDITIONS (pro-rated path only, BEFORE any claim so an abort has zero
// side effects): unreadable live credit stats, or a group with no refundable
// members and no stuck order. forceFull needs no stats — it returns the group's
// entire paid total.
func (b *Bot) doRefundApikeyGroup(ctx context.Context, o *store.Order, method string, forceFull bool) *apikeyRefundResult {
	des := o.Des
	rootDes, members := b.apikeyRefundGroup(o)
	var krow *store.APIKey
	if rootDes != "" {
		krow, _ = b.store.APIKeyForOrder(rootDes)
	}
	// Credit base + live remaining, resolved BEFORE any claim so an unreadable
	// pro-ration aborts untouched. (After the claims below the key gets revoked —
	// stats must be read while it still exists.)
	var bought float64
	for _, m := range members {
		bought += float64(m.Qty)
	}
	remaining, haveRemaining := 0.0, false
	if krow != nil {
		remaining, haveRemaining = b.apikeyRemainingCredits(ctx, krow)
	}
	stuck := b.apikeyStuckRecharge(o, members)

	if !forceFull {
		if len(members) > 0 && (bought <= 0 || !haveRemaining) {
			// No safe pro-rated amount for the group. The previews block this path
			// up front; reaching it means stats died in between — abort untouched
			// so the caller can retry / go force-full.
			return nil
		}
		if len(members) == 0 && !stuck {
			return nil // nothing refundable at all
		}
		if len(members) > 0 {
			if remaining >= 0 {
				remaining = math.Max(0, math.Min(remaining, bought))
			} else {
				remaining = bought
			}
		}
	}
	// Claim EVERY refundable member atomically; the set WE won is exactly the
	// set we pay for (a racing refund's winners are excluded here, so money for
	// any single order is only ever returned once).
	var won []apikeyRefundMember
	for _, m := range members {
		// customer | admin_cancel (stats-fixes-spec.md §2.1): each group member is
		// pre-gated to {delivered, paid} by apikeyRefundGroup (isRefundableStatus),
		// so refundKindForStatus's status branch always applies. This is a REAL
		// customer refund either way — the API-key rail never reaches
		// adminorders.go:doRefund's own claim, doRefund branches into this path
		// first for any isApikeyFamily product.
		w, err := b.store.ClaimOrderKind(m.Des, m.Status, "refunded", refundKindForStatus(m.Status))
		if err != nil {
			log.Printf("apikey group refund: claim failed [des=%s]: %v", m.Des, err)
			continue
		}
		if w != nil {
			won = append(won, apikeyRefundMember{Des: w.Des, Status: "refunded",
				Total: w.Total, Qty: w.Qty})
			// Any frozen warranty clock is meaningless for credit proration —
			// drop it so a later unrelated refund never reuses a stale
			// snapshot (telegram_bot.py:4825).
			b.dropRefundAsOf(w.Des)
		}
	}
	// The stuck never-applied order refunds at face value via its own atomic
	// claim (its credits never reached the key, so it sits outside the maths).
	var extra int64
	var stuckDes string
	if stuck {
		// customer | admin_cancel (stats-fixes-spec.md §2.1): apikeyStuckRecharge
		// only returns true when isRefundableStatus(o.Status) already holds, so
		// the same {delivered, paid} branch applies as the group loop above.
		if w, err := b.store.ClaimOrderKind(o.Des, o.Status, "refunded", refundKindForStatus(o.Status)); err == nil && w != nil {
			extra = w.Total
			stuckDes = w.Des
			b.dropRefundAsOf(w.Des) // telegram_bot.py:4833
		}
	}
	if len(won) == 0 && extra == 0 {
		return nil
	}
	var groupPaid int64
	for _, w := range won {
		groupPaid += w.Total
	}
	var amount, used int64
	switch {
	case forceFull:
		amount = groupPaid + extra
		if bought > 0 {
			used = int64(math.Round(bought - math.Min(remaining, bought)))
		}
	case len(won) > 0:
		// Per-credit rate from the FULL group, applied to the totals actually won.
		//
		// RoundToEven-to-nearest-unit is correct TODAY because groupPaid is
		// VND. After the ledger migration (task #11) groupPaid becomes
		// micro-USDT and orders.total/refunded_amount are bucketCentRounded —
		// rounding to the nearest MICRO here would land off a whole-cent
		// (10,000 micro) boundary, silently producing a value the migration's
		// own whole-cent invariant (cmd/migrate-usdt/migrate.go's
		// checkWholeCentInvariant) exists to reject. Post-cutover this must
		// round to the nearest 10,000 micro, not the nearest micro.
		amount = min(int64(math.RoundToEven(float64(groupPaid)*remaining/bought)), groupPaid) + extra
		used = int64(math.Round(bought - remaining))
	default:
		// Only the stuck order was claimable (e.g. the rest of the group was
		// already refunded earlier): pay its face value, nothing to pro-rate.
		amount, used, bought = extra, 0, 0
	}
	// The portion of `amount` attributable to the pro-rated `won` members,
	// as opposed to the stuck order's own face-value `extra`. This is also
	// correct for forceFull (amountGroup == groupPaid, so splitRefundedAmount
	// hands each member back its own Total) and for the stuck-only default
	// case (amountGroup == 0, nothing to split).
	amountGroup := amount - extra
	// REVOKE the sold key on Kiro-Go so the refunded credits can't be spent, and
	// stamp the row so the reveal button / My-Keys view stop re-serving it.
	// Best-effort (the refund has already committed); on failure alert the OWNER
	// to delete the key manually — until then the credits stay spendable.
	//
	// Gated on `won`: a stuck-only refund left the group's delivered orders (and
	// thus the live key) untouched — revoking it would kill a key the customer
	// still rightfully holds.
	if krow != nil && len(won) > 0 {
		if err := b.store.MarkAPIKeyRefunded(rootDes); err != nil {
			log.Println("mark_api_key_refunded failed:", err)
		}
		ok, detail := b.revokeAPIKey(ctx, krow)
		if !ok {
			// _notify_owners — telegram_bot.py:4888-4902.
			log.Printf("OWNER ALERT: REFUNDED API-KEY ORDER %s — REVOKE FAILED (%s) 🔑 %s · id %s · name tg-%d-order-%s — DELETE this key on Kiro-Go now; its remaining credits stay usable until you do",
				rootDes, detail, maskAPIKey(krow.APIKey), orDash(krow.KeyID), krow.ChatID, rootDes)
			b.notifyOwners(ctx, fmt.Sprintf(
				"⚠️ REFUNDED API-KEY ORDER %s — REVOKE FAILED (%s)\n🔑 %s · id %s\n🏷 name tg-%d-order-%s\n"+
					"DELETE this key on Kiro-Go now — its remaining credits stay usable until you do.",
				rootDes, detail, maskAPIKey(krow.APIKey), orDash(krow.KeyID), krow.ChatID, rootDes))
		} else {
			log.Printf("apikey group refund: revoked key for order %s (%s)", rootDes, detail)
		}
	}

	res := &apikeyRefundResult{
		Amount: amount, UnitsTotal: int64(math.Round(bought)), UnitNum: used,
		Des: des, Method: method, GroupN: len(won), PaidTotal: groupPaid + extra,
	}
	if extra != 0 {
		res.GroupN++
	}
	switch method {
	case "balance":
		newBal, err := b.store.CreditBalance(o.ChatID, amount)
		if err != nil {
			// The claims already committed. Money must never be silently dropped:
			// page loudly so an operator credits it by hand (the orders are
			// 'refunded', so no automatic path will retry). CreditFailed skips
			// the customer DM below — telling them "refunded" would be a lie.
			log.Printf("CRITICAL apikey group refund credit failed [des=%s chat=%s amount=%d]: %v — orders ARE 'refunded'; credit the wallet by hand",
				des, o.ChatID, amount, err)
			res.CreditFailed = true
			// Owner alert (#39): every order in the group is 'refunded' with
			// NOTHING credited, and no automatic path will ever retry a
			// 'refunded' order — the log line above was the only trace
			// before this.
			b.notifyOwners(ctx, fmt.Sprintf(
				"🚨 REFUND CREDIT FAILED — MANUAL FIX REQUIRED\n🧾 %s\n👤 %s\n💰 %s\nOrders ARE 'refunded'; credit the wallet by hand.",
				des, o.ChatID, b.usdtDisp(amount)))
			// The claims already committed but nothing was credited: record 0
			// (not NULL — 0 is literally true) for every order this call
			// claimed, group and stuck alike. This write must never fail the
			// refund it records (see SetRefundedAmount's doc comment), so
			// log-and-continue only.
			for _, w := range won {
				if serr := b.store.SetRefundedAmount(w.Des, 0); serr != nil {
					log.Printf("SetRefundedAmount(%s, 0) failed: %v", w.Des, serr)
				}
			}
			if stuckDes != "" {
				if serr := b.store.SetRefundedAmount(stuckDes, 0); serr != nil {
					log.Printf("SetRefundedAmount(%s, 0) failed: %v", stuckDes, serr)
				}
			}
		} else {
			res.Balance = newBal
			b.recordGroupRefundedAmounts(won, groupPaid, amountGroup, stuckDes, extra)
		}
	case "manual":
		// No CreditBalance call for a manual payout — the admin pays out
		// off-platform. Cash still left the business, so for a revenue
		// report the honest value is the computed split, though it is
		// unverifiable from the wallet ledger (no balance transaction backs
		// it).
		b.recordGroupRefundedAmounts(won, groupPaid, amountGroup, stuckDes, extra)
	default:
		// An unrecognised payout method has no branch above that knows how
		// to credit it or record amounts — every claim in this group already
		// committed (each order IS 'refunded'), so refunded_amount is left
		// exactly as it was for EVERY member of the group: the root order,
		// every applied recharge, and the stuck order if any — not just the
		// one order this call was invoked from. NULL is a load-bearing
		// state meaning "genuinely refunded, actual amount unrecoverable"
		// (SetRefundedAmount's doc comment); falling through silently here
		// would forge that state for a group that in fact has a computable
		// amount. Must be caught loudly, matching the CreditFailed alert
		// above (the only "should not happen on a money path" pattern this
		// file already has).
		members := make([]string, 0, len(won)+1)
		for _, w := range won {
			members = append(members, w.Des)
		}
		if stuckDes != "" {
			members = append(members, stuckDes)
		}
		log.Printf("CRITICAL apikey group refund: unrecognised payout method %q [des=%s chat=%s amount=%d members=%v] — orders ARE 'refunded'; refunded_amount NOT recorded for any of them",
			method, des, o.ChatID, amount, members)
		b.notifyOwners(ctx, fmt.Sprintf(
			"🚨 REFUND UNKNOWN PAYOUT METHOD — MANUAL FIX REQUIRED\n🧾 %s\n👤 %s\n💰 %s\nmethod=%s\nmembers=%v\n"+
				"Orders ARE 'refunded'; refunded_amount was NOT recorded for any of them — investigate and record by hand.",
			des, o.ChatID, b.usdtDisp(amount), method, members))
	}
	// Notify the customer (best-effort) — the same receipt the generic refund
	// sends, anchored on the order they acted from. Skipped entirely when the
	// credit failed: the money never moved, so a "refunded" DM would be false
	// reassurance (the caller's own receipt/log carries the loud failure).
	if res.CreditFailed {
		return res
	}
	chatID, _ := strconv.ParseInt(o.ChatID, 10, 64)
	blang := b.langFor(chatID, "vi")
	date := "—"
	if o.TS != 0 {
		date = fmtWhen(o.TS, blang)
	}
	msg := i18n.F(i18n.C().APIKey.RefundCustomer.T(blang),
		"des", des, "tier", b.tierLabel(o.Tier, blang), "date", date,
		"amount", b.usdtDisp(amount), "balance", b.usdtDisp(res.Balance))
	// Whole-key semantics made explicit when top-ups were involved: the payout
	// covered the original order plus its recharges, and the key was revoked.
	if res.GroupN > 1 {
		msg += "\n" + i18n.F(i18n.C().APIKey.RefundGroupNote.T(blang), "n", itoa(res.GroupN-1))
	}
	res.Notified = b.sendOK(ctx, chatID, msg)
	return res
}

// sendOK sends a plain message and reports whether Telegram accepted it — the
// `notified` flag on a refund receipt (telegram_bot.py:4902-4905).
func (b *Bot) sendOK(ctx context.Context, chatID int64, text string) bool {
	if _, err := b.tg.SendMessage(ctx, tg.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		log.Println("refund customer notify failed:", err)
		return false
	}
	return true
}

// splitRefundedAmount divides amountGroup (the slice of the group payout
// attributable to `won`, i.e. amount minus the stuck order's own extra)
// across won's members proportional to each member's paid Total — the exact
// per-order share of a pro-rated group refund (task #20, stage S2). Plain
// rounding of each share independently can drift from amountGroup by a unit
// or two, so the residual is handed entirely to the member with the largest
// paid Total (NOT the textbook largest-remainder method, which ranks
// candidates by the FRACTIONAL PART of their share — this dumps the whole
// leftover on the biggest payer instead): this keeps SUM(shares) ==
// amountGroup exactly, which is the property that makes refunded_amount
// trustworthy for a group refund — the stats query will one day just SUM()
// this column.
func splitRefundedAmount(won []apikeyRefundMember, groupPaid, amountGroup int64) map[string]int64 {
	out := make(map[string]int64, len(won))
	if len(won) == 0 || groupPaid <= 0 {
		return out
	}
	var assigned int64
	largestIdx, largestTotal := 0, int64(-1)
	for i, m := range won {
		share := int64(math.Round(float64(amountGroup) * float64(m.Total) / float64(groupPaid)))
		out[m.Des] = share
		assigned += share
		if m.Total > largestTotal {
			largestTotal, largestIdx = m.Total, i
		}
	}
	// An individual member's share can go NEGATIVE here. Worked example: 4
	// members each with Total=250 (groupPaid=1000), amountGroup=2 — every
	// member's independently-rounded share is math.Round(2*250/1000) =
	// math.Round(0.5) = 1 (Go's math.Round rounds half away from zero), so
	// assigned=4 and the residual folded onto the largest member is
	// amountGroup-assigned = 2-4 = -2, landing that member's final share at
	// 1-2 = -1. SUM(shares) stays EXACTLY amountGroup regardless — that
	// invariant is what recordGroupRefundedAmounts/SetRefundedAmount rely
	// on, and it still holds here. This is unreachable in the admin UI
	// TODAY only because the revenue screen renders the group aggregate
	// (admin.go's Refunded block sums refunded_amount, never displays one
	// member's row) — a negative per-member share never surfaces. If
	// per-order or per-member refunded_amount rendering is ever added, the
	// displayed residual must be clamped (e.g. to 0) before it is shown,
	// since a negative "amount refunded" on a single order reads as a bug
	// even though the group total is correct.
	out[won[largestIdx].Des] += amountGroup - assigned
	return out
}

// recordGroupRefundedAmounts writes refunded_amount for every order this
// call paid out FOR REAL: the pro-rata split across won (via
// splitRefundedAmount) plus the stuck order's own face value (extra).
// Callers must only reach this after money has genuinely moved — a
// successful CreditBalance, or a manual payout where cash left the business
// off-platform — never on a CreditFailed path (that path writes 0 directly,
// bypassing the split entirely). Same rule as SetRefundedAmount itself: this
// bookkeeping write must never fail the refund it records, so every error is
// logged and swallowed here too.
func (b *Bot) recordGroupRefundedAmounts(won []apikeyRefundMember, groupPaid, amountGroup int64, stuckDes string, extra int64) {
	shares := splitRefundedAmount(won, groupPaid, amountGroup)
	for _, m := range won {
		share := shares[m.Des]
		if err := b.store.SetRefundedAmount(m.Des, share); err != nil {
			log.Printf("SetRefundedAmount(%s, %d) failed: %v", m.Des, share, err)
		}
	}
	if stuckDes != "" {
		if err := b.store.SetRefundedAmount(stuckDes, extra); err != nil {
			log.Printf("SetRefundedAmount(%s, %d) failed: %v", stuckDes, extra, err)
		}
	}
}

// ---- customer self-service preview / confirm --------------------------------

// apikeyRefundPreview shows the self-service API-key refund preview with
// Confirm/Cancel — _apikey_refund_preview (telegram_bot.py:16625-16675).
//
// API-key-family orders are SELF-SERVICE: they skip the admin queue entirely.
// That is safe to automate because the payout is pro-rated by credits REMAINING
// (a heavily used key refunds little or nothing) and the key is revoked, so
// there is no abuse edge.
//
// Two terminal states with NO Confirm button:
//   - live credits unreadable (bought == 0 sentinel) -> UNAVAIL notice,
//   - key fully used (amount <= 0 but units known)   -> NONE notice.
//
// The Confirm button carries only the order id — the payout is RECOMPUTED at
// confirm time, never trusted from the button.
func (b *Bot) apikeyRefundPreview(ctx context.Context, chatID int64, lang i18n.Lang, o *store.Order) {
	c := &i18n.C().APIKey
	amount, bought, used := b.apikeyRefundAmount(ctx, o)
	remaining := max(int64(0), bought-used)
	if bought <= 0 {
		b.send(ctx, chatID, c.RefundUnavail.T(lang), nil)
		return
	}
	if amount <= 0 {
		b.send(ctx, chatID, i18n.F(c.RefundNone.T(lang),
			"used", itoa64(used), "bought", itoa64(bought)), nil)
		return
	}
	text := i18n.F(c.RefundPreview.T(lang),
		"des", o.Des, "bought", itoa64(bought), "used", itoa64(used),
		"remaining", itoa64(remaining), "amount", b.usdtDisp(amount))
	// Whole-key semantics made explicit BEFORE the customer confirms: with
	// applied top-ups, the original order + every recharge refund together and
	// the key is revoked — never a per-order partial. A stuck never-applied
	// recharge refunds alongside too, so it counts toward the tally.
	_, members := b.apikeyRefundGroup(o)
	nGrp := len(members)
	if b.apikeyStuckRecharge(o, members) {
		nGrp++
	}
	if nGrp > 1 {
		text += "\n\n" + i18n.F(c.RefundGroupNote.T(lang), "n", itoa(nGrp-1))
	}
	b.send(ctx, chatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		btnRow(c.RefundOKBtn.T(lang), cbData("apikeyrefok", o.Des)),
		btnRow(c.RefundNoBtn.T(lang), "apikeyrefno"),
	}})
}

// cbApikeyRefOK handles apikeyrefok:<des> — the customer confirmed the preview
// (telegram_bot.py:16678-16711, callback @12371).
//
// Re-validates ownership + refundable status (a stale button can't refund
// someone else's order or double-refund), then runs the group executor, which
// atomically claims delivered/paid->refunded (so a double-tap's loser is a
// no-op), recomputes the amount from the rows, credits the wallet, revokes the
// key on Kiro-Go and DMs the customer. Fully automatic per policy: no admin is
// in the approval loop; admins are notified after the fact, via the
// notifyAdmins call a few lines below — not deferred to a future milestone,
// already landed right here.
func (b *Bot) cbApikeyRefOK(cc *cbCtx) {
	c := &i18n.C().APIKey
	lang := b.langFor(cc.ChatID, "en")
	des := strings.TrimPrefix(cc.Data, "apikeyrefok:")
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil || o.ChatID != fmtInt(cc.ChatID) ||
		!isApikeyFamily(o.Product) || !isRefundableStatus(o.Status) {
		b.send(cc.Ctx, cc.ChatID, c.RefundRace.T(lang), nil)
		return
	}
	res := b.doRefundApikeyGroup(cc.Ctx, o, "balance", false)
	if res == nil {
		b.send(cc.Ctx, cc.ChatID, c.RefundRace.T(lang), nil)
		return
	}
	// _notify_admins — telegram_bot.py:17068-17075.
	log.Printf("API-KEY REFUND (self-service) [des=%s chat=%d amount=%d used=%d/%d]",
		des, cc.ChatID, res.Amount, res.UnitNum, res.UnitsTotal)
	b.notifyAdmins(cc.Ctx, fmt.Sprintf(
		"↩️ API-KEY REFUND (self-service)\n🧾 %s\n👤 %d\n💰 refunded %s to wallet\n🔑 %d of %d credits used",
		des, cc.ChatID, b.usdtDisp(res.Amount), res.UnitNum, res.UnitsTotal))
}

// cbApikeyRefNo handles apikeyrefno — the customer tapped Cancel on the preview
// (telegram_bot.py:12375-12379). Nothing moves.
func (b *Bot) cbApikeyRefNo(cc *cbCtx) {
	b.send(cc.Ctx, cc.ChatID, i18n.C().APIKey.RefundCancelled.T(b.langFor(cc.ChatID, "en")), nil)
}
