package handlers

// apikey_deliver.go — M2.5 slice, part 2: FULFILLMENT of a PAID API-key order.
//
// Ports _deliver_apikey (telegram_bot.py:7458-7673) and
// _deliver_apikey_recharge (telegram_bot.py:7676-7820). Both are reached from
// the ONE shared settle path (settlePaid -> deliverPaid), so every payment rail
// (balance, on-chain USDT) fulfills the virtual product through this single
// branch — there is no second settle path.
//
// The two money invariants this file exists to enforce:
//
//  1. MINT-ONCE: the api_keys row (PRIMARY KEY = order des) is persisted BEFORE
//     the paid->delivered status flip. A replayed settle finds the stored row
//     and RE-DELIVERS the SAME key instead of minting a second one; a process
//     crash between mint and flip can only cause a re-send, never a double mint.
//  2. RECHARGE-ONCE: apikey_recharges.applied_ts is stamped only AFTER Kiro-Go
//     confirms the top-up. A replayed settle sees the stamp and re-sends the
//     confirmation instead of adding the credits twice.
//
// Both leave their only crash window in the CUSTOMER's favour (a duplicate
// message, never lost money), exactly like the Python original.

import (
	"context"
	"errors"
	"html"
	"log"
	"math"
	"strconv"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/store"
)

// errNotConfigured stands in for Python's "kiro-go not configured" RuntimeError
// on the mint path — a defence-in-depth backstop only: apikeyForSale() already
// hides the product, so reaching this means the admin wiped the config between
// order creation and settle.
var errKiroGoUnconfigured = errors.New("kiro-go not configured (base URL / admin key / account id missing)")

// deliverApikey mints a metered key on Kiro-Go for a PAID order and delivers it
// — _deliver_apikey (telegram_bot.py:7458-7673). Returns true when the buyer
// ends up holding a key (fresh mint OR a replayed re-delivery).
//
// If minting fails after payment the money is refunded to the wallet balance
// exactly like the paid-but-no-stock path (never lost), and both the buyer and
// the owners are told.
func (b *Bot) deliverApikey(ctx context.Context, order *store.Order, des string) bool {
	c := &i18n.C().APIKey
	chatIDStr := order.ChatID
	chatID, _ := strconv.ParseInt(chatIDStr, 10, 64)
	lang := langSafe(order)
	credits, total := order.Qty, order.Total
	// The order's product sentinel IS its SKU: it fixes the mint payload
	// (routing partition + billing currency on the server) and every label
	// below. Fixed at order creation — never inferred from settings at deliver
	// time, so an admin toggling SKUs mid-flight can't reclass a paid order.
	saleClass := apikeyClassForProduct(order.Product)

	row, err := b.store.APIKeyForOrder(des)
	if err != nil {
		// A read failure here must NOT fall through to the mint branch: that
		// would treat "unknown" as "not yet minted" and could double-mint a
		// paid key. Leave the order 'paid' for the stuck-paid reconcile.
		log.Printf("CRITICAL apikey_for_order read failed [des=%s]: %v — order left 'paid', NOT minting on an unknown row", des, err)
		return false
	}
	if row == nil {
		minted, mintErr := b.mintAPIKey(ctx, chatID, des, credits, saleClass)
		if mintErr != nil {
			b.apikeyMintFailRefund(ctx, order, des, chatID, lang, credits, total, mintErr)
			return false
		}
		// UPSTREAM BINDING STATUS — "pending" IS SUCCESS. DO NOT "FIX" THIS.
		//
		// provision_customer also binds a per-customer upstream (llmgate) key to
		// the pool key. upstreamStatus is one of:
		//   "bound"   — upstream key created and linked.
		//   "pending" — llmgate's control plane was unreachable, so the upstream
		//               key does not exist YET. The POOL KEY WAS STILL CREATED,
		//               is fully working and billable, and forwards on the
		//               shared account credential until a background retry binds
		//               it.
		//   "skipped" — binding deliberately not attempted (ALWAYS the case for
		//               a kiro mint, which sends skipUpstream:true).
		// In ALL THREE cases the customer-facing outcome is identical: deliver
		// the key and complete the sale. Only a non-2xx (which surfaced as
		// mintErr above) means the sale actually failed. Retrying on "pending"
		// would mint a SECOND paid key for one sale and charge us for it — that
		// is the exact regression this comment exists to prevent.
		//
		// upstreamKeyId is logged for attribution: it is a NON-SECRET id. The
		// backend never returns the upstream KEY itself and the bot must never
		// receive, store, log or forward one — any llmgate key can read the
		// whole account's quota and spend.
		if minted.UpstreamStatusAnomalous(saleClass) || minted.UpstreamStatus != kirogo.UpstreamStatusBound {
			// Log only ANOMALOUS non-bound states: a kiro mint's "skipped" is its
			// normal terminal status, and logging it would put one warning-shaped
			// line on EVERY kiro sale (telegram_bot.py:7578-7584).
			if !(saleClass == saleClassKiro && minted.UpstreamStatus == kirogo.UpstreamStatusSkipped) {
				log.Printf("apikey mint upstream not bound [des=%s class=%s status=%s upstreamKeyId=%d] — pool key is live, delivering as normal",
					des, saleClass, minted.UpstreamStatus, minted.UpstreamKeyID)
			}
		}
		// Persist the cleartext key (WITH the SERVER'S echoed SKU) BEFORE
		// flipping the order status — invariant 1. If the process dies right
		// here, the next settle replay finds the row and re-delivers.
		//
		// The stored class is the SERVER'S ECHO, not the order's: it arms the
		// recharge 409 guard, and the server 409s any guard against a key it
		// recorded as unclassed ('' matches only an OMITTED guard). Echo absent
		// -> store '' -> the guard stays disarmed for exactly the keys the
		// server itself left unclassed (telegram_bot.py:7603-7614).
		if _, err := b.store.AddAPIKey(des, chatID, minted.ID, minted.Key,
			float64(credits), minted.SaleClass); err != nil {
			// The key EXISTS on Kiro-Go and the customer's money is spent, but we
			// could not record it. Do NOT refund (the key is real and billable)
			// and do NOT re-mint. Deliver from the in-memory response below and
			// page loudly: without the row, a replay would double-mint.
			log.Printf("CRITICAL apikey row persist failed [des=%s chat=%d keyId=%s]: %v — key IS minted; store the row by hand before any settle replay",
				des, chatID, minted.ID, err)
		}
		row, err = b.store.APIKeyForOrder(des)
		if err != nil || row == nil {
			// Fall back to the just-minted response so the buyer still receives
			// the key they paid for (never leave them empty-handed).
			row = &store.APIKey{OrderID: des, ChatID: chatID, KeyID: minted.ID,
				APIKey: minted.Key, Credits: float64(credits), SaleClass: minted.SaleClass}
		}
		// A BEDROCK sale that completed with NO upstream binding has no
		// per-customer attribution and no independent kill switch — usually the
		// configured account holds no llmgate credential. Alert, never block:
		// the customer's key is live and paid for. Fires AFTER the persist above
		// on purpose — anything slow between a committed mint and the api_keys
		// row reopens the double-mint window that row exists to close.
		// TODO(M3.1): route through _notify_owners once the owner-alert channel lands.
		if saleClass != saleClassKiro &&
			minted.UpstreamStatus != kirogo.UpstreamStatusBound &&
			minted.UpstreamStatus != kirogo.UpstreamStatusPending {
			log.Printf("OWNER ALERT: API-KEY SOLD WITHOUT UPSTREAM BINDING [des=%s upstreamStatus=%s] — the key works and the sale is complete, but it has no per-customer llmgate binding; check the configured provisioning account holds the llmgate credential",
				des, minted.UpstreamStatus)
		}
	}

	// Close the order. A replay that LOSES this claim already delivered — still
	// fall through to re-send the key so a buyer who never got the message can
	// recover (telegram_bot.py:7627-7630).
	if _, err := b.store.ClaimOrder(des, "paid", "delivered"); err != nil {
		log.Printf("CRITICAL apikey delivered-claim failed [des=%s chat=%s]: %v — key delivered but order left 'paid'",
			des, chatIDStr, err)
	}
	// Referral payout: credit the referrer REFERRAL_PCT% of the delivered total
	// (_pay_referrer telegram_bot.py:7637). The redemption row stays — it is the
	// lifetime "one use" record for the redeeming customer.
	b.payReferrer(ctx, order)

	// SKU-correct delivery wording ("Kiro credits" vs bedrock "credits") — read
	// off the STORED row, not the order, so a settle replay of a pre-split order
	// (sale_class '') keeps its original bedrock message.
	tmpl := c.Delivered
	if apikeyKeyClass(row) == saleClassKiro {
		tmpl = c.KiroDelivered
	}
	base := html.EscapeString(b.kiroGoPublicURL())
	b.sendHTML(ctx, chatID, i18n.F(tmpl.T(lang),
		"credits", itoa64(credits),
		"key", html.EscapeString(row.APIKey),
		"base", base))
	// Owner sale alert (notify_admin_sale @8022) is M3.
	return true
}

// mintAPIKey performs the one provision_customer call — the payload half of
// _deliver_apikey (telegram_bot.py:7489-7524). Returns the response, or an
// error the caller turns into a refund.
func (b *Bot) mintAPIKey(ctx context.Context, chatID int64, des string, credits int64, saleClass string) (*kirogo.ProvisionCustomerResponse, error) {
	// The backend rejects credits<=0 with a 400; catch it here so a malformed
	// order fails with a readable reason instead of an opaque server rejection.
	if credits <= 0 {
		return nil, errors.New("refusing to mint a non-positive-credit key")
	}
	client := b.kirogoClient()
	if client == nil {
		return nil, errKiroGoUnconfigured
	}
	req := kirogo.ProvisionCustomerRequest{
		Name:      "tg-" + fmtInt(chatID) + "-order-" + des,
		Credits:   credits,
		SaleClass: saleClass,
	}
	if saleClass != saleClassKiro {
		// BEDROCK mint: accountId is PINNED (never omitted). The server resolves
		// a missing one only while a single account holds upstream credentials
		// and starts 400-ing "specify accountId" the moment a second one does.
		// kiroGoConfigured() already hides the product when it is unset, so this
		// is a defence-in-depth backstop, not the primary gate.
		acct := b.kiroGoConfig().AccountID
		if acct == "" {
			return nil, errKiroGoUnconfigured
		}
		req.AccountID = acct
		req.SkipUpstream = false
	}
	// KIRO mint needs no accountId and forces skipUpstream server-side — the
	// kirogo client hardcodes both for that class (invariant 3b), so nothing
	// extra is set here.
	res, err := client.ProvisionCustomer(ctx, req)
	if err != nil {
		return nil, err
	}
	if !res.Deliverable() || res.Key == "" {
		return nil, errors.New("unexpected mint response shape")
	}
	return res, nil
}

// apikeyMintFailRefund is the mint-failure chokepoint (telegram_bot.py:7525-7555):
// the money is already secured, so give it back to the wallet, tell the buyer
// and alert the owners. The paid->refunded claim guards double-refund on a
// settle replay.
func (b *Bot) apikeyMintFailRefund(ctx context.Context, order *store.Order, des string,
	chatID int64, lang i18n.Lang, credits, total int64, cause error) {
	// Never log the admin key; `cause` carries only the server's error text
	// (internal/kirogo never puts credentials in an error).
	log.Printf("apikey mint failed [des=%s chat=%d]: %v", des, chatID, cause)
	// mint_failed (stats-fixes-spec.md §2.1): OUR own Kiro-Go provisioning
	// failed — shop's fault, not a customer-requested refund. Excluded from the
	// customer-facing refund rate (§2.2); a rising count is an outage signal.
	won, err := b.store.ClaimOrderKind(des, "paid", "refunded", store.RefundKindMintFailed)
	if err != nil || won == nil {
		// Nothing moved: the order stays 'paid' and surfaces in the stuck-paid
		// reconcile. Crediting without the claim could double-refund on a replay.
		log.Printf("CRITICAL apikey mint-fail refund claim failed [des=%s won=%t err=%v] — order left 'paid' for reconcile, nothing credited",
			des, won != nil, err)
		return
	}
	// Give back any promo use / referral reservation this order was holding —
	// _release_order_code (telegram_bot.py:7535). The sale did not complete, so
	// the code must become usable again.
	b.releaseOrderCode(order)
	newBal, cerr := b.store.CreditBalance(order.ChatID, total)
	if cerr != nil {
		// Credit failed AFTER the refunded claim committed. Left as-is the money
		// would vanish behind a settled-looking status — revert to 'paid' so the
		// reconcile surfaces it (money must NEVER be silently dropped).
		log.Printf("CRITICAL apikey mint-fail credit failed [des=%s total=%d err=%v] — reverting to 'paid' for reconcile",
			des, total, cerr)
		if _, rerr := b.store.ClaimOrder(des, "refunded", "paid"); rerr != nil {
			log.Printf("CRITICAL apikey mint-fail revert ALSO failed [des=%s err=%v] — MANUAL FIX REQUIRED: customer paid %d, not credited",
				des, rerr, total)
		}
		return
	}
	b.send(ctx, chatID, i18n.F(i18n.C().APIKey.MintFail.T(lang),
		"amount", b.usdtDisp(total), "balance", b.usdtDisp(newBal)), nil)
	// TODO(M3.1): _notify_owners — paid money bounced back because Kiro-Go is
	// down or rejected the mint; needs a look before the next sale.
	log.Printf("OWNER ALERT: API-KEY MINT FAILED (auto-refunded) [des=%s chat=%d credits=%d total=%d]: %v",
		des, chatID, credits, total, cause)
}

// deliverApikeyRecharge tops up the target key on Kiro-Go for a PAID recharge
// order and confirms — _deliver_apikey_recharge (telegram_bot.py:7676-7820).
//
// The per-key cap was enforced (fail-closed, on live stats) at pick, typed-
// amount and order-creation time; it is deliberately NOT re-checked here —
// payment is already secured, the window since creation is minutes, and a cap
// lowered by the admin mid-flight should not eat a customer's paid order.
func (b *Bot) deliverApikeyRecharge(ctx context.Context, order *store.Order, des string) bool {
	c := &i18n.C().APIKey
	chatID, _ := strconv.ParseInt(order.ChatID, 10, 64)
	lang := langSafe(order)
	credits, total := order.Qty, order.Total

	rec, err := b.store.APIKeyRechargeForOrder(des)
	if err != nil {
		log.Printf("CRITICAL apikey_recharge_for_order read failed [des=%s]: %v — order left 'paid'", des, err)
		return false
	}
	var krow *store.APIKey
	if rec != nil {
		krow, err = b.store.APIKeyForOrder(rec.TargetOrderID)
		if err != nil {
			log.Printf("CRITICAL api_key_for_order read failed [des=%s]: %v — order left 'paid'", des, err)
			return false
		}
	}
	// No binding / no key row (both are written at order creation, so this
	// should be impossible) or the key was refunded+revoked while this order sat
	// unpaid: nothing to top up, money back.
	if rec == nil || krow == nil {
		return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total,
			errors.New("recharge binding or key row missing"))
	}
	if krow.RefundedTS != nil {
		return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total,
			errors.New("target key was refunded before fulfillment"))
	}

	remaining := "—" // fresh creditsRemaining for the receipt ("—" when unknown)
	if rec.AppliedTS == nil {
		// A recharge RE-ENABLES a key disabled for ANY reason server-side, so a
		// key the admin manually disabled for abuse during the payment window
		// must be refused here too (the gate only ran at order time). Refuse ONLY
		// on an explicit "disabled" status: a stats hiccup fails OPEN — payment is
		// secured and blocking good orders on a read outage is worse (exhausted
		// keys legitimately recharge).
		if st := b.apikeyLiveStats(ctx, krow); st != nil && st.Status == "disabled" {
			return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total,
				errors.New("target key is disabled (recharge would re-enable it)"))
		}
		client := b.kirogoClient()
		if client == nil {
			return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total, errKiroGoUnconfigured)
		}
		// Arm the server's class guard with the key's RAW stored class: legacy ''
		// rows send NO saleClass (the server's own record wins); classed rows send
		// theirs, so a bot/server class disagreement is a 409 refusal (money
		// auto-refunded below) instead of a silently mis-billed ~5.7x top-up.
		// NOTE: credits ONLY, never tokens — kirogo.RechargeKeyRequest has no
		// tokens field at all, which makes the "sets the token cap to N and
		// instantly disables the key" bug unrepresentable.
		req := kirogo.RechargeKeyRequest{Credits: credits, SaleClass: krow.SaleClass}
		if krow.KeyID != "" {
			req.ID = krow.KeyID
		} else {
			req.APIKey = krow.APIKey
		}
		res, rerr := client.RechargeKey(ctx, req)
		if rerr != nil {
			// Never log the admin key; rerr carries only the server's error text.
			return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total, rerr)
		}
		if res.CreditsRemaining >= 0 {
			// Round, don't truncate: CreditsRemaining is fractional now (live
			// keys accrue sub-1-credit usage) — int64(...) would floor a
			// balance like 1999.95 down to 1999 on the receipt.
			remaining = itoa64(roundCredits(float64(res.CreditsRemaining)))
		}
		// TOCTOU vs a concurrent whole-key refund: the group refund marks the key
		// refunded and DELETES it on Kiro-Go. If that ran between our check above
		// and the recharge commit, this order's credits landed on (or resurrected)
		// a key whose money was just paid back — and this order, not yet applied,
		// was excluded from that group payout. Re-read the mark; if the key got
		// refunded meanwhile, revoke it again (best-effort, kills any
		// resurrection) and refund THIS order instead of delivering.
		if fresh, ferr := b.store.APIKeyForOrder(rec.TargetOrderID); ferr == nil &&
			fresh != nil && fresh.RefundedTS != nil {
			b.revokeAPIKey(ctx, krow)
			return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total,
				errors.New("target key was refunded mid-recharge"))
		}
		// Stamp AFTER the confirmed top-up — invariant 2. A crash right here can
		// only cause a re-send / (rare) replay duplicate in the customer's
		// favour, never a paid-but-never-applied order.
		if err := b.store.MarkAPIKeyRechargeApplied(des); err != nil {
			log.Printf("CRITICAL mark_apikey_recharge_applied failed [des=%s]: %v — credits ARE on the key; a settle replay would top it up AGAIN",
				des, err)
		}
		// Keep the locally stored quota in step with the server-side limit so the
		// My-Keys / reveal / picker displays stay truthful after top-ups.
		if err := b.store.BumpAPIKeyCredits(rec.TargetOrderID, float64(credits)); err != nil {
			log.Println("bump_api_key_credits failed:", err)
		}
	} else {
		// Replay of an already-applied recharge: fetch the live balance for the
		// receipt (best-effort; "—" when unreadable).
		if v, ok := b.apikeyRemainingCredits(ctx, krow); ok && v >= 0 {
			remaining = itoa64(int64(v))
		}
	}

	// Close the order. A replay that lost this claim already delivered — still
	// fall through to re-send the confirmation.
	if _, err := b.store.ClaimOrder(des, "paid", "delivered"); err != nil {
		log.Printf("CRITICAL apikey recharge delivered-claim failed [des=%s]: %v — credits applied but order left 'paid'", des, err)
	}
	// Referral payout: credit the referrer REFERRAL_PCT% of the delivered total
	// (_pay_referrer telegram_bot.py:7794). Redemption row stays (lifetime record).
	b.payReferrer(ctx, order)
	b.send(ctx, chatID, i18n.F(c.RCDone.T(lang),
		"credits", itoa64(credits), "masked", maskAPIKey(krow.APIKey),
		"remaining", remaining), nil)
	// Owner sale alert (notify_admin_sale) is M3.
	return true
}

// apikeyRCFailRefund is the recharge-failure chokepoint
// (telegram_bot.py:7701-7721) — the exact twin of apikeyMintFailRefund. Always
// returns false so callers can `return b.apikeyRCFailRefund(...)`.
func (b *Bot) apikeyRCFailRefund(ctx context.Context, order *store.Order, des string,
	chatID int64, lang i18n.Lang, credits, total int64, cause error) bool {
	log.Printf("apikey recharge failed [des=%s chat=%d]: %v", des, chatID, cause)
	// mint_failed (stats-fixes-spec.md §2.1): the recharge twin of
	// apikeyMintFailRefund above — OUR Kiro-Go recharge call failed, not a
	// customer-requested refund. Excluded from the customer-facing refund rate
	// (§2.2); a rising count is an outage signal.
	won, err := b.store.ClaimOrderKind(des, "paid", "refunded", store.RefundKindMintFailed)
	if err != nil || won == nil {
		log.Printf("CRITICAL apikey recharge refund claim failed [des=%s won=%t err=%v] — order left 'paid' for reconcile, nothing credited",
			des, won != nil, err)
		return false
	}
	// Non-delivery: give back any promo use / referral reservation this order
	// held — _release_order_code (telegram_bot.py:7706). The order never
	// delivers, so its code must become usable again.
	b.releaseOrderCode(order)
	newBal, cerr := b.store.CreditBalance(order.ChatID, total)
	if cerr != nil {
		log.Printf("CRITICAL apikey recharge refund credit failed [des=%s total=%d err=%v] — reverting to 'paid' for reconcile",
			des, total, cerr)
		if _, rerr := b.store.ClaimOrder(des, "refunded", "paid"); rerr != nil {
			log.Printf("CRITICAL apikey recharge refund revert ALSO failed [des=%s err=%v] — MANUAL FIX REQUIRED: customer paid %d, not credited",
				des, rerr, total)
		}
		return false
	}
	b.send(ctx, chatID, i18n.F(i18n.C().APIKey.RCFail.T(lang),
		"amount", b.usdtDisp(total), "balance", b.usdtDisp(newBal)), nil)
	// TODO(M3.1): _notify_owners.
	log.Printf("OWNER ALERT: API-KEY RECHARGE FAILED (auto-refunded) [des=%s chat=%d credits=%d total=%d]: %v",
		des, chatID, credits, total, cause)
	return false
}

// revokeAPIKey deletes a sold key on Kiro-Go so its remaining credits can no
// longer be spent — kiro_go_delete_key (telegram_bot.py:6076-6122). Returns
// (ok, detail): ok is true on a successful delete OR a 404 ("already gone" is
// the desired end state). NEVER returns an error to the caller — the refund has
// already committed, so a revoke hiccup must not break it; the caller alerts
// the owner instead.
//
// A successful delete whose `upstreamRevoked` is EXPLICITLY false means the
// bound llmgate key is STILL LIVE and spending against our account: it must be
// removed by hand in the llmgate portal. That is a loud alert, but the
// pool-side revoke still counts as ok (the customer's key is dead either way),
// so refunds are never blocked (KIRO_GO_API.md:100).
func (b *Bot) revokeAPIKey(ctx context.Context, row *store.APIKey) (bool, string) {
	if row == nil || (row.KeyID == "" && row.APIKey == "") {
		return false, "no key id or value to delete"
	}
	client := b.kirogoClient()
	if client == nil {
		return false, "kiro-go not configured"
	}
	req := kirogo.DeleteAPIKeyRequest{}
	if row.KeyID != "" {
		req.ID = row.KeyID
	} else {
		req.APIKey = row.APIKey
	}
	res, err := client.DeleteAPIKey(ctx, req)
	if err != nil {
		// A 404 means the key is already gone — the desired end state, so this
		// counts as a successful revoke (telegram_bot.py:6117-6121).
		var apiErr *kirogo.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return true, "already gone"
		}
		return false, err.Error()
	}
	if !res.Success {
		return false, "unexpected delete response"
	}
	if res.UpstreamStillLive() {
		// TODO(M3.1): _notify_owners with this text.
		log.Printf("OWNER ALERT: UPSTREAM KEY NOT REVOKED for pool key id %s (%s) — the Kiro-Go pool key is deleted, but its llmgate-side key is STILL LIVE and spending against our account; DELETE it manually in the llmgate portal now",
			orDash(row.KeyID), maskAPIKey(row.APIKey))
		return true, "revoked (upstream NOT revoked — manual cleanup needed)"
	}
	return true, "revoked"
}

// orDash renders an empty id as "—" for log/alert text (Python's `or '—'`).
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// sendHTML sends a parse_mode=HTML message with the link preview suppressed —
// the delivery/reveal send shape (telegram_bot.py:7646-7654). The three
// guidance URLs are bare (Telegram auto-links them); the preview card is
// disabled so nothing pushes the tap-to-copy key down the screen, and so
// Telegram's servers don't fetch the dashboard on every delivery and reveal.
func (b *Bot) sendHTML(ctx context.Context, chatID int64, text string) {
	if _, err := b.tg.SendMessage(ctx, tgSendHTML(chatID, text)); err != nil {
		// The key is safe in the DB and reachable via Self-Service -> My API Keys.
		log.Printf("apikey HTML send failed [chat=%d]: %v", chatID, err)
	}
}

// roundCredits renders a stored float credit total as the whole number every
// customer-facing label uses (Python's int(round(float(x or 0)))).
func roundCredits(v float64) int64 { return int64(math.Round(v)) }
