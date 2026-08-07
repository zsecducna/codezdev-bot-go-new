package handlers

// usdt_rate_config_test.go — task #22: the admin-set USD→VND rate must reach
// the DEPOSIT CREDIT path, not just the quote.
//
// Before this fix, internal/pay/bep20.UsdtToVND and its TRON twin read
// os.LookupEnv("USD_TO_VND") to value an on-chain amount, while every quote
// the customer was shown (vndToUSD in usdt.go, usdtDisp in money.go,
// usdtToVND in deposit.go) resolved b.cfg.USDToVND() — settings.json over the
// .env/ambient snapshot over config.DefaultUSDToVND. An admin running
// adm:setrate therefore moved what a customer was ASKED to pay without moving
// what they were CREDITED for paying it.
//
// The fixture below creates that divergence deliberately, and the assertions
// are written so a regression reports a WRONG AMOUNT rather than an error:
// newUSDTBot pins the ambient USD_TO_VND at 25,000 before config.Load, and
// the wizard then sets the configured rate to 30,000. A credit path reading
// the environment still finds a perfectly valid 25,000 and converts with it —
// which is the whole point, since a test whose env var happened to be unset
// would pass on the "rate is missing" error path whether or not the fix
// exists.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestUSDTDepositCreditsAtAdminSetRate drives the REAL admin rate wizard and
// then the REAL deposit-verify path, on both surviving rails, and asserts the
// customer's BALANCE — money credited, not a converter's return value.
//
// The deposit deliberately OVERPAYS well outside tolerance. Inside tolerance
// DepositCredit credits at LEAST the order total (bands/credit.go: it returns
// the desired VND unless the received value exceeds it), so an exact 6.00
// payment credits the order total whichever rate source the converter used and
// would have proved nothing. Only an off-amount payment credits the converted
// value unconditionally, which is what makes the rate source observable.
func TestUSDTDepositCreditsAtAdminSetRate(t *testing.T) {
	cases := []struct {
		name   string
		depCB  string // deposit rail picker callback
		chain  string
		isBSC  bool
		txSeed string
	}{
		{"bep20", "dep:bsc", chainBSC, true, "e"},
		{"tron", "dep:trx", chainTRX, false, "d"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, f, st, _, fk := newUSDTBot(t)
			chat := int64(4820 + i)

			// Fixture guard, checked rather than assumed: the ambient rate must
			// be present AND different from the one the wizard is about to set,
			// or the two sources cannot diverge and the assertions below hold
			// for the wrong reason.
			if env := os.Getenv("USD_TO_VND"); env != "25000" {
				t.Fatalf("fixture broken: ambient USD_TO_VND = %q, want 25000 to diverge from the wizard's 30000", env)
			}

			// The REAL wizard, as an admin drives it (newUSDTBot's admin is
			// "999"). 30,000 is inside capSetRate's [rateMin, rateMax]; assert
			// the confirmation, because a rejected value returns early WITHOUT
			// writing settings.json and would misread as the fix not working.
			b.Process(ctx, callbackUpdate(999, "adm:setrate"))
			b.Process(ctx, textUpdate(999, "30000"))
			if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USD→VND rate set to 30,000.") {
				t.Fatalf("setrate wizard did not confirm the new rate, got %+v", last)
			}
			if got := b.cfg.USDToVND(); got != "30000" {
				t.Fatalf("configured rate = %q, want 30000 after the wizard", got)
			}

			// Quote side: a 6 USDT deposit is priced at the CONFIGURED rate, so
			// the order total is 180,000 VND. This already worked before the
			// fix — it is the half that made the divergence a money defect
			// rather than a cosmetic one.
			b.Process(ctx, callbackUpdate(chat, tc.depCB))
			b.Process(ctx, textUpdate(chat, "6"))
			o := reservedDeposit(t, st, chat)
			if o.Total != 180000 {
				t.Fatalf("deposit order total = %d VND, want 180000 (6 USDT at the admin-set 30,000)", o.Total)
			}
			armTxid(t, ctx, b, chat, tc.chain, o.Des)

			// Credit side: 8.00 USDT actually arrives.
			h := hash64(tc.txSeed)
			if tc.isBSC {
				fk.setBSC(h, bscTx(h, "8.00", time.Now().Unix()))
				b.Process(ctx, textUpdate(chat, "0x"+h))
			} else {
				fk.setTRX(h, trxTx(h, "8.00", time.Now().UnixMilli()))
				b.Process(ctx, textUpdate(chat, h))
			}

			// 8 USDT at the admin-set 30,000 = 240,000 VND. A credit path still
			// reading the process environment converts at 25,000 and credits
			// 200,000 — the customer is 40,000 VND short on a payment the shop
			// priced at the new rate.
			bal, _ := st.GetBalance(fmtInt(chat))
			if bal != 240000 {
				t.Errorf("credited %d VND, want 240000 (8 USDT at the admin-set 30,000); 200000 means the credit path converted at the ambient env rate", bal)
			}
		})
	}
}

// TestUSDTCreditSurvivesAbsentEnvRate pins the second half of task #22: with
// no USD_TO_VND in the environment at all, the quote side fell back to
// config.DefaultUSDToVND and happily priced a deposit, while the credit side
// returned "USD_TO_VND is not set" and verifyUSDTTxid re-armed — a refusal to
// credit a payment that had already been made at a correctly quoted amount.
// Both sides must now agree on the compiled default.
func TestUSDTCreditSurvivesAbsentEnvRate(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fk := newUSDTBot(t)
	const chat = 4830

	// config.Load froze its env snapshot when newUSDTBot built the Bot, so
	// clearing the LIVE process variable now leaves cfg.USDToVND() answering
	// 25,000 while a raw os.LookupEnv finds nothing at all. That is exactly
	// the shape of the production case (a shop whose environment never
	// carried USD_TO_VND, where config answers DefaultUSDToVND instead) with
	// the same numeric value, and it is reproducible inside one process in a
	// way an already-loaded config is not. t.Setenv in newUSDTBot registered
	// the restore, so this Unsetenv is undone when the test ends.
	if got := b.cfg.USDToVND(); got != "25000" {
		t.Fatalf("configured rate = %q, want the ambient 25000", got)
	}
	os.Unsetenv("USD_TO_VND")
	if _, ok := os.LookupEnv("USD_TO_VND"); ok {
		t.Fatal("fixture broken: USD_TO_VND is still set, so a raw env read would succeed")
	}

	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	if o.Total != 150000 {
		t.Fatalf("deposit order total = %d VND, want 150000", o.Total)
	}
	armTxid(t, ctx, b, chat, chainBSC, o.Des)

	h := hash64("c")
	fk.setBSC(h, bscTx(h, "8.00", time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	// The pre-fix credit path read os.LookupEnv and, finding nothing, errored
	// into txidNotFound — crediting 0 for a real 8 USDT transfer.
	bal, _ := st.GetBalance(fmtInt(chat))
	if bal != 200000 {
		t.Errorf("credited %d VND, want 200000 (8 USDT at the snapshot 25,000); 0 means the credit path refused to value a paid deposit because the live environment no longer carries the rate", bal)
	}
}
