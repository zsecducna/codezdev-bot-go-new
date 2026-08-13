package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"time"

	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/bep20"
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
	// tickOnchain is the TRON rail's tick. BEP20 has its own (tickBep20); a
	// call here for any other rail is a no-op so a misrouted tick does nothing
	// rather than half-polling.
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
		// TRON carries its own block timestamp, so the freshness anchor is the
		// transfer's own ms (exact). BEP20 adapts a block number instead — see
		// the BEP20 branch added alongside this rail-agnostic settle path.
		b.settleWatchedTransfer(ctx, p, prm, watchedTransfer{
			id:    tx.ID,
			frm:   tx.Frm,
			units: tx.Units,
			recv:  new(big.Rat).SetFrac(tx.Value, pow10BigInt(tron.TokenDecimals)),
			ms:    tx.TS,
		})
	}
}

// tickBep20 is the BEP20 rail's hands-free watcher tick — the block-range twin
// of tickOnchain's TRON path. Kept separate from tickOnchain (rather than a
// branch inside it) so the proven TRON path is untouched, and because BEP20
// differs in two load-bearing ways:
//
//   - BSC reorgs. A wallet credit is irreversible, so a transfer is settled
//     only once (head - block) >= minConf blocks bury it. Shallower than that
//     it can still vanish in a reorg; it is left for the next tick (the head
//     advances, it buries, it settles) — the routine "payment landed, not safe
//     yet", not an error and not audited.
//   - An eth_getLogs row carries no timestamp. So the fetch bound is a FIXED
//     block window (a timestamp->block ESTIMATE is unsafe under variable block
//     times — it slides past promptly-paid transfers), and the freshness anchor
//     is the transfer's REAL block timestamp fetched via eth_getBlockByNumber —
//     never an estimate. (An earlier estimate was rejected in review: BSC's
//     ~1.5-3s block time makes a block->time estimate wrong in both directions.
//     Do NOT "optimize" the real fetch back into an estimate.)
func (b *Bot) tickBep20(ctx context.Context, p bands.Provider) {
	if b.bep20Incoming == nil || b.bep20Head == nil || b.bep20BlockTS == nil {
		return
	}
	// THE GATE. Same rule as TRON: no live deposit band -> no RPC call. A shop
	// with no open deposits must not poll a public BSC endpoint on a timer.
	live := b.bands.DepositRows(p)
	if len(live) == 0 {
		return
	}
	prm, ok := b.bands.Params(p)
	if !ok {
		log.Printf("usdt watch [%s]: no band params — skipping tick", p)
		return
	}
	head, herr := b.bep20Head()
	if herr != nil {
		// Transient (BSC RPC blip, rate limit). Hold every band, retry next tick.
		log.Printf("usdt watch [%s]: head block fetch failed: %v", p, herr)
		return
	}
	// FIXED trailing window [head-lookback, head]: a transfer mined at block B
	// stays in the window for `lookback` blocks — far longer than the minConf
	// (~12) it needs to become settleable — so it cannot be silently dropped the
	// way a timestamp-derived, block-time-estimated offset would drop it. The
	// lookback must cover the pending grace at the fastest plausible block time
	// and stay under the node's eth_getLogs range cap (see bep20WatchLookback).
	from := head - b.bep20WatchLookback
	if from < 0 {
		from = 0
	}
	transfers, err := b.bep20Incoming(from)
	if err != nil {
		// Includes eth_getLogs range-cap errors (lookback wider than the node
		// allows) — loud with head/lookback so an operator can lower
		// BEP20_WATCH_LOOKBACK_BLOCKS, because that failure looks identical to
		// "no transfers" in a silent log and would otherwise mean BEP20 deposits
		// never credit. The fixed window is re-read whole every tick (not a
		// cursor), so a transient blip re-reads next tick. Hold every band.
		log.Printf("usdt watch [%s]: fetch failed (fromBlock=%d head=%d lookback=%d): %v", p, from, head, b.bep20WatchLookback, err)
		return
	}
	for _, tx := range transfers {
		// CONFIRMATION DEPTH. Not deep enough -> skip silently; the head advances
		// ~once per block and the next tick retries.
		if head-tx.Block < int64(b.bep20MinConf) {
			continue
		}
		// Hoist the dedupe pre-filter ABOVE the timestamp fetch. An already-settled
		// transfer still inside the ~2h window would otherwise be re-timestamped
		// every tick (one eth_getBlockByNumber per old transfer per tick — RPC
		// amplification on a busy address). TxnExists is the same cheap pre-filter
		// settleWatchedTransfer applies; calling it here is idempotent and just
		// avoids the expensive read for the settled set. Fail-closed on a read
		// error (skip; next tick retries), matching settleWatchedTransfer.
		used, derr := b.store.TxnExists(ledgerPrefix(p) + ":" + tx.ID)
		if derr != nil || used {
			continue
		}
		// REAL block timestamp for the freshness anchor (not an estimate). One
		// eth_getBlockByNumber per still-UNSETTLED conf-passing transfer. A read
		// failure holds the transfer for the next tick rather than estimating — an
		// estimate under variable block times is the bug this replaced.
		ms, tsErr := b.bep20BlockTS(tx.Block)
		if tsErr != nil {
			log.Printf("usdt watch [%s]: block timestamp fetch failed for %s (block=%d): %v", p, tx.ID, tx.Block, tsErr)
			continue
		}
		b.settleWatchedTransfer(ctx, p, prm, watchedTransfer{
			id:    tx.ID,
			frm:   tx.Frm,
			units: tx.Units,
			recv:  new(big.Rat).SetFrac(tx.Wei, pow10BigInt(bep20.TokenDecimals)),
			ms:    ms,
		})
	}
}

// watchedTransfer is one incoming on-chain transfer normalized for the settle
// path, which is identical across rails. TRON and BEP20 differ only in how
// they produce one of these — the amount's raw units (sun at 6 dp vs wei at
// 18) and the freshness anchor's source (TRON's is the tx's own block timestamp
// in ms; BEP20's is the REAL block timestamp fetched via eth_getBlockByNumber,
// since an eth_getLogs row carries no timestamp). Everything the money path
// does below — dedupe, quarantine, attribution, per-band freshness, the credit
// — is rail-agnostic.
type watchedTransfer struct {
	id    string   // raw transaction id; the ledger-id suffix (ledgerPrefix(p)+":"+id)
	frm   string   // sender address
	units int64    // amount at the rail's AmountDecimals — the unit MatchIncoming keys on
	recv  *big.Rat // amount as a ratio of the rail's TokenDecimals, for display + rate
	ms    int64    // freshness anchor, epoch ms (TRON: tx ts; BEP20: real block timestamp)
}

// settleWatchedTransfer attributes ONE incoming transfer and settles it if
// names exactly one live deposit. Split out of the loop so every early return is
// "skip this transfer", never "abandon the tick" — one unattributable payment
// must not stop the next one in the same batch from being credited.
func (b *Bot) settleWatchedTransfer(ctx context.Context, p bands.Provider, prm bands.Params, wt watchedTransfer) {
	// The ledger id is the dedupe key and its shape is fixed: "trx:" + the
	// transaction id, identical to what the hash-paste path writes. The two
	// flows MUST agree here or a payment could be credited once by each.
	ledgerID := ledgerPrefix(p) + ":" + wt.id
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
	// Rendered here, above the refusals, because every one of them records the
	// amount in its audit row — an operator asking "what did the bot decide
	// about this transaction" needs the figure on the row itself, not a second
	// lookup. Same rendering as the hash-paste path: the RAIL's precision, so
	// two bands 0.00101 apart do not collapse onto one string.
	recv := wt.recv
	recvStr := recv.FloatString(prm.Decimals)

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
		// Audited as its OWN verdict, distinct from the sighting that created
		// the quarantine: an operator reading the trail has to be able to tell
		// "just became ambiguous" from "already known, skipped again". No chat
		// id — an ambiguous transfer has no single owner, which is the whole
		// reason it is here.
		b.auditWatch("", p, "quarantined", ledgerID, "skipped, already quarantined",
			"recv="+recvStr+" from="+wt.frm)
		return
	}
	res, kind, merr := b.bands.MatchIncoming(p, wt.units)
	if merr != nil {
		log.Printf("usdt watch [%s]: match failed for %s: %v", p, ledgerID, merr)
		return
	}
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
		outcome := "credited nothing, quarantined"
		detail := "recv=" + recvStr + " from=" + wt.frm + " units=" + fmtInt(wt.units)
		// The quarantine row is the durable gate against the attribution-by-
		// attrition this branch exists to prevent (see the comment above it). A
		// TRANSIENT write failure leaves the transfer unprotected, and the next
		// tick is exactly when the damage happens — so the write is retried a few
		// times with a short backoff before we give up and fall through to the
		// loud "protection did not take" record below.
		//
		// Bounded, not unbounded, and the bound is the point: under a PERSISTENT
		// outage store.Credit cannot succeed either, so no credit lands while the
		// DB is down. The only residual corner is "DB down for the whole ~2h a
		// competing band takes to be swept, then recovers" — narrow enough to
		// name here rather than hide, and wider than the per-tick re-fetch's 20s
		// backstop can always cover under load.
		var qwerr error
		for attempt := 0; attempt < 3; attempt++ {
			qwerr = b.store.MarkAmbiguousTxn(ledgerID, string(p), recvStr,
				float64(b.now().UnixNano())/1e9)
			if qwerr == nil {
				break
			}
			if attempt < 2 {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if qwerr != nil {
			// Nothing was credited either way, so the money is not at risk right
			// now — but the protection did not take, and a later tick may
			// auto-credit this once the bands change. Loud, because that is a
			// silent wrong-customer credit waiting to happen and the retry is
			// simply the next tick.
			log.Printf("usdt watch [%s]: QUARANTINE WRITE FAILED for %s: %v — this transfer is NOT protected and may be auto-credited later",
				p, ledgerID, qwerr)
			// And the durable record must say so too. The whole point of auditing
			// this path is that the DB answers what the bot decided AND whether
			// the decision stuck; a row reading "quarantined" after the
			// quarantine write failed asserts a protection that is not there, so
			// the one case actually needing a human looks exactly like the
			// routine one. Distinct wording, and the cause in the detail, so an
			// operator querying the trail sees it without cross-reading logs.
			//
			// Note what this row can and cannot promise: it never LIES, but it
			// can be MISSING. If the quarantine write failed because the database
			// itself is unavailable, the audit insert is going to the SAME
			// database and LogAction is best-effort — it swallows its own error.
			// The log line above is the backstop for precisely that case, which
			// is why it stays rather than being replaced by the row.
			outcome = "credited nothing, QUARANTINE WRITE FAILED"
			detail += " quarantine_err=" + qwerr.Error()
		}
		log.Printf("usdt watch [%s]: AMBIGUOUS %s from=%s recv=%s (%d units) — two or more live deposits match, credited nothing, quarantined for manual attribution",
			p, ledgerID, wt.frm, recvStr, wt.units)
		// The quarantine row and this audit row are different things and both
		// are needed: the quarantine is the durable GATE that changes what
		// later ticks do, this is the durable RECORD of what happened and when.
		// Same action either way, so one query by action still returns every
		// ambiguity; only the outcome distinguishes the failed write.
		// No chat id — no single customer owns an ambiguous payment.
		b.auditWatch("", p, "ambiguous", ledgerID, outcome, detail)
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
		b.auditWatch(res.ChatID, p, "no_start", ledgerID, "refused, band has no start time",
			"recv="+recvStr+" from="+wt.frm+" des="+res.Des)
		return
	}
	if wt.ms < lo-b.usdtVerifyGrace.Milliseconds() {
		log.Printf("usdt watch [%s]: %s predates deposit %s (tx_ts=%d lo=%d) — not attributing",
			p, ledgerID, res.Des, wt.ms, lo)
		b.auditWatch(res.ChatID, p, "stale", ledgerID, "refused, predates the deposit it matched",
			fmt.Sprintf("recv=%s from=%s des=%s tx_ts=%d lo=%d", recvStr, wt.frm, res.Des, wt.ms, lo))
		return
	}

	o, gerr := b.store.GetOrder(res.Des)
	if gerr != nil {
		// A DB blip, not a decision — the next tick asks again. Deliberately NOT
		// audited: the audit row would itself need the DB that just failed, and
		// "we could not look" is not a verdict about the transfer.
		log.Printf("usdt watch [%s]: get_order failed for %s: %v", p, res.Des, gerr)
		return
	}
	if o == nil {
		// The band outlived its order (vanished row). Sweep will clear it; there
		// is nothing to credit against in the meantime.
		log.Printf("usdt watch [%s]: order %s gone, leaving %s unattributed", p, res.Des, ledgerID)
		b.auditWatch(res.ChatID, p, "order_gone", ledgerID, "refused, matched order no longer exists",
			"recv="+recvStr+" from="+wt.frm+" des="+res.Des)
		return
	}
	// Second lock on the deposit-only door. MatchIncoming already filtered to
	// deposit bands, so reaching here with a product order means the flag and
	// the order table disagree — refuse rather than wallet-credit an order
	// whose stock was never delivered.
	if o.Product != depositProduct {
		log.Printf("usdt watch [%s]: BUG — matched deposit band %s is a %q order, refusing to credit %s",
			p, res.Des, o.Product, ledgerID)
		b.auditWatch(o.ChatID, p, "not_deposit", ledgerID, "refused, matched band is a product order",
			"recv="+recvStr+" from="+wt.frm+" des="+res.Des+" product="+o.Product)
		return
	}
	// settleUSDTMatch keys the wallet credit on fmtInt(chatID), so an
	// unparseable owner would credit chat "0" — a stranger's balance. Fail
	// closed; the customer can still settle by pasting the hash.
	chatID, cerr := strconv.ParseInt(o.ChatID, 10, 64)
	if cerr != nil {
		log.Printf("usdt watch [%s]: order %s has unparseable chat id %q: %v", p, res.Des, o.ChatID, cerr)
		// The order's own chat id is the thing that is broken, so it is not used
		// as the audit row's actor — the row would then be filed under the same
		// unusable value it is reporting.
		b.auditWatch("", p, "bad_chat", ledgerID, "refused, order owner is unparseable",
			"recv="+recvStr+" from="+wt.frm+" des="+res.Des+" chat_id="+o.ChatID)
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
		b.auditWatch(o.ChatID, p, "no_rate", ledgerID, "held, cannot value the payment",
			"recv="+recvStr+" from="+wt.frm+" des="+res.Des)
		return
	}
	// res.Amount IS the atomic snapshot MatchIncoming took — a second locked
	// read here could disagree with the amount that actually matched.
	if !b.settleUSDTMatch(ctx, p, res.Des, ledgerID, wt.units, recvVND, o, langSafe(o), chatID,
		prm, res.Amount, wt.frm, recvStr) {
		// Nothing moved (bad stored amount, or the credit call errored). The
		// band and the persisted row are untouched, so the next tick retries.
		log.Printf("usdt watch [%s]: settle did not credit %s for %s — retrying next tick", p, ledgerID, res.Des)
		b.auditWatch(o.ChatID, p, "uncredited", ledgerID, "attributed but not credited, retrying",
			"recv="+recvStr+" from="+wt.frm+" des="+res.Des)
	}
	// The SUCCESS row is not written here: settleUSDTMatch already writes
	// "usdt_<chain>_deposit" against this same ledger id, so a query by ledger
	// id shows the whole history of the transaction — every refusal above and
	// the credit — without this function double-recording the one outcome that
	// was already covered.
}

// auditWatch records ONE watcher decision about ONE transfer in the queryable
// audit table.
//
// WHY EVERY REFUSAL AND NOT JUST THE CREDITS. settleUSDTMatch already audits a
// successful credit against the ledger id, so "which transactions were
// credited" was answerable. "Which transactions did the bot see and decide NOT
// to credit, and why" was not — it lived only in log lines, which is the wrong
// place for the history of a money path. `target` is the ledger id here for
// exactly the same reason the settle path uses it: one query by transaction now
// returns the FULL sequence of decisions made about it, refusals and credit
// alike, in order.
//
// NOT audited, deliberately:
//
//   - MatchNone. Most transfers to a shop address are not deposits anyone is
//     waiting on, and a row per non-match would bury the decisions that matter
//     under noise. A transfer earns a row only once it is ABOUT a live deposit
//     band — or is one a human already owns, which is the quarantine skip
//     above: that gate runs before the matcher on purpose, so its row is
//     written whether or not a band still matches the amount.
//   - Read failures (the ledger, the quarantine, GetOrder). "We could not look"
//     is not a verdict about the transfer, and the row would need the very
//     database that just failed.
//
// REPEATS ARE BOUNDED, which is what makes per-tick refusals affordable to
// record: the chain query starts at the OLDEST live deposit's own start, so once
// the bands older than a refused transfer are swept, that transfer falls out of
// the window and is never fetched again. A refusal repeats for hours at worst,
// not forever, and the audit-retention janitor prunes what is left. Bounded is
// not free, though: at a 20s tick and a 2h pending grace one refused transfer
// can write a few hundred identical rows, which is more than the admin audit
// screen's unfiltered tail shows at once. Querying the table by action or
// target is unaffected; reading the last N rows by eye is. Collapsing a repeat
// into its first sighting is the fix if that becomes a nuisance in practice.
//
// chatID is the actor: the matched band's owner when the decision is about a
// known deposit, and empty when nobody owns the transfer yet — an ambiguous
// payment has no single owner by definition, which is the whole reason it was
// refused.
func (b *Bot) auditWatch(chatID string, p bands.Provider, verdict, ledgerID, outcome, detail string) {
	b.audit(chatID, "usdt_"+ledgerPrefix(p)+"_watch_"+verdict, ledgerID, outcome, detail)
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
