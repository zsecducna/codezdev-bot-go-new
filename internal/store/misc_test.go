// Tests for PruneAudit (task #14 — the audit table had no pruning path at
// all before this). Rows are seeded via a raw INSERT rather than LogAction
// because LogAction always stamps `now()` internally and these tests need
// explicit, controlled ts values on both sides of a cutoff.
package store

import "testing"

// seedAuditRow inserts one audit row at an explicit ts, bypassing LogAction's
// now()-only stamping. target carries a human label so a test can identify
// which seeded row survived without relying on ts alone.
func seedAuditRow(t *testing.T, s *Store, ts float64, target string) {
	t.Helper()
	if _, err := s.db.Exec(
		"INSERT INTO audit(ts, actor_chat, actor_kind, action, target, outcome, detail) "+
			"VALUES (?,?,?,?,?,?,?)",
		ts, "1", "admin", "test_seed", target, "", ""); err != nil {
		t.Fatalf("seed audit row %q: %v", target, err)
	}
}

// auditRowCount is a direct, unfiltered COUNT(*) — deliberately not a
// RecentAudit lookup. A lookup can miss the case where a row is altered
// (e.g. its ts rewritten) rather than removed; only a raw count proves rows
// actually left the table.
func auditRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// TestPruneAuditDeletesOnlyOlderRows is the row-count-delta pin for the
// `ts<?` WHERE clause itself: one row strictly before the cutoff, one row
// strictly after. Fixed, far-past epoch values (not time.Now()-relative)
// keep this test's cutoff comfortably past PruneAudit's own 30-day floor, so
// the floor clamp (covered separately by TestAuditRetentionFloorClamped)
// never engages here and cannot mask a break in the WHERE clause itself.
//
// Falsification: loosening, dropping, or inverting `ts<?` changes which row
// (if either) survives, and the row-count delta below stops matching.
func TestPruneAuditDeletesOnlyOlderRows(t *testing.T) {
	s := newTestStore(t)

	const (
		cutoff = 1_000_000_100.0
		oldTS  = 1_000_000_000.0 // strictly before cutoff -> must be deleted
		newTS  = 1_000_000_200.0 // strictly after cutoff -> must survive
	)
	seedAuditRow(t, s, oldTS, "old-row")
	seedAuditRow(t, s, newTS, "new-row")

	if n := auditRowCount(t, s); n != 2 {
		t.Fatalf("pre-prune count = %d, want 2", n)
	}

	deleted, err := s.PruneAudit(cutoff)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}
	if deleted != 1 {
		t.Errorf("PruneAudit returned %d, want 1 (only old-row)", deleted)
	}

	if n := auditRowCount(t, s); n != 1 {
		t.Fatalf("post-prune count = %d, want 1 — old-row must be gone, "+
			"new-row must survive", n)
	}

	rows, err := s.RecentAudit(10, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(rows) != 1 || rows[0].Target != "new-row" {
		t.Errorf("surviving row = %+v, want the single row target=new-row", rows)
	}
}

// TestPruneAuditBoundaryRowSurvives pins the boundary choice explicitly:
// PruneAudit uses `ts<?`, so a row whose ts EQUALS the cutoff is NOT older
// than the cutoff and must survive. This matches ReservedOrdersBefore's
// identical `ts<?` boundary elsewhere in this package (orders.go).
//
// Falsification: changing the query to `ts<=?` deletes this row too, and
// the post-prune count goes to 0 instead of staying at 1.
func TestPruneAuditBoundaryRowSurvives(t *testing.T) {
	s := newTestStore(t)

	const cutoff = 1_000_000_100.0
	seedAuditRow(t, s, cutoff, "boundary-row")

	deleted, err := s.PruneAudit(cutoff)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}
	if deleted != 0 {
		t.Errorf("PruneAudit returned %d, want 0 — the boundary row (ts==cutoff) "+
			"must not be counted as deleted", deleted)
	}
	if n := auditRowCount(t, s); n != 1 {
		t.Fatalf("post-prune count = %d, want 1 — the boundary row must survive "+
			"a cutoff that exactly equals its ts", n)
	}
}

// TestAuditRetentionFloorClamped is the test that matters most: it proves
// PruneAudit refuses to honor a cutoff more aggressive than its own 30-day
// floor, REGARDLESS of what the caller passed in — the defense-in-depth
// guard against a config typo (AUDIT_RETENTION_DAYS=1) self-destructing the
// audit trail through this one call site.
//
// A row 10 days old is seeded — inside the 30-day floor's protection window,
// but a cutoff simulating a 1-day retention request would delete it if the
// floor did not clamp that cutoff first. Asserting the row survives (and the
// returned count is 0) is only possible if the clamp actually fired.
//
// Falsification: removing the clamp lets the raw 1-day-retention cutoff
// through unmodified, the 10-day-old row is strictly older than it, and it
// gets deleted — this test goes red the moment the floor is removed.
func TestAuditRetentionFloorClamped(t *testing.T) {
	s := newTestStore(t)

	tenDaysAgo := now() - 10*24*3600
	seedAuditRow(t, s, tenDaysAgo, "recent-but-within-floor")

	// Simulates a caller that resolved AUDIT_RETENTION_DAYS=1 into a cutoff
	// of "now minus one day" — far more aggressive than the 30-day floor
	// this store enforces on its own.
	aggressiveCutoff := now() - 1*24*3600

	deleted, err := s.PruneAudit(aggressiveCutoff)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}
	if deleted != 0 {
		t.Errorf("PruneAudit returned %d, want 0 — the 30-day floor must have "+
			"clamped the cutoff before this 10-day-old row could be reached", deleted)
	}
	if n := auditRowCount(t, s); n != 1 {
		t.Fatalf("post-prune count = %d, want 1 — a config typo requesting "+
			"1-day retention must not be able to delete a 10-day-old row", n)
	}
}

// TestAuditRetentionFloorClampedExactValue pins the floor's EXACT value, not
// merely its existence. TestAuditRetentionFloorClamped above only proves the
// floor is greater than 10 days — it would stay green even if
// AuditRetentionFloor silently drifted from 30 days to 90 or 365, since a
// 10-day-old row survives under any of those. This test seeds one row 1 day
// YOUNGER than AuditRetentionFloor (must survive — it is still inside the
// window) and one row 1 day OLDER (must be deleted — it is just past the
// boundary), computed FROM the exported constant itself rather than a
// hand-typed "30", so the test tracks a deliberate future change to the
// constant while still catching the clamp math disagreeing with it (e.g. the
// comparison in PruneAudit using a different value than AuditRetentionFloor
// declares).
//
// Falsification: if PruneAudit's clamp used a floor even slightly
// different from AuditRetentionFloor (too short OR too long), one of these
// two rows would land on the wrong side and either the deleted-count or the
// surviving row's Target would be wrong.
func TestAuditRetentionFloorClampedExactValue(t *testing.T) {
	s := newTestStore(t)

	justInside := now() - AuditRetentionFloor.Seconds() + 86400  // 1 day younger than the floor
	justOutside := now() - AuditRetentionFloor.Seconds() - 86400 // 1 day older than the floor
	seedAuditRow(t, s, justInside, "just-inside")
	seedAuditRow(t, s, justOutside, "just-outside")

	// An aggressive cutoff (simulating a 1-day retention request) forces the
	// clamp to fire, so AuditRetentionFloor's own value — not the caller's
	// cutoff — decides both rows' fate.
	aggressiveCutoff := now() - 1*24*3600

	deleted, err := s.PruneAudit(aggressiveCutoff)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}
	if deleted != 1 {
		t.Errorf("PruneAudit returned %d, want 1 (only just-outside)", deleted)
	}
	if n := auditRowCount(t, s); n != 1 {
		t.Fatalf("post-prune count = %d, want 1", n)
	}
	rows, err := s.RecentAudit(10, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(rows) != 1 || rows[0].Target != "just-inside" {
		t.Errorf("surviving row = %+v, want the single row target=just-inside", rows)
	}
}

// TestPruneAuditFutureCutoffClampsToFloor is the maximum-deletion path: a
// cutoff in the FUTURE (past now()) is, unclamped, the most destructive
// input PruneAudit can ever receive — every row in the table has a ts less
// than a future timestamp, so an unclamped future cutoff would delete
// everything. This proves the floor still protects a recent row even then.
//
// Falsification: if the clamp only guarded against a "too-recent-but-still-
// past" cutoff and not a genuinely future one (e.g. a bug that compared
// olderThanTS against now() instead of floor), a future cutoff would slip
// through unclamped and within-floor would be deleted too.
func TestPruneAuditFutureCutoffClampsToFloor(t *testing.T) {
	s := newTestStore(t)

	beyondFloor := now() - AuditRetentionFloor.Seconds() - 86400 // older than the floor
	withinFloor := now() - AuditRetentionFloor.Seconds() + 86400 // newer than the floor
	seedAuditRow(t, s, beyondFloor, "beyond-floor")
	seedAuditRow(t, s, withinFloor, "within-floor")

	futureCutoff := now() + 10*24*3600 // ten days from now

	deleted, err := s.PruneAudit(futureCutoff)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}
	if deleted != 1 {
		t.Errorf("PruneAudit returned %d, want 1 (only beyond-floor) — a future "+
			"cutoff must still be clamped to the floor, not honored as-is", deleted)
	}
	if n := auditRowCount(t, s); n != 1 {
		t.Fatalf("post-prune count = %d, want 1 — a future cutoff must not be "+
			"able to delete a row within the retention floor", n)
	}
	rows, err := s.RecentAudit(10, "")
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(rows) != 1 || rows[0].Target != "within-floor" {
		t.Errorf("surviving row = %+v, want the single row target=within-floor", rows)
	}
}

// ---- RecentAuditCollapsed ---------------------------------------------
//
// Covers the flood-collapsing display path: the hands-free deposit watcher
// (usdtwatch.go) writes one audit row per tick for every payment still stuck
// unresolved, so a single stubborn transfer can leave hundreds of identical
// rows in the trail. RecentAuditCollapsed folds those down for the admin
// audit screen; RecentAudit itself is untouched (proven separately above).

// seedAuditRowFull inserts one audit row with an explicit action/target/
// outcome and ts, for tests that need to distinguish (action, target)
// groups or drive the free-text filter against a specific column.
func seedAuditRowFull(t *testing.T, s *Store, ts float64, action, target, outcome string) {
	t.Helper()
	if _, err := s.db.Exec(
		"INSERT INTO audit(ts, actor_chat, actor_kind, action, target, outcome, detail) "+
			"VALUES (?,?,?,?,?,?,?)",
		ts, "1", "admin", action, target, outcome, ""); err != nil {
		t.Fatalf("seed audit row action=%q target=%q: %v", action, target, err)
	}
}

// TestRecentAuditCollapsedFoldsRepeatedRow is the core flood scenario: one
// (action, target) pair logged 50 times (the same stuck payment re-flagged
// on every watcher tick) must collapse to a SINGLE entry whose Count carries
// the true total, not the raw row count leaking through uncollapsed.
//
// Falsification: if grouping were dropped (or grouped on the wrong columns),
// this would return 50 rows instead of 1, or Count would come back 0/1
// instead of 50. The TS assertion additionally pins WHICH row represents
// the group: the query picks MAX(id) (the newest row), so a regression to
// MIN(id) (oldest) would leave Count and the row count unchanged but flip
// TS to the oldest seeded value instead of the newest.
func TestRecentAuditCollapsedFoldsRepeatedRow(t *testing.T) {
	s := newTestStore(t)
	const seedCount = 50
	for i := range seedCount {
		seedAuditRowFull(t, s, 1_000_000_000.0+float64(i), "watch_ambiguous", "tx-stuck", "still unresolved")
	}

	rows, err := s.RecentAuditCollapsed(25, "")
	if err != nil {
		t.Fatalf("RecentAuditCollapsed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 collapsed entry for the single repeated group", len(rows))
	}
	if rows[0].Count != 50 {
		t.Errorf("Count = %d, want 50 (true total, not an approximation)", rows[0].Count)
	}
	if rows[0].Action != "watch_ambiguous" || rows[0].Target != "tx-stuck" {
		t.Errorf("collapsed row = %+v, want action=watch_ambiguous target=tx-stuck", rows[0])
	}
	wantTS := 1_000_000_000.0 + float64(seedCount-1) // the LAST (newest) seeded row
	if rows[0].TS != wantTS {
		t.Errorf("TS = %v, want %v (the newest row must represent the group, not the oldest)", rows[0].TS, wantTS)
	}
}

// TestRecentAuditCollapsedKeepsDistinctGroupsSeparate proves two different
// (action, target) pairs, each repeated, are never merged into one group —
// only rows that share BOTH columns collapse together.
func TestRecentAuditCollapsedKeepsDistinctGroupsSeparate(t *testing.T) {
	s := newTestStore(t)
	for i := range 3 {
		seedAuditRowFull(t, s, 1_000_000_000.0+float64(i), "watch_ambiguous", "tx-a", "")
	}
	for i := range 7 {
		seedAuditRowFull(t, s, 1_000_000_100.0+float64(i), "watch_quarantine", "tx-b", "")
	}

	rows, err := s.RecentAuditCollapsed(25, "")
	if err != nil {
		t.Fatalf("RecentAuditCollapsed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (distinct groups must not merge)", len(rows))
	}
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Action+"/"+r.Target] = r.Count
	}
	if counts["watch_ambiguous/tx-a"] != 3 {
		t.Errorf("watch_ambiguous/tx-a count = %d, want 3", counts["watch_ambiguous/tx-a"])
	}
	if counts["watch_quarantine/tx-b"] != 7 {
		t.Errorf("watch_quarantine/tx-b count = %d, want 7", counts["watch_quarantine/tx-b"])
	}
}

// TestRecentAuditCollapsedDistinguishesDifferentOutcomes pins the group-key
// choice against a real usdtwatch.go scenario (see the RecentAuditCollapsed
// doc comment): the SAME (action, target) — e.g. action="ambiguous",
// target=some ledger id — can legitimately carry two different outcomes
// across ticks: "credited nothing, quarantined" on a normal tick vs.
// "credited nothing, QUARANTINE WRITE FAILED" on a tick where the
// protective quarantine write itself failed (the exact case commit
// 139b3b0 exists to surface). Grouping on (action, target) alone would let
// the later routine-outcome row win MAX(id) and silently fold the
// write-failure row's occurrence into a happy-looking Count — hiding a
// live "this transfer is not protected" signal.
//
// Falsification: if the group key were narrowed back to just
// (action, target), this test would see 1 collapsed row instead of 2, and
// the write-failure outcome would be gone from the result entirely.
func TestRecentAuditCollapsedDistinguishesDifferentOutcomes(t *testing.T) {
	s := newTestStore(t)
	seedAuditRowFull(t, s, 1_000_000_000.0, "ambiguous", "ledger-1", "credited nothing, QUARANTINE WRITE FAILED")
	seedAuditRowFull(t, s, 1_000_000_100.0, "ambiguous", "ledger-1", "credited nothing, quarantined")

	rows, err := s.RecentAuditCollapsed(25, "")
	if err != nil {
		t.Fatalf("RecentAuditCollapsed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — same (action, target) but different "+
			"outcome must NOT collapse into one entry", len(rows))
	}
	byOutcome := map[string]int{}
	for _, r := range rows {
		if r.Action != "ambiguous" || r.Target != "ledger-1" {
			t.Errorf("unexpected row %+v", r)
		}
		byOutcome[r.Outcome] = r.Count
	}
	if byOutcome["credited nothing, QUARANTINE WRITE FAILED"] != 1 {
		t.Errorf("write-failure outcome missing or wrong count: %+v", byOutcome)
	}
	if byOutcome["credited nothing, quarantined"] != 1 {
		t.Errorf("routine outcome missing or wrong count: %+v", byOutcome)
	}
}

// TestRecentAuditCollapsedFilterAppliesBeforeGrouping pins the ordering the
// task requires: the free-text filter must narrow the ROW set first, and
// grouping/picking-the-latest must run on what's left — not the reverse
// (group everything, pick a representative row, then filter on it).
//
// Group (action=A, target=T) has an OLD row whose outcome matches the
// filter ("apple") and four newer rows that do NOT ("banana"). If filtering
// ran after grouping, the group's representative (latest) row would be a
// "banana" row, it wouldn't match the "apple" filter, and the whole group
// would vanish from a filtered query. Filtering the rows first means only
// the "apple" row is ever a grouping candidate, so the group survives with
// that row as its (only, and therefore latest) representative.
func TestRecentAuditCollapsedFilterAppliesBeforeGrouping(t *testing.T) {
	s := newTestStore(t)
	seedAuditRowFull(t, s, 1_000_000_000.0, "watch_ambiguous", "tx-a", "apple")
	for i := range 4 {
		seedAuditRowFull(t, s, 1_000_000_100.0+float64(i), "watch_ambiguous", "tx-a", "banana")
	}

	rows, err := s.RecentAuditCollapsed(25, "apple")
	if err != nil {
		t.Fatalf("RecentAuditCollapsed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — filtering must narrow to the single "+
			"apple row before grouping, not drop the whole group", len(rows))
	}
	if rows[0].Outcome != "apple" {
		t.Errorf("outcome = %q, want apple (the only row matching the filter)", rows[0].Outcome)
	}
	if rows[0].Count != 1 {
		t.Errorf("Count = %d, want 1 — only the apple row matches the filter, "+
			"the 4 banana rows must not be counted in", rows[0].Count)
	}
}

// TestRecentAuditCollapsedSingleOccurrenceCountIsOne is the regression
// anchor for the common case: an ordinary, never-repeated row must come
// back with Count exactly 1 (not 0), so the caller's ">1" check to suppress
// the count suffix behaves correctly.
func TestRecentAuditCollapsedSingleOccurrenceCountIsOne(t *testing.T) {
	s := newTestStore(t)
	seedAuditRowFull(t, s, 1_000_000_000.0, "delete_account", "user123", "ok")

	rows, err := s.RecentAuditCollapsed(25, "")
	if err != nil {
		t.Fatalf("RecentAuditCollapsed: %v", err)
	}
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("got %+v, want a single row with Count=1", rows)
	}
}

// TestRecentAuditCollapsedLimitCapsGroups constructs raw-row count (30)
// that would exceed a small limit (2) while distinct-group count (2) does
// not, and confirms `limit` caps GROUPS, not raw rows — both groups must
// still appear.
func TestRecentAuditCollapsedLimitCapsGroups(t *testing.T) {
	s := newTestStore(t)
	for i := range 15 {
		seedAuditRowFull(t, s, 1_000_000_000.0+float64(i), "watch_ambiguous", "tx-a", "")
	}
	for i := range 15 {
		seedAuditRowFull(t, s, 1_000_001_000.0+float64(i), "watch_quarantine", "tx-b", "")
	}

	rows, err := s.RecentAuditCollapsed(2, "")
	if err != nil {
		t.Fatalf("RecentAuditCollapsed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — limit=2 caps at 2 GROUPS even though "+
			"30 raw rows exist", len(rows))
	}
	total := rows[0].Count + rows[1].Count
	if total != 30 {
		t.Errorf("combined Count = %d, want 30 (15+15 across both groups)", total)
	}
}
