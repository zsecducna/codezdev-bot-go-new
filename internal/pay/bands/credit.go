package bands

// ---- deposit credit decision ------------------------------------------------
//
// A DEPOSIT is not a pay-for-order: it credits the SUBMITTER's own wallet, so
// there is no claim-jack band gate — a neighbour-band collision must not
// false-reject a deposit settling against its own stored amount. As of the
// hands-free-deposits work, a deposit DOES take a unique offset from the same
// picker product orders use (handlers.reserveUSDTAmount): `expected` below is
// that PICKED amount — the request plus a 0.01000-0.01999 USDT jitter, so a
// "120" top-up is quoted "120.01000" or the next free MinGap-spaced slot above
// it, NEVER the round request itself. The tolerance math is unchanged, it is
// just measured against the picked amount instead of the raw request. The
// window + confirmed check + hash-dedupe remain the deposit's other defenses
// (telegram_bot.py:9479-9488, 9748-9756).

// DefaultDepositMatchTolUnits is DEPOSIT_MATCH_TOL_UNITS
// (telegram_bot.py:9552-9555's DEPOSIT_MATCH_TOL_CENTS): a received amount this
// close to the amount the customer was asked to send credits at least that
// asked-for amount, covering wallet rounding.
//
// RENAMED FROM DefaultDepositMatchTolCents, AND RE-VALUED FROM 30, by the
// hands-free-deposits geometry ruling — both halves matter:
//
//   - The value is in AMOUNT UNITS at the rail's Decimals, which are now 1e-5
//     USDT, not cents. Leaving the name saying "Cents" while the unit is a
//     hundred-thousandth invites an operator to set 30 meaning 0.30 USDT.
//     30000 units IS 0.30 USDT — thirty times the entire 0.01-0.02 jitter
//     window and ~297 band widths — and a tolerance that wide would call one
//     receipt a match for hundreds of live orders' amounts at once. The old
//     name is the trap; deleting it is the fix.
//   - 50 is the LARGEST value that provably cannot span two bands. Live
//     amounts are >= MinGap = 2*Tol+1 = 101 units apart, so if a receipt were
//     within 50 of two of them those two would be at most 100 apart — a
//     contradiction. 51 would not have that proof (2*51 = 102 >= 101). 50 is
//     also exactly the rail's own band half-width, so "the credit rule
//     forgives it" and "the rail calls it this band" mean the same thing.
//
// Worth recording, because it is why shrinking from "30 cents" costs the
// customer nothing: AT A STABLE EXCHANGE RATE the picker's jitter is at least
// 0.01000 USDT, so any receipt within tolerance of the picked amount is worth
// strictly MORE than the customer's requested total, recvVND > desiredVND, and
// DepositCredit's round-a-small-underpay-up branch does not fire.
//
// It is NOT unreachable, though, and the difference matters. The quote is
// priced at the rate in force when the screen was drawn; the receipt is valued
// at the rate in force when the hash is verified, which can be up to an hour
// later (the deposit window). A downward rate move of even ~0.02% between the
// two makes recvVND fall below the requested total, and then this tolerance is
// what decides whether the customer is credited what they asked for or the
// slightly smaller amount the new rate says they sent. Rare, real, and
// customer-favourable when it fires.
const DefaultDepositMatchTolUnits = 50

// DepositMatchTolUnits reads DEPOSIT_MATCH_TOL_UNITS (in amount units, i.e.
// 1e-5 USDT at the shipped 5 decimals).
func DepositMatchTolUnits() int {
	return envInt("DEPOSIT_MATCH_TOL_UNITS", DefaultDepositMatchTolUnits)
}

// ClampDepositTol caps a configured deposit tolerance at the rail's own band
// half-width p.Tol, and floors it at 0.
//
// This is the structural guard behind the proof above: the disjointness
// argument holds for any tol <= p.Tol, because MinGap is 2*p.Tol+1 by
// construction, and it holds for NO larger value. p.Tol is operator-tunable
// (*_MATCH_TOL) and so is the deposit tolerance (DEPOSIT_MATCH_TOL_UNITS), and
// the two knobs are set independently in different sections of .env — so
// "they were configured consistently" is an assumption, not a fact. Clamping
// makes a receipt-matches-two-orders configuration unreachable instead of
// merely undocumented.
func ClampDepositTol(p Params, tolUnits int) int {
	if tolUnits < 0 {
		return 0
	}
	if tolUnits > p.Tol {
		return p.Tol
	}
	return tolUnits
}

// DepositCredit is the deposit credit decision as a pure function — the exact
// boundary logic at telegram_bot.py:9748-9761:
//
//	desired_units = amount_units(expected)                  # what they were asked to send
//	if abs(recv_units - desired_units) <= DEPOSIT_MATCH_TOL_UNITS:
//	    credit_vnd = max(int(o["total"]), recv_vnd)
//	else:
//	    credit_vnd = recv_vnd
//
// Two rules ride on that `max`, and both are pinned by the Python
// tests/test_deposit_credit.py acceptance suite:
//
//   - UNDERPAY within tolerance is rounded UP to the desired top-up rather than
//     short-crediting a confusing amount. At a STABLE exchange rate this branch
//     does not fire on the deposit path — see DefaultDepositMatchTolUnits: the
//     picker's jitter puts the quoted amount at least 0.01 USDT above the
//     requested total, so a receipt within 0.00050 of it is still worth more
//     than the request and the overpay rule below wins. It fires when the rate
//     moves DOWN between the quote and the verify, which the hour-long deposit
//     window makes a real occurrence, not a theoretical one.
//   - OVERPAY within tolerance credits the ACTUAL received amount: a customer
//     quoted 120.01000 and credited for a 120 request gets 120.01000's value,
//     never the smaller desired 120. The customer is never shorted — which is
//     what makes the always-upward jitter free for them.
//
// Anything further off than the tolerance (e.g. 200 for a 120 request) credits
// what actually ARRIVED — money is never lost, and a deposit never rejects.
//
// `recvUnits`/`desiredUnits` are amount units (1e-5 USDT at the shipped 5
// decimals) at the rail's AMOUNT_DECIMALS; `desiredVND`/`recvVND` are the VND
// values of the requested and the received amounts. `tolUnits` is
// DepositMatchTolUnits(), and callers MUST pass it through ClampDepositTol so
// no configuration can widen it past the rail's band half-width.
//
// withinTol is returned for the caller's audit line; it does NOT mean the credit
// equals desiredVND (see the overpay rule above).
func DepositCredit(recvUnits, desiredUnits int64, desiredVND, recvVND int64, tolUnits int) (creditVND int64, withinTol bool) {
	if abs64(recvUnits-desiredUnits) <= int64(tolUnits) {
		// Round a tiny UNDERpay up to the desired top-up but NEVER credit less
		// than what actually arrived (telegram_bot.py:9758-9761).
		if recvVND > desiredVND {
			return recvVND, true
		}
		return desiredVND, true
	}
	return recvVND, false // off-amount self-deposit -> credit actual
}
