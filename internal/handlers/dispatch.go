package handlers

import (
	"context"
	"log"
	"maps"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/refund"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// TG is the subset of *tg.Client the handlers call. Narrowing to an interface
// lets tests drive the dispatch with a recording fake (handlers-go.md §7: the
// handler doc owns the fake-Telegram harness) while production wires the real
// client, which satisfies this interface as-is.
type TG interface {
	SendMessage(ctx context.Context, p tg.SendMessageParams) (*tg.Message, error)
	EditMessageText(ctx context.Context, p tg.EditMessageTextParams) (*tg.Message, error)
	AnswerCallbackQuery(ctx context.Context, p tg.AnswerCallbackQueryParams) error
	DeleteMessage(ctx context.Context, chatID, messageID int64) error
	SetMyCommands(ctx context.Context, commands []tg.BotCommand, scope *tg.BotCommandScope) error
	GetChat(ctx context.Context, chatID int64) (*tg.Chat, error)
	// SendDocument uploads a local file (order delivery, _deliver_paid @7817).
	SendDocument(ctx context.Context, chatID int64, filePath, caption string) (*tg.Message, error)
	// SendPhotoURLOpts posts a photo by URL with parse_mode + reply_markup — the
	// BEP20/TRON pay screens attach the "I have made the payment" button to the
	// QR photo and render the address as a Markdown code block
	// (_send_bep20_qr @9457, _send_tron_qr @9907). The plain-SendPhotoURL variant
	// (no options) was dropped along with SePay and Binance Pay, its only
	// callers (S1/S3).
	SendPhotoURLOpts(ctx context.Context, p tg.SendPhotoURLParams) (*tg.Message, error)
	// SendPhoto uploads a LOCAL image file — the *_QR_PATH fallback the
	// BEP20/TRON screens use when no hosted QR URL is configured
	// (send_photo_file @10015, called from the two on-chain QR senders).
	SendPhoto(ctx context.Context, chatID int64, filePath, caption string) (*tg.Message, error)
	// GetFileBytes downloads an uploaded document's raw content by file_id —
	// the 📦 Stock admin category's upstock/provpool file intercepts
	// (adminstock.go) are the first callers that need actual bytes rather
	// than just a re-sendable file_id (tg/client.go's own doc comment on why
	// this method is new, not a refactor).
	GetFileBytes(ctx context.Context, fileID string) ([]byte, error)
}

// Deps bundles everything Bot needs so New can build an isolated instance (no
// package-level mutable state, handlers-go.md §1). Store/Config may be nil in
// unit tests that exercise only the dispatch/keyboard logic.
type Deps struct {
	TG          TG
	Store       *store.Store
	Stock       *stock.Store  // folder-state stock machine (accounts.py) — M2.2 buy flow
	RefundStore *refund.Store // customer refund request store (refund_store.py) — M2.6
	Config      *config.Config
	// Bands is the ONE cross-provider unique-amount registry shared by the
	// Binance, BEP20 and TRON rails. Injected (not constructed here) because the
	// cross-provider collision guarantee only holds while every rail consults the
	// SAME instance — see internal/pay/bands deviation 1. nil is tolerated for
	// unit tests that never touch a USDT rail; New then builds a default one so
	// the rails still work in an isolated Bot.
	Bands   *bands.Registry
	AdminID string          // ADMIN_ID (telegram_bot.py:76) — the owner / super admin
	DevIDs  map[string]bool // DEV_IDS (telegram_bot.py:84) — implicit full admins
	Banned  map[string]bool // banned_chats snapshot (telegram_bot.py:6555)
}

// Bot holds handler dependencies plus the per-chat state registry and the
// globals that were Python module state (banned cache, chat_handles cache, the
// standard-admin set). Everything hangs off Bot so tests are isolated
// (handlers-go.md §1).
type Bot struct {
	tg          TG
	store       *store.Store
	stock       *stock.Store
	refundStore *refund.Store // customer refund-request store (refund_store.py) — M2.6
	cfg         *config.Config
	states      *States

	// workers tracks the per-order expiry goroutines (order_expiry_worker @8974)
	// so Shutdown can drain them on exit (handlers-go.md §3/§8).
	workers *workerRegistry

	// Product-click anti-spam limits (_product_click_allowed @12191) + the order
	// timing constants (DEPOSIT_TIMEOUT_S / RESERVE_TTL_S @10542-10550). Set from
	// env in New (defaults when unset); overridable by tests for fast expiry.
	clickLimit    int
	clickWindow   time.Duration
	clickCooldown time.Duration
	expiryTimeout time.Duration
	reserveTTL    time.Duration
	depositPoll   time.Duration // POLL_INTERVAL @10548 — seconds between deposit polls

	// ---- USDT rails (BEP20 + TRON; Binance Pay removed, S3) -----------------
	//
	// bands is the ONE cross-provider unique-amount registry. Every rail reserves
	// through it, so a live BEP20 amount also blocks the same TRON amount
	// (bands deviation 1). Sharing ONE instance is the whole guarantee — never
	// build a per-rail registry.
	bands *bands.Registry
	// usdtMu guards usdtRows. It is a LEAF lock: never held across a chain RPC, a
	// Telegram call, a DB query, or a bands.Registry call.
	usdtMu sync.Mutex
	// usdtRows is the rail-side half of a pending session — the fields the bands
	// Reservation deliberately does not carry (bands/registry.go: "minus the
	// rail-specific timestamps which stay with the rails"). Keyed provider -> des.
	usdtRows map[bands.Provider]map[string]*usdtRow

	// usdtPollMu guards usdtPolls AND the per-chat st.USDTPolling test-and-set, so
	// "one confirm-poll per chat, at most USDT_MAX_POLLS globally" is decided in
	// ONE atomic step (_usdt_polling @9856-9860 + USDT_MAX_POLLS @9853).
	usdtPollMu   sync.Mutex
	usdtPolls    int
	maxUSDTPolls int

	// USDT timing knobs (telegram_bot.py:9541-9558). Fields so tests can shrink
	// them; New reads the env defaults.
	expirySweepInterval time.Duration // EXPIRY_SWEEP_INTERVAL 120s
	usdtPendingGrace    time.Duration // USDT_PENDING_GRACE_S 2h
	usdtVerifyGrace     time.Duration // USDT_VERIFY_GRACE_S 120s
	usdtConfirmWait     time.Duration // USDT_CONFIRM_WAIT_S 300s
	usdtConfirmPoll     time.Duration // USDT_CONFIRM_POLL_S 20s

	// Periodic janitor intervals — wired to the worker registry's tickers in
	// reconcile.go / stocksweep.go. Fields (not constants) so tests can shrink
	// them to 1ms; New reads the env defaults. See reconcile.go and
	// stocksweep.go for the money-safety rationale behind each default.
	reconcileInterval  time.Duration // RECONCILE_STUCK_INTERVAL_S 600s (Python */10 crontab)
	stockSweepInterval time.Duration // STOCK_SWEEP_INTERVAL_S 300s (folder-stock reclaim)

	// reconcileAlertMu guards the owner-alert de-duplication state for the
	// stuck-paid reconcile pass (reconcile.go's reconcileShouldAlert): the
	// last (count, oldestTS) that actually reached the owners, and when. See
	// reconcileShouldAlert's doc comment for the suppress/escalate policy.
	reconcileAlertMu     sync.Mutex
	reconcileAlertCount  int64
	reconcileAlertOldest float64
	reconcileAlertSentAt time.Time
	// auditPruneInterval is the audit-retention janitor's ticker cadence
	// (auditprune.go). Fixed at 24h by New() — unlike its siblings above,
	// deliberately NOT env-overridable: pruning is an irreversible delete of
	// the investigative record, and there is no benefit to running it more
	// than once a day, so no operator knob is offered for the cadence
	// itself (AUDIT_RETENTION_DAYS, the actual retention window, is the
	// intended knob — see auditprune.go). A field, not a constant, purely so
	// tests can shrink it to 1ms like its siblings.
	auditPruneInterval time.Duration

	// now is the clock, overridable in tests (band deadlines and the verify window
	// are wall-clock so they survive a restart, exactly like Python's time.time()).
	now func() time.Time

	// Chain/exchange seams. Same rationale as paypal*: tests drive the rails
	// with fakes (or httptest-backed real clients) instead of live chain RPC,
	// while New binds the real internal/pay/* clients.
	bep20Configured func() bool
	bep20ByHash     func(txhash string) (*bep20.TransferByHash, error)
	bep20UsdtToVND  func(usdt *big.Rat) (int64, error)

	tronConfigured func() bool
	tronByHash     func(txhash string, minTSMS int64) (*tron.Transfer, error)
	tronUsdtToVND  func(usdt *big.Rat) (int64, error)

	// ---- login-code (OTP) suite (M2.6, otp.go) ------------------------------
	//
	// otpSem is the GLOBAL ceiling on concurrent fetch subprocesses across ALL
	// chats — the Go twin of Python's `threading.BoundedSemaphore(OTP_MAX_CONCURRENCY)`
	// (@680). A buffered channel so the acquire can carry a timeout (fail fast,
	// never queue: a queued fetch would hold its chat's session lock the whole
	// time). ONE per Bot: a per-chat semaphore would not bound host load at all.
	otpSem chan struct{}
	// otpFetch runs the server-side OTP / reset-link lookup for one username,
	// returning (code, resetURL) with either or both possibly empty. A field for
	// the same reason sepayFind is one: tests drive the whole suite without ever
	// spawning node. New binds it to otpFetchNode.
	//
	// NEVER log or forward what this returns — both values are live credentials
	// for a sold account (CLAUDE.md).
	otpFetch func(ctx context.Context, username string) (code, reset string, err error)
	// OTP timing knobs — the Python constants (@643, @677, @8283, @8288) as
	// fields so tests can shrink them; New applies the shipped defaults.
	otpCooldown     time.Duration // OTP_COOLDOWN_S 15s
	otpSessionMax   time.Duration // OTP_SESSION_MAX_S 240s (stale-session watchdog)
	otpSemAcquire   time.Duration // subprocess-slot acquire budget 45s
	otpFetchTimeout time.Duration // node subprocess budget 90s

	adminID string
	devIDs  map[string]bool

	adminsMu sync.RWMutex
	admins   map[string]bool // extra "standard" admins (admins.json) — populated in M3.1

	bannedMu sync.RWMutex
	banned   map[string]bool // in-memory ban gate (telegram_bot.py:6555)

	handlesMu sync.Mutex
	handles   map[int64]string // chat_handles cache (telegram_bot.py:6548)

	chatNamesMu sync.Mutex
	chatNames   map[int64]string // chat_names cache (telegram_bot.py:6543)

	chatLangMu sync.Mutex
	chatLang   map[int64]i18n.Lang // chat_lang runtime map (telegram_bot.py:6539)

	// Ordered dispatch stages. Each is append-only; later slices attach their
	// entries and the golden-order test pins the ordering. Empty in M2.1 but
	// fully plumbed so steps 5/9/10 run without special-casing.
	fileIntercepts []stage // step 5: document/photo intercepts (@16942-17091)
	adminText      []stage // step 9: admin text routing (@17123-17215)
	captureChain   []stage // step 10: text-capture chain (@17220-18693)

	// cbRoutes is the ordered callback dispatch table (handle_callback prefix
	// chain @12287-15697): first-match-wins, appended by each slice. A golden
	// test can pin the order. M2.2 registers the customer buy-pipeline routes.
	cbRoutes []cbRoute

	// ---- 🧾 ORDERS / refund subsystem (adminorders.go) ----------------------

	// refundAsOfMu guards refundAsOfByDes — the Go home of telegram_bot.py's
	// module-level _refund_asof_by_des dict (@4468): a FROZEN warranty-clock
	// snapshot set by refappr: (a customer-request approval) and consumed
	// exactly once by the refund executor, so admin-review delay can never
	// shrink an already-approved payout. Deliberately process-wide (keyed by
	// order des, not by chat) exactly like the Python dict — a des is globally
	// unique, so this needs no per-chat home.
	refundAsOfMu    sync.Mutex
	refundAsOfByDes map[string]float64

	// upstockRenderThrottle is UPSTOCK_RENDER_THROTTLE (py_stock_spec.md §1.7)
	// — how long the running upstock preview's render loop sleeps between
	// passes after its first (immediate) iteration, coalescing a burst of
	// pastes/file uploads into one edited message instead of one per input. A
	// Bot field (not a bare const) so tests can shrink it, the same pattern
	// as every other timing knob on this struct (otpCooldown, usdtConfirmPoll, …).
	upstockRenderThrottle time.Duration
}

// Ctx is the per-update working context threaded through the dispatch stages,
// so a stage func has everything it needs without re-deriving it. Fields are
// populated progressively as Process advances (Text/Lang/st are only set once
// past the no-text gate).
type Ctx struct {
	Ctx     context.Context
	ChatID  int64
	Msg     *tg.Message
	IsAdmin bool
	Text    string
	Lang    i18n.Lang
	st      *ChatState
}

// stage is one ordered dispatch entry: a name (for the golden-order test) and a
// handler returning true when it CONSUMED the update (first-match-wins, stop).
type stage struct {
	name string
	fn   func(b *Bot, c *Ctx) bool
}

// New builds a Bot from Deps, copying the mutable maps so the caller can't
// mutate Bot's state behind its locks. The ordered stages start empty; later
// slices append to them via the registration helpers.
func New(d Deps) *Bot {
	b := &Bot{
		tg:          d.TG,
		store:       d.Store,
		stock:       d.Stock,
		refundStore: d.RefundStore,
		cfg:         d.Config,
		states:      NewStates(),
		workers:     newWorkerRegistry(),
		adminID:     d.AdminID,
		devIDs:      copyBoolMap(d.DevIDs),
		// admins is the standard-admin set — loaded from admins.json
		// (adminusers.go's loadAdminsFile) so a restart doesn't drop every
		// grant (store.py-equivalent persistence: add_admin/remove_admin,
		// telegram_bot.py:150-172). Reading a missing/corrupt file yields an
		// empty set, matching Python's load_admins bare-except default.
		admins:          loadAdminsFile(),
		banned:          copyBoolMap(d.Banned),
		handles:         map[int64]string{},
		chatNames:       map[int64]string{},
		chatLang:        map[int64]i18n.Lang{},
		refundAsOfByDes: map[string]float64{},
	}
	// Product-click limits + order timers — env-tunable (telegram_bot.py:12183-12185,
	// 10542-10550), defaults applied when unset or no config is wired (unit tests).
	// max(1, …): a 0 limit would otherwise lock every customer out (@12183).
	b.clickLimit = max(b.envInt("PRODUCT_CLICK_LIMIT", 10), 1)
	b.clickWindow = time.Duration(b.envInt("PRODUCT_CLICK_WINDOW_S", 60)) * time.Second
	b.clickCooldown = time.Duration(b.envInt("PRODUCT_CLICK_COOLDOWN_S", 60)) * time.Second
	// DEPOSIT_TIMEOUT_S=600 hard payment limit; RESERVE_TTL_S=600+120 slack.
	b.expiryTimeout = 600 * time.Second
	b.reserveTTL = (600 + 120) * time.Second
	b.depositPoll = 10 * time.Second // POLL_INTERVAL @10548
	// ---- USDT rails ---------------------------------------------------------
	b.now = time.Now
	// ONE shared registry. A caller that does not inject one (unit tests) gets a
	// default built from the two rails' own env-backed geometry — still ONE
	// instance spanning both providers, so the cross-provider collision
	// guarantee holds either way (bands deviation 1). Binance Pay was removed
	// by the USDT-only migration (S3); the registry itself needs no change —
	// it never had provider-count baked into its collision logic.
	b.bands = d.Bands
	if b.bands == nil {
		reg, err := bands.New(bands.Config{Providers: map[bands.Provider]bands.Params{
			bands.ProviderBEP20: {Step: bep20.AmountOffsetStep(), Tol: bep20.AmountMatchTol(), Decimals: bep20.AmountDecimals()},
			bands.ProviderTron:  {Step: tron.AmountOffsetStep(), Tol: tron.AmountMatchTol(), Decimals: tron.AmountDecimals()},
		}})
		if err != nil {
			// Misconfigured band geometry (e.g. a hand-set step of 0) would let two
			// live orders share an amount — a money-safety failure, not a cosmetic
			// one. Fall back to the SHIPPED defaults rather than run without a
			// registry: the rails then price with known-good geometry and the
			// operator sees the log line.
			log.Printf("bands: invalid env geometry (%v) — falling back to shipped defaults", err)
			reg, err = bands.New(bands.Config{Providers: map[bands.Provider]bands.Params{
				bands.ProviderBEP20: {Step: 21, Tol: 10, Decimals: 2},
				bands.ProviderTron:  {Step: 21, Tol: 10, Decimals: 2},
			}})
			if err != nil { // unreachable: the shipped defaults validate
				panic("handlers: shipped band defaults are invalid: " + err.Error())
			}
		}
		b.bands = reg
	}
	b.usdtRows = map[bands.Provider]map[string]*usdtRow{
		bands.ProviderBEP20: {},
		bands.ProviderTron:  {},
	}
	// Timing knobs — Python's module constants, env-overridable (telegram_bot.py:9541-9558).
	// envDuration (not a bare envInt*time.Second) so an operator-configured 0
	// or negative value is clamped to a sane floor and logged rather than
	// firing time.After immediately — see envDuration's own doc comment and
	// the per-key *MinInterval comments above (task #30).
	b.expirySweepInterval = b.envDuration("EXPIRY_SWEEP_INTERVAL", 120*time.Second, expirySweepMinInterval)
	// RECONCILE_STUCK_INTERVAL_S default 600s = Python's reconcile crontab
	// `*/10 * * * *` (reconcile_stuck_orders.py:99-107). The pass is read-only
	// (reconcile.go), so a tighter interval is cheap and harmless; the default
	// matches Python to keep alarm cadence comparable across the ports.
	b.reconcileInterval = b.envDuration("RECONCILE_STUCK_INTERVAL_S", 600*time.Second, reconcileMinInterval)
	// STOCK_SWEEP_INTERVAL_S default 300s: folder-stock expiry is a cheap
	// directory walk per product (stock.go:254), so a 5-min cadence keeps
	// stock counts honest (abandoned reservations reclaimed to active/
	// promptly) without burdening the DB-order sweep path. See stocksweep.go.
	b.stockSweepInterval = b.envDuration("STOCK_SWEEP_INTERVAL_S", 300*time.Second, stockSweepMinInterval)
	// auditPruneInterval: fixed 24h, not env-tunable — see the field's own
	// doc comment above for why.
	b.auditPruneInterval = 24 * time.Hour
	b.usdtPendingGrace = time.Duration(b.envInt("USDT_PENDING_GRACE_S", 2*3600)) * time.Second
	b.usdtVerifyGrace = time.Duration(b.envInt("USDT_VERIFY_GRACE_S", 120)) * time.Second
	b.usdtConfirmWait = time.Duration(b.envInt("USDT_CONFIRM_WAIT_S", 300)) * time.Second
	b.usdtConfirmPoll = b.envDuration("USDT_CONFIRM_POLL_S", 20*time.Second, usdtConfirmPollMinInterval)
	b.maxUSDTPolls = max(b.envInt("USDT_MAX_POLLS", 200), 1)
	// Chain/exchange clients. The receiving address is resolved LIVE on every
	// call through b.bep20AddrStr()/b.tronAddrStr() (settings.json's admin-set
	// address overriding .env/ambient, config.Config.BEP20Address()/
	// TRONAddress()) rather than read by the pay packages themselves from raw
	// env — task #17: the admin address wizards (capSetBep20/capSetTron,
	// adminsettings.go) only ever write settings.json, and internal/pay/bep20
	// and internal/pay/tron deliberately do not import internal/config (see
	// their Address() doc comments), so this closure is the one place that
	// bridges the two. The pre-fix binding (`b.bep20Configured =
	// bep20.Configured`) was ALSO re-evaluated per call — a Go func value
	// doesn't freeze anything at assignment — so the bug was never about
	// staleness from binding-once; it was that every one of those per-call
	// reads consulted the wrong source (raw os.Getenv, which never sees a
	// settings.json write) instead of this getter.
	b.bep20Configured = func() bool { return bep20.ValidAddress(b.bep20AddrStr()) }
	b.bep20ByHash = func(txhash string) (*bep20.TransferByHash, error) {
		return bep20.GetTransferToSellerByHash(b.bep20AddrStr(), txhash)
	}
	b.tronConfigured = func() bool { return tron.ValidAddress(b.tronAddrStr()) }
	// b.tronAPIKeyStr() is read per call, same as the address above — task
	// #16: the raw os.Getenv("TRON_API_KEY") apiGet used to read only ever
	// saw the ambient environment, never .env, so TronGrid calls went out
	// unauthenticated on the deposit-verification path. config.Config.
	// TRONAPIKey() is the bridge; internal/pay/tron stays free of an
	// internal/config import (see apiGet's doc comment).
	b.tronByHash = func(txhash string, minTSMS int64) (*tron.Transfer, error) {
		return tron.FindConfirmedTransferByHash(b.tronAddrStr(), txhash, minTSMS, b.tronAPIKeyStr())
	}
	// The USD->VND rate is bridged the same way, and for the same reason —
	// task #22, another instance of this port keeping a config reader while
	// dropping the config source. These two converters value the amount a
	// customer is CREDITED for an on-chain payment; the amount that same
	// customer was QUOTED comes from vndToUSD/usdtDisp/usdtToVND, which all
	// resolve b.cfg.USDToVND(). Before this binding the converters read
	// os.LookupEnv("USD_TO_VND") instead, so the two sides could disagree in
	// two ways: an admin running adm:setrate moved the quote and left
	// crediting on the environment value, and a process with no USD_TO_VND set
	// at all quoted at config.DefaultUSDToVND while crediting returned an
	// error, which verifyUSDTTxid turns into a re-arm — a refusal to credit a
	// payment that had already been made at a correctly quoted amount.
	//
	// The rate concept itself is slated to disappear in the USDT migration's
	// slice S5, when the ledger flips to micro-USDT and there is nothing left
	// to convert (see vndToUSD in usdt.go, which S5 deletes outright). Until
	// that lands, the two sides have to agree.
	b.bep20UsdtToVND = func(usdt *big.Rat) (int64, error) {
		return bep20.UsdtToVND(usdt, b.usdRateStr())
	}
	b.tronUsdtToVND = func(usdt *big.Rat) (int64, error) {
		return tron.UsdtToVND(usdt, b.usdRateStr())
	}

	// ---- login-code (OTP) suite (M2.6) --------------------------------------
	// ONE semaphore for the whole process (telegram_bot.py:680) and the real node
	// runner as the default fetch seam.
	b.otpSem = make(chan struct{}, otpMaxConcurrency)
	b.otpFetch = b.otpFetchNode
	b.otpCooldown = otpCooldown
	b.otpSessionMax = otpSessionMax
	b.otpSemAcquire = otpSemAcquire
	b.otpFetchTimeout = otpFetchTimeout

	// 📦 Stock admin category (adminstock.go): the upstock preview render
	// loop's throttle, shipped default 2s (py_stock_spec.md §1.7's literal
	// UPSTOCK_RENDER_THROTTLE=2.0).
	b.upstockRenderThrottle = 2 * time.Second

	// Register the M2.2 catalog + purchase capture(s) and callback routes, then
	// the M2.3 deposit / order-pay-by-QR / history captures + routes.
	b.registerCatalog()
	b.registerDeposit()
	// M2.5: the API-key family (both mint SKUs + recharge + reveal + the
	// self-service group refund). Registered last so its captures can be
	// PREPENDED into their canonical slots (#45/#46) ahead of pending_purchase.
	b.registerAPIKey()
	// USDT rails last: pending_txid is capture #43, ahead of every capture the
	// earlier slices registered (#44+), so registerUSDT splices it in before
	// "deposit" and the chain stays an in-order subsequence of the canonical list.
	b.registerUSDT()
	// M2.6 login-code suite last: pending_code is capture #36, ahead of every
	// capture registered above (#43+), so registerOTP splices it in before "txid"
	// and the chain stays an in-order subsequence of the canonical list.
	b.registerOTP()
	// M2.6 promo/referral code capture: pending_ordercode is #47, between
	// apikeyrc (#46) and purchase (#48). Spliced in before "purchase" so the
	// chain stays an in-order subsequence of CanonicalCaptureOrder.
	b.registerCodeCapture()
	// M3.1 first cut: the admin panel entry point. Registers the two step-9 text
	// stages (🔑 Admin button + the panel slash commands) and the panel's callback
	// routes. Without this the Admin button rendered but nothing listened, so a
	// tap fell through every stage to the step-12 no-op (admin.go header).
	b.registerAdmin()
	// ⚙️ SETTINGS + 💳 PAYMENT METHODS (adminsettings.go). Its 7 captures
	// (setrate/setkirourl/setkirokey/setkiroacct/setbep20/settron/setmaxqty)
	// are canonically ordered well before "code" (captures.go #10-#23 vs
	// #37-ish), so it must register after registerCodeCapture has landed
	// "code" — insertCapturesBefore("code", ...) finds "code" already in the
	// chain and splices this file's 7 captures ahead of it.
	b.registerAdminSettings()
	// 🏷️ PRICING & DISCOUNTS (adminpricing.go). Its captures (setapiprice
	// through setapicap, canonically #11-#17; discount/promo #34/#36;
	// tier/price #41/#42) splice ahead of anchors registerAdminSettings /
	// registerOTP / registerUSDT already landed ("setkirourl", "code",
	// "txid") — this MUST run last so those anchors already exist in the
	// chain.
	b.registerAdminPricing()
	// 👥 CUSTOMERS / 🛡️ ADMIN ROLES / whitelist / resellers (adminusers.go).
	// Runs LAST: its captures splice ahead of "setrate" (registered by
	// registerAdminSettings) and "promo" (registered by registerAdminPricing),
	// so both anchors must already exist in the chain — see
	// registerAdminUsers's own doc comment.
	b.registerAdminUsers()
	// 🧾 ORDERS + refund subsystem (adminorders.go). Its captures splice ahead
	// of "custlookup" (registerAdminUsers) and "discount" (registerAdminPricing)
	// — both anchors must already exist in the chain, so this runs LAST,
	// after every other admin slice.
	b.registerAdminOrders()
	// 📦 STOCK + pool/audit views (adminstock.go). Its captures splice ahead
	// of "getord" (registerAdminOrders' own first anchor — "upstock" is
	// canonical #1, earliest of all), "refund" ("newprod"/"renprod"/
	// "provpool" ahead of it, then "setnotice"/"setdelivery"/"editfile" ahead
	// of "refund" again — both groups anchor on registerAdminOrders' "refund"
	// capture, landing in registration order), and "tier" ("hold",
	// registerAdminPricing's anchor) — every one of those anchors must
	// already exist in the chain, so this runs absolutely LAST, after every
	// other admin slice registers.
	b.registerAdminStock()
	return b
}

// envInt reads an int env var via config, falling back to def when unset,
// blank, non-numeric, or no config is wired.
func (b *Bot) envInt(key string, def int) int {
	if b.cfg == nil {
		return def
	}
	v := strings.TrimSpace(b.cfg.Env(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Floors for the seconds-valued env knobs envDuration resolves below. Each
// backs one of the four periodic-janitor intervals wired in New(); the
// comment on each explains what a hot loop at that interval would actually
// contend, not just "burns CPU" (task #30).
const (
	// stockSweepMinInterval is STOCK_SWEEP_INTERVAL_S's floor — the
	// sharpest edge of the four. Its pass (sweepStockExpiredOnce,
	// stocksweep.go) calls stock.SweepExpired for EVERY product on EVERY
	// tick, and SweepExpired takes the SAME per-product mutex that Reserve
	// and Finalize take on the live customer buy path (stock.go's
	// lockProduct). Unlike expirySweepMinInterval below, this pass is not
	// scoped to orders that actually expired — it walks every product
	// regardless — so it gets the HIGHEST floor of the four: a
	// misconfiguration here can least afford to come close to a hot loop
	// against real purchases.
	stockSweepMinInterval = 10 * time.Second
	// reconcileMinInterval is RECONCILE_STUCK_INTERVAL_S's floor. The pass
	// (reconcileReport, reconcile.go) is read-only — one SELECT — but
	// store.db is a SINGLE SQLite connection shared with every write the
	// process makes, so a hot loop here still queues behind, and ahead of,
	// the writer on that one connection.
	reconcileMinInterval = 5 * time.Second
	// expirySweepMinInterval is EXPIRY_SWEEP_INTERVAL's floor — pre-existing
	// (predates task #30, unlike the two above). Each pass
	// (expirySweepOnce, usdt.go) reads store.ReservedOrdersBefore, then per
	// expired order writes via store.ClaimOrder and releases folder stock
	// via stock.Release — which takes the SAME per-product mutex as the
	// stock sweep above, just scoped to orders that actually expired
	// rather than every product on every tick.
	expirySweepMinInterval = 5 * time.Second
	// usdtConfirmPollMinInterval is USDT_CONFIRM_POLL_S's floor —
	// pre-existing (predates task #30). Each poll is an outbound chain RPC
	// call (BEP20/TRON), not a local lock or the shared DB connection, so
	// this floor can be the lowest of the four — but zero still means
	// hammering an external chain-explorer API on every scheduler tick,
	// risking a rate-limit ban on the very address the shop verifies
	// deposits against.
	usdtConfirmPollMinInterval = 2 * time.Second
)

// envDuration resolves a seconds-valued env var into a time.Duration through
// b.envInt — NEVER os.Getenv (see envInt's own contract: config.Load's
// frozen .env snapshot is never written back to the process environment,
// and production's systemd unit exports nothing beyond it, so a raw env
// read would see an empty string in production even with the key correctly
// set in .env — the exact mistake that produced five live defects earlier
// in this port, task #23's sweep) — then clamps the result UP to min.
//
// envInt's own contract is "fall back to def only when unset, blank, or
// non-numeric" — an explicit 0 parses fine and is returned as-is, and so
// does a negative value. Fed straight into time.After (every call site
// below), a non-positive duration fires IMMEDIATELY, turning a periodic
// janitor into a tight loop (task #30) — see the per-key floor comments
// above for what each loop actually contends when that happens.
//
// A clamp triggered by a genuinely CONFIGURED value is logged — def is
// always >= min by construction, so an unset/blank/non-numeric key (which
// resolves to def) never logs here. Mirrors auditprune.go's
// AUDIT_RETENTION_DAYS floor and its rationale: an operator who configured
// 0 (or a negative) and silently got min back cannot tell that apart from
// "it is working as configured."
func (b *Bot) envDuration(key string, def, min time.Duration) time.Duration {
	secs := b.envInt(key, int(def/time.Second))
	d := time.Duration(secs) * time.Second
	if d < min {
		log.Printf("%s=%ds is below the %s floor — using %s", key, secs, min, min)
		d = min
	}
	return d
}

// Shutdown drains the background expiry workers — called by cmd/bot after the
// poll loop stops so an in-flight expiry timer doesn't outlive the process.
func (b *Bot) Shutdown() { b.workers.Shutdown() }

func copyBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	maps.Copy(out, m)
	return out
}

// Process routes one update, mirroring process() (telegram_bot.py:16916-18720)
// step-for-step, first-match-wins. The numbered steps below match handlers-go.md
// §2. Steps 5/9/10 iterate their (empty-in-M2.1) ordered slices so later slices
// only append.
func (b *Bot) Process(ctx context.Context, u tg.Update) {
	// Step 1: a callback tap routes to the callback pipeline and nothing else
	// (@16918-16920).
	if u.CallbackQuery != nil {
		b.handleCallback(ctx, u.CallbackQuery)
		return
	}
	// Step 2: no message payload -> nothing to route (@16921-16923).
	msg := u.Message
	if msg == nil {
		return
	}
	chatID := msg.Chat.ID

	// Step 3: private-chat only. The bot is only ever DM'd; a non-private chat
	// type is ignored so a group add can't drive any flow (handlers-go.md §2
	// step 3; @16930's private-only assumption made explicit). An absent type
	// is treated as private (Telegram always sets it, but a hand-built test
	// update may omit it).
	if msg.Chat.Type != "" && msg.Chat.Type != "private" {
		return
	}

	// Step 4: banned-user gate. Drop EVERY update from a banned chat before any
	// handler runs; admins are never gated (they can't self-lock and must still
	// /unban). Silent on purpose — no reply (@16932-16938).
	admin := b.isAdmin(chatID)
	if b.isBanned(chatID) && !admin {
		return
	}

	c := &Ctx{Ctx: ctx, ChatID: chatID, Msg: msg, IsAdmin: admin}

	// Step 5: document/photo intercepts, in Python order (@16942-17091). These
	// run BEFORE the no-text gate because upload messages carry no "text".
	// Empty in M2.1; the plumbing + golden order let later slices append.
	for _, fi := range b.fileIntercepts {
		if fi.fn(b, c) {
			return
		}
	}

	// Step 6: text-only gate. A message with no text (and no intercept above)
	// is dropped (@17093-17094).
	if msg.Text == "" {
		return
	}
	text := strings.TrimSpace(msg.Text)
	c.Text = text

	// client language: the Telegram UI locale, used only as the lang_for
	// fallback for chats that never chose one (@17097).
	clientLang := "en"
	if msg.From != nil && msg.From.LanguageCode != "" {
		clientLang = msg.From.LanguageCode
	}

	// Step 7: handle + display-name capture. Persist the sender's @username only
	// on CHANGE (@17100-17106); the display name (first/last + @handle) is
	// refreshed in memory for the Customer Info card + sale notices (@17098-17107).
	b.captureHandle(chatID, msg.From)
	b.captureName(chatID, msg.From)

	lang := b.langFor(chatID, clientLang)
	c.Lang = lang
	st := b.states.Get(chatID)
	c.st = st

	// Step 8: re-push the reply keyboard once when KEYBOARD_VERSION changed,
	// UNLESS the text is /start… (its handler already pushes) or the chat is
	// mid free-text capture on custreset / code / custdelete — so the refresh
	// note never wedges between a customer's answer and its result
	// (@17116-17119).
	if !strings.HasPrefix(strings.ToLower(text), "/start") && !b.kbRefreshSkip(st) {
		b.ensureKeyboard(ctx, chatID, lang)
	}

	// Step 9: admin text routing (admins only) — @17123-17215. Empty in M2.1.
	if admin {
		for _, h := range b.adminText {
			if h.fn(b, c) {
				return
			}
		}
	}

	// Step 10: text-capture chain (~46 entries, exact order) — @17220-18693.
	// Empty in M2.1; TestCaptureChainOrder pins the ordering of whatever lands.
	for _, cap := range b.captureChain {
		if cap.fn(b, c) {
			return
		}
	}

	// Step 11: /command routing (alias map + label fallthrough) — @18695-18719.
	if strings.HasPrefix(text, "/") {
		cmd := parseSlashCommand(text)
		// /otp is intercepted HERE, before handleCommand, because it is the one
		// command that takes ARGUMENTS and handleCommand only ever receives the
		// bare command word — exactly as in Python, which special-cases /otp inside
		// process() @19416-19431 for the same reason. Everything else keeps its
		// current signature and behaviour.
		if cmd == "otp" {
			arg := ""
			if _, rest, found := strings.Cut(text, " "); found {
				arg = rest
			}
			b.cmdOTPSlash(ctx, chatID, lang, arg)
			return
		}
		b.handleCommand(ctx, chatID, cmd, clientLang)
		return
	}
	// A tapped reply-keyboard button arrives as its plain-text label.
	if cmd := labelToCmd(text); cmd != "" {
		b.handleCommand(ctx, chatID, cmd, clientLang)
		return
	}
	// Step 12: unknown plain text -> no-op (@18717 returns for a non-button).
}

// parseSlashCommand extracts the bare command word from a slash message:
// "/cmd@bot arg" -> "cmd", lowercased. Port of
// `text[1:].split()[0].split("@")[0].lower()` (telegram_bot.py:18697).
func parseSlashCommand(text string) string {
	body := strings.TrimPrefix(text, "/")
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return ""
	}
	word := fields[0]
	if i := strings.IndexByte(word, '@'); i >= 0 {
		word = word[:i]
	}
	return strings.ToLower(word)
}

// kbRefreshSkip reports whether the KEYBOARD_VERSION re-push must be skipped
// because the chat is mid free-text capture on custreset / code / custdelete
// (telegram_bot.py:17117-17118). Read under st.mu (§8: never hold across I/O —
// this only reads three pointers).
func (b *Bot) kbRefreshSkip(st *ChatState) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.CustReset != nil || st.Code != nil || st.CustDelete != nil
}

// captureHandle records the sender's @username in the in-memory cache and
// persists it only when it CHANGED (telegram_bot.py:17100-17106). from may be
// nil (some update shapes omit it) — then there's nothing to capture.
func (b *Bot) captureHandle(chatID int64, from *tg.User) {
	if from == nil || from.Username == "" {
		return
	}
	handle := "@" + from.Username
	b.handlesMu.Lock()
	changed := b.handles[chatID] != handle
	if changed {
		b.handles[chatID] = handle
	}
	b.handlesMu.Unlock()
	if changed && b.store != nil {
		if err := b.store.SetHandle(strconv.FormatInt(chatID, 10), handle); err != nil {
			log.Println("set_handle failed:", err)
		}
	}
}

// captureName refreshes the sender's display name in the in-memory cache —
// port of telegram_bot.py:17098-17107. The name is first+last, with the
// @handle appended in parens when present (or the bare handle when there is no
// name); it is stored only when non-empty (@17106-17107). Runtime-only, like
// chat_names (@6543) — the Customer Info card + sale notices read it.
func (b *Bot) captureName(chatID int64, from *tg.User) {
	if from == nil {
		return
	}
	nm := strings.TrimSpace(strings.TrimSpace(from.FirstName + " " + from.LastName))
	if from.Username != "" {
		handle := "@" + from.Username
		if nm != "" {
			nm = nm + " (" + handle + ")"
		} else {
			nm = handle
		}
	}
	if nm == "" {
		return
	}
	b.chatNamesMu.Lock()
	b.chatNames[chatID] = nm
	b.chatNamesMu.Unlock()
}

// nameFor returns the cached display name for a chat, or "" — the read side of
// captureName (chat_names.get, telegram_bot.py:11763). Exposed for later slices
// (Customer Info card, sale notices).
func (b *Bot) nameFor(chatID int64) string {
	b.chatNamesMu.Lock()
	defer b.chatNamesMu.Unlock()
	return b.chatNames[chatID]
}

// ---- admin / ban / lang helpers -----------------------------------------

// isSuperAdmin reports a FULL admin: the owner (ADMIN_ID) or a hard-coded dev
// override (DEV_IDS). Port of is_super_admin (telegram_bot.py:130-136).
func (b *Bot) isSuperAdmin(chatID int64) bool {
	cid := strconv.FormatInt(chatID, 10)
	return b.devIDs[cid] || cid == b.adminID
}

// isAdmin reports a full admin OR a granted "standard" admin. Port of is_admin
// (telegram_bot.py:139-146). The standard-admin set is loaded from admins.json
// by New() (loadAdminsFile, adminusers.go) and can be non-empty for real —
// comment corrected by Opus review: an earlier draft of this comment claimed
// the set was "empty until M3.1", which was stale the moment admins.json
// loading landed and is exactly the kind of sentence that makes a capability
// gap invisible on a skim.
func (b *Bot) isAdmin(chatID int64) bool {
	if b.isSuperAdmin(chatID) {
		return true
	}
	cid := strconv.FormatInt(chatID, 10)
	b.adminsMu.RLock()
	defer b.adminsMu.RUnlock()
	return b.admins[cid]
}

// isBanned reports whether the chat is in the in-memory ban gate
// (telegram_bot.py:16937 `str(chat_id) in banned_chats`).
func (b *Bot) isBanned(chatID int64) bool {
	cid := strconv.FormatInt(chatID, 10)
	b.bannedMu.RLock()
	defer b.bannedMu.RUnlock()
	return b.banned[cid]
}

// langFor returns the chat's chosen language, or English — the runtime-only
// chat_lang lookup (telegram_bot.py:6896-6906). The Telegram client locale is
// no longer auto-applied (Python dropped it), so the fallback arg only matters
// when no explicit choice exists; English wins regardless of a non-supported
// client locale.
func (b *Bot) langFor(chatID int64, _ string) i18n.Lang {
	b.chatLangMu.Lock()
	defer b.chatLangMu.Unlock()
	if l, ok := b.chatLang[chatID]; ok {
		return l
	}
	return i18n.EN
}

// setLang records a chat's language choice (setlang: callback, @15027).
func (b *Bot) setLang(chatID int64, lang i18n.Lang) {
	b.chatLangMu.Lock()
	b.chatLang[chatID] = lang
	b.chatLangMu.Unlock()
}
