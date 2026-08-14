package handlers

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// usdtDisp is the customer-facing money string for a stored micro-USDT amount
// (1 USDT = 1,000,000 micro), USDT-only — port of usdt_disp -> usdt_str
// (telegram_bot.py:6434-6442, 6425-6431). e.g. 5_000_000 micro -> "5.00 USDT".
// Rounds UP to the cent (never undercharge) and clears IEEE-754 negative zero so
// a zero balance never renders "-0.00 USDT". micro is always a valid stored
// amount, so there is no rate to fail and no "— USDT" fallback — this never
// returns a bare unit string.
func (b *Bot) usdtDisp(micro int64) string {
	usd := float64(micro) / 1_000_000
	// Python: cents = int(usd*100) + (1 if (usd*100) % 1 else 0)  -> ceil to cent.
	cents := math.Ceil(usd*100 - 1e-9)
	// At micro==0, usd*100-1e-9 is a tiny negative number, and math.Ceil of a
	// small negative returns IEEE-754 NEGATIVE ZERO — which %.2f then prints
	// WITH its sign, rendering "-0.00 USDT" on every healthy zero-amount line
	// (Wallet balance, a zero Refunded total, …). cents==0 is true for both
	// +0.0 and -0.0 under IEEE-754 equality, so this reassignment to the
	// positive-zero literal clears the sign bit. (No amount the bot renders can
	// change value here — verified exhaustively: every difference the guard
	// makes is the sign of a zero, never a digit.)
	if cents == 0 {
		cents = 0
	}
	return fmt.Sprintf("%.2f USDT", cents/100)
}

// auditAmount renders a SIGNED micro-USDT magnitude as a USDT audit/description
// string that can never come back missing its number. sign is "+", "-", or ""
// for an unsigned figure (e.g. a resulting balance).
//
// Built BY CONSTRUCTION, not per-site (task #15): a naively hand-rolled
// sign+usdtStr(micro)+" USDT" used to be a per-site trap because usdtStr could
// return "" on an invalid exchange rate, producing "+ USDT" — a row that LOOKED
// well-formed while silently missing the one number that matters. Two call
// sites (the on-chain deposit-settle audit line in usdt.go and execAddBal's
// admin credit/debit audit lines in adminusers.go) each hand-guarded it; both
// collapsed into this one helper so a THIRD audit/description site can't
// reintroduce the trap by simply forgetting to add its own guard.
//
// The micro-USDT ledger has no exchange rate, so usdtStr now ALWAYS returns a
// valid 2dp quote for a micro amount — there is no invalid-rate branch left to
// fall back from. The raw-amount fallback the old VND ledger needed (the
// "X VND (usdt rate unavailable)" marker) is therefore unreachable and gone;
// this helper still centralizes the sign+quote+unit shape so a future
// audit/description site can't mis-build it.
func (b *Bot) auditAmount(sign string, micro int64) string {
	return sign + b.usdtStr(micro) + " USDT"
}

// itoa / itoa64 are the terse int->string helpers used to fill i18n {qty}/{max}/
// {stock} placeholders (i18n.F takes string kv pairs).
func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// cbData joins callback-data parts with ":" — the single builder for compound
// callback strings (buy:N, tier:N:CODE, cancelord:des:cmd …). Telegram rejects
// any callback_data over 64 BYTES with a 400, so cbDataLen (used by tests)
// checks the budget; product refs stay INDEX-based to keep well under it
// (handlers-go.md §4).
func cbData(parts ...string) string { return strings.Join(parts, ":") }

// cbDataLen reports whether a built callback string is within Telegram's 64-byte
// hard limit — the assertion TestCallbackDataLen runs on every builder.
func cbDataLen(data string) bool { return len(data) <= 64 }
