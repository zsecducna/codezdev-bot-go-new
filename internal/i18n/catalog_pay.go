package i18n

// catalog_pay.go — the USDT rail string cluster (telegram_bot.py:1326-1481):
// the BEP20/TRON on-chain deposit screens, the two "not configured" notices,
// the band-exhausted USDT_BUSY notice, and the one surviving TXID_* string
// (TxidAlreadyUsed, used by the shared settle's already-credited branch). The
// "I have made the payment" button and the rest of the verify-by-tx-hash
// message family were removed along with the hash-paste path — deposits now
// credit hands-free via the watcher (usdtwatch.go). The Binance Pay screen
// (BinancePay / BinanceNoID) was removed by the USDT-only migration (S3).
//
// Same conventions as the other catalog_<feature>.go files: values are copied
// byte-verbatim from the cited Python constants, `{token}` placeholders are kept
// exactly as written (Go's i18n.F substitutes them; no format specs are used in
// this cluster), and the token SET is identical across the three languages so
// TestStringsComplete passes.

// PayCatalog holds the BEP20 / TRON payment strings.
type PayCatalog struct {
	// BEP20Pay / TronPay — BEP20_PAY / TRON_PAY (telegram_bot.py:1352/1371): the
	// on-chain deposit screens; {usdt} {address}. Markdown so the address
	// renders as a copyable code block.
	//
	// DELIBERATE DEVIATION from the byte-verbatim rule this file otherwise
	// follows, required by the hands-free-deposits ruling. The quoted amount is
	// now the price plus a 0.01000-0.01999 USDT jitter carried to five decimals,
	// and a payment is matched to an order within ±0.00050 — essentially
	// exactly. The Python copy was written for a ±0.10 tolerance, where "be
	// aware of your own network fee" was advice; at this precision a platform
	// that skims its fee out of the transfer makes the payment unmatchable, so
	// the screen has to say two things plainly and in every language:
	//
	//  1. the network fee is the customer's, paid ON TOP of the amount;
	//  2. the figure that must ARRIVE is exact to all five decimals, so a
	//     sender whose platform deducts from the transfer must add it back.
	//
	// {usdt} is wrapped in Markdown backticks — the same emphasis the address
	// already uses on this screen — so the amount is visually prominent and
	// tap-to-copy, rather than a long decimal a customer retypes by hand.
	//
	// The "five decimals" figure is HARDCODED in all three languages while the
	// precision itself is operator-tunable (*_AMOUNT_DECIMALS). That is a
	// deliberate simplification, not an oversight — the ruling fixed the shipped
	// precision at 5 — but an operator who changes AMOUNT_DECIMALS must update
	// these strings too, or the screen will claim a digit count the amount does
	// not have.
	BEP20Pay Str
	TronPay  Str
	// TxidAlreadyUsed — TXID_ALREADY_USED (telegram_bot.py:1437): the transfer was
	// already consumed (the processed-ledger PK is the atomic guard). Still used
	// by settleUSDTMatch's already-credited branch; the rest of the TXID_*
	// verify-by-hash family was removed along with the hash-paste path.
	TxidAlreadyUsed Str
	// TronNoID / BEP20NoID — TRON_NOID / BEP20_NOID (telegram_bot.py:1462/1468): the
	// rail was tapped but no receiving address is configured.
	TronNoID  Str
	BEP20NoID Str
	// USDTBusy — USDT_BUSY (telegram_bot.py:1477): the unique-amount picker found no
	// free slot for this price right now (bands.ErrBandExhausted). Deliberately
	// DISTINCT from *_NOID: the method IS configured, it is temporarily busy.
	USDTBusy Str
}

// buildPayCatalog materializes the USDT rail strings — byte-verbatim from the
// cited Python constants.
func buildPayCatalog() PayCatalog {
	return PayCatalog{
		// BEP20_PAY (telegram_bot.py:1352), REWRITTEN — see the BEP20Pay/TronPay
		// field doc for why these two strings are no longer byte-verbatim. Final
		// line is hands-free: the watcher credits by amount once the transfer
		// confirms, so there is no button to tap and no hash to paste.
		BEP20Pay: Str{
			VI: "🟡 Gửi CHÍNH XÁC `{usdt}` USDT tới địa chỉ BEP20 (BNB Smart Chain) sau:\n" +
				"`{address}`\n" +
				"⚠️ CHỈ dùng mạng BEP20.\n" +
				"⚠️ Số tiền phải VỀ TỚI NƠI đúng từng chữ số — đủ cả 5 chữ số thập phân của `{usdt}` USDT. " +
				"Chính con số này nhận diện đơn của bạn.\n" +
				"⚠️ Phí mạng do bạn trả, CỘNG THÊM. Nếu ví hoặc sàn của bạn trừ phí VÀO chính giao dịch, " +
				"hãy tăng số tiền gửi để đúng `{usdt}` USDT vẫn về tới nơi.\n" +
				"👉 Sau khi chuyển và giao dịch được xác nhận trên chuỗi, số dư của bạn sẽ tự động cập nhật — bạn không cần làm gì thêm.",
			EN: "🟡 Send EXACTLY `{usdt}` USDT to this BEP20 (BNB Smart Chain) address:\n" +
				"`{address}`\n" +
				"⚠️ BEP20 network ONLY.\n" +
				"⚠️ The amount must ARRIVE exact to every digit — all 5 decimals of `{usdt}` USDT. " +
				"That exact figure is what identifies your order.\n" +
				"⚠️ The network fee is yours to pay, ON TOP. If your wallet or exchange takes its fee OUT of " +
				"the transfer, raise what you send so exactly `{usdt}` USDT still arrives.\n" +
				"👉 Once the transfer confirms on-chain, your balance updates automatically — nothing else to do.",
			ZH: "🟡 请准确发送 `{usdt}` USDT 到以下 BEP20（BNB 智能链）地址：\n" +
				"`{address}`\n" +
				"⚠️ 仅限 BEP20 网络。\n" +
				"⚠️ 到账金额必须每一位都完全一致——`{usdt}` USDT 的全部 5 位小数都要对上。" +
				"正是这个数字用来识别您的订单。\n" +
				"⚠️ 网络费由您另行承担。如果您的钱包或交易所从转账金额中扣除手续费，" +
				"请相应提高发送金额，确保准确的 `{usdt}` USDT 到账。\n" +
				"👉 转账在链上确认后，余额会自动更新——您无需其他操作。",
		},
		// TRON_PAY (telegram_bot.py:1371), REWRITTEN — same reasoning as BEP20Pay.
		// Final line is hands-free: the watcher credits by amount once the
		// transfer confirms, so there is no button to tap and no hash to paste.
		TronPay: Str{
			VI: "🔴 Gửi CHÍNH XÁC `{usdt}` USDT tới địa chỉ TRC20 (TRON) sau:\n" +
				"`{address}`\n" +
				"⚠️ CHỈ dùng mạng TRC20.\n" +
				"⚠️ Số tiền phải VỀ TỚI NƠI đúng từng chữ số — đủ cả 5 chữ số thập phân của `{usdt}` USDT. " +
				"Chính con số này nhận diện đơn của bạn.\n" +
				"⚠️ Phí mạng do bạn trả, CỘNG THÊM. Nếu ví hoặc sàn của bạn trừ phí VÀO chính giao dịch, " +
				"hãy tăng số tiền gửi để đúng `{usdt}` USDT vẫn về tới nơi.\n" +
				"👉 Sau khi chuyển và giao dịch được xác nhận trên chuỗi, số dư của bạn sẽ tự động cập nhật — bạn không cần làm gì thêm.",
			EN: "🔴 Send EXACTLY `{usdt}` USDT to this TRC20 (TRON) address:\n" +
				"`{address}`\n" +
				"⚠️ TRC20 network ONLY.\n" +
				"⚠️ The amount must ARRIVE exact to every digit — all 5 decimals of `{usdt}` USDT. " +
				"That exact figure is what identifies your order.\n" +
				"⚠️ The network fee is yours to pay, ON TOP. If your wallet or exchange takes its fee OUT of " +
				"the transfer, raise what you send so exactly `{usdt}` USDT still arrives.\n" +
				"👉 Once the transfer confirms on-chain, your balance updates automatically — nothing else to do.",
			ZH: "🔴 请准确发送 `{usdt}` USDT 到以下 TRC20（TRON）地址：\n" +
				"`{address}`\n" +
				"⚠️ 仅限 TRC20 网络。\n" +
				"⚠️ 到账金额必须每一位都完全一致——`{usdt}` USDT 的全部 5 位小数都要对上。" +
				"正是这个数字用来识别您的订单。\n" +
				"⚠️ 网络费由您另行承担。如果您的钱包或交易所从转账金额中扣除手续费，" +
				"请相应提高发送金额，确保准确的 `{usdt}` USDT 到账。\n" +
				"👉 转账在链上确认后，余额会自动更新——您无需其他操作。",
		},
		// TXID_ALREADY_USED (telegram_bot.py:1437).
		TxidAlreadyUsed: Str{
			VI: "⚠️ Giao dịch này đã được sử dụng. Mỗi giao dịch chỉ ghi nhận một lần. Cần hỗ trợ? @zsec_ducna",
			EN: "⚠️ This transaction was already used. Each transfer is credited once. Need help? @zsec_ducna",
			ZH: "⚠️ 该交易已被使用。每笔转账仅记账一次。需要帮助？@zsec_ducna",
		},
		// TRON_NOID (telegram_bot.py:1462).
		TronNoID: Str{
			VI: "⚠️ Thanh toán USDT TRC20 chưa được cấu hình. Vui lòng chọn cách khác hoặc liên hệ @zsec_ducna.",
			EN: "⚠️ USDT TRC20 payment is not configured. Please choose another method or contact @zsec_ducna.",
			ZH: "⚠️ 未配置 USDT TRC20 支付。请选择其他方式或联系 @zsec_ducna。",
		},
		// BEP20_NOID (telegram_bot.py:1468).
		BEP20NoID: Str{
			VI: "⚠️ Thanh toán USDT BEP20 chưa được cấu hình. Vui lòng chọn cách khác hoặc liên hệ @zsec_ducna.",
			EN: "⚠️ USDT BEP20 payment is not configured. Please choose another method or contact @zsec_ducna.",
			ZH: "⚠️ 未配置 USDT BEP20 支付。请选择其他方式或联系 @zsec_ducna。",
		},
		// USDT_BUSY (telegram_bot.py:1477).
		USDTBusy: Str{
			VI: "⚠️ Đang có quá nhiều thanh toán chờ xử lý cho mức giá này. Vui lòng thử lại sau vài phút hoặc chọn cách thanh toán khác.",
			EN: "⚠️ Too many pending payments for this price right now. Please try again in a few minutes or choose another payment method.",
			ZH: "⚠️ 该价格当前待处理付款过多。请几分钟后重试或选择其他支付方式。",
		},
	}
}
