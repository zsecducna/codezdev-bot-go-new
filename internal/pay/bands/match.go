package bands

import "fmt"

// ---- attribution by amount alone --------------------------------------------
//
// WHAT THIS FILE IS FOR, and how it differs from everything else in the package.
//
// The rest of `bands` answers "the customer told us which order this is — does
// the amount agree?". That is the verify-by-hash path: the customer pastes a tx
// hash on their own pay screen, so the order is NAMED, and MinGap only has to
// stop a payment being claimed by a DIFFERENT session.
//
// This file answers the harder question the hands-free watcher will ask: "a
// payment arrived and nobody told us whose it is — which live order, if any,
// does this amount identify?". Nothing calls it in production yet; the watcher
// is a later task. It is written and tested now because the RULE is what the
// fee-forgiveness ruling actually decided, and getting it wrong later means
// crediting the wrong customer.
//
// THE RULING (user amendment, 2026-08-11). Two payment shapes must both settle:
//
//  1. the customer sent quoted + fee, so exactly the quoted amount ARRIVES —
//     the clean case, and the one the pay screen instructs;
//  2. the customer sent exactly the quoted amount but their wallet or exchange
//     deducted the network fee OUT of the transfer, so `quoted - fee` arrives.
//
// Shape 2 is what forces everything below. A fee allowance is orders of
// magnitude wider than the 0.01000-0.01999 jitter that makes amounts unique, so
// one short receipt sits inside the allowance of MANY live bands at once. No
// jitter that tight can make a 0.5 USDT window unambiguous, and MinGap does not
// help — MinGap governs the EXACT path only.
//
// So attribution stops being "the amount identifies the band" and becomes:
//
//	EXACT first. At most one live band can be within +/-Tol (that is exactly
//	what MinGap buys), so an exact receipt settles even while other bands are
//	live and even while it sits inside their shortfall windows.
//
//	SHORTFALL second, and ONLY when exactly one live band is a candidate. Two
//	or more candidates means the receipt cannot be attributed: credit NOTHING
//	and route it to the unmatched / owner-alert path for a human to attribute.
//
// Refusing to guess is not a workaround, it is the only sound rule. Crediting
// an ambiguous payment credits the WRONG CUSTOMER, which is strictly worse than
// making an operator look at one deposit. At the shop's real volume (peak about
// one deposit per thirty minutes) ambiguity should almost never fire — but it
// must be handled rather than assumed away.

// FeeShortfallPercent and the 0.5 USDT cap define the shortfall allowance:
//
//	allowance = min(0.5 USDT, FeeShortfallPercent% of the QUOTED amount)
//
// WHY PROPORTIONAL, not a flat 0.5. The reason to forgive a fee at all is that
// a bigger transfer carries a bigger fee, so the absorbed loss stays a small
// fraction of the deposit. That is true for large deposits and FALSE for small
// ones, and a flat cap does not express it.
//
// MIN_DEPOSIT_USDT is 1 (handlers/deposit.go). Under a flat 0.5 a customer
// quoted 1.01437 who sends 0.52 is short 0.49 — inside the allowance — and
// would be attributed. That is a ~50% giveaway and it is repeatable ON PURPOSE:
// send half, get attributed whole. On TRON a sender with staked energy pays
// almost nothing per attempt, so it is a money pump rather than an edge case.
// The proportional cap closes it without touching large deposits: 200 USDT
// still forgives the full 0.5, 1 USDT forgives at most 0.05, and the flat 0.5
// becomes the binding limit from 10 USDT upward.
//
// (The pump is dead twice over — see MatchShortfall on why a shortfall match
// credits what ARRIVED. This cap is what keeps ATTRIBUTION safe; crediting the
// actual amount is what removes the incentive. Both, deliberately.)
const FeeShortfallPercent = 5

// FeeShortfallCapHalfUnits expresses the 0.5 USDT ceiling as a fraction of a
// whole unit — halves — so it stays the same amount of money at any Decimals,
// the same way JitterLoHundredths does. A hardcoded unit count would silently
// mean 0.5 USDT at 5 decimals and 0.00005 USDT at 9.
const FeeShortfallCapHalfUnits = 1

// FeeAllowance is the largest shortfall, in amount units, that may still be
// attributed to a band quoted at `quotedUnits`.
//
// COMPUTED FROM THE QUOTE, NEVER FROM WHAT ARRIVED. Deriving it from the
// received amount inverts the rule: a bigger underpayment would yield a smaller
// received figure and therefore a smaller allowance, so the allowance would
// shrink exactly when it needs to bind — and a large shortfall could argue
// itself into range. The quoted amount is the fixed, shop-chosen reference; the
// received amount is attacker-controlled.
//
// Integer floor on the percentage, which rounds in the shop's favour.
//
// Overflow-free: quotedUnits is bounded by maxWholeUnits*10^Decimals = 10^16 at
// the Decimals<=9 ceiling, and 10^16 * 5 is 5e16, well under int64's 9.22e18.
func (p Params) FeeAllowance(quotedUnits int64) int64 {
	if quotedUnits <= 0 {
		return 0
	}
	// hardCap, not `cap` — shadowing the builtin in money code is exactly the
	// kind of small confusion that survives review.
	hardCap := pow10Int64(p.Decimals) / 2 * FeeShortfallCapHalfUnits
	if pct := quotedUnits * FeeShortfallPercent / 100; pct < hardCap {
		return pct
	}
	return hardCap
}

// MatchKind is the verdict of MatchReceipt. The zero value is MatchNone, so a
// caller that forgets to check defaults to "do not credit".
type MatchKind int

const (
	// MatchNone: no live band can claim this receipt. The caller must NOT
	// credit against a band — bank it to the payer's wallet if the payer is
	// known, otherwise route it to the unmatched path.
	MatchNone MatchKind = iota

	// MatchExact: the receipt is within +/-Tol of exactly one live band. Settle
	// it normally, including DepositCredit's round-a-tiny-underpay-up goodwill
	// rule — inside Tol the difference is rounding dust or an exchange-rate
	// move, which is what that rule was written for.
	MatchExact

	// MatchShortfall: no band matches exactly, and exactly ONE live band is
	// within its fee allowance above the receipt. Attribute the payment to that
	// band.
	//
	// CREDIT WHAT ARRIVED, never the quoted amount. This is a decision, and it
	// is load-bearing:
	//
	//   - It kills the money pump structurally rather than by a bound. Send
	//     less, get credited less, always. FeeAllowance then only has to make
	//     ATTRIBUTION safe, which is a far smaller thing to get right than
	//     making a giveaway safe.
	//   - The goodwill round-up was calibrated for rounding dust. A network fee
	//     is not dust, and absorbing up to 0.5 USDT on an unbounded number of
	//     deposits is a real, uncapped cost.
	//   - The pay screen already tells the customer, in every language, that
	//     the fee is theirs and that they must add it on top if their platform
	//     skims. Forgiving the amount silently would contradict the instruction
	//     and train customers out of following it.
	//
	// This needs no enforcement code, which is the point — but the composition
	// rests on TWO things, and naming only the first is how it would get broken:
	//
	//   1. a shortfall candidate exists only when the gap EXCEEDS Tol (anything
	//      within Tol is MatchExact and never reaches here), and
	//   2. ClampDepositTol caps the configured deposit tolerance at p.Tol.
	//
	// Together those give gap > tolUnits always, so DepositCredit's own
	// tolerance test fails and it credits the actual received amount without
	// anyone special-casing it. Drop the CLAMP and (1) alone is not enough:
	// DEPOSIT_MATCH_TOL_UNITS=40000 would make a 0.3 shortfall on a 120 deposit
	// "within tolerance", round it UP to the quote, and hand the money pump
	// straight back.
	//
	// SCOPE: the reasoning above is about DEPOSITS, where crediting less is a
	// coherent outcome. A PRODUCT ORDER settled by shortfall has no such option
	// — stock is indivisible, so the order either delivers in full for up to
	// min(0.5, 5%) under its price, or does not deliver.
	//
	// That question was settled by NOT ASKING IT. Reservation.IsDeposit now
	// separates the two, and MatchIncoming — the only entry point the watcher
	// uses — considers deposit rows only, so no product order can reach this
	// verdict at all. MatchReceipt itself stays flag-agnostic: it is the pure
	// rule, and the day product orders want hands-free settlement they get a
	// deliberate decision of their own rather than inheriting this one.
	MatchShortfall

	// MatchAmbiguous: two or more live bands could claim this receipt. Credit
	// NOTHING and raise it for manual attribution. Never fall back to "closest"
	// or "first" — both are a coin flip with someone else's money.
	MatchAmbiguous
)

func (k MatchKind) String() string {
	switch k {
	case MatchExact:
		return "exact"
	case MatchShortfall:
		return "shortfall"
	case MatchAmbiguous:
		return "ambiguous"
	default:
		return "none"
	}
}

// MatchReceipt attributes `recvUnits` (already floored to p.Decimals by the
// rail's chain reader) to at most one of `live` by amount alone.
//
// `live` may carry rows at another rail's precision; each is rescaled into
// p.Decimals the same way the picker's collision set does.
//
// The returned Reservation is meaningful only for MatchExact and MatchShortfall.
//
// LIVENESS IS THE CALLER'S FILTER, not this function's: every row handed in is
// a candidate, including sessions past their deadline but inside the pending
// grace. That is right for the exact path — recovering a late payment is what
// the grace exists for — but it does inflate the SHORTFALL path's ambiguity
// rate, because a dead-but-in-grace band keeps competing for short receipts.
//
// Size the operator workload accordingly, because "ambiguity almost never
// fires" is true of exact matching and NOT of this: two customers depositing
// the same round amount are quoted 0.00101 apart, which is far inside any
// fee-shaped window, so a skimmed payment from either goes to manual
// attribution. That is the correct outcome — the alternative is crediting a
// coin flip — but it is a routine outcome, not a rare one, and the unmatched
// path must be built to be used rather than as a tripwire nobody watches.
//
// KIND IS ALSO THE CALLER'S FILTER. This function weighs every row it is
// handed, deposit or product order alike; MatchIncoming is what narrows the set
// to deposits (Reservation.IsDeposit). Anyone calling MatchReceipt directly with
// a mixed slice gets exactly what they asked for, which on the watcher's path
// would be a wallet credit against an undelivered order.
//
// AN OVERPAY beyond Tol is deliberately MatchNone rather than a match. The
// ruling defines two shapes and both are "at most the quote"; a receipt above a
// band by more than the tolerance is not evidence of that band, and guessing
// upward has no fee-shaped explanation behind it. Unattributed money is
// recoverable by hand; money credited to the wrong customer is not.
func MatchReceipt(p Params, recvUnits int64, live []Reservation) (Reservation, MatchKind) {
	// Pass 1 — EXACT. MinGap guarantees at most one hit for a sane geometry;
	// finding two means the geometry is broken, and the money-safe reading of a
	// broken geometry is "cannot attribute", not "pick one".
	var exact Reservation
	hits := 0
	for _, res := range live {
		if abs64(rescaleUnits(res.Units, res.Decimals, p.Decimals)-recvUnits) <= int64(p.Tol) {
			exact, hits = res, hits+1
		}
	}
	switch {
	case hits == 1:
		return exact, MatchExact
	case hits > 1:
		return Reservation{}, MatchAmbiguous
	}

	// Pass 2 — FEE SHORTFALL. Strictly one-directional and strictly beyond Tol
	// (anything inside Tol was already claimed by pass 1). The allowance is each
	// candidate's OWN, computed from ITS quote, because two live bands at
	// different prices forgive different amounts.
	var short Reservation
	hits = 0
	for _, res := range live {
		quoted := rescaleUnits(res.Units, res.Decimals, p.Decimals)
		gap := quoted - recvUnits
		if gap > 0 && gap <= p.FeeAllowance(quoted) {
			short, hits = res, hits+1
		}
	}
	switch hits {
	case 1:
		return short, MatchShortfall
	case 0:
		return Reservation{}, MatchNone
	}
	return Reservation{}, MatchAmbiguous
}

// MatchIncoming is MatchReceipt against the registry's own live DEPOSIT rows for
// one rail, taken atomically. This is the entry point the hands-free watcher
// wants: the candidate set must be read in ONE lock section, or a row reserved
// midway through the scan could make an ambiguous receipt look unambiguous.
//
// DEPOSITS ONLY (Reservation.IsDeposit), and that is a money rule, not a
// convenience. The watcher's one action is a wallet credit; a product order's
// band matching here would credit a wallet for an order whose stock is still
// sitting reserved and undelivered. Product orders are paid through their own
// hash-paste flow and are simply not candidates for attribution by amount.
// Filtering INSIDE the lock also keeps the ambiguity verdict honest: a product
// order sharing the window must not make a deposit's receipt look ambiguous, or
// a perfectly attributable payment would be held for manual review.
//
// Same-provider only, matching RecvMatchesAnyOtherBand: a payment arrives on one
// chain and can only belong to a session on that chain. (The picker's collision
// set spans providers so amounts never collide in the first place; attribution
// does not need to re-cross that line.)
//
// An unknown provider is an error, never a silent MatchNone — the caller is
// deciding whether to move money.
func (r *Registry) MatchIncoming(p Provider, recvUnits int64) (Reservation, MatchKind, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prm, ok := r.params[p]
	if !ok {
		return Reservation{}, MatchNone, fmt.Errorf("bands: unknown provider %q", p)
	}
	live := make([]Reservation, 0, len(r.rows[p]))
	for _, res := range r.rows[p] {
		if !res.IsDeposit {
			continue // a product order is never attributed by amount alone
		}
		live = append(live, res)
	}
	res, kind := MatchReceipt(prm, recvUnits, live)
	return res, kind, nil
}
