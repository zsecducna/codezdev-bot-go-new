package bands

import (
	"testing"
	"time"
)

// liveAt builds a live Reservation holding `amount` at the shipped precision —
// the only fields MatchReceipt reads are Units and Decimals, but Des is set so a
// failure message names which band was picked.
func liveAt(t *testing.T, des, amount string) Reservation {
	t.Helper()
	u, err := AmountUnits(amount, shippedParams.Decimals)
	if err != nil {
		t.Fatalf("AmountUnits(%q): %v", amount, err)
	}
	return Reservation{
		Provider: ProviderTron, Des: des, ChatID: "7", Amount: amount,
		Units: u, Decimals: shippedParams.Decimals,
		Deadline: time.Unix(1<<40, 0),
	}
}

func mustUnitsAt(t *testing.T, amount string) int64 {
	t.Helper()
	u, err := AmountUnits(amount, shippedParams.Decimals)
	if err != nil {
		t.Fatalf("AmountUnits(%q): %v", amount, err)
	}
	return u
}

// TestFeeAllowanceIsProportional pins the user's rule directly:
//
//	allowance = min(0.5 USDT, 5% of the QUOTED amount)
//
// and the crossover at 10 USDT where the flat cap takes over from the
// percentage.
func TestFeeAllowanceIsProportional(t *testing.T) {
	p := shippedParams
	cases := []struct {
		quoted string
		want   string // the allowance, as a USDT string at the rail's precision
		why    string
	}{
		{"1.00000", "0.05000", "5% binds far below the cap on a minimum deposit"},
		{"1.01000", "0.05050", "5% of a jittered 1-USDT quote"},
		{"9.99999", "0.49999", "just under the crossover, the percentage still binds"},
		{"10.00000", "0.50000", "the crossover: 5% equals the 0.5 cap exactly"},
		{"20.00000", "0.50000", "past the crossover the flat cap binds"},
		{"120.01000", "0.50000", "a large deposit forgives the full 0.5, not 6.0"},
		{"200.00000", "0.50000", "and it stays capped however large the deposit"},
	}
	for _, tc := range cases {
		got := p.FeeAllowance(mustUnitsAt(t, tc.quoted))
		if want := mustUnitsAt(t, tc.want); got != want {
			t.Errorf("FeeAllowance(%s) = %s, want %s (%s)",
				tc.quoted, FormatUnits(got, p.Decimals), tc.want, tc.why)
		}
	}
	// Degenerate inputs must not produce a usable allowance.
	if got := p.FeeAllowance(0); got != 0 {
		t.Errorf("FeeAllowance(0) = %d, want 0", got)
	}
	if got := p.FeeAllowance(-1); got != 0 {
		t.Errorf("FeeAllowance(-1) = %d, want 0", got)
	}
}

// TestFeeAllowanceIsComputedFromTheQuote is the direction pin the brief asks
// for, built on the ONE input that discriminates it.
//
// A 10.00000 quote short by exactly 0.50000 must be accepted: 5% of the QUOTE is
// 0.50000, so the shortfall sits exactly on the allowance. Computed from what
// ARRIVED (9.50000) the allowance would be 5% of 9.50000 = 0.47500, and the same
// payment would be refused. Any smaller shortfall passes under both readings, so
// this boundary is the only case that tells them apart.
//
// The direction matters because the received amount is attacker-controlled: if
// the allowance shrank as the underpayment grew it would bind exactly when it
// must not, and a large shortfall could argue itself into range.
func TestFeeAllowanceIsComputedFromTheQuote(t *testing.T) {
	p := shippedParams
	quoted := mustUnitsAt(t, "10.00000")
	recv := mustUnitsAt(t, "9.50000")

	fromQuote := p.FeeAllowance(quoted)
	fromRecv := p.FeeAllowance(recv)
	if fromQuote <= fromRecv {
		t.Fatalf("fixture broken: allowance from quote %d must exceed allowance from receipt %d "+
			"for this case to discriminate the direction", fromQuote, fromRecv)
	}
	if gap := quoted - recv; gap > fromRecv {
		// Confirms the receipt-derived reading really would refuse this payment.
		t.Logf("shortfall %d is outside the receipt-derived allowance %d, as intended", gap, fromRecv)
	} else {
		t.Fatalf("fixture broken: shortfall %d is inside the receipt-derived allowance %d", gap, fromRecv)
	}

	res, kind := MatchReceipt(p, recv, []Reservation{liveAt(t, "d1", "10.00000")})
	if kind != MatchShortfall || res.Des != "d1" {
		t.Errorf("MatchReceipt = (%s, %s), want (shortfall, d1) — a shortfall exactly on the "+
			"quote-derived allowance must be attributed; refusing it means the allowance was "+
			"derived from the received amount instead", res.Des, kind)
	}
}

// TestMatchReceiptShortfallOnALargeDeposit is the brief's first amendment case:
// a 120 USDT deposit (allowance min(0.5, 6.0) = 0.5) short by 0.3, with exactly
// one live band, is attributed to it.
func TestMatchReceiptShortfallOnALargeDeposit(t *testing.T) {
	p := shippedParams
	live := []Reservation{liveAt(t, "d1", "120.01000")}
	res, kind := MatchReceipt(p, mustUnitsAt(t, "119.71000"), live)
	if kind != MatchShortfall || res.Des != "d1" {
		t.Errorf("MatchReceipt = (%s, %s), want (shortfall, d1)", res.Des, kind)
	}
}

// TestMatchReceiptMoneyPumpIsClosed is the case the proportional cap exists for,
// and the brief asks for it to be pinned explicitly.
//
// MIN_DEPOSIT_USDT is 1. Under a FLAT 0.5 allowance a customer quoted 1.01000
// who sends 0.71000 is short 0.30 — inside the flat allowance — and would be
// attributed and credited. Repeat at will and it is a money pump, cheap to run
// on TRON with staked energy. The proportional cap makes the allowance 0.05050
// here, so the payment is not attributable at all.
func TestMatchReceiptMoneyPumpIsClosed(t *testing.T) {
	p := shippedParams
	live := []Reservation{liveAt(t, "d1", "1.01000")}
	recv := mustUnitsAt(t, "0.71000") // short by 0.30

	// The premise, asserted rather than assumed: a flat 0.5 really would have
	// accepted this.
	if flat := mustUnitsAt(t, "0.50000"); mustUnitsAt(t, "1.01000")-recv > flat {
		t.Fatalf("fixture broken: this shortfall is outside even a flat 0.5, so it would not " +
			"demonstrate what the proportional cap adds")
	}
	if _, kind := MatchReceipt(p, recv, live); kind != MatchNone {
		t.Errorf("MatchReceipt = %s, want none — a 30%% shortfall on a 1-USDT deposit must not "+
			"be attributable, or 'send half, get credited whole' becomes repeatable", kind)
	}
}

// TestMatchReceiptAmbiguousShortfallCreditsNothing: two live bands at the same
// price sit 0.00101 apart, so ANY shortfall window wide enough to forgive a fee
// covers both by construction. Refusing is the only sound answer — picking one
// credits a coin flip to somebody's wallet.
func TestMatchReceiptAmbiguousShortfallCreditsNothing(t *testing.T) {
	p := shippedParams
	live := []Reservation{liveAt(t, "d1", "120.01000"), liveAt(t, "d2", "120.01101")}
	res, kind := MatchReceipt(p, mustUnitsAt(t, "119.71000"), live)
	if kind != MatchAmbiguous {
		t.Errorf("MatchReceipt = (%s, %s), want ambiguous — two live bands can both claim this "+
			"receipt, so it must go to manual attribution", res.Des, kind)
	}
	if res.Des != "" {
		t.Errorf("ambiguous match returned band %q, want the zero Reservation — a caller that "+
			"ignores the verdict must not find a usable band sitting in the result", res.Des)
	}
}

// TestMatchReceiptBeyondTheAllowance: 0.6 short on a 120 deposit is past the
// 0.5 cap, so nothing is attributable even with a single live band.
func TestMatchReceiptBeyondTheAllowance(t *testing.T) {
	p := shippedParams
	live := []Reservation{liveAt(t, "d1", "120.01000")}
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "119.41000"), live); kind != MatchNone {
		t.Errorf("MatchReceipt = %s, want none for a 0.6 shortfall past the 0.5 cap", kind)
	}
	// The exact boundary, both sides. 0.50000 short is in; one unit further is out.
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "119.51000"), live); kind != MatchShortfall {
		t.Errorf("MatchReceipt at exactly the 0.5 allowance = %s, want shortfall (the bound is inclusive)", kind)
	}
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "119.50999"), live); kind != MatchNone {
		t.Errorf("MatchReceipt one unit past the allowance = %s, want none", kind)
	}
}

// TestMatchReceiptExactWinsOverShortfall is the precedence rule. An exact
// receipt settles even while other bands are live AND even while it sits inside
// their shortfall windows — MinGap guarantees only one band can be within Tol,
// which is exactly what makes the exact path immune to the ambiguity the wider
// window creates.
func TestMatchReceiptExactWinsOverShortfall(t *testing.T) {
	p := shippedParams
	// d2 is quoted 0.30 above d1, so d1's exact amount is ALSO inside d2's
	// shortfall window. The exact hit must win outright.
	live := []Reservation{liveAt(t, "d1", "120.01000"), liveAt(t, "d2", "120.31000")}
	recv := mustUnitsAt(t, "120.01000")
	if gap := mustUnitsAt(t, "120.31000") - recv; gap > p.FeeAllowance(mustUnitsAt(t, "120.31000")) {
		t.Fatalf("fixture broken: the receipt is not inside d2's shortfall window (gap %d), so this "+
			"does not test precedence", gap)
	}
	res, kind := MatchReceipt(p, recv, live)
	if kind != MatchExact || res.Des != "d1" {
		t.Errorf("MatchReceipt = (%s, %s), want (exact, d1)", res.Des, kind)
	}
	// ...and the same holds at the edge of the exact tolerance.
	res, kind = MatchReceipt(p, recv-int64(p.Tol), live)
	if kind != MatchExact || res.Des != "d1" {
		t.Errorf("MatchReceipt at the tolerance edge = (%s, %s), want (exact, d1)", res.Des, kind)
	}
}

// TestMatchReceiptOverpayIsNotAMatch: the ruling defines two shapes and both are
// "at most the quote". A receipt above a band by more than Tol has no
// fee-shaped explanation, so it is not evidence of that band. Unattributed money
// is recoverable by hand; money credited to the wrong customer is not.
func TestMatchReceiptOverpayIsNotAMatch(t *testing.T) {
	p := shippedParams
	live := []Reservation{liveAt(t, "d1", "120.01000")}
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "120.31000"), live); kind != MatchNone {
		t.Errorf("MatchReceipt for an overpay = %s, want none", kind)
	}
	// An overpay INSIDE the tolerance is still the exact path, as it always was.
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "120.01000")+int64(p.Tol), live); kind != MatchExact {
		t.Errorf("MatchReceipt for an overpay inside Tol = %s, want exact", kind)
	}
}

// TestMatchReceiptEmptyAndDuplicateBands covers the two degenerate sets. No live
// bands is plainly MatchNone; two bands within Tol of each other is only
// possible if the geometry is broken, and the money-safe reading of a broken
// geometry is "cannot attribute", never "pick one".
func TestMatchReceiptEmptyAndDuplicateBands(t *testing.T) {
	p := shippedParams
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "120.01000"), nil); kind != MatchNone {
		t.Errorf("MatchReceipt against no live bands = %s, want none", kind)
	}
	dup := []Reservation{liveAt(t, "d1", "120.01000"), liveAt(t, "d2", "120.01000")}
	if _, kind := MatchReceipt(p, mustUnitsAt(t, "120.01000"), dup); kind != MatchAmbiguous {
		t.Errorf("MatchReceipt against two identical live amounts = %s, want ambiguous", kind)
	}
}

// TestMatchReceiptRescalesForeignPrecision: a row stored at a different
// AMOUNT_DECIMALS must still be comparable, the same way the picker's collision
// set rescales.
func TestMatchReceiptRescalesForeignPrecision(t *testing.T) {
	p := shippedParams // 5 decimals
	fine := Reservation{
		Provider: ProviderTron, Des: "d6", ChatID: "7", Amount: "120.010000",
		Units: 120_010_000, Decimals: 6, Deadline: time.Unix(1<<40, 0),
	}
	res, kind := MatchReceipt(p, mustUnitsAt(t, "120.01000"), []Reservation{fine})
	if kind != MatchExact || res.Des != "d6" {
		t.Errorf("MatchReceipt against a 6-dp row = (%s, %s), want (exact, d6)", res.Des, kind)
	}
	res, kind = MatchReceipt(p, mustUnitsAt(t, "119.71000"), []Reservation{fine})
	if kind != MatchShortfall || res.Des != "d6" {
		t.Errorf("MatchReceipt shortfall against a 6-dp row = (%s, %s), want (shortfall, d6)", res.Des, kind)
	}
}

// TestShortfallAlwaysCreditsTheActualAmount is the credit decision, asserted
// rather than left to a caller's memory.
//
// A shortfall candidate exists only when the gap EXCEEDS Tol — anything within
// Tol is MatchExact and never reaches the second pass — so DepositCredit's own
// tolerance test already fails for every shortfall receipt and it already
// credits what ARRIVED. The two rules compose by construction. If someone ever
// widens DepositCredit's tolerance past the band half-width, this test goes red
// before the giveaway ships.
func TestShortfallAlwaysCreditsTheActualAmount(t *testing.T) {
	p := shippedParams
	quoted := mustUnitsAt(t, "120.01000")
	live := []Reservation{liveAt(t, "d1", "120.01000")}
	tol := ClampDepositTol(p, DefaultDepositMatchTolUnits)

	// Walk the whole shortfall window in 1000-unit (0.01 USDT) steps.
	for gap := int64(p.Tol) + 1; gap <= p.FeeAllowance(quoted); gap += 1000 {
		recv := quoted - gap
		if _, kind := MatchReceipt(p, recv, live); kind != MatchShortfall {
			t.Fatalf("gap %d: MatchReceipt = %s, want shortfall", gap, kind)
		}
		// desiredVND is deliberately LARGER than recvVND here — that is the only
		// configuration in which the round-up could fire. It must not.
		const desiredVND, recvVND = 3_000_000, 2_900_000
		credit, within := DepositCredit(recv, quoted, desiredVND, recvVND, tol)
		if within || credit != recvVND {
			t.Errorf("gap %d: DepositCredit = (%d, within=%v), want (%d, false) — a fee shortfall "+
				"must credit what ARRIVED; rounding it up to the quote re-opens the money pump "+
				"the proportional allowance exists to close", gap, credit, within, recvVND)
		}
	}
}

// TestMatchIncomingIsAtomicOverTheRegistry pins the registry entry point the
// watcher will use: same-provider only, unknown provider is a loud error rather
// than a silent "no match", and the verdicts match the pure function's.
func TestMatchIncomingIsAtomicOverTheRegistry(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderTron, "trx-1", "7", "120.01000", tLive)
	adopt(t, r, ProviderBEP20, "bsc-1", "9", "120.31000", tLive)

	recv := mustUnitsAt(t, "120.01000")
	res, kind, err := r.MatchIncoming(ProviderTron, recv)
	if err != nil || kind != MatchExact || res.Des != "trx-1" {
		t.Errorf("MatchIncoming = (%s, %s, %v), want (trx-1, exact, nil)", res.Des, kind, err)
	}
	// The BEP20 row is 0.30 above and would be a shortfall candidate on its own
	// rail — but a TRON payment can only belong to a TRON session, so it must not
	// enter the TRON verdict at all.
	if _, kind, _ := r.MatchIncoming(ProviderTron, mustUnitsAt(t, "120.01000")); kind == MatchAmbiguous {
		t.Error("a foreign-rail row made a TRON receipt ambiguous — attribution is same-provider")
	}
	// Unknown provider must FAIL LOUDLY: the caller is deciding whether to move
	// money, so a silent MatchNone would be indistinguishable from a real answer.
	if _, kind, err := r.MatchIncoming("nosuch", recv); err == nil {
		t.Errorf("MatchIncoming on an unknown provider = (%s, nil error), want an error", kind)
	}
}
