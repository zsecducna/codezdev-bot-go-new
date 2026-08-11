// Command migrate-usdt converts store.db's ledger from VND to micro-USDT —
// the S9 slice of .omc/plans/usdt-migration.md, task #11. Run OFFLINE, with
// the bot process stopped (plan §6.3): this binary does not itself gate
// against a live writer beyond SQLite's own file locking.
//
// Usage:
//
//	migrate-usdt -db store.db -rate 25000                                  # dry run (default): reports, writes nothing
//	migrate-usdt -db store.db -rate 25000 -apply -i-have-a-backup -yes     # the real, one-transaction conversion
//
// -rate has NO default and is REQUIRED: a bare run must never silently pick
// a rate for real money. Per plan §1.G2, freeze it by reading production's
// settings.json usd_to_vnd and its exported USD_TO_VND ambient var, confirm
// they agree (or pick the settings.json value if they don't — that is what
// the balance screen has been showing), and pass that frozen number as
// -rate.
//
// -apply additionally REQUIRES both -i-have-a-backup and -yes (this tool
// has no other interactive confirmation) — a bare `migrate-usdt -apply`
// must never be able to run against a real database.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	dbPath := flag.String("db", "store.db", "path to store.db")
	rate := flag.Int64("rate", 0, "VND-per-USD rate, FROZEN off production per plan §1.G2 (do not re-derive) — REQUIRED, no default")
	apply := flag.Bool("apply", false, "actually write the conversion, in one transaction. Without this flag, report only — nothing is written")
	allowZeroCollapse := flag.String("allow-zero-collapse", "",
		"per-column zero-collapse override ONLY — a tolerance breach is never overridable, for any column. "+
			"Comma-separated table.column list, e.g. orders.code_discount,refund_requests.total — naming one "+
			"column does not excuse a collapse in any other. Copy the exact \"table.column\" string straight "+
			"out of a dry run's refusal message. Empty (default) refuses every zero-collapse.")
	haveBackup := flag.Bool("i-have-a-backup", false, "REQUIRED with -apply: confirms a store.db backup exists before this tool writes to it")
	yes := flag.Bool("yes", false, "REQUIRED with -apply: confirms this run is intentional (this tool has no other interactive confirmation)")
	flag.Parse()

	// ---- GUARD: operational foot-guns (Fix 5) ----------------------------
	// A bare `migrate-usdt -apply` run from the bot's working directory
	// must never be able to convert a real database at an unfrozen default
	// rate with zero confirmation. validateCLIFlags is a plain function
	// (never calls os.Exit) so tests can exercise it directly.
	if err := validateCLIFlags(*rate, *apply, *haveBackup, *yes); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-usdt:", err)
		os.Exit(1)
	}
	// ---- end guard --------------------------------------------------------

	allowed, err := parseAllowZeroCollapse(*allowZeroCollapse)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-usdt:", err)
		os.Exit(1)
	}

	var report *Report
	if *apply {
		report, err = RunApply(*dbPath, *rate, allowed)
	} else {
		report, err = RunDryRun(*dbPath, *rate)
	}
	if report != nil {
		printReport(os.Stdout, report)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-usdt:", err)
		os.Exit(1)
	}
}

// validateCLIFlags enforces the operational guard-rails around -rate and
// -apply: rate must be a positive, explicitly-chosen number (no silent
// default), and an apply run must carry both explicit confirmation flags.
// Returns a plain error rather than calling os.Exit so main() controls
// process exit and tests can call this directly.
func validateCLIFlags(rate int64, apply, haveBackup, yes bool) error {
	if rate <= 0 {
		return fmt.Errorf("-rate is required and must be positive — read it from production's settings.json usd_to_vnd per plan §1.G2, do not guess")
	}
	if apply && !(haveBackup && yes) {
		return fmt.Errorf("-apply requires both -i-have-a-backup and -yes — this tool converts real money in place, in one transaction, with no further confirmation")
	}
	return nil
}

// parseAllowZeroCollapse turns the -allow-zero-collapse flag's comma-
// separated "table.column" list into the per-column allowlist evaluateColumns
// checks each ZeroCollapse against. Kept per-column rather than one global
// bool: overriding it for a legitimate tiny collapse (e.g.
// orders.code_discount) must not ALSO blind orders.total — exactly the
// column requirement #6 exists to protect. It fails SAFE in both
// directions: an empty flag refuses every column (unchanged from before),
// and an entry that doesn't look like "table.column" is a hard error rather
// than being silently ignored — a typo'd column name must not read to the
// operator as "I allowed this collapse" when it did nothing.
func parseAllowZeroCollapse(s string) (map[string]bool, error) {
	if s == "" {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, ".") {
			return nil, fmt.Errorf(
				"-allow-zero-collapse entry %q is not table.column — copy the exact string out of a dry run's refusal message, e.g. orders.code_discount", entry)
		}
		allowed[entry] = true
	}
	return allowed, nil
}

// printReport renders a Report as plain text: mode, rate, the §1.G3 pending
// gate status, and one block per money column with its row count, sum
// before/after, reconciliation verdict, and any zero-collapse rows —
// exactly the fields requirement #6 asks a dry run to surface.
func printReport(w io.Writer, r *Report) {
	fmt.Fprintf(w, "migrate-usdt: mode=%s db=%s rate=%d\n", r.Mode, r.DBPath, r.Rate)
	if r.AlreadyMigrated {
		fmt.Fprintln(w, "database is ALREADY MIGRATED (meta.ledger_unit=micro_usdt) — nothing to do")
		return
	}
	if r.Mode == "apply-REFUSED" {
		fmt.Fprintln(w, "\n*** APPLY REFUSED — the transaction was rolled back, NOTHING was written ***")
	}

	if len(r.PendingCounts) > 0 {
		fmt.Fprintln(w, "\npending tables (plan §1.G3 — all must be 0 before an apply run):")
		tables := make([]string, 0, len(r.PendingCounts))
		for t := range r.PendingCounts {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		for _, t := range tables {
			n := r.PendingCounts[t]
			status := "ok"
			if n > 0 {
				status = "NOT EMPTY — apply will refuse"
			}
			fmt.Fprintf(w, "  %-16s rows=%-6d %s\n", t, n, status)
		}
	}

	fmt.Fprintln(w, "\nmoney columns:")
	for _, c := range r.Columns {
		verdict := "OK"
		if c.Delta > c.Tolerance {
			verdict = "ABORT — outside tolerance"
		}
		fmt.Fprintf(w, "  %s.%s [%s]\n", c.Table, c.Column, c.Bucket)
		fmt.Fprintf(w, "    rows=%d  sum_before(VND)=%d  sum_after(micro)=%d  expected(micro)=%d  delta=%d  tolerance=%d  %s\n",
			c.RowCount, c.SumBefore, c.SumAfter, c.ExpectedSumAfter, c.Delta, c.Tolerance, verdict)
		// Reported, never enforced. A column that converts with NO rounded
		// rows is exactly convertible at this rate — the strongest statement
		// this report can honestly make, and checkable at a glance. The line
		// is printed either way so its absence never has to be interpreted.
		fmt.Fprintf(w, "    rounded_rows=%d of %d%s\n", c.RoundedRows, c.RowCount,
			map[bool]string{true: "  (exact at this rate)", false: ""}[c.RoundedRows == 0])
		if len(c.ZeroCollapses) > 0 {
			fmt.Fprintf(w, "    ZERO-COLLAPSE (non-zero VND -> zero micro), %d row(s):\n", len(c.ZeroCollapses))
			for _, z := range c.ZeroCollapses {
				fmt.Fprintf(w, "      pk=%v old_vnd=%d\n", z.PK, z.Old)
			}
		}
	}
}
