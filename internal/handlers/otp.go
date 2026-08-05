package handlers

// otp.go — the login-code (OTP / password-reset link) self-service suite. Ports:
//
//	chat_owns_username     @8140  -> chatOwnsUsername
//	_otp_rate_ok           @8155  -> otpRateOK
//	_otp_session_live      @8172  -> otpSessionLive     (+ 240s watchdog)
//	_otp_session_start     @8194  -> otpSessionStart
//	_otp_session_claim     @8216  -> otpSessionClaim
//	_otp_session_stop      @8234  -> otpSessionStop
//	otp_guard              @8246  -> otpGuard           (both call sites)
//	fetch_login_code       @8266  -> fetchLoginCode
//	_parse_otp_usernames   @8339  -> parseOTPUsernames
//	_run_otp_request       @8358  -> runOTPRequest
//	getcode command        @11674 -> handleCommand case "getcode"
//	/otp <usernames>       @19413 -> Process step 11 (see cmdOTPSlash)
//	pending_code capture   @18341 -> capCode (capture #36)
//	otpstop: / otpkeep     @15645 -> cbOTPStop / cbOTPKeep
//
// SECRET HANDLING (CLAUDE.md): an OTP code and a reset URL are live credentials
// for a sold account. They are NEVER logged, never echoed into an error string,
// and never put in a subprocess argv beyond the username. A failed fetch logs
// only "no code/reset found" — deliberately NOT the child's output blob, which
// the Python original prints (@8324). That is the one intentional deviation in
// this file, and it is the safer direction.
//
// SESSION MODEL. A login-code session owns the chat in two phases:
//
//	await   — st.Code armed (pending_code): waiting for the username line.
//	stream  — st.OTPSession.Active > 0: N fetches in flight, results stream in.
//
// While either is true, otpGuard blocks EVERY other command and inline tap
// behind a stop/keep confirmation. Leaving the await phase and entering the
// stream phase happens in ONE st.mu critical section (otpSessionStart), so two
// pooled updates for the same chat can never both slip past the guard and fire
// duplicate batches — the Python invariant at @8194-8203.
//
// LOCKING (handlers-go.md §8). st.mu is a leaf lock: it is taken only inside the
// small helpers below and NEVER held across a Telegram call or the node
// subprocess. The concurrency ceiling is a buffered channel, not a mutex.

import (
	"context"
	"errors"
	"html"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/subproc"
	"codezdev-bot/internal/tg"
)

// Python module constants (telegram_bot.py:638-680). Plain consts here for the
// same reason they are plain consts there: they are product limits, not
// deployment knobs. The two TIMING values are copied into Bot fields in New so
// tests can shrink them without touching the shipped defaults.
const (
	// otpMaxBatch — OTP_MAX_BATCH (@640): most accounts one request may fetch.
	// Each account spawns its own node subprocess + mail poll.
	otpMaxBatch = 5
	// otpCooldown — OTP_COOLDOWN_S (@643): minimum gap between accepted requests
	// per chat (admins exempt).
	otpCooldown = 15 * time.Second
	// otpSessionMax — OTP_SESSION_MAX_S (@677): a streaming session older than
	// this is treated as dead by the watchdog, so a fetch goroutine that dies
	// before settling can never lock the chat forever. Sized above the worst-case
	// per-batch wall time (45s semaphore acquire + 90s subprocess).
	otpSessionMax = 240 * time.Second
	// otpMaxConcurrency — OTP_MAX_CONCURRENCY (@680): global ceiling on
	// concurrent fetch subprocesses across ALL chats.
	otpMaxConcurrency = 10
	// otpSemAcquire — the semaphore acquire budget (@8283 `acquire(timeout=45)`).
	// Fail fast rather than queue for minutes: a queued fetch would hold the
	// chat's session lock the whole time.
	otpSemAcquire = 45 * time.Second
	// otpFetchTimeout — the node subprocess budget (@8288 `timeout=90`).
	otpFetchTimeout = 90 * time.Second
	// otpScriptName is the Node helper the fetch shells out to (@8287). It only
	// READS the newest inbound mail from Resend and prints OTP:/RESET:/OTP_ERROR:.
	otpScriptName = "fetch_login_code.mjs"
	// otpUsernameMaxLen — `w.strip()[:64]` (@8352): argv-safe per-username cap.
	otpUsernameMaxLen = 64
)

// otpUsernameRE is the username charset gate — `_re.fullmatch(r"[A-Za-z0-9._-]+", w)`
// (@8353). Applied AFTER the length cap, exactly like the Python order.
var otpUsernameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// registerOTP attaches the login-code capture + callback routes. Called from New.
//
// pending_code is capture #36 (handlers-go.md §2.2) — ahead of every capture the
// earlier slices registered (#43+), so it is SPLICED in before "txid" rather than
// appended, keeping the chain an in-order subsequence of CanonicalCaptureOrder.
//
// otpstop: / otpkeep are appended: they sit at @15645-15661 in the Python chain,
// but no earlier-registered route prefix-matches "otp…", so appending cannot be
// shadowed by first-match dispatch and the effective behaviour is identical.
func (b *Bot) registerOTP() {
	b.insertCapturesBefore("txid",
		stage{name: "code", fn: (*Bot).capCode}, // #36 @18341 peek
	)
	b.cbRoutes = append(b.cbRoutes,
		cbRoute{key: "otpstop:", fn: (*Bot).cbOTPStop},            // @15645
		cbRoute{key: "otpkeep", exact: true, fn: (*Bot).cbOTPKeep}, // @15656
	)
}

// ---- session bookkeeping ----------------------------------------------------

// otpSessionLocked returns (allocating on first use) this chat's session record.
// Caller MUST hold st.mu. The record is never freed: Epoch and LastAccepted must
// outlive one session (see the OTPSession doc comment).
func (st *ChatState) otpSessionLocked() *OTPSession {
	if st.OTPSession == nil {
		st.OTPSession = &OTPSession{}
	}
	return st.OTPSession
}

// otpSessionLive reports whether a login-code session owns this chat — port of
// _otp_session_live (telegram_bot.py:8172-8192): either the await phase
// (st.Code armed) or the streaming phase (Active > 0).
//
// Watchdog: a streaming session past otpSessionMax is treated as DEAD and
// cleared here, bumping the epoch so the presumed-dead fetches can never deliver
// later. A chat therefore self-heals instead of staying locked forever if a
// goroutine dies before settling its count.
func (b *Bot) otpSessionLive(st *ChatState) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.Code != nil {
		return true
	}
	s := st.OTPSession
	if s == nil || s.Active <= 0 {
		return false
	}
	if b.now().Sub(s.StartedAt) > b.otpSessionMax {
		s.Active = 0
		s.StartedAt = time.Time{}
		s.Epoch++
		return false
	}
	return true
}

// otpSessionStart opens the streaming phase for n fetches and returns this
// batch's epoch — port of _otp_session_start (telegram_bot.py:8194-8214).
//
// It atomically LEAVES the await phase (clears st.Code under the same lock) so
// "session live" never flickers false in between. ok=false means a racing submit
// already opened a session for this chat: the caller then does nothing rather
// than fire a duplicate (paid-for) batch. Each fetch captures the returned epoch;
// a later stop bumps it so those fetches discard their in-flight result.
func (b *Bot) otpSessionStart(st *ChatState, n int) (epoch int64, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.otpSessionLocked()
	if s.Active > 0 {
		return 0, false
	}
	s.Epoch++
	s.Active = n
	s.StartedAt = b.now()
	st.Code = nil // pops pending_code inside the SAME critical section (@8203)
	return s.Epoch, true
}

// otpSessionClaim is called exactly once by a finishing fetch — port of
// _otp_session_claim (telegram_bot.py:8216-8232). It returns true when the
// result is still wanted, decrementing the live count and closing the session as
// the last fetch lands; false when the session was stopped or superseded, and the
// caller must drop the result silently.
func (b *Bot) otpSessionClaim(st *ChatState, epoch int64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.OTPSession
	if s == nil || s.Epoch != epoch {
		return false
	}
	if s.Active-1 <= 0 {
		s.Active = 0
		s.StartedAt = time.Time{}
	} else {
		s.Active--
	}
	return true
}

// otpSessionStop tears down a live session — port of _otp_session_stop
// (telegram_bot.py:8234-8244). Bumps the epoch so still-running fetches suppress
// their results, and clears BOTH phases (await flag + streaming count).
func (b *Bot) otpSessionStop(st *ChatState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Code = nil
	s := st.otpSessionLocked()
	s.Epoch++
	s.Active = 0
	s.StartedAt = time.Time{}
}

// ---- gates ------------------------------------------------------------------

// otpRateOK is the anti-spam gate for login-code fetches — port of _otp_rate_ok
// (telegram_bot.py:8155-8170): at most one accepted request per otpCooldown per
// chat, admins exempt. On refusal it DMs the remaining wait and returns false;
// on success it stamps the acceptance time. Applied to both /otp and the
// "Login code" button since both reach the expensive fetch path.
func (b *Bot) otpRateOK(ctx context.Context, chatID int64, lang i18n.Lang, st *ChatState) bool {
	if b.isAdmin(chatID) {
		return true
	}
	st.mu.Lock()
	s := st.otpSessionLocked()
	now := b.now()
	// A zero LastAccepted (never requested) yields an enormous elapsed time, so
	// the first request always passes — same as Python's `_otp_last.get(chat, 0.0)`.
	wait := b.otpCooldown - now.Sub(s.LastAccepted)
	if wait > 0 {
		st.mu.Unlock() // never hold st.mu across the Telegram call (§8)
		// int(wait) + 1 (@8166): truncate to whole seconds, then round up so the
		// customer is never told to wait 0s.
		b.send(ctx, chatID, i18n.F(i18n.C().OTP.OTPRateLimit.T(lang), "sec", itoa(int(wait.Seconds())+1)), nil)
		return false
	}
	s.LastAccepted = now
	st.mu.Unlock()
	return true
}

// chatOwnsUsername reports whether username was delivered in one of this chat's
// orders — port of chat_owns_username (telegram_bot.py:8140-8153). This is the
// authorization gate for the login-code feature: a customer may only fetch OTPs
// for accounts they actually purchased.
//
// is_admin (NOT is_super_admin) short-circuits here, matching Python exactly, so
// every OTHER caller keeps Python's behaviour. Callers that must restrict the
// bypass to FULL admins call chatOwnsAccount directly — see runOTPRequest and
// DEVIATION below.
func (b *Bot) chatOwnsUsername(chatID int64, username string) bool {
	if b.isAdmin(chatID) {
		return true
	}
	return b.chatOwnsAccount(chatID, username)
}

// chatOwnsAccount is the ownership scan with NO admin short-circuit: it answers
// only "was this account delivered in one of THIS chat's orders".
//
// DEVIATION (deliberate, security). Python's _run_otp_request wraps its scan in
// `if not is_super_admin(chat_id)` and states the intent explicitly: "Standard
// admins are owner-scoped like any customer" (@8385-8389). That intent does not
// hold in Python, because the scan calls chat_owns_username, which itself returns
// True for any is_admin — so a STANDARD admin there passes the batch gate for
// every account in the shop and can pull a live OTP / password-reset link for any
// sold account, i.e. take it over. The Go port keeps the documented intent rather
// than the bug: the OTP batch gate scans with this strict variant, so only a FULL
// admin (ADMIN_ID / DEV_IDS) bypasses ownership.
func (b *Bot) chatOwnsAccount(chatID int64, username string) bool {
	username = strings.TrimSpace(username)
	// No store/stock wired (unit tests, or a partial boot) -> own nothing. Failing
	// CLOSED is mandatory: this is an authorization decision.
	if b.store == nil || b.stock == nil {
		return false
	}
	orders, err := b.store.OrdersForChat(fmtInt(chatID), 200, nil, nil)
	if err != nil {
		log.Println("chatOwnsAccount: OrdersForChat:", err)
		return false
	}
	for _, o := range orders {
		for _, u := range b.stock.OrderUsernames(o.Des) {
			if u == username {
				return true
			}
		}
	}
	return false
}

// otpGuard makes the customer confirm stopping a live login-code session before
// any other action runs — port of otp_guard (telegram_bot.py:8246-8264), the
// same shape as the unpaid-checkout guard.
//
// Returns true when a prompt was shown (the caller MUST stop and not run the
// requested action); false when no session is live. resumeCmd is the command word
// re-run after the customer confirms the stop ("" = nothing to resume, e.g. an
// inline tap).
func (b *Bot) otpGuard(ctx context.Context, chatID int64, lang i18n.Lang, resumeCmd string) bool {
	st := b.states.Get(chatID)
	if !b.otpSessionLive(st) {
		return false
	}
	c := &i18n.C().OTP
	kb := &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		{{Text: c.OTPStopBtn.T(lang), CallbackData: "otpstop:" + resumeCmd}},
		{{Text: c.OTPKeepBtn.T(lang), CallbackData: "otpkeep"}},
	}}
	b.send(ctx, chatID, c.OTPBusyWarn.T(lang), kb)
	return true
}

// ---- request parsing + dispatch ---------------------------------------------

// parseOTPUsernames splits a batch request into a clean, ordered username list —
// port of _parse_otp_usernames (telegram_bot.py:8339-8356).
//
// Accepts space-separated usernames (one per line works too, since fields split
// on any whitespace), caps each at otpUsernameMaxLen, validates the charset, and
// dedups while keeping first-seen order. The COUNT is deliberately not capped
// here: the caller compares against otpMaxBatch so it can show a clear "too many"
// error instead of silently dropping accounts.
func parseOTPUsernames(text string) []string {
	var out []string
	for _, w := range strings.Fields(text) {
		// Truncate by RUNE, not byte: Python's `[:64]` counts characters, and a
		// byte slice could split a multibyte rune into invalid UTF-8. Either way
		// the charset check below rejects anything non-ASCII.
		if r := []rune(w); len(r) > otpUsernameMaxLen {
			w = string(r[:otpUsernameMaxLen])
		}
		if w == "" || !otpUsernameRE.MatchString(w) {
			continue
		}
		if slicesContains(out, w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// slicesContains is a tiny local helper (kept explicit so the dedup mirrors
// Python's `w in out` list membership, order-preserving and O(n²) on a list
// bounded by the batch cap).
func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// runOTPRequest gates + dispatches a login-code batch — port of _run_otp_request
// (telegram_bot.py:8358-8408).
//
// Enforced IN ORDER: non-empty parse (else re-arm and re-prompt), the per-request
// account cap, the per-chat rate limit, then ownership of EVERY account. The
// rate limit runs BEFORE the ownership scan on purpose: chatOwnsUsername reads up
// to 200 order files per username, so gating first stops a non-owner from
// hammering that expensive path for free (a rejected attempt still spends the
// cooldown).
func (b *Bot) runOTPRequest(ctx context.Context, chatID int64, lang i18n.Lang, unames []string) {
	c := &i18n.C().OTP
	st := b.states.Get(chatID)

	if len(unames) == 0 {
		// Nothing usable typed — guide the customer and stay armed for the next
		// line (@8369-8373).
		st.mu.Lock()
		st.Code = &OTPCtx{}
		st.mu.Unlock()
		b.send(ctx, chatID, i18n.F(c.CodePrompt.T(lang), "n", itoa(otpMaxBatch)), nil)
		return
	}
	if len(unames) > otpMaxBatch {
		b.send(ctx, chatID, i18n.F(c.OTPTooMany.T(lang), "n", itoa(otpMaxBatch)), nil)
		return
	}
	if !b.otpRateOK(ctx, chatID, lang, st) {
		return
	}
	// Ownership: the chat must own EVERY requested account. Only a FULL admin
	// (owner / developer override) may fetch codes for accounts they don't own —
	// OTP/reset retrieval is NOT a standard-admin capability (it would allow
	// takeover of any sold account), so a standard admin is owner-scoped like any
	// customer. One missing account rejects the WHOLE batch, naming the offenders
	// (@8382-8396).
	if !b.isSuperAdmin(chatID) {
		var bad []string
		for _, u := range unames {
			// chatOwnsAccount, NOT chatOwnsUsername: the latter short-circuits on
			// isAdmin, which would hand a STANDARD admin the full bypass this branch
			// exists to deny (see the DEVIATION note on chatOwnsAccount).
			if !b.chatOwnsAccount(chatID, u) {
				bad = append(bad, u)
			}
		}
		if len(bad) > 0 {
			b.send(ctx, chatID, i18n.F(c.CodeNotOwned.T(lang), "u", strings.Join(bad, ", ")), nil)
			return
		}
	}
	// Open the streaming session (atomically leaving the await phase) and tag
	// every fetch with its epoch so a stop can silence them. !ok => a racing
	// submit already opened a session; do nothing rather than duplicate the batch
	// (@8401-8403).
	epoch, ok := b.otpSessionStart(st, len(unames))
	if !ok {
		return
	}
	b.send(ctx, chatID, i18n.F(c.CodeWorking.T(lang), "u", strings.Join(unames, ", ")), nil)
	// One independent fetch per account, so each result is delivered the moment it
	// lands (results stream, in any order) — @8406-8408.
	for _, u := range unames {
		uname := u
		started := b.workers.TryGo(func(stop <-chan struct{}) {
			// The originating update's ctx dies with the update; the fetch outlives
			// it. Shutdown cancels the child instead of orphaning it.
			fctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-stop:
					cancel()
				case <-done:
				}
			}()
			b.fetchLoginCode(fctx, chatID, lang, st, uname, epoch)
		})
		if !started {
			// Shutting down: the fetch will never run, so settle its slot now or the
			// chat stays locked until the watchdog expires.
			b.otpSessionClaim(st, epoch)
		}
	}
}

// ---- the fetch itself -------------------------------------------------------

// fetchLoginCode runs the server-side OTP / reset-link fetch for one username and
// DMs the result — port of fetch_login_code (telegram_bot.py:8266-8337).
//
// The customer triggers the email themselves: signing in at the access portal
// makes AWS mail a 6-digit code, and "Forgot password" makes AWS mail a reset
// link. The node helper only READS the newest inbound mail and prints
// OTP:<code> / RESET:<url> / OTP_ERROR:<reason> — no browser, just an API poll.
//
// epoch ties this fetch to its streaming session: the result is delivered only if
// the session is still live at completion, so a customer who hit "stop" never
// receives trailing codes. b.otpSem caps how many node subprocesses run at once
// across ALL chats.
func (b *Bot) fetchLoginCode(ctx context.Context, chatID int64, lang i18n.Lang, st *ChatState, username string, epoch int64) {
	c := &i18n.C().OTP
	// Default outcome: the per-account failure notice (@8271). Overwritten below
	// only when the helper actually produced something.
	text, mode := i18n.F(c.CodeFail.T(lang), "u", username), ""

	// settled is flipped by the ONE claim below. The deferred guard exists for a
	// fault on a path the claim never reaches (rendering, the Telegram send): it
	// settles the counter so the chat is not locked until the 240s watchdog, and
	// swallows the panic so the worker goroutine — and the process — survives, the
	// way Python's `except Exception` around this body did. It can never
	// double-settle: it only claims when settled is still false. The recovered
	// value is NOT logged; it can carry the child's output (the code / reset link).
	settled := false
	defer func() {
		if r := recover(); r != nil {
			log.Printf("fetchLoginCode: panicked for %s (payload suppressed)\n%s",
				username, debug.Stack())
			if !settled {
				b.otpSessionClaim(st, epoch)
			}
		}
	}()

	// Bound concurrent subprocesses host-wide; fail fast rather than queue for
	// minutes (@8283-8285).
	if !b.otpAcquireSlot(ctx) {
		log.Printf("fetchLoginCode: concurrency timeout for %s", username)
		// Settle the slot now so the chat is never left locked — the "ALWAYS
		// settle" block below is not reached when we return here.
		settled = true
		b.otpSessionClaim(st, epoch)
		return
	}
	// The slot is held across exactly one call; release it and settle the session
	// on EVERY exit, including a panic out of the fetch seam. Python got both from
	// its `finally` blocks (@8299 release, @8326 claim): without them a panicking
	// node runner would leak one of the ten global subprocess slots permanently
	// (ten faults deadlock OTP fetches host-wide for every chat) AND leave this
	// chat locked until the 240s watchdog. The panic is logged WITHOUT its payload
	// (a recovered value from the fetch seam could carry the code) and re-raised
	// only as a fixed marker, matching Python's log-and-continue.
	code, reset, err := b.otpFetchGuarded(ctx, username)
	switch {
	case err != nil:
		// Error text only — a partial blob could carry the credential.
		log.Printf("fetchLoginCode: fetch failed for %s: %v", username, err)
	case code == "" && reset == "":
		// Python prints the tail of the child's output here (@8324); we
		// deliberately do not (see the SECRET HANDLING note at the top).
		log.Printf("fetchLoginCode: no code/reset found for %s", username)
	default:
		// Build ONE DM from whatever was found. Both messages render as HTML
		// (the code sits in <b>…</b>, the URL is bare so Telegram auto-links
		// it), so the username, the code and the URL are all escaped
		// (@8306-8318).
		var parts []string
		if code != "" {
			parts = append(parts, i18n.F(c.CodeOK.T(lang),
				"u", html.EscapeString(username), "code", html.EscapeString(code)))
		}
		if reset != "" {
			parts = append(parts, i18n.F(c.ResetOK.T(lang),
				"u", html.EscapeString(username), "link", html.EscapeString(reset)))
		}
		text, mode = strings.Join(parts, "\n\n"), "HTML"
	}

	// ALWAYS settle the session counter — even on an unexpected failure — so a
	// batch can never leave the chat permanently locked. This also decides
	// delivery: drop the result silently when the customer stopped the session (or
	// it was superseded), so no stale code/link is ever sent (@8326-8334).
	settled = true
	if !b.otpSessionClaim(st, epoch) {
		return
	}
	p := tg.SendMessageParams{ChatID: chatID, Text: text}
	if mode != "" {
		p.ParseMode = mode
	}
	if _, err := b.tg.SendMessage(ctx, p); err != nil {
		// Never log the payload: it may hold the code/link.
		log.Printf("fetchLoginCode: send failed [chat=%d]: %v", chatID, err)
	}
}

// otpFetchGuarded calls the fetch seam with the slot release and the panic guard
// of Python's `try/except Exception/finally` around the same call (@8286-8299).
//
// The caller MUST already hold a slot. On a panic the slot is still released (the
// defer runs during unwinding) and the panic is CONVERTED to an ordinary error so
// the caller's settle-and-deliver path still runs — a re-panic would kill the
// worker goroutine with the session counter unsettled, locking the chat for 240s.
//
// The recovered value is deliberately NOT logged: a panic raised from inside the
// node runner can carry the child's output, which may hold the OTP or reset link.
func (b *Bot) otpFetchGuarded(ctx context.Context, username string) (code, reset string, err error) {
	defer b.otpReleaseSlot()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("fetchLoginCode: fetch panicked for %s (payload suppressed)\n%s",
				username, debug.Stack())
			code, reset, err = "", "", errOTPFetchPanic
		}
	}()
	return b.otpFetch(ctx, username)
}

// errOTPFetchPanic is the fixed error a recovered fetch panic becomes. A fixed
// sentinel (never the panic value) keeps a credential out of the log line the
// caller writes for a failed fetch.
var errOTPFetchPanic = errors.New("otp: fetch panicked")

// otpAcquireSlot takes one of the otpMaxConcurrency subprocess slots, waiting at
// most otpSemAcquire — the Go twin of `_otp_sem.acquire(timeout=45)` (@8283).
// Returns false on timeout or cancellation, and then NOTHING must be released.
func (b *Bot) otpAcquireSlot(ctx context.Context) bool {
	t := time.NewTimer(b.otpSemAcquire)
	defer t.Stop()
	select {
	case b.otpSem <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// otpReleaseSlot returns a slot taken by otpAcquireSlot (@8299 `release()`).
func (b *Bot) otpReleaseSlot() { <-b.otpSem }

// otpFetchNode is the production otpFetch seam: it shells out to
// fetch_login_code.mjs and parses its OTP:/RESET: lines (@8286-8305).
//
// A non-zero exit is NOT an error (the helper prints OTP_ERROR: and exits 2 when
// it finds nothing), so only a start failure / timeout / cancellation is
// reported as one — matching Python, where subprocess.run ignores returncode but
// a TimeoutExpired falls through to CODE_FAIL.
func (b *Bot) otpFetchNode(ctx context.Context, username string) (code, reset string, err error) {
	dir := scriptDir()
	res, err := subproc.Run(ctx, subproc.Request{
		Path:    otpNodeBin(),
		Args:    []string{filepath.Join(dir, otpScriptName), username},
		Dir:     dir,
		Timeout: b.otpFetchTimeout,
	})
	if err != nil {
		return "", "", err
	}
	// The helper may print an OTP line, a RESET line, or both (@8300-8305).
	code, reset = parseOTPOutput(res.Stdout + res.Stderr)
	return code, reset, nil
}

// parseOTPOutput extracts the OTP:/RESET: values from the helper's combined
// output (@8300-8305). Anything else (including OTP_ERROR:) yields empty strings,
// which the caller renders as the per-account failure notice.
func parseOTPOutput(blob string) (code, reset string) {
	for _, line := range strings.Split(blob, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "OTP:"):
			code = strings.TrimSpace(strings.TrimPrefix(line, "OTP:"))
		case strings.HasPrefix(line, "RESET:"):
			reset = strings.TrimSpace(strings.TrimPrefix(line, "RESET:"))
		}
	}
	return code, reset
}

// otpNodeBin resolves the Node binary. Python hardcodes "node" (@8287); the env
// override exists because a Go deployment may not have node on the service PATH.
func otpNodeBin() string {
	if v := strings.TrimSpace(os.Getenv("KIRO_NODE")); v != "" {
		return v
	}
	return "node"
}

// scriptDir resolves the directory holding the bot's sibling helper scripts —
// fetch_login_code.mjs (@8280, @8289), aws_idc_provision_cli.py, and
// aws_idc_login.mjs (idc_brief.md item D: promoted from the OTP-only
// otpScriptDir so the AWS-IDC feature can resolve the same scripts without a
// second copy of this logic). Python uses
// `os.path.dirname(os.path.abspath(__file__))` — the bot's own directory — and
// also runs the child with cwd=there (@8280, @8289). The Go twin of "my own
// directory" is the executable's directory; KIRO_SCRIPT_DIR overrides it (read
// lazily, at call time, because .env is loaded into the environment after link
// time — same reasoning as provision.LoginPythonFromEnv).
func scriptDir() string {
	if v := strings.TrimSpace(os.Getenv("KIRO_SCRIPT_DIR")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// ---- entry points -----------------------------------------------------------

// cmdGetCode arms the login-code await phase — the getcode branch of
// handle_command (telegram_bot.py:11674-11680). Clears the sibling customer
// captures FIRST so a stale one can't eat the username line, then arms st.Code
// and prompts.
func (b *Bot) cmdGetCode(ctx context.Context, chatID int64, lang i18n.Lang) {
	st := b.states.Get(chatID)
	st.ClearCustomerPending() // drop stale sibling captures first (@11677)
	st.mu.Lock()
	st.Code = &OTPCtx{}
	st.mu.Unlock()
	b.send(ctx, chatID, i18n.F(i18n.C().OTP.CodePrompt.T(lang), "n", itoa(otpMaxBatch)), nil)
}

// cmdOTPSlash handles `/otp [username …]` — port of the /otp branch of process()
// (telegram_bot.py:19413-19431).
//
// WHY IT LIVES IN Process AND NOT handleCommand: /otp is the one command that
// takes ARGUMENTS, and handleCommand receives only the bare command word
// (parseSlashCommand strips the rest) — exactly as in Python, where handle_command
// also only ever sees `cmd`. Python therefore intercepts /otp inside process()
// before dispatching to handle_command, and this port does the same. That keeps
// every other command's signature and behaviour untouched, and preserves two
// Python quirks: /otp never reaches the maintenance-mode gate, and it always
// returns (bare /otp re-prompts rather than falling through).
//
// arg is the raw text after the first space ("" for a bare /otp).
func (b *Bot) cmdOTPSlash(ctx context.Context, chatID int64, lang i18n.Lang, arg string) {
	// A session already live? Block this /otp behind the stop prompt too, so one
	// session runs at a time (matches every other action). The FIRST /otp, with no
	// session yet, falls through and starts one (@19426-19429).
	if b.otpGuard(ctx, chatID, lang, "") {
		return
	}
	b.runOTPRequest(ctx, chatID, lang, parseOTPUsernames(arg))
}

// capCode is capture #36 — the pending_code branch (telegram_bot.py:18341-18348,
// @19059-19062): the next plain-text line is one or more usernames to fetch OTPs
// / reset links for.
//
// PEEK semantics: a slash-command or a tapped keyboard label escapes the capture
// and leaves the slot ARMED. st.Code is NOT cleared here — otpSessionStart pops it
// atomically as the session opens (so "live" never flickers off mid-transition),
// and a rejected submit deliberately leaves the customer in the await phase to
// retry.
func (b *Bot) capCode(c *Ctx) bool {
	c.st.mu.Lock()
	armed := c.st.Code != nil
	c.st.mu.Unlock()
	if !armed {
		return false
	}
	// peek escapes (@19059's `and` guards).
	if strings.HasPrefix(c.Text, "/") || labelToCmd(c.Text) != "" {
		return false
	}
	b.runOTPRequest(c.Ctx, c.ChatID, c.Lang, parseOTPUsernames(c.Text))
	return true
}

// cbOTPStop handles `otpstop:<resume_cmd>` (telegram_bot.py:15645-15655): stop the
// live session (silencing any in-flight fetches), confirm, then run the action the
// customer was trying to take. An empty resume_cmd means there is nothing to
// resume (they tapped an inline button).
func (b *Bot) cbOTPStop(cc *cbCtx) {
	resumeCmd, _ := strings.CutPrefix(cc.Data, "otpstop:")
	lang := b.langFor(cc.ChatID, cbClientLang(cc.CB))
	b.otpSessionStop(b.states.Get(cc.ChatID))
	b.send(cc.Ctx, cc.ChatID, i18n.C().OTP.OTPStopped.T(lang), nil)
	if resumeCmd != "" {
		b.handleCommand(cc.Ctx, cc.ChatID, resumeCmd, string(lang))
	}
}

// cbOTPKeep handles `otpkeep` (telegram_bot.py:15656-15661): keep waiting — the
// session stays live, so just remind the customer what to do.
func (b *Bot) cbOTPKeep(cc *cbCtx) {
	lang := b.langFor(cc.ChatID, cbClientLang(cc.CB))
	b.send(cc.Ctx, cc.ChatID,
		i18n.F(i18n.C().OTP.OTPKept.T(lang), "n", itoa(otpMaxBatch)), nil)
}

// cbClientLang is the tapper's Telegram UI locale, used only as the lang_for
// fallback for a chat that never picked a language (`(cb.get("from") or
// {}).get("language_code", "en")`, @15649).
func cbClientLang(cb *tg.CallbackQuery) string {
	if cb != nil && cb.From.LanguageCode != "" {
		return cb.From.LanguageCode
	}
	return "en"
}
