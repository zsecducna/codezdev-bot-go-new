package bands

import (
	"errors"
	"math"
	"testing"
)

// Shipped geometry of the two surviving rails, used across the suite. The
// Python original ran step 21, tol 10 at 2 decimals (bep20_pay.py:40,54-55 /
// tron_pay.py:41,50-51); the hands-free-deposits ruling replaced that with a
// 0.01000-0.01999 USDT jitter window at 5 decimals, which needs slots 0.00101
// apart with a ±0.00050 band.
//
// wideParams is a DIFFERENT valid geometry (fewer, wider slots) kept so the
// picker's tests exercise something other than the shipped numbers — otherwise
// a hardcoded 101 anywhere in the picker would pass every test.
var (
	bep20Params   = Params{Step: 101, Tol: 50, Decimals: 5}
	tronParams    = Params{Step: 101, Tol: 50, Decimals: 5}
	shippedParams = bep20Params
	wideParams    = Params{Step: 250, Tol: 100, Decimals: 5}
)

// TestParamsGeometry pins the invariant tests/test_usdt_verify.py:125-131
// asserts: step >= 2*tol+1 (so +/-tol bands never overlap) and the resulting
// per-price slot counts.
func TestParamsGeometry(t *testing.T) {
	for name, p := range map[string]Params{"bep20": bep20Params, "tron": tronParams, "wide": wideParams} {
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
	if got := bep20Params.MinGap(); got != 101 {
		t.Errorf("bep20 MinGap() = %d, want 101", got)
	}
	// Step == MinGap exactly: the tightest legal spacing, which is what lets ten
	// slots fit a window only 0.01 USDT wide.
	if bep20Params.Step != bep20Params.MinGap() {
		t.Errorf("bep20 Step %d != MinGap %d — the shipped geometry is meant to sit exactly on the disjointness boundary", bep20Params.Step, bep20Params.MinGap())
	}
	if got := bep20Params.Slots(); got != 10 {
		t.Errorf("bep20 Slots() = %d, want 10 (offsets 0.01000..0.01909 inside the jitter window)", got)
	}
	if got := wideParams.Slots(); got != 4 {
		t.Errorf("wide Slots() = %d, want 4", got)
	}
	// The window itself, in units, at the shipped precision.
	if lo, hi := bep20Params.JitterLo(), bep20Params.JitterHiExcl(); lo != 1000 || hi != 2000 {
		t.Errorf("jitter window = [%d,%d), want [1000,2000) — i.e. 0.01000 to 0.01999 USDT", lo, hi)
	}
}

func TestParamsValidateRejectsBadGeometry(t *testing.T) {
	cases := map[string]Params{
		"zero step":     {Step: 0, Tol: 50, Decimals: 5},
		"negative step": {Step: -1, Tol: 50, Decimals: 5},
		"negative tol":  {Step: 101, Tol: -1, Decimals: 5},
		"negative dec":  {Step: 101, Tol: 50, Decimals: -1},
		"dec too high":  {Step: 101, Tol: 50, Decimals: 10},
		// Below 2 decimals a hundredth of a whole unit is not representable, so
		// the jitter window collapses to width 0 and every order would be quoted
		// the bare base — the exact collision the picker exists to prevent.
		"one decimal":  {Step: 1, Tol: 0, Decimals: 1},
		"zero decimal": {Step: 1, Tol: 0, Decimals: 0},
		// RESTATED BOUND (was "Step wider than one whole unit"). A Step past the
		// jitter span can only ever place the k=0 slot inside the window, so the
		// price supports exactly one concurrent order and every other customer is
		// refused USDT_BUSY. Slots() returns 1 there, not 0, so the
		// at-least-one-slot check below cannot catch it — this is a real check,
		// not a restatement of that one.
		"step past the jitter span": {Step: 1001, Tol: 50, Decimals: 5},
		// Tol needs an upper bound for the same window reason — and, far more
		// urgently, because MinGap() computes 2*Tol+1 in int. Without this bound
		// a Tol at or above 2^62 overflows to a NEGATIVE gap, slotClear's
		// `abs64(...) < b.minGap` never fires, every live row stops blocking, and
		// two concurrent same-price orders are handed the SAME amount. Verified:
		// before the bound, Params{Step: 101, Tol: 4611686018427387904,
		// Decimals: 5} passed Validate and Reserve returned "6.01000" twice.
		"tol past the jitter span": {Step: 101, Tol: 1001, Decimals: 5},
		"tol overflows MinGap":     {Step: 101, Tol: 1 << 62, Decimals: 5},
		"tol at int64 max":         {Step: 101, Tol: math.MaxInt64, Decimals: 5},
	}
	for name, p := range cases {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

// TestParamsValidateAllowsStepAtExactlyTheSpan pins the boundary of the
// restated bound: a Step equal to (not exceeding) the jitter span is legal, and
// yields exactly the one slot at the window's low edge.
func TestParamsValidateAllowsStepAtExactlyTheSpan(t *testing.T) {
	p := Params{Step: 1000, Tol: 50, Decimals: 5}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for Step == the jitter span", err)
	}
	if got := p.Slots(); got != 1 {
		t.Errorf("Slots() at Step == span = %d, want 1", got)
	}
}

// TestParamsValidateAllowsTolAtExactlyTheSpan is the matching boundary for the
// Tol bound, and the direct regression pin for the overflow above: a Tol equal
// to the span is legal (useless in practice — MinGap 2001 leaves one usable slot
// — but not a misconfiguration the picker can be harmed by), one unit past it is
// not, and MinGap must stay positive for every accepted Params.
func TestParamsValidateAllowsTolAtExactlyTheSpan(t *testing.T) {
	p := Params{Step: 1000, Tol: 1000, Decimals: 5}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for Tol == the jitter span", err)
	}
	if p.MinGap() <= 0 {
		t.Errorf("MinGap() = %d for accepted params — a non-positive gap disables slotClear entirely", p.MinGap())
	}
	if err := (Params{Step: 1000, Tol: 1001, Decimals: 5}).Validate(); err == nil {
		t.Error("Validate() = nil for Tol one past the span, want error")
	}
}

// TestPickedAmountStaysInsideTheJitterWindow is the user's ruling stated as a
// property: whatever the picker hands an UPWARD-ONLY caller, it is between
// 0.01000 and 0.01999 USDT above the base — never the bare base, never more.
//
// Scoped to the upward-only path on purpose. Product orders keep the single
// last-resort slot BELOW base (see TestUniqueAmountStepBands), so the same
// assertion against Reserve/UniqueAmount would be asserting the opposite of the
// documented behaviour.
func TestPickedAmountStaysInsideTheJitterWindow(t *testing.T) {
	p := shippedParams
	baseU, err := AmountUnits("6.00", p.Decimals)
	if err != nil {
		t.Fatalf("AmountUnits: %v", err)
	}
	var used []blocker
	for k := 0; k < p.Slots(); k++ {
		cu, ok := uniqueAmountUnits(p, baseU, used, false)
		if !ok {
			t.Fatalf("slot %d: picker exhausted early, want %d slots", k, p.Slots())
		}
		off := cu - baseU
		if off < p.JitterLo() || off >= p.JitterHiExcl() {
			t.Errorf("slot %d: offset %d units (%s USDT) is outside the jitter window [%d,%d)",
				k, off, FormatUnits(off, p.Decimals), p.JitterLo(), p.JitterHiExcl())
		}
		used = append(used, blocker{units: cu, minGap: int64(p.MinGap())})
	}
	// Every slot consumed -> upward-only must REFUSE, never reach for the
	// downward last resort.
	if cu, ok := uniqueAmountUnits(p, baseU, used, false); ok {
		t.Errorf("upward-only picker returned %s after the window was full, want refusal", FormatUnits(cu, p.Decimals))
	}
	// The worst case a customer can be quoted, stated in money so a future
	// geometry change that regresses it is loud.
	worst := p.JitterLo() + int64(p.Slots()-1)*int64(p.Step)
	if got := FormatUnits(worst, p.Decimals); got != "0.01909" {
		t.Errorf("worst-case overquote = %s USDT, want 0.01909", got)
	}
}

// TestLiveAmountsStayDisjoint is the MinGap guarantee itself: no two amounts the
// picker hands out for the same price are within 2*Tol of each other, so no
// single receipt can fall inside two bands.
//
// MUTATION CHECK, run and documented so the next person can repeat it. Note
// what it takes to break this, because that is itself the design's strength:
// widening Tol alone does NOT break it (slotClear simply skips the newly
// overlapping slots and the picker hands out fewer, still-disjoint amounts), and
// shrinking Step alone does not either (slotClear rejects the too-close
// candidates). Only weakening slotClear ITSELF can produce a colliding set.
// Verified by mutating slotClear's `abs64(cu-b.units) < b.minGap` to
// `< b.minGap/2` and setting bep20Params' Step to 51: the picker then hands out
// amounts 51 units apart and this test fails on its own
// "51 units apart, within 2*Tol (100) — one receipt could match both" message,
// not on a panic or a downstream error. Both edits restored.
func TestLiveAmountsStayDisjoint(t *testing.T) {
	p := shippedParams
	baseU, err := AmountUnits("6.00", p.Decimals)
	if err != nil {
		t.Fatalf("AmountUnits: %v", err)
	}
	var picked []int64
	var used []blocker
	// Every upward slot plus the downward last resort — the full set of amounts
	// that can be live for one price at once.
	for {
		cu, ok := uniqueAmountUnits(p, baseU, used, true)
		if !ok {
			break
		}
		picked = append(picked, cu)
		used = append(used, blocker{units: cu, minGap: int64(p.MinGap())})
	}
	if len(picked) != p.Slots()+1 {
		t.Fatalf("picker produced %d amounts, want %d upward slots + 1 last resort", len(picked), p.Slots())
	}
	for i := range picked {
		for j := i + 1; j < len(picked); j++ {
			if d := abs64(picked[i] - picked[j]); d < int64(2*p.Tol+1) {
				t.Errorf("%s and %s are %d units apart, within 2*Tol (%d) — one receipt could match both",
					FormatUnits(picked[i], p.Decimals), FormatUnits(picked[j], p.Decimals), d, 2*p.Tol)
			}
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
		// The shipped precision. A 2-dp base string (usdtStr still emits whole
		// cents) must parse at 5 decimals without loss — that is what lets the
		// picker add its jitter to a price it was handed as "6.00".
		{"6.00", 5, 600_000},
		{"120.00", 5, 12_000_000},
		{"6.01000", 5, 601_000},
		{"6.01909", 5, 601_909},
		{"5.99899", 5, 599_899},
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
		// Note the FIRST slot is the base plus the jitter floor, never the round
		// base: a bare "6.00" is exactly the amount two customers would collide on.
		{"nothing live -> base + the jitter floor", nil, "6.01000"},
		{"first slot taken -> +1 step", []string{"6.01000"}, "6.01101"},
		{"two taken -> +2 steps", []string{"6.01000", "6.01101"}, "6.01202"},
		{"three taken -> +3 steps", []string{"6.01000", "6.01101", "6.01202"}, "6.01303"},
		{"all 10 upward taken -> last-resort slot BELOW base",
			[]string{"6.01000", "6.01101", "6.01202", "6.01303", "6.01404",
				"6.01505", "6.01606", "6.01707", "6.01808", "6.01909"}, "5.99899"},
		// An off-grid live amount blocks EVERY slot whose +/-tol band it
		// overlaps: 6.01050's band is [6.01000,6.01100] and it sits within
		// minGap (101) of both the 6.01000 and the 6.01101 slots, so the picker
		// lands on 6.01202.
		{"off-grid live amount blocks both overlapping slots", []string{"6.01050"}, "6.01202"},
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
	used := []string{"6.01000", "6.01101", "6.01202", "6.01303", "6.01404",
		"6.01505", "6.01606", "6.01707", "6.01808", "6.01909", "5.99899"}
	if _, err := UniqueAmount(bep20Params, "6.00", used); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("UniqueAmount with every slot taken = %v, want ErrBandExhausted", err)
	}
}

// TestUniqueAmountNoNegativeLastResort: for a price below one step, base-step
// would be <= 0, so the last-resort slot must not be offered
// (telegram_bot.py:9089 `if cu > 0`).
func TestUniqueAmountNoNegativeLastResort(t *testing.T) {
	// base 0.001 = 100 units, step 101 -> down slot would be -1. All 10 upward
	// slots must be filled first, and then the picker must refuse rather than
	// quote a negative amount.
	p := bep20Params
	base := "0.00100"
	baseU, err := AmountUnits(base, p.Decimals)
	if err != nil {
		t.Fatalf("AmountUnits: %v", err)
	}
	var used []string
	for k := range p.Slots() {
		used = append(used, FormatUnits(baseU+p.JitterLo()+int64(k)*int64(p.Step), p.Decimals))
	}
	if _, err := UniqueAmount(p, base, used); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("UniqueAmount at a sub-step price = %v, want ErrBandExhausted (no negative slot)", err)
	}
}

// TestUniqueAmountWideGeometry: a geometry other than the shipped one — fewer,
// wider slots (step 250, tol 100 -> minGap 201) — must walk the same way, so the
// picker is provably driven by Params rather than by the shipped constants.
func TestUniqueAmountWideGeometry(t *testing.T) {
	got, err := UniqueAmount(wideParams, "6.00", []string{"6.01000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "6.01250" {
		t.Errorf("wide second slot = %q, want %q", got, "6.01250")
	}
	// 4 upward slots at offsets 1000/1250/1500/1750; then the 5.99750 last
	// resort; then exhausted.
	var used []string
	for k := range wideParams.Slots() {
		used = append(used, FormatUnits(600_000+wideParams.JitterLo()+int64(k)*int64(wideParams.Step), wideParams.Decimals))
	}
	got, err = UniqueAmount(wideParams, "6.00", used)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "5.99750" {
		t.Errorf("wide last-resort slot = %q, want %q", got, "5.99750")
	}
	used = append(used, "5.99750")
	if _, err := UniqueAmount(wideParams, "6.00", used); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("wide all slots taken = %v, want ErrBandExhausted", err)
	}
}

// TestRecvMatchesBandTolerance pins the fuzzy-match boundary at
// telegram_bot.py:9745 (`abs(recv_units - want_units) <= AMOUNT_MATCH_TOL`):
// exactly tol matches, one unit past it does not.
//
// The EXACT path is now essentially exact: a payment right to the last decimal
// settles, and one further off than 0.00050 does not match THIS band. "Does not
// match" is not "is rejected" — a short receipt may still be attributable by the
// separate fee-shortfall rule in match.go, which is deliberately much wider and
// carries its own ambiguity check. This test owns the exact path only.
func TestRecvMatchesBandTolerance(t *testing.T) {
	const expected = "6.01000" // a real picked amount: base 6.00 + the jitter floor
	cases := []struct {
		recv int64
		want bool
		why  string
	}{
		{601_000, true, "exact to all five decimals"},
		{601_050, true, "0.00050 over = exactly tol"},
		{601_051, false, "one unit past tol"},
		{600_950, true, "0.00050 under = exactly tol"},
		{600_949, false, "one unit under past tol"},
		{601_101, false, "the NEXT slot's amount must never match this band"},
		{600_000, false, "the bare base is a full jitter away and must not match"},
	}
	for _, tc := range cases {
		got, err := RecvMatchesBand(bep20Params, expected, tc.recv)
		if err != nil {
			t.Fatalf("RecvMatchesBand error: %v", err)
		}
		if got != tc.want {
			t.Errorf("RecvMatchesBand(%s, %d) = %v, want %v (%s)", expected, tc.recv, got, tc.want, tc.why)
		}
	}
	if _, err := RecvMatchesBand(bep20Params, "junk", 601_000); err == nil {
		t.Error("RecvMatchesBand with a bad expected amount = nil error, want error")
	}
}

// TestRecvMatchesAnyBand is the claim-jack gate at telegram_bot.py:9635-9642:
// a mismatched payment that lands in ANOTHER live order's band is refused, not
// credited to the wrong session.
func TestRecvMatchesAnyBand(t *testing.T) {
	others := []string{"6.01101", "6.01202"}
	if !RecvMatchesAnyBand(bep20Params, others, 601_101) {
		t.Error("exact neighbour amount must be reported as a band hit")
	}
	if !RecvMatchesAnyBand(bep20Params, others, 601_151) { // 6.01101 + tol
		t.Error("neighbour band edge must be reported as a band hit")
	}
	// 6.01101's band is [6.01051,6.01151] and 6.01202's is [6.01152,6.01252] —
	// they are adjacent, so the only clear side is below: 6.01050 is one unit
	// under 6.01101's low edge.
	if RecvMatchesAnyBand(bep20Params, others, 601_050) {
		t.Error("one unit outside every neighbour band must NOT be a band hit")
	}
	if RecvMatchesAnyBand(bep20Params, nil, 601_000) {
		t.Error("no neighbours must never be a band hit")
	}
	// An unparseable neighbour is skipped, never treated as a match.
	if RecvMatchesAnyBand(bep20Params, []string{"junk"}, 601_000) {
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
