// Command bot is the entry point for the Go port of the Kiro Telegram shop
// bot. It wires the config, store, Telegram client, i18n white-label rewrite,
// the handlers.Bot dispatcher, and the worker pool + long-poll loop — the port
// of main() (telegram_bot.py:18946-19039).
//
// M2.1 scope (handlers-go.md §7 row M2.1): boot far enough to run the dispatch
// core against Telegram — config + boot guards, support-handle apply, the
// 16-worker pool with per-update panic recovery (safe_process parity @19021),
// the getUpdates poll loop feeding handlers.Process, and signal-driven graceful
// shutdown. Payment webhooks, sweepers and startup recovery threads are wired
// by their own later slices.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/dbfile"
	"codezdev-bot/internal/handlers"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/pay/bands"
	"codezdev-bot/internal/pay/bep20"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/refund"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// DefaultAdminID is the owner chat id frozen into this build — telegram_bot.py:76
// `ADMIN_ID = os.environ.get("ADMIN_ID", "7302701741")`. It is the "bound"
// value the multi-tenant boot guard compares the .env against.
const DefaultAdminID = "7302701741"

// storeDBPath is the SQLite store filename (Python opens store.db in the script
// directory).
const storeDBPath = "store.db"

// accountsDir / ordersDir are the folder-state stock roots next to the binary,
// mirroring Python's ACCOUNTS_DIR / ORDERS_DIR (accounts.py:89-91).
const (
	accountsDir = "accounts"
	ordersDir   = "orders"
)

// poolSize mirrors ThreadPoolExecutor(max_workers=16) (telegram_bot.py:19019).
const poolSize = 16

func main() {
	// Before ANY file is opened: make owner-only the default for everything
	// this process (and its login subprocesses) creates. Backstop only — the
	// explicit modes in internal/stock and internal/dbfile are the real fix.
	hardenUmask()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("bot: ")

	// Config snapshot (.env merged under ambient env) + live settings view.
	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	// Owner id: env wins over the compiled default, exactly like Python's
	// import-time bind `ADMIN_ID = os.environ.get("ADMIN_ID", "7302701741")`
	// (telegram_bot.py:76). A reseller / white-label instance sets ADMIN_ID to
	// its own owner, so this MUST honor the override — hardcoding the default
	// would make every non-main deployment unbootable.
	adminID := cfg.EnvOr("ADMIN_ID", DefaultAdminID)

	// Multi-tenant money-safety boot guard (telegram_bot.py:19180-19194): ADMIN_ID
	// is "bound" at import from the AMBIENT env (before .env loads). If the merged
	// config's value disagrees with that bound value, this instance was started
	// WITHOUT its systemd EnvironmentFile and would silently act as a DIFFERENT
	// tenant. Refuse to boot rather than mis-route money.
	//
	// The bound value reads os.Getenv (ambient only, or the compiled default),
	// mirroring Python's import-time freeze; cfg.Env reads the ambient-wins merge
	// of ambient + .env. When ambient set the value both agree; when only .env set
	// it, they differ and we refuse — exactly Python's semantics. An unset key is
	// never a mismatch (BootGuard only compares a present .env value).
	//
	// USDT migration note (2026-08-06): the SePay bank-account boot guard was
	// removed along with the SePay/VietQR rail (internal/pay/sepay is gone) — the
	// shop takes USDT via BEP20/TRON only, so there is no bank account to bind.
	boundAdmin := os.Getenv("ADMIN_ID")
	if boundAdmin == "" {
		boundAdmin = DefaultAdminID
	}
	if err := cfg.BootGuard("ADMIN_ID", boundAdmin); err != nil {
		log.Fatal(err)
	}
	log.Printf("Instance config: admin=%s", boundAdmin)

	// Token check comes before opening any resource, so a missing token exits
	// cleanly with a clear message and no side effects (telegram_bot.py:18953-18955).
	token := cfg.Env("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN not found in .env or environment. Refusing to start.")
	}

	client := tg.NewClient(token)

	// White-label support @handle: a pinned SUPPORT_HANDLE env override always
	// wins (reseller white-label) and disables the owner auto-resolve; otherwise
	// resolve the owner's LIVE @username by id so a rename never leaves customers
	// messaging a dead handle (telegram_bot.py:18948, 18915-18932).
	rootCtx := context.Background()
	if h := cfg.Env("SUPPORT_HANDLE"); h != "" {
		i18n.ApplySupportHandle(h)
	} else {
		resolveSupportHandle(rootCtx, client, adminID)
	}

	// Store (balances / orders / handles / bans). Opening also runs the
	// idempotent schema migrations.
	st, err := store.Open(storeDBPath)
	if err != nil {
		log.Fatalf("store open failed: %v", err)
	}
	defer st.Close()

	// Customer refund-REQUEST store (Go port of /Users/z/Desktop/Kiro/refund_store.py,
	// M2.6). This is a SEPARATE handle on the SAME store.db file — refund.Store
	// owns its own `refund_requests` table, not a second database — so it opens
	// right next to store.Open the same way the Python bot's two modules each
	// held their own sqlite3 connection to one file. Before this wiring existed,
	// handlers.Deps.RefundStore was left nil by this constructor, which made the
	// whole M2.6 customer refund-request flow silently inert in production (any
	// nil-guard downstream just no-ops the request instead of erroring loudly),
	// so this must fail the same way store.Open does rather than boot half-wired.
	rst, err := refund.Open(storeDBPath)
	if err != nil {
		log.Fatalf("refund store open failed: %v", err)
	}
	defer rst.Close()

	// Load the in-memory ban gate once at startup (telegram_bot.py:6555).
	banned, err := st.BannedChatIDs()
	if err != nil {
		log.Fatalf("load banned set failed: %v", err)
	}

	// Folder-state stock machine (accounts.py): the buy flow reserves/finalizes
	// credential files under ./accounts and writes delivery records to ./orders.
	stk := stock.New(accountsDir, ordersDir)

	// One-shot permission migration for data written by an older build.
	tightenDataModes()

	// THE ONE unique-amount band registry, constructed here and injected, so
	// both USDT rails (BEP20, TRON) share a single collision set — a live BEP20
	// amount also blocks the same TRON amount (internal/pay/bands deviation 1).
	// Building one per rail would silently restore the Python behaviour this
	// port deliberately strengthens, so the registry is a process-level
	// singleton wired at boot. Binance Pay was removed by the USDT-only
	// migration (S3) — the registry itself needed no change; it was always
	// provider-agnostic.
	//
	// Geometry comes from each rail's own env-backed getters (BEP20/TRON step
	// 101, tol 50, at 5 decimals — 0.00101 USDT apart with a ±0.00050 band), so
	// an operator tuning *_OFFSET_STEP / *_MATCH_TOL is honoured. Geometry
	// Validate REJECTS is one the picker cannot work with at all — no slot fits
	// the jitter window, or a tolerance so wide it would overflow the gap
	// arithmetic — and that refuses to boot rather than risk handing two live
	// orders the same amount.
	//
	// A Step below MinGap is NOT such a case and deliberately does not stop the
	// boot: the picker enforces MinGap per slot at pick time, so a short step
	// can never produce a colliding amount, it only shrinks how many concurrent
	// same-price orders fit (Python warns here too — bep20_pay.py:56-58). Left
	// silent it is invisible, though: customers just start seeing USDT_BUSY. So
	// it is logged, loudly, right after the registry is built.
	//
	// DEPLOY NOTE for the 5-decimal cutover: an existing .env that still pins
	// the superseded *_AMOUNT_DECIMALS=2 / *_OFFSET_STEP=21 / *_MATCH_TOL=10
	// now fails Validate (at 2 decimals the 0.01-0.02 jitter window is one unit
	// wide and a 21-unit step cannot fit in it), so this Fatalf fires and the
	// bot will not start until those lines are removed or updated. That is the
	// money-safe direction — booting on a geometry whose slots fall outside the
	// window would quote amounts the watcher's matcher was not built for.
	bandReg, err := bands.New(bands.Config{Providers: map[bands.Provider]bands.Params{
		bands.ProviderBEP20: {Step: bep20.AmountOffsetStep(), Tol: bep20.AmountMatchTol(), Decimals: bep20.AmountDecimals()},
		bands.ProviderTron:  {Step: tron.AmountOffsetStep(), Tol: tron.AmountMatchTol(), Decimals: tron.AmountDecimals()},
	}})
	if err != nil {
		log.Fatalf("USDT band geometry invalid: %v", err)
	}
	// The warn-don't-refuse case above. SpacingOK had no production caller until
	// now, which is exactly why a Step-below-MinGap misconfiguration produced no
	// diagnostic anywhere.
	for _, prov := range []bands.Provider{bands.ProviderBEP20, bands.ProviderTron} {
		if prm, ok := bandReg.Params(prov); ok && !prm.SpacingOK() {
			log.Printf("WARNING: %s band step %d is below MinGap %d (2*tol+1) — amounts stay disjoint, "+
				"but concurrent same-price orders will be refused USDT_BUSY far sooner than the %d slots the window holds",
				prov, prm.Step, prm.MinGap(), prm.Slots())
		}
	}

	bot := handlers.New(handlers.Deps{
		TG:          client,
		Store:       st,
		Stock:       stk,
		RefundStore: rst,
		Config:      cfg,
		Bands:       bandReg,
		AdminID:     adminID,
		DevIDs:      map[string]bool{DefaultAdminID: true}, // DEV_IDS (telegram_bot.py:84)
		Banned:      banned,
	})

	// Confirm connectivity + identity before polling (telegram_bot.py:18978).
	if me, err := client.GetMe(rootCtx); err != nil {
		log.Fatalf("getMe failed (bad token or no network?): %v", err)
	} else {
		log.Printf("Bot @%s online. Long-polling... (Ctrl-C to stop)", me.Username)
	}

	// Menus: global default (Vietnamese) for chats that never touch /language,
	// plus the admin chat's extended menu now (telegram_bot.py:18982-18988).
	if err := client.SetMyCommands(rootCtx, handlers.MenuCommands(i18n.VI, false), nil); err != nil {
		log.Println("set default menu failed:", err)
	}
	if aid, err := strconv.ParseInt(adminID, 10, 64); err == nil {
		bot.ApplyMenu(rootCtx, aid, i18n.VI)
	}

	// Graceful shutdown: SIGINT/SIGTERM cancels the context; the Poll iterator
	// exits its loop and we wait for in-flight workers to drain.
	ctx, stop := signal.NotifyContext(rootCtx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// USDT migration note (2026-08-06): the PayPal webhook listener was removed
	// along with the PayPal rail itself (internal/pay/paypal is gone) — the shop
	// takes USDT via BEP20/TRON only, so there is no webhook port to bind here.

	// Boot-time recovery + the periodic expiry backstop — _startup_recovery
	// (telegram_bot.py:9995-10025). Rehydrates the persisted BEP20/TRON pending
	// rows into the band registry (so post-restart pickers still avoid
	// pre-restart amounts and a customer can still submit a late tx hash), and
	// then keeps a 120s expiry sweep running so a restart-orphaned checkout
	// still expires and its band is freed after the grace. Tracked by the
	// worker registry, so bot.Shutdown drains it.
	bot.StartupRecovery(ctx)

	// Report-only stuck-order reconcile + folder-stock expiry janitor, both
	// on tracked worker-registry tickers so Shutdown drains them. The
	// reconcile (reconcile.go) is READ-ONLY: it logs an OWNER ALERT for
	// paid-but-undelivered orders and NEVER delivers/refunds/mutates them
	// (reconcile_stuck_orders.py:25-26). The stock sweep (stocksweep.go)
	// resolves #26 — stock.SweepExpired previously had no periodic caller, so
	// folder-stock expiry ran only lazily; it reclaims reserved/<order>/ dirs
	// past their .expiry back to active/ across every product.
	bot.StartReconcileStuckOrders(ctx)
	bot.StartStockSweep(ctx)
	// Audit-retention janitor (task #14) — store.PruneAudit previously had no
	// caller, the same defect StartStockSweep resolved for folder-stock
	// expiry. Deletes audit rows older than AUDIT_RETENTION_DAYS (default
	// 180d, floored at 30d in both the handlers layer and the store itself),
	// once a day. See auditprune.go for the full rationale.
	bot.StartAuditPrune(ctx)

	// Hourly owner-@username refresher so a rename propagates to customer
	// messages without a restart — port of _support_handle_refresher
	// (telegram_bot.py:18935-18943). Skipped when SUPPORT_HANDLE is pinned
	// (reseller white-label). Stops on shutdown.
	if cfg.Env("SUPPORT_HANDLE") == "" {
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					resolveSupportHandle(rootCtx, client, adminID)
				}
			}
		}()
	}

	// Worker pool: a 16-slot semaphore + one goroutine per update, each with a
	// panic-recover so one bad update can't kill a worker (safe_process,
	// telegram_bot.py:19021-19025). No per-chat serialization — parity with the
	// Python pool; the per-chat flags are the only guards (handlers-go.md §8).
	sem := make(chan struct{}, poolSize)
	var wg sync.WaitGroup
	for u, perr := range client.Poll(ctx) {
		if perr != nil {
			// Poll already logs 409 conflicts loudly; other transient errors are
			// logged + retried by the iterator, so just note and continue.
			log.Println("poll error:", perr)
			continue
		}
		sem <- struct{}{} // acquire (blocks when 16 in flight)
		wg.Add(1)
		go func(u tg.Update) {
			defer wg.Done()
			defer func() { <-sem }() // release
			defer func() {
				if r := recover(); r != nil {
					// The recovered VALUE is deliberately not logged — only a
					// sentinel and the stack. This is the process-wide catch-all,
					// so the panic can have unwound through any frame, including
					// the api-key mint path (holding a cleartext sk-… key) and the
					// provisioning path (holding account passwords and TOTP
					// seeds); a panic message built from such a local would write
					// that secret to the system journal, which is persistent,
					// root-readable and often shipped off-box. Same policy the OTP
					// fetch guards already apply (otp.go:465-470, :553), applied
					// here at the one recover that catches everything. The stack
					// identifies the fault precisely enough to debug from.
					log.Printf("handler panic (payload suppressed)\n%s", debug.Stack())
				}
			}()
			// Handlers run on rootCtx, NOT the signal context: on shutdown the
			// Poll loop above stops, but an in-flight update must finish its
			// Telegram replies rather than have them canceled mid-flow (the
			// wg.Wait below then drains them). Matches Python daemon-thread
			// handlers completing their sends after the poll loop ends.
			bot.Process(rootCtx, u)
		}(u)
	}

	log.Println("shutting down; waiting for in-flight updates to finish...")
	wg.Wait()
	// Drain the per-order expiry timers so a pending checkout's 600s goroutine
	// doesn't outlive the process (handlers-go.md §3/§8).
	bot.Shutdown()
	log.Println("stopped.")
}

// tightenDataModes removes group and other permission bits from the bot's own
// data on disk: the ./accounts and ./orders trees, plus store.db and its WAL
// sidecars.
//
// Security warning, normal prose: builds before this one created account
// credential files at 0644 and their directories at 0755, and let the SQLite
// driver create store.db at the umask default. Those files hold plaintext
// account passwords, TOTP seeds, OAuth refresh tokens and cleartext customer
// API keys, and any local user on the host could read them. Changing the modes
// used for NEW files does nothing for the ones already written, so an existing
// install stays fully exposed until this pass runs. Treat any ./accounts tree
// that predates this build as compromised and rotate what it holds.
//
// Runs on every boot rather than behind a marker file: it is idempotent (a
// second run finds nothing to change and reports 0), it never widens a mode —
// each pass subtracts bits from whatever the entry already has — and it never
// follows a symlink out of the data root. A marker would only add a way for
// the migration to be silently skipped.
//
// Logs exactly one summary line, with a COUNT and no paths: a path under
// ./accounts is an account username, and the whole point of this function is
// that such material does not belong in the system journal. Every failure is
// non-fatal; a permissions pass must never be what stops the shop from
// booting.
func tightenDataModes() {
	total := 0
	var firstErr error
	note := func(n int, err error) {
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	note(stock.TightenModes(accountsDir))
	note(stock.TightenModes(ordersDir))
	note(dbfile.Tighten(storeDBPath))

	if firstErr != nil {
		// The error text can carry a path, so report only that something
		// failed and how far the pass got.
		log.Printf("permission migration: tightened %d entries, but at least one failed (check ownership of %s/, %s/ and %s)",
			total, accountsDir, ordersDir, storeDBPath)
		return
	}
	if total > 0 {
		log.Printf("permission migration: tightened %d data files/directories to owner-only", total)
	}
}

// resolveSupportHandle best-effort resolves the owner's live @username via
// getChat(ADMIN_ID) and applies it as the support handle — port of
// _resolve_support_handle (telegram_bot.py:18915-18932). On any failure or an
// owner with no @username, the compiled default stays in place.
func resolveSupportHandle(ctx context.Context, client *tg.Client, adminID string) {
	aid, err := strconv.ParseInt(adminID, 10, 64)
	if err != nil {
		return
	}
	chat, err := client.GetChat(ctx, aid)
	if err != nil {
		log.Println("support-handle resolve failed:", err)
		return
	}
	if chat.Username != "" {
		i18n.ApplySupportHandle("@" + chat.Username)
		log.Printf("support handle -> @%s", chat.Username)
	}
}
