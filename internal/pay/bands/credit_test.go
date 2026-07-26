package bands

import (
	"math"
	"testing"
)

// The Python acceptance suite tests/test_deposit_credit.py pins these numbers at
// USD_TO_VND=23000 with a desired top-up of 120 USDT.
const (
	rateVND     = 23000
	desiredUSDT = 120
	desiredVND  = desiredUSDT * rateVND // 2,760,000
)

// usdtToVND mirrors the rails' UsdtToVND (bep20_pay.py:61-66 -> round(usdt*rate)),
// so the table below can be written in USDT like the Python test is.
func usdtToVND(usdt float64) int64 { return int64(math.Round(usdt * rateVND)) }

// TestDepositCredit is the ported credit-decision table from
// tests/test_deposit_credit.py:60-89 (telegram_bot.py:9748-9761), plus the two
// boundary rows the Python suite implies but does not spell out (exactly 30c
// under, and one cent past the tolerance on each side).
func TestDepositCredit(t *testing.T) {
	desiredUnits := int64(desiredUSDT * 100) // 12000
	tol := DefaultDepositMatchTolCents       // 30
	cases := []struct {
		name       string
		recvUSDT   float64
		recvUnits  int64
		wantVND    int64
		wantWithin bool
	}{
		// tests/test_deposit_credit.py:60-63 — a dropped cent still tops up 120.
		{"1c under -> credit desired", 119.99, 11999, desiredVND, true},
		// :66-69 — exact.
		{"exact -> credit desired", 120.00, 12000, desiredVND, true},
		// :72-75 — 29c under, inside tolerance.
		{"29c under -> credit desired", 119.71, 11971, desiredVND, true},
		// Boundary: EXACTLY the 30c tolerance under still rounds up to desired.
		{"exactly 30c under -> credit desired", 119.70, 11970, desiredVND, true},
		// Boundary: one cent past the tolerance -> credit what arrived.
		{"31c under -> credit actual", 119.69, 11969, usdtToVND(119.69), false},
		// :78-83 — overpay inside tolerance credits the LARGER actual amount; the
		// customer is never shorted by the max().
		{"30c over -> credit actual (never short the customer)", 120.30, 12030, usdtToVND(120.30), true},
		// Boundary: one cent past the tolerance on the high side.
		{"31c over -> credit actual", 120.31, 12031, usdtToVND(120.31), false},
		// :86-89 — way off -> credit what actually arrived (money is never lost).
		{"way over -> credit actual", 200.00, 20000, usdtToVND(200), false},
		{"way under -> credit actual", 50.00, 5000, usdtToVND(50), false},
	}
	for _, tc := range cases {
		gotVND, within := DepositCredit(tc.recvUnits, desiredUnits, desiredVND, usdtToVND(tc.recvUSDT), tol)
		if gotVND != tc.wantVND {
			t.Errorf("%s: credit = %d VND, want %d", tc.name, gotVND, tc.wantVND)
		}
		if within != tc.wantWithin {
			t.Errorf("%s: withinTol = %v, want %v", tc.name, within, tc.wantWithin)
		}
	}
	// The two invariants the table encodes, asserted directly.
	if got, _ := DepositCredit(11999, desiredUnits, desiredVND, usdtToVND(119.99), tol); got <= usdtToVND(119.99) {
		t.Error("an underpay within tolerance must be rounded UP to the desired top-up")
	}
	if got, _ := DepositCredit(12030, desiredUnits, desiredVND, usdtToVND(120.30), tol); got <= desiredVND {
		t.Error("an overpay within tolerance must credit the larger ACTUAL amount")
	}
}

// TestDepositMatchTolCents pins the default and the env override
// (telegram_bot.py:9552-9555).
func TestDepositMatchTolCents(t *testing.T) {
	if got := DepositMatchTolCents(); got != 30 {
		t.Errorf("DepositMatchTolCents() = %d, want 30 (the shop's 30-cent rule)", got)
	}
	t.Setenv("DEPOSIT_MATCH_TOL_CENTS", "5")
	if got := DepositMatchTolCents(); got != 5 {
		t.Errorf("with the env set, DepositMatchTolCents() = %d, want 5", got)
	}
	t.Setenv("DEPOSIT_MATCH_TOL_CENTS", "not-a-number")
	if got := DepositMatchTolCents(); got != 30 {
		t.Errorf("with a garbage env, DepositMatchTolCents() = %d, want the 30 default", got)
	}
	// t.Setenv restores the previous value on cleanup, so no manual unset here.
}

// TestEvictAndGraceFromEnv pins USDT_BAND_EVICT_S / USDT_PENDING_GRACE_S
// (telegram_bot.py:9428, 9548).
func TestEvictAndGraceFromEnv(t *testing.T) {
	if got := EvictAfterFromEnv(); got != DefaultEvictAfter {
		t.Errorf("EvictAfterFromEnv() = %v, want %v (1800s)", got, DefaultEvictAfter)
	}
	if got := PendingGraceFromEnv(); got != DefaultPendingGrace {
		t.Errorf("PendingGraceFromEnv() = %v, want %v (2h)", got, DefaultPendingGrace)
	}
	t.Setenv("USDT_BAND_EVICT_S", "60")
	if got := EvictAfterFromEnv(); got.Seconds() != 60 {
		t.Errorf("EvictAfterFromEnv() with env = %v, want 60s", got)
	}
}
