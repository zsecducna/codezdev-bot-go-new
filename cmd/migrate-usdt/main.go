// Command migrate-usdt converts store.db's ledger from VND to micro-USDT —
// the S9 slice of .omc/plans/usdt-migration.md, task #11. Run OFFLINE, with
// the bot process stopped (plan §6.3): this binary does not itself gate
// against a live writer beyond SQLite's own file locking.
//
// Usage:
//
//	migrate-usdt -db store.db -rate 25000            # dry run (default): reports, writes nothing
//	migrate-usdt -db store.db -rate 25000 -apply     # the real, one-transaction conversion
//
// The rate is NOT re-derived here. Per plan §1.G2, freeze it by reading
// production's settings.json usd_to_vnd and its exported USD_TO_VND ambient
// var, confirm they agree (or pick the settings.json value if they don't —
// that is what the balance screen has been showing), and pass that frozen
// number as -rate. The 25000 default is the value production is known to
// have been serving from settings.json — it is a documented starting point,
// not a substitute for that read.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
)

func main() {
	dbPath := flag.String("db", "store.db", "path to store.db")
	rate := flag.Int64("rate", 25000, "VND-per-USD rate, FROZEN off production per plan §1.G2 (do not re-derive)")
	apply := flag.Bool("apply", false, "actually write the conversion, in one transaction. Without this flag, report only — nothing is written")
	flag.Parse()

	var (
		report *Report
		err    error
	)
	if *apply {
		report, err = RunApply(*dbPath, *rate)
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
		if len(c.ZeroCollapses) > 0 {
			fmt.Fprintf(w, "    ZERO-COLLAPSE (non-zero VND -> zero micro), %d row(s):\n", len(c.ZeroCollapses))
			for _, z := range c.ZeroCollapses {
				fmt.Fprintf(w, "      pk=%v old_vnd=%d\n", z.PK, z.Old)
			}
		}
	}
}
