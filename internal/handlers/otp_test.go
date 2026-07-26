package handlers

// otp_test.go — the M2.6 login-code suite. Every test drives the real dispatch
// entry points (Process / handleCommand / handleCallback) against the fake
// Telegram, with the node fetch replaced by the b.otpFetch seam so no test ever
// spawns a subprocess.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
)

// otpTestBot builds a test Bot whose fetch seam is a no-op (returns nothing
// found) unless the caller replaces it. adminID "999" keeps chat 1 a plain
// customer; pass "1" to make chat 1 a FULL admin (cooldown + ownership exempt).
func otpTestBot(adminID string) (*Bot, *fakeTG) {
	b, f := newTestBot(adminID, nil)
	b.otpFetch = func(context.Context, string) (string, string, error) { return "", "", nil }
	return b, f
}

// sentTexts snapshots the fake's message texts under its lock.
func sentTexts(f *fakeTG) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.Sends))
	for _, s := range f.Sends {
		out = append(out, s.Text)
	}
	return out
}

// countText reports how many sends had exactly this text.
func countText(f *fakeTG, want string) int {
	n := 0
	for _, t := range sentTexts(f) {
		if t == want {
			n++
		}
	}
	return n
}

// hasTextPrefix reports whether any send starts with prefix (used for the
// tokenized messages, whose {u}/{n} are filled in).
func hasTextPrefix(f *fakeTG, prefix string) bool {
	for _, t := range sentTexts(f) {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// waitBatchDone blocks until the chat's streaming session has settled — every
// fetch claimed its slot and delivered (or dropped) its result.
//
// Tests must join HERE rather than on Shutdown: Shutdown cancels the fetch
// context, which a fetch that has not yet acquired its subprocess slot correctly
// treats as "give up" — so joining on Shutdown alone would race a real batch into
// a cancelled one.
func waitBatchDone(t *testing.T, b *Bot, chatID int64) {
	t.Helper()
	st := b.states.Get(chatID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !b.otpSessionLive(st) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("batch for chat %d never settled", chatID)
}

// armStreaming puts the chat into the STREAMING phase directly (n fetches in
// flight, started now) without running a batch.
func armStreaming(b *Bot, chatID int64, n int) *ChatState {
	st := b.states.Get(chatID)
	st.mu.Lock()
	s := st.otpSessionLocked()
	s.Active = n
	s.StartedAt = b.now()
	st.mu.Unlock()
	return st
}

// ---- otp_guard ---------------------------------------------------------------

// TestOTPGuardBlocksCommands — otp_guard @11647: while a session is live, every
// other command is refused behind the stop/keep prompt and the real handler never
// runs. Both phases (await + streaming) must block.
func TestOTPGuardBlocksCommands(t *testing.T) {
	ctx := context.Background()
	c := &i18n.C().OTP

	cases := []struct {
		name string
		arm  func(b *Bot) *ChatState
	}{
		{"await phase (pending_code)", func(b *Bot) *ChatState {
			st := b.states.Get(1)
			st.mu.Lock()
			st.Code = &OTPCtx{}
			st.mu.Unlock()
			return st
		}},
		{"streaming phase", func(b *Bot) *ChatState { return armStreaming(b, 1, 2) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f := otpTestBot("999")
			tc.arm(b)
			b.handleCommand(ctx, 1, "language", "en")

			if got := countText(f, c.OTPBusyWarn.T(i18n.EN)); got != 1 {
				t.Fatalf("busy warning sent %d times, want 1 (sends=%v)", got, sentTexts(f))
			}
			// The blocked command must NOT have run.
			if countText(f, i18n.C().LangPrompt.T(i18n.EN)) != 0 {
				t.Errorf("blocked command still ran")
			}
			// The prompt carries the two control buttons, and the stop button
			// resumes the command that was blocked.
			f.mu.Lock()
			rows := inlineRows(f.Sends[0])
			f.mu.Unlock()
			if btn, ok := findCB(rows, "otpstop:"); !ok || btn.CallbackData != "otpstop:language" {
				t.Errorf("stop button must resume the blocked command, got %q", btn.CallbackData)
			}
			if _, ok := findCB(rows, "otpkeep"); !ok {
				t.Errorf("keep button missing from the guard prompt")
			}
		})
	}
}

// TestOTPGuardBlocksCallbacks — otp_guard @12428: inline taps are blocked too,
// EXCEPT the otpkeep / otpstop: control taps (otherwise the confirmation itself
// could never be answered). The guard also runs BEFORE the tapped-menu delete, so
// the button survives for a re-tap.
func TestOTPGuardBlocksCallbacks(t *testing.T) {
	ctx := context.Background()
	c := &i18n.C().OTP

	t.Run("ordinary tap blocked, menu not deleted", func(t *testing.T) {
		b, f := otpTestBot("999")
		armStreaming(b, 1, 1)
		b.Process(ctx, callbackUpdate(1, "ss:info"))

		if got := countText(f, c.OTPBusyWarn.T(i18n.EN)); got != 1 {
			t.Fatalf("busy warning sent %d times, want 1 (sends=%v)", got, sentTexts(f))
		}
		f.mu.Lock()
		deletes := len(f.Deletes)
		f.mu.Unlock()
		if deletes != 0 {
			t.Errorf("guard must run BEFORE the tapped-menu delete, deletes=%d", deletes)
		}
	})

	t.Run("otpkeep exempt", func(t *testing.T) {
		b, f := otpTestBot("999")
		armStreaming(b, 1, 1)
		b.Process(ctx, callbackUpdate(1, "otpkeep"))

		if countText(f, c.OTPBusyWarn.T(i18n.EN)) != 0 {
			t.Errorf("otpkeep must be exempt from the guard")
		}
		want := i18n.F(c.OTPKept.T(i18n.EN), "n", itoa(otpMaxBatch))
		if countText(f, want) != 1 {
			t.Errorf("otpkeep should send OTP_KEPT, sends=%v", sentTexts(f))
		}
		// Keeping leaves the session live.
		if !b.otpSessionLive(b.states.Get(1)) {
			t.Errorf("otpkeep must leave the session live")
		}
	})

	t.Run("otpstop exempt, stops and resumes", func(t *testing.T) {
		b, f := otpTestBot("999")
		st := armStreaming(b, 1, 1)
		before := epochOf(st)
		b.Process(ctx, callbackUpdate(1, "otpstop:language"))

		if countText(f, c.OTPBusyWarn.T(i18n.EN)) != 0 {
			t.Errorf("otpstop must be exempt from the guard")
		}
		if countText(f, c.OTPStopped.T(i18n.EN)) != 1 {
			t.Errorf("otpstop should send OTP_STOPPED, sends=%v", sentTexts(f))
		}
		// resume_cmd re-runs the blocked command.
		if countText(f, i18n.C().LangPrompt.T(i18n.EN)) != 1 {
			t.Errorf("otpstop:<cmd> should resume the command, sends=%v", sentTexts(f))
		}
		if b.otpSessionLive(st) {
			t.Errorf("session must be dead after otpstop")
		}
		if epochOf(st) != before+1 {
			t.Errorf("otpstop must bump the epoch (%d -> %d)", before, epochOf(st))
		}
	})

	t.Run("otpstop with empty resume runs nothing", func(t *testing.T) {
		b, f := otpTestBot("999")
		armStreaming(b, 1, 1)
		b.Process(ctx, callbackUpdate(1, "otpstop:"))
		if got := sentTexts(f); len(got) != 1 || got[0] != c.OTPStopped.T(i18n.EN) {
			t.Errorf("bare otpstop: should send only OTP_STOPPED, sends=%v", got)
		}
	})
}

// epochOf reads the chat's current session epoch.
func epochOf(st *ChatState) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.OTPSession == nil {
		return 0
	}
	return st.OTPSession.Epoch
}

// ---- gates: rate limit / batch cap / ownership -------------------------------

// TestOTPRateLimit — _otp_rate_ok @8155: one accepted request per OTP_COOLDOWN_S
// per chat, admins exempt, and the refusal names the remaining wait.
func TestOTPRateLimit(t *testing.T) {
	ctx := context.Background()
	c := &i18n.C().OTP

	t.Run("second request inside the cooldown is refused", func(t *testing.T) {
		b, f := otpTestBot("999") // chat 1 is a plain customer
		st := b.states.Get(1)
		// First call: accepted (stamps LastAccepted).
		if !b.otpRateOK(ctx, 1, i18n.EN, st) {
			t.Fatalf("first request must be accepted")
		}
		// Second, immediately after: refused with a wait of 1..15s.
		if b.otpRateOK(ctx, 1, i18n.EN, st) {
			t.Fatalf("second request inside the cooldown must be refused")
		}
		if !hasTextPrefix(f, strings.SplitN(c.OTPRateLimit.T(i18n.EN), "{", 2)[0]) {
			t.Errorf("refusal should send OTP_RATELIMIT, sends=%v", sentTexts(f))
		}
	})

	t.Run("cooldown elapses", func(t *testing.T) {
		b, _ := otpTestBot("999")
		b.otpCooldown = 0 // no cooldown configured -> never refuses
		st := b.states.Get(1)
		if !b.otpRateOK(ctx, 1, i18n.EN, st) || !b.otpRateOK(ctx, 1, i18n.EN, st) {
			t.Errorf("a zero cooldown must accept back-to-back requests")
		}
	})

	t.Run("admin exempt", func(t *testing.T) {
		b, f := otpTestBot("1") // chat 1 IS the admin
		st := b.states.Get(1)
		for i := 0; i < 3; i++ {
			if !b.otpRateOK(ctx, 1, i18n.EN, st) {
				t.Fatalf("admin must be cooldown-exempt (attempt %d)", i+1)
			}
		}
		if len(sentTexts(f)) != 0 {
			t.Errorf("admin exemption must send nothing, sends=%v", sentTexts(f))
		}
	})
}

// TestOTPBatchCap — _run_otp_request @8374: more than OTP_MAX_BATCH accounts is
// rejected with OTP_TOO_MANY, before the cooldown is even spent.
func TestOTPBatchCap(t *testing.T) {
	ctx := context.Background()
	c := &i18n.C().OTP
	b, f := otpTestBot("999")

	unames := []string{"u1", "u2", "u3", "u4", "u5", "u6"} // 6 > otpMaxBatch
	b.runOTPRequest(ctx, 1, i18n.EN, unames)

	want := i18n.F(c.OTPTooMany.T(i18n.EN), "n", itoa(otpMaxBatch))
	if got := sentTexts(f); len(got) != 1 || got[0] != want {
		t.Fatalf("over-cap batch should send only OTP_TOO_MANY, sends=%v", got)
	}
	// No session opened, and the cooldown was NOT spent (the cap check precedes it).
	if b.otpSessionLive(b.states.Get(1)) {
		t.Errorf("an over-cap batch must not open a session")
	}
	st := b.states.Get(1)
	st.mu.Lock()
	spent := !st.otpSessionLocked().LastAccepted.IsZero()
	st.mu.Unlock()
	if spent {
		t.Errorf("the cap rejection must not spend the cooldown")
	}
}

// TestOTPOwnershipRejection — _run_otp_request @8382-8396: the chat must own
// EVERY requested account; one missing rejects the WHOLE batch and the message
// names the offenders. A test Bot has no store/stock, so a customer owns nothing
// (the gate fails CLOSED) while a FULL admin bypasses it entirely.
func TestOTPOwnershipRejection(t *testing.T) {
	ctx := context.Background()
	c := &i18n.C().OTP

	t.Run("customer owns nothing: whole batch rejected, offenders named", func(t *testing.T) {
		b, f := otpTestBot("999")
		b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice", "bob"})

		want := i18n.F(c.CodeNotOwned.T(i18n.EN), "u", "alice, bob")
		if got := sentTexts(f); len(got) != 1 || got[0] != want {
			t.Fatalf("should send only CODE_NOT_OWNED naming both, sends=%v", got)
		}
		if b.otpSessionLive(b.states.Get(1)) {
			t.Errorf("a rejected batch must not open a session")
		}
	})

	t.Run("full admin bypasses ownership", func(t *testing.T) {
		b, f := otpTestBot("1") // chat 1 is the FULL admin
		done := make(chan string, 2)
		b.otpFetch = func(_ context.Context, u string) (string, string, error) {
			done <- u
			return "123456", "", nil
		}
		b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice", "bob"})
		// Wait for BOTH fetches to be entered before draining: Shutdown cancels the
		// fetch context, so joining first would race a not-yet-started fetch into a
		// cancelled acquire.
		fetched := map[string]bool{}
		for i := 0; i < 2; i++ {
			select {
			case u := <-done:
				fetched[u] = true
			case <-time.After(5 * time.Second):
				t.Fatalf("fetch %d never ran (got %v)", i+1, fetched)
			}
		}
		b.Shutdown()

		if countText(f, i18n.F(c.CodeNotOwned.T(i18n.EN), "u", "alice, bob")) != 0 {
			t.Errorf("a full admin must not be ownership-gated, sends=%v", sentTexts(f))
		}
		if !fetched["alice"] || !fetched["bob"] {
			t.Errorf("both accounts should have been fetched, got %v", fetched)
		}
		if !hasTextPrefix(f, "⏳ Retrieving OTP") {
			t.Errorf("accepted batch should send CODE_WORKING, sends=%v", sentTexts(f))
		}
	})
}

// TestOTPStandardAdminIsOwnerScoped — the batch gate's `not is_super_admin`
// branch (@8385-8389: "Standard admins are owner-scoped like any customer").
//
// A STANDARD admin (granted via admins.json, not ADMIN_ID/DEV_IDS) must be
// ownership-gated exactly like a customer: OTP / reset-link retrieval hands over
// a live account, so the bypass is FULL-admin-only. Regression test for the
// isAdmin short-circuit leaking the bypass to standard admins.
func TestOTPStandardAdminIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("999") // chat 1 is NOT the owner
	b.adminsMu.Lock()
	b.admins["1"] = true // grant STANDARD admin (what M3.1's admins.json will do)
	b.adminsMu.Unlock()
	if b.isSuperAdmin(1) || !b.isAdmin(1) {
		t.Fatalf("setup: chat 1 must be a STANDARD admin (super=%v admin=%v)",
			b.isSuperAdmin(1), b.isAdmin(1))
	}

	b.runOTPRequest(ctx, 1, i18n.EN, []string{"someone-elses-account"})
	t.Cleanup(b.Shutdown)

	want := i18n.F(i18n.C().OTP.CodeNotOwned.T(i18n.EN), "u", "someone-elses-account")
	if got := sentTexts(f); len(got) != 1 || got[0] != want {
		t.Fatalf("a standard admin must be ownership-gated, sends=%v", got)
	}
	if b.otpSessionLive(b.states.Get(1)) {
		t.Errorf("a rejected standard-admin batch must not open a session")
	}
	// The strict scan is what the gate must use; the isAdmin-short-circuiting
	// variant stays available for Python-parity call sites.
	if b.chatOwnsAccount(1, "someone-elses-account") {
		t.Errorf("chatOwnsAccount must not short-circuit on isAdmin")
	}
	if !b.chatOwnsUsername(1, "someone-elses-account") {
		t.Errorf("chatOwnsUsername keeps Python's isAdmin short-circuit (@8145)")
	}
}

// TestOTPFetchPanicSettlesAndReleases — the `finally` pair Python wraps the fetch
// in (@8299 sem.release, @8326 session claim). A panic out of the node runner must
// NOT (a) kill the worker goroutine, (b) leak one of the ten global subprocess
// slots, or (c) leave the chat locked until the 240s watchdog.
func TestOTPFetchPanicSettlesAndReleases(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("1") // full admin: ownership/cooldown exempt

	b.otpFetch = func(context.Context, string) (string, string, error) {
		panic("boom from the node runner")
	}
	b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice"})
	waitBatchDone(t, b, 1) // (c): the session settled despite the panic
	b.Shutdown()           // (a): the worker did not die mid-flight

	if held := len(b.otpSem); held != 0 { // (b)
		t.Errorf("a panicking fetch leaked %d subprocess slot(s)", held)
	}
	// A panic is an ordinary failed fetch to the customer: the per-account notice,
	// never a leaked payload.
	if !hasTextPrefix(f, "⚠️ No recent OTP or reset link found for alice") {
		t.Errorf("a panicking fetch should still report CODE_FAIL, sends=%v", sentTexts(f))
	}
	// Every slot is still usable afterwards, so one fault can't degrade the host.
	for i := 0; i < otpMaxConcurrency; i++ {
		if !b.otpAcquireSlot(ctx) {
			t.Fatalf("slot %d unavailable after a panic — the ceiling leaked", i+1)
		}
	}
}

// TestChatOwnsUsernameFailsClosed — chat_owns_username @8140: with no store/stock
// wired the answer is NO for a customer (an authorization decision must fail
// closed) and YES for an admin (Python's is_admin short-circuit).
func TestChatOwnsUsernameFailsClosed(t *testing.T) {
	b, _ := otpTestBot("999")
	if b.chatOwnsUsername(1, "alice") {
		t.Errorf("customer ownership must fail closed with no store wired")
	}
	adminBot, _ := otpTestBot("1")
	if !adminBot.chatOwnsUsername(1, "alice") {
		t.Errorf("is_admin must short-circuit ownership (@8145)")
	}
}

// ---- session races -----------------------------------------------------------

// TestOTPDoubleSubmitOpensOneSession — _otp_session_start @8194: two updates for
// the same chat racing through the thread pool must open exactly ONE session and
// fire exactly ONE batch (a duplicate would mint a second paid fetch round).
//
// The injected fetch blocks until released, so both submits overlap for real.
func TestOTPDoubleSubmitOpensOneSession(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("1") // admin: skips ownership + cooldown, isolating the race

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var fetches int
	var fmu sync.Mutex
	b.otpFetch = func(context.Context, string) (string, string, error) {
		fmu.Lock()
		fetches++
		fmu.Unlock()
		entered <- struct{}{}
		<-release
		return "123456", "", nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice"})
		}()
	}
	wg.Wait() // both submits have returned; one is streaming, one no-oped
	// The winning submit's fetch has entered (so "fetch ran once" is observable
	// without racing goroutine startup).
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("the winning batch's fetch never ran")
	}

	// Exactly one CODE_WORKING ack -> exactly one batch was dispatched.
	if got := countText(f, i18n.F(i18n.C().OTP.CodeWorking.T(i18n.EN), "u", "alice")); got != 1 {
		t.Fatalf("CODE_WORKING sent %d times, want exactly 1 (sends=%v)", got, sentTexts(f))
	}
	st := b.states.Get(1)
	st.mu.Lock()
	active := st.OTPSession.Active
	st.mu.Unlock()
	if active != 1 {
		t.Errorf("Active = %d, want 1 (one session, one fetch)", active)
	}

	close(release)
	waitBatchDone(t, b, 1)
	b.Shutdown()

	fmu.Lock()
	n := fetches
	fmu.Unlock()
	if n != 1 {
		t.Errorf("fetch ran %d times, want 1", n)
	}
	if b.otpSessionLive(st) {
		t.Errorf("the session must close once its last fetch settles")
	}
}

// TestOTPStopDropsInFlightResult — fetch_login_code @8326-8334 +
// _otp_session_claim @8216: a customer who stops the session never receives a
// trailing code. The fetch is held open, the session is stopped (bumping the
// epoch), and the released result must be DROPPED, not DM'd.
func TestOTPStopDropsInFlightResult(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("1")

	entered := make(chan struct{})
	release := make(chan struct{})
	b.otpFetch = func(context.Context, string) (string, string, error) {
		close(entered)
		<-release
		return "654321", "", nil
	}
	b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice"})
	<-entered // the fetch is in flight

	// Stop mid-flight (the otpstop: path), then let the fetch finish.
	b.Process(ctx, callbackUpdate(1, "otpstop:"))
	close(release)
	b.Shutdown()

	for _, txt := range sentTexts(f) {
		if strings.Contains(txt, "654321") {
			t.Fatalf("a stopped session must never deliver its in-flight code")
		}
	}
	if countText(f, i18n.C().OTP.OTPStopped.T(i18n.EN)) != 1 {
		t.Errorf("stop should confirm once, sends=%v", sentTexts(f))
	}
	// The stop already closed the session; the dropped claim must not corrupt it.
	st := b.states.Get(1)
	st.mu.Lock()
	active := st.OTPSession.Active
	st.mu.Unlock()
	if active != 0 {
		t.Errorf("Active = %d after stop, want 0", active)
	}
}

// TestOTPStaleSessionWatchdog — _otp_session_live @8172-8192: a streaming session
// older than OTP_SESSION_MAX_S is treated as dead and cleared, so a chat whose
// fetch goroutine died before settling self-heals instead of staying locked.
func TestOTPStaleSessionWatchdog(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("999")

	// A batch that started just inside the window is still live...
	st := b.states.Get(1)
	st.mu.Lock()
	s := st.otpSessionLocked()
	s.Active = 2
	s.StartedAt = time.Now().Add(-(otpSessionMax - time.Second))
	st.mu.Unlock()
	if !b.otpSessionLive(st) {
		t.Fatalf("a session inside OTP_SESSION_MAX_S must still be live")
	}

	// ...and one past it is swept, bumping the epoch so the presumed-dead fetches
	// can never deliver.
	before := epochOf(st)
	st.mu.Lock()
	st.OTPSession.StartedAt = time.Now().Add(-(otpSessionMax + time.Second))
	st.mu.Unlock()
	if b.otpSessionLive(st) {
		t.Fatalf("a session past OTP_SESSION_MAX_S must be swept")
	}
	if epochOf(st) != before+1 {
		t.Errorf("the watchdog must bump the epoch (%d -> %d)", before, epochOf(st))
	}
	// Self-healed: the guard no longer blocks the chat.
	b.handleCommand(ctx, 1, "language", "en")
	if countText(f, i18n.C().OTP.OTPBusyWarn.T(i18n.EN)) != 0 {
		t.Errorf("a swept session must not block the chat, sends=%v", sentTexts(f))
	}
	if countText(f, i18n.C().LangPrompt.T(i18n.EN)) != 1 {
		t.Errorf("the command should run after the sweep, sends=%v", sentTexts(f))
	}
}

// ---- parsing -----------------------------------------------------------------

// TestParseOTPUsernames — _parse_otp_usernames @8339: charset gate, 64-char cap,
// order-preserving dedup, and NO count cap (the caller reports "too many").
func TestParseOTPUsernames(t *testing.T) {
	long := strings.Repeat("a", 80)

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "alice", []string{"alice"}},
		{"space separated", "alice bob carol", []string{"alice", "bob", "carol"}},
		{"newline separated", "alice\nbob", []string{"alice", "bob"}},
		{"mixed whitespace collapses", "  alice \t bob  ", []string{"alice", "bob"}},
		{"dedup keeps first-seen order", "bob alice bob", []string{"bob", "alice"}},
		{"legal charset kept", "a.b_c-d1", []string{"a.b_c-d1"}},
		{"illegal chars dropped whole word", "al!ce bob", []string{"bob"}},
		{"path traversal dropped", "../etc/passwd bob", []string{"bob"}},
		{"shell metachars dropped", "alice;rm bob", []string{"bob"}},
		{"non-ascii dropped", "álice bob", []string{"bob"}},
		{"empty", "", nil},
		{"whitespace only", "   \n\t ", nil},
		{"64-char cap applied before the charset check", long, []string{strings.Repeat("a", 64)}},
		// The count is deliberately NOT capped here.
		{"over-cap count survives parsing", "u1 u2 u3 u4 u5 u6 u7",
			[]string{"u1", "u2", "u3", "u4", "u5", "u6", "u7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOTPUsernames(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseOTPUsernames(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("parseOTPUsernames(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestParseOTPOutput — fetch_login_code @8300-8305: the helper may print an OTP
// line, a RESET line, or both; anything else (including OTP_ERROR:) yields
// nothing found.
func TestParseOTPOutput(t *testing.T) {
	cases := []struct {
		name, blob, code, reset string
	}{
		{"code only", "OTP:123456\n", "123456", ""},
		{"reset only", "RESET:https://example.com/r/abc\n", "", "https://example.com/r/abc"},
		{"both", "OTP:123456\nRESET:https://e/x\n", "123456", "https://e/x"},
		{"error line", "OTP_ERROR:no-recent-code\n", "", ""},
		{"noise around", "starting…\nOTP:999111\ndone\n", "999111", ""},
		{"empty", "", "", ""},
		{"last wins", "OTP:111111\nOTP:222222\n", "222222", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reset := parseOTPOutput(tc.blob)
			if code != tc.code || reset != tc.reset {
				t.Errorf("parseOTPOutput(%q) = (%q,%q), want (%q,%q)", tc.blob, code, reset, tc.code, tc.reset)
			}
		})
	}
}

// ---- capture #36 -------------------------------------------------------------

// TestOTPCaptureEscapeSemantics — pending_code is capture #36 with PEEK
// semantics (@18341, @19059): a slash-command or a keyboard label escapes the
// capture and leaves the slot ARMED; a plain line is consumed.
func TestOTPCaptureEscapeSemantics(t *testing.T) {
	ctx := context.Background()

	armed := func(st *ChatState) bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.Code != nil
	}

	t.Run("slash command escapes, slot stays armed", func(t *testing.T) {
		b, f := otpTestBot("999")
		primeKB(b, 1)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Code = &OTPCtx{}
		st.mu.Unlock()

		b.Process(ctx, textUpdate(1, "/language"))
		if !armed(st) {
			t.Errorf("a peek escape must leave pending_code armed")
		}
		// The command itself is blocked by the guard (the await phase IS live).
		if countText(f, i18n.C().OTP.OTPBusyWarn.T(i18n.EN)) != 1 {
			t.Errorf("the escaped command should hit the OTP guard, sends=%v", sentTexts(f))
		}
	})

	t.Run("keyboard label escapes, slot stays armed", func(t *testing.T) {
		b, _ := otpTestBot("999")
		primeKB(b, 1)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Code = &OTPCtx{}
		st.mu.Unlock()

		b.Process(ctx, textUpdate(1, i18n.LangButton))
		if !armed(st) {
			t.Errorf("a tapped label must leave pending_code armed")
		}
	})

	t.Run("plain line is consumed by the capture", func(t *testing.T) {
		b, f := otpTestBot("999")
		primeKB(b, 1)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Code = &OTPCtx{}
		st.mu.Unlock()

		b.Process(ctx, textUpdate(1, "alice"))
		// The customer owns nothing here, so the batch is rejected — but the
		// capture DID consume the line (the refusal proves it reached the OTP path)
		// and the peek leaves the slot armed for a retry.
		if !hasTextPrefix(f, "⛔ These accounts aren't in any of your orders") {
			t.Errorf("the capture should have run the OTP request, sends=%v", sentTexts(f))
		}
		if !armed(st) {
			t.Errorf("a rejected submit must leave the customer in the await phase")
		}
	})

	t.Run("unparseable line re-prompts and stays armed", func(t *testing.T) {
		b, f := otpTestBot("999")
		primeKB(b, 1)
		st := b.states.Get(1)
		st.mu.Lock()
		st.Code = &OTPCtx{}
		st.mu.Unlock()

		b.Process(ctx, textUpdate(1, "!!! ???")) // nothing survives the charset gate
		if !hasTextPrefix(f, "🔑 First, try to SIGN IN") {
			t.Errorf("an empty parse should re-show CODE_PROMPT, sends=%v", sentTexts(f))
		}
		if !armed(st) {
			t.Errorf("an empty parse must keep the slot armed")
		}
	})
}

// ---- entry points ------------------------------------------------------------

// TestGetCodeCommand — the getcode branch @11674-11680: clear sibling captures,
// arm pending_code, prompt with {n} = OTP_MAX_BATCH.
func TestGetCodeCommand(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("999")

	// A stale sibling capture must be dropped by the ClearCustomerPending call.
	st := b.states.Get(1)
	st.mu.Lock()
	st.Deposit = &DepositCtx{Method: "qr"}
	st.mu.Unlock()

	b.handleCommand(ctx, 1, "getcode", "en")

	want := i18n.F(i18n.C().OTP.CodePrompt.T(i18n.EN), "n", itoa(otpMaxBatch))
	if got := sentTexts(f); len(got) != 1 || got[0] != want {
		t.Fatalf("getcode should send CODE_PROMPT, sends=%v", got)
	}
	if !strings.Contains(want, itoa(otpMaxBatch)) {
		t.Errorf("the prompt must state the batch cap")
	}
	st.mu.Lock()
	code, dep := st.Code, st.Deposit
	st.mu.Unlock()
	if code == nil {
		t.Errorf("getcode must arm pending_code")
	}
	if dep != nil {
		t.Errorf("getcode must clear sibling customer captures first (@11677)")
	}
	// The "Login code" keyboard label routes here (@7021).
	if labelToCmd(i18n.CodeBtn) != "getcode" {
		t.Errorf("CODE_BTN must map to getcode")
	}
}

// TestOTPSlashCommand — the /otp branch @19413-19431: the argument after the
// first space is the batch, a bare /otp re-prompts, and a live session blocks it
// behind the stop prompt.
func TestOTPSlashCommand(t *testing.T) {
	ctx := context.Background()
	c := &i18n.C().OTP

	t.Run("argument is threaded through", func(t *testing.T) {
		b, f := otpTestBot("999")
		primeKB(b, 1)
		b.Process(ctx, textUpdate(1, "/otp alice bob"))
		// Ownership rejects it, but the offenders prove BOTH argv words arrived.
		want := i18n.F(c.CodeNotOwned.T(i18n.EN), "u", "alice, bob")
		if countText(f, want) != 1 {
			t.Fatalf("/otp args must reach runOTPRequest, sends=%v", sentTexts(f))
		}
	})

	t.Run("bare /otp prompts", func(t *testing.T) {
		b, f := otpTestBot("999")
		primeKB(b, 1)
		b.Process(ctx, textUpdate(1, "/otp"))
		want := i18n.F(c.CodePrompt.T(i18n.EN), "n", itoa(otpMaxBatch))
		if got := sentTexts(f); len(got) != 1 || got[0] != want {
			t.Fatalf("bare /otp should send CODE_PROMPT, sends=%v", got)
		}
		st := b.states.Get(1)
		st.mu.Lock()
		armed := st.Code != nil
		st.mu.Unlock()
		if !armed {
			t.Errorf("bare /otp must arm the await phase")
		}
	})

	t.Run("blocked while a session streams", func(t *testing.T) {
		b, f := otpTestBot("999")
		primeKB(b, 1)
		armStreaming(b, 1, 1)
		b.Process(ctx, textUpdate(1, "/otp alice"))
		if countText(f, c.OTPBusyWarn.T(i18n.EN)) != 1 {
			t.Fatalf("/otp must be gated by the guard too, sends=%v", sentTexts(f))
		}
		if hasTextPrefix(f, "⏳ Retrieving OTP") {
			t.Errorf("a blocked /otp must not dispatch a batch")
		}
	})
}

// TestOTPDeliversCodeAndReset — fetch_login_code @8306-8318: a successful fetch
// DMs the code and/or the reset link in HTML mode, one message per account.
func TestOTPDeliversCodeAndReset(t *testing.T) {
	ctx := context.Background()
	b, f := otpTestBot("1") // admin: ownership-exempt

	b.otpFetch = func(_ context.Context, u string) (string, string, error) {
		if u == "alice" {
			return "123456", "https://reset.example/abc", nil
		}
		return "", "", nil // bob: nothing found -> CODE_FAIL
	}
	b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice", "bob"})
	waitBatchDone(t, b, 1)
	b.Shutdown()

	var aliceMsg, bobMsg string
	f.mu.Lock()
	for _, s := range f.Sends {
		switch {
		case strings.Contains(s.Text, "123456"):
			aliceMsg = s.Text
			if s.ParseMode != "HTML" {
				t.Errorf("a delivered code must use parse_mode=HTML, got %q", s.ParseMode)
			}
		case strings.Contains(s.Text, "bob"):
			bobMsg = s.Text
		}
	}
	f.mu.Unlock()

	if aliceMsg == "" {
		t.Fatalf("no code delivered, sends=%v", sentTexts(f))
	}
	// Both the code and the reset link land in ONE message (@8316).
	if !strings.Contains(aliceMsg, "<b>123456</b>") || !strings.Contains(aliceMsg, "https://reset.example/abc") {
		t.Errorf("code + reset link should share one message, got %q", aliceMsg)
	}
	if !strings.HasPrefix(bobMsg, "⚠️ No recent OTP or reset link found for bob") {
		t.Errorf("an empty fetch should send CODE_FAIL, got %q", bobMsg)
	}
	// The session closed once both fetches settled.
	if b.otpSessionLive(b.states.Get(1)) {
		t.Errorf("the session must close after the last fetch")
	}
}

// TestOTPConcurrencyCeiling — OTP_MAX_CONCURRENCY @680: the subprocess-slot
// semaphore is ONE per Bot and bounded, and an acquire that cannot be satisfied
// times out (fail fast) rather than queueing.
func TestOTPConcurrencyCeiling(t *testing.T) {
	b, _ := otpTestBot("1")

	if cap(b.otpSem) != otpMaxConcurrency {
		t.Fatalf("otpSem cap = %d, want %d", cap(b.otpSem), otpMaxConcurrency)
	}
	ctx := context.Background()
	for i := 0; i < otpMaxConcurrency; i++ {
		if !b.otpAcquireSlot(ctx) {
			t.Fatalf("slot %d should be available", i+1)
		}
	}
	// Saturated: the next acquire fails fast instead of blocking for 45s.
	b.otpSemAcquire = 10 * time.Millisecond
	start := time.Now()
	if b.otpAcquireSlot(ctx) {
		t.Fatalf("acquire must fail once the ceiling is reached")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("acquire waited %v, want a fast timeout", elapsed)
	}
	// Releasing one frees exactly one.
	b.otpReleaseSlot()
	if !b.otpAcquireSlot(ctx) {
		t.Errorf("a released slot must be reusable")
	}
}

// TestOTPFetchTimeoutStillSettlesSession — fetch_login_code @8326: the session
// counter is settled on EVERY path, so a fetch that cannot even get a subprocess
// slot can't leave the chat locked.
func TestOTPFetchTimeoutStillSettlesSession(t *testing.T) {
	ctx := context.Background()
	b, _ := otpTestBot("1")
	// Saturate the ceiling so the fetch's acquire times out immediately.
	for i := 0; i < otpMaxConcurrency; i++ {
		b.otpSem <- struct{}{}
	}
	b.otpSemAcquire = 10 * time.Millisecond
	b.runOTPRequest(ctx, 1, i18n.EN, []string{"alice"})
	waitBatchDone(t, b, 1)
	b.Shutdown()

	// The key invariant is that the session is settled (chat not locked), NOT
	// that CODE_FAIL was sent — on a semaphore timeout we exit silently to avoid
	// sending a delivery message when we never even attempted the fetch.
	if b.otpSessionLive(b.states.Get(1)) {
		t.Errorf("the session must be settled even when no slot was available")
	}
}
