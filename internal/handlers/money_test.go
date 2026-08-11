package handlers

import (
	"testing"

	"codezdev-bot/internal/config"
)

// TestUsdtDispZeroHasNoNegativeSign is the review-round fix for a real,
// pre-existing bug: usdtDisp(0) rendered "-0.00 USDT" on every healthy
// zero-amount line (Wallet balance, a zero A7 Refunded total, …). Root
// cause: usd*100-1e-9 at usd==0 is a tiny negative float, and math.Ceil of a
// small negative returns IEEE-754 NEGATIVE ZERO, which Go's %.2f prints WITH
// its sign. Fixed at the source (money.go's cents==0 reassignment) rather
// than worked around at each call site, so every current AND future zero-VND
// render is correct — a local sign-strip on just the new A7 Refunded line
// would have left this exact defect live on the Wallet balances line and any
// future zero-amount render.
//
// Falsification: reverting the `if cents == 0 { cents = 0 }` guard in
// usdtDisp makes this test fail with "-0.00 USDT" — verified by mutation
// (see the executor report).
func TestUsdtDispZeroHasNoNegativeSign(t *testing.T) {
	b := &Bot{}
	if got := b.usdtDisp(0); got != "0.00 USDT" {
		t.Errorf("usdtDisp(0) = %q, want %q (no negative-zero sign)", got, "0.00 USDT")
	}
}

// TestUsdtDispNonZeroUnaffected checks that the cents==0 reassignment never
// touches a real, nonzero amount's digits.
//
// Scope, stated honestly: these are POSITIVE cases only, so this test does not
// reach the sub-cent negative interval where cents is also negative zero
// (usd ∈ (-0.01, 0]) — it is not the full blast-radius pin an earlier version
// of this comment claimed to be. The exhaustive check lives in money.go's
// comment: 1.4M rate×value combinations, zero digit changes. What this test
// does pin is the everyday path, and it fails if the guard is ever widened
// into something that rounds.
//
// Uses the compiled default rate (25000 VND/USD, config.DefaultUSDToVND) since
// b.cfg is nil here, matching money.go's own fallback.
func TestUsdtDispNonZeroUnaffected(t *testing.T) {
	b := &Bot{}
	cases := []struct {
		vnd  int64
		want string
	}{
		{5000, "0.20 USDT"},
		{2000, "0.08 USDT"},
		{1, "0.01 USDT"}, // rounds UP to the cent, never undercharge
	}
	for _, c := range cases {
		if got := b.usdtDisp(c.vnd); got != c.want {
			t.Errorf("usdtDisp(%d) = %q, want %q", c.vnd, got, c.want)
		}
	}
}

// ---- task #15: auditAmount can never render a bare sign+unit -------------

// newMoneyRateBot builds a bare Bot (no store/stock) wired to a REAL,
// disk-backed *config.Config, so auditAmount's rate reads go through the
// same settings.json/env/default chain production uses instead of a plain
// Bot{}'s nil-cfg default (which always falls back to
// config.DefaultUSDToVND and can never be invalid).
func newMoneyRateBot(t *testing.T) (*Bot, *config.Config) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	b := New(Deps{Config: cfg})
	t.Cleanup(b.Shutdown)
	return b, cfg
}

// setUsdToVND writes a raw "usd_to_vnd" settings.json value, BYPASSING
// capSetRate's [1000,200000]/digits-only wizard guard — simulating a
// hand-edited settings.json, which per task #15's reachability check is the
// ONLY way an invalid (not merely unset) rate can land in settings.json:
// capSetRate itself can never write a value outside that range or with a
// non-digit character (adminsettings.go:466-497), so the admin-set path
// cannot produce this state on its own.
func setUsdToVND(t *testing.T, cfg *config.Config, v any) {
	t.Helper()
	s := cfg.Settings()
	s["usd_to_vnd"] = v
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatalf("save settings: %v", err)
	}
}

// TestAuditAmountFallsBackOnInvalidRate is task #15's delete->red pin:
// auditAmount must NEVER return a bare sign+unit string ("+ USDT") — the
// returned string always carries a real number or an explicit non-numeric
// marker. Falsification: reverting auditAmount to the naive
// "sign + b.usdtStr(vnd) + \" USDT\"" it replaced (i.e. the body
// adminusers.go's old usdtAuditAmt and usdt.go's old settle-audit block
// each hand-rolled) turns every non-healthy subtest below from the raw-VND
// fallback into exactly "+ USDT" / "- USDT" — verified by mutation (see the
// executor report) and restored afterward.
//
// Three genuinely-invalid-ON-ITS-MERITS shapes, each reachable a DIFFERENT
// way (see setUsdToVND's and newUSDTBotAtRate's doc comments) — a
// whitespace-padded rate is deliberately NOT one of them: vndToUSD
// TrimSpaces since task #25, so that shape no longer reaches this branch at
// all, and a test built on it would pass whether or not this fix exists
// (the exact wrong-reason trap task #15 warned against).
func TestAuditAmountFallsBackOnInvalidRate(t *testing.T) {
	t.Run("healthy rate is unaffected (control)", func(t *testing.T) {
		b, cfg := newMoneyRateBot(t)
		setUsdToVND(t, cfg, 25000)
		if got := b.auditAmount("+", 125000); got != "+5.00 USDT" {
			t.Fatalf("auditAmount(+, 125000) at a healthy rate = %q, want %q — proves the fallback isn't ALWAYS taken", got, "+5.00 USDT")
		}
	})
	t.Run("negative settings.json rate", func(t *testing.T) {
		b, cfg := newMoneyRateBot(t)
		setUsdToVND(t, cfg, -5000) // hand-edited; capSetRate could never write this
		if got := b.auditAmount("+", 125000); got != "+125000 VND (usdt rate unavailable)" {
			t.Errorf("auditAmount(+, 125000) = %q, want the raw-VND fallback, never a bare \"+ USDT\"", got)
		}
		if got := b.auditAmount("-", 125000); got != "-125000 VND (usdt rate unavailable)" {
			t.Errorf("auditAmount(-, 125000) = %q, want the raw-VND fallback with the debit sign preserved (never dropped or flipped to '+')", got)
		}
		if got := b.auditAmount("", 40000); got != "40000 VND (usdt rate unavailable)" {
			t.Errorf("auditAmount(\"\", 40000) = %q, want the unsigned raw-VND fallback (the resulting-balance case)", got)
		}
	})
	t.Run("settings.json rate truncates to zero", func(t *testing.T) {
		// A DIFFERENT invalid shape than the negative case above:
		// USDToVND()'s truthyFloat(0.5) != 0, so this value overrides
		// env/default, but strconv.Itoa(int(0.5)) == "0" — config/settings.go's
		// own USDToVND doc names this exact "positive but under 1, truncated
		// to 0" trap.
		b, cfg := newMoneyRateBot(t)
		setUsdToVND(t, cfg, 0.5)
		if got := cfg.USDToVND(); got != "0" {
			t.Fatalf("fixture broken: cfg.USDToVND() = %q, want \"0\" (0.5 truncated) to actually exercise the rate<=0 branch", got)
		}
		if got := b.auditAmount("+", 125000); got != "+125000 VND (usdt rate unavailable)" {
			t.Errorf("auditAmount(+, 125000) = %q, want the raw-VND fallback", got)
		}
	})
	t.Run("ambient env garbage text (only reachable pre-config.Load)", func(t *testing.T) {
		// Unlike the two settings.json shapes above, garbage text can't reach
		// this branch via settings.json at all: truthyFloat("garbage") fails
		// to parse, returns 0, and USDToVND() silently falls through to
		// env/default instead of surfacing the bad value (a separate gap,
		// flagged not fixed here). The only way a resolved rate is literally
		// non-numeric text is the ambient env, snapshotted at config.Load —
		// hence newUSDTBotAtRate instead of setUsdToVND.
		b, _, _, _, _ := newUSDTBotAtRate(t, "not-a-number")
		if got := b.auditAmount("+", 125000); got != "+125000 VND (usdt rate unavailable)" {
			t.Errorf("auditAmount(+, 125000) = %q, want the raw-VND fallback", got)
		}
	})
}
