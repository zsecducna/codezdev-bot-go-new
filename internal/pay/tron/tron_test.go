package tron

import (
	"math/big"
	"testing"
)

// TestValidAddress mirrors tron_pay.py:71-73's base58 shape check.
func TestValidAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", true}, // USDT contract itself, valid shape
		{"", false},
		{"T123", false}, // too short
		{"1R7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6tX", false}, // doesn't start with T
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t0", false}, // '0' not in base58 alphabet, wrong length too
	}
	for _, tc := range cases {
		if got := ValidAddress(tc.addr); got != tc.want {
			t.Errorf("ValidAddress(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestAmountUnits mirrors tron_pay.py:98-101 — Decimal(str(amount)) *
// 10^AMOUNT_DECIMALS, exact-decimal (no float drift).
func TestAmountUnits(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	cases := []struct {
		amount string
		want   int64
	}{
		{"5.23", 523},
		{"0", 0},
		{"100", 10000},
		{"5.2", 520},
	}
	for _, tc := range cases {
		got, err := AmountUnits(tc.amount)
		if err != nil {
			t.Fatalf("AmountUnits(%q) error: %v", tc.amount, err)
		}
		if got != tc.want {
			t.Errorf("AmountUnits(%q) = %d, want %d", tc.amount, got, tc.want)
		}
	}
}

// TestValToUnits mirrors tron_pay.py:104-108 — floor a 6-decimal base-unit
// value to AMOUNT_DECIMALS (2) units; floor, not round, so dust never
// rounds up to a higher-matching amount.
func TestValToUnits(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	cases := []struct {
		value string
		want  int64
	}{
		{"105000000", 10500}, // 105 USDT exactly -> 10500 cents
		{"105009999", 10500}, // dust overpay floors down, still matches 105.00
		{"105004999", 10500},
	}
	for _, tc := range cases {
		v, ok := new(big.Int).SetString(tc.value, 10)
		if !ok {
			t.Fatalf("bad test value %q", tc.value)
		}
		if got := valToUnits(v); got != tc.want {
			t.Errorf("valToUnits(%s) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestMatchPayment exercises the matcher (tron_pay.py:165-185): fuzzy amount
// tolerance, at/after min_ts_ms, isUsed dedupe with "trx:" prefix.
func TestMatchPayment(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	t.Setenv("TRON_MATCH_TOL", "10")
	minTS := int64(1_700_000_000_000)

	v1, _ := new(big.Int).SetString("105000000", 10) // 105.00 USDT
	v2, _ := new(big.Int).SetString("105050000", 10) // 105.05 USDT (5 cents off, within tol 10)

	transfers := []Transfer{
		{ID: "old", Value: v1, Units: 10500, TS: minTS - 1000},  // too early
		{ID: "good", Value: v2, Units: 10505, TS: minTS + 1000}, // matches
	}
	got, err := MatchPayment(transfers, "105.00", minTS, nil)
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "good" {
		t.Fatalf("MatchPayment = %+v, want id=good", got)
	}
}

// TestMatchPayment_IsUsedSkips verifies the "trx:" prefixed dedupe hook.
func TestMatchPayment_IsUsedSkips(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	minTS := int64(1_700_000_000_000)
	v, _ := new(big.Int).SetString("105000000", 10)

	transfers := []Transfer{
		{ID: "first", Value: v, Units: 10500, TS: minTS + 1000},
		{ID: "second", Value: v, Units: 10500, TS: minTS + 2000},
	}
	got, err := MatchPayment(transfers, "105.00", minTS, func(id string) bool { return id == "trx:first" })
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "second" {
		t.Fatalf("MatchPayment = %+v, want id=second (first skipped)", got)
	}
}
