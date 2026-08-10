package main

import (
	"database/sql"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// statMTime returns the file's modification time as an int64 (unix nanos)
// so tests can assert a dry run touched nothing on disk, without depending
// on any particular os.FileInfo type across platforms.
func statMTime(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.ModTime().UnixNano(), nil
}

// ---- fixture helpers ---------------------------------------------------

// newFixtureDB creates a fresh store.db-shaped SQLite file (only the tables
// and columns this tool touches, plus one non-money column (orders.qty) to
// exercise the "leave everything else alone" requirement) and returns its
// path. The setup connection is closed before returning, so the tool under
// test gets a clean file to open itself.
func newFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()

	stmts := []string{
		"CREATE TABLE balances (chat_id TEXT PRIMARY KEY, amount INTEGER NOT NULL DEFAULT 0)",
		"CREATE TABLE processed (txn_id TEXT PRIMARY KEY, chat_id TEXT, amount INTEGER, ts REAL)",
		"CREATE TABLE orders (des TEXT PRIMARY KEY, chat_id TEXT, product TEXT, tier TEXT, " +
			"qty INTEGER, total INTEGER, status TEXT, list_total INTEGER, code_discount INTEGER, ts REAL)",
		"CREATE TABLE bep20_pending (des TEXT PRIMARY KEY, chat_id TEXT, amount TEXT, total INTEGER, ts REAL)",
		"CREATE TABLE tron_pending (des TEXT PRIMARY KEY, chat_id TEXT, amount TEXT, total INTEGER, ts REAL)",
		"CREATE TABLE refund_requests (id INTEGER PRIMARY KEY AUTOINCREMENT, des TEXT, chat_id TEXT, total INTEGER, ts REAL)",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create fixture schema: %v: %s", err, s)
		}
	}
	return path
}

// seedKnownValues populates the fixture with the exact rows the golden
// end-to-end tests assert against. All money values are chosen so the
// intended rounding behaviour (exact vs. cent-rounded-down) is unambiguous
// at rate=25000, where 1_000_000/25000 == 40 exactly.
func seedKnownValues(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	// balances: a positive value matching the plan's own worked example
	// (§6.1: 125,000 VND -> 5,000,000 micro = 5.00 USDT), a negative
	// balance (rounding must stay well-defined for it too), and a zero
	// balance (must stay zero, not flagged as a collapse).
	exec("INSERT INTO balances(chat_id, amount) VALUES (?,?)", "111", 125000)
	exec("INSERT INTO balances(chat_id, amount) VALUES (?,?)", "222", -50000)
	exec("INSERT INTO balances(chat_id, amount) VALUES (?,?)", "333", 0)

	exec("INSERT INTO processed(txn_id, chat_id, amount, ts) VALUES (?,?,?,?)",
		"txn1", "111", 125000, 1000.0)

	// ord1: NULL list_total/code_discount (the current, no-writer-yet
	// state for every real row today) — must stay NULL.
	exec("INSERT INTO orders(des, chat_id, product, tier, qty, total, status, list_total, code_discount, ts) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		"ord1", "111", "prod", "t1", 3, 125000, "paid", nil, nil, 1000.0)
	// ord2: list_total/code_discount populated, both multiples of 250 so
	// every rounding step in this row is exact — isolates the NULL-vs-not
	// behaviour from the cent-rounding behaviour, which ord3 covers.
	exec("INSERT INTO orders(des, chat_id, product, tier, qty, total, status, list_total, code_discount, ts) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		"ord2", "111", "prod", "t2", 1, 300000, "delivered", 350000, 50000, 1001.0)
	// ord3: total=100100 is NOT a multiple of 250, so its raw micro value
	// (4,004,000) is NOT a multiple of 10,000 and the §2.2 cent-rounding
	// step actually has to round something (down, to 4,000,000).
	exec("INSERT INTO orders(des, chat_id, product, tier, qty, total, status, list_total, code_discount, ts) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		"ord3", "222", "prod", "t1", 2, 100100, "reserved", nil, nil, 1002.0)

	// refund_requests.total mirrors ord1's total exactly — B9: a refund
	// must credit back exactly the original charge.
	exec("INSERT INTO refund_requests(des, chat_id, total, ts) VALUES (?,?,?,?)",
		"ord1", "111", 125000, 1000.0)
}

func queryNullableInt64(t *testing.T, path, q string, args ...any) sql.NullInt64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var v sql.NullInt64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func queryInt64(t *testing.T, path, q string, args ...any) int64 {
	t.Helper()
	v := queryNullableInt64(t, path, q, args...)
	if !v.Valid {
		t.Fatalf("query %q: got NULL, want a value", q)
	}
	return v.Int64
}

// ---- pure math ----------------------------------------------------------

func TestRoundDivHalfUp(t *testing.T) {
	cases := []struct {
		num, den, want int64
	}{
		{5, 2, 3},   // 2.5 -> 3 (half rounds up)
		{-5, 2, -3}, // -2.5 -> -3 (half rounds away from zero)
		{4, 2, 2},   // exact
		{3, 2, 2},   // 1.5 -> 2
		{1, 2, 1},   // 0.5 -> 1
		{-1, 2, -1}, // -0.5 -> -1
		{0, 2, 0},
	}
	for _, c := range cases {
		got := roundDivHalfUp(big.NewInt(c.num), big.NewInt(c.den)).Int64()
		if got != c.want {
			t.Errorf("roundDivHalfUp(%d,%d) = %d, want %d", c.num, c.den, got, c.want)
		}
	}
}

func TestVndToMicroWorkedExample(t *testing.T) {
	// Plan §6.1's own worked example: 125_000 VND -> 5_000_000 micro at
	// R=25000.
	if got := vndToMicro(125000, 25000); got != 5_000_000 {
		t.Errorf("vndToMicro(125000, 25000) = %d, want 5000000", got)
	}
	if got := vndToMicro(-50000, 25000); got != -2_000_000 {
		t.Errorf("vndToMicro(-50000, 25000) = %d, want -2000000", got)
	}
}

func TestRoundToNearestCent(t *testing.T) {
	if got := roundToNearestCent(4_004_000); got != 4_000_000 {
		t.Errorf("roundToNearestCent(4004000) = %d, want 4000000 (400.4 cents rounds down to 400)", got)
	}
	if got := roundToNearestCent(4_005_000); got != 4_010_000 {
		t.Errorf("roundToNearestCent(4005000) = %d, want 4010000 (400.5 cents rounds up to 401)", got)
	}
	if got := roundToNearestCent(5_000_000); got != 5_000_000 {
		t.Errorf("roundToNearestCent(5000000) = %d, want unchanged (already exact)", got)
	}
}

// ---- end-to-end: the fixture converts to exactly the expected figures ---

func TestApplyConvertsFixtureToExactFigures(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	report, err := RunApply(path, 25000)
	if err != nil {
		t.Fatalf("RunApply: %v", err)
	}
	if report.Mode != "apply" {
		t.Errorf("report.Mode = %q, want apply", report.Mode)
	}

	// balances: free-micro, exact at this rate.
	if got := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'"); got != 5_000_000 {
		t.Errorf("balances[111].amount = %d, want 5000000", got)
	}
	if got := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='222'"); got != -2_000_000 {
		t.Errorf("balances[222].amount = %d, want -2000000", got)
	}
	if got := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='333'"); got != 0 {
		t.Errorf("balances[333].amount = %d, want 0", got)
	}
	// provenance: original VND retained exactly.
	if got := queryInt64(t, path, "SELECT legacy_vnd_amount FROM balances WHERE chat_id='111'"); got != 125000 {
		t.Errorf("balances[111].legacy_vnd_amount = %d, want 125000", got)
	}

	if got := queryInt64(t, path, "SELECT amount FROM processed WHERE txn_id='txn1'"); got != 5_000_000 {
		t.Errorf("processed[txn1].amount = %d, want 5000000", got)
	}

	// orders: cent-rounded.
	if got := queryInt64(t, path, "SELECT total FROM orders WHERE des='ord1'"); got != 5_000_000 {
		t.Errorf("orders[ord1].total = %d, want 5000000", got)
	}
	if got := queryInt64(t, path, "SELECT total FROM orders WHERE des='ord2'"); got != 12_000_000 {
		t.Errorf("orders[ord2].total = %d, want 12000000", got)
	}
	if got := queryInt64(t, path, "SELECT list_total FROM orders WHERE des='ord2'"); got != 14_000_000 {
		t.Errorf("orders[ord2].list_total = %d, want 14000000", got)
	}
	if got := queryInt64(t, path, "SELECT code_discount FROM orders WHERE des='ord2'"); got != 2_000_000 {
		t.Errorf("orders[ord2].code_discount = %d, want 2000000", got)
	}
	// ord3: the row that actually exercises the cent-rounding-down step.
	if got := queryInt64(t, path, "SELECT total FROM orders WHERE des='ord3'"); got != 4_000_000 {
		t.Errorf("orders[ord3].total = %d, want 4000000 (100100 VND rounds down to $4.00)", got)
	}
	if got := queryInt64(t, path, "SELECT legacy_vnd_total FROM orders WHERE des='ord3'"); got != 100100 {
		t.Errorf("orders[ord3].legacy_vnd_total = %d, want 100100", got)
	}

	// B9: the refund snapshot converts identically to the order it mirrors.
	orderTotal := queryInt64(t, path, "SELECT total FROM orders WHERE des='ord1'")
	refundTotal := queryInt64(t, path, "SELECT total FROM refund_requests WHERE des='ord1'")
	if orderTotal != refundTotal {
		t.Errorf("orders[ord1].total = %d != refund_requests[ord1].total = %d (B9 violated)", orderTotal, refundTotal)
	}

	// marker.
	var unit string
	db, _ := sql.Open("sqlite", "file:"+path)
	defer db.Close()
	if err := db.QueryRow("SELECT value FROM meta WHERE key='ledger_unit'").Scan(&unit); err != nil {
		t.Fatalf("read meta.ledger_unit: %v", err)
	}
	if unit != "micro_usdt" {
		t.Errorf("meta.ledger_unit = %q, want micro_usdt", unit)
	}
	var rateStr string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='migration_rate'").Scan(&rateStr); err != nil {
		t.Fatalf("read meta.migration_rate: %v", err)
	}
	if rateStr != "25000" {
		t.Errorf("meta.migration_rate = %q, want 25000", rateStr)
	}

	// reconciliation: every column must be within its own tolerance.
	for _, c := range report.Columns {
		if c.Delta > c.Tolerance {
			t.Errorf("%s.%s: delta %d exceeds tolerance %d (sum_before=%d sum_after=%d expected=%d)",
				c.Table, c.Column, c.Delta, c.Tolerance, c.SumBefore, c.SumAfter, c.ExpectedSumAfter)
		}
	}
}

// TestNullStaysNull is PINNED: a NULL money value must never become 0.
func TestNullStaysNull(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	if _, err := RunApply(path, 25000); err != nil {
		t.Fatalf("RunApply: %v", err)
	}

	lt := queryNullableInt64(t, path, "SELECT list_total FROM orders WHERE des='ord1'")
	if lt.Valid {
		t.Errorf("orders[ord1].list_total = %d, want NULL", lt.Int64)
	}
	cd := queryNullableInt64(t, path, "SELECT code_discount FROM orders WHERE des='ord1'")
	if cd.Valid {
		t.Errorf("orders[ord1].code_discount = %d, want NULL", cd.Int64)
	}
	// The legacy/provenance column for a NULL row must also stay NULL —
	// there is no original value to retain for a row that was never
	// written.
	legacy := queryNullableInt64(t, path, "SELECT legacy_vnd_list_total FROM orders WHERE des='ord1'")
	if legacy.Valid {
		t.Errorf("orders[ord1].legacy_vnd_list_total = %d, want NULL", legacy.Int64)
	}
}

// TestNonMoneyColumnUntouched is PINNED: orders.qty is not in baseSpecs and
// must be byte-for-byte unchanged after a conversion.
func TestNonMoneyColumnUntouched(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	if _, err := RunApply(path, 25000); err != nil {
		t.Fatalf("RunApply: %v", err)
	}
	if got := queryInt64(t, path, "SELECT qty FROM orders WHERE des='ord1'"); got != 3 {
		t.Errorf("orders[ord1].qty = %d, want 3 (unchanged)", got)
	}
	if got := queryInt64(t, path, "SELECT qty FROM orders WHERE des='ord2'"); got != 1 {
		t.Errorf("orders[ord2].qty = %d, want 1 (unchanged)", got)
	}
}

// TestIdempotenceGuardRefusesSecondRun is PINNED: running the migration
// twice must not double-convert, and the specific mechanism under test is
// the typed ErrAlreadyMigrated guard — not merely "some error happened" or
// "values happen to be unchanged" (the transaction's atomicity would mask a
// missing guard on that second front — see the guard's own comment in
// migrate.go for how the mutation-verification pass exercised this).
func TestIdempotenceGuardRefusesSecondRun(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	if _, err := RunApply(path, 25000); err != nil {
		t.Fatalf("first RunApply: %v", err)
	}
	firstBal := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'")

	_, err := RunApply(path, 25000)
	if err == nil {
		t.Fatal("second RunApply: got nil error, want ErrAlreadyMigrated")
	}
	if !errors.Is(err, ErrAlreadyMigrated) {
		t.Errorf("second RunApply error = %v, want errors.Is(err, ErrAlreadyMigrated)", err)
	}

	secondBal := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'")
	if secondBal != firstBal {
		t.Errorf("balance changed across the refused second run: %d -> %d", firstBal, secondBal)
	}
}

// TestRollbackOnMidMigrationError is PINNED, and is also the target of the
// "drop the transaction" mutation check: it injects a real failure
// mid-transaction (a spec pointed at a non-numeric TEXT column, which
// passes the schema-presence check but fails to scan as an amount) AFTER at
// least one earlier spec (balances) has already been converted within the
// same transaction, and asserts NOTHING persisted — not the earlier
// column's conversion, not the ALTER TABLE that added its legacy column,
// not the meta table itself.
func TestRollbackOnMidMigrationError(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	before := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'")
	if before != 125000 {
		t.Fatalf("fixture precondition: balances[111].amount = %d, want 125000", before)
	}

	broken := []moneySpec{
		baseSpecs[0], // balances.amount — would succeed if reached alone
		{"orders", "des", "status", "legacy_vnd_test_inject_fail", bucketFreeMicro}, // status is TEXT ("paid" etc.) — fails to scan as int64
	}
	if _, err := runApply(path, 25000, broken); err == nil {
		t.Fatal("runApply with an injected bad spec: got nil error, want a scan failure")
	}

	after := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'")
	if after != before {
		t.Errorf("balances[111].amount = %d after a rolled-back apply, want unchanged %d — "+
			"the earlier spec's conversion was not rolled back", after, before)
	}
	// The meta table (and therefore the marker) must not exist at all —
	// CREATE TABLE is transactional DDL in SQLite and must roll back too.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'").Scan(&n); err != nil {
		t.Fatalf("check meta table: %v", err)
	}
	if n != 0 {
		t.Error("meta table exists after a rolled-back apply — CREATE TABLE was not rolled back")
	}
	// The legacy column ALTER for the FIRST (successful-if-reached) spec
	// must also have been rolled back.
	cols, err := tableColumnsQ(db, "balances")
	if err != nil {
		t.Fatalf("table_info balances: %v", err)
	}
	if cols["legacy_vnd_amount"] {
		t.Error("balances.legacy_vnd_amount exists after a rolled-back apply — ALTER TABLE was not rolled back")
	}
}

// TestRequiredSchemaMissingColumnRefuses is requirement #7: refuse rather
// than convert a partial schema.
func TestRequiredSchemaMissingColumnRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Deliberately omit orders.code_discount, as if this were an older
	// schema that predates that column.
	stmts := []string{
		"CREATE TABLE balances (chat_id TEXT PRIMARY KEY, amount INTEGER NOT NULL DEFAULT 0)",
		"CREATE TABLE processed (txn_id TEXT PRIMARY KEY, chat_id TEXT, amount INTEGER, ts REAL)",
		"CREATE TABLE orders (des TEXT PRIMARY KEY, total INTEGER, list_total INTEGER)",
		"CREATE TABLE bep20_pending (des TEXT PRIMARY KEY, total INTEGER)",
		"CREATE TABLE tron_pending (des TEXT PRIMARY KEY, total INTEGER)",
		"CREATE TABLE refund_requests (id INTEGER PRIMARY KEY AUTOINCREMENT, total INTEGER)",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create partial schema: %v", err)
		}
	}
	db.Close()

	if _, err := RunApply(path, 25000); !errors.Is(err, ErrMissingSchema) {
		t.Errorf("RunApply on a partial schema: err = %v, want errors.Is(err, ErrMissingSchema)", err)
	}
	if _, err := RunDryRun(path, 25000); !errors.Is(err, ErrMissingSchema) {
		t.Errorf("RunDryRun on a partial schema: err = %v, want errors.Is(err, ErrMissingSchema)", err)
	}
}

// TestPendingNotEmptyRefusesApply is plan §1.G3's hard gate: a live
// in-flight on-chain order must block the migration outright.
func TestPendingNotEmptyRefusesApply(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO bep20_pending(des, chat_id, amount, total, ts) VALUES (?,?,?,?,?)",
		"pend1", "111", "5.00", 125000, 1000.0); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}
	db.Close()

	if _, err := RunApply(path, 25000); !errors.Is(err, ErrPendingNotEmpty) {
		t.Errorf("RunApply with a non-empty bep20_pending: err = %v, want errors.Is(err, ErrPendingNotEmpty)", err)
	}
	// Nothing should have been written.
	bal := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'")
	if bal != 125000 {
		t.Errorf("balances[111].amount = %d after a refused apply, want unchanged 125000", bal)
	}
}

// TestDryRunWritesNothing is requirement #6: a dry run must never write.
func TestDryRunWritesNothing(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)

	info1, err := statMTime(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	report, err := RunDryRun(path, 25000)
	if err != nil {
		t.Fatalf("RunDryRun: %v", err)
	}
	if report.Mode != "dry-run" {
		t.Errorf("report.Mode = %q, want dry-run", report.Mode)
	}
	// Values must read exactly as seeded — still VND.
	bal := queryInt64(t, path, "SELECT amount FROM balances WHERE chat_id='111'")
	if bal != 125000 {
		t.Errorf("balances[111].amount = %d after a dry run, want unchanged 125000", bal)
	}
	// No legacy column, no meta table.
	db, _ := sql.Open("sqlite", "file:"+path)
	defer db.Close()
	cols, err := tableColumnsQ(db, "balances")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	if cols["legacy_vnd_amount"] {
		t.Error("balances.legacy_vnd_amount exists after a dry run — dry run wrote to the schema")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'").Scan(&n); err != nil {
		t.Fatalf("check meta table: %v", err)
	}
	if n != 0 {
		t.Error("meta table exists after a dry run")
	}

	info2, err := statMTime(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info1 != info2 {
		t.Error("store.db mtime changed after a dry run")
	}
}

// TestAlreadyMigratedDryRunDoesNotDoubleReport: once migrated, a dry run
// must report that fact and stop, not compute a nonsense report by
// re-treating already-micro values as VND.
func TestAlreadyMigratedDryRunDoesNotDoubleReport(t *testing.T) {
	path := newFixtureDB(t)
	seedKnownValues(t, path)
	if _, err := RunApply(path, 25000); err != nil {
		t.Fatalf("RunApply: %v", err)
	}
	report, err := RunDryRun(path, 25000)
	if err != nil {
		t.Fatalf("RunDryRun after apply: %v", err)
	}
	if !report.AlreadyMigrated {
		t.Error("report.AlreadyMigrated = false, want true")
	}
	if len(report.Columns) != 0 {
		t.Errorf("report.Columns has %d entries after already-migrated short-circuit, want 0", len(report.Columns))
	}
}
