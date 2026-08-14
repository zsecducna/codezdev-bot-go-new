package handlers

// usdt_test.go — acceptance pins for the USDT rails, ported from the Python
// suites named in the port mandate:
//
//	tests/test_usdt_redirect.py  — the picker excluding a chat's own DEAD sessions
//	tests/test_deposit_credit.py — the deposit credit decision (the pure function
//	                               itself is pinned in bands/credit_test.go; here
//	                               it is pinned END-TO-END through a deposit)
//
// Plus the integration pin this slice exists for: ONE bands.Registry shared by
// both surviving rails, so a live BEP20 band blocks the same TRON amount
// END-TO-END THROUGH THE HANDLERS — not merely in the bands unit tests.
//
// USDT migration note (2026-08-06): Binance Pay (the shared poller, its
// reconcile pass, tests/test_binance_match.py's port) was removed — the shop
// takes USDT via BEP20/TRON only (S3).
//
// Hands-free deposits note: the customer-pastes-a-tx-hash credit path was
// removed — deposits are credited by the watcher (usdtwatch.go), and these
// tests drive that credit through settleWatchedTransfer, the watcher's
// per-transfer settle entry point. The by-hash verify/replay/poll suites and
// the real-client seam tests went with the hash path.

import (
	"context"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/store"
)

// ---- harness ---------------------------------------------------------------

// testSellerBSC / testSellerTRX are shape-valid seller addresses (bep20 addrRE:
// 0x + 40 hex; tron addrRE: 'T' + 33 base58 chars).
var (
	testSellerBSC = "0x" + strings.Repeat("ab", 20) // 0x + 40 hex
	testSellerTRX = "T" + strings.Repeat("A", 33)   // T + 33 base58
)

// usdtFakes is the (now empty) handle newUSDTBot returns. The by-hash scripted
// fakes it used to carry left with the hash-paste path: deposits are credited
// through settleWatchedTransfer (the watcher's per-transfer entry point)
// directly, with no chain seam to script. Kept as a non-nil placeholder so
// newUSDTBot's return signature stays stable.
type usdtFakes struct{}

// newUSDTBot builds a Bot with both surviving USDT rails (BEP20, TRON)
// CONFIGURED and ENABLED and fast sweep timers. Binance Pay was removed by the
// USDT-only migration (S3).
//
// Env is set BEFORE handlers.New so the band geometry, the seller addresses and
// the USD_TO_VND rate are all in place when the registry is built.
func newUSDTBot(t *testing.T) (*Bot, *fakeTG, *store.Store, string, *usdtFakes) {
	t.Helper()
	return newUSDTBotAtRate(t, "25000") // 1 USDT = 25,000 VND
}

// newUSDTBotAtRate is newUSDTBot's twin, parameterized on the AMBIENT
// USD_TO_VND value (task #15 — needs a bot whose env-derived rate is
// invalid ON ITS MERITS from the moment config.Load runs, e.g. garbage
// text or "0"; that shape can only be built pre-Load, since cfg.USDToVND()
// snapshots the ambient env once and a settings.json write afterward
// can't touch it — see TestUSDToVNDTrimPropagatesToOtherRateConsumers's
// doc comment for the same snapshot fact used the other direction).
func newUSDTBotAtRate(t *testing.T, usdToVND string) (*Bot, *fakeTG, *store.Store, string, *usdtFakes) {
	t.Helper()
	t.Setenv("USD_TO_VND", usdToVND)
	t.Setenv("BEP20_ADDRESS", testSellerBSC)
	t.Setenv("TRON_ADDRESS", testSellerTRX)

	b, f, st, _, dir := newBuyBot(t, "999")
	// bep20/tron default OFF (pay_enabled @5150) — turn both USDT rails on.
	// SaveSettings persists the WHOLE dict, so edit the live settings rather than
	// a hand-built subset (which would drop every other default).
	s := b.cfg.Settings()
	s["pay_bep20"], s["pay_tron"] = true, true
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	// Fast sweep timer so the expiry backstop runs inside a test.
	b.expirySweepInterval = 5 * time.Millisecond
	return b, f, st, dir, &usdtFakes{}
}

// TestMicroConvertersAreRateFree pins that the USDT<->micro converters no
// longer depend on an exchange rate at all (the micro-USDT ledger has none).
// The predecessor (TestUSDToVNDTrimPropagatesToOtherRateConsumers) existed to
// prove a whitespace-padded USD_TO_VND reached every ParseFloat caller trimmed;
// with the rate gone there is nothing to trim, and these three converters are
// pure arithmetic over the input. An ambient USD_TO_VND env is set only to
// prove it is now a no-op for them — they ignore it entirely.
func TestMicroConvertersAreRateFree(t *testing.T) {
	t.Setenv("USD_TO_VND", " 25000 ") // legacy/harmless: no converter reads it now
	b, _, _, _, _ := newBuyBot(t, "999")

	if got := b.usdtToMicro(6); got != 6_000_000 {
		t.Errorf("deposit.go usdtToMicro(6) = %d, want 6,000,000 (6 USDT * 1e6, rate-independent)", got)
	}
	if got := b.usdtStr(6_000_000); got != "6.00" {
		t.Errorf("usdt.go usdtStr(6_000_000) = %q, want %q (micro->2dp, rate-independent)", got, "6.00")
	}
	if micro, ok := b.adminUsdtToMicro("6"); !ok || micro != 6_000_000 {
		t.Errorf("adminpricing.go adminUsdtToMicro(\"6\") = (%d,%t), want (6_000_000,true) — rate-independent scaling", micro, ok)
	}
}

// usdtOrder creates ONE reserved product order for `chat` at `totalVND` without
// touching folder stock (band allocation never reads stock). Used by the band
// tests, which care only about price and ownership.
func usdtOrder(t *testing.T, st *store.Store, chat int64, totalVND int64) string {
	t.Helper()
	des := store.NewDes()
	if err := st.CreateOrder(des, fmtInt(chat), "en", "prodA", "KBH", 1, totalVND); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return des
}

// reservedDeposit returns the chat's newest reserved __deposit__ order.
// store.ReservedOrderForChat deliberately EXCLUDES deposits (so topping up to
// pay a pending order stays free of pending_guard), so the deposit tests query
// the orders table directly.
func reservedDeposit(t *testing.T, st *store.Store, chat int64) *store.Order {
	t.Helper()
	rows, err := st.OrdersForChat(fmtInt(chat), 20, []string{"reserved"}, nil)
	if err != nil {
		t.Fatalf("orders_for_chat: %v", err)
	}
	for _, o := range rows {
		if o.Product == depositProduct {
			return o
		}
	}
	t.Fatal("no reserved deposit order")
	return nil
}

// bandUnits reads the amount a rail handed an order, in amount units.
func bandUnits(t *testing.T, b *Bot, p bands.Provider, des string) int64 {
	t.Helper()
	res, ok := b.bands.Get(p, des)
	if !ok {
		t.Fatalf("no band reserved for %s on %s", des, p)
	}
	return res.Units
}

// usdtScreenAmounts walks f.Sends and returns the amount rendered on every
// BEP20/TRON pay/deposit screen, in send order. sendUSDTScreen (usdt.go) fills
// the caption from i18n's BEP20Pay/TronPay template — "Send EXACTLY {usdt}
// USDT ..." in EN — so the marker "EXACTLY " unambiguously identifies a
// deposit/pay screen among the other sends a test may have triggered, and the
// amount is the text between that marker and the following " USDT".
func usdtScreenAmounts(t *testing.T, f *fakeTG) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var amounts []string
	for _, s := range f.Sends {
		// The backticks are part of the assertion, not noise to strip loosely:
		// the pay screen must render the amount in the same copyable code span
		// the address uses, so a customer never retypes five decimals by hand.
		_, rest, ok := strings.Cut(s.Text, "EXACTLY `")
		if !ok {
			continue
		}
		amount, _, ok := strings.Cut(rest, "` USDT")
		if !ok {
			continue
		}
		amounts = append(amounts, amount)
	}
	return amounts
}

// trxTx builds a confirmed TRON transfer of `amount` USDT at `tsMS` (epoch ms).
func trxTx(id, amount string, tsMS int64) *tron.Transfer {
	units, err := bands.AmountUnits(amount, tron.AmountDecimals())
	if err != nil {
		panic(err)
	}
	rat, ok := new(big.Rat).SetString(amount)
	if !ok {
		panic("bad amount " + amount)
	}
	// value = amount * 10^6 (TRON USDT base units).
	scaled := new(big.Rat).Mul(rat, new(big.Rat).SetInt(pow10BigInt(tron.TokenDecimals)))
	return &tron.Transfer{ID: id, Frm: "TFrom", Value: scaled.Num(), Units: units, TS: tsMS}
}

// hash64 builds a distinct well-formed 64-hex tx hash from a seed byte.
func hash64(seed string) string {
	h := strings.Repeat(seed, 64)
	return h[:64]
}

// anySendContains reports whether any recorded sendMessage carries `want`.
func anySendContains(f *fakeTG, want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Sends {
		if strings.Contains(s.Text, want) {
			return true
		}
	}
	return false
}

// reserveOrderBand reserves a PRODUCT-ORDER band directly through
// reserveUSDTAmount (allowDownward=true) — the reservation the deleted paybsc:
// on-chain pay route used to drive. The band/picker tests below care about
// amount/ownership geometry, not the (removed) pay screen, so they reserve here
// rather than tapping a button.
func reserveOrderBand(t *testing.T, b *Bot, p bands.Provider, chat int64, des string, total int64) {
	t.Helper()
	if _, err := b.reserveUSDTAmount(p, b.usdtStr(total), fmtInt(chat), des, total,
		b.now().UnixMilli(), b.now().Add(b.expiryTimeout), true); err != nil {
		t.Fatalf("reserve order band %s on %s: %v", des, p, err)
	}
}

// ---- 1. the integration pin: ONE registry across both rails -----------------

// TestUSDTSharedBandAcrossRails is the reason this slice exists: a live BEP20
// band must block the same amount on TRON END-TO-END THROUGH THE HANDLERS.
// Python kept independent per-rail registries, so a same-price order on two
// rails could get the same amount; this port routes both surviving rails
// through the ONE injected bands.Registry (bands deviation 1), whose
// collision set spans every provider. (Binance Pay was a third rail sharing
// this same registry until the USDT-only migration removed it, S3; the
// property under test — ONE shared registry, not "three registries" — is
// unaffected by the provider count.)
//
// Two same-price orders, one per rail, each opened by tapping its real pay
// button: the live amounts must stay at least max(tol)*2+1 units apart, so
// one incoming payment can never fall inside both bands.
func TestUSDTSharedBandAcrossRails(t *testing.T) {
	b, _, st, _, _ := newUSDTBot(t)
	const total = 6_000_000 // 6.00 USDT (micro: 1 USDT = 1,000,000)

	desB := usdtOrder(t, st, 102, total)
	desC := usdtOrder(t, st, 103, total)

	reserveOrderBand(t, b, bands.ProviderBEP20, 102, desB, total)
	reserveOrderBand(t, b, bands.ProviderTron, 103, desC, total)

	uB := bandUnits(t, b, bands.ProviderBEP20, desB)
	uC := bandUnits(t, b, bands.ProviderTron, desC)

	// Cross-provider spacing is max(tolBEP20,tolTron)*2+1 — full band
	// disjointness from EITHER side's tolerance (bands deviation 1).
	minGap := int64(2*max(bep20.AmountMatchTol(), tron.AmountMatchTol()) + 1)
	if uB == uC {
		t.Fatalf("bep20/tron: both rails handed out the SAME amount %d — the shared registry is not shared", uB)
	}
	if d := uB - uC; (d < 0 && -d < minGap) || (d >= 0 && d < minGap) {
		t.Errorf("bep20/tron: amounts %d and %d are closer than the %d-unit min gap — bands overlap", uB, uC, minGap)
	}
}

// TestUSDTBandExhaustedShowsBusy: when every slot for a price is taken the rail
// must answer USDT_BUSY and hand out NOTHING — refusing beats colliding
// (telegram_bot.py:9092 "no clear slot: refuse rather than collide"). Product
// orders (allowDownward) fill the 10 upward jitter slots inside the
// 0.01000-0.01999 window PLUS the one last-resort downward slot; the next
// deposit at the same price then has no slot and is refused with USDT_BUSY.
func TestUSDTBandExhaustedShowsBusy(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)
	const total = 6_000_000 // 6.00 USDT (micro)

	// Fill every slot from DISTINCT chats (product-order bands, allowDownward),
	// so the own-dead-session rule (which only forgives a chat's OWN expired
	// sessions) cannot free one.
	taken := map[int64]bool{}
	const slots = 11 // 10 upward jitter slots + 1 last-resort downward
	for i := range slots {
		chat := int64(200 + i)
		des := usdtOrder(t, st, chat, total)
		reserveOrderBand(t, b, bands.ProviderBEP20, chat, des, total)
		u := bandUnits(t, b, bands.ProviderBEP20, des)
		if taken[u] {
			t.Fatalf("slot %d: amount %d handed out twice", i, u)
		}
		taken[u] = true
	}
	// The next same-price DEPOSIT has no unambiguous slot left -> USDT_BUSY.
	chat := int64(299)
	before := len(f.Sends)
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))

	busy := i18n.C().Pay.USDTBusy.T(i18n.EN)
	found := false
	f.mu.Lock()
	for _, s := range f.Sends[before:] {
		if s.Text == busy {
			found = true
		}
	}
	f.mu.Unlock()
	if !found {
		t.Errorf("band exhaustion did not show USDT_BUSY; last send = %q", lastSend(f))
	}
}

// TestUSDTPickerExcludesOwnDeadSessions is the 2026-07-02 incident pin (Python
// tests/test_usdt_redirect.py::test_picker_excludes_own_dead_sessions): a
// customer's OWN abandoned sessions must not make the price point unbuyable for
// them. Four dead sessions of one chat held every upward slot in production.
func TestUSDTPickerExcludesOwnDeadSessions(t *testing.T) {
	b, _, st, _, _ := newUSDTBot(t)
	const total = 6_000_000 // 6.00 USDT (micro)
	const chat = 300

	// Four DEAD sessions owned by this chat (deadline already passed).
	for i := range 4 {
		des := store.NewDes()
		if err := b.bands.Adopt(bands.Reservation{
			Provider: bands.ProviderBEP20, Des: des, ChatID: fmtInt(chat),
			Amount:    bands.FormatUnits(int64(601_000+101*i), bep20.AmountDecimals()),
			CreatedAt: time.Now().Add(-time.Hour), Deadline: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatalf("adopt: %v", err)
		}
	}
	// The same chat re-orders: it must still get an amount (its own dead sessions
	// never block it), and the FIRST slot at that — the base price.
	des := usdtOrder(t, st, chat, total)
	reserveOrderBand(t, b, bands.ProviderBEP20, chat, des, total)
	if got := bandUnits(t, b, bands.ProviderBEP20, des); got != 601_000 {
		t.Errorf("own-dead-session retry got %d, want the first slot 601000 (6.00 + the jitter floor)", got)
	}
}

// ---- 4. deposits -----------------------------------------------------------

// TestUSDTDepositCreditBoundaries pins the credit decision END-TO-END through a
// TRON deposit (Python tests/test_deposit_credit.py) at the 5-decimal geometry.
//
// The customer asks for 6 USDT and is QUOTED 6.01000 (the request plus the
// jitter floor). Every row below therefore credits what actually ARRIVED, on
// both sides of the ±0.00050 tolerance — the customer keeps the jitter, and the
// round-a-small-underpay-up-to-the-request rule is unreachable here because any
// receipt within tolerance of the quote is already worth more than the request.
// That branch stays pinned as a pure function in bands/credit_test.go.
func TestUSDTDepositCreditBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		recv       string // USDT actually received
		wantCredit int64  // micro-USDT credited (1 USDT = 1,000,000 micro)
	}{
		{"exact to the last decimal credits actual", "6.01000", 6_010_000},
		{"inside tolerance under credits actual", "6.00960", 6_009_600},
		{"shortfall credits actual", "6.00940", 6_009_400},
		{"inside tolerance over credits actual", "6.01040", 6_010_400},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, _, st, _, _ := newUSDTBot(t)
			chat := int64(450 + i)

			// dep:trx -> amount prompt -> "6" (== 6,000,000 micro; 6 USDT desired).
			b.Process(ctx, callbackUpdate(chat, "dep:trx"))
			b.Process(ctx, textUpdate(chat, "6"))

			o := reservedDeposit(t, st, chat)
			// A deposit takes the SAME picked amount an order does: the request
			// plus the smallest jitter, never the round number.
			res, ok := b.bands.Get(bands.ProviderTron, o.Des)
			if !ok || res.Amount != "6.01000" {
				t.Fatalf("deposit amount = %q (found=%t), want the picked 6.01000", res.Amount, ok)
			}
			// Credit through the hands-free watcher's per-transfer settle path —
			// the now-sole deposit credit route (no hash paste).
			prm, _ := b.bands.Params(bands.ProviderTron)
			b.settleWatchedTransfer(ctx, bands.ProviderTron, prm, watchedIn(t, chainTRX, tc.recv, b))

			bal, _ := st.GetBalance(fmtInt(chat))
			if bal != tc.wantCredit {
				t.Errorf("credited %d micro, want %d", bal, tc.wantCredit)
			}
			// The deposit order is closed (history/revenue) but NOTHING delivered.
			done, _ := st.GetOrder(o.Des)
			if done.Status != "delivered" {
				t.Errorf("deposit order status = %s, want delivered (closed)", done.Status)
			}
		})
	}
}

// TestUSDTDepositAuditRowIsWellFormed pins the on-chain settle audit row
// ("usdt_"+chain+"_deposit", written by settleUSDTMatch): the row's Outcome must
// be the ordinary signed "+X.XX USDT" figure rendered by auditAmount, never a
// bare sign+unit. Driven through the hands-free watcher's per-transfer settle
// path (the now-sole credit route), crediting the exact quoted amount so the
// credited value is the converted received value (the jitter puts it just above
// the requested total).
func TestUSDTDepositAuditRowIsWellFormedAtHealthyRate(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	const chat = 4840

	before, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit before: %v", err)
	}

	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6")) // 6 USDT desired == 6,000,000 micro
	o := reservedDeposit(t, st, chat)
	bandRes, _ := b.bands.Get(bands.ProviderBEP20, o.Des)
	// Credit the EXACT quoted amount through the watcher's settle path — an
	// exact match credits the converted received value (6.01000 USDT ->
	// 6,010,000 micro), so the audit row's Outcome is the ordinary signed
	// USDT figure rendered by auditAmount.
	prm, _ := b.bands.Params(bands.ProviderBEP20)
	b.settleWatchedTransfer(ctx, bands.ProviderBEP20, prm, watchedIn(t, chainBSC, bandRes.Amount, b))

	bal, _ := st.GetBalance(fmtInt(chat))
	if bal != 6_010_000 {
		t.Fatalf("fixture broken: credited %d micro, want 6,010,000 (6.01000 USDT)", bal)
	}

	after, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("audit row count = %d, want exactly %d (before+1): %+v", len(after), len(before)+1, after)
	}
	row := after[0] // RecentAudit is newest-first
	if row.Action != "usdt_bsc_deposit" {
		t.Errorf("Action = %q, want usdt_bsc_deposit", row.Action)
	}
	if want := "+6.01 USDT"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q — the healthy-rate row must be the ordinary signed figure, not the raw-VND fallback", row.Outcome, want)
	}
}

// watchedIn builds ONE incoming on-chain transfer of recvUSDT for the hands-free
// watcher's per-transfer settle entry point (settleWatchedTransfer) — the now-
// sole way a deposit is credited. It is the same shape tickOnchain/tickBep20
// hand to settleWatchedTransfer: units at the rail's AmountDecimals (what
// MatchIncoming keys on) and recv as the amount's big.Rat (the rail's
// SetFrac(value, 10^TokenDecimals) collapses to exactly the amount).
func watchedIn(t *testing.T, chain, recvUSDT string, b *Bot) watchedTransfer {
	t.Helper()
	rat, ok := new(big.Rat).SetString(recvUSDT)
	if !ok {
		t.Fatalf("bad recv amount %q", recvUSDT)
	}
	decimals := tron.AmountDecimals()
	if chain == chainBSC {
		decimals = bep20.AmountDecimals()
	}
	units, err := bands.AmountUnits(recvUSDT, decimals)
	if err != nil {
		t.Fatalf("amount units for %q: %v", recvUSDT, err)
	}
	return watchedTransfer{
		id: hash64("w"), frm: "TFrom", units: units, recv: rat,
		ms: b.now().UnixMilli(),
	}
}

// providerFor is the test-side twin of providerForChain, failing loudly rather
// than returning a zero Provider.
func providerFor(t *testing.T, chain string) bands.Provider {
	t.Helper()
	p, ok := providerForChain(chain)
	if !ok {
		t.Fatalf("no provider for chain %q", chain)
	}
	return p
}

// TestPayScreenWarnsAboutFeesInEveryLanguage pins the copy requirement that
// makes exact matching safe for the customer. Now that a payment is matched to
// an order within +/-0.00050 USDT, a platform that skims its network fee OUT of
// the transfer makes the payment unmatchable — so the screen has to say, in
// every language the catalog supports:
//
//  1. the network fee is theirs, paid ON TOP of the amount;
//  2. the figure that must ARRIVE is exact to all five decimals.
//
// It also pins the emphasis: the amount is rendered in the same Markdown code
// span the address uses, so it is tap-to-copy rather than retyped by hand.
//
// Rendering through startOnchainDeposit rather than reading the catalog
// directly is deliberate — an English-only string added straight to the screen,
// or a token that stops substituting in one language, both fail here.
func TestPayScreenWarnsAboutFeesInEveryLanguage(t *testing.T) {
	ctx := context.Background()
	const vnd = 120_000_000 // 120.00 USDT (micro: 1 USDT = 1,000,000)

	// Per-language markers for the two required statements, plus the
	// five-decimal claim. Chosen from the catalog copy itself so a translation
	// that silently drops one of the three warnings fails this test.
	cases := []struct {
		lang    i18n.Lang
		markers []string
	}{
		{i18n.EN, []string{"must ARRIVE exact to every digit", "all 5 decimals", "network fee is yours to pay, ON TOP"}},
		{i18n.VI, []string{"VỀ TỚI NƠI đúng từng chữ số", "5 chữ số thập phân", "Phí mạng do bạn trả, CỘNG THÊM"}},
		{i18n.ZH, []string{"到账金额必须每一位都完全一致", "5 位小数", "网络费由您另行承担"}},
	}
	for _, chain := range []string{chainTRX, chainBSC} {
		for i, tc := range cases {
			t.Run(chain+"/"+string(tc.lang), func(t *testing.T) {
				b, f, _, _, _ := newUSDTBot(t)
				b.startOnchainDeposit(ctx, int64(700+i), tc.lang, vnd, chain)

				f.mu.Lock()
				n := len(f.Sends)
				var text string
				if n == 1 {
					text = f.Sends[0].Text
				}
				f.mu.Unlock()
				if n != 1 {
					t.Fatalf("want exactly one screen, got %d", n)
				}
				for _, m := range tc.markers {
					if !strings.Contains(text, m) {
						t.Errorf("screen is missing %q:\n%s", m, text)
					}
				}
				// The amount, at full precision, inside a Markdown code span.
				// Matched by value rather than by an English marker word, so the
				// VI/ZH screens are held to the same standard as the EN one:
				// this is the FIRST deposit on a fresh bot, so it takes the
				// uncontended slot at base + the jitter floor.
				prm, _ := b.bands.Params(providerFor(t, chain))
				baseU, err := bands.AmountUnits(b.usdtStr(vnd), prm.Decimals)
				if err != nil {
					t.Fatalf("AmountUnits(base): %v", err)
				}
				want := bands.FormatUnits(baseU+prm.JitterLo(), prm.Decimals)
				if want != "120.01000" {
					t.Fatalf("fixture drifted: picked amount = %q, want 120.01000", want)
				}
				// At least three code spans: the headline figure, the
				// exact-to-five-decimals line, and the fee restatement.
				if n := strings.Count(text, "`"+want+"`"); n < 3 {
					t.Errorf("the amount appears in a code span %d times, want at least 3 (headline, exactness, fee restatement):\n%s", n, text)
				}
			})
		}
	}
}

// TestDepositAmountsAreUnique pins the hands-free-deposits behaviour:
// startOnchainDeposit hands out a PICKED band amount (reserveUSDTAmount),
// never the customer's round number verbatim. Two deposits of the SAME round
// amount must be handed different band amounts — without this the watcher
// cannot tell whose payment is whose, and amount-based attribution is unsound.
func TestDepositAmountsAreUnique(t *testing.T) {
	b, f, st, _, _ := newUSDTBot(t)
	ctx := context.Background()
	const total = 120_000_000 // 120.00 USDT (micro: 1 USDT = 1,000,000)
	b.startOnchainDeposit(ctx, 111, i18n.EN, total, chainTRX)
	b.startOnchainDeposit(ctx, 222, i18n.EN, total, chainTRX)

	amounts := usdtScreenAmounts(t, f) // amounts rendered on the two deposit screens
	if len(amounts) != 2 {
		t.Fatalf("want 2 deposit screens, got %d: %+v", len(amounts), amounts)
	}
	// Even the FIRST, uncontended deposit is offset: the jitter is what makes
	// the amount an order identifier, so a round number is never quoted. The
	// quote is the base plus exactly the jitter floor.
	baseU, err := bands.AmountUnits(b.usdtStr(total), tron.AmountDecimals())
	if err != nil {
		t.Fatalf("AmountUnits(base): %v", err)
	}
	prm, _ := b.bands.Params(bands.ProviderTron)
	want := bands.FormatUnits(baseU+prm.JitterLo(), prm.Decimals)
	if amounts[0] != want {
		t.Errorf("first deposit quoted %q, want the base plus the jitter floor %q", amounts[0], want)
	}
	if amounts[0] == b.usdtStr(total) {
		t.Error("first deposit was quoted the round base — two customers depositing the same amount would collide")
	}
	if amounts[0] == amounts[1] {
		t.Fatalf("both deposits were quoted %q — bands are not unique", amounts[0])
	}
	// Plain inequality isn't the property the hands-free watcher actually
	// needs: two amounts that differ by less than MinGap (2*tol+1) could both
	// still match one on-chain receipt within tolerance. Compare in UNITS
	// (the same minGap check TestUSDTSharedBandAcrossRails runs for its
	// cross-provider twin of this invariant), not the rendered strings.
	desA, desB := reservedDeposit(t, st, 111).Des, reservedDeposit(t, st, 222).Des
	uA, uB := bandUnits(t, b, bands.ProviderTron, desA), bandUnits(t, b, bands.ProviderTron, desB)
	minGap := int64(2*tron.AmountMatchTol() + 1)
	if d := uA - uB; (d < 0 && -d < minGap) || (d >= 0 && d < minGap) {
		t.Errorf("deposit amounts %d and %d (units) are closer than the %d-unit min gap — bands overlap", uA, uB, minGap)
	}
}

// TestDepositNeverQuotedBelowBase pins hands-free-deposits ruling (a): a
// deposit must never fall back to the picker's last-resort DOWNWARD slot.
// DepositCredit rounds an exact payment up to the customer's FULL requested
// total regardless of which band amount they were actually quoted, so a
// downward-slot deposit would be a straight giveaway — reserveUSDTAmount
// (allowDownward=false on the deposit path) must never hand one out.
//
// Fills every upward TRON jitter slot (10) with distinct deposits at the same
// round base, then asserts the 11th is REFUSED with USDT_BUSY rather than
// handed the downward amount a product order would get in the same spot (see
// TestOrderCanTakeDownwardSlotDepositsCannot).
//
// Also pins the user's stated range end-to-end: every quote sits between
// 0.01000 and 0.01999 USDT above the base, so the worst case a real customer
// can be asked for is 0.01909 over — not the 1.68 the superseded geometry
// could produce.
func TestDepositNeverQuotedBelowBase(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newUSDTBot(t)
	const vnd = 120_000_000 // 120.00 USDT (micro: 1 USDT = 1,000,000)

	prm, _ := b.bands.Params(bands.ProviderTron)
	slots := prm.Slots()
	for i := range slots {
		chat := int64(500 + i)
		b.startOnchainDeposit(ctx, chat, i18n.EN, vnd, chainTRX)
	}
	amounts := usdtScreenAmounts(t, f)
	if len(amounts) != slots {
		t.Fatalf("want %d deposit screens (the upward jitter window), got %d: %+v", slots, len(amounts), amounts)
	}
	seen := map[string]bool{}
	base, err := bands.AmountUnits(b.usdtStr(vnd), tron.AmountDecimals())
	if err != nil {
		t.Fatalf("AmountUnits(base): %v", err)
	}
	for _, a := range amounts {
		if seen[a] {
			t.Fatalf("amount %q handed to two deposits — bands are not unique", a)
		}
		seen[a] = true
		u, err := bands.AmountUnits(a, tron.AmountDecimals())
		if err != nil {
			t.Fatalf("AmountUnits(%q): %v", a, err)
		}
		if u < base {
			t.Errorf("deposit quoted %q, BELOW the base %q — the downward last-resort slot leaked into a deposit", a, b.usdtStr(vnd))
		}
		if off := u - base; off < prm.JitterLo() || off >= prm.JitterHiExcl() {
			t.Errorf("deposit quoted %q — %s USDT over the base, outside the 0.01000-0.01999 range the user ruled",
				a, bands.FormatUnits(off, prm.Decimals))
		}
	}

	// The next deposit at the same price has no upward slot left. It must be
	// REFUSED (USDT_BUSY), never handed the downward amount.
	before := len(f.Sends)
	b.startOnchainDeposit(ctx, 599, i18n.EN, vnd, chainTRX)
	busy := i18n.C().Pay.USDTBusy.T(i18n.EN)
	found := false
	f.mu.Lock()
	for _, s := range f.Sends[before:] {
		if s.Text == busy {
			found = true
		}
	}
	f.mu.Unlock()
	if !found {
		t.Fatalf("the overflow deposit was not refused with USDT_BUSY; last send = %q", lastSend(f))
	}
	if got := usdtScreenAmounts(t, f); len(got) != slots {
		t.Fatalf("an extra deposit screen was rendered (%+v) — the downward slot was handed to a deposit", got)
	}
}

// TestOrderCanTakeDownwardSlotDepositsCannot pins the other half of ruling
// (a): unlike a deposit, a PRODUCT ORDER may still be quoted the picker's
// downward last-resort amount — a seller-absorbed <=step undercharge is an
// acceptable cost there (Reserve, unlike ReserveUpwardOnly, must keep
// offering it). At the 5-decimal geometry that undercharge is 0.00101 USDT,
// where the superseded 21-cent step made it 0.21.
func TestOrderCanTakeDownwardSlotDepositsCannot(t *testing.T) {
	b, _, st, _, _ := newUSDTBot(t)
	const total = 6_000_000 // 6.00 USDT (micro: 1 USDT = 1,000,000)

	prm, _ := b.bands.Params(bands.ProviderBEP20)
	for i := range prm.Slots() {
		chat := int64(600 + i)
		des := usdtOrder(t, st, chat, total)
		reserveOrderBand(t, b, bands.ProviderBEP20, chat, des, total)
	}
	chat := int64(699)
	des := usdtOrder(t, st, chat, total)
	reserveOrderBand(t, b, bands.ProviderBEP20, chat, des, total)

	res, ok := b.bands.Get(bands.ProviderBEP20, des)
	if !ok {
		t.Fatal("the overflow order was refused — the downward last-resort slot must still be offered to orders")
	}
	if res.Amount != "5.99899" {
		t.Errorf("overflow order amount = %q, want %q (the downward last-resort slot)", res.Amount, "5.99899")
	}
}

// TestUSDTDepositQuoteAbandonsOnInvalidRate is task #15's quote-site
// regression pin: startOnchainDeposit (usdt.go ~420) must keep calling
// usdtStr directly and ABANDONING when it returns "" — a customer quote may
// not be approximate, only an audit row may fall back to a marker.
//
// This does NOT just re-check the existing `if amount == ""` guard by
// deleting it outright: that specific mutation changes nothing observable
// here, because bands.Registry.Reserve (the deposit path since
// TestUSDTDepositOKRendersAmount pins the end-to-end property a successful
// on-chain deposit must render: Deposit.OK shows the credited amount and the
// new balance as real USDT figures, never a bare "— USDT" placeholder.
//
// (This was originally the task #25 whitespace-padded-ambient-rate regression
// test. The padded-rate premise is gone with the USD→VND rate itself — the
// micro-USDT ledger has no rate to mis-trim, so usdtDisp always renders a real
// figure for a micro amount. The end-to-end OK-render property it pinned
// survives and is still worth holding.)
func TestUSDTDepositOKRendersAmount(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)

	const chat = 4840
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	bandRes, _ := b.bands.Get(bands.ProviderBEP20, o.Des)

	// Credit the EXACT quoted amount through the watcher's settle path. The
	// jitter puts the quote (6.01000) just above the 6.00 request, so the credit
	// is the received value in micro (6.01000 USDT -> 6,010,000 micro).
	prm, _ := b.bands.Params(bands.ProviderBEP20)
	b.settleWatchedTransfer(ctx, bands.ProviderBEP20, prm, watchedIn(t, chainBSC, bandRes.Amount, b))

	bal, _ := st.GetBalance(fmtInt(chat))
	if bal != 6_010_000 {
		t.Fatalf("fixture broken: credited %d micro, want 6,010,000 (6.01000 USDT)", bal)
	}

	// Scoped to the customer's own chat, not lastSendMsg's "most recent send
	// overall": a settled deposit also fires notifyAdminDeposit (notify.go)
	// to the admin chat ("999" here, never == chat), which would otherwise
	// be mistaken for the customer's own Deposit.OK confirmation.
	last := lastSendMsgTo(f, chat)
	if last == nil {
		t.Fatal("no deposit confirmation sent")
	}
	if strings.Contains(last.Text, "— USDT") {
		t.Errorf("Deposit.OK = %q — the credited amount rendered as the empty placeholder instead of a real figure", last.Text)
	}
	want := i18n.F(i18n.C().Deposit.OK.T(i18n.EN), "amount", "6.01 USDT", "balance", "6.01 USDT")
	if last.Text != want {
		t.Errorf("Deposit.OK = %q, want %q", last.Text, want)
	}
}

// TestUSDTPersistFailureRefusesScreen: a pending row that cannot be PERSISTED
// would not survive a restart (the rehydrate/watcher could never see it), so
// the rail must undo its reservation and refuse rather than invite a payment it
// might be unable to match. Python raises out of the handler at
// store.bep20_pending_add (@9520) with the same effect.
func TestUSDTPersistFailureRefusesScreen(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)
	const chat = 650

	// Break the persisted table so *PendingAdd fails.
	if _, err := st.DB().Exec("DROP TABLE bep20_pending"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	before := len(f.Sends)
	// Drive a real deposit: dep:bsc -> amount prompt -> "6". startOnchainDeposit
	// reserves the band, then persistUSDTPending fails (table dropped), so it
	// must roll the reservation back and refuse rather than strand a slot.
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))

	if b.bands.Len(bands.ProviderBEP20) != 0 {
		t.Error("a band was kept for a session that could not be persisted — the slot is now held by a session nobody can settle")
	}
	noID := i18n.C().Pay.BEP20NoID.T(i18n.EN)
	refused := false
	f.mu.Lock()
	for _, s := range f.Sends[before:] {
		if s.Text == noID {
			refused = true
		}
		if strings.Contains(s.Text, "Send EXACTLY") {
			t.Error("a pay screen was shown despite the failed persist")
		}
	}
	f.mu.Unlock()
	if !refused {
		t.Errorf("customer was not told the rail is unavailable; last send = %q", lastSend(f))
	}
}

// ---- 7. restart recovery ----------------------------------------------------

// TestStartupRecoveryReadoptsBands: after a restart the persisted pending rows
// must re-enter the registry, so a NEW order at the same price still avoids the
// pre-restart amounts (telegram_bot.py:9527-9560, 9965-9993). Without this the
// picker would happily re-issue a live amount and one payment could settle two
// orders.
func TestStartupRecoveryReadoptsBands(t *testing.T) {
	b, _, st, _, _ := newUSDTBot(t)
	const total = 150000

	// A pre-restart BEP20 session, persisted only (in-memory state is "lost").
	oldDes := usdtOrder(t, st, 600, total)
	now := float64(time.Now().Unix())
	if err := st.Bep20PendingAdd(oldDes, "600", "6.00", total, 0, now+600); err != nil {
		t.Fatal(err)
	}
	// A TRON row for the SAME amount, to prove both rails rehydrate.
	oldTron := usdtOrder(t, st, 601, total)
	if err := st.TronPendingAdd(oldTron, "601", "6.21", total, 0, now+600); err != nil {
		t.Fatal(err)
	}

	b.rehydrateOnchainPending(bands.ProviderBEP20)
	b.rehydrateOnchainPending(bands.ProviderTron)

	if res, ok := b.bands.Get(bands.ProviderBEP20, oldDes); !ok || res.Amount != "6.00" {
		t.Fatalf("BEP20 row not re-adopted (%v, %t)", res, ok)
	}
	if _, ok := b.bands.Get(bands.ProviderTron, oldTron); !ok {
		t.Fatal("TRON row not re-adopted")
	}
	// A fresh order at the same price must avoid BOTH rehydrated amounts.
	fresh := usdtOrder(t, st, 602, total)
	reserveOrderBand(t, b, bands.ProviderBEP20, 602, fresh, total)
	u := bandUnits(t, b, bands.ProviderBEP20, fresh)
	for _, taken := range []int64{600, 621} {
		if d := u - taken; d > -21 && d < 21 {
			t.Errorf("fresh amount %d collides with rehydrated %d", u, taken)
		}
	}
}

// TestUSDTSweepFreesBandsPastGrace: the expiry sweep is the band-freeing backstop
// (_prune_usdt_pending @9606). A finished order past usdtPendingGrace loses its
// band and its persisted row; a still-'reserved' one keeps both.
func TestUSDTSweepFreesBandsPastGrace(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	const total = 150000

	keep := usdtOrder(t, st, 630, total)
	reserveOrderBand(t, b, bands.ProviderBEP20, 630, keep, total)
	drop := usdtOrder(t, st, 631, total)
	reserveOrderBand(t, b, bands.ProviderBEP20, 631, drop, total)
	st.ClaimOrder(drop, "reserved", "expired")

	// Grace measured from CreatedAt (the bands contract), so a zero grace makes
	// the finished row immediately sweepable while the reserved one is untouched.
	b.usdtPendingGrace = 0
	b.expirySweepOnce(ctx)

	if _, ok := b.bands.Get(bands.ProviderBEP20, drop); ok {
		t.Error("a finished session past the grace kept its band")
	}
	if _, ok := b.bands.Get(bands.ProviderBEP20, keep); !ok {
		t.Error("a still-reserved session lost its band")
	}
	rows, _ := st.Bep20PendingAll()
	for _, r := range rows {
		if r.Des == drop {
			t.Error("the persisted row of a swept session was not deleted")
		}
	}
}

func TestNewResolvesBandGeometryFromDotEnvSnapshot(t *testing.T) {
	for _, key := range []string{
		"BEP20_OFFSET_STEP", "BEP20_MATCH_TOL", "BEP20_AMOUNT_DECIMALS",
		"TRON_OFFSET_STEP", "TRON_MATCH_TOL", "TRON_AMOUNT_DECIMALS",
	} {
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("fixture broken: %s is set in the ambient environment, "+
				"so this test would pass even if New read raw env", key)
		}
	}

	b := envDurationTestBot(t,
		"BEP20_OFFSET_STEP=99\nBEP20_MATCH_TOL=17\nBEP20_AMOUNT_DECIMALS=4\n"+
			"TRON_OFFSET_STEP=77\nTRON_MATCH_TOL=13\nTRON_AMOUNT_DECIMALS=6\n")

	bepPrm, ok := b.bands.Params(bands.ProviderBEP20)
	if !ok {
		t.Fatal("no BEP20 params registered")
	}
	if bepPrm.Step != 99 || bepPrm.Tol != 17 || bepPrm.Decimals != 4 {
		t.Errorf("BEP20 params = %+v, want {Step:99 Tol:17 Decimals:4} (the .env-configured "+
			"geometry) — {101 50 5} would mean the shipped defaults leaked through", bepPrm)
	}
	tronPrm, ok := b.bands.Params(bands.ProviderTron)
	if !ok {
		t.Fatal("no TRON params registered")
	}
	if tronPrm.Step != 77 || tronPrm.Tol != 13 || tronPrm.Decimals != 6 {
		t.Errorf("TRON params = %+v, want {Step:77 Tol:13 Decimals:6} (the .env-configured "+
			"geometry) — {101 50 5} would mean the shipped defaults leaked through", tronPrm)
	}
}

// ---- task #17: the payout-address wizard must reach the customer screen ---
//
// Before this fix, sendUSDTScreen rendered internal/pay/bep20.Address()/
// tron.Address() — a raw os.Getenv read that never sees a settings.json
// write — so an admin who "successfully" ran /setbep20 or /settron kept
// showing every customer the STALE address forever. These tests drive the
// REAL admin wizard (adm:setbep20 / adm:settron, the same capSetBep20/
// capSetTron TestSetBep20Wizard/TestSetTronWizard exercise) and then call
// the REAL customer-facing sendUSDTScreen, asserting the rendered caption
// carries the NEW address and not the stale env one. Reverting usdt.go's
// b.bep20AddrStr()/b.tronAddrStr() calls back to bep20.Address()/
// tron.Address() reproduces the original bug and fails this test — verified
// by hand while writing this fix (not committed, since a self-reverting test
// would defeat itself the moment the revert lands back on top of it).

// TestBep20PayoutAddressWizardReachesCustomerPayScreen is the BEP20 half.
func TestBep20PayoutAddressWizardReachesCustomerPayScreen(t *testing.T) {
	ctx := context.Background()
	oldAddr := testSellerBSC                   // the .env/ambient value, set BEFORE Load below
	newAddr := "0x" + strings.Repeat("cd", 20) // admin rotates to this via the wizard
	t.Setenv("BEP20_ADDRESS", oldAddr)

	b, f, _ := newSettingsTestBot(t, "1") // super-admin id "1"

	// Run the REAL admin wizard end-to-end, exactly like TestSetBep20Wizard.
	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	b.Process(ctx, textUpdate(1, newAddr))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT BEP20 address set:\n"+newAddr) {
		t.Fatalf("wizard did not confirm the new address, got %+v", last)
	}

	// The customer-facing pay screen must now show newAddr, never oldAddr.
	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainBSC, "test-des", "5.23")
	screen := lastSendMsg(f)
	if screen == nil {
		t.Fatal("sendUSDTScreen sent nothing")
	}
	if !strings.Contains(screen.Text, newAddr) {
		t.Errorf("pay screen caption = %q, want it to contain the admin-set address %q", screen.Text, newAddr)
	}
	if strings.Contains(screen.Text, oldAddr) {
		t.Errorf("pay screen caption = %q, still contains the STALE env address %q — task #17 regressed", screen.Text, oldAddr)
	}
}

// TestTronPayoutAddressWizardReachesCustomerPayScreen is the TRON twin.
func TestTronPayoutAddressWizardReachesCustomerPayScreen(t *testing.T) {
	ctx := context.Background()
	oldAddr := testSellerTRX
	newAddr := "T" + strings.Repeat("9", 33)
	t.Setenv("TRON_ADDRESS", oldAddr)

	b, f, _ := newSettingsTestBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, newAddr))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "✅ USDT TRC20 (TRON) address set:\n"+newAddr) {
		t.Fatalf("wizard did not confirm the new address, got %+v", last)
	}

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainTRX, "test-des", "5.23")
	screen := lastSendMsg(f)
	if screen == nil {
		t.Fatal("sendUSDTScreen sent nothing")
	}
	if !strings.Contains(screen.Text, newAddr) {
		t.Errorf("pay screen caption = %q, want it to contain the admin-set address %q", screen.Text, newAddr)
	}
	if strings.Contains(screen.Text, oldAddr) {
		t.Errorf("pay screen caption = %q, still contains the STALE env address %q — task #17 regressed", screen.Text, oldAddr)
	}
}

// TestPayoutAddressWizardFlipsConfiguredGate covers the OTHER half of task
// #17 the two pay-screen tests above don't reach: New()'s b.bep20Configured/
// b.tronConfigured closures (dispatch.go), which gate cmdDeposit, the buy
// screen, and cbTogglePay's last-deposit-rail guard. sendUSDTScreen never
// calls Configured() itself, so a regression back to the pre-fix
// `b.bep20Configured = bep20.Configured` binding (raw env, task #17's
// original bug) would leave those two tests green while every Configured()
// gate stayed broken. This uses NO env address at all (an admin who
// configures purely through the wizard, never .env) so Configured() can only
// become true by actually reading settings.json through b.cfg.
func TestPayoutAddressWizardFlipsConfiguredGate(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BEP20_ADDRESS", "") // explicitly unset regardless of ambient env
	t.Setenv("TRON_ADDRESS", "")

	b, _, _ := newSettingsTestBot(t, "1")

	if b.bep20Configured() {
		t.Fatal("bep20Configured() = true before any address is set (env or settings.json)")
	}
	if b.tronConfigured() {
		t.Fatal("tronConfigured() = true before any address is set (env or settings.json)")
	}

	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	b.Process(ctx, textUpdate(1, "0x"+strings.Repeat("ef", 20)))
	if !b.bep20Configured() {
		t.Error("bep20Configured() = false after the wizard set a settings.json-only address — task #17 regressed")
	}

	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, "T"+strings.Repeat("8", 33)))
	if !b.tronConfigured() {
		t.Error("tronConfigured() = false after the wizard set a settings.json-only address — task #17 regressed")
	}
}

// ---- task #17 follow-up (independent review): the QR-consistency guard ----
//
// sendUSDTScreen's QR image knobs (*_QR_URL/*_QR_PATH) are env-only — no
// admin wizard writes them to settings.json — so once the caption started
// resolving through cfg (this task) a QR image became a SEPARATE source that
// can silently disagree with the address the caption shows. The guard drops
// the QR (falls back to the always-correct text-only caption) the instant
// its raw-env address stops matching the live cfg one. These three tests
// were flagged as missing by an independent review: the reviewer deleted
// both guard `if` blocks and got zero test failures across the whole module
// — proof the pre-fix caption tests alone don't cover this path.

// TestQRGuardShowsMatchingQR is the "no rotation" case: env and settings.json
// agree (settings.json was never touched), so the QR is exactly as
// trustworthy as it always was and must still be sent.
func TestQRGuardShowsMatchingQR(t *testing.T) {
	ctx := context.Background()
	addr := testSellerBSC
	qrURL := "https://example.com/bep20.png"
	t.Setenv("BEP20_ADDRESS", addr)
	t.Setenv("BEP20_QR_URL", qrURL)

	b, f, _, _, _ := newBuyBot(t, "1")

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainBSC, "test-des", "5.23")

	if len(f.Photos) != 1 {
		t.Fatalf("Photos = %d, want exactly 1 (QR must still show when the address is unrotated)", len(f.Photos))
	}
	if f.Photos[0].PhotoURL != qrURL {
		t.Errorf("QR URL = %q, want %q", f.Photos[0].PhotoURL, qrURL)
	}
	if !strings.Contains(f.Photos[0].Caption, addr) {
		t.Errorf("QR caption = %q, want it to contain %q", f.Photos[0].Caption, addr)
	}
}

// TestQRGuardDropsQROnRotation is the "rotated" case: the mutation this
// whole follow-up exists to catch. An admin rotates the BEP20 address via
// the real wizard; BEP20_QR_URL (env, untouched by the wizard) still encodes
// the OLD address. Without the guard this would send photos=1 with a QR for
// the wrong address next to correct-looking NEW-address text — worse than no
// fix at all, since a customer trusts the QR over reading the caption.
func TestQRGuardDropsQROnRotation(t *testing.T) {
	ctx := context.Background()
	oldAddr := testSellerBSC
	newAddr := "0x" + strings.Repeat("cd", 20)
	t.Setenv("BEP20_ADDRESS", oldAddr)
	t.Setenv("BEP20_QR_URL", "https://example.com/bep20.png")

	b, f, _, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:setbep20"))
	b.Process(ctx, textUpdate(1, newAddr))

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainBSC, "test-des", "5.23")

	if len(f.Photos) != 0 {
		t.Fatalf("Photos = %d, want 0 — a stale-address QR must never be sent (photos = %+v)", len(f.Photos), f.Photos)
	}
	screen := lastSendMsg(f)
	if screen == nil {
		t.Fatal("no text fallback sent after the QR was dropped")
	}
	if !strings.Contains(screen.Text, newAddr) {
		t.Errorf("fallback caption = %q, want it to contain the NEW address %q", screen.Text, newAddr)
	}
	if strings.Contains(screen.Text, oldAddr) {
		t.Errorf("fallback caption = %q, still contains the STALE address %q", screen.Text, oldAddr)
	}
}

// TestQRGuardNoLeakAcrossChains proves the BEP20 QR URL can never leak onto
// a TRON screen. BEP20 is left CORRECT (matches, so its half of the guard
// would set qrURL/qrPath to the BEP20 QR) while TRON is ROTATED (mismatch),
// which is exactly the state that would expose a missing `qrURL, qrPath =
// "", ""` reset ahead of the TRON check: without that reset, a leftover
// BEP20 QR value from the unconditional first assignment would survive into
// the TRON branch's failed-match case.
func TestQRGuardNoLeakAcrossChains(t *testing.T) {
	ctx := context.Background()
	bep20QRURL := "https://example.com/bep20.png"
	oldTron := testSellerTRX
	newTron := "T" + strings.Repeat("9", 33)
	t.Setenv("BEP20_ADDRESS", testSellerBSC) // left untouched -> BEP20 half of the guard matches
	t.Setenv("BEP20_QR_URL", bep20QRURL)
	t.Setenv("TRON_ADDRESS", oldTron)

	b, f, _, _, _ := newBuyBot(t, "1")

	b.Process(ctx, callbackUpdate(1, "adm:settron"))
	b.Process(ctx, textUpdate(1, newTron)) // rotate TRON only

	b.sendUSDTScreen(ctx, 1, i18n.Lang("en"), chainTRX, "test-des", "5.23")

	for _, p := range f.Photos {
		if p.PhotoURL == bep20QRURL {
			t.Fatalf("the BEP20 QR (%q) leaked onto the TRON screen: %+v", bep20QRURL, p)
		}
	}
	screen := lastSendMsg(f)
	if screen == nil {
		t.Fatal("no text fallback sent")
	}
	if !strings.Contains(screen.Text, newTron) {
		t.Errorf("fallback caption = %q, want it to contain the NEW TRON address %q", screen.Text, newTron)
	}
	if strings.Contains(screen.Text, oldTron) {
		t.Errorf("fallback caption = %q, still contains the STALE TRON address %q", screen.Text, oldTron)
	}
}
