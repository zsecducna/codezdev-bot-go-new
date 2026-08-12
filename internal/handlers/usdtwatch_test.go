package handlers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/tron"
)

// ---- harness ----------------------------------------------------------------

// watchFake stands in for the TronGrid incoming-transfer read. It counts calls
// because "no live deposits means NO RPC call at all" is a property of the
// watcher that only a counter can prove — a test that merely asserted "nothing
// was credited" would pass just as happily on a bot that polls a public API
// forever with nobody depositing.
type watchFake struct {
	mu        sync.Mutex
	transfers []tron.Transfer
	err       error
	calls     atomic.Int64
	lastSince atomic.Int64
}

func (w *watchFake) fetch(minTSMS int64) ([]tron.Transfer, error) {
	w.calls.Add(1)
	w.lastSince.Store(minTSMS)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.transfers, w.err
}

func (w *watchFake) set(txs ...tron.Transfer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.transfers = txs
}

// installWatchFake points the bot's one chain-read seam at a fake.
func installWatchFake(b *Bot) *watchFake {
	w := &watchFake{}
	b.tronTransfersSince = w.fetch
	return w
}

// ---- the four watcher tests -------------------------------------------------

// TestWatcherCreditsMatchingTransfer is the feature in one test: a customer
// opens a deposit, sends the amount they were quoted, and their balance moves
// WITHOUT them ever pasting a transaction hash.
func TestWatcherCreditsMatchingTransfer(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4701

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, ok := b.bands.Get(bands.ProviderTron, o.Des)
	if !ok {
		t.Fatalf("no TRON band for deposit %s", o.Des)
	}
	w.set(*trxTx(hash64("a"), res.Amount, b.now().UnixMilli()))
	// Read before the tick: a settled deposit's rail row is dropped along with
	// its band, so afterwards there is nothing left to compare the bound to.
	row, _ := b.getUSDTRow(bands.ProviderTron, o.Des)

	b.tickOnchain(ctx, bands.ProviderTron)

	if w.calls.Load() != 1 {
		t.Errorf("fetch called %d times, want exactly 1 — one chain read serves every open deposit on the rail", w.calls.Load())
	}
	// The query is bounded by the OLDEST open deposit's start, minus the verify
	// grace. Pinned exactly, because a bound one second too tight silently drops
	// a matchable payment and nothing else in the system would notice.
	if want := row.SinceMS - b.usdtVerifyGrace.Milliseconds(); w.lastSince.Load() != want {
		t.Errorf("fetched since %d ms, want %d (the deposit's own start minus the verify grace)", w.lastSince.Load(), want)
	}
	// The customer asked for 6 USDT and was quoted 6.01000 — the jitter that
	// makes the amount name their order. They sent the quoted figure, so
	// 6.01 USDT arrived, and at the harness rate of 25,000 VND that is 150,250.
	// DepositCredit never credits LESS than what arrived, so the customer keeps
	// the jitter; the round-up-to-requested branch only fires when a payment
	// lands slightly UNDER the quote.
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 150250 {
		t.Errorf("balance after the watcher tick = %d VND, want 150250 — the deposit was not credited hands-free", bal)
	}
	// The order is closed for history/revenue, and the band is released so its
	// amount returns to the picker.
	done, _ := st.GetOrder(o.Des)
	if done == nil || done.Status != "delivered" {
		t.Errorf("deposit order status = %s, want delivered", orderStatus(done))
	}
	if _, still := b.bands.Get(bands.ProviderTron, o.Des); still {
		t.Error("the settled deposit's band is still live — its amount can never be re-quoted")
	}
}

// TestWatcherDoesNotDoubleCredit re-presents the SAME transfer on a second tick.
// The processed.txn_id primary key inside store.Credit is what makes this safe;
// TxnExists is only the pre-filter that saves the work of getting there.
//
// A second live deposit (a different customer) is deliberately left open, so the
// second tick genuinely performs the fetch and genuinely re-examines the already
// settled transfer. Without it the tick would exit at the empty-bands gate and
// the test would prove nothing about dedupe at all.
func TestWatcherDoesNotDoubleCredit(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const payer, bystander = 4702, 4703

	b.Process(ctx, callbackUpdate(payer, "dep:trx"))
	b.Process(ctx, textUpdate(payer, "6"))
	paid := reservedDeposit(t, st, payer)
	res, _ := b.bands.Get(bands.ProviderTron, paid.Des)

	b.Process(ctx, callbackUpdate(bystander, "dep:trx"))
	b.Process(ctx, textUpdate(bystander, "9"))

	w.set(*trxTx(hash64("b"), res.Amount, b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)
	first, _ := st.GetBalance(fmtInt(payer))
	if first != 150250 {
		t.Fatalf("first tick credited %d VND, want 150250", first)
	}

	b.tickOnchain(ctx, bands.ProviderTron)
	if w.calls.Load() != 2 {
		t.Fatalf("fetch called %d times, want 2 — the second tick must actually re-read the chain for this test to mean anything", w.calls.Load())
	}
	if again, _ := st.GetBalance(fmtInt(payer)); again != first {
		t.Errorf("a re-seen transfer credited again (%d -> %d VND)", first, again)
	}
	if bal, _ := st.GetBalance(fmtInt(bystander)); bal != 0 {
		t.Errorf("the bystander's balance moved (%d VND) — the transfer was attributed to the wrong customer", bal)
	}
}

// TestWatcherSkipsFetchWhenNoLiveBands: a shop with nobody depositing must not
// poll a public RPC endpoint on a timer.
//
// The bot here is not idle — it has a live PRODUCT ORDER band on the same rail.
// That is the sharper half of the assertion: product orders are paid through
// their own hash-paste flow, the watcher cannot settle one, so their presence
// must not wake the fetch either.
func TestWatcherSkipsFetchWhenNoLiveBands(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4704

	// Truly empty first.
	b.tickOnchain(ctx, bands.ProviderTron)
	if w.calls.Load() != 0 {
		t.Fatalf("fetch called %d times with no bands at all, want 0", w.calls.Load())
	}

	// Now a live product order — allowDownward=true is the order path.
	des := usdtOrder(t, st, chat, 150000)
	if _, err := b.reserveUSDTAmount(bands.ProviderTron, "6.00", fmtInt(chat), des,
		150000, b.now().UnixMilli(), b.now().Add(time.Hour), true); err != nil {
		t.Fatalf("reserve order band: %v", err)
	}
	if b.bands.Len(bands.ProviderTron) != 1 {
		t.Fatalf("expected exactly one live band, got %d", b.bands.Len(bands.ProviderTron))
	}

	b.tickOnchain(ctx, bands.ProviderTron)
	if w.calls.Load() != 0 {
		t.Errorf("fetch called %d times with only a PRODUCT ORDER live, want 0 — the watcher cannot settle one, so it must not poll for one", w.calls.Load())
	}
}

// TestWatcherMatchesRehydratedBand pins the hole the rejected
// per-deposit-goroutine design had: a deposit made just before a restart. The
// band is loaded from the persisted table rather than reserved in memory, and
// the very next tick must still match it — otherwise the customer's money
// arrives during a deploy and is never credited.
//
// releaseUnpersisted is the restart stand-in: it drops the in-memory band and
// rail row while deliberately leaving the persisted pending row alone, which is
// exactly the state a fresh process boots into.
func TestWatcherMatchesRehydratedBand(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4705

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, _ := b.bands.Get(bands.ProviderTron, o.Des)
	amount := res.Amount

	b.releaseUnpersisted(bands.ProviderTron, o.Des) // "restart"
	if b.bands.Len(bands.ProviderTron) != 0 {
		t.Fatalf("band survived the simulated restart — the test is not exercising rehydration")
	}
	b.rehydrateOnchainPending(bands.ProviderTron)
	if b.bands.Len(bands.ProviderTron) != 1 {
		t.Fatalf("rehydrate loaded %d bands, want 1", b.bands.Len(bands.ProviderTron))
	}

	w.set(*trxTx(hash64("c"), amount, b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)

	if w.calls.Load() != 1 {
		t.Errorf("fetch called %d times after rehydration, want 1 — a rehydrated deposit must still be watched", w.calls.Load())
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 150250 {
		t.Errorf("balance after rehydrate+tick = %d VND, want 150250 — a deposit made across a restart was dropped", bal)
	}
}

// ---- the prerequisite's assertion, end to end -------------------------------

// TestWatcherNeverSettlesAProductOrder is the handlers-level twin of
// bands.TestMatchIncomingIgnoresProductOrders, and the reason
// Reservation.IsDeposit exists.
//
// A transfer lands EXACTLY on a live product order's quoted amount while a
// deposit is also open (so the fetch definitely runs and the transfer is
// definitely examined). settleUSDTMatch is deposit-only logic — it credits the
// wallet — so settling the order here would pay the customer money for goods
// whose stock is still reserved and undelivered, and leave them able to buy
// them again with it.
func TestWatcherNeverSettlesAProductOrder(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const buyer, depositor = 4706, 4707

	// A live product order on TRON.
	des := usdtOrder(t, st, buyer, 150000)
	orderAmount, err := b.reserveUSDTAmount(bands.ProviderTron, "6.00", fmtInt(buyer), des,
		150000, b.now().UnixMilli(), b.now().Add(time.Hour), true)
	if err != nil {
		t.Fatalf("reserve order band: %v", err)
	}
	// A live deposit, so the watcher has a reason to fetch at all.
	b.Process(ctx, callbackUpdate(depositor, "dep:trx"))
	b.Process(ctx, textUpdate(depositor, "9"))

	w.set(*trxTx(hash64("d"), orderAmount, b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)

	if w.calls.Load() != 1 {
		t.Fatalf("fetch called %d times, want 1 — the open deposit should have driven one read", w.calls.Load())
	}
	if bal, _ := st.GetBalance(fmtInt(buyer)); bal != 0 {
		t.Errorf("the buyer's wallet was credited %d VND for a PRODUCT ORDER payment — the order's stock was never delivered", bal)
	}
	o, _ := st.GetOrder(des)
	if o == nil || o.Status != "reserved" {
		t.Errorf("product order status = %s, want reserved (untouched) — the watcher settled an order it has no delivery logic for", orderStatus(o))
	}
	if _, still := b.bands.Get(bands.ProviderTron, des); !still {
		t.Error("the product order's band was released by the watcher — its unique amount is no longer bound to the order the customer is paying")
	}
}

// TestWatcherFetchFailureHoldsEveryBand: a read failure must never free a band.
// Releasing a live amount on a transient TronGrid error could hand it to another
// order, and then one payment settles the wrong one.
func TestWatcherFetchFailureHoldsEveryBand(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4708

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, _ := b.bands.Get(bands.ProviderTron, o.Des)

	w.mu.Lock()
	w.err = context.DeadlineExceeded
	w.mu.Unlock()
	b.tickOnchain(ctx, bands.ProviderTron)

	if _, still := b.bands.Get(bands.ProviderTron, o.Des); !still {
		t.Fatal("a fetch failure released the band — the customer's quoted amount could now be handed to someone else")
	}
	// The next tick, with the chain answering, still settles it.
	w.mu.Lock()
	w.err = nil
	w.mu.Unlock()
	w.set(*trxTx(hash64("e"), res.Amount, b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 150250 {
		t.Errorf("balance after the retry tick = %d VND, want 150250 — a transient fetch error lost the deposit", bal)
	}
}
