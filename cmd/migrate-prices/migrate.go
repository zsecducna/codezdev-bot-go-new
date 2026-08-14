// Command migrate-prices is the on-disk price-file half of the VND->micro-USDT
// flip — the COMPLEMENT of cmd/migrate-usdt. migrate-usdt rewrites the MONEY
// columns in store.db; THIS tool rewrites the SECOND on-disk money store the
// DB migration missed: the per-product tier price files under accounts/
// (`price_<CODE>_VND`), read/written by internal/stock/catalog.go and charged
// verbatim by purchase.go. The DB columns and these files MUST flip together:
// after the catalog code change, ProductPrice reads its int as MICRO-USDT, so a
// file still holding a VND-scale value would be undercharged by ~40x at cutover
// (rate 25000). Run OFFLINE, with the bot stopped — this binary does not take
// the stock flock, and a price file rewritten mid-buy would race a sale.
//
// This file holds the conversion + walk logic; main.go is the CLI wrapper.
package main

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// microPerCent is 1 cent of USDT in micro-USDT units (1 USDT = 1_000_000 micro,
// so 1 cent = 10_000 micro) — the SAME constant cmd/migrate-usdt uses, because
// product prices are order-shaped (a tier price becomes an orders.total), so
// they must satisfy the identical whole-cent invariant the DB migration
// enforces on orders.total.
const microPerCent = 10_000

// oldPriceFileRe matches a price_<CODE>_VND file name (case-insensitive),
// capturing the tier CODE — the SAME pattern internal/stock/catalog.go used to
// DISCOVER these files before the flip. Case-insensitive so a lower-case legacy
// file (which the old DeleteProductTier also matched case-insensitively) is not
// left behind to trip the boot guard.
var oldPriceFileRe = regexp.MustCompile(`(?i)^price_(.+)_vnd$`)

// ErrBadRate means -rate was missing or non-positive. Sentinel so tests assert
// by errors.Is, mirroring migrate-usdt's typed errors.
type ErrBadRate struct{ rate int64 }

func (e *ErrBadRate) Error() string {
	return fmt.Sprintf("migrate-prices: -rate is required and must be positive — pass the SAME VND-per-USD rate migrate-usdt used (got %d)", e.rate)
}

// validateRate refuses a missing/non-positive rate. Kept as a plain function
// (never calls os.Exit) so tests exercise it directly, exactly like
// migrate-usdt's validateCLIFlags. -rate has NO default: a bare run must never
// silently pick a rate for real money (plan §1.G2, same rule as migrate-usdt).
func validateRate(rate int64) error {
	if rate <= 0 {
		return &ErrBadRate{rate: rate}
	}
	return nil
}

// ---- exact-integer conversion math (no float64 on the value path) ----------
//
// These three helpers are the price-file analogue of migrate-usdt's
// roundDivHalfUp / vndToMicro / roundToNearestCent. Duplicated rather than
// shared because migrate-usdt is package main (not importable) and lifting the
// math into a shared internal package would expand scope onto the working DB
// tool; the math is tiny, self-evidently correct, and unlikely to change. A
// product price's conversion is `convertPrice` below = migrate-usdt's
// bucketCentRounded convertValue for one row, bit for bit.

// roundDivHalfUp returns round(numerator/denominator) using round-half-away-
// from-zero, in exact big.Int arithmetic. denominator must be positive;
// numerator may be negative (a price is never negative, but the rounding must
// still be well-defined). Same algorithm as migrate-usdt's roundDivHalfUp.
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

// vndToMicro converts a VND integer price to micro-USDT at `rate` VND-per-USD:
// micro = round(vnd * 1_000_000 / rate). big.Int so the intermediate
// vnd*1_000_000 product can never silently overflow. Product prices are tiny
// (a 500000 VND price is 2e11 in the product — far under int64), so unlike
// migrate-usdt's aggregate-sized inputs no IsInt64 overflow guard is needed
// here; the result always fits. rate must be > 0 — validateRate enforces that
// once, at the CLI boundary.
func vndToMicro(vnd, rate int64) int64 {
	num := new(big.Int).Mul(big.NewInt(vnd), big.NewInt(1_000_000))
	q := roundDivHalfUp(num, big.NewInt(rate))
	return q.Int64()
}

// roundToNearestCent rounds a micro-USDT amount to the nearest whole cent
// (10_000 micro). Applied AFTER vndToMicro, never before — rounding the VND
// input first would double-round and drift further from the true value. Same
// rule as migrate-usdt's roundToNearestCent.
func roundToNearestCent(micro int64) int64 {
	q := roundDivHalfUp(big.NewInt(micro), big.NewInt(microPerCent))
	return q.Int64() * microPerCent
}

// convertPrice applies the cent-rounded bucket to one VND price: vndToMicro
// then roundToNearestCent. Identical to migrate-usdt's convertValue for a
// bucketCentRounded row — a product price is charged verbatim into
// orders.total, so it must round the SAME way the order total it becomes does,
// or a charged-vs-recorded mismatch becomes representable.
func convertPrice(vnd, rate int64) int64 {
	return roundToNearestCent(vndToMicro(vnd, rate))
}

// Outcome classifies one file's planned action.
type Outcome string

const (
	// outcomeConvert: a _VND file with no matching _USDT — convert + rename.
	outcomeConvert Outcome = "convert"
	// outcomeSkip: a _USDT already exists alongside the _VND — do NOT
	// re-convert (that would divide by the rate a second time). In apply mode
	// the redundant _VND is removed so the boot guard passes; in dry-run it is
	// left untouched.
	outcomeSkip Outcome = "skip-exists"
	// outcomeError: the file's int did not parse — leave it for a human, never
	// silently invent a value.
	outcomeError Outcome = "error"
)

// Action is one price file's planned outcome.
type Action struct {
	Product string // parent folder name (the product)
	Code    string // tier code, UPPER-CASE (canonical, as the new bot writes it)
	VND     int64  // original value read from the _VND file (0 on parse error)
	Micro   int64  // converted micro-USDT (0 on skip/error)
	Outcome Outcome
	Note    string // human-readable detail (parse error text, etc.)
	OldPath string // the price_<CODE>_VND path, relative to accountsDir
	NewPath string // the price_<CODE>_USDT path, relative to accountsDir
}

// Report is the full result of a dry-run or an apply, printed by printReport
// and asserted against directly by tests.
type Report struct {
	AccountsDir string
	Rate        int64
	Apply       bool
	Actions     []Action
}

// Converted, Skipped, Errored count Actions by outcome for the summary line.
func (r *Report) Converted() int { return countBy(r.Actions, outcomeConvert) }
func (r *Report) Skipped() int   { return countBy(r.Actions, outcomeSkip) }
func (r *Report) Errored() int   { return countBy(r.Actions, outcomeError) }

func countBy(actions []Action, o Outcome) int {
	n := 0
	for _, a := range actions {
		if a.Outcome == o {
			n++
		}
	}
	return n
}

// planFile reads one _VND file and decides its Action WITHOUT writing. It
// resolves the matching _USDT path (idempotence), reads + parses the VND int,
// and computes the converted micro. Pure I/O (read-only) so the SAME path
// feeds both dry-run reporting and apply writing.
func planFile(absPath, relPath, product string, code string, rate int64) Action {
	code = strings.ToUpper(code) // canonical, as the new SetProductPrice writes
	newRel := "price_" + code + "_USDT"
	newAbs := filepath.Join(filepath.Dir(absPath), newRel)
	act := Action{
		Product: product,
		Code:    code,
		OldPath: relPath,
		NewPath: newRel,
	}
	// Idempotence: a _USDT already present means a prior run (or an
	// interrupted one) already converted this tier — re-converting would
	// divide by the rate a SECOND time. Skip the conversion; apply mode below
	// still removes the redundant _VND so the boot guard sees a clean tree.
	if _, err := os.Stat(newAbs); err == nil {
		act.Outcome = outcomeSkip
		return act
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		act.Outcome = outcomeError
		act.Note = "read: " + err.Error()
		return act
	}
	vnd, perr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if perr != nil {
		// A price file that is not a bare integer is already corrupt relative
		// to what catalog.go reads — converting it would invent a number. Leave
		// it and surface it so a human decides.
		act.Outcome = outcomeError
		act.Note = "parse int: " + perr.Error()
		return act
	}
	act.VND = vnd
	act.Micro = convertPrice(vnd, rate)
	act.Outcome = outcomeConvert
	return act
}

// Run walks accountsDir for price_<CODE>_VND files, plans each one's
// conversion, and — when apply is true — writes price_<CODE>_USDT and removes
// the _VND original. Dry-run (apply=false) computes + reports the exact same
// plan without writing. Returns a Report naming every file, converted value,
// and outcome; an empty Actions slice means "nothing to do".
//
// Idempotent by construction: a _USDT already present skips the conversion, so
// re-running after a successful apply finds no _VND files and reports nothing
// to do. A missing accountsDir is reported (not fatal) as nothing to do, since
// a fresh install has no product folders yet.
func Run(accountsDir string, rate int64, apply bool) (*Report, error) {
	if err := validateRate(rate); err != nil {
		return nil, err
	}
	report := &Report{AccountsDir: accountsDir, Rate: rate, Apply: apply}

	if _, err := os.Stat(accountsDir); err != nil {
		// No accounts dir = nothing to migrate. Return an empty report rather
		// than erroring: a brand-new deploy has no product folders, and
		// "nothing to do" is the correct answer for it.
		if os.IsNotExist(err) {
			return report, nil
		}
		return nil, fmt.Errorf("stat %s: %w", accountsDir, err)
	}

	err := filepath.WalkDir(accountsDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		m := oldPriceFileRe.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		rel, relErr := filepath.Rel(accountsDir, path)
		if relErr != nil {
			rel = d.Name()
		}
		product := filepath.Base(filepath.Dir(rel))
		act := planFile(path, rel, product, m[1], rate)
		if apply {
			applyAction(path, filepath.Join(filepath.Dir(path), act.NewPath), &act)
		}
		report.Actions = append(report.Actions, act)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", accountsDir, err)
	}

	// Deterministic order for readable output + stable test assertions:
	// sort by (product, code) so a product's tiers cluster together.
	sort.SliceStable(report.Actions, func(i, j int) bool {
		if report.Actions[i].Product != report.Actions[j].Product {
			return report.Actions[i].Product < report.Actions[j].Product
		}
		return report.Actions[i].Code < report.Actions[j].Code
	})
	return report, nil
}

// applyAction performs the filesystem writes for one planned Action under
// apply mode: writes the converted _USDT atomically (tmp + rename) and removes
// the _VND original. For a skip-exists outcome it ONLY removes the redundant
// _VND (the _USDT is already correct — re-writing would need the rate, which
// is exactly what we are refusing to apply a second time). For an error
// outcome it writes nothing. Errors mutate the Action to outcomeError + a Note
// so the report still reflects what actually happened on disk.
func applyAction(oldAbs, newAbs string, act *Action) {
	switch act.Outcome {
	case outcomeConvert:
		// Atomic write so a crash never leaves price_<CODE>_USDT half-written
		// (a torn read would serve a garbage price on the next buy-menu
		// render) — the same contract stock.writeFileAtomic upholds.
		tmp := newAbs + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.FormatInt(act.Micro, 10)), 0o600); err != nil {
			act.Outcome = outcomeError
			act.Note = "write tmp: " + err.Error()
			return
		}
		if err := os.Rename(tmp, newAbs); err != nil {
			_ = os.Remove(tmp)
			act.Outcome = outcomeError
			act.Note = "rename: " + err.Error()
			return
		}
		if err := os.Remove(oldAbs); err != nil {
			// The _USDT is already written + correct, so a failure to remove
			// the _VND original is not data loss — but it leaves a file the
			// boot guard will flag on next boot. Surface it as an error so the
			// operator removes it by hand rather than booting into the guard.
			act.Outcome = outcomeError
			act.Note = "remove old _VND (USDT already written): " + err.Error()
			return
		}
	case outcomeSkip:
		// _USDT already exists — do not re-convert. Just retire the redundant
		// _VND so the tree is clean and the boot guard passes.
		if err := os.Remove(oldAbs); err != nil {
			act.Outcome = outcomeError
			act.Note = "remove stray _VND (USDT already present): " + err.Error()
		}
	case outcomeError:
		// Unparseable/unreadable — leave it untouched for a human.
	}
}
