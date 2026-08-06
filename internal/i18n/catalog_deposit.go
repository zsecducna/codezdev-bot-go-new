package i18n

// catalog_deposit.go — the M2.3 string clusters: the deposit flow
// (DEPOSIT_QR / DEPOSIT_OK / DEPOSIT_METHOD / DEP_BTN_* / DEPOSIT_*_AMOUNT /
// QR_UNDERPAID / PURCHASE_QR / DEPOSIT_LABEL), the /info card (BALANCE +
// REFERRAL_INFO) and the /history views (HISTORY_* + STATUS_LABEL). Every value
// is copied verbatim from the cited Python constant so the tables stay
// byte-diffable against telegram_bot.py.
//
// Format-spec note: Python renders VND amounts with `{paid:,}` etc. (thousands
// separators). Go's i18n.F does plain `{token}` substitution with no format
// spec, so those placeholders are kept as bare `{paid}` / `{total}` / `{balance}`
// here and the handler pre-formats the number with grouping (groupVND) before
// substituting — same customer-visible output, and the `{token}` set stays
// identical across the three languages for the completeness test.

// DepositCatalog holds the deposit-flow strings (telegram_bot.py:421-489,
// 1490-1497). SePay/VietQR (QR / BtnQR / Underpaid / PurchaseQR) was removed by
// the USDT-only migration (S1) — the shop no longer accepts VND bank transfers.
type DepositCatalog struct {
	// OK — DEPOSIT_OK (telegram_bot.py:427): credited notice; {amount} {balance}.
	OK Str
	// Method — DEPOSIT_METHOD (telegram_bot.py:450): the funding-method picker header.
	Method Str
	// BtnPayPal / BtnBEP20 / BtnBinance / BtnTRON — DEP_BTN_*
	// (telegram_bot.py:457-460): per-method deposit picker buttons.
	BtnPayPal  Str
	BtnBEP20   Str
	BtnBinance Str
	BtnTRON    Str
	// PPAmount / BEP20Amount / TronAmount — DEPOSIT_PP_AMOUNT / _BEP20_AMOUNT /
	// _TRON_AMOUNT (telegram_bot.py:462-481): the "enter USDT amount" prompts; {min}.
	PPAmount    Str
	BEP20Amount Str
	TronAmount  Str
	// PPLink — DEPOSIT_PP_LINK (telegram_bot.py:482): the message carrying the
	// PayPal approval link for a WALLET top-up (dep:pp); {usdt} {url}. Distinct
	// from Buy.PaypalLink (an order pay), which promises account delivery.
	PPLink Str
	// BadAmount — DEPOSIT_BAD_AMOUNT (telegram_bot.py:487): amount out of bounds; {min}.
	BadAmount Str
	// Label — DEPOSIT_LABEL (telegram_bot.py:582): friendly wallet-top-up row label
	// in /history.
	Label Str
}

// InfoCatalog holds the /info card strings (telegram_bot.py:493-499, 1568-1572).
type InfoCatalog struct {
	// Balance — BALANCE (telegram_bot.py:493): the Customer Info card;
	// {name} {chat_id} {balance}.
	Balance Str
	// ReferralInfo — REFERRAL_INFO (telegram_bot.py:1568): referral section appended
	// to /info while the referral program is on; {code} {pct}.
	ReferralInfo Str
}

// HistoryCatalog holds the /history strings (telegram_bot.py:508-583, 721-727).
type HistoryCatalog struct {
	// Header / Empty — HISTORY_HEADER / HISTORY_EMPTY (telegram_bot.py:510/514).
	Header Str
	Empty  Str
	// RangeNote — HISTORY_RANGE_NOTE (telegram_bot.py:526): "5"/"all" note appended
	// to the detailed-view header. The "5" entry carries {n}; "all" carries none.
	RangeNote map[string]Str
	// BtnAll / BtnRecent — HISTORY_BTN_ALL / _RECENT (telegram_bot.py:530/531):
	// the detailed-view range toggle buttons.
	BtnAll    Str
	BtnRecent Str
	// SummaryHeader / SummaryLine / SummaryTotal / SummaryMore — the per-product
	// summary view (telegram_bot.py:535-547). SummaryLine {product} {n} {amount};
	// SummaryTotal {n} {amount}; SummaryMore {n}.
	SummaryHeader Str
	SummaryLine   Str
	SummaryTotal  Str
	SummaryMore   Str
	// BtnDetails / BtnSummary — HISTORY_BTN_DETAILS / _SUMMARY (telegram_bot.py:548/549):
	// summary<->detail toggle buttons.
	BtnDetails Str
	BtnSummary Str
	// RedlBtn / RedlSending / RedlNone — the per-order re-download strings
	// (telegram_bot.py:557-561). RedlBtn carries {label}.
	RedlBtn     Str
	RedlSending Str
	RedlNone    Str
	// Truncated — HISTORY_TRUNCATED (telegram_bot.py:566): note when the "show all"
	// list hit the char/row cap; {n}.
	Truncated Str
	// StatusLabel — STATUS_LABEL (telegram_bot.py:721): per-status history line prefix.
	StatusLabel map[string]Str
}

// buildDepositCatalog materializes the deposit strings — byte-verbatim from the
// cited Python constants (format specs dropped per the file note).
func buildDepositCatalog() DepositCatalog {
	return DepositCatalog{
		// DEPOSIT_OK (telegram_bot.py:427).
		OK: Str{
			VI: "✅ Nạp tiền thành công: +{amount}\nSố dư hiện tại: {balance}",
			EN: "✅ Deposit successful: +{amount}\nCurrent balance: {balance}",
			ZH: "✅ 充值成功：+{amount}\n当前余额：{balance}",
		},
		// DEPOSIT_METHOD (telegram_bot.py:450).
		Method: Str{
			VI: "💳 Chọn cách nạp tiền:",
			EN: "💳 Choose a deposit method:",
			ZH: "💳 请选择充值方式：",
		},
		// DEP_BTN_PAYPAL (telegram_bot.py:457).
		BtnPayPal: Str{VI: "🌍 PayPal", EN: "🌍 PayPal", ZH: "🌍 PayPal"},
		// DEP_BTN_BEP20 (telegram_bot.py:458).
		BtnBEP20: Str{VI: "USDT (BEP20)", EN: "USDT (BEP20)", ZH: "USDT (BEP20)"},
		// DEP_BTN_BINANCE (telegram_bot.py:459).
		BtnBinance: Str{VI: "🪙 Binance (USDT)", EN: "🪙 Binance (USDT)", ZH: "🪙 Binance (USDT)"},
		// DEP_BTN_TRON (telegram_bot.py:460).
		BtnTRON: Str{VI: "USDT (TRC20)", EN: "USDT (TRC20)", ZH: "USDT (TRC20)"},
		// DEPOSIT_PP_AMOUNT (telegram_bot.py:462).
		PPAmount: Str{
			VI: "🌍 Nhập số USDT muốn nạp, tối thiểu {min}:",
			EN: "🌍 Enter the amount of USDT to deposit, minimum {min}:",
			ZH: "🌍 请输入要充值的 USDT 金额，最低 {min}：",
		},
		// DEPOSIT_BEP20_AMOUNT (telegram_bot.py:469).
		BEP20Amount: Str{
			VI: "🟡 Nhập số USDT muốn nạp, tối thiểu {min}.\n⚠️ Lưu ý phí mạng của bạn (tùy nền tảng). Màn hình sau hiển thị số tiền chính xác.",
			EN: "🟡 Enter the amount of USDT to deposit, minimum {min}.\n⚠️ Be aware of your own network fee (varies by platform). The next screen shows the exact amount.",
			ZH: "🟡 请输入要充值的 USDT 金额，最低 {min}。\n⚠️ 请注意您自己的网络费（因平台而异）。下一屏显示准确金额。",
		},
		// DEPOSIT_TRON_AMOUNT (telegram_bot.py:476).
		TronAmount: Str{
			VI: "🔴 Nhập số USDT bạn sẽ nạp, tối thiểu {min}.\n⚠️ Lưu ý phí mạng của bạn (tùy nền tảng). Màn hình sau hiển thị số tiền chính xác cần gửi.",
			EN: "🔴 Enter the amount of USDT you will send, minimum {min}.\n⚠️ Be aware of your own network fee (varies by platform). The next screen shows the exact amount to send.",
			ZH: "🔴 请输入您将充值的 USDT 金额，最低 {min}。\n⚠️ 请注意您自己的网络费（因平台而异）。下一屏显示需要发送的准确金额。",
		},
		// DEPOSIT_PP_LINK (telegram_bot.py:482).
		PPLink: Str{
			VI: "🌍 Nhấn để nạp {usdt} USDT bằng PayPal:\n{url}\n\nSố dư sẽ được cộng sau khi thanh toán hoàn tất.",
			EN: "🌍 Tap to deposit {usdt} USDT with PayPal:\n{url}\n\nYour balance is credited once payment completes.",
			ZH: "🌍 点击使用 PayPal 充值 {usdt} USDT：\n{url}\n\n付款完成后将计入您的余额。",
		},
		// DEPOSIT_BAD_AMOUNT (telegram_bot.py:487).
		BadAmount: Str{
			VI: "⚠️ Số tiền không hợp lệ. Nhập một số ≥ {min}.",
			EN: "⚠️ Invalid amount. Enter a number ≥ {min}.",
			ZH: "⚠️ 金额无效。请输入 ≥ {min} 的数字。",
		},
		// DEPOSIT_LABEL (telegram_bot.py:582).
		Label: Str{VI: "💳 Nạp tiền ví", EN: "💳 Wallet top-up", ZH: "💳 钱包充值"},
	}
}

// buildInfoCatalog materializes the /info strings — byte-verbatim.
func buildInfoCatalog() InfoCatalog {
	return InfoCatalog{
		// BALANCE (telegram_bot.py:493).
		Balance: Str{
			VI: "👤 THÔNG TIN KHÁCH HÀNG\n────────────────────────\nTên: {name}\nMã khách hàng: {chat_id}\n💰 Số dư: {balance}",
			EN: "👤 CUSTOMER INFO\n────────────────────────\nName: {name}\nCustomer ID: {chat_id}\n💰 Balance: {balance}",
			ZH: "👤 客户信息\n────────────────────────\n姓名：{name}\n客户编号：{chat_id}\n💰 余额：{balance}",
		},
		// REFERRAL_INFO (telegram_bot.py:1568).
		ReferralInfo: Str{
			VI: "🎟️ Mã giới thiệu của bạn: {code}\nChia sẻ với bạn bè: mỗi khách dùng mã của bạn được giảm {pct}% (một lần cho mỗi khách, thay cho chiết khấu số lượng) và bạn nhận {pct}% mỗi đơn giới thiệu.",
			EN: "🎟️ Your referral code: {code}\nShare it with friends: each person who uses your code gets {pct}% off (once per customer, replaces the volume discount) and you earn {pct}% of each referred order.",
			ZH: "🎟️ 您的推荐码：{code}\n分享给好友：使用您推荐码的每位客户可减 {pct}%（每位客户限一次，替代批量折扣），您每笔推荐订单可获得 {pct}%。",
		},
	}
}

// buildHistoryCatalog materializes the /history strings — byte-verbatim.
func buildHistoryCatalog() HistoryCatalog {
	return HistoryCatalog{
		// HISTORY_HEADER (telegram_bot.py:510).
		Header: Str{VI: "🧾 Lịch sử đơn hàng của bạn:", EN: "🧾 Your order history:", ZH: "🧾 您的订单历史："},
		// HISTORY_EMPTY (telegram_bot.py:514).
		Empty: Str{VI: "🧾 Bạn chưa có đơn hàng nào.", EN: "🧾 You have no orders yet.", ZH: "🧾 您还没有任何订单。"},
		// HISTORY_RANGE_NOTE (telegram_bot.py:526).
		RangeNote: map[string]Str{
			"5":   {VI: "({n} đơn gần nhất)", EN: "(last {n} orders)", ZH: "（最近 {n} 笔订单）"},
			"all": {VI: "(tất cả)", EN: "(all)", ZH: "（全部）"},
		},
		// HISTORY_BTN_ALL / _RECENT (telegram_bot.py:530/531).
		BtnAll:    Str{VI: "📜 Xem tất cả", EN: "📜 Show all", ZH: "📜 显示全部"},
		BtnRecent: Str{VI: "🧾 5 đơn gần nhất", EN: "🧾 Last 5 orders", ZH: "🧾 最近 5 笔订单"},
		// HISTORY_SUMMARY_HEADER (telegram_bot.py:535).
		SummaryHeader: Str{VI: "📦 Đơn đã mua (theo sản phẩm):", EN: "📦 Your purchases (by product):", ZH: "📦 您的购买（按产品）："},
		// HISTORY_SUMMARY_LINE (telegram_bot.py:538).
		SummaryLine: Str{VI: "🏷 {product}: {n} tài khoản · {amount}", EN: "🏷 {product}: {n} accounts · {amount}", ZH: "🏷 {product}：{n} 个账号 · {amount}"},
		// HISTORY_SUMMARY_TOTAL (telegram_bot.py:541).
		SummaryTotal: Str{VI: "Σ {n} tài khoản · {amount}", EN: "Σ {n} accounts · {amount}", ZH: "Σ {n} 个账号 · {amount}"},
		// HISTORY_SUMMARY_MORE (telegram_bot.py:544).
		SummaryMore: Str{VI: "… và {n} sản phẩm khác", EN: "… and {n} more products", ZH: "… 及另外 {n} 个产品"},
		// HISTORY_BTN_DETAILS / _SUMMARY (telegram_bot.py:548/549).
		BtnDetails: Str{VI: "📄 Chi tiết đơn", EN: "📄 Order details", ZH: "📄 订单明细"},
		BtnSummary: Str{VI: "📦 Tổng hợp", EN: "📦 Summary", ZH: "📦 汇总"},
		// HISTORY_REDL_BTN / _SENDING / _NONE (telegram_bot.py:557-561).
		RedlBtn:     Str{VI: "📥 Tải lại: {label}", EN: "📥 Re-download: {label}", ZH: "📥 重新下载：{label}"},
		RedlSending: Str{VI: "📤 Đang gửi lại file đơn hàng của bạn…", EN: "📤 Resending your order file…", ZH: "📤 正在重新发送您的订单文件…"},
		RedlNone:    Str{VI: "ℹ️ Không tìm thấy file cho đơn này. Vui lòng liên hệ hỗ trợ.", EN: "ℹ️ No file found for this order. Please contact support.", ZH: "ℹ️ 未找到此订单的文件，请联系客服。"},
		// HISTORY_TRUNCATED (telegram_bot.py:566).
		Truncated: Str{VI: "… chỉ hiển thị {n} đơn gần nhất.", EN: "… showing the {n} most recent only.", ZH: "… 仅显示最近 {n} 条。"},
		// STATUS_LABEL (telegram_bot.py:721).
		StatusLabel: map[string]Str{
			"delivered": {VI: "✅ Đã giao", EN: "✅ Delivered", ZH: "✅ 已交付"},
			"paid":      {VI: "💳 Đã thanh toán", EN: "💳 Paid", ZH: "💳 已付款"},
			"reserved":  {VI: "⏳ Chờ thanh toán", EN: "⏳ Awaiting payment", ZH: "⏳ 待付款"},
			"refunded":  {VI: "↩️ Đã hoàn tiền", EN: "↩️ Refunded", ZH: "↩️ 已退款"},
			"expired":   {VI: "⌛ Đã hết hạn", EN: "⌛ Expired", ZH: "⌛ 已过期"},
		},
	}
}
