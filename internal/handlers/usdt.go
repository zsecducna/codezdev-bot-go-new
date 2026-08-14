package handlers

// usdt.go — the USDT rails: USDT BEP20 and USDT TRC20. Ports:
//
//	start_bep20_deposit         @9478   -> startBep20Deposit
//	start_tron_deposit          @9935   -> startTronDeposit
//	_send_bep20_qr / _send_tron_qr @9457/9907 -> sendOnchainQR
//	_send_usdt_screen           @9580   -> sendUSDTScreen
//	_prune_usdt_pending         @9606   -> bands.Registry.Sweep + expirySweepOnce
//	_expiry_sweep_once          @9313   -> expirySweepOnce
//	_startup_recovery           @9995   -> StartupRecovery
//
// HOW A DEPOSIT GETS CREDITED. Every on-chain USDT top-up is credited HANDS-FREE
// by the watcher in usdtwatch.go, which reads the seller address's own incoming
// transfers and attributes each one by AMOUNT ALONE (possible because every
// deposit is quoted a unique amount). The customer never pastes a transaction
// hash: the deposit screen is the address + the exact amount (+ optional QR), and
// their balance moves once the transfer confirms on-chain. Product orders are
// paid from wallet balance (paybal:), never on-chain.
//
// USDT migration note (2026-08-06): Binance Pay (the shared poller, its
// reconcile pass, and start_binance_deposit / cbPayBNB) was removed — the
// shop takes USDT via BEP20/TRON only. THE ONE REGISTRY comment below still
// applies to the two surviving rails: bands.Registry (Bot.bands) is shared
// across providers so a live BEP20 amount also blocks the same TRON amount
// (bands deviation 1) — that guarantee no longer needs to span three
// providers, but nothing about how it works changed.
//
// registry owns amount / chat / deadline / created-at; usdtRows holds only the
// rail-specific extras (VND total and the since-ms floor the watcher anchors its
// chain query on) that a Reservation deliberately does not carry.
//
// MONEY INVARIANTS (CLAUDE.md):
//   - Settlement goes through the ONE shared chokepoint: settlePaid's
//     reserved->paid CAS. Never both credit a wallet and deliver the same order.
//   - Deposit/credit idempotency lives in the STORAGE layer: store.Credit /
//     store.MarkTxn are PK-guarded on the ledger id ("bsc:"/"trx:" + hash).
//   - bands.ErrBandExhausted -> USDT_BUSY. NEVER fall back to a colliding amount.
//   - No st.mu / usdtMu / DB transaction is held across chain RPC or Telegram I/O.
//   - Every goroutine is workers-registry tracked and drained by Shutdown.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"runtime/debug"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// usdtRow is the rail-side half of one pending USDT session — the fields the
// bands.Reservation deliberately does not carry.
type usdtRow struct {
	// Total is the order's VND total (the DESIRED top-up for an on-chain deposit).
	Total int64
	// SinceMS is the row's own start stamp in epoch milliseconds — the floor the
	// hands-free watcher anchors its chain query and the per-band freshness gate
	// on (usdtwatch.go). Survives a restart via the persisted pending row.
	SinceMS int64
}

// ---- chain <-> provider plumbing -------------------------------------------

// chain codes as they appear in callback data and the deposit screen's caption
// (telegram_bot.py:9584 `chain: 'bsc' | 'trx'`).
const (
	chainBSC = "bsc"
	chainTRX = "trx"
)

// providerForChain maps a chain code to its bands provider.
func providerForChain(chain string) (bands.Provider, bool) {
	switch chain {
	case chainBSC:
		return bands.ProviderBEP20, true
	case chainTRX:
		return bands.ProviderTron, true
	}
	return "", false
}

// ledgerPrefix is the processed-ledger id namespace per rail, so a BSC hash and
// a TRON hash can never collide in the single `processed` PK
// (telegram_bot.py:9737 prefix).
func ledgerPrefix(p bands.Provider) string {
	if p == bands.ProviderBEP20 {
		return "bsc"
	}
	return "trx"
}

// usdtStr is Python's usdt_str — the customer-facing 2-decimal USDT string for a
// micro-USDT amount (1 USDT = 1,000,000 micro). It must produce the pay screen's
// base amount and the deposit's desired amount, or a correct payment could miss
// its band.
//
// Rounds UP to the cent (never undercharge) and ALWAYS returns a valid 2dp quote:
// micro is an exact stored value, so there is no exchange rate to fail and no ""
// return for a caller to guard. (The ledger used to be VND priced via a
// USD→VND rate; that rate is gone, and with it the "rate unavailable -> return
// ”" branch. The two historical call-site kinds — a customer quote and an audit
// row — no longer need to treat "" specially: a quote always renders now.)
func (b *Bot) usdtStr(micro int64) string {
	usd := float64(micro) / 1_000_000
	// Python: cents = int(usd*100) + (1 if (usd*100) % 1 else 0) -> ceil to cent.
	cents := math.Ceil(usd*100 - 1e-9)
	// Clear IEEE-754 negative zero so a zero amount renders "0.00", not "-0.00"
	// (see usdtDisp's matching guard in money.go for the full reasoning).
	if cents == 0 {
		cents = 0
	}
	return fmt.Sprintf("%.2f", cents/100)
}

// ---- pending-row registry (the rail-side half) ------------------------------

// putUSDTRow records/overwrites the rail-side row for a session. Leaf lock, no
// I/O inside.
func (b *Bot) putUSDTRow(p bands.Provider, des string, row usdtRow) {
	b.usdtMu.Lock()
	defer b.usdtMu.Unlock()
	if b.usdtRows[p] == nil {
		b.usdtRows[p] = map[string]*usdtRow{}
	}
	r := row
	b.usdtRows[p][des] = &r
}

// getUSDTRow returns a COPY of a session's rail-side row.
func (b *Bot) getUSDTRow(p bands.Provider, des string) (usdtRow, bool) {
	b.usdtMu.Lock()
	defer b.usdtMu.Unlock()
	r, ok := b.usdtRows[p][des]
	if !ok {
		return usdtRow{}, false
	}
	return *r, true
}

// persistUSDTPending writes the rail's persisted pending row so a restart can
// rehydrate the band the hands-free watcher attributes the next transfer against
// (store.bep20_pending_add / tron_pending_add).
func (b *Bot) persistUSDTPending(p bands.Provider, des, chatID, amount string, total, sinceMS int64, deadline float64) error {
	if p == bands.ProviderBEP20 {
		// from_block is unused (the BEP20 watcher fetches its own block range
		// via eth_getLogs, not per-pending) -> 0 (@9520).
		return b.store.Bep20PendingAdd(des, chatID, amount, total, 0, deadline)
	}
	return b.store.TronPendingAdd(des, chatID, amount, total, sinceMS, deadline)
}

// delUSDTPersisted drops the rail's persisted pending row. Idempotent.
func (b *Bot) delUSDTPersisted(p bands.Provider, des string) error {
	if p == bands.ProviderBEP20 {
		return b.store.Bep20PendingDel(des)
	}
	return b.store.TronPendingDel(des)
}

// dropUSDTPending removes a resolved session from the band registry, the
// rail-side rows and the persisted table — _drop_usdt_pending (@9596-9604) and
// the poller's `pop + *_pending_del` pairs. FREES THE BAND, so only call it once
// the credit is truly resolved (settled, or money moved).
func (b *Bot) dropUSDTPending(p bands.Provider, des string) {
	b.bands.Release(p, des)
	b.usdtMu.Lock()
	delete(b.usdtRows[p], des)
	b.usdtMu.Unlock()
	if err := b.delUSDTPersisted(p, des); err != nil {
		log.Printf("usdt: pending_del failed [%s %s]: %v", p, des, err)
	}
}

// ---- unique-amount reservation ---------------------------------------------

// reserveUSDTAmount hands this order its UNIQUE amount for `provider`, records
// the rail-side row and persists the pending row. Two-pass eviction, the
// cross-provider collision set and the own-dead-session rule all live in
// bands.Registry.Reserve; this is only the wiring
// (_binance_unique_amount @9101 / _bep20_unique_amount @9397 / _tron_unique_amount @9888).
//
// A re-tap reuses the EXISTING reservation rather than burning a second slot, so
// a customer is never shown two different amounts for one order (@15926-15928,
// @15975, @16018).
//
// Returns bands.ErrBandExhausted when every slot for this price is ambiguous —
// the caller MUST answer USDT_BUSY and refuse, never fall back to a colliding
// amount.
//
// allowDownward selects Reserve (product orders: the last-resort downward
// slot is an acceptable seller-absorbed undercharge) vs ReserveUpwardOnly
// (deposits: DepositCredit would round an exact downward-slot payment up to
// the customer's full requested total, so quoting below their request is a
// straight giveaway there — see ReserveUpwardOnly's doc comment).
func (b *Bot) reserveUSDTAmount(p bands.Provider, baseUSDT, chatID, des string, total, sinceMS int64, deadline time.Time, allowDownward bool) (string, error) {
	if res, ok := b.bands.Get(p, des); ok {
		return res.Amount, nil // re-tap: same amount + same window
	}
	reserve := b.bands.ReserveUpwardOnly
	if allowDownward {
		reserve = b.bands.Reserve
	}
	res, err := reserve(p, baseUSDT, chatID, des, deadline)
	if err != nil {
		return "", err
	}
	// SinceMS is the row's own start in epoch ms — the floor the hands-free
	// watcher anchors its chain query and the per-band freshness gate on. It is
	// the reservation's CreatedAt, which Sweep's grace is also measured from.
	b.putUSDTRow(p, des, usdtRow{
		Total: total, SinceMS: sinceMS,
	})
	// A session that is not PERSISTED cannot survive a restart: the Binance
	// reconcile would never see it and an on-chain submit would hit ORDER_GONE
	// with its band un-rehydrated. Python raises out of the handler at
	// store.*_pending_add (@9520, @10648), so the customer is never shown an
	// amount that a restart would strand. Undo the reservation and refuse rather
	// than invite a payment we might not be able to match.
	if perr := b.persistUSDTPending(p, des, chatID, res.Amount, total, sinceMS,
		float64(deadline.UnixNano())/1e9); perr != nil {
		b.releaseUnpersisted(p, des)
		return "", fmt.Errorf("usdt: persist pending [%s %s]: %w", p, des, perr)
	}
	return res.Amount, nil
}

// releaseUnpersisted rolls back an in-memory-only reservation whose persisted row
// could not be written. It deliberately does NOT touch the persisted tables (the
// write is what failed) — only the band and the rail-side row, so the slot is
// returned to the picker instead of being held by a session nobody can settle.
func (b *Bot) releaseUnpersisted(p bands.Provider, des string) {
	b.bands.Release(p, des)
	b.usdtMu.Lock()
	delete(b.usdtRows[p], des)
	b.usdtMu.Unlock()
}

// ---- deposit starts ---------------------------------------------------------

// startBep20Deposit / startTronDeposit create an on-chain USDT top-up —
// start_bep20_deposit (telegram_bot.py:9478-9525) and start_tron_deposit
// (@9935-9962).
//
// HOW IT GETS CREDITED. The deposit is credited HANDS-FREE by the watcher in
// usdtwatch.go: it reads the seller address's incoming transfers and attributes
// each one by amount, with no hash paste and no "I have paid" tap. Both rails
// work the same way.
//
// PICKED amount, like an order (see reserveUSDTAmount): a hands-free watcher
// attributes an incoming payment by amount alone, so two customers depositing
// the same round "120" at once must be handed different amounts or their
// payments would be indistinguishable. Every deposit is quoted the round base
// PLUS a 0.01000-0.01999 USDT jitter — "120.01000" for a free first slot, then
// "120.01101", "120.01202" and so on for concurrent same-price sessions — so a
// round number is never quoted and the amount alone names one order. The
// customer keeps the difference: an on-chain deposit credits what actually
// ARRIVED, which is always at least the jitter above what they asked for.
func (b *Bot) startBep20Deposit(ctx context.Context, chatID int64, lang i18n.Lang, micro int64) {
	b.startOnchainDeposit(ctx, chatID, lang, micro, chainBSC)
}

func (b *Bot) startTronDeposit(ctx context.Context, chatID int64, lang i18n.Lang, micro int64) {
	b.startOnchainDeposit(ctx, chatID, lang, micro, chainTRX)
}

// startOnchainDeposit is the shared body of the two functions above — the Python
// twins are line-for-line identical apart from their rail constants.
func (b *Bot) startOnchainDeposit(ctx context.Context, chatID int64, lang i18n.Lang, micro int64, chain string) {
	c := &i18n.C().Pay
	p, _ := providerForChain(chain)
	noID, tier := c.BEP20NoID, "USDT BEP20"
	configured := b.bep20Configured
	if chain == chainTRX {
		noID, tier, configured = c.TronNoID, "USDT TRC20", b.tronConfigured
	}
	if !configured() {
		b.send(ctx, chatID, noID.T(lang), nil)
		return
	}
	des := store.NewDes()
	if err := b.store.CreateOrder(des, fmtInt(chatID), string(lang), depositProduct, tier, 0, micro); err != nil {
		log.Printf("%s deposit create_order failed: %v", chain, err)
		b.send(ctx, chatID, noID.T(lang), nil)
		return
	}
	amount := b.usdtStr(micro) // the DESIRED amount, e.g. "120.00" (micro is always valid post-flip — no rate to fail)
	now := b.now()
	// depositTimeout, NOT expiryTimeout: a deposit reserves no folder stock, so
	// it gets the wide customer-facing window instead of the product-order
	// hard limit (see depositTimeout's field comment in dispatch.go).
	deadline := now.Add(b.depositTimeout) // wall-clock so it survives a restart
	// PICKED amount now, not adopted verbatim — see the doc comment above
	// startBep20Deposit/startTronDeposit for why. No caller-side log:
	// sendBandError already logs a non-exhaustion failure itself, so logging
	// here too would double it up.
	// allowDownward=false: a deposit must never be quoted below what the
	// customer asked to send (see reserveUSDTAmount's doc comment).
	picked, err := b.reserveUSDTAmount(p, amount, fmtInt(chatID), des, micro, now.UnixMilli(), deadline, false)
	if err != nil {
		b.abandonUSDTStub(des)
		// Band exhaustion is "busy", not "unconfigured" — sendBandError picks
		// the right refusal.
		b.sendBandError(ctx, chatID, lang, err, noID)
		return
	}
	b.sendUSDTScreen(ctx, chatID, lang, chain, des, picked)
}

// abandonUSDTStub releases a deposit stub order whose rail side failed —
// `store.claim_order(des, "reserved", "expired")` (@10653, @9498). A deposit
// reserves no folder stock, so expiring the row is the whole cleanup; it also
// clears pending_guard for the customer's next attempt.
func (b *Bot) abandonUSDTStub(des string) {
	if _, err := b.store.ClaimOrder(des, "reserved", "expired"); err != nil {
		log.Printf("usdt deposit stub abandon failed [des=%s]: %v", des, err)
	}
}

// sendBandError renders the right refusal for a failed reservation: a band
// exhaustion is USDT_BUSY ("busy, NOT unconfigured" @15942), anything else is the
// rail's own not-configured notice. Refusing is CORRECT — handing out a colliding
// amount would let one payment settle the wrong order.
func (b *Bot) sendBandError(ctx context.Context, chatID int64, lang i18n.Lang, err error, noID i18n.Str) {
	if errors.Is(err, bands.ErrBandExhausted) {
		b.send(ctx, chatID, i18n.C().Pay.USDTBusy.T(lang), nil)
		return
	}
	log.Println("usdt band reserve failed:", err)
	b.send(ctx, chatID, noID.T(lang), nil)
}

// ---- pay screens ------------------------------------------------------------

// sendUSDTScreen shows the on-chain deposit screen: the seller address and the
// EXACT amount to send (optionally a QR), hands-free — _send_usdt_screen
// (telegram_bot.py:9580-9594). There is no "I have made the payment" button any
// more: the watcher (usdtwatch.go) credits the deposit by amount alone once it
// confirms on-chain, so the customer has nothing to tap and no hash to paste.
func (b *Bot) sendUSDTScreen(ctx context.Context, chatID int64, lang i18n.Lang, chain, des, amount string) {
	c := &i18n.C().Pay
	// The receiving address MUST be the config-layered value (settings.json's
	// admin-set address overriding .env/ambient), not bep20.Address()/
	// tron.Address() — those read raw env only and never see a /setbep20 or
	// /settron write, which is exactly task #17 (a customer would keep being
	// shown the stale address after an admin "successfully" rotated it).
	//
	// The QR image knobs stay env-only: no admin wizard writes *_QR_URL/
	// *_QR_PATH to settings.json, so there is nothing for a config getter to
	// layer over. But that means a QR image is now a SEPARATE source from the
	// caption text, and before this fix both read the same raw env — always
	// consistent with each other, even though both could be stale. A wizard
	// rotation now makes the caption correct while a configured QR image
	// still encodes the OLD address: a customer who scans the code pays the
	// wrong address, and the watcher (also on the NEW address) would never
	// see that payment. Rather than ship a QR that can silently disagree with
	// the text next to it, drop the QR the moment its raw-env address stops
	// matching the effective one, falling back to the text-only caption below
	// (which is always correct) until the operator updates the QR to match.
	//
	// TASK #41 FIX: that drop-on-mismatch comparison used to read
	// bep20.Address()/tron.Address() — a bare os.Getenv, exactly like
	// task #17's stale-address bug, and #42's shared root cause: production
	// systemd carries NO EnvironmentFile, so ambient os.Getenv sees nothing,
	// while config.Load parses dir/.env itself into its own snapshot. So the
	// raw read always returned "", EqualFold(realAddress, "") was always
	// false, and the QR was UNCONDITIONALLY dropped in production, forever —
	// not "drop on a real rotation," just "never show." The fix below reads
	// the same env/QR knobs through b.envAddrStr/b.envQRStr instead, both of
	// which resolve via cfg (the .env-aware snapshot), so the comparison is
	// between two values that both actually resolve in production: the
	// EFFECTIVE address (settings.json-overridable) against the RAW
	// .env-snapshot address the QR was actually generated against. The
	// drop-on-mismatch LOGIC below is unchanged — only the operand is fixed.
	addr := b.bep20AddrStr()
	caption := i18n.F(c.BEP20Pay.T(lang), "usdt", amount, "address", addr)
	qrURL, qrPath := "", ""
	// EqualFold, not ==: a BSC/EVM address (0x + 40 hex) is case-insensitive
	// on-chain, so an operator's QR encoded from a same-address-different-case
	// env value (e.g. checksummed "0xABab…" vs a lowercase settings.json
	// write) must still be trusted, not dropped to text-only over a casing
	// difference that isn't a real address mismatch.
	if strings.EqualFold(addr, b.envAddrStr("BEP20_ADDRESS")) {
		qrURL, qrPath = b.envQRStr("BEP20_QR_URL"), b.envQRStr("BEP20_QR_PATH")
	}
	if chain == chainTRX {
		addr = b.tronAddrStr()
		caption = i18n.F(c.TronPay.T(lang), "usdt", amount, "address", addr)
		qrURL, qrPath = "", ""
		// Exact compare here, deliberately NOT EqualFold: TRON addresses are
		// base58, which IS case-sensitive on-chain — folding case could match
		// two genuinely different addresses and wrongly keep a stale QR.
		if addr == b.envAddrStr("TRON_ADDRESS") {
			qrURL, qrPath = b.envQRStr("TRON_QR_URL"), b.envQRStr("TRON_QR_PATH")
		}
	}
	b.sendOnchainQR(ctx, chatID, caption, qrURL, qrPath)
}

// envAddrStr resolves the RAW .env-snapshot value of an address env var
// through cfg.Env — deliberately NOT the settings.json-aware
// b.bep20AddrStr()/b.tronAddrStr(). This is the config-aware replacement
// for bep20.Address()/tron.Address()'s raw os.Getenv (task #41): in
// production there is no systemd EnvironmentFile, so ambient os.Getenv
// sees nothing, but cfg already parsed dir/.env itself at Load() time, so
// this is the only path that still resolves the value the operator's QR
// was actually generated against. sendUSDTScreen compares this against the
// EFFECTIVE address to detect a settings.json wizard rotation that left a
// configured QR encoding the now-stale address.
func (b *Bot) envAddrStr(key string) string {
	if b.cfg == nil {
		return ""
	}
	return strings.TrimSpace(b.cfg.Env(key))
}

// envQRStr resolves a *_QR_URL/*_QR_PATH knob through cfg.EnvOr — the
// config-aware replacement for bep20.QRURL()/QRPath()/tron.QRURL()/
// QRPath()'s raw os.Getenv (task #41, same root cause as envAddrStr
// above). These knobs are env-only: no settings.json wizard writes them,
// so there is nothing for a typed config getter to layer over.
func (b *Bot) envQRStr(key string) string {
	if b.cfg == nil {
		return ""
	}
	return strings.TrimSpace(b.cfg.EnvOr(key, ""))
}

// sendOnchainQR is _send_bep20_qr / _send_tron_qr (telegram_bot.py:9457-9476,
// 9907-9933): hosted URL, else uploaded file, else text-only, with the caption
// in Markdown so the address renders as a copyable code block.
func (b *Bot) sendOnchainQR(ctx context.Context, chatID int64, caption, qrURL, qrPath string) {
	if qrURL != "" {
		if _, err := b.tg.SendPhotoURLOpts(ctx, tg.SendPhotoURLParams{
			ChatID: chatID, Photo: qrURL, Caption: caption,
			ParseMode: "Markdown",
		}); err == nil {
			return
		} else {
			log.Println("usdt qr send error:", err) // fall through to text
		}
	} else if qrPath != "" {
		if _, err := b.tg.SendPhoto(ctx, chatID, qrPath, caption); err == nil {
			return
		} else {
			log.Println("usdt qr send error:", err)
		}
	}
	if _, err := b.tg.SendMessage(ctx, tg.SendMessageParams{
		ChatID: chatID, Text: caption, ParseMode: "Markdown",
	}); err != nil {
		log.Println("usdt screen send failed:", err)
	}
}

// orderTS converts an order's stored epoch-seconds creation stamp to a time.
func (b *Bot) orderTS(o *store.Order) time.Time {
	sec, frac := int64(o.TS), o.TS-float64(int64(o.TS))
	return time.Unix(sec, int64(frac*1e9))
}

// settleUSDTMatch credits a CONFIRMED on-chain payment for order `des` and
// resolves the deposit session. This is the ONE settle path: the hands-free
// on-chain watcher (usdtwatch.go) spots a payment by amount alone — with no hash
// paste — and routes every credit through here so dedupe and notify are shared,
// not duplicated.
//
// recvMicro/frm/recvStr are passed through rather than recomputed here: they
// come from the specific on-chain transfer the caller looked up (recvMicro via
// recvToMicro on the tx's exact `big.Rat` amount, frm/recvStr straight off the
// tx), and re-deriving them from recvUnits alone
// would lose precision the caller already has in hand — recvStr is rendered
// via FloatString (rounds) while recvUnits comes straight off the tx lookup
// (floored), a known deferred mismatch this extraction must not compound by
// trying to reconstruct one from the other. o/lang/prm/expected are likewise
// the caller's already-validated order/snapshot data —
// re-fetching them here could race a sweep/expiry between the caller's checks
// and this call, which the caller's single read cannot.
//
// Returns true when the payment was settled (or was already settled — the
// dedupe case, store.Credit's ledgerID PK match); false when nothing could be
// credited (bad stored amount, or the credit call itself errored). On false the
// caller holds the band and retries on the next tick.
func (b *Bot) settleUSDTMatch(ctx context.Context, p bands.Provider, des, ledgerID string, recvUnits, recvMicro int64, o *store.Order, lang i18n.Lang, chatID int64, prm bands.Params, expected, frm, recvStr string) bool {
	pc := i18n.C().Pay
	chain := ledgerPrefix(p) // "bsc" | "trx" — same string space as ledgerID's own prefix
	chatStr := fmtInt(chatID)

	// No claim-jack band gate — a deposit credits the SUBMITTER's own wallet
	// against their own desired amount, so a neighbour-band collision must not
	// false-reject (@9745-9751).
	desiredUnits, aerr := bands.AmountUnits(expected, prm.Decimals)
	if aerr != nil {
		log.Printf("usdt deposit (%s) bad stored amount %q for %s: %v", chain, expected, des, aerr)
		return false
	}
	// DEPOSIT_MATCH_TOL_UNITS is read through b.envInt (the config.Load
	// snapshot), NOT bands.DepositMatchTolUnits() — that helper uses the
	// pay package's raw os.Getenv, which production never populates
	// (config.Load deliberately does not write os.Environ and systemd
	// exports no EnvironmentFile), so the knob silently did nothing on
	// the live deposit-credit path while working in local testing. Same
	// class as tasks #10/#17/#22/#23.
	//
	// ClampDepositTol caps whatever that resolves to at this rail's own
	// band half-width. The two knobs live in different .env sections and
	// are tuned independently, so a deposit tolerance wider than the band
	// spacing is a reachable operator mistake — and it would make one
	// receipt equidistant-and-matching for two live orders' amounts.
	creditMicro, _ := bands.DepositCredit(recvUnits, desiredUnits, o.Total, recvMicro,
		bands.ClampDepositTol(prm,
			b.envInt("DEPOSIT_MATCH_TOL_UNITS", bands.DefaultDepositMatchTolUnits)))
	credited, bal, cerr := b.store.Credit(chatStr, creditMicro, ledgerID) // atomic dedupe + credit
	if cerr != nil {
		log.Printf("usdt deposit credit failed [des=%s %s]: %v", des, ledgerID, cerr)
		return false // nothing moved -> retry
	}
	if !credited {
		b.send(ctx, chatID, pc.TxidAlreadyUsed.T(lang), nil)
		return true
	}
	// Close the deposit order so it shows in history/revenue, then drop the row
	// (@9760-9762). The wallet is already credited above, so settlePaid is NOT
	// used here: it would credit o.Total a second time.
	if _, cerr := b.store.ClaimOrder(des, "reserved", "paid"); cerr != nil {
		log.Printf("usdt deposit paid-claim failed [des=%s]: %v", des, cerr)
	}
	if _, cerr := b.store.ClaimOrder(des, "paid", "delivered"); cerr != nil {
		log.Printf("usdt deposit delivered-claim failed [des=%s]: %v", des, cerr)
	}
	b.dropUSDTPending(p, des)
	// The wallet above is already credited correctly (that math never
	// calls usdtStr) — this line only formats the audit-row description.
	// auditAmount (money.go) builds sign+quote+unit by construction; it can
	// never render a bare "+ USDT", and with the micro-USDT ledger there is no
	// rate to fail so it always carries a real number.
	creditDesc := b.auditAmount("+", creditMicro)
	b.audit(chatStr, "usdt_"+chain+"_deposit", ledgerID,
		creditDesc,
		fmt.Sprintf("from=%s recv=%s balance=%d", frm, recvStr, bal))
	b.send(ctx, chatID, i18n.F(i18n.C().Deposit.OK.T(lang),
		"amount", b.usdtDisp(creditMicro), "balance", b.usdtDisp(bal)), nil)
	// notify_admin_deposit (telegram_bot.py:9861-9862) — method falls back
	// to the chain's own rail label exactly like Python's
	// `o.get("tier") or ("USDT BEP20" if chain == "bsc" else "USDT TRC20")`.
	method := o.Tier
	if method == "" {
		method = "USDT TRC20"
		if chain == chainBSC {
			method = "USDT BEP20"
		}
	}
	b.notifyAdminDeposit(ctx, chatID, creditMicro, bal, method)
	return true
}

// recvToMicro values a received on-chain USDT amount (a big.Rat of the rail's
// TokenDecimals: wei/10^18 for BEP20, value/10^6 for TRON) as micro-USDT:
// recv_usdt * 1_000_000 (1 USDT = 1,000,000 micro). Exact big.Rat arithmetic,
// rounded half-up to int64 — no float, no exchange rate, no error path. A
// received amount is credited by its exact on-chain value; the rail-credited
// figure and the customer-quote figure both live in the same micro unit now.
func (b *Bot) recvToMicro(p bands.Provider, recv *big.Rat) int64 {
	scaled := new(big.Rat).Mul(recv, big.NewRat(1_000_000, 1))
	num, den := scaled.Num(), scaled.Denom() // big.Rat guarantees Denom() > 0
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem)                     // truncated division; round half-up next
	if new(big.Int).Lsh(rem, 1).Cmp(den) >= 0 { // 2*rem >= den -> round up
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// pow10BigInt returns 10^n as a big.Int (TRON base-unit scaling).
func pow10BigInt(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// orderStatus renders an order's status for a log line, "?" when it vanished.
func orderStatus(o *store.Order) string {
	if o == nil {
		return "?"
	}
	return o.Status
}

// ---- panic isolation for the USDT money loops --------------------------------
//
// Python wraps EVERY long-lived USDT money loop in `try/except Exception` so an
// unexpected failure logs and the loop keeps going: _startup_recovery's
// per-step excepts (@10001-10019) and the sweep-loop except (@10022).
//
// Go's error returns only cover the failures a callee chose to name. The Python
// guards ALSO catch programming faults, and in Go those are panics — which,
// untracked, unwind straight out of the goroutine and kill the WHOLE process.
// On the settle path that is the worst possible failure mode: a single nil-deref
// under settlePaid/deliverPaid takes down both rails, and because
// StartupRecovery replays the same pending row on the next boot it becomes a
// crash loop. The update workers already recover for exactly this reason
// (cmd/bot/main.go "one bad update can't kill a worker"); these goroutines did
// not. safeStep restores that parity at the same granularity Python retries at.

// safeStep runs one recovery/sweep/watcher step with the panic guard Python's
// per-step `except Exception` provides. A faulted step is logged and SKIPPED;
// the caller continues with the next step / the next tick.
func safeStep(tag string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			// Value suppressed, stack kept — same policy as otp.go:465-470.
			// safeStep is generic: `fn` is any watcher/sweep step, including
			// ones that credit balances, so the recovered value can be built
			// from a credential-holding local. `tag` is a caller-supplied
			// constant and is safe to print.
			log.Printf("%s: recovered from panic (payload suppressed)\n%s", tag, debug.Stack())
		}
	}()
	fn()
}

// audit writes one customer-actor audit row (_audit @… -> store.LogAction). The
// admin `notify` flag Python passes on some of these is an M3 push concern; the
// row itself is what the forensic trail needs.
func (b *Bot) audit(chatID, action, target, outcome, detail string) {
	if b.store == nil {
		return
	}
	b.store.LogAction(chatID, "customer", action, target, outcome, detail)
}

// ---- startup recovery + expiry backstop -------------------------------------

// tsToTime converts a stored epoch-seconds float to a time.
func tsToTime(ts float64) time.Time {
	sec := int64(ts)
	return time.Unix(sec, int64((ts-float64(sec))*1e9))
}

// StartupRecovery is _startup_recovery (telegram_bot.py:9995-10025): boot-time
// rehydrate + the periodic expiry backstop, on a tracked goroutine so it never
// delays startup and Shutdown drains it.
//
// BEP20 and TRON only LOAD their pending rows so the hands-free watcher
// (started just below) keeps attributing transfers to them across a restart.
// (Binance Pay used to additionally rehydrate AND reconcile through its own
// shared poller; that rail was removed by the USDT-only migration, S3.) Then a
// light expiry sweep keeps running so a restart-orphaned checkout still expires
// and its band is eventually freed.
func (b *Bot) StartupRecovery(ctx context.Context) {
	b.workers.Go(func(stop <-chan struct{}) {
		// Each step is panic-isolated exactly as Python isolates them
		// (@10001-10019): a fault rehydrating one rail must not stop the others,
		// and must never take the bot down at boot — StartupRecovery replays the
		// same persisted rows on the next start, so an unguarded panic here is a
		// crash loop, not a one-off.
		safeStep("startup recovery (bep20)", func() { b.rehydrateOnchainPending(bands.ProviderBEP20) })
		safeStep("startup recovery (tron)", func() { b.rehydrateOnchainPending(bands.ProviderTron) })
		// The hands-free deposit watcher (usdtwatch.go), started AFTER the two
		// rehydrate steps and from INSIDE this goroutine so that ordering is
		// structural rather than a race: a first tick that ran before
		// rehydration would see no live bands, skip its fetch, and quietly not
		// credit a deposit that was mid-flight across the restart — the exact
		// window this whole feature exists to close.
		b.workers.Go(func(watchStop <-chan struct{}) {
			for {
				// Guarded like every other USDT money loop: a fault in one tick
				// logs and the watcher keeps running, because the alternative is
				// deposits silently going uncredited until someone notices.
				safeStep("usdt watch (tron)", func() { b.tickOnchain(ctx, bands.ProviderTron) })
				select {
				case <-watchStop:
					return
				case <-ctx.Done():
					return
				case <-time.After(b.usdtWatchInterval):
				}
			}
		})
		// The BEP20 hands-free watcher (usdtwatch.go), on its own goroutine so a
		// slow BSC RPC tick cannot delay TRON settlement or vice versa. Same
		// shape and the same ordering constraint as TRON's loop above: started
		// AFTER rehydration, so a first tick sees bands reloaded from the
		// persisted rows rather than an empty registry.
		b.workers.Go(func(watchStop <-chan struct{}) {
			for {
				safeStep("usdt watch (bep20)", func() { b.tickBep20(ctx, bands.ProviderBEP20) })
				select {
				case <-watchStop:
					return
				case <-ctx.Done():
					return
				case <-time.After(b.usdtWatchInterval):
				}
			}
		})
		for {
			// Python guards the sweep body too (@10021-10024) so one bad row can
			// never stop the backstop that frees bands and expires stale orders.
			safeStep("expiry sweep", func() { b.expirySweepOnce(ctx) })
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(b.expirySweepInterval):
			}
		}
	})
}

// rehydrateOnchainPending LOADS persisted BEP20/TRON pending rows back into the
// band registry (no settle) so (a) the unique-amount picker keeps avoiding
// collisions with sessions that predate the restart and (b) the hands-free
// watcher keeps attributing incoming transfers to them — including a LATE
// transfer on an order that already expired but is within usdtPendingGrace.
// Rows whose order vanished or is past the grace are pruned
// (_rehydrate_bep20_pending @9527 / _rehydrate_tron_pending @9965).
func (b *Bot) rehydrateOnchainPending(p bands.Provider) {
	type row struct {
		des, chatID, amount string
		total, sinceMS      int64
		ts, deadline        float64
	}
	var rows []row
	if p == bands.ProviderBEP20 {
		all, err := b.store.Bep20PendingAll()
		if err != nil {
			log.Println("bep20 rehydrate: load failed:", err)
			return
		}
		for _, r := range all {
			rows = append(rows, row{r.Des, r.ChatID, r.Amount, r.Total, 0, r.TS, r.Deadline})
		}
	} else {
		all, err := b.store.TronPendingAll()
		if err != nil {
			log.Println("tron rehydrate: load failed:", err)
			return
		}
		for _, r := range all {
			rows = append(rows, row{r.Des, r.ChatID, r.Amount, r.Total, r.MinTS, r.TS, r.Deadline})
		}
	}
	now, n := b.now(), 0
	for _, r := range rows {
		o, err := b.store.GetOrder(r.des)
		if err != nil {
			continue // DB blip: leave the row for the next boot / sweep
		}
		live := o != nil && o.Status == "reserved"
		inGrace := o != nil && now.Sub(tsToTime(r.ts)) < b.usdtPendingGrace
		if !(live || inGrace) {
			b.dropUSDTPending(p, r.des) // vanished / past grace -> drop
			continue
		}
		// Nothing in the persisted band row records what the money was for, so
		// deposit-ness is re-derived from the ORDER we just read — the same
		// test startOnchainDeposit's product constant makes on the way in. Both
		// branches above guarantee o != nil here (live and inGrace each require
		// it), so this cannot silently read false off a vanished order.
		if aerr := b.adoptPersisted(p, r.des, r.chatID, r.amount, r.total, r.sinceMS, r.ts, r.deadline,
			o.Product == depositProduct); aerr != nil {
			log.Printf("%s rehydrate: adopt %s failed: %v", p, r.des, aerr)
			continue
		}
		n++
	}
	if n > 0 {
		log.Printf("%s rehydrate: loaded %d pending order(s)", p, n)
	}
}

// adoptPersisted re-enters one persisted pending row into the band registry and
// the rail-side rows. CreatedAt is the row's own write time, which is what
// Sweep's grace is measured from (bands: "grace anchors CreatedAt"), so a
// restart does not silently extend a band's life.
//
// isDeposit is the caller's, for the reason bands.Adopt spells out: the
// persisted row does not record it, so it must come from the order.
func (b *Bot) adoptPersisted(p bands.Provider, des, chatID, amount string, total, sinceMS int64, ts, deadline float64, isDeposit bool) error {
	if err := b.bands.Adopt(bands.Reservation{
		Provider: p, Des: des, ChatID: chatID, Amount: amount,
		CreatedAt: tsToTime(ts), Deadline: tsToTime(deadline), IsDeposit: isDeposit,
	}); err != nil {
		return err
	}
	b.putUSDTRow(p, des, usdtRow{Total: total, SinceMS: sinceMS})
	return nil
}

// expirySweepOnce is the restart-safe backstop — _expiry_sweep_once
// (telegram_bot.py:9313-9348). It expires reserved orders past the hard payment
// limit whose per-order timer died (e.g. a restart between checkout and the
// deadline), then frees the bands of finished sessions past their grace.
//
// Deliberately QUIET (no customer message): a late credit may still be reconciled
// within the grace window, and a "window closed" note here would contradict the
// follow-up "refunded to balance". The per-order worker still notifies in the
// common (non-restart) case.
func (b *Bot) expirySweepOnce(ctx context.Context) {
	_ = ctx
	now := b.now()
	// Query with the SHORTER (product-order) window so nothing that's actually
	// due gets missed; a deposit row that's merely older than expiryTimeout but
	// still inside its own longer depositTimeout is skipped per-row below,
	// instead of being reaped out from under a customer who still has time to
	// pay (that mismatch is exactly what made the deposit window widening a
	// no-op the first time: this backstop kept expiring the DB row at the old
	// 10-minute mark regardless of what the deposit's own deadline said).
	cutoff := float64(now.Add(-b.expiryTimeout).UnixNano()) / 1e9
	orders, err := b.store.ReservedOrdersBefore(cutoff)
	if err != nil {
		log.Println("expiry sweep: load failed:", err)
	}
	for _, o := range orders {
		if o.Product == depositProduct && b.orderTS(o).Add(b.depositTimeout).After(now) {
			continue // deposit hasn't hit its own (longer) deadline yet
		}
		won, cerr := b.store.ClaimOrder(o.Des, "reserved", "expired")
		if cerr != nil {
			log.Printf("expiry sweep: claim failed for %s: %v", o.Des, cerr)
			continue
		}
		if won == nil {
			continue // already paid/handled elsewhere
		}
		// Virtual products (deposits, API keys) reserve no folder stock.
		if !isNoStockProduct(won.Product) && b.stock != nil {
			if _, rerr := b.stock.Release(won.Product, o.Des); rerr != nil {
				log.Printf("expiry sweep: release failed for %s: %v", o.Des, rerr)
			}
		}
		// _release_order_code (promo/referral hold) is M2.6.
	}
	b.sweepUSDTBands()
}

// sweepUSDTBands is the hard, band-freeing pass — _prune_usdt_pending
// (telegram_bot.py:9606-9623) expressed through bands.Registry.Sweep.
//
// The `keep` rule is the CALLER's per the Sweep contract: Sweep runs it UNLOCKED
// (one DB read per row) and owns nothing about order status or grace. Rows whose
// order is still 'reserved', or finished but within usdtPendingGrace measured
// from CreatedAt, are KEPT so a late on-chain payment can still be submitted and
// recovered; the rest are deleted and their persisted rows dropped.
func (b *Bot) sweepUSDTBands() {
	now := b.now()
	dropped, err := b.bands.Sweep(func(res bands.Reservation) bool {
		o, gerr := b.store.GetOrder(res.Des)
		if gerr != nil {
			// A read failure must NEVER free a band: releasing a live amount could
			// hand it to another order and let one payment settle the wrong one.
			log.Printf("usdt sweep: get_order failed for %s: %v", res.Des, gerr)
			return true
		}
		if o == nil {
			return false // order VANISHED -> free the band now
		}
		if o.Status == "reserved" {
			return true // still live -> keep
		}
		return now.Sub(res.CreatedAt) < b.usdtPendingGrace // finished but within grace
	})
	if err != nil {
		log.Println("usdt sweep failed:", err)
		return
	}
	for _, res := range dropped {
		b.usdtMu.Lock()
		delete(b.usdtRows[res.Provider], res.Des)
		b.usdtMu.Unlock()
		if derr := b.delUSDTPersisted(res.Provider, res.Des); derr != nil {
			log.Printf("usdt sweep: pending_del failed [%s %s]: %v", res.Provider, res.Des, derr)
		}
	}
}
