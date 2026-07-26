package i18n

// catalog_otp.go — M2.6 login-code (OTP / password-reset link) string cluster.
//
// Covers the whole /otp + "Login code" self-service suite: the arming prompt,
// the ownership/batch/rate-limit refusals, the per-account result messages
// (OTP code and/or reset link), and the session-lock stop/keep confirmation.
//
// Field names mirror the Python constant names 1:1 so the tables stay
// byte-diffable against telegram_bot.py. Values are copied verbatim from the
// cited Python constant, including emoji, spacing and `{placeholder}` tokens.
//
// NOTE: CODE_BTN (telegram_bot.py:680) is NOT here — it is the same string in
// every language, so it lives beside the other fixed labels as the plain
// `CodeBtn` const in catalog.go. The `otp` slash-menu entry likewise already
// lives in Catalog.Commands.

// OTPCatalog holds every login-code string (telegram_bot.py:583-681).
type OTPCatalog struct {
	// CodePrompt — CODE_PROMPT (telegram_bot.py:585-596): the arming message that
	// asks for the purchased username(s). {n} is OTP_MAX_BATCH so the per-request
	// cap is stated in exactly one place.
	CodePrompt Str

	// CodeNotOwned — CODE_NOT_OWNED (telegram_bot.py:598-607): batch ownership
	// failure. {u} lists the accounts that aren't in any of the buyer's orders;
	// naming them is deliberate (a typo is the common case).
	CodeNotOwned Str

	// CodeWorking — CODE_WORKING (telegram_bot.py:609-613): the single
	// acknowledgement sent when a batch is accepted. {u} is the comma-joined
	// account list; results then stream in per account.
	CodeWorking Str

	// CodeOK — CODE_OK (telegram_bot.py:614-618): a delivered sign-in code.
	// {u} = account, {code} = the 6-digit OTP. Rendered with parse_mode=HTML
	// (the code sits in <b>…</b>), so both tokens are HTML-escaped at the call
	// site.
	CodeOK Str

	// ResetOK — RESET_OK (telegram_bot.py:619-623): a delivered password-reset
	// link. {u} = account, {link} = the reset URL (bare, so Telegram auto-links
	// it). Also HTML mode, so both tokens are escaped at the call site.
	ResetOK Str

	// CodeFail — CODE_FAIL (telegram_bot.py:626-630): per-account fetch failure.
	// {u} names the single account that came up empty — every account in a batch
	// reports its own result independently. Carries the support @handle, so the
	// white-label rewrite reaches it.
	CodeFail Str

	// OTPTooMany — OTP_TOO_MANY (telegram_bot.py:632-636): the batch exceeded the
	// per-request account cap. {n} = OTP_MAX_BATCH.
	OTPTooMany Str

	// OTPRateLimit — OTP_RATELIMIT (telegram_bot.py:648-652): the per-chat
	// cooldown refusal. {sec} = whole seconds left to wait.
	OTPRateLimit Str

	// OTPBusyWarn — OTP_BUSY_WARN (telegram_bot.py:656-660): shown by otp_guard
	// when a live OTP session blocks another action, above the stop/keep buttons.
	OTPBusyWarn Str

	// OTPStopBtn / OTPKeepBtn — OTP_STOP_BTN / OTP_KEEP_BTN
	// (telegram_bot.py:661-662): the two otp_guard confirmation buttons
	// (otpstop:<resume_cmd> / otpkeep).
	OTPStopBtn Str
	OTPKeepBtn Str

	// OTPStopped — OTP_STOPPED (telegram_bot.py:663): confirmation after the
	// session is torn down (any in-flight fetch is silenced).
	OTPStopped Str

	// OTPKept — OTP_KEPT (telegram_bot.py:664-668): shown when the customer
	// chooses to stay in the session. {n} = OTP_MAX_BATCH.
	OTPKept Str
}

// buildOTPCatalog materializes the login-code strings. Values are verbatim from
// telegram_bot.py:585-668 — same emoji, same line breaks, same tokens.
func buildOTPCatalog() OTPCatalog {
	return OTPCatalog{
		// CODE_PROMPT (telegram_bot.py:585-596).
		CodePrompt: Str{
			VI: "🔑 Trước tiên hãy thử ĐĂNG NHẬP tại cổng AWS bằng (các) username trong FILE ĐƠN HÀNG của bạn.\n\n" +
				"Sau đó gửi (các) username vào đây để lấy OTP — một hoặc nhiều tài khoản, cách nhau bằng dấu cách " +
				"(tối đa {n}).\nVí dụ: user1 user2 user3",
			EN: "🔑 First, try to SIGN IN at the AWS access portal using the username(s) from your ORDER FILE.\n\n" +
				"Then send the username(s) here to retrieve the OTP — one account, or several separated by spaces " +
				"(up to {n}).\nExample: user1 user2 user3",
			ZH: "🔑 请先使用订单文件中的用户名在 AWS 访问门户尝试登录。\n\n" +
				"然后在此发送一个或多个用户名以获取 OTP，多个用户名用空格分隔（最多 {n} 个）。\n示例：user1 user2 user3",
		},

		// CODE_NOT_OWNED (telegram_bot.py:598-607).
		CodeNotOwned: Str{
			VI: "⛔ Các tài khoản sau không có trong đơn hàng của bạn: {u}\n" +
				"Bạn chỉ lấy được mã cho tài khoản đã mua. Vui lòng kiểm tra lại username rồi thử lại.",
			EN: "⛔ These accounts aren't in any of your orders: {u}\n" +
				"You can only retrieve codes for accounts you purchased. Please check the usernames and try again.",
			ZH: "⛔ 以下账户不在您的任何订单中：{u}\n" +
				"您只能获取已购买账户的验证码。请核对用户名后重试。",
		},

		// CODE_WORKING (telegram_bot.py:609-613).
		CodeWorking: Str{
			VI: "⏳ Đang lấy OTP / link đặt lại mật khẩu cho: {u}\nKết quả sẽ được gửi ngay khi từng tài khoản sẵn sàng.",
			EN: "⏳ Retrieving OTP / reset link for: {u}\nResults are sent to you as soon as each account is ready.",
			ZH: "⏳ 正在获取以下账户的 OTP / 重置链接：{u}\n每个账户准备好后将立即发送结果。",
		},

		// CODE_OK (telegram_bot.py:614-618).
		CodeOK: Str{
			VI: "🔑 Mã đăng nhập cho {u}:\n\n<b>{code}</b>\n\n⚠️ Có hiệu lực 10 phút. Đăng nhập tại cổng AWS với username + mã này.",
			EN: "🔑 Login code for {u}:\n\n<b>{code}</b>\n\n⚠️ Valid 10 minutes. Sign in at the AWS portal with your username + this code.",
			ZH: "🔑 {u} 的登录验证码：\n\n<b>{code}</b>\n\n⚠️ 10 分钟内有效。请使用用户名 + 此验证码在 AWS 门户登录。",
		},

		// RESET_OK (telegram_bot.py:619-623).
		ResetOK: Str{
			VI: "🔗 Link đặt lại mật khẩu cho {u}:\n\n{link}\n\n⚠️ Mở link để đặt mật khẩu mới. Link hết hạn nhanh — dùng ngay.",
			EN: "🔗 Password reset link for {u}:\n\n{link}\n\n⚠️ Open it to set a new password. The link expires quickly — use it now.",
			ZH: "🔗 {u} 的密码重置链接：\n\n{link}\n\n⚠️ 打开链接设置新密码。链接很快过期，请立即使用。",
		},

		// CODE_FAIL (telegram_bot.py:626-630).
		CodeFail: Str{
			VI: "⚠️ Chưa thấy OTP hoặc link đặt lại mật khẩu cho {u}. Hãy chắc chắn bạn vừa thử ĐĂNG NHẬP (hoặc yêu cầu đặt lại mật khẩu) tại cổng AWS, đợi vài giây rồi thử lại. Cần hỗ trợ? @zsec_ducna",
			EN: "⚠️ No recent OTP or reset link found for {u}. Make sure you just tried to SIGN IN (or requested a password reset) at the AWS portal, wait a few seconds, then try again. Need help? @zsec_ducna",
			ZH: "⚠️ 未找到 {u} 最近的 OTP 或重置链接。请确认您刚刚在 AWS 门户尝试过登录（或申请了密码重置），稍等几秒后重试。需要帮助？@zsec_ducna",
		},

		// OTP_TOO_MANY (telegram_bot.py:632-636).
		OTPTooMany: Str{
			VI: "⚠️ Mỗi lần chỉ lấy tối đa {n} tài khoản. Vui lòng gửi {n} username trở xuống.",
			EN: "⚠️ You can request up to {n} accounts at a time. Please send {n} or fewer usernames.",
			ZH: "⚠️ 每次最多 {n} 个账户。请发送不超过 {n} 个用户名。",
		},

		// OTP_RATELIMIT (telegram_bot.py:648-652).
		OTPRateLimit: Str{
			VI: "⏳ Vui lòng đợi {sec}s trước khi lấy mã OTP tiếp theo.",
			EN: "⏳ Please wait {sec}s before requesting another OTP code.",
			ZH: "⏳ 请等待 {sec} 秒后再获取下一个 OTP。",
		},

		// OTP_BUSY_WARN (telegram_bot.py:656-660).
		OTPBusyWarn: Str{
			VI: "⏳ Bạn đang có một phiên lấy OTP. Hãy dừng phiên này trước khi làm việc khác.",
			EN: "⏳ You have an OTP session in progress. Please stop it before doing anything else.",
			ZH: "⏳ 您有一个进行中的 OTP 会话。请先停止它再进行其他操作。",
		},

		// OTP_STOP_BTN / OTP_KEEP_BTN (telegram_bot.py:661-662).
		OTPStopBtn: Str{VI: "🛑 Dừng OTP & tiếp tục", EN: "🛑 Stop OTP & continue", ZH: "🛑 停止 OTP 并继续"},
		OTPKeepBtn: Str{VI: "↩️ Tiếp tục chờ OTP", EN: "↩️ Keep the OTP session", ZH: "↩️ 继续 OTP 会话"},

		// OTP_STOPPED (telegram_bot.py:663).
		OTPStopped: Str{VI: "🛑 Đã dừng phiên OTP.", EN: "🛑 OTP session stopped.", ZH: "🛑 已停止 OTP 会话。"},

		// OTP_KEPT (telegram_bot.py:664-668).
		OTPKept: Str{
			VI: "⏳ Vẫn đang trong phiên OTP. Gửi username (tối đa {n}) hoặc chờ kết quả.",
			EN: "⏳ Still in your OTP session. Send the username(s) (up to {n}) or wait for results.",
			ZH: "⏳ OTP 会话仍在进行中。请发送用户名（最多 {n} 个）或等待结果。",
		},
	}
}
