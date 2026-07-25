package i18n

// catalog_customer.go — M2.6 customer self-service string cluster.
//
// Covers the Self-Service submenu (SELFSERVICE_HEADER / SS_*_BTN), the
// refund-request picker (REFUND_PICK_*), the customer password-reset flow
// (CUST_RESET_*), the account-deletion flow (CUST_DEL_*), and the
// promo/referral code prompts (ENTER_CODE, CODE_INVALID, CODE_REMOVED).
//
// Field names mirror the Python constant names 1:1 so the tables stay
// byte-diffable against telegram_bot.py. Values are copied verbatim from the
// cited Python constant, including emoji, spacing and `{placeholder}` tokens.

// CustomerCatalog holds every customer self-service string
// (telegram_bot.py:7042-7320, 1506-1556).
type CustomerCatalog struct {

	// ---- Self-Service submenu (telegram_bot.py:7042-7059) ----

	// SelfServiceHeader — SELFSERVICE_HEADER (telegram_bot.py:7042): the submenu
	// title shown by showSelfService / _show_self_service @4259.
	SelfServiceHeader Str

	// SS button labels — SS_*_BTN (telegram_bot.py:7049-7059). Each maps to one
	// inline-button row whose callback re-dispatches to the matching command.
	SSHistoryBtn Str // SS_HISTORY_BTN
	SSInfoBtn    Str // SS_INFO_BTN
	SSCodeBtn    Str // SS_CODE_BTN
	SSResetBtn   Str // SS_RESET_BTN
	SSDelAcctBtn Str // SS_DELACCT_BTN (shown only when delete-accounts is enabled)
	SSMyKeysBtn  Str // SS_MYKEYS_BTN
	SSRefundBtn  Str // SS_REFUND_BTN
	SSSupportBtn Str // SS_SUPPORT_BTN

	// ---- Refund-request picker (telegram_bot.py:7060-7075) ----

	// RefundPickHeader — REFUND_PICK_HEADER (telegram_bot.py:7060): the
	// pick-an-order menu header.
	RefundPickHeader Str

	// RefundPickEmpty — REFUND_PICK_EMPTY (telegram_bot.py:7065): no eligible
	// orders in the 30-day window.
	RefundPickEmpty Str

	// RefundRequestOK — REFUND_REQUEST_OK (telegram_bot.py:7070): confirmation
	// sent after a non-apikey request is filed; {des}.
	RefundRequestOK Str

	// ---- Customer password-reset flow (telegram_bot.py:7162-7192) ----

	// CustResetPrompt — CUST_RESET_PROMPT (telegram_bot.py:7162): arming message
	// that asks the customer to send their account email(s).
	CustResetPrompt Str

	// CustResetNoSSO — CUST_RESET_NO_SSO (telegram_bot.py:7192): Entra is not
	// reachable; reset unavailable, customer directed to support.
	CustResetNoSSO Str

	// ---- Customer account-deletion flow (telegram_bot.py:7264-7300) ----

	// CustDelDisabled — CUST_DEL_DISABLED (telegram_bot.py:7264): soft
	// kill-switch hit; feature temporarily unavailable.
	CustDelDisabled Str

	// CustDelNotAllowed — CUST_DEL_NOT_ALLOWED (telegram_bot.py:7271): caller is
	// not an admin or authorized reseller.
	CustDelNotAllowed Str

	// CustDelPrompt — CUST_DEL_PROMPT (telegram_bot.py:7280): arming message
	// asking for email(s) to delete, one per line; {n} = CUST_DEL_MAX.
	CustDelPrompt Str

	// CustDelNoSSO — CUST_DEL_NO_SSO (telegram_bot.py:7295): Entra unreachable
	// at the time the flow was started.
	CustDelNoSSO Str

	// ---- Promo/referral code prompts (telegram_bot.py:1506-1556) ----

	// EnterCode — ENTER_CODE (telegram_bot.py:1506): prompt sent after a customer
	// taps "Apply code" on the pay screen; tells them to type their 8-char code.
	EnterCode Str

	// CodeInvalid — CODE_INVALID (telegram_bot.py:1512): the typed code matched
	// nothing; {code}.
	CodeInvalid Str

	// CodeRemoved — CODE_REMOVED (telegram_bot.py:1556): code stripped, price
	// reverts to the volume-discount baseline.
	CodeRemoved Str
}

// buildCustomerCatalog populates the M2.6 customer string cluster verbatim
// from the Python originals.
func buildCustomerCatalog() CustomerCatalog {
	return CustomerCatalog{

		// ── Self-Service submenu ──────────────────────────────────────────────

		// SELFSERVICE_HEADER (telegram_bot.py:7042-7045).
		SelfServiceHeader: Str{
			VI: "🛠️ Tự phục vụ — chọn một chức năng:",
			EN: "🛠️ Self-Service — choose an option:",
			ZH: "🛠️ 自助服务 — 请选择一项：",
		},

		// SS_HISTORY_BTN (telegram_bot.py:7049).
		SSHistoryBtn: Str{
			VI: "🧾 Lịch sử đơn hàng",
			EN: "🧾 Order history",
			ZH: "🧾 订单历史",
		},

		// SS_INFO_BTN (telegram_bot.py:7050).
		SSInfoBtn: Str{
			VI: "👤 Thông tin",
			EN: "👤 Customer info",
			ZH: "👤 客户信息",
		},

		// SS_CODE_BTN (telegram_bot.py:7051).
		SSCodeBtn: Str{
			VI: "🔑 Mã đăng nhập",
			EN: "🔑 Login code",
			ZH: "🔑 登录码",
		},

		// SS_RESET_BTN (telegram_bot.py:7052).
		SSResetBtn: Str{
			VI: "♻️ Đặt lại mật khẩu",
			EN: "♻️ Password reset",
			ZH: "♻️ 密码重置",
		},

		// SS_DELACCT_BTN (telegram_bot.py:7053): only rendered when the feature
		// is enabled AND the chat is an admin / authorized reseller.
		SSDelAcctBtn: Str{
			VI: "🗑 Xoá tài khoản",
			EN: "🗑 Delete accounts",
			ZH: "🗑 删除账户",
		},

		// SS_MYKEYS_BTN (telegram_bot.py:7056).
		SSMyKeysBtn: Str{
			VI: "🔑 API key của tôi",
			EN: "🔑 My API Keys",
			ZH: "🔑 我的 API Key",
		},

		// SS_REFUND_BTN (telegram_bot.py:7059).
		SSRefundBtn: Str{
			VI: "🔁 Yêu cầu hoàn tiền",
			EN: "🔁 Ask for refund",
			ZH: "🔁 申请退款",
		},

		// SS_SUPPORT_BTN (telegram_bot.py:7054).
		SSSupportBtn: Str{
			VI: "🆘 Hỗ trợ",
			EN: "🆘 Support",
			ZH: "🆘 客服",
		},

		// ── Refund picker ─────────────────────────────────────────────────────

		// REFUND_PICK_HEADER (telegram_bot.py:7060-7063).
		RefundPickHeader: Str{
			VI: "🔁 Chọn đơn hàng bạn muốn yêu cầu hoàn tiền:",
			EN: "🔁 Pick the order you want to request a refund for:",
			ZH: "🔁 选择您要申请退款的订单：",
		},

		// REFUND_PICK_EMPTY (telegram_bot.py:7065-7068).
		RefundPickEmpty: Str{
			VI: "Bạn chưa có đơn hàng nào đủ điều kiện hoàn tiền gần đây.",
			EN: "You have no recent refund-eligible orders.",
			ZH: "您最近没有符合退款条件的订单。",
		},

		// REFUND_REQUEST_OK (telegram_bot.py:7070-7073); {des}.
		RefundRequestOK: Str{
			VI: "✅ Đã gửi yêu cầu hoàn tiền cho đơn {des}. Quản trị viên sẽ xem xét.",
			EN: "✅ Refund request submitted for order {des}. An admin will review it.",
			ZH: "✅ 已提交订单 {des} 的退款申请。管理员将进行审核。",
		},

		// ── Password reset ────────────────────────────────────────────────────

		// CUST_RESET_PROMPT (telegram_bot.py:7162-7177).
		CustResetPrompt: Str{
			VI: "♻️ Gửi EMAIL của tài khoản bạn muốn đặt lại mật khẩu (chính là email đăng nhập " +
				"trong file đơn hàng của bạn).\n\n" +
				"📄 Đặt lại nhiều tài khoản: gửi mỗi email một dòng, dòng dạng email|matkhau, " +
				"hoặc tải lên file .txt đơn hàng của bạn.\n\n" +
				"⚠️ Đặt lại sẽ vô hiệu hoá mật khẩu hiện tại của các tài khoản đó.",
			EN: "♻️ Send the EMAIL of the account whose password you want to reset (the sign-in " +
				"email from your order file).\n\n" +
				"📄 Reset several at once: send one email per line, email|password lines, " +
				"or upload your order .txt file.\n\n" +
				"⚠️ Resetting will invalidate those accounts' current passwords.",
			ZH: "♻️ 请发送您要重置密码的账户邮箱（即订单文件中的登录邮箱）。\n\n" +
				"📄 批量重置：每行一个邮箱、email|密码 格式，或直接上传订单 .txt 文件。\n\n" +
				"⚠️ 重置将使这些账户当前的密码失效。",
		},

		// CUST_RESET_NO_SSO (telegram_bot.py:7192-7196).
		CustResetNoSSO: Str{
			VI: "⚠️ Hiện chưa thể đặt lại mật khẩu tự động. Vui lòng liên hệ hỗ trợ: @zsec_ducna",
			EN: "⚠️ Automatic password reset is unavailable right now. Please contact support: @zsec_ducna",
			ZH: "⚠️ 当前无法自动重置密码。请联系客服：@zsec_ducna",
		},

		// ── Account deletion ──────────────────────────────────────────────────

		// CUST_DEL_DISABLED (telegram_bot.py:7264-7268).
		CustDelDisabled: Str{
			VI: "ℹ️ Tính năng xoá tài khoản tạm thời không khả dụng. Cần xoá? Liên hệ hỗ trợ: @zsec_ducna",
			EN: "ℹ️ Account deletion is temporarily unavailable. Need an account removed? Contact support: @zsec_ducna",
			ZH: "ℹ️ 账户删除功能暂时不可用。需要删除账户？请联系客服：@zsec_ducna",
		},

		// CUST_DEL_NOT_ALLOWED (telegram_bot.py:7271-7275).
		CustDelNotAllowed: Str{
			VI: "ℹ️ Tính năng xoá tài khoản chỉ dành cho đại lý uỷ quyền. Cần xoá? Liên hệ hỗ trợ: @zsec_ducna",
			EN: "ℹ️ Account deletion is for authorized resellers only. Need an account removed? Contact support: @zsec_ducna",
			ZH: "ℹ️ 账户删除功能仅限授权分销商使用。需要删除账户？请联系客服：@zsec_ducna",
		},

		// CUST_DEL_PROMPT (telegram_bot.py:7280-7293); {n} = CUST_DEL_MAX.
		CustDelPrompt: Str{
			VI: "🗑 Xoá những tài khoản bạn đã dùng hết tín dụng.\n\n" +
				"Gửi EMAIL tài khoản — MỖI DÒNG MỘT EMAIL, tối đa {n}.\nVí dụ:\n" +
				"  name1@domain.com\n  name2@domain.com\n\n" +
				"⚠️ Thao tác này xoá VĨNH VIỄN tài khoản. Bạn sẽ KHÔNG dùng lại được.",
			EN: "🗑 Delete accounts you've finished using (all credits spent).\n\n" +
				"Send the account email(s) — ONE PER LINE, up to {n}.\nExample:\n" +
				"  name1@domain.com\n  name2@domain.com\n\n" +
				"⚠️ This permanently deletes the account(s). You will NOT be able to use them again.",
			ZH: "🗑 删除您已用完额度的账户。\n\n" +
				"请发送账户邮箱 — 每行一个，最多 {n} 个。\n示例：\n" +
				"  name1@domain.com\n  name2@domain.com\n\n" +
				"⚠️ 此操作将永久删除账户，您将无法再次使用。",
		},

		// CUST_DEL_NO_SSO (telegram_bot.py:7295-7298).
		CustDelNoSSO: Str{
			VI: "⚠️ Hiện chưa thể xoá tài khoản. Vui lòng liên hệ hỗ trợ: @zsec_ducna",
			EN: "⚠️ Account deletion isn't available right now. Please contact support: @zsec_ducna",
			ZH: "⚠️ 当前无法删除账户。请联系客服：@zsec_ducna",
		},

		// ── Promo / referral code prompts ─────────────────────────────────────

		// ENTER_CODE (telegram_bot.py:1506-1509): arming message for the
		// pending_ordercode capture after the customer taps "Apply code".
		EnterCode: Str{
			VI: "🎟️ Gửi mã giảm giá hoặc mã giới thiệu của bạn (8 ký tự):",
			EN: "🎟️ Send your promo or referral code (8 characters):",
			ZH: "🎟️ 请发送您的优惠码或推荐码（8 位字符）：",
		},

		// CODE_INVALID (telegram_bot.py:1512-1516); {code}.
		CodeInvalid: Str{
			VI: "⚠️ Mã {code} không hợp lệ.",
			EN: "⚠️ Code {code} is not valid.",
			ZH: "⚠️ 优惠码 {code} 无效。",
		},

		// CODE_REMOVED (telegram_bot.py:1556-1559): code stripped from the
		// reserved order; pay screen re-renders at the volume-discount price.
		CodeRemoved: Str{
			VI: "✅ Đã bỏ mã. Hiển thị lại giá theo chiết khấu số lượng.",
			EN: "✅ Code removed. Showing the volume-discount price again.",
			ZH: "✅ 已移除优惠码。重新显示批量折扣价格。",
		},
	}
}
