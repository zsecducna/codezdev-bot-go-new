package handlers

// auditprune_test.go — pins the audit-retention janitor (auditprune.go),
// the periodic caller that resolves task #14's remaining half:
// store.PruneAudit previously had zero callers, so the audit table grew
// forever. Mirrors stocksweep_test.go's structure and its lesson: every
// assertion here reads the row count via a DIRECT SQL query
// (tableRowCount, reconcile_test.go), never through any accessor that could
// itself mutate the table — the audit table has no lazy-sweep accessor the
// way stock.Store.CountAvailable does, but the discipline is kept anyway so
// a future reader does not have to re-derive why a raw count was chosen.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
)

// seedAuditRowBot inserts one audit row at an explicit ts into b's store,
// bypassing store.LogAction (which always stamps now()) — mirrors
// store/misc_test.go's seedAuditRow, duplicated here because that helper is
// unexported in a different package.
func seedAuditRowBot(t *testing.T, b *Bot, ts float64, target string) {
	t.Helper()
	if _, err := b.store.DB().Exec(
		"INSERT INTO audit(ts, actor_chat, actor_kind, action, target, outcome, detail) "+
			"VALUES (?,?,?,?,?,?,?)",
		ts, "1", "admin", "test_seed", target, "", ""); err != nil {
		t.Fatalf("seed audit row %q: %v", target, err)
	}
}

// TestPruneAuditOnceDeletesOldRows is the row-count-delta pin for
// pruneAuditOnce's default (env-unset) 180-day retention: a row far older
// than 180 days must be gone after one pass, a row well within it must
// survive. Both timestamps are seeded relative to b.now() rather than
// hard-coded epoch values, so the test does not depend on which fixed
// cutoff the 180-day default happens to land on relative to "today."
//
// Falsification: emptying pruneAuditOnce's body (or short-circuiting before
// the store.PruneAudit call) leaves the old row present, and the post-prune
// count assertion fails.
func TestPruneAuditOnceDeletesOldRows(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()

	oldTS := float64(b.now().Unix()) - 200*86400 // 200 days ago — older than the 180-day default
	newTS := float64(b.now().Unix()) - 5*86400   // 5 days ago — well within it
	seedAuditRowBot(t, b, oldTS, "old-row")
	seedAuditRowBot(t, b, newTS, "new-row")

	if n := tableRowCount(t, b, "audit"); n != 2 {
		t.Fatalf("pre-prune audit count = %d, want 2", n)
	}

	b.pruneAuditOnce(ctx)

	if n := tableRowCount(t, b, "audit"); n != 1 {
		t.Errorf("post-prune audit count = %d, want 1 — the 200-day-old row "+
			"must be gone under the 180-day default retention, the 5-day-old "+
			"row must survive", n)
	}
}

// TestPruneAuditOnceRaisesBelowFloorRetentionAndLogsIt is the load-bearing
// test for the handlers-layer floor (auditprune.go's auditRetentionFloorDays),
// which is DISTINCT from store.PruneAudit's own internal 30-day floor
// (internal/store/misc.go). A row 10 days old is seeded — inside BOTH
// floors' protection window — and AUDIT_RETENTION_DAYS is set to 1 via
// .env, simulating an operator typo.
//
// Because store.PruneAudit ALSO clamps to 30 days internally, the row would
// survive even if this file's own floor check were deleted — the store's
// clamp alone is sufficient to save it. That means "the row survived" alone
// CANNOT distinguish "the handlers floor fired" from "only the store's
// silent clamp fired." The log line is what distinguishes them:
// pruneAuditOnce logs "AUDIT_RETENTION_DAYS=1 is below the 30-day floor"
// ONLY when ITS OWN floor check runs — store.PruneAudit's clamp is silent
// by design (misc.go's own doc comment: no logger reference, no operator to
// explain itself to). This test therefore asserts BOTH: the row survives
// (belt-and-suspenders, matches the outcome an operator actually cares
// about) AND the specific log line appears (the assertion that actually
// pins THIS layer's floor, not the store's).
//
// Falsification: deleting the `if days < auditRetentionFloorDays { ... }`
// block in pruneAuditOnce leaves the row alive (the store's own clamp still
// saves it) but the log line never appears — the log assertion below fails
// even though a naive row-survival-only test would stay green.
func TestPruneAuditOnceRaisesBelowFloorRetentionAndLogsIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("AUDIT_RETENTION_DAYS=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	ctx := context.Background()

	tenDaysAgo := float64(b.now().Unix()) - 10*86400
	seedAuditRowBot(t, b, tenDaysAgo, "recent-but-within-floor")

	out := captureLog(t, func() { b.pruneAuditOnce(ctx) })

	if n := tableRowCount(t, b, "audit"); n != 1 {
		t.Errorf("post-prune audit count = %d, want 1 — a config typo requesting "+
			"1-day retention must not be able to delete a 10-day-old row", n)
	}
	if !strings.Contains(out, "AUDIT_RETENTION_DAYS=1") || !strings.Contains(out, "30-day floor") {
		t.Errorf("pruneAuditOnce logged %q, want it to name the below-floor "+
			"configured value AND the floor it was raised to — this is the "+
			"ONLY signal that the HANDLERS-layer floor fired, as opposed to "+
			"store.PruneAudit's own silent internal clamp", out)
	}
}

// TestPruneAuditOnceResolvesRetentionFromDotEnvSnapshot is the same class of
// fix task #23 closed for TRON_API_KEY (tron_apikey_config_test.go), applied
// to AUDIT_RETENTION_DAYS: config.Load merges .env into a frozen snapshot
// that is NEVER copied back into the real process environment, so a raw
// os.Getenv read in production would be blind to a retention value supplied
// only via .env (production's systemd unit exports nothing else). The
// fixture asserts its own premise first — the key really is absent from the
// ambient environment — exactly like tron_apikey_config_test.go's own
// os.LookupEnv guard, so this test cannot pass "by accident" if the ambient
// shell happens to already export the variable.
//
// AUDIT_RETENTION_DAYS=45 via .env; a row 100 days old is seeded. Under
// correct (b.envInt) resolution, 100 days is older than 45 -> deleted.
// Under a raw-os.Getenv mutation, the ambient-blind read falls back to the
// 180-day default -> 100 days is within that -> the row survives instead.
//
// Falsification: replacing b.envInt("AUDIT_RETENTION_DAYS", ...) with a raw
// os.Getenv-based read makes the 100-day-old row survive the prune (it
// resolves to the 180-day default instead of the .env-configured 45), and
// the post-prune count assertion fails.
func TestPruneAuditOnceResolvesRetentionFromDotEnvSnapshot(t *testing.T) {
	if _, ok := os.LookupEnv("AUDIT_RETENTION_DAYS"); ok {
		t.Fatal("fixture broken: AUDIT_RETENTION_DAYS is set in the ambient " +
			"environment, so this test would pass even if pruneAuditOnce read raw env")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("AUDIT_RETENTION_DAYS=45\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	ctx := context.Background()

	hundredDaysAgo := float64(b.now().Unix()) - 100*86400
	seedAuditRowBot(t, b, hundredDaysAgo, "hundred-days-old")

	b.pruneAuditOnce(ctx)

	if n := tableRowCount(t, b, "audit"); n != 0 {
		t.Errorf("post-prune audit count = %d, want 0 — a 100-day-old row must "+
			"be pruned under the .env-configured 45-day retention; it surviving "+
			"means the resolution fell back to the ambient-blind 180-day default", n)
	}
}

// TestStartAuditPruneDrainsOnShutdown mirrors TestStartStockSweepDrainsOnShutdown
// / TestStartReconcileStuckOrdersDrainsOnShutdown: with auditPruneInterval
// shrunk to 1ms, StartAuditPrune actually ticks and Shutdown drains it
// without hanging.
//
// Falsification: dropping the `<-stop` (or `<-ctx.Done()`) case from the
// ticker's select makes Shutdown() never return via that path, and this
// test times out at 5s instead of observing a clean drain.
func TestStartAuditPruneDrainsOnShutdown(t *testing.T) {
	b, _, _, _, _ := newBuyBot(t, "999")
	ctx := context.Background()

	oldTS := float64(b.now().Unix()) - 200*86400
	seedAuditRowBot(t, b, oldTS, "old-row")

	b.auditPruneInterval = time.Millisecond
	b.StartAuditPrune(ctx)

	// One tick is enough for the periodic prune to run. Then prove it
	// happened (the body the goroutine runs is correct).
	time.Sleep(20 * time.Millisecond)
	if n := tableRowCount(t, b, "audit"); n != 0 {
		t.Errorf("post-tick audit count = %d, want 0 — the periodic prune did "+
			"not remove the old row within a tick", n)
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
		t.Fatal("bot.Shutdown did not drain the audit-prune worker within 5s — " +
			"the ticker loop is not honoring the registry stop chan")
	}
}
