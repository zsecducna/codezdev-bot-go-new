package handlers

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/store"
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
//
// WHAT THIS PROVES, precisely: the TxnExists PRE-FILTER stops the second
// attempt before it reaches storage. It does NOT prove the processed.txn_id
// primary key inside store.Credit — the second tick never gets that far.
// TestUSDTDepositReplayCreditsOnce is what pins the PK as the real gate (it
// replays a hash through the verify path, which does not consult TxnExists at
// all). Both matter: the PK is the guarantee, this is the shortcut.
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

// TestWatcherCreditsNothingWhenAmbiguous: two customers depositing the same
// amount are quoted 0.00101 apart, which is far inside any fee-shaped window —
// so a payment that arrives SHORT (the sender's platform skimmed the network
// fee) sits inside both of their allowances and names neither. Crediting either
// one is a coin flip with the other's money.
//
// The transfer must also be left UNCONSUMED — no ledger row — because a human
// still has to be able to attribute it by hand afterwards. A watcher that
// "resolved" the ambiguity by marking the transfer used would take that away.
func TestWatcherCreditsNothingWhenAmbiguous(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const first, second = 4709, 4710

	b.Process(ctx, callbackUpdate(first, "dep:trx"))
	b.Process(ctx, textUpdate(first, "6"))
	o1 := reservedDeposit(t, st, first)
	r1, _ := b.bands.Get(bands.ProviderTron, o1.Des)

	b.Process(ctx, callbackUpdate(second, "dep:trx"))
	b.Process(ctx, textUpdate(second, "6"))
	o2 := reservedDeposit(t, st, second)
	r2, _ := b.bands.Get(bands.ProviderTron, o2.Des)
	if r1.Amount == r2.Amount {
		t.Fatalf("both deposits were quoted %s — the band picker did not separate them", r1.Amount)
	}

	// Short by 0.10 USDT against the LOWER of the two quotes: beyond Tol (so it
	// is not an exact match for either) and inside both 5%/0.5-capped fee
	// allowances (so both are shortfall candidates).
	lowUnits, err := bands.AmountUnits(min(r1.Amount, r2.Amount), tron.AmountDecimals())
	if err != nil {
		t.Fatalf("amount units: %v", err)
	}
	shortUnits := lowUnits - 10_000 // -0.10 USDT
	// Pin that the receipt really IS ambiguous. Without this the test would pass
	// just as happily on a receipt nothing matches, and "credited nobody" would
	// mean nothing.
	if _, kind, merr := b.bands.MatchIncoming(bands.ProviderTron, shortUnits); merr != nil || kind != bands.MatchAmbiguous {
		t.Fatalf("MatchIncoming(%d) = (%s, %v), want ambiguous — the test is not exercising the branch it claims", shortUnits, kind, merr)
	}
	short := bands.FormatUnits(shortUnits, tron.AmountDecimals())
	h := hash64("f")
	w.set(*trxTx(h, short, b.now().UnixMilli()))

	b.tickOnchain(ctx, bands.ProviderTron)

	if bal, _ := st.GetBalance(fmtInt(first)); bal != 0 {
		t.Errorf("first depositor credited %d VND on an ambiguous payment", bal)
	}
	if bal, _ := st.GetBalance(fmtInt(second)); bal != 0 {
		t.Errorf("second depositor credited %d VND on an ambiguous payment", bal)
	}
	// Unconsumed: the operator must still be able to settle it by hand.
	if used, terr := b.store.TxnExists("trx:" + h); terr != nil || used {
		t.Errorf("TxnExists(trx:%s) = (%t, %v), want (false, nil) — an ambiguous transfer must stay "+
			"available for manual attribution", h, used, terr)
	}
}

// TestWatcherQuarantineSurvivesTheCompetitionAndARestart is the durable half of
// the ambiguity refusal, and the reason `ambiguous_txns` exists.
//
// Refusing per tick is not enough. The transfer is deliberately left unconsumed
// in the processed ledger so a human can still settle it, which also means a
// later tick re-examines it — and once the competing deposit settles, the very
// same transfer looks unambiguous and would be credited to whichever deposit
// survived. Waiting produced no new evidence about whose money it is, so that
// is exactly as much of a guess as crediting it in the first place.
//
// The second half rebuilds the Bot against the SAME store, which is the whole
// point of durable over in-memory: this project deploys often, so a
// process-local memory would only shrink the window to "until the next deploy".
func TestWatcherQuarantineSurvivesTheCompetitionAndARestart(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const first, second = 4712, 4713

	b.Process(ctx, callbackUpdate(first, "dep:trx"))
	b.Process(ctx, textUpdate(first, "6"))
	o1 := reservedDeposit(t, st, first)
	r1, _ := b.bands.Get(bands.ProviderTron, o1.Des)

	b.Process(ctx, callbackUpdate(second, "dep:trx"))
	b.Process(ctx, textUpdate(second, "6"))
	o2 := reservedDeposit(t, st, second)
	r2, _ := b.bands.Get(bands.ProviderTron, o2.Des)

	// A receipt short of the lower quote by 0.10 USDT: beyond Tol for either, and
	// inside both fee allowances, so neither band can claim it.
	lowUnits, err := bands.AmountUnits(min(r1.Amount, r2.Amount), tron.AmountDecimals())
	if err != nil {
		t.Fatalf("amount units: %v", err)
	}
	shortUnits := lowUnits - 10_000
	if _, kind, merr := b.bands.MatchIncoming(bands.ProviderTron, shortUnits); merr != nil || kind != bands.MatchAmbiguous {
		t.Fatalf("MatchIncoming(%d) = (%s, %v), want ambiguous", shortUnits, kind, merr)
	}
	ambiguousHash := hash64("7")
	ambiguous := *trxTx(ambiguousHash, bands.FormatUnits(shortUnits, tron.AmountDecimals()), b.now().UnixMilli())

	w.set(ambiguous)
	b.tickOnchain(ctx, bands.ProviderTron)

	if held, qerr := st.AmbiguousTxnExists("trx:" + ambiguousHash); qerr != nil || !held {
		t.Fatalf("AmbiguousTxnExists = (%t, %v), want (true, nil) — the refusal was not recorded", held, qerr)
	}
	// It must stay OUT of the processed ledger: a human still has to be able to
	// credit it by hand through the hash-paste path.
	if used, terr := st.TxnExists("trx:" + ambiguousHash); terr != nil || used {
		t.Fatalf("TxnExists = (%t, %v), want (false, nil) — quarantine must not consume the transfer", used, terr)
	}

	// Now thin the competition out the way production would: the first deposit
	// is settled by ITS OWN payment, so only one band is left standing.
	w.set(ambiguous, *trxTx(hash64("6"), r1.Amount, b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)
	if bal, _ := st.GetBalance(fmtInt(first)); bal != 150250 {
		t.Fatalf("first depositor balance = %d VND, want 150250 — its own payment should have settled", bal)
	}
	if b.bands.Len(bands.ProviderTron) != 1 {
		t.Fatalf("expected exactly one surviving band, got %d", b.bands.Len(bands.ProviderTron))
	}
	// The ambiguous transfer is now a clean shortfall match for the survivor.
	// It must STILL not be credited.
	b.tickOnchain(ctx, bands.ProviderTron)
	if bal, _ := st.GetBalance(fmtInt(second)); bal != 0 {
		t.Errorf("survivor credited %d VND from a quarantined transfer — attribution by attrition", bal)
	}

	// ---- restart: a brand new Bot and band registry, same database ----------
	b2 := New(Deps{TG: &fakeTG{}, Store: st, Stock: b.stock, Config: b.cfg, AdminID: "999"})
	defer b2.Shutdown() // st is closed by the first bot's cleanup, not here
	w2 := installWatchFake(b2)
	b2.rehydrateOnchainPending(bands.ProviderTron)
	if b2.bands.Len(bands.ProviderTron) != 1 {
		t.Fatalf("rebuilt bot rehydrated %d bands, want 1 — without a live band the tick would "+
			"skip for the wrong reason and prove nothing", b2.bands.Len(bands.ProviderTron))
	}
	w2.set(ambiguous)
	b2.tickOnchain(ctx, bands.ProviderTron)
	if bal, _ := st.GetBalance(fmtInt(second)); bal != 0 {
		t.Errorf("a restarted bot credited %d VND from a quarantined transfer — the refusal did not survive the deploy", bal)
	}
}

// TestWatcherAuditsTheDecisionsItRefuses pins the durable trail. The audit
// table could already answer "which transactions were credited"; the question
// that had no answer outside a log file was "which transactions did the bot SEE
// and decide NOT to credit, and why". On a money path that has to be queryable.
//
// Every row is asserted from the STORE, not from the helper's return value —
// this repo's standard for audit assertions, because a formatter that returns
// the right string and persists nothing is exactly the regression worth
// catching. Both rows are keyed on the LEDGER ID, the same target the settle
// path uses, so one query by transaction returns its whole decision history.
func TestWatcherAuditsTheDecisionsItRefuses(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4714

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, _ := b.bands.Get(bands.ProviderTron, o.Des)
	row, _ := b.getUSDTRow(bands.ProviderTron, o.Des)

	// A stale transfer: matches the band's amount exactly, but predates it.
	staleHash := hash64("3")
	w.set(*trxTx(staleHash, res.Amount, row.SinceMS-b.usdtVerifyGrace.Milliseconds()-1000))
	b.tickOnchain(ctx, bands.ProviderTron)

	got := latestAuditRow(t, st, "usdt_trx_watch_stale")
	if got == nil {
		t.Fatal("no usdt_trx_watch_stale audit row — the refusal exists only in a log line")
	}
	if got.Target != "trx:"+staleHash {
		t.Errorf("audit target = %q, want %q — a refusal not keyed on the ledger id cannot be found "+
			"by the same query that finds the credit", got.Target, "trx:"+staleHash)
	}
	if got.ActorChat != fmtInt(chat) {
		t.Errorf("audit chat = %q, want %q — the matched band names an owner, so the row should", got.ActorChat, fmtInt(chat))
	}

	// An ambiguous transfer: a second deposit at the same price makes a short
	// receipt claimable by either, and the refusal is recorded with NO owner.
	b.Process(ctx, callbackUpdate(4715, "dep:trx"))
	b.Process(ctx, textUpdate(4715, "6"))
	lowUnits, err := bands.AmountUnits(res.Amount, tron.AmountDecimals())
	if err != nil {
		t.Fatalf("amount units: %v", err)
	}
	shortUnits := lowUnits - 10_000
	if _, kind, merr := b.bands.MatchIncoming(bands.ProviderTron, shortUnits); merr != nil || kind != bands.MatchAmbiguous {
		t.Fatalf("MatchIncoming(%d) = (%s, %v), want ambiguous", shortUnits, kind, merr)
	}
	ambiguousHash := hash64("4")
	w.set(*trxTx(ambiguousHash, bands.FormatUnits(shortUnits, tron.AmountDecimals()), b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)

	got = latestAuditRow(t, st, "usdt_trx_watch_ambiguous")
	if got == nil {
		t.Fatal("no usdt_trx_watch_ambiguous audit row — the one refusal a human has to act on is unqueryable")
	}
	if got.Target != "trx:"+ambiguousHash {
		t.Errorf("audit target = %q, want %q", got.Target, "trx:"+ambiguousHash)
	}
	if got.ActorChat != "" {
		t.Errorf("audit chat = %q, want empty — an ambiguous payment has no single owner, which is "+
			"precisely why it was refused; naming one would assert the thing the bot declined to decide", got.ActorChat)
	}

	// The NEXT tick over the same transfer is a distinct verdict: an operator
	// must be able to tell "just became ambiguous" from "already known, skipped
	// again", or a re-read looks like a fresh incident.
	b.tickOnchain(ctx, bands.ProviderTron)
	got = latestAuditRow(t, st, "usdt_trx_watch_quarantined")
	if got == nil {
		t.Fatal("no usdt_trx_watch_quarantined audit row — a repeat skip is indistinguishable from a new detection")
	}
	if got.Target != "trx:"+ambiguousHash {
		t.Errorf("audit target = %q, want %q", got.Target, "trx:"+ambiguousHash)
	}
}

// adoptWatchedDeposit installs a synthetic live DEPOSIT band straight into the
// registry.
//
// The refusals below are about a band whose ORDER ROW is wrong — missing, not a
// deposit, or owned by an unparseable chat id. The normal deposit flow cannot
// produce any of those (it writes the order and the band together, consistently),
// so the band has to be adopted by hand against an order the test shaped itself.
// Adopt is the same entry point rehydrateOnchainPending uses after a restart, so
// this is a real production shape and not a test-only door.
func adoptWatchedDeposit(t *testing.T, b *Bot, des, chatID, amount string) bands.Reservation {
	t.Helper()
	if err := b.bands.Adopt(bands.Reservation{
		Provider:  bands.ProviderTron,
		Des:       des,
		ChatID:    chatID,
		Amount:    amount,
		CreatedAt: b.now(),
		Deadline:  b.now().Add(time.Hour),
		IsDeposit: true,
	}); err != nil {
		t.Fatalf("adopt band %s: %v", des, err)
	}
	res, ok := b.bands.Get(bands.ProviderTron, des)
	if !ok {
		t.Fatalf("band %s did not stick", des)
	}
	return res
}

// tickAuditRow drives one tick over a single transfer and returns the audit row
// the watcher wrote for the named verdict. Fails the test when there is none:
// "the bot silently did nothing" is the regression these tests exist to catch.
func tickAuditRow(t *testing.T, b *Bot, w *watchFake, st *store.Store, verdict, hash, amount string) *store.AuditRow {
	t.Helper()
	w.set(*trxTx(hash, amount, b.now().UnixMilli()))
	b.tickOnchain(context.Background(), bands.ProviderTron)
	got := latestAuditRow(t, st, "usdt_trx_watch_"+verdict)
	if got == nil {
		t.Fatalf("no usdt_trx_watch_%s audit row — that refusal exists only in a log line", verdict)
	}
	if got.Target != "trx:"+hash {
		t.Errorf("audit target = %q, want %q — a refusal not keyed on the ledger id cannot be found "+
			"by the same query that finds the credit", got.Target, "trx:"+hash)
	}
	return got
}

// TestWatcherAuditsRefusalsOnABrokenOrderRow covers the three verdicts the
// watcher reaches AFTER a clean amount match, when the order behind the matched
// band turns out to be unusable. Each is a payment that arrived and was not
// credited, so each is a support ticket in waiting — and until the audit rows
// existed, answering it meant grepping logs on the box.
func TestWatcherAuditsRefusalsOnABrokenOrderRow(t *testing.T) {
	// The matched band's order row has vanished. The band outlives it until the
	// sweep, and in the meantime there is nothing to credit against.
	t.Run("order_gone", func(t *testing.T) {
		b, _, st, _, _ := newUSDTBot(t)
		w := installWatchFake(b)
		res := adoptWatchedDeposit(t, b, "ORDERGONE", "4720", "6.01000")

		got := tickAuditRow(t, b, w, st, "order_gone", hash64("b1"), res.Amount)
		if got.ActorChat != "4720" {
			t.Errorf("audit chat = %q, want \"4720\" — the band still names its owner even though "+
				"the order row does not", got.ActorChat)
		}
	})

	// The order exists but is a PRODUCT order, which the watcher must never
	// wallet-credit. Reaching this means the deposit flag and the order table
	// disagree, so the row is the only trace of a real bug.
	t.Run("not_deposit", func(t *testing.T) {
		b, _, st, _, _ := newUSDTBot(t)
		w := installWatchFake(b)
		const des, chat = "NOTDEP", "4721"
		if err := st.CreateOrder(des, chat, "vi", "kiro_pro", "", 1, 150000); err != nil {
			t.Fatalf("create order: %v", err)
		}
		res := adoptWatchedDeposit(t, b, des, chat, "6.01000")

		got := tickAuditRow(t, b, w, st, "not_deposit", hash64("b2"), res.Amount)
		if got.ActorChat != chat {
			t.Errorf("audit chat = %q, want %q", got.ActorChat, chat)
		}
		if bal, _ := st.GetBalance(chat); bal != 0 {
			t.Errorf("balance = %d VND, want 0 — a product order was wallet-credited", bal)
		}
	})

	// The order's own chat id is not a number. settleUSDTMatch keys the credit on
	// it, so proceeding would pay chat "0" — a stranger. The row must NOT be filed
	// under the same unusable value it is reporting.
	t.Run("bad_chat", func(t *testing.T) {
		b, _, st, _, _ := newUSDTBot(t)
		w := installWatchFake(b)
		const des, badChat = "BADCHAT", "not-a-number"
		if err := st.CreateOrder(des, badChat, "vi", depositProduct, "", 1, 150000); err != nil {
			t.Fatalf("create order: %v", err)
		}
		res := adoptWatchedDeposit(t, b, des, badChat, "6.01000")

		got := tickAuditRow(t, b, w, st, "bad_chat", hash64("b3"), res.Amount)
		if got.ActorChat != "" {
			t.Errorf("audit chat = %q, want empty — filing the row under the very chat id it is "+
				"reporting as unusable makes it unfindable", got.ActorChat)
		}
		if bal, _ := st.GetBalance("0"); bal != 0 {
			t.Errorf("chat \"0\" balance = %d VND, want 0 — the unparseable owner was credited to a stranger", bal)
		}
	})
}

// TestWatcherAuditsAHeldPaymentItCannotValue is the no_rate verdict. Without a
// usable USDT rate the payment cannot be turned into VND at all, and crediting 0
// would lose the customer's money outright — so the watcher holds and retries.
// A HELD payment is the case most likely to be reported as "my deposit never
// arrived", and the reason it is held has to be answerable from the database.
func TestWatcherAuditsAHeldPaymentItCannotValue(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4722

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, _ := b.bands.Get(bands.ProviderTron, o.Des)
	// The rail's own converter is a seam; break it the way a missing or garbage
	// admin rate breaks it in production.
	b.tronUsdtToVND = func(*big.Rat) (int64, error) { return 0, errors.New("no usdt rate configured") }

	got := tickAuditRow(t, b, w, st, "no_rate", hash64("b4"), res.Amount)
	if got.ActorChat != fmtInt(chat) {
		t.Errorf("audit chat = %q, want %q", got.ActorChat, fmtInt(chat))
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("balance = %d VND, want 0 — an unvaluable payment must not be credited at a guessed rate", bal)
	}
	// Held, not refused: the band and its order stay put so a later tick with a
	// working rate still credits the customer.
	if _, still := b.bands.Get(bands.ProviderTron, o.Des); !still {
		t.Error("the band was released on a rate failure — its amount can now be re-quoted to someone else")
	}
}

// TestWatcherAuditsAnAttributedPaymentThatDidNotCredit is the uncredited verdict:
// the transfer was attributed to exactly one deposit and the credit itself still
// failed. Nothing moved, so the next tick retries — which is precisely why it
// needs a durable record: a failure that silently retries forever looks identical
// to one that never happened.
//
// The credit is forced to fail by pushing the customer's balance to the top of
// the int64 range first. SQLite does not raise on integer overflow — it redoes
// the `amount + excluded.amount` in floating point and stores a REAL — so the
// error actually comes from store.Credit's read-back scanning that REAL into an
// int64. Either way the call returns an error, which is the only thing this test
// needs; the mechanism is incidental, and if a driver ever changed here the test
// would fail loudly rather than pass for the wrong reason (the assertions below
// are all on the uncredited verdict, which no other branch writes).
func TestWatcherAuditsAnAttributedPaymentThatDidNotCredit(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4723

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, _ := b.bands.Get(bands.ProviderTron, o.Des)
	if _, err := st.CreditBalance(fmtInt(chat), math.MaxInt64); err != nil {
		t.Fatalf("seed balance: %v", err)
	}

	got := tickAuditRow(t, b, w, st, "uncredited", hash64("b5"), res.Amount)
	if got.ActorChat != fmtInt(chat) {
		t.Errorf("audit chat = %q, want %q", got.ActorChat, fmtInt(chat))
	}
	// The order and the band are untouched, which is what makes the retry real.
	if still, _ := st.GetOrder(o.Des); still == nil || still.Status != "reserved" {
		t.Errorf("order status = %s, want reserved — a failed credit must not close the deposit", orderStatus(still))
	}
	if _, still := b.bands.Get(bands.ProviderTron, o.Des); !still {
		t.Error("the band was released after a failed credit — the retry has nothing left to match against")
	}
}

// TestWatcherIgnoresATransferOlderThanItsBand is the per-band freshness gate.
//
// The chain query's lower bound is the OLDEST live deposit's start, which is
// deliberately loose — but band amounts are RECYCLED as sessions finish, so a
// transfer that was never credited can still be sitting in that window when a
// NEW deposit is quoted the same amount. Attributing it would pay one customer
// with another customer's money, and the hash-paste path has always refused it
// (verifyUSDTTxid's "outside_window"). The two flows must agree.
func TestWatcherIgnoresATransferOlderThanItsBand(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, _ := newUSDTBot(t)
	w := installWatchFake(b)
	const chat = 4711

	b.Process(ctx, callbackUpdate(chat, "dep:trx"))
	b.Process(ctx, textUpdate(chat, "6"))
	o := reservedDeposit(t, st, chat)
	res, _ := b.bands.Get(bands.ProviderTron, o.Des)
	row, _ := b.getUSDTRow(bands.ProviderTron, o.Des)

	// One second older than the grace allows — the amount matches exactly, which
	// is the point: the amount is not enough on its own.
	stale := row.SinceMS - b.usdtVerifyGrace.Milliseconds() - 1000
	w.set(*trxTx(hash64("9"), res.Amount, stale))
	b.tickOnchain(ctx, bands.ProviderTron)

	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("credited %d VND for a transfer that predates the deposit — a recycled band amount "+
			"just paid this customer with someone else's money", bal)
	}
	if _, still := b.bands.Get(bands.ProviderTron, o.Des); !still {
		t.Error("the band was released by a refusal — a stale transfer must change nothing")
	}
	// A fresh transfer for the same band still settles, so the gate is a window
	// and not a blanket refusal.
	w.set(*trxTx(hash64("8"), res.Amount, b.now().UnixMilli()))
	b.tickOnchain(ctx, bands.ProviderTron)
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 150250 {
		t.Errorf("balance after a fresh transfer = %d VND, want 150250 — the freshness gate is refusing valid payments", bal)
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
