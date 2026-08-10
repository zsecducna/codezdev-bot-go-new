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
