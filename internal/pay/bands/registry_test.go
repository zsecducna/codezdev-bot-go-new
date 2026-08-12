package bands

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The Python acceptance tests (tests/test_usdt_redirect.py) freeze the clock at
// t=10000 with live deadlines at 10400 and dead ones at 9000. Same numbers here
// so the ported cases are line-comparable.
var (
	tNow   = time.Unix(10000, 0)
	tLive  = time.Unix(10400, 0) // deadline in the future -> session still payable
	tDead  = time.Unix(9000, 0)  // deadline passed 1000s ago (< EvictAfter)
	tStale = time.Unix(7000, 0)  // dead 3000s ago (> EvictAfter 1800s)
)

// slots600 is every amount the picker can hand out for the base "6.00" at the
// shipped geometry, in the order it tries them: the ten upward jitter slots
// (0.01000 + k*0.00101) then the single downward last resort (base - 0.00101).
//
// Written out rather than computed so the tests assert against literal money the
// way the Python suite did — if the geometry changes, these strings are supposed
// to break, not silently follow along.
var slots600 = []string{
	"6.01000", "6.01101", "6.01202", "6.01303", "6.01404",
	"6.01505", "6.01606", "6.01707", "6.01808", "6.01909",
	"5.99899",
}

// fakeClock is a settable clock; mutex-guarded so the concurrency test can read
// it from many goroutines under -race.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// newTestRegistry wires three geometries into ONE registry (the cross-provider
// collision set) with the clock pinned at t=10000 and the shipped 30-min
// USDT_BAND_EVICT_S window. The third slot carries wideParams — a valid but
// deliberately different geometry — so the cross-provider assertions are not
// comparing two identical parameter sets.
func newTestRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: tNow}
	r, err := New(Config{
		Providers: map[Provider]Params{
			ProviderBEP20:   bep20Params,
			ProviderTron:    tronParams,
			ProviderBinance: wideParams,
		},
		EvictAfter: DefaultEvictAfter,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, clk
}

// adopt is a test shortcut for planting a pre-existing pending row (what
// Python's tests do by writing straight into _bep20_pending).
//
// IsDeposit: true, because these rows stand in for rehydrated pending DEPOSIT
// sessions — the only kind MatchIncoming considers. Allocation tests are
// indifferent to the flag (the picker avoids every live row whatever it is
// for); adoptOrder below is the deliberate opposite for the tests that are not.
func adopt(t *testing.T, r *Registry, p Provider, des, chat, amount string, deadline time.Time) {
	t.Helper()
	if err := r.Adopt(Reservation{Provider: p, Des: des, ChatID: chat, Amount: amount, Deadline: deadline, IsDeposit: true}); err != nil {
		t.Fatalf("Adopt(%s): %v", des, err)
	}
}

// adoptOrder plants a live PRODUCT ORDER row: same band, same lock, same
// blocking power over the picker — but invisible to attribution by amount.
func adoptOrder(t *testing.T, r *Registry, p Provider, des, chat, amount string, deadline time.Time) {
	t.Helper()
	if err := r.Adopt(Reservation{Provider: p, Des: des, ChatID: chat, Amount: amount, Deadline: deadline}); err != nil {
		t.Fatalf("Adopt(%s): %v", des, err)
	}
}

// TestMatchIncomingIgnoresProductOrders is the money assertion behind
// Reservation.IsDeposit. The watcher's only action is a WALLET CREDIT, so a
// product order's band must never be attributable by amount: matching one would
// pay the customer's wallet for goods whose stock is still sitting reserved and
// undelivered, and the customer would then have both the balance and an order
// they can still pay for from it.
//
// The transfer here lands EXACTLY on the order's quoted amount — the strongest
// possible evidence, and still not enough, because the evidence was never the
// problem. Attribution by amount is only sound for a flow whose settlement is
// "credit what arrived", which is deposits and nothing else.
func TestMatchIncomingIgnoresProductOrders(t *testing.T) {
	r, _ := newTestRegistry(t)
	adoptOrder(t, r, ProviderTron, "order-1", "7", "120.01000", tLive)

	recv := mustUnitsAt(t, "120.01000")
	res, kind, err := r.MatchIncoming(ProviderTron, recv)
	if err != nil {
		t.Fatalf("MatchIncoming: %v", err)
	}
	if kind != MatchNone {
		t.Errorf("MatchIncoming on a live PRODUCT ORDER band = (%s, %s), want none — the watcher "+
			"would have wallet-credited an order whose stock was never delivered", res.Des, kind)
	}

	// And the flag must not merely DEMOTE the order row to a weaker verdict: a
	// deposit sharing the window has to still match cleanly, not come back
	// ambiguous because a product order was standing next to it.
	adopt(t, r, ProviderTron, "dep-1", "8", "120.01000", tLive)
	res, kind, err = r.MatchIncoming(ProviderTron, recv)
	if err != nil || kind != MatchExact || res.Des != "dep-1" {
		t.Errorf("MatchIncoming with a deposit beside the order = (%s, %s, %v), want (dep-1, exact, nil)",
			res.Des, kind, err)
	}
}

// TestDepositRowsMatchesMatchIncomingsCandidateSet pins the shared-rule
// invariant: the watcher decides whether to make an RPC call at all from
// DepositRows, then attributes through MatchIncoming. If those two disagreed
// about what a candidate is, the gate could skip a fetch for a band the matcher
// would have matched — a deposit silently never credited.
func TestDepositRowsMatchesMatchIncomingsCandidateSet(t *testing.T) {
	r, _ := newTestRegistry(t)
	adoptOrder(t, r, ProviderTron, "order-1", "7", "120.01000", tLive)
	if rows := r.DepositRows(ProviderTron); len(rows) != 0 {
		t.Fatalf("DepositRows with only a product order = %d rows, want 0 — the watcher would "+
			"poll a public RPC on a timer with nobody depositing", len(rows))
	}
	// A row PAST its deadline is still a candidate, exactly as it is for
	// MatchIncoming: recovering a late payment inside the pending grace is the
	// whole reason the row is kept.
	adopt(t, r, ProviderTron, "dep-dead", "8", "120.01000", tDead)
	rows := r.DepositRows(ProviderTron)
	if len(rows) != 1 || rows[0].Des != "dep-dead" {
		t.Fatalf("DepositRows = %v, want the one dead-but-in-grace deposit row", rows)
	}
	if _, kind, _ := r.MatchIncoming(ProviderTron, mustUnitsAt(t, "120.01000")); kind != MatchExact {
		t.Errorf("MatchIncoming on a dead-but-listed row = %s, want exact — the gate and the "+
			"matcher must agree on the candidate set", kind)
	}
}

func TestNewRejectsEmptyAndBadParams(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no providers = nil error, want error")
	}
	_, err := New(Config{Providers: map[Provider]Params{ProviderTron: {Step: 0, Tol: 50, Decimals: 5}}})
	if err == nil {
		t.Error("New with invalid params = nil error, want error")
	}
}

// TestReserveFillsSlotsThenExhausts is the capacity contract: the ten upward
// jitter slots per price, then the single last-resort slot below base
// (telegram_bot.py:9089), then a clear ErrBandExhausted instead of a silent
// collision (telegram_bot.py:9092). Every reserver is a DIFFERENT chat and every
// session is live, so no exclusion rule applies. This is also the test that pins
// the hands-free-deposits geometry directly: eleven concurrent same-price orders
// fit, and none of them is quoted the round base.
func TestReserveFillsSlotsThenExhausts(t *testing.T) {
	r, _ := newTestRegistry(t)
	want := slots600
	for i, w := range want {
		res, err := r.Reserve(ProviderBEP20, "6.00", chatN(i), desN(i), tLive)
		if err != nil {
			t.Fatalf("Reserve #%d: %v", i, err)
		}
		if res.Amount != w {
			t.Errorf("Reserve #%d amount = %q, want %q", i, res.Amount, w)
		}
		if res.Units != mustUnits(t, w) || res.Decimals != 5 {
			t.Errorf("Reserve #%d units = %d/%d dec, want %d/5", i, res.Units, res.Decimals, mustUnits(t, w))
		}
		if res.Amount == "6.00" {
			t.Errorf("Reserve #%d quoted the round base — the jitter is what identifies an order", i)
		}
	}
	if _, err := r.Reserve(ProviderBEP20, "6.00", chatN(len(want)), desN(len(want)), tLive); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("Reserve past capacity = %v, want ErrBandExhausted", err)
	}
	if got := r.Len(ProviderBEP20); got != len(want) {
		t.Errorf("Len after exhaustion = %d, want %d (the failed Reserve must not store a row)", got, len(want))
	}
}

// TestReserveIsCrossProvider: a BEP20 amount may not collide with a live TRON
// amount, and a Binance amount may not collide with either. This is the port's
// cross-provider collision set (see the package doc's DELIBERATE DEVIATION
// note); Python's per-registry sets are a subset of it.
func TestReserveIsCrossProvider(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.Reserve(ProviderTron, "6.00", "7", "trx-1", tLive); err != nil {
		t.Fatalf("tron Reserve: %v", err)
	}
	got, err := r.Reserve(ProviderBEP20, "6.00", "8", "bsc-1", tLive)
	if err != nil {
		t.Fatalf("bep20 Reserve: %v", err)
	}
	if got.Amount == slots600[0] {
		t.Fatalf("bep20 reused the live TRON amount %s — collision set is not cross-provider", slots600[0])
	}
	if got.Amount != slots600[1] {
		t.Errorf("bep20 amount = %q, want %q", got.Amount, slots600[1])
	}
	// The third rail runs wideParams (step 250, tol 100, MinGap 201), so ITS
	// tolerance is the wider one and dictates the gap in both directions: against
	// the on-chain rails' tol-50 bands the required gap is 2*100+1 = 201. Its own
	// first two slots (offsets 0.01000 and 0.01250) sit 0 and 149 units from the
	// live 6.01000/6.01101 rows, so the offset-0.01500 slot is the first clear of
	// both.
	bn, err := r.Reserve(ProviderBinance, "6.00", "9", "bn-1", tLive)
	if err != nil {
		t.Fatalf("wide-rail Reserve: %v", err)
	}
	if bn.Amount != "6.01500" {
		t.Errorf("wide-rail amount = %q, want %q (must clear the wider foreign bands)", bn.Amount, "6.01500")
	}
	// The disjointness this buys, stated directly: no live amount on any rail is
	// close enough to the wide rail's amount for one receipt to match both.
	for _, other := range []string{slots600[0], slots600[1]} {
		u := mustUnits(t, other)
		if abs64(bn.Units-u) < int64(2*tronParams.Tol+1) {
			t.Errorf("wide-rail %s and live %s are %d units apart, want >= %d",
				bn.Amount, other, abs64(bn.Units-u), 2*tronParams.Tol+1)
		}
	}
}

// TestReserveAcrossDifferentDecimals exercises the rescale path end-to-end: a
// rail whose AMOUNT_DECIMALS differs (env-tunable per bep20_pay.py:40 /
// tron_pay.py:41) must still share one comparable collision set.
func TestReserveAcrossDifferentDecimals(t *testing.T) {
	clk := &fakeClock{t: tNow}
	r, err := New(Config{
		Providers: map[Provider]Params{
			ProviderBEP20: {Step: 101, Tol: 50, Decimals: 5},   // the shipped precision
			ProviderTron:  {Step: 1010, Tol: 500, Decimals: 6}, // same money, 6 dp
		},
		EvictAfter: DefaultEvictAfter,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	trx, err := r.Reserve(ProviderTron, "6.000000", "7", "trx-1", tLive)
	if err != nil {
		t.Fatalf("tron Reserve: %v", err)
	}
	// The 6-dp rail's jitter floor is one hundredth of a whole unit there too:
	// 10_000 units = 0.010000 USDT.
	if trx.Amount != "6.010000" || trx.Units != 6_010_000 || trx.Decimals != 6 {
		t.Fatalf("tron reservation = %q/%d/%ddp, want 6.010000/6010000/6dp", trx.Amount, trx.Units, trx.Decimals)
	}
	// The 6-dp 6.010000 row rescales to 601000 and must block the 5-dp rail's
	// first slot.
	bsc, err := r.Reserve(ProviderBEP20, "6.00", "8", "bsc-1", tLive)
	if err != nil {
		t.Fatalf("bep20 Reserve: %v", err)
	}
	if bsc.Amount != slots600[1] {
		t.Errorf("bep20 amount = %q, want %q (the 6-dp TRON row must rescale into the set)", bsc.Amount, slots600[1])
	}
	// ...and the reverse direction: the 5-dp row blocks the 6-dp rail's slots.
	// Base 6.001010 puts the fine rail's first slot exactly on the coarse row.
	trx2, err := r.Reserve(ProviderTron, "6.001010", "9", "trx-2", tLive)
	if err != nil {
		t.Fatalf("tron Reserve #2: %v", err)
	}
	if trx2.Amount == "6.011010" {
		t.Error("tron reused the live BEP20 amount 6.01101 across precisions")
	}
	// Pin the exact slot too: 6.011010 is blocked (distance 0 < the
	// coarse-widened gap 1010), so the next step lands on 6.012020 — 1010 from
	// the coarse row (>= the widened gap, boundary-exact) and 2020 from the
	// same-rail 6.010000 row.
	if trx2.Amount != "6.012020" {
		t.Errorf("tron amount = %q, want %q (next slot past the coarse-widened gap)", trx2.Amount, "6.012020")
	}
	// Redirect and the claim-jack gate both compare in the rail's own units.
	adopt(t, r, ProviderTron, "trx-sib", "7", "9.000000", tDead)
	if sib, ok := r.ResolveRedirect(ProviderTron, "7", "trx-1", 9_000_000, false); !ok || sib != "trx-sib" {
		t.Errorf("6-dp redirect = (%q,%v), want (\"trx-sib\",true)", sib, ok)
	}
	hit, err := r.RecvMatchesAnyOtherBand(ProviderTron, "trx-1", 9_000_500) // 9.000000 + tol
	if err != nil {
		t.Fatalf("RecvMatchesAnyOtherBand: %v", err)
	}
	if !hit {
		t.Error("6-dp band edge must trip the claim-jack gate")
	}
}

// TestReserveCoarseNeighbourSlack pins the review fix in collisionUnits: a
// foreign row on a COARSER rail effectively reaches one full coarse unit past
// its band's top edge, because each rail floors the received amount to its OWN
// precision before matching. Without the extra 10^(a-b)-1 slack the picker
// hands out an ambiguous amount:
//
//	requester TRON 6dp tol 500, foreign BEP20 5dp row 5.80000 tol 50;
//	candidate 5.801005 sits 1005 rescaled units away (>= the naive 1001 gap), yet
//	a single 5.800505 USDT receipt is 500 from it on TRON (<= 500, matches
//	5.801005) AND floors to 580050 on BEP20 (|580050-580000| = 50 <= 50, matches
//	5.80000) — one payment inside two live bands, i.e. it could settle either order.
//
// With the widened 1010 gap the 5.801005 slot is refused and the pick moves one
// step up, where no such double-match exists.
func TestReserveCoarseNeighbourSlack(t *testing.T) {
	clk := &fakeClock{t: tNow}
	coarse := Params{Step: 101, Tol: 50, Decimals: 5}
	fine := Params{Step: 1010, Tol: 500, Decimals: 6}
	r, err := New(Config{
		Providers: map[Provider]Params{
			ProviderBEP20: coarse,
			ProviderTron:  fine,
		},
		EvictAfter: DefaultEvictAfter,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adopt(t, r, ProviderBEP20, "coarse", "9", "5.80000", tLive)

	// First: demonstrate the ambiguity is REAL for the naive pick, using the
	// package's own matchers — 5800505 (a 5.800505 receipt on the fine rail) is
	// inside 5.801005's band, and its 5-dp floor 580050 is inside 5.80000's band.
	if ok, _ := RecvMatchesBand(fine, "5.801005", 5_800_505); !ok {
		t.Fatal("premise broken: 5800505 no longer matches the 5.801005 band")
	}
	if ok, _ := RecvMatchesBand(coarse, "5.80000", rescaleUnits(5_800_505, 6, 5)); !ok {
		t.Fatal("premise broken: the floored receipt no longer matches the 5.80000 band")
	}

	// Then: the picker must therefore refuse 5.801005 and step past it. Base
	// 5.791005 puts the fine rail's first jitter slot exactly on that candidate.
	res, err := r.Reserve(ProviderTron, "5.791005", "7", "fine-1", tLive)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Amount == "5.801005" {
		t.Fatal("picker handed out 5.801005 next to the coarse 5.80000 band — a 5.800505 receipt would match both orders")
	}
	if res.Amount != "5.802015" {
		t.Errorf("amount = %q, want %q (next slot, clear of the widened coarse band)", res.Amount, "5.802015")
	}
}

// TestReserveOwnDeadSessionsDoNotBlock is the direct port of Python's
// test_picker_excludes_own_dead_sessions (tests/test_usdt_redirect.py:110-134) —
// the 2026-07-02 incident fix (telegram_bot.py:9400-9409, 9440-9442).
func TestReserveOwnDeadSessionsDoNotBlock(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "own-a", "7", slots600[1], tDead) // own, dead
	adopt(t, r, ProviderBEP20, "own-b", "7", slots600[2], tDead) // own, dead
	adopt(t, r, ProviderBEP20, "own-c", "7", slots600[3], tDead) // own, dead
	adopt(t, r, ProviderBEP20, "far-d", "9", slots600[0], tLive) // foreign, live
	// Foreign, live: fills the rest of the upward jitter window so the chat-less
	// call below still has to fall through to the downward last-resort slot
	// rather than landing on a free upper slot.
	for i, a := range slots600[4:10] {
		adopt(t, r, ProviderBEP20, "far-"+strconv.Itoa(i), "9", a, tLive)
	}

	// Own dead rows freed -> the first slot clear of the foreign slot-0 band is
	// slot 1.
	res, err := r.Reserve(ProviderBEP20, "6.00", "7", "new-1", tLive)
	if err != nil {
		t.Fatalf("Reserve for the same chat: %v", err)
	}
	if res.Amount != slots600[1] {
		t.Errorf("own-dead-excluded amount = %q, want %q", res.Amount, slots600[1])
	}
	r.Release(ProviderBEP20, "new-1") // restore state for the next assertion

	// No chat id (Python's legacy chat_id=None call): nothing is excluded, all
	// upward slots are blocked -> the single downward slot is the last resort.
	res, err = r.Reserve(ProviderBEP20, "6.00", "", "new-2", tLive)
	if err != nil {
		t.Fatalf("Reserve without a chat: %v", err)
	}
	if res.Amount != slots600[10] {
		t.Errorf("chat-less amount = %q, want %q (last-resort slot)", res.Amount, slots600[10])
	}
	r.Release(ProviderBEP20, "new-2")

	// Downward slot also held (foreign, LIVE) and the blockers are only
	// RECENTLY dead (1000s < EvictAfter 1800s) -> genuinely exhausted.
	adopt(t, r, ProviderBEP20, "far-e", "9", slots600[10], tLive)
	if _, err := r.Reserve(ProviderBEP20, "6.00", "", "new-3", tLive); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("Reserve with every slot held = %v, want ErrBandExhausted", err)
	}
}

// TestReserveEvictsStaleRowsUnderPressure: the SECOND pass
// (telegram_bot.py:9099-9103 / 9414-9420 / 9899-9905). Rows dead longer than
// USDT_BAND_EVICT_S stop blocking — but ONLY when the first pass found no slot,
// and the rows are KEPT so their late payment is still submittable.
func TestReserveEvictsStaleRowsUnderPressure(t *testing.T) {
	r, _ := newTestRegistry(t)
	// All eleven slots (10 upward jitter slots + 1 downward last-resort) held by
	// a FOREIGN chat, every row dead for 3000s > 1800s.
	all := slots600
	for i, a := range all {
		adopt(t, r, ProviderBEP20, desN(i), "9", a, tStale)
	}
	res, err := r.Reserve(ProviderBEP20, "6.00", "7", "fresh", tLive)
	if err != nil {
		t.Fatalf("Reserve under pressure with stale blockers: %v", err)
	}
	if res.Amount != slots600[0] {
		t.Errorf("evicted-pass amount = %q, want %q", res.Amount, slots600[0])
	}
	// Eviction is soft: the stale rows are still present (their late payment can
	// still be submitted against their own order).
	if want := len(all) + 1; r.Len(ProviderBEP20) != want {
		t.Errorf("Len = %d, want %d — eviction must NOT delete rows", r.Len(ProviderBEP20), want)
	}

	// A stale row does NOT get evicted when the first pass already has a slot:
	// pass 1 avoids it, so the fresh amount must dodge it.
	r2, _ := newTestRegistry(t)
	adopt(t, r2, ProviderBEP20, "stale-1", "9", slots600[0], tStale)
	res, err = r2.Reserve(ProviderBEP20, "6.00", "7", "fresh", tLive)
	if err != nil {
		t.Fatalf("Reserve with one stale blocker: %v", err)
	}
	if res.Amount != slots600[1] {
		t.Errorf("no-pressure amount = %q, want %q (pass 1 must still avoid the stale row)", res.Amount, slots600[1])
	}
}

// TestReleaseFreesSlot: an explicit release (settle / refund / abandoned flow)
// returns the amount to the pool immediately — _drop_usdt_pending
// (telegram_bot.py:9808).
func TestReleaseFreesSlot(t *testing.T) {
	r, _ := newTestRegistry(t)
	first, err := r.Reserve(ProviderBEP20, "6.00", "7", "d1", tLive)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !r.Release(ProviderBEP20, "d1") {
		t.Fatal("Release of a live row = false, want true")
	}
	if r.Release(ProviderBEP20, "d1") {
		t.Error("second Release = true, want false (idempotent)")
	}
	again, err := r.Reserve(ProviderBEP20, "6.00", "8", "d2", tLive)
	if err != nil {
		t.Fatalf("Reserve after Release: %v", err)
	}
	if again.Amount != first.Amount {
		t.Errorf("amount after Release = %q, want the freed %q", again.Amount, first.Amount)
	}
}

// TestSweepMirrorsPrunePending ports _prune_usdt_pending's three-way decision
// (telegram_bot.py:9615-9623) through the `keep` callback: a still-"reserved"
// order keeps its band, a finished order keeps it until USDT_PENDING_GRACE_S
// past the row's CREATION time, and a VANISHED order's band is freed at once.
func TestSweepMirrorsPrunePending(t *testing.T) {
	r, clk := newTestRegistry(t)
	// CreatedAt = tNow (10000) for all three, via Adopt's zero-value default.
	adopt(t, r, ProviderBEP20, "reserved", "7", slots600[0], tLive)
	adopt(t, r, ProviderBEP20, "finished", "7", slots600[1], tDead)
	adopt(t, r, ProviderTron, "vanished", "7", "9.01000", tLive)

	// The exact Python rule, expressed once and reused as the clock moves.
	status := map[string]string{"reserved": "reserved", "finished": "delivered"}
	keep := func(res Reservation) bool {
		st, exists := status[res.Des]
		if !exists {
			return false // order VANISHED -> free the band immediately
		}
		if st == "reserved" {
			return true // still live -> keep
		}
		return clk.Now().Sub(res.CreatedAt) < DefaultPendingGrace // within grace -> keep
	}

	// t=10000: only the vanished order's band is freed — note it is freed even
	// though its deadline is still in the FUTURE and no grace has elapsed.
	dropped, err := r.Sweep(keep)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Des != "vanished" {
		t.Fatalf("Sweep dropped %+v, want exactly the vanished row", dropped)
	}
	if _, ok := r.Get(ProviderTron, "vanished"); ok {
		t.Error("swept row is still in the registry")
	}

	// Still inside the grace for the finished order -> nothing new is dropped.
	clk.Set(tNow.Add(DefaultPendingGrace - time.Second))
	if dropped, err = r.Sweep(keep); err != nil || len(dropped) != 0 {
		t.Fatalf("Sweep within grace = (%+v,%v), want no drops", dropped, err)
	}

	// One second past the grace (measured from CreatedAt) -> the finished order's
	// band is freed; the still-reserved one is untouched no matter how long it lives.
	clk.Set(tNow.Add(DefaultPendingGrace).Add(time.Second))
	dropped, err = r.Sweep(keep)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Des != "finished" {
		t.Fatalf("Sweep past grace dropped %+v, want exactly the finished row", dropped)
	}
	if _, ok := r.Get(ProviderBEP20, "reserved"); !ok {
		t.Error("a still-reserved order lost its band")
	}
	// Idempotent.
	if dropped, err = r.Sweep(keep); err != nil || len(dropped) != 0 {
		t.Fatalf("second Sweep = (%+v,%v), want no drops", dropped, err)
	}
	if _, err := r.Sweep(nil); err == nil {
		t.Error("Sweep(nil) = nil error, want error (a nil keep would free every band)")
	}
}

// TestSweepKeepMayCallBackIntoRegistry: `keep` runs with the mutex RELEASED, so
// an order-status check that touches the Registry (or does slow DB I/O) can
// neither deadlock nor stall allocation on the other rails. A reentrant keep
// hangs this test rather than failing it, which is the point.
func TestSweepKeepMayCallBackIntoRegistry(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "a", "7", slots600[0], tDead)
	adopt(t, r, ProviderBEP20, "b", "7", slots600[1], tDead)
	done := make(chan struct{})
	go func() {
		defer close(done)
		dropped, err := r.Sweep(func(res Reservation) bool {
			// Every read/write method, called from inside the callback.
			r.Len(res.Provider)
			r.Get(res.Provider, res.Des)
			r.OtherAmounts(res.Provider, res.Des)
			r.ResolveRedirect(res.Provider, res.ChatID, res.Des, res.Units, false)
			if _, err := r.Reserve(ProviderTron, "6.00", "9", "probe-"+res.Des, tLive); err != nil {
				t.Errorf("Reserve from inside keep: %v", err)
			}
			return false
		})
		if err != nil {
			t.Errorf("Sweep: %v", err)
		}
		if len(dropped) != 2 {
			t.Errorf("Sweep dropped %d rows, want 2", len(dropped))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Sweep deadlocked: keep must not run while r.mu is held")
	}
}

// TestSweepSkipsRowsReReservedWhileUnlocked: a des released and re-reserved
// during the unlocked keep phase must keep its NEW band — the seq identity
// re-check at the end of Sweep. The clock is deliberately NOT advanced, so the
// replacement shares its predecessor's CreatedAt: only a monotonic seq can tell
// the two rows apart.
func TestSweepSkipsRowsReReservedWhileUnlocked(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "churn", "7", slots600[0], tDead)
	dropped, err := r.Sweep(func(res Reservation) bool {
		// Simulate the race: the row is replaced while keep is deciding.
		r.Release(ProviderBEP20, res.Des)
		if _, err := r.Reserve(ProviderBEP20, "6.00", "7", res.Des, tLive); err != nil {
			t.Fatalf("re-Reserve: %v", err)
		}
		return false // condemn the row we were shown
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("Sweep dropped %+v, want nothing (the row it judged no longer exists)", dropped)
	}
	if _, ok := r.Get(ProviderBEP20, "churn"); !ok {
		t.Error("the freshly re-reserved row was swept by a stale decision")
	}
}

// TestGetAdoptAndOtherAmounts covers restart rehydration (telegram_bot.py:9503 /
// 9959) and the `other_amounts` snapshot the verify path takes
// (telegram_bot.py:9689).
func TestGetAdoptAndOtherAmounts(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "a", "7", slots600[1], tLive)
	adopt(t, r, ProviderBEP20, "b", "8", slots600[2], tLive)
	adopt(t, r, ProviderTron, "c", "7", slots600[0], tLive)

	res, ok := r.Get(ProviderBEP20, "a")
	if !ok {
		t.Fatal("Get after Adopt = not found")
	}
	if res.Units != 601_101 || res.Decimals != 5 {
		t.Errorf("Adopt units = %d/%d dec, want 601101/5 (recomputed from Amount)", res.Units, res.Decimals)
	}
	if _, ok := r.Get(ProviderBEP20, "nope"); ok {
		t.Error("Get of an unknown des = found, want not found")
	}
	// Same provider only, requesting des excluded.
	if got := r.OtherAmounts(ProviderBEP20, "a"); len(got) != 1 || got[0] != slots600[2] {
		t.Errorf("OtherAmounts = %v, want [%s]", got, slots600[2])
	}
	if err := r.Adopt(Reservation{Provider: "nosuch", Des: "x", Amount: "1.00"}); err == nil {
		t.Error("Adopt with an unknown provider = nil error, want error")
	}
	if err := r.Adopt(Reservation{Provider: ProviderBEP20, Des: "x", Amount: "junk"}); err == nil {
		t.Error("Adopt with a bad amount = nil error, want error")
	}
}

// TestRecvMatchesAnyOtherBand: the locked claim-jack gate — a receipt landing in
// another SAME-provider session's band is a hit (telegram_bot.py:9635-9642,
// 9835-9840), the order's own band is excluded (that path is the `matched`
// branch), and a cross-provider band is not consulted (Python compares only
// within the rail's own registry).
func TestRecvMatchesAnyOtherBand(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "mine", "7", slots600[1], tLive)
	adopt(t, r, ProviderBEP20, "theirs", "9", slots600[0], tLive)
	adopt(t, r, ProviderTron, "other-rail", "9", slots600[3], tLive)

	mustGate := func(p Provider, des string, recv int64) bool {
		t.Helper()
		hit, err := r.RecvMatchesAnyOtherBand(p, des, recv)
		if err != nil {
			t.Fatalf("RecvMatchesAnyOtherBand: %v", err)
		}
		return hit
	}
	if !mustGate(ProviderBEP20, "mine", 601_000) {
		t.Error("a receipt inside a stranger's band must be a hit (claim-jack refusal)")
	}
	if mustGate(ProviderBEP20, "mine", 601_101) {
		t.Error("the order's OWN band must be excluded from the gate")
	}
	if mustGate(ProviderBEP20, "mine", 601_303) {
		t.Error("a band on a different rail must not trip this rail's gate")
	}
	// An unknown provider must FAIL LOUDLY: `false` here is the accept side of a
	// money refusal, so it may never be returned by accident.
	if _, err := r.RecvMatchesAnyOtherBand("nosuch", "mine", 601_000); err == nil {
		t.Error("unknown provider = nil error, want error")
	}
}

// TestSnapshot is the atomic (expected, other_amounts) read Python takes in one
// lock section before the chain lookup (telegram_bot.py:9685-9693).
func TestSnapshot(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "mine", "7", slots600[1], tLive)
	adopt(t, r, ProviderBEP20, "theirs", "9", slots600[0], tLive)
	adopt(t, r, ProviderTron, "other-rail", "9", slots600[2], tLive)

	res, others, ok := r.Snapshot(ProviderBEP20, "mine")
	if !ok {
		t.Fatal("Snapshot of a live row = not found")
	}
	if res.Amount != slots600[1] || res.Units != 601_101 {
		t.Errorf("Snapshot reservation = %q/%d, want %s/601101", res.Amount, res.Units, slots600[1])
	}
	if len(others) != 1 || others[0] != slots600[0] {
		t.Errorf("Snapshot others = %v, want [%s] (same rail, self excluded)", others, slots600[0])
	}
	if _, _, ok := r.Snapshot(ProviderBEP20, "gone"); ok {
		t.Error("Snapshot of a missing row = found, want the ORDER_GONE branch")
	}
}

// TestResolveRedirect is the same-chat sibling redirect with its depth-1 guard
// (_usdt_own_sibling @9620; call sites @9648 and @9824-9832), i.e. the ported
// cases from tests/test_usdt_redirect.py:74-107.
func TestResolveRedirect(t *testing.T) {
	// Current session 6.01101 (des "new"), the customer's earlier EXPIRED session
	// 6.01000 (des "old") — they had already sent 6.01000.
	setup := func(siblingChat string) *Registry {
		r, _ := newTestRegistry(t)
		adopt(t, r, ProviderBEP20, "new", "7", slots600[1], tLive)
		adopt(t, r, ProviderBEP20, "old", siblingChat, slots600[0], tDead)
		return r
	}

	// Same chat: a 6.01000 receipt reroutes to their OWN 6.01000 session.
	sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 601_000, false)
	if !ok || sib != "old" {
		t.Errorf("same-chat redirect = (%q,%v), want (\"old\",true)", sib, ok)
	}
	// The sibling's band is fuzzy too: 6.01050 is 6.01000 + exactly tol.
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 601_050, false); !ok || sib != "old" {
		t.Errorf("sibling band edge = (%q,%v), want (\"old\",true)", sib, ok)
	}
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 601_051, false); ok {
		t.Errorf("one unit past the sibling band redirected to %q, want no redirect", sib)
	}
	// Cross-chat: the 6.01000 band belongs to a STRANGER -> claim-jack reject, no
	// redirect (tests/test_usdt_redirect.py:82-88).
	if sib, ok := setup("9").ResolveRedirect(ProviderBEP20, "7", "new", 601_000, false); ok {
		t.Errorf("cross-chat band matched redirected to %q, want no redirect", sib)
	}
	// DEPTH-1 GUARD: a verify that was ALREADY redirected never redirects again.
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 601_000, true); ok {
		t.Errorf("depth-2 redirect returned %q, want refusal", sib)
	}
	// Paying the CURRENT session's own amount finds no sibling (the caller's
	// `matched` branch settles it directly).
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 601_101, false); ok {
		t.Errorf("exact-amount receipt redirected to %q, want no redirect", sib)
	}
	// An unknown owner is never a sibling, and neither is an unknown provider.
	if _, ok := setup("7").ResolveRedirect(ProviderBEP20, "", "new", 601_000, false); ok {
		t.Error("chat-less redirect lookup = true, want false")
	}
	if _, ok := setup("7").ResolveRedirect("nosuch", "7", "new", 601_000, false); ok {
		t.Error("unknown-provider redirect lookup = true, want false")
	}
}

// TestResolveRedirectPrefersFreshestSibling pins determinism where Python leans
// on dict insertion order: with two same-chat rows in the SAME band (possible
// only after an eviction reused an amount), the freshest deadline wins.
func TestResolveRedirectPrefersFreshestSibling(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "cur", "7", slots600[1], tLive)
	adopt(t, r, ProviderBEP20, "z-older", "7", slots600[0], tStale)
	adopt(t, r, ProviderBEP20, "a-newer", "7", slots600[0], tDead)
	for range 20 { // map iteration order is randomised; the answer must not be
		sib, ok := r.ResolveRedirect(ProviderBEP20, "7", "cur", 601_000, false)
		if !ok || sib != "a-newer" {
			t.Fatalf("redirect = (%q,%v), want (\"a-newer\",true)", sib, ok)
		}
	}
}

// TestReserveRejectsBadInput covers the guard rails around Reserve.
func TestReserveRejectsBadInput(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.Reserve(ProviderBEP20, "6.00", "7", "", tLive); err == nil {
		t.Error("Reserve with an empty des = nil error, want error")
	}
	if _, err := r.Reserve("nosuch", "6.00", "7", "d", tLive); err == nil {
		t.Error("Reserve on an unknown provider = nil error, want error")
	}
	if _, err := r.Reserve(ProviderBEP20, "junk", "7", "d", tLive); err == nil {
		t.Error("Reserve with an unparseable base = nil error, want error")
	}
	if prm, ok := r.Params(ProviderBEP20); !ok || prm != bep20Params {
		t.Errorf("Params(bep20) = (%+v,%v), want (%+v,true)", prm, ok, bep20Params)
	}
}

// TestReserveSameDesDoesNotBlockItself: re-opening the pay screen for the SAME
// order must not let its own previous row consume the slot it is about to reuse.
func TestReserveSameDesDoesNotBlockItself(t *testing.T) {
	r, _ := newTestRegistry(t)
	first, err := r.Reserve(ProviderBEP20, "6.00", "7", "same", tLive)
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	second, err := r.Reserve(ProviderBEP20, "6.00", "7", "same", tLive)
	if err != nil {
		t.Fatalf("re-Reserve: %v", err)
	}
	if second.Amount != first.Amount {
		t.Errorf("re-Reserve amount = %q, want the same %q", second.Amount, first.Amount)
	}
	if got := r.Len(ProviderBEP20); got != 1 {
		t.Errorf("Len = %d, want 1 (re-Reserve overwrites, never duplicates)", got)
	}
}

// TestConcurrentReserveNeverDuplicates: many chats race for the same price on
// the same rail. Exactly the 11 slots (10 upward jitter slots + the downward
// last resort) must be handed out, each exactly once, and everyone else must get
// ErrBandExhausted — never a duplicate amount. Run with -race.
func TestConcurrentReserveNeverDuplicates(t *testing.T) {
	r, _ := newTestRegistry(t)
	const n = 64
	var (
		mu       sync.Mutex
		granted  = map[string]string{} // amount -> des (duplicate detector)
		refused  int
		otherErr []error
		wg       sync.WaitGroup
	)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			res, err := r.Reserve(ProviderBEP20, "6.00", chatN(i), desN(i), tLive)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				if prev, dup := granted[res.Amount]; dup {
					t.Errorf("amount %q handed to both %s and %s", res.Amount, prev, res.Des)
				}
				granted[res.Amount] = res.Des
			case errors.Is(err, ErrBandExhausted):
				refused++
			default:
				otherErr = append(otherErr, err)
			}
		}(i)
	}
	wg.Wait()
	if len(otherErr) != 0 {
		t.Fatalf("unexpected errors: %v", otherErr)
	}
	wantSlots := len(slots600)
	if len(granted) != wantSlots {
		t.Errorf("granted %d amounts (%v), want exactly the %d slots", len(granted), granted, wantSlots)
	}
	if refused != n-wantSlots {
		t.Errorf("refused %d, want %d", refused, n-wantSlots)
	}
	for _, want := range slots600 {
		if _, ok := granted[want]; !ok {
			t.Errorf("slot %q was never handed out", want)
		}
	}
	if got := r.Len(ProviderBEP20); got != wantSlots {
		t.Errorf("Len = %d, want %d", got, wantSlots)
	}
}

// TestConcurrentMixedOpsAreRaceFree exercises every mutating and reading method
// at once so -race can prove the mutex covers them all.
func TestConcurrentMixedOpsAreRaceFree(t *testing.T) {
	r, _ := newTestRegistry(t)
	providers := []Provider{ProviderBEP20, ProviderTron, ProviderBinance}
	var wg sync.WaitGroup
	for i := range 48 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := providers[i%len(providers)]
			des := desN(i)
			if _, err := r.Reserve(p, "6.00", chatN(i%4), des, tLive); err != nil && !errors.Is(err, ErrBandExhausted) {
				t.Errorf("Reserve: %v", err)
			}
			r.Get(p, des)
			r.Snapshot(p, des)
			r.OtherAmounts(p, des)
			if _, err := r.RecvMatchesAnyOtherBand(p, des, 601_000); err != nil {
				t.Errorf("RecvMatchesAnyOtherBand: %v", err)
			}
			r.ResolveRedirect(p, chatN(i%4), des, 601_000, false)
			if _, err := r.Sweep(func(Reservation) bool { return true }); err != nil {
				t.Errorf("Sweep: %v", err)
			}
			r.Release(p, des)
			r.Len(p)
		}(i)
	}
	wg.Wait()
}

func chatN(i int) string { return "chat-" + strconv.Itoa(i) }
func desN(i int) string  { return "des-" + strconv.Itoa(i) }

func mustUnits(t *testing.T, amount string) int64 {
	t.Helper()
	u, err := AmountUnits(amount, bep20Params.Decimals)
	if err != nil {
		t.Fatalf("AmountUnits(%q): %v", amount, err)
	}
	return u
}
