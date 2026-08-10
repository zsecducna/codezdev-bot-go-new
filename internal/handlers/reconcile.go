package handlers

// reconcile.go — the REPORT-ONLY stuck-order reconcile pass, porting
// reconcile_stuck_orders.py (the cron-driven owner alarm). Ports:
//
//	reconcile_stuck_orders.py:25-26  -> reconcileReport (the read)
//	reconcile_stuck_orders.py:99-107 -> reconcileStuckOrdersOnce (the log)
//	store.py:827-830                 -> the money-safety rationale (read-only)
//
// INVARIANT: this file NEVER delivers, NEVER refunds, NEVER mutates order
// status, and NEVER credits a balance. Python's reconcile_stuck_orders.py is
// explicit (@25-26) that re-driving a paid order moves real funds, so a stuck
// order is surfaced for a HUMAN to triage, never auto-handled. The CLAUDE.md
// invariants reinforce this independently: deposits are idempotent by txn_id
// at the storage layer, and provision_customer's upstreamStatus "pending" IS
// success (no retry, no double-mint) — so an automatic retry is forbidden
// regardless of how the stuck state arose. The whole pass is ONE read
// (store.PaidUndelivered) plus, conditionally, one log line. Nothing else.

import (
	"context"
	"fmt"
	"log"
	"time"
)

// reconcileAlertEscalate bounds how long an UNCHANGED stuck-order alert may
// stay suppressed before reconcileShouldAlert re-fires it anyway (the
// escalation half of the policy below). A package var, not a literal inline,
// purely so a test can shrink it like every other timing knob in this file.
var reconcileAlertEscalate = time.Hour

// reconcileReport is the testable core of the stuck-order reconcile pass —
// returns what the periodic worker would log. Read-only: it NEVER delivers,
// NEVER refunds, NEVER mutates order status. Python reconcile_stuck_orders.py
// :25-26 and store.py:827-830 are explicit that re-driving a paid order moves
// real funds, so stuck orders are surfaced for a human, never auto-handled;
// the CLAUDE.md invariants (txn_id idempotency, "pending IS success / no
// double-mint") forbid auto-retry regardless. The empty depositProduct arg
// makes PaidUndelivered apply its DepositProduct default, excluding deposit
// top-up orders (which are never "undelivered" — they have no product).
func (b *Bot) reconcileReport() (count int64, oldestTS float64, err error) {
	// b.store is nil only when an embedder or a test wires partial Deps —
	// main.go always sets it in production. Guarded for uniformity with
	// stocksweep.go's identical b.stock nil-check: without this,
	// PaidUndelivered would nil-deref, safeStep would recover the panic
	// (usdt.go:1137-1152), and reconcileStuckOrdersOnce would repeat that
	// panic/recover/log cycle every tick forever instead of quietly
	// reporting nothing until Store is wired.
	if b.store == nil {
		return 0, 0, nil
	}
	// Empty arg -> store.PaidUndelivered substitutes store.DepositProduct,
	// which the SQL filters out via COALESCE(product,'')!=DepositProduct
	// (misc.go:560-578). That is the exact split A4 introduced: deposit
	// top-ups that stall in 'paid' are a different incident class (a wallet
	// credit that never landed) and must NOT inflate the goods alarm.
	return b.store.PaidUndelivered("")
}

// reconcileStuckOrdersOnce is one pass of the report-only stuck-order
// reconcile (Python reconcile_stuck_orders.py:99-107, run every 10min by
// crontab). Logs an OWNER ALERT with the count and the oldest order's age
// when count > 0, and DMs the owners (de-duped — see reconcileShouldAlert)
// through the same channel; quiet at zero, matching the dashboard alarm at
// admin.go (which suppresses at 0).
//
// Read-only: the only call is reconcileReport (a single SELECT). No settle,
// no deliver, no ClaimOrder, no CreditBalance — re-driving a paid order
// moves real funds (reconcile_stuck_orders.py:25-26), so this pass may only
// surface, never act.
func (b *Bot) reconcileStuckOrdersOnce(ctx context.Context) {
	count, oldestTS, err := b.reconcileReport()
	if err != nil {
		// A read failure is surfaced but never fatal: the worker keeps
		// ticking, so a transient DB hiccup self-corrects on the next pass
		// (matching the cron's own resilience — a failed run does not stop
		// the schedule).
		log.Printf("reconcile stuck orders: PaidUndelivered read failed: %v", err)
		return
	}
	if count == 0 {
		// Healthy-shop path: dashboard suppresses the whole alarm line at 0
		// (admin.go:ageString caller), and so do we. Logging here would be
		// noise on every 10-min tick of a healthy shop.
		//
		// Also resets the alert de-dup fingerprint. Without this, a backlog
		// that clears and later recurs with the SAME (count, oldestTS) —
		// entirely possible, e.g. the exact same single order gets stuck
		// again — would look "unchanged" to reconcileShouldAlert and stay
		// silent, even though it is a brand-new incident from the operator's
		// point of view. A recovery must always arm the NEXT backlog's first
		// alert, matching the same first-alert rule any fresh startup gets.
		b.reconcileAlertMu.Lock()
		b.reconcileAlertCount, b.reconcileAlertOldest = 0, 0
		b.reconcileAlertSentAt = time.Time{}
		b.reconcileAlertMu.Unlock()
		return
	}

	// ageString (admin.go:455) formats a unix-seconds float as "3h ago" /
	// "2d ago", capping the granularity. It is the SAME helper the dashboard
	// alarm uses for this exact count, so the periodic log and the live
	// dashboard describe the oldest order in identical terms — a consistency
	// that matters when an operator is paging through logs to correlate.
	ageSuffix := ""
	if oldestTS > 0 {
		ageSuffix = " (oldest " + ageString(oldestTS) + ")"
	}
	log.Printf("OWNER ALERT: %d PAID-BUT-UNDELIVERED order(s)%s — MANUAL REVIEW (never auto-handled; reconcile_stuck_orders.py:25-26)",
		count, ageSuffix)
	// The Telegram send is DE-DUPED (reconcileShouldAlert) even though the log
	// line above never is: this pass re-runs every 10 minutes forever, and an
	// identical DM every tick trains the operator to ignore the channel — the
	// exact failure the log doesn't have (nobody "ignores" a log they only
	// read when investigating). See reconcileShouldAlert's doc comment.
	if !b.reconcileShouldAlert(count, oldestTS) {
		return
	}
	b.notifyOwners(ctx, fmt.Sprintf(
		"⚠️ %d PAID-BUT-UNDELIVERED order(s)%s — MANUAL REVIEW (never auto-handled)",
		count, ageSuffix))
}

// reconcileShouldAlert decides whether THIS pass's (count, oldestTS) should
// reach the owners, given what the last alert already reported.
//
// Policy: alert on the FIRST nonzero pass, and again only when the situation
// actually CHANGED — the count moved, or the oldest stuck order did. oldestTS
// is a stable fingerprint for "same order still stuck": it is that order's own
// creation timestamp, not an age, so unlike ageSuffix's "3h ago" -> "3h10m
// ago" it does NOT drift tick to tick on its own, which is what makes
// "unchanged" a meaningful, non-flapping condition here.
//
// KNOWN HOLE, stated because an earlier version of this comment claimed the
// opposite: (count, oldestTS) does NOT uniquely identify the backlog, so a
// genuinely NEW stuck order can be suppressed. PaidUndelivered reports
// MIN(ts), so if a NON-oldest order is resolved and a newer one gets stuck in
// the same window, the count is unchanged AND the minimum is unchanged, and
// this returns false. Concretely: {A ts=100, B ts=200} alerts as (2, 100); B
// is delivered while C sticks at ts=300; the pass sees {A, C} — still (2,
// 100) — and stays quiet about C.
//
// It is bounded rather than unbounded: the escalation below re-alerts within
// reconcileAlertEscalate regardless, so the blind window is at most that, not
// forever. Closing it properly means fingerprinting the SET of stuck order ids
// (a hash of the des list) instead of a count and a minimum, which needs
// PaidUndelivered to return more than an aggregate — deliberately not done
// here.
//
// Escalation backstop: even a genuinely unchanged backlog re-alerts after
// reconcileAlertEscalate (1h) so a human who missed/dismissed the first DM
// isn't left with a channel that goes silent forever on a problem that never
// got fixed — the same worry that motivates de-duping in the first place,
// applied in the other direction.
func (b *Bot) reconcileShouldAlert(count int64, oldestTS float64) bool {
	b.reconcileAlertMu.Lock()
	defer b.reconcileAlertMu.Unlock()
	first := b.reconcileAlertSentAt.IsZero()
	changed := count != b.reconcileAlertCount || oldestTS != b.reconcileAlertOldest
	stale := !first && b.now().Sub(b.reconcileAlertSentAt) >= reconcileAlertEscalate
	if !first && !changed && !stale {
		return false
	}
	b.reconcileAlertCount = count
	b.reconcileAlertOldest = oldestTS
	b.reconcileAlertSentAt = b.now()
	return true
}

// StartReconcileStuckOrders runs the report-only stuck-order reconcile on a
// ticker under the worker registry, mirroring StartupRecovery's loop shape
// (usdt.go:1201-1223): each pass is panic-isolated by safeStep, and the loop
// exits on either the registry stop chan or context cancellation. Started
// from main.go after StartupRecovery; drained automatically by bot.Shutdown.
//
// Default 600s matches Python's `*/10 * * * *` crontab (RECONCILE_STUCK_
// INTERVAL_S). The interval is a field so tests can shrink it to 1ms.
func (b *Bot) StartReconcileStuckOrders(ctx context.Context) {
	b.workers.Go(func(stop <-chan struct{}) {
		for {
			// safeStep isolates a faulted pass (the panic guard Python's
			// per-step `except Exception` provides, usdt.go:1137-1152) so one
			// bad read never stops the reconcile schedule — the whole point
			// of a backstop is that it keeps running.
			safeStep("reconcile stuck orders", func() { b.reconcileStuckOrdersOnce(ctx) })
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(b.reconcileInterval):
			}
		}
	})
}
