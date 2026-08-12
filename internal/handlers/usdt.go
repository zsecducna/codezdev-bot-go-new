package handlers

// usdt.go — the USDT rails: USDT BEP20 and USDT TRC20 (customer-driven
// verify-by-tx-hash, no poller). Ports:
//
//	paybsc: / paytrx:              @15947/15990 -> cbPayBSC / cbPayTRX
//	start_bep20_deposit         @9478   -> startBep20Deposit
//	start_tron_deposit          @9935   -> startTronDeposit
//	_send_bep20_qr / _send_tron_qr @9457/9907 -> sendOnchainQR
//	_send_usdt_screen           @9580   -> sendUSDTScreen
//	usdtpaid:                   @16032  -> cbUSDTPaid
//	pending_txid capture        @18978  -> capTxid (capture #43)
//	_verify_usdt_txid           @9652   -> verifyUSDTTxid
//	_poll_usdt_confirm          @9850   -> pollUSDTConfirm
//	_prune_usdt_pending         @9606   -> bands.Registry.Sweep + expirySweepOnce
//	_expiry_sweep_once          @9313   -> expirySweepOnce
//	_startup_recovery           @9995   -> StartupRecovery
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
// rail-specific extras (VND total, TRON's min_ts floor, and the on-chain
// [t_start, t_click] freshness window) that a Reservation deliberately does not
// carry.
//
// MONEY INVARIANTS (CLAUDE.md):
//   - Settlement goes through the ONE shared chokepoint: settlePaid's
//     reserved->paid CAS. Never both credit a wallet and deliver the same order.
//   - Deposit/credit idempotency lives in the STORAGE layer: store.Credit /
//     store.MarkTxn are PK-guarded on the ledger id ("bsc:"/"trx:" + hash).
//   - An underpay never delivers: a PAY order settles only when the received
//     amount is inside its own band; anything else is banked to the wallet or
//     refused.
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
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// txHashRE is the shape gate the pending_txid capture applies BEFORE any chain
// round-trip (telegram_bot.py:18988 `_re.fullmatch(r"[0-9a-fA-F]{64}", hexpart)`).
// A clearly-wrong paste gets a precise message and keeps the slot armed.
var txHashRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// usdtRow is the rail-side half of one pending USDT session — the fields Python
// keeps in _binance_pending / _bep20_pending / _tron_pending (@9070-9072,
// @9391-9393, @9895-9897) that the bands.Reservation does not carry.
type usdtRow struct {
	// Total is the order's VND total (what the wallet is credited on a second
	// Binance payment, and the DESIRED top-up for an on-chain deposit).
	Total int64
	// SinceMS is Binance's `since_ms` pay/transactions floor, and TRON's `min_ts`
	// verify-query floor. Unused for BEP20 (verify is by-hash, from_block=0).
	SinceMS int64
	// TStart / TClick are the on-chain freshness window (epoch seconds). TClick is
	// 0 until "I have made the payment" is tapped; the verify then upper-bounds at
	// `now` instead (safe — the UNIQUE AMOUNT is the real binding, @9678-9682).
	TStart float64
	TClick float64
}

// registerUSDT attaches the USDT rails' capture + callback routes.
//
// pending_txid is capture #43 (handlers-go.md §2.2) — it must sit BEFORE
// pending_deposit (#44) so a pasted 64-hex hash is never misread as a deposit
// amount (telegram_bot.py:18976 "Consumed before the deposit/qty handlers").
//
// The paybsc:/paytrx: callback routes are NOT appended here: M2.2 registered
// them as documented stubs, and dispatchCB is first-match-wins, so an
// appended route would be shadowed forever. rebindCBRoute swaps the real
// handler into the stub's EXISTING slot, which also keeps the route order
// pinned. paybnb: (Binance Pay) stays a stub — Binance Pay was removed by the
// USDT-only migration (S3).
func (b *Bot) registerUSDT() {
	b.insertCapturesBefore("deposit",
		stage{name: "txid", fn: (*Bot).capTxid}, // #43 @18978 peek
	)
	b.rebindCBRoute("paybsc:", (*Bot).cbPayBSC) // @15947
	b.rebindCBRoute("paytrx:", (*Bot).cbPayTRX) // @15990
	// usdtpaid: has no stub to replace — append it (it is the last of the
	// pay-screen callbacks in the Python chain, @16032).
	b.cbRoutes = append(b.cbRoutes, cbRoute{key: "usdtpaid:", fn: (*Bot).cbUSDTPaid})
}

// rebindCBRoute replaces the handler of an already-registered route key in place,
// preserving its position in the first-match dispatch order. Appends when the key
// is not registered (so the rails still work if the stubs are ever removed).
func (b *Bot) rebindCBRoute(key string, fn func(b *Bot, cc *cbCtx)) {
	for i := range b.cbRoutes {
		if b.cbRoutes[i].key == key {
			b.cbRoutes[i].fn = fn
			return
		}
	}
	b.cbRoutes = append(b.cbRoutes, cbRoute{key: key, fn: fn})
}

// ---- chain <-> provider plumbing -------------------------------------------

// chain codes as they appear in callback data and the pending_txid slot
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

// usdtStr is Python's usdt_str(vnd) — the customer-facing 2-decimal USDT string
// for a VND amount. It must produce the pay screen's base amount and the
// deposit's desired amount, or a correct payment could miss its band.
//
// Stale-comment fix: this used to say "" on a missing/invalid rate. That is
// no longer accurate — vndToUSD always has a fallback (config.DefaultUSDToVND,
// "25000") for a genuinely MISSING rate, so "missing" can no longer produce
// "". "" is still possible on an actually INVALID configured rate (the
// resolved string fails to parse as a float, or parses to <=0) — e.g. a
// broken ambient USD_TO_VND env var, since capSetRate already range-guards
// the admin-set path. A whitespace-padded-but-otherwise-valid ambient rate
// (e.g. `USD_TO_VND=" 25000 "`) used to land in this same "" bucket — Go's
// ParseFloat rejects surrounding whitespace outright — but vndToUSD now
// TrimSpaces before parsing (task #25), so that specific trigger is closed;
// this branch still fires for a rate that is invalid on its merits (garbage
// text, zero, negative).
//
// That branch is real, not dead. Its callers split into two kinds by what
// "" is allowed to do:
//   - startOnchainDeposit (~line 420) and cbPayOnchain (~line 607) are
//     customer-facing quotes: both test usdtStr == "" and ABANDON rather
//     than quote a broken price. Quoting wrong is worse than not quoting —
//     deliberate, and it stays this way.
//   - the on-chain settle audit line (~line 986) does NOT call usdtStr
//     directly any more (task #15) — it goes through auditAmount
//     (money.go), which can never render "+ USDT" with the amount silently
//     missing; it falls back to the raw VND figure plus an explicit marker
//     instead. adminusers.go's admin-credit/-debit audit lines route
//     through the same helper. See auditAmount's doc comment for the full
//     reasoning, including why that fallback is unreachable at BOTH of
//     today's call sites (an identical rate check already gates each one
//     upstream — usdtToVNDFor here, adminUsdtToVnd in adminpricing.go) and
//     is kept anyway as by-construction protection for the next
//     audit/description site, which may have no such upstream gate.
func (b *Bot) usdtStr(vnd int64) string {
	s, err := b.vndToUSD(vnd)
	if err != nil {
		return ""
	}
	return s
}

// vndToUSD is SCAFFOLDING introduced by the SePay/PayPal/Binance removal
// (USDT migration slices S1-S3, 2026-08-06). usdtStr used to route through
// paypal.VndToUSD for its VND->USD conversion; paypal.go is gone along with
// the rest of the PayPal rail. There is no drop-in replacement on the
// surviving BEP20/TRON rails — their UsdtToVND converters go the opposite
// direction (USD->VND) and read the rate a different way.
//
// Reads the rate through the CONFIG CHAIN (cfg.USDToVND(): settings.json's
// admin-set usd_to_vnd overriding .env/ambient USD_TO_VND, else the compiled
// default) — deliberately NOT os.LookupEnv, which is the quote/charge
// rate-source split rate-source-investigation.md flags as a defect this
// migration supersedes rather than reproduces.
//
// Migration slice S5 deletes this function, its one call site (usdtStr), and
// the VND rate itself outright when the ledger flips to micro-USDT — do not
// build new callers on it.
func (b *Bot) vndToUSD(vnd int64) (string, error) {
	rateStr := config.DefaultUSDToVND
	if b.cfg != nil {
		rateStr = b.cfg.USDToVND()
	}
	// TrimSpace: see money.go's usdtDisp for why — cfg.USDToVND() can return an
	// un-trimmed AMBIENT env value, and ParseFloat rejects surrounding
	// whitespace, so this must match the trim bep20/tron's UsdtToVND already
	// apply to the same rate for the two sides (quote vs. credit) to agree.
	//
	// config.USDToVND() itself now trims too (internal/config/settings.go) —
	// this line is REDUNDANT, kept for defence in depth on a money path. Do
	// not delete the SOURCE trim on the theory that this call site (or
	// money.go's matching one) covers it: deposit.go, apikey.go and
	// adminpricing.go each call cfg.USDToVND() directly with no trim of
	// their own and depend entirely on the source fix.
	rateStr = strings.TrimSpace(rateStr)
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || rate <= 0 {
		return "", fmt.Errorf("usdt: no USD_TO_VND rate configured")
	}
	usd := float64(vnd) / rate
	// Python: cents = int(usd*100) + (1 if (usd*100) % 1 else 0) -> ceil to cent
	// (mirrors the deleted paypal.VndToUSD's own rounding exactly).
	cents := math.Ceil(usd*100 - 1e-9)
	return fmt.Sprintf("%.2f", cents/100), nil
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

// markUSDTClick stamps t_click (the upper bound of the [start, click] freshness
// window) — the usdtpaid: branch @16057-16060. Reports false when the row is gone
// (past the grace / never created), which is the caller's ORDER_GONE signal.
func (b *Bot) markUSDTClick(p bands.Provider, des string, at float64) bool {
	b.usdtMu.Lock()
	defer b.usdtMu.Unlock()
	r, ok := b.usdtRows[p][des]
	if !ok {
		return false
	}
	r.TClick = at
	return true
}

// persistUSDTPending writes the rail's persisted pending row so a restart can
// rehydrate the band + the customer can still submit a hash
// (store.bep20_pending_add / tron_pending_add).
func (b *Bot) persistUSDTPending(p bands.Provider, des, chatID, amount string, total, sinceMS int64, deadline float64) error {
	if p == bands.ProviderBEP20 {
		// from_block is unused (verify is by-hash, not eth_getLogs) -> 0 (@9520).
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
	b.putUSDTRow(p, des, usdtRow{
		Total: total, SinceMS: sinceMS,
		TStart: float64(res.CreatedAt.UnixNano()) / 1e9,
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

// adoptUSDTAmount records a NON-picked amount: the customer's DESIRED top-up
// amount, verbatim. As of the hands-free-deposits work it has no caller on the
// deposit path any more — startOnchainDeposit below now calls
// reserveUSDTAmount instead, so every deposit's amount is unique too — but the
// function itself stays until Task 7 deletes it along with the rest of the
// customer-pastes-a-hash flow. The row still enters the registry so it (a)
// blocks other orders' pickers and (b) supplies the `expected` amount the
// verify compares against.
func (b *Bot) adoptUSDTAmount(p bands.Provider, amount, chatID, des string, total, sinceMS int64, deadline time.Time) error {
	now := b.now()
	if err := b.bands.Adopt(bands.Reservation{
		Provider: p, Des: des, ChatID: chatID, Amount: amount,
		CreatedAt: now, Deadline: deadline,
	}); err != nil {
		return err
	}
	b.putUSDTRow(p, des, usdtRow{Total: total, SinceMS: sinceMS, TStart: float64(now.UnixNano()) / 1e9})
	// Same rule as reserveUSDTAmount: never show a pay/deposit screen for a
	// session a restart could strand.
	if perr := b.persistUSDTPending(p, des, chatID, amount, total, sinceMS,
		float64(deadline.UnixNano())/1e9); perr != nil {
		b.releaseUnpersisted(p, des)
		return fmt.Errorf("usdt: persist pending [%s %s]: %w", p, des, perr)
	}
	return nil
}

// ---- deposit starts ---------------------------------------------------------

// startBep20Deposit / startTronDeposit create an on-chain USDT top-up credited
// when the customer submits a CONFIRMED tx hash — start_bep20_deposit
// (telegram_bot.py:9478-9525) and start_tron_deposit (@9935-9962).
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
func (b *Bot) startBep20Deposit(ctx context.Context, chatID int64, lang i18n.Lang, vnd int64) {
	b.startOnchainDeposit(ctx, chatID, lang, vnd, chainBSC)
}

func (b *Bot) startTronDeposit(ctx context.Context, chatID int64, lang i18n.Lang, vnd int64) {
	b.startOnchainDeposit(ctx, chatID, lang, vnd, chainTRX)
}

// startOnchainDeposit is the shared body of the two functions above — the Python
// twins are line-for-line identical apart from their rail constants.
func (b *Bot) startOnchainDeposit(ctx context.Context, chatID int64, lang i18n.Lang, vnd int64, chain string) {
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
	if err := b.store.CreateOrder(des, fmtInt(chatID), string(lang), depositProduct, tier, 0, vnd); err != nil {
		log.Printf("%s deposit create_order failed: %v", chain, err)
		b.send(ctx, chatID, noID.T(lang), nil)
		return
	}
	amount := b.usdtStr(vnd) // the DESIRED amount, e.g. "120.00"
	if amount == "" {        // exchange rate unset -> can't price it (@9497)
		b.abandonUSDTStub(des)
		b.send(ctx, chatID, noID.T(lang), nil)
		return
	}
	now := b.now()
	// depositTimeout, NOT expiryTimeout: a deposit reserves no folder stock, so
	// it gets the wide customer-facing window instead of the product-order
	// hard limit (see depositTimeout's field comment in dispatch.go).
	deadline := now.Add(b.depositTimeout) // wall-clock so it survives a restart
	// PICKED amount now, not adopted verbatim — see the doc comment above
	// startBep20Deposit/startTronDeposit for why. Mirrors cbPayOnchain's
	// order-pay call below: no caller-side log, sendBandError already logs a
	// non-exhaustion failure itself, so logging here too would double it up.
	// allowDownward=false: a deposit must never be quoted below what the
	// customer asked to send (see reserveUSDTAmount's doc comment).
	picked, err := b.reserveUSDTAmount(p, amount, fmtInt(chatID), des, vnd, now.UnixMilli(), deadline, false)
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

// sendUSDTScreen shows the on-chain pay/deposit screen: the seller address, the
// EXACT amount to send, and the "I have made the payment" button that arms the
// verify-by-hash flow — _send_usdt_screen (telegram_bot.py:9580-9594).
func (b *Bot) sendUSDTScreen(ctx context.Context, chatID int64, lang i18n.Lang, chain, des, amount string) {
	c := &i18n.C().Pay
	// _usdt_paid_markup @9570: the single button carrying (chain, des).
	markup := &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		{{Text: c.USDTPaidBtn.T(lang), CallbackData: cbData("usdtpaid", chain, des)}},
	}}
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
	// wrong address, and the by-hash verify (also on the NEW address) would
	// never find that payment. Rather than ship a QR that can silently
	// disagree with the text next to it, drop the QR the moment its raw-env
	// address stops matching the effective one, falling back to the
	// text-only caption below (which is always correct) until the operator
	// updates the QR to match.
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
	b.sendOnchainQR(ctx, chatID, caption, qrURL, qrPath, markup)
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
//
// The local-file path cannot carry a keyboard (multipart), so — exactly like
// Python (@9470-9473) — it follows the image with a one-line button message.
func (b *Bot) sendOnchainQR(ctx context.Context, chatID int64, caption, qrURL, qrPath string, markup *tg.InlineKeyboardMarkup) {
	if qrURL != "" {
		if _, err := b.tg.SendPhotoURLOpts(ctx, tg.SendPhotoURLParams{
			ChatID: chatID, Photo: qrURL, Caption: caption,
			ParseMode: "Markdown", ReplyMarkup: markup,
		}); err == nil {
			return
		} else {
			log.Println("usdt qr send error:", err) // fall through to text
		}
	} else if qrPath != "" {
		if _, err := b.tg.SendPhoto(ctx, chatID, qrPath, caption); err == nil {
			// multipart can't carry a keyboard -> follow-up button message (@9471).
			b.send(ctx, chatID, "👇", markup)
			return
		} else {
			log.Println("usdt qr send error:", err)
		}
	}
	if _, err := b.tg.SendMessage(ctx, tg.SendMessageParams{
		ChatID: chatID, Text: caption, ParseMode: "Markdown", ReplyMarkup: markup,
	}); err != nil {
		log.Println("usdt screen send failed:", err)
	}
}

// ---- pay-by-USDT callbacks --------------------------------------------------

// payableOrder is the shared ownership + method + hard-expiry gate every
// pay-by-USDT button runs (telegram_bot.py:15903-15922, 15952-15972,
// 15995-16015). Returns nil after messaging the customer when the tap must not
// proceed.
//
// The window check uses the order's REAL hard expiry (creation +
// DEPOSIT_TIMEOUT_S), so a re-tap can never extend payment past it.
func (b *Bot) payableOrder(cc *cbCtx, prefix, method string, configured func() bool, noID i18n.Str) *store.Order {
	des := strings.TrimPrefix(cc.Data, prefix)
	o, err := b.store.GetOrder(des)
	// Ownership guard: a `des` can leak (it is shown in QR transfer content), so a
	// payment button must only act for the chat that actually owns the order.
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(b.langFor(cc.ChatID, "en")), nil)
		return nil
	}
	lang := langSafe(o)
	if !b.payEnabled(method) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
		return nil
	}
	if !configured() { // the button only renders when set, but guard a stale tap
		b.send(cc.Ctx, cc.ChatID, noID.T(lang), nil)
		return nil
	}
	if !b.orderTS(o).Add(b.expiryTimeout).After(b.now()) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return nil
	}
	return o
}

// orderTS converts an order's stored epoch-seconds creation stamp to a time.
func (b *Bot) orderTS(o *store.Order) time.Time {
	sec, frac := int64(o.TS), o.TS-float64(int64(o.TS))
	return time.Unix(sec, int64(frac*1e9))
}

// cbPayBSC / cbPayTRX pay a reserved order on-chain — the paybsc: (@15947) and
// paytrx: (@15990) branches. The UNIQUE amount binds the payment to THIS order
// (anti-claim-jack); there is no poller, the customer pastes their tx hash.
func (b *Bot) cbPayBSC(cc *cbCtx) { b.cbPayOnchain(cc, chainBSC) }
func (b *Bot) cbPayTRX(cc *cbCtx) { b.cbPayOnchain(cc, chainTRX) }

// cbPayOnchain is the shared body of the two branches above (line-for-line twins
// in Python apart from their rail constants).
func (b *Bot) cbPayOnchain(cc *cbCtx, chain string) {
	c := &i18n.C().Pay
	p, _ := providerForChain(chain)
	prefix, method, noID, configured := "paybsc:", "bep20", c.BEP20NoID, b.bep20Configured
	if chain == chainTRX {
		prefix, method, noID, configured = "paytrx:", "tron", c.TronNoID, b.tronConfigured
	}
	o := b.payableOrder(cc, prefix, method, configured, noID)
	if o == nil {
		return
	}
	lang := langSafe(o)
	base := b.usdtStr(o.Total)
	if base == "" {
		b.send(cc.Ctx, cc.ChatID, noID.T(lang), nil)
		return
	}
	created := b.orderTS(o)
	// allowDownward=true: an order MAY absorb the last-resort downward slot's
	// undercharge as a seller cost, unlike a deposit — this is Reserve's
	// original, unchanged behavior.
	amount, err := b.reserveUSDTAmount(p, base, o.ChatID, o.Des,
		o.Total, created.UnixMilli(), created.Add(b.expiryTimeout), true)
	if err != nil {
		b.sendBandError(cc.Ctx, cc.ChatID, lang, err, noID)
		return
	}
	b.sendUSDTScreen(cc.Ctx, cc.ChatID, lang, chain, o.Des, amount)
}

// cbUSDTPaid handles "I have made the payment" — the usdtpaid: branch
// (telegram_bot.py:16032-16065). Records the click time (the upper bound of the
// [start, click] freshness window) and arms pending_txid so the NEXT plain text
// is verified as the tx hash.
func (b *Bot) cbUSDTPaid(cc *cbCtx) {
	parts := strings.SplitN(cc.Data, ":", 3)
	if len(parts) != 3 {
		return
	}
	chain, des := parts[1], parts[2]
	p, ok := providerForChain(chain)
	if !ok {
		return
	}
	o, err := b.store.GetOrder(des)
	lang := b.langFor(cc.ChatID, "en")
	if o != nil {
		lang = langSafe(o)
	}
	// Ownership guard (a des can leak in the screen text). NOTE: status is NOT
	// required to be 'reserved' — an order that just expired is still recoverable
	// while its pending row survives the grace, so a late tap can still submit the
	// hash (the verify then refunds-to-wallet / credits, @16046-16048).
	if err != nil || o == nil || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}
	if !b.markUSDTClick(p, des, float64(b.now().UnixNano())/1e9) {
		// Pending row gone (past the grace, or never created) -> the unique amount
		// is no longer reserved; ask them to reopen the payment screen (@16062).
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return
	}
	cc.st.ClearCustomerPending() // drop stale sibling captures first (@16066)
	cc.st.mu.Lock()
	cc.st.Txid = &TxidCtx{Chain: chain, Des: des}
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, i18n.C().Pay.EnterTxid.T(lang), nil)
}

// ---- pending_txid capture (#43, @18978) -------------------------------------

// capTxid is capture-chain entry #43, pending_txid — the on-chain tx-hash capture
// (telegram_bot.py:18978-19027). PEEK semantics: an escaping text (a "/command"
// or a tapped keyboard label) leaves the slot armed and falls through.
//
// A bad SHAPE is answered precisely and keeps the slot armed (no chain
// round-trip). A well-formed hash CLAIMS the slot (so a second paste during the
// wait can't re-enter), then verifies once QUIETLY: an unconfirmed tx is not
// reported as "not found" — a background poll keeps checking and settles
// automatically once it confirms.
func (b *Bot) capTxid(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Txid != nil
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	// peek escape (@18978's `and` guards).
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	pc := &i18n.C().Pay
	raw := strings.TrimSpace(c.Text)
	// Shape-check FIRST so a clearly-wrong paste gets a precise message and stays
	// armed. BSC: 0x + 64 hex; TRON: bare 64 hex (0x tolerated) — @18986-18991.
	hexPart := raw
	if len(raw) >= 2 && strings.EqualFold(raw[:2], "0x") {
		hexPart = raw[2:]
	}
	if !txHashRE.MatchString(hexPart) {
		b.send(c.Ctx, c.ChatID, pc.TxidBadFormat.T(c.Lang), nil)
		return true // keep pending for a valid retry
	}
	// Atomically CLAIM the capture (double-submit guard, @18996).
	c.st.mu.Lock()
	cap := c.st.Txid
	c.st.Txid = nil
	c.st.mu.Unlock()
	if cap == nil {
		return true // lost the claim to a concurrent duplicate message
	}
	b.send(c.Ctx, c.ChatID, pc.TxidChecking.T(c.Lang), nil)
	// Panic-guarded: Python's `except -> terminal = False` (@19141) turns a blown-up
	// verify into "keep checking in the background" rather than dropping the hash.
	terminal := b.verifyUSDTTxidSafe(c.Ctx, c.ChatID, cap.Chain, cap.Des, raw, true)
	if terminal {
		return true
	}
	// Not terminal -> keep checking in the background. One poll per chat, and at
	// most maxUSDTPolls globally, decided in ONE atomic step (@19000-19007).
	st := c.st
	started, capped := b.tryStartUSDTPoll(st)
	switch {
	case started:
		chain, des := cap.Chain, cap.Des
		if !b.workers.TryGo(func(stop <-chan struct{}) {
			b.pollUSDTConfirm(st, c.ChatID, chain, des, raw, stop)
		}) {
			// Registry shutting down -> free the slot so the chat isn't locked out
			// and tell them to retry rather than silently dropping the credit
			// (@19012-19019). The hash is still submittable: nothing was consumed.
			b.endUSDTPoll(st)
			b.send(c.Ctx, c.ChatID, pc.TxidNotFound.T(c.Lang), nil)
			return true
		}
		b.send(c.Ctx, c.ChatID, pc.TxidWaiting.T(c.Lang), nil)
	case capped:
		b.send(c.Ctx, c.ChatID, pc.TxidNotFound.T(c.Lang), nil)
	default:
		// A poll for this chat is already running -> they already saw TXID_WAITING.
	}
	return true
}

// tryStartUSDTPoll is the atomic test-and-set behind "one confirmation poll per
// chat, at most USDT_MAX_POLLS globally" (_usdt_polling @9856 + USDT_MAX_POLLS
// @9853). Returns (started, capped); (false,false) means this chat already has a
// live poll. Both locks are released before any I/O (handlers-go.md §8).
func (b *Bot) tryStartUSDTPoll(st *ChatState) (started, capped bool) {
	b.usdtPollMu.Lock()
	defer b.usdtPollMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.USDTPolling {
		return false, false
	}
	if b.usdtPolls >= b.maxUSDTPolls {
		return false, true
	}
	st.USDTPolling = true
	b.usdtPolls++
	return true, false
}

// endUSDTPoll releases a chat's confirmation-poll slot (the `finally` at @9877).
func (b *Bot) endUSDTPoll(st *ChatState) {
	b.usdtPollMu.Lock()
	defer b.usdtPollMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.USDTPolling {
		st.USDTPolling = false
		if b.usdtPolls > 0 {
			b.usdtPolls--
		}
	}
}

// pollUSDTConfirm keeps re-checking a submitted hash until it CONFIRMS (then
// settles) or usdtConfirmWait elapses — _poll_usdt_confirm
// (telegram_bot.py:9850-9878). Retries are quiet (no per-cycle message); the
// FINAL attempt is not, so a still-missing tx gets one closing note.
func (b *Bot) pollUSDTConfirm(st *ChatState, chatID int64, chain, des, txhash string, stop <-chan struct{}) {
	defer b.endUSDTPoll(st)
	ctx := context.Background() // the request ctx is long gone
	deadline := b.now().Add(b.usdtConfirmWait)
	for b.now().Before(deadline) {
		select {
		case <-stop:
			return // shutdown before the window elapsed
		case <-time.After(b.usdtConfirmPoll):
		}
		// Per-attempt panic guard (Python @9862 "never let a cycle kill the poll"):
		// one faulted attempt must not end the poll or take the bot down.
		if b.verifyUSDTTxidSafe(ctx, chatID, chain, des, txhash, true) {
			return // terminal (settled / window / used)
		}
	}
	// Window elapsed: one last, NON-quiet attempt so the customer gets a final
	// word (@9872, guarded the same way at @9868).
	b.verifyUSDTTxidSafe(ctx, chatID, chain, des, txhash, false)
}

// ---- verify-by-hash engine --------------------------------------------------

// verifyUSDTTxid verifies a customer-submitted on-chain tx hash for order `des`
// and settles it — _verify_usdt_txid (telegram_bot.py:9652-9843).
//
// PAY-for-order: the UNIQUE per-order amount binds a payment to exactly one
// order; the [start, click] window + the processed-ledger PK + confirmed-only
// complete the proof.
// DEPOSIT: also a picked amount (the request plus the 0.01000-0.01999 jitter),
// but no claim-jack band gate — a deposit credits the SUBMITTER's own wallet, so
// it is credited the actual received amount, or the requested total when that is
// larger and the receipt is within DEPOSIT_MATCH_TOL_UNITS (bands.DepositCredit);
// the window + confirmed + hash-dedupe are the deposit's defenses. tx.from is
// logged for forensics, NEVER used as an auth gate.
//
// `quiet` (confirmation-poll retries) suppresses the "not found / not confirmed
// yet" messages so the background poll can keep re-checking silently; terminal
// outcomes always message.
// `redirected` is the DEPTH-1 guard: a verify rerouted from a sibling session
// never redirects again.
//
// Returns true if TERMINAL (clear pending_txid), false to RE-ARM / keep polling.
func (b *Bot) verifyUSDTTxid(ctx context.Context, chatID int64, chain, des, txhash string, quiet, redirected bool) bool {
	pc := &i18n.C().Pay
	p, ok := providerForChain(chain)
	if !ok {
		return true
	}
	o, err := b.store.GetOrder(des)
	lang := b.langFor(chatID, "en")
	if o != nil {
		lang = langSafe(o)
	}
	// Ownership/existence: a des can leak, so only the owning chat may settle it.
	if err != nil || o == nil || o.ChatID != fmtInt(chatID) {
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return true
	}
	isDeposit := o.Product == depositProduct

	// ONE locked read for the expected amount. Snapshot also returns the sibling
	// amounts (Python's `other_amounts`, taken in the same `with lock` block at
	// @9685-9689), but this port does NOT use that stale list: the claim-jack gate
	// and the sibling redirect below each take their own fresh locked read AFTER
	// the (seconds-long) chain lookup. Fresher is the money-safe direction here —
	// the gate's `false` is the ACCEPT side of a refusal, so a session reserved
	// during the lookup should be able to block, not be invisible. Bands are spaced
	// >= 2*tol+1 apart, so a fresh row can never turn a correct payment into a
	// false conflict, and a same-chat row is caught by the redirect first.
	res, _, live := b.bands.Snapshot(p, des)
	if !live {
		// Session amount gone (settled/expired/swept) -> nothing to verify against.
		b.send(ctx, chatID, i18n.C().Buy.OrderGone.T(lang), nil)
		return true
	}
	expected := res.Amount
	prm, _ := b.bands.Params(p)
	row, _ := b.getUSDTRow(p, des)
	// Window LOWER bound = order creation (the earliest a payment for it could
	// exist). UPPER bound = the "I've paid" tap; lost across a restart -> now
	// (still safe — the unique amount is the real binding, @9676-9682).
	winLo := o.TS
	tClick := row.TClick
	if tClick == 0 {
		tClick = float64(b.now().UnixNano()) / 1e9
	}
	grace := b.usdtVerifyGrace.Seconds()

	// ---- fetch the ONE confirmed transfer named by the hash ------------------
	var (
		txID      string
		txTS      float64 // epoch seconds
		recvUnits int64
		recv      *big.Rat
		frm       string
	)
	switch p {
	case bands.ProviderBEP20:
		tx, lerr := b.bep20ByHash(txhash)
		if lerr != nil {
			log.Printf("usdt verify (%s) lookup error for %s: %v", chain, des, lerr)
			return b.txidNotFound(ctx, chatID, lang, quiet) // transient -> let them retry
		}
		if tx == nil || !tx.Confirmed {
			return b.txidNotFound(ctx, chatID, lang, quiet) // re-arm: maybe not confirmed yet
		}
		txID, txTS, recvUnits, recv, frm = tx.ID, float64(tx.TS), tx.Units, tx.Amount, tx.Frm
	default: // TRON
		minTSMS := int64((winLo - grace) * 1000)
		tx, lerr := b.tronByHash(txhash, minTSMS)
		if lerr != nil {
			log.Printf("usdt verify (%s) lookup error for %s: %v", chain, des, lerr)
			return b.txidNotFound(ctx, chatID, lang, quiet)
		}
		if tx == nil {
			return b.txidNotFound(ctx, chatID, lang, quiet)
		}
		txID, txTS, recvUnits, frm = tx.ID, float64(tx.TS)/1000.0, tx.Units, tx.Frm
		recv = new(big.Rat).SetFrac(tx.Value, pow10BigInt(tron.TokenDecimals))
	}
	// Rendered at the RAIL's precision, not a hardcoded 2 dp. Bands are now
	// 0.00101 USDT apart, so a 2-dp render collapses ten distinct slots onto one
	// string — it would show "120.01" for every one of them, in the customer's
	// amount-mismatch message and in every audit line an operator would use to
	// reconstruct which order a payment was meant for.
	recvStr := recv.FloatString(prm.Decimals)
	chatStr := fmtInt(chatID)
	ledgerID := ledgerPrefix(p) + ":" + txID

	// ---- freshness window: creation - grace <= tx_ts <= click + grace --------
	if txTS < winLo-grace || txTS > tClick+grace {
		// Before the terminal reject: the tx may have been paid for ANOTHER of this
		// SAME chat's sessions (during THAT session's window) and merely submitted
		// from the newest screen — the dominant prod pattern after an expired-order
		// retry. Redirect once; the recursion re-applies the ownership check and
		// re-computes the window against the sibling order (@9724-9734).
		if sib, okSib := b.bands.ResolveRedirect(p, chatStr, des, recvUnits, redirected); okSib {
			b.audit(chatStr, "usdt_"+chain+"_redirect", ledgerID,
				"recv="+recvStr+" rerouted to own sibling order (window)",
				"from="+frm+" des="+des+" -> "+sib)
			return b.verifyUSDTTxid(ctx, chatID, chain, sib, txhash, quiet, true)
		}
		b.audit(chatStr, "usdt_"+chain+"_reject", ledgerID, "outside_window",
			fmt.Sprintf("from=%s recv=%s tx_ts=%d lo=%d click=%d",
				frm, recvStr, int64(txTS), int64(winLo), int64(tClick)))
		b.send(ctx, chatID, pc.TxidWindow.T(lang), nil)
		return true
	}

	matched, merr := bands.RecvMatchesBand(prm, expected, recvUnits)
	if merr != nil { // an unparseable stored amount must never read as "matched"
		log.Printf("usdt verify (%s) band compare failed for %s: %v", chain, des, merr)
		matched = false
	}
	recvVND, verr := b.usdtToVNDFor(p, recv)
	if verr != nil {
		// No usable USD_TO_VND rate -> we cannot value the credit. Crediting 0
		// (or delivering against an unpriced payment) would lose the customer's
		// money; re-arm instead so a fixed rate settles it on the next attempt.
		//
		// Since task #22 the rate resolves through b.usdRateStr()
		// (config.Config.USDToVND), which always answers something — an absent
		// USD_TO_VND now falls back to config.DefaultUSDToVND rather than
		// erroring here, which is the point: quoting at the compiled default
		// while refusing to credit at it was the worse half of that bug.
		//
		// What can still reach this branch, traced through USDToVND rather
		// than assumed: a hand-edited settings.json usd_to_vnd that is
		// negative, or positive but under 1 and so truncated to "0"; or, when
		// settings.json carries no usable value, an environment-snapshot
		// USD_TO_VND that is non-numeric, zero, or negative. A NON-NUMERIC
		// settings.json value does NOT reach here — truthyFloat reads it as 0
		// and USDToVND falls through to the env/default instead.
		log.Printf("usdt verify (%s) rate error for %s: %v", chain, des, verr)
		return b.txidNotFound(ctx, chatID, lang, quiet)
	}

	// ---- settle: DEPOSIT ----------------------------------------------------
	if isDeposit {
		if !b.settleUSDTMatch(ctx, p, des, ledgerID, recvUnits, recvVND, o, lang, chatID, prm, expected, frm, recvStr) {
			return b.txidNotFound(ctx, chatID, lang, quiet) // nothing moved -> retry
		}
		return true
	}

	// ---- settle: PAY order --------------------------------------------------
	if matched {
		// Dedupe-only (the on-chain transfer pays for the accounts directly; no
		// wallet credit), THEN deliver. MarkTxn's processed-PK is the atomic gate ->
		// one hash delivers once even across racing submits (@9773-9776).
		isNew, kerr := b.store.MarkTxn(ledgerID, chatStr, recvVND)
		if kerr != nil {
			log.Printf("usdt mark_txn failed [%s]: %v", ledgerID, kerr)
			return b.txidNotFound(ctx, chatID, lang, quiet) // nothing consumed -> retry
		}
		if !isNew {
			b.send(ctx, chatID, pc.TxidAlreadyUsed.T(lang), nil)
			return true
		}
		settledClean := true
		if _, serr := b.settlePaid(ctx, des); serr != nil {
			// Mirror the QR worker: if NOTHING moved (still 'reserved'), release the
			// dedupe claim, KEEP the pending row, and re-arm so a retry settles; if
			// money already moved, keep the mark (re-settling would double-pay) and
			// flag for manual review (@9782-9797).
			o2, _ := b.store.GetOrder(des)
			if o2 != nil && o2.Status == "reserved" {
				if uerr := b.store.UnmarkTxn(ledgerID); uerr != nil {
					log.Printf("usdt unmark_txn failed [%s]: %v", ledgerID, uerr)
				}
				log.Printf("usdt %s: settle failed pre-claim for %s, will retry: %v", chain, des, serr)
				return b.txidNotFound(ctx, chatID, lang, quiet) // re-arm: keep the row
			}
			settledClean = false
			log.Printf("usdt %s: MANUAL REVIEW %s %s %sUSDT — settle raised after money moved (status=%s): %v",
				chain, des, ledgerID, recvStr, orderStatus(o2), serr)
		}
		b.dropUSDTPending(p, des) // only after a settled (or money-moved) outcome
		outcome := recvStr + " USDT"
		if !settledClean {
			outcome = recvStr + " USDT — settle raised after money moved (MANUAL REVIEW)"
		}
		b.audit(chatStr, "usdt_"+chain+"_pay", ledgerID, outcome, "from="+frm+" des="+des)
		return true
	}

	// ---- mismatch -----------------------------------------------------------
	// FIRST: if the amount lands in the band of ANOTHER pending session owned by
	// THIS SAME chat, it is not a claim-jack — the customer paid one of their OWN
	// earlier sessions and submitted the hash from the newest screen. Redirect:
	// ownership is identical, hash-dedupe still applies, and settling an expired
	// sibling refunds to their wallet (@9812-9821).
	if sib, okSib := b.bands.ResolveRedirect(p, chatStr, des, recvUnits, redirected); okSib {
		b.audit(chatStr, "usdt_"+chain+"_redirect", ledgerID,
			"recv="+recvStr+" rerouted to own sibling order",
			"from="+frm+" des="+des+" -> "+sib)
		return b.verifyUSDTTxid(ctx, chatID, chain, sib, txhash, quiet, true)
	}
	// Cross-chat band match: refuse (claim-jack defense — the payment may belong to
	// another customer's session). Same-provider comparison, matching Python
	// (@9824-9829). An error here is the ACCEPT side of a money refusal, so it
	// fails CLOSED: treat it as a conflict rather than crediting.
	conflict, cerr := b.bands.RecvMatchesAnyOtherBand(p, des, recvUnits)
	if cerr != nil {
		log.Printf("usdt verify (%s) claim-jack gate error for %s: %v", chain, des, cerr)
		conflict = true
	}
	if conflict {
		b.audit(chatStr, "usdt_"+chain+"_reject", ledgerID, "amount_conflict",
			"from="+frm+" recv="+recvStr+" want="+expected)
		b.send(ctx, chatID, pc.TxidAmountConflict.T(lang), nil)
		return true
	}
	// Genuine self-misfund: bank it to the wallet and leave the order open to pay
	// from balance (@9830-9840).
	credited, bal, crerr := b.store.Credit(chatStr, recvVND, ledgerID)
	if crerr != nil {
		log.Printf("usdt mismatch credit failed [%s]: %v", ledgerID, crerr)
		return b.txidNotFound(ctx, chatID, lang, quiet)
	}
	if !credited {
		b.send(ctx, chatID, pc.TxidAlreadyUsed.T(lang), nil)
		return true
	}
	b.audit(chatStr, "usdt_"+chain+"_pay_mismatch", ledgerID, "+"+recvStr+" USDT to wallet",
		fmt.Sprintf("from=%s want=%s balance=%d", frm, expected, bal))
	b.send(ctx, chatID, i18n.F(pc.TxidPayMismatch.T(lang),
		"recv", recvStr, "want", expected, "balance", b.usdtDisp(bal)), nil)
	return true
}

// settleUSDTMatch credits a CONFIRMED on-chain payment for order `des` and
// resolves the deposit session. This is the ONE settle path shared by the
// customer-pasted-hash flow (verifyUSDTTxid's deposit branch, above) and the
// (future) on-chain watcher that will spot a payment without any hash paste —
// both must dedupe and notify identically, so neither gets its own copy of
// this logic.
//
// recvVND/frm/recvStr are passed through rather than recomputed here: they
// come from the specific on-chain transfer the caller looked up (recvVND via
// the rail's own USD_TO_VND conversion of the tx's exact `big.Rat` amount,
// frm/recvStr straight off the tx), and re-deriving them from recvUnits alone
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
// credited (bad stored amount, or the credit call itself errored). On false
// the caller decides what to tell the customer — verifyUSDTTxid re-arms via
// txidNotFound; a silent watcher would just retry on the next tick.
func (b *Bot) settleUSDTMatch(ctx context.Context, p bands.Provider, des, ledgerID string, recvUnits, recvVND int64, o *store.Order, lang i18n.Lang, chatID int64, prm bands.Params, expected, frm, recvStr string) bool {
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
	creditVND, _ := bands.DepositCredit(recvUnits, desiredUnits, o.Total, recvVND,
		bands.ClampDepositTol(prm,
			b.envInt("DEPOSIT_MATCH_TOL_UNITS", bands.DefaultDepositMatchTolUnits)))
	credited, bal, cerr := b.store.Credit(chatStr, creditVND, ledgerID) // atomic dedupe + credit
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
	// auditAmount (money.go) guarantees it can never render "+ USDT"
	// with the amount silently missing; see its doc comment for why,
	// and for why this particular call site's invalid-rate branch is
	// actually unreachable today (the caller's usdtToVNDFor gate already
	// excludes it before recvVND ever reaches here) but kept anyway as
	// by-construction protection rather than per-site (task #15 — this
	// call site used to hand-guard the same gap, now collapsed into the
	// shared helper).
	creditDesc := b.auditAmount("+", creditVND)
	b.audit(chatStr, "usdt_"+chain+"_deposit", ledgerID,
		creditDesc,
		fmt.Sprintf("from=%s recv=%s balance=%d", frm, recvStr, bal))
	b.send(ctx, chatID, i18n.F(i18n.C().Deposit.OK.T(lang),
		"amount", b.usdtDisp(creditVND), "balance", b.usdtDisp(bal)), nil)
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
	b.notifyAdminDeposit(ctx, chatID, creditVND, bal, method)
	return true
}

// txidNotFound sends TXID_NOTFOUND unless the caller is a quiet retry, and always
// reports NON-terminal so the capture re-arms / the poll keeps checking.
func (b *Bot) txidNotFound(ctx context.Context, chatID int64, lang i18n.Lang, quiet bool) bool {
	if !quiet {
		b.send(ctx, chatID, i18n.C().Pay.TxidNotFound.T(lang), nil)
	}
	return false
}

// usdtToVNDFor values a received on-chain amount in VND using the rail's own
// converter (bep20_pay.usdt_to_vnd / tron_pay.usdt_to_vnd).
func (b *Bot) usdtToVNDFor(p bands.Provider, recv *big.Rat) (int64, error) {
	if p == bands.ProviderBEP20 {
		return b.bep20UsdtToVND(recv)
	}
	return b.tronUsdtToVND(recv)
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
// unexpected failure logs and the loop keeps going: the poller's per-cycle
// `except ... continue` (telegram_bot.py:9243-9245), _poll_usdt_confirm's
// per-attempt except (@9862, @9868), _startup_recovery's per-step excepts
// (@10001-10019) and the sweep-loop except (@10022), and the txid handler's
// `except -> terminal = False` (@19141-19143).
//
// Go's error returns only cover the failures a callee chose to name. The Python
// guards ALSO catch programming faults, and in Go those are panics — which,
// untracked, unwind straight out of the goroutine and kill the WHOLE process.
// On the settle path that is the worst possible failure mode: a single nil-deref
// under settlePaid/deliverPaid takes down all three rails, and because
// StartupRecovery replays the same pending row on the next boot it becomes a
// crash loop. The update workers already recover for exactly this reason
// (cmd/bot/main.go "one bad update can't kill a worker"); these goroutines did
// not. The two helpers below restore that parity at the same granularity Python
// retries at.

// safeStep runs one recovery/sweep step with the panic guard Python's per-step
// `except Exception` provides. A faulted step is logged and SKIPPED; the caller
// continues with the next step / the next tick.
func safeStep(tag string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			// Value suppressed, stack kept — same policy as otp.go:465-470.
			// safeStep is generic: `fn` is any poller step, including ones that
			// credit balances and deliver purchased goods, so the recovered
			// value can be built from a credential-holding local. `tag` is a
			// caller-supplied constant and is safe to print.
			log.Printf("%s: recovered from panic (payload suppressed)\n%s", tag, debug.Stack())
		}
	}()
	fn()
}

// verifyUSDTTxidSafe is verifyUSDTTxid with the panic guard of Python's verify
// call sites (@9862/@9868 in the confirm poll, @19141 in the txid capture).
//
// A faulted attempt reports NON-terminal, which is the money-safe direction: the
// capture re-arms / the background poll keeps checking, so the customer's hash is
// never silently dropped by a fault that moved nothing.
func (b *Bot) verifyUSDTTxidSafe(ctx context.Context, chatID int64, chain, des, txhash string, quiet bool) (terminal bool) {
	defer func() {
		if r := recover(); r != nil {
			// Value suppressed, stack kept — same policy as otp.go:465-470.
			// chain and des are non-secret identifiers and stay, so the log line
			// still says WHICH order faulted.
			log.Printf("usdt verify (%s) panic for %s (payload suppressed)\n%s", chain, des, debug.Stack())
			terminal = false // nothing consumed -> let the caller retry
		}
	}()
	return b.verifyUSDTTxid(ctx, chatID, chain, des, txhash, quiet, false)
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
// BEP20 and TRON only LOAD their pending rows — verify is customer-driven
// by-hash, so there is no poller and no reconcile. (Binance Pay used to
// additionally rehydrate AND reconcile through its own shared poller; that
// rail was removed by the USDT-only migration, S3.) Then a light expiry
// sweep keeps running so a restart-orphaned checkout still expires and its
// band is eventually freed.
func (b *Bot) StartupRecovery(ctx context.Context) {
	b.workers.Go(func(stop <-chan struct{}) {
		// Each step is panic-isolated exactly as Python isolates them
		// (@10001-10019): a fault rehydrating one rail must not stop the others,
		// and must never take the bot down at boot — StartupRecovery replays the
		// same persisted rows on the next start, so an unguarded panic here is a
		// crash loop, not a one-off.
		safeStep("startup recovery (bep20)", func() { b.rehydrateOnchainPending(bands.ProviderBEP20) })
		safeStep("startup recovery (tron)", func() { b.rehydrateOnchainPending(bands.ProviderTron) })
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
// band registry (no poller, no settle) so (a) the unique-amount picker keeps
// avoiding collisions with sessions that predate the restart and (b) a customer
// can still submit their hash — including a LATE submit on an order that already
// expired but is within usdtPendingGrace. Rows whose order vanished or is past
// the grace are pruned (_rehydrate_bep20_pending @9527 / _rehydrate_tron_pending
// @9965).
//
// t_start comes from the row's write time; t_click is unknown post-restart, so
// the verify window upper-bounds at 'now' (safe — the unique amount is the
// binding).
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
		if aerr := b.adoptPersisted(p, r.des, r.chatID, r.amount, r.total, r.sinceMS, r.ts, r.deadline); aerr != nil {
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
func (b *Bot) adoptPersisted(p bands.Provider, des, chatID, amount string, total, sinceMS int64, ts, deadline float64) error {
	if err := b.bands.Adopt(bands.Reservation{
		Provider: p, Des: des, ChatID: chatID, Amount: amount,
		CreatedAt: tsToTime(ts), Deadline: tsToTime(deadline),
	}); err != nil {
		return err
	}
	b.putUSDTRow(p, des, usdtRow{Total: total, SinceMS: sinceMS, TStart: ts})
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
