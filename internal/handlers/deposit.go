package handlers

// deposit.go — the M2.3 deposit flow (SePay VietQR only), the order-pay-by-QR
// rail, and the shared settle chokepoint. Ports:
//   - start_deposit @10592 (the funding-method picker),
//   - the dep:* callbacks @15494-15556,
//   - start_deposit_qr @10617 + deposit_worker @10559,
//   - the pending_deposit amount capture @18809,
//   - payqr: @15627 + qr_payment_worker @9001,
//   - settle_paid @8932 (reused by the QR order-pay path; PayPal/Binance/on-chain
//     rails that also call it are M2.4).
//
// The PayPal deposit START (start_paypal_deposit @10661) landed in M2.4 and
// lives in paypal.go; startDepositMethod routes "pp" there. The Binance/BEP20/
// TRON deposit starts remain documented stubs until their own slice (the picker
// already renders their buttons per the admin toggles, for parity).

import (
	"context"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/binance"
	"codezdev-bot/internal/pay/sepay"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// registerDeposit attaches the M2.3 capture + callback routes. pending_deposit is
// capture #44 (handlers-go.md §2.2) — it must sit BEFORE pending_purchase (#48,
// registered by registerCatalog), matching the customer priority comment @3632
// (deposit > … > purchase), so it is PREPENDED to the chain.
func (b *Bot) registerDeposit() {
	b.captureChain = append([]stage{{name: "deposit", fn: (*Bot).capDeposit}}, b.captureChain...)
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "dep:", fn: (*Bot).cbDep},   // @15494 deposit method picks
		cbRoute{key: "hist:", fn: (*Bot).cbHist}, // @12293 history range toggle
		cbRoute{key: "redl:", fn: (*Bot).cbRedl}, // @12299 re-download order file
	)
}

// ---- deposit amount bounds (MIN/MAX_DEPOSIT_USDT @3760-3765) ----------------

// envFloat reads a float env var via config, falling back to def.
func (b *Bot) envFloat(key string, def float64) float64 {
	if b.cfg == nil {
		return def
	}
	v := strings.TrimSpace(b.cfg.Env(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func (b *Bot) minDepositUSDT() float64 { return b.envFloat("MIN_DEPOSIT_USDT", 1) }
func (b *Bot) maxDepositUSDT() float64 { return b.envFloat("MAX_DEPOSIT_USDT", 100000) }

// minDepositStr is MIN_DEPOSIT_STR (telegram_bot.py:3761): the display form of
// the minimum, e.g. "1 USDT". %g drops a trailing ".0" like Python's :g.
func (b *Bot) minDepositStr() string {
	return strconv.FormatFloat(b.minDepositUSDT(), 'g', -1, 64) + " USDT"
}

// usdtToVND converts a typed USDT amount to the VND stored/charged internally,
// at the live rate — usdt_to_vnd (telegram_bot.py:18816). 0 when the rate is
// unset/invalid (the amount then fails the >0 bound and is rejected).
func (b *Bot) usdtToVND(usdt float64) int64 {
	rateStr := config.DefaultUSDToVND
	if b.cfg != nil {
		rateStr = b.cfg.USDToVND()
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || rate <= 0 {
		return 0
	}
	// int(round(...)) — usdt_to_vnd (@10569) rounds, not truncates, so the stored
	// VND matches to the exact đồng.
	return int64(math.Round(usdt * rate))
}

// groupVND formats a VND amount with thousands separators — the Go stand-in for
// Python's `{n:,}` format spec (i18n.F does no format spec), so QR_UNDERPAID /
// PURCHASE_QR read "1,234,567" exactly like the Python source.
func groupVND(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteByte(s[i])
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

// ---- deposit method picker (start_deposit @10592) --------------------------

// cmdDeposit renders the funding-method picker — start_deposit (telegram_bot.py:
// 10592-10614). Only admin-ENABLED methods show (same toggles as the buy screen);
// binance/bep20/tron additionally need their config set. No method enabled ->
// PAY_METHOD_OFF.
func (b *Bot) cmdDeposit(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Deposit
	var rows [][]tg.InlineKeyboardButton
	if b.payEnabled("qr") {
		rows = append(rows, btnRow(c.BtnQR.T(lang), "dep:qr"))
	}
	if b.payEnabled("paypal") {
		rows = append(rows, btnRow(c.BtnPayPal.T(lang), "dep:pp"))
	}
	if b.payEnabled("binance") && binance.Configured() {
		rows = append(rows, btnRow(c.BtnBinance.T(lang), "dep:bnb"))
	}
	if b.payEnabled("bep20") && bep20.Configured() {
		rows = append(rows, styledBtnRow(c.BtnBEP20.T(lang), "dep:bsc", "primary"))
	}
	if b.payEnabled("tron") && tron.Configured() {
		rows = append(rows, styledBtnRow(c.BtnTRON.T(lang), "dep:trx", "danger"))
	}
	if len(rows) == 0 { // every funding method disabled
		b.send(ctx, chatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
		return
	}
	b.sendMenu(ctx, chatID, c.Method.T(lang), rows)
}

// cbDep dispatches a deposit-method tap — the dep:* branches (telegram_bot.py:
// 15494-15556). dep:qr runs the SePay flow immediately (no amount step); the
// other methods clear sibling captures, arm pending_deposit with the method and
// prompt for the USDT amount (the pending_deposit capture converts + routes).
func (b *Bot) cbDep(cc *cbCtx) {
	lang := b.langFor(cc.ChatID, "en")
	c := &i18n.C().Deposit
	method := strings.TrimPrefix(cc.Data, "dep:")

	switch method {
	case "qr":
		// VietQR deposit (variable amount; SePay detects the transfer) @15494.
		if !b.payEnabled("qr") { // guard a stale button
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.startDepositQR(cc.Ctx, cc.ChatID, lang)
		return
	case "pp":
		if !b.payEnabled("paypal") {
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.armDeposit(cc, "pp", i18n.F(c.PPAmount.T(lang), "min", b.minDepositStr()))
	case "bnb":
		// Binance/on-chain: the button only renders when configured; a stale tap
		// on an unconfigured method degrades to PAY_METHOD_OFF (the NOID notices
		// land with M2.4).
		if !b.payEnabled("binance") || !binance.Configured() {
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.armDeposit(cc, "bnb", i18n.F(c.PPAmount.T(lang), "min", b.minDepositStr()))
	case "bsc":
		if !b.payEnabled("bep20") || !bep20.Configured() {
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.armDeposit(cc, "bsc", i18n.F(c.BEP20Amount.T(lang), "min", b.minDepositStr()))
	case "trx":
		if !b.payEnabled("tron") || !tron.Configured() {
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.armDeposit(cc, "trx", i18n.F(c.TronAmount.T(lang), "min", b.minDepositStr()))
	}
}

// armDeposit clears sibling customer captures and arms pending_deposit with the
// funding method, then sends the amount prompt (the shared tail of the dep:pp /
// bnb / bsc / trx branches @15508-15556).
func (b *Bot) armDeposit(cc *cbCtx, method, prompt string) {
	cc.st.ClearCustomerPending() // drop stale sibling captures first
	cc.st.mu.Lock()
	cc.st.Deposit = &DepositCtx{Method: method}
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, prompt, nil)
}

// ---- SePay VietQR deposit (start_deposit_qr @10617 + deposit_worker @10559) --

// startDepositQR sends the VietQR image and starts the background SePay poll —
// start_deposit_qr (telegram_bot.py:10617-10624). No order row is created for a
// QR deposit (Python parity): the worker credits the wallet directly on a match.
func (b *Bot) startDepositQR(ctx context.Context, chatID int64, lang i18n.Lang) {
	des := store.NewDes() // 32-hex, survives bank content mangling (@10619)
	qrURL := sepay.BuildQRURL(des, nil) // amount=None -> free top-up
	if _, err := b.tg.SendPhotoURL(ctx, chatID, qrURL,
		i18n.F(i18n.C().Deposit.QR.T(lang), "des", des)); err != nil {
		log.Println("deposit QR sendPhoto failed:", err)
	}
	b.depositWorker(chatID, lang, des)
}

// depositWorker polls SePay every depositPoll up to expiryTimeout for a transfer
// matching `des` — deposit_worker (telegram_bot.py:10559-10589). On a match it
// credits the wallet ONCE (store.Credit dedupes by SePay txn id) and confirms; on
// timeout it sends the support-contact notice. Registry-tracked so Shutdown drains
// it; the request ctx is long gone so it runs on context.Background.
//
// Deviation note: Python guards the timeout message behind
// claim_order(des, "reserved", "expired"), but a QR deposit creates NO order row
// (@10617), so that claim is always a no-op and Python is silent on QR timeout.
// We send DEPOSIT_TIMEOUT directly: a QR deposit has no competing settle path
// (PayPal/Binance are their own des), so an exhausted poll unambiguously means
// "not paid" — matching deposit_worker's documented intent and the slice's
// timeout→support test.
func (b *Bot) depositWorker(chatID int64, lang i18n.Lang, des string) {
	b.workers.Go(func(stop <-chan struct{}) {
		ctx := context.Background()
		deadline := time.Now().Add(b.expiryTimeout)
		for time.Now().Before(deadline) {
			select {
			case <-stop:
				return // shutdown before the window elapsed
			case <-time.After(b.depositPoll):
			}
			txn, err := b.sepayFind(des, nil)
			if err != nil { // network/API blip: keep polling
				log.Println("sepay poll error:", err)
				continue
			}
			if txn == nil {
				continue
			}
			credited, balance, cerr := b.store.Credit(fmtInt(chatID), txn.Amount, txn.ID)
			if cerr != nil {
				log.Println("deposit credit failed:", cerr)
				return
			}
			if credited {
				b.send(ctx, chatID, i18n.F(i18n.C().Deposit.OK.T(lang),
					"amount", b.usdtDisp(txn.Amount), "balance", b.usdtDisp(balance)), nil)
				// notify_admin_deposit (@10580) is M3 — admin push notifications.
			}
			return // matched (or already credited) -> stop polling
		}
		// Loop exhausted -> timed out (see the deviation note above).
		b.send(ctx, chatID, i18n.C().Buy.DepositTimeout.T(lang), nil)
	})
}

// ---- pending_deposit amount capture (#44, @18809) --------------------------

// capDeposit is capture-chain entry #44, pending_deposit — the deposit-amount
// free-text capture (telegram_bot.py:18809-18830). PEEK semantics: an escaping
// text (a "/command" or a keyboard label) leaves the slot armed and falls through
// (returns false); a valid USDT amount within [min,max] pops the method and routes
// to its start; an out-of-bounds amount is answered with DEPOSIT_BAD_AMOUNT and
// the slot STAYS armed for a retry (returns true).
func (b *Bot) capDeposit(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Deposit != nil
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	// peek escape: a slash command or a tapped keyboard label leaves the slot
	// armed and falls through to the command router (@18809's `and` guards).
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	dc := &i18n.C().Deposit
	raw := strings.ReplaceAll(strings.TrimSpace(c.Text), ",", "") // keep the decimal point
	usdt, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		usdt = 0
	}
	if usdt >= b.minDepositUSDT() && usdt <= b.maxDepositUSDT() {
		vnd := b.usdtToVND(usdt)
		// Atomically CLAIM the armed method (double-submit guard) then route.
		c.st.mu.Lock()
		dc := c.st.Deposit
		c.st.Deposit = nil
		c.st.mu.Unlock()
		if dc == nil {
			// Lost the claim to a concurrent duplicate message — the 16-worker pool
			// does not serialize per chat (handlers-go.md §8), so two copies of the
			// same amount can reach here together. Python defaulted the loser to
			// "pp" (`pending_deposit.pop(chat_id, "pp")`), which now that "pp"
			// actually mints a PayPal order would either start a SECOND deposit or
			// start the WRONG rail for a chat that armed trx/bsc. Dropping the loser
			// is the correct double-submit guard and matches every other atomic pop
			// in this port.
			return true
		}
		b.startDepositMethod(c.Ctx, c.ChatID, c.Lang, dc.Method, vnd)
	} else {
		b.send(c.Ctx, c.ChatID, i18n.F(dc.BadAmount.T(c.Lang), "min", b.minDepositStr()), nil)
	}
	return true
}

// startDepositMethod routes a validated deposit amount to its funding rail —
// the dispatch tail of the pending_deposit capture (telegram_bot.py:19040-19051).
// PayPal is start_paypal_deposit @10661 (paypal.go); the three USDT rails are
// start_binance_deposit @10627 / start_bep20_deposit @9478 / start_tron_deposit
// @9935 (usdt.go). QR never reaches this (it has no amount step).
func (b *Bot) startDepositMethod(ctx context.Context, chatID int64, lang i18n.Lang, method string, vnd int64) {
	switch method {
	case "pp":
		b.startPaypalDeposit(ctx, chatID, lang, vnd)
	case "bnb":
		b.startBinanceDeposit(ctx, chatID, lang, vnd)
	case "bsc":
		b.startBep20Deposit(ctx, chatID, lang, vnd)
	case "trx":
		b.startTronDeposit(ctx, chatID, lang, vnd)
	default:
		log.Printf("deposit method %q (vnd=%d) — unknown funding rail, ignored", method, vnd)
	}
}

// ---- order-pay-by-QR (payqr: @15627 + qr_payment_worker @9001) --------------

// cbPayQR shows the amount-pinned VietQR for a reserved ORDER and starts the SePay
// poller — payqr: (telegram_bot.py:15627-15644). Ownership-guarded (a des can leak
// in QR content). Unlike a deposit, the transfer pays for accounts and must NOT
// top up the wallet — qr_payment_worker settles via the shared claim path.
func (b *Bot) cbPayQR(cc *cbCtx) {
	des := strings.TrimPrefix(cc.Data, "payqr:")
	o, err := b.store.GetOrder(des)
	langEN := b.langFor(cc.ChatID, "en")
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(langEN), nil)
		return
	}
	lang := langSafe(o)
	if !b.payEnabled("qr") {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
		return
	}
	total := o.Total
	qrURL := sepay.BuildQRURL(des, &total) // amount-pinned QR
	if _, err := b.tg.SendPhotoURL(cc.Ctx, cc.ChatID, qrURL,
		i18n.F(i18n.C().Deposit.PurchaseQR.T(lang), "total", groupVND(total), "des", des)); err != nil {
		log.Println("order-pay QR sendPhoto failed:", err)
	}
	b.qrPaymentWorker(des)
}

// qrPaymentWorker polls SePay up to the hard limit; on a matching transfer it
// settles the ORDER — qr_payment_worker (telegram_bot.py:9001-9056). Uses MarkTxn
// (dedupe only, NO wallet credit) so a QR purchase pays for accounts without
// topping up the wallet, then settles through the shared claim path (idempotent).
// An UNDERPAYMENT never settles: the partial is banked to the wallet (Credit,
// deduped by txn id) so it isn't lost, the customer is told, and polling continues
// for a sufficient transfer (is_used=TxnExists skips the banked partial). Stops
// early if the order left 'reserved'.
func (b *Bot) qrPaymentWorker(des string) {
	b.workers.Go(func(stop <-chan struct{}) {
		ctx := context.Background()
		deadline := time.Now().Add(b.expiryTimeout)
		for time.Now().Before(deadline) {
			select {
			case <-stop:
				return
			case <-time.After(b.depositPoll):
			}
			o, err := b.store.GetOrder(des)
			if err != nil {
				log.Println("qr worker get_order error:", err)
				continue
			}
			if o == nil || o.Status != "reserved" {
				return // handled elsewhere (paid another way / expired)
			}
			// Skip txns already consumed (settled or banked as an underpayment).
			txn, err := b.sepayFind(des, b.txnUsed)
			if err != nil {
				log.Println("sepay poll error:", err)
				continue
			}
			if txn == nil {
				continue
			}
			// AMOUNT GATE: never deliver on an underpayment (@9029). The VietQR
			// amount is only a prefill the payer can edit; a short transfer banks
			// to the wallet and keeps polling.
			if txn.Amount < o.Total {
				credited, balance, cerr := b.store.Credit(o.ChatID, txn.Amount, txn.ID)
				if cerr != nil {
					log.Println("qr underpay credit failed:", cerr)
					continue
				}
				if credited {
					chatID, _ := strconv.ParseInt(o.ChatID, 10, 64)
					b.send(ctx, chatID, i18n.F(i18n.C().Deposit.Underpaid.T(langSafe(o)),
						"paid", groupVND(txn.Amount), "total", groupVND(o.Total),
						"balance", groupVND(balance)), nil)
				}
				continue
			}
			// Dedupe the txn BEFORE settling (@9036) so a re-tapped order's second
			// worker can't double-settle the same transfer.
			isNew, merr := b.store.MarkTxn(txn.ID, o.ChatID, txn.Amount)
			if merr != nil {
				log.Println("qr mark_txn failed:", merr)
				continue
			}
			if !isNew {
				return // duplicate txn
			}
			// Settle through the shared claim path. If it errors BEFORE anything
			// moved (order still 'reserved'), release the txn mark and keep polling
			// so the next cycle retries; if money already moved, keep the mark and
			// flag for manual review (@9045-9054).
			if _, serr := b.settlePaid(ctx, des); serr != nil {
				o2, _ := b.store.GetOrder(des)
				if o2 != nil && o2.Status == "reserved" {
					if uerr := b.store.UnmarkTxn(txn.ID); uerr != nil {
						log.Println("qr unmark_txn failed:", uerr)
					}
					log.Printf("sepay settle failed pre-claim for %s, retrying: %v", des, serr)
					continue
				}
				log.Printf("sepay: MANUAL REVIEW %s txn=%s — settle errored after money moved: %v",
					des, txn.ID, serr)
			}
			return
		}
		// No payment in-window: the order_expiry_worker releases + messages (@9056).
	})
}

// settlePaid idempotently settles an order whose payment is CONFIRMED (money in)
// — settle_paid (telegram_bot.py:8932-8972). The reserved->paid claim is the
// single guard against double-delivery: only the winner delivers (or, for a
// __deposit__ order, credits the wallet). A payment that lands AFTER expiry
// refunds to the wallet so confirmed money is never dropped. Returns
// (settled, err); the QR worker uses err to decide whether to release its txn
// mark. Reuses deliverPaid (M2.2) for the folder-stock delivery — no duplicate
// settle logic. NO wallet debit here: the money arrived off-wallet (bank/PayPal).
func (b *Bot) settlePaid(ctx context.Context, des string) (bool, error) {
	won, err := b.store.ClaimOrder(des, "reserved", "paid")
	if err != nil {
		return false, err
	}
	if won != nil {
		if won.Product == depositProduct {
			// Wallet top-up (PayPal/Binance deposit): credit balance, don't deliver.
			if _, cerr := b.store.ClaimOrder(des, "paid", "delivered"); cerr != nil {
				log.Printf("deposit close-claim failed [des=%s]: %v", des, cerr)
			}
			newBal, cerr := b.store.CreditBalance(won.ChatID, won.Total)
			if cerr != nil {
				return false, cerr
			}
			chatID, _ := strconv.ParseInt(won.ChatID, 10, 64)
			b.send(ctx, chatID, i18n.F(i18n.C().Deposit.OK.T(langSafe(won)),
				"amount", b.usdtDisp(won.Total), "balance", b.usdtDisp(newBal)), nil)
			return true, nil
		}
		b.deliverPaid(ctx, won, des)
		return true, nil
	}
	// Lost the reserved->paid claim: the order already moved on. If it expired,
	// the payment landed after the hard limit released the rows -> refund (@8956).
	o, err := b.store.GetOrder(des)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, nil // unknown order
	}
	if o.Status == "expired" {
		refunded, err := b.store.ClaimOrder(des, "expired", "refunded")
		if err != nil {
			return false, err
		}
		if refunded != nil {
			newBal, cerr := b.store.CreditBalance(o.ChatID, o.Total)
			if cerr != nil {
				return false, cerr
			}
			chatID, _ := strconv.ParseInt(o.ChatID, 10, 64)
			if o.Product == depositProduct {
				b.send(ctx, chatID, i18n.F(i18n.C().Deposit.OK.T(langSafe(o)),
					"amount", b.usdtDisp(o.Total), "balance", b.usdtDisp(newBal)), nil)
			} else {
				b.send(ctx, chatID, i18n.F(i18n.C().Buy.PaidNoStock.T(langSafe(o)),
					"amount", b.usdtDisp(o.Total), "balance", b.usdtDisp(newBal)), nil)
			}
		}
	}
	// delivered / paid / refunded -> already handled, dedupe (do nothing).
	return false, nil
}

// txnUsed adapts store.TxnExists to sepay's isUsed predicate (drops the error;
// a read failure conservatively reports "not used" so a real match isn't hidden).
func (b *Bot) txnUsed(txnID string) bool {
	used, err := b.store.TxnExists(txnID)
	if err != nil {
		log.Println("txn_exists check failed:", err)
		return false
	}
	return used
}
