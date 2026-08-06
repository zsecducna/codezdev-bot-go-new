package handlers

import "testing"

// TestUsdtDispZeroHasNoNegativeSign is the review-round fix for a real,
// pre-existing bug: usdtDisp(0) rendered "-0.00 USDT" on every healthy
// zero-amount line (Wallet balance, a zero A7 Refunded total, …). Root
// cause: usd*100-1e-9 at usd==0 is a tiny negative float, and math.Ceil of a
// small negative returns IEEE-754 NEGATIVE ZERO, which Go's %.2f prints WITH
// its sign. Fixed at the source (money.go's cents==0 reassignment) rather
// than worked around at each call site, so every current AND future zero-VND
// render is correct — a local sign-strip on just the new A7 Refunded line
// would have left this exact defect live on the Wallet balances line and any
// future zero-amount render.
//
// Falsification: reverting the `if cents == 0 { cents = 0 }` guard in
// usdtDisp makes this test fail with "-0.00 USDT" — verified by mutation
// (see the executor report).
func TestUsdtDispZeroHasNoNegativeSign(t *testing.T) {
	b := &Bot{}
	if got := b.usdtDisp(0); got != "0.00 USDT" {
		t.Errorf("usdtDisp(0) = %q, want %q (no negative-zero sign)", got, "0.00 USDT")
	}
}

// TestUsdtDispNonZeroUnaffected checks that the cents==0 reassignment never
// touches a real, nonzero amount's digits.
//
// Scope, stated honestly: these are POSITIVE cases only, so this test does not
// reach the sub-cent negative interval where cents is also negative zero
// (usd ∈ (-0.01, 0]) — it is not the full blast-radius pin an earlier version
// of this comment claimed to be. The exhaustive check lives in money.go's
// comment: 1.4M rate×value combinations, zero digit changes. What this test
// does pin is the everyday path, and it fails if the guard is ever widened
// into something that rounds.
//
// Uses the compiled default rate (25000 VND/USD, config.DefaultUSDToVND) since
// b.cfg is nil here, matching money.go's own fallback.
func TestUsdtDispNonZeroUnaffected(t *testing.T) {
	b := &Bot{}
	cases := []struct {
		vnd  int64
		want string
	}{
		{5000, "0.20 USDT"},
		{2000, "0.08 USDT"},
		{1, "0.01 USDT"}, // rounds UP to the cent, never undercharge
	}
	for _, c := range cases {
		if got := b.usdtDisp(c.vnd); got != c.want {
			t.Errorf("usdtDisp(%d) = %q, want %q", c.vnd, got, c.want)
		}
	}
}
