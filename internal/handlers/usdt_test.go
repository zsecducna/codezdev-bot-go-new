package handlers

// usdt_test.go — acceptance pins for the USDT rails, ported from the Python
// suites named in the port mandate:
//
//	tests/test_usdt_redirect.py  — same-chat sibling redirect, cross-chat conflict,
//	                               exact-amount settle, window redirect, the picker
//	                               excluding a chat's own DEAD sessions
//	tests/test_usdt_verify.py    — confirmed/unconfirmed/reverted/wrong-recipient
//	                               by-hash lookups + the band-spacing invariant
//	tests/test_binance_match.py  — the shared poller's match invariants
//	tests/test_deposit_credit.py — the ±30c deposit credit decision (the pure
//	                               function itself is pinned in bands/credit_test.go;
//	                               here it is pinned END-TO-END through a deposit)
//
// Plus the integration pin this slice exists for: ONE bands.Registry shared by
// all three rails, so a live Binance band blocks the same TRON/BEP20 amount
// END-TO-END THROUGH THE HANDLERS — not merely in the bands unit tests.
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/binance"
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

// usdtFakes holds the scripted chain/exchange responses a test installs on the
// Bot's seams, plus call counters.
type usdtFakes struct {
	mu sync.Mutex
	// bscByHash is consulted per (txhash) lookup; nil result == "no confirmed
	// transfer to us" (unknown hash / reverted / too shallow / wrong recipient).
	bscByHash map[string]*bep20.TransferByHash
	trxByHash map[string]*tron.Transfer
	// binanceTxns is the whole pay/transactions payload the shared poller sees.
	binanceTxns []binance.Transaction
	binanceHits atomic.Int64
	bscHits     atomic.Int64
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

func (u *usdtFakes) setBinanceTxns(txns []binance.Transaction) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.binanceTxns = txns
}

// newUSDTBot builds a Bot with all three USDT rails CONFIGURED and ENABLED, fast
// timers, and the chain/exchange seams pointed at scripted fakes.
//
// Env is set BEFORE handlers.New so the band geometry, the seller addresses and
// the USD_TO_VND rate are all in place when the registry is built.
func newUSDTBot(t *testing.T) (*Bot, *fakeTG, *store.Store, string, *usdtFakes) {
	t.Helper()
	t.Setenv("USD_TO_VND", "25000") // 1 USDT = 25,000 VND
	t.Setenv("BEP20_ADDRESS", testSellerBSC)
	t.Setenv("TRON_ADDRESS", testSellerTRX)
	t.Setenv("BINANCE_PAY_ID", "payid-1")
	t.Setenv("BINANCE_API_KEY", "k")
	t.Setenv("BINANCE_SECRET_KEY", "s")

	b, f, st, _, dir := newBuyBot(t, "999")
	// bep20/tron default OFF (pay_enabled @5150) — turn every USDT rail on.
	// SaveSettings persists the WHOLE dict, so edit the live settings rather than
	// a hand-built subset (which would drop every other default).
	s := b.cfg.Settings()
	s["pay_bep20"], s["pay_tron"], s["pay_binance"] = true, true, true
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
	b.binanceGetTxns = func(_ *int64) ([]binance.Transaction, error) {
		fk.binanceHits.Add(1)
		fk.mu.Lock()
		defer fk.mu.Unlock()
		return append([]binance.Transaction(nil), fk.binanceTxns...), nil
	}
	// Fast timers so the pollers/sweeps run inside a test.
	b.binancePollInterval = 2 * time.Millisecond
	b.usdtConfirmPoll = 2 * time.Millisecond
	b.usdtConfirmWait = 2 * time.Second
	b.expirySweepInterval = 5 * time.Millisecond
	return b, f, st, dir, fk
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

// ---- 1. the integration pin: ONE registry across all three rails ------------

// TestUSDTSharedBandAcrossRails is the reason this slice exists: a live Binance
// band must block the same amount on TRON and BEP20 END-TO-END THROUGH THE
// HANDLERS. Python kept three independent registries, so a Binance amount could
// equal a live TRON amount; this port routes every rail through the ONE injected
// bands.Registry (bands deviation 1), whose collision set spans all providers.
//
// Three same-price orders, one per rail, each opened by tapping its real pay
// button: every pair of live amounts must stay at least max(tol)*2+1 units
// apart, so one incoming payment can never fall inside two bands.
func TestUSDTSharedBandAcrossRails(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newUSDTBot(t)
	const total = 150000 // 6.00 USDT at 25,000 VND/USDT

	desA := usdtOrder(t, st, 101, total)
	desB := usdtOrder(t, st, 102, total)
	desC := usdtOrder(t, st, 103, total)

	b.Process(ctx, callbackUpdate(101, "paybnb:"+desA))
	b.Process(ctx, callbackUpdate(102, "paybsc:"+desB))
	b.Process(ctx, callbackUpdate(103, "paytrx:"+desC))

	uA := bandUnits(t, b, bands.ProviderBinance, desA)
	uB := bandUnits(t, b, bands.ProviderBEP20, desB)
	uC := bandUnits(t, b, bands.ProviderTron, desC)

	// Cross-provider spacing is max(tolA,tolB)*2+1 — full band disjointness from
	// EITHER side's tolerance (bands deviation 1).
	minGap := int64(2*max(bep20.AmountMatchTol(), max(tron.AmountMatchTol(), binance.AmountMatchTol())) + 1)
	for _, pair := range [][3]any{
		{"binance/bep20", uA, uB}, {"binance/tron", uA, uC}, {"bep20/tron", uB, uC},
	} {
		name := pair[0].(string)
		x, y := pair[1].(int64), pair[2].(int64)
		if x == y {
			t.Fatalf("%s: both rails handed out the SAME amount %d — the shared registry is not shared", name, x)
		}
		if d := x - y; d < 0 && -d < minGap || d >= 0 && d < minGap {
			t.Errorf("%s: amounts %d and %d are closer than the %d-unit min gap — bands overlap", name, x, y, minGap)
		}
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
	if lastSend(f) != i18n.C().Pay.TxidAlreadyUsed.T(i18n.EN) {
		t.Errorf("last send = %q, want TXID_ALREADY_USED", lastSend(f))
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

// ---- 6. Binance: the ONE shared poller --------------------------------------

// TestBinancePollerMatchesMultiplePendings: ONE pay/transactions call must settle
// EVERY pending Binance order it matches (telegram_bot.py:9190-9247) — that is
// the whole point of a single shared poller (bounded API weight).
func TestBinancePollerMatchesMultiplePendings(t *testing.T) {
	ctx := context.Background()
	b, _, st, dir, fk := newUSDTBot(t)
	const total = 150000

	desA := deliverableOrder(t, b, st, dir, 500, total)
	seedProduct(t, dir, "prodDeliver2", total, 3)
	desB := store.NewDes()
	if r, err := b.stock.Reserve("prodDeliver2", 1, desB, b.reserveTTL); err != nil || len(r) != 1 {
		t.Fatalf("reserve B: %v", err)
	}
	if err := st.CreateOrder(desB, fmtInt(501), "en", "prodDeliver2", "KBH", 1, total); err != nil {
		t.Fatal(err)
	}

	b.Process(ctx, callbackUpdate(500, "paybnb:"+desA))
	b.Process(ctx, callbackUpdate(501, "paybnb:"+desB))
	resA, _ := b.bands.Get(bands.ProviderBinance, desA)
	resB, _ := b.bands.Get(bands.ProviderBinance, desB)
	if resA.Amount == resB.Amount {
		t.Fatal("two live Binance orders share an amount")
	}

	// ONE payload carrying BOTH credits.
	nowMS := time.Now().UnixMilli()
	fk.setBinanceTxns([]binance.Transaction{
		{TransactionID: "1001", TransactionTime: nowMS, Amount: resA.Amount, Currency: "USDT"},
		{TransactionID: "1002", TransactionTime: nowMS, Amount: resB.Amount, Currency: "USDT"},
	})

	if !waitFor(t, func() bool {
		oa, _ := st.GetOrder(desA)
		ob, _ := st.GetOrder(desB)
		return oa != nil && oa.Status == "delivered" && ob != nil && ob.Status == "delivered"
	}) {
		oa, _ := st.GetOrder(desA)
		ob, _ := st.GetOrder(desB)
		t.Fatalf("poller did not settle both: A=%s B=%s", oa.Status, ob.Status)
	}
	// Both credits consumed exactly once (processed PK).
	for _, id := range []string{"bn:1001", "bn:1002"} {
		if used, _ := st.TxnExists(id); !used {
			t.Errorf("%s was not recorded in the processed ledger", id)
		}
	}
	// Both rows dropped -> bands freed.
	if _, ok := b.bands.Get(bands.ProviderBinance, desA); ok {
		t.Error("band A still held after settlement")
	}
}

// TestBinancePollerRestartsAfterIdleExit: the poller stops when no orders remain
// and MUST be restartable — the latch it holds is cleared on the way out
// (telegram_bot.py:9221-9223). A regression here silently strands every Binance
// order placed after the first idle exit: the customer sees a unique amount that
// nothing is watching for.
func TestBinancePollerRestartsAfterIdleExit(t *testing.T) {
	ctx := context.Background()
	b, _, st, dir, fk := newUSDTBot(t)
	const total = 150000

	// Start the poller with NO pending orders; it must exit and release the latch.
	b.ensureBinancePoller()
	if !waitFor(t, func() bool {
		b.binancePollerMu.Lock()
		defer b.binancePollerMu.Unlock()
		return !b.binancePollerOn
	}) {
		t.Fatal("an idle poller never exited / never released its latch")
	}

	// A NEW order must get a working poller.
	des := deliverableOrder(t, b, st, dir, 640, total)
	b.Process(ctx, callbackUpdate(640, "paybnb:"+des))
	res, _ := b.bands.Get(bands.ProviderBinance, des)
	fk.setBinanceTxns([]binance.Transaction{
		{TransactionID: "3001", TransactionTime: time.Now().UnixMilli(), Amount: res.Amount, Currency: "USDT"},
	})
	if !waitFor(t, func() bool {
		o, _ := st.GetOrder(des)
		return o != nil && o.Status == "delivered"
	}) {
		t.Fatal("the restarted poller never settled the order — the singleton latch leaked")
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

// TestStartupRecoveryNoDoubleCredit: the boot Binance reconcile must NOT re-settle
// a credit already consumed. store.MarkTxn's PK is the guard (telegram_bot.py:
// 9254-9262 "idempotent and concurrency-safe").
func TestStartupRecoveryNoDoubleCredit(t *testing.T) {
	ctx := context.Background()
	b, _, st, dir, fk := newUSDTBot(t)
	const chat = 610
	const total = 150000

	des := deliverableOrder(t, b, st, dir, chat, total)
	b.Process(ctx, callbackUpdate(chat, "paybnb:"+des))
	res, _ := b.bands.Get(bands.ProviderBinance, des)
	fk.setBinanceTxns([]binance.Transaction{
		{TransactionID: "2001", TransactionTime: time.Now().UnixMilli(), Amount: res.Amount, Currency: "USDT"},
	})

	// First reconcile settles it.
	b.binanceReconcileOnce(ctx)
	o, _ := st.GetOrder(des)
	if o.Status != "delivered" {
		t.Fatalf("first reconcile left status %s", o.Status)
	}
	balAfter, _ := st.GetBalance(fmtInt(chat))

	// Re-persist the row (as a crashed run might) and reconcile again: the
	// processed PK must stop any second settlement or credit.
	if err := st.BinancePendingAdd(des, fmtInt(chat), res.Amount, total,
		time.Now().Add(-time.Minute).UnixMilli(), float64(time.Now().Add(time.Minute).Unix())); err != nil {
		t.Fatal(err)
	}
	b.binanceReconcileOnce(ctx)
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != balAfter {
		t.Errorf("a second reconcile credited again (%d -> %d)", balAfter, bal)
	}
}

// TestBinanceReconcileGrace: an EXPIRED order still gets ONE last match attempt
// while within BINANCE_RECONCILE_GRACE_S (a late credit then refunds to the
// wallet), and is dropped once past it (telegram_bot.py:9280-9300).
func TestBinanceReconcileGrace(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fk := newUSDTBot(t)
	const total = 150000
	fk.setBinanceTxns(nil) // no credit ever arrives

	// (a) expired but WITHIN the grace -> the row survives one more attempt.
	inGrace := usdtOrder(t, st, 620, total)
	st.ClaimOrder(inGrace, "reserved", "expired")
	if err := st.BinancePendingAdd(inGrace, "620", "6.00", total,
		time.Now().UnixMilli(), float64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	// (b) expired and PAST the grace -> pruned before any match attempt.
	stale := usdtOrder(t, st, 621, total)
	st.ClaimOrder(stale, "reserved", "expired")
	if err := st.BinancePendingAdd(stale, "621", "6.21", total,
		time.Now().UnixMilli(), float64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	// Shrink the grace so (b)'s just-written row is already past it, then widen it
	// again for (a) in a second pass.
	b.binanceReconcileGrace = 0
	b.binanceReconcileOnce(ctx)
	rows, _ := st.BinancePendingAll()
	if len(rows) != 0 {
		t.Errorf("rows past the grace were kept: %d", len(rows))
	}

	// Re-add (a) with a generous grace: no match -> dropIfNoMatch fires, but only
	// AFTER the reconcile actually queried Binance for it.
	if err := st.BinancePendingAdd(inGrace, "620", "6.00", total,
		time.Now().UnixMilli(), float64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	b.binanceReconcileGrace = time.Hour
	hits := fk.binanceHits.Load()
	b.binanceReconcileOnce(ctx)
	if fk.binanceHits.Load() != hits+1 {
		t.Error("an in-grace expired order was not given its last match attempt")
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
	// clients against the fakes.
	b, _, _, _, _ := newUSDTBot(t)
	// newUSDTBot replaces the chain seams with scripted fakes; restore the real
	// bindings so this test exercises the production path.
	b.bep20ByHash = bep20.GetTransferToSellerByHash
	b.tronByHash = func(txhash string, minTSMS int64) (*tron.Transfer, error) {
		return tron.FindConfirmedTransferByHash(tron.Address(), txhash, minTSMS)
	}

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
