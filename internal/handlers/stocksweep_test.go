package handlers

// stocksweep_test.go — pins the FOLDER-STOCK expiry janitor
// (stocksweep.go), the periodic caller that resolves task #26: before this,
// stock.SweepExpired had zero callers, so folder-stock expiry ran only
// lazily inside CountAvailable/Reserve/Hold/CountsSnapshot.
//
// Money-safety invariant under test: the stock sweep touches FOLDER STOCK
// ONLY (reserved/<order>/ .expiry -> back to active/). It does NOT touch DB
// orders, balances, or api_keys — those are the separate DB-order sweep in
// usdt.go's expirySweepOnce, which is already wired and untouched here.
//
// Fixture: newBuyBot wires a REAL *stock.Store (buy_test.go:23) at a temp
// accounts/ tree. seedProduct (buy_test.go:43) writes a product folder with
// active accounts; we then Reserve + backdate .expiry exactly as the stock
// package's own tests do (stock/stock_test.go:151-169).
//
// AUDIT FIX (task #14/#26 follow-up): every reclaim assertion in this file
// now reads the filesystem RAW (os.ReadDir/os.Stat) instead of through
// stock.Store.CountAvailable. CountAvailable sweeps expired reservations
// itself as a lazy side effect (stock.go:267-276, sweepExpiredLocked) before
// it counts — so a test that reads the result through CountAvailable
// performs the very reclaim it is trying to observe, and passes even if
// sweepStockExpiredOnce's body is emptied out. See activeTxtCount /
// reservedOrderDirGone below.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reserveAndBackdate reserves qty active accounts for orderID on product, then
// overwrites the .expiry marker with epoch 1 so the reservation is already
// expired — the same technique as stock/stock_test.go:158-160. Returns the
// path to the reserved order dir for assertions.
func reserveAndBackdate(t *testing.T, b *Bot, product, orderID string, qty int) string {
	t.Helper()
	users, err := b.stock.Reserve(product, qty, orderID, time.Minute)
	if err != nil || len(users) != qty {
		t.Fatalf("Reserve %s/%s: users=%v err=%v", product, orderID, users, err)
	}
	odir := filepath.Join(b.stock.AccountsDir, product, "reserved", orderID)
	if err := os.WriteFile(filepath.Join(odir, ".expiry"), []byte("1"), 0o644); err != nil {
		t.Fatalf("backdate .expiry %s: %v", odir, err)
	}
	return odir
}

// activeTxtCount counts *.txt credential files directly under
// AccountsDir/product/active/ via a raw os.ReadDir — the same filename rule
// stock.listTxt uses internally (unexported there, so duplicated here rather
// than exported just for tests).
//
// Deliberately NOT a call into *stock.Store: several of its methods
// (CountAvailable, Reserve, Hold) sweep expired reservations themselves as a
// lazy side effect (stock.go:267-276, sweepExpiredLocked) before doing their
// own work. Reading the count through one of those would let the
// assertion's OWN read perform the reclaim this file's tests exist to prove
// sweepStockExpiredOnce performs — the exact defect being fixed here (an
// earlier version of this file asserted through CountAvailable, and the
// assertion could not fail even with an empty sweepStockExpiredOnce body).
func activeTxtCount(t *testing.T, b *Bot, product string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.stock.AccountsDir, product, "active"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("ReadDir active/%s: %v", product, err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			n++
		}
	}
	return n
}

// reservedOrderDirGone reports whether
// AccountsDir/product/reserved/orderID no longer exists on disk — a raw
// os.Stat, same non-sweeping rationale as activeTxtCount above.
func reservedOrderDirGone(t *testing.T, b *Bot, product, orderID string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(b.stock.AccountsDir, product, "reserved", orderID))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		t.Fatalf("Stat reserved/%s/%s: %v", product, orderID, err)
	}
	return false
}

// TestSweepStockExpiredOnceReclaimsExpiredReservation is S6: one pass of the
// folder-stock janitor reclaims a reserved/<order>/ dir past its .expiry back
// to active/, so the credential files land back under active/ and the
// reserved order dir is removed. Before #26's fix this only happened lazily
// on a read of the product; now it happens on the periodic caller too.
//
// Both assertions read the filesystem RAW (activeTxtCount /
// reservedOrderDirGone), taken immediately after the sweep and BEFORE any
// stock.Store method runs — see this file's header comment for why: a
// CountAvailable-based assertion would sweep on its own read and could never
// distinguish "the janitor reclaimed it" from "my own assertion just did."
//
// Falsification: comment out the SweepExpired call inside
// sweepStockExpiredOnce and reserved/p1/ORDX still exists on disk, and
// active/p1 still has 0 .txt files, after this call returns.
func TestSweepStockExpiredOnceReclaimsExpiredReservation(t *testing.T) {
	b, _, _, _, dir := newBuyBot(t, "999")
	ctx := context.Background()

	// 2 active accounts on p1, then reserve both and backdate the expiry so
	// the reservation is already expired.
	seedProduct(t, dir, "p1", 50000, 2)
	reserveAndBackdate(t, b, "p1", "ORDX", 2)

	// Sanity check on the starting state, via the same raw read used below,
	// so a broken fixture can never be mistaken for a broken sweep: Reserve
	// moves both files OUT of active/ into reserved/ORDX/.
	if n := activeTxtCount(t, b, "p1"); n != 0 {
		t.Fatalf("active/p1 has %d .txt files before the sweep, want 0 (both "+
			"should be sitting in reserved/ORDX/)", n)
	}

	b.sweepStockExpiredOnce(ctx)

	if !reservedOrderDirGone(t, b, "p1", "ORDX") {
		t.Error("reserved/p1/ORDX still exists after the sweep — the periodic " +
			"folder-stock sweep did not reclaim the expired reservation")
	}
	if n := activeTxtCount(t, b, "p1"); n != 2 {
		t.Errorf("active/p1 has %d .txt files after the sweep, want 2 — the "+
			"expired reservation was not reclaimed to active/ by the periodic "+
			"folder-stock sweep", n)
	}
}

// TestSweepStockExpiredOnceHandlesMultipleProducts is S7: one pass reclaims
// expired reservations across MULTIPLE products. A per-product error must not
// stop the sweep (the swallow-and-continue posture), so asserting both
// products reclaim in one pass pins that the loop does not short-circuit.
// Same raw-filesystem rationale as S6 above.
func TestSweepStockExpiredOnceHandlesMultipleProducts(t *testing.T) {
	b, _, _, _, dir := newBuyBot(t, "999")
	ctx := context.Background()

	seedProduct(t, dir, "pa", 50000, 2)
	seedProduct(t, dir, "pb", 50000, 2)
	reserveAndBackdate(t, b, "pa", "ORD_A", 2)
	reserveAndBackdate(t, b, "pb", "ORD_B", 2)

	b.sweepStockExpiredOnce(ctx)

	for _, tc := range []struct{ product, order string }{
		{"pa", "ORD_A"},
		{"pb", "ORD_B"},
	} {
		if !reservedOrderDirGone(t, b, tc.product, tc.order) {
			t.Errorf("reserved/%s/%s still exists after the sweep — a "+
				"multi-product sweep did not reclaim this product's expired "+
				"reservation", tc.product, tc.order)
		}
		if n := activeTxtCount(t, b, tc.product); n != 2 {
			t.Errorf("active/%s has %d .txt files after the sweep, want 2 — a "+
				"multi-product sweep did not reclaim this product's expired "+
				"reservation", tc.product, n)
		}
	}
}

// TestStartStockSweepDrainsOnShutdown is S8: with stockSweepInterval shrunk
// to 1ms, StartStockSweep actually ticks and Shutdown drains it without
// hanging. Mirrors T5: the deterministic half asserts the once-function
// reclaims after a tick (raw filesystem read, same rationale as S6/S7); the
// shutdown half asserts the registry WaitGroup returns — that half is
// unchanged, it was already genuinely load-bearing (it catches a dropped
// <-stop/<-ctx.Done() case).
func TestStartStockSweepDrainsOnShutdown(t *testing.T) {
	b, _, _, _, dir := newBuyBot(t, "999")
	ctx := context.Background()

	seedProduct(t, dir, "p1", 50000, 2)
	reserveAndBackdate(t, b, "p1", "ORDX", 2)

	// Shrink the interval so the ticker fires promptly; the once-function is
	// asserted directly below (the goroutine is a thin wrapper around it).
	b.stockSweepInterval = time.Millisecond
	b.StartStockSweep(ctx)

	// One tick is enough for the periodic sweep to reclaim. Then prove the
	// reclaim happened (the body the goroutine runs is correct), via the
	// same raw filesystem reads S6/S7 use — not CountAvailable, which would
	// sweep on its own read and mask a broken ticker body.
	time.Sleep(20 * time.Millisecond)
	if !reservedOrderDirGone(t, b, "p1", "ORDX") {
		t.Error("reserved/p1/ORDX still exists after a tick — the periodic " +
			"sweep did not reclaim the expired reservation within a tick")
	}
	if n := activeTxtCount(t, b, "p1"); n != 2 {
		t.Errorf("active/p1 has %d .txt files after a tick, want 2 — the "+
			"periodic sweep did not reclaim the expired reservation within a tick", n)
	}

	// Shutdown must drain the goroutine without hanging. The workerRegistry
	// closes its stop chan; the loop's select returns on <-stop. If the loop
	// ignored stop this blocks forever and the test times out.
	done := make(chan struct{})
	go func() {
		b.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		// drained cleanly
	case <-time.After(5 * time.Second):
		t.Fatal("bot.Shutdown did not drain the stock-sweep worker within 5s — " +
			"the ticker loop is not honoring the registry stop chan")
	}
}
