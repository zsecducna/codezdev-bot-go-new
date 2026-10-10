package handlers

// apikey_name_test.go — the admin-renameable API-key SKU DISPLAY name
// (settings.json `apikey_names`, apikeyProductName in apikey.go plus the
// adm:apiname / adm:kironame wizards in adminpricing.go).
//
// The load-bearing test here is TestApikeyDefaultNamesRenderByteIdentical: a
// feature whose whole promise is "nothing changes until an admin renames
// something" is only trustworthy if the DEFAULT rendering is provably
// unchanged, so the pre-change catalog strings are hardcoded below and
// compared byte-for-byte against what the live templates render. Asserting
// against the catalog itself would be circular — it would pass no matter how
// the catalog drifted.

import (
	"context"
	"html"
	"slices"
	"strings"
	"testing"

	"codezdev-bot/internal/i18n"
)

// nameRenderArgs are the non-{name} placeholder values used by the
// byte-identity table: single-character sentinels so the expected strings stay
// readable and any accidental substitution shows up immediately. i18n.F is a
// single-pass strings.Replacer, so passing a token a template does not carry
// (e.g. {opus} to KiroPrompt) is a no-op.
var nameRenderArgs = []string{
	"rate", "R", "min", "M", "balance", "B", "opus", "O",
	"credits", "C", "total", "T", "key", "K", "base", "U",
}

// preChangeBtn … preChangeKiroDelivered are the catalog values EXACTLY as they
// were before {name} was introduced, product name literal included. They must
// never be "refreshed" from the catalog — the moment they are, this file stops
// guarding anything.
var (
	preChangeBtn = i18n.Str{
		EN: "🔑 Bedrock API Credits — {rate}/100 credits",
		VI: "🔑 Bedrock API Credits — {rate}/100 credits",
		ZH: "🔑 Bedrock API Credits — {rate}/100 credits",
	}
	preChangePrompt = i18n.Str{
		EN: "🔑 BUY BEDROCK API CREDITS\nPrice: {rate} / 100 credits\nMinimum: {min} credits\nYour balance: {balance}\n{opus}\nType how many credits you want (e.g. 7500):",
		VI: "🔑 MUA BEDROCK API CREDITS\nGiá: {rate} / 100 credits\nTối thiểu: {min} credits\nSố dư của bạn: {balance}\n{opus}\nNhập số credits bạn muốn mua (ví dụ 7500):",
		ZH: "🔑 购买 BEDROCK API CREDITS\n价格：{rate} / 100 credits\n最低：{min} credits\n您的余额：{balance}\n{opus}\n请输入要购买的 credits 数量（例如 7500）：",
	}
	preChangePayPrompt = i18n.Str{
		EN: "🧾 🔑 Bedrock API Credits — {credits} credits\nPrice: {rate} / 100 credits\nTotal: {total}\n\nYour balance: {balance}\nChoose a payment method:",
		VI: "🧾 🔑 Bedrock API Credits — {credits} credits\nĐơn giá: {rate} / 100 credits\nThành tiền: {total}\n\nSố dư của bạn: {balance}\nChọn cách thanh toán:",
		ZH: "🧾 🔑 Bedrock API Credits — {credits} credits\n价格：{rate} / 100 credits\n总计：{total}\n\n您的余额：{balance}\n请选择支付方式：",
	}
	preChangeDelivered = i18n.Str{
		EN: "✅ Your Bedrock API Credits are ready — {credits} credits\n\n🔑 Your API key (tap to copy):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Send it as header: <code>Authorization: Bearer &lt;your key&gt;</code>\n\n👉 <b>How to use it</b>\n📊 Dashboard — track your quota:\n{base}/\n📖 API docs + client setup (Claude Code, Codex, curl):\n{base}/docs\n🧠 Models &amp; pricing:\n{base}/models\n\n⚠️ Keep this key secret — anyone holding it can spend your credits. You can view it again anytime: Self-Service → 🔑 My API Keys.",
		VI: "✅ Bedrock API Credits của bạn đã sẵn sàng — {credits} credits\n\n🔑 API key của bạn (chạm để sao chép):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Gửi kèm header: <code>Authorization: Bearer &lt;key của bạn&gt;</code>\n\n👉 <b>Hướng dẫn sử dụng</b>\n📊 Bảng điều khiển — theo dõi hạn mức:\n{base}/\n📖 Tài liệu API + cách cài đặt (Claude Code, Codex, curl):\n{base}/docs\n🧠 Danh sách model &amp; bảng giá:\n{base}/models\n\n⚠️ Giữ kín key này — ai có nó là dùng được credits của bạn. Xem lại bất cứ lúc nào: Tự phục vụ → 🔑 API key của tôi.",
		ZH: "✅ 您的 Bedrock API Credits 已就绪 — {credits} credits\n\n🔑 您的 API key（点击复制）：\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 请求头：<code>Authorization: Bearer &lt;您的 key&gt;</code>\n\n👉 <b>使用指南</b>\n📊 控制台 — 查询余量：\n{base}/\n📖 API 文档 + 客户端配置（Claude Code、Codex、curl）：\n{base}/docs\n🧠 模型与价格：\n{base}/models\n\n⚠️ 请妥善保管此 key — 任何持有者都能消耗您的 credits。随时可再次查看：自助服务 → 🔑 我的 API Key。",
	}
	preChangeKiroBtn = i18n.Str{
		EN: "🟣 Kiro Credits API — {rate}/100 Kiro credits",
		VI: "🟣 Kiro Credits API — {rate}/100 Kiro credits",
		ZH: "🟣 Kiro Credits API — {rate}/100 Kiro credits",
	}
	preChangeKiroPrompt = i18n.Str{
		EN: "🟣 BUY KIRO CREDITS API KEY\nPrice: {rate} / 100 Kiro credits\nMinimum: {min} credits\nYour balance: {balance}\n\nWARRANTY: NO WARRANTY\n\nType how many Kiro credits you want (e.g. 7500):",
		VI: "🟣 MUA KIRO CREDITS API KEY\nGiá: {rate} / 100 Kiro credits\nTối thiểu: {min} credits\nSố dư của bạn: {balance}\n\nBẢO HÀNH: KHÔNG BẢO HÀNH\n\nNhập số Kiro credits bạn muốn mua (ví dụ 7500):",
		ZH: "🟣 购买 KIRO CREDITS API KEY\n价格：{rate} / 100 Kiro credits\n最低：{min} credits\n您的余额：{balance}\n\n保修：无保修\n\n请输入要购买的 Kiro credits 数量（例如 7500）：",
	}
	preChangeKiroPayPrompt = i18n.Str{
		EN: "🧾 🟣 Kiro Credits API — {credits} Kiro credits\nPrice: {rate} / 100 credits\nTotal: {total}\n\nYour balance: {balance}\nChoose a payment method:",
		VI: "🧾 🟣 Kiro Credits API — {credits} Kiro credits\nĐơn giá: {rate} / 100 credits\nThành tiền: {total}\n\nSố dư của bạn: {balance}\nChọn cách thanh toán:",
		ZH: "🧾 🟣 Kiro Credits API — {credits} Kiro credits\n价格：{rate} / 100 credits\n总计：{total}\n\n您的余额：{balance}\n请选择支付方式：",
	}
	preChangeKiroDelivered = i18n.Str{
		EN: "✅ Your Kiro Credits API key is ready — {credits} Kiro credits\nWARRANTY: NO WARRANTY\nPlease be aware that the Kiro Credits API key depends on the Kiro accounts source and it could be not stable and no warranty. In return, you will have good pricing.\n\n🔑 Your API key (tap to copy):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Send it as header: <code>Authorization: Bearer &lt;your key&gt;</code>\n\n👉 <b>How to use it</b>\n📊 Dashboard — track your quota:\n{base}/\n📖 API docs + client setup (Claude Code, Codex, curl):\n{base}/docs\n🧠 Models &amp; pricing:\n{base}/models\n\n⚠️ Keep this key secret — anyone holding it can spend your credits. You can view it again anytime: Self-Service → 🔑 My API Keys.",
		VI: "✅ Kiro Credits API key của bạn đã sẵn sàng — {credits} Kiro credits\nBẢO HÀNH: KHÔNG BẢO HÀNH\nLưu ý: Kiro Credits API key phụ thuộc vào nguồn tài khoản Kiro nên có thể không ổn định và không được bảo hành. Đổi lại, bạn có mức giá tốt.\n\n🔑 API key của bạn (chạm để sao chép):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Gửi kèm header: <code>Authorization: Bearer &lt;key của bạn&gt;</code>\n\n👉 <b>Hướng dẫn sử dụng</b>\n📊 Bảng điều khiển — theo dõi hạn mức:\n{base}/\n📖 Tài liệu API + cách cài đặt (Claude Code, Codex, curl):\n{base}/docs\n🧠 Danh sách model &amp; bảng giá:\n{base}/models\n\n⚠️ Giữ kín key này — ai có nó là dùng được credits của bạn. Xem lại bất cứ lúc nào: Tự phục vụ → 🔑 API key của tôi.",
		ZH: "✅ 您的 Kiro Credits API key 已就绪 — {credits} Kiro credits\n保修：无保修\n请注意：Kiro Credits API key 依赖 Kiro 账号来源，可能不稳定且无保修。作为回报，您将获得优惠的价格。\n\n🔑 您的 API key（点击复制）：\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 请求头：<code>Authorization: Bearer &lt;您的 key&gt;</code>\n\n👉 <b>使用指南</b>\n📊 控制台 — 查询余量：\n{base}/\n📖 API 文档 + 客户端配置（Claude Code、Codex、curl）：\n{base}/docs\n🧠 模型与价格：\n{base}/models\n\n⚠️ 请妥善保管此 key — 任何持有者都能消耗您的 credits。随时可再次查看：自助服务 → 🔑 我的 API Key。",
	}
)

// TestApikeyDefaultNamesRenderByteIdentical: with NO apikey_names override
// configured, every renamed template renders byte-for-byte what it rendered
// before the rename feature existed — in all three languages. The name
// argument is supplied exactly the way the real call sites supply it
// (uppercased for the two all-caps prompt headers, HTML-escaped for the two
// delivery messages, plain elsewhere), so a call site that picked the wrong
// transform would also show up here.
func TestApikeyDefaultNamesRenderByteIdentical(t *testing.T) {
	// No config wired at all: apikeyProductName must be nil-map safe and hand
	// back the built-in defaults — the same state a fresh install is in.
	b, _ := newTestBot("1", nil)
	c := &i18n.C().APIKey

	bedrock, kiro := b.apikeyProductName(saleClassBedrock), b.apikeyProductName(saleClassKiro)
	cases := []struct {
		field   string
		live    i18n.Str
		want    i18n.Str
		nameArg string
	}{
		{"Btn", c.Btn, preChangeBtn, bedrock},
		{"Prompt", c.Prompt, preChangePrompt, strings.ToUpper(bedrock)},
		{"PayPrompt", c.PayPrompt, preChangePayPrompt, bedrock},
		{"Delivered", c.Delivered, preChangeDelivered, html.EscapeString(bedrock)},
		{"KiroBtn", c.KiroBtn, preChangeKiroBtn, kiro},
		{"KiroPrompt", c.KiroPrompt, preChangeKiroPrompt, strings.ToUpper(kiro)},
		{"KiroPayPrompt", c.KiroPayPrompt, preChangeKiroPayPrompt, kiro},
		{"KiroDelivered", c.KiroDelivered, preChangeKiroDelivered, html.EscapeString(kiro)},
	}
	for _, tc := range cases {
		for _, lang := range []i18n.Lang{i18n.EN, i18n.VI, i18n.ZH} {
			// Clone before appending: nameRenderArgs is package-level, and an
			// append with spare capacity would let one iteration overwrite the
			// next one's argument.
			args := append(slices.Clone(nameRenderArgs), "name", tc.nameArg)
			got := i18n.F(tc.live.T(lang), args...)
			want := i18n.F(tc.want.T(lang), nameRenderArgs...)
			if got != want {
				t.Errorf("%s[%s] rendered\n  got  %q\n  want %q", tc.field, lang, got, want)
			}
		}
	}
}

// TestApikeyProductNameFallbacks pins every "fall back to the built-in
// default" branch of the helper: absent map, absent SKU entry, a wrong-typed
// map (hand-edited settings.json), and a value that is empty after trimming.
// Legacy rows (sale_class "") resolve to the BEDROCK entry, override included
// — every render site collapses "not kiro" onto the bedrock template, so the
// name has to follow.
func TestApikeyProductNameFallbacks(t *testing.T) {
	nilCfg, _ := newTestBot("1", nil)
	if got := nilCfg.apikeyProductName(saleClassBedrock); got != apikeyDefaultNameBedrock {
		t.Errorf("nil-config bedrock name = %q, want %q", got, apikeyDefaultNameBedrock)
	}
	if got := nilCfg.apikeyProductName(saleClassKiro); got != apikeyDefaultNameKiro {
		t.Errorf("nil-config kiro name = %q, want %q", got, apikeyDefaultNameKiro)
	}
	if got := nilCfg.apikeyProductName(saleClassFixed); got != apikeyDefaultNameFixed {
		t.Errorf("nil-config fixed name = %q, want %q", got, apikeyDefaultNameFixed)
	}

	b, _, _, _, _, dir := newAPIKeyBot(t)
	fkSettings := func(names any) {
		s := map[string]any{"apikey_usdt_per_100": 4.0}
		if names != nil {
			s["apikey_names"] = names
		}
		writeSettings(t, dir, s)
	}
	cases := []struct {
		name                         string
		names                        any
		wantBed, wantKiro, wantFixed string
	}{
		{"absent map", nil, apikeyDefaultNameBedrock, apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"wrong type", "not-a-map", apikeyDefaultNameBedrock, apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"empty map", map[string]any{}, apikeyDefaultNameBedrock, apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"blank value", map[string]any{"bedrock": "   ", "kiro": "", "fixed": " "}, apikeyDefaultNameBedrock, apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		// Hand-edited settings.json bypasses the admin wizard's validation, so
		// the READ path has to reject the same shapes the wizard does:
		// a newline would break the inline-button layout, an unbounded length
		// would push the price off the end of the buy-menu row.
		{"embedded newline", map[string]any{"bedrock": "Evil\nName", "kiro": "Bad\rName", "fixed": "Also\nBad"}, apikeyDefaultNameBedrock, apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"over-long", map[string]any{"bedrock": strings.Repeat("x", apikeyNameMaxRunes+1)}, apikeyDefaultNameBedrock, apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"exactly at cap", map[string]any{"bedrock": strings.Repeat("x", apikeyNameMaxRunes)}, strings.Repeat("x", apikeyNameMaxRunes), apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"bedrock only", map[string]any{"bedrock": "Shiny Credits"}, "Shiny Credits", apikeyDefaultNameKiro, apikeyDefaultNameFixed},
		{"kiro only", map[string]any{"kiro": "Purple Keys"}, apikeyDefaultNameBedrock, "Purple Keys", apikeyDefaultNameFixed},
		{"fixed only", map[string]any{"fixed": "Flat Rate Keys"}, apikeyDefaultNameBedrock, apikeyDefaultNameKiro, "Flat Rate Keys"},
		{"trimmed", map[string]any{"bedrock": "  Padded Name  "}, "Padded Name", apikeyDefaultNameKiro, apikeyDefaultNameFixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fkSettings(tc.names)
			if got := b.apikeyProductName(saleClassBedrock); got != tc.wantBed {
				t.Errorf("bedrock name = %q, want %q", got, tc.wantBed)
			}
			// Legacy '' rows must track the bedrock entry, override included.
			if got := b.apikeyProductName(""); got != tc.wantBed {
				t.Errorf("legacy('') name = %q, want the bedrock name %q", got, tc.wantBed)
			}
			if got := b.apikeyProductName(saleClassKiro); got != tc.wantKiro {
				t.Errorf("kiro name = %q, want %q", got, tc.wantKiro)
			}
			if got := b.apikeyProductName(saleClassFixed); got != tc.wantFixed {
				t.Errorf("fixed name = %q, want %q", got, tc.wantFixed)
			}
		})
	}
}

// TestApikeyRenameReachesCustomerScreens drives the REAL customer paths (buy
// menu, purchase prompt, pay screen, delivery, re-reveal) with only the
// bedrock SKU renamed and asserts (a) the custom name shows up, (b) the kiro
// SKU is untouched, and (c) no literal "{name}" leaks into any of them — the
// failure mode of a call site that forgot to pass the argument.
func TestApikeyRenameReachesCustomerScreens(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, dir := newAPIKeyBot(t)
	s := apikeySettings(fk, fl)
	s["kiro_apikey_enabled"] = true
	s["kiro_apikey_usdt_per_100"] = 1.0
	s["apikey_names"] = map[string]any{"bedrock": "Nebula Credits"}
	writeSettings(t, dir, s)
	const chat = 100

	// (1) Buy-menu rows: the renamed bedrock row, the untouched kiro row.
	// Only bedrock and kiro are priced here (the fixed SKU stays OFF), so the
	// row order below is bedrock, kiro — index-addressed on purpose so a SKU
	// rendering under another SKU's row would fail.
	rows, sellable := b.apikeyBuyRows(chat, i18n.EN)
	if !sellable || len(rows) < 2 {
		t.Fatalf("buy rows = %v (sellable=%v), want both priced SKU rows", rows, sellable)
	}
	if got := rows[0][0].Text; !strings.Contains(got, "Nebula Credits") || strings.Contains(got, "Bedrock API Credits") {
		t.Errorf("bedrock buy row = %q, want the renamed SKU", got)
	}
	if got := rows[1][0].Text; !strings.Contains(got, apikeyDefaultNameKiro) {
		t.Errorf("kiro buy row = %q, want the untouched kiro name", got)
	}

	// (2) Purchase prompt: renamed, UPPERCASED (it is an all-caps header).
	b.Process(ctx, callbackUpdate(chat, "apikey:start"))
	prompt := lastMsg(t, f)
	if !strings.Contains(prompt.Text, "BUY NEBULA CREDITS") {
		t.Errorf("bedrock prompt = %q, want the uppercased renamed header", prompt.Text)
	}
	b.Process(ctx, callbackUpdate(chat, "kiroapi:start"))
	if kp := lastMsg(t, f); !strings.Contains(kp.Text, "BUY KIRO CREDITS API KEY") {
		t.Errorf("kiro prompt = %q, want the untouched kiro header", kp.Text)
	}

	// (3) Pay screen + (4) delivery + (5) re-reveal, on a real bedrock order.
	const des = "0f0f0000f0f00000f0f00000f0f00000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")
	if pay := b.apikeyPayScreenText(o, i18n.EN, 0); !strings.Contains(pay, "Nebula Credits") {
		t.Errorf("pay screen = %q, want the renamed SKU", pay)
	}
	f.Sends = nil
	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("delivery must succeed")
	}
	delivered := lastSendMsgTo(f, chat)
	if delivered == nil || !strings.Contains(delivered.Text, "Your Nebula Credits are ready") {
		t.Fatalf("delivery message = %v, want the renamed SKU", delivered)
	}
	// The owner sale alert carries the renamed label too.
	if alert := lastSendMsgTo(f, 999); alert == nil || !strings.Contains(alert.Text, "Nebula Credits") {
		t.Errorf("owner sale alert = %v, want the renamed SKU label", alert)
	}
	f.Sends = nil
	b.Process(ctx, callbackUpdate(chat, "apikeyrev:"+des))
	reveal := lastSendMsgTo(f, chat)
	if reveal == nil || !strings.Contains(reveal.Text, "Your Nebula Credits are ready") {
		t.Fatalf("re-reveal message = %v, want the renamed SKU", reveal)
	}

	// (6) No screen anywhere may leak the raw placeholder.
	for _, m := range f.Sends {
		if strings.Contains(m.Text, "{name}") {
			t.Errorf("message leaked the raw {name} placeholder: %q", m.Text)
		}
	}
}

// TestApikeyDeliveredEscapesHostileName: the delivery message goes out with
// parse_mode=HTML, so an admin-typed name containing HTML metacharacters must
// arrive escaped. Unescaped, Telegram rejects the whole message and the buyer
// never receives the key they just paid for.
func TestApikeyDeliveredEscapesHostileName(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, dir := newAPIKeyBot(t)
	s := apikeySettings(fk, fl)
	s["apikey_names"] = map[string]any{"bedrock": "A & B <x>"}
	writeSettings(t, dir, s)

	const chat, des = 100, "0e0e0000e0e00000e0e00000e0e00000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")
	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("delivery must succeed")
	}
	msg := lastSendMsgTo(f, chat)
	if msg == nil {
		t.Fatal("no delivery message")
	}
	if !strings.Contains(msg.Text, "A &amp; B &lt;x&gt;") {
		t.Errorf("delivery message = %q, want the HTML-escaped name", msg.Text)
	}
	if strings.Contains(msg.Text, "A & B <x>") {
		t.Errorf("delivery message = %q, want NO raw HTML metacharacters from the name", msg.Text)
	}
}

// TestApikeyRenameNoDoubleEscapeOnPlainScreens is the counterpart to
// TestApikeyDeliveredEscapesHostileName: the buy-menu button and the pay
// screen are plain Telegram text (no parse_mode=HTML), so an admin-typed name
// containing "&" must render LITERALLY there — a call site that reused the
// delivery path's HTML-escaping helper on these plain screens would show the
// customer a garbled "A &amp; B" instead of the name they were actually sold.
func TestApikeyRenameNoDoubleEscapeOnPlainScreens(t *testing.T) {
	b, _, st, fk, fl, dir := newAPIKeyBot(t)
	s := apikeySettings(fk, fl)
	s["apikey_names"] = map[string]any{"bedrock": "A & B"}
	writeSettings(t, dir, s)
	const chat = 100

	rows, sellable := b.apikeyBuyRows(chat, i18n.EN)
	if !sellable || len(rows) == 0 {
		t.Fatalf("buy rows = %v (sellable=%v), want the bedrock row", rows, sellable)
	}
	if got := rows[0][0].Text; !strings.Contains(got, "A & B") || strings.Contains(got, "&amp;") {
		t.Errorf("buy-menu button = %q, want the literal name, not HTML-escaped", got)
	}

	const des = "0d0d0000d0d00000d0d00000d0d00000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")
	pay := b.apikeyPayScreenText(o, i18n.EN, 0)
	if !strings.Contains(pay, "A & B") || strings.Contains(pay, "&amp;") {
		t.Errorf("pay screen = %q, want the literal name, not HTML-escaped", pay)
	}
}

// TestApikeyRenameReachesKiroCustomerScreens mirrors
// TestApikeyRenameReachesCustomerScreens for the KIRO SKU — the bedrock test
// above never exercises the kiro branch of the pay/delivery/re-reveal
// templates, so a call site that hardcoded saleClassBedrock there would pass
// that test while silently never showing a kiro rename to a real buyer.
func TestApikeyRenameReachesKiroCustomerScreens(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, dir := newAPIKeyBot(t)
	s := apikeySettings(fk, fl)
	s["kiro_apikey_enabled"] = true
	s["kiro_apikey_usdt_per_100"] = 1.0
	s["apikey_names"] = map[string]any{"kiro": "Aurora Kiro Credits"}
	writeSettings(t, dir, s)
	const chat = 100

	// Pay screen on a real kiro order.
	const des = "0c0c0000c0c00000c0c00000c0c00000"
	o := seedOrder(t, st, des, chat, kiroApikeyProduct, 500, 500000, "paid")
	if pay := b.apikeyPayScreenText(o, i18n.EN, 0); !strings.Contains(pay, "Aurora Kiro Credits") {
		t.Errorf("kiro pay screen = %q, want the renamed SKU", pay)
	}

	// Delivery.
	f.Sends = nil
	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("kiro delivery must succeed")
	}
	delivered := lastSendMsgTo(f, chat)
	if delivered == nil || !strings.Contains(delivered.Text, "Your Aurora Kiro Credits key is ready") {
		t.Fatalf("kiro delivery message = %v, want the renamed SKU", delivered)
	}
	if alert := lastSendMsgTo(f, 999); alert == nil || !strings.Contains(alert.Text, "Aurora Kiro Credits") {
		t.Errorf("kiro owner sale alert = %v, want the renamed SKU label", alert)
	}

	// Re-reveal.
	f.Sends = nil
	b.Process(ctx, callbackUpdate(chat, "apikeyrev:"+des))
	reveal := lastSendMsgTo(f, chat)
	if reveal == nil || !strings.Contains(reveal.Text, "Your Aurora Kiro Credits key is ready") {
		t.Fatalf("kiro re-reveal message = %v, want the renamed SKU", reveal)
	}
}

// TestApikeyOwnerAlertUsesStoredRowClassNotOrderClass pins the owner sale
// alert to the STORED api_keys row's class (keyCls), not the order-derived
// saleClass. They normally agree, but a LEGACY row — sale_class "" (see
// APIKey.SaleClass doc, store/apikeys.go) — can be attached to a kiro-family
// order (e.g. a row minted before the bedrock/kiro split, or an unclassed
// echo from Kiro-Go). The delivery message the buyer actually receives is
// already keyed off the stored row (apikey_deliver.go's keyCls), so the
// owner alert must read off the SAME variable or the two can silently
// disagree about what was sold. The row is seeded directly (bypassing the
// mint call) so deliverApikey takes its "already minted, re-deliver" branch,
// which is the only path that reads the stored row instead of the order.
func TestApikeyOwnerAlertUsesStoredRowClassNotOrderClass(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _, _ := newAPIKeyBot(t)
	const chat, des = 100, "0b0b0000b0b00000b0b00000b0b00000"

	// Order is kiro-family (order-derived saleClass == "kiro")...
	o := seedOrder(t, st, des, chat, kiroApikeyProduct, 500, 500000, "paid")
	// ...but the stored row is unclassed — the legacy/pre-split state.
	if _, err := st.AddAPIKey(des, chat, "keyid-1", "sk-legacy-key", 500, "", ""); err != nil {
		t.Fatalf("seed api_keys row: %v", err)
	}

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("delivery must succeed on an already-minted row")
	}
	delivered := lastSendMsgTo(f, chat)
	if delivered == nil || !strings.Contains(delivered.Text, "Your Bedrock API Credits are ready") {
		t.Fatalf("delivery message = %v, want the BEDROCK template (unclassed row collapses to bedrock)", delivered)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, apikeyDefaultNameBedrock) {
		t.Errorf("owner sale alert = %v, want the BEDROCK label %q, matching the delivery template the buyer received", alert, apikeyDefaultNameBedrock)
	}
	if alert != nil && strings.Contains(alert.Text, apikeyDefaultNameKiro) {
		t.Errorf("owner sale alert = %q, must NOT carry the kiro label — that would be the order's saleClass leaking in instead of the stored row's class", alert.Text)
	}
}
