package i18n

// catalog_pay.go — the USDT rail string cluster (telegram_bot.py:1326-1481):
// the BEP20/TRON on-chain pay+deposit screens, the "I have made the payment"
// button, and the whole verify-by-tx-hash message family (TXID_*), plus the
// two "not configured" notices and the band-exhausted USDT_BUSY notice. The
// Binance Pay screen (BinancePay / BinanceNoID) was removed by the USDT-only
// migration (S3).
//
// Same conventions as the other catalog_<feature>.go files: values are copied
// byte-verbatim from the cited Python constants, `{token}` placeholders are kept
// exactly as written (Go's i18n.F substitutes them; no format specs are used in
// this cluster), and the token SET is identical across the three languages so
// TestStringsComplete passes.

// PayCatalog holds the BEP20 / TRON payment + verification strings.
type PayCatalog struct {
	// BEP20Pay / TronPay — BEP20_PAY / TRON_PAY (telegram_bot.py:1352/1371): the
	// on-chain pay+deposit screens; {usdt} {address}. Markdown so the address
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
	// USDTPaidBtn — USDT_PAID_BTN (telegram_bot.py:1391): the inline button under
	// a BEP20/TRON screen that arms the verify-by-hash flow.
	USDTPaidBtn Str
	// EnterTxid — ENTER_TXID (telegram_bot.py:1393): "paste your tx hash" prompt.
	EnterTxid Str
	// TxidChecking — TXID_CHECKING (telegram_bot.py:1399): sent BEFORE the
	// on-chain lookup (which is I/O).
	TxidChecking Str
	// TxidWaiting — TXID_WAITING (telegram_bot.py:1406): the hash is not confirmed
	// yet; the background confirm-poll will settle it automatically.
	TxidWaiting Str
	// TxidNotFound — TXID_NOTFOUND (telegram_bot.py:1413): no CONFIRMED transfer to
	// our address for that id (yet). Non-terminal — the capture re-arms.
	TxidNotFound Str
	// TxidBadFormat — TXID_BADFORMAT (telegram_bot.py:1422): the pasted text is not
	// a tx-hash shape (checked before any chain round-trip).
	TxidBadFormat Str
	// TxidWindow — TXID_WINDOW (telegram_bot.py:1429): real + confirmed, but the
	// on-chain timestamp falls outside this order's [start, click] window.
	TxidWindow Str
	// TxidAlreadyUsed — TXID_ALREADY_USED (telegram_bot.py:1437): the hash was
	// already consumed (the processed-ledger PK is the atomic guard).
	TxidAlreadyUsed Str
	// TxidAmountConflict — TXID_AMOUNT_CONFLICT (telegram_bot.py:1444): the amount
	// lands in ANOTHER pending order's band — a claim-jack; refused, never credited.
	TxidAmountConflict Str
	// TxidPayMismatch — TXID_PAY_MISMATCH (telegram_bot.py:1453): the amount matched
	// no live band, so it was banked to the wallet instead of delivering the order;
	// {recv} {want} {balance}.
	TxidPayMismatch Str
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
		// field doc for why these two strings are no longer byte-verbatim.
		BEP20Pay: Str{
			VI: "🟡 Gửi CHÍNH XÁC `{usdt}` USDT tới địa chỉ BEP20 (BNB Smart Chain) sau:\n" +
				"`{address}`\n" +
				"⚠️ CHỈ dùng mạng BEP20.\n" +
				"⚠️ Số tiền phải VỀ TỚI NƠI đúng từng chữ số — đủ cả 5 chữ số thập phân của `{usdt}` USDT. " +
				"Chính con số này nhận diện đơn của bạn.\n" +
				"⚠️ Phí mạng do bạn trả, CỘNG THÊM. Nếu ví hoặc sàn của bạn trừ phí VÀO chính giao dịch, " +
				"hãy tăng số tiền gửi để đúng `{usdt}` USDT vẫn về tới nơi.\n" +
				"👉 Sau khi chuyển và giao dịch được xác nhận trên chuỗi, bấm nút bên dưới và dán mã giao dịch (tx ID).",
			EN: "🟡 Send EXACTLY `{usdt}` USDT to this BEP20 (BNB Smart Chain) address:\n" +
				"`{address}`\n" +
				"⚠️ BEP20 network ONLY.\n" +
				"⚠️ The amount must ARRIVE exact to every digit — all 5 decimals of `{usdt}` USDT. " +
				"That exact figure is what identifies your order.\n" +
				"⚠️ The network fee is yours to pay, ON TOP. If your wallet or exchange takes its fee OUT of " +
				"the transfer, raise what you send so exactly `{usdt}` USDT still arrives.\n" +
				"👉 After you transfer and the tx is confirmed on-chain, tap the button below and paste your transaction ID (hash).",
			ZH: "🟡 请准确发送 `{usdt}` USDT 到以下 BEP20（BNB 智能链）地址：\n" +
				"`{address}`\n" +
				"⚠️ 仅限 BEP20 网络。\n" +
				"⚠️ 到账金额必须每一位都完全一致——`{usdt}` USDT 的全部 5 位小数都要对上。" +
				"正是这个数字用来识别您的订单。\n" +
				"⚠️ 网络费由您另行承担。如果您的钱包或交易所从转账金额中扣除手续费，" +
				"请相应提高发送金额，确保准确的 `{usdt}` USDT 到账。\n" +
				"👉 转账并在链上确认后，点击下方按钮并粘贴您的交易 ID（哈希）。",
		},
		// TRON_PAY (telegram_bot.py:1371), REWRITTEN — same reasoning as BEP20Pay.
		TronPay: Str{
			VI: "🔴 Gửi CHÍNH XÁC `{usdt}` USDT tới địa chỉ TRC20 (TRON) sau:\n" +
				"`{address}`\n" +
				"⚠️ CHỈ dùng mạng TRC20.\n" +
				"⚠️ Số tiền phải VỀ TỚI NƠI đúng từng chữ số — đủ cả 5 chữ số thập phân của `{usdt}` USDT. " +
				"Chính con số này nhận diện đơn của bạn.\n" +
				"⚠️ Phí mạng do bạn trả, CỘNG THÊM. Nếu ví hoặc sàn của bạn trừ phí VÀO chính giao dịch, " +
				"hãy tăng số tiền gửi để đúng `{usdt}` USDT vẫn về tới nơi.\n" +
				"👉 Sau khi chuyển và giao dịch được xác nhận trên chuỗi, bấm nút bên dưới và dán mã giao dịch (tx ID).",
			EN: "🔴 Send EXACTLY `{usdt}` USDT to this TRC20 (TRON) address:\n" +
				"`{address}`\n" +
				"⚠️ TRC20 network ONLY.\n" +
				"⚠️ The amount must ARRIVE exact to every digit — all 5 decimals of `{usdt}` USDT. " +
				"That exact figure is what identifies your order.\n" +
				"⚠️ The network fee is yours to pay, ON TOP. If your wallet or exchange takes its fee OUT of " +
				"the transfer, raise what you send so exactly `{usdt}` USDT still arrives.\n" +
				"👉 After you transfer and the tx is confirmed on-chain, tap the button below and paste your transaction ID (hash).",
			ZH: "🔴 请准确发送 `{usdt}` USDT 到以下 TRC20（TRON）地址：\n" +
				"`{address}`\n" +
				"⚠️ 仅限 TRC20 网络。\n" +
				"⚠️ 到账金额必须每一位都完全一致——`{usdt}` USDT 的全部 5 位小数都要对上。" +
				"正是这个数字用来识别您的订单。\n" +
				"⚠️ 网络费由您另行承担。如果您的钱包或交易所从转账金额中扣除手续费，" +
				"请相应提高发送金额，确保准确的 `{usdt}` USDT 到账。\n" +
				"👉 转账并在链上确认后，点击下方按钮并粘贴您的交易 ID（哈希）。",
		},
		// USDT_PAID_BTN (telegram_bot.py:1391).
		USDTPaidBtn: Str{VI: "✅ Tôi đã thanh toán", EN: "✅ I have made the payment", ZH: "✅ 我已完成付款"},
		// ENTER_TXID (telegram_bot.py:1393). Sent on the "I've paid" tap. The
		// hands-free watcher credits a deposit with NO tap and NO hash, so this no
		// longer demands one — it reassures the customer that verification is
		// automatic. The pending_txid capture stays armed, so a pasted hash is
		// still accepted as a manual fallback (capTxid) if the credit is slow.
		EnterTxid: Str{
			VI: "✅ Đã nhận — bot tự động xác nhận khoản nạp ngay khi giao dịch xác nhận trên chuỗi, bạn không cần làm gì cả. Nếu sau vài phút chưa thấy cộng tiền, dán mã giao dịch (tx hash) tại đây để bot kiểm tra lại.",
			EN: "✅ Got it — your deposit is verified automatically once it confirms on-chain, so there's nothing to paste. If it hasn't credited after a few minutes, paste the transaction hash here and I'll check it.",
			ZH: "✅ 已收到——您的充值会在链上确认后自动核实，无需粘贴任何内容。如果几分钟后仍未到账，请在此粘贴交易哈希，我会重新核实。",
		},
		// TXID_CHECKING (telegram_bot.py:1399).
		TxidChecking: Str{
			VI: "⏳ Đang kiểm tra giao dịch trên chuỗi...",
			EN: "⏳ Checking your transaction on-chain...",
			ZH: "⏳ 正在链上检查您的交易...",
		},
		// TXID_WAITING (telegram_bot.py:1406).
		TxidWaiting: Str{
			VI: "⏳ Giao dịch chưa được xác nhận trên chuỗi. Bot sẽ tự kiểm tra trong vài phút và cộng tiền ngay khi giao dịch được xác nhận — bạn không cần gửi lại.",
			EN: "⏳ Your transaction isn't confirmed on-chain yet. I'll keep checking for a few minutes and credit you automatically once it confirms — no need to resend.",
			ZH: "⏳ 您的交易尚未在链上确认。机器人将在几分钟内自动检查，确认后立即入账——无需重新发送。",
		},
		// TXID_NOTFOUND (telegram_bot.py:1413).
		TxidNotFound: Str{
			VI: "⚠️ Chưa tìm thấy giao dịch ĐÃ XÁC NHẬN cho mã đó tới địa chỉ của chúng tôi.\n" +
				"Nếu bạn vừa chuyển, hãy đợi giao dịch được xác nhận trên chuỗi rồi gửi lại mã. Cần hỗ trợ? @zsec_ducna",
			EN: "⚠️ No CONFIRMED transfer to our address found for that ID yet.\n" +
				"If you just paid, wait for on-chain confirmation and send the ID again. Need help? @zsec_ducna",
			ZH: "⚠️ 暂未找到该 ID 向我们地址的已确认转账。\n" +
				"如果您刚刚付款，请等待链上确认后再次发送该 ID。需要帮助？@zsec_ducna",
		},
		// TXID_BADFORMAT (telegram_bot.py:1422).
		TxidBadFormat: Str{
			VI: "⚠️ Mã giao dịch không hợp lệ. Hãy dán đầy đủ mã hash của giao dịch.",
			EN: "⚠️ That isn't a valid transaction ID. Paste the full transaction hash.",
			ZH: "⚠️ 交易 ID 无效。请粘贴完整的交易哈希。",
		},
		// TXID_WINDOW (telegram_bot.py:1429).
		TxidWindow: Str{
			VI: "⚠️ Giao dịch này nằm NGOÀI khoảng thời gian thanh toán của đơn (phải được thực hiện trong lúc đặt đơn).\n" +
				"Không thể ghi nhận. Nếu bạn cho rằng có nhầm lẫn, liên hệ @zsec_ducna.",
			EN: "⚠️ That transaction is OUTSIDE this order's payment window (it must be made during the order).\n" +
				"Not credited. If you believe this is an error, contact @zsec_ducna.",
			ZH: "⚠️ 该交易不在此订单的付款时间窗口内（必须在下单期间完成）。\n未予记账。如有疑问，请联系 @zsec_ducna。",
		},
		// TXID_ALREADY_USED (telegram_bot.py:1437).
		TxidAlreadyUsed: Str{
			VI: "⚠️ Giao dịch này đã được sử dụng. Mỗi giao dịch chỉ ghi nhận một lần. Cần hỗ trợ? @zsec_ducna",
			EN: "⚠️ This transaction was already used. Each transfer is credited once. Need help? @zsec_ducna",
			ZH: "⚠️ 该交易已被使用。每笔转账仅记账一次。需要帮助？@zsec_ducna",
		},
		// TXID_AMOUNT_CONFLICT (telegram_bot.py:1444).
		TxidAmountConflict: Str{
			VI: "⚠️ Số tiền của giao dịch này không khớp đơn của bạn và trùng với một đơn đang chờ khác.\n" +
				"Không thể ghi nhận. Liên hệ @zsec_ducna nếu cần.",
			EN: "⚠️ That transfer's amount doesn't match your order and matches another pending order.\n" +
				"Not credited. Contact @zsec_ducna if you need help.",
			ZH: "⚠️ 该转账金额与您的订单不符，且与另一笔待处理订单相同。\n未予记账。如需帮助请联系 @zsec_ducna。",
		},
		// TXID_PAY_MISMATCH (telegram_bot.py:1453).
		TxidPayMismatch: Str{
			VI: "⚠️ Số tiền nhận ({recv} USDT) không khớp đơn hàng ({want} USDT).\n" +
				"Đã cộng vào số dư ví của bạn (số dư: {balance}). Bạn có thể thanh toán đơn bằng số dư hoặc liên hệ @zsec_ducna.",
			EN: "⚠️ The amount received ({recv} USDT) doesn't match this order ({want} USDT).\n" +
				"We added it to your wallet balance instead (balance: {balance}). You can pay the order from your balance or contact @zsec_ducna.",
			ZH: "⚠️ 收到的金额（{recv} USDT）与此订单（{want} USDT）不符。\n" +
				"已改为存入您的钱包余额（余额：{balance}）。您可用余额支付该订单或联系 @zsec_ducna。",
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
