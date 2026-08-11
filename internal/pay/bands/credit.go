package bands

// ---- deposit credit decision ------------------------------------------------
//
// A DEPOSIT is not a pay-for-order: it credits the SUBMITTER's own wallet, so
// there is no claim-jack band gate — a neighbour-band collision must not
// false-reject a deposit settling against its own stored amount. As of the
// hands-free-deposits work, a deposit DOES take a unique offset from the same
// picker product orders use (handlers.reserveUSDTAmount): `expected` below is
// that PICKED amount (a round "120.00" when its band is free, else the next
// MinGap-spaced slot), not necessarily the customer's original round request —
// the tolerance math is unchanged, it is just measured against the picked
// amount instead of the raw request. The window + confirmed check + hash-dedupe
// remain the deposit's other defenses (telegram_bot.py:9479-9488, 9748-9756).

// DefaultDepositMatchTolCents is DEPOSIT_MATCH_TOL_CENTS
// (telegram_bot.py:9552-9555): a received amount this close to the customer's
// desired top-up credits exactly the desired amount (covers wallet rounding / a
// dropped cent). 30 = 30 cents, per the shop's customer-facing "max 30c diff"
// rule.
const DefaultDepositMatchTolCents = 30

// DepositMatchTolCents reads DEPOSIT_MATCH_TOL_CENTS (in amount units, i.e.
// cents at 2 decimals).
func DepositMatchTolCents() int {
	return envInt("DEPOSIT_MATCH_TOL_CENTS", DefaultDepositMatchTolCents)
}

// DepositCredit is the deposit credit decision as a pure function — the exact
// boundary logic at telegram_bot.py:9748-9761:
//
//	desired_units = amount_units(expected)                  # what they were asked to send
//	if abs(recv_units - desired_units) <= DEPOSIT_MATCH_TOL_CENTS:
//	    credit_vnd = max(int(o["total"]), recv_vnd)
//	else:
//	    credit_vnd = recv_vnd
//
// Two rules ride on that `max`, and both are pinned by the Python
// tests/test_deposit_credit.py acceptance suite:
//
//   - UNDERPAY within tolerance is rounded UP to the desired top-up: a wallet
//     that sent 119.99 (or 119.71, 29c short) still credits the full 120. The
//     seller absorbs <=30c rather than short-crediting a confusing amount.
//   - OVERPAY within tolerance credits the ACTUAL received amount: 120.30
//     credits 120.30, never the smaller desired 120. The customer is never
//     shorted.
//
// Anything further off than the tolerance (e.g. 200 for a 120 request) credits
// what actually ARRIVED — money is never lost, and a deposit never rejects.
//
// `recvUnits`/`desiredUnits` are amount units (cents) at the rail's
// AMOUNT_DECIMALS; `desiredVND`/`recvVND` are the VND values of the requested
// and the received amounts. `tolUnits` is DepositMatchTolCents().
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
