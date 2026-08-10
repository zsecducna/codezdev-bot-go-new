// Command migrate-usdt is the S9 live-data step of the VND->USDT ledger
// migration (.omc/plans/usdt-migration.md §6). It is a separate, one-shot,
// OFFLINE tool — run with the bot process stopped — never a code path the
// bot itself executes. That separation matters: a unit change has no
// self-describing probe the way a structural CREATE TABLE IF NOT EXISTS
// does (a balance of 125000 is a valid number in EITHER unit), so this must
// live outside store.initSchema and be gated by an explicit marker instead
// (plan §6.2).
//
// This file holds the conversion logic; main.go is the CLI wrapper.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Sentinel errors — callers (and tests) distinguish these by errors.Is, not
// by string matching.
var (
	// ErrAlreadyMigrated means meta.ledger_unit already reads "micro_usdt".
	// Returned BEFORE any row is touched — a re-run is a refusal, not a
	// silent no-op (plan §6.2 point 2: running the conversion twice divides
	// every balance by the rate twice, with no error, unless something
	// stops it).
	ErrAlreadyMigrated = errors.New("migrate-usdt: database already migrated (meta.ledger_unit=micro_usdt) — refusing to run again")
	// ErrMissingSchema means a column this tool must convert (or the table
	// that holds it) does not exist. Refusing beats guessing: converting a
	// database with a partial/unexpected schema is exactly the "silently
	// wrong" failure mode this tool exists to avoid.
	ErrMissingSchema = errors.New("migrate-usdt: database is missing an expected money column")
	// ErrPendingNotEmpty means a *_pending table (bep20/tron/binance) still
	// has rows. Plan §1.G3: a checkout that straddles the cutover is
	// denominated in two units at once, so all pending tables must be
	// verified empty immediately before the live conversion runs.
	ErrPendingNotEmpty = errors.New("migrate-usdt: a *_pending table still has rows — settle or expire them before migrating (plan §1.G3)")
	// ErrDBMissing means the path passed to -db does not exist. Refusing
	// here (rather than letting the driver auto-create an empty file) is
	// what stops a typo'd path from "migrating" 0 rows and stamping the
	// marker on a brand-new, empty database.
	ErrDBMissing = errors.New("migrate-usdt: database file does not exist")
)

// bucket is the rounding rule a money column follows, per plan §2.2/§2.3.
type bucket int

const (
	// bucketFreeMicro: a single round-half-up VND->micro conversion, no
	// further rounding. Balances and processed deposit amounts are "free
	// micro" by design — an on-chain deposit arrives at whatever precision
	// the sender chose, and truncating it to a cent would silently pocket
	// customer money.
	bucketFreeMicro bucket = iota
	// bucketCentRounded: VND->micro, THEN rounded to the nearest whole cent
	// (10,000 micro). Every order-total-shaped column must satisfy
	// orders.total % 10_000 == 0 (plan §2.2) — that invariant is the single
	// most valuable design decision in the migration, because it makes
	// "charged amount disagrees with recorded total" unrepresentable.
	bucketCentRounded
)

func (b bucket) String() string {
	if b == bucketCentRounded {
		return "cent-rounded"
	}
	return "free-micro"
}

// microPerCent is 1 cent of USDT in micro-USDT units (1 USDT = 1_000_000
// micro, so 1 cent = 10_000 micro).
const microPerCent = 10_000

// moneySpec names one money column this tool converts: which table, its
// primary key (for the per-row UPDATE), the money column itself, the
// provenance column the original VND value is preserved into, and the
// rounding bucket it belongs to.
//
// This list IS the migration's scope. Anything not named here is left
// completely alone — that is how requirement "convert every column in
// §3.1's table, and NOTHING else" is enforced structurally rather than by
// discipline: a reviewer checks this slice, not the whole file.
type moneySpec struct {
	table     string
	pkCol     string
	valueCol  string
	legacyCol string
	bucket    bucket
}

// baseSpecs is every money column that exists in the CURRENT schema
// (internal/store/store.go, internal/refund/refund.go) as of this writing.
// binance_pending is handled separately by buildSpecs because plan §3.1
// marks that table "dropped" in a slice (S3) that may or may not have run
// yet — its presence is optional, everything below is not.
var baseSpecs = []moneySpec{
	// balances.amount: current spendable balance. Free-micro — this is a
	// live balance, not a quoted price (store.go:115-116).
	{"balances", "chat_id", "amount", "legacy_vnd_amount", bucketFreeMicro},
	// processed.amount: the credited-deposit record tied to a txn_id, the
	// deposit-idempotency anchor. Same character as a balance (an amount
	// actually received), so free-micro too (store.go:119-120).
	{"processed", "txn_id", "amount", "legacy_vnd_amount", bucketFreeMicro},
	// orders.total: the charged total. Cent-rounded — this is exactly the
	// column the §2.2 invariant is about (store.go:123-127).
	{"orders", "des", "total", "legacy_vnd_total", bucketCentRounded},
	// orders.list_total: pre-discount base price (unit x qty), nullable —
	// no writer exists yet as of this writing, so every row is NULL today.
	// Plan §3.1's own collision table requires converting this identically
	// to `total`, in the SAME migration pass, or "discounts stop
	// reconciling against totals" (store.go:215-221).
	{"orders", "des", "list_total", "legacy_vnd_list_total", bucketCentRounded},
	// orders.code_discount: promo/referral campaign cost, nullable, same
	// no-writer-yet caveat and same §3.1 instruction as list_total
	// (store.go:215-221).
	{"orders", "des", "code_discount", "legacy_vnd_code_discount", bucketCentRounded},
	// bep20_pending.total / tron_pending.total: the VND total an in-flight
	// on-chain order will credit on settlement — same shape as orders.total,
	// so cent-rounded. In practice these tables must be EMPTY before a live
	// run (plan §1.G3, enforced by pendingCounts + the gate in RunApply), so
	// this bucket choice only matters for dry-run reporting and tests.
	{"bep20_pending", "des", "total", "legacy_vnd_total", bucketCentRounded},
	{"tron_pending", "des", "total", "legacy_vnd_total", bucketCentRounded},
	// refund_requests.total: a snapshot of orders.total taken at request
	// time (refund.go:95 "snapshot of paid total (VND)"). Cent-rounding it
	// identically to orders.total is what keeps a refund's credited amount
	// exactly equal to the original charge (plan B9) — rounding it
	// independently could differ from the source order by up to half a
	// cent.
	{"refund_requests", "id", "total", "legacy_vnd_total", bucketCentRounded},
}

// buildSpecs returns baseSpecs plus binance_pending.total IF that table
// still exists. Plan §3.1 marks binance_pending as "table dropped" (slice
// S3), which is application-code work out of this tool's fence and may not
// have landed yet — so its presence is checked, never assumed either way.
func buildSpecs(q querier) ([]moneySpec, error) {
	specs := make([]moneySpec, len(baseSpecs))
	copy(specs, baseSpecs)
	exists, err := tableExistsQ(q, "binance_pending")
	if err != nil {
		return nil, fmt.Errorf("check binance_pending: %w", err)
	}
	if exists {
		specs = append(specs, moneySpec{"binance_pending", "des", "total", "legacy_vnd_total", bucketCentRounded})
	}
	return specs, nil
}

// pendingTables is every crypto pending table the §1.G3 gate applies to.
// binance_pending is added by pendingCounts only if present, same as
// buildSpecs.
var pendingTables = []string{"bep20_pending", "tron_pending"}

// querier is the subset of *sql.DB / *sql.Tx this file needs. Every helper
// below takes a querier so the SAME code path computes the dry-run report
// (querier = *sql.DB, read-only connection) and the live conversion
// (querier = *sql.Tx) — there is no second, hand-maintained "preview"
// implementation that could drift from what apply actually does.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// tableExistsQ reports whether `name` is a table in the database.
func tableExistsQ(q querier, name string) (bool, error) {
	var n int
	err := q.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&n)
	return n > 0, err
}

// tableColumnsQ returns the set of column names in `table`, via
// PRAGMA table_info — the same probe internal/store uses for its own
// idempotent column-add migrations (store.go:306-330). `table` is always one
// of this file's own constants, never external input, so string
// concatenation (PRAGMA does not accept bound parameters for object names)
// carries no injection risk.
func tableColumnsQ(q querier, table string) (map[string]bool, error) {
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// checkRequiredSchema refuses to proceed (dry-run OR apply) if any spec's
// table/pk/value column is missing. This is requirement #7: "refuse to run
// against a database that is missing an expected column, rather than
// converting a partial schema." A table that does not exist at all reads as
// "every one of its columns is missing" via the same PRAGMA table_info probe
// (an empty result set, not an error), so no special-casing is needed for
// that case.
func checkRequiredSchema(q querier, specs []moneySpec) error {
	var missing []string
	seen := map[string]bool{} // "table.column" already reported
	for _, spec := range specs {
		cols, err := tableColumnsQ(q, spec.table)
		if err != nil {
			return err
		}
		for _, col := range []string{spec.pkCol, spec.valueCol} {
			key := spec.table + "." + col
			if !cols[col] && !seen[key] {
				missing = append(missing, key)
				seen[key] = true
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %v", ErrMissingSchema, missing)
	}
	return nil
}

// pendingCounts returns the row count of every *_pending table this tool
// knows about (bep20/tron always, binance if present) — the data the §1.G3
// gate is checked against, and what the dry-run report surfaces so an
// operator can see whether the gate would currently pass.
func pendingCounts(q querier) (map[string]int64, error) {
	out := map[string]int64{}
	tables := append([]string{}, pendingTables...)
	hasBinance, err := tableExistsQ(q, "binance_pending")
	if err != nil {
		return nil, err
	}
	if hasBinance {
		tables = append(tables, "binance_pending")
	}
	for _, t := range tables {
		var n int64
		if err := q.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// alreadyMigrated reads the idempotence marker. A missing `meta` table (or a
// missing `ledger_unit` row) both mean "not migrated yet" — that is a normal
// state for a database that has never run this tool, not an error.
func alreadyMigrated(q querier) (bool, error) {
	exists, err := tableExistsQ(q, "meta")
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var v string
	err = q.QueryRow("SELECT value FROM meta WHERE key='ledger_unit'").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "micro_usdt", nil
}

// ---- exact-integer conversion math (plan §2.4: no float64 on a money path) ----

// roundDivHalfUp returns round(numerator/denominator) using round-half-away-
// from-zero, in exact big.Int arithmetic. denominator must be positive;
// numerator may be negative (a balance can in principle be negative — the
// rounding direction must still be well-defined for it).
func roundDivHalfUp(numerator, denominator *big.Int) *big.Int {
	neg := numerator.Sign() < 0
	n := new(big.Int).Abs(numerator)
	// round(n/d) for n,d >= 0 == floor((2n + d) / (2d)).
	twoN := new(big.Int).Lsh(n, 1)
	twoD := new(big.Int).Lsh(denominator, 1)
	q := new(big.Int).Add(twoN, denominator)
	q.Div(q, twoD)
	if neg {
		q.Neg(q)
	}
	return q
}

// vndToMicro converts a VND integer amount to micro-USDT at `rate`
// VND-per-USD: micro = round(vnd * 1_000_000 / rate), computed via big.Int
// so the intermediate vnd*1_000_000 product can never silently overflow
// int64 (plan §6.1). rate must be > 0 — callers validate this once, at the
// CLI boundary.
func vndToMicro(vnd, rate int64) int64 {
	num := new(big.Int).Mul(big.NewInt(vnd), big.NewInt(1_000_000))
	den := big.NewInt(rate)
	return roundDivHalfUp(num, den).Int64()
}

// roundToNearestCent rounds a micro-USDT amount to the nearest whole cent
// (10,000 micro). Applied AFTER vndToMicro, never before — rounding the VND
// input first would double-round and drift further from the true value.
func roundToNearestCent(micro int64) int64 {
	q := roundDivHalfUp(big.NewInt(micro), big.NewInt(microPerCent))
	return q.Int64() * microPerCent
}

// convertValue applies a spec's bucket rule to one VND value.
func convertValue(vnd, rate int64, b bucket) int64 {
	micro := vndToMicro(vnd, rate)
	if b == bucketCentRounded {
		return roundToNearestCent(micro)
	}
	return micro
}

// perRowTolerance is the maximum reconciliation drift one row can
// contribute, per plan §6.6 check #2 (generalised from "row_count micro,
// one micro of rounding per row" to the cent-rounded bucket, which rounds
// twice and so can drift up to half a cent = 5,000 micro per row).
func perRowTolerance(b bucket) int64 {
	if b == bucketCentRounded {
		return microPerCent / 2
	}
	return 1
}

// ---- per-row work -----------------------------------------------------

// rowVal is one (primary key, pre-conversion value) pair. pk is `any`
// because the primary key type varies by table (TEXT for every table here
// except refund_requests.id, which is INTEGER) — scanning into a
// *interface{} lets database/sql hand back whatever concrete type the
// driver produced, which is also exactly what a later `WHERE pk = ?` needs
// as a bind argument.
type rowVal struct {
	pk  any
	old int64
}

// fetchRows reads every NON-NULL (pk, value) pair for one spec. The
// `WHERE valueCol IS NOT NULL` clause is the FIRST line of defense against
// ever touching a NULL row — NULL means "not recorded" (plan §3.1), never
// zero, and a NULL row must never be written to at all.
func fetchRows(q querier, spec moneySpec) ([]rowVal, error) {
	rows, err := q.Query(fmt.Sprintf(
		"SELECT %s, %s FROM %s WHERE %s IS NOT NULL",
		spec.pkCol, spec.valueCol, spec.table, spec.valueCol))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rowVal
	for rows.Next() {
		var pk any
		var v sql.NullInt64
		if err := rows.Scan(&pk, &v); err != nil {
			return nil, err
		}
		if !v.Valid {
			// ---- GUARD: NULL never becomes zero ---------------------------
			// Should be UNREACHABLE — the WHERE clause above already
			// excludes NULLs. Kept as a second, independent line of defense
			// so that if the SQL filter is ever weakened, this still
			// refuses to invent a value rather than silently writing 0 over
			// a real "not recorded" row (plan §3.1: NULL != 0, e.g. for
			// code_discount a NULL means "no discount was recorded", not
			// "zero discount", and collapsing the two would understate a
			// future discount-cost report).
			continue
			// ---- end guard --------------------------------------------
		}
		out = append(out, rowVal{pk, v.Int64})
	}
	return out, rows.Err()
}

// ensureLegacyColumn adds `col` to `table` as an idempotent
// ALTER TABLE ... ADD COLUMN, mirroring internal/store's own
// PRAGMA table_info-gated migration pattern (store.go:159-221). Reached only
// while running inside the single migration transaction (see RunApply), so
// in the normal flow the column will not already exist — the existence
// check is defensive, not load-bearing.
func ensureLegacyColumn(q querier, table, col string) error {
	cols, err := tableColumnsQ(q, table)
	if err != nil {
		return err
	}
	if cols[col] {
		return nil
	}
	_, err = q.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s INTEGER", table, col))
	return err
}

// ColumnReport is the dry-run/apply result for one money column — the same
// shape either way, because processSpecs is the same code path for both.
type ColumnReport struct {
	Table     string
	Column    string
	Bucket    bucket
	RowCount  int64
	SumBefore int64 // VND, pre-conversion
	SumAfter  int64 // micro-USDT, sum of the INDIVIDUALLY rounded per-row values

	// ExpectedSumAfter is the AGGREGATE conversion of SumBefore in one shot
	// (not a sum of per-row roundings) — the number §6.6 check #2 compares
	// SumAfter against. Delta is |SumAfter - ExpectedSumAfter|; the column
	// reconciles when Delta <= Tolerance (RowCount * perRowTolerance(Bucket)).
	ExpectedSumAfter int64
	Delta            int64
	Tolerance        int64

	// ZeroCollapses lists every row whose ORIGINAL value was non-zero but
	// whose converted value rounded to zero — requirement #6, "the class
	// that loses a customer real money", surfaced rather than silently
	// floored.
	ZeroCollapses []ZeroCollapse
}

// ZeroCollapse names one row that would round from non-zero to zero.
type ZeroCollapse struct {
	Table string
	PK    any
	Old   int64
}

// processSpecs is the ONE code path that both RunDryRun (q = read-only
// *sql.DB, write = false) and RunApply (q = *sql.Tx, write = true) call.
// Sharing it means the dry-run report is not a second, hand-maintained
// preview of what apply "should" do — it IS what apply does, minus the
// actual writes.
func processSpecs(q querier, specs []moneySpec, rate int64, write bool) ([]ColumnReport, error) {
	out := make([]ColumnReport, 0, len(specs))
	for _, spec := range specs {
		if write {
			if err := ensureLegacyColumn(q, spec.table, spec.legacyCol); err != nil {
				return nil, fmt.Errorf("add %s.%s: %w", spec.table, spec.legacyCol, err)
			}
		}
		rows, err := fetchRows(q, spec)
		if err != nil {
			return nil, fmt.Errorf("read %s.%s: %w", spec.table, spec.valueCol, err)
		}
		cr := ColumnReport{Table: spec.table, Column: spec.valueCol, Bucket: spec.bucket}
		for _, r := range rows {
			newVal := convertValue(r.old, rate, spec.bucket)
			cr.RowCount++
			cr.SumBefore += r.old
			cr.SumAfter += newVal
			if r.old != 0 && newVal == 0 {
				cr.ZeroCollapses = append(cr.ZeroCollapses, ZeroCollapse{spec.table, r.pk, r.old})
			}
			if write {
				_, err := q.Exec(fmt.Sprintf(
					"UPDATE %s SET %s = ?, %s = ? WHERE %s = ?",
					spec.table, spec.valueCol, spec.legacyCol, spec.pkCol),
					newVal, r.old, r.pk)
				if err != nil {
					return nil, fmt.Errorf("update %s.%s pk=%v: %w", spec.table, spec.valueCol, r.pk, err)
				}
			}
		}
		cr.ExpectedSumAfter = convertValue(cr.SumBefore, rate, spec.bucket)
		cr.Delta = cr.SumAfter - cr.ExpectedSumAfter
		if cr.Delta < 0 {
			cr.Delta = -cr.Delta
		}
		cr.Tolerance = perRowTolerance(spec.bucket) * cr.RowCount
		out = append(out, cr)
	}
	return out, nil
}

// Report is the full result of a dry-run or an apply — printed by
// printReport (main.go) and asserted against directly by tests.
type Report struct {
	DBPath          string
	Rate            int64
	Mode            string // "dry-run" | "apply"
	AlreadyMigrated bool
	Columns         []ColumnReport
	PendingCounts   map[string]int64
}

// openExisting refuses a path that does not exist (ErrDBMissing) rather
// than letting the sqlite driver silently create an empty file — see
// ErrDBMissing's doc comment for why that matters.
func openExisting(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrDBMissing, dbPath)
		}
		return err
	}
	return nil
}

// RunDryRun computes and returns the full conversion report WITHOUT writing
// anything — requirement #6. It never calls store.Open/refund.Open (which
// would run initSchema, a WRITE, before a human has decided anything); it
// opens its own read-only connection instead. "mode=ro" is defense in
// depth — the real guarantee is architectural: this function's only calls
// into the querier interface below are Query/QueryRow, never Exec.
func RunDryRun(dbPath string, rate int64) (*Report, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("migrate-usdt: rate must be positive, got %d", rate)
	}
	if err := openExisting(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", dbPath, err)
	}
	defer db.Close()

	report := &Report{DBPath: dbPath, Rate: rate, Mode: "dry-run"}

	migrated, err := alreadyMigrated(db)
	if err != nil {
		return nil, err
	}
	report.AlreadyMigrated = migrated
	if migrated {
		// Nothing further to compute: the database is already in the
		// target unit, and re-reading it as if it were still VND would
		// produce a nonsense report (dividing already-micro values by the
		// rate again).
		return report, nil
	}

	specs, err := buildSpecs(db)
	if err != nil {
		return nil, err
	}
	if err := checkRequiredSchema(db, specs); err != nil {
		return nil, err
	}
	pending, err := pendingCounts(db)
	if err != nil {
		return nil, err
	}
	report.PendingCounts = pending

	cols, err := processSpecs(db, specs, rate, false)
	if err != nil {
		return nil, err
	}
	report.Columns = cols
	return report, nil
}

// RunApply performs the live conversion in ONE SQLite transaction —
// requirement #2. Any error at any point rolls back everything: the
// provenance writes, the column conversions, the ALTER TABLEs that added
// the legacy_vnd_* columns, all of it, because SQLite DDL is transactional
// exactly like its DML.
func RunApply(dbPath string, rate int64) (*Report, error) {
	return runApply(dbPath, rate, nil)
}

// runApply is RunApply's implementation. overrideSpecs, when non-nil,
// replaces the normal buildSpecs(db) discovery — a test-only seam (used by
// TestRollbackOnMidMigrationError to inject a spec that fails partway
// through the transaction) that main.go never exercises: RunApply always
// calls this with overrideSpecs nil.
func runApply(dbPath string, rate int64, overrideSpecs []moneySpec) (*Report, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("migrate-usdt: rate must be positive, got %d", rate)
	}
	if err := openExisting(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+
		"?_txlock=immediate&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer db.Close()
	// Single writer, matching internal/store's own posture (store.go:76-79)
	// — this tool runs with the bot stopped, but there is no reason to open
	// more than one connection to a file this dangerous to write.
	db.SetMaxOpenConns(1)

	var specs []moneySpec
	if overrideSpecs != nil {
		specs = overrideSpecs
	} else {
		specs, err = buildSpecs(db)
		if err != nil {
			return nil, err
		}
		if err := checkRequiredSchema(db, specs); err != nil {
			return nil, err
		}
	}

	// Plan §1.G3 hard gate, checked BEFORE opening the transaction: if a
	// checkout is still in flight, converting straddles the cutover and
	// produces a credit off by ~the conversion rate in whichever direction
	// the arithmetic falls. Refuse outright rather than convert an empty
	// set and hope nothing lands mid-run.
	pending, err := pendingCounts(db)
	if err != nil {
		return nil, err
	}
	for table, n := range pending {
		if n > 0 {
			return nil, fmt.Errorf("%w: %s has %d row(s)", ErrPendingNotEmpty, table, n)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op after a successful Commit below

	if _, err := tx.Exec(
		"CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		return nil, fmt.Errorf("create meta: %w", err)
	}

	// ---- GUARD: idempotence marker check ----------------------------------
	// Refuses a re-run BEFORE a single money row is touched. This is what
	// turns "ran twice" into a clean, typed ErrAlreadyMigrated instead of
	// relying solely on the plain INSERT below — that INSERT's PK conflict
	// is the actual, mutation-proof safety net (it rolls back the whole
	// transaction even if this check is ever deleted), but on its own it
	// surfaces as an opaque driver error, not something a caller can branch
	// on.
	migrated, err := alreadyMigrated(tx)
	if err != nil {
		return nil, err
	}
	if migrated {
		return nil, ErrAlreadyMigrated
	}
	// ---- end guard --------------------------------------------------------

	cols, err := processSpecs(tx, specs, rate, true)
	if err != nil {
		return nil, err
	}

	// Plain INSERT — deliberately NOT "INSERT OR REPLACE". `key` is the
	// meta table's PRIMARY KEY, so if the guard above were ever bypassed
	// (e.g. deleted by a future edit), a second run reaches THIS statement
	// having just re-converted every row, hits SQLITE_CONSTRAINT_PRIMARYKEY
	// on "ledger_unit", and the deferred tx.Rollback() undoes the entire
	// transaction — legacy columns, re-conversion, everything. This is the
	// load-bearing safety net; the guard above is the fast, friendly path.
	if _, err := tx.Exec(
		"INSERT INTO meta(key, value) VALUES (?,?), (?,?), (?,?)",
		"ledger_unit", "micro_usdt",
		"migration_rate", strconv.FormatInt(rate, 10),
		"ledger_migrated_ts", strconv.FormatInt(time.Now().Unix(), 10),
	); err != nil {
		return nil, fmt.Errorf("write migration marker: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &Report{
		DBPath: dbPath, Rate: rate, Mode: "apply",
		Columns: cols, PendingCounts: pending,
	}, nil
}
