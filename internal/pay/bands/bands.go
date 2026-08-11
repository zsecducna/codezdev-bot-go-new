// Package bands ports the unique-amount BAND ALLOCATION logic that binds an
// incoming USDT payment to exactly ONE order across every USDT rail
// (Binance Pay, BEP20, TRON).
//
// WHY THIS EXISTS. On-chain / Binance-Pay credits carry no memo or order id.
// The only thing that ties a received amount to an order is the amount itself.
// So every pending order is handed a UNIQUE amount = base + k*step, and a
// credit within +/-tol of that amount settles that order. For the mapping to
// stay 1:1 the live amounts must be spaced at least 2*tol+1 apart, so a single
// received amount can never fall inside two bands at once
// (telegram_bot.py:9070-9092, bep20_pay.py:44-58, tron_pay.py:42-54,
// binance_pay.py:50-58).
//
// PORTED FROM (Kiro Python original, /Users/z/Desktop/Kiro/telegram_bot.py):
//   - _unique_usdt_amount        @9070  the picker (step bands, slot cap, last-resort down-slot)
//   - _binance_unique_amount     @9095  two-pass wrapper (retry with stale rows evicted)
//   - _bep20_unique_amount       @9397  same wrapper, BEP20 params
//   - _tron_unique_amount        @9888  same wrapper, TRON params
//   - _usdt_collision_amounts    @9431  which live rows block a slot
//   - USDT_BAND_EVICT_S          @9428  pressure-only soft eviction window
//   - USDT_PENDING_GRACE_S       @9548  how long a dead row keeps its band
//   - _prune_usdt_pending        @9600  hard sweep that frees bands
//   - _usdt_own_sibling          @9620  same-chat sibling redirect (depth-1)
//   - _usdt_recv_matches_band    @9635  claim-jack band gate on receipt
//   - deposit credit decision    @9748  within-30c-under -> desired, else actual
//
// INCIDENT THIS CODE IS HARDENED AGAINST (prod audit 2026-07-02, see
// tests/test_usdt_redirect.py header in the Python repo): one customer's
// abandoned sessions held 3 of the 4 upward slots for a price for the whole 2h
// pending grace, making that price point unbuyable for EVERYONE ("band
// exhausted"), while their own already-sent payment dead-ended on
// "amount_conflict" because it matched their OLD session's band. The two fixes
// are both in this package: (a) a chat's own DEAD sessions never block that
// chat's next allocation, plus a pressure-only eviction pass, and (b) the
// same-chat sibling redirect.
//
// DELIBERATE DEVIATIONS FROM THE PYTHON (each documented, each strictly safer):
//
//  1. CROSS-PROVIDER COLLISION SET. Python keeps three separate registries
//     (_binance_pending / _bep20_pending / _tron_pending) and applies
//     _usdt_collision_amounts to each in isolation, so a Binance amount could
//     equal a live TRON amount. This package holds ONE registry whose collision
//     set spans ALL providers, per the port's requirement. The spacing enforced
//     against a FOREIGN row is max(tolA,tolB)*2+1, i.e. full band-disjointness
//     in both directions, not merely the requesting rail's own gap — "cannot
//     collide" has to hold from either side's tolerance. That is a SUPERSET of
//     what Python avoids, so every Python non-collision guarantee still holds;
//     the only cost is capacity (fewer free slots per price under load), which
//     the eviction passes above exist to reclaim.
//  2. RE-RESERVING THE SAME ORDER. Python's collision set never excludes by
//     des, so re-picking for a des whose row is still live yields a NEW amount.
//     Registry.Reserve excludes the requester's own row, so re-opening a pay
//     screen for the same order shows the SAME amount instead of burning a
//     second slot. A row cannot claim-jack itself, so nothing is weakened.
//  3. DETERMINISM WHERE PYTHON LEANED ON DICT ORDER. Go map iteration is
//     randomised, so Registry.ResolveRedirect picks the freshest matching
//     sibling (des string as tie-break) rather than the first-inserted one.
//  4. ROUNDING MODE. Python's Decimal.to_integral_value() is ROUND_HALF_EVEN;
//     AmountUnits rounds half-away-from-zero (matching this repo's existing
//     bep20/tron ratToIntegral). Amount strings are always emitted at exactly
//     AMOUNT_DECIMALS precision, so the rounding step is a no-op on every real
//     input and the two modes can only differ on a malformed extra digit.
//
// Stdlib only. All money math is integer "amount units" (cents at 2 decimals)
// or big.Rat for decimal-string parsing — never float, per the port's
// money-safety rule.
package bands

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strconv"
)

// ErrBandExhausted is returned by Reserve/UniqueAmount when every slot for a
// price is ambiguous. Python returns None here and the handlers answer "we're
// busy, try again" — refusing the sale is CORRECT: handing out a colliding
// amount would let one payment settle the wrong order
// (telegram_bot.py:9092 "no clear slot: refuse rather than collide").
var ErrBandExhausted = errors.New("bands: band exhausted (no unambiguous amount slot for this price)")

// Params is one rail's band geometry. Rails build it from their own env-backed
// getters, e.g. bands.Params{Step: bep20.AmountOffsetStep(), Tol:
// bep20.AmountMatchTol(), Decimals: bep20.AmountDecimals()}.
//
// Defaults in the Python original:
//
//	BEP20   step 21, tol 10, dec 2 (bep20_pay.py:40,54-55)
//	TRON    step 21, tol 10, dec 2 (tron_pay.py:41,50-51)
//	Binance step 10, tol  4, dec 2 (binance_pay.py:54-55; cents, no dec knob)
type Params struct {
	// Step is AMOUNT_OFFSET_STEP: spacing between consecutive slots, in
	// amount units.
	Step int
	// Tol is AMOUNT_MATCH_TOL: half-width of a band, in amount units. A
	// receipt within +/-Tol of an amount matches it.
	Tol int
	// Decimals is AMOUNT_DECIMALS: the precision of the amount strings this
	// rail hands out (2 = cents).
	Decimals int
}

// MinGap is the smallest distance two live amounts may have and still keep
// their +/-Tol bands disjoint: 2*tol+1 (telegram_bot.py:9084 `min_gap`).
func (p Params) MinGap() int { return 2*p.Tol + 1 }

// SpacingOK reports whether Step >= MinGap. Python only WARNs when it is not
// (bep20_pay.py:56-58 / tron_pay.py:52-54 / binance_pay.py:56-58): a smaller
// step never creates ambiguity — the picker still enforces MinGap per slot —
// it only reduces how many concurrent same-price orders fit.
func (p Params) SpacingOK() bool { return p.Step >= p.MinGap() }

// SlotSpanWholeUnits is how many WHOLE units of price the upward slot search
// spans: the picker tries base, base+step, ... up to
// base + SlotSpanWholeUnits (exclusive) before giving up upward and, for
// callers that allow it, falling back to the single last-resort downward
// slot (telegram_bot.py:9085 originally spanned exactly 1 whole unit; ported
// as `scale // step`).
//
// TRADEOFF (hands-free-deposits ruling (b)): doubled from 1 to 2 whole units
// to buy back headroom the deposit unique-amount work now spends one-way —
// a live band is held for the full customer-facing deposit window (up to an
// hour) and a deposit can no longer fall back to the downward slot (see
// ReserveUpwardOnly), so more upward slots means fewer customers refused
// USDT_BUSY under concurrent same-price traffic. The cost is a wider
// worst-case gap between the amount a customer is quoted and the amount they
// actually asked for — up to just under SlotSpanWholeUnits whole units (e.g.
// up to ~1.89 USDT at the shipped 21-cent BEP20/TRON step) before the last
// upward slot is tried.
const SlotSpanWholeUnits = 2

// Slots is the number of UPWARD slots the picker will try:
// SlotSpanWholeUnits*scale//step, i.e. how many `base + k*step` values stay
// strictly below base+SlotSpanWholeUnits (telegram_bot.py:9085 `for k in
// range(scale // step)`, widened by SlotSpanWholeUnits). At the shipped
// BEP20/TRON defaults (step 21, decimals 2) that is 200//21 = 9; Binance
// (step 10) gets 200//10 = 20. One extra LAST-RESORT slot at base-step exists
// below these for callers that allow it (telegram_bot.py:9089).
func (p Params) Slots() int {
	return int(int64(SlotSpanWholeUnits) * pow10Int64(p.Decimals) / int64(p.Step))
}

// Validate rejects geometry the picker cannot work with.
func (p Params) Validate() error {
	if p.Step <= 0 {
		return fmt.Errorf("bands: Step must be > 0, got %d", p.Step)
	}
	if p.Tol < 0 {
		return fmt.Errorf("bands: Tol must be >= 0, got %d", p.Tol)
	}
	if p.Decimals < 0 || p.Decimals > 9 {
		return fmt.Errorf("bands: Decimals must be in [0,9], got %d", p.Decimals)
	}
	if p.Slots() < 1 {
		return fmt.Errorf("bands: Step %d exceeds one whole unit at %d decimals (no slots)", p.Step, p.Decimals)
	}
	return nil
}

// maxWholeUnits bounds a parsed amount at 10 million WHOLE USDT, whatever the
// rail's precision (so the cap is 10^7 * 10^dec units — it scales with Decimals
// instead of silently shrinking to 1,000 USDT at 6 decimals). No real amount in
// this shop comes near it; the point is that a CORRUPTED persisted row (Adopt
// parses strings straight out of SQLite) fails loudly instead of wrapping int64
// somewhere downstream.
//
// It also makes every downstream multiplication provably overflow-free:
// rescaleUnits' up-scale is bounded by the same whole-unit cap at the target
// precision, i.e. 10^7 * 10^9 = 10^16 at the Decimals<=9 ceiling, well under
// int64's 9.22e18; the picker's baseU + k*step stays within one unit of that.
const maxWholeUnits = 10_000_000

// amountRE is an ALLOWLIST of the only syntax a stored amount may have: an
// optional minus, digits, optionally a fractional part. big.Rat.SetString is far
// more permissive — it accepts "3/2", "1e100" and, less obviously, base prefixes
// ("0x10" parses as 16, so a corrupted row could read as 16.00 with no error).
// Python's Decimal(str(x)) raises on all of those, and on a money path an
// unexpected syntax must be an error, never a silently different number.
var amountRE = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// AmountUnits returns the integer amount-unit value of a USDT amount string at
// `dec` precision (e.g. "5.23" -> 523 at 2 decimals). big.Rat parsing mirrors
// Python's Decimal math — no float drift
// (bep20_pay.py:97-100 / tron_pay.py:98-101).
func AmountUnits(amountStr string, dec int) (int64, error) {
	if !amountRE.MatchString(amountStr) {
		return 0, fmt.Errorf("bands: amount %q is not a plain decimal string", amountStr)
	}
	r, ok := new(big.Rat).SetString(amountStr)
	if !ok {
		return 0, fmt.Errorf("bands: invalid amount %q", amountStr)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).SetInt64(pow10Int64(dec))))
	u, ok := ratToIntegral(r)
	max := maxWholeUnits * pow10Int64(dec)
	if !ok || u > max || u < -max {
		return 0, fmt.Errorf("bands: amount %q is out of range at %d decimals", amountStr, dec)
	}
	return u, nil
}

// FormatUnits renders amount units back to the `dec`-decimal string the
// customer is shown (523, 2 -> "5.23"). Python does `f"{cu/scale:.{dec}f}"`
// (telegram_bot.py:9088); integer formatting here is exact for every value,
// including ones a float64 divide would render off-by-a-cent.
func FormatUnits(units int64, dec int) string {
	if dec <= 0 {
		return strconv.FormatInt(units, 10)
	}
	neg := units < 0
	if neg {
		units = -units
	}
	scale := pow10Int64(dec)
	s := fmt.Sprintf("%d.%0*d", units/scale, dec, units%scale)
	if neg {
		s = "-" + s
	}
	return s
}

// UniqueAmount is the Python-shaped entry point to the picker: pick the
// smallest amount = base + k*step whose +/-tol band is clear of EVERY live
// amount in `used`, so an incoming payment within tol of it maps to exactly
// one order even across products (telegram_bot.py:9070-9092).
//
// Slot order, exactly as in Python:
//  1. UPWARD slots base, base+step, ... for k < Slots(). Upward is preferred
//     because it never undercharges.
//  2. ONE downward slot base-step (e.g. 5.79 for a 6.00 price) as a LAST
//     RESORT when every upward slot is taken. The seller absorbs that <=step
//     undercharge, which beats refusing the sale outright
//     (telegram_bot.py:9077-9080, 9089-9091).
//
// Returns ErrBandExhausted when every slot is ambiguous.
func UniqueAmount(p Params, base string, used []string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	baseU, err := AmountUnits(base, p.Decimals)
	if err != nil {
		return "", err
	}
	usedU := make([]blocker, 0, len(used))
	for _, a := range used {
		u, err := AmountUnits(a, p.Decimals)
		if err != nil {
			return "", err
		}
		usedU = append(usedU, blocker{units: u, minGap: int64(p.MinGap())})
	}
	// true: UniqueAmount is the general-purpose picker (no production caller
	// since the deposit path moved to Registry.ReserveUpwardOnly) — it keeps
	// the original allow-downward behavior every existing test here pins.
	cu, ok := uniqueAmountUnits(p, baseU, usedU, true)
	if !ok {
		return "", ErrBandExhausted
	}
	return FormatUnits(cu, p.Decimals), nil
}

// blocker is one live amount the picker must dodge, carrying the minimum
// distance that keeps ITS band disjoint from the candidate's. Same-rail rows use
// the rail's own MinGap (2*tol+1, telegram_bot.py:9084); a row from ANOTHER rail
// with a wider tolerance carries the wider gap, so disjointness holds from both
// sides (see deviation 1 in the package doc).
type blocker struct {
	units  int64
	minGap int64
}

// uniqueAmountUnits is the picker in integer units. Callers have already
// normalised `usedU` to p.Decimals. Pure and allocation-free so the registry
// can call it under its mutex.
//
// allowDownward gates the single last-resort slot BELOW base
// (telegram_bot.py:9089-9091, e.g. 5.79 for a 6.00 price): a product order
// may absorb that <=step undercharge as a seller cost rather than refuse the
// sale, but a DEPOSIT must never be quoted below what the customer asked to
// send — DepositCredit rounds an exact match up to the customer's full
// requested total regardless of which band amount they were actually quoted,
// so a downward deposit slot is a straight giveaway, not a courtesy. Callers
// that must never underquote (Registry.ReserveUpwardOnly) pass false and
// accept ErrBandExhausted instead once the upward slots run out.
func uniqueAmountUnits(p Params, baseU int64, usedU []blocker, allowDownward bool) (int64, bool) {
	step := int64(p.Step)
	// Upward slots first: base, base+step, ... strictly below
	// base+SlotSpanWholeUnits. Upward is preferred because it never
	// undercharges.
	for k := int64(0); k < int64(p.Slots()); k++ {
		cu := baseU + k*step
		if slotClear(cu, usedU) {
			return cu, true
		}
	}
	if !allowDownward {
		return 0, false
	}
	// Last resort: the single slot BELOW base (telegram_bot.py:9089-9091).
	// `cu > 0` guards a price so small that base-step would be zero/negative.
	if cu := baseU - step; cu > 0 && slotClear(cu, usedU) {
		return cu, true
	}
	return 0, false
}

// slotClear reports whether candidate `cu` is at least each blocker's minGap
// away from it — i.e. its +/-tol band overlaps none of theirs
// (telegram_bot.py:9086 `all(abs(cu - u) >= min_gap for u in used_u)`).
func slotClear(cu int64, usedU []blocker) bool {
	for _, b := range usedU {
		if abs64(cu-b.units) < b.minGap {
			return false
		}
	}
	return true
}

// RecvMatchesBand reports whether a received amount (already floored to
// `p.Decimals` units by the rail's chain reader) settles `expected`: the fuzzy
// +/-Tol match, so a customer who drops a cent or two still confirms. This is
// the `matched` decision at telegram_bot.py:9745
// (`abs(recv_units - want_units) <= mod.AMOUNT_MATCH_TOL`).
//
// An underpay of up to Tol is absorbed as goodwill; the picker's MinGap
// spacing is what guarantees a receipt matches at most ONE live amount
// (bep20_pay.py:44-53).
func RecvMatchesBand(p Params, expected string, recvUnits int64) (bool, error) {
	want, err := AmountUnits(expected, p.Decimals)
	if err != nil {
		return false, err
	}
	return abs64(recvUnits-want) <= int64(p.Tol), nil
}

// RecvMatchesAnyBand reports whether `recvUnits` is within Tol of ANY amount in
// `amounts` (other live orders' unique amounts). The rails use it to REFUSE a
// mismatched payment that lands in ANOTHER order's band — a claim-jack attempt
// — instead of crediting the wrong session (telegram_bot.py:9635-9642).
//
// A malformed amount string is skipped rather than failing the whole gate: the
// caller is deciding whether to REFUSE money, and an unparseable neighbour row
// must not be able to turn a refusal into an accept or vice versa. Skipping is
// the conservative side — it can only narrow the refusal set, never widen it.
func RecvMatchesAnyBand(p Params, amounts []string, recvUnits int64) bool {
	tol := int64(p.Tol)
	for _, a := range amounts {
		u, err := AmountUnits(a, p.Decimals)
		if err != nil {
			continue
		}
		if abs64(recvUnits-u) <= tol {
			return true
		}
	}
	return false
}

// rescaleUnits converts amount units between two decimal precisions so rows
// from a rail with different AMOUNT_DECIMALS can share one collision set. Down
// scaling FLOORS, matching every other units conversion in the port (floor so a
// dust overpay still matches and a dust underpay never credits more —
// bep20_pay.py:103-107).
//
// CAVEAT for anyone changing a rail's AMOUNT_DECIMALS: MinGap (2*tol+1) is the
// EXACT minimum only between SAME-precision bands. Across precisions the
// receipt-flooring semantics give a COARSER rail's band one extra coarse unit
// of reach past its top edge, so Registry.collisionUnits widens the gap against
// a coarser foreign row by 10^(reqDec-rowDec)-1 (see the COARSE-NEIGHBOUR SLACK
// comment there). With every shipped rail at Decimals=2 the rescale here is
// identity and the slack is zero.
func rescaleUnits(units int64, from, to int) int64 {
	switch {
	case from == to:
		return units
	case to > from:
		return units * pow10Int64(to-from)
	default:
		d := pow10Int64(from - to)
		if units < 0 { // floor for negatives too (Go's / truncates toward zero)
			return -((-units + d - 1) / d)
		}
		return units / d
	}
}

// rescaleTolCeil converts a TOLERANCE between two decimal precisions, rounding
// UP on a down-scale. Unlike an amount, a tolerance floored down would understate
// the gap the picker must leave — always err toward the wider band.
func rescaleTolCeil(tol, from, to int) int {
	switch {
	case from == to:
		return tol
	case to > from:
		return tol * int(pow10Int64(to-from))
	default:
		d := int(pow10Int64(from - to))
		return (tol + d - 1) / d
	}
}

func pow10Int64(n int) int64 {
	v := int64(1)
	for range n {
		v *= 10
	}
	return v
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ratToIntegral rounds a big.Rat to the nearest integer, half-away-from-zero
// (mirrors bep20.ratToIntegral / tron.ratToIntegral in this repo). ok is false
// when the rounded value does not fit int64 — big.Int.Int64() is undefined in
// that case, and a silently wrapped amount on a money path is unacceptable.
func ratToIntegral(r *big.Rat) (v int64, ok bool) {
	num, den := r.Num(), r.Denom() // big.Rat guarantees Denom() > 0
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem) // truncated division; rem's sign matches num's
	rem.Abs(rem)
	if new(big.Int).Lsh(rem, 1).Cmp(den) >= 0 { // 2*|rem| >= den -> away from zero
		if num.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
