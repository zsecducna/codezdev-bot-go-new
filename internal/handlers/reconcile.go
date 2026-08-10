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
	"log"
	"time"
)

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
// when count > 0; quiet at zero, matching the dashboard alarm at admin.go
// (which suppresses at 0). Log-only today: the _notify_owners channel is a
// known deferred gap (TODO(M3.1)); swap this log.Printf for the owner-alert
// call when that lands.
//
// Read-only: the only call is reconcileReport (a single SELECT). No settle,
// no deliver, no ClaimOrder, no CreditBalance — re-driving a paid order
// moves real funds (reconcile_stuck_orders.py:25-26), so this pass may only
// surface, never act.
func (b *Bot) reconcileStuckOrdersOnce(ctx context.Context) {
	// ctx is unused on this read-only path; mirror expirySweepOnce
	// (usdt.go:1310) which keeps the param for API symmetry with the worker
	// registry's ticker shape.
	_ = ctx

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
