package handlers

import (
	"context"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// cbRoute is one entry of the ordered callback dispatch table (the port of
// handle_callback's if/elif prefix chain @12287-15697). `key` is either an exact
// data string (exact=true, e.g. "keepord") or a prefix (exact=false, e.g.
// "buy:"). First match wins, mirroring the Python chain order.
type cbRoute struct {
	key   string
	exact bool
	fn    func(b *Bot, cc *cbCtx)
}

// cbCtx is the per-callback working context handed to a route handler.
type cbCtx struct {
	Ctx    context.Context
	ChatID int64
	Data   string
	CB     *tg.CallbackQuery
	st     *ChatState
}

// registerCatalog appends the M2.2 catalog + purchase capture(s) and callback
// routes to the ordered dispatch slices. Called once from New. The capture chain
// stays an in-order subsequence of the canonical list (only "purchase" lands
// here; the earlier captures are other slices'), so TestCaptureChainOrder still
// passes.
func (b *Bot) registerCatalog() {
	// Text-capture chain: pending_purchase (buy-qty free text) is entry #48,
	// deliberately LAST (telegram_bot.py:18676; handlers-go.md §2.2 table).
	b.captureChain = append(b.captureChain, stage{name: "purchase", fn: (*Bot).capPurchase})

	// Callback routes in handle_callback chain order (customer buy pipeline).
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "cancelord:", fn: (*Bot).cbCancelOrd},   // @15345
		cbRoute{key: "keepord", exact: true, fn: (*Bot).cbKeepOrd}, // @15367
		cbRoute{key: "back:products", exact: true, fn: (*Bot).cbBackProducts}, // @15019
		cbRoute{key: "buyx:", fn: (*Bot).cbBuyX},   // @15032
		cbRoute{key: "buy:", fn: (*Bot).cbBuy},     // @15224
		cbRoute{key: "buygo:", fn: (*Bot).cbBuyGo}, // @15262
		cbRoute{key: "tier:", fn: (*Bot).cbTier},   // @15281
		cbRoute{key: "buyqty:", fn: (*Bot).cbBuyQty}, // @15330
		cbRoute{key: "paybal:", fn: (*Bot).cbPayBalance}, // @15819
		// Pay-by-QR (SePay VietQR) and Pay-by-PayPal (Orders v2 API + webhook)
		// were removed by the USDT-only migration (S1/S2) — SePay and PayPal are
		// gone. Remaining pay-method buttons stay as stubs until their slices
		// land; applycode/rmcode and the self-service routes are now real
		// handlers wired in M2.6.
		cbRoute{key: "paybnb:", fn: (*Bot).cbPayMethodStub},
		cbRoute{key: "paybsc:", fn: (*Bot).cbPayMethodStub},
		cbRoute{key: "paytrx:", fn: (*Bot).cbPayMethodStub},
		// M2.6: apply/remove promo-referral code on a reserved order (@16055/16069).
		cbRoute{key: "applycode:", fn: (*Bot).cbApplyCode},
		cbRoute{key: "rmcode:", fn: (*Bot).cbRemoveCode},
		// M2.6: Self-Service submenu (ss:*) and refund-request picker (refreq:).
		cbRoute{key: "ss:", fn: (*Bot).cbSelfService},
		cbRoute{key: "refreq:", fn: (*Bot).cbFileRefundReq},
	)
}

// dispatchCB runs the first matching route for a callback, returning true if one
// handled it (so handleCallback can fall through to nothing on no match).
func (b *Bot) dispatchCB(cc *cbCtx) bool {
	for _, r := range b.cbRoutes {
		if (r.exact && cc.Data == r.key) || (!r.exact && strings.HasPrefix(cc.Data, r.key)) {
			r.fn(b, cc)
			return true
		}
	}
	return false
}

// cbPayMethodStub re-renders the pay screen for the reserved order named in the
// callback ("prefix:des" — the remaining unwired pay-method buttons plus
// applycode:/rmcode:). It is ownership-guarded so a forged des only ever
// re-shows the tapper their own order (or ORDER_GONE), never another chat's.
func (b *Bot) cbPayMethodStub(cc *cbCtx) {
	_, des, ok := strings.Cut(cc.Data, ":")
	if !ok || des == "" {
		return
	}
	o, err := b.store.GetOrder(des)
	langEN := b.langFor(cc.ChatID, "en")
	if err != nil || o == nil || o.Status != "reserved" || o.ChatID != fmtInt(cc.ChatID) {
		b.send(cc.Ctx, cc.ChatID, i18n.C().Buy.OrderGone.T(langEN), nil)
		return
	}
	b.sendPayScreen(cc.Ctx, cc.ChatID, des, "")
}
