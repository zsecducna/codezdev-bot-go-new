package i18n

// Catalog is the single value that holds every localized string constant the
// bot renders. handlers-go.md §5 mandates one struct (populated once in
// buildCatalog) rather than scattered package vars, so the white-label rewrite
// can deep-copy + atomic-swap the whole table and the completeness test can
// reflection-walk it.
//
// This M2.1 slice carries only the CORE strings the dispatch skeleton needs
// (start / language / support / menu / maintenance / keyboard-refresh, plus the
// per-language command menu). Feature-string clusters (buy, deposit, apikey,
// refund, entra, sell, admin …) are added by their own slices in dedicated
// catalog_*.go files with the SAME identifier names as the Python constants
// (PURCHASE_OK -> PurchaseOK) for branch-for-branch auditability. See
// catalog_todo.go for the placeholder markers.
type Catalog struct {
	// Welcome — WELCOME (telegram_bot.py:501-505), the /start banner shown above
	// the persistent keyboard.
	Welcome Str
	// LangPrompt — PROMPT (telegram_bot.py:414): the /language picker header.
	LangPrompt Str
	// LangConfirm — CONFIRM (telegram_bot.py:415-417): shown after a language
	// switch, in the NEWLY selected language.
	LangConfirm Str
	// LangLabel — LANG_LABEL (telegram_bot.py:418). Each language's own label,
	// e.g. LangLabel.T(VI) == "🇻🇳 Tiếng Việt"; used to build the language
	// keyboard's per-language buttons.
	LangLabel Str
	// Support — SUPPORT (telegram_bot.py:508-512): the contact card. Its
	// @handle is the token ApplySupportHandle rewrites.
	Support Str
	// Maintenance — MAINTENANCE (telegram_bot.py:682-686): shown to non-admins
	// for every action while maintenance mode is on.
	Maintenance Str
	// KBRefreshNote — KB_REFRESH_NOTE (telegram_bot.py:6935-6939): the one-line
	// notice sent alongside a KEYBOARD_VERSION re-push so it isn't a silent
	// mystery message.
	KBRefreshNote Str
	// SelfServiceBtn — SELFSERVICE_BTN (telegram_bot.py:6919): per-language
	// label of the Self-Service reply-keyboard button.
	SelfServiceBtn Str
	// SellBtn — SELL_BTN (telegram_bot.py:818): per-language label of the "Sell
	// AWS Account" reply-keyboard button.
	SellBtn Str
	// Commands — COMMANDS_BY_LANG (telegram_bot.py:359-384) transposed: one
	// entry per command in menu order, each carrying its trilingual description.
	// The per-language menu shown to Telegram (setMyCommands) and the reply
	// keyboard's business-button labels both derive from this single source, so
	// labels and routing can never drift (handlers-go.md §6).
	Commands []Command
	// Buy — the M2.2 catalog-browse + purchase-pipeline string cluster, in its
	// own nested struct (catalog_buy.go) per the "dedicated catalog_<feature>.go"
	// convention. Reachable from Catalog so the reflection-driven white-label
	// rewrite + completeness test cover it automatically.
	Buy BuyCatalog
	// Deposit / Info / History — the M2.3 (deposits + /info + /history) string
	// clusters, in catalog_deposit.go. Same nested-struct convention as Buy.
	Deposit DepositCatalog
	Info    InfoCatalog
	History HistoryCatalog
	// APIKey — the M2.5 API-key-family cluster (both mint SKUs, recharge,
	// My-Keys, self-service group refund), in catalog_apikey.go.
	APIKey APIKeyCatalog
	// Pay — the USDT rail cluster (Binance Pay + BEP20/TRON on-chain screens and
	// the whole verify-by-tx-hash TXID_* family), in catalog_pay.go.
	Pay PayCatalog
	// Customer — the M2.6 customer self-service cluster (Self-Service submenu,
	// refund picker, password reset, account deletion, promo code prompts),
	// in catalog_customer.go.
	Customer CustomerCatalog
	// OTP — the M2.6 login-code cluster (the /otp + "Login code" suite: prompt,
	// ownership/batch/cooldown refusals, code + reset-link results, and the
	// session stop/keep confirmation), in catalog_otp.go.
	OTP OTPCatalog
}

// Command is one slash-command menu entry: the bare command word plus its
// trilingual description (the tappable button label). Mirrors one dict of
// COMMANDS_BY_LANG's per-language list (telegram_bot.py:360-383).
type Command struct {
	Cmd  string
	Desc Str
}

// DefaultSupportHandle is the source literal baked into the Support string —
// telegram_bot.py:93 DEFAULT_SUPPORT_HANDLE. It is both the pre-resolve
// fallback and the token the first white-label rewrite replaces, so
// appliedHandle starts here.
const DefaultSupportHandle = "@zsec_ducna"

// buildCatalog materializes the core catalog. Every value is copied verbatim
// from the cited Python constant so the tables stay byte-diffable against the
// source; the SAME emoji/prefix/spacing is preserved intentionally.
func buildCatalog() Catalog {
	return Catalog{
		// WELCOME (telegram_bot.py:501-505).
		Welcome: Str{
			EN: "👋 Welcome! Tap an option below:",
			VI: "👋 Chào mừng! Chọn một chức năng bên dưới:",
			ZH: "👋 欢迎！请点击下方选项：",
		},
		// PROMPT (telegram_bot.py:414).
		LangPrompt: Str{
			EN: "🌐 Choose a language:",
			VI: "🌐 Chọn ngôn ngữ:",
			ZH: "🌐 请选择语言：",
		},
		// CONFIRM (telegram_bot.py:415-417).
		LangConfirm: Str{
			EN: "✅ Switched to English.",
			VI: "✅ Đã chuyển sang Tiếng Việt.",
			ZH: "✅ 已切换为中文。",
		},
		// LANG_LABEL (telegram_bot.py:418).
		LangLabel: Str{
			EN: "🇬🇧 English",
			VI: "🇻🇳 Tiếng Việt",
			ZH: "🇨🇳 中文",
		},
		// SUPPORT (telegram_bot.py:508-512) — carries DefaultSupportHandle.
		Support: Str{
			EN: "🆘 Need help? Contact @zsec_ducna",
			VI: "🆘 Cần hỗ trợ? Liên hệ @zsec_ducna",
			ZH: "🆘 需要帮助？请联系 @zsec_ducna",
		},
		// MAINTENANCE (telegram_bot.py:682-686).
		Maintenance: Str{
			EN: "🛠️ The bot is under maintenance. Please check back in a few minutes. Thank you!",
			VI: "🛠️ Bot đang bảo trì. Vui lòng quay lại sau ít phút. Cảm ơn bạn!",
			ZH: "🛠️ 机器人正在维护中，请稍后再试。谢谢！",
		},
		// KB_REFRESH_NOTE (telegram_bot.py:6935-6939).
		KBRefreshNote: Str{
			EN: "🔄 Keyboard updated: self-service actions (history, info, login code, support, password reset) now live under the 🛠️ Self-Service button.",
			VI: "🔄 Bàn phím đã được cập nhật: các chức năng tự phục vụ (lịch sử, thông tin, mã đăng nhập, hỗ trợ, đặt lại mật khẩu) nay nằm trong nút 🛠️ Self-Service.",
			ZH: "🔄 键盘已更新：自助功能（历史、信息、登录码、客服、密码重置）现已移至 🛠️ Self-Service 按钮中。",
		},
		// SELFSERVICE_BTN (telegram_bot.py:6919).
		SelfServiceBtn: Str{
			EN: "🛠️ Self-Service",
			VI: "🛠️ Tự phục vụ",
			ZH: "🛠️ 自助服务",
		},
		// SELL_BTN (telegram_bot.py:818).
		SellBtn: Str{
			EN: "💰 Sell AWS Account",
			VI: "💰 Bán tài khoản AWS",
			ZH: "💰 出售 AWS 账户",
		},
		// COMMANDS_BY_LANG (telegram_bot.py:359-384), transposed to one entry
		// per command in the source's menu order.
		Commands: []Command{
			{Cmd: "buy", Desc: Str{EN: "🛒 Buy", VI: "🛒 Mua hàng", ZH: "🛒 购买"}},
			{Cmd: "deposit", Desc: Str{EN: "💳 Deposit", VI: "💳 Nạp tiền", ZH: "💳 充值"}},
			{Cmd: "history", Desc: Str{EN: "🧾 History", VI: "🧾 Lịch sử", ZH: "🧾 历史记录"}},
			{Cmd: "info", Desc: Str{EN: "👤 Customer Info", VI: "👤 Thông tin", ZH: "👤 客户信息"}},
			{Cmd: "otp", Desc: Str{EN: "🔑 Get OTP code", VI: "🔑 Lấy mã OTP", ZH: "🔑 获取 OTP"}},
			{Cmd: "language", Desc: Str{EN: "🌐 Language", VI: "🌐 Ngôn ngữ", ZH: "🌐 语言"}},
		},
		// M2.2 catalog + purchase strings (catalog_buy.go).
		Buy: buildBuyCatalog(),
		// M2.3 deposit / info / history strings (catalog_deposit.go).
		Deposit: buildDepositCatalog(),
		Info:    buildInfoCatalog(),
		History: buildHistoryCatalog(),
		// M2.5 API-key strings (catalog_apikey.go).
		APIKey: buildAPIKeyCatalog(),
		// USDT rail strings — Binance / BEP20 / TRON (catalog_pay.go).
		Pay: buildPayCatalog(),
		// M2.6 customer self-service cluster (catalog_customer.go).
		Customer: buildCustomerCatalog(),
		// M2.6 login-code / OTP cluster (catalog_otp.go).
		OTP: buildOTPCatalog(),
	}
}

// DescFor returns the trilingual description of command cmd, or a zero Str if
// the command is not a menu command. Used by render.go to label the reply
// keyboard's business buttons from the same source as the menu.
func (c *Catalog) DescFor(cmd string) Str {
	for _, cm := range c.Commands {
		if cm.Cmd == cmd {
			return cm.Desc
		}
	}
	return Str{}
}

// ---- fixed multilingual button labels (telegram_bot.py:6994-7026) ---------
//
// These labels are the SAME string in every language on purpose: a user stuck
// in a language they can't read must still be able to find them. They contain
// no support @handle, so they are NOT catalog fields (nothing to rewrite) —
// plain consts shared by render.go's keyboard builder and its labelToCmd map.

const (
	// LangButton — LANG_BUTTON (telegram_bot.py:7008): the fixed language button.
	LangButton = "🌐 Language / Ngôn ngữ / 语言"
	// SupportButton — SUPPORT_BUTTON (telegram_bot.py:7013): the fixed support
	// button.
	SupportButton = "🆘 Support / Hỗ trợ / 客服"
	// CodeBtn — CODE_BTN (telegram_bot.py:680): the "Login code" button label,
	// mapped to the getcode command by legacy keyboards.
	CodeBtn = "🔑 Login code"
	// AdminButton — ADMIN_BUTTON (telegram_bot.py:6994): the admin-only reply
	// button (only ever shown to ADMIN_ID).
	AdminButton = "🔑 Admin"
	// SelfServiceBtnLegacy — SELFSERVICE_BTN_LEGACY (telegram_bot.py:6923): the
	// pre-v5 fixed trilingual Self-Service label, kept ONLY so a customer whose
	// cached keyboard still shows it can still tap it (resolves to selfservice).
	SelfServiceBtnLegacy = "🛠️ Self-Service / Tự phục vụ / 自助服务"
)
