// Command migrate-prices converts the on-disk product price files under
// accounts/ from VND-scale ints named price_<CODE>_VND to micro-USDT ints named
// price_<CODE>_USDT — the price-file half of the VND->micro-USDT flip (the DB
// half is cmd/migrate-usdt). Run OFFLINE, with the bot process stopped.
//
// Usage:
//
//	migrate-prices -accounts ./accounts -rate 25000            # dry run (default): reports, writes nothing
//	migrate-prices -accounts ./accounts -rate 25000 -apply     # the real conversion: write _USDT, remove _VND
//
// -rate has NO default and is REQUIRED, and must be the SAME VND-per-USD rate
// migrate-usdt was run with: the price-file flip and the DB flip must agree, or
// a tier price and the balance that pays for it would be in different units.
//
// DRY-RUN BY DEFAULT. Without -apply this binary reports every rename +
// conversion it WOULD make and writes nothing; pass -apply to write. Unlike
// migrate-usdt there are no -i-have-a-backup / -yes confirmation flags here:
// the operation is a per-file rename + integer rescale, fully reversible (the
// _VND originals are the backup until -apply removes them), and idempotent (a
// _USDT already present is skipped, so re-running after a successful apply is a
// clean no-op that reports "nothing to do").
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	accountsDir := flag.String("accounts", "./accounts", "accounts dir to walk for price_<CODE>_VND files (the bot's accounts root)")
	rate := flag.Int64("rate", 0, "VND-per-USD rate — REQUIRED, no default. Must match the rate migrate-usdt was run with")
	apply := flag.Bool("apply", false, "actually write price_<CODE>_USDT and remove the _VND original. Without this flag, report only — nothing is written")
	flag.Parse()

	// A bare run must never pick a rate for real money. validateRate is a plain
	// function (never calls os.Exit) so tests exercise it directly.
	if err := validateRate(*rate); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-prices:", err)
		os.Exit(1)
	}

	report, err := Run(*accountsDir, *rate, *apply)
	if report != nil {
		printReport(os.Stdout, report)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-prices:", err)
		os.Exit(1)
	}
}

// printReport renders a Report as plain text: mode, the accounts dir + rate, a
// line per price file (its outcome + the VND->micro conversion + the rename),
// then a summary count. Empty Actions prints "nothing to do".
func printReport(w *os.File, r *Report) {
	mode := "dry-run"
	if r.Apply {
		mode = "apply"
	}
	fmt.Fprintf(w, "migrate-prices: mode=%s accounts=%s rate=%d\n", mode, r.AccountsDir, r.Rate)
	if len(r.Actions) == 0 {
		fmt.Fprintln(w, "nothing to do — no price_*_VND files found (already micro-USDT, or no product folders)")
		return
	}
	for _, a := range r.Actions {
		switch a.Outcome {
		case outcomeConvert:
			fmt.Fprintf(w, "  CONVERT  %s/%s  %d VND -> %d micro  (%s -> %s)\n",
				a.Product, a.Code, a.VND, a.Micro, a.OldPath, a.NewPath)
		case outcomeSkip:
			note := "already migrated, _USDT present"
			if r.Apply {
				note = "already migrated, removed stray _VND"
			}
			fmt.Fprintf(w, "  SKIP     %s/%s  %s  (%s)\n", a.Product, a.Code, note, a.OldPath)
		case outcomeError:
			fmt.Fprintf(w, "  ERROR    %s/%s  %s  (%s)\n", a.Product, a.Code, a.Note, a.OldPath)
		}
	}
	fmt.Fprintf(w, "summary: %d converted, %d skipped, %d error(s)\n",
		r.Converted(), r.Skipped(), r.Errored())
}
