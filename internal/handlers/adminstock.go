package handlers

// adminstock.go — the 📦 STOCK admin category, plus the 🏊 Pool health and
// 📜 Audit log root-panel views (py_stock_spec.md, the authoritative spec for
// every string/callback/state-machine transition below).
//
// Why this file exists: admin.go's adminCategoryMenu already renders the
// "stock" submenu's seven rows (adm:stock, adm:upstock, adm:upstockstore,
// adm:holdstock, adm:mngprod, adm:stockbroadcast, adm:provpool) plus the root
// panel's 🏊/📜 buttons, but every one of them except adm:stock (already
// wired to adminStockText) fell into cbAdmAction's default case and answered
// with adminNotPorted. This file replaces that stub with the real flows.
//
// Ports (py_stock_spec.md section numbers):
//   §1  adm:upstock            -> cbAdmUpstock + the upstockprod:/upstocktype:
//                                  callback chain, the paste/file captures, the
//                                  throttled render loop, upstockok/upstockno.
//   §2  adm:upstockstore       -> cbAdmUpstockStore + upstockstoreprod:/
//                                  upstockstoresub:.
//   §3  adm:holdstock          -> cbAdmHoldStock + holdprod:/holdact: + capHold.
//   §4  adm:mngprod            -> cbAdmMngProd + the whole prod*: family.
//   §5  adm:stockbroadcast     -> cbAdmStockBroadcast.
//   §6  adm:provpool           -> cbAdmProvPool + its file/paste captures +
//                                  the Kiro-pool provisioning run.
//   §7  adm:pool / admpoolr    -> cbAdmPool / cbAdmPoolRefresh.
//   §8  adm:audit              -> cbAdmAudit + the /audit slash command
//                                  (admin.go's handleAdmin).
//   §9  broadcast_stock_update -> broadcastStockUpdate (the one customer-
//                                  facing, trilingual string set in this
//                                  category).
//
// Reused, not reimplemented (owned by internal/stock, built this session):
// ParseUpstockText/ParseOrderFileText/ParseKiroAPIText/ParseJSONAccounts, the
// UpstockBatch accumulator + UpstockPreview.RenderText(), and every stock.Store
// data method (AddAccounts, AddJSONAccounts, ListStoreSubs, UpstockFromStore,
// CreateProduct, RenameProduct, RemoveProduct, ReadTextFile/WriteTextFile,
// GetAccountType/SetAccountType, Hold/Restore, SetProductNotice/Delivery). This
// file owns exactly what the task brief calls "the Telegram half": the
// throttled/coalesced render loop, the confirm/cancel buttons, file-download +
// zip handling, the product/subscription pickers, and the pool/audit views.
//
// Deliberate divergences from the Python original (each also called out at its
// own call site, and in the final task report):
//   - doTokenUpstock commits parsed JSON accounts directly via AddJSONAccounts,
//     WITHOUT Python's per-account AWS SSO OIDC token-refresh + quota-check
//     pass (refresh_json_account/check_account_quota) — that machinery has no
//     Go port anywhere in internal/stock, and writing a brand-new AWS SSO
//     refresh client is out of this slice's scope (porting the Telegram half
//     of an ALREADY-BUILT lower layer). Fabricating a fake "live/died" pass
//     would be worse than an honest, documented gap.
//   - adm:provpool's PASTE path is NEW: Python wires pending_provpool a FILE
//     handler only and never a matching text handler despite its own prompt
//     inviting "either PASTE it or UPLOAD a .txt" (§6.2, a confirmed latent
//     Python bug) — this port fixes it rather than replicating the dead end.
//   - The STOCK_BROADCAST trilingual strings live in a small local table
//     (stockBroadcastText) rather than a new internal/i18n/catalog_*.go entry:
//     this task's file ownership is scoped to internal/handlers, and
//     internal/i18n is concurrently touched elsewhere in this tree.
//   - Per-product suspend/private WRITE helpers (setProductSuspended,
//     setProductPublic/setProductPrivateInit/setProductPrivateList) are new —
//     only READ-side helpers (catalog.go/adminorders.go) existed before this
//     slice, since no earlier admin flow ever needed to flip these settings.
//   - _no_autodelete() has no Go equivalent — no auto-delete sweep exists in
//     this port at all, so there is nothing for the broadcast to opt out of.
import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/provision"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/tg"
)

// noProducts is the admin-surface reuse of the CUSTOMER NO_PRODUCTS string,
// keyed by the ADMIN's own lang_for preference (py_stock_spec.md §0's one
// documented exception to "fixed English everywhere" in this category).
func (b *Bot) noProducts(chatID int64) string {
	return i18n.C().Buy.NoProducts.T(b.langFor(chatID, "en"))
}

// errNoConfig is returned by the settings-backed setters below when no
// *config.Config is wired (unit tests, or a partially configured install) —
// callers treat it exactly like any other save failure.
var errNoConfig = errors.New("adminstock: no config wired")

// registerAdminStock wires every callback prefix, text capture, and file
// intercept this category needs into the ordered dispatch tables. Runs LAST
// of every admin slice (dispatch.go's New): its captures splice ahead of
// "getord" (registerAdminOrders' own earliest anchor — "upstock" is
// canonical #1), "refund" ("newprod"/"renprod"/"provpool" spliced ahead of
// it, then "setnotice"/"setdelivery"/"editfile" spliced ahead of it again —
// both groups anchor on registerAdminOrders' "refund" capture, landing in
// registration order since each insert-before-the-same-anchor call lands
// closer to it than the previous one), and "tier" ("hold", spliced ahead
// of registerAdminPricing's own "tier" anchor) — every one of those anchors
// must already exist in the chain by the time this runs.
func (b *Bot) registerAdminStock() {
	b.insertCapturesBefore("getord",
		stage{name: "upstock", fn: (*Bot).capUpstock},
	)
	b.insertCapturesBefore("refund",
		stage{name: "newprod", fn: (*Bot).capNewProd},
		stage{name: "renprod", fn: (*Bot).capRenProd},
		stage{name: "provpool", fn: (*Bot).capProvPoolText},
	)
	b.insertCapturesBefore("refund",
		stage{name: "setnotice", fn: (*Bot).capSetNotice},
		stage{name: "setdelivery", fn: (*Bot).capSetDelivery},
		stage{name: "editfile", fn: (*Bot).capEditFile},
	)
	b.insertCapturesBefore("tier",
		stage{name: "hold", fn: (*Bot).capHold},
	)

	// Step 5 file intercepts — document uploads carry no text and never reach
	// the text-capture chain above (dispatch.go's step 5 runs before the
	// step-6 no-text gate).
	b.fileIntercepts = append(b.fileIntercepts,
		stage{name: "upstockfile", fn: (*Bot).capUpstockFile},
		stage{name: "provpoolfile", fn: (*Bot).capProvPoolFile},
	)

	// Every prefix below is disjoint from every other cbRoute already
	// registered (go_structure.md §2's "ordering trap" note: none is a
	// prefix of, or prefixed by, an existing key), so append order among
	// them doesn't matter.
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "upstockprod:", fn: (*Bot).cbUpstockProd},
		cbRoute{key: "upstocktype:", fn: (*Bot).cbUpstockType},
		cbRoute{key: "upstockok", exact: true, fn: (*Bot).cbUpstockOK},
		cbRoute{key: "upstockno", exact: true, fn: (*Bot).cbUpstockNo},
		cbRoute{key: "upstockstoreprod:", fn: (*Bot).cbUpstockStoreProd},
		cbRoute{key: "upstockstoresub:", fn: (*Bot).cbUpstockStoreSub},
		cbRoute{key: "holdprod:", fn: (*Bot).cbHoldProd},
		cbRoute{key: "holdact:", fn: (*Bot).cbHoldAct},
		cbRoute{key: "prodnew", exact: true, fn: (*Bot).cbProdNew},
		cbRoute{key: "prodmng:", fn: (*Bot).cbProdMng},
		cbRoute{key: "prodsusp:", fn: (*Bot).cbProdSusp},
		cbRoute{key: "prodvis:", fn: (*Bot).cbProdVis},
		cbRoute{key: "prodwlt:", fn: (*Bot).cbProdWlt},
		cbRoute{key: "prodnotice:", fn: (*Bot).cbProdNotice},
		cbRoute{key: "proddelivery:", fn: (*Bot).cbProdDelivery},
		cbRoute{key: "prodren:", fn: (*Bot).cbProdRen},
		cbRoute{key: "prodcfg:", fn: (*Bot).cbProdCfg},
		cbRoute{key: "prodenv:", fn: (*Bot).cbProdEnv},
		cbRoute{key: "prodrm:", fn: (*Bot).cbProdRm},
		cbRoute{key: "prodrmok", exact: true, fn: (*Bot).cbProdRmOK},
	)
}

// ============================================================================
// §3 adm:holdstock — Hold / restore stock (hold_restore capability, one of
// the two default-granted stock caps — a standard admin with the shipped
// default set CAN use this).
// ============================================================================

// cbAdmHoldStock is the adm:holdstock case in admin.go's cbAdmAction, which
// has already confirmed isAdmin — no further gate needed here (the
// hold_restore capability itself is enforced upstream by the admincap.go
// chokepoint, before this handler ever runs).
func (b *Bot) cbAdmHoldStock(cc *cbCtx) {
	products := b.listProducts()
	if len(products) == 0 {
		b.send(cc.Ctx, cc.ChatID, b.noProducts(cc.ChatID), admNav("admcat:stock"))
		return
	}
	rows := make([][]tg.InlineKeyboardButton, 0, len(products)+1)
	for i, p := range products {
		rows = append(rows, btnRow(p, cbData("holdprod", itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:stock"))
	b.sendMenu(cc.Ctx, cc.ChatID, "🔒 Hold/restore stock — which product?", rows)
}

// cbHoldProd is a standalone top-level route (not nested under "adm:"), so it
// re-checks isAdmin itself — the admincap.go chokepoint already enforces the
// hold_restore capability for a standard admin before dispatchCB runs, but a
// plain non-admin is only ever stopped by the handler's own check.
func (b *Bot) cbHoldProd(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	cc.st.ClearAdminPending()
	idxStr := strings.TrimPrefix(cc.Data, "holdprod:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product not found.", admNav("adm:holdstock"))
		return
	}
	avail, held := 0, 0
	if b.stock != nil {
		avail, _ = b.stock.CountAvailable(product)
		held = b.stock.CountHeld(product)
	}
	rows := [][]tg.InlineKeyboardButton{
		btnRow("🔒 Hold (available: "+itoa(avail)+")", cbData("holdact", idxStr, "hold")),
		btnRow("🔓 Restore (held: "+itoa(held)+")", cbData("holdact", idxStr, "restore")),
		btnRow("↩️ Back", "adm:holdstock"),
	}
	b.sendMenu(cc.Ctx, cc.ChatID, "🔒 "+product+"\nAvailable: "+itoa(avail)+" · Held: "+itoa(held), rows)
}

// cbHoldAct arms pending_hold (HoldArm) and prompts for the quantity.
func (b *Bot) cbHoldAct(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "holdact:")
	idxStr, act, cut := strings.Cut(rest, ":")
	if !cut || (act != "hold" && act != "restore") {
		return
	}
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product not found.", admNav("adm:holdstock"))
		return
	}
	cc.st.mu.Lock()
	cc.st.Hold = &HoldArm{Product: product, Action: act}
	cc.st.mu.Unlock()
	var prompt string
	if act == "hold" {
		avail := 0
		if b.stock != nil {
			avail, _ = b.stock.CountAvailable(product)
		}
		prompt = "🔒 " + product + "\nAvailable: " + itoa(avail) + "\nHow many to HOLD (remove from sale)?"
	} else {
		held := 0
		if b.stock != nil {
			held = b.stock.CountHeld(product)
		}
		prompt = "🔓 " + product + "\nHeld: " + itoa(held) + "\nHow many to RESTORE (return to sale)?"
	}
	b.send(cc.Ctx, cc.ChatID, prompt, backKB(cbData("holdprod", idxStr)))
}

// capHold is the ONE capture in this whole category that RE-ARMS on bad
// input instead of aborting (py_stock_spec.md §3.4/§11): a non-numeric or
// non-positive quantity leaves c.st.Hold untouched so the admin can just
// retype; only a SUCCESSFUL parse pops it.
func (b *Bot) capHold(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Hold
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isAdmin(c.ChatID) {
		return false
	}
	raw := strings.NewReplacer(",", "", ".", "").Replace(strings.TrimSpace(c.Text))
	n, convErr := strconv.Atoi(raw)
	if !isDigits(raw) || convErr != nil || n <= 0 {
		// RE-ARM: c.st.Hold stays set. The error reply's nav goes to
		// admcat:stock (not back into the hold prompt) — matching Python's
		// literal behaviour: the admin must re-tap Hold/Restore, though the
		// underlying pending state is technically still live until they do.
		b.send(c.Ctx, c.ChatID, "⚠️ Enter a positive number, e.g. 50.", admNav("admcat:stock"))
		return true
	}
	// Only pop on SUCCESS.
	c.st.mu.Lock()
	c.st.Hold = nil
	c.st.mu.Unlock()
	if b.stock == nil {
		b.send(c.Ctx, c.ChatID, "⚠️ Stock not configured.", admNav("admcat:stock"))
		return true
	}
	var moved int
	var err error
	if armed.Action == "hold" {
		moved, err = b.stock.Hold(armed.Product, n)
	} else {
		moved, err = b.stock.Restore(armed.Product, n)
	}
	if err != nil {
		log.Println("adminstock: hold/restore failed:", err)
		b.send(c.Ctx, c.ChatID, "⚠️ Failed. Check server logs.", admNavRetry("admcat:stock", "🔒 Hold / restore", "adm:holdstock"))
		return true
	}
	avail, _ := b.stock.CountAvailable(armed.Product)
	held := b.stock.CountHeld(armed.Product)
	verb, prep := "🔒 Held", "from"
	if armed.Action == "restore" {
		verb, prep = "🔓 Restored", "to"
	}
	text := verb + " " + itoa(moved) + " " + prep + " " + armed.Product + ".\n" +
		"Now available: " + itoa(avail) + " · held: " + itoa(held)
	b.send(c.Ctx, c.ChatID, text, admNavRetry("admcat:stock", "🔒 Hold / restore", "adm:holdstock"))
	return true
}

// ============================================================================
// §1 adm:upstock — Upstock from text/file. Full-admin-only.
// ============================================================================

// cbAdmUpstock is adm:upstock's entry point.
func (b *Bot) cbAdmUpstock(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	products := b.listProducts()
	if len(products) == 0 {
		b.send(cc.Ctx, cc.ChatID, b.noProducts(cc.ChatID), admNav("admcat:stock"))
		return
	}
	rows := make([][]tg.InlineKeyboardButton, 0, len(products)+1)
	for i, p := range products {
		rows = append(rows, btnRow(p, cbData("upstockprod", itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:stock"))
	b.sendMenu(cc.Ctx, cc.ChatID, "📥 Upstock — which product?", rows)
}

// cbUpstockProd: product chosen -> account-type picker (§1.2).
func (b *Bot) cbUpstockProd(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	cc.st.ClearAdminPending()
	idxStr := strings.TrimPrefix(cc.Data, "upstockprod:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product not found.", admNav("adm:upstock"))
		return
	}
	curType := "password"
	if b.stock != nil {
		curType = b.stock.GetAccountType(product)
	}
	rows := [][]tg.InlineKeyboardButton{
		btnRow("🔑 Password accounts (username|password)", cbData("upstocktype", idxStr, "password")),
		btnRow("🔄 JSON accounts (one JSON object each)", cbData("upstocktype", idxStr, "token")),
		btnRow("♻️ Resell from codezdev (order .txt)", cbData("upstocktype", idxStr, "resell")),
		btnRow("🔑 Kiro API Keys (ksk_xxxxx)", cbData("upstocktype", idxStr, "kiroapi")),
		btnRow("↩️ Back", "adm:upstock"),
	}
	text := "📥 Upstock — " + product + "\nCurrent type: " + curType + "\n\nChoose account type:"
	b.sendMenu(cc.Ctx, cc.ChatID, text, rows)
}

// cbUpstockType: type chosen -> arm the wizard + prompt (§1.3). Does NOT
// persist the type yet — only stamped at confirm time (doUpstock/
// doTokenUpstock), so an abandoned flow never mutates a live product's
// stamped format.
func (b *Bot) cbUpstockType(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "upstocktype:")
	pidxStr, actypeRaw, _ := strings.Cut(rest, ":")
	product, ok := b.productAt(pidxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product not found.", admNav("adm:upstock"))
		return
	}
	kind := stock.UpstockPassword
	switch actypeRaw {
	case "token":
		kind = stock.UpstockToken
	case "resell":
		kind = stock.UpstockResell
	case "kiroapi":
		kind = stock.UpstockKiroAPI
	}
	cc.st.mu.Lock()
	cc.st.Upstock = &UpstockArm{Product: product, Kind: kind}
	cc.st.mu.Unlock()

	var prompt string
	switch kind {
	case stock.UpstockToken:
		prompt = "📥 JSON upstock — " + product + "\n\n" +
			"Paste the account JSON, or upload .json/.txt file(s) — or a\n" +
			".zip bundling many .json files (all imported at once).\n" +
			"One JSON object = one account. You may paste/upload a JSON\n" +
			"array, or several objects — each must include an \"email\" field."
	case stock.UpstockResell:
		prompt = "♻️ Resell upstock — " + product + "\n\n" +
			"Upload the order file (.txt) you received from the codezdev bot, or paste its contents. The username/password accounts in it are added to this product's stock.\n" +
			"(JSON/token products: use the 🔄 JSON option with the order .zip.)"
	case stock.UpstockKiroAPI:
		prompt = "🔑 Kiro API Keys — " + product + "\n\n" +
			"Paste the API key list, or upload the .txt file.\n" +
			"Format: lines of  ksk_xxxxx  (ksk_ prefix required)."
	default:
		prompt = "📥 Upstock — " + product + "\n\n" +
			"Paste the account text, or upload the .txt file.\n" +
			"Format: lines of  username|password  (headers ignored)."
	}
	b.send(cc.Ctx, cc.ChatID, prompt, backKB(cbData("upstockprod", pidxStr)))
}

// capUpstock is the paste-input path (§1.5) — PEEK: several messages may
// accumulate into one batch, so this never pops c.st.Upstock itself (only
// clearUpstockBatch does, on abort/confirm/cancel).
func (b *Bot) capUpstock(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Upstock
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") {
		return false
	}
	if !b.isAdmin(c.ChatID) {
		return false
	}
	if labelToCmd(c.Text) != "" {
		// A recognized menu-button label ABORTS the whole batch (§1.5) — falls
		// through so the tapped button's own handler still runs.
		b.clearUpstockBatch(c.ChatID)
		return false
	}
	b.submitUpstockBlob(c.Ctx, c.ChatID, armed.Product, armed.Kind, c.Text)
	return true
}

// capUpstockFile is the step-5 file intercept (§1.4) — PEEKS pending_upstock
// (does not pop; several files can accumulate).
func (b *Bot) capUpstockFile(c *Ctx) bool {
	if c.Msg == nil || c.Msg.Document == nil {
		return false
	}
	st := b.states.Get(c.ChatID)
	st.mu.Lock()
	armed := st.Upstock
	st.mu.Unlock()
	if armed == nil {
		return false
	}
	if !b.isAdmin(c.ChatID) {
		return false
	}
	data, err := b.tg.GetFileBytes(c.Ctx, c.Msg.Document.FileID)
	if err != nil {
		log.Println("adminstock: upstock GetFileBytes failed:", err)
		b.send(c.Ctx, c.ChatID, "⚠️ Could not download the file. Try again.", admNav("adm:upstock"))
		return true
	}
	if isZipFile(data) {
		blob, warn := extractUpstockZip(data, armed.Kind)
		if warn != "" {
			b.send(c.Ctx, c.ChatID, warn, nil)
			return true
		}
		b.submitUpstockBlob(c.Ctx, c.ChatID, armed.Product, armed.Kind, blob)
		return true
	}
	text := strings.ToValidUTF8(string(data), "�")
	b.submitUpstockBlob(c.Ctx, c.ChatID, armed.Product, armed.Kind, text)
	return true
}

// isZipFile reports whether data parses as a zip archive — the Go analogue
// of Python's zipfile.is_zipfile().
func isZipFile(data []byte) bool {
	_, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	return err == nil
}

// extractUpstockZip pulls matching member texts out of a zip and joins them
// with "\n" into ONE blob (so a whole zip imports as one batch) — Python's
// _extract_upstock_zip (§1.4). .json members only when kind==token, else
// .json+.txt; directories, __MACOSX/, and dotfiles/resource forks are
// skipped. An empty match returns a warning instead of silently
// accumulating nothing.
func extractUpstockZip(data []byte, kind stock.UpstockKind) (blob, warn string) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", "❌ Could not read the .zip file."
	}
	var parts []string
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		base := name
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, ".") {
			continue
		}
		low := strings.ToLower(base)
		isJSON := strings.HasSuffix(low, ".json")
		isTxt := strings.HasSuffix(low, ".txt")
		if kind == stock.UpstockToken {
			if !isJSON {
				continue
			}
		} else if !isJSON && !isTxt {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		parts = append(parts, strings.ToValidUTF8(string(content), "�"))
	}
	if len(parts) == 0 {
		kindWord := ".json/.txt"
		if kind == stock.UpstockToken {
			kindWord = ".json"
		}
		return "", "❌ No " + kindWord + " account files found in the .zip."
	}
	return strings.Join(parts, "\n"), ""
}

// clearUpstockBatch drops all 4 batch maps (pending_upstock, _acc, _confirm,
// preview_msg) — Python's _clear_upstock (§1.5/§1.8). Deliberately does NOT
// touch UpstockRendering: a live render-loop goroutine notices its accumulator
// pointer no longer matches st.UpstockAcc on its own next tick (pointer-
// identity check, runUpstockRenderLoop) and exits without touching that flag,
// so a stray reset here can't race a fresh batch's own renderer into thinking
// it isn't already claimed.
func (b *Bot) clearUpstockBatch(chatID int64) {
	st := b.states.Get(chatID)
	st.mu.Lock()
	st.Upstock = nil
	st.UpstockAcc = nil
	st.UpstockConfirm = nil
	st.UpstockPreviewMsg = 0
	st.mu.Unlock()
}

// submitUpstockBlob is the parsing half of Python's accumulate_upstock
// (§1.7): appends blob into the running accumulator (creating one if this is
// the first paste/upload for this product+kind), marks it dirty, and starts
// the render loop UNLESS one is already running for this chat.
func (b *Bot) submitUpstockBlob(ctx context.Context, chatID int64, product string, kind stock.UpstockKind, blob string) {
	st := b.states.Get(chatID)
	st.mu.Lock()
	if st.UpstockAcc == nil || st.UpstockAcc.Product != product || st.UpstockAcc.Kind != kind {
		st.UpstockAcc = stock.NewUpstockBatch(product, kind)
	}
	acc := st.UpstockAcc
	acc.Add(blob)
	st.UpstockDirty = true
	alreadyRendering := st.UpstockRendering
	st.UpstockRendering = true
	st.mu.Unlock()

	if alreadyRendering {
		return // that renderer will pick up the new items on its next pass
	}
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			b.runUpstockRenderLoop(fctx, chatID, acc)
		})
	})
}

// runUpstockRenderLoop is _render_upstock_loop (§1.7): the FIRST iteration
// runs immediately (coalescing whatever accumulated before this goroutine
// even started); every later iteration sleeps upstockRenderThrottle first.
// myAcc pins the SPECIFIC accumulator instance this goroutine serves —
// comparing st.UpstockAcc against it (pointer identity) on every pass is how
// a superseded batch (cleared by confirm/cancel/abort, or replaced by a
// fresh upstock arm while this goroutine slept) is detected safely: on a
// mismatch this loop just stops, WITHOUT touching UpstockRendering, since
// ownership of that flag belongs to whichever flow is current now.
func (b *Bot) runUpstockRenderLoop(ctx context.Context, chatID int64, myAcc *stock.UpstockBatch) {
	st := b.states.Get(chatID)
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(b.upstockRenderThrottle):
			}
		}
		first = false

		st.mu.Lock()
		if st.UpstockAcc != myAcc {
			st.mu.Unlock()
			return
		}
		if !st.UpstockDirty {
			st.UpstockRendering = false
			st.mu.Unlock()
			return
		}
		st.UpstockDirty = false
		preview := myAcc.Preview(b.stock)
		st.UpstockConfirm = &preview
		prevMsgID := st.UpstockPreviewMsg
		st.mu.Unlock()

		// ---- network calls OUTSIDE the lock (§1.7) ----
		text := preview.RenderText()
		var markup any
		if preview.New > 0 {
			markup = &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
				btnRow("✅ Confirm upstock "+itoa(preview.New), "upstockok"),
				btnRow("❌ Cancel", "upstockno"),
			}}
		}
		msg, err := b.tg.SendMessage(ctx, tg.SendMessageParams{ChatID: chatID, Text: text, ReplyMarkup: markup})
		if err != nil || msg == nil {
			log.Println("adminstock: upstock preview send failed:", err)
			continue // old preview (still valid buttons) stays intact — send-before-delete
		}
		st.mu.Lock()
		if st.UpstockAcc == myAcc {
			st.UpstockPreviewMsg = msg.MessageID
		}
		st.mu.Unlock()
		if prevMsgID != 0 {
			_ = b.tg.DeleteMessage(ctx, chatID, prevMsgID) // best-effort, AFTER the new send succeeded
		}
	}
}

// cbUpstockOK is upstockok (§1.8): pops UpstockConfirm, then UNCONDITIONALLY
// clears the whole batch ("batch done" regardless of outcome).
func (b *Bot) cbUpstockOK(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	cc.st.mu.Lock()
	preview := cc.st.UpstockConfirm
	cc.st.mu.Unlock()
	b.clearUpstockBatch(cc.ChatID)
	if preview == nil || preview.New == 0 {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Nothing to confirm (preview expired). Start again.", admNav("adm:upstock"))
		return
	}
	if preview.Kind == stock.UpstockToken {
		product, objs := preview.Product, preview.NewObjs
		b.workers.Go(func(stop <-chan struct{}) {
			runOnStoppableCtx(stop, func(fctx context.Context) {
				b.doTokenUpstock(fctx, cc.ChatID, product, objs)
			})
		})
		return
	}
	b.doUpstock(cc.Ctx, cc.ChatID, preview.Product, preview.NewPairs, string(preview.Kind))
}

// cbUpstockNo is upstockno (§1.8).
func (b *Bot) cbUpstockNo(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	b.clearUpstockBatch(cc.ChatID)
	b.send(cc.Ctx, cc.ChatID, "❌ Upstock cancelled.", admNavRetry("adm:upstock", "📥 Upstock more", "adm:upstock"))
}

// doUpstock commits a password/resell/kiroapi confirm — Python do_upstock
// (§1.9). Runs synchronously (file I/O only, no network) on the callback's
// own context; only its broadcast side-effect is backgrounded.
func (b *Bot) doUpstock(ctx context.Context, chatID int64, product string, pairs []stock.CredPair, actype string) {
	if b.stock == nil {
		b.send(ctx, chatID, "⚠️ Stock not configured.", admNav("admcat:stock"))
		return
	}
	// Type file written only NOW (confirm time) — §1.3/§1.9.
	if err := b.stock.SetAccountType(product, actype); err != nil {
		log.Println("adminstock: SetAccountType failed:", err)
	}
	res, err := b.stock.AddAccounts(product, pairs)
	if err != nil {
		log.Println("adminstock: AddAccounts failed:", err)
		b.send(ctx, chatID, "⚠️ Upstock failed. Check server logs.", admNav("admcat:stock"))
		return
	}
	avail, _ := b.stock.CountAvailable(product)
	text := "📦 Upstock — " + product + "\n" +
		"✅ Added: " + itoa(res.Added) + "\n" +
		"↩️ Skipped (already in stock/sold): " + itoa(res.Skipped) + "\n" +
		"📊 Now available: " + itoa(avail)
	b.send(ctx, chatID, text, admNavRetry("admcat:stock", "📥 Upstock more", "adm:upstock"))
	if res.Added > 0 && b.autoStockBroadcastEnabled() {
		b.workers.Go(func(stop <-chan struct{}) {
			runOnStoppableCtx(stop, func(fctx context.Context) {
				b.broadcastStockUpdate(fctx, product)
			})
		})
	}
}

// doTokenUpstock commits a JSON/token confirm — Python do_token_upstock
// (§1.9), MINUS the per-account AWS SSO OIDC token-refresh + quota-check
// network pass (see file header's divergence note). Runs on its OWN
// background goroutine (spawned by cbUpstockOK), so its broadcast
// side-effect runs directly on ctx rather than nesting another worker.
func (b *Bot) doTokenUpstock(ctx context.Context, chatID int64, product string, objs []stock.JSONAccount) {
	if b.stock == nil {
		b.send(ctx, chatID, "⚠️ Stock not configured.", admNav("admcat:stock"))
		return
	}
	if err := b.stock.SetAccountType(product, "token"); err != nil {
		log.Println("adminstock: SetAccountType failed:", err)
	}
	res, err := b.stock.AddJSONAccounts(product, objs)
	if err != nil {
		log.Println("adminstock: AddJSONAccounts failed:", err)
		b.send(ctx, chatID, "⚠️ Upstock failed. Check server logs.", admNav("admcat:stock"))
		return
	}
	avail, _ := b.stock.CountAvailable(product)
	text := "📦 JSON upstock — " + product + "\n" +
		"✅ Added: " + itoa(res.Added) + "\n" +
		"↩️ Skipped (already in stock/sold): " + itoa(res.Skipped) + "\n"
	if res.Bad > 0 {
		text += "⚠️ Rejected (no email): " + itoa(res.Bad) + "\n"
	}
	text += "📊 Now available: " + itoa(avail)
	b.send(ctx, chatID, text, admNavRetry("admcat:stock", "📥 Upstock more", "adm:upstock"))
	if res.Added > 0 && b.autoStockBroadcastEnabled() {
		b.broadcastStockUpdate(ctx, product)
	}
}

// ============================================================================
// §2 adm:upstockstore — Upstock from store. Full-admin-only.
// ============================================================================

func (b *Bot) cbAdmUpstockStore(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	products := b.listProducts()
	if len(products) == 0 {
		b.send(cc.Ctx, cc.ChatID, b.noProducts(cc.ChatID), admNav("admcat:stock"))
		return
	}
	rows := make([][]tg.InlineKeyboardButton, 0, len(products)+1)
	for i, p := range products {
		rows = append(rows, btnRow(p, cbData("upstockstoreprod", itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:stock"))
	b.sendMenu(cc.Ctx, cc.ChatID, "🏬 Upstock from store — which product?", rows)
}

// cbUpstockStoreProd (§2.3): subscription-folder picker, cached server-side
// (StoreSubCache) since a subscription NAME could be long/arbitrary and blow
// the 64-byte callback_data budget — unlike every other product picker in
// this category, an index alone can't safely round-trip this choice.
func (b *Bot) cbUpstockStoreProd(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "upstockstoreprod:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product not found.", admNav("adm:upstockstore"))
		return
	}
	if b.stock == nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Stock not configured.", admNav("admcat:stock"))
		return
	}
	subs := b.stock.ListStoreSubs(product)
	if len(subs) == 0 {
		b.send(cc.Ctx, cc.ChatID, "🏬 "+product+": the store is empty (no staged accounts).", admNav("admcat:stock"))
		return
	}
	if len(subs) > 50 {
		subs = subs[:50]
	}
	cc.st.mu.Lock()
	cc.st.UpstockStoreSubs = &StoreSubCache{Product: product, Subs: subs}
	cc.st.mu.Unlock()
	rows := make([][]tg.InlineKeyboardButton, 0, len(subs)+1)
	for i, s := range subs {
		rows = append(rows, btnRow(s.Name+" ("+itoa(s.Count)+")", cbData("upstockstoresub", itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", "adm:upstockstore"))
	text := "🏬 Upstock from store — " + product + "\nPick a subscription folder to promote into stock\n(all its accounts are assumed subscribed + ready):"
	b.sendMenu(cc.Ctx, cc.ChatID, text, rows)
}

// cbUpstockStoreSub (§2.4): NO separate confirm step — tapping immediately
// promotes.
func (b *Bot) cbUpstockStoreSub(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "upstockstoresub:")
	cc.st.mu.Lock()
	cache := cc.st.UpstockStoreSubs
	cc.st.mu.Unlock()
	idx, convErr := strconv.Atoi(idxStr)
	if cache == nil || convErr != nil || idx < 0 || idx >= len(cache.Subs) {
		b.send(cc.Ctx, cc.ChatID, "⚠️ That list expired. Open 🏬 Upstock from store again.", admNav("admcat:stock"))
		return
	}
	product, sub := cache.Product, cache.Subs[idx].Name
	if b.stock == nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Stock not configured.", admNav("admcat:stock"))
		return
	}
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			b.runUpstockFromStore(fctx, cc.ChatID, product, sub)
		})
	})
}

func (b *Bot) runUpstockFromStore(ctx context.Context, chatID int64, product, sub string) {
	if b.stock.GetAccountType(product) == "token" {
		b.send(ctx, chatID,
			"⚠️ "+product+" is a JSON/token product. Store accounts are email|password (SSO) — promoting them would mix formats. Aborted.",
			admNav("admcat:stock"))
		return
	}
	if err := b.stock.SetAccountType(product, "password"); err != nil {
		log.Println("adminstock: SetAccountType failed:", err)
	}
	res, err := b.stock.UpstockFromStore(product, sub)
	if err != nil {
		log.Println("adminstock: UpstockFromStore failed:", err)
		b.send(ctx, chatID, "⚠️ Upstock from store failed. Check server logs.", admNav("admcat:stock"))
		return
	}
	avail, _ := b.stock.CountAvailable(product)
	text := "🏬 Upstock from store — " + product + " / " + sub + "\n" +
		"✅ Added: " + itoa(res.Added) + "\n" +
		"↩️ Duplicate (already in stock/sold, removed): " + itoa(res.SkippedKnown) + "\n"
	if res.SkippedNoEmail > 0 {
		text += "⚠️ Bad (no email, kept for inspection): " + itoa(res.SkippedNoEmail) + "\n"
	}
	text += "📦 From store: " + itoa(res.Total) + "\n" +
		"📊 Now available: " + itoa(avail)
	b.send(ctx, chatID, text, admNavRetry("admcat:stock", "🏬 Upstock from store", "adm:upstockstore"))
	if res.Added > 0 && b.autoStockBroadcastEnabled() {
		b.broadcastStockUpdate(ctx, product)
	}
}

// ============================================================================
// §4 adm:mngprod — Manage products. Full-admin-only.
// ============================================================================

func (b *Bot) cbAdmMngProd(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	cc.st.ClearAdminPending()
	b.showManageProducts(cc.Ctx, cc.ChatID)
}

func (b *Bot) showManageProducts(ctx context.Context, chatID int64) {
	rows := [][]tg.InlineKeyboardButton{btnRow("➕ Create new product", "prodnew")}
	for i, p := range b.listProducts() {
		avail := 0
		if b.stock != nil {
			avail, _ = b.stock.CountAvailable(p)
		}
		rows = append(rows, btnRow("🗂️ "+p+" ("+itoa(avail)+")", cbData("prodmng", itoa(i))))
	}
	rows = append(rows, btnRow("↩️ Back", "admcat:stock"))
	b.sendMenu(ctx, chatID, "🗂️ MANAGE PRODUCTS\nPick one, or create a new product:", rows)
}

// ---- §4.1 create ------------------------------------------------------------

func (b *Bot) cbProdNew(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	cc.st.mu.Lock()
	cc.st.NewProd = true
	cc.st.mu.Unlock()
	text := "➕ Send the new product name.\n" +
		"Allowed: letters, digits, space, . _ -\n" +
		"(This creates an empty product; add stock with Upstock.)"
	b.send(cc.Ctx, cc.ChatID, text, backKB("adm:mngprod"))
}

// capNewProd ABORTS SILENTLY on a nav/command escape — Python's pop-inside-
// the-if pattern (§4.1/§11): the pop happens UNCONDITIONALLY here too, ahead
// of the escape check, matching that shape exactly.
func (b *Bot) capNewProd(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.NewProd
	c.st.NewProd = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	if b.stock == nil {
		b.send(c.Ctx, c.ChatID, "⚠️ Stock not configured.", admNav("adm:mngprod"))
		return true
	}
	name := strings.TrimSpace(c.Text)
	if err := b.stock.CreateProduct(name); err != nil {
		b.send(c.Ctx, c.ChatID, "⛔ Couldn't create: "+err.Error(), admNav("adm:mngprod"))
		return true
	}
	idx := indexOf(b.listProducts(), name)
	idxStr := itoa(idx)
	rows := [][]tg.InlineKeyboardButton{
		styledBtnRow("🌐 Public Sale — every customer", cbData("prodvis", idxStr, "pub"), "success"),
		styledBtnRow("🔒 Private Sale — selected customers only", cbData("prodvis", idxStr, "priv"), "danger"),
	}
	text := "✅ Created product " + name + ".\n\n" +
		"Who can buy it?\n" +
		"• 🌐 Public Sale — visible to everyone (current flow, also the default if you skip this).\n" +
		"• 🔒 Private Sale — only customers you pick from the private-sale customers list can see or buy it.\n\n" +
		"Add stock via 📥 Upstock, set prices in 🏷️ Pricing."
	b.send(c.Ctx, c.ChatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
	return true
}

// ---- §4.2 product action menu ------------------------------------------------

func (b *Bot) cbProdMng(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	cc.st.ClearAdminPending()
	idxStr := strings.TrimPrefix(cc.Data, "prodmng:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	b.showProductActions(cc.Ctx, cc.ChatID, product)
}

func (b *Bot) showProductActions(ctx context.Context, chatID int64, product string) {
	idx := indexOf(b.listProducts(), product)
	if idx < 0 {
		b.send(ctx, chatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	idxStr := itoa(idx)
	actype := "password"
	if b.stock != nil {
		actype = b.stock.GetAccountType(product)
	}
	statusBtn := "⏸️ Set SUSPENDED (pause sales)"
	statusLine := "🟢 Status: Active"
	if ts, susp := b.productSuspendedAt(product); susp {
		statusBtn = "🟢 Set ACTIVE (resume sales)"
		statusLine = "⏸️ Status: SUSPENDED (since " + fmtWhen(ts, i18n.EN) + ")"
	}
	visLine := "🌐 Visibility: Public"
	if allowed, isPriv := b.productPrivateAllowed(product); isPriv {
		visLine = "🔒 Visibility: PRIVATE (" + itoa(len(allowed)) + " customer(s) allowed)"
	}
	// Task #19 follow-up: Available and Reserved must come from the SAME
	// sweep instant on this rendered line, not two separate CountAvailable/
	// CountReserved lock acquisitions (see stock.CountsSnapshot's doc
	// comment for the full mechanism — an expiry landing between two
	// separate calls can drop an account from both buckets for one render).
	avail, resv, sold := 0, 0, 0
	if b.stock != nil {
		counts, _ := b.stock.CountsSnapshot(product)
		avail, resv = counts.Available, counts.Reserved
		sold = b.stock.CountSold(product)
	}
	text := "🗂️ " + product + "\n" +
		"Type: " + actype + "\n" +
		statusLine + "\n" +
		visLine + "\n" +
		"🟢 Available: " + itoa(avail) + " · 🔒 Reserved: " + itoa(resv) + " · ✅ Sold: " + itoa(sold) + "\n\n" +
		"Choose an action:"
	rows := [][]tg.InlineKeyboardButton{
		btnRow(statusBtn, cbData("prodsusp", idxStr)),
		styledBtnRow("🌐 Public Sale", cbData("prodvis", idxStr, "pub"), "success"),
		styledBtnRow("🔒 Private Sale", cbData("prodvis", idxStr, "priv"), "danger"),
		btnRow("✏️ Rename", cbData("prodren", idxStr)),
		btnRow("📢 Buy notification", cbData("prodnotice", idxStr)),
		btnRow("🚚 Delivery message", cbData("proddelivery", idxStr)),
		btnRow("📝 Edit config", cbData("prodcfg", idxStr)),
		btnRow("🔑 Edit .env", cbData("prodenv", idxStr)),
		btnRow("🗑 Remove product", cbData("prodrm", idxStr)),
		btnRow("↩️ Back", "adm:mngprod"),
	}
	b.sendMenu(ctx, chatID, text, rows)
}

// ---- §4.3 suspend/activate ---------------------------------------------------

func (b *Bot) cbProdSusp(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "prodsusp:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	_, wasSuspended := b.productSuspendedAt(product)
	if err := b.setProductSuspended(product, !wasSuspended, cc.ChatID); err != nil {
		log.Println("adminstock: setProductSuspended failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("adm:mngprod"))
		return
	}
	action, outcome := "product_suspend", "suspended"
	if wasSuspended {
		action, outcome = "product_activate", "active"
	}
	if b.store != nil {
		b.store.LogAction(fmtInt(cc.ChatID), "admin", action, product, outcome, "")
	}
	b.showProductActions(cc.Ctx, cc.ChatID, product)
}

// ---- §4.4 visibility (public/private) + allowed-customers picker ------------

func (b *Bot) cbProdVis(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "prodvis:")
	idxStr, mode, _ := strings.Cut(rest, ":")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	switch mode {
	case "pub":
		if err := b.setProductPublic(product); err != nil {
			log.Println("adminstock: setProductPublic failed:", err)
			b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("adm:mngprod"))
			return
		}
		if b.store != nil {
			b.store.LogAction(fmtInt(cc.ChatID), "admin", "product_public", product, "public", "")
		}
		b.send(cc.Ctx, cc.ChatID, "🌐 "+product+" is now PUBLIC — every customer can see and buy it.", nil)
		b.showProductActions(cc.Ctx, cc.ChatID, product)
	case "priv":
		if _, alreadyPrivate := b.productPrivateAllowed(product); !alreadyPrivate {
			if err := b.setProductPrivateInit(product); err != nil {
				log.Println("adminstock: setProductPrivateInit failed:", err)
				b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("adm:mngprod"))
				return
			}
			if b.store != nil {
				b.store.LogAction(fmtInt(cc.ChatID), "admin", "product_private", product, "private", "")
			}
		}
		b.showPrivatePicker(cc.Ctx, cc.ChatID, product)
	}
}

func (b *Bot) showPrivatePicker(ctx context.Context, chatID int64, product string) {
	idx := indexOf(b.listProducts(), product)
	if idx < 0 {
		b.send(ctx, chatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	allowed, _ := b.productPrivateAllowed(product)
	wl := b.whitelist()
	rows := make([][]tg.InlineKeyboardButton, 0, len(wl)+1)
	for wi, e := range wl {
		mark := "☐"
		if stockContains(allowed, e.V) {
			mark = "☑️"
		}
		label := e.V
		if !isAllDigits(e.V) {
			label = "@" + e.V
		}
		rows = append(rows, btnRow(mark+" "+label, cbData("prodwlt", itoa(idx), itoa(wi))))
	}
	rows = append(rows, btnRow("↩️ Back", cbData("prodmng", itoa(idx))))
	text := "🔒 " + product + " — allowed customers\nTap to grant/revoke:"
	b.sendMenu(ctx, chatID, text, rows)
}

func (b *Bot) cbProdWlt(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	rest := strings.TrimPrefix(cc.Data, "prodwlt:")
	pidxStr, widxStr, _ := strings.Cut(rest, ":")
	product, ok := b.productAt(pidxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	wl := b.whitelist()
	widx, convErr := strconv.Atoi(widxStr)
	if convErr != nil || widx < 0 || widx >= len(wl) {
		b.showPrivatePicker(cc.Ctx, cc.ChatID, product) // stale index — re-render fresh
		return
	}
	entry := wl[widx].V
	allowed, _ := b.productPrivateAllowed(product)
	granted := !stockContains(allowed, entry)
	next := allowed
	if granted {
		next = append(append([]string{}, allowed...), entry)
	} else {
		next = stockRemove(allowed, entry)
	}
	if err := b.setProductPrivateList(product, next); err != nil {
		log.Println("adminstock: setProductPrivateList failed:", err)
		b.send(cc.Ctx, cc.ChatID, settingsSaveFailed, admNav("adm:mngprod"))
		return
	}
	outcome := "revoke " + entry
	if granted {
		outcome = "grant " + entry
	}
	if b.store != nil {
		b.store.LogAction(fmtInt(cc.ChatID), "admin", "product_private_access", product, outcome, "")
	}
	b.showPrivatePicker(cc.Ctx, cc.ChatID, product)
}

// ---- §4.5 buy notification / delivery message -------------------------------

func (b *Bot) cbProdNotice(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "prodnotice:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	cur := "(none)"
	if b.stock != nil {
		if v := b.stock.ProductNotice(product); v != "" {
			cur = v
		}
	}
	cc.st.mu.Lock()
	cc.st.SetNotice = product
	cc.st.mu.Unlock()
	text := "📢 Buy notification — " + product + "\n\n" +
		"Current:\n" + cur + "\n\n" +
		"Send the new text (shown to the customer right after they pick this product, with a Continue button before the tiers).\n" +
		"Send a single '-' to clear it."
	b.send(cc.Ctx, cc.ChatID, text, backKB("adm:mngprod"))
}

func (b *Bot) cbProdDelivery(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "proddelivery:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	cur := "(none)"
	if b.stock != nil {
		if v := b.stock.ProductDelivery(product); v != "" {
			cur = v
		}
	}
	cc.st.mu.Lock()
	cc.st.SetDelivery = product
	cc.st.mu.Unlock()
	text := "🚚 Delivery message — " + product + "\n\n" +
		"Current:\n" + cur + "\n\n" +
		"Send the new text (appended below the purchase confirmation, sent to the buyer with their account files). Put your login / usage instructions here.\n" +
		"Send a single '-' to clear it."
	b.send(cc.Ctx, cc.ChatID, text, backKB("adm:mngprod"))
}

// capSetNotice / capSetDelivery ABORT SILENTLY on a nav/command escape,
// mirroring capNewProd's unconditional-pop shape (§11).
func (b *Bot) capSetNotice(c *Ctx) bool {
	c.st.mu.Lock()
	product := c.st.SetNotice
	c.st.SetNotice = ""
	c.st.mu.Unlock()
	if product == "" {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	if b.stock == nil {
		return true
	}
	if strings.TrimSpace(c.Text) == "-" {
		if err := b.stock.SetProductNotice(product, ""); err != nil {
			log.Println("adminstock: SetProductNotice clear failed:", err)
		}
		b.send(c.Ctx, c.ChatID, "✅ Buy notification cleared for "+product+".", admNav("adm:mngprod"))
		return true
	}
	if err := b.stock.SetProductNotice(product, c.Text); err != nil {
		log.Println("adminstock: SetProductNotice failed:", err)
		b.send(c.Ctx, c.ChatID, "⛔ Save failed.", admNav("adm:mngprod"))
		return true
	}
	n := len([]rune(c.Text))
	b.send(c.Ctx, c.ChatID,
		"✅ Buy notification set for "+product+" ("+itoa(n)+" chars). Customers see it after picking the product, before the tiers.",
		admNav("adm:mngprod"))
	return true
}

func (b *Bot) capSetDelivery(c *Ctx) bool {
	c.st.mu.Lock()
	product := c.st.SetDelivery
	c.st.SetDelivery = ""
	c.st.mu.Unlock()
	if product == "" {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	if b.stock == nil {
		return true
	}
	if strings.TrimSpace(c.Text) == "-" {
		if err := b.stock.SetProductDelivery(product, ""); err != nil {
			log.Println("adminstock: SetProductDelivery clear failed:", err)
		}
		b.send(c.Ctx, c.ChatID, "✅ Delivery message cleared for "+product+".", admNav("adm:mngprod"))
		return true
	}
	if err := b.stock.SetProductDelivery(product, c.Text); err != nil {
		log.Println("adminstock: SetProductDelivery failed:", err)
		b.send(c.Ctx, c.ChatID, "⛔ Save failed.", admNav("adm:mngprod"))
		return true
	}
	n := len([]rune(c.Text))
	b.send(c.Ctx, c.ChatID,
		"✅ Delivery message set for "+product+" ("+itoa(n)+" chars). Buyers see it with their account files at delivery.",
		admNav("adm:mngprod"))
	return true
}

// ---- §4.6 rename --------------------------------------------------------------

func (b *Bot) cbProdRen(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "prodren:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	resv := 0
	if b.stock != nil {
		resv, _ = b.stock.CountReserved(product)
	}
	if resv > 0 {
		b.send(cc.Ctx, cc.ChatID,
			"⛔ "+product+" has "+itoa(resv)+" reserved (in-flight) order(s). Rename after they settle.",
			admNav("adm:mngprod"))
		return
	}
	cc.st.mu.Lock()
	cc.st.RenProd = product
	cc.st.mu.Unlock()
	text := "✏️ Rename " + product + "\nSend the new name.\n⚠️ Past orders + sold history keep the OLD name in records."
	b.send(cc.Ctx, cc.ChatID, text, backKB(cbData("prodmng", idxStr)))
}

// capRenProd ABORTS SILENTLY (unconditional pop, §11).
func (b *Bot) capRenProd(c *Ctx) bool {
	c.st.mu.Lock()
	oldProd := c.st.RenProd
	c.st.RenProd = ""
	c.st.mu.Unlock()
	if oldProd == "" {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	if b.stock == nil {
		b.send(c.Ctx, c.ChatID, "⚠️ Stock not configured.", admNav("adm:mngprod"))
		return true
	}
	newName := strings.TrimSpace(c.Text)
	if err := b.stock.RenameProduct(oldProd, newName); err != nil {
		b.send(c.Ctx, c.ChatID, "⛔ Rename failed: "+err.Error(), admNav("adm:mngprod"))
		return true
	}
	reply := "✅ Renamed " + oldProd + " → " + newName + ".\n⚠️ Existing orders/history still reference the old name."
	// carryProductSettings's failure can't stay a log-only concern: both
	// productSuspended and productPrivateAllowed (catalog.go) key off
	// PRESENCE in the settings map, not a "renamed" flag. RenameProduct just
	// moved the folder to newName on disk, so the instant the settings carry
	// fails, the cached config still has the flag under oldProd and NOTHING
	// under newName — a suspended product reads as sellable and a
	// private-only product reads as public, with no restart or delay needed
	// to trigger it. The admin who just renamed the product is the one
	// person positioned to re-apply the flag immediately, so the reply must
	// say so instead of quietly trusting a log line nobody is watching.
	if err := b.carryProductSettings(oldProd, newName); err != nil {
		reply += "\n⚠️ Suspension / private-sale flags did NOT carry over to the new name. The product may now be SELLABLE and PUBLIC. Re-apply them manually."
	}
	b.send(c.Ctx, c.ChatID, reply, nil)
	b.showManageProducts(c.Ctx, c.ChatID)
	return true
}

// carryProductSettings moves the suspended/private settings.json entries from
// old to new after a successful rename (§4.6) — the timestamp and allow-list
// survive the move; only the JSON's KEY changes. Returns a non-nil error only
// when there WAS something to carry and the save failed, so the caller can
// tell the admin the old flags are now orphaned under a folder that no
// longer exists.
func (b *Bot) carryProductSettings(old, new string) error {
	if b.cfg == nil {
		return nil
	}
	s := b.settings()
	changed := false
	if susp, ok := s["suspended"].(map[string]any); ok {
		if v, ok := susp[old]; ok {
			susp[new] = v
			delete(susp, old)
			s["suspended"] = susp
			changed = true
		}
	}
	if pp, ok := s["private_products"].(map[string]any); ok {
		if v, ok := pp[old]; ok {
			pp[new] = v
			delete(pp, old)
			s["private_products"] = pp
			changed = true
		}
	}
	if changed {
		if err := b.cfg.SaveSettings(s); err != nil {
			log.Println("adminstock: carryProductSettings save failed:", err)
			return err
		}
	}
	return nil
}

// ---- §4.7 edit config/.env ----------------------------------------------------

func (b *Bot) cbProdCfg(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "prodcfg:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	body := ""
	if b.stock != nil {
		body = b.stock.ReadTextFile(product, "config")
	}
	cc.st.mu.Lock()
	cc.st.EditFile = &EditFileCtx{Product: product, File: "config"}
	cc.st.mu.Unlock()
	text := "📝 Edit config for " + product + "\n\n" +
		"Current:\n----\n" + body + "\n----\n\n" +
		"Send the FULL new contents (replaces the file).\n" +
		"Format: KEY=\"value\" lines, e.g. START_URL=\"https://...\""
	b.send(cc.Ctx, cc.ChatID, text, backKB(cbData("prodmng", idxStr)))
}

func (b *Bot) cbProdEnv(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "prodenv:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	body := ""
	if b.stock != nil {
		body = b.stock.ReadTextFile(product, ".env")
	}
	cc.st.mu.Lock()
	cc.st.EditFile = &EditFileCtx{Product: product, File: ".env"}
	cc.st.mu.Unlock()
	text := "🔑 Edit .env for " + product + "\n\n" +
		"Current:\n----\n" + body + "\n----\n\n" +
		"Send the FULL new contents (replaces the file).\n" +
		"Format: KEY=value lines (per-product AWS creds etc.).\n" +
		"A bare ' #' starts a comment — quote any value that contains one: KEY=\"pa #ss\"."
	b.send(cc.Ctx, cc.ChatID, text, backKB(cbData("prodmng", idxStr)))
}

// capEditFile is the ONE capture in this whole category that aborts WITH an
// explicit message (§11), not silently.
func (b *Bot) capEditFile(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.EditFile
	c.st.mu.Unlock()
	if armed == nil {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		c.st.mu.Lock()
		if c.st.EditFile == armed {
			c.st.EditFile = nil
		}
		c.st.mu.Unlock()
		b.send(c.Ctx, c.ChatID, "↩️ Edit cancelled (you tapped a menu / command). Nothing was changed.", admNav("admcat:stock"))
		return false // falls through so the tapped thing still runs
	}
	c.st.mu.Lock()
	if c.st.EditFile == armed {
		c.st.EditFile = nil
	}
	c.st.mu.Unlock()
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	if b.stock == nil {
		return true
	}
	content := strings.TrimSpace(c.Text) // outer-whitespace-trimmed, internal newlines kept
	if err := b.stock.WriteTextFile(armed.Product, armed.File, content); err != nil {
		b.send(c.Ctx, c.ChatID, "⛔ Save failed: "+err.Error(), admNav("adm:mngprod"))
		return true
	}
	b.send(c.Ctx, c.ChatID, "✅ Saved "+armed.File+" for "+armed.Product+".", nil)
	b.showProductActions(c.Ctx, c.ChatID, armed.Product)
	return true
}

// ---- §4.8 remove --------------------------------------------------------------

func (b *Bot) cbProdRm(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	idxStr := strings.TrimPrefix(cc.Data, "prodrm:")
	product, ok := b.productAt(idxStr)
	if !ok {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Product no longer exists.", admNav("adm:mngprod"))
		return
	}
	// Task #19 follow-up — same reasoning as the prod-detail screen above:
	// Available/Reserved must share one sweep instant via CountsSnapshot.
	//
	// Noted, not fixed: the error from CountsSnapshot is discarded below
	// (`_`), same as this function discarded two separate errors pre-fix. A
	// lock failure now silently yields resv==0, which — unlike the
	// pre-fix behaviour on adjacent screens — skips the very guard right
	// below that stands between an admin and deleting a product with LIVE
	// reservations. Not a regression introduced by this fix, but this guard
	// is now one discarded error away from not firing on a destructive path.
	avail, sold, resv := 0, 0, 0
	if b.stock != nil {
		counts, _ := b.stock.CountsSnapshot(product)
		avail, resv = counts.Available, counts.Reserved
		sold = b.stock.CountSold(product)
	}
	if resv > 0 {
		b.send(cc.Ctx, cc.ChatID,
			"⛔ "+product+" has "+itoa(resv)+" reserved (in-flight) order(s). Remove after they settle.",
			admNav("adm:mngprod"))
		return
	}
	// Stashed by NAME (survives a list-order change between prompt and
	// confirm), not index (§10/§4.8).
	cc.st.mu.Lock()
	cc.st.RmProd = product
	cc.st.mu.Unlock()
	text := "⚠️ Delete product " + product + "?\n" +
		"🟢 " + itoa(avail) + " available · ✅ " + itoa(sold) + " sold will be erased from this folder.\n" +
		"Order history in ./orders is kept. This cannot be undone."
	rows := [][]tg.InlineKeyboardButton{
		styledBtnRow("🗑 DELETE "+product+" permanently", "prodrmok", "danger"),
		btnRow("❌ Cancel", "adm:mngprod"),
	}
	b.send(cc.Ctx, cc.ChatID, text, &tg.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) cbProdRmOK(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "adm:mngprod") {
		return
	}
	cc.st.mu.Lock()
	product := cc.st.RmProd
	cc.st.RmProd = ""
	cc.st.mu.Unlock()
	if product == "" {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Nothing to delete (confirm expired). Start again.", admNav("adm:mngprod"))
		return
	}
	if b.stock == nil {
		b.send(cc.Ctx, cc.ChatID, "⚠️ Stock not configured.", admNav("adm:mngprod"))
		return
	}
	if err := b.stock.RemoveProduct(product); err != nil {
		b.send(cc.Ctx, cc.ChatID, "⛔ Could not remove "+product+": "+err.Error(), admNav("adm:mngprod"))
		return
	}
	if err := b.setProductSuspended(product, false, cc.ChatID); err != nil {
		log.Println("adminstock: clear suspended on remove failed:", err)
	}
	if err := b.setProductPublic(product); err != nil {
		log.Println("adminstock: clear private on remove failed:", err)
	}
	b.send(cc.Ctx, cc.ChatID, "🗑 Removed product "+product+".", nil)
	b.showManageProducts(cc.Ctx, cc.ChatID)
}

// ---- settings.json read/write helpers (suspend/private) ----------------------
//
// Only READ-side helpers existed before this slice (catalog.go's
// productSuspended/productPrivateAllowed, adminorders.go's
// productSuspendedAt) — no earlier admin flow ever needed to flip these
// settings, so the writers below are new.

func (b *Bot) setProductSuspended(product string, suspended bool, by int64) error {
	if b.cfg == nil {
		return errNoConfig
	}
	s := b.settings()
	susp, _ := s["suspended"].(map[string]any)
	if susp == nil {
		susp = map[string]any{}
	}
	if suspended {
		susp[product] = map[string]any{"ts": float64(b.now().Unix()), "by": fmtInt(by)}
	} else {
		delete(susp, product)
	}
	s["suspended"] = susp
	return b.cfg.SaveSettings(s)
}

func (b *Bot) setProductPublic(product string) error {
	if b.cfg == nil {
		return errNoConfig
	}
	s := b.settings()
	pp, ok := s["private_products"].(map[string]any)
	if !ok || pp == nil {
		return nil // never was private — nothing to clear
	}
	if _, present := pp[product]; !present {
		return nil
	}
	delete(pp, product)
	s["private_products"] = pp
	return b.cfg.SaveSettings(s)
}

func (b *Bot) setProductPrivateInit(product string) error {
	if b.cfg == nil {
		return errNoConfig
	}
	s := b.settings()
	pp, _ := s["private_products"].(map[string]any)
	if pp == nil {
		pp = map[string]any{}
	}
	pp[product] = []any{}
	s["private_products"] = pp
	return b.cfg.SaveSettings(s)
}

func (b *Bot) setProductPrivateList(product string, allowed []string) error {
	if b.cfg == nil {
		return errNoConfig
	}
	s := b.settings()
	pp, _ := s["private_products"].(map[string]any)
	if pp == nil {
		pp = map[string]any{}
	}
	list := make([]any, len(allowed))
	for i, v := range allowed {
		list[i] = v
	}
	pp[product] = list
	s["private_products"] = pp
	return b.cfg.SaveSettings(s)
}

func stockContains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func stockRemove(xs []string, v string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// ============================================================================
// §5 adm:stockbroadcast — Stock update broadcast. Full-admin-only. No input,
// no confirmation — a single tap immediately spawns the fan-out.
// ============================================================================

func (b *Bot) cbAdmStockBroadcast(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	chatID := cc.ChatID
	b.workers.Go(func(stop <-chan struct{}) {
		runOnStoppableCtx(stop, func(fctx context.Context) {
			announced := b.broadcastAllStock(fctx)
			var text string
			if len(announced) > 0 {
				lines := make([]string, len(announced))
				for i, p := range announced {
					lines[i] = "• " + p
				}
				text = "📣 Stock broadcast sent for:\n" + strings.Join(lines, "\n")
			} else {
				text = "⚠️ No in-stock products to announce."
			}
			b.send(fctx, chatID, text, admNav("admcat:stock"))
		})
	})
}

// broadcastAllStock is Python's broadcast_all_stock (§5): every non-suspended
// product with available stock gets one broadcastStockUpdate call; returns
// the announced names.
func (b *Bot) broadcastAllStock(ctx context.Context) []string {
	if b.stock == nil {
		return nil
	}
	var announced []string
	for _, p := range b.listProducts() {
		if b.productSuspended(p) {
			continue
		}
		avail, err := b.stock.CountAvailable(p)
		if err != nil || avail <= 0 {
			continue
		}
		b.broadcastStockUpdate(ctx, p)
		announced = append(announced, p)
	}
	return announced
}

// autoStockBroadcastEnabled reads the "auto_stock_broadcast" settings toggle
// (default ON) — gates the automatic post-upstock broadcast (§1.9/§2.4).
func (b *Bot) autoStockBroadcastEnabled() bool {
	return settingBool(b.settings(), "auto_stock_broadcast", true)
}

// ============================================================================
// §9 broadcast_stock_update — the customer-facing, trilingual restock alert
// used by doUpstock/runUpstockFromStore/broadcastAllStock.
// ============================================================================

// stockBroadcastLang is the ONE place in this whole admin category whose
// per-recipient fallback is Vietnamese, not English (§9) — b.langFor's OWN
// fallback argument is always ignored (dispatch.go hard-codes "en" for any
// chat that never chose a language), so this reads the same in-memory map
// directly rather than going through that shared helper.
func (b *Bot) stockBroadcastLang(chatID int64) i18n.Lang {
	b.chatLangMu.Lock()
	defer b.chatLangMu.Unlock()
	if l, ok := b.chatLang[chatID]; ok {
		return l
	}
	return i18n.VI
}

// broadcastStockUpdate fans a restock alert out to every eligible customer —
// Python broadcast_stock_update (§9). Suppressed entirely for a suspended
// product; a PRIVATE product only pings its allow-listed customers. No
// _no_autodelete() equivalent — this Go port has no auto-delete sweep at all
// (grep-verified), so there is nothing to opt out of.
func (b *Bot) broadcastStockUpdate(ctx context.Context, product string) {
	if b.productSuspended(product) {
		return
	}
	if b.store == nil {
		return
	}
	ids, err := b.store.AllChatIDs()
	if err != nil {
		return
	}
	avail := 0
	var tiers []stock.Tier
	if b.stock != nil {
		avail, _ = b.stock.CountAvailable(product)
		tiers = b.stock.ProductTiers(product)
	}
	for _, idStr := range ids {
		cid, convErr := strconv.ParseInt(idStr, 10, 64)
		if convErr != nil {
			continue
		}
		if !b.productVisibleTo(cid, product) {
			continue
		}
		lang := b.stockBroadcastLang(cid)
		prices := ""
		if len(tiers) > 0 {
			lines := make([]string, len(tiers))
			for i, t := range tiers {
				lines[i] = "💰 " + b.usdtDisp(int64(t.Price)) + " (" + b.tierLabel(t.Code, lang) + ")"
			}
			prices = strings.Join(lines, "\n")
		}
		text := stockBroadcastText(lang, product, prices, avail)
		_, _ = b.tg.SendMessage(ctx, tg.SendMessageParams{ChatID: cid, Text: text}) // best-effort, never aborts the loop
	}
}

// stockBroadcastText renders one language's STOCK_BROADCAST body — verbatim
// strings (§9). Kept as a local table (not a internal/i18n/catalog_*.go
// entry — see file header's divergence note).
func stockBroadcastText(lang i18n.Lang, product, prices string, available int) string {
	avail := itoa(available)
	switch lang {
	case i18n.VI:
		return "🔥 SẢN PHẨM VỪA CẬP NHẬT!\n\n" +
			"🤖 " + product + "\n" +
			prices + "\n" +
			"📊 Còn: " + avail + " tài khoản\n\n" +
			"🛒 Mua ngay tại bot trước khi hết hàng!\n" +
			"👉 Bấm /start để đặt hàng"
	case i18n.ZH:
		return "🔥 商品刚刚更新！\n\n" +
			"🤖 " + product + "\n" +
			prices + "\n" +
			"📊 剩余：" + avail + " 个账户\n\n" +
			"🛒 立即购买，数量有限！\n" +
			"👉 按 /start 下单"
	default:
		return "🔥 STOCK JUST UPDATED!\n\n" +
			"🤖 " + product + "\n" +
			prices + "\n" +
			"📊 Available: " + avail + " accounts\n\n" +
			"🛒 Buy now before it's gone!\n" +
			"👉 Press /start to order"
	}
}

// ============================================================================
// §6 adm:provpool — Provision Kiro pool. SUPER-ADMIN ONLY.
// ============================================================================

// provPoolMu/provPoolRunning is the single-flight guard for a pool-
// provisioning batch — Python's _provpool_running bool under _provpool_lock
// (§6.3). Process-wide (not per-chat): only ONE browser-login batch may run
// at a time on this host, matching the fixed M365 OAuth callback port's own
// single-fleet constraint.
var (
	provPoolMu      sync.Mutex
	provPoolRunning bool
)

// provPoolPreflight checks Kiro-Go is configured enough to provision into —
// §6.1's preflight, run at ARM time (before the flow mix is even known, so
// the browser-stack check itself waits until runPoolProvision).
func (b *Bot) provPoolPreflight() string {
	if b.cfg == nil {
		return "Kiro-Go not configured."
	}
	if b.cfg.KiroGoBaseURL() == "" {
		return "Kiro-Go Server URL not set (Settings → Kiro-Go Server URL)."
	}
	if b.cfg.KiroGoAdminKey() == "" {
		return "Kiro-Go Admin Password not set (Settings → Kiro-Go Admin Password)."
	}
	if b.cfg.KiroGoAccountID() == "" {
		return "Kiro-Go Account ID not set (Settings → Kiro-Go Account ID)."
	}
	return ""
}

func (b *Bot) cbAdmProvPool(cc *cbCtx) {
	if !b.fullAdminOnly(cc, "admcat:stock") {
		return
	}
	if !b.isSuperAdmin(cc.ChatID) { // belt-and-suspenders, matches fullAdminOnly's own pattern
		return
	}
	if errText := b.provPoolPreflight(); errText != "" {
		b.send(cc.Ctx, cc.ChatID, "⛔ Can't provision: "+errText, admNav("admcat:stock"))
		return
	}
	cc.st.ClearAdminPending()
	cc.st.mu.Lock()
	cc.st.ProvPool = true
	cc.st.mu.Unlock()
	text := "🌐 PROVISION KIRO POOL\n" +
		"Send what to add to the pool — either PASTE it or UPLOAD a .txt.\n\n" +
		"One per line, any of these (they can be mixed):\n" +
		"• Kiro API key — <code>ksk_…</code> (added directly, no login)\n" +
		"• AWS IdC — <code>username|password|start_url</code>\n" +
		"• M365 SSO — <code>email|password</code> or <code>email|password|mfa_secret</code>\n\n" +
		"Or send a delivered order .txt whole — its <code>username:</code>/<code>password:</code> accounts under the order's <code>Login portal:</code> are provisioned as AWS IdC logins.\n\n" +
		"A <code>ksk_</code> key is already minted, so it is added to the pool instantly. Each account line instead runs a headless browser login on the server (~1–2 min each); a forced password reset (IdC) or a newly-registered MFA seed (M365) is handled automatically and reported back."
	b.sendHTMLKB(cc.Ctx, cc.ChatID, text, backKB("admcat:stock"))
}

// capProvPoolFile is the step-5 file intercept — Python's ONLY wired input
// path for this flow (§6.2). Atomic pop-claim: a racing duplicate upload
// drops rather than double-provisioning.
func (b *Bot) capProvPoolFile(c *Ctx) bool {
	if c.Msg == nil || c.Msg.Document == nil {
		return false
	}
	st := b.states.Get(c.ChatID)
	st.mu.Lock()
	armed := st.ProvPool
	st.ProvPool = false
	st.mu.Unlock()
	if !armed {
		return false
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	data, err := b.tg.GetFileBytes(c.Ctx, c.Msg.Document.FileID)
	if err != nil {
		log.Println("adminstock: provpool GetFileBytes failed:", err)
		b.send(c.Ctx, c.ChatID, "⚠️ Could not download the file. Try again.", admNav("admcat:stock"))
		return true
	}
	text := strings.ToValidUTF8(string(data), "�")
	b.startPoolProvision(c.Ctx, c.ChatID, text)
	return true
}

// capProvPoolText is the PASTE path — NOT wired in Python (§6.2's confirmed
// gap: file handler only, despite the arm prompt inviting "either PASTE it or
// UPLOAD a .txt"). This Go port fixes it, mirroring the file handler's
// ATOMIC POP-CLAIM shape (single-shot batch kickoff, not an accumulating
// preview like "upstock").
func (b *Bot) capProvPoolText(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.ProvPool
	c.st.ProvPool = false
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false // ABORT silently — falls through so the tapped thing still runs
	}
	if !b.isSuperAdmin(c.ChatID) {
		return true
	}
	b.startPoolProvision(c.Ctx, c.ChatID, c.Text)
	return true
}

func (b *Bot) startPoolProvision(ctx context.Context, chatID int64, accountsText string) {
	provPoolMu.Lock()
	if provPoolRunning {
		provPoolMu.Unlock()
		b.send(ctx, chatID, "⏳ A pool provisioning run is already in progress. Wait for it to finish.", admNav("admcat:stock"))
		return
	}
	provPoolRunning = true
	provPoolMu.Unlock()

	b.workers.Go(func(stop <-chan struct{}) {
		defer func() {
			provPoolMu.Lock()
			provPoolRunning = false
			provPoolMu.Unlock()
		}()
		runOnStoppableCtx(stop, func(fctx context.Context) {
			b.runPoolProvision(fctx, chatID, accountsText)
		})
	})
}

// runPoolProvision is _run_pool_provision (§6.3), steps 1-8.
func (b *Bot) runPoolProvision(ctx context.Context, chatID int64, accountsText string) {
	accounts := provision.ParseAllAccounts(accountsText)
	if len(accounts) == 0 {
		b.send(ctx, chatID,
			"⚠️ No accounts recognized. One per line, any of:\n"+
				"• ksk_… (Kiro API key)\n"+
				"• username|password|start_url (AWS IdC)\n"+
				"• email|password[|mfa_secret] (M365 SSO)\n"+
				"…or paste a whole delivered order .txt.",
			admNav("admcat:stock"))
		return
	}
	var nIDC, nM365, nKey int
	for _, a := range accounts {
		switch a.Flow {
		case provision.FlowIDC:
			nIDC++
		case provision.FlowM365:
			nM365++
		case provision.FlowRawKey:
			nKey++
		}
	}
	var mixParts []string
	if nKey > 0 {
		mixParts = append(mixParts, itoa(nKey)+" API key(s)")
	}
	if nIDC > 0 {
		mixParts = append(mixParts, itoa(nIDC)+" AWS IdC")
	}
	if nM365 > 0 {
		mixParts = append(mixParts, itoa(nM365)+" M365 SSO")
	}
	mix := strings.Join(mixParts, ", ")

	// b.loginPythonStr() resolves through cfg (.env/ambient, task #16) rather
	// than provision.LoginPythonFromEnv's raw os.Getenv, which never sees .env.
	runner := provision.NewScriptRunner(b.loginPythonStr(), scriptDir())
	if nIDC+nM365 > 0 {
		if errText := runner.BrowserStackError(ctx); errText != "" {
			b.send(ctx, chatID,
				"⛔ Browser-login stack unavailable ("+errText+") — "+itoa(nIDC+nM365)+
					" account(s) blocked; "+itoa(nKey)+" rawkey-only would still succeed if uploaded alone.",
				admNav("admcat:stock"))
			return
		}
	}
	if nM365 > 0 && !provision.M365CallbackPortFree() {
		b.send(ctx, chatID,
			"⛔ M365 OAuth callback port "+itoa(provision.M365CallbackPort)+" is busy — free it, or upload IdC/key-only lines separately.",
			admNav("admcat:stock"))
		return
	}

	note := "Kiro API key(s) are added instantly."
	if nIDC+nM365 > 0 {
		note = "Browser-login accounts run ~1–2 min each on the server."
	}
	b.send(ctx, chatID, "🌐 Provisioning "+itoa(len(accounts))+" account(s) into the Kiro pool ("+mix+").\n"+note, nil)

	pool := provision.NewAdminClient(b.cfg.KiroGoBaseURL(), b.cfg.KiroGoAdminKey(), http.DefaultClient)
	p := provision.New(provision.Options{
		Login:       runner,
		Pool:        pool,
		ArchiveRoot: provision.ArchiveRootFromEnv("."),
		Now:         time.Now,
	})
	progress := func(idx, total int, username string, status provision.Status, detail string) {
		text := provPoolStatusIcon(status) + " [" + itoa(idx) + "/" + itoa(total) + "] " + username + ": " + string(status)
		if detail != "" {
			text += " — " + detail
		}
		b.send(ctx, chatID, text, nil)
	}
	results, err := p.Provision(ctx, accountsText, provision.RunOptions{Headless: true, StepDelay: 0, Progress: progress})
	if err != nil {
		b.send(ctx, chatID, "⛔ Provisioning failed: "+err.Error(), admNav("admcat:stock"))
		return
	}

	// Credential rewrites FIRST, ahead of the summary (§6.3 step 7) — never
	// risk truncation cutting them off.
	var rewrites []string
	for _, r := range results {
		if r.NewPassword != "" {
			rewrites = append(rewrites, "• "+r.Username+" password → "+r.NewPassword)
		}
		if r.NewMFA != "" {
			rewrites = append(rewrites, "• "+r.Username+" mfa_secret → "+r.NewMFA)
		}
	}
	if len(rewrites) > 0 {
		b.send(ctx, chatID,
			"⚠️ CREDENTIALS WERE REWRITTEN — save these now:\n"+strings.Join(rewrites, "\n")+"\n(also archived server-side.)",
			nil)
	}

	added := 0
	lines := make([]string, 0, len(results))
	for _, r := range results {
		if r.Status.InPool() {
			added++
		}
		line := provPoolStatusIcon(r.Status) + " [" + provPoolFlowTag(r.Flow) + "] " + r.Username + ": " + string(r.Status)
		if r.Detail != "" {
			line += " — " + r.Detail
		}
		lines = append(lines, line)
	}
	summary := "🌐 Pool provisioning done — " + itoa(added) + "/" + itoa(len(results)) + " in pool.\n\n" + strings.Join(lines, "\n")
	if headroom, ok := b.poolHeadroomCredits(ctx); ok {
		h := "Pool headroom: " + fmtCredits(headroom) + " credits"
		if headroom < 0 {
			h += " (oversold — restock soon)"
		}
		summary += "\n\n" + h
	}
	// 3900, not 4000 — the house standard used by every other report in this
	// layer (adminorders.go's revenue builders, admin.go's sendRecentOrders,
	// adminusers.go). Telegram counts UTF-16 code units, not runes; a non-BMP
	// emoji is 1 rune but 2 units, so 4000 left only ~96 chars of margin
	// against the 4096 hard cap (stats-fixes-spec.md §0 — downgraded from a
	// real risk to a consistency nit there, but still worth matching).
	if runes := []rune(summary); len(runes) > 3900 {
		summary = string(runes[:3900])
	}
	b.send(ctx, chatID, summary, admNav("admcat:stock"))
}

func provPoolStatusIcon(s provision.Status) string {
	switch s {
	case provision.StatusAdded:
		return "✅"
	case provision.StatusDuplicate:
		return "♻️"
	case provision.StatusRepaired:
		return "🔧"
	case provision.StatusDryRun:
		return "🧪"
	default:
		return "❌"
	}
}

func provPoolFlowTag(f provision.Flow) string {
	switch f {
	case provision.FlowIDC:
		return "IdC"
	case provision.FlowM365:
		return "M365"
	case provision.FlowRawKey:
		return "Key"
	default:
		return string(f)
	}
}

// ============================================================================
// §7 adm:pool / admpoolr — Pool health view. Cap: pool_view (NOT in
// DEFAULT_STD_CAPS — full-admin-only until explicitly granted).
// ============================================================================

// fetchPool is kiro_go_pool() (§7): a single FAST attempt (5s), fails open
// (nil,nil) on any error — this runs on the shared worker pool and must
// never let a hung Kiro-Go pin a goroutine.
func (b *Bot) fetchPool(ctx context.Context) map[string]any {
	if b.cfg == nil {
		return nil
	}
	client := provision.NewAdminClient(b.cfg.KiroGoBaseURL(), b.cfg.KiroGoAdminKey(), http.DefaultClient)
	if !client.Configured() {
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := client.Pool(fctx)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// poolHeadroomCredits is the same creditsAvailable-minus-outstandingCredits
// figure fmtPool renders, exposed for runPoolProvision's own summary line.
func (b *Bot) poolHeadroomCredits(ctx context.Context) (float64, bool) {
	pool := b.fetchPool(ctx)
	if pool == nil {
		return 0, false
	}
	avail, _ := settingsNumber(pool, "creditsAvailable")
	outstanding, _ := settingsNumber(pool, "outstandingCredits")
	return avail - outstanding, true
}

func (b *Bot) cbAdmPool(cc *cbCtx) {
	b.sendPoolView(cc.Ctx, cc.ChatID, 0)
}

// cbAdmPoolRefresh is admpoolr — a standalone top-level route, so it
// re-checks isAdmin itself; re-renders IN PLACE on the tapped message.
func (b *Bot) cbAdmPoolRefresh(cc *cbCtx) {
	if !b.isAdmin(cc.ChatID) {
		return
	}
	msgID := int64(0)
	if cc.CB != nil && cc.CB.Message != nil {
		msgID = cc.CB.Message.MessageID
	}
	b.sendPoolView(cc.Ctx, cc.ChatID, msgID)
}

// sendPoolView is _send_pool_view (§7): editMessageID==0 sends fresh;
// otherwise tries editMessageText first, falling back to a fresh send on any
// failure (message too old/deleted) so Refresh never dead-ends.
func (b *Bot) sendPoolView(ctx context.Context, chatID, editMessageID int64) {
	text := fmtPool(b.fetchPool(ctx))
	markup := &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		btnRow("🔄 Refresh", "admpoolr"),
		btnRow("↩️ Back", "admroot"),
	}}
	if editMessageID != 0 {
		if _, err := b.tg.EditMessageText(ctx, tg.EditMessageTextParams{
			ChatID: chatID, MessageID: editMessageID, Text: text, ParseMode: "HTML", ReplyMarkup: markup,
		}); err == nil {
			return
		}
	}
	if _, err := b.tg.SendMessage(ctx, tg.SendMessageParams{
		ChatID: chatID, Text: text, ParseMode: "HTML", ReplyMarkup: markup,
	}); err != nil {
		log.Println("adminstock: sendPoolView failed:", err)
	}
}

// fmtPool is _fmt_pool (§7), HTML parse_mode. ADMIN-ONLY data (business
// economics, pool account emails inside accountStates) — never send to
// customers. No "-1 = unlimited" rendering exists here — that convention is
// the customer-facing per-key quota view's alone (§7's own note); this view
// surfaces unlimitedKeys as a COUNT instead.
func fmtPool(pool map[string]any) string {
	if pool == nil {
		return "⚠️ Pool unavailable (Kiro-Go not reachable)."
	}
	creditsAvailable, _ := settingsNumber(pool, "creditsAvailable")
	creditsUsed, _ := settingsNumber(pool, "creditsUsed")
	creditsLimit, _ := settingsNumber(pool, "creditsLimit")
	outstanding, _ := settingsNumber(pool, "outstandingCredits")
	activeKeys, _ := settingsNumber(pool, "activeKeys")
	unlimitedKeys, _ := settingsNumber(pool, "unlimitedKeys")
	sessionAffinity, _ := pool["sessionAffinity"].(bool)

	var lines []string
	lines = append(lines, "🏊 <b>Kiro-Go Pool</b> — as of "+time.Now().Format("15:04:05"))

	accountStates, hasCensus := pool["accountStates"].(map[string]any)
	if !hasCensus {
		if a, ok := pool["accounts"].(map[string]any); ok {
			usable, _ := settingsNumber(a, "usable")
			total, _ := settingsNumber(a, "total")
			lines = append(lines, "Accounts: "+fmtCredits(usable)+"/"+fmtCredits(total)+" usable")
		}
	}

	lines = append(lines, "Pool credits: "+fmtCredits(creditsAvailable)+" avail ("+
		fmtCredits(creditsUsed)+" used / "+fmtCredits(creditsLimit)+" cap)")

	outLine := "Sold outstanding: " + fmtCredits(outstanding) + " (" + fmtCredits(activeKeys) + " keys"
	if unlimitedKeys > 0 {
		outLine += ", " + fmtCredits(unlimitedKeys) + " unlimited"
	}
	outLine += ")"
	lines = append(lines, outLine)

	raw := creditsAvailable - outstanding
	headroomLine := headroomIcon(raw) + " Headroom: " + fmtCredits(raw) + " credits"
	if raw < 0 {
		headroomLine += " — OVERSOLD by " + fmtCredits(-raw) + " (restock; sales are NOT blocked)"
	}
	lines = append(lines, headroomLine)
	lines = append(lines, "Session affinity: "+onOffWord(sessionAffinity))

	if hasCensus {
		active, _ := settingsNumber(accountStates, "active")
		cooldown, _ := settingsNumber(accountStates, "cooldown")
		banned, _ := settingsNumber(accountStates, "banned")
		disabled, _ := settingsNumber(accountStates, "disabled")
		lines = append(lines, "", "<b>Accounts</b>")
		lines = append(lines, "🟢 Active/Usable: "+fmtCredits(active)+" accounts")
		lines = append(lines, "❄️ Cooldown: "+fmtCredits(cooldown)+" accounts")
		lines = append(lines, "⛔ Banned: "+fmtCredits(banned)+" accounts")
		if disabled > 0 {
			lines = append(lines, "⏸️ Disabled: "+fmtCredits(disabled)+" accounts")
		}
		lines = append(lines, "Σ Total: "+fmtCredits(active+cooldown+banned+disabled)+" accounts")
	}

	if classes, ok := pool["classes"].(map[string]any); ok && len(classes) > 0 {
		lines = append(lines, "", "⚠️ Figures above merge SKUs (mixed units) — per-SKU truth:")
		for _, name := range []string{"bedrock", "kiro", "legacy"} {
			cls, ok := classes[name].(map[string]any)
			if !ok || len(cls) == 0 {
				continue
			}
			usable, _ := settingsNumber(cls, "usable")
			cAvail, _ := settingsNumber(cls, "creditsAvailable")
			cUsed, _ := settingsNumber(cls, "creditsUsed")
			cLimit, _ := settingsNumber(cls, "creditsLimit")
			cOut, _ := settingsNumber(cls, "outstandingCredits")
			cActive, _ := settingsNumber(cls, "activeKeys")
			cUsedCr, _ := settingsNumber(cls, "usedCredits")
			cQuota, _ := settingsNumber(cls, "quotaCredits")
			sc := cAvail - cOut
			label := strings.ToUpper(name[:1]) + name[1:]
			lines = append(lines, headroomIcon(sc)+" <b>"+label+"</b>")
			lines = append(lines, "Accounts: "+fmtCredits(usable)+" usable · "+fmtCredits(cAvail)+" avail ("+
				fmtCredits(cUsed)+" used / "+fmtCredits(cLimit)+" cap)")
			lines = append(lines, "Sold: "+fmtCredits(cOut)+" outstanding ("+fmtCredits(cActive)+" keys, "+
				fmtCredits(cUsedCr)+" used / "+fmtCredits(cQuota)+" quota)")
			scLine := headroomIcon(sc) + " Sellable: " + fmtCredits(sc) + " credits"
			if sc < 0 {
				scLine += " — OVERSOLD by " + fmtCredits(-sc)
			}
			lines = append(lines, scLine)
		}
	}

	return strings.Join(lines, "\n")
}

func headroomIcon(v float64) string {
	switch {
	case v < 0:
		return "🔴"
	case v > 0:
		return "🟢"
	default:
		return "🟠"
	}
}

func onOffWord(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func fmtCredits(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// ============================================================================
// §8 adm:audit — Audit log. Cap: audit (IS in DEFAULT_STD_CAPS).
// ============================================================================

func (b *Bot) cbAdmAudit(cc *cbCtx) {
	b.sendAuditLog(cc.Ctx, cc.ChatID, "")
}

// sendAuditLog is _send_audit (§8.2), default limit 25 (distinct from
// store.RecentAudit's own internal default — this admin viewer always passes
// 25 explicitly).
func (b *Bot) sendAuditLog(ctx context.Context, chatID int64, query string) {
	const limit = 25
	head := "📜 AUDIT LOG"
	if query != "" {
		head += " · filter '" + query + "'"
	}
	if b.store == nil {
		b.send(ctx, chatID, head+" (last 0)\n\n(no matching entries)", admNav("admroot"))
		return
	}
	rows, err := b.store.RecentAudit(limit, query)
	if err != nil {
		b.send(ctx, chatID, "⚠️ Could not read the audit log.", admNav("admroot"))
		return
	}
	head += " (last " + itoa(len(rows)) + ")"
	if len(rows) == 0 {
		b.send(ctx, chatID, head+"\n\n(no matching entries)", admNav("admroot"))
		return
	}
	entries := make([]string, 0, len(rows))
	for _, r := range rows {
		entry := fmtWhen(r.TS, i18n.EN) + " · " + auditEmoji(r.ActorKind) + " " + r.ActorKind + " " + r.ActorChat + "\n" +
			"  " + r.Action + " → " + r.Target
		if r.Outcome != "" {
			entry += "\n  ⮑ " + r.Outcome
		}
		entries = append(entries, entry)
	}
	text := head + "\n\n" + strings.Join(entries, "\n\n")
	if runes := []rune(text); len(runes) > 3900 {
		text = string(runes[:3900]) + "\n…(truncated — narrow with /audit <query>)"
	}
	b.send(ctx, chatID, text, admNav("admroot"))
}

func auditEmoji(kind string) string {
	switch kind {
	case "customer":
		return "🧑"
	case "reseller":
		return "🤝"
	case "admin":
		return "🛡"
	default:
		return "•"
	}
}
