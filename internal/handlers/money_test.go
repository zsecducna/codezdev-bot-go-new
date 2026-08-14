package handlers

// money_test.go — pins for the micro-USDT money-string helpers (usdtDisp /
// usdtStr / auditAmount). The stored unit is micro-USDT (1 USDT = 1,000,000
// micro); these helpers are pure arithmetic over an int64 micro amount with NO
// exchange rate, so a bare &Bot{} (no config wired) exercises every path.

import "testing"

// TestUsdtDispZeroHasNoNegativeSign is the review-round fix for a real,
// pre-existing bug: usdtDisp(0) rendered "-0.00 USDT" on every healthy
// zero-amount line (Wallet balance, a zero A7 Refunded total, …). Root cause:
// usd*100-1e-9 at usd==0 is a tiny negative float, and math.Ceil of a small
// negative returns IEEE-754 NEGATIVE ZERO, which Go's %.2f prints WITH its
// sign. Fixed at the source (money.go's cents==0 reassignment) rather than
// worked around at each call site.
//
// Falsification: reverting the `if cents == 0 { cents = 0 }` guard in usdtDisp
// makes this test fail with "-0.00 USDT".
func TestUsdtDispZeroHasNoNegativeSign(t *testing.T) {
	b := &Bot{}
	if got := b.usdtDisp(0); got != "0.00 USDT" {
		t.Errorf("usdtDisp(0) = %q, want %q (no negative-zero sign)", got, "0.00 USDT")
	}
}

// TestUsdtDispMicroRoundsUpToCent pins the ceil-to-cent "never undercharge"
// rule and the micro->USDT scaling. usdtDisp takes a micro amount and divides
// by 1e6, then rounds UP to the cent — a sub-cent amount still renders at the
// next whole cent, never the floor.
func TestUsdtDispMicroRoundsUpToCent(t *testing.T) {
	b := &Bot{}
	cases := []struct {
		micro int64
		want  string
	}{
		{5_000_000, "5.00 USDT"}, // 5 USDT exactly
		{200_000, "0.20 USDT"},   // 0.20 USDT
		{80_000, "0.08 USDT"},    // 0.08 USDT
		{40, "0.01 USDT"},        // 0.00004 USDT -> rounds UP to 0.01, never undercharge
		{1, "0.01 USDT"},         // 1 micro -> still the next whole cent up
	}
	for _, c := range cases {
		if got := b.usdtDisp(c.micro); got != c.want {
			t.Errorf("usdtDisp(%d) = %q, want %q", c.micro, got, c.want)
		}
	}
}

// TestUsdtStrIsExactTwoDecimalQuote pins usdtStr's micro->2dp quote — the
// customer-facing price string the pay screen and deposit screen render. It is
// the cent-rounded twin of usdtDisp minus the " USDT" suffix.
func TestUsdtStrIsExactTwoDecimalQuote(t *testing.T) {
	b := &Bot{}
	cases := []struct {
		micro int64
		want  string
	}{
		{5_000_000, "5.00"},
		{12_502_500, "12.51"}, // 12.5025 USDT -> ceil to 12.51
		{0, "0.00"},
		{40, "0.01"},
	}
	for _, c := range cases {
		if got := b.usdtStr(c.micro); got != c.want {
			t.Errorf("usdtStr(%d) = %q, want %q", c.micro, got, c.want)
		}
	}
}

// TestAuditAmountAlwaysQuotesMicro is the micro-ledger successor to the old
// "falls back on invalid rate" test. With the exchange rate gone, auditAmount
// can NEVER render a bare sign+unit ("+ USDT") — usdtStr always returns a valid
// 2dp quote for a micro amount, so the old raw-amount fallback is unreachable
// and deleted. This pins that by-construction guarantee across the three sign
// shapes auditAmount serves (debit "-", credit "+", unsigned resulting balance).
//
// Falsification: reverting auditAmount to a hand-rolled sign+amount+" USDT"
// that could drop the number on some path turns one of these red.
func TestAuditAmountAlwaysQuotesMicro(t *testing.T) {
	b := &Bot{}
	cases := []struct {
		sign  string
		micro int64
		want  string
	}{
		{"+", 5_000_000, "+5.00 USDT"},
		{"-", 5_000_000, "-5.00 USDT"},
		{"", 5_000_000, "5.00 USDT"}, // unsigned (resulting balance)
		{"", 0, "0.00 USDT"},         // zero balance, never a bare unit
		{"+", 40, "+0.01 USDT"},      // sub-cent rounds up
	}
	for _, c := range cases {
		if got := b.auditAmount(c.sign, c.micro); got != c.want {
			t.Errorf("auditAmount(%q, %d) = %q, want %q (never a bare sign+unit)", c.sign, c.micro, got, c.want)
		}
	}
}
