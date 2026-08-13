package handlers

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"codezdev-bot/internal/config"
)

// usdtDisp is the customer-facing money string for a stored-VND amount, USDT-only
// — port of usdt_disp -> usdt_str -> paypal.vnd_to_usd (telegram_bot.py:6434-6442,
// 6425-6431; paypal.py:46-55). e.g. 125000 -> "5.00 USDT". Rounds UP to the cent
// (never undercharge). Falls back to "— USDT" if the rate is unset/invalid so a
// render never crashes. Uses the config rate (settings.json usd_to_vnd overriding
// .env USD_TO_VND, else the compiled default) rather than os.Getenv, so display
// is deterministic and independent of the process environment.
func (b *Bot) usdtDisp(vnd int64) string {
	rateStr := config.DefaultUSDToVND
	if b.cfg != nil {
		rateStr = b.cfg.USDToVND()
	}
	// TrimSpace: cfg.USDToVND() can surface an AMBIENT env value verbatim —
	// config.Load only trims .env-file lines (parseEnvLines), never the
	// process environment, so a whitespace-padded ambient USD_TO_VND (e.g.
	// `export USD_TO_VND=" 25000 "`) reaches here un-trimmed. strconv.ParseFloat
	// rejects surrounding whitespace outright, so without this the rate looked
	// "invalid" and every customer-facing render fell back to "— USDT" even
	// though the wallet was credited correctly (bep20/tron's own UsdtToVND
	// already TrimSpace their rate string, which is what let this asymmetry
	// hide: the money path worked while the display path silently didn't).
	//
	// config.USDToVND() itself now trims too (internal/config/settings.go) —
	// a review found THREE more callers of that method with this same gap,
	// two of them functional (a customer's deposit / an admin's price
	// rejected outright, not merely mis-displayed), so the fix moved to the
	// source rather than staying a per-caller convention. This line is now
	// REDUNDANT, kept deliberately for defence in depth on a money path — do
	// not delete the SOURCE trim on the theory that this one (or usdt.go's
	// matching one) covers it; every other caller of cfg.USDToVND() has only
	// the source trim protecting it.
	rateStr = strings.TrimSpace(rateStr)
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || rate <= 0 {
		return "— USDT"
	}
	usd := float64(vnd) / rate
	// Python: cents = int(usd*100) + (1 if (usd*100) % 1 else 0)  -> ceil to cent.
	cents := math.Ceil(usd*100 - 1e-9)
	// Review finding: at vnd==0, usd*100-1e-9 is a tiny negative number, and
	// math.Ceil of a small negative returns IEEE-754 NEGATIVE ZERO — which
	// %.2f then prints WITH its sign, rendering "-0.00 USDT" on every healthy
	// zero-amount line (Wallet balance, a zero Refunded total, …). cents==0
	// is true for both +0.0 and -0.0 under IEEE-754 equality, so this
	// reassignment to the positive-zero literal clears the sign bit.
	//
	// Blast radius, stated precisely because an earlier draft of this comment
	// got it wrong: cents is -0.0 for the whole half-open interval
	// usd ∈ (-0.01, 0], not for usd==0 alone — every sub-cent NEGATIVE amount
	// ceils to negative zero too. The consequence is nonetheless stronger than
	// that sounds and was verified exhaustively rather than argued: over 7
	// rates × 200,001 VND values spanning ±100,000, the guard changed 210,584
	// outputs and NOT ONE of them differed in a digit — every difference was
	// the sign of a zero. No amount the bot renders can change value here.
	// (The negative half of that interval is unreachable in practice anyway:
	// the only plausible negative source is a wallet total, and balance.go
	// debits under `WHERE ... AND amount >= ?`, so a balance cannot go
	// negative.)
	if cents == 0 {
		cents = 0
	}
	return fmt.Sprintf("%.2f USDT", cents/100)
}

// auditAmount renders a SIGNED VND magnitude as a USDT audit/description
// string that can never come back missing its number. sign is "+", "-", or
// "" for an unsigned figure (e.g. a resulting balance).
//
// Built BY CONSTRUCTION, not per-site (task #15): usdtStr (usdt.go) returns
// "" on a configured-but-INVALID exchange rate (garbage text, zero,
// negative — see its own doc comment), and naively building
// sign+usdtStr(vnd)+" USDT" from that "" produces "+ USDT" — a row that
// LOOKS well-formed while silently missing the one number that matters.
// Two call sites (the on-chain deposit-settle audit line in usdt.go and
// execAddBal's admin credit/debit audit lines in adminusers.go) used to
// each hand-guard this the same way; both now collapse into this one
// helper so a THIRD audit/description site can't reintroduce the trap by
// simply forgetting to add its own guard.
//
// As it happens, an audit reader on THOSE two specific call sites is
// unreachable today: usdtToVNDFor (usdt.go, backed by bep20/tron.UsdtToVND)
// and adminUsdtToVnd (adminpricing.go) each apply this exact same
// ParseFloat->rate<=0 predicate to the exact same cfg.USDToVND() value
// BEFORE either call site is reached (adminUsdtToVnd relies on
// config.USDToVND()'s own TrimSpace at settings.go:331 rather than
// repeating it locally, and that function's settings.json branch returns a
// strconv.Itoa'd integer string, so the two readers cannot disagree on the
// string they parse), so an invalid
// rate already aborts the deposit (re-arms) or rejects the admin amount
// (outright) upstream of the audit line. That does not make this dead
// code: it is by-construction protection for whatever audit/description
// site gets written next, which may have no such upstream gate — exactly
// the failure mode task #15 flagged ("closed per site, not by
// construction"). Do not delete the fallback branch on the theory that the
// two current call sites can't trigger it; a future call site is the point.
//
// Deliberately does NOT delegate to usdtDisp above, even though usdtDisp
// already has a non-empty fallback ("— USDT"): concatenated as
// sign+usdtDisp(vnd) that becomes "+— USDT" on a bad rate, which is
// non-empty but throws away the one thing an audit row can still offer
// when the USD conversion breaks — the real VND figure. "+120000 VND (usdt
// rate unavailable)" tells an investigator something; "+— USDT" does not.
// So this reimplements usdtStr's own trim/parse/fallback shape but keeps
// the VND number in the fallback branch instead of discarding it.
//
// NOT for customer-facing quotes. startOnchainDeposit (usdt.go) must keep
// calling usdtStr directly and ABANDONING the flow when it returns "" — a
// customer quote may not be approximate, only an audit row may fall back to an
// explicit marker instead of a number.
// Routing a customer quote through this function would silently hand out
// a fallback STRING as if it were a price.
func (b *Bot) auditAmount(sign string, vnd int64) string {
	if s := b.usdtStr(vnd); s != "" {
		return sign + s + " USDT"
	}
	return fmt.Sprintf("%s%d VND (usdt rate unavailable)", sign, vnd)
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
