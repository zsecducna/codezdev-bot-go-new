package handlers

// admin.go — the admin panel ENTRY POINT (first cut of the M3.1 slice).
//
// Why this file exists: mainKeyboard (render.go) has always drawn the 🔑 Admin
// reply button for admins, but step 9 of Process (dispatch.go) iterated an EMPTY
// adminText slice and labelMap deliberately never mapped the Admin label — so a
// tap fell through every stage and died at "unknown plain text -> no-op". The
// button looked broken because nothing was listening.
//
// Ports:
//   - text == ADMIN_BUTTON        (telegram_bot.py:17797) → capAdminButton
//   - /stock|orders|…|admin|pool  (telegram_bot.py:17801) → capAdminCmd
//   - handle_admin                (telegram_bot.py:17383) → handleAdmin
//   - _admin_dashboard_text       (telegram_bot.py:16363) → adminDashboardText
//   - _admin_stock_text           (telegram_bot.py:16341) → adminStockText
//   - _admin_category_menu        (telegram_bot.py:11823) → adminCategoryMenu
//   - _back_kb / _adm_nav         (telegram_bot.py:11794/11805) → backKB / admNav
//   - admroot / admcat: routing   (telegram_bot.py:12697/12865) → cbAdmRoot/cbAdmCat
//
// Scope note (deliberate, reported to the user): only the panel NAVIGATION plus
// the three read-only views whose Go backing already exists (dashboard, stock
// overview, recent orders) do real work here. Every other action button and
// admin slash command answers with an explicit "not ported yet" notice carrying
// a Back button. That is the whole point of the fix: an admin tap must always
// produce a visible reply, never silence. Later M3 slices replace each notice
// with its real flow; the callback data strings are already the Python ones, so
// those slices only swap handlers.
//
// The standard-admin capability filter (_filter_admin_rows @11770, admin_can)
// is NOT ported: the standard-admin set (admins.json) is still empty (dispatch.go
// New), so every admin reaching this file is a full admin and the filter would
// be a no-op. Landing it with admins.json in M3.1 keeps the two halves of that
// feature in one reviewable change instead of a half-enforced gate today.

import (
	"context"
	"strconv"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// adminPanelCmds is the set of slash commands routed to handleAdmin from step 9
// — the tuple at telegram_bot.py:17802-17803. Kept as a set (not a slice) since
// only membership is asked; the ORDER that matters (the two adminText stages) is
// pinned by CanonicalAdminTextOrder below.
// D2 fix (orders/refunds review, landed alongside this slice): this used to
// be a STRICT SUBSET of Python's _ADMIN_CMDS (telegram_bot.py:211-213) — Go
// had only {stock, orders, addbal, broadcast, refund, getord, admin, pool}
// (+ this slice's own "audit"). A command word ABSENT from this map makes
// capAdminCmd return false — that is NO GATE AT ALL, not deny — so /reset,
// /ban, /unban, /banned, /fixhandle, /restart, /suspend used to bypass
// stdCmdCap entirely rather than being denied by it. Now the FULL Python set
// is gated; handleAdmin's default case still answers adminNotPorted for
// whichever of these have no real case yet, so the not-yet-ported ones stay
// honest (a visible "not wired up" reply) while becoming properly gated.
var adminPanelCmds = map[string]bool{
	"stock": true, "orders": true, "addbal": true, "broadcast": true,
	"refund": true, "getord": true, "admin": true, "pool": true, "audit": true,
	"reset": true, "ban": true, "unban": true, "banned": true,
	"fixhandle": true, "restart": true, "suspend": true,
}

// CanonicalAdminTextOrder is the source order of the step-9 admin text stages
// (handlers-go.md §2.1; telegram_bot.py:17123-17215). Same contract as
// CanonicalCaptureOrder: append-only, and a golden test pins the registered
// stages against this list as an in-order SUBSEQUENCE so later slices can land
// their entries (the std-admin command gate, /suspend, /fixhandle, /restart, …)
// without renegotiating precedence.
var CanonicalAdminTextOrder = []string{
	"stdcmdgate",  // 1  @17128 — standard-admin command capability gate (M3.1)
	"ban",         // 2  @17137 — /ban|/unban
	"bannedlist",  // 3  @17145 — /banned
	"adminbutton", // 4  @17151 — text == ADMIN_BUTTON
	"admincmd",    // 5  @17155 — /stock|orders|addbal|broadcast|refund|getord|admin|pool
	"reset",       // 6  @17165 — /reset quick + batch
	"suspend",     // 7  @17193 — /suspend
	"fixhandle",   // 8  @17203 — /fixhandle
	"restart",     // 9  @17208 — /restart
	"admininput",  // 10 @17213 — pendingAdmin multi-step collector
}

// adminNotPorted is the reply an admin gets for a panel action whose flow has
// not landed yet. Fixed English like every other admin-surface string in the
// Python original (admins are never a localized audience there).
const adminNotPorted = "🚧 Not wired up yet in this build.\n" +
	"The button is live and its action lands in a later admin slice — " +
	"nothing was changed."

// registerAdmin wires the panel into the ordered dispatch tables. Called once
// from New. Step 9 gets the two entry stages in Python order (button before
// slash commands), and the callback table gets every prefix the panel screens
// can emit — including the not-yet-ported ones, so no rendered button is ever a
// dead tap.
func (b *Bot) registerAdmin() {
	b.adminText = append(b.adminText,
		stage{name: "adminbutton", fn: (*Bot).capAdminButton}, // #4 @17151
		stage{name: "admincmd", fn: (*Bot).capAdminCmd},       // #5 @17155
	)

	// Callback routes. admroot is EXACT; the rest are prefixes. Order matters
	// only in that "admcat:" and "adm:" are disjoint, so first-match is stable.
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "admroot", exact: true, fn: (*Bot).cbAdmRoot}, // @12697
		cbRoute{key: "admcat:", fn: (*Bot).cbAdmCat},               // @12865
		cbRoute{key: "adm:", fn: (*Bot).cbAdmAction},               // @12808+
		// Prefixes reachable from the panels rendered here whose handlers belong
		// to later slices. Routed to the same notice so the tap always answers.
		cbRoute{key: "admrefdel", exact: true, fn: (*Bot).cbAdmRefDel},     // 🗑️ Delete Refunded Accounts (adminorders.go)
		cbRoute{key: "admpoolr", exact: true, fn: (*Bot).cbAdmPoolRefresh}, // pool refresh (adminstock.go)
		// Resellers / whitelist (adminusers.go): full-admin-only management
		// submenu + per-entry actions, and the toggle_sales-capability
		// tap-to-remove route.
		cbRoute{key: "resmng:", fn: (*Bot).cbResMng},
		cbRoute{key: "resdisc:", fn: (*Bot).cbResDisc},
		cbRoute{key: "rmreseller:", fn: (*Bot).cbRmReseller},
		cbRoute{key: "rmwl:", fn: (*Bot).cbRmWL},
		cbRoute{key: "refrev:", fn: (*Bot).cbRefundReview}, // refund-request review (adminorders.go)
		cbRoute{key: "refrej:", fn: (*Bot).cbRefundReject}, // refund-request reject (adminorders.go)
	)
}

// adminTextOrderOK is used by the golden-order test: the registered step-9 stage
// names must be an in-order subsequence of CanonicalAdminTextOrder.
func (b *Bot) adminTextOrderOK() bool {
	i := 0
	for _, name := range b.adminTextNames() {
		for i < len(CanonicalAdminTextOrder) && CanonicalAdminTextOrder[i] != name {
			i++
		}
		if i == len(CanonicalAdminTextOrder) {
			return false
		}
		i++
	}
	return true
}

// ---- step 9 stages -----------------------------------------------------------

// capAdminButton consumes a tap on the 🔑 Admin reply button — telegram_bot.py:
// 17797-17800. Tapping Admin also ABANDONS any half-typed admin prompt
// (_clear_admin_pending @17798) so a stale flag can't eat the next plain text.
// Only reached for admins: Process gates the whole adminText slice on isAdmin.
func (b *Bot) capAdminButton(c *Ctx) bool {
	if c.Text != i18n.AdminButton {
		return false
	}
	c.st.ClearAdminPending()
	b.handleAdmin(c.Ctx, c.ChatID, "admin", c.Text)
	return true
}

// capAdminCmd routes the admin slash commands that open the panel or one of its
// views — telegram_bot.py:17801-17808. Same clear-pending rule as the button.
//
// H1 fix (Opus review of admincap.go): the callback surface (admincap.go's
// stdAdminCbCap, wired into command.go) was fully policed, but this slash-
// command path had NO capability gate at all — a standard admin denied
// `adm:orders`/`admcat:orders` could still type `/orders 50` and get the full
// orders_view report (customer chat ids, product, tier, total, des), since
// step 9 only requires isAdmin (dispatch.go). Port of the second Python
// chokepoint (telegram_bot.py:17766-17779): a command absent from stdCmdCap,
// or present with a capability the chat doesn't hold, is denied with a plain
// chat message (this path has no callback_query to answer, unlike the
// alert-based callback gate). Full admins always pass.
func (b *Bot) capAdminCmd(c *Ctx) bool {
	if !strings.HasPrefix(c.Text, "/") {
		return false
	}
	cmd := parseSlashCommand(c.Text)
	if !adminPanelCmds[cmd] {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		cap, governed := stdCmdCap[cmd]
		if !governed || (cap != "" && !b.adminCan(c.ChatID, cap)) {
			b.send(c.Ctx, c.ChatID, settingsPermissionDenied, nil)
			return true
		}
	}
	c.st.ClearAdminPending()
	b.handleAdmin(c.Ctx, c.ChatID, cmd, c.Text)
	return true
}

// ---- handleAdmin -------------------------------------------------------------

// handleAdmin is the admin command handler — port of handle_admin
// (telegram_bot.py:17383). The caller has already verified isAdmin (step 9's
// gate, or the isAdmin check inside each callback route).
func (b *Bot) handleAdmin(ctx context.Context, chatID int64, cmd, text string) {
	switch cmd {
	case "admin":
		b.showAdminPanel(ctx, chatID)
	case "stock":
		// Read-only per-product stock report (@17446-17449).
		b.send(ctx, chatID, b.adminStockText(), admNav("admroot"))
	case "orders":
		// /orders [N] — newest first, N capped at 50 (@17458-17461).
		n := 20
		if parts := strings.Fields(text); len(parts) > 1 {
			if v, err := strconv.Atoi(parts[1]); err == nil && v > 0 {
				n = min(v, 50)
			}
		}
		b.sendRecentOrders(ctx, chatID, n)
	case "pool":
		// /pool — pool-health view (adminstock.go, py_stock_spec.md §7). Fresh
		// send (edit_message_id=0), matching adm:pool's own callback branch.
		b.sendPoolView(ctx, chatID, 0)
	case "audit":
		// /audit [query] — the arg is everything after the FIRST space, raw
		// (py_stock_spec.md §8.3: `text.split(" ",1)[1].strip()`).
		query := ""
		if _, rest, found := strings.Cut(text, " "); found {
			query = strings.TrimSpace(rest)
		}
		b.sendAuditLog(ctx, chatID, query)
	default:
		// addbal / broadcast / refund / getord / pool: their input-collecting
		// flows are later slices. Answer explicitly instead of dropping the
		// command silently (the bug this file fixes).
		b.send(ctx, chatID, adminNotPorted, admNav("admroot"))
	}
}

// showAdminPanel renders the root panel: the dashboard text plus one button per
// CATEGORY — the cmd=="admin" branch of handle_admin (telegram_bot.py:17386-17417).
// Callback data is unchanged from Python so later slices only add handlers.
func (b *Bot) showAdminPanel(ctx context.Context, chatID int64) {
	rows := [][]tg.InlineKeyboardButton{
		btnRow("💰 Revenue", "adm:revenue"),
		btnRow("📦 Stock", "admcat:stock"),
		btnRow("🧾 Orders", "admcat:orders"),
		btnRow("👥 Customers", "admcat:customers"),
		btnRow("🏷️ Pricing & discounts", "admcat:pricing"),
		btnRow("⚙️ Settings", "admcat:settings"),
		btnRow("☁️ Entra / Kiro SSO", "admcat:entra"),
		btnRow("🏊 Pool health", "adm:pool"),
		btnRow("📜 Audit log", "adm:audit"),
	}
	// H1 fix: a standard admin sees only the categories/actions their
	// capabilities unlock (_filter_admin_rows, telegram_bot.py:17409); a full
	// admin sees everything (the filter no-ops) plus Admin roles below.
	rows = b.filterAdminRows(chatID, rows)
	// Admin roles configuration is owner-only (@17414-17415).
	prompt := "🔑 Choose an action:"
	if b.isSuperAdmin(chatID) {
		rows = append(rows, btnRow("🛡️ Admin roles", "admcat:roles"))
		prompt = "🔑 Choose a category:"
	}
	b.sendMenu(ctx, chatID, b.adminDashboardText(chatID)+"\n\n"+prompt, rows)
}

// ---- dashboard / stock text --------------------------------------------------

// adminDashboardText builds the panel header — port of _admin_dashboard_text
// (telegram_bot.py:16363-16398): the revenue/orders summary plus a compact
// per-product stock line. A missing store or stock (unit tests, or a partially
// configured install) drops the corresponding block rather than failing the
// whole render, so the panel always opens.
func (b *Bot) adminDashboardText(chatID int64) string {
	parts := []string{"📈 ADMIN DASHBOARD", strings.Repeat("─", 24)}
	if b.store != nil {
		if r, err := b.store.GetRevenueSummary(depositProduct); err == nil {
			parts = append(parts, "💰 REVENUE (delivered orders)\n"+
				"   Total: "+b.usdtDisp(r.DeliveredRevenue)+"  ("+itoa64(r.DeliveredCount)+" orders)\n"+
				"   Today: "+b.usdtDisp(r.TodayRevenue)+"  ("+itoa64(r.TodayCount)+" orders)\n"+
				"   Customers: "+itoa64(r.Customers)+"\n"+
				"\n🧾 ORDERS\n"+
				"   ⏳ Pending: "+itoa64(r.PendingCount)+"    ↩️ Refunded/expired: "+itoa64(r.RefundedCount)+"\n"+
				"   💳 Wallet balances held: "+b.usdtDisp(r.WalletTotal))
		}
	}
	if b.stock != nil {
		var lines []string
		total := 0
		for _, p := range b.listProducts() {
			a, err := b.stock.CountAvailable(p)
			if err != nil {
				continue
			}
			total += a
			line := "   • " + p + ": 🟢 " + itoa(a)
			if h := b.stock.CountHeld(p); h > 0 {
				line += "  🔒 " + itoa(h) + " held"
			}
			lines = append(lines, line)
		}
		body := "   (no products)"
		if len(lines) > 0 {
			body = strings.Join(lines, "\n")
		}
		parts = append(parts, "📦 STOCK (available: "+itoa(total)+")\n"+body)
	}
	return strings.Join(parts, "\n")
}

// adminStockText builds the /stock report — port of _admin_stock_text
// (telegram_bot.py:16341-16360): per-product available/reserved/sold counts plus
// that product's tier prices.
func (b *Bot) adminStockText() string {
	if b.stock == nil {
		return "📊 STOCK OVERVIEW\n" + strings.Repeat("─", 24) + "\n\nNo products."
	}
	var blocks []string
	for _, p := range b.listProducts() {
		a, err := b.stock.CountAvailable(p)
		if err != nil {
			continue
		}
		r, err := b.stock.CountReserved(p)
		if err != nil {
			r = 0
		}
		block := "📦 " + p + "\n" +
			"   🟢 Available: " + itoa(a) +
			"    🔒 Reserved: " + itoa(r) +
			"    ✅ Sold: " + itoa(b.stock.CountSold(p))
		for _, t := range b.stock.ProductTiers(p) {
			block += "\n   • " + t.Code + ": " + b.usdtDisp(int64(t.Price))
		}
		blocks = append(blocks, block)
	}
	body := "No products."
	if len(blocks) > 0 {
		body = strings.Join(blocks, "\n\n")
	}
	return "📊 STOCK OVERVIEW\n" + strings.Repeat("─", 24) + "\n\n" + body
}

// sendRecentOrders renders the newest N orders — the cmd=="orders" branch
// (telegram_bot.py:17458-17487). Telegram hard-caps a message at 4096 chars, so
// the list is trimmed at ~3900 like every other admin list builder; without the
// trim sendMessage 400s and the admin sees nothing.
func (b *Bot) sendRecentOrders(ctx context.Context, chatID int64, n int) {
	if b.store == nil {
		b.send(ctx, chatID, "No orders yet.", admNav("admcat:orders"))
		return
	}
	orders, err := b.store.RecentOrders(n, nil)
	if err != nil || len(orders) == 0 {
		b.send(ctx, chatID, "No orders yet.", admNav("admcat:orders"))
		return
	}
	out := []string{"🧾 Last " + itoa(len(orders)) + " orders:"}
	used := 0
	for _, o := range orders {
		product := o.Product
		if len(product) > 20 {
			product = product[:20]
		}
		des := o.Des
		if len(des) > 8 {
			des = des[:8]
		}
		line := padRight(o.Status, 9) + " " + itoa64(o.Qty) + "× " + product +
			" (" + o.Tier + ") " + b.usdtDisp(o.Total) +
			"  chat=" + o.ChatID + "  " + des
		if used+len(line)+1 > 3900 {
			out = append(out, "… truncated, showing fewer than "+itoa(len(orders))+" (ask a smaller N)")
			break
		}
		used += len(line) + 1
		out = append(out, line)
	}
	b.send(ctx, chatID, strings.Join(out, "\n"), admNav("admcat:orders"))
}

// padRight left-aligns s in a width-w field, matching Python's f"{s:9}" in the
// orders list. Longer strings are returned untouched (Python does the same).
func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// ---- nav keyboards -----------------------------------------------------------

// admBackRoot is ADM_BACK_ROOT (telegram_bot.py:11791) — the shared Back-to-root
// row every category submenu ends with.
func admBackRoot() []tg.InlineKeyboardButton { return btnRow("↩️ Back", "admroot") }

// backKB is _back_kb (telegram_bot.py:11794-11802): a lone Back button attached
// to a text-input PROMPT, so an admin who changes their mind can leave without
// typing (the nav handlers clear the half-finished pending input first).
func backKB(dest string) *tg.InlineKeyboardMarkup {
	return &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{btnRow("↩️ Back", dest)}}
}

// admNav is _adm_nav (telegram_bot.py:11805-11820): the keyboard for a TERMINAL
// admin message (result/confirmation/error) so an admin never dead-ends and has
// to retype /admin.
func admNav(back string) *tg.InlineKeyboardMarkup { return backKB(back) }

// ---- category menus ----------------------------------------------------------

// adminCategoryMenu returns (title, rows) for a category submenu — port of
// _admin_category_menu (telegram_bot.py:11823-11983+). The action callbacks are
// the Python ones verbatim, so a later slice implementing an action needs no
// re-render change here. ok=false for an unknown category (a stale button).
//
// Toggle labels read LIVE settings, exactly as Python does, so the label tells
// the truth even while the toggle handler itself is still a later slice.
//
// H1 fix: this wraps adminCategoryMenuRows with filterAdminRows
// (_filter_admin_rows, telegram_bot.py:12875) so EVERY caller — cbAdmCat,
// cbRmWL, cbRmReseller, cbResMng, cbResDisc, TestAdminActionsAlwaysAnswer's
// probe — gets a standard-admin-safe row set for free, instead of each call
// site needing to remember to filter.
func (b *Bot) adminCategoryMenu(chatID int64, cat string) (title string, rows [][]tg.InlineKeyboardButton, ok bool) {
	title, rows, ok = b.adminCategoryMenuRows(cat)
	if !ok {
		return title, rows, ok
	}
	return title, b.filterAdminRows(chatID, rows), ok
}

// adminCategoryMenuRows is the raw (unfiltered) row builder — port of
// _admin_category_menu (telegram_bot.py:11823-11983+). See adminCategoryMenu
// above for the standard-admin row filter every caller now gets automatically.
func (b *Bot) adminCategoryMenuRows(cat string) (title string, rows [][]tg.InlineKeyboardButton, ok bool) {
	s := b.settings()
	switch cat {
	case "stock":
		return "📦 STOCK", append([][]tg.InlineKeyboardButton{
			btnRow("📊 Stock overview", "adm:stock"),
			btnRow("📥 Upstock from text/file", "adm:upstock"),
			btnRow("🏬 Upstock from store", "adm:upstockstore"),
			btnRow("🔒 Hold / restore stock", "adm:holdstock"),
			btnRow("🗂️ Manage products", "adm:mngprod"),
			btnRow("🔥 Stock update broadcast", "adm:stockbroadcast"),
			btnRow("🌐 Provision Kiro pool", "adm:provpool"),
		}, admBackRoot()), true
	case "orders":
		return "🧾 ORDERS", append([][]tg.InlineKeyboardButton{
			btnRow("🧾 Recent orders", "adm:orders"),
			btnRow("🔎 Look up order by ID", "adm:getord"),
			btnRow("💸 Refund order", "adm:refund"),
			btnRow("🔁 Refund requests ("+itoa64(b.pendingRefundCount())+")", "admcat:refundreq"),
			btnRow("🗑️ Delete Refunded Accounts", "admrefdel"),
		}, admBackRoot()), true
	case "customers":
		return "👥 CUSTOMERS", append([][]tg.InlineKeyboardButton{
			btnRow("🔎 Look up customer", "adm:custlookup"),
			btnRow("💰 Update balance (+/−)", "adm:addbal"),
		}, admBackRoot()), true
	case "pricing":
		return "🏷️ PRICING & DISCOUNTS", append([][]tg.InlineKeyboardButton{
			btnRow("🏷️ Prices & warranty tiers", "adm:setprice"),
			btnRow("📉 Volume discounts", "adm:setdiscount"),
			btnRow("🎟️ Promo codes", "adm:promocodes"),
			btnRow(b.apiPricingLabel("🔑 API pricing", saleClassBedrock), "adm:apiprice"),
			btnRow("🔽 Min credits/key: "+itoa64(b.apikeyMinCredits(saleClassBedrock)), "adm:apimin"),
			btnRow("🔢 Max credits/key: "+itoa64(b.apikeyMaxCredits(saleClassBedrock)), "adm:apimax"),
			btnRow(b.apiCapLabel(), "adm:apicap"),
			btnRow(onOffLabel("🟣 Kiro SKU", b.kiroApikeyEnabled()), "adm:kirotoggle"),
			btnRow(b.apiPricingLabel("🟣 Kiro pricing", saleClassKiro), "adm:kiroprice"),
			btnRow("🟣 Kiro min credits/key: "+itoa64(b.apikeyMinCredits(saleClassKiro)), "adm:kiromin"),
			btnRow("🟣 Kiro max credits/key: "+itoa64(b.apikeyMaxCredits(saleClassKiro)), "adm:kiromax"),
		}, admBackRoot()), true
	case "settings":
		return "⚙️ SETTINGS", append([][]tg.InlineKeyboardButton{
			btnRow("📣 Broadcast", "adm:broadcast"),
			btnRow(bellLabel("Sale broadcast", settingBool(s, "sale_broadcast", true)), "adm:togglebcast"),
			btnRow(bellLabel("Auto restock alert", settingBool(s, "auto_stock_broadcast", true)), "adm:togglestockbcast"),
			btnRow(bellLabel("New-sale alerts", settingBool(s, "admin_sale_notify", true)), "adm:togglesalenotify"),
			btnRow(bellLabel("New-deposit alerts", settingBool(s, "admin_deposit_notify", true)), "adm:toggledepnotify"),
			btnRow(maintenanceLabel(settingBool(s, "maintenance", false)), "adm:togglemaint"),
			btnRow(salesLabel(settingBool(s, "sales_paused", false)), "adm:togglesales"),
			btnRow(privateSaleLabel(settingBool(s, "private_sale", false)), "adm:toggleprivate"),
			btnRow("👥 Private-sale customers ("+itoa(len(b.whitelist()))+")", "admcat:whitelist"),
			btnRow(referralLabel(b.referralEnabled()), "adm:togglereferral"),
			btnRow("🤝 Authorized resellers ("+itoa(len(b.resellerList()))+")", "admcat:resellers"),
			btnRow("💳 Payment methods", "admcat:payments"),
			btnRow("🧑‍⚖️ Standard-admin permissions", "admcat:stdperms"),
			btnRow("🔢 Max bulk buy: "+bulkLabel(b.maxBulk()), "adm:setmaxqty"),
			btnRow("💱 USD→VND rate: "+b.rateStr(), "adm:setrate"),
			btnRow("🌐 Kiro-Go Server URL: "+setUnsetValue(b.kirogoURLStr()), "adm:setkirourl"),
			btnRow("🔑 Kiro-Go Admin Password: "+setUnsetState(b.kirogoKeySet()), "adm:setkirokey"),
			btnRow("🆔 Kiro-Go Account ID: "+setUnsetValue(b.kirogoAccountStr()), "adm:setkiroacct"),
		}, admBackRoot()), true
	case "payments":
		return "💳 PAYMENT METHODS", [][]tg.InlineKeyboardButton{
			btnRow("💰 Balance (wallet): "+onOffState(b.payEnabled("balance")), "adm:togglepay:balance"),
			btnRow("📷 Bank QR (VN): "+onOffState(b.payEnabled("qr")), "adm:togglepay:qr"),
			btnRow("🌍 PayPal: "+onOffState(b.payEnabled("paypal")), "adm:togglepay:paypal"),
			btnRow("🪙 Binance USDT: "+b.payMethodLabel("binance", b.binanceConfigured, "keys not set"), "adm:togglepay:binance"),
			btnRow("🟡 USDT BEP20: "+b.payMethodLabel("bep20", b.bep20Configured, "address not set"), "adm:togglepay:bep20"),
			btnRow("🟡 Set BEP20 address: "+setUnsetValue(b.bep20AddrStr()), "adm:setbep20"),
			btnRow("🔴 USDT TRC20 (TRON): "+b.payMethodLabel("tron", b.tronConfigured, "address not set"), "adm:togglepay:tron"),
			btnRow("🔴 Set TRON address: "+setUnsetValue(b.tronAddrStr()), "adm:settron"),
			btnRow("↩️ Back", "admcat:settings"),
		}, true
	case "roles":
		return "🛡️ ADMIN ROLES", append([][]tg.InlineKeyboardButton{
			btnRow("📋 List admins", "adm:listadmins"),
			btnRow("➕ Add admin", "adm:addadmin"),
			btnRow("➖ Remove admin", "adm:rmadmin"),
		}, admBackRoot()), true
	case "entra":
		return "☁️ ENTRA / KIRO SSO", append([][]tg.InlineKeyboardButton{
			btnRow("🔐 Login (device code)", "adm:entralogin"),
			btnRow("📋 Session status", "adm:entrastatus"),
			btnRow("🏢 Create Enterprise App", "adm:entracreateapp"),
			btnRow("👥 Generate users", "adm:entragenusers"),
			btnRow("🔁 Verify CSV → store", "adm:entraverifycsv"),
			btnRow("🔑 Reset password", "adm:entraresetpw"),
			btnRow("🔗 Configure provisioning", "adm:entraprovision"),
			btnRow("🧹 Delete users in group (reclaim quota)", "adm:entradelusers"),
			btnRow("🧼 Clean Up Orders (delete expired tenant users)", "adm:entracleanup"),
			btnRow("🗑 Delete Enterprise App", "adm:entradelapp"),
			btnRow("🚪 Sign out", "adm:entralogout"),
		}, admBackRoot()), true
	case "resellers":
		// Each current reseller is its own manage submenu, keyed BY USERNAME
		// (usernames are [A-Za-z0-9_], so they never contain ':' and the callback
		// stays inside Telegram's 64-byte budget) — @11999-12019.
		rz := b.resellerList()
		for _, r := range rz {
			label := "🤝 @" + r.U
			if r.Note != "" {
				label += " — " + r.Note
			}
			if r.Disc > 0 {
				label += " · " + itoa(r.Disc) + "% off"
			}
			rows = append(rows, btnRow(label, "resmng:"+r.U))
		}
		rows = append(rows,
			btnRow("➕ Add reseller", "adm:addreseller"),
			btnRow("↩️ Back", "admcat:settings"))
		tail := "None yet — add one below."
		if len(rz) > 0 {
			tail = "Tap a reseller to set its discount or remove it."
		}
		return "🤝 AUTHORIZED RESELLERS\nShown to customers on the Buy screen (/buy).\n" +
			"Each reseller can have its OWN reselling discount, applied to their " +
			"purchases (a promo/referral code overrides it).\n" + tail, rows, true
	case "whitelist":
		// Private-sale whitelist: tap-to-remove per entry, add by typed
		// id-or-@username. Removal is BY VALUE, not index (@12020+).
		wl := b.whitelist()
		for _, e := range wl {
			label := e.V
			if !isAllDigits(e.V) {
				label = "@" + e.V
			}
			if e.Note != "" {
				label += " — " + e.Note
			}
			rows = append(rows, btnRow("👤 "+label, "rmwl:"+e.V))
		}
		rows = append(rows,
			btnRow("➕ Add customer", "adm:addwl"),
			btnRow("↩️ Back", "admcat:settings"))
		// L11 fix: this diverged from Python's verbatim wording (py_customers_
		// spec.md §6, telegram_bot.py:12031-12033) — restored to the literal
		// strings rather than a paraphrase.
		tail := "None yet — add one below."
		if len(wl) > 0 {
			tail = "Tap a customer to remove."
		}
		return "👥 PRIVATE-SALE CUSTOMERS\nThey can buy while sale is paused + " +
			"Private Sale is ON (resellers also pass).\n" + tail, rows, true
	case "stdperms":
		// Super-admin only (gated by the caller). M2 fix: the old text here
		// ("No standard admins exist in this build...") went stale the moment
		// dispatch.go's New() started loading admins.json for real (H1) —
		// standard admins can now exist, and the old wording actively misled
		// the super-admin into thinking there was nothing to restrict. This
		// describes the REAL state: admincap.go's fixed defaultStdCaps set
		// (every standard admin holds exactly these four, no per-admin
		// grant/revoke UI exists yet) rather than a toggle list the runtime
		// can't actually enforce per-admin.
		return "🧑‍⚖️ STANDARD-ADMIN PERMISSIONS\n" +
				"A granted standard admin (admins.json, not the owner or the developer " +
				"override) currently holds a FIXED capability set — the same four for " +
				"every standard admin, with no per-admin grant/revoke UI yet:\n" +
				"  • 📦 Stock — read + hold/restore\n" +
				"  • 📜 Audit log\n" +
				"  • toggle_sales — Sales on/off, Private sale, private-sale whitelist\n" +
				"Everything else (Customers, Orders, Revenue, Pricing, Payments, " +
				"Pool health, Admin roles, Resellers, password reset, …) is full-admin-only.",
			[][]tg.InlineKeyboardButton{btnRow("↩️ Back", "admcat:settings")}, true
	case "refundreq":
		// The live refund-request queue (adminorders.go, py_orders_spec.md).
		return b.refundRequestQueueMenu()
	}
	return "", nil, false
}

// ---- label helpers -----------------------------------------------------------
//
// The Python menu builds its toggle labels inline; Go needs them as small named
// helpers to keep adminCategoryMenu readable. Each mirrors one f-string.

// settingBool reads a bool settings key with a default — the s.get(key, default)
// pattern the menu builder uses throughout.
func settingBool(s map[string]any, key string, def bool) bool {
	if v, ok := s[key].(bool); ok {
		return v
	}
	return def
}

func onOffLabel(prefix string, on bool) string {
	if on {
		return prefix + ": ON"
	}
	return prefix + ": OFF"
}

func onOffState(on bool) string {
	if on {
		return "ON ✅"
	}
	return "OFF 🚫"
}

func bellLabel(name string, on bool) string {
	if on {
		return "🔔 " + name + ": ON"
	}
	return "🔕 " + name + ": OFF"
}

func maintenanceLabel(on bool) string {
	if on {
		return "🛠️ Maintenance: ON"
	}
	return "🟢 Maintenance: OFF"
}

func salesLabel(paused bool) string {
	if paused {
		return "⏸️ Sales: PAUSED"
	}
	return "🟢 Sales: ON"
}

func privateSaleLabel(on bool) string {
	if on {
		return "🔒 Private sale: ON ✅"
	}
	return "🔓 Private sale: OFF 🚫"
}

func referralLabel(on bool) string {
	if on {
		return "👥 Referral program: ON"
	}
	return "🚫 Referral program: OFF"
}

// bulkLabel renders the max-bulk cap, 0 meaning unlimited (@11921's `or '∞'`).
func bulkLabel(n int) string {
	if n == 0 {
		return "∞"
	}
	return itoa(n)
}

// setUnsetValue shows a non-secret config value, or "(not set)" — the
// current_*_str helpers' empty case.
func setUnsetValue(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

// setUnsetState reports only whether a SECRET is configured. The Kiro-Go admin
// key must never be rendered — surfacing it would leak an account-wide
// credential into chat history (CLAUDE.md: never store, log or forward upstream
// credentials), which is why this takes a bool, not the key.
func setUnsetState(isSet bool) string {
	if isSet {
		return "set ✅"
	}
	return "not set 🚫"
}

// apiPricingLabel renders one SKU's price-per-100-credits row, or OFF when the
// rate is 0 (a 0 rate HIDES the product) — @11871-11873/11887-11889.
func (b *Bot) apiPricingLabel(prefix, saleClass string) string {
	rate := b.apikeyRateUSDT(saleClass)
	if rate <= 0 {
		return prefix + ": OFF"
	}
	return prefix + ": " + strconv.FormatFloat(rate, 'g', -1, 64) + " USDT/100cr"
}

// apiCapLabel renders the per-customer key cap, 0 meaning unlimited (@11878).
func (b *Bot) apiCapLabel() string {
	if n := b.apikeyMaxPerCustomer(); n > 0 {
		return "👤 API keys/customer: " + itoa64(n)
	}
	return "👤 API keys/customer: ∞"
}

// payMethodLabel renders a payment toggle plus the "won't show until configured"
// hint — the bnb/bsc/trx label assembly at @11941-11951. The toggle itself works
// regardless; the hint explains why an enabled method may still be invisible to
// customers.
func (b *Bot) payMethodLabel(method string, configured func() bool, missing string) string {
	label := onOffState(b.payEnabled(method))
	if !configured() {
		label += " — " + missing
	}
	return label
}

// pendingRefundCount is refund_store.count_pending() with a nil-store guard —
// the count shown on the Orders category button (@11851).
func (b *Bot) pendingRefundCount() int64 {
	if b.refundStore == nil {
		return 0
	}
	n, err := b.refundStore.CountPending()
	if err != nil {
		return 0
	}
	return n
}

// rateStr is current_rate_str (telegram_bot.py:5291) — the effective USD→VND rate.
func (b *Bot) rateStr() string {
	if b.cfg == nil {
		return "(not set)"
	}
	return b.cfg.USDToVND()
}

// kirogoURLStr / kirogoAccountStr / kirogoKeySet expose the Kiro-Go config for
// the Settings labels. The KEY is reported as a boolean only, never its value.
func (b *Bot) kirogoURLStr() string {
	if b.cfg == nil {
		return ""
	}
	return b.cfg.KiroGoBaseURL()
}

func (b *Bot) kirogoAccountStr() string {
	if b.cfg == nil {
		return ""
	}
	return b.cfg.KiroGoAccountID()
}

func (b *Bot) kirogoKeySet() bool {
	return b.cfg != nil && b.cfg.KiroGoAdminKey() != ""
}

func (b *Bot) bep20AddrStr() string {
	if b.cfg == nil {
		return ""
	}
	return b.cfg.BEP20Address()
}

func (b *Bot) tronAddrStr() string {
	if b.cfg == nil {
		return ""
	}
	return b.cfg.TRONAddress()
}

// isAllDigits reports whether s is a bare number — the `removed.isdigit()` test
// deciding whether a whitelist entry renders as an @handle or a chat id.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- callback routes ---------------------------------------------------------

// cbAdmRoot re-renders the root panel from any submenu — the data=="admroot"
// branch (telegram_bot.py:12697-12703). Back also ABANDONS a half-typed prompt.
func (b *Bot) cbAdmRoot(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.ClearAdminPending()
	b.showAdminPanel(cc.Ctx, cc.ChatID)
}

// cbAdmCat opens a category submenu — the admcat: branch (telegram_bot.py:
// 12865-12879). roles/stdperms are super-admin only; an unknown category (stale
// button) is ignored, exactly as Python's `if not rows: return`.
func (b *Bot) cbAdmCat(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.ClearAdminPending()
	cat := strings.TrimPrefix(cc.Data, "admcat:")
	// roles/stdperms/resellers are super-admin-only CATEGORIES — none of their
	// actions appear in any capability table (py_customers_spec.md §0/§7), so
	// even a standard admin holding every other grant is blocked from opening
	// the submenu at all, matching Python's chokepoint denial before
	// handle_callback's admcat: branch even runs (@12411-12415, @12871).
	if (cat == "roles" || cat == "stdperms" || cat == "resellers") && !b.isSuperAdmin(cc.ChatID) {
		return
	}
	title, rows, ok := b.adminCategoryMenu(cc.ChatID, cat)
	if !ok || len(rows) == 0 {
		return
	}
	b.sendMenu(cc.Ctx, cc.ChatID, "🔑 "+title, rows)
}

// cbAdmAction runs an adm:<action> button. The three read-only views whose Go
// backing exists are wired; everything else answers with the not-ported notice
// so the tap is never silent. Later slices replace cases here one at a time.
func (b *Bot) cbAdmAction(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	action := strings.TrimPrefix(cc.Data, "adm:")
	// adm:togglepay:<method> carries a 3rd colon-segment (admin.go:96's own
	// note); pull it out before the exact-match switch below, which cannot
	// express a prefix case. Case bodies for every "settings" branch below
	// live in adminsettings.go (⚙️ SETTINGS + 💳 PAYMENT METHODS submenu,
	// py_settings_spec.md).
	if method, ok := strings.CutPrefix(action, "togglepay:"); ok {
		b.cbTogglePay(cc, method)
		return
	}
	switch action {
	case "stock":
		b.send(cc.Ctx, cc.ChatID, b.adminStockText(), admNav("admcat:stock"))
	// 📦 STOCK category actions (adminstock.go, py_stock_spec.md). upstock/
	// upstockstore/mngprod/stockbroadcast/provpool are full-admin-only
	// (admincap.go's default-deny — absent from stdAdminCbCap's table);
	// holdstock is hold_restore-capability-gated (a standard admin CAN reach
	// it with the default capability set).
	case "holdstock":
		b.cbAdmHoldStock(cc)
	case "upstock":
		b.cbAdmUpstock(cc)
	case "upstockstore":
		b.cbAdmUpstockStore(cc)
	case "mngprod":
		b.cbAdmMngProd(cc)
	case "stockbroadcast":
		b.cbAdmStockBroadcast(cc)
	case "provpool":
		b.cbAdmProvPool(cc)
	// 🏊 Pool health / 📜 Audit log (adminstock.go). Both live on the ADMIN
	// ROOT panel, not inside admcat:stock (py_stock_spec.md §0).
	case "pool":
		b.cbAdmPool(cc)
	case "audit":
		b.cbAdmAudit(cc)
	case "orders":
		b.sendRecentOrders(cc.Ctx, cc.ChatID, 20)
	case "revenue":
		// Full by-product/by-date matrix view (adminorders.go), default 7-day
		// by-product — adm:revenue (py_orders_spec.md).
		b.cbRevenue(cc)
	case "getord":
		// 🔎 Look up order by ID (adminorders.go, py_orders_spec.md).
		b.cbGetOrd(cc)
	case "refund":
		// 💸 Refund order — admin-initiated refund entry point
		// (adminorders.go, py_orders_spec.md).
		b.cbRefund(cc)
	case "broadcast":
		b.cbBroadcastStart(cc)
	case "togglebcast":
		b.cbToggleBcast(cc)
	case "togglestockbcast":
		b.cbToggleStockBcast(cc)
	case "togglesalenotify":
		b.cbToggleSaleNotify(cc)
	case "toggledepnotify":
		b.cbToggleDepNotify(cc)
	case "togglemaint":
		b.cbToggleMaint(cc)
	case "togglesales":
		b.cbToggleSales(cc)
	case "toggleprivate":
		b.cbTogglePrivate(cc)
	case "togglereferral":
		b.cbToggleReferral(cc)
	case "setmaxqty":
		b.cbSetMaxQty(cc)
	case "setrate":
		b.cbSetRate(cc)
	case "setkirourl":
		b.cbSetKiroURL(cc)
	case "setkirokey":
		b.cbSetKiroKey(cc)
	case "setkiroacct":
		b.cbSetKiroAcct(cc)
	case "setbep20":
		b.cbSetBep20(cc)
	case "settron":
		b.cbSetTron(cc)
	case "setprice":
		b.cbSetPrice(cc)
	case "setdiscount":
		b.cbSetDiscount(cc)
	case "promocodes":
		b.cbPromoCodes(cc)
	case "promonew":
		b.cbPromoNew(cc)
	case "apiprice":
		b.cbApiPrice(cc)
	case "apimin":
		b.cbApiMin(cc)
	case "apimax":
		b.cbApiMax(cc)
	case "apicap":
		b.cbApiCap(cc)
	case "kirotoggle":
		b.cbKiroToggle(cc)
	case "kiroprice":
		b.cbKiroPrice(cc)
	case "kiromin":
		b.cbKiroMin(cc)
	case "kiromax":
		b.cbKiroMax(cc)
	// 👥 CUSTOMERS (adminusers.go). custlookup is customers_view-capability
	// (grantable, hence no fullAdminOnly gate here); addbal is full-admin-only.
	case "custlookup":
		b.cbCustLookup(cc)
	case "addbal":
		b.cbAddBal(cc)
	// 🛡️ ADMIN ROLES (adminusers.go). All three are full-admin-only.
	case "listadmins":
		b.cbListAdmins(cc)
	case "addadmin":
		b.cbAddAdmin(cc)
	case "rmadmin":
		b.cbRmAdmin(cc)
	// Private-sale whitelist arm (adminusers.go). toggle_sales-capability
	// (grantable); rmwl: itself is a standalone top-level route above.
	case "addwl":
		b.cbAddWL(cc)
	// Authorized resellers arm (adminusers.go). Full-admin-only.
	case "addreseller":
		b.cbAddReseller(cc)
	default:
		b.send(cc.Ctx, cc.ChatID, adminNotPorted, admNav("admroot"))
	}
}

// cbAdmTodo answers a panel button whose flow belongs to a later slice. It
// exists so every button the panel renders produces a reply — the failure this
// file fixes was precisely a tap that produced nothing.
func (b *Bot) cbAdmTodo(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	b.send(cc.Ctx, cc.ChatID, adminNotPorted, admNav("admroot"))
}
