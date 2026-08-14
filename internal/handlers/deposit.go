package handlers

// deposit.go — the deposit flow (BEP20/TRON on-chain rails; SePay VietQR,
// PayPal and Binance Pay were removed by the USDT-only migration, S1-S3), the
// order-pay balance rail, and the shared settle chokepoint. Ports:
//   - start_deposit @10592 (the funding-method picker),
//   - the dep:* callbacks @15494-15556,
//   - the pending_deposit amount capture @18809,
//   - settle_paid @8932 (the shared claim path every USDT rail settles through).
//
// USDT migration note (2026-08-06): SePay/VietQR (start_deposit_qr,
// deposit_worker, payqr:, qr_payment_worker, groupVND), the PayPal deposit
// start (paypal.go) and Binance Pay (start_binance_deposit, the shared poller)
// were removed — the shop takes USDT via BEP20/TRON only. `settlePaid`
// survives as the shared claim path the surviving rails settle through.
import (
	"context"
	"log"
	"math"
	"strconv"
	"strings"

	"codezdev-bot/internal/i18n"
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

// usdtToMicro converts a typed USDT amount to micro-USDT (1 USDT = 1,000,000
// micro) — the stored/charged internal unit. No exchange rate, no failure: every
// representable float maps to an exact rounded micro value, so capDeposit's >0
// bound can only fail on a genuinely non-positive amount.
func (b *Bot) usdtToMicro(usdt float64) int64 {
	// int(round(...)) — round, not truncate, so the stored micro value matches
	// to the exact micro.
	return int64(math.Round(usdt * 1_000_000))
}

// ---- deposit method picker (start_deposit @10592) --------------------------

// cmdDeposit renders the funding-method picker — start_deposit (telegram_bot.py:
// 10592-10614). Only admin-ENABLED methods show (same toggles as the buy
// screen); bep20/tron additionally need their config set. No method enabled ->
// PAY_METHOD_OFF.
func (b *Bot) cmdDeposit(ctx context.Context, chatID int64, lang i18n.Lang) {
	c := &i18n.C().Deposit
	var rows [][]tg.InlineKeyboardButton
	if b.payEnabled("bep20") && b.bep20Configured() {
		rows = append(rows, styledBtnRow(c.BtnBEP20.T(lang), "dep:bsc", "primary"))
	}
	if b.payEnabled("tron") && b.tronConfigured() {
		rows = append(rows, styledBtnRow(c.BtnTRON.T(lang), "dep:trx", "danger"))
	}
	if len(rows) == 0 { // every funding method disabled
		b.send(ctx, chatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
		return
	}
	b.sendMenu(ctx, chatID, c.Method.T(lang), rows)
}

// cbDep dispatches a deposit-method tap — the dep:* branches (telegram_bot.py:
// 15494-15556). Each surviving method clears sibling captures, arms
// pending_deposit with the method and prompts for the USDT amount (the
// pending_deposit capture converts + routes).
func (b *Bot) cbDep(cc *cbCtx) {
	lang := b.langFor(cc.ChatID, "en")
	c := &i18n.C().Deposit
	method := strings.TrimPrefix(cc.Data, "dep:")

	switch method {
	case "bsc":
		if !b.payEnabled("bep20") || !b.bep20Configured() {
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.armDeposit(cc, "bsc", i18n.F(c.BEP20Amount.T(lang), "min", b.minDepositStr()))
	case "trx":
		if !b.payEnabled("tron") || !b.tronConfigured() {
			b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
			return
		}
		b.armDeposit(cc, "trx", i18n.F(c.TronAmount.T(lang), "min", b.minDepositStr()))
	default:
		// Residual fix (Opus review of 11e033d): a stale "dep:qr"/"dep:pp"/
		// "dep:bnb" tap — a picker rendered before the SePay/PayPal/Binance
		// removal (S1-S3) and left open in a customer's chat — has no
		// matching rail here. command.go:182 unconditionally deletes the
		// message that held the tapped buttons BEFORE this dispatches, so
		// silently falling through (the pre-fix behavior) left the customer
		// with NOTHING: no menu, no error, no way forward. Mirrors
		// cmdDeposit's own "every method disabled" reply just above.
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.PayMethodOff.T(lang), nil)
	}
}

// armDeposit clears sibling customer captures and arms pending_deposit with the
// funding method, then sends the amount prompt (the shared tail of the dep:bsc
// / trx branches @15508-15556).
func (b *Bot) armDeposit(cc *cbCtx, method, prompt string) {
	cc.st.ClearCustomerPending() // drop stale sibling captures first
	cc.st.mu.Lock()
	cc.st.Deposit = &DepositCtx{Method: method}
	cc.st.mu.Unlock()
	b.send(cc.Ctx, cc.ChatID, prompt, nil)
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
		micro := b.usdtToMicro(usdt)
		// Atomically CLAIM the armed method (double-submit guard) then route.
		c.st.mu.Lock()
		dc := c.st.Deposit
		c.st.Deposit = nil
		c.st.mu.Unlock()
		if dc == nil {
			// Lost the claim to a concurrent duplicate message — the 16-worker pool
			// does not serialize per chat (handlers-go.md §8), so two copies of the
			// same amount can reach here together. Dropping the loser is the
			// correct double-submit guard (starting a rail for the loser too would
			// either double-start a deposit or start the wrong rail for a chat
			// that armed a different method) and matches every other atomic pop in
			// this port.
			return true
		}
		b.startDepositMethod(c.Ctx, c.ChatID, c.Lang, dc.Method, micro)
	} else {
		b.send(c.Ctx, c.ChatID, i18n.F(dc.BadAmount.T(c.Lang), "min", b.minDepositStr()), nil)
	}
	return true
}

// startDepositMethod routes a validated deposit amount to its funding rail —
// the dispatch tail of the pending_deposit capture (telegram_bot.py:19040-19051).
// The two surviving USDT rails are start_bep20_deposit @9478 / start_tron_deposit
// @9935 (usdt.go). PayPal (formerly "pp") and Binance Pay (formerly "bnb") were
// removed by the USDT-only migration (S2/S3).
func (b *Bot) startDepositMethod(ctx context.Context, chatID int64, lang i18n.Lang, method string, micro int64) {
	switch method {
	case "bsc":
		b.startBep20Deposit(ctx, chatID, lang, micro)
	case "trx":
		b.startTronDeposit(ctx, chatID, lang, micro)
	default:
		log.Printf("deposit method %q (micro=%d) — unknown funding rail, ignored", method, micro)
	}
}

// settlePaid idempotently settles an order whose payment is CONFIRMED (money in)
// — settle_paid (telegram_bot.py:8932-8972). The reserved->paid claim is the
// single guard against double-delivery: only the winner delivers (or, for a
// __deposit__ order, credits the wallet). A payment that lands AFTER expiry
// refunds to the wallet so confirmed money is never dropped. Returns
// (settled, err); a caller that pre-deduped (e.g. MarkTxn before delivery) uses
// err to decide whether to release its dedupe mark. Reuses deliverPaid (M2.2)
// for the folder-stock delivery — no duplicate settle logic. NO wallet debit
// here: the money arrived off-wallet (on-chain).
func (b *Bot) settlePaid(ctx context.Context, des string) (bool, error) {
	won, err := b.store.ClaimOrder(des, "reserved", "paid")
	if err != nil {
		return false, err
	}
	if won != nil {
		if won.Product == depositProduct {
			// Wallet top-up (an on-chain USDT deposit): credit balance, don't deliver.
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
			// notify_admin_deposit (telegram_bot.py:9028-9034) — method falls
			// back to "Deposit" exactly like Python's `won.get("tier") or "Deposit"`.
			method := won.Tier
			if method == "" {
				method = "Deposit"
			}
			b.notifyAdminDeposit(ctx, chatID, won.Total, newBal, method)
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
		// Kind depends on WHICH product this is — the two branches below send
		// different messages and are different events, not one event with two
		// wordings (review finding on this slice):
		//
		//   __deposit__: late_settled (stats-fixes-spec.md §2.1) — the wallet is
		//   credited and Deposit.OK is sent below. A SUCCESS wearing a
		//   'refunded' status; must never count toward the customer-facing
		//   refund rate (§2.2).
		//
		//   any other product: the reserved stock was already released back at
		//   expiry (spawnOrderExpiry, purchase.go), so a payment landing after
		//   that has nothing to deliver against — money back, Buy.PaidNoStock
		//   sent below, IDENTICAL in kind to purchase.go's out_of_stock path
		//   (customer paid, stock gone, wallet credited). Tagging this
		//   late_settled would make a paid-not-delivered event invisible on
		//   every refund-kind bucket on the dashboard — the exact A2 defect
		//   this taxonomy exists to remove, reintroduced for product orders.
		kind := store.RefundKindLateSettled
		if o.Product != depositProduct {
			kind = store.RefundKindOutOfStock
		}
		refunded, err := b.store.ClaimOrderKind(des, "expired", "refunded", kind)
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
				// notify_admin_deposit (telegram_bot.py:9048-9053) — the
				// late-settled (post-expiry) twin of the fresh-claim notify above.
				method := o.Tier
				if method == "" {
					method = "Deposit"
				}
				b.notifyAdminDeposit(ctx, chatID, o.Total, newBal, method)
			} else {
				b.send(ctx, chatID, i18n.F(i18n.C().Buy.PaidNoStock.T(langSafe(o)),
					"amount", b.usdtDisp(o.Total), "balance", b.usdtDisp(newBal)), nil)
			}
		}
	}
	// delivered / paid / refunded -> already handled, dedupe (do nothing).
	return false, nil
}
