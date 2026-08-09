package handlers

// stocksweep.go — the FOLDER-STOCK expiry janitor, giving stock.SweepExpired
// a periodic caller (task #26: SweepExpired had zero callers, so the
// folder-stock reclaim was entirely lazy before this). Ports:
//
//	accounts.py _sweep_expired  -> stock.SweepExpired (the locked reclaim)
//	(this file)                 -> the periodic driver over every product
//
// CRITICAL DISTINCTION — do not conflate the two "sweep" concepts:
//
//   - (*Bot).expirySweepOnce (usdt.go:1309) sweeps DB ORDERS reserved->expired
//     past the hard payment limit, via store.ClaimOrder + stock.Release. It is
//     ALREADY wired inside StartupRecovery's ticker. Do NOT duplicate it.
//
//   - stock.Store.SweepExpired (internal/stock/stock.go:254) sweeps FOLDER-
//     STOCK account files: reserved/<order>/ dirs whose .expiry passed are
//     reclaimed back to active/. Before this file it only ran LAZILY inside
//     CountAvailable/Reserve/Hold/CountsSnapshot — so a product nobody is
//     actively buying or holding leaked reserved slots forever. THIS file is
//     the periodic caller for that second, folder-stock layer.
//
// INVARIANT: this file touches FOLDER STOCK ONLY. It calls stock.SweepExpired
// (stock's own locked, idempotent reclaim) and nothing else — no DB writes,
// no order mutation, no balance change. The DB-order sweep is a separate
// concern living in usdt.go and is untouched here.

import (
	"context"
	"log"
	"time"
)

// sweepStockExpiredOnce is one pass of the folder-stock expiry janitor — it
// forces stock.SweepExpired across every product so reserved account files
// whose .expiry passed are reclaimed to active/ even for products nobody is
// actively buying (where the lazy sweep inside CountAvailable/Reserve/Hold
// would otherwise never run). This resolves task #26: SweepExpired had zero
// callers, so expiry was entirely lazy. Per-product under stock's own lock;
// errors per product are logged and skipped so one bad product never stops
// the sweep. This is the FOLDER-STOCK layer — distinct from expirySweepOnce
// (usdt.go:1309), which sweeps DB orders reserved→expired; do not conflate.
func (b *Bot) sweepStockExpiredOnce(ctx context.Context) {
	// ctx is unused on this folder-stock path; the param exists for API
	// symmetry with the worker registry's ticker shape (see reconcile.go's
	// reconcileStuckOrdersOnce and usdt.go:1310 for the same _ = ctx idiom).
	_ = ctx

	// ListProducts (catalog.go:29) returns the sorted product folders under
	// AccountsDir, excluding hidden dot-dirs. Empty/missing is a fresh
	// install — the loop is simply a no-op then.
	for _, product := range b.stock.ListProducts() {
		// SweepExpired acquires the product lock itself (stock.go:254-262),
		// so we do NOT hold any lock across the call — matching how the lazy
		// callers (CountAvailable etc.) invoke it. One product's failure is
		// logged and skipped so a single corrupt product tree can never stop
		// the rest of the sweep (the same swallow-and-continue posture the
		// lazy sweep inside CountsSnapshot adopts, stock.go:sweepExpired-
		// Locked).
		if err := b.stock.SweepExpired(product); err != nil {
			log.Printf("stock sweep: %s failed: %v", product, err)
		}
	}
}

// StartStockSweep runs the folder-stock expiry janitor on a ticker under the
// worker registry, the same shape as StartReconcileStuckOrders. Resolves #26.
//
// Default 300s (STOCK_SWEEP_INTERVAL_S): folder-stock expiry is cheaper than
// the DB-order sweep (a directory walk per product vs. a SQL scan plus
// ClaimOrder under the writer lock), and reclaiming abandoned reservations
// promptly matters for accurate stock counts — a stale reserved slot shows
// up to buyers as unavailable until the next reclaim, so the tighter loop
// keeps the count honest without burdening the DB path. The interval is a
// field so tests can shrink it to 1ms.
func (b *Bot) StartStockSweep(ctx context.Context) {
	b.workers.Go(func(stop <-chan struct{}) {
		for {
			// safeStep isolates a faulted pass so one panicking product tree
			// never stops the folder-stock janitor (usdt.go:1137-1152 is the
			// panic guard). The sweep's own per-product error swallow
			// (above) handles ordinary failures; safeStep handles the rest.
			safeStep("stock sweep", func() { b.sweepStockExpiredOnce(ctx) })
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(b.stockSweepInterval):
			}
		}
	})
}
