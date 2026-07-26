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

// newTestRegistry wires all three rails into ONE registry (the cross-provider
// collision set) with the clock pinned at t=10000 and the shipped 30-min
// USDT_BAND_EVICT_S window.
func newTestRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: tNow}
	r, err := New(Config{
		Providers: map[Provider]Params{
			ProviderBEP20:   bep20Params,
			ProviderTron:    tronParams,
			ProviderBinance: binanceParams,
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
func adopt(t *testing.T, r *Registry, p Provider, des, chat, amount string, deadline time.Time) {
	t.Helper()
	if err := r.Adopt(Reservation{Provider: p, Des: des, ChatID: chat, Amount: amount, Deadline: deadline}); err != nil {
		t.Fatalf("Adopt(%s): %v", des, err)
	}
}

func TestNewRejectsEmptyAndBadParams(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no providers = nil error, want error")
	}
	_, err := New(Config{Providers: map[Provider]Params{ProviderTron: {Step: 0, Tol: 10, Decimals: 2}}})
	if err == nil {
		t.Error("New with invalid params = nil error, want error")
	}
}

// TestReserveFillsSlotsThenExhausts is the capacity contract: 4 upward slots per
// price (telegram_bot.py:9085, 100//21), then the single last-resort slot below
// base (telegram_bot.py:9089), then a clear ErrBandExhausted instead of a silent
// collision (telegram_bot.py:9092). Every reserver is a DIFFERENT chat and every
// session is live, so no exclusion rule applies.
func TestReserveFillsSlotsThenExhausts(t *testing.T) {
	r, _ := newTestRegistry(t)
	want := []string{"6.00", "6.21", "6.42", "6.63", "5.79"}
	for i, w := range want {
		res, err := r.Reserve(ProviderBEP20, "6.00", chatN(i), desN(i), tLive)
		if err != nil {
			t.Fatalf("Reserve #%d: %v", i, err)
		}
		if res.Amount != w {
			t.Errorf("Reserve #%d amount = %q, want %q", i, res.Amount, w)
		}
		if res.Units != mustUnits(t, w) || res.Decimals != 2 {
			t.Errorf("Reserve #%d units = %d/%d dec, want %d/2", i, res.Units, res.Decimals, mustUnits(t, w))
		}
	}
	if _, err := r.Reserve(ProviderBEP20, "6.00", chatN(9), desN(9), tLive); !errors.Is(err, ErrBandExhausted) {
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
	if got.Amount == "6.00" {
		t.Fatal("bep20 reused the live TRON amount 6.00 — collision set is not cross-provider")
	}
	if got.Amount != "6.21" {
		t.Errorf("bep20 amount = %q, want %q", got.Amount, "6.21")
	}
	// Binance has a tighter geometry (step 10, own MinGap 9), but a FOREIGN row's
	// wider tolerance wins: against the on-chain rails' tol-10 bands the required
	// gap is 2*10+1 = 21, so disjointness holds from either side. 6.00/6.10/6.20
	// are inside TRON's 6.00 band, and 6.20/6.30/6.40 inside BEP20's 6.21 band —
	// 6.50 is the first slot clear of both.
	bn, err := r.Reserve(ProviderBinance, "6.00", "9", "bn-1", tLive)
	if err != nil {
		t.Fatalf("binance Reserve: %v", err)
	}
	if bn.Amount != "6.50" {
		t.Errorf("binance amount = %q, want %q (must clear the wider foreign bands)", bn.Amount, "6.50")
	}
	// The disjointness this buys, stated directly: no live amount on any rail is
	// close enough to the Binance amount for one receipt to match both.
	for _, other := range []string{"6.00", "6.21"} {
		u := mustUnits(t, other)
		if abs64(bn.Units-u) < int64(2*tronParams.Tol+1) {
			t.Errorf("binance %s and live %s are %d units apart, want >= %d",
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
			ProviderBEP20: {Step: 21, Tol: 10, Decimals: 2},   // cents
			ProviderTron:  {Step: 210, Tol: 100, Decimals: 3}, // same money, 3 dp
		},
		EvictAfter: DefaultEvictAfter,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	trx, err := r.Reserve(ProviderTron, "6.000", "7", "trx-1", tLive)
	if err != nil {
		t.Fatalf("tron Reserve: %v", err)
	}
	if trx.Amount != "6.000" || trx.Units != 6000 || trx.Decimals != 3 {
		t.Fatalf("tron reservation = %q/%d/%ddp, want 6.000/6000/3dp", trx.Amount, trx.Units, trx.Decimals)
	}
	// The 3-dp 6.000 row must block the 2-dp rail's 6.00 slot after rescaling.
	bsc, err := r.Reserve(ProviderBEP20, "6.00", "8", "bsc-1", tLive)
	if err != nil {
		t.Fatalf("bep20 Reserve: %v", err)
	}
	if bsc.Amount != "6.21" {
		t.Errorf("bep20 amount = %q, want %q (the 3-dp TRON row must rescale into the set)", bsc.Amount, "6.21")
	}
	// ...and the reverse direction: the 2-dp row blocks the 3-dp rail's slots.
	trx2, err := r.Reserve(ProviderTron, "6.210", "9", "trx-2", tLive)
	if err != nil {
		t.Fatalf("tron Reserve #2: %v", err)
	}
	if trx2.Amount == "6.210" {
		t.Error("tron reused the live BEP20 amount 6.21 across precisions")
	}
	// Pin the exact slot too: 6.210 is blocked (distance 0 < the coarse-widened
	// gap 210), so the next step lands on 6.420 — 210 from the coarse row (>= the
	// widened gap, boundary-exact) and 420 from the same-rail 6.000 row.
	if trx2.Amount != "6.420" {
		t.Errorf("tron amount = %q, want %q (next slot past the coarse-widened gap)", trx2.Amount, "6.420")
	}
	// Redirect and the claim-jack gate both compare in the rail's own units.
	adopt(t, r, ProviderTron, "trx-sib", "7", "9.000", tDead)
	if sib, ok := r.ResolveRedirect(ProviderTron, "7", "trx-1", 9000, false); !ok || sib != "trx-sib" {
		t.Errorf("3-dp redirect = (%q,%v), want (\"trx-sib\",true)", sib, ok)
	}
	hit, err := r.RecvMatchesAnyOtherBand(ProviderTron, "trx-1", 9100) // 9.000 + tol
	if err != nil {
		t.Fatalf("RecvMatchesAnyOtherBand: %v", err)
	}
	if !hit {
		t.Error("3-dp band edge must trip the claim-jack gate")
	}
}

// TestReserveCoarseNeighbourSlack pins the review fix in collisionUnits: a
// foreign row on a COARSER rail effectively reaches one full coarse unit past
// its band's top edge, because each rail floors the received amount to its OWN
// precision before matching. Without the extra 10^(a-b)-1 slack the picker
// hands out an ambiguous amount:
//
//	requester TRON 3dp tol 100, foreign BEP20 2dp row 5.80 tol 10;
//	candidate 6.005 sits 205 rescaled units away (>= the naive 201 gap), yet a
//	single 5.906 USDT receipt floors to 5906 on TRON (|5906-6005| = 99 <= 100,
//	matches 6.005) AND to 590 on BEP20 (|590-580| = 10 <= 10, matches 5.80) —
//	one payment inside two live bands, i.e. it could settle either order.
//
// With the widened 210 gap the 6.005 slot is refused and the pick moves one
// step up, where no such double-match exists.
func TestReserveCoarseNeighbourSlack(t *testing.T) {
	clk := &fakeClock{t: tNow}
	r, err := New(Config{
		Providers: map[Provider]Params{
			ProviderBEP20: {Step: 21, Tol: 10, Decimals: 2},   // coarse rail
			ProviderTron:  {Step: 210, Tol: 100, Decimals: 3}, // fine rail
		},
		EvictAfter: DefaultEvictAfter,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adopt(t, r, ProviderBEP20, "coarse", "9", "5.80", tLive)

	// First: demonstrate the ambiguity is REAL for the naive pick, using the
	// package's own matchers — 5906 (a 5.906 receipt on the fine rail) is inside
	// 6.005's band, and its 2-dp floor 590 is inside 5.80's band.
	if ok, _ := RecvMatchesBand(Params{Step: 210, Tol: 100, Decimals: 3}, "6.005", 5906); !ok {
		t.Fatal("premise broken: 5906 no longer matches the 6.005 band")
	}
	if ok, _ := RecvMatchesBand(Params{Step: 21, Tol: 10, Decimals: 2}, "5.80", rescaleUnits(5906, 3, 2)); !ok {
		t.Fatal("premise broken: the floored receipt no longer matches the 5.80 band")
	}

	// Then: the picker must therefore refuse 6.005 and step past it.
	res, err := r.Reserve(ProviderTron, "6.005", "7", "fine-1", tLive)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Amount == "6.005" {
		t.Fatal("picker handed out 6.005 next to the coarse 5.80 band — a 5.906 receipt would match both orders")
	}
	if res.Amount != "6.215" {
		t.Errorf("amount = %q, want %q (base+step, clear of the widened coarse band)", res.Amount, "6.215")
	}
}

// TestReserveOwnDeadSessionsDoNotBlock is the direct port of Python's
// test_picker_excludes_own_dead_sessions (tests/test_usdt_redirect.py:110-134) —
// the 2026-07-02 incident fix (telegram_bot.py:9400-9409, 9440-9442).
func TestReserveOwnDeadSessionsDoNotBlock(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "own-a", "7", "6.21", tDead) // own, dead
	adopt(t, r, ProviderBEP20, "own-b", "7", "6.42", tDead) // own, dead
	adopt(t, r, ProviderBEP20, "own-c", "7", "6.63", tDead) // own, dead
	adopt(t, r, ProviderBEP20, "far-d", "9", "6.00", tLive) // foreign, live

	// Own dead rows freed -> the first slot clear of the foreign 6.00 band is 6.21.
	res, err := r.Reserve(ProviderBEP20, "6.00", "7", "new-1", tLive)
	if err != nil {
		t.Fatalf("Reserve for the same chat: %v", err)
	}
	if res.Amount != "6.21" {
		t.Errorf("own-dead-excluded amount = %q, want %q", res.Amount, "6.21")
	}
	r.Release(ProviderBEP20, "new-1") // restore state for the next assertion

	// No chat id (Python's legacy chat_id=None call): nothing is excluded, all
	// upward slots are blocked -> the single downward slot is the last resort.
	res, err = r.Reserve(ProviderBEP20, "6.00", "", "new-2", tLive)
	if err != nil {
		t.Fatalf("Reserve without a chat: %v", err)
	}
	if res.Amount != "5.79" {
		t.Errorf("chat-less amount = %q, want %q (last-resort slot)", res.Amount, "5.79")
	}
	r.Release(ProviderBEP20, "new-2")

	// Downward slot also held (foreign, LIVE) and the blockers are only
	// RECENTLY dead (1000s < EvictAfter 1800s) -> genuinely exhausted.
	adopt(t, r, ProviderBEP20, "far-e", "9", "5.79", tLive)
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
	// All five slots held by a FOREIGN chat, every row dead for 3000s > 1800s.
	for i, a := range []string{"6.00", "6.21", "6.42", "6.63", "5.79"} {
		adopt(t, r, ProviderBEP20, desN(i), "9", a, tStale)
	}
	res, err := r.Reserve(ProviderBEP20, "6.00", "7", "fresh", tLive)
	if err != nil {
		t.Fatalf("Reserve under pressure with stale blockers: %v", err)
	}
	if res.Amount != "6.00" {
		t.Errorf("evicted-pass amount = %q, want %q", res.Amount, "6.00")
	}
	// Eviction is soft: the stale rows are still present (their late payment can
	// still be submitted against their own order).
	if got := r.Len(ProviderBEP20); got != 6 {
		t.Errorf("Len = %d, want 6 — eviction must NOT delete rows", got)
	}

	// A stale row does NOT get evicted when the first pass already has a slot:
	// pass 1 avoids it, so the fresh amount must dodge it.
	r2, _ := newTestRegistry(t)
	adopt(t, r2, ProviderBEP20, "stale-1", "9", "6.00", tStale)
	res, err = r2.Reserve(ProviderBEP20, "6.00", "7", "fresh", tLive)
	if err != nil {
		t.Fatalf("Reserve with one stale blocker: %v", err)
	}
	if res.Amount != "6.21" {
		t.Errorf("no-pressure amount = %q, want %q (pass 1 must still avoid the stale row)", res.Amount, "6.21")
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
	adopt(t, r, ProviderBEP20, "reserved", "7", "6.00", tLive)
	adopt(t, r, ProviderBEP20, "finished", "7", "6.21", tDead)
	adopt(t, r, ProviderTron, "vanished", "7", "9.00", tLive)

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
	adopt(t, r, ProviderBEP20, "a", "7", "6.00", tDead)
	adopt(t, r, ProviderBEP20, "b", "7", "6.21", tDead)
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
	adopt(t, r, ProviderBEP20, "churn", "7", "6.00", tDead)
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
	adopt(t, r, ProviderBEP20, "a", "7", "6.21", tLive)
	adopt(t, r, ProviderBEP20, "b", "8", "6.42", tLive)
	adopt(t, r, ProviderTron, "c", "7", "6.00", tLive)

	res, ok := r.Get(ProviderBEP20, "a")
	if !ok {
		t.Fatal("Get after Adopt = not found")
	}
	if res.Units != 621 || res.Decimals != 2 {
		t.Errorf("Adopt units = %d/%d dec, want 621/2 (recomputed from Amount)", res.Units, res.Decimals)
	}
	if _, ok := r.Get(ProviderBEP20, "nope"); ok {
		t.Error("Get of an unknown des = found, want not found")
	}
	// Same provider only, requesting des excluded.
	if got := r.OtherAmounts(ProviderBEP20, "a"); len(got) != 1 || got[0] != "6.42" {
		t.Errorf("OtherAmounts = %v, want [6.42]", got)
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
	adopt(t, r, ProviderBEP20, "mine", "7", "6.21", tLive)
	adopt(t, r, ProviderBEP20, "theirs", "9", "6.00", tLive)
	adopt(t, r, ProviderTron, "other-rail", "9", "6.63", tLive)

	mustGate := func(p Provider, des string, recv int64) bool {
		t.Helper()
		hit, err := r.RecvMatchesAnyOtherBand(p, des, recv)
		if err != nil {
			t.Fatalf("RecvMatchesAnyOtherBand: %v", err)
		}
		return hit
	}
	if !mustGate(ProviderBEP20, "mine", 600) {
		t.Error("a receipt inside a stranger's band must be a hit (claim-jack refusal)")
	}
	if mustGate(ProviderBEP20, "mine", 621) {
		t.Error("the order's OWN band must be excluded from the gate")
	}
	if mustGate(ProviderBEP20, "mine", 663) {
		t.Error("a band on a different rail must not trip this rail's gate")
	}
	// An unknown provider must FAIL LOUDLY: `false` here is the accept side of a
	// money refusal, so it may never be returned by accident.
	if _, err := r.RecvMatchesAnyOtherBand("nosuch", "mine", 600); err == nil {
		t.Error("unknown provider = nil error, want error")
	}
}

// TestSnapshot is the atomic (expected, other_amounts) read Python takes in one
// lock section before the chain lookup (telegram_bot.py:9685-9693).
func TestSnapshot(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "mine", "7", "6.21", tLive)
	adopt(t, r, ProviderBEP20, "theirs", "9", "6.00", tLive)
	adopt(t, r, ProviderTron, "other-rail", "9", "6.42", tLive)

	res, others, ok := r.Snapshot(ProviderBEP20, "mine")
	if !ok {
		t.Fatal("Snapshot of a live row = not found")
	}
	if res.Amount != "6.21" || res.Units != 621 {
		t.Errorf("Snapshot reservation = %q/%d, want 6.21/621", res.Amount, res.Units)
	}
	if len(others) != 1 || others[0] != "6.00" {
		t.Errorf("Snapshot others = %v, want [6.00] (same rail, self excluded)", others)
	}
	if _, _, ok := r.Snapshot(ProviderBEP20, "gone"); ok {
		t.Error("Snapshot of a missing row = found, want the ORDER_GONE branch")
	}
}

// TestResolveRedirect is the same-chat sibling redirect with its depth-1 guard
// (_usdt_own_sibling @9620; call sites @9648 and @9824-9832), i.e. the ported
// cases from tests/test_usdt_redirect.py:74-107.
func TestResolveRedirect(t *testing.T) {
	// Current session 6.21 (des "new"), the customer's earlier EXPIRED session
	// 6.00 (des "old") — they had already sent 6.00.
	setup := func(siblingChat string) *Registry {
		r, _ := newTestRegistry(t)
		adopt(t, r, ProviderBEP20, "new", "7", "6.21", tLive)
		adopt(t, r, ProviderBEP20, "old", siblingChat, "6.00", tDead)
		return r
	}

	// Same chat: a 6.00 receipt reroutes to their OWN 6.00 session.
	sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 600, false)
	if !ok || sib != "old" {
		t.Errorf("same-chat redirect = (%q,%v), want (\"old\",true)", sib, ok)
	}
	// The sibling's band is fuzzy too: 6.10 is 6.00 + exactly tol.
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 610, false); !ok || sib != "old" {
		t.Errorf("sibling band edge = (%q,%v), want (\"old\",true)", sib, ok)
	}
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 611, false); ok {
		t.Errorf("one unit past the sibling band redirected to %q, want no redirect", sib)
	}
	// Cross-chat: the 6.00 band belongs to a STRANGER -> claim-jack reject, no
	// redirect (tests/test_usdt_redirect.py:82-88).
	if sib, ok := setup("9").ResolveRedirect(ProviderBEP20, "7", "new", 600, false); ok {
		t.Errorf("cross-chat band matched redirected to %q, want no redirect", sib)
	}
	// DEPTH-1 GUARD: a verify that was ALREADY redirected never redirects again.
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 600, true); ok {
		t.Errorf("depth-2 redirect returned %q, want refusal", sib)
	}
	// Paying the CURRENT session's own amount finds no sibling (the caller's
	// `matched` branch settles it directly).
	if sib, ok := setup("7").ResolveRedirect(ProviderBEP20, "7", "new", 621, false); ok {
		t.Errorf("exact-amount receipt redirected to %q, want no redirect", sib)
	}
	// An unknown owner is never a sibling, and neither is an unknown provider.
	if _, ok := setup("7").ResolveRedirect(ProviderBEP20, "", "new", 600, false); ok {
		t.Error("chat-less redirect lookup = true, want false")
	}
	if _, ok := setup("7").ResolveRedirect("nosuch", "7", "new", 600, false); ok {
		t.Error("unknown-provider redirect lookup = true, want false")
	}
}

// TestResolveRedirectPrefersFreshestSibling pins determinism where Python leans
// on dict insertion order: with two same-chat rows in the SAME band (possible
// only after an eviction reused an amount), the freshest deadline wins.
func TestResolveRedirectPrefersFreshestSibling(t *testing.T) {
	r, _ := newTestRegistry(t)
	adopt(t, r, ProviderBEP20, "cur", "7", "6.21", tLive)
	adopt(t, r, ProviderBEP20, "z-older", "7", "6.00", tStale)
	adopt(t, r, ProviderBEP20, "a-newer", "7", "6.00", tDead)
	for range 20 { // map iteration order is randomised; the answer must not be
		sib, ok := r.ResolveRedirect(ProviderBEP20, "7", "cur", 600, false)
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
// the same rail. Exactly the 5 slots must be handed out, each exactly once, and
// everyone else must get ErrBandExhausted — never a duplicate amount. Run with
// -race.
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
	if len(granted) != 5 {
		t.Errorf("granted %d amounts (%v), want exactly the 5 slots", len(granted), granted)
	}
	if refused != n-5 {
		t.Errorf("refused %d, want %d", refused, n-5)
	}
	for _, want := range []string{"6.00", "6.21", "6.42", "6.63", "5.79"} {
		if _, ok := granted[want]; !ok {
			t.Errorf("slot %q was never handed out", want)
		}
	}
	if got := r.Len(ProviderBEP20); got != 5 {
		t.Errorf("Len = %d, want 5", got)
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
			if _, err := r.RecvMatchesAnyOtherBand(p, des, 600); err != nil {
				t.Errorf("RecvMatchesAnyOtherBand: %v", err)
			}
			r.ResolveRedirect(p, chatN(i%4), des, 600, false)
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
	u, err := AmountUnits(amount, 2)
	if err != nil {
		t.Fatalf("AmountUnits(%q): %v", amount, err)
	}
	return u
}
