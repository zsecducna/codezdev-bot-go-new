package bands

import (
	"errors"
	"testing"
)

// Shipped geometry of the three rails, used across the suite.
//
//	bep20_pay.py:40,54-55 / tron_pay.py:41,50-51  -> step 21, tol 10, dec 2
//	binance_pay.py:54-55                          -> step 10, tol  4, cents
var (
	bep20Params   = Params{Step: 21, Tol: 10, Decimals: 2}
	tronParams    = Params{Step: 21, Tol: 10, Decimals: 2}
	binanceParams = Params{Step: 10, Tol: 4, Decimals: 2}
)

// TestParamsGeometry pins the invariant tests/test_usdt_verify.py:125-131
// asserts: step >= 2*tol+1 (so +/-tol bands never overlap) and the resulting
// per-price slot counts. Widened by SlotSpanWholeUnits=2 (hands-free-deposits
// ruling (b)): 200//21 = 9 upward slots for the on-chain rails, 200//10 = 20
// for Binance.
func TestParamsGeometry(t *testing.T) {
	for name, p := range map[string]Params{"bep20": bep20Params, "tron": tronParams, "binance": binanceParams} {
		if !p.SpacingOK() {
			t.Errorf("%s: step %d < minGap %d — concurrent same-price bands would overlap", name, p.Step, p.MinGap())
		}
		if p.Tol <= 0 {
			t.Errorf("%s: tol must be positive, got %d", name, p.Tol)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v", name, err)
		}
	}
	if got := bep20Params.MinGap(); got != 21 {
		t.Errorf("bep20 MinGap() = %d, want 21", got)
	}
	if got := bep20Params.Slots(); got != 9 {
		t.Errorf("bep20 Slots() = %d, want 9 (the widened max-slots-per-price cap)", got)
	}
	if got := binanceParams.Slots(); got != 20 {
		t.Errorf("binance Slots() = %d, want 20", got)
	}
}

func TestParamsValidateRejectsBadGeometry(t *testing.T) {
	cases := map[string]Params{
		"zero step":     {Step: 0, Tol: 10, Decimals: 2},
		"negative step": {Step: -1, Tol: 10, Decimals: 2},
		"negative tol":  {Step: 21, Tol: -1, Decimals: 2},
		"bad decimals":  {Step: 21, Tol: 10, Decimals: -1},
		"step too wide": {Step: 201, Tol: 10, Decimals: 2}, // 200//201 = 0 slots
	}
	for name, p := range cases {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

// TestAmountUnitsAndFormat mirrors bep20_pay.py:97-100 / tron_pay.py:98-101 and
// the round-trip the picker relies on (telegram_bot.py:9083,9088).
func TestAmountUnitsAndFormat(t *testing.T) {
	cases := []struct {
		s     string
		dec   int
		units int64
	}{
		{"5.23", 2, 523},
		{"6.00", 2, 600},
		{"120.00", 2, 12000},
		{"0.01", 2, 1},
		{"5.79", 2, 579},
		{"5.2345", 4, 52345},
		// Rounding only fires on a string carrying MORE digits than the rail's
		// precision, which never happens for a real amount (they are all emitted
		// at exactly Decimals). Go rounds half-away-from-zero here; Python's
		// Decimal.to_integral_value() is ROUND_HALF_EVEN, so "5.245" would give
		// 524 there and 525 here — see deviation 4 in the package doc.
		{"5.235", 2, 524}, // 523.5 -> 524 under both modes
		{"5.245", 2, 525}, // 524.5 -> 525 here, 524 under Python's banker's rounding
	}
	for _, tc := range cases {
		got, err := AmountUnits(tc.s, tc.dec)
		if err != nil {
			t.Fatalf("AmountUnits(%q,%d) error: %v", tc.s, tc.dec, err)
		}
		if got != tc.units {
			t.Errorf("AmountUnits(%q,%d) = %d, want %d", tc.s, tc.dec, got, tc.units)
		}
	}
	for _, tc := range []struct {
		units int64
		dec   int
		want  string
	}{
		{600, 2, "6.00"}, {621, 2, "6.21"}, {579, 2, "5.79"}, {12000, 2, "120.00"},
		{1, 2, "0.01"}, {0, 2, "0.00"}, {52345, 4, "5.2345"}, {600, 0, "600"},
		{-621, 2, "-6.21"}, // never produced by the picker, but must not garble
	} {
		if got := FormatUnits(tc.units, tc.dec); got != tc.want {
			t.Errorf("FormatUnits(%d,%d) = %q, want %q", tc.units, tc.dec, got, tc.want)
		}
	}
}

// TestAmountUnitsRejectsBadInput: Adopt parses amount strings straight out of
// SQLite on restart, so a corrupted or exotic value must be a loud error, never a
// silently different number on a money path.
func TestAmountUnitsRejectsBadInput(t *testing.T) {
	bad := []string{
		"not-a-number",
		"",
		"3/2",                  // big.Rat accepts fractions; Python's Decimal would not
		"1e100",                // scientific notation
		"6.00_1",               // Go numeric underscore
		"92233720368547758.08", // wraps int64 if unchecked
		"10000000.01",          // just past the 10-million-whole-unit cap
		"-10000000.01",         // same, negative
		// big.Rat.SetString quietly accepts base prefixes — "0x10" would read as
		// 16.00 with no error, which is exactly the silent-wrong-number case.
		"0x10", "0b101", "0o17",
		"+6.00", // Python's Decimal takes it, but nothing here ever emits it
		"6.",    // trailing dot
		".5",    // leading dot
		"6.0 ",  // trailing space
	}
	for _, s := range bad {
		if u, err := AmountUnits(s, 2); err == nil {
			t.Errorf("AmountUnits(%q) = %d with nil error, want error", s, u)
		}
	}
	// The cap is inclusive and — crucially — scales with the rail's precision, so
	// a 6-decimal rail is not silently limited to 1,000 USDT.
	for _, tc := range []struct {
		s    string
		dec  int
		want int64
	}{
		{"10000000.00", 2, 1_000_000_000},
		{"2000.000000", 6, 2_000_000_000},
		{"10000000.000000", 6, 10_000_000_000_000},
	} {
		if u, err := AmountUnits(tc.s, tc.dec); err != nil || u != tc.want {
			t.Errorf("AmountUnits(%q,%d) = (%d,%v), want (%d,nil)", tc.s, tc.dec, u, err, tc.want)
		}
	}
	if _, err := AmountUnits("10000000.000001", 6); err == nil {
		t.Error("one unit past the 6-decimal cap = nil error, want error")
	}
}

// TestRescaleTolCeil: a tolerance must round UP across precisions, so the picker
// never leaves a gap narrower than a foreign rail's real band.
func TestRescaleTolCeil(t *testing.T) {
	cases := []struct {
		tol, from, to, want int
	}{
		{10, 2, 2, 10},  // identity
		{10, 2, 3, 100}, // up-scale
		{100, 3, 2, 10}, // down-scale, exact
		{105, 3, 2, 11}, // down-scale rounds UP (10.5 -> 11), never down to 10
		{1, 3, 2, 1},    // a sub-unit tolerance never collapses to 0
	}
	for _, tc := range cases {
		if got := rescaleTolCeil(tc.tol, tc.from, tc.to); got != tc.want {
			t.Errorf("rescaleTolCeil(%d,%d,%d) = %d, want %d", tc.tol, tc.from, tc.to, got, tc.want)
		}
	}
}

// TestUniqueAmountStepBands walks the picker's slot order exactly as
// telegram_bot.py:9085-9091 does: upward slots first, then the ONE downward
// last-resort slot, then refuse.
func TestUniqueAmountStepBands(t *testing.T) {
	cases := []struct {
		name string
		used []string
		want string
	}{
		{"nothing live -> base itself", nil, "6.00"},
		{"base taken -> +1 step", []string{"6.00"}, "6.21"},
		{"two taken -> +2 steps", []string{"6.00", "6.21"}, "6.42"},
		{"three taken -> +3 steps", []string{"6.00", "6.21", "6.42"}, "6.63"},
		{"all 9 upward taken -> last-resort slot BELOW base",
			[]string{"6.00", "6.21", "6.42", "6.63", "6.84", "7.05", "7.26", "7.47", "7.68"}, "5.79"},
		// An off-grid live amount blocks EVERY slot whose +/-tol band it
		// overlaps: 6.05's band is [5.95,6.15] and 6.21's is [6.11,6.31], so
		// minGap (21) rejects both 6.00 and 6.21 -> the picker lands on 6.42.
		{"off-grid live amount blocks both overlapping slots", []string{"6.05"}, "6.42"},
	}
	for _, tc := range cases {
		got, err := UniqueAmount(bep20Params, "6.00", tc.used)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestUniqueAmountExhausted: every upward slot AND the downward last resort
// taken -> refuse rather than hand out a colliding amount
// (telegram_bot.py:9092). This is the "band exhausted" incident case; the
// handlers must show BUSY, never fall through to a duplicate amount.
func TestUniqueAmountExhausted(t *testing.T) {
	used := []string{"6.00", "6.21", "6.42", "6.63", "6.84", "7.05", "7.26", "7.47", "7.68", "5.79"}
	if _, err := UniqueAmount(bep20Params, "6.00", used); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("UniqueAmount with every slot taken = %v, want ErrBandExhausted", err)
	}
}

// TestUniqueAmountNoNegativeLastResort: for a price below one step, base-step
// would be <= 0, so the last-resort slot must not be offered
// (telegram_bot.py:9089 `if cu > 0`).
func TestUniqueAmountNoNegativeLastResort(t *testing.T) {
	// base 0.10 = 10 units, step 21 -> down slot would be -11. All 9 upward
	// slots (widened SlotSpanWholeUnits=2) must be filled first.
	used := []string{"0.10", "0.31", "0.52", "0.73", "0.94", "1.15", "1.36", "1.57", "1.78"}
	if _, err := UniqueAmount(bep20Params, "0.10", used); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("UniqueAmount at a sub-step price = %v, want ErrBandExhausted (no negative slot)", err)
	}
}

// TestUniqueAmountBinanceGeometry: the tighter Binance geometry (step 10, tol 4)
// yields 20 upward slots (widened SlotSpanWholeUnits=2) and a 9-unit minGap
// (binance_pay.py:50-58).
func TestUniqueAmountBinanceGeometry(t *testing.T) {
	got, err := UniqueAmount(binanceParams, "6.00", []string{"6.00"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "6.10" {
		t.Errorf("binance second slot = %q, want %q", got, "6.10")
	}
	// 6.00..7.90 is 20 slots; then the 5.90 last resort; then exhausted.
	var used []string
	for k := range 20 {
		used = append(used, FormatUnits(600+int64(k)*10, 2))
	}
	got, err = UniqueAmount(binanceParams, "6.00", used)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "5.90" {
		t.Errorf("binance last-resort slot = %q, want %q", got, "5.90")
	}
	used = append(used, "5.90")
	if _, err := UniqueAmount(binanceParams, "6.00", used); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("binance all slots taken = %v, want ErrBandExhausted", err)
	}
}

// TestRecvMatchesBandTolerance pins the fuzzy-match boundary at
// telegram_bot.py:9745 (`abs(recv_units - want_units) <= AMOUNT_MATCH_TOL`):
// exactly tol matches, one unit past it does not.
func TestRecvMatchesBandTolerance(t *testing.T) {
	cases := []struct {
		recv int64
		want bool
		why  string
	}{
		{600, true, "exact"},
		{610, true, "10c over = exactly tol"},
		{611, false, "11c over = one unit past tol"},
		{590, true, "10c under = exactly tol (goodwill underpay)"},
		{589, false, "11c under = one unit past tol"},
		{621, false, "the NEXT slot's amount must never match this band"},
	}
	for _, tc := range cases {
		got, err := RecvMatchesBand(bep20Params, "6.00", tc.recv)
		if err != nil {
			t.Fatalf("RecvMatchesBand error: %v", err)
		}
		if got != tc.want {
			t.Errorf("RecvMatchesBand(6.00, %d) = %v, want %v (%s)", tc.recv, got, tc.want, tc.why)
		}
	}
	if _, err := RecvMatchesBand(bep20Params, "junk", 600); err == nil {
		t.Error("RecvMatchesBand with a bad expected amount = nil error, want error")
	}
}

// TestRecvMatchesAnyBand is the claim-jack gate at telegram_bot.py:9635-9642:
// a mismatched payment that lands in ANOTHER live order's band is refused, not
// credited to the wrong session.
func TestRecvMatchesAnyBand(t *testing.T) {
	others := []string{"6.21", "6.42"}
	if !RecvMatchesAnyBand(bep20Params, others, 621) {
		t.Error("exact neighbour amount must be reported as a band hit")
	}
	if !RecvMatchesAnyBand(bep20Params, others, 631) { // 6.21 + tol
		t.Error("neighbour band edge must be reported as a band hit")
	}
	// 6.21's band is [6.11,6.31] and 6.42's is [6.32,6.52] — they are adjacent,
	// so the only clear side is below: 6.10 is one unit under 6.21's low edge.
	if RecvMatchesAnyBand(bep20Params, others, 610) {
		t.Error("one unit outside every neighbour band must NOT be a band hit")
	}
	if RecvMatchesAnyBand(bep20Params, nil, 600) {
		t.Error("no neighbours must never be a band hit")
	}
	// An unparseable neighbour is skipped, never treated as a match.
	if RecvMatchesAnyBand(bep20Params, []string{"junk"}, 600) {
		t.Error("unparseable neighbour must not produce a band hit")
	}
}

// TestRescaleUnits covers the cross-provider comparison path: rails with
// different AMOUNT_DECIMALS must be comparable in ONE collision set, and the
// down-scale floors (bep20_pay.py:103-107's floor rule).
func TestRescaleUnits(t *testing.T) {
	cases := []struct {
		units    int64
		from, to int
		want     int64
		why      string
	}{
		{600, 2, 2, 600, "same precision is identity"},
		{600, 2, 4, 60000, "up-scale multiplies"},
		{60049, 4, 2, 600, "down-scale floors"},
		{-60049, 4, 2, -601, "down-scale floors negatives too"},
	}
	for _, tc := range cases {
		if got := rescaleUnits(tc.units, tc.from, tc.to); got != tc.want {
			t.Errorf("rescaleUnits(%d,%d,%d) = %d, want %d (%s)", tc.units, tc.from, tc.to, got, tc.want, tc.why)
		}
	}
}
