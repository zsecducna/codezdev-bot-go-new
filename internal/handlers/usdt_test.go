package handlers

// usdt_test.go — acceptance pins for the USDT rails, ported from the Python
// suites named in the port mandate:
//
//	tests/test_usdt_redirect.py  — same-chat sibling redirect, cross-chat conflict,
//	                               exact-amount settle, window redirect, the picker
//	                               excluding a chat's own DEAD sessions
//	tests/test_usdt_verify.py    — confirmed/unconfirmed/reverted/wrong-recipient
//	                               by-hash lookups + the band-spacing invariant
//	tests/test_deposit_credit.py — the ±30c deposit credit decision (the pure
//	                               function itself is pinned in bands/credit_test.go;
//	                               here it is pinned END-TO-END through a deposit)
//
// Plus the integration pin this slice exists for: ONE bands.Registry shared by
// both surviving rails, so a live BEP20 band blocks the same TRON amount
// END-TO-END THROUGH THE HANDLERS — not merely in the bands unit tests.
//
// USDT migration note (2026-08-06): Binance Pay (the shared poller, its
// reconcile pass, tests/test_binance_match.py's port) was removed — the shop
// takes USDT via BEP20/TRON only (S3). TestBinancePollerMatchesMultiplePendings,
// TestBinancePollerRestartsAfterIdleExit, TestStartupRecoveryNoDoubleCredit and
// TestBinanceReconcileGrace went with it. Tests that merely used
// bands.ProviderBinance as a vehicle to prove the shared-registry property
// (TestUSDTSharedBandAcrossRails) were retargeted to the two surviving
// providers instead of deleted.
//
// No live network: the chain/exchange seams are driven by fakes, and
// TestUSDTRealClientsByHash additionally drives the REAL bep20/tron clients
// against httptest servers so the production seam bindings are covered too.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
)

// ---- harness ---------------------------------------------------------------

// testSellerBSC / testSellerTRX are shape-valid seller addresses (bep20 addrRE:
// 0x + 40 hex; tron addrRE: 'T' + 33 base58 chars).
var (
	testSellerBSC = "0x" + strings.Repeat("ab", 20) // 0x + 40 hex
	testSellerTRX = "T" + strings.Repeat("A", 33)   // T + 33 base58
)

// usdtFakes holds the scripted chain/exchange responses a test installs on the
// Bot's seams, plus call counters.
type usdtFakes struct {
	mu sync.Mutex
	// bscByHash is consulted per (txhash) lookup; nil result == "no confirmed
	// transfer to us" (unknown hash / reverted / too shallow / wrong recipient).
	bscByHash map[string]*bep20.TransferByHash
	trxByHash map[string]*tron.Transfer
	bscHits   atomic.Int64
}

func (u *usdtFakes) setBSC(hash string, tx *bep20.TransferByHash) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.bscByHash == nil {
		u.bscByHash = map[string]*bep20.TransferByHash{}
	}
	u.bscByHash[strings.ToLower(hash)] = tx
}

func (u *usdtFakes) setTRX(hash string, tx *tron.Transfer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.trxByHash == nil {
		u.trxByHash = map[string]*tron.Transfer{}
	}
	u.trxByHash[strings.ToLower(hash)] = tx
}

// newUSDTBot builds a Bot with both surviving USDT rails (BEP20, TRON)
// CONFIGURED and ENABLED, fast timers, and the chain seams pointed at
// scripted fakes. Binance Pay was removed by the USDT-only migration (S3).
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

	fk := &usdtFakes{}
	b.bep20ByHash = func(txhash string) (*bep20.TransferByHash, error) {
		fk.bscHits.Add(1)
		fk.mu.Lock()
		defer fk.mu.Unlock()
		return fk.bscByHash[strings.ToLower(strings.TrimPrefix(txhash, "0x"))], nil
	}
	b.tronByHash = func(txhash string, _ int64) (*tron.Transfer, error) {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		return fk.trxByHash[strings.ToLower(strings.TrimPrefix(txhash, "0x"))], nil
	}
	// Fast timers so the confirm-poll/sweeps run inside a test.
	b.usdtConfirmPoll = 2 * time.Millisecond
	b.usdtConfirmWait = 2 * time.Second
	b.expirySweepInterval = 5 * time.Millisecond
	return b, f, st, dir, fk
}

// TestUSDToVNDTrimPropagatesToOtherRateConsumers is the follow-up to task
// #25's whitespace-padded-ambient-rate fix. The original fix trimmed at two
// call sites (money.go's usdtDisp, usdt.go's vndToUSD); a review swept every
// ParseFloat(cfg.USDToVND()) call site in the handlers package and found
// THREE more that read the exact same un-trimmed rate — deposit.go's
// usdtToVND, apikey.go's usdtStrCfg, and adminpricing.go's adminUsdtToVnd.
// Two of those three are FUNCTIONAL rejections, not cosmetic: a padded rate
// used to make usdtToVND return 0, which fails capDeposit's >0 bound and
// refuses the customer's deposit outright, and made adminUsdtToVnd return
// (0,false), refusing an admin's typed price outright.
//
// The fix moved to the STRUCTURAL source instead of a fourth/fifth call-site
// trim: config.USDToVND() (internal/config/settings.go) now trims once,
// where the env-derived and settings.json-derived branches converge, so
// every current AND future ParseFloat(cfg.USDToVND()) caller is correct by
// construction rather than by each remembering to trim. This test verifies
// — not assumes — that the source fix actually reaches all three other
// consumers (adminpricing.go belongs to a different lane; this only CALLS
// its existing method to verify behavior, it does not modify that file).
func TestUSDToVNDTrimPropagatesToOtherRateConsumers(t *testing.T) {
	t.Setenv("USD_TO_VND", " 25000 ")
	b, _, _, _, _ := newBuyBot(t, "999")

	if got := b.usdtToVND(6); got != 150000 {
		t.Errorf("deposit.go usdtToVND(6) = %d, want 150000 (6 USDT at the padded-but-trimmed 25,000) — 0 means the rate is still un-trimmed and a real deposit would be REJECTED OUTRIGHT", got)
	}
	if got := b.usdtStrCfg(150000); got != "6.00" {
		t.Errorf("apikey.go usdtStrCfg(150000) = %q, want %q — empty means the rate is still un-trimmed", got, "6.00")
	}
	if vnd, ok := b.adminUsdtToVnd("6"); !ok || vnd != 150000 {
		t.Errorf("adminpricing.go adminUsdtToVnd(\"6\") = (%d,%t), want (150000,true) — (0,false) means the rate is still un-trimmed and a real admin price edit would be REJECTED OUTRIGHT", vnd, ok)
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

// deliverableOrder seeds real folder stock and reserves it, so settlePaid ->
// deliverPaid actually delivers (the pay-verify happy path needs this).
func deliverableOrder(t *testing.T, b *Bot, st *store.Store, dir string, chat int64, totalVND int64) string {
	t.Helper()
	seedProduct(t, dir, "prodDeliver", int(totalVND), 3)
	des := store.NewDes()
	reserved, err := b.stock.Reserve("prodDeliver", 1, des, b.reserveTTL)
	if err != nil || len(reserved) != 1 {
		t.Fatalf("stock reserve: %v (%d rows)", err, len(reserved))
	}
	if err := st.CreateOrder(des, fmtInt(chat), "en", "prodDeliver", "KBH", 1, totalVND); err != nil {
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
		_, rest, ok := strings.Cut(s.Text, "EXACTLY ")
		if !ok {
			continue
		}
		amount, _, ok := strings.Cut(rest, " USDT")
		if !ok {
			continue
		}
		amounts = append(amounts, amount)
	}
	return amounts
}

// bscTx builds a CONFIRMED BEP20 transfer to the seller of `amount` USDT at
// `ts` (epoch seconds) — the shape GetTransferToSellerByHash returns.
func bscTx(id, amount string, ts int64) *bep20.TransferByHash {
	units, err := bands.AmountUnits(amount, bep20.AmountDecimals())
	if err != nil {
		panic(err)
	}
	rat, ok := new(big.Rat).SetString(amount)
	if !ok {
		panic("bad amount " + amount)
	}
	return &bep20.TransferByHash{
		ID: id, Frm: "0xfeed", To: testSellerBSC, Amount: rat, Units: units,
		Block: 1000, TS: ts, Confirmed: true, Success: true,
	}
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
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)
	const total = 150000 // 6.00 USDT at 25,000 VND/USDT

	desB := usdtOrder(t, st, 102, total)
	desC := usdtOrder(t, st, 103, total)

	b.Process(ctx, callbackUpdate(102, "paybsc:"+desB))
	b.Process(ctx, callbackUpdate(103, "paytrx:"+desC))

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
	// Every rail actually rendered its screen (the flow really ran end-to-end).
	if !anySendContains(f, "USDT") && len(f.Photos) == 0 {
		t.Error("no pay screen was rendered by any rail")
	}
}

// TestUSDTBandExhaustedShowsBusy: when every slot for a price is taken the rail
// must answer USDT_BUSY and hand out NOTHING — refusing beats colliding
// (telegram_bot.py:9092 "no clear slot: refuse rather than collide"). BEP20 has
// 100//21 = 4 upward slots plus ONE last-resort slot below base.
func TestUSDTBandExhaustedShowsBusy(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)
	const total = 150000 // 6.00 USDT

	// Fill every slot from DISTINCT chats, so the own-dead-session rule (which
	// only forgives a chat's OWN expired sessions) cannot free one.
	taken := map[int64]bool{}
	const slots = 5 // 4 upward (100/21) + 1 last-resort downward
	for i := range slots {
		chat := int64(200 + i)
		des := usdtOrder(t, st, chat, total)
		b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
		u := bandUnits(t, b, bands.ProviderBEP20, des)
		if taken[u] {
			t.Fatalf("slot %d: amount %d handed out twice", i, u)
		}
		taken[u] = true
	}
	// The next same-price order has no unambiguous slot left.
	chat := int64(299)
	des := usdtOrder(t, st, chat, total)
	before := len(f.Sends)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))

	if _, ok := b.bands.Get(bands.ProviderBEP20, des); ok {
		t.Fatal("a band was reserved despite exhaustion — a colliding amount was handed out")
	}
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
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	const total = 150000
	const chat = 300

	// Four DEAD sessions owned by this chat (deadline already passed).
	for i := range 4 {
		des := store.NewDes()
		if err := b.bands.Adopt(bands.Reservation{
			Provider: bands.ProviderBEP20, Des: des, ChatID: fmtInt(chat),
			Amount:    bands.FormatUnits(int64(600+21*i), 2),
			CreatedAt: time.Now().Add(-time.Hour), Deadline: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatalf("adopt: %v", err)
		}
	}
	// The same chat re-orders: it must still get an amount (its own dead sessions
	// never block it), and the FIRST slot at that — the base price.
	des := usdtOrder(t, st, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
	if got := bandUnits(t, b, bands.ProviderBEP20, des); got != 600 {
		t.Errorf("own-dead-session retry got %d, want the base slot 600", got)
	}
}

// ---- 2. verify-by-hash: PAY order ------------------------------------------

// armTxid drives the real "I have made the payment" callback so pending_txid is
// armed exactly as production arms it, and returns the reserved amount.
func armTxid(t *testing.T, ctx context.Context, b *Bot, chat int64, chain, des string) string {
	t.Helper()
	p, _ := providerForChain(chain)
	res, ok := b.bands.Get(p, des)
	if !ok {
		t.Fatalf("no band for %s", des)
	}
	b.Process(ctx, callbackUpdate(chat, "usdtpaid:"+chain+":"+des))
	st := b.states.Get(chat)
	st.mu.Lock()
	armed := st.Txid != nil
	st.mu.Unlock()
	if !armed {
		t.Fatal("usdtpaid: did not arm pending_txid")
	}
	return res.Amount
}

// TestUSDTPayVerifySettlesExactlyOnce: the happy path delivers ONCE, and pasting
// the same hash again is refused by the processed-ledger PK (TXID_ALREADY_USED)
// — telegram_bot.py:9773-9776. Python pin: test_exact_amount_still_settles_directly.
func TestUSDTPayVerifySettlesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	b, f, st, dir, fk := newUSDTBot(t)
	const chat = 400
	const total = 150000

	des := deliverableOrder(t, b, st, dir, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
	amount := armTxid(t, ctx, b, chat, chainBSC, des)

	h := hash64("a")
	fk.setBSC(h, bscTx(h, amount, time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	o, _ := st.GetOrder(des)
	if o == nil || o.Status != "delivered" {
		t.Fatalf("order status = %v, want delivered", o)
	}
	docs := len(f.Docs)
	if docs == 0 {
		t.Error("nothing was delivered to the buyer")
	}
	// The band must be freed once the credit is resolved.
	if _, ok := b.bands.Get(bands.ProviderBEP20, des); ok {
		t.Error("band still held after a settled payment")
	}

	// Replay: re-arm and paste the SAME hash. Nothing more may be delivered.
	b.states.Get(chat).mu.Lock()
	b.states.Get(chat).Txid = &TxidCtx{Chain: chainBSC, Des: des}
	b.states.Get(chat).mu.Unlock()
	b.Process(ctx, textUpdate(chat, "0x"+h))
	if len(f.Docs) != docs {
		t.Errorf("a replayed hash delivered again (%d -> %d docs)", docs, len(f.Docs))
	}
}

// TestUSDTVerifyRejections pins every terminal/non-terminal refusal of the
// by-hash lookup (Python tests/test_usdt_verify.py): an unknown hash, a
// wrong-recipient tx, a REVERTED tx and a tx with too few confirmations all
// present as "no confirmed transfer" and are NON-terminal (the customer may
// paste the same id again once it confirms). None of them may move money.
func TestUSDTVerifyRejections(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fk := newUSDTBot(t)
	const chat = 410
	const total = 150000

	des := usdtOrder(t, st, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
	amount := armTxid(t, ctx, b, chat, chainBSC, des)

	cases := []struct {
		name string
		tx   *bep20.TransferByHash
	}{
		// GetTransferToSellerByHash returns nil for an unknown/unmined hash AND for
		// a tx carrying no USDT transfer to the seller (wrong recipient).
		{"unknown or wrong recipient", nil},
		{"reverted", func() *bep20.TransferByHash {
			tx := bscTx("h", amount, time.Now().Unix())
			tx.Success, tx.Confirmed = false, false
			return tx
		}()},
		{"too few confirmations", func() *bep20.TransferByHash {
			tx := bscTx("h", amount, time.Now().Unix())
			tx.Confirmed = false // success, but not deep enough yet
			return tx
		}()},
	}
	for i, tc := range cases {
		h := hash64(fmt.Sprintf("%d", i+1))
		fk.setBSC(h, tc.tx)
		before, _ := st.GetBalance(fmtInt(chat))
		terminal := b.verifyUSDTTxid(ctx, chat, chainBSC, des, h, false, false)
		if terminal {
			t.Errorf("%s: verify reported TERMINAL, want re-arm", tc.name)
		}
		if !anySendContains(f, "CONFIRMED") {
			t.Errorf("%s: customer was not told the transfer is unconfirmed", tc.name)
		}
		after, _ := st.GetBalance(fmtInt(chat))
		if after != before {
			t.Errorf("%s: balance moved %d -> %d on a rejected tx", tc.name, before, after)
		}
		if o, _ := st.GetOrder(des); o.Status != "reserved" {
			t.Errorf("%s: order left 'reserved' (%s) on a rejected tx", tc.name, o.Status)
		}
	}
}

// TestUSDTOutOfWindowRejected: a real, confirmed transfer whose on-chain
// timestamp falls outside the order's [creation, click] window (a stale/replayed
// transfer) is TERMINAL and never credited — TXID_WINDOW (telegram_bot.py:9722).
func TestUSDTOutOfWindowRejected(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fk := newUSDTBot(t)
	const chat = 420
	const total = 150000

	des := usdtOrder(t, st, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
	amount := armTxid(t, ctx, b, chat, chainBSC, des)

	h := hash64("b")
	// A day older than the order — well outside USDT_VERIFY_GRACE_S.
	fk.setBSC(h, bscTx(h, amount, time.Now().Add(-24*time.Hour).Unix()))

	if !b.verifyUSDTTxid(ctx, chat, chainBSC, des, h, false, false) {
		t.Error("an out-of-window tx must be TERMINAL")
	}
	if lastSend(f) != i18n.C().Pay.TxidWindow.T(i18n.EN) {
		t.Errorf("last send = %q, want TXID_WINDOW", lastSend(f))
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("balance = %d, want 0 — an out-of-window tx must never credit", bal)
	}
	if o, _ := st.GetOrder(des); o.Status != "reserved" {
		t.Errorf("order status = %s, want reserved", o.Status)
	}
}

// ---- 3. same-chat sibling redirect (depth-1) --------------------------------

// TestUSDTSiblingRedirect ports tests/test_usdt_redirect.py:
//   - a same-chat mismatch REDIRECTS to the sibling session whose band matches
//     (the dominant amount_conflict pattern: first order expires, the re-order
//     gets a new offset, and the already-sent payment matches the OLD band),
//   - the redirect is DEPTH-1: a call already flagged redirected never redirects
//     again (two mutually-matching sessions would otherwise ping-pong forever),
//   - a CROSS-CHAT band match is NOT a sibling — it stays a claim-jack refusal.
func TestUSDTSiblingRedirect(t *testing.T) {
	ctx := context.Background()
	b, f, st, dir, fk := newUSDTBot(t)
	const chat = 430
	const total = 150000

	// Sibling: the customer's FIRST session, deliverable, holding the base band.
	sibling := deliverableOrder(t, b, st, dir, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+sibling))
	sibAmount := bandUnits(t, b, bands.ProviderBEP20, sibling)

	// Current: the customer's re-order, which got a DIFFERENT offset amount.
	current := usdtOrder(t, st, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+current))
	curAmount := bandUnits(t, b, bands.ProviderBEP20, current)
	if sibAmount == curAmount {
		t.Fatal("the two sessions share an amount — the fixture is wrong")
	}
	armTxid(t, ctx, b, chat, chainBSC, current)

	// They paste, from the NEWEST screen, the hash of the payment they made for
	// the SIBLING's amount.
	h := hash64("c")
	fk.setBSC(h, bscTx(h, bands.FormatUnits(sibAmount, 2), time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	sibOrder, _ := st.GetOrder(sibling)
	if sibOrder.Status != "delivered" {
		t.Errorf("sibling order status = %s, want delivered (the redirect must settle it)", sibOrder.Status)
	}
	curOrder, _ := st.GetOrder(current)
	if curOrder.Status != "reserved" {
		t.Errorf("current order status = %s, want reserved (the payment was not for it)", curOrder.Status)
	}

	// DEPTH-1: the same call flagged `redirected` must NOT redirect again. The
	// sibling's band is gone now (it settled), so re-seed a matching sibling.
	sib2 := usdtOrder(t, st, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+sib2))
	u2 := bandUnits(t, b, bands.ProviderBEP20, sib2)
	h2 := hash64("d")
	fk.setBSC(h2, bscTx(h2, bands.FormatUnits(u2, 2), time.Now().Unix()))
	before, _ := st.GetOrder(sib2)
	b.verifyUSDTTxid(ctx, chat, chainBSC, current, h2, false, true) // already redirected
	after, _ := st.GetOrder(sib2)
	if before.Status != after.Status {
		t.Errorf("a depth-2 redirect fired: sibling went %s -> %s", before.Status, after.Status)
	}
	if lastSend(f) != i18n.C().Pay.TxidAmountConflict.T(i18n.EN) {
		t.Errorf("depth-2 last send = %q, want TXID_AMOUNT_CONFLICT", lastSend(f))
	}
}

// TestUSDTCrossChatStillConflicts: another customer's live band is NOT a sibling
// — the payment may genuinely belong to them, so the verify REFUSES rather than
// crediting the wrong session (telegram_bot.py:9824-9829). Python pin:
// test_cross_chat_mismatch_still_conflicts.
func TestUSDTCrossChatStillConflicts(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fk := newUSDTBot(t)
	const total = 150000
	const mine, theirs = 440, 441

	otherDes := usdtOrder(t, st, theirs, total)
	b.Process(ctx, callbackUpdate(theirs, "paybsc:"+otherDes))
	otherUnits := bandUnits(t, b, bands.ProviderBEP20, otherDes)

	myDes := usdtOrder(t, st, mine, total)
	b.Process(ctx, callbackUpdate(mine, "paybsc:"+myDes))
	armTxid(t, ctx, b, mine, chainBSC, myDes)

	h := hash64("e")
	fk.setBSC(h, bscTx(h, bands.FormatUnits(otherUnits, 2), time.Now().Unix()))
	b.Process(ctx, textUpdate(mine, "0x"+h))

	if lastSend(f) != i18n.C().Pay.TxidAmountConflict.T(i18n.EN) {
		t.Errorf("last send = %q, want TXID_AMOUNT_CONFLICT", lastSend(f))
	}
	if bal, _ := st.GetBalance(fmtInt(mine)); bal != 0 {
		t.Errorf("balance = %d, want 0 — a cross-chat band match must never credit", bal)
	}
	if o, _ := st.GetOrder(otherDes); o.Status != "reserved" {
		t.Errorf("the OTHER customer's order was touched (%s)", o.Status)
	}
}

// ---- 4. deposits -----------------------------------------------------------

// TestUSDTDepositCreditBoundaries pins the ±30c decision END-TO-END through a
// TRON deposit (Python tests/test_deposit_credit.py): a small UNDERpay credits
// the DESIRED top-up (the seller absorbs <=30c), an OVERpay within tolerance
// credits the ACTUAL amount (the customer is never shorted), and a way-off
// amount credits what actually arrived (money is never lost, a deposit never
// rejects).
func TestUSDTDepositCreditBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		recv       string // USDT actually received
		wantCredit int64  // VND credited
	}{
		{"short by one cent credits desired", "5.99", 150000},
		{"exact credits desired", "6.00", 150000},
		{"underpay within 30c credits desired", "5.71", 150000},
		{"overpay within tol credits actual", "6.30", 157500},
		{"way off credits actual", "8.00", 200000},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, _, st, _, fk := newUSDTBot(t)
			chat := int64(450 + i)

			// dep:trx -> amount prompt -> "6" (== 150,000 VND at 25,000/USDT).
			b.Process(ctx, callbackUpdate(chat, "dep:trx"))
			b.Process(ctx, textUpdate(chat, "6"))

			o := reservedDeposit(t, st, chat)
			// A deposit uses NO unique offset: the DESIRED amount, verbatim.
			res, ok := b.bands.Get(bands.ProviderTron, o.Des)
			if !ok || res.Amount != "6.00" {
				t.Fatalf("deposit amount = %q (found=%t), want the desired 6.00", res.Amount, ok)
			}
			armTxid(t, ctx, b, chat, chainTRX, o.Des)

			h := hash64("f")
			fk.setTRX(h, trxTx(h, tc.recv, time.Now().UnixMilli()))
			b.Process(ctx, textUpdate(chat, h))

			bal, _ := st.GetBalance(fmtInt(chat))
			if bal != tc.wantCredit {
				t.Errorf("credited %d VND, want %d", bal, tc.wantCredit)
			}
			// The deposit order is closed (history/revenue) but NOTHING delivered.
			done, _ := st.GetOrder(o.Des)
			if done.Status != "delivered" {
				t.Errorf("deposit order status = %s, want delivered (closed)", done.Status)
			}
		})
	}
}

// TestUSDTDepositReplayCreditsOnce: the processed-ledger PK is the sole deposit
// dedupe (CLAUDE.md: idempotency at the STORAGE layer). Re-submitting the same
// hash must credit nothing more and answer TXID_ALREADY_USED.
func TestUSDTDepositReplayCreditsOnce(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fk := newUSDTBot(t)
	const chat = 460

	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainBSC, o.Des)

	h := hash64("1")
	fk.setBSC(h, bscTx(h, "6.00", time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))
	first, _ := st.GetBalance(fmtInt(chat))
	if first != 150000 {
		t.Fatalf("first credit = %d, want 150000", first)
	}
	// Replay against the SAME (now closed) order: the band is gone, so the verify
	// stops at ORDER_GONE. Replay against a FRESH deposit still hits the ledger PK.
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))
	o2 := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainBSC, o2.Des)
	b.Process(ctx, textUpdate(chat, "0x"+h))

	again, _ := st.GetBalance(fmtInt(chat))
	if again != first {
		t.Errorf("a replayed hash credited again (%d -> %d)", first, again)
	}
	// Scoped to the customer's own chat, not lastSend's "most recent send
	// overall": this passes today only because a guard suppresses the owner
	// alert on the replay path, so lastSend happens to still be the
	// customer's TXID_ALREADY_USED reply. If that ordering ever changes,
	// lastSend would report a confusing failure about the WRONG message.
	got := lastSendMsgTo(f, chat)
	if got == nil || got.Text != i18n.C().Pay.TxidAlreadyUsed.T(i18n.EN) {
		t.Errorf("last send to chat = %v, want TXID_ALREADY_USED", got)
	}
}

// TestUSDTDepositAuditRowIsWellFormedAtHealthyRate is task #15's
// healthy-rate control for the on-chain settle audit line (usdt.go
// ~986, "usdt_"+chain+"_deposit"): at a healthy rate the row this line
// writes must be the ordinary signed "+X.XX USDT" figure, not the raw-VND
// fallback — proving TestAuditAmountFallsBackOnInvalidRate's fallback isn't
// unconditionally taken. Reads the row back from the STORE (RecentAudit),
// not auditAmount's return value, per this repo's own standard that an
// audit-row assertion must pin what actually persisted, not what a
// formatter merely returned.
//
// The invalid-rate counterpart of this SAME audit line is deliberately NOT
// pinned end-to-end here: usdtToVNDFor (usdt.go ~923, backed by
// bep20/tron.UsdtToVND) applies the identical rate<=0 predicate to the
// identical cfg.USDToVND() value earlier in this same verifyUSDTTxid call
// and re-arms (txidNotFound) before ever reaching this audit line, so a
// genuinely invalid rate can never actually reach this code path through
// the standard verify flow — traced, not assumed (see auditAmount's doc
// comment in money.go for the full reachability discussion).
// TestAuditAmountFallsBackOnInvalidRate pins the fallback branch itself
// directly instead, which is the only form in which it's reachable today.
func TestUSDTDepositAuditRowIsWellFormedAtHealthyRate(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fk := newUSDTBot(t)
	const chat = 4840

	before, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit before: %v", err)
	}

	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6")) // 6 USDT desired == 150,000 VND @25,000
	o := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainBSC, o.Des)

	// Overpay well outside tolerance so the credited VND is the CONVERTED
	// received value (bands/credit.go), not the order total verbatim — the
	// same reason TestUSDTDepositCreditsAtAdminSetRate overpays: an exact
	// payment would credit o.Total regardless of what usdtStr resolves,
	// proving nothing about this render.
	h := hash64("f")
	fk.setBSC(h, bscTx(h, "8.00", time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	bal, _ := st.GetBalance(fmtInt(chat))
	if bal != 200000 {
		t.Fatalf("fixture broken: credited %d VND, want 200000 (8 USDT at the healthy 25,000)", bal)
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
	if want := "+8.00 USDT"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q — the healthy-rate row must be the ordinary signed figure, not the raw-VND fallback", row.Outcome, want)
	}
}

// TestDepositAmountsAreUnique pins the hands-free-deposits behaviour swap:
// startOnchainDeposit now hands out a PICKED band amount (reserveUSDTAmount)
// instead of adopting the customer's round number verbatim (adoptUSDTAmount).
// Two deposits of the SAME round amount must be handed different band
// amounts — without this the watcher cannot tell whose payment is whose, and
// amount-based attribution is unsound.
func TestDepositAmountsAreUnique(t *testing.T) {
	b, f, st, _, _ := newUSDTBot(t)
	ctx := context.Background()
	const total = 3000000 // 120.00 USDT at 25,000 VND/USDT
	b.startOnchainDeposit(ctx, 111, i18n.EN, total, chainTRX)
	b.startOnchainDeposit(ctx, 222, i18n.EN, total, chainTRX)

	amounts := usdtScreenAmounts(t, f) // amounts rendered on the two deposit screens
	if len(amounts) != 2 {
		t.Fatalf("want 2 deposit screens, got %d: %+v", len(amounts), amounts)
	}
	// The FIRST (uncontended) deposit must still get the exact round base —
	// the picker only offsets on collision, it does not spread every deposit
	// out pre-emptively.
	if want := b.usdtStr(total); amounts[0] != want {
		t.Errorf("first deposit quoted %q, want the uncontended round base %q", amounts[0], want)
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

// TestUSDTDepositQuoteAbandonsOnInvalidRate is task #15's quote-site
// regression pin: startOnchainDeposit (usdt.go ~420) must keep calling
// usdtStr directly and ABANDONING when it returns "" — a customer quote may
// not be approximate, only an audit row may fall back to a marker.
//
// This does NOT just re-check the existing `if amount == ""` guard by
// deleting it outright: that specific mutation changes nothing observable
// here, because bands.Registry.Reserve (the deposit path since
// hands-free-deposits; formerly Adopt, same guard either way) parses the base
// amount via AmountUnits, which rejects "" too (empty is not a decimal), so
// reserveUSDTAmount still fails and falls through to the same noID reply — a
// wrong-reason pass. The same
// is true of literally routing this line through auditAmount: its fallback
// ("150000 VND (usdt rate unavailable)") isn't a decimal either, so
// AmountUnits rejects THAT too and the quote still abandons — verified,
// not assumed. What actually endangers a customer, and what this test is
// built to catch, is a fallback that IS a valid decimal — e.g. a future
// "simplification" that substitutes "0.00" for an empty usdtStr instead of
// abandoning, which Adopt accepts and ships as a real pay screen quoting
// zero. Verified red for exactly that mutation (usdt.go's
// `if amount == "" { b.abandonUSDTStub(...); ...; return }` swapped for
// `if amount == "" { amount = "0.00" }`), then restored — see the executor
// report.
func TestUSDTDepositQuoteAbandonsOnInvalidRate(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBotAtRate(t, "not-a-number")
	const chat = 4850

	before := len(f.Sends)
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))

	noID := i18n.C().Pay.BEP20NoID.T(i18n.EN)
	refused := false
	f.mu.Lock()
	for _, s := range f.Sends[before:] {
		if s.Text == noID {
			refused = true
		}
		if strings.Contains(s.Text, "VND (usdt rate unavailable)") {
			t.Errorf("a fallback audit string leaked into a customer-facing send: %q", s.Text)
		}
		if strings.Contains(s.Text, "Send EXACTLY") {
			t.Error("a pay screen was shown despite the invalid rate")
		}
	}
	f.mu.Unlock()
	if !refused {
		t.Errorf("customer was not told the rail is unavailable; sends since start: %+v", f.Sends[before:])
	}

	rows, err := st.OrdersForChat(fmtInt(chat), 20, []string{"reserved"}, nil)
	if err != nil {
		t.Fatalf("orders_for_chat: %v", err)
	}
	for _, o := range rows {
		if o.Product == depositProduct {
			t.Errorf("a reserved deposit order survived an invalid-rate quote: %+v", o)
		}
	}
}

// TestUSDTDepositOKRendersAmountAtWhitespacePaddedAmbientRate is task #25: a
// customer must see the actual credited/balance amounts on a successful
// deposit, not "credited — USDT, balance — USDT".
//
// Root cause, found by review and confirmed by probe before this test was
// written: config.Load's env snapshot copies the AMBIENT process environment
// verbatim (config.go:42-53) — only .env-FILE lines are trimmed, by
// parseEnvLines. So an operator-exported `USD_TO_VND=" 25000 "` used to reach
// every caller of cfg.USDToVND() with its whitespace intact. strconv.
// ParseFloat rejects surrounding whitespace outright, so before this fix
// usdtDisp/vndToUSD treated the padded-but-otherwise-valid rate as "no rate
// configured" and fell back to "— USDT" — while bep20.UsdtToVND/tron.
// UsdtToVND already TrimSpace the SAME rate string before parsing, so the
// wallet credit itself was (and remains) correct. That asymmetry is exactly
// what made the bug invisible to anyone checking the balance: the money was
// always right, only the two customer-facing renders in Deposit.OK ("amount"
// and "balance") were broken.
//
// Fixed at the SOURCE now, not just here: a follow-up review swept every
// ParseFloat(cfg.USDToVND()) call site and found three MORE with the exact
// same gap (deposit.go, apikey.go, adminpricing.go — two of them functional
// rejections, not display bugs), so config.USDToVND() itself trims
// (internal/config/settings.go). The trims in money.go/usdt.go are kept as
// deliberate defence in depth on this money path but are now redundant —
// this end-to-end test still passes through the real config chain, so it
// exercises the source fix regardless of which layer is doing the trimming.
//
// t.Setenv is the deliberate, correct fixture shape HERE — unlike the
// .env-FILE tests elsewhere in this package — because the defect lives
// specifically in the un-trimmed AMBIENT path; a .env-file fixture would be
// trimmed by parseEnvLines before it ever reached the bug. This bypasses
// newUSDTBot (which hardcodes an unpadded "25000" and calls newBuyBot, i.e.
// config.Load, before a caller could override it) so the padded value is in
// place when config.Load builds its snapshot.
func TestUSDTDepositOKRendersAmountAtWhitespacePaddedAmbientRate(t *testing.T) {
	ctx := context.Background()

	t.Setenv("USD_TO_VND", " 25000 ")
	t.Setenv("BEP20_ADDRESS", testSellerBSC)
	t.Setenv("TRON_ADDRESS", testSellerTRX)

	b, f, st, _, _ := newBuyBot(t, "999")
	s := b.cfg.Settings()
	s["pay_bep20"] = true
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	fk := &usdtFakes{}
	b.bep20ByHash = func(txhash string) (*bep20.TransferByHash, error) {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		return fk.bscByHash[strings.ToLower(strings.TrimPrefix(txhash, "0x"))], nil
	}
	b.usdtConfirmPoll = 2 * time.Millisecond
	b.usdtConfirmWait = 2 * time.Second
	b.expirySweepInterval = 5 * time.Millisecond

	// Fixture guard: confirm the padding is genuinely present in the AMBIENT
	// environment (not e.g. lost to shell quoting), so this test can't pass
	// for the wrong reason. Checked against os.Getenv directly, deliberately
	// NOT against b.cfg.USDToVND() — an earlier version of this guard
	// asserted cfg.USDToVND() == " 25000 " (the raw, un-trimmed value), which
	// broke the moment the fix moved to the structural source (config.
	// USDToVND() itself now trims, internal/config/settings.go): that
	// intermediate value became unreachable BY CONSTRUCTION, and the test
	// correctly refused to pass on a premise that had silently gone false.
	// This test's job is the END-TO-END property — a padded ambient rate
	// still produces a correct Deposit.OK — which survives regardless of
	// which layer does the trimming, so the only fixture assertion left is
	// that the padding genuinely reached the ambient environment; where it
	// gets trimmed is pinned separately in config_test.go
	// (TestUSDToVNDPrecedence/whitespace-padded_ambient_rate_is_trimmed).
	if env := os.Getenv("USD_TO_VND"); env != " 25000 " {
		t.Fatalf("fixture broken: ambient USD_TO_VND = %q, want the padded %q", env, " 25000 ")
	}

	const chat = 4840
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainBSC, o.Des)

	// Overpay outside tolerance, same reasoning as
	// TestUSDTDepositCreditBoundaries's "way off" case: an exact/near-exact
	// payment would credit the order total regardless of which rate the
	// credit path used, proving nothing about the rate resolution itself.
	h := hash64("f")
	fk.setBSC(h, bscTx(h, "8.00", time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	// Sanity: the wallet math must be unaffected by this bug — 8 USDT at the
	// padded-but-parseable 25,000 = 200,000 VND. bep20.UsdtToVND already
	// TrimSpace-s, so this should pass whether or not the display fix is in
	// place; if it fails, the fixture itself is broken.
	bal, _ := st.GetBalance(fmtInt(chat))
	if bal != 200000 {
		t.Fatalf("fixture broken: credited %d VND, want 200000 (wallet crediting should be unaffected by this bug)", bal)
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
		t.Errorf("Deposit.OK = %q — the whitespace-padded ambient rate made usdtDisp fall back to its empty-rate placeholder even though the deposit credited correctly", last.Text)
	}
	want := i18n.F(i18n.C().Deposit.OK.T(i18n.EN), "amount", "8.00 USDT", "balance", "8.00 USDT")
	if last.Text != want {
		t.Errorf("Deposit.OK = %q, want %q", last.Text, want)
	}
}

// TestVerifyUSDTTxidDepositNotifiesAdminOwner covers usdt.go's on-chain
// deposit settle (the isDeposit branch inside verifyUSDTTxid) —
// notify_admin_deposit (telegram_bot.py:9861-9862). Also pins the CARE
// requirement for this rail specifically: replaying the SAME tx hash is
// rejected by store.Credit's ledger PK BEFORE the notify call further down
// is ever reached, so a duplicate submit must not produce a second alert.
//
// This is NOT a duplicate-CREDIT guard, and must never be described as one:
// store.Credit's ledger primary key already does dedupe-and-credit
// atomically, so the balance can never double regardless of anything below
// it. The `if !credited` branch this test exercises (usdt.go, right after
// the store.Credit call) only stops the SIDE EFFECTS that would otherwise
// repeat if execution fell through past it — the customer DM, this owner
// alert, the order close/deliver claims, and the audit row.
//
// The replay is driven against a FRESH deposit order (o2), not the original
// (now-closed) one, mirroring TestUSDTDepositReplayCreditsOnce above and for
// the same reason: capTxid's pending_txid slot is nil'd the instant the
// first submit consumes it (usdt.go's capTxid: `cap := c.st.Txid; c.st.Txid
// = nil`), so re-submitting the identical text WITHOUT re-arming the
// capture never reaches verifyUSDTTxid at all — capTxid itself finds
// nothing armed and the text falls through one layer earlier. Re-arming via
// armTxid on o2 gives the second submit a live capture, so the same hash
// genuinely reaches verifyUSDTTxid and is rejected there, by the ledger PK
// — not by capTxid short-circuiting first, which would pass this test for
// the wrong reason (proving nothing about the notify-suppression this test
// claims to cover).
func TestVerifyUSDTTxidDepositNotifiesAdminOwner(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fk := newUSDTBot(t)
	const chat = 471

	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "5"))
	o := reservedDeposit(t, st, chat)
	amount := armTxid(t, ctx, b, chat, chainBSC, o.Des)

	h := hash64("d")
	fk.setBSC(h, bscTx(h, amount, time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "💰 NEW DEPOSIT") {
		t.Fatalf("owner alert = %v, want a 💰 NEW DEPOSIT message", alert)
	}
	if !strings.Contains(alert.Text, "USDT BEP20") {
		t.Errorf("deposit alert = %q, want the BEP20 rail label", alert.Text)
	}

	// Re-arm on a fresh order (o2) so the replay's capTxid slot is live and
	// the same hash genuinely reaches verifyUSDTTxid — see the doc comment
	// above for why replaying against the original closed order, without
	// re-arming, would never get there at all.
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "5"))
	o2 := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainBSC, o2.Des)
	b.Process(ctx, textUpdate(chat, "0x"+h))
	if n := countContains(f, "💰 NEW DEPOSIT"); n != 1 {
		t.Errorf("after replaying the same tx hash: NEW DEPOSIT count = %d, want still 1", n)
	}
}

// TestUSDTDepositVsProductBranch pins the M2.3 branch this slice must follow:
// the SAME settle machinery credits a __deposit__ order's wallet and delivers a
// product order — never both for one order.
func TestUSDTDepositVsProductBranch(t *testing.T) {
	ctx := context.Background()
	b, f, st, dir, fk := newUSDTBot(t)
	const chat = 470
	const total = 150000

	// (a) product order -> delivers, wallet untouched.
	prod := deliverableOrder(t, b, st, dir, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+prod))
	amount := armTxid(t, ctx, b, chat, chainBSC, prod)
	h := hash64("2")
	fk.setBSC(h, bscTx(h, amount, time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h))

	if o, _ := st.GetOrder(prod); o.Status != "delivered" {
		t.Errorf("product order status = %s, want delivered", o.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("a product payment credited the wallet (%d) — money must not do both", bal)
	}
	if len(f.Docs) == 0 {
		t.Error("product order delivered no file")
	}

	// (b) deposit order -> credits the wallet, delivers nothing.
	docsBefore := len(f.Docs)
	b.Process(ctx, callbackUpdate(chat, "dep:bsc"))
	b.Process(ctx, textUpdate(chat, "6"))
	dep := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainBSC, dep.Des)
	h2 := hash64("3")
	fk.setBSC(h2, bscTx(h2, "6.00", time.Now().Unix()))
	b.Process(ctx, textUpdate(chat, "0x"+h2))

	if bal, _ := st.GetBalance(fmtInt(chat)); bal != total {
		t.Errorf("deposit credited %d, want %d", bal, total)
	}
	if len(f.Docs) != docsBefore {
		t.Error("a deposit delivered accounts — the sentinel branch is wrong")
	}
}

// ---- 5. unconfirmed -> background confirm-poll ------------------------------

// TestUSDTUnconfirmedThenPollSettles: a hash submitted before confirmation must
// NOT be reported as "not found"; the background poll keeps checking quietly and
// settles automatically once it confirms (telegram_bot.py:9850-9878).
func TestUSDTUnconfirmedThenPollSettles(t *testing.T) {
	ctx := context.Background()
	b, f, st, dir, fk := newUSDTBot(t)
	const chat = 480
	const total = 150000

	des := deliverableOrder(t, b, st, dir, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
	amount := armTxid(t, ctx, b, chat, chainBSC, des)

	h := hash64("4")
	fk.setBSC(h, nil) // not indexed / not confirmed yet
	b.Process(ctx, textUpdate(chat, "0x"+h))

	if !anySendContains(f, "isn't confirmed on-chain yet") {
		t.Fatalf("expected TXID_WAITING, got %q", lastSend(f))
	}
	if o, _ := st.GetOrder(des); o.Status != "reserved" {
		t.Fatalf("order settled on an unconfirmed tx (%s)", o.Status)
	}
	// It confirms a moment later -> the poll settles it with no re-paste.
	fk.setBSC(h, bscTx(h, amount, time.Now().Unix()))
	if !waitFor(t, func() bool {
		o, _ := st.GetOrder(des)
		return o != nil && o.Status == "delivered"
	}) {
		t.Fatal("the confirmation poll never settled the order")
	}
	// The poll slot is released so the chat is not locked out.
	if !waitFor(t, func() bool {
		stt := b.states.Get(chat)
		stt.mu.Lock()
		defer stt.mu.Unlock()
		return !stt.USDTPolling
	}) {
		t.Error("USDTPolling was never cleared")
	}
}

// TestUSDTPollingOnePerChat pins the per-chat flag + the global cap
// (_usdt_polling @9856, USDT_MAX_POLLS @9853): one confirmation poll per chat,
// and the whole bot never exceeds maxUSDTPolls.
func TestUSDTPollingOnePerChat(t *testing.T) {
	b, _, _, _, _ := newUSDTBot(t)
	stA, stB := b.states.Get(1), b.states.Get(2)

	if started, capped := b.tryStartUSDTPoll(stA); !started || capped {
		t.Fatalf("first poll for a chat: started=%t capped=%t, want true/false", started, capped)
	}
	if started, capped := b.tryStartUSDTPoll(stA); started || capped {
		t.Errorf("second poll for the SAME chat: started=%t capped=%t, want false/false", started, capped)
	}
	// Global cap.
	b.maxUSDTPolls = 1
	if started, capped := b.tryStartUSDTPoll(stB); started || !capped {
		t.Errorf("over the global cap: started=%t capped=%t, want false/true", started, capped)
	}
	b.endUSDTPoll(stA)
	if started, capped := b.tryStartUSDTPoll(stB); !started || capped {
		t.Errorf("after a release: started=%t capped=%t, want true/false", started, capped)
	}
	if b.usdtPolls != 1 {
		t.Errorf("usdtPolls = %d, want 1", b.usdtPolls)
	}
}

// TestUSDTBadHashKeepsSlotArmed: a malformed paste is answered precisely and the
// capture STAYS armed, with no chain round-trip (telegram_bot.py:18986-18991).
func TestUSDTBadHashKeepsSlotArmed(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fk := newUSDTBot(t)
	const chat = 490

	des := usdtOrder(t, st, chat, 150000)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))
	armTxid(t, ctx, b, chat, chainBSC, des)

	before := fk.bscHits.Load()
	b.Process(ctx, textUpdate(chat, "not-a-hash"))

	if lastSend(f) != i18n.C().Pay.TxidBadFormat.T(i18n.EN) {
		t.Errorf("last send = %q, want TXID_BADFORMAT", lastSend(f))
	}
	if fk.bscHits.Load() != before {
		t.Error("a malformed paste hit the chain")
	}
	stt := b.states.Get(chat)
	stt.mu.Lock()
	armed := stt.Txid != nil
	stt.mu.Unlock()
	if !armed {
		t.Error("a malformed paste consumed the capture — it must stay armed for a retry")
	}
}

// TestUSDTPersistFailureRefusesScreen: a pending row that cannot be PERSISTED
// would not survive a restart (the reconcile/rehydrate could never see it), so
// the rail must undo its reservation and refuse rather than invite a payment it
// might be unable to match. Python raises out of the handler at
// store.bep20_pending_add (@9520) with the same effect.
func TestUSDTPersistFailureRefusesScreen(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)
	const chat = 650
	const total = 150000

	// Break the persisted table so *PendingAdd fails.
	if _, err := st.DB().Exec("DROP TABLE bep20_pending"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	des := usdtOrder(t, st, chat, total)
	before := len(f.Sends)
	b.Process(ctx, callbackUpdate(chat, "paybsc:"+des))

	if _, ok := b.bands.Get(bands.ProviderBEP20, des); ok {
		t.Error("the band was kept for a session that could not be persisted — the slot is now held by a session nobody can settle")
	}
	if _, ok := b.getUSDTRow(bands.ProviderBEP20, des); ok {
		t.Error("the rail-side row survived a failed persist")
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
	ctx := context.Background()
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
	b.Process(ctx, callbackUpdate(602, "paybsc:"+fresh))
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
	b.Process(ctx, callbackUpdate(630, "paybsc:"+keep))
	drop := usdtOrder(t, st, 631, total)
	b.Process(ctx, callbackUpdate(631, "paybsc:"+drop))
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

// ---- 8. the real clients, no live network ----------------------------------

// TestUSDTRealClientsByHash drives the PRODUCTION seam bindings (bep20 JSON-RPC
// and TRON TronGrid) against httptest servers, so the wiring New installs — not
// just the test fakes — is covered. Nothing leaves the process.
func TestUSDTRealClientsByHash(t *testing.T) {
	t.Setenv("USD_TO_VND", "25000")
	t.Setenv("BEP20_ADDRESS", testSellerBSC)
	t.Setenv("TRON_ADDRESS", testSellerTRX)

	// --- BEP20: eth_getTransactionReceipt + eth_blockNumber + getBlockByNumber.
	sellerTopic := "0x000000000000000000000000" + strings.TrimPrefix(testSellerBSC, "0x")
	fromTopic := "0x" + strings.Repeat("0", 24) + strings.Repeat("b", 40)
	// 6.00 USDT at 18 decimals = 6 * 10^18.
	weiHex := fmt.Sprintf("0x%x", new(big.Int).Mul(big.NewInt(6), pow10BigInt(18)))
	bsc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req.Method {
		case "eth_getTransactionReceipt":
			result = map[string]any{
				"blockNumber": "0x64", "status": "0x1",
				"logs": []any{map[string]any{
					"address": bep20.USDTContract,
					"data":    weiHex,
					"topics":  []string{bep20.TransferTopic, fromTopic, sellerTopic},
				}},
			}
		case "eth_blockNumber":
			result = "0x100" // 256 >> 100 + 12 confirmations
		case "eth_getBlockByNumber":
			result = map[string]any{"timestamp": "0x60000000"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer bsc.Close()
	oldRPC := bep20.RPCEndpoints
	bep20.RPCEndpoints = []string{bsc.URL}
	defer func() { bep20.RPCEndpoints = oldRPC }()

	// --- TRON: the TRC20 transactions endpoint.
	trxHash := hash64("7")
	trx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []any{map[string]any{
				"transaction_id":  trxHash,
				"type":            "Transfer",
				"from":            "TSender",
				"value":           "6000000", // 6.00 USDT at 6 decimals
				"block_timestamp": float64(time.Now().UnixMilli()),
				"token_info":      map[string]any{"address": tron.USDTContract},
			}},
		})
	}))
	defer trx.Close()
	t.Setenv("TRON_API_BASE", trx.URL)

	// Build the bot AFTER the env/endpoint overrides so New binds the real
	// clients against the fakes, via newBuyBot (NOT newUSDTBot/newUSDTBotAtRate
	// — those unconditionally overwrite b.bep20ByHash/b.tronByHash with
	// scripted fakes for the buy/deposit-flow tests, which this test does not
	// need and must not go through: this test exists specifically to exercise
	// the closures New() installs (dispatch.go) and nothing else. An earlier
	// version of this test built via newUSDTBot and then hand-reconstructed
	// "the real closure" here to undo that override — which silently drifted
	// out of sync with dispatch.go (it was still calling the pre-task#42
	// bep20.GetTransferToSellerByHash/tron.FindConfirmedTransferByHash 2-
	// and 4-arg forms, missing the BEP20_MIN_CONF/TRON_PAGE_LIMIT/
	// TRON_API_BASE plumbing dispatch.go now does) and so quietly stopped
	// testing what its own doc comment claims. Not touching b.bep20ByHash/
	// b.tronByHash at all is the only way this test can't go stale again.
	b, _, _, _, _ := newBuyBot(t, "999")

	tx, err := b.bep20ByHash("0x" + hash64("8"))
	if err != nil || tx == nil {
		t.Fatalf("bep20 by-hash = (%v, %v), want a transfer", tx, err)
	}
	if !tx.Confirmed || tx.Units != 600 {
		t.Errorf("bep20 transfer confirmed=%t units=%d, want true/600", tx.Confirmed, tx.Units)
	}

	tr, err := b.tronByHash(trxHash, 0)
	if err != nil || tr == nil {
		t.Fatalf("tron by-hash = (%v, %v), want a transfer", tr, err)
	}
	if tr.Units != 600 {
		t.Errorf("tron transfer units = %d, want 600", tr.Units)
	}
}

// ---- task #42: pin the CALLER wiring, not just the package plumbing -------
//
// The tests above (and internal/pay/bep20, internal/pay/tron's own test
// suites) already prove that an INJECTED value is honoured once it reaches
// MatchPaymentConf/GetTransferToSellerByHashConf/GetIncomingTransfersConf/
// bands.Config.EvictAfter/bands.DepositCredit's tolUnits param. None of that
// proves dispatch.go/usdt.go actually RESOLVE the right key and PASS it in —
// a typo'd key string (BEP20_MINCONF instead of BEP20_MIN_CONF) would build
// clean and pass every package-level test while the knob stayed just as dead
// as before task #42. These four tests close that gap, one per fixed call
// site, each built the same way: write the knob into dir/.env (NEVER
// t.Setenv) before config.Load, so a raw package-level os.Getenv/envInt read
// sees nothing (production's systemd unit exports nothing either — no
// EnvironmentFile, config.Load's own doc comment) while config.Config.Env
// (and b.envInt/b.cfg.EnvOr, which read through it) see the .env-supplied
// value. Each fixture asserts the key is absent from the ambient environment
// FIRST, so none of these can pass "by accident" if the shell running the
// suite happens to already export one of these names.

// TestBep20ByHashResolvesMinConfFromDotEnvSnapshot pins dispatch.go's
// b.bep20ByHash closure: BEP20_MIN_CONF must reach
// GetTransferToSellerByHashConf's minConf parameter via b.envInt(b.cfg),
// not bep20.MinConfirmations()'s raw env read.
//
// The fake BSC node reports the transfer at block 0x64 (100) with a chain
// head of 0x69 (105) — exactly 5 confirmations deep. BEP20_MIN_CONF=3 (via
// .env only) must confirm it (5>=3); the compiled default of 12 would not
// (5<12). Since BEP20_MIN_CONF is never set ambient here, a caller that
// dropped back to the raw bep20.MinConfirmations() read (or typo'd the key)
// would silently see "" -> the 12 default -> Confirmed=false, so this test
// only passes when dispatch.go's b.envInt("BEP20_MIN_CONF", ...) call is
// both present and correctly spelled.
//
// Falsification: change dispatch.go's b.bep20ByHash closure back to call
// bep20.GetTransferToSellerByHash(b.bep20AddrStr(), txhash) (the pre-#42
// 2-arg form, which reads MinConfirmations() itself) and tx.Confirmed comes
// back false instead of true.
func TestBep20ByHashResolvesMinConfFromDotEnvSnapshot(t *testing.T) {
	if _, ok := os.LookupEnv("BEP20_MIN_CONF"); ok {
		t.Fatal("fixture broken: BEP20_MIN_CONF is set in the ambient environment, " +
			"so this test would pass even if the caller read raw env")
	}

	sellerTopic := "0x000000000000000000000000" + strings.TrimPrefix(testSellerBSC, "0x")
	fromTopic := "0x" + strings.Repeat("0", 24) + strings.Repeat("b", 40)
	weiHex := fmt.Sprintf("0x%x", new(big.Int).Mul(big.NewInt(3), pow10BigInt(18))) // 3.00 USDT
	bsc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req.Method {
		case "eth_getTransactionReceipt":
			result = map[string]any{
				"blockNumber": "0x64", "status": "0x1", // block 100
				"logs": []any{map[string]any{
					"address": bep20.USDTContract,
					"data":    weiHex,
					"topics":  []string{bep20.TransferTopic, fromTopic, sellerTopic},
				}},
			}
		case "eth_blockNumber":
			result = "0x69" // block 105 -> 5 confirmations deep
		case "eth_getBlockByNumber":
			result = map[string]any{"timestamp": "0x60000000"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer bsc.Close()
	oldRPC := bep20.RPCEndpoints
	bep20.RPCEndpoints = []string{bsc.URL}
	defer func() { bep20.RPCEndpoints = oldRPC }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"BEP20_ADDRESS="+testSellerBSC+"\n"+
			"BEP20_MIN_CONF=3\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })

	// b.bep20ByHash is the exact closure dispatch.go's New() installs — not
	// overridden here — so this calls production code all the way down to
	// confirmedAt's minConf parameter.
	tx, err := b.bep20ByHash("0x" + hash64("3"))
	if err != nil {
		t.Fatalf("bep20ByHash error: %v", err)
	}
	if tx == nil {
		t.Fatal("bep20ByHash = nil, want a transfer (5 confs deep must at least be FOUND)")
	}
	if !tx.Confirmed {
		t.Errorf("bep20ByHash Confirmed = false, want true — 5 confs >= the .env-configured " +
			"BEP20_MIN_CONF=3 (a fallback to the 12 default would reject it: 5<12)")
	}
}

// TestTronByHashResolvesPageLimitAndAPIBaseFromDotEnvSnapshot pins
// dispatch.go's b.tronByHash closure: TRON_PAGE_LIMIT and TRON_API_BASE must
// reach FindConfirmedTransferByHashConf's pageLimit/base parameters via
// b.envInt/b.cfg.EnvOr, not tron.PageLimit()/apiBase()'s raw env reads.
//
// Both knobs are set via .env only, to a distinctive TRON_PAGE_LIMIT=13
// (default 200) and to this test's own httptest server as TRON_API_BASE
// (default "https://api.trongrid.io"). A caller that dropped back to the raw
// readers would send limit=200 to the WRONG host entirely — this test
// asserts on the actual "limit" query parameter the fake TronGrid receives,
// which only comes back "13" if both the base (to reach this server at all)
// and the limit (the value itself) resolved through cfg.
//
// Falsification: change dispatch.go's b.tronByHash closure back to call
// tron.FindConfirmedTransferByHash(b.tronAddrStr(), txhash, minTSMS,
// b.tronAPIKeyStr()) (the pre-#42 4-arg form) and this test times out /
// errors dialing the real api.trongrid.io instead of seeing "13".
//
// LOAD-BEARING BEYOND ITS STATED PURPOSE: verified by hand that on that same
// revert the request does not merely fail closed — it actually leaves the
// process and hits the real api.trongrid.io (which answered with a live 400
// "valid account address required"). So this test is also what keeps `go
// test` from making an outbound network call to a third party the moment
// this wiring regresses — do not delete it as "just an env plumbing test."
func TestTronByHashResolvesPageLimitAndAPIBaseFromDotEnvSnapshot(t *testing.T) {
	for _, key := range []string{"TRON_PAGE_LIMIT", "TRON_API_BASE"} {
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("fixture broken: %s is set in the ambient environment, "+
				"so this test would pass even if the caller read raw env", key)
		}
	}

	trxHash := hash64("4")
	var gotLimit string
	trx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []any{map[string]any{
				"transaction_id":  trxHash,
				"type":            "Transfer",
				"from":            "TSender",
				"value":           "3000000", // 3.00 USDT at 6 decimals
				"block_timestamp": float64(time.Now().UnixMilli()),
				"token_info":      map[string]any{"address": tron.USDTContract},
			}},
		})
	}))
	defer trx.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"TRON_ADDRESS="+testSellerTRX+"\n"+
			"TRON_PAGE_LIMIT=13\n"+
			"TRON_API_BASE="+trx.URL+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })

	// b.tronByHash is the exact closure dispatch.go's New() installs.
	tr, err := b.tronByHash(trxHash, 0)
	if err != nil {
		t.Fatalf("tronByHash error: %v", err)
	}
	if tr == nil {
		t.Fatal("tronByHash = nil, want the matching transfer")
	}
	if gotLimit != "13" {
		t.Errorf("TronGrid saw limit=%q, want \"13\" (the .env-configured TRON_PAGE_LIMIT) — "+
			"\"200\" would mean the raw default leaked through", gotLimit)
	}
}

// TestNewResolvesBandGeometryFromDotEnvSnapshot pins dispatch.go's bands.New
// call: BEP20_OFFSET_STEP/BEP20_MATCH_TOL/BEP20_AMOUNT_DECIMALS and their
// TRON_ counterparts must reach the registry's Params via b.envInt(b.cfg),
// not bep20.AmountOffsetStep()/tron.AmountOffsetStep() etc.'s raw env reads.
// (USDT_BAND_EVICT_S sits on the exact same bands.Config{EvictAfter: ...}
// literal, same b.envInt call shape — see this test's own doc for why it is
// not independently pinned here.)
//
// Every geometry value is set via .env only, to numbers that don't collide
// with the shipped defaults (Step 21/Tol 10/Decimals 2 for both rails).
//
// Falsification: revert dispatch.go's bands.Config{Providers: ...} literal
// back to calling bep20.AmountOffsetStep()/AmountMatchTol()/AmountDecimals()
// and tron's equivalents directly, and Params comes back {21,10,2} for both
// providers instead of the .env-configured values.
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
		"BEP20_OFFSET_STEP=99\nBEP20_MATCH_TOL=17\nBEP20_AMOUNT_DECIMALS=3\n"+
			"TRON_OFFSET_STEP=77\nTRON_MATCH_TOL=13\nTRON_AMOUNT_DECIMALS=4\n")

	bepPrm, ok := b.bands.Params(bands.ProviderBEP20)
	if !ok {
		t.Fatal("no BEP20 params registered")
	}
	if bepPrm.Step != 99 || bepPrm.Tol != 17 || bepPrm.Decimals != 3 {
		t.Errorf("BEP20 params = %+v, want {Step:99 Tol:17 Decimals:3} (the .env-configured "+
			"geometry) — {21 10 2} would mean the shipped defaults leaked through", bepPrm)
	}
	tronPrm, ok := b.bands.Params(bands.ProviderTron)
	if !ok {
		t.Fatal("no TRON params registered")
	}
	if tronPrm.Step != 77 || tronPrm.Tol != 13 || tronPrm.Decimals != 4 {
		t.Errorf("TRON params = %+v, want {Step:77 Tol:13 Decimals:4} (the .env-configured "+
			"geometry) — {21 10 2} would mean the shipped defaults leaked through", tronPrm)
	}
}

// TestUSDTDepositCreditResolvesMatchTolFromDotEnvSnapshot is the money-path
// pin — the highest priority, per the review that requested this file:
// DEPOSIT_MATCH_TOL_CENTS must reach bands.DepositCredit's tolUnits
// parameter (usdt.go's real settle path) via b.envInt(b.cfg), not
// bands.DepositMatchTolCents()'s raw env read, driven through an ACTUAL
// deposit (dep:trx -> desired amount -> submit hash -> balance credited),
// not a call to the pure function directly (that half is already pinned in
// bands/credit_test.go — this test exists to catch usdt.go's call site
// forgetting to resolve the value at all).
//
// Desired top-up: 6.00 USDT (150,000 VND @ 25,000/USDT). Received: 5.50 USDT
// — 50 cents short, which is OUTSIDE the compiled default tolerance (30c)
// but INSIDE the .env-configured DEPOSIT_MATCH_TOL_CENTS=200. Under correct
// resolution the underpay rounds UP to the full desired 150,000 VND
// (bands.DepositCredit's within-tolerance rule); under the 30c default it
// would instead credit only the actual 137,500 VND received.
//
// Falsification: change usdt.go's verifyUSDTTxid deposit branch back to
// bands.DepositCredit(recvUnits, desiredUnits, o.Total, recvVND,
// bands.DepositMatchTolCents()) (the pre-#42 direct raw-env call) and the
// credited balance comes back 137,500 instead of 150,000.
func TestUSDTDepositCreditResolvesMatchTolFromDotEnvSnapshot(t *testing.T) {
	if _, ok := os.LookupEnv("DEPOSIT_MATCH_TOL_CENTS"); ok {
		t.Fatal("fixture broken: DEPOSIT_MATCH_TOL_CENTS is set in the ambient environment, " +
			"so this test would pass even if the caller read raw env")
	}

	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"USD_TO_VND=25000\n"+
			"TRON_ADDRESS="+testSellerTRX+"\n"+
			"DEPOSIT_MATCH_TOL_CENTS=200\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "999"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })

	// pay_tron defaults OFF; turn it on the same way newUSDTBotAtRate does.
	s := b.cfg.Settings()
	s["pay_tron"] = true
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	// The chain-lookup seam is scripted (this test targets the CREDIT-DECISION
	// wiring, not the TRON client wiring — that half is TestTronByHash...
	// above) — matches newUSDTBotAtRate's own fake shape.
	trxByHash := map[string]*tron.Transfer{}
	b.tronByHash = func(txhash string, _ int64) (*tron.Transfer, error) {
		return trxByHash[strings.ToLower(strings.TrimPrefix(txhash, "0x"))], nil
	}

	const chat = 470
	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6")) // desired 6.00 USDT = 150,000 VND

	o := reservedDeposit(t, st, chat)
	armTxid(t, ctx, b, chat, chainTRX, o.Des)

	h := hash64("5")
	trxByHash[strings.ToLower(h)] = trxTx(h, "5.50", time.Now().UnixMilli()) // 50c short
	b.Process(ctx, textUpdate(chat, h))

	bal, _ := st.GetBalance(fmtInt(chat))
	if bal != 150000 {
		t.Errorf("credited %d VND, want 150000 — a 50c underpay is outside the 30c default "+
			"tolerance but inside the .env-configured 200c one, so it must round up to the "+
			"full desired amount; 137500 would mean the default (30c) leaked through and the "+
			"underpay was credited at its actual (out-of-tolerance) value instead", bal)
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
