package handlers

import (
	"context"
	"errors"
	"log"
	"math/big"
	"strconv"

	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/tron"
)

// ---- the hands-free on-chain deposit watcher ---------------------------------
//
// WHAT THIS REPLACES. Until now a USDT top-up was only credited when the
// customer pasted their transaction hash on their own deposit screen
// (verifyUSDTTxid). That works, but it puts the last and most error-prone step
// of a payment on the customer: they must come back to Telegram, find the right
// screen, and paste a 64-character string. The ones who do not are the support
// queue.
//
// The watcher removes that step for the common case. It reads the seller
// address's own incoming transfers and attributes each one by AMOUNT ALONE,
// which is possible because every deposit is quoted a unique amount (bands: the
// 0.01000-0.01999 jitter). The customer sends the amount they were shown and
// their balance moves on its own.
//
// WHAT IT DELIBERATELY DOES NOT DO:
//
//   - It never settles a PRODUCT ORDER. Those are still hash-paste, they are
//     paid through cbPayOnchain, and settleUSDTMatch is deposit-only logic
//     (it wallet-credits). bands.MatchIncoming enforces this at the source via
//     Reservation.IsDeposit; the extra product check below is a second lock on
//     the same door, because the cost of it being wrong is money out for goods
//     never delivered.
//   - It never guesses, and never un-guesses later. An amount two live deposits
//     could both claim is logged, left uncredited, and QUARANTINED durably, so
//     no later tick can credit it once the competition thins out. See the
//     MatchAmbiguous branch and the ambiguous_txns table.
//   - It never frees a band. Every failure here (fetch, DB read, rate) logs and
//     returns, so the next tick retries; releasing a live amount on a read error
//     could hand it to another order and let one payment settle the wrong one.
//     Same rule sweepUSDTBands already follows.
//
// It does NOT replace the dedupe either. store.Credit's processed.txn_id PK,
// reached through settleUSDTMatch, is still the only thing that makes crediting
// idempotent; the TxnExists pre-filter below is an optimisation, not a gate.

// tickOnchain is ONE watcher pass for a rail.
//
// Shape note, because the obvious alternative is wrong: this loops over
// TRANSFERS and asks the registry about each, not over bands looking for a
// transfer. Attribution needs the WHOLE live candidate set read atomically to
// tell "this names one deposit" from "this could be either of two", and
// MatchIncoming does that under one lock — so one transfer is the natural unit
// of work, and one chain fetch serves every open deposit on the rail.
func (b *Bot) tickOnchain(ctx context.Context, p bands.Provider) {
	// TRON is the only rail with a watcher today. BEP20 keeps its hash-paste
	// flow (it has no equivalent fetch seam wired), so a call for it is a
	// no-op rather than a half-implemented poll.
	if p != bands.ProviderTron || b.tronTransfersSince == nil {
		return
	}
	// THE GATE. No live deposit band means nobody is mid-payment, so there is
	// nothing an incoming transfer could be attributed to — and a shop with no
	// open deposits must not hammer a public RPC endpoint on a timer. The
	// candidate set comes from DepositRows precisely so this gate and
	// MatchIncoming below cannot disagree about what counts (see its doc).
	live := b.bands.DepositRows(p)
	if len(live) == 0 {
		return
	}
	prm, ok := b.bands.Params(p)
	if !ok {
		log.Printf("usdt watch [%s]: no band params — skipping tick", p)
		return
	}
	transfers, err := b.tronTransfersSince(b.watchSinceMS(p, live))
	// A TRUNCATED window is partial, not failed: the rows that did come back are
	// real payments and settling them is strictly better than dropping the whole
	// tick, and the tail is re-read next tick anyway since the window is
	// anchored on the oldest open deposit rather than a cursor. The rail already
	// logged the truncation; do not log it twice.
	if err != nil && !errors.Is(err, tron.ErrWindowTruncated) {
		// Transient by assumption: TronGrid rate limits, a network blip, a bad
		// gateway. Retry next tick, hold every band.
		log.Printf("usdt watch [%s]: fetch failed: %v", p, err)
		return
	}
	for _, tx := range transfers {
		b.settleWatchedTransfer(ctx, p, prm, tx)
	}
}

// settleWatchedTransfer attributes ONE incoming transfer and settles it if it
// names exactly one live deposit. Split out of the loop so every early return is
// "skip this transfer", never "abandon the tick" — one unattributable payment
// must not stop the next one in the same batch from being credited.
func (b *Bot) settleWatchedTransfer(ctx context.Context, p bands.Provider, prm bands.Params, tx tron.Transfer) {
	// The ledger id is the dedupe key and its shape is fixed: "trx:" + the
	// transaction id, identical to what the hash-paste path writes. The two
	// flows MUST agree here or a payment could be credited once by each.
	ledgerID := ledgerPrefix(p) + ":" + tx.ID
	// Cheap pre-filter, NOT the dedupe. Most transfers on a busy address are
	// ones we already settled, and this skips the order read and the rate
	// conversion for them. An unreadable ledger counts as USED: skipping only
	// delays a credit to the next tick, whereas proceeding on a failed dedupe
	// read risks paying twice.
	used, terr := b.store.TxnExists(ledgerID)
	if terr != nil {
		log.Printf("usdt watch [%s]: txn_exists failed for %s: %v", p, ledgerID, terr)
		return
	}
	if used {
		return
	}
	// QUARANTINE, checked BEFORE the matcher so a transfer a human already owns
	// can never be re-attributed by a later tick's changed band set. This is the
	// durable half of the ambiguity refusal — see the ambiguous_txns table.
	// A read failure skips, same fail-closed direction as the ledger read above.
	held, qerr := b.store.AmbiguousTxnExists(ledgerID)
	if qerr != nil {
		log.Printf("usdt watch [%s]: quarantine read failed for %s: %v", p, ledgerID, qerr)
		return
	}
	if held {
		return
	}
	res, kind, merr := b.bands.MatchIncoming(p, tx.Units)
	if merr != nil {
		log.Printf("usdt watch [%s]: match failed for %s: %v", p, ledgerID, merr)
		return
	}
	recv := new(big.Rat).SetFrac(tx.Value, pow10BigInt(tron.TokenDecimals))
	recvStr := recv.FloatString(prm.Decimals)
	switch kind {
	case bands.MatchExact, bands.MatchShortfall:
		// named below
	case bands.MatchAmbiguous:
		// TWO OR MORE live deposits could claim this amount, so crediting
		// either one is a coin flip with someone else's money. Log everything a
		// human needs to attribute it by hand — which chain, which transaction,
		// who sent it, how much — and leave the payment alone. Nothing alerts
		// on this line yet; raising it to the owners is a later task, and this
		// log is the record that task will read.
		//
		// THE REFUSAL IS PERMANENT, and it has to be. Left to per-tick logic it
		// would not be: the transfer is deliberately left UNCONSUMED in the
		// processed ledger so a human can still settle it, which also means a
		// later tick re-examines it — and once the competing band settles or is
		// swept the very same transfer looks unambiguous and would be credited
		// to whichever deposit happened to survive. That is attribution by
		// attrition rather than by evidence, and it is exactly as wrong as
		// guessing was in the first place; waiting does not produce the missing
		// information.
		//
		// So the sighting is recorded durably (ambiguous_txns) and checked
		// before the matcher on every later tick. Durable rather than in-memory
		// because this project deploys often, and a process-local memory would
		// only shrink the window to "until the next restart".
		if qwerr := b.store.MarkAmbiguousTxn(ledgerID, string(p), recvStr,
			float64(b.now().UnixNano())/1e9); qwerr != nil {
			// Nothing was credited either way, so the money is not at risk right
			// now — but the protection did not take, and a later tick may
			// auto-credit this once the bands change. Loud, because that is a
			// silent wrong-customer credit waiting to happen and the retry is
			// simply the next tick.
			log.Printf("usdt watch [%s]: QUARANTINE WRITE FAILED for %s: %v — this transfer is NOT protected and may be auto-credited later",
				p, ledgerID, qwerr)
		}
		log.Printf("usdt watch [%s]: AMBIGUOUS %s from=%s recv=%s (%d units) — two or more live deposits match, credited nothing, quarantined for manual attribution",
			p, ledgerID, tx.Frm, recvStr, tx.Units)
		return
	default: // bands.MatchNone
		// Routine and not an error: not every transfer to the shop's address is
		// a deposit someone is waiting on.
		return
	}

	// FRESHNESS, per matched band — not just per rail.
	//
	// The fetch bound above is the OLDEST live deposit's start, which is far too
	// loose to attribute by on its own: band amounts are recycled as sessions
	// finish, so a transfer that was never credited (it arrived when nothing
	// matched, or while its amount was ambiguous) can sit in that window and
	// then land exactly on a NEW deposit quoted the same amount. Crediting it
	// would pay one customer with another customer's money.
	//
	// The hash-paste path has always applied this check per order
	// (verifyUSDTTxid's "outside_window" reject); the watcher must agree with it
	// or the two flows disagree about the same transfer. There is deliberately
	// no UPPER bound here — the watcher has no "I have paid" tap to bound
	// against, and a payment arriving after its session started is exactly what
	// is expected.
	lo := int64(0)
	if row, ok := b.getUSDTRow(p, res.Des); ok {
		lo = row.SinceMS
	}
	if lo <= 0 && !res.CreatedAt.IsZero() {
		lo = res.CreatedAt.UnixMilli()
	}
	if lo <= 0 {
		// Unreachable in practice — Reserve stamps CreatedAt and Adopt backfills
		// it — but this is a money gate, so an unanchorable band refuses rather
		// than accepts. The hash-paste path can still settle it.
		log.Printf("usdt watch [%s]: deposit %s has no usable start time, refusing to attribute %s", p, res.Des, ledgerID)
		return
	}
	if tx.TS < lo-b.usdtVerifyGrace.Milliseconds() {
		log.Printf("usdt watch [%s]: %s predates deposit %s (tx_ts=%d lo=%d) — not attributing",
			p, ledgerID, res.Des, tx.TS, lo)
		return
	}

	o, gerr := b.store.GetOrder(res.Des)
	if gerr != nil {
		log.Printf("usdt watch [%s]: get_order failed for %s: %v", p, res.Des, gerr)
		return
	}
	if o == nil {
		// The band outlived its order (vanished row). Sweep will clear it; there
		// is nothing to credit against in the meantime.
		log.Printf("usdt watch [%s]: order %s gone, leaving %s unattributed", p, res.Des, ledgerID)
		return
	}
	// Second lock on the deposit-only door. MatchIncoming already filtered to
	// deposit bands, so reaching here with a product order means the flag and
	// the order table disagree — refuse rather than wallet-credit an order
	// whose stock was never delivered.
	if o.Product != depositProduct {
		log.Printf("usdt watch [%s]: BUG — matched deposit band %s is a %q order, refusing to credit %s",
			p, res.Des, o.Product, ledgerID)
		return
	}
	// settleUSDTMatch keys the wallet credit on fmtInt(chatID), so an
	// unparseable owner would credit chat "0" — a stranger's balance. Fail
	// closed; the customer can still settle by pasting the hash.
	chatID, cerr := strconv.ParseInt(o.ChatID, 10, 64)
	if cerr != nil {
		log.Printf("usdt watch [%s]: order %s has unparseable chat id %q: %v", p, res.Des, o.ChatID, cerr)
		return
	}
	// Deliberately NO status check. A payment that lands just after its session
	// expired still credits the customer's wallet, inside the pending grace —
	// that is what the grace is for, and it is exactly what the hash-paste path
	// does with the same late payment.
	recvVND, verr := b.usdtToVNDFor(p, recv)
	if verr != nil {
		// No usable rate means we cannot value the credit. Crediting 0 would
		// lose the customer's money outright, so hold and retry once the rate
		// is fixed (same call the hash-paste path makes, same reasoning).
		log.Printf("usdt watch [%s]: rate error for %s: %v", p, res.Des, verr)
		return
	}
	// res.Amount IS the atomic snapshot MatchIncoming took — a second locked
	// read here could disagree with the amount that actually matched.
	if !b.settleUSDTMatch(ctx, p, res.Des, ledgerID, tx.Units, recvVND, o, langSafe(o), chatID,
		prm, res.Amount, tx.Frm, recvStr) {
		// Nothing moved (bad stored amount, or the credit call errored). The
		// band and the persisted row are untouched, so the next tick retries.
		log.Printf("usdt watch [%s]: settle did not credit %s for %s — retrying next tick", p, ledgerID, res.Des)
	}
}

// watchSinceMS is the lower bound of the chain query: the start of the OLDEST
// live deposit on the rail, minus the verify grace.
//
// Anchored on the oldest rather than "since last tick" ON PURPOSE. A watcher
// that remembered a cursor would silently lose every payment made while it was
// down or slow; anchoring on the oldest open deposit means a restart re-reads
// whatever is still outstanding and credits it, which is the property the
// per-deposit-goroutine design lacked.
//
// The grace subtraction mirrors verifyUSDTTxid's own window lower bound
// (o.TS - grace): block timestamps and our clock are not the same clock, and a
// query bound one second too tight silently drops a matchable payment.
func (b *Bot) watchSinceMS(p bands.Provider, live []bands.Reservation) int64 {
	oldest := int64(0)
	for _, res := range live {
		// SinceMS is the rail-side row's own start stamp (survives a restart via
		// the persisted pending row); CreatedAt is the band's, and is the
		// fallback for a row that predates the stamp or lost it.
		ms := int64(0)
		if row, ok := b.getUSDTRow(p, res.Des); ok {
			ms = row.SinceMS
		}
		if ms <= 0 && !res.CreatedAt.IsZero() {
			ms = res.CreatedAt.UnixMilli()
		}
		if ms <= 0 {
			continue // no usable anchor for this row; another row still bounds the query
		}
		if oldest == 0 || ms < oldest {
			oldest = ms
		}
	}
	if oldest == 0 {
		// Unreachable while Reserve stamps CreatedAt and Adopt backfills it, but
		// NOT harmlessly so: the chain query is oldest-first with a page limit,
		// so a zero bound would return the address's oldest transfers ever and
		// push everything recent off the end. Fall back to the furthest back a
		// live row can legitimately be — Sweep measures its grace from
		// CreatedAt, so nothing survives past that.
		oldest = b.now().Add(-b.usdtPendingGrace).UnixMilli()
	}
	return max(oldest-b.usdtVerifyGrace.Milliseconds(), 0)
}
