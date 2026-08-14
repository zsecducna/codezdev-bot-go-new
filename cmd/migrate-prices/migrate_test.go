package main

// Tests for migrate-prices's conversion + walk logic (migrate.go). Covers the
// money-correctness contract the cutover depends on: exact conversion at the
// rate, dry-run writes nothing, apply writes + removes + is idempotent on a
// re-run, a pre-existing _USDT is skipped (never double-converted), and a
// bad/missing rate is refused.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestConvertPriceCorrectness proves the VND->micro math against hand-computed
// values: round(vnd * 1_000_000 / rate), then cent-round. At rate 25000 the
// factor is exactly x40 and prices land on whole cents; at a non-dividing rate
// (24000) the cent-round step is exercised.
func TestConvertPriceCorrectness(t *testing.T) {
	for _, tc := range []struct {
		name string
		vnd  int64
		rate int64
		want int64
	}{
		{"50k VND @25000 -> 2.00 USDT", 50_000, 25_000, 2_000_000},
		{"90k VND @25000 -> 3.60 USDT", 90_000, 25_000, 3_600_000},
		{"55k VND @25000 -> 2.20 USDT", 55_000, 25_000, 2_200_000},
		{"120k VND @25000 -> 4.80 USDT", 120_000, 25_000, 4_800_000},
		{"1 VND @25000 -> 0 (rounds to 0 micro)", 1, 25_000, 0},
		// Non-dividing rate: 50_000 VND -> 2_083_333 micro raw -> cent-rounds to
		// 2_080_000 (2.08 USDT), proving the whole-cent invariant holds.
		{"50k VND @24000 cent-rounds to 2.08 USDT", 50_000, 24_000, 2_080_000},
	} {
		if got := convertPrice(tc.vnd, tc.rate); got != tc.want {
			t.Errorf("%s: convertPrice(%d,%d) = %d, want %d", tc.name, tc.vnd, tc.rate, got, tc.want)
		}
	}
}

// TestValidateRate proves -rate is required and must be positive — no silent
// default, the same money-safety rule migrate-usdt enforces.
func TestValidateRate(t *testing.T) {
	if err := validateRate(0); err == nil {
		t.Error("validateRate(0) should error")
	}
	if err := validateRate(-25000); err == nil {
		t.Error("validateRate(-25000) should error")
	}
	var badRate *ErrBadRate
	if err := validateRate(0); !errors.As(err, &badRate) {
		t.Errorf("validateRate(0) should return *ErrBadRate, got %T (%v)", err, err)
	}
	if err := validateRate(25000); err != nil {
		t.Errorf("validateRate(25000) should be nil, got %v", err)
	}
}

// seedVND writes a price_<CODE>_VND file under dir/<product>/ holding vnd, and
// returns the product's folder path.
func seedVND(t *testing.T, dir, product, code string, vnd int64) string {
	t.Helper()
	prod := filepath.Join(dir, product)
	if err := os.MkdirAll(prod, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prod, "price_"+code+"_VND"),
		[]byte(strconv.FormatInt(vnd, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	return prod
}

// TestRunDryRunWritesNothing proves the default (apply=false) computes the plan
// and reports the conversions but leaves the tree byte-identical: _VND files
// intact, no _USDT files created.
func TestRunDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	seedVND(t, dir, "kbh", "KBH", 50_000)
	seedVND(t, dir, "kbh", "BH30D", 55_000)

	rep, err := Run(dir, 25_000, false)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if rep.Converted() != 2 {
		t.Fatalf("dry-run converted count = %d, want 2", rep.Converted())
	}
	// Verify the computed micro for the KBH file (50_000 VND -> 2_000_000).
	var kbh *Action
	for i := range rep.Actions {
		if rep.Actions[i].Code == "KBH" {
			kbh = &rep.Actions[i]
		}
	}
	if kbh == nil || kbh.Micro != 2_000_000 {
		t.Fatalf("KBH micro = %v, want 2000000", kbh)
	}

	// Nothing written: _VND still present, no _USDT.
	for _, f := range []string{"kbh/price_KBH_VND", "kbh/price_BH30D_VND"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("dry-run should leave %s intact: %v", f, err)
		}
	}
	for _, f := range []string{"kbh/price_KBH_USDT", "kbh/price_BH30D_USDT"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("dry-run must not create %s: err=%v", f, err)
		}
	}
}

// TestRunApplyConvertsAndRemoves proves -apply writes price_<CODE>_USDT with the
// converted micro value, removes the _VND original, and round-trips through
// ProductPrice's naming: the new file is canonical-case _USDT.
func TestRunApplyConvertsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	seedVND(t, dir, "kbh", "KBH", 50_000)

	rep, err := Run(dir, 25_000, true)
	if err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if rep.Converted() != 1 {
		t.Fatalf("apply converted count = %d, want 1", rep.Converted())
	}
	// _VND gone, _USDT present with the converted value.
	if _, err := os.Stat(filepath.Join(dir, "kbh", "price_KBH_VND")); !os.IsNotExist(err) {
		t.Errorf("_VND should be removed: err=%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "kbh", "price_KBH_USDT"))
	if err != nil {
		t.Fatalf("read _USDT: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "2000000" {
		t.Errorf("_USDT content = %q, want \"2000000\"", string(raw))
	}
}

// TestRunIdempotentReRun proves the money-critical idempotence contract: after a
// successful apply, re-running finds NO _VND files and reports nothing to do.
// This is what stops a double-application from dividing every price by the rate
// a second time.
func TestRunIdempotentReRun(t *testing.T) {
	dir := t.TempDir()
	seedVND(t, dir, "kbh", "KBH", 50_000)

	if _, err := Run(dir, 25_000, true); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	rep, err := Run(dir, 25_000, true)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Fatalf("re-run should be a no-op, got %d actions", len(rep.Actions))
	}
	// And the value is still the single-converted micro, not re-divided.
	raw, _ := os.ReadFile(filepath.Join(dir, "kbh", "price_KBH_USDT"))
	if strings.TrimSpace(string(raw)) != "2000000" {
		t.Errorf("re-run must not re-convert; _USDT = %q, want \"2000000\"", string(raw))
	}
}

// TestRunSkipExistingUSDT proves the both-exist case: when a _USDT already sits
// alongside a _VND (an interrupted run's residue), the conversion is SKIPPED so
// the value is never divided twice; in apply mode the redundant _VND is removed
// so the boot guard sees a clean tree.
func TestRunSkipExistingUSDT(t *testing.T) {
	dir := t.TempDir()
	prod := seedVND(t, dir, "kbh", "KBH", 50_000)
	// Pre-place the already-converted _USDT (simulating a prior/interrupted run).
	if err := os.WriteFile(filepath.Join(prod, "price_KBH_USDT"), []byte("2000000"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Dry-run reports a skip, never a convert.
	rep, err := Run(dir, 25_000, false)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if rep.Converted() != 0 || rep.Skipped() != 1 {
		t.Fatalf("dry-run want 0 converted / 1 skipped, got %d / %d", rep.Converted(), rep.Skipped())
	}
	// Dry-run leaves both files.
	if _, err := os.Stat(filepath.Join(prod, "price_KBH_VND")); err != nil {
		t.Errorf("dry-run should leave _VND: %v", err)
	}

	// Apply skips the conversion AND retires the redundant _VND.
	rep, err = Run(dir, 25_000, true)
	if err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if rep.Skipped() != 1 || rep.Converted() != 0 {
		t.Fatalf("apply want 0 converted / 1 skipped, got %d / %d", rep.Converted(), rep.Skipped())
	}
	if _, err := os.Stat(filepath.Join(prod, "price_KBH_VND")); !os.IsNotExist(err) {
		t.Errorf("apply should remove redundant _VND: err=%v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(prod, "price_KBH_USDT"))
	if strings.TrimSpace(string(raw)) != "2000000" {
		t.Errorf("_USDT must be unchanged (not re-converted): %q", string(raw))
	}
}

// TestRunNothingToDo proves a clean tree (no _VND files) reports nothing to do,
// and a missing accounts dir is treated the same way (fresh deploy).
func TestRunNothingToDo(t *testing.T) {
	// Empty dir.
	dir := t.TempDir()
	rep, err := Run(dir, 25_000, true)
	if err != nil {
		t.Fatalf("Run empty: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Fatalf("empty dir want no actions, got %d", len(rep.Actions))
	}
	// A dir with only _USDT files.
	prod := filepath.Join(dir, "kbh")
	_ = os.MkdirAll(prod, 0o700)
	_ = os.WriteFile(filepath.Join(prod, "price_KBH_USDT"), []byte("2000000"), 0o600)
	rep, err = Run(dir, 25_000, true)
	if err != nil {
		t.Fatalf("Run clean: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Fatalf("clean _USDT tree want no actions, got %d", len(rep.Actions))
	}
	// Missing dir entirely.
	rep, err = Run(filepath.Join(dir, "nope"), 25_000, true)
	if err != nil {
		t.Fatalf("Run missing dir: %v", err)
	}
	if rep == nil || len(rep.Actions) != 0 {
		t.Fatalf("missing dir want non-nil empty report, got %v", rep)
	}
}

// TestRunRefusesBadRate proves Run itself refuses a bad rate (not just the CLI),
// so the logic-layer guard is independently testable.
func TestRunRefusesBadRate(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(dir, 0, true); err == nil {
		t.Error("Run(rate=0) should error")
	}
	if _, err := Run(dir, -1, true); err == nil {
		t.Error("Run(rate=-1) should error")
	}
}

// TestRunUnparseableFile proves a price file whose content is not a bare integer
// is reported as an error and left untouched — the tool never invents a value.
func TestRunUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	prod := filepath.Join(dir, "kbh")
	_ = os.MkdirAll(prod, 0o700)
	if err := os.WriteFile(filepath.Join(prod, "price_KBH_VND"), []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(dir, 25_000, true)
	if err != nil {
		t.Fatalf("Run should not abort on one bad file: %v", err)
	}
	if rep.Errored() != 1 {
		t.Fatalf("want 1 error, got %d", rep.Errored())
	}
	// The unparseable _VND file is left in place for a human.
	if _, err := os.Stat(filepath.Join(prod, "price_KBH_VND")); err != nil {
		t.Errorf("unparseable file should be left untouched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prod, "price_KBH_USDT")); !os.IsNotExist(err) {
		t.Errorf("no _USDT should be written for an unparseable file: err=%v", err)
	}
}
