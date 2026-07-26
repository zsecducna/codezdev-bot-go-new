package i18n

// catalog_buy.go — the M2.2 (catalog browse + purchase pipeline) string cluster,
// the port of telegram_bot.py's PRODUCTS_HEADER / TIER_HEADER / VOLUME_PROMPT /
// PAY_* / PENDING_* / PURCHASE_OK constant regions (handlers-go.md §5 + §7 row
// M2.2). Grouped into a nested BuyCatalog struct so these strings live in their
// own file (the "dedicated catalog_<feature>.go" convention) while still being
// reachable from Catalog for the reflection-driven white-label rewrite AND the
// completeness test — every value is copied verbatim from the cited Python
// constant so the tables stay byte-diffable against the source.
type BuyCatalog struct {
	// ProductsHeader — PRODUCTS_HEADER (telegram_bot.py:729): the /buy product
	// list header (also reused as the header once any product/reseller row shows).
	ProductsHeader Str
	// NoProducts — NO_PRODUCTS (telegram_bot.py:742): empty product list.
	NoProducts Str
	// RateLimitProduct — RATE_LIMIT_PRODUCT (telegram_bot.py:737): the anti-spam
	// toast shown (as a callback alert, not a chat message) when a customer taps
	// product buttons too fast. {sec} = cooldown seconds left.
	RateLimitProduct Str
	// SalesPaused — SALES_PAUSED (telegram_bot.py:691): shown to a customer who
	// tries to buy while the whole shop's sales are paused.
	SalesPaused Str
	// ProductSuspended — PRODUCT_SUSPENDED (telegram_bot.py:701): shown when a
	// customer tries to buy a product an admin suspended (per-product pause).
	ProductSuspended Str
	// ResellerNote — RESELLER_NOTE (telegram_bot.py:714): header note above the
	// authorized-reseller buttons on the Buy screen.
	ResellerNote Str
	// TierHeader — TIER_HEADER (telegram_bot.py:756): header above the warranty
	// tier buttons; {product} {stock}.
	TierHeader Str
	// VolumePrompt — VOLUME_PROMPT (telegram_bot.py:768): the quantity prompt
	// after a tier is tapped; {product} {tier} {unit} {stock} {balance} {discounts}.
	VolumePrompt Str
	// BadQty — BAD_QTY (telegram_bot.py:792): invalid quantity typed.
	BadQty Str
	// QtyTooMany — QTY_TOO_MANY (telegram_bot.py:802): quantity above the admin
	// per-order cap; {max}.
	QtyTooMany Str
	// NoPrice — NO_PRICE (telegram_bot.py:808): product has no price configured.
	NoPrice Str
	// SoldOut — SOLD_OUT (telegram_bot.py:1211): could not reserve enough stock
	// (sold out between price and payment; no money taken).
	SoldOut Str
	// OutOfStock — OUT_OF_STOCK (telegram_bot.py:1217): tapped a fully out-of-stock
	// product.
	OutOfStock Str
	// ProductNoticeContinue — PRODUCT_NOTICE_CONTINUE (telegram_bot.py:4230): the
	// "✅ Continue" button on a product's buy-notice gate.
	ProductNoticeContinue Str
	// BackBtn — BACK_BTN (telegram_bot.py:4227): "Go back" button (tier menu ->
	// product list).
	BackBtn Str
	// TierNoWarranty / TierDaysWarranty / TierHoursWarranty — the three tier_label
	// branches (telegram_bot.py:4659-4672). The day/hour forms carry {n}.
	TierNoWarranty   Str
	TierDaysWarranty Str
	TierHoursWarranty Str
	// DiscountHeader / DiscountUnit — _DISCOUNT_HEADER / _DISCOUNT_UNIT
	// (telegram_bot.py:6513-6514): rendered by discount_block into VOLUME_PROMPT's
	// {discounts} slot.
	DiscountHeader Str
	DiscountUnit   Str
	// PayPrompt — PAY_PROMPT (telegram_bot.py:1294): the payment screen body;
	// {product} {unit} {qty} {pct} {total} {balance}.
	PayPrompt Str
	// PayBtn* — the per-method pay buttons (telegram_bot.py:1302-1310). PayBtnRmCode
	// carries {code}.
	PayBtnBalance   Str
	PayBtnQR        Str
	PayBtnPayPal    Str
	PayBtnBinance   Str
	PayBtnBEP20     Str
	PayBtnTRON      Str
	PayBtnApplyCode Str
	PayBtnRmCode    Str
	// PendingWarn — PENDING_WARN (telegram_bot.py:1275): the confirm-cancel prompt
	// shown when a customer starts a new purchase with one still awaiting payment;
	// {qty} {product} {total}.
	PendingWarn      Str
	PendingCancelBtn Str
	PendingKeepBtn   Str
	PendingCancelled Str
	PendingKept      Str
	// PurchaseOK / PurchaseOKToken — PURCHASE_OK / PURCHASE_OK_TOKEN
	// (telegram_bot.py:1041/1049): the delivery confirmation; {total} {qty} {product}.
	PurchaseOK      Str
	PurchaseOKToken Str
	// OrderGone — ORDER_GONE (telegram_bot.py:1483): the reserved order expired /
	// was already handled when a pay button was tapped.
	OrderGone Str
	// Insufficient — INSUFFICIENT (telegram_bot.py:1498): balance payment rejected.
	Insufficient Str
	// PaidNoStock — PAID_NO_STOCK (telegram_bot.py:1224): money in but the
	// reservation could not be finalized -> refunded to balance; {amount} {balance}.
	PaidNoStock Str
	// PayMethodOff — PAY_METHOD_OFF (telegram_bot.py:1575): a pay method disabled
	// under the button (stale tap).
	PayMethodOff Str
	// PaypalLink — PAYPAL_LINK (telegram_bot.py:1313): the message carrying the
	// PayPal approval link for an ORDER pay (paypp:); {usd} {url}.
	PaypalLink Str
	// PaypalError — PAYPAL_ERROR (telegram_bot.py:1318): the PayPal order could
	// not be created (API/credential failure or no approve link). Shared by the
	// order-pay (paypp:) and wallet-deposit (dep:pp) rails.
	PaypalError Str
	// DepositTimeout — DEPOSIT_TIMEOUT (telegram_bot.py:431): the "payment window
	// closed" notice the expiry worker sends when it wins reserved->expired.
	DepositTimeout Str
}

// buildBuyCatalog materializes the M2.2 string cluster — byte-verbatim from the
// cited Python constants.
func buildBuyCatalog() BuyCatalog {
	return BuyCatalog{
		// PRODUCTS_HEADER (telegram_bot.py:729).
		ProductsHeader: Str{
			EN: "🛒 Choose a product:",
			VI: "🛒 Chọn sản phẩm:",
			ZH: "🛒 请选择商品：",
		},
		// NO_PRODUCTS (telegram_bot.py:742).
		NoProducts: Str{
			EN: "🛒 No products available right now.",
			VI: "🛒 Hiện chưa có sản phẩm nào.",
			ZH: "🛒 暂无可用商品。",
		},
		// RATE_LIMIT_PRODUCT (telegram_bot.py:737).
		RateLimitProduct: Str{
			EN: "⏳ Too many taps. Please wait {sec}s before trying again.",
			VI: "⏳ Bạn bấm quá nhanh. Vui lòng đợi {sec} giây rồi thử lại.",
			ZH: "⏳ 点击过于频繁。请等待 {sec} 秒后再试。",
		},
		// SALES_PAUSED (telegram_bot.py:691).
		SalesPaused: Str{
			EN: "⏸️ Sales are paused right now. You can still deposit, view your history, get login codes and reset passwords. Please check back in a few minutes!",
			VI: "⏸️ Cửa hàng tạm dừng bán hàng. Bạn vẫn có thể nạp tiền, xem lịch sử, lấy mã đăng nhập và đặt lại mật khẩu. Vui lòng quay lại sau ít phút!",
			ZH: "⏸️ 商店暂停销售。您仍可充值、查看历史、获取登录码和重置密码。请稍后再来！",
		},
		// PRODUCT_SUSPENDED (telegram_bot.py:701).
		ProductSuspended: Str{
			EN: "⏸️ This product is temporarily unavailable. Please pick another product or check back later.",
			VI: "⏸️ Sản phẩm này tạm ngừng bán. Vui lòng chọn sản phẩm khác hoặc quay lại sau.",
			ZH: "⏸️ 该产品暂时停售。请选择其他产品或稍后再来。",
		},
		// RESELLER_NOTE (telegram_bot.py:714).
		ResellerNote: Str{
			EN: "🤝 Or buy from an authorized reseller:",
			VI: "🤝 Hoặc mua từ đại lý uỷ quyền:",
			ZH: "🤝 或从授权分销商购买：",
		},
		// TIER_HEADER (telegram_bot.py:756).
		TierHeader: Str{
			EN: "🛡️ {product}\n📦 {stock} in stock\n\nChoose a warranty option (price shown):",
			VI: "🛡️ {product}\n📦 Còn {stock} tài khoản\n\nChọn gói bảo hành (kèm giá):",
			ZH: "🛡️ {product}\n📦 库存 {stock} 个\n\n请选择保修方案（含价格）：",
		},
		// VOLUME_PROMPT (telegram_bot.py:768).
		VolumePrompt: Str{
			EN: "🛒 {product} — {tier}\nUnit price: {unit}\nIn stock: {stock} accounts\nYour balance: {balance}\n\n{discounts}\n\nTap a quick amount below or enter how many to buy:",
			VI: "🛒 {product} — {tier}\nĐơn giá: {unit}\nCòn lại: {stock} tài khoản\nSố dư của bạn: {balance}\n\n{discounts}\n\nChọn nhanh bên dưới hoặc nhập số lượng muốn mua:",
			ZH: "🛒 {product} — {tier}\n单价：{unit}\n库存：{stock} 个账户\n您的余额：{balance}\n\n{discounts}\n\n点击下方快捷数量，或输入购买数量：",
		},
		// BAD_QTY (telegram_bot.py:792).
		BadQty: Str{
			EN: "⚠️ Please enter a positive whole number (e.g. 5).",
			VI: "⚠️ Vui lòng nhập một số nguyên dương (ví dụ: 5).",
			ZH: "⚠️ 请输入一个正整数（例如：5）。",
		},
		// QTY_TOO_MANY (telegram_bot.py:802).
		QtyTooMany: Str{
			EN: "⚠️ At most {max} accounts per order. Please enter a smaller number.",
			VI: "⚠️ Tối đa {max} tài khoản mỗi đơn. Vui lòng nhập số nhỏ hơn.",
			ZH: "⚠️ 每单最多 {max} 个账户。请输入更小的数量。",
		},
		// NO_PRICE (telegram_bot.py:808) — carries the support @handle.
		NoPrice: Str{
			EN: "⚠️ This product has no price set. Contact support @zsec_ducna",
			VI: "⚠️ Sản phẩm này chưa có giá. Liên hệ hỗ trợ @zsec_ducna",
			ZH: "⚠️ 该商品尚未设置价格。请联系客服 @zsec_ducna",
		},
		// SOLD_OUT (telegram_bot.py:1211).
		SoldOut: Str{
			EN: "⚠️ Sorry, not enough accounts left. Please try again with a smaller quantity.",
			VI: "⚠️ Rất tiếc, không đủ tài khoản còn lại. Vui lòng thử lại với số lượng nhỏ hơn.",
			ZH: "⚠️ 抱歉，剩余账户不足。请尝试减少购买数量。",
		},
		// OUT_OF_STOCK (telegram_bot.py:1217).
		OutOfStock: Str{
			EN: "❌ This product is sold out. Please check back later.",
			VI: "❌ Sản phẩm này đã hết hàng. Vui lòng quay lại sau.",
			ZH: "❌ 该商品已售罄，请稍后再来。",
		},
		// PRODUCT_NOTICE_CONTINUE (telegram_bot.py:4230).
		ProductNoticeContinue: Str{EN: "✅ Continue", VI: "✅ Tiếp tục", ZH: "✅ 继续"},
		// BACK_BTN (telegram_bot.py:4227).
		BackBtn: Str{EN: "↩️ Go back", VI: "↩️ Quay lại", ZH: "↩️ 返回"},
		// tier_label branches (telegram_bot.py:4659-4672).
		TierNoWarranty:    Str{EN: "No warranty", VI: "Không bảo hành", ZH: "无保修"},
		TierDaysWarranty:  Str{EN: "{n} days warranty", VI: "Bảo hành {n} ngày", ZH: "{n}天保修"},
		TierHoursWarranty: Str{EN: "{n} hours warranty", VI: "Bảo hành {n} giờ", ZH: "{n}小时保修"},
		// _DISCOUNT_HEADER / _DISCOUNT_UNIT (telegram_bot.py:6513-6514).
		DiscountHeader: Str{EN: "Volume discount:", VI: "Chiết khấu theo số lượng:", ZH: "批量折扣："},
		DiscountUnit:   Str{EN: "accounts", VI: "tài khoản", ZH: "个账户"},
		// PAY_PROMPT (telegram_bot.py:1294).
		PayPrompt: Str{
			EN: "🧾 {product}\nUnit price: {unit} × {qty}\nDiscount: {pct}%\nTotal: {total}\n\nYour balance: {balance}\nChoose a payment method:",
			VI: "🧾 {product}\nĐơn giá: {unit} × {qty}\nChiết khấu: {pct}%\nThành tiền: {total}\n\nSố dư của bạn: {balance}\nChọn cách thanh toán:",
			ZH: "🧾 {product}\n单价：{unit} × {qty}\n折扣：{pct}%\n总计：{total}\n\n您的余额：{balance}\n请选择支付方式：",
		},
		// PAY_BTN_* (telegram_bot.py:1302-1310).
		PayBtnBalance:   Str{EN: "💰 Pay with balance", VI: "💰 Trả bằng số dư", ZH: "💰 余额支付"},
		PayBtnQR:        Str{EN: "📷 Pay with QR (VN)", VI: "📷 Trả bằng QR (VN)", ZH: "📷 扫码支付 (越南)"},
		PayBtnPayPal:    Str{EN: "🌍 Pay with PayPal", VI: "🌍 Trả bằng PayPal", ZH: "🌍 PayPal 支付"},
		PayBtnBinance:   Str{EN: "🪙 Pay with Binance (USDT)", VI: "🪙 Trả bằng Binance (USDT)", ZH: "🪙 Binance 支付 (USDT)"},
		PayBtnBEP20:     Str{EN: "Pay with USDT (BEP20)", VI: "Trả bằng USDT (BEP20)", ZH: "USDT 支付 (BEP20)"},
		PayBtnTRON:      Str{EN: "Pay with USDT (TRC20)", VI: "Trả bằng USDT (TRC20)", ZH: "USDT 支付 (TRC20)"},
		PayBtnApplyCode: Str{EN: "🎟️ Apply promo / referral code", VI: "🎟️ Nhập mã giảm giá", ZH: "🎟️ 使用优惠码 / 推荐码"},
		PayBtnRmCode:    Str{EN: "❌ Remove code {code}", VI: "❌ Bỏ mã {code}", ZH: "❌ 移除优惠码 {code}"},
		// PENDING_* (telegram_bot.py:1275-1291).
		PendingWarn: Str{
			EN: "⚠️ You have an order awaiting payment ({qty}× {product}, {total}).\nCancel it to continue?",
			VI: "⚠️ Bạn đang có một đơn hàng chờ thanh toán ({qty}× {product}, {total}).\nHủy đơn này để tiếp tục?",
			ZH: "⚠️ 您有一个待付款的订单（{qty}× {product}，{total}）。\n取消它以继续？",
		},
		PendingCancelBtn: Str{EN: "❌ Cancel & continue", VI: "❌ Hủy đơn & tiếp tục", ZH: "❌ 取消并继续"},
		PendingKeepBtn:   Str{EN: "↩️ Keep order, go back", VI: "↩️ Giữ đơn, quay lại", ZH: "↩️ 保留订单，返回"},
		PendingCancelled: Str{
			EN: "✅ Pending order cancelled. You can continue.",
			VI: "✅ Đã hủy đơn chờ thanh toán. Bạn có thể tiếp tục.",
			ZH: "✅ 待付款订单已取消。您可以继续。",
		},
		PendingKept: Str{
			EN: "👍 Order kept. Please complete your payment.",
			VI: "👍 Giữ nguyên đơn hàng. Vui lòng hoàn tất thanh toán.",
			ZH: "👍 订单已保留。请完成付款。",
		},
		// PURCHASE_OK / PURCHASE_OK_TOKEN (telegram_bot.py:1041/1049).
		PurchaseOK: Str{
			EN: "✅ Paid {total} for {qty}× {product}.\nYour accounts are in the file attached below. 👇",
			VI: "✅ Đã thanh toán {total} cho {qty}× {product}.\nTài khoản của bạn ở file đính kèm bên dưới. 👇",
			ZH: "✅ 已支付 {total} 购买 {qty}× {product}。\n您的账户在下方附件中。👇",
		},
		PurchaseOKToken: Str{
			EN: "✅ Paid {total} for {qty}× {product}.\nYour account(s) and the auto-import tool are in the .zip attached below. 👇\n\n📖 Instructions: open the .zip, read the README and follow the steps.",
			VI: "✅ Đã thanh toán {total} cho {qty}× {product}.\nTài khoản và công cụ tự động nhập nằm trong file .zip đính kèm bên dưới. 👇\n\n📖 Hướng dẫn: mở file .zip, đọc README và làm theo các bước.",
			ZH: "✅ 已支付 {total} 购买 {qty}× {product}。\n您的账户和自动导入工具都在下方的 .zip 附件中。👇\n\n📖 使用说明：打开 .zip，阅读 README 并按步骤操作。",
		},
		// ORDER_GONE (telegram_bot.py:1483).
		OrderGone: Str{
			EN: "⚠️ This order has expired or was already handled. Please start again from /buy.",
			VI: "⚠️ Đơn hàng đã hết hạn hoặc đã được xử lý. Vui lòng đặt lại từ /buy.",
			ZH: "⚠️ 此订单已过期或已处理。请从 /buy 重新下单。",
		},
		// INSUFFICIENT (telegram_bot.py:1498).
		Insufficient: Str{
			EN: "⚠️ Insufficient balance. Please top up or pay with QR.",
			VI: "⚠️ Số dư không đủ. Vui lòng nạp tiền hoặc trả bằng QR.",
			ZH: "⚠️ 余额不足。请充值或使用二维码支付。",
		},
		// PAID_NO_STOCK (telegram_bot.py:1224) — carries the support @handle.
		PaidNoStock: Str{
			EN: "⚠️ Payment received but accounts could not be delivered. Refunded {amount} to your balance (balance: {balance}). Contact @zsec_ducna if needed.",
			VI: "⚠️ Đã nhận thanh toán nhưng không thể giao tài khoản. Đã hoàn {amount} vào số dư của bạn (số dư: {balance}). Liên hệ @zsec_ducna nếu cần.",
			ZH: "⚠️ 已收到付款但无法交付账户。已退款 {amount} 至您的余额（余额：{balance}）。如需帮助请联系 @zsec_ducna。",
		},
		// PAY_METHOD_OFF (telegram_bot.py:1575).
		PayMethodOff: Str{
			EN: "⚠️ This payment method is currently unavailable. Please choose another.",
			VI: "⚠️ Phương thức thanh toán này hiện không khả dụng. Vui lòng chọn cách khác.",
			ZH: "⚠️ 此支付方式当前不可用。请选择其他方式。",
		},
		// PAYPAL_LINK (telegram_bot.py:1313).
		PaypalLink: Str{
			EN: "🌍 Tap to pay with PayPal ({usd} USDT):\n{url}\n\nYour accounts are delivered once payment completes.",
			VI: "🌍 Nhấn để thanh toán bằng PayPal ({usd} USDT):\n{url}\n\nTài khoản sẽ được giao sau khi thanh toán hoàn tất.",
			ZH: "🌍 点击使用 PayPal 支付（{usd} USDT）：\n{url}\n\n付款完成后将交付账户。",
		},
		// PAYPAL_ERROR (telegram_bot.py:1318) — carries the support @handle.
		PaypalError: Str{
			EN: "⚠️ Could not create PayPal payment. Please try another method or contact @zsec_ducna.",
			VI: "⚠️ Không thể tạo thanh toán PayPal. Vui lòng thử cách khác hoặc liên hệ @zsec_ducna.",
			ZH: "⚠️ 无法创建 PayPal 付款。请尝试其他方式或联系 @zsec_ducna。",
		},
		// DEPOSIT_TIMEOUT (telegram_bot.py:431) — carries the support @handle.
		DepositTimeout: Str{
			EN: "⏱️ Payment timed out. If you already transferred, contact support @zsec_ducna",
			VI: "⏱️ Hết thời gian chờ thanh toán. Nếu bạn đã chuyển khoản, liên hệ hỗ trợ @zsec_ducna",
			ZH: "⏱️ 付款超时。如果您已转账，请联系客服 @zsec_ducna",
		},
	}
}
