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
	"fmt"
	"html"
	"log"
	"math"
	"strconv"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/llmgate"
	"codezdev-bot/internal/store"
)

// errNotConfigured stands in for Python's "kiro-go not configured" RuntimeError
// on the mint path — a defence-in-depth backstop only: apikeyForSale() already
// hides the product, so reaching this means the admin wiped the config between
// order creation and settle.
var errKiroGoUnconfigured = errors.New("kiro-go not configured (base URL / admin key / account id missing)")

// errLlmgateUnconfigured is the llmgate twin of errKiroGoUnconfigured — same
// defence-in-depth shape: apikeyForSale(bedrock) gates on llmgateClient() !=
// nil, so reaching this means the admin wiped the llmgate credentials between
// order creation and settle. The sale then auto-refunds below, exactly like a
// pool mint with a dead pool.
var errLlmgateUnconfigured = errors.New("llmgate not configured (base URL / username / password missing)")

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
		if apikeyBindsUpstream(saleClass) {
			// LLMGATE MINT (bedrock + legacy '' — the classes the cutover moved
			// off the pool). One sold key = one CreateKey call; failure below is
			// the ONLY real failure and auto-refunds.
			minted, mintErr := b.mintLlmgateKey(ctx, chatID, des, credits)
			if mintErr != nil {
				b.apikeyMintFailRefund(ctx, order, des, chatID, lang, credits, total, mintErr)
				return false
			}
			// NO upstream-status logging / owner alerts on this branch — and
			// that is a structural fact, not an omission. "upstreamStatus" is
			// a POOL concept: provision_customer binds a per-customer llmgate
			// key BEHIND a pool key, and reports how that binding went
			// (bound/pending/skipped). Here the llmgate key IS the upstream —
			// there is nothing behind it to be pending or skipped, no
			// upstreamKeyId to attribute, and the pool's
			// "sold without upstream binding" alert would fire on EVERY sale
			// while describing a key that does not exist. A CreateKey 2xx is
			// the whole success condition (the pool-path comments below keep
			// their wording because that path is unchanged and still live for
			// kiro/fixed).
			keyID := strconv.FormatInt(minted.ID, 10)
			// Persist the cleartext key BEFORE flipping the order status —
			// invariant 1, same ordering as the pool path. The stored class is
			// saleClassBedrock, NOT the server's echo: llmgate has no sale-class
			// echo at all, and every class this branch serves (bedrock + the
			// legacy '') bills as bedrock anyway — apikeyKeyClass collapses ''
			// onto bedrock everywhere else, so pinning it here also retires the
			// legacy-'' ambiguity for newly minted rows. Backend stamps the
			// row LLMGATE so recharge/stats/revoke route to the right gateway.
			if _, err := b.store.AddAPIKey(des, chatID, keyID, minted.APIKey,
				float64(credits), saleClassBedrock, store.BackendLlmgate); err != nil {
				// The key EXISTS on llmgate and the customer's money is spent,
				// but we could not record it. Do NOT refund (the key is real
				// and billable) and do NOT re-mint. Deliver from the in-memory
				// response below and page loudly: without the row, a replay
				// would double-mint.
				log.Printf("CRITICAL apikey row persist failed [des=%s chat=%d llmgateKeyId=%s]: %v — key IS minted; store the row by hand before any settle replay",
					des, chatID, keyID, err)
				// Owner alert (#39): PaidUndelivered never sees this row — the
				// order still proceeds to 'delivered' below (the buyer holds the
				// key), so nothing else will ever surface a missing api_keys row.
				// The llmgate key ID is non-secret; the cleartext key itself is
				// never logged or alerted.
				b.notifyOwners(ctx, fmt.Sprintf(
					"🚨 API-KEY ROW PERSIST FAILED — MANUAL FIX REQUIRED\n🧾 %s\n👤 %d\n🔑 llmgate key id %s\nThe key IS minted and billable; store the row by hand before any settle replay.",
					des, chatID, keyID))
			}
			row, err = b.store.APIKeyForOrder(des)
			if err != nil || row == nil {
				// Fall back to the just-minted response so the buyer still
				// receives the key they paid for (never leave them
				// empty-handed).
				row = &store.APIKey{OrderID: des, ChatID: chatID, KeyID: keyID,
					APIKey: minted.APIKey, Credits: float64(credits),
					SaleClass: saleClassBedrock, Backend: store.BackendLlmgate}
			}
		} else {
			// POOL MINT (kiro / fixed — SKUs that stay on the pool gateway).
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
			//               a kiro or fixed mint, both of which send
			//               skipUpstream:true — see apikeyBindsUpstream).
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
				// Log only ANOMALOUS non-bound states: for a class that binds no
				// upstream key, "skipped" is its normal terminal status and logging
				// it would put one warning-shaped line on EVERY sale of that SKU
				// (telegram_bot.py:7578-7584).
				if apikeyBindsUpstream(saleClass) || minted.UpstreamStatus != kirogo.UpstreamStatusSkipped {
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
			// This mint came off the Kiro-Go/Pool gateway, so the row is stamped
			// pool-backend; the llmgate mint path above stamps BackendLlmgate at
			// its own AddAPIKey call.
			if _, err := b.store.AddAPIKey(des, chatID, minted.ID, minted.Key,
				float64(credits), minted.SaleClass, store.BackendPool); err != nil {
				// The key EXISTS on Kiro-Go and the customer's money is spent, but we
				// could not record it. Do NOT refund (the key is real and billable)
				// and do NOT re-mint. Deliver from the in-memory response below and
				// page loudly: without the row, a replay would double-mint.
				log.Printf("CRITICAL apikey row persist failed [des=%s chat=%d keyId=%s]: %v — key IS minted; store the row by hand before any settle replay",
					des, chatID, minted.ID, err)
				// Owner alert (#39): PaidUndelivered never sees this row — the
				// order still proceeds to 'delivered' below (the buyer holds the
				// key), so nothing else will ever surface a missing api_keys row.
				// keyId is non-secret; the cleartext key itself is never logged
				// or alerted.
				b.notifyOwners(ctx, fmt.Sprintf(
					"🚨 API-KEY ROW PERSIST FAILED — MANUAL FIX REQUIRED\n🧾 %s\n👤 %d\n🔑 key id %s\nThe key IS minted and billable; store the row by hand before any settle replay.",
					des, chatID, minted.ID))
			}
			row, err = b.store.APIKeyForOrder(des)
			if err != nil || row == nil {
				// Fall back to the just-minted response so the buyer still receives
				// the key they paid for (never leave them empty-handed).
				row = &store.APIKey{OrderID: des, ChatID: chatID, KeyID: minted.ID,
					APIKey: minted.Key, Credits: float64(credits), SaleClass: minted.SaleClass}
			}
			// A sale of an UPSTREAM-BINDING class (bedrock/legacy — see
			// apikeyBindsUpstream) that completed with NO upstream binding has no
			// per-customer attribution and no independent kill switch — usually the
			// configured account holds no llmgate credential. Kiro and fixed are
			// excluded by that predicate, not by a local class literal: they skip
			// binding by construction, so alerting on them would page the owners on
			// every normal sale. Alert, never block:
			// the customer's key is live and paid for. Fires AFTER the persist above
			// on purpose — anything slow between a committed mint and the api_keys
			// row reopens the double-mint window that row exists to close.
			// _notify_owners — telegram_bot.py:7701-7711.
			//
			// POOL-PATH ONLY since the cutover: this arm runs for kiro/fixed
			// (always "skipped", never alerted) — a bedrock order never reaches
			// it because bedrock mints on llmgate above, where no binding
			// concept exists.
			if apikeyBindsUpstream(saleClass) &&
				minted.UpstreamStatus != kirogo.UpstreamStatusBound &&
				minted.UpstreamStatus != kirogo.UpstreamStatusPending {
				log.Printf("OWNER ALERT: API-KEY SOLD WITHOUT UPSTREAM BINDING [des=%s upstreamStatus=%s] — the key works and the sale is complete, but it has no per-customer llmgate binding; check the configured provisioning account holds the llmgate credential",
					des, minted.UpstreamStatus)
				b.notifyOwners(ctx, fmt.Sprintf(
					"⚠️ API-KEY SOLD WITHOUT UPSTREAM BINDING\n🧾 %s\nupstreamStatus: %s\n"+
						"The key works and the sale is complete, but it has no per-customer llmgate binding. "+
						"Check that the configured provisioning account is the one holding the llmgate credential "+
						"(⚙️ Settings > 🆔 Kiro-Go Account > 🧪 Verify).",
					des, minted.UpstreamStatus))
			}
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
	// (sale_class '') keeps its original bedrock message. Backend-aware since
	// the cutover: a BackendLlmgate row (whatever its class) gets the
	// llmgate wording via apikeyDeliveredTemplate.
	keyCls := apikeyKeyClass(row)
	tmpl := apikeyDeliveredTemplate(c, keyCls, row.Backend)
	base := html.EscapeString(b.kiroGoPublicURL())
	// "name" is admin-typed text landing in a parse_mode=HTML message, so it is
	// escaped like {key}/{base}: an unescaped `<` or `&` in a renamed SKU would
	// make Telegram reject the whole delivery message — the one message the
	// buyer must receive. "apibase"/"pubbase" are only placeholders on the
	// llmgate template; for the pool templates the kv pairs are silent no-ops
	// (the same arrangement the prompts use for {opus}/{prices}). {base} is no
	// longer on the llmgate template (the legacy-endpoint line was removed
	// 2026-10-10) but stays passed for the POOL templates, which still carry
	// it.
	b.sendHTML(ctx, chatID, i18n.F(tmpl.T(lang),
		"name", html.EscapeString(b.apikeyProductName(keyCls)),
		"credits", itoa64(credits),
		"key", html.EscapeString(row.APIKey),
		"base", base,
		"apibase", b.llmgateAPIBaseDisp(),
		"pubbase", b.llmgatePublicURLDisp()))
	// Owner sale alert — notify_admin_sale (telegram_bot.py:7750-7755): the
	// SKU label mirrors the delivery template split just above (Kiro vs
	// Bedrock credits) by reusing the SAME keyCls (the STORED row's class),
	// not the order-derived saleClass — a settle replay of a pre-split order
	// (stored sale_class '') would otherwise show the buyer the bedrock
	// template while telling the admin it sold as Kiro. Now the
	// admin-renameable display name, so the alert matches what the shop
	// actually sells it as (notifyAdminSale HTML-escapes the label itself).
	b.notifyAdminSale(ctx, chatID, b.apikeyProductName(keyCls), "API", credits, total, des)
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
	if apikeyBindsUpstream(saleClass) {
		// BEDROCK/legacy mint: accountId is PINNED (never omitted). The server
		// resolves a missing one only while a single account holds upstream
		// credentials and starts 400-ing "specify accountId" the moment a
		// second one does. kiroGoConfigured() already hides the product when it
		// is unset, so this is a defence-in-depth backstop, not the primary
		// gate.
		//
		// The gate is apikeyBindsUpstream, NOT `!= saleClassKiro`: the kirogo
		// client REJECTS an accountId for both non-binding classes
		// (ErrAccountIDForbiddenForKiro / ErrAccountIDForbiddenForFixed), so a
		// "not kiro" test sends one on every FIXED mint and fails the sale
		// client-side before the request is even made — auto-refunding the
		// buyer on a product that was never broken.
		acct := b.kiroGoConfig().AccountID
		if acct == "" {
			return nil, errKiroGoUnconfigured
		}
		req.AccountID = acct
		req.SkipUpstream = false
	}
	// A KIRO or FIXED mint needs no accountId and forces skipUpstream
	// server-side — the kirogo client hardcodes both for those classes
	// (invariant 3b), so nothing extra is set here.
	res, err := client.ProvisionCustomer(ctx, req)
	if err != nil {
		return nil, err
	}
	if !res.Deliverable() || res.Key == "" {
		return nil, errors.New("unexpected mint response shape")
	}
	return res, nil
}

// mintLlmgateKey performs the one CreateKey call for a BEDROCK (or legacy ”)
// sale — the llmgate cutover's replacement for the pool provision_customer
// mint. Returns the created key (cleartext APIKey + numeric ID), or an error
// the caller turns into a refund.
//
// WIRE FACTS this path leans on (verified live, encoded in
// internal/llmgate): CreateKey sends expires_at as RFC3339 when given a
// time — this path always gives it now+llmgate.DefaultValidity, the 30
// DAYS every sold key lives (business rule 2026-10-10); a zero time would
// omit the field (never expires), the legacy shape no caller here uses. credit_limit is in credits where
// 1 credit = $1, so the order's credits map 1:1.
//
// AMBIGUOUS-CREATE RECONCILIATION (the no-double-mint recovery): CreateKey
// sends a SINGLE attempt (internal/llmgate sendOnce — a create retried
// after a timeout could mint a second paid key for one sale). A timeout can
// still strike AFTER the server committed the create, which would leave a
// live billable key recorded NOWHERE. When (and only when) the failure is
// ambiguous (llmgate.IsAmbiguous — network/timeout/5xx, never a definitive
// 4xx), this path makes ONE reconciliation pass: ListKeys → the entry whose
// name is EXACTLY this order's deterministic mint name (names are unique
// per order) → RevealKey on it → the revealed cleartext IS the mint result.
// That recovers the committed-but-unanswered window WITHOUT ever minting
// twice — no second create is sent under any circumstance. A definitive 4xx
// skips reconciliation entirely and fails straight to the refund.
func (b *Bot) mintLlmgateKey(ctx context.Context, chatID int64, des string, credits int64) (*llmgate.Key, error) {
	// Same readable-reason guard as the pool mint: a non-positive credit order
	// is a malformed row, not an upstream failure.
	if credits <= 0 {
		return nil, errors.New("refusing to mint a non-positive-credit key")
	}
	client := b.llmgateClient()
	if client == nil {
		return nil, errLlmgateUnconfigured
	}
	// The deterministic mint name, same convention as the pool path — it is
	// what an operator greps in the llmgate console to find a sale's key, and
	// what the reconciliation below matches on (unique per order).
	name := "tg-" + fmtInt(chatID) + "-order-" + des
	// 30-DAY DEFAULT VALIDITY (business rule 2026-10-10, rule 1, revised
	// same day to a flat 30 days — NOT a calendar month, which would hand
	// buyers 28-to-31-day "months"): every freshly minted key expires
	// exactly 30 days out; a later recharge of the still-ACTIVE key renews
	// this window (rule 2). The old never-expire mint is gone — only an
	// explicit zero time can still ask llmgate for it, and no caller here
	// passes one. The constant is shared with the recharge renewal and the
	// migration tool so all three agree by construction.
	expires := time.Now().Add(llmgate.DefaultValidity)
	res, err := client.CreateKey(ctx, name, float64(credits), expires)
	if err != nil {
		// Ambiguity only: a 4xx is a definitive rejection (nothing was
		// minted, reconciliation would be wasted work) and fails straight
		// through to the refund below.
		if llmgate.IsAmbiguous(err) {
			rec, rerr := recoverLlmgateMint(ctx, client, name)
			if rerr == nil {
				// The create WAS committed — we just never saw its answer.
				// Deliver the recovered key: refunding now would pay the
				// customer back while a key they bought stays live.
				log.Printf("llmgate mint recovered via list+reveal [des=%s chat=%d llmgateKeyId=%d] — the single create attempt was ambiguous but the server had committed it",
					des, chatID, rec.ID)
				return rec, nil
			}
			// Not found (or the reconciliation itself failed): no key with
			// this order's name exists, so nothing was minted — the ORIGINAL
			// error is the true verdict and the refund proceeds on it.
			log.Printf("llmgate mint reconciliation found no key [des=%s chat=%d]: %v", des, chatID, rerr)
		}
		// llmgate error text is already redacted of key/JWT shapes by the
		// client (its invariant 7), so it is safe as refund-cause text.
		return nil, err
	}
	// Shape guard mirroring the pool mint's: a 2xx without both the cleartext
	// key and a real id is a contract break we cannot deliver from — fail to
	// the refund path rather than storing an unusable row.
	if res.APIKey == "" || res.ID <= 0 {
		// The id (when present) is NON-SECRET and is the ORPHAN CANDIDATE: a
		// 2xx means the server likely created SOMETHING, and after the refund
		// below nothing in the store will point at it. Log it so an operator
		// can find and delete/record it in the llmgate console.
		if res.ID > 0 {
			log.Printf("llmgate mint 2xx shape-broken but carried id %d [des=%s chat=%d] — ORPHAN CANDIDATE: after the auto-refund nothing references it; delete or record this id on llmgate by hand",
				res.ID, des, chatID)
		}
		return nil, errors.New("unexpected mint response shape")
	}
	return res, nil
}

// recoverLlmgateMint is mintLlmgateKey's ONE reconciliation pass for an
// ambiguous create failure: list the account's keys, find the entry whose
// name is EXACTLY the deterministic mint name, and reveal its cleartext.
// Returns the recovered key, or a non-nil error when no such key exists
// (the create truly never landed) or the list/reveal calls themselves fail
// — the caller then falls back to the ORIGINAL create error and refunds.
// It NEVER re-sends the create: the only mint that can exist is the one the
// ambiguous attempt may have committed.
func recoverLlmgateMint(ctx context.Context, client *llmgate.Client, name string) (*llmgate.Key, error) {
	keys, err := client.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list for reconciliation: %w", err)
	}
	for _, k := range keys {
		if k.Name != name {
			continue
		}
		revealed, rerr := client.RevealKey(ctx, k.ID)
		if rerr != nil {
			return nil, fmt.Errorf("reveal for reconciliation (id %d): %w", k.ID, rerr)
		}
		if revealed.APIKey == "" {
			return nil, fmt.Errorf("reveal for llmgate key %d returned no cleartext", k.ID)
		}
		// Reveal's wire shape carries the full record, but do not let a thin
		// response (id/name absent) defeat the shape guard downstream: fill
		// from the list entry, the authoritative metadata we just matched.
		if revealed.ID == 0 {
			revealed.ID = k.ID
		}
		if revealed.Name == "" {
			revealed.Name = k.Name
		}
		return revealed, nil
	}
	return nil, fmt.Errorf("no llmgate key named %q exists", name)
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
			// Owner alert (#39): the order is stuck 'refunded' with NOTHING
			// credited, and PaidUndelivered only scans status='paid' — this
			// row is structurally invisible to that backstop forever.
			b.notifyOwners(ctx, fmt.Sprintf(
				"🚨 REFUND REVERT FAILED — MANUAL FIX REQUIRED\n🧾 %s\n👤 %d\n💰 customer paid %s, not credited\nCredit the wallet by hand.",
				des, chatID, b.usdtDisp(total)))
		}
		return
	}
	b.send(ctx, chatID, i18n.F(i18n.C().APIKey.MintFail.T(lang),
		"amount", b.usdtDisp(total), "balance", b.usdtDisp(newBal)), nil)
	// _notify_owners — paid money bounced back because Kiro-Go is down or
	// rejected the mint; needs a look before the next sale (telegram_bot.py:
	// 7628-7637).
	log.Printf("OWNER ALERT: API-KEY MINT FAILED (auto-refunded) [des=%s chat=%d credits=%d total=%d]: %v",
		des, chatID, credits, total, cause)
	b.notifyOwners(ctx, fmt.Sprintf(
		"⚠️ API-KEY MINT FAILED (auto-refunded)\n🧾 %s\n👤 %d\n🔢 %d credits · %s\nKiro-Go error: %v",
		des, chatID, credits, b.usdtDisp(total), cause))
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
		// Delivery-time refusal re-checks, all on ONE stats read (fail OPEN on
		// an unreadable read — payment is secured and blocking good orders on a
		// read outage is worse; the in-lock re-read in applyLlmgateRecharge is
		// the fail-CLOSED backstop for llmgate rows). The RC gate ran these at
		// order time; the world may have moved since.
		//
		// (a) A recharge RE-ENABLES a key disabled for ANY reason server-side,
		// so a key the admin manually disabled for abuse during the payment
		// window must be refused too. Refuse ONLY on an explicit "disabled"
		// status — "exhausted" keys legitimately recharge. Backend-agnostic
		// since the cutover: apikeyLiveStats routes llmgate rows through
		// KeyStats and pool rows through /admin/stats, same tri-state.
		//
		// (b) EXPIRED keys cannot be recharged (business rule 2026-10-10,
		// rule 3): the top-up below RENEWS the validity of an ACTIVE key —
		// applying it to one already past its expires_at would resurrect a
		// key llmgate has killed, which the rule exists to forbid. Only
		// llmgate rows can ever be expired — pool keys never expire.
		if st := b.apikeyLiveStats(ctx, krow); st != nil {
			if st.Status == "disabled" {
				return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total,
					errors.New("target key is disabled (recharge would re-enable it)"))
			}
			if st.Expired {
				return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total,
					errors.New("target key is expired (recharge cannot renew an expired key)"))
			}
		}
		if krow.Backend == store.BackendLlmgate {
			// LLMGATE top-up: UpdateKey SETS credit_limit ABSOLUTELY, so the
			// read-modify-write below must be serialized per key or two
			// concurrent recharges of the same key lose one paid write (see
			// llmgateRCKeyLock). Receipt remaining = newLimit − used, "—" when
			// negative (overspend — do not clamp, but the receipt line only
			// carries a usable balance).
			rem, rerr := b.applyLlmgateRecharge(ctx, krow, rec.TargetOrderID, credits)
			if rerr != nil {
				// Never log the key; rerr carries only the client's redacted
				// error text.
				return b.apikeyRCFailRefund(ctx, order, des, chatID, lang, credits, total, rerr)
			}
			if rem >= 0 {
				remaining = itoa64(roundCredits(rem))
			}
		} else {
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
		}
		// TOCTOU vs a concurrent whole-key refund: the group refund marks the key
		// refunded and DELETES it on its gateway (Kiro-Go for a pool row,
		// llmgate for a direct one — revokeAPIKey routes by backend). If that
		// ran between our check above and the recharge commit, this order's
		// credits landed on (or resurrected) a key whose money was just paid
		// back — and this order, not yet applied, was excluded from that group
		// payout. Re-read the mark; if the key got refunded meanwhile, revoke
		// it again (best-effort, kills any resurrection) and refund THIS order
		// instead of delivering.
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
			// Owner alert (#39). An earlier version of this comment claimed
			// the order stays 'paid' here and would therefore surface in the
			// stuck-paid reconcile eventually. That is FALSE, and it was the
			// only thing making this site look redundant: control falls
			// through to BumpAPIKeyCredits and then to the
			// ClaimOrder(des, "paid", "delivered") below, which SUCCEEDS —
			// nothing in this failure path blocks the orders table. The row
			// ends 'delivered', and store.PaidUndelivered selects
			// WHERE status='paid', so the reconcile can NEVER see it.
			//
			// This is therefore the LEAST covered of the eight sites, not
			// the most: the credits are live on the key, the local stamp
			// says they are not, a settle replay would apply them a SECOND
			// time, and this alert is the only trace that will ever exist.
			// The risk is a replay, not a slow backstop.
			b.notifyOwners(ctx, fmt.Sprintf(
				"🚨 RECHARGE-APPLIED STAMP FAILED — DOUBLE-SPEND RISK\n🧾 %s\n👤 %d\n🔢 %d credits\nCredits ARE on the key; a settle replay would top it up AGAIN. Fix the stamp by hand before any replay.",
				des, chatID, credits))
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
	// Owner sale alert — notify_admin_sale (telegram_bot.py:7897-7902), the
	// recharge twin of deliverApikey's sale alert above. Built from the
	// admin-renameable SKU name + the " Recharge" suffix so a rename shows up
	// on both alert kinds instead of only the sale one.
	b.notifyAdminSale(ctx, chatID, b.apikeyProductName(apikeyKeyClass(krow))+" Recharge",
		"API", credits, total, des)
	return true
}

// applyLlmgateRecharge performs one llmgate credit top-up as a SERIALIZED
// read-modify-write and returns the receipt remaining (newLimit − used), or
// an error the caller turns into a refund. Called only for
// backend==BackendLlmgate rows with an unapplied recharge.
//
// WHY A LOCK (llmgateRCKeyLock): llmgate's UpdateKey SETS credit_limit
// ABSOLUTELY — there is no additive recharge endpoint — so the top-up is
// KeyStats(newLimit = CreditLimit + credits) → UpdateKey(newLimit). Two
// concurrent recharges of the same key that interleave both reads before
// either write lose one customer's paid credits in the gap. The mutex spans
// exactly the read-then-write pair, per TARGET key; it is never held across
// the shared post-recharge steps (TOCTOU re-read, stamp, bump) which are
// per-ORDER and already idempotent.
//
// The stats read inside the lock is FRESH (not the pre-check's read): the
// pre-check only screened for "disabled"/"expired" and failed OPEN on error,
// but the arithmetic MUST have a real limit or the write would set a garbage
// cap — so an unreadable stats read HERE fails closed into the refund path
// (as does an expiry the open pre-check could not see).
func (b *Bot) applyLlmgateRecharge(ctx context.Context, krow *store.APIKey, targetOrderID string, credits int64) (float64, error) {
	client := b.llmgateClient()
	if client == nil {
		return 0, errLlmgateUnconfigured
	}
	mu := b.llmgateRCKeyLock(targetOrderID)
	mu.Lock()
	defer mu.Unlock()
	st := b.llmgateKeyUsage(ctx, krow)
	if st == nil {
		return 0, errors.New("llmgate key stats unreadable (cannot compute the new credit limit)")
	}
	// 0 = UNLIMITED on llmgate: an unlimited key cannot take a credit top-up
	// (there is nothing to add to), and a newLimit of 0+credits would silently
	// CONVERT an unlimited key into a limited one — money spent to REMOVE a
	// feature. Refund instead. (The RC gate screens this earlier; this is the
	// delivery-time re-check, same layering as the pool path's guards.)
	if st.Info.CreditLimit <= 0 {
		return 0, errors.New("unlimited llmgate key cannot take a credit top-up")
	}
	// DISABLED re-check on the same FRESH in-lock read: the update below pins
	// is_active:true, so a key the admin disabled between the pre-check and
	// this lock would otherwise be silently RE-ENABLED, credited and renewed
	// by this very write — the exact outcome the gate refuses. Mirrors the
	// unlimited/expired guards' fail-CLOSED layering.
	if !st.Info.IsActive {
		return 0, errors.New("llmgate key is disabled (recharge would re-enable it)")
	}
	// EXPIRED re-check on the FRESH in-lock read (business rule 2026-10-10,
	// rule 3): the pre-check above fails OPEN on unreadable stats, so the key
	// may have expired (or died) since — and the renewal write below is
	// exactly what the rule forbids on a dead key. Same fail-CLOSED layering
	// as the unlimited guard: this read is the one the arithmetic AND the
	// renewal write are computed from.
	if exp, has := st.Info.Expiry(); has && exp.Before(time.Now()) {
		return 0, errors.New("llmgate key is expired (recharge cannot renew an expired key)")
	}
	// The stored key id is llmgate's NUMERIC id as a decimal string (minted by
	// mintLlmgateKey); anything else is a corrupt row for an operator, never a
	// condition to guess a top-up around.
	id, perr := strconv.ParseInt(krow.KeyID, 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("llmgate key id %q is not a decimal id: %w", krow.KeyID, perr)
	}
	newLimit := st.Info.CreditLimit + float64(credits)
	// RECHARGE RENEWS VALIDITY (business rule 2026-10-10, rule 2): the top-up
	// also pushes expires_at to now+30 days, so an ACTIVE key's window
	// resets on every recharge — the delivery message's "recharging before
	// expiry extends validity" promise is this write.
	// expires is ALWAYS concrete here (never the zero time): update-side
	// omission semantics are unverified (internal/llmgate invariant 4), and
	// the expired guards above prove this key is alive to renew.
	expires := time.Now().Add(llmgate.DefaultValidity)
	// MUST resend the CURRENT name: UpdateKey CLEARS the stored name when the
	// field is omitted (verified live — internal/llmgate invariant 4), and the
	// name is the operator's only grep handle tying a key to its sale.
	// is_active is pinned true: the disabled pre-check already refused a
	// disabled key, and re-asserting it is the verified normal state for a
	// live key.
	if _, uerr := client.UpdateKey(ctx, id, st.Info.Name, true, newLimit, expires); uerr != nil {
		return 0, uerr
	}
	return newLimit - st.Used(), nil
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
			// Owner alert (#39): the order is stuck 'refunded' with NOTHING
			// credited, and PaidUndelivered only scans status='paid' — this
			// row is structurally invisible to that backstop forever.
			b.notifyOwners(ctx, fmt.Sprintf(
				"🚨 REFUND REVERT FAILED — MANUAL FIX REQUIRED\n🧾 %s\n👤 %d\n💰 customer paid %s, not credited\nCredit the wallet by hand.",
				des, chatID, b.usdtDisp(total)))
		}
		return false
	}
	b.send(ctx, chatID, i18n.F(i18n.C().APIKey.RCFail.T(lang),
		"amount", b.usdtDisp(total), "balance", b.usdtDisp(newBal)), nil)
	// _notify_owners — the recharge twin of apikeyMintFailRefund's alert above.
	log.Printf("OWNER ALERT: API-KEY RECHARGE FAILED (auto-refunded) [des=%s chat=%d credits=%d total=%d]: %v",
		des, chatID, credits, total, cause)
	b.notifyOwners(ctx, fmt.Sprintf(
		"⚠️ API-KEY RECHARGE FAILED (auto-refunded)\n🧾 %s\n👤 %d\n🔢 %d credits · %s\nKiro-Go error: %v",
		des, chatID, credits, b.usdtDisp(total), cause))
	return false
}

// revokeAPIKey deletes a sold key on ITS backend so its remaining credits can
// no longer be spent — kiro_go_delete_key (telegram_bot.py:6076-6122), made
// backend-routing by the llmgate cutover. Returns (ok, detail): ok is true on
// a successful delete OR a 404 ("already gone" is the desired end state).
// NEVER returns an error to the caller — the refund has already committed, so
// a revoke hiccup must not break it; the caller alerts the owner instead.
//
// MIGRATED ROWS (backend=llmgate with preserved pool_key_id/pool_api_key):
// the llmgate delete is followed by a best-effort delete of the POOL TWIN.
// The migration deliberately left the old pool key live (customers keep it
// working until the pool retires), so a migrated row is TWO spendable keys
// with the same remaining — and a refund pays that remaining back ONCE. If
// only the llmgate half died, the customer's preserved pool twin would stay
// live and spendable after the payout: double value for one refund, real
// money leaked on a fully automated path. The twin revoke is therefore
// mandatory on refund, but BEST-EFFORT in mechanics — a twin failure must
// never fail the llmgate revoke or block the refund; it alerts the owners
// for manual cleanup instead (mirroring the pool branch's failure shape).
//
// A successful POOL delete whose `upstreamRevoked` is EXPLICITLY false means
// the bound llmgate key is STILL LIVE and spending against our account: it
// must be removed by hand in the llmgate portal. That is a loud alert, but
// the pool-side revoke still counts as ok (the customer's key is dead either
// way), so refunds are never blocked (KIRO_GO_API.md:100). The upstream-still-
// live alert is POOL-ONLY by construction: it exists because a pool key is a
// wrapper AROUND a separately-managed llmgate credential the pool may fail to
// clean up. An llmgate-backend key minted by THIS bot is the credential
// itself — DeleteKey removes the only thing that can spend, and the one
// second layer that can exist (a migrated row's pool twin) is handled by the
// twin revoke above.
func (b *Bot) revokeAPIKey(ctx context.Context, row *store.APIKey) (bool, string) {
	if row == nil || (row.KeyID == "" && row.APIKey == "") {
		return false, "no key id or value to delete"
	}
	if row.Backend == store.BackendLlmgate {
		client := b.llmgateClient()
		if client == nil {
			return false, "llmgate not configured"
		}
		// llmgate addresses keys by their NUMERIC id; the row stores it as the
		// decimal string mintLlmgateKey wrote. A non-decimal id is a corrupt
		// row — surface it as a revoke failure so the owner alert carries the
		// reason, never silently succeed.
		id, err := strconv.ParseInt(row.KeyID, 10, 64)
		if err != nil {
			return false, fmt.Sprintf("llmgate key id %q is not a decimal id: %v", row.KeyID, err)
		}
		if derr := client.DeleteKey(ctx, id); derr != nil {
			// A 404 means the key is already gone — the desired end state, so
			// the TWIN revoke still runs (a missing llmgate key says nothing
			// about the pool twin's fate) and this counts as a successful
			// revoke, mirroring the pool's 404 rule (telegram_bot.py:6117-6121).
			if errors.Is(derr, llmgate.ErrNotFound) {
				return b.revokeMigratedPoolTwin(ctx, row, "already gone")
			}
			return false, derr.Error()
		}
		return b.revokeMigratedPoolTwin(ctx, row, "revoked")
	}
	return b.revokePoolKey(ctx, row.KeyID, row.APIKey)
}

// revokeMigratedPoolTwin best-effort-deletes the preserved POOL twin of a
// refunded llmgate-backend row (see revokeAPIKey's doc for WHY the twin must
// die on refund: the payout covers the remaining credits ONCE, and a live
// twin carries the same remaining — double value). lgDetail is the already-
// won llmgate outcome ("revoked" | "already gone"); the return combines both
// outcomes. A row with no preserved twin (minted directly on llmgate, never
// migrated) passes lgDetail through untouched. A TWIN failure must NOT fail
// the revoke — the refund has already committed — so the outcome stays ok
// and the owners are paged for manual cleanup, exactly like the pool
// branch's failure alert. Both key ids ride the alert (both non-secret
// identifiers); the twin's cleartext only ever appears masked.
func (b *Bot) revokeMigratedPoolTwin(ctx context.Context, row *store.APIKey, lgDetail string) (bool, string) {
	if row.PoolKeyID == "" && row.PoolAPIKey == "" {
		// Not a migrated row: there is no pool twin to kill — the llmgate
		// key was the only spendable thing, and it is dead.
		return true, lgDetail
	}
	twinOK, twinDetail := b.revokePoolKey(ctx, row.PoolKeyID, row.PoolAPIKey)
	if !twinOK {
		// _notify_owners — the twin-revoke-failure shape. The llmgate key is
		// dead and the refund WILL proceed; the preserved pool twin is still
		// live and spending, so this is a manual-cleanup page, never a block.
		// PoolAPIKey is the CUSTOMER's old cleartext pool key: masked only.
		log.Printf("OWNER ALERT: POOL TWIN NOT REVOKED for refunded llmgate key id %s — the preserved pool twin id %s (%s) is STILL LIVE and spending; DELETE it on Kiro-Go now (%s)",
			orDash(row.KeyID), orDash(row.PoolKeyID), maskAPIKey(row.PoolAPIKey), twinDetail)
		b.notifyOwners(ctx, fmt.Sprintf(
			"⚠️ POOL TWIN NOT REVOKED for refunded llmgate key id %s\n🔑 pool twin id %s (%s)\n"+
				"The llmgate key is revoked and the refund is paid, but the customer's preserved POOL key is STILL LIVE with the same remaining credits — double value.\n"+
				"DELETE it on Kiro-Go now. (%s)",
			orDash(row.KeyID), orDash(row.PoolKeyID), maskAPIKey(row.PoolAPIKey), twinDetail))
		return true, lgDetail + " llmgate; POOL TWIN REVOKE FAILED — manual cleanup"
	}
	// Combine the two outcomes into the one detail string the refund receipt
	// and log line carry. "already gone" on either side is still success —
	// the desired end state was reached for that half.
	lgGone, twinGone := lgDetail == "already gone", twinDetail == "already gone"
	switch {
	case !lgGone && !twinGone:
		return true, "revoked (llmgate + pool)"
	case lgGone && !twinGone:
		return true, "already gone on llmgate; pool twin revoked"
	case !lgGone:
		return true, "revoked llmgate; pool twin already gone"
	default:
		return true, "already gone (llmgate + pool)"
	}
}

// revokePoolKey deletes ONE key on the Kiro-Go/Pool gateway by uuid
// (falling back to the cleartext key when the uuid is empty) — the shared
// body of both pool revokes revokeAPIKey performs: a plain pool row's own
// key (key_id/api_key) and a migrated row's preserved twin
// (pool_key_id/pool_api_key). Same verdicts either way: a 404 is "already
// gone" (ok), a non-404 failure is !ok with the reason, and a 2xx whose
// upstreamRevoked is explicitly false pages the owners about the still-live
// llmgate-side key while still counting the pool delete as ok.
func (b *Bot) revokePoolKey(ctx context.Context, keyID, apiKey string) (bool, string) {
	client := b.kirogoClient()
	if client == nil {
		return false, "kiro-go not configured"
	}
	req := kirogo.DeleteAPIKeyRequest{}
	if keyID != "" {
		req.ID = keyID
	} else {
		req.APIKey = apiKey
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
		// _notify_owners — telegram_bot.py:6130-6144. Pool path only — see
		// revokeAPIKey's doc for why the llmgate branch has no such concept.
		// apiKey is the pool key's cleartext: masked only.
		log.Printf("OWNER ALERT: UPSTREAM KEY NOT REVOKED for pool key id %s (%s) — the Kiro-Go pool key is deleted, but its llmgate-side key is STILL LIVE and spending against our account; DELETE it manually in the llmgate portal now",
			orDash(keyID), maskAPIKey(apiKey))
		b.notifyOwners(ctx, fmt.Sprintf(
			"⚠️ UPSTREAM KEY NOT REVOKED for pool key id %s (%s)\n"+
				"The Kiro-Go pool key is deleted, but its llmgate-side key is STILL LIVE and spending against our account.\n"+
				"DELETE it manually in the llmgate portal now.",
			orDash(keyID), maskAPIKey(apiKey)))
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
