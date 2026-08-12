package bands

import (
	"testing"
)

// The Python acceptance suite tests/test_deposit_credit.py pins these numbers at
// USD_TO_VND=23000 with a desired top-up of 120 USDT. Re-expressed at the
// 5-decimal geometry: the customer still ASKS for 120, but is QUOTED
// 120.01000 — the request plus the smallest jitter the picker can hand out.
const (
	rateVND     = 23000
	desiredUSDT = 120
	desiredVND  = desiredUSDT * rateVND // 2,760,000 — o.Total, the REQUEST
	// pickedUnits is the amount the customer is actually asked to send, in
	// amount units at 5 decimals: 120.01000 USDT.
	pickedUnits = int64(12_001_000)
)

// vndOf converts amount units (5 decimals) to VND with the rails' round-half-up,
// in integer arithmetic. Deliberately NOT float: a table that depends on whether
// 120.0095*23000 lands just under or just over .5 is a test that fails on a
// different machine rather than on a real defect.
func vndOf(units int64) int64 { return (units*rateVND + 50_000) / 100_000 }

// TestDepositCredit is the ported credit-decision table from
// tests/test_deposit_credit.py:60-89 (telegram_bot.py:9748-9761), re-anchored on
// the picked amount and the 50-unit (0.00050 USDT) tolerance, with the boundary
// rows on each side of that tolerance.
func TestDepositCredit(t *testing.T) {
	tol := DefaultDepositMatchTolUnits // 50
	cases := []struct {
		name       string
		recvUnits  int64
		wantVND    int64
		wantWithin bool
	}{
		{"exact to the last decimal -> within tolerance", pickedUnits, vndOf(pickedUnits), true},
		{"50 units under (the exact edge) -> still within", pickedUnits - 50, vndOf(pickedUnits - 50), true},
		{"51 units under -> outside, credit actual", pickedUnits - 51, vndOf(pickedUnits - 51), false},
		{"50 units over (the exact edge) -> still within", pickedUnits + 50, vndOf(pickedUnits + 50), true},
		{"51 units over -> outside, credit actual", pickedUnits + 51, vndOf(pickedUnits + 51), false},
		// :86-89 — way off -> credit what actually arrived (money is never lost).
		{"way over -> credit actual", 20_000_000, vndOf(20_000_000), false},
		{"way under -> credit actual", 5_000_000, vndOf(5_000_000), false},
	}
	for _, tc := range cases {
		gotVND, within := DepositCredit(tc.recvUnits, pickedUnits, desiredVND, vndOf(tc.recvUnits), tol)
		if gotVND != tc.wantVND {
			t.Errorf("%s: credit = %d VND, want %d", tc.name, gotVND, tc.wantVND)
		}
		if within != tc.wantWithin {
			t.Errorf("%s: withinTol = %v, want %v", tc.name, within, tc.wantWithin)
		}
	}
	// :78-83 — the customer is never shorted: every row above credits the ACTUAL
	// received value, which is above the 120 they requested precisely because the
	// jitter is upward. Asserted directly so a regression that started crediting
	// the smaller requested total is loud.
	for _, recv := range []int64{pickedUnits, pickedUnits - 50, pickedUnits + 50} {
		got, _ := DepositCredit(recv, pickedUnits, desiredVND, vndOf(recv), tol)
		if got != vndOf(recv) || got <= desiredVND {
			t.Errorf("recv %d: credit = %d, want the larger ACTUAL %d (> the requested %d)",
				recv, got, vndOf(recv), desiredVND)
		}
	}
	// :60-63 — the round-a-small-underpay-UP rule itself. recvVND is below
	// desiredVND here, which on the deposit path happens when the USD_TO_VND rate
	// moves DOWN between the quote and the verify: the quote was priced at the
	// old rate, the receipt is valued at the new one, and the customer's exact
	// payment is suddenly worth less than the total they asked to top up. At a
	// stable rate the jitter keeps every in-tolerance receipt above the request
	// and the credit-the-actual rule above wins instead.
	if got, _ := DepositCredit(pickedUnits, pickedUnits, desiredVND, desiredVND-1, tol); got != desiredVND {
		t.Errorf("underpay within tolerance credited %d, want the desired %d rounded up", got, desiredVND)
	}
}

// TestDepositTolCannotSpanTwoBands is the disjointness argument behind the
// tolerance's value, asserted rather than left in a comment: live amounts are at
// least MinGap apart, so a tolerance of at most MinGap/2 can never put one
// receipt inside two of them.
//
// SCOPE: this proof covers the EXACT path only, and that is not a weakness. The
// fee-shortfall path (match.go) is deliberately far too wide to disambiguate by
// spacing, which is exactly why it carries an explicit ambiguity check instead
// of relying on a proof like this one.
//
// SCOPE: this proof covers the EXACT path only, and that is not a weakness. The
// fee-shortfall path (match.go) is deliberately far too wide to disambiguate by
// spacing, which is exactly why it carries an explicit ambiguity check instead
// of relying on a proof like this one.
func TestDepositTolCannotSpanTwoBands(t *testing.T) {
	p := shippedParams
	if got := 2 * DefaultDepositMatchTolUnits; int64(got) >= int64(p.MinGap()) {
		t.Errorf("2*DefaultDepositMatchTolUnits = %d >= MinGap %d: one receipt could be within tolerance of two live amounts at once, settling the wrong order", got, p.MinGap())
	}
	// And the value is the LARGEST that holds — one more would not.
	if 2*(DefaultDepositMatchTolUnits+1) < p.MinGap() {
		t.Errorf("DefaultDepositMatchTolUnits = %d leaves room: %d would still be disjoint, so the default is needlessly tight", DefaultDepositMatchTolUnits, DefaultDepositMatchTolUnits+1)
	}
}

// TestClampDepositTol pins the guard that makes the argument above hold for any
// operator configuration, not just the shipped one.
func TestClampDepositTol(t *testing.T) {
	p := shippedParams
	if got := ClampDepositTol(p, 10); got != 10 {
		t.Errorf("ClampDepositTol(10) = %d, want 10 (a value under the band half-width passes through)", got)
	}
	if got := ClampDepositTol(p, p.Tol); got != p.Tol {
		t.Errorf("ClampDepositTol(%d) = %d, want %d (the half-width itself is allowed)", p.Tol, got, p.Tol)
	}
	if got := ClampDepositTol(p, 30_000); got != p.Tol {
		t.Errorf("ClampDepositTol(30000) = %d, want the clamp to %d — an operator who typed cents must not widen the tolerance past the band", got, p.Tol)
	}
	if got := ClampDepositTol(p, -1); got != 0 {
		t.Errorf("ClampDepositTol(-1) = %d, want 0", got)
	}
}

// TestDepositMatchTolUnits pins the default and the env override
// (telegram_bot.py:9552-9555).
func TestDepositMatchTolUnits(t *testing.T) {
	if got := DepositMatchTolUnits(); got != 50 {
		t.Errorf("DepositMatchTolUnits() = %d, want 50 (the band half-width at 5 decimals)", got)
	}
	t.Setenv("DEPOSIT_MATCH_TOL_UNITS", "5")
	if got := DepositMatchTolUnits(); got != 5 {
		t.Errorf("with the env set, DepositMatchTolUnits() = %d, want 5", got)
	}
	t.Setenv("DEPOSIT_MATCH_TOL_UNITS", "not-a-number")
	if got := DepositMatchTolUnits(); got != 50 {
		t.Errorf("with a garbage env, DepositMatchTolUnits() = %d, want the 50 default", got)
	}
	// The superseded key must be genuinely dead, not quietly still honoured:
	// its unit changed by a factor of 1000, so reading it would be worse than
	// ignoring it.
	t.Setenv("DEPOSIT_MATCH_TOL_CENTS", "30")
	if got := DepositMatchTolUnits(); got != 50 {
		t.Errorf("DEPOSIT_MATCH_TOL_CENTS still read (= %d): the renamed key must not resurrect a cents-denominated value", got)
	}
	// t.Setenv restores the previous value on cleanup, so no manual unset here.
}

// TestEvictFromEnv pins USDT_BAND_EVICT_S (telegram_bot.py:9428).
//
// No PendingGraceFromEnv counterpart here (task #42 item 6, deleted): it had
// zero production callers, unlike EvictAfterFromEnv which registry.go:125
// still falls back to whenever bands.Config.EvictAfter is left unset.
func TestEvictFromEnv(t *testing.T) {
	if got := EvictAfterFromEnv(); got != DefaultEvictAfter {
		t.Errorf("EvictAfterFromEnv() = %v, want %v (1800s)", got, DefaultEvictAfter)
	}
	t.Setenv("USDT_BAND_EVICT_S", "60")
	if got := EvictAfterFromEnv(); got.Seconds() != 60 {
		t.Errorf("EvictAfterFromEnv() with env = %v, want 60s", got)
	}
}
