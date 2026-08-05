package i18n

// catalog_apikey.go — the M2.5 API-key (metered Kiro-Go key) string cluster:
// both mint SKUs ("bedrock" __apikey__ / "kiro" __kiroapi__), the credit
// recharge product (__apikeyrc__), the Self-Service "My API Keys" view and the
// self-service group-refund preview/receipt.
//
// Field names mirror the Python constant names 1:1 (APIKEY_BTN -> BtnBedrock is
// NOT the convention; APIKEY_BTN -> Btn under an APIKey* namespace is) so the
// tables stay byte-diffable against telegram_bot.py. Every value below is
// copied verbatim from the cited Python constant, including emoji, spacing and
// the `{token}` placeholders i18n.F substitutes.

// APIKeyCatalog holds every API-key-family string (telegram_bot.py:3884-4211,
// 7110-7151, 1232-1251).
type APIKeyCatalog struct {
	// ---- bedrock SKU (__apikey__) ----

	// Btn — APIKEY_BTN (telegram_bot.py:3884): buy-menu row; {rate}.
	Btn Str
	// Prompt — APIKEY_PROMPT (telegram_bot.py:3888): credit-amount prompt;
	// {rate} {min} {balance} {opus}. {opus} is the Opus-5 token-value estimate
	// block (see apikeyOpusEstFor, apikey.go) — Kiro commit 3069329. Already
	// newline-terminated when present and EMPTY when it can't be computed, so
	// the surrounding layout holds either way. BEDROCK ONLY: the kiro credit is
	// a ~5.7x smaller unit, so the same estimate would overstate it by that
	// factor — KiroPrompt below carries no {opus} placeholder at all.
	Prompt Str
	// PayPrompt — APIKEY_PAY_PROMPT (telegram_bot.py:3917): pay screen;
	// {credits} {rate} {total} {balance}.
	PayPrompt Str
	// Delivered — APIKEY_DELIVERED (telegram_bot.py:3944): delivery/reveal
	// message (HTML); {credits} {key} {base}.
	Delivered Str
	// HistLabel — APIKEY_HIST_LABEL (telegram_bot.py:3992); {credits}.
	HistLabel Str
	// OpusEst — APIKEY_OPUS_EST (telegram_bot.py, Kiro commit 3069329): the
	// {opus} block itself, rendered into Prompt above. Two facts the buyer
	// actually shops on: how many Opus 5 tokens 1 USDT buys, and what 1M Opus 5
	// tokens costs; {tokens} {per1m}. Both are derived from the SAME
	// buyer-specific rate as the "Price:" line (apikeyOpusEstFor prices 100
	// credits through apikeyPriceFor, the same path apikeyRateDispFor uses), so
	// a reseller's discounted rate is reflected here too and the screen can
	// never self-contradict. Worded as an estimate in every language.
	OpusEst Str

	// ---- kiro SKU (__kiroapi__) ----

	// KiroBtn — KIROAPI_BTN (telegram_bot.py:4000); {rate}.
	KiroBtn Str
	// KiroPrompt — KIROAPI_PROMPT (telegram_bot.py:4006); {rate} {min} {balance}.
	KiroPrompt Str
	// KiroPayPrompt — KIROAPI_PAY_PROMPT (telegram_bot.py:4020).
	KiroPayPrompt Str
	// KiroDelivered — KIROAPI_DELIVERED (telegram_bot.py:4031).
	KiroDelivered Str
	// KiroHistLabel — KIROAPI_HIST_LABEL (telegram_bot.py:4078); {credits}.
	KiroHistLabel Str

	// ---- shared gates / errors ----

	// BadAmount — APIKEY_BAD_AMOUNT (telegram_bot.py:3899); {min} {max}.
	BadAmount Str
	// CustCap — APIKEY_CUST_CAP (telegram_bot.py:3905); {cap}.
	CustCap Str
	// Off — APIKEY_OFF (telegram_bot.py:3911).
	Off Str
	// MintFail — APIKEY_MINT_FAIL (telegram_bot.py:3981); {amount} {balance}.
	MintFail Str
	// SkuTag — APIKEY_SKU_TAG (telegram_bot.py:4085), keyed "bedrock"/"kiro".
	SkuTag map[string]Str
	// Status — APIKEY_STATUS (telegram_bot.py:4213), keyed by the Kiro-Go
	// /admin/stats "status" value (active|exhausted|disabled).
	Status map[string]Str

	// ---- recharge (__apikeyrc__) ----

	// RCBtn — APIKEY_RC_BTN (telegram_bot.py:4093).
	RCBtn Str
	// RCPick — APIKEY_RC_PICK (telegram_bot.py:4097): key-picker header.
	RCPick Str
	// RCNone — APIKEY_RC_NONE (telegram_bot.py:4101).
	RCNone Str
	// RCPrompt — APIKEY_RC_PROMPT (telegram_bot.py:4106);
	// {masked} {remaining} {rate} {min} {headroom} {balance}.
	RCPrompt Str
	// RCStatsNA — APIKEY_RC_STATS_NA (telegram_bot.py:4123): live stats
	// unreadable -> the per-key cap can't be verified, so recharge FAILS CLOSED.
	RCStatsNA Str
	// RCDisabled — APIKEY_RC_DISABLED (telegram_bot.py:4131).
	RCDisabled Str
	// RCFull — APIKEY_RC_FULL (telegram_bot.py:4137); {remaining} {cap}.
	RCFull Str
	// RCTooBig — APIKEY_RC_TOO_BIG (telegram_bot.py:4146); {headroom}.
	RCTooBig Str
	// RCPayPrompt — APIKEY_RC_PAY_PROMPT (telegram_bot.py:4153);
	// {credits} {masked} {rate} {total} {balance}.
	RCPayPrompt Str
	// RCDone — APIKEY_RC_DONE (telegram_bot.py:4166); {credits} {masked} {remaining}.
	RCDone Str
	// RCFail — APIKEY_RC_FAIL (telegram_bot.py:4179); {amount} {balance}.
	RCFail Str
	// RCHistLabel — APIKEY_RC_HIST_LABEL (telegram_bot.py:4190); {credits}.
	RCHistLabel Str

	// ---- Self-Service > My API Keys ----

	// MyKeysHeader / MyKeysEmpty / MyKeysStatsNA — MYKEYS_* (telegram_bot.py:4205-4211).
	MyKeysHeader  Str
	MyKeysEmpty   Str
	MyKeysStatsNA Str
	// MyKeysLineStats — MYKEYS_LINE_STATS (telegram_bot.py:4285);
	// {used} {bought} {left} {when}.
	MyKeysLineStats Str
	// MyKeysLineLocal — MYKEYS_LINE_LOCAL (telegram_bot.py:4288); {bought} {when}.
	MyKeysLineLocal Str

	// ---- self-service group refund ----

	// RefundPreview — APIKEY_REFUND_PREVIEW (telegram_bot.py:7110);
	// {des} {bought} {used} {remaining} {amount}.
	RefundPreview Str
	// RefundOKBtn / RefundNoBtn — APIKEY_REFUND_OK_BTN / _NO_BTN (7125-7126).
	RefundOKBtn Str
	RefundNoBtn Str
	// RefundNone — APIKEY_REFUND_NONE (telegram_bot.py:7128); {used} {bought}.
	RefundNone Str
	// RefundUnavail — APIKEY_REFUND_UNAVAIL (telegram_bot.py:7134).
	RefundUnavail Str
	// RefundCancelled — APIKEY_REFUND_CANCELLED (telegram_bot.py:7140).
	RefundCancelled Str
	// RefundRace — APIKEY_REFUND_RACE (telegram_bot.py:7147).
	RefundRace Str
	// RefundGroupNote — APIKEY_REFUND_GROUP_NOTE (telegram_bot.py:4196); {n}.
	RefundGroupNote Str
	// RefundCustomer — REFUND_CUSTOMER (telegram_bot.py:1232): the buyer's
	// refund receipt DM; {des} {tier} {date} {amount} {balance}. Lives here (not
	// in a refund cluster) because the API-key group refund is the only path
	// that sends it this slice; the generic customer-request refund (M2.6) and
	// the admin refund flows (M3.3) reuse it.
	RefundCustomer Str
}

// buildAPIKeyCatalog materializes the API-key strings verbatim from Python.
func buildAPIKeyCatalog() APIKeyCatalog {
	return APIKeyCatalog{
		// APIKEY_BTN (telegram_bot.py:3884).
		Btn: Str{
			EN: "🔑 Bedrock API Credits — {rate}/100 credits",
			VI: "🔑 Bedrock API Credits — {rate}/100 credits",
			ZH: "🔑 Bedrock API Credits — {rate}/100 credits",
		},
		// APIKEY_PROMPT (telegram_bot.py:3888, {opus} added by commit 3069329).
		Prompt: Str{
			EN: "🔑 BUY BEDROCK API CREDITS\nPrice: {rate} / 100 credits\nMinimum: {min} credits\nYour balance: {balance}\n{opus}\nType how many credits you want (e.g. 7500):",
			VI: "🔑 MUA BEDROCK API CREDITS\nGiá: {rate} / 100 credits\nTối thiểu: {min} credits\nSố dư của bạn: {balance}\n{opus}\nNhập số credits bạn muốn mua (ví dụ 7500):",
			ZH: "🔑 购买 BEDROCK API CREDITS\n价格：{rate} / 100 credits\n最低：{min} credits\n您的余额：{balance}\n{opus}\n请输入要购买的 credits 数量（例如 7500）：",
		},
		// APIKEY_PAY_PROMPT (telegram_bot.py:3917).
		PayPrompt: Str{
			EN: "🧾 🔑 Bedrock API Credits — {credits} credits\nPrice: {rate} / 100 credits\nTotal: {total}\n\nYour balance: {balance}\nChoose a payment method:",
			VI: "🧾 🔑 Bedrock API Credits — {credits} credits\nĐơn giá: {rate} / 100 credits\nThành tiền: {total}\n\nSố dư của bạn: {balance}\nChọn cách thanh toán:",
			ZH: "🧾 🔑 Bedrock API Credits — {credits} credits\n价格：{rate} / 100 credits\n总计：{total}\n\n您的余额：{balance}\n请选择支付方式：",
		},
		// APIKEY_DELIVERED (telegram_bot.py:3944) — HTML; {key}/{base} escaped
		// by the sender.
		Delivered: Str{
			EN: "✅ Your Bedrock API Credits are ready — {credits} credits\n\n🔑 Your API key (tap to copy):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Send it as header: <code>Authorization: Bearer &lt;your key&gt;</code>\n\n👉 <b>How to use it</b>\n📊 Dashboard — track your quota:\n{base}/\n📖 API docs + client setup (Claude Code, Codex, curl):\n{base}/docs\n🧠 Models &amp; pricing:\n{base}/models\n\n⚠️ Keep this key secret — anyone holding it can spend your credits. You can view it again anytime: Self-Service → 🔑 My API Keys.",
			VI: "✅ Bedrock API Credits của bạn đã sẵn sàng — {credits} credits\n\n🔑 API key của bạn (chạm để sao chép):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Gửi kèm header: <code>Authorization: Bearer &lt;key của bạn&gt;</code>\n\n👉 <b>Hướng dẫn sử dụng</b>\n📊 Bảng điều khiển — theo dõi hạn mức:\n{base}/\n📖 Tài liệu API + cách cài đặt (Claude Code, Codex, curl):\n{base}/docs\n🧠 Danh sách model &amp; bảng giá:\n{base}/models\n\n⚠️ Giữ kín key này — ai có nó là dùng được credits của bạn. Xem lại bất cứ lúc nào: Tự phục vụ → 🔑 API key của tôi.",
			ZH: "✅ 您的 Bedrock API Credits 已就绪 — {credits} credits\n\n🔑 您的 API key（点击复制）：\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 请求头：<code>Authorization: Bearer &lt;您的 key&gt;</code>\n\n👉 <b>使用指南</b>\n📊 控制台 — 查询余量：\n{base}/\n📖 API 文档 + 客户端配置（Claude Code、Codex、curl）：\n{base}/docs\n🧠 模型与价格：\n{base}/models\n\n⚠️ 请妥善保管此 key — 任何持有者都能消耗您的 credits。随时可再次查看：自助服务 → 🔑 我的 API Key。",
		},
		// APIKEY_HIST_LABEL (telegram_bot.py:3992).
		HistLabel: Str{
			EN: "🔑 API key — {credits} credits",
			VI: "🔑 API key — {credits} credits",
			ZH: "🔑 API key — {credits} credits",
		},
		// APIKEY_OPUS_EST (telegram_bot.py, Kiro commit 3069329).
		OpusEst: Str{
			EN: "\n📊 Opus 5 estimate: ≈ {tokens} tokens per 1 USDT\n     ≈ {per1m} USDT per 1M tokens (estimate only)\n",
			VI: "\n📊 Ước tính Opus 5: ≈ {tokens} token / 1 USDT\n     ≈ {per1m} USDT / 1 triệu token (chỉ là ước tính)\n",
			ZH: "\n📊 Opus 5 估算：每 1 USDT ≈ {tokens} tokens\n     每 100 万 tokens ≈ {per1m} USDT（仅供参考）\n",
		},
		// KIROAPI_BTN (telegram_bot.py:4000).
		KiroBtn: Str{
			EN: "🟣 Kiro Credits API — {rate}/100 Kiro credits",
			VI: "🟣 Kiro Credits API — {rate}/100 Kiro credits",
			ZH: "🟣 Kiro Credits API — {rate}/100 Kiro credits",
		},
		// KIROAPI_PROMPT (telegram_bot.py:4006) — states NO WARRANTY before pay.
		KiroPrompt: Str{
			EN: "🟣 BUY KIRO CREDITS API KEY\nPrice: {rate} / 100 Kiro credits\nMinimum: {min} credits\nYour balance: {balance}\n\nWARRANTY: NO WARRANTY\n\nType how many Kiro credits you want (e.g. 7500):",
			VI: "🟣 MUA KIRO CREDITS API KEY\nGiá: {rate} / 100 Kiro credits\nTối thiểu: {min} credits\nSố dư của bạn: {balance}\n\nBẢO HÀNH: KHÔNG BẢO HÀNH\n\nNhập số Kiro credits bạn muốn mua (ví dụ 7500):",
			ZH: "🟣 购买 KIRO CREDITS API KEY\n价格：{rate} / 100 Kiro credits\n最低：{min} credits\n您的余额：{balance}\n\n保修：无保修\n\n请输入要购买的 Kiro credits 数量（例如 7500）：",
		},
		// KIROAPI_PAY_PROMPT (telegram_bot.py:4020).
		KiroPayPrompt: Str{
			EN: "🧾 🟣 Kiro Credits API — {credits} Kiro credits\nPrice: {rate} / 100 credits\nTotal: {total}\n\nYour balance: {balance}\nChoose a payment method:",
			VI: "🧾 🟣 Kiro Credits API — {credits} Kiro credits\nĐơn giá: {rate} / 100 credits\nThành tiền: {total}\n\nSố dư của bạn: {balance}\nChọn cách thanh toán:",
			ZH: "🧾 🟣 Kiro Credits API — {credits} Kiro credits\n价格：{rate} / 100 credits\n总计：{total}\n\n您的余额：{balance}\n请选择支付方式：",
		},
		// KIROAPI_DELIVERED (telegram_bot.py:4031).
		KiroDelivered: Str{
			EN: "✅ Your Kiro Credits API key is ready — {credits} Kiro credits\nWARRANTY: NO WARRANTY\nPlease be aware that the Kiro Credits API key depends on the Kiro accounts source and it could be not stable and no warranty. In return, you will have good pricing.\n\n🔑 Your API key (tap to copy):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Send it as header: <code>Authorization: Bearer &lt;your key&gt;</code>\n\n👉 <b>How to use it</b>\n📊 Dashboard — track your quota:\n{base}/\n📖 API docs + client setup (Claude Code, Codex, curl):\n{base}/docs\n🧠 Models &amp; pricing:\n{base}/models\n\n⚠️ Keep this key secret — anyone holding it can spend your credits. You can view it again anytime: Self-Service → 🔑 My API Keys.",
			VI: "✅ Kiro Credits API key của bạn đã sẵn sàng — {credits} Kiro credits\nBẢO HÀNH: KHÔNG BẢO HÀNH\nLưu ý: Kiro Credits API key phụ thuộc vào nguồn tài khoản Kiro nên có thể không ổn định và không được bảo hành. Đổi lại, bạn có mức giá tốt.\n\n🔑 API key của bạn (chạm để sao chép):\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 Gửi kèm header: <code>Authorization: Bearer &lt;key của bạn&gt;</code>\n\n👉 <b>Hướng dẫn sử dụng</b>\n📊 Bảng điều khiển — theo dõi hạn mức:\n{base}/\n📖 Tài liệu API + cách cài đặt (Claude Code, Codex, curl):\n{base}/docs\n🧠 Danh sách model &amp; bảng giá:\n{base}/models\n\n⚠️ Giữ kín key này — ai có nó là dùng được credits của bạn. Xem lại bất cứ lúc nào: Tự phục vụ → 🔑 API key của tôi.",
			ZH: "✅ 您的 Kiro Credits API key 已就绪 — {credits} Kiro credits\n保修：无保修\n请注意：Kiro Credits API key 依赖 Kiro 账号来源，可能不稳定且无保修。作为回报，您将获得优惠的价格。\n\n🔑 您的 API key（点击复制）：\n<code>{key}</code>\n\n📍 Base URL: <code>{base}</code>\n🔐 请求头：<code>Authorization: Bearer &lt;您的 key&gt;</code>\n\n👉 <b>使用指南</b>\n📊 控制台 — 查询余量：\n{base}/\n📖 API 文档 + 客户端配置（Claude Code、Codex、curl）：\n{base}/docs\n🧠 模型与价格：\n{base}/models\n\n⚠️ 请妥善保管此 key — 任何持有者都能消耗您的 credits。随时可再次查看：自助服务 → 🔑 我的 API Key。",
		},
		// KIROAPI_HIST_LABEL (telegram_bot.py:4078).
		KiroHistLabel: Str{
			EN: "🟣 Kiro API key — {credits} Kiro credits",
			VI: "🟣 Kiro API key — {credits} Kiro credits",
			ZH: "🟣 Kiro API key — {credits} Kiro credits",
		},
		// APIKEY_BAD_AMOUNT (telegram_bot.py:3899).
		BadAmount: Str{
			EN: "⚠️ Invalid credit amount. Enter a whole number between {min} and {max}:",
			VI: "⚠️ Số credits không hợp lệ. Nhập một số nguyên từ {min} đến {max}:",
			ZH: "⚠️ credits 数量无效。请输入 {min} 到 {max} 之间的整数：",
		},
		// APIKEY_CUST_CAP (telegram_bot.py:3905).
		CustCap: Str{
			EN: "⛔ You've reached the limit of {cap} API key(s) per customer. Contact support if you need more.",
			VI: "⛔ Bạn đã đạt giới hạn {cap} API key mỗi khách. Liên hệ hỗ trợ nếu cần thêm.",
			ZH: "⛔ 您已达到每位客户 {cap} 个 API key 的上限。如需更多请联系客服。",
		},
		// APIKEY_OFF (telegram_bot.py:3911).
		Off: Str{
			EN: "⛔ API keys are not for sale right now.",
			VI: "⛔ API key hiện chưa mở bán.",
			ZH: "⛔ API key 暂未开售。",
		},
		// APIKEY_MINT_FAIL (telegram_bot.py:3981).
		MintFail: Str{
			EN: "⚠️ Payment received but the API key could not be created. {amount} was refunded to your balance (now {balance}). Please try again later or contact support.",
			VI: "⚠️ Đã nhận thanh toán nhưng chưa tạo được API key. {amount} đã được hoàn vào số dư của bạn (hiện {balance}). Vui lòng thử lại sau hoặc liên hệ hỗ trợ.",
			ZH: "⚠️ 已收到付款，但 API key 创建失败。{amount} 已退回您的余额（当前 {balance}）。请稍后重试或联系客服。",
		},
		// APIKEY_SKU_TAG (telegram_bot.py:4085).
		SkuTag: map[string]Str{
			"bedrock": {EN: "🔷 Bedrock", VI: "🔷 Bedrock", ZH: "🔷 Bedrock"},
			"kiro":    {EN: "🟣 Kiro", VI: "🟣 Kiro", ZH: "🟣 Kiro"},
		},
		// APIKEY_STATUS (telegram_bot.py:4213).
		Status: map[string]Str{
			"active":    {EN: "🟢 active", VI: "🟢 đang hoạt động", ZH: "🟢 有效"},
			"exhausted": {EN: "🔴 exhausted", VI: "🔴 hết hạn mức", ZH: "🔴 已用完"},
			"disabled":  {EN: "⛔ disabled", VI: "⛔ bị khóa", ZH: "⛔ 已停用"},
		},
		// APIKEY_RC_BTN (telegram_bot.py:4093).
		RCBtn: Str{
			EN: "🔁 Recharge API key credits",
			VI: "🔁 Nạp thêm credits cho API key",
			ZH: "🔁 为 API key 充值 credits",
		},
		// APIKEY_RC_PICK (telegram_bot.py:4097).
		RCPick: Str{
			EN: "🔁 Pick the API key to recharge:",
			VI: "🔁 Chọn API key muốn nạp thêm credits:",
			ZH: "🔁 选择要充值的 API key：",
		},
		// APIKEY_RC_NONE (telegram_bot.py:4101).
		RCNone: Str{
			EN: "You have no API key to recharge — buy one first.",
			VI: "Bạn chưa có API key nào để nạp — hãy mua một key trước.",
			ZH: "您没有可充值的 API key — 请先购买。",
		},
		// APIKEY_RC_PROMPT (telegram_bot.py:4106).
		RCPrompt: Str{
			EN: "🔁 RECHARGE API KEY\n🔑 Key: {masked}\nRemaining now: {remaining} credits\nPrice: {rate} / 100 credits\nMin: {min} — max: {headroom} credits\nYour balance: {balance}\n\nType how many credits to add (e.g. 2500):",
			VI: "🔁 NẠP THÊM CREDITS\n🔑 Key: {masked}\nCòn lại: {remaining} credits\nGiá: {rate} / 100 credits\nTối thiểu: {min} — tối đa: {headroom} credits\nSố dư của bạn: {balance}\n\nNhập số credits muốn nạp (ví dụ 2500):",
			ZH: "🔁 API KEY 充值\n🔑 Key：{masked}\n当前剩余：{remaining} credits\n价格：{rate} / 100 credits\n最少：{min} — 最多：{headroom} credits\n您的余额：{balance}\n\n请输入要充值的 credits 数量（例如 2500）：",
		},
		// APIKEY_RC_STATS_NA (telegram_bot.py:4123).
		RCStatsNA: Str{
			EN: "⚠️ Couldn't read the key's current quota. Please try again in a few minutes.",
			VI: "⚠️ Không đọc được hạn mức hiện tại của key. Vui lòng thử lại sau ít phút.",
			ZH: "⚠️ 无法读取该 key 的当前余量。请几分钟后重试。",
		},
		// APIKEY_RC_DISABLED (telegram_bot.py:4131).
		RCDisabled: Str{
			EN: "⛔ This key has been disabled and can't be recharged. Contact support.",
			VI: "⛔ Key này đã bị khóa và không thể nạp thêm. Liên hệ hỗ trợ.",
			ZH: "⛔ 该 key 已被停用，无法充值。请联系客服。",
		},
		// APIKEY_RC_FULL (telegram_bot.py:4137).
		RCFull: Str{
			EN: "⛔ This key still has {remaining} unused credits — it's at the {cap} credits-per-key cap, so nothing can be added right now.",
			VI: "⛔ Key này còn {remaining} credits chưa dùng — đã chạm giới hạn {cap} credits/key nên chưa thể nạp thêm.",
			ZH: "⛔ 该 key 尚有 {remaining} 未用 credits — 已达每个 key {cap} credits 的上限，暂时无法充值。",
		},
		// APIKEY_RC_TOO_BIG (telegram_bot.py:4146).
		RCTooBig: Str{
			EN: "⚠️ At most {headroom} credits can be added to this key. Enter a smaller amount:",
			VI: "⚠️ Chỉ có thể nạp tối đa {headroom} credits cho key này. Nhập số nhỏ hơn:",
			ZH: "⚠️ 该 key 最多还能充值 {headroom} credits。请输入更小的数量：",
		},
		// APIKEY_RC_PAY_PROMPT (telegram_bot.py:4153).
		RCPayPrompt: Str{
			EN: "🧾 🔁 Recharge {credits} credits into API key {masked}\nPrice: {rate} / 100 credits\nTotal: {total}\n\nYour balance: {balance}\nChoose a payment method:",
			VI: "🧾 🔁 Nạp {credits} credits vào API key {masked}\nĐơn giá: {rate} / 100 credits\nThành tiền: {total}\n\nSố dư của bạn: {balance}\nChọn cách thanh toán:",
			ZH: "🧾 🔁 为 API key {masked} 充值 {credits} credits\n价格：{rate} / 100 credits\n总计：{total}\n\n您的余额：{balance}\n请选择支付方式：",
		},
		// APIKEY_RC_DONE (telegram_bot.py:4166).
		RCDone: Str{
			EN: "✅ Recharged {credits} credits into API key {masked}.\nCredits remaining now: {remaining}.\nYour key is unchanged — keep using it as before.",
			VI: "✅ Đã nạp {credits} credits vào API key {masked}.\nHạn mức còn lại hiện tại: {remaining} credits.\nKey của bạn không đổi — tiếp tục dùng như cũ.",
			ZH: "✅ 已为 API key {masked} 充值 {credits} credits。\n当前剩余：{remaining} credits。\n您的 key 不变 — 继续照常使用。",
		},
		// APIKEY_RC_FAIL (telegram_bot.py:4179).
		RCFail: Str{
			EN: "⚠️ Payment received but the credit recharge failed. {amount} was refunded to your balance (now {balance}). Please try again later or contact support.",
			VI: "⚠️ Đã nhận thanh toán nhưng nạp credits thất bại. {amount} đã được hoàn vào số dư của bạn (hiện {balance}). Vui lòng thử lại sau hoặc liên hệ hỗ trợ.",
			ZH: "⚠️ 已收到付款，但 credits 充值失败。{amount} 已退回您的余额（当前 {balance}）。请稍后重试或联系客服。",
		},
		// APIKEY_RC_HIST_LABEL (telegram_bot.py:4190).
		RCHistLabel: Str{
			EN: "🔁 API key recharge — {credits} credits",
			VI: "🔁 Nạp API key — {credits} credits",
			ZH: "🔁 API key 充值 — {credits} credits",
		},
		// MYKEYS_HEADER / _EMPTY / _STATS_NA (telegram_bot.py:4205-4211).
		MyKeysHeader: Str{
			EN: "🔑 YOUR API KEYS",
			VI: "🔑 API KEY CỦA BẠN",
			ZH: "🔑 您的 API KEY",
		},
		MyKeysEmpty: Str{
			EN: "You don't have any API keys yet — buy one from the shop.",
			VI: "Bạn chưa có API key nào — mua trong mục Mua hàng.",
			ZH: "您还没有 API key — 请在商店购买。",
		},
		MyKeysStatsNA: Str{
			EN: "(live stats unavailable — showing stored info)",
			VI: "(không lấy được thống kê trực tiếp — hiển thị thông tin đã lưu)",
			ZH: "（无法获取实时用量 — 显示本地信息）",
		},
		// MYKEYS_LINE_STATS / _LOCAL (telegram_bot.py:4285-4290).
		MyKeysLineStats: Str{
			EN: "Used {used}/{bought} · {left} left · {when}",
			VI: "Đã dùng {used}/{bought} · còn {left} · {when}",
			ZH: "已用 {used}/{bought} · 剩余 {left} · {when}",
		},
		MyKeysLineLocal: Str{
			EN: "{bought} credits · {when}",
			VI: "{bought} credits · {when}",
			ZH: "{bought} credits · {when}",
		},
		// APIKEY_REFUND_PREVIEW (telegram_bot.py:7110).
		RefundPreview: Str{
			EN: "↩️ API KEY REFUND — order {des}\n🔑 Bought: {bought} credits · Used: {used} · Remaining: {remaining}\n💰 Refund (pro-rated by credits left): {amount}\n\nConfirm to credit your wallet now and REVOKE this key.",
			VI: "↩️ HOÀN TIỀN API KEY — đơn {des}\n🔑 Đã mua: {bought} credits · Đã dùng: {used} · Còn lại: {remaining}\n💰 Số tiền hoàn (theo credits còn lại): {amount}\n\nXác nhận sẽ CỘNG tiền vào ví của bạn ngay và VÔ HIỆU HOÁ key này.",
			ZH: "↩️ API 密钥退款 — 订单 {des}\n🔑 已购买：{bought} credits · 已使用：{used} · 剩余：{remaining}\n💰 退款金额（按剩余额度）：{amount}\n\n确认后将立即将金额存入您的钱包并注销此密钥。",
		},
		// APIKEY_REFUND_OK_BTN / _NO_BTN (telegram_bot.py:7125-7126).
		RefundOKBtn: Str{EN: "✅ Confirm refund", VI: "✅ Xác nhận hoàn tiền", ZH: "✅ 确认退款"},
		RefundNoBtn: Str{EN: "❌ Cancel", VI: "❌ Huỷ", ZH: "❌ 取消"},
		// APIKEY_REFUND_NONE (telegram_bot.py:7128).
		RefundNone: Str{
			EN: "ℹ️ This key is fully used ({used}/{bought} credits) — nothing to refund.",
			VI: "ℹ️ Key này đã dùng hết credits ({used}/{bought}) — không còn gì để hoàn.",
			ZH: "ℹ️ 此密钥额度已用完（{used}/{bought}）— 无可退款。",
		},
		// APIKEY_REFUND_UNAVAIL (telegram_bot.py:7134) — carries the support handle.
		RefundUnavail: Str{
			EN: "⚠️ Couldn't check this key's remaining credits right now. Please try again shortly or contact @zsec_ducna.",
			VI: "⚠️ Chưa kiểm tra được số credits còn lại lúc này. Vui lòng thử lại sau ít phút hoặc liên hệ @zsec_ducna.",
			ZH: "⚠️ 目前无法查询此密钥的剩余额度。请稍后重试或联系 @zsec_ducna。",
		},
		// APIKEY_REFUND_CANCELLED (telegram_bot.py:7140).
		RefundCancelled: Str{
			EN: "❌ Refund cancelled.",
			VI: "❌ Đã huỷ yêu cầu hoàn tiền.",
			ZH: "❌ 已取消退款。",
		},
		// APIKEY_REFUND_RACE (telegram_bot.py:7147).
		RefundRace: Str{
			EN: "ℹ️ This order has already been refunded or is no longer refundable.",
			VI: "ℹ️ Đơn này đã được hoàn tiền hoặc không còn ở trạng thái hoàn được.",
			ZH: "ℹ️ 此订单已退款或已不可退款。",
		},
		// APIKEY_REFUND_GROUP_NOTE (telegram_bot.py:4196).
		RefundGroupNote: Str{
			EN: "ℹ️ The refund covers this WHOLE key: the original order + {n} recharge(s) (all refunded together and the key is revoked).",
			VI: "ℹ️ Hoàn tiền áp dụng cho TOÀN BỘ key này: đơn gốc + {n} lần nạp (tất cả cùng được hoàn và key sẽ bị thu hồi).",
			ZH: "ℹ️ 退款覆盖整个 key：原始订单 + {n} 次充值（全部一并退款，key 将被吊销）。",
		},
		// REFUND_CUSTOMER (telegram_bot.py:1232) — carries the support handle.
		RefundCustomer: Str{
			EN: "✅ Your order has been refunded\n🧾 {des}\n🛡️ Tier: {tier}\n📅 Purchased: {date}\n💰 {amount} credited to your balance (balance: {balance}).\nThank you! Contact @zsec_ducna if needed.",
			VI: "✅ Đơn hàng của bạn đã được hoàn tiền\n🧾 {des}\n🛡️ Gói: {tier}\n📅 Ngày mua: {date}\n💰 Đã hoàn {amount} vào số dư (số dư: {balance}).\nCảm ơn bạn! Liên hệ @zsec_ducna nếu cần.",
			ZH: "✅ 您的订单已退款\n🧾 {des}\n🛡️ 方案：{tier}\n📅 购买日期：{date}\n💰 已退款 {amount} 至您的余额（余额：{balance}）。\n谢谢！如需帮助请联系 @zsec_ducna。",
		},
	}
}
