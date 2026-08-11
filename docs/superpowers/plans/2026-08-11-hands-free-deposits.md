# Hands-Free Deposit Crediting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Credit USDT deposits automatically by matching the amount that arrives on-chain, so the customer never pastes a transaction hash.

**Architecture:** Deposits switch from an adopted round amount to the unique picked amount orders already use, so a received amount identifies exactly one order. One watcher goroutine per chain scans incoming transfers and settles matches through the existing credit path. The customer-facing hash prompt is deleted.

**Tech Stack:** Go, stdlib only. SQLite via `modernc.org/sqlite`. No new dependencies.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-11-hands-free-deposits-design.md`.
- Deposit crediting stays idempotent by `processed.txn_id` PRIMARY KEY, enforced at the storage layer, never in application code (repo CLAUDE.md invariant).
- Ledger IDs are `"trx:<hash>"` and `"bsc:<hash>"` — the exact strings `MatchPayment`'s `isUsed` callback already builds (tron.go:425, bep20.go:486).
- Executive comments before the function or line being written or edited. The repo has a heavy-comment style; keep it.
- `gofmt -l .` empty, `go build ./...`, `go vet ./...`, `go test ./...` all clean before every commit.
- No new goroutine may outlive shutdown: use `b.workers.Go(func(stop <-chan struct{}))`, the pattern `StartupRecovery` uses (usdt.go:1265).
- A read failure must NEVER free a band. Releasing a live amount could hand it to another order and let one payment settle the wrong one (the rule already enforced in `sweepUSDTBands`, usdt.go:1412-1416).

---

### Task 1: Extend the deposit window

**Files:**
- Modify: `internal/handlers/dispatch.go:301`
- Test: `internal/handlers/deposit_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `b.expiryTimeout` is 3600s. Tasks 2-7 assume nothing about its value.

This ships alone and may be the entire fix. Do not batch it with Task 2.

- [ ] **Step 1: Write the failing test**

```go
// A deposit must stay claimable long enough for a customer to leave Telegram,
// open a wallet app, and send — ten minutes was not enough (204 of 426 deposit
// orders expired). Pinned as a constant expectation so a future edit to the
// default is a deliberate, visible change.
func TestDepositWindowIsOneHour(t *testing.T) {
	b := newTestBot(t)
	if got, want := b.expiryTimeout, time.Hour; got != want {
		t.Fatalf("expiryTimeout = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/handlers/ -run TestDepositWindowIsOneHour -v`
Expected: FAIL with `expiryTimeout = 10m0s, want 1h0m0s`

- [ ] **Step 3: Change the constant**

In `internal/handlers/dispatch.go:301`, replace:

```go
	b.expiryTimeout = 600 * time.Second
```

with:

```go
	// One hour, not the original ten minutes: a deposit has to survive the
	// customer leaving Telegram, finding their wallet app and sending. At
	// roughly one deposit per thirty minutes the wider window costs nothing —
	// the only resource a live band consumes is a slot in the amount picker,
	// and MinGap spacing keeps collisions impossible at this volume.
	b.expiryTimeout = 3600 * time.Second
```

- [ ] **Step 4: Run the full handlers suite**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS. Any test that hard-codes a ten-minute expectation is a real finding — fix the test, not the constant, and say so in the commit.

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/dispatch.go internal/handlers/deposit_test.go
git commit -m "Give a deposit an hour instead of ten minutes"
```

---

### Task 2: Deposits get a picked amount

**Files:**
- Modify: `internal/handlers/usdt.go:408-441` (`startOnchainDeposit`)
- Test: `internal/handlers/usdt_test.go`

**Interfaces:**
- Consumes: `reserveUSDTAmount(p bands.Provider, baseUSDT, chatID, des string, total, sinceMS int64, deadline time.Time) (string, error)` — already exists at usdt.go:326. Returns the picked amount, or `bands.ErrBandExhausted`.
- Produces: a deposit's band amount is now unique, so a received amount identifies exactly one order. Tasks 4-6 depend on this.

`adoptUSDTAmount` (usdt.go:369) stays — it has no other caller after this change, but deleting it belongs in Task 7 with the rest of the deletions, so this task stays a pure behaviour swap.

- [ ] **Step 1: Write the failing test**

```go
// Two deposits of the SAME round amount must be handed different band amounts.
// Without this the watcher cannot tell whose payment is whose, and amount-based
// attribution is unsound.
func TestDepositAmountsAreUnique(t *testing.T) {
	b, f := newUSDTTestBot(t)
	b.startOnchainDeposit(context.Background(), 111, i18n.EN, 3000000, chainTRX)
	b.startOnchainDeposit(context.Background(), 222, i18n.EN, 3000000, chainTRX)

	amounts := usdtScreenAmounts(t, f) // amounts rendered on the two deposit screens
	if len(amounts) != 2 {
		t.Fatalf("want 2 deposit screens, got %d", len(amounts))
	}
	if amounts[0] == amounts[1] {
		t.Fatalf("both deposits were quoted %q — bands are not unique", amounts[0])
	}
}
```

`usdtScreenAmounts` is a helper this task adds to the test file: it walks `f.Sends`, keeps messages whose text contains the deposit-screen amount marker, and extracts the amount. Follow whatever extraction the existing USDT screen tests already use rather than inventing a second parser.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/handlers/ -run TestDepositAmountsAreUnique -v`
Expected: FAIL — both screens quote the same amount, because `adoptUSDTAmount` stores the round number verbatim.

- [ ] **Step 3: Swap the call**

In `startOnchainDeposit`, replace the `adoptUSDTAmount` block (usdt.go:434-439):

```go
	if err := b.adoptUSDTAmount(p, amount, fmtInt(chatID), des, vnd, now.UnixMilli(), deadline); err != nil {
		log.Printf("%s deposit band adopt failed: %v", chain, err)
		b.abandonUSDTStub(des)
		b.send(ctx, chatID, noID.T(lang), nil)
		return
	}
	b.sendUSDTScreen(ctx, chatID, lang, chain, des, amount)
```

with:

```go
	// Deposits now take a PICKED amount, like orders, instead of adopting the
	// customer's round number. The watcher attributes a payment by amount
	// alone, so two customers topping up 120.00 at once would be
	// indistinguishable; Reserve's MinGap (2*tol+1) spacing is what makes the
	// match unambiguous. The customer is asked for "120.07" rather than
	// "120.00" — DepositCredit's tolerance still governs what they are
	// credited if the amount that lands differs slightly.
	picked, err := b.reserveUSDTAmount(p, amount, fmtInt(chatID), des, vnd, now.UnixMilli(), deadline)
	if err != nil {
		log.Printf("%s deposit band reserve failed: %v", chain, err)
		b.abandonUSDTStub(des)
		// Band exhaustion is "busy", not "unconfigured" — sendBandError picks
		// the right refusal (usdt.go:453).
		b.sendBandError(ctx, chatID, lang, err, noID)
		return
	}
	b.sendUSDTScreen(ctx, chatID, lang, chain, des, picked)
```

Check `sendBandError`'s real signature at usdt.go:453 before writing this; match it exactly rather than the shape above if they differ.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS, including the new test.

- [ ] **Step 5: Verify the mutation**

Revert the `reserveUSDTAmount` call to `adoptUSDTAmount` in a scratch copy, confirm `TestDepositAmountsAreUnique` fails on its own message, restore.

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/usdt.go internal/handlers/usdt_test.go
git commit -m "Give each deposit its own amount so a payment identifies one order"
```

---

### Task 3: Extract the settle core

**Files:**
- Modify: `internal/handlers/usdt.go` (inside `verifyUSDTTxid`, usdt.go:866-1160)
- Test: existing `internal/handlers/usdt_test.go` is the safety net

**Interfaces:**
- Consumes: nothing new.
- Produces:

```go
// settleUSDTMatch credits a CONFIRMED on-chain payment for order `des` and
// resolves the session. Returns true when the payment was settled (or was
// already settled — the dedupe case), false when nothing could be credited.
func (b *Bot) settleUSDTMatch(ctx context.Context, p bands.Provider, des, ledgerID string, recvUnits int64) bool
```

Tasks 4-6 call exactly this. Refactor only — no behaviour change, no test changes.

- [ ] **Step 1: Confirm the current tests are green**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS. This is the baseline the refactor must preserve.

- [ ] **Step 2: Read the settle region**

Read `verifyUSDTTxid` end to end (usdt.go:866-1160). The crediting happens at three points: `b.store.Credit` at usdt.go:1015, `b.store.MarkTxn` at usdt.go:1068, and `b.store.Credit` again at usdt.go:1133. Identify which of those belong to the deposit path and which to the purchase path — they are different branches, and only the deposit path is what the watcher needs.

- [ ] **Step 3: Extract**

Move the deposit settle branch into `settleUSDTMatch` with the signature above. `verifyUSDTTxid` calls it in place of the moved code. Everything the extracted code touches — `DepositCredit`, `store.Credit`, `dropUSDTPending`, the customer receipt — moves with it or is passed in. Write an executive comment on the new function stating that it is the single settle path shared by the hash verify and the watcher.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS, unchanged. A failure here means the extraction changed behaviour — fix the extraction, never the test.

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/usdt.go
git commit -m "Give the settle path one name so two callers can share it"
```

---

### Task 4: The watcher, TRON

**Files:**
- Create: `internal/handlers/usdtwatch.go`
- Test: `internal/handlers/usdtwatch_test.go`
- Modify: `internal/handlers/usdt.go` (`StartupRecovery`, usdt.go:1264) to start it

**Interfaces:**
- Consumes: `settleUSDTMatch` (Task 3); `tron.GetIncomingTransfersConf(addr string, minTSMS int64, apiKey string, pageLimit int, base string) ([]tron.Transfer, error)`; `tron.MatchPayment(transfers []tron.Transfer, expectedAmount string, minTSMS int64, isUsed func(string) bool) (*tron.MatchedTransfer, error)`; `b.bands` live rows; `b.store.TxnExists(txnID string) (bool, error)`.
- Produces: `func (b *Bot) watchOnchain(ctx context.Context, p bands.Provider)` — the per-chain loop, started once per rail.

- [ ] **Step 1: Write the failing tests**

Three tests, all driving the tick function directly rather than the loop:

```go
// A transfer inside a live band credits exactly once.
func TestWatcherCreditsMatchingTransfer(t *testing.T)

// A second tick over the same transfer credits nothing — the processed.txn_id
// PK is the gate, and TxnExists is what stops the second attempt reaching it.
func TestWatcherDoesNotDoubleCredit(t *testing.T)

// No live bands means no RPC call at all: a bot with no pending deposits must
// not poll a public RPC endpoint on a timer.
func TestWatcherSkipsFetchWhenNoLiveBands(t *testing.T)

// A band rehydrated from the persisted table — the deposit made just before a
// deploy — still matches on the next tick. This is the hole the rejected
// per-deposit-goroutine design had, so it is pinned rather than assumed:
// build the bot, call rehydrateOnchainPending (usdt.go:1288) to load the row
// from store rather than reserving it in memory, then tick.
func TestWatcherMatchesRehydratedBand(t *testing.T)
```

Inject the transfer fetch as a field on `Bot` (a `func(...) ([]tron.Transfer, error)` defaulting to the real one) so tests supply a fake and count calls. That injection point is what makes test 3 possible at all.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/handlers/ -run TestWatcher -v`
Expected: FAIL — `watchOnchain` undefined.

- [ ] **Step 3: Implement the tick**

```go
// tickOnchain is one watcher pass for a rail: fetch what has arrived, match it
// against every live band, settle hits. Returns nothing — a failed pass is
// logged and retried next tick, never escalated, because the alternative
// (freeing a band on a read error) could hand a live amount to another order.
func (b *Bot) tickOnchain(ctx context.Context, p bands.Provider) {
	live := b.bands.Live(p) // add if absent; Registry.Len exists, a lister does not
	if len(live) == 0 {
		return // nobody is depositing: no RPC
	}
	// Fetch once for the whole rail, from the OLDEST live band's start, then
	// match every band against that one list. One call per tick regardless of
	// how many deposits are open.
	sinceMS := oldestSinceMS(live)
	transfers, err := b.fetchTronTransfers(sinceMS)
	if err != nil {
		log.Printf("usdt watch [%s]: fetch failed: %v", p, err)
		return
	}
	for _, res := range live {
		m, merr := tron.MatchPayment(transfers, res.Amount, sinceMS, func(id string) bool {
			used, terr := b.store.TxnExists(id)
			if terr != nil {
				// Treat an unreadable ledger as USED: crediting on a failed
				// dedupe read risks a double credit, skipping only delays.
				log.Printf("usdt watch: TxnExists failed for %s: %v", id, terr)
				return true
			}
			return used
		})
		if merr != nil {
			log.Printf("usdt watch [%s]: match failed for %s: %v", p, res.Des, merr)
			continue
		}
		if m == nil {
			continue
		}
		b.settleUSDTMatch(ctx, p, res.Des, "trx:"+m.ID, recvUnitsOf(m))
	}
}
```

`b.bands.Live(p) []bands.Reservation` is a new Registry method — a locked copy of the rows for one provider, in the same shape as `OtherAmounts` (registry.go:341). `recvUnitsOf` converts `MatchedTransfer.Amount` (`*big.Rat`) to units at the rail's decimals; reuse whatever `verifyUSDTTxid` already does for this rather than writing a second conversion.

- [ ] **Step 4: Wire the loop into StartupRecovery**

In `StartupRecovery` (usdt.go:1264), after the two rehydrate steps, start the watcher on the same worker registry:

```go
	// The watcher must start AFTER rehydration, or its first tick sees no live
	// bands and skips a deposit that was mid-flight across the restart.
	b.workers.Go(func(stop <-chan struct{}) {
		for {
			safeStep("usdt watch (tron)", func() { b.tickOnchain(ctx, bands.ProviderTron) })
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(b.usdtWatchInterval):
			}
		}
	})
```

`b.usdtWatchInterval` is a new field set in `dispatch.go` beside `usdtConfirmPoll` (dispatch.go:369), read from `USDT_WATCH_INTERVAL_S` with a 20s default and the same minimum-interval clamp `envDuration` already applies — an interval of 0 would spin hot against a public RPC (the failure mode task #30 already fixed once).

- [ ] **Step 5: Run tests**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/usdtwatch.go internal/handlers/usdtwatch_test.go internal/handlers/usdt.go internal/handlers/dispatch.go internal/pay/bands/registry.go
git commit -m "Watch the TRON address instead of asking the customer for a hash"
```

---

### Task 5: The watcher, BEP20

**Files:**
- Modify: `internal/handlers/usdtwatch.go`
- Test: `internal/handlers/usdtwatch_test.go`

**Interfaces:**
- Consumes: `bep20.LatestBlock() (int64, error)`; `bep20.GetIncomingTransfers(addr string, fromBlock int64, toBlock *int64) ([]bep20.Transfer, error)`; `bep20.MatchPaymentConf(transfers []bep20.Transfer, expectedAmount string, minBlock, latest int64, minConf int, isUsed func(string) bool) (*bep20.MatchedTransfer, error)`.
- Produces: `tickOnchain` handles both providers.

BEP20 differs from TRON in two ways that matter: it is block-ranged rather than timestamp-ranged, and it needs an explicit confirmation depth (`MinConfirmations`, default 12). TRON's rows are already `only_confirmed`.

- [ ] **Step 1: Write the failing test**

```go
// A BEP20 transfer that is not yet MinConfirmations deep must NOT settle — it
// can still be reorged away, and a credit is not reversible.
func TestWatcherWaitsForBep20Confirmations(t *testing.T)
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/handlers/ -run TestWatcherWaitsForBep20Confirmations -v`
Expected: FAIL — BEP20 branch not implemented.

- [ ] **Step 3: Implement the BEP20 branch**

Branch `tickOnchain` on provider. The BEP20 path calls `LatestBlock()`, derives `fromBlock` from the oldest live band's persisted block (the `Bep20PendingAdd` row stores 0 today — usdt.go:284 — so use a lookback window from `LatestBlock` instead and say so in the comment), then `MatchPaymentConf(..., minConf, isUsed)` with `isUsed` building `"bsc:"+id`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS.

- [ ] **Step 5: Start the BEP20 loop**

Add a second `b.workers.Go` block in `StartupRecovery` for `bands.ProviderBEP20`, identical in shape to TRON's.

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/usdtwatch.go internal/handlers/usdtwatch_test.go internal/handlers/usdt.go
git commit -m "Watch the BEP20 address, and wait out the reorg window"
```

---

### Task 6: Alert on unattributable payments

**Files:**
- Modify: `internal/handlers/usdtwatch.go`
- Test: `internal/handlers/usdtwatch_test.go`

**Interfaces:**
- Consumes: `b.notifyOwners(ctx context.Context, text string)` (notify.go:100); `b.store.MarkTxn(txnID, chatID string, amount int64) (bool, error)` (balance.go:93).
- Produces: nothing later tasks depend on.

With the hash prompt gone, a customer who sends the wrong amount has no self-service route. The watcher sees that money and must say so, exactly once.

- [ ] **Step 1: Write the failing test**

```go
// A transfer matching no live band fires exactly ONE owner alert, and a
// restart does not re-fire it. MarkTxn is the dedupe: it consumes the ledger
// id without crediting, so the second pass sees it as already handled.
func TestWatcherAlertsOnceForUnmatchedTransfer(t *testing.T)
```

Drive two ticks over the same fake transfer and assert exactly one owner send.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/handlers/ -run TestWatcherAlertsOnce -v`
Expected: FAIL — no alert sent.

- [ ] **Step 3: Implement**

After the per-band match loop, walk the transfers that matched no band. For each, `MarkTxn(ledgerID, "", recvVND)`; when it reports the id is new, `notifyOwners` with amount, sender, hash and time. When it reports already-seen, do nothing — that is the restart case.

Write the alert text so it says plainly that nothing was credited and the money is on-chain, naming the amount and the hash, in the shape the other `notifyOwners` money alerts already use.

`MarkTxn` with an empty chat id is deliberate: nobody owns this payment yet. Comment it, or a later reader will assume it is a bug.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/handlers/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/usdtwatch.go internal/handlers/usdtwatch_test.go
git commit -m "Say something when money arrives that belongs to nobody"
```

---

### Task 7: Delete the hash-entry path

**Files:**
- Modify: `internal/handlers/usdt.go` (`capTxid` usdt.go:723, the capture-chain registration usdt.go:101, the prompt at usdt.go:707-709, `pollUSDTConfirm` usdt.go:825, `endUSDTPoll` usdt.go:808, `verifyUSDTTxidSafe` usdt.go:1217, `adoptUSDTAmount` usdt.go:369)
- Modify: `internal/handlers/pending.go` (`TxidCtx` pending.go:155, the `Txid` field pending.go:63, the clear at pending.go:312)
- Test: `internal/handlers/usdt_test.go`, `internal/handlers/pending_test.go`

**Interfaces:**
- Consumes: everything above already landed.
- Produces: a smaller codebase.

Do this LAST. Until the watcher is proven in the test bot, the hash path is the only way a deposit completes.

- [ ] **Step 1: Delete, and let the compiler find the callers**

Remove `capTxid` and its capture-chain entry, the `EnterTxid` prompt and the callback that arms it, `pollUSDTConfirm`, `endUSDTPoll`, the `USDTPolling`/`usdtPolls` bookkeeping, `verifyUSDTTxidSafe`, `TxidCtx` and its `ChatState` field and its line in `ClearCustomerPending`, and `adoptUSDTAmount` (no callers after Task 2).

Keep `settleUSDTMatch`. Keep `verifyUSDTTxid` ONLY if something still calls it after the deletions — if nothing does, delete it and the by-hash lookups it uses (`tron.FindConfirmedTransferByHashConf`, `bep20.GetTransferToSellerByHashConf`) go with it.

- [ ] **Step 2: Fix the tests the deletions break**

Tests covering the deleted paths get deleted with them. A test asserting a behaviour that still exists must keep passing — if one fails for a reason other than "the thing it tested is gone", stop and investigate.

`pending_test.go` asserts `ClearCustomerPending` clears an exact set of slots by reflection (pending.go:301 notes this). Update that expected set, or the test will fail for the right reason.

- [ ] **Step 3: Verify**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1`
Expected: all clean. `grep -rn "capTxid\|pollUSDTConfirm\|TxidCtx" internal/` returns nothing but comments you have deliberately kept.

- [ ] **Step 4: Commit**

```bash
git add -A internal/handlers/
git commit -m "Delete the step that asked a customer to go find a hash"
```

---

## Deployment

After Task 1, deploy to `kiro-go-test` and leave it. The expiry rate is the measurement — if it drops sharply, stages 2-7 may be unnecessary. `.omc/RESUME.md` has the deploy sequence; note that a re-clone of production data is needed because the current test DB has been written to since its migration.

After Task 7, the same deploy, then a real deposit end to end on the test bot before this goes near production.

## Sequencing risk

Bands hold USDT-denominated strings (`usdtStr`), so the watcher is largely insulated from the S5-S8 ledger flip (#44, #45). Largely, not certainly. Before Task 4 lands, check `settleUSDTMatch`'s VND arithmetic against #44's plan — the conversion at the settle boundary is where a unit mismatch would hide.
