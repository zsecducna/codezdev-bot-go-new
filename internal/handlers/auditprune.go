package handlers

// auditprune.go — the audit-table retention janitor, giving
// store.PruneAudit a periodic caller. Before this file, PruneAudit existed
// with zero callers — the identical defect that made stock.SweepExpired a
// task in the first place (see stocksweep.go, whose shape this file
// deliberately mirrors: same worker-registry ticker pattern, same
// panic-isolated safeStep, same stop/ctx.Done() select).
//
// INVARIANT: pruning the audit trail is an IRREVERSIBLE DELETE of the record
// an investigator reaches for AFTER an incident nobody flagged at the time
// (internal/store/misc.go's own doc comment on AuditRetentionFloor makes the
// same point at the storage layer — a disputed refund, a suspicious balance
// change, an address swap). Running this often buys nothing: the table
// grows by admin-action volume, not customer-request volume, so a tight
// cadence only adds needless DB-write churn. Running it with too SHORT a
// retention is the real risk — it destroys exactly the evidence the table
// exists to preserve, and the delete cannot be undone (no soft-delete, no
// archive-before-delete). Hence: a DAILY cadence (not minutes), and a SECOND
// floor enforced HERE, in the handlers layer, on top of store.PruneAudit's
// own floor (store.AuditRetentionFloor, misc.go) — belt-and-suspenders, not
// redundant. The store's clamp is the last line of defence against a bad
// cutoff reaching it by ANY path (a bug, a future second caller, a
// mis-wired test); this layer is the operator-facing one, and it refuses a
// bad CONFIGURED value outright and says so in the log, rather than
// silently relying on the data layer to catch it for it.

import (
	"context"
	"log"
	"time"

	"codezdev-bot/internal/store"
)

// auditRetentionDefaultDays is the retention window when AUDIT_RETENTION_DAYS
// is unset — 180 days (6 months). A real dispute over a refund, a balance
// change, or a broadcast surfaces on a timescale of weeks to a couple of
// months in a small shop-bot operation, not days; six months is generous
// headroom past that while still being a real bound, not "keep forever."
const auditRetentionDefaultDays = 180

// auditRetentionFloorDays is store.AuditRetentionFloor (internal/store/
// misc.go) expressed as a whole day count — a CONSTANT EXPRESSION derived
// from that exported symbol, not a second hand-typed "30". Two independent
// literals for the same invariant would drift the moment either is tuned
// without the other; deriving this one from the store's own constant makes
// that impossible.
//
// Re-enforced here as the operator-facing guard: an admin who configures
// AUDIT_RETENTION_DAYS below this gets it raised, WITH a log line saying so.
// The store's own clamp ALSO logs now (misc.go), but from inside the store,
// with no visibility into what the admin actually typed into
// AUDIT_RETENTION_DAYS — this layer is the one that can say "your setting
// was N, that's why," not just "a cutoff was overridden."
const auditRetentionFloorDays = int(store.AuditRetentionFloor / (24 * time.Hour))

// pruneAuditOnce is one pass of the audit-retention janitor: resolves the
// configured retention window, clamps it to the floor (logging if it had
// to), and deletes every audit row older than the resulting cutoff.
//
// AUDIT_RETENTION_DAYS is read via b.envInt — NOT os.Getenv. config.Load
// merges the ambient process environment with dir/.env into a frozen
// snapshot that is never written back to the real process environment
// (config.Load's own doc comment, internal/config/config.go), and
// production's systemd unit exports nothing beyond what .env supplies — so a
// raw os.Getenv read here would see an empty string in production even with
// AUDIT_RETENTION_DAYS correctly set in .env. That exact mistake (a raw env
// read blind to .env) produced five live defects earlier in this port
// (TRON_API_KEY, KIRO_LOGIN_PYTHON, and others — task #23's sweep). envInt
// (dispatch.go) resolves through b.cfg correctly.
func (b *Bot) pruneAuditOnce(ctx context.Context) {
	// ctx is unused on this store-only path; mirrors sweepStockExpiredOnce
	// (stocksweep.go) and reconcileStuckOrdersOnce (reconcile.go)'s identical
	// _ = ctx idiom — API symmetry with the worker registry's ticker shape.
	_ = ctx

	days := b.envInt("AUDIT_RETENTION_DAYS", auditRetentionDefaultDays)
	if days < auditRetentionFloorDays {
		// store.PruneAudit ALSO logs when its own clamp fires (misc.go), but
		// from inside the store it only sees a raw cutoff timestamp — it
		// cannot say "your AUDIT_RETENTION_DAYS was 1." This log line is the
		// one that names the actual configured value an admin typed, so a
		// typo (AUDIT_RETENTION_DAYS=1) is traceable back to its cause, not
		// just observable as "pruning was gentler than configured."
		log.Printf("audit prune: AUDIT_RETENTION_DAYS=%d is below the %d-day floor — using %d",
			days, auditRetentionFloorDays, auditRetentionFloorDays)
		days = auditRetentionFloorDays
	}
	cutoff := float64(b.now().Unix()) - float64(days)*86400

	deleted, err := b.store.PruneAudit(cutoff)
	if err != nil {
		// A failed prune is surfaced but never fatal, matching
		// reconcileStuckOrdersOnce's read-failure posture: the worker keeps
		// ticking, so a transient DB hiccup self-corrects on the next pass.
		log.Printf("audit prune: PruneAudit failed: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("audit prune: removed %d row(s) older than %d days", deleted, days)
	}
}

// StartAuditPrune runs the audit-retention janitor on a ticker under the
// worker registry, the same shape as StartStockSweep/
// StartReconcileStuckOrders. Resolves task #14's remaining half (the
// broadcast-audit half is a separate, already-landed change).
//
// Default 24h (auditPruneInterval, dispatch.go): pruning is an irreversible
// delete, and the table's growth rate does not justify a tighter cadence —
// see this file's header comment. The interval is a field so tests can
// shrink it to 1ms, matching stockSweepInterval/reconcileInterval.
func (b *Bot) StartAuditPrune(ctx context.Context) {
	b.workers.Go(func(stop <-chan struct{}) {
		for {
			// safeStep isolates a faulted pass so one bad read/delete never
			// stops the janitor — same panic guard as stocksweep.go /
			// reconcile.go (usdt.go:1137-1152).
			safeStep("audit prune", func() { b.pruneAuditOnce(ctx) })
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(b.auditPruneInterval):
			}
		}
	})
}
