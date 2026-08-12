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
// PRECISION (hands-free-deposits ruling, 2026-08-11). Amount units are
// 1e-5 USDT (Decimals=5), not cents. The picker adds a jitter of 0.01000 to
// 0.01999 USDT to the price so the amount identifies ONE order, and an EXACT
// match is +/-0.00050. See JitterLoHundredths for the ruling and Params.Slots
// for the resulting geometry.
//
// The exact path is not the whole matching story any more. A later user
// amendment added FEE FORGIVENESS: a payment short by up to
// min(0.5 USDT, 5% of the quote) is also attributable, because some senders
// deduct the network fee out of the transfer. That window is far too wide for
// MinGap to disambiguate, so it is a SEPARATE, explicitly ambiguity-checked
// path — see match.go, which owns the whole rule. Everything in THIS file
// governs the exact path only: MinGap, Tol, Slots, the picker.
//
// COHERENCE WITH THE MICRO-USDT LEDGER (.omc/plans/usdt-migration.md task #44,
// which flips every money column to 1e-6 USDT): 5 decimals divides 6 exactly —
// one amount unit is 10 micro, so amount-units <-> micro is an exact integer
// *10 / /10 with no remainder in either direction, and an order total that is
// a whole cent (the plan's §2.2 invariant, 10_000 micro) is exactly 1000 units.
// 5 rather than 6 is deliberate: the plan's §4 caveat — do not let the ledger's
// precision propagate into Params.Decimals — still holds, because a 6-decimal
// amount string would ask customers to send digits below the 10-micro grid that
// some wallets round away, for no gain in band capacity.
//
// Stdlib only. All money math is integer "amount units" (1e-5 USDT at the
// shipped Decimals=5) or big.Rat for decimal-string parsing — never float, per
// the port's money-safety rule.
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
// Defaults in the Python original (all at 2 decimals, i.e. cents):
//
//	BEP20   step 21, tol 10, dec 2 (bep20_pay.py:40,54-55)
//	TRON    step 21, tol 10, dec 2 (tron_pay.py:41,50-51)
//	Binance step 10, tol  4, dec 2 (binance_pay.py:54-55; cents, no dec knob)
//
// SHIPPED HERE, after the hands-free-deposits ruling: BEP20 and TRON both run
// step 101, tol 50, dec 5 — 0.00101 USDT apart with a +/-0.00050 band, so
// MinGap (2*50+1 = 101) is met exactly by Step and 10 slots fit the
// 0.01000-0.01999 jitter window.
type Params struct {
	// Step is AMOUNT_OFFSET_STEP: spacing between consecutive slots, in
	// amount units.
	Step int
	// Tol is AMOUNT_MATCH_TOL: half-width of a band, in amount units. A
	// receipt within +/-Tol of an amount matches it.
	Tol int
	// Decimals is AMOUNT_DECIMALS: the precision of the amount strings this
	// rail hands out (5 = hundred-thousandths of a USDT).
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

// JitterLoHundredths / JitterHiHundredths bound the UPWARD JITTER WINDOW in
// HUNDREDTHS of a whole USDT unit: every picked amount is base + an offset in
// [0.01000, 0.02000) USDT, i.e. the customer is asked for between one and two
// hundredths MORE than the price they were quoted, never less and never more.
//
// USER RULING (hands-free-deposits, 2026-08-11) — this SUPERSEDES the previous
// `SlotSpanWholeUnits = 2` geometry. That one spanned two WHOLE units at a
// 21-cent step, so the last tried slot asked for 1.68 USDT over the price —
// +28% on a 6.00 order, and uncredited on the order path. The ruling was
// "28% is not acceptable, the range should be 0.01000 to 0.01999 USDT".
//
// The window is expressed as a fraction of a whole unit rather than as a raw
// unit count so it stays the SAME amount of money at any Decimals: at 5
// decimals it is offsets [1000, 2000) units, at 6 it would be [10000, 20000).
// Decimals < 2 has no representable window at all and Validate rejects it.
//
// CONSEQUENCE worth stating plainly: no slot sits AT base any more. A round
// "120.00" deposit is always quoted "120.01xxx", which is precisely what makes
// the amount an order identifier instead of a number two customers can both
// pick. The customer keeps every cent of the difference — a deposit credits
// what actually arrived (see DepositCredit).
const (
	JitterLoHundredths = 1 // inclusive lower edge: base + 0.01000
	JitterHiHundredths = 2 // EXCLUSIVE upper edge: base + 0.02000
)

// JitterLo is the smallest upward offset the picker may quote, in amount units
// (1000 at 5 decimals = 0.01000 USDT).
func (p Params) JitterLo() int64 {
	return pow10Int64(p.Decimals) / 100 * JitterLoHundredths
}

// JitterHiExcl is the first offset the picker may NOT quote, in amount units
// (2000 at 5 decimals = 0.02000 USDT). Exclusive, so the largest representable
// offset is 1999 = 0.01999 USDT — the top of the user's stated range.
func (p Params) JitterHiExcl() int64 {
	return pow10Int64(p.Decimals) / 100 * JitterHiHundredths
}

// JitterSpan is the width of the window the upward slots have to fit inside
// (1000 units = 0.01 USDT at 5 decimals).
func (p Params) JitterSpan() int64 { return p.JitterHiExcl() - p.JitterLo() }

// Slots is the number of UPWARD slots the picker will try: how many
// `base + JitterLo() + k*Step` values keep their offset strictly below
// JitterHiExcl(). Descends from telegram_bot.py:9085 `for k in range(scale //
// step)`, re-anchored on the jitter window instead of on whole units of price.
//
// At the shipped BEP20/TRON defaults (Step 101, Decimals 5) that is
// (1000-1)/101 + 1 = 10 slots, at offsets 0.01000, 0.01101, ... 0.01909 — one
// MORE than the 9 the superseded geometry produced, so per-price capacity does
// not regress even though the worst-case overquote falls from 1.68 USDT to
// 0.01909 USDT (a 98.9% reduction, and 0.32% of a 6.00 order instead of 28%).
// One extra LAST-RESORT slot at base-Step exists below these for callers that
// allow it (telegram_bot.py:9089).
func (p Params) Slots() int {
	span := p.JitterSpan()
	if span <= 0 || p.Step <= 0 {
		return 0
	}
	// Count of k >= 0 with k*Step <= span-1, i.e. floor((span-1)/Step) + 1.
	return int((span-1)/int64(p.Step)) + 1
}

// Validate rejects geometry the picker cannot work with.
func (p Params) Validate() error {
	if p.Step <= 0 {
		return fmt.Errorf("bands: Step must be > 0, got %d", p.Step)
	}
	if p.Tol < 0 {
		return fmt.Errorf("bands: Tol must be >= 0, got %d", p.Tol)
	}
	// Decimals < 2 cannot represent the jitter window at all (a hundredth of a
	// whole unit rounds to 0 units), so JitterLo()==JitterHiExcl()==0 and every
	// picked amount would collapse onto base. Checked before the Tol bound below
	// because that bound is stated against the window, which needs a precision to
	// be measured at.
	if p.Decimals < 2 || p.Decimals > 9 {
		return fmt.Errorf("bands: Decimals must be in [2,9], got %d (below 2 the %d/100..%d/100 jitter window is not representable)", p.Decimals, JitterLoHundredths, JitterHiHundredths)
	}
	// Tol has an UPPER bound, and it is not cosmetic — this is the check whose
	// absence let a configuration break the package's one absolute invariant.
	// Tol is operator-tunable through *_MATCH_TOL, MinGap() computes 2*Tol+1 in
	// int, and at Tol >= 2^62 that OVERFLOWS to a negative gap. slotClear's
	// `abs64(...) < b.minGap` is then never true, every live row stops blocking,
	// and two concurrent same-price orders are handed the SAME amount — one
	// receipt settling the wrong order, which is the exact failure this package
	// exists to prevent. Nothing downstream would have noticed: Validate passed,
	// bands.New succeeded, the bot booted. The same missing bound also overflows
	// registry.collisionUnits' gap and rescaleTolCeil's up-scale (which
	// multiplies Tol by up to 10^7 across a 2-to-9-decimal rail spread) at Tol
	// values far smaller than 2^62.
	//
	// The bound is the jitter span rather than some arbitrary large number
	// because it also means something: a Tol wider than the whole window makes
	// every slot in the window overlap every other, so the price collapses to a
	// single usable slot — the same misconfiguration the Step bound below
	// rejects, arrived at from the tolerance side.
	if span := p.JitterSpan(); int64(p.Tol) > span {
		return fmt.Errorf("bands: Tol %d exceeds the jitter span %d at %d decimals: every slot in the window would overlap every other", p.Tol, span, p.Decimals)
	}
	// RESTATED (was: "Step must not exceed one whole unit"). That bound existed
	// to stop the FIRST upward slot overshooting the customer's ask by more than
	// a whole unit. The jitter window now caps EVERY slot's overshoot below
	// 0.02 USDT by construction, so the whole-unit bound has become vacuous —
	// at 5 decimals one whole unit is 100000 units, a hundred times wider than
	// the entire window, and no Step that large could ever be reached. The
	// bound that carries the intent now is against the WINDOW: a Step wider
	// than the span can never place a second slot inside it, so band allocation
	// silently degenerates to one concurrent order per price and every further
	// customer is refused USDT_BUSY. That is a misconfiguration, and it is one
	// the Slots() >= 1 check below cannot see (it yields exactly 1, not 0).
	if span := p.JitterSpan(); int64(p.Step) > span {
		return fmt.Errorf("bands: Step %d exceeds the jitter span %d at %d decimals: only one slot would ever fit, so a second concurrent order at the same price is impossible", p.Step, span, p.Decimals)
	}
	// Backstop. Unreachable given the two checks above (Decimals >= 2 makes the
	// span positive, Step <= span makes k=0 fit), and kept so that a future edit
	// to JitterLoHundredths/JitterHiHundredths cannot ship a window with no slots.
	if p.Slots() < 1 {
		return fmt.Errorf("bands: Step %d leaves no slot inside the jitter span %d at %d decimals", p.Step, p.JitterSpan(), p.Decimals)
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
// int64's 9.22e18; the picker's baseU + JitterLo() + k*step stays within one
// hundredth of a whole unit above that, since every slot's offset is bounded by
// JitterHiExcl().
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
// smallest amount = base + JitterLo() + k*step whose +/-tol band is clear of
// EVERY live amount in `used`, so an incoming payment within tol of it maps to
// exactly one order even across products (telegram_bot.py:9070-9092).
//
// Slot order:
//  1. UPWARD slots base+0.01000, +step, ... for k < Slots(), i.e. every offset
//     inside the jitter window. Upward is preferred because it never
//     undercharges, and the window bounds the overquote below 0.02 USDT.
//  2. ONE downward slot base-step (5.99899 for a 6.00 price at the shipped
//     101-unit step) as a LAST RESORT when every upward slot is taken. The
//     seller absorbs that <=step undercharge — now under a tenth of a cent,
//     where the superseded 21-cent geometry made it 0.21 USDT — which beats
//     refusing the sale outright (telegram_bot.py:9077-9080, 9089-9091).
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
	// Upward slots first: base+JitterLo(), +step, ... while the offset stays
	// strictly below JitterHiExcl(). Upward is preferred because it never
	// undercharges, and the window keeps the overquote under 0.02 USDT.
	lo := p.JitterLo()
	// Slots() is hoisted, not called per iteration: it is constant for the whole
	// walk, and this loop runs under Registry.mu on the money path.
	slots := int64(p.Slots())
	for k := int64(0); k < slots; k++ {
		cu := baseU + lo + k*step
		if slotClear(cu, usedU) {
			return cu, true
		}
	}
	if !allowDownward {
		return 0, false
	}
	// Last resort: the single slot BELOW base (telegram_bot.py:9089-9091,
	// 5.99899 for a 6.00 price at the shipped 101-unit step).
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
// `p.Decimals` units by the rail's chain reader) settles `expected`: the
// +/-Tol match. This is the `matched` decision at telegram_bot.py:9745
// (`abs(recv_units - want_units) <= mod.AMOUNT_MATCH_TOL`).
//
// "Fuzzy" is the wrong word for it now, and the old phrasing here — "a customer
// who drops a cent or two still confirms" — was left over from the 2-decimal
// era and is FALSE by a factor of twenty at the shipped geometry: Tol is 50
// units = 0.00050 USDT, a fiftieth of a cent, so a customer who really does
// drop a cent lands 1000 units out and does NOT confirm here. What catches that
// customer is the separate fee-shortfall rule in match.go, which is a different
// mechanism with its own ambiguity check — not this one, and widening Tol is
// not how to reach it.
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
// comment there). With every shipped rail at Decimals=5 the rescale here is
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
