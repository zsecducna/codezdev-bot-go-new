# Hands-free deposit crediting

Date: 2026-08-11
Status: design approved, not implemented
Related: task #47. Sequencing dependency on #44 (S5 ledger flip).

## Problem

A customer who wants to top up their wallet today must: open the deposit
screen, type an amount, send USDT from their wallet, then find and paste the
transaction hash back into the bot. The last step is the problem. It is the
only step that requires the customer to go looking for something, and it is
the step that comes after their money has already left.

Measured on a clone of production: **426 deposit orders, 222 delivered, 204
expired.** Nearly half never complete.

One thing that number does not tell us, and this matters for sequencing: an
expired order can mean "opened the screen and never paid" or "paid and never
claimed". Those have different cures. Only the second is fixed by
auto-detection. Separating them needs a chain scan nobody has run.

The deposit window is 600 seconds (`dispatch.go:301`). Ten minutes to open the
screen, switch apps, send, and confirm.

## What already exists

Most of this feature is built and unwired:

- `bands.UniqueAmount` (bands.go:217) picks an amount at least `2*tol+1` away
  from every live band, so a received amount is within tolerance of at most
  one order.
- `bands.RecvMatchesBand` / `RecvMatchesAnyBand` are the match and claim-jack
  gates.
- `tron.MatchPayment` / `bep20.MatchPaymentConf` scan a transfer list against
  an expected amount with an `isUsed` dedupe callback. Both tested. **Neither
  has a non-test caller.**
- Bands persist and rehydrate at boot (`adoptPersisted`, `StartupRecovery`).
- `store.Credit` makes crediting idempotent by `processed.txn_id` PRIMARY KEY.

The Python original only ever wired `match_payment` for Binance Pay, which
stages S1-S3 removed. TRON and BEP20 never had a watcher there either, so the
Go port dropped nothing.

The actual gap: `startOnchainDeposit` calls `adoptUSDTAmount`, which records
the customer's round number with **no unique offset**. `bands/credit.go:5-9`
states this deliberately — a deposit credits the submitter's own wallet, so it
was given no offset and no claim-jack gate, and the hash was left doing the
attribution work.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Address model | One shared address | Per-customer derived addresses are better UX but mean holding a seed and sweeping funds. Not worth it at this volume. |
| Attribution | Picked amount + watcher | Customer sends `120.07`. Viable because customers pay from personal wallets, where the exact amount arrives. |
| Customer hash entry | Removed entirely | It is the step being deleted. |
| Unmatched payments | `notifyOwners` alert | An admin screen for a rare event is speculative. Reuses #34's alert channel and #31's audited wallet-adjust. Build the screen when alerts get noisy. |
| Window | 600s -> 3600s | Six times today's window; band collisions stay impossible at this volume. |
| Watcher shape | One goroutine per chain | See below. |

### Watcher shape

Rejected: **per-deposit goroutine** (mirroring `pollUSDTConfirm`). RPC cost
scales with concurrent deposits, each poller sees only its own band so
unmatched transfers are invisible to all of them, and it dies on restart — a
deploy mid-deposit strands a payment. Disqualifying for a money path on a
codebase that deploys often.

Rejected: **folding the scan into the existing expiry tick**. Least new
machinery, but it couples deposit latency to a housekeeping interval and mixes
two unrelated jobs in one loop.

Chosen: **one global watcher per chain**. Flat RPC cost, sees every transfer
including unattributable ones, and restart recovery falls out of the existing
band rehydration for free.

## Design

Four stages, in shipping order. Stage 1 ships alone.

### Stage 1 — window

`dispatch.go:301`, `600 * time.Second` -> `3600`. Everything downstream already
reads it: the deposit deadline, `expirySweepOnce`'s cutoff, the band sweep
grace.

Ship this first and measure. If the expiries were mostly people who opened the
screen and wandered off, this is the whole cure and stages 2-4 buy nothing.

### Stage 2 — picked deposit amount

`startOnchainDeposit` (usdt.go:408) swaps `adoptUSDTAmount` for the registry's
picker, the same path orders already use. The customer is asked for `120.07`
rather than `120.00`. `DepositCredit`'s under/overpay tolerance still applies
to whatever lands.

### Stage 3 — the watcher

One goroutine per chain, started in `main.go` beside `StartupRecovery`. Each
tick:

1. No live bands for the chain -> return. No RPC when nobody is depositing.
2. One `GetIncomingTransfers` for the address, since the oldest live band's
   `CreatedAt`.
3. For each live reservation, `MatchPayment(transfers, res.Amount, sinceMS,
   isUsed)` with `isUsed` = `store.TxnExists`.
4. On a hit, call the existing settle path — the one `verifyUSDTTxid` already
   uses. Crediting, idempotency, receipt and band release are unchanged code.
5. Transfers matching no band: `notifyOwners` with amount, sender, hash and
   time, deduped through `MarkTxn` so a restart cannot re-alert.

### Stage 4 — delete

The customer-facing hash prompt, its capture, and `pollUSDTConfirm`.
`verifyUSDTTxid`'s settle core stays — the watcher calls it. The by-hash
lookup and the per-chat poll goroutine go.

## Failure modes

- **RPC failure**: log, retry next tick. Never frees a band, matching the rule
  already enforced in `sweepUSDTBands` ("a read failure must NEVER free a
  band") — releasing a live amount could hand it to another order and let one
  payment settle the wrong one.
- **Panic in a tick**: per-tick guard, same shape as `verifyUSDTTxidSafe`. One
  bad cycle must not end the watcher or take the bot down.
- **Double credit**: impossible via `processed.txn_id`. The watcher introduces
  no new crediting path.
- **Ambiguous attribution**: impossible by construction. `MinGap` = `2*tol+1`
  means a transfer is within tolerance of at most one live band.
- **Late payment** (arrives after its band is swept): matches nothing, becomes
  an owner alert. This is the designed exit, not a bug.
- **Wrong amount sent**: same path. Owner alert, credited by hand.

## Sequencing

Bands hold USDT-denominated strings (`usdtStr`), so the watcher is largely
insulated from the S5-S8 ledger flip. Largely, not certainly. This wants an
explicit check against #44 before it lands, not after.

## Tests

Five:

1. A transfer inside a band credits exactly once.
2. A second tick over the same transfer credits nothing (`TxnExists` guard).
3. A transfer matching no band fires exactly one owner alert, and a simulated
   restart does not re-fire it.
4. Zero live bands produces zero RPC calls.
5. A rehydrated band (simulated restart) still matches on the next tick.

Test 3 covers the most ground — dedupe and alerting together. The rails
already have transfer fakes in `usdt_test.go` to build on.

## Out of scope

- Per-customer deposit addresses.
- An admin unmatched-payments screen.
- Any change to the purchase (non-deposit) payment flow.
