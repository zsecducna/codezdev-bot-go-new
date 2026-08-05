package i18n

// catalog_orders.go — the 🧾 ORDERS / refund-subsystem customer-facing string
// cluster (telegram_bot.py:1232-1284, 7097-7108). Admin-surface strings for
// this feature are FIXED ENGLISH LITERALS in internal/handlers/adminorders.go
// (the established convention: admin.go:74-75, adminusers.go's own header) —
// only the few DMs a customer actually receives live here.
//
// REFUND_CUSTOMER (the balance-payout receipt) already lives in
// APIKeyCatalog.RefundCustomer (catalog_apikey.go) because the M2.5 API-key
// group-refund path was the first to send it; this cluster adds its manual-
// payout twin plus the two REFUND_DECISION notices, all reused verbatim by
// every refund entry point (admin adm:refund, force-refund, and the approved/
// rejected customer-request outcomes).
type OrdersCatalog struct {
	// RefundCustomerManual — REFUND_CUSTOMER_MANUAL (telegram_bot.py:1254):
	// the buyer's refund receipt DM when the admin pays out by manual bank/QR
	// transfer (no wallet credit); {des} {tier} {date} {amount}.
	RefundCustomerManual Str
	// RefundDecisionApproved — REFUND_DECISION["approved"] (telegram_bot.py:
	// 7097-7100): sent the instant an admin approves a filed refund request,
	// BEFORE the actual payout completes (the payout DM — RefundCustomer /
	// RefundCustomerManual — fires later when the refund actually commits);
	// {des}.
	RefundDecisionApproved Str
	// RefundDecisionRejected — REFUND_DECISION["rejected"] (telegram_bot.py:
	// 7101-7104); {des} {reason}.
	RefundDecisionRejected Str
}

// buildOrdersCatalog materializes the orders/refund customer strings verbatim
// from Python.
func buildOrdersCatalog() OrdersCatalog {
	return OrdersCatalog{
		// REFUND_CUSTOMER_MANUAL (telegram_bot.py:1254).
		RefundCustomerManual: Str{
			EN: "✅ Your order has been refunded\n🧾 {des}\n🛡️ Tier: {tier}\n📅 Purchased: {date}\n💰 Refund amount: {amount} will be sent to you by bank transfer.\nContact @zsec_ducna with your payment details if needed.",
			VI: "✅ Đơn hàng của bạn đã được hoàn tiền\n🧾 {des}\n🛡️ Gói: {tier}\n📅 Ngày mua: {date}\n💰 Số tiền hoàn: {amount} sẽ được chuyển khoản cho bạn.\nLiên hệ @zsec_ducna để cung cấp thông tin nhận tiền nếu cần.",
			ZH: "✅ 您的订单已退款\n🧾 {des}\n🛡️ 方案：{tier}\n📅 购买日期：{date}\n💰 退款金额 {amount} 将通过银行转账给您。\n如需提供收款信息请联系 @zsec_ducna。",
		},
		// REFUND_DECISION["approved"] (telegram_bot.py:7097-7100).
		RefundDecisionApproved: Str{
			EN: "✅ Your refund request for order {des} was APPROVED. The refund is being processed.",
			VI: "✅ Yêu cầu hoàn tiền cho đơn {des} đã được DUYỆT. Hệ thống đang xử lý hoàn tiền.",
			ZH: "✅ 您订单 {des} 的退款申请已获批准，正在处理退款。",
		},
		// REFUND_DECISION["rejected"] (telegram_bot.py:7101-7104).
		RefundDecisionRejected: Str{
			EN: "❌ Your refund request for order {des} was REJECTED.\nReason: {reason}",
			VI: "❌ Yêu cầu hoàn tiền cho đơn {des} bị TỪ CHỐI.\nLý do: {reason}",
			ZH: "❌ 您订单 {des} 的退款申请被拒绝。\n原因：{reason}",
		},
	}
}
