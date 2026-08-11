package handlers

// adminorders.go — 🧾 ORDERS category + the refund subsystem (py_orders_spec.md
// is the exact behavioural/verbatim-string reference for every callback below,
// keyed off telegram_bot.py). Every callback here used to fall through
// cbAdmAction's default branch (or cbAdmTodo, for admrefdel/refrev:/refrej:) to
// the generic "not wired up yet" notice — this file replaces each stub with
// real behaviour.
//
// MONEY-CRITICAL FILE (task brief): a defect here pays a customer twice or not
// at all. THE single invariant every path below is built around is
// store.ClaimOrderKind(des, from, "refunded", kind) — ONE conditional UPDATE,
// proceed only on RowsAffected()==1. store.CreditBalance is plain additive and NOT
// idempotent by itself; it is safe only because ClaimOrder's atomic status
// flip is what actually prevents a double-pay. Every refund path (admin
// adm:refund, force-full, request-approve, API-key group) goes through
// doRefund, which enforces this ordering: compute the payout BEFORE claiming,
// claim, THEN credit — never the other way around.
//
// Permission model (py_orders_spec.md's own permission section): adm:orders /
// admcat:orders / admcat:refundreq / refrev: / refrej: are grantable via the
// orders_view capability (admincap.go's stdAdminCbCap + catCaps — already
// classified there, no change needed in this slice). adm:getord and adm:refund
// (and every sub-callback they emit) are absent from that table entirely, so
// the CENTRAL chokepoint (command.go's handleCallback, admincap.go) denies a
// standard admin before this file's handlers ever run — matching Python, each
// handler below still re-checks only plain isAdmin (not isSuperAdmin) inline,
// belt-and-braces, exactly as py_orders_spec.md states for this family.
// refappr: / admrefdel / refdelconf / refdelone: / refdeloneok: are the
// exception: Python gates THOSE with an explicit, LOUD is_super_admin check
// and its own denial text (no capability unlocks them, ever) — this file does
// the same rather than relying on silence.
import (
	"context"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/entra"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/refund"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// ============================================================================
// Shared refund-maths chokepoint (non-API-key orders)
// ============================================================================

// productSuspendedAt returns the epoch-seconds timestamp a product was
// suspended at, and whether one is recorded — settings.json "suspended" holds
// {product: {ts, by}} (catalog.go's productSuspended checks only presence,
// which is all its one caller today needs). The refund-maths warranty clock
// below needs the TIMESTAMP: a product pulled from sale mid-warranty must stop
// accruing "elapsed" the moment it was suspended, not whenever an admin
// happens to look. A present-but-malformed entry (no numeric ts) returns
// ok=false rather than guessing 0, which would zero out every warranty
// retroactively — the same fail-safe posture selfservice.go's
// fileRefundRequest already documents for its own (currently no-op) suspend
// check.
func (b *Bot) productSuspendedAt(product string) (ts float64, ok bool) {
	susp, _ := b.settings()["suspended"].(map[string]any)
	v, present := susp[product]
	if !present {
		return 0, false
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return 0, false
	}
	return settingsNumber(m, "ts")
}

// refundAsOf peeks the FROZEN warranty-clock snapshot for order des, if a
// refappr: approval froze one — the Go home of Python's module-level
// _refund_asof_by_des dict (dispatch.go's refundAsOfByDes/refundAsOfMu
// fields). Non-destructive: both the preview (refundMethodPrompt,
// cbRefundReview) and the executor (doRefund) read it; only a WINNING claim
// consumes it (dropRefundAsOf), never a peek.
func (b *Bot) refundAsOf(des string) (float64, bool) {
	b.refundAsOfMu.Lock()
	defer b.refundAsOfMu.Unlock()
	v, ok := b.refundAsOfByDes[des]
	return v, ok
}

// freezeRefundAsOf snapshots the as-of clock for des — called once, by
// cbRefundApprove, at the moment a customer refund REQUEST is approved, so
// review delay before the admin actually picks a payout method can never
// shrink what was approved.
func (b *Bot) freezeRefundAsOf(des string, ts float64) {
	b.refundAsOfMu.Lock()
	b.refundAsOfByDes[des] = ts
	b.refundAsOfMu.Unlock()
}

// dropRefundAsOf single-use-consumes a frozen snapshot — called ONLY after a
// WINNING ClaimOrder inside doRefund (py_orders_spec.md: "On win: drops any
// frozen snapshot"). A LOST claim must leave it in place, so a retry (or the
// next tap) still sees the frozen value.
func (b *Bot) dropRefundAsOf(des string) {
	b.refundAsOfMu.Lock()
	delete(b.refundAsOfByDes, des)
	b.refundAsOfMu.Unlock()
}

// refundAmount is the shared warranty-TIME proration for a non-API-key order
// — the day/hour-tier branch of _refund_amount (py_orders_spec.md). Day tiers
// (BH<N>D) prorate whole days, purchase day counted as day 1; hour tiers
// (BH<N>H) prorate whole hours the same way. The elapsed clock STOPS at
// min(now, the product's suspend timestamp if any, asOf if non-nil) — a
// suspended product's warranty stops ticking the moment it was pulled from
// sale, and a frozen customer-request approval can never have its payout
// shrunk by admin-review delay.
//
// unitsTotal<=0 (KBH / unknown tier code) or a non-positive amount (fully
// elapsed warranty) both mean "nothing to prorate" — the caller must escalate
// to the force-full flow, exactly like Python's automatic
// _force_refund_prompt fallback; this function itself never sends anything.
func (b *Bot) refundAmount(o *store.Order, asOf *float64) (amount, unitsTotal, unitNum int64, unit string) {
	code := strings.ToUpper(o.Tier)
	var unitSeconds float64
	switch {
	case tierDayRe.MatchString(code):
		n, _ := strconv.Atoi(tierDayRe.FindStringSubmatch(code)[1])
		unitsTotal, unit, unitSeconds = int64(n), "Day", 86400
	case tierHourRe.MatchString(code):
		n, _ := strconv.Atoi(tierHourRe.FindStringSubmatch(code)[1])
		unitsTotal, unit, unitSeconds = int64(n), "Hour", 3600
	default:
		return 0, 0, 0, "" // KBH / unknown — no warranty was ever sold
	}
	if unitsTotal <= 0 {
		return 0, 0, 0, ""
	}

	clockEnd := float64(b.now().Unix())
	// A suspend ts of exactly 0 is a falsy/absent value in Python (`if
	// susp.get("ts")` @4491, `if as_of` @4476) — honouring it here would
	// collapse clockEnd to the Unix epoch, OVERPAYING every refund (elapsed
	// floors to 0, unitNum floors to 1, remaining floors to N-1). Require a
	// POSITIVE timestamp before either value is allowed to cap the clock.
	if ts, ok := b.productSuspendedAt(o.Product); ok && ts > 0 && ts < clockEnd {
		clockEnd = ts
	}
	if asOf != nil && *asOf > 0 && *asOf < clockEnd {
		clockEnd = *asOf
	}
	elapsed := clockEnd - o.TS
	if elapsed < 0 {
		elapsed = 0
	}
	unitNum = int64(math.Floor(elapsed/unitSeconds)) + 1
	remaining := unitsTotal - unitNum
	if remaining < 0 {
		remaining = 0
	}
	amount = int64(math.RoundToEven(float64(o.Total) * float64(remaining) / float64(unitsTotal)))
	if amount < 0 {
		amount = 0
	}
	if amount > o.Total {
		amount = o.Total
	}
	return amount, unitsTotal, unitNum, unit
}

// ============================================================================
// doRefund — THE atomic refund executor
// ============================================================================

// refundResult mirrors _do_refund's return shape for the receipt every admin
// confirmation and customer DM reads. Unit is "Day"/"Hour" (non-API-key) or
// "Credit" (API-key family, reusing doRefundApikeyGroup's own maths).
type refundResult struct {
	Amount       int64
	UnitsTotal   int64
	UnitNum      int64
	Unit         string
	Balance      int64
	Method       string
	Notified     bool
	PaidTotal    int64
	CreditFailed bool // CreditBalance errored AFTER the claim already committed — see doRefund
}

// doRefund is THE atomic refund executor shared by every entry point: admin
// adm:refund, force-refund, and a customer request's refappr: approval —
// _do_refund (py_orders_spec.md). API-key-family orders delegate entirely to
// doRefundApikeyGroup (apikey_refund.go), which already owns the multi-order
// group claim, the live-credits maths and the Kiro-Go revoke; everything else
// runs the single-order path below.
//
// Returns nil when the order is not currently refundable (deposit / wrong
// status) OR when the atomic claim was LOST (status changed under us) — the
// caller must not act as if anything moved in either case.
// refundKindForStatus classifies a genuine admin-issued refund by the
// order's status AT THE MOMENT OF THE CLAIM (stats-fixes-spec.md §2.1): a
// 'delivered' order means goods already went out and money came back (a
// completed-sale refund); a 'paid' order means money was taken and cancelled
// before delivery (no goods ever shipped). Both count toward the
// customer-facing refund rate (§2.2) — they are kept distinct because "how
// often do we refund after delivering" and "how often do we cancel before
// delivering" are different shop-health signals. Callers must pass the
// PRE-transition status (the same value used as ClaimOrderKind's fromStatus);
// isRefundableStatus/refundableStatuses already gate the input to
// {delivered, paid}, so any other value is a caller bug — default to
// admin_cancel rather than mint an invalid kind.
func refundKindForStatus(status string) string {
	if status == "delivered" {
		return store.RefundKindCustomer
	}
	return store.RefundKindAdminCancel
}

func (b *Bot) doRefund(ctx context.Context, o *store.Order, method string, forceFull bool) *refundResult {
	if !isRefundableStatus(o.Status) || o.Product == depositProduct {
		return nil
	}
	if isApikeyFamily(o.Product) {
		res := b.doRefundApikeyGroup(ctx, o, method, forceFull)
		if res == nil {
			return nil
		}
		return &refundResult{
			Amount: res.Amount, UnitsTotal: res.UnitsTotal, UnitNum: res.UnitNum,
			Unit: "Credit", Balance: res.Balance, Method: res.Method,
			Notified: res.Notified, PaidTotal: res.PaidTotal,
			CreditFailed: res.CreditFailed,
		}
	}

	// Compute the payout BEFORE claiming (py_orders_spec.md step order) —
	// _refund_amount is read-only, so computing it ahead of the claim can
	// never itself cause a double-pay; only the claim below decides who wins.
	var asOf *float64
	if v, ok := b.refundAsOf(o.Des); ok {
		asOf = &v
	}
	amount, unitsTotal, unitNum, unit := b.refundAmount(o, asOf)
	if forceFull {
		amount = o.Total // unit/unitNum/unitsTotal kept for context; unused in the force-full receipt
	}

	// THE double-refund guard: one conditional UPDATE, proceed only on
	// RowsAffected()==1. A nil return means another path already flipped
	// this order's status — we must not act as if we won.
	won, err := b.store.ClaimOrderKind(o.Des, o.Status, "refunded", refundKindForStatus(o.Status))
	if err != nil {
		log.Println("adminorders: ClaimOrder failed:", err)
		return nil
	}
	if won == nil {
		return nil
	}
	// Single-use: only the WINNING claim spends the frozen snapshot (if any).
	b.dropRefundAsOf(o.Des)

	res := &refundResult{Amount: amount, UnitsTotal: unitsTotal, UnitNum: unitNum, Unit: unit,
		Method: method, PaidTotal: won.Total}
	var balanceStr string
	switch method {
	case "balance":
		newBal, err := b.store.CreditBalance(won.ChatID, amount)
		if err != nil {
			// The claim already committed — the order IS 'refunded'. Money must
			// never be silently dropped: page loudly so an operator credits it
			// by hand (no automatic path will retry a 'refunded' order). Set
			// CreditFailed so sendRefundReceipt renders a loud failure notice
			// (never a quiet ✅ "refunded, balance now 0") and so the customer
			// DM below is skipped entirely — telling either party this
			// "worked" would be a lie.
			log.Printf("CRITICAL refund credit failed [des=%s chat=%s amount=%d]: %v — order IS 'refunded'; credit the wallet by hand",
				o.Des, won.ChatID, amount, err)
			res.CreditFailed = true
			// Owner alert (#39): the order is 'refunded' with NOTHING
			// credited, and no automatic path will ever retry a 'refunded'
			// order — the log line above was the only trace before this.
			b.notifyOwners(ctx, "🚨 REFUND CREDIT FAILED — MANUAL FIX REQUIRED\n"+
				"🧾 "+o.Des+"\n👤 "+won.ChatID+"\n💰 "+b.usdtDisp(amount)+"\n"+
				"Order IS 'refunded'; credit the wallet by hand.")
			// The claim already committed but nothing was credited: record 0,
			// not NULL — 0 is literally true (nothing reached the customer).
			// This write must never fail the refund it records (see
			// SetRefundedAmount's doc comment), so log-and-continue only.
			// Residual: if an operator hand-credits later per the alert
			// above, nothing updates this column — that order under-reports
			// forever.
			if serr := b.store.SetRefundedAmount(won.Des, 0); serr != nil {
				log.Printf("SetRefundedAmount(%s, 0) failed: %v", won.Des, serr)
			}
		} else {
			res.Balance = newBal
			balanceStr = b.usdtDisp(newBal)
			if serr := b.store.SetRefundedAmount(won.Des, amount); serr != nil {
				log.Printf("SetRefundedAmount(%s, %d) failed: %v", won.Des, amount, serr)
			}
		}
	case "manual":
		// No CreditBalance call for a manual payout — the admin pays out
		// off-platform. Cash still left the business, so for a revenue
		// report the honest value is the computed amount, though it is
		// unverifiable from the wallet ledger (no balance transaction backs
		// it).
		if serr := b.store.SetRefundedAmount(won.Des, amount); serr != nil {
			log.Printf("SetRefundedAmount(%s, %d) failed: %v", won.Des, amount, serr)
		}
	}

	// Skip the customer DM entirely when the credit failed: the money never
	// moved, so a "refunded" notification would be false reassurance.
	if !res.CreditFailed {
		if chatID, perr := strconv.ParseInt(won.ChatID, 10, 64); perr == nil {
			lang := b.langFor(chatID, "vi")
			date := "—"
			if won.TS != 0 {
				date = fmtWhen(won.TS, lang)
			}
			var msg string
			if method == "manual" {
				msg = i18n.F(i18n.C().Orders.RefundCustomerManual.T(lang),
					"des", won.Des, "tier", b.tierLabel(won.Tier, lang), "date", date, "amount", b.usdtDisp(amount))
			} else {
				msg = i18n.F(i18n.C().APIKey.RefundCustomer.T(lang),
					"des", won.Des, "tier", b.tierLabel(won.Tier, lang), "date", date,
					"amount", b.usdtDisp(amount), "balance", balanceStr)
			}
			res.Notified = b.sendOK(ctx, chatID, msg)
		}
	}
	return res
}

// ============================================================================
// adm:getord — order lookup by ID (read-only)
// ============================================================================

// cbGetOrd arms the order-lookup prompt — adm:getord (py_orders_spec.md).
// Local check mirrors Python: plain isAdmin — the CENTRAL admincap.go
// chokepoint (command.go) is what actually makes this full-admin-only for a
// standard admin, since "adm:getord" is absent from stdAdminCbCap's table.
func (b *Bot) cbGetOrd(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.mu.Lock()
	cc.st.GetOrd = true
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "🔎 Send the FULL order ID to look up:", backKB("admcat:orders"))
}

// capGetOrd is the "getord" capture (#4, PEEK semantics —
// py_orders_spec.md/telegram_bot.py:18164-18165: the pending flag survives a
// reply-keyboard label or slash command UNTOUCHED, so an accidental nav tap
// can't destroy the armed state; it clears ONLY once the text has passed
// that guard). An invalid order ID past the guard still ABORTS the flow
// (the admin must re-tap 🔎 Look up order by ID to retry) — only the
// "is this candidate input at all" check is peek-then-clear, not the
// "is this a valid order ID" check.
func (b *Bot) capGetOrd(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.GetOrd
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	c.st.mu.Lock()
	c.st.GetOrd = false
	c.st.mu.Unlock()
	if !b.isAdmin(c.ChatID) {
		return true
	}
	if b.store == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:orders"))
		return true
	}
	des := strings.TrimSpace(c.Text)
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil {
		b.send(c.Ctx, c.ChatID, "❌ No order found with ID:\n"+des, admNav("admcat:orders"))
		return true
	}
	b.sendOrderDetail(c.Ctx, c.ChatID, o)
	return true
}

// sendOrderDetail renders the order-detail screen plus the delivery artifact
// — the found branch of adm:getord's text-capture (py_orders_spec.md). The
// 🗑️ Delete Account button is prepended ONLY when refunded, not yet Entra-
// deleted, and the viewer is a super admin (shared with the admrefdel flow's
// refdelone:). Read-only: no writes happen here.
func (b *Bot) sendOrderDetail(ctx context.Context, chatID int64, o *store.Order) {
	buyer := o.ChatID
	if cid, err := strconv.ParseInt(o.ChatID, 10, 64); err == nil {
		if nm := b.nameFor(cid); nm != "" {
			buyer = nm + " (" + o.ChatID + ")"
		}
	}
	created := time.Unix(int64(o.TS), 0).Format("2006-01-02 15:04:05")
	text := "🧾 Order " + o.Des + "\n" +
		"Status: " + o.Status + "\n" +
		"Product: " + o.Product + "\n" +
		"Warranty: " + b.tierLabel(o.Tier, i18n.EN) + " (" + o.Tier + ")\n" +
		"Qty: " + itoa64(o.Qty) + "\n" +
		"Total: " + b.usdtDisp(o.Total) + "\n" +
		"Buyer: " + buyer + "\n" +
		"Created: " + created
	var rows [][]tg.InlineKeyboardButton
	if o.Status == "refunded" && o.EntraDeletedTS == nil && b.isSuperAdmin(chatID) {
		rows = append(rows, btnRow("🗑️ Delete Account", cbData("refdelone", o.Des)))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:orders"))
	b.sendMenu(ctx, chatID, text, rows)

	// Delivery artifact: the order's .zip (JSON-account orders) if present,
	// else every OrderFiles path — orderRedlPaths (history.go) already owns
	// this exact selection for the customer /redl: path; reused verbatim so
	// the two views can never disagree about what was actually delivered.
	paths := b.orderRedlPaths(o.Des)
	if len(paths) == 0 {
		b.send(ctx, chatID, "ℹ️ No delivery file (order not delivered yet).", admNav("admcat:orders"))
		return
	}
	for _, fp := range paths {
		if _, err := b.tg.SendDocument(ctx, chatID, fp, ""); err != nil {
			log.Println("adminorders: SendDocument failed:", err)
		}
	}
}

// ============================================================================
// adm:refund — admin-initiated refund entry point
// ============================================================================

// cbRefund arms the refund-by-ID prompt — adm:refund (py_orders_spec.md).
// Local check is plain isAdmin, matching Python (the CENTRAL chokepoint is
// what actually makes this full-admin-only, same reasoning as cbGetOrd).
func (b *Bot) cbRefund(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.mu.Lock()
	cc.st.Refund = true
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "💸 Send the FULL order ID to refund:", backKB("admcat:orders"))
}

// capRefund is the "refund" capture (#33, PEEK semantics —
// py_orders_spec.md/telegram_bot.py:18955-18956: the pending flag survives a
// reply-keyboard label or slash command UNTOUCHED; it clears ONLY once the
// text has passed that guard). An invalid order ID past the guard still
// ABORTS rather than re-arming. A not-found order gets a retry button
// (unlike getord's plain Back) so the admin can immediately try another ID.
func (b *Bot) capRefund(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Refund
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	c.st.mu.Lock()
	c.st.Refund = false
	c.st.mu.Unlock()
	if !b.isAdmin(c.ChatID) {
		return true
	}
	if b.store == nil {
		b.send(c.Ctx, c.ChatID, settingsNotConfigured, admNav("admcat:orders"))
		return true
	}
	des := strings.TrimSpace(c.Text)
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil {
		b.send(c.Ctx, c.ChatID, "❌ No order found with ID:\n"+des,
			admNavRetry("admcat:orders", "💸 Refund order", "adm:refund"))
		return true
	}
	b.refundMethodPrompt(c.Ctx, c.ChatID, o)
	return true
}

// refundGroupTotal returns (groupN, paid) for an API-key-family order: the
// number of orders that refund together (root + applied recharges + a stuck
// never-applied recharge, if any) and their combined paid total — the shared
// computation refundMethodPrompt/forceRefundPrompt/cbForceRefundConfirm all
// need for their "whole-key group" line / full-total button.
func (b *Bot) refundGroupTotal(o *store.Order) (groupN int, paid int64) {
	_, members := b.apikeyRefundGroup(o)
	groupN = len(members)
	for _, m := range members {
		paid += m.Total
	}
	if b.apikeyStuckRecharge(o, members) {
		groupN++
		paid += o.Total
	}
	return groupN, paid
}

// refundMethodPrompt is _refund_method_prompt — the shared pro-rated refund
// preview used by BOTH adm:refund's admin-initiated flow and refappr:'s
// approve-then-payout flow (py_orders_spec.md). Three validations, each a
// terminal reply on failure: already refunded, not in a refundable status, or
// a deposit (deposits are never refundable — CLAUDE.md guard lives here, not
// only in the button set). A non-positive prorated amount (KBH/expired
// warranty, or unreadable API-key credit stats) is NOT a dead end — it
// escalates straight to the force-full prompt.
func (b *Bot) refundMethodPrompt(ctx context.Context, chatID int64, o *store.Order) {
	if o.Status == "refunded" {
		b.send(ctx, chatID, "⛔ Order already refunded:\n"+o.Des, admNav("admcat:orders"))
		return
	}
	if !isRefundableStatus(o.Status) {
		b.send(ctx, chatID, "⛔ Order is '"+o.Status+"', not refundable (only paid/delivered):\n"+o.Des, admNav("admcat:orders"))
		return
	}
	if o.Product == depositProduct {
		b.send(ctx, chatID, "⛔ This is a deposit/top-up, not a purchase — not refundable here:\n"+o.Des, admNav("admcat:orders"))
		return
	}

	var amount, unitsTotal, unitNum int64
	var unit string
	var paid int64
	var groupN int
	if isApikeyFamily(o.Product) {
		amount, unitsTotal, unitNum = b.apikeyRefundAmount(ctx, o)
		unit = "Credit"
		groupN, paid = b.refundGroupTotal(o)
	} else {
		var asOf *float64
		if v, ok := b.refundAsOf(o.Des); ok {
			asOf = &v
		}
		amount, unitsTotal, unitNum, unit = b.refundAmount(o, asOf)
		paid = o.Total
	}

	if unitsTotal <= 0 || amount <= 0 {
		b.forceRefundPrompt(ctx, chatID, o)
		return
	}

	date := fmtWhen(o.TS, i18n.EN)
	var groupLine string
	if groupN > 1 {
		groupLine = "🔁 whole-key group: " + itoa(groupN) + " orders refund together\n"
	}
	text := "💸 Refund — order " + o.Des + "\n" +
		"🛡️ " + b.tierLabel(o.Tier, i18n.EN) + "  📅 " + date + "\n" +
		groupLine +
		unit + " " + itoa64(unitNum) + " of " + itoa64(unitsTotal) + " (paid " + b.usdtDisp(paid) + ")\n" +
		"Formula: (" + itoa64(unitsTotal) + "−" + itoa64(unitNum) + ")/" + itoa64(unitsTotal) + " × " + b.usdtDisp(paid) + "\n" +
		"➡️ Refund: " + b.usdtDisp(amount) + "\n\n" +
		"Choose refund method:"
	rows := [][]tg.InlineKeyboardButton{
		btnRow("💰 Credit balance ("+b.usdtDisp(amount)+")", cbData("refundm", o.Des, "balance")),
		btnRow("🏦 QR / manual transfer ("+b.usdtDisp(amount)+")", cbData("refundm", o.Des, "manual")),
		btnRow("⚠️ Force FULL refund ("+b.usdtDisp(paid)+")", cbData("frefund", o.Des)),
		btnRow("❌ Cancel", "refundno"),
	}
	b.sendMenu(ctx, chatID, text, rows)
}

// refundFailureText re-runs refundMethodPrompt's three terminal checks
// against the CURRENT order row — every method-execution callback below
// re-validates from a freshly-read row rather than trusting the tapped
// button, so a stale screen can never double-refund. Returns "" when all
// three pass.
func refundFailureText(o *store.Order) string {
	switch {
	case o.Status == "refunded":
		return "⛔ Order already refunded:\n" + o.Des
	case !isRefundableStatus(o.Status):
		return "⛔ Order is '" + o.Status + "', not refundable (only paid/delivered):\n" + o.Des
	case o.Product == depositProduct:
		return "⛔ This is a deposit/top-up, not a purchase — not refundable here:\n" + o.Des
	}
	return ""
}

// ============================================================================
// Force-full refund: frefund: -> frefundc: -> frefundm:
// ============================================================================

// forceRefundPrompt is _force_refund_prompt — same 3 validations as
// refundMethodPrompt, then offers the method picker routed through frefundc:
// (a CONFIRM screen, no money moves yet) instead of straight to refundm:.
// Reached both by an explicit ⚠️ Force FULL refund tap (frefund:<des>) and by
// refundMethodPrompt's own automatic escalation when nothing prorates.
func (b *Bot) forceRefundPrompt(ctx context.Context, chatID int64, o *store.Order) {
	if msg := refundFailureText(o); msg != "" {
		b.send(ctx, chatID, msg, admNav("admcat:orders"))
		return
	}
	full := o.Total
	if isApikeyFamily(o.Product) {
		_, full = b.refundGroupTotal(o)
	}
	text := "⚠️ Order " + o.Des + " has nothing left to prorate (or credits are unreadable).\n" +
		"Refund the ENTIRE paid total: " + b.usdtDisp(full) + "\n\nChoose refund method:"
	rows := [][]tg.InlineKeyboardButton{
		btnRow("💰 Force refund to balance ("+b.usdtDisp(full)+")", cbData("frefundc", o.Des, "balance")),
		btnRow("🏦 Force refund manual ("+b.usdtDisp(full)+")", cbData("frefundc", o.Des, "manual")),
		btnRow("❌ Cancel", "refundno"),
	}
	b.sendMenu(ctx, chatID, text, rows)
}

// cbForceRefund handles frefund:<des> — re-fetches the order and delegates to
// forceRefundPrompt, which owns the 3 validations.
func (b *Bot) cbForceRefund(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	des := strings.TrimPrefix(cc.Data, "frefund:")
	o := b.mustGetOrder(cc.Ctx, cc.ChatID, des)
	if o == nil {
		return
	}
	b.forceRefundPrompt(cc.Ctx, cc.ChatID, o)
}

// mustGetOrder fetches an order by des, sending the standard "not found"
// reply and returning nil when it's missing — the shared not-found guard
// every refund sub-callback below needs before it can re-validate anything.
func (b *Bot) mustGetOrder(ctx context.Context, chatID int64, des string) *store.Order {
	if b.store == nil {
		b.send(ctx, chatID, settingsNotConfigured, admNav("admcat:orders"))
		return nil
	}
	o, err := b.store.GetOrder(des)
	if err != nil || o == nil {
		b.send(ctx, chatID, "❌ No order found with ID:\n"+des, admNav("admcat:orders"))
		return nil
	}
	return o
}

// cbForceRefundConfirm handles frefundc:<des>:<method> — the "no money moves
// yet" confirm screen, verbatim per py_orders_spec.md.
func (b *Bot) cbForceRefundConfirm(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "frefundc:")
	des, method, ok := strings.Cut(rest, ":")
	if !ok || (method != "balance" && method != "manual") {
		return // malformed callback
	}
	o := b.mustGetOrder(cc.Ctx, cc.ChatID, des)
	if o == nil {
		return
	}
	if msg := refundFailureText(o); msg != "" {
		b.send(cc.Ctx, cc.ChatID, msg, admNav("admcat:orders"))
		return
	}
	full := o.Total
	if isApikeyFamily(o.Product) {
		_, full = b.refundGroupTotal(o)
	}
	dest := "→ buyer balance"
	if method == "manual" {
		dest = "→ QR / bank transfer (manual)"
	}
	text := "⚠️ CONFIRM force-full refund\n🧾 " + des + "\n" +
		"Refund the ENTIRE paid total: " + b.usdtDisp(full) + " " + dest + "\n" +
		"This cannot be undone."
	rows := [][]tg.InlineKeyboardButton{
		btnRow("✅ Confirm full refund "+b.usdtDisp(full), cbData("frefundm", des, method)),
		btnRow("❌ Cancel", "refundno"),
	}
	b.sendMenu(cc.Ctx, cc.ChatID, text, rows)
}

// cbForceRefundExec handles frefundm:<des>:<method> — executes the force-full
// refund via doRefund(..., forceFull=true).
func (b *Bot) cbForceRefundExec(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "frefundm:")
	des, method, ok := strings.Cut(rest, ":")
	if !ok || (method != "balance" && method != "manual") {
		return
	}
	o := b.mustGetOrder(cc.Ctx, cc.ChatID, des)
	if o == nil {
		return
	}
	if msg := refundFailureText(o); msg != "" {
		b.send(cc.Ctx, cc.ChatID, msg, admNav("admcat:orders"))
		return
	}
	res := b.doRefund(cc.Ctx, o, method, true)
	if res == nil {
		b.send(cc.Ctx, cc.ChatID, "⛔ Could not refund (status changed). Re-check:\n"+des, admNav("admcat:orders"))
		return
	}
	b.sendRefundReceipt(cc.Ctx, cc.ChatID, o, res, true)
}

// ============================================================================
// refundm: — pro-rated method execution
// ============================================================================

// cbRefundMethod handles refundm:<des>:<method> — re-validates fully from the
// CURRENT row (never trusts the tapped button, so a stale screen can't
// double-refund) then executes via doRefund.
func (b *Bot) cbRefundMethod(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "refundm:")
	des, method, ok := strings.Cut(rest, ":")
	if !ok || (method != "balance" && method != "manual") {
		return // malformed callback
	}
	o := b.mustGetOrder(cc.Ctx, cc.ChatID, des)
	if o == nil {
		return
	}
	if msg := refundFailureText(o); msg != "" {
		b.send(cc.Ctx, cc.ChatID, msg, admNav("admcat:orders"))
		return
	}
	res := b.doRefund(cc.Ctx, o, method, false)
	if res == nil {
		b.send(cc.Ctx, cc.ChatID, "⛔ Could not refund (status changed). Re-check:\n"+des, admNav("admcat:orders"))
		return
	}
	b.sendRefundReceipt(cc.Ctx, cc.ChatID, o, res, false)
}

// sendRefundReceipt renders the post-refund confirmation shared by refundm:
// and frefundm: — verbatim text per py_orders_spec.md, differing only in the
// header line and whether the "day/hour X of N" context or the "full undo"
// line is shown. When force-full left named accounts in sold/, offers the
// ↩️ Restore button.
func (b *Bot) sendRefundReceipt(ctx context.Context, chatID int64, o *store.Order, res *refundResult, forceFull bool) {
	date := fmtWhen(o.TS, i18n.EN)
	header := "✅ Order REFUNDED"
	if forceFull {
		header = "✅ Order FORCE-REFUNDED (full total)"
	}
	var balLine string
	switch {
	case res.CreditFailed:
		// The claim committed but CreditBalance errored — the order IS
		// 'refunded' and no automatic path will ever retry it, so this
		// screen must be as loud as the customer DM we just skipped: never a
		// quiet ✅ "balance now 0", always a page for a manual credit.
		header = "❌ WALLET CREDIT FAILED — credit " + b.usdtDisp(res.Amount) + " to " + o.ChatID + " by hand"
		balLine = "⚠️ Wallet credit FAILED — balance NOT updated. Credit the buyer manually."
	case res.Method == "manual":
		balLine = "Refund " + b.usdtDisp(res.Amount) + " → send by QR / bank transfer (manual)"
	default:
		balLine = "Refund " + b.usdtDisp(res.Amount) + " → buyer balance (now " + b.usdtDisp(res.Balance) + ")"
	}
	var notifyLine string
	switch {
	case res.CreditFailed:
		notifyLine = "⚠️ Customer NOT notified — do not tell them it's refunded until the credit above is fixed."
	case res.Notified:
		notifyLine = "Customer notified."
	default:
		notifyLine = "⚠️ Customer DM FAILED (they may have blocked the bot)."
	}
	lines := []string{header,
		"🧾 " + o.Des,
		"🛡️ " + b.tierLabel(o.Tier, i18n.EN) + "  📅 " + date,
		balLine,
	}
	if forceFull {
		lines = append(lines, "(full undo — paid "+b.usdtDisp(res.PaidTotal)+")")
	} else {
		lines = append(lines, "("+strings.ToLower(res.Unit)+" "+itoa64(res.UnitNum)+"/"+itoa64(res.UnitsTotal)+", paid "+b.usdtDisp(res.PaidTotal)+")")
	}
	// Single concatenated line, matching Python's f-string `+` chain
	// (telegram_bot.py:15546) exactly — NOT two separate lines with a stray
	// leading space.
	lines = append(lines, notifyLine+" Status 'refunded' — won't refund again.")

	var names []string
	if b.stock != nil {
		names = b.stock.OrderUsernames(o.Des)
	}
	var rows [][]tg.InlineKeyboardButton
	if forceFull && len(names) > 0 {
		lines = append(lines, "",
			"The accounts are still in SOLD. Restore them to active stock only if the buyer never got usable accounts.")
		rows = append(rows, btnRow("↩️ Restore "+itoa(len(names))+" account(s) to stock", cbData("frstock", o.Des)))
	}
	rows = append(rows, btnRow("⬅️ Orders", "admcat:orders"))
	b.sendMenu(ctx, chatID, strings.Join(lines, "\n"), rows)
}

// ============================================================================
// Stock restore (force-refund undo): frstock: -> frstockyes:
// ============================================================================

// cbForceRestore handles frstock:<des> — the "DANGER" confirm gate before any
// sold account is re-listed for resale.
func (b *Bot) cbForceRestore(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	des := strings.TrimPrefix(cc.Data, "frstock:")
	var names []string
	if b.stock != nil {
		names = b.stock.OrderUsernames(des)
	}
	if len(names) == 0 {
		b.send(cc.Ctx, cc.ChatID, "🧾 "+des+"\nNo named accounts to restore.", admNav("admcat:orders"))
		return
	}
	text := "⚠️ Restore " + itoa(len(names)) + " delivered account(s) to ACTIVE stock?\n🧾 " + des + "\n\n" +
		"DANGER: the buyer still holds these credentials. Restoring re-lists the SAME accounts for resale — a future buyer would share the account with this one. Only do this if the accounts were never usable (fraud / dead on arrival)."
	rows := [][]tg.InlineKeyboardButton{
		btnRow("✅ Yes, restore "+itoa(len(names))+" to stock", cbData("frstockyes", des)),
		btnRow("❌ No, keep them sold", "refundno"),
	}
	b.sendMenu(cc.Ctx, cc.ChatID, text, rows)
}

// cbForceRestoreYes handles frstockyes:<des> — re-checks status=='refunded'
// (defense against a stale confirm screen) then moves sold/ -> active/ via
// RestoreSold, which is itself idempotent (already-active entries count as
// restored, no clobber).
func (b *Bot) cbForceRestoreYes(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	des := strings.TrimPrefix(cc.Data, "frstockyes:")
	o := b.mustGetOrder(cc.Ctx, cc.ChatID, des)
	if o == nil {
		return
	}
	if o.Status != "refunded" {
		b.send(cc.Ctx, cc.ChatID, "⛔ Order is '"+o.Status+"', not refunded — refund it first before restoring its stock:\n"+des, admNav("admcat:orders"))
		return
	}
	if b.stock == nil {
		b.send(cc.Ctx, cc.ChatID, settingsNotConfigured, admNav("admcat:orders"))
		return
	}
	names := b.stock.OrderUsernames(des)
	restored, unmatched, err := b.stock.RestoreSold(o.Product, names)
	if err != nil {
		b.send(cc.Ctx, cc.ChatID, "❌ Restore failed: "+err.Error(), admNav("admcat:orders"))
		return
	}
	note := ""
	if len(unmatched) > 0 {
		note = "\n⚠️ " + itoa(len(unmatched)) + " could not be matched in sold/ (already restored, or legacy token stock) — left as-is."
	}
	b.send(cc.Ctx, cc.ChatID,
		"✅ Restored "+itoa(restored)+" account(s) to active stock\n🧾 "+des+"\nProduct: "+o.Product+note,
		admNav("admcat:orders"))
}

// cbRefundNo handles refundno — cancels any in-flight refund confirm screen.
// No pending state to pop: every step up to here carries its own des/method
// in the callback data, never in ChatState.
func (b *Bot) cbRefundNo(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	b.send(cc.Ctx, cc.ChatID, "❌ Refund cancelled.", admNav("admcat:orders"))
}

// ============================================================================
// admcat:refundreq / refrev: / refrej: / refappr: — refund-request review queue
// ============================================================================

// refundRequestQueueMenu renders the 🔁 Refund requests queue — the
// admcat:refundreq case of adminCategoryMenuRows (admin.go). Always ok=true;
// the empty-pending case still returns a non-empty row slice (just the Back
// button) so cbAdmCat's `len(rows)==0` guard never swallows the "none
// pending" message.
func (b *Bot) refundRequestQueueMenu() (title string, rows [][]tg.InlineKeyboardButton, ok bool) {
	backRow := [][]tg.InlineKeyboardButton{btnRow("↩️ Back", "admcat:orders")}
	if b.refundStore == nil {
		return "🔁 Refund requests: none pending.", backRow, true
	}
	reqs, err := b.refundStore.RequestsByStatus([]string{refund.StatusPending}, 50)
	if err != nil || len(reqs) == 0 {
		return "🔁 Refund requests: none pending.", backRow, true
	}
	rows = make([][]tg.InlineKeyboardButton, 0, len(reqs)+1)
	for _, r := range reqs {
		flag := "•"
		if r.PredatedSuspend {
			flag = "⚡"
		}
		when := time.Unix(int64(r.TS), 0).Format("01-02")
		label := flag + " #" + itoa64(r.ID) + " · " + r.Product + " · " + b.usdtDisp(r.Total) + " · " + when
		rows = append(rows, btnRow(label, cbData("refrev", itoa64(r.ID))))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:orders"))
	return "🔁 REFUND REQUESTS (" + itoa(len(reqs)) + " pending)", rows, true
}

// refundRequestAsOf computes the FROZEN as-of snapshot for a customer refund
// request's pro-rated preview — _refund_request_asof (py_orders_spec.md):
// min(filing ts, the product-suspend ts snapshotted AT FILING TIME, if any).
// Freezing here (both for the review preview AND for refappr:'s later
// payout) is what stops review delay from shrinking what the admin is about
// to approve.
func refundRequestAsOf(r *refund.Request) float64 {
	asOf := r.TS
	// A stored suspend ts of exactly 0 is falsy in Python (`if t` @4476) —
	// treat it as absent rather than collapsing the frozen clock to the Unix
	// epoch (see refundAmount's identical guard, and refund.go's scanRequest
	// which already filters this at the storage layer — this check is
	// defense in depth).
	if r.ProductSuspendTS != nil && *r.ProductSuspendTS > 0 && *r.ProductSuspendTS < asOf {
		asOf = *r.ProductSuspendTS
	}
	return asOf
}

// cbRefundReview handles refrev:<id> — shows one pending request's detail
// plus Approve (super-admin only)/Reject/Back. An unparsable id is a
// malformed callback and is silently swallowed, matching Python's
// `except ValueError: pass`.
func (b *Bot) cbRefundReview(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(cc.Data, "refrev:"), 10, 64)
	if err != nil {
		return
	}
	if b.refundStore == nil {
		return
	}
	r, err := b.refundStore.GetRequest(id)
	if err != nil || r == nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Request not found.", admNav("admcat:refundreq"))
		return
	}
	if r.Status != refund.StatusPending {
		b.send(cc.Ctx, cc.ChatID, "#"+itoa64(id)+" already "+r.Status+".", admNav("admcat:refundreq"))
		return
	}
	when := time.Unix(int64(r.TS), 0).Format("01-02 15:04")
	lines := []string{
		"🔁 REFUND REQUEST #" + itoa64(id),
		"🧾 " + r.Des,
		"📦 " + r.Product + " · 🛡️ " + b.tierLabel(r.Tier, i18n.EN),
		"💰 paid " + b.usdtDisp(r.Total) + " · 📅 " + when,
	}
	if r.PredatedSuspend && b.productSuspended(r.Product) {
		lines = append(lines, "⚡ Customer filed BEFORE this product was suspended.")
	}
	if o, oerr := b.getOrderIfWired(r.Des); oerr != nil || o == nil {
		lines = append(lines, "➡️ ⚠️ order row not found")
	} else {
		asOf := refundRequestAsOf(r)
		var amount, unitsTotal, unitNum int64
		var unit string
		if isApikeyFamily(o.Product) {
			amount, unitsTotal, unitNum = b.apikeyRefundAmount(cc.Ctx, o)
			unit = "Credit"
		} else {
			amount, unitsTotal, unitNum, unit = b.refundAmount(o, &asOf)
		}
		if unitsTotal > 0 {
			lines = append(lines, "➡️ "+unit+" "+itoa64(unitNum)+" of "+itoa64(unitsTotal)+" → pro-rated "+b.usdtDisp(amount))
		} else {
			lines = append(lines, "➡️ nothing to prorate — approving routes to a force-full refund")
		}
	}
	lines = append(lines, "")
	if b.isSuperAdmin(cc.ChatID) {
		lines = append(lines, "Approve routes you into the refund method picker.")
	} else {
		lines = append(lines, "Approval (payout) is restricted to a full admin.")
	}
	lines = append(lines, "Choose:")

	var rows [][]tg.InlineKeyboardButton
	if b.isSuperAdmin(cc.ChatID) {
		rows = append(rows, btnRow("✅ Approve (open refund UI)", cbData("refappr", itoa64(id))))
	}
	rows = append(rows,
		btnRow("❌ Reject", cbData("refrej", itoa64(id))),
		btnRow("↩️ Back", "admcat:refundreq"))
	b.sendMenu(cc.Ctx, cc.ChatID, strings.Join(lines, "\n"), rows)
}

// getOrderIfWired is a nil-safe store.GetOrder — several read-only preview
// paths in this file need "order row, or none" without a repeated b.store==nil
// dance at every call site.
func (b *Bot) getOrderIfWired(des string) (*store.Order, error) {
	if b.store == nil {
		return nil, nil
	}
	return b.store.GetOrder(des)
}

// cbRefundReject arms the rejection-reason prompt — refrej:<id>
// (py_orders_spec.md step 1). orders_view-capability, reachable by any admin
// (not just super), matching admincap.go's classification.
func (b *Bot) cbRefundReject(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(cc.Data, "refrej:"), 10, 64)
	if err != nil {
		return
	}
	cc.st.mu.Lock()
	cc.st.RefundReject = &id
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, "❌ Enter the rejection reason for refund request #"+itoa64(id)+":", backKB("admcat:refundreq"))
}

// capRefundReject is the "refundreject" capture (#26, PEEK semantics —
// py_orders_spec.md/telegram_bot.py:18732-18733: the pending id survives a
// reply-keyboard label or slash command UNTOUCHED; it clears ONLY once the
// text has passed that guard). UNLIKE every other capture in this package,
// there is no "invalid input" branch past the guard at all — ANY typed line
// (even empty after trim) is accepted as the reason and commits on the first
// non-nav reply, matching py_orders_spec.md exactly.
func (b *Bot) capRefundReject(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.RefundReject
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	c.st.mu.Lock()
	c.st.RefundReject = nil
	c.st.mu.Unlock()
	if !b.isAdmin(c.ChatID) || b.refundStore == nil {
		return true
	}
	reason := strings.TrimSpace(c.Text)
	req, err := b.refundStore.AdminDecide(*armed, c.ChatID, false, &reason)
	if err != nil {
		log.Println("adminorders: AdminDecide (reject) failed:", err)
		b.send(c.Ctx, c.ChatID, settingsSaveFailed, admNav("admcat:refundreq"))
		return true
	}
	if req == nil {
		b.send(c.Ctx, c.ChatID, "⚠️ Request #"+itoa64(*armed)+" is no longer pending.", admNav("admcat:refundreq"))
		return true
	}
	if cid, perr := strconv.ParseInt(req.ChatID, 10, 64); perr == nil {
		lang := b.langFor(cid, "en")
		b.send(c.Ctx, cid, i18n.F(i18n.C().Orders.RefundDecisionRejected.T(lang), "des", req.Des, "reason", reason), nil)
	}
	b.send(c.Ctx, c.ChatID, "❌ Refund request #"+itoa64(*armed)+" rejected.", admNav("admcat:refundreq"))
	return true
}

// cbRefundApprove handles refappr:<id> — approve does NOT itself move money;
// it flips refund_requests.status -> approved, freezes the warranty clock,
// notifies the customer that a refund is "being processed", then re-uses the
// EXACT SAME refundMethodPrompt the admin adm:refund flow uses for the actual
// payout (py_orders_spec.md). Super-admin ONLY, with a loud denial — no
// capability unlocks this, matching Python's server-side belt-and-braces
// (the button itself is already hidden for non-super-admins in
// cbRefundReview).
func (b *Bot) cbRefundApprove(cc *cbCtx) {
	if !b.isSuperAdmin(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, "⛔ Approving a refund is restricted to a full admin.", admNav("admcat:refundreq"))
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(cc.Data, "refappr:"), 10, 64)
	if err != nil {
		return
	}
	if b.refundStore == nil {
		return
	}
	req, err := b.refundStore.AdminDecide(id, cc.ChatID, true, nil)
	if err != nil {
		log.Println("adminorders: AdminDecide (approve) failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("admcat:refundreq"))
		return
	}
	if req == nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Request #"+itoa64(id)+" is no longer pending.", admNav("admcat:refundreq"))
		return
	}
	b.freezeRefundAsOf(req.Des, refundRequestAsOf(req))
	if cid, perr := strconv.ParseInt(req.ChatID, 10, 64); perr == nil {
		lang := b.langFor(cid, "en")
		b.send(cc.Ctx, cid, i18n.F(i18n.C().Orders.RefundDecisionApproved.T(lang), "des", req.Des), nil)
	}
	o, oerr := b.getOrderIfWired(req.Des)
	if oerr != nil || o == nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Approved, but order "+req.Des+" row not found to refund.", admNav("admcat:refundreq"))
		return
	}
	b.refundMethodPrompt(cc.Ctx, cc.ChatID, o)
}

// ============================================================================
// admrefdel / refdelone: / refdeloneok: / refdelconf — Entra deprovisioning sweep
// ============================================================================

// entraDeleteRestrictedNote is the shared LOUD refusal for every callback in
// this sweep — Python's own comment: "restricted to a full admin", no
// capability ever unlocks it.
const entraDeleteRestrictedNote = "⛔ Deleting refunded accounts is restricted to a full admin."

// entraDeleteHazardNote is shown on every confirm screen this sweep renders.
//
// RESOLVED DIVERGENCE (was flagged, now fixed): an earlier revision of this
// file had to call entra.CleanupDeleteByUPN — the ALWAYS-"--purge" primitive
// built for the unrelated "🧼 Clean Up Orders" action (py_entra_spec.md §9)
// — because internal/entra had no soft-delete export yet, which made every
// delete in this sweep PERMANENT despite py_orders_spec.md describing
// admrefdel/refdelone: as a SOFT delete ("--delete-by-upn" WITHOUT
// "--purge", recoverable in Entra for ~30 days). entra.SoftDeleteByUPN now
// exists and entraRunRefundDelete below calls it, so this note goes back to
// stating the TRUE, spec-matching behaviour instead of the now-false
// "PERMANENT purge" warning.
const entraDeleteHazardNote = "⚠️ Soft-delete only (NOT purged) — recoverable in Entra for ~30 days if this was a mistake or the refund is reversed."

// entraDeleteListMax caps how many UPNs a delete confirm screen names
// individually before falling back to a "+N more" summary — an irreversible-
// adjacent action (recoverable, but not something to click through blind)
// must show WHICH accounts are targeted, but the bulk sweep can carry
// hundreds of emails and Telegram messages have a hard length limit.
const entraDeleteListMax = 20

// truncatedEmailList renders up to `max` UPNs joined by ", ", with a
// "+N more" suffix beyond that — used by both the bulk preview and the
// single-order confirm screen so an admin approving a delete can always see
// WHICH accounts are named, not just a count.
func truncatedEmailList(emails []string, max int) string {
	if len(emails) <= max {
		return strings.Join(emails, ", ")
	}
	return strings.Join(emails[:max], ", ") + ", +" + itoa(len(emails)-max) + " more"
}

// cbAdmRefDel starts the bulk sweep — admrefdel (py_orders_spec.md).
func (b *Bot) cbAdmRefDel(cc *cbCtx) {
	if !b.isSuperAdmin(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, entraDeleteRestrictedNote, admNav("admcat:orders"))
		return
	}
	b.send(cc.Ctx, cc.ChatID, "🔎 Scanning refunded orders…", admNav("admcat:orders"))
	chatID := cc.ChatID
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			b.entraRefundDeletePreview(fctx, chatID)
		})
	})
}

// entraRefundDeletePreview scans every 'refunded' order, collects the
// still-undeleted @-shaped identifiers (password-file/token accounts with no
// '@' have no Entra tenant account and are never touched by this sweep),
// de-dupes emails across orders, arms the bulk confirm gate, and shows the
// preview — _entra_refund_delete_preview (py_orders_spec.md).
func (b *Bot) entraRefundDeletePreview(ctx context.Context, chatID int64) {
	none := "ℹ️ No refunded order has an undeleted Entra account — nothing to delete."
	if b.store == nil || b.stock == nil {
		b.send(ctx, chatID, none, admNav("admcat:orders"))
		return
	}
	orders, err := b.store.AllOrders([]string{"refunded"})
	if err != nil {
		log.Println("adminorders: AllOrders(refunded) failed:", err)
		b.send(ctx, chatID, none, admNav("admcat:orders"))
		return
	}
	var eligibleOrders []string
	emailSeen := map[string]bool{}
	var emails []string
	for _, o := range orders {
		if o.EntraDeletedTS != nil {
			continue
		}
		var upns []string
		for _, u := range b.stock.OrderUsernames(o.Des) {
			if strings.Contains(u, "@") {
				upns = append(upns, u)
			}
		}
		if len(upns) == 0 {
			continue
		}
		eligibleOrders = append(eligibleOrders, o.Des)
		for _, u := range upns {
			key := strings.ToLower(u)
			if !emailSeen[key] {
				emailSeen[key] = true
				emails = append(emails, u)
			}
		}
	}
	if len(emails) == 0 {
		b.send(ctx, chatID, none, admNav("admcat:orders"))
		return
	}
	st := b.states.Get(chatID)
	st.mu.Lock()
	st.RefundDelete = &RefundDeleteCtx{Orders: eligibleOrders, Emails: emails}
	st.mu.Unlock()
	text := "🗑️ DELETE REFUNDED ACCOUNTS — preview\n" +
		"📦 Refunded orders eligible: " + itoa(len(eligibleOrders)) + "\n" +
		"👤 Tenant users to delete: " + itoa(len(emails)) + "\n" +
		"👤 " + truncatedEmailList(emails, entraDeleteListMax) + "\n" +
		strings.Repeat("─", 12) + "\n" +
		entraDeleteHazardNote
	rows := [][]tg.InlineKeyboardButton{
		btnRow("🗑 Yes, delete "+itoa(len(emails))+" accounts", "refdelconf"),
		btnRow("❌ Cancel", "admcat:orders"),
	}
	b.sendMenu(ctx, chatID, text, rows)
}

// cbRefundDeleteConfirm handles refdelconf — the bulk sweep's execute step.
func (b *Bot) cbRefundDeleteConfirm(cc *cbCtx) {
	if !b.isSuperAdmin(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, entraDeleteRestrictedNote, admNav("admcat:orders"))
		return
	}
	cc.st.mu.Lock()
	pend := cc.st.RefundDelete
	valid := pend != nil && !pend.Single && len(pend.Emails) > 0
	if valid {
		cc.st.RefundDelete = nil
	}
	cc.st.mu.Unlock()
	if !valid {
		b.send(cc.Ctx, cc.ChatID, "⚠️ That confirmation expired. Open 🗑️ Delete Refunded Accounts again.", admNav("admcat:orders"))
		return
	}
	b.send(cc.Ctx, cc.ChatID,
		"🗑️ Deleting "+itoa(len(pend.Emails))+" accounts from "+itoa(len(pend.Orders))+" refunded order(s)… this can take a while.",
		nil)
	orders, chatID := pend.Orders, cc.ChatID
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			// Re-resolve each order's CURRENT tenant emails at confirm time
			// rather than trusting the preview-time snapshot
			// (py_orders_spec.md / telegram_bot.py:12797's orders_with_emails
			// re-derivation) — the preview runs in its own thread, so an
			// admin can restore or already delete an account (e.g. via
			// refdelone:) in the gap before this tap; the accounts actually
			// targeted must reflect LIVE state, not the stale scan.
			emails := b.currentRefundDeleteEmails(orders)
			b.entraRunRefundDelete(fctx, chatID, orders, emails)
		})
	})
}

// currentRefundDeleteEmails re-resolves the CURRENT set of "@"-shaped tenant
// UPNs across `orders`, de-duped case-insensitively — the same selection
// entraRefundDeletePreview uses, called again at confirm time so the bulk
// sweep never acts on a stale preview-time snapshot (see cbRefundDeleteConfirm).
func (b *Bot) currentRefundDeleteEmails(orders []string) []string {
	if b.stock == nil {
		return nil
	}
	seen := map[string]bool{}
	var emails []string
	for _, des := range orders {
		for _, u := range b.stock.OrderUsernames(des) {
			if !strings.Contains(u, "@") {
				continue
			}
			key := strings.ToLower(u)
			if !seen[key] {
				seen[key] = true
				emails = append(emails, u)
			}
		}
	}
	return emails
}

// entraRunRefundDelete stages upns, runs entra.SoftDeleteByUPN (soft, no
// "--purge" — see entraDeleteHazardNote above), and marks every order in the
// batch processed ONLY when the run was fully clean (res.Clean(): Failed==0
// && Protected==0) — a partial-failure batch marks NOTHING, so a re-run
// stays safe (Graph reports already-deleted accounts as not_found, i.e. this
// whole operation is idempotent). `emails` is expected to already be the
// CURRENT resolved set for `orders` (cbRefundDeleteConfirm re-resolves via
// currentRefundDeleteEmails rather than trusting a preview-time snapshot);
// an empty set means nothing is left to target and is handled before any
// subprocess call or marking.
func (b *Bot) entraRunRefundDelete(ctx context.Context, chatID int64, orders, emails []string) {
	if len(emails) == 0 {
		// Every targeted order's account(s) resolved empty at confirm time
		// (e.g. already deleted via refdelone: in the gap since the
		// preview). A zero-UPN batch would come back trivially ok/clean and
		// falsely mark every order in `orders` as deleted — bail out before
		// the subprocess and before any marking (mirrors Python's identical
		// guard, telegram_bot.py:2978-2984).
		b.send(ctx, chatID, "ℹ️ No Entra account is linked to the selected order(s) — nothing to delete.", admNav("admcat:orders"))
		return
	}
	// b.loginPythonStr() resolves through cfg (.env/ambient, task #16) rather
	// than provision.LoginPythonFromEnv's raw os.Getenv, which never sees .env.
	res, err := entra.SoftDeleteByUPN(ctx, b.scriptDirStr(), b.loginPythonStr(), emails, 0)
	if err != nil {
		b.send(ctx, chatID, "⛔ Entra delete failed: "+err.Error(), admNav("admcat:orders"))
		return
	}
	clean := res.Clean()
	marked := 0
	if clean && b.store != nil {
		now := float64(time.Now().Unix())
		for _, des := range orders {
			if err := b.store.MarkEntraDeleted(des, now); err != nil {
				log.Println("adminorders: MarkEntraDeleted failed:", err)
				continue
			}
			marked++
		}
	}
	lines := []string{
		"🗑️ Refund delete done:",
		"🗑 deleted (soft, recoverable ~30d): " + itoa(res.Deleted),
	}
	if res.NotFound > 0 {
		lines = append(lines, "ℹ️ already gone / not in tenant: "+itoa(res.NotFound))
	}
	if res.Protected > 0 {
		lines = append(lines, "🛡 protected (admins) skipped: "+itoa(res.Protected))
	}
	if res.Failed > 0 {
		lines = append(lines, "❌ failed: "+itoa(res.Failed))
	}
	lines = append(lines, "📦 orders marked processed: "+itoa(marked)+"/"+itoa(len(orders)))
	if !clean {
		lines = append(lines, "⚠️ Run wasn't fully clean — no order marked processed; re-run after resolving the above, or use the per-order 🗑️ Delete Account button (order lookup) to process orders whose accounts DID delete.")
	}
	lines = append(lines, "(targeted: "+itoa(res.Targeted)+")")
	b.send(ctx, chatID, strings.Join(lines, "\n"), admNav("admcat:orders"))
	if b.store != nil {
		// Field mapping matches Python's _audit call exactly
		// (telegram_bot.py:3035-3037): target is a plain account count,
		// outcome carries every batch counter plus the marking ratio — NOT a
		// comma-joined des list, and NOT just "deleted=N".
		b.store.LogAction(fmtInt(chatID), "admin", "refund_delete",
			itoa(len(emails))+" accounts",
			"deleted="+itoa(res.Deleted)+" gone="+itoa(res.NotFound)+" protected="+itoa(res.Protected)+
				" failed="+itoa(res.Failed)+" orders_marked="+itoa(marked)+"/"+itoa(len(orders)),
			"")
	}
}

// cbRefundDeleteOne handles refdelone:<des> — the single-order variant,
// reachable from the adm:getord detail screen's 🗑️ Delete Account button or
// the per-order refund receipt.
func (b *Bot) cbRefundDeleteOne(cc *cbCtx) {
	if !b.isSuperAdmin(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, entraDeleteRestrictedNote, admNav("admcat:orders"))
		return
	}
	des := strings.TrimPrefix(cc.Data, "refdelone:")
	o := b.mustGetOrder(cc.Ctx, cc.ChatID, des)
	if o == nil {
		return
	}
	// Three DISTINCT refusals (py_orders_spec.md/telegram_bot.py:12806-12840),
	// not one collapsed message — an admin staring at "not eligible" can't
	// tell "already deleted" from "not even refunded" apart.
	if o.Status != "refunded" {
		b.send(cc.Ctx, cc.ChatID, "⚠️ That order is no longer refunded (or doesn't exist).", admNav("admcat:orders"))
		return
	}
	if o.EntraDeletedTS != nil {
		b.send(cc.Ctx, cc.ChatID, "ℹ️ This order's account was already deleted.", admNav("admcat:orders"))
		return
	}
	var emails []string
	if b.stock != nil {
		for _, u := range b.stock.OrderUsernames(des) {
			if strings.Contains(u, "@") {
				emails = append(emails, u)
			}
		}
	}
	if len(emails) == 0 {
		b.send(cc.Ctx, cc.ChatID, "ℹ️ No Entra account is linked to this order — nothing to delete.", admNav("admcat:orders"))
		return
	}
	cc.st.mu.Lock()
	cc.st.RefundDelete = &RefundDeleteCtx{Single: true, Des: des, Orders: []string{des}, Emails: emails}
	cc.st.mu.Unlock()
	text := "🗑️ Delete " + itoa(len(emails)) + " tenant account(s) for order " + des + "?\n" +
		"👤 " + truncatedEmailList(emails, entraDeleteListMax) + "\n" + entraDeleteHazardNote
	rows := [][]tg.InlineKeyboardButton{
		btnRow("🗑 Yes, delete", cbData("refdeloneok", des)),
		btnRow("❌ Cancel", "admcat:orders"),
	}
	b.sendMenu(cc.Ctx, cc.ChatID, text, rows)
}

// cbRefundDeleteOneConfirm handles refdeloneok:<des> — re-validates the
// pending confirm matches THIS des exactly before running the same core
// sweep with a single-entry list.
func (b *Bot) cbRefundDeleteOneConfirm(cc *cbCtx) {
	if !b.isSuperAdmin(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, entraDeleteRestrictedNote, admNav("admcat:orders"))
		return
	}
	des := strings.TrimPrefix(cc.Data, "refdeloneok:")
	cc.st.mu.Lock()
	pend := cc.st.RefundDelete
	valid := pend != nil && pend.Single && pend.Des == des && len(pend.Emails) > 0
	if valid {
		cc.st.RefundDelete = nil
	}
	cc.st.mu.Unlock()
	if !valid {
		b.send(cc.Ctx, cc.ChatID, "⚠️ That confirmation expired. Open the order lookup again.", admNav("admcat:orders"))
		return
	}
	b.send(cc.Ctx, cc.ChatID, "🗑️ Deleting "+itoa(len(pend.Emails))+" account(s)… this can take a while.", nil)
	orders, emails, chatID := pend.Orders, pend.Emails, cc.ChatID
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			b.entraRunRefundDelete(fctx, chatID, orders, emails)
		})
	})
}

// ============================================================================
// adm:revenue / admrev: / admrevmore: / admrevp: — revenue view
// ============================================================================

// revenueRangeSince computes the (sinceTS, label) pair for a range key —
// _revenue_range (py_orders_spec.md), LOCAL-time midnight boundaries so "7
// days" always means 7 calendar days on the admin's clock, not a rolling
// 168h window.
func revenueRangeSince(rangeKey string, now time.Time) (sinceTS *float64, label string) {
	local := now.Local()
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	switch rangeKey {
	case "today":
		ts := float64(midnight.Unix())
		return &ts, "Today"
	case "30":
		ts := float64(midnight.AddDate(0, 0, -29).Unix())
		return &ts, "Last 30 days"
	case "all":
		return nil, "All time"
	default: // "7"
		ts := float64(midnight.AddDate(0, 0, -6).Unix())
		return &ts, "Last 7 days"
	}
}

// revenueKeyboard is _revenue_kb (py_orders_spec.md): range picker row, group
// toggle row, an optional "Show more" row (prod group only), Back to root.
func (b *Bot) revenueKeyboard(rangeKey, group string) [][]tg.InlineKeyboardButton {
	mark := func(key, cur, txt string) string {
		if key == cur {
			return "• " + txt
		}
		return txt
	}
	row1 := []tg.InlineKeyboardButton{
		{Text: mark("today", rangeKey, "Today"), CallbackData: cbData("admrev", "today", group)},
		{Text: mark("7", rangeKey, "7d"), CallbackData: cbData("admrev", "7", group)},
		{Text: mark("30", rangeKey, "30d"), CallbackData: cbData("admrev", "30", group)},
		{Text: mark("all", rangeKey, "All"), CallbackData: cbData("admrev", "all", group)},
	}
	row2 := []tg.InlineKeyboardButton{
		{Text: mark("date", group, "📅 By date"), CallbackData: cbData("admrev", rangeKey, "date")},
		{Text: mark("prod", group, "🏷 By product"), CallbackData: cbData("admrev", rangeKey, "prod")},
	}
	rows := [][]tg.InlineKeyboardButton{row1, row2}
	if group == "prod" {
		rows = append(rows, btnRow("🔎 Show more (pick a product)", cbData("admrevmore", rangeKey)))
	}
	rows = append(rows, btnRow("↩️ Back", "admroot"))
	return rows
}

// sendRevenue renders the by-date or by-product matrix for (rangeKey, group).
func (b *Bot) sendRevenue(ctx context.Context, chatID int64, rangeKey, group string) {
	sinceTS, label := revenueRangeSince(rangeKey, b.now())
	kb := &tg.InlineKeyboardMarkup{InlineKeyboard: b.revenueKeyboard(rangeKey, group)}
	if b.store == nil {
		b.send(ctx, chatID, "No orders yet.", kb)
		return
	}
	m, err := b.store.GetRevenueMatrix(depositProduct, sinceTS, 60)
	if err != nil {
		b.send(ctx, chatID, "⚠️ Could not compute revenue: "+err.Error(), admNav("admroot"))
		return
	}
	var text string
	if group == "date" {
		text = b.revenueByDateText(m, label)
	} else {
		text = b.revenueByProductText(m, label)
	}
	b.send(ctx, chatID, text, kb)
}

// cbRevenue handles adm:revenue (default view: 7-day, by-product).
func (b *Bot) cbRevenue(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	b.sendRevenue(cc.Ctx, cc.ChatID, "7", "prod")
}

// cbRevenueRange handles admrev:<range>:<group>.
func (b *Bot) cbRevenueRange(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "admrev:")
	rangeKey, group, ok := strings.Cut(rest, ":")
	if !ok {
		return
	}
	b.sendRevenue(cc.Ctx, cc.ChatID, rangeKey, group)
}

// revenuePickableProducts is _revenue_pickable_products (py_orders_spec.md):
// the folder catalogue plus the two virtual API-pool sale SKUs, fixed order
// so an index stays stable across ranges, capped to 90 rows.
func (b *Bot) revenuePickableProducts() []string {
	const maxPick = 90
	products := append([]string(nil), b.listProducts()...)
	products = append(products, apikeyProduct, kiroApikeyProduct)
	if len(products) > maxPick {
		products = products[:maxPick]
	}
	return products
}

// cbRevenueMore handles admrevmore:<range> — lists every pickable product,
// keyed by INDEX (cbData("admrevp", range, i)) to keep the callback short.
func (b *Bot) cbRevenueMore(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rangeKey := strings.TrimPrefix(cc.Data, "admrevmore:")
	products := b.revenuePickableProducts()
	if len(products) == 0 {
		b.send(cc.Ctx, cc.ChatID, "No products.", admNav("admroot"))
		return
	}
	rows := make([][]tg.InlineKeyboardButton, 0, len(products)+1)
	for i, p := range products {
		label := p
		if b.productSuspended(p) {
			label = "⏸ " + label
		}
		rows = append(rows, btnRow(label, cbData("admrevp", rangeKey, itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", cbData("admrev", rangeKey, "prod")))
	b.sendMenu(cc.Ctx, cc.ChatID, "💰 Pick a product:", rows)
}

// cbRevenueProduct handles admrevp:<range>:<pidx> — single-product revenue,
// bounds-checked against the LIVE pickable list at render time (works for any
// product including suspended/removed ones).
func (b *Bot) cbRevenueProduct(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "admrevp:")
	rangeKey, idxStr, ok := strings.Cut(rest, ":")
	if !ok {
		return
	}
	idx, err := strconv.Atoi(idxStr)
	products := b.revenuePickableProducts()
	if err != nil || idx < 0 || idx >= len(products) {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product not found.", admNav("admroot"))
		return
	}
	product := products[idx]
	sinceTS, label := revenueRangeSince(rangeKey, b.now())
	if b.store == nil {
		b.send(cc.Ctx, cc.ChatID, "No orders yet.", admNav("admroot"))
		return
	}
	m, err := b.store.GetRevenueMatrix(depositProduct, sinceTS, 60)
	if err != nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Could not compute revenue: "+err.Error(), admNav("admroot"))
		return
	}
	text := b.revenueSingleProductText(m, product, label)
	rows := [][]tg.InlineKeyboardButton{btnRow("↩️ Back", cbData("admrevmore", rangeKey))}
	b.send(cc.Ctx, cc.ChatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// moneyShort is a compact USDT figure for the revenue view's per-line
// entries (money_short) — no "USDT" suffix (the header line already
// establishes the currency), same cent-rounding as usdtDisp so every number
// in the view agrees. py_orders_spec.md gives only the function's name and
// call sites, no worked example of its numeral format — this is a
// deliberate, low-risk choice for a read-only admin report, not a
// money-movement path.
func (b *Bot) moneyShort(vnd int64) string {
	return strings.TrimSuffix(b.usdtDisp(vnd), " USDT")
}

// mmdd renders a "YYYY-MM-DD" RevenueDay.Date as "MM-DD".
func mmdd(date string) string {
	if len(date) >= 10 {
		return date[5:7] + "-" + date[8:10]
	}
	return date
}

// revenueByDateText is _admin_revenue_text (py_orders_spec.md): per-day
// blocks, each product line revenue-descending capped at 8 with a "+k more
// products" note, whole message capped to 3900 chars by dropping OLDEST
// day-blocks first (m.Rows already sorts most-recent-first).
func (b *Bot) revenueByDateText(m *store.RevenueMatrix, label string) string {
	head := "💰 REVENUE — " + label + "\nDelivered: " + b.usdtDisp(m.GrandTotal) + "  (" + itoa64(m.GrandCount) + " orders)"
	if len(m.Rows) == 0 {
		return head + "\n\n(no delivered orders in this range)"
	}
	const maxProductsPerDay = 8
	const limit = 3900
	footer := "Σ " + b.usdtDisp(m.GrandTotal) + " (" + itoa64(m.GrandCount) + " orders)"
	truncMarker := "⚠️ older days not shown (limit reached)"

	lines := []string{head, ""}
	used := len(head) + 1
	truncated := m.Truncated
	for _, row := range m.Rows {
		block := []string{"📅 " + mmdd(row.Date) + " · " + b.moneyShort(row.Total) + " (" + itoa64(row.Count) + ")"}
		type pv struct {
			p string
			v int64
		}
		var prs []pv
		for p, v := range row.Cells {
			if v != 0 {
				prs = append(prs, pv{p, v})
			}
		}
		sort.Slice(prs, func(a, c int) bool {
			if prs[a].v != prs[c].v {
				return prs[a].v > prs[c].v
			}
			return prs[a].p < prs[c].p
		})
		shown := min(len(prs), maxProductsPerDay)
		for _, pr := range prs[:shown] {
			block = append(block, "   • "+pr.p+": "+b.moneyShort(pr.v))
		}
		if len(prs) > maxProductsPerDay {
			block = append(block, "   • +"+itoa(len(prs)-maxProductsPerDay)+" more products")
		}
		blockLen := 0
		for _, l := range block {
			blockLen += len(l) + 1
		}
		if used+blockLen+len(footer)+len(truncMarker)+2 > limit {
			truncated = true
			break
		}
		used += blockLen
		lines = append(lines, block...)
	}
	lines = append(lines, footer)
	if truncated {
		lines = append(lines, truncMarker)
	}
	return strings.Join(lines, "\n")
}

// revenueByProductText is _admin_revenue_by_product_text (py_orders_spec.md):
// scoped to ACTIVE products (folder exists, not suspended) plus either
// virtual API-pool SKU that has revenue in range, sorted revenue-descending,
// each product's day lines (also newest-first) capped at 8. Same 3900-char
// budget, drops LOWEST-revenue products first (the caller stops appending
// once the budget is spent, and `active` is already sorted descending, so
// the products dropped are exactly the lowest-revenue tail).
func (b *Bot) revenueByProductText(m *store.RevenueMatrix, label string) string {
	type activeProd struct {
		name  string
		total int64
		count int64
	}
	var active []activeProd
	var activeTotal, activeCount int64
	for _, p := range m.Products {
		isVirtual := p == apikeyProduct || p == kiroApikeyProduct
		isActive := isVirtual || (b.stock != nil && b.stock.ProductExists(p) && !b.productSuspended(p))
		if !isActive {
			continue
		}
		active = append(active, activeProd{p, m.ProductTotals[p], m.ProductCounts[p]})
		activeTotal += m.ProductTotals[p]
		activeCount += m.ProductCounts[p]
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].total != active[j].total {
			return active[i].total > active[j].total
		}
		return active[i].name < active[j].name
	})
	head := "💰 REVENUE — " + label + " (active products + API pool)\nDelivered: " +
		b.usdtDisp(activeTotal) + "  (" + itoa64(activeCount) + " orders)"
	if len(active) == 0 {
		return head + "\n\n(no delivered revenue in this range)"
	}
	const maxDaysPerProduct = 8
	const limit = 3900
	footer := "Σ " + b.usdtDisp(activeTotal) + " (" + itoa64(activeCount) + " orders)"
	truncMarker := "⚠️ some products/days not shown (limit reached)"

	lines := []string{head, ""}
	used := len(head) + 1
	truncated := m.Truncated
	for _, ap := range active {
		block := []string{"🏷 " + ap.name + ": " + b.moneyShort(ap.total) + " (" + itoa64(ap.count) + ")"}
		type dv struct {
			date string
			v    int64
		}
		var days []dv
		for _, row := range m.Rows { // m.Rows is already most-recent-first
			if v := row.Cells[ap.name]; v != 0 {
				days = append(days, dv{row.Date, v})
			}
		}
		shown := min(len(days), maxDaysPerProduct)
		for _, d := range days[:shown] {
			block = append(block, "   • "+mmdd(d.date)+": "+b.moneyShort(d.v))
		}
		if len(days) > maxDaysPerProduct {
			block = append(block, "   • +"+itoa(len(days)-maxDaysPerProduct)+" more days")
		}
		blockLen := 0
		for _, l := range block {
			blockLen += len(l) + 1
		}
		if used+blockLen+len(footer)+len(truncMarker)+2 > limit {
			truncated = true
			break
		}
		used += blockLen
		lines = append(lines, block...)
	}
	lines = append(lines, footer)
	if truncated {
		lines = append(lines, truncMarker)
	}
	return strings.Join(lines, "\n")
}

// revenueSingleProductText is _admin_revenue_single_text (py_orders_spec.md):
// one product's own day-by-day breakdown for the range, any product
// (including suspended/removed, tagged accordingly); API-pool virtuals never
// get a tag.
func (b *Bot) revenueSingleProductText(m *store.RevenueMatrix, product, label string) string {
	tag := ""
	if product != apikeyProduct && product != kiroApikeyProduct {
		switch {
		case b.stock == nil || !b.stock.ProductExists(product):
			tag = " (removed)"
		case b.productSuspended(product):
			tag = " ⏸ suspended"
		}
	}
	total, count := m.ProductTotals[product], m.ProductCounts[product]
	head := "💰 " + product + tag + " — " + label + "\nDelivered: " + b.usdtDisp(total) + "  (" + itoa64(count) + " orders)"
	type dv struct {
		date string
		v    int64
	}
	var days []dv
	for _, row := range m.Rows {
		if v := row.Cells[product]; v != 0 {
			days = append(days, dv{row.Date, v})
		}
	}
	if len(days) == 0 {
		return head + "\n\n(no delivered revenue in this range)"
	}
	const limit = 3900
	footer := "Σ " + b.usdtDisp(total) + " (" + itoa64(count) + " orders)"
	lines := []string{head, ""}
	used := len(head) + 1
	for _, d := range days {
		line := "   • " + mmdd(d.date) + ": " + b.moneyShort(d.v)
		if used+len(line)+1+len(footer)+2 > limit {
			break
		}
		used += len(line) + 1
		lines = append(lines, line)
	}
	lines = append(lines, footer)
	return strings.Join(lines, "\n")
}

// ============================================================================
// registration
// ============================================================================

// registerAdminOrders wires this file's 3 captures + every disjoint callback
// prefix into the ordered dispatch tables. MUST run after registerAdminPricing
// and registerAdminUsers (dispatch.go) so their anchors ("discount",
// "custlookup") already exist for insertCapturesBefore to splice ahead of.
// "refundreject" and "refund" both anchor on "discount", in that order, so
// they land in that relative order too (each insert-before-the-same-anchor
// call lands closer to the anchor than the previous one).
func (b *Bot) registerAdminOrders() {
	b.insertCapturesBefore("custlookup",
		stage{name: "getord", fn: (*Bot).capGetOrd}, // canonical #4, ahead of custlookup #5
	)
	b.insertCapturesBefore("discount",
		stage{name: "refundreject", fn: (*Bot).capRefundReject}, // canonical #26, ahead of discount #35
	)
	b.insertCapturesBefore("discount",
		stage{name: "refund", fn: (*Bot).capRefund}, // canonical #33, ahead of discount #34
	)
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "refundm:", fn: (*Bot).cbRefundMethod},
		cbRoute{key: "frefundc:", fn: (*Bot).cbForceRefundConfirm},
		cbRoute{key: "frefundm:", fn: (*Bot).cbForceRefundExec},
		cbRoute{key: "frefund:", fn: (*Bot).cbForceRefund},
		cbRoute{key: "frstockyes:", fn: (*Bot).cbForceRestoreYes},
		cbRoute{key: "frstock:", fn: (*Bot).cbForceRestore},
		cbRoute{key: "refundno", exact: true, fn: (*Bot).cbRefundNo},
		cbRoute{key: "refdeloneok:", fn: (*Bot).cbRefundDeleteOneConfirm},
		cbRoute{key: "refdelone:", fn: (*Bot).cbRefundDeleteOne},
		cbRoute{key: "refdelconf", exact: true, fn: (*Bot).cbRefundDeleteConfirm},
		cbRoute{key: "refappr:", fn: (*Bot).cbRefundApprove},
		cbRoute{key: "admrevmore:", fn: (*Bot).cbRevenueMore},
		cbRoute{key: "admrevp:", fn: (*Bot).cbRevenueProduct},
		cbRoute{key: "admrev:", fn: (*Bot).cbRevenueRange},
	)
}
