package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// isSubsequence reports whether got appears in canon in order (not necessarily
// contiguous) — the ordering contract every landed capture chain must satisfy
// against CanonicalCaptureOrder (handlers-go.md §7).
func isSubsequence(got, canon []string) bool {
	j := 0
	for _, name := range got {
		for j < len(canon) && canon[j] != name {
			j++
		}
		if j == len(canon) {
			return false
		}
		j++
	}
	return true
}

// TestCaptureChainOrder — handlers-go.md test contract #1. Each slice APPENDS
// its captures; whatever is registered must stay an in-order subsequence of the
// canonical list (never reordered). M2.2 adds only "purchase" (entry #48, LAST).
func TestCaptureChainOrder(t *testing.T) {
	b, _ := newTestBot("1", nil)
	got := b.captureNames()
	if !isSubsequence(got, CanonicalCaptureOrder) {
		t.Errorf("capture chain %v is not an in-order subsequence of canonical", got)
	}
	// M2.2 registers the buy-quantity capture; M2.3 adds the deposit-amount
	// capture ahead of it (deposit #46 < purchase #50); M2.5 splices the two
	// API-key captures between them (#47/#48); the M2.6 login-code slice puts
	// pending_code (#38) ahead of pending_deposit — it is the highest-priority
	// customer capture in the Python priority comment @3632
	// (`code > sell > deposit > apikey > …`). Together they match that priority
	// comment, including the M2.6 promo/referral ordercode capture (#49)
	// between apikeyrc and purchase.
	// `code > … > deposit > apikey > apikeyrc > ordercode > purchase`.
	// (The customer tx-hash capture "txid" is gone: deposits now credit
	// hands-free via the watcher with no hash paste, so there is no such
	// capture to splice in.)
	// adminsettings.go's 7 captures (setrate/setkirourl/setkirokey/
	// setkiroacct/setbep20/settron/setmaxqty, canonical #10-#23) are spliced
	// in ahead of "code" (#38-ish) by registerAdminSettings running after
	// registerCodeCapture (dispatch.go).
	// adminpricing.go (🏷️ PRICING & DISCOUNTS) splices its 11 captures in
	// last: setapiprice/setapimin/setapimax/setkiroprice/setkiromin/
	// setkiromax/setapicap (canonical #11-#17) ahead of "setkirourl" (#18);
	// discount/promo (#35/#37) ahead of "code" (#38); tier/price (#42/#43)
	// ahead of "deposit" (#46).
	// adminusers.go (👥 CUSTOMERS / 🛡️ ADMIN ROLES / whitelist / resellers)
	// registers next-to-last: custlookup/addadmin/addreseller/addwl/rmadmin
	// land ahead of "setrate" (canonical #5-#9, all before #10), and
	// reseller_discount lands ahead of "promo" (canonical #35) — both anchors
	// already exist in the chain by the time registerAdminUsers runs. addbal
	// is NOT in this chain (M3/M4 fix) — it lives in the step-9 adminText
	// stages instead (TestAdminTextStageOrder), alongside "broadcast".
	// adminorders.go (🧾 ORDERS + refund subsystem) registers LAST of all:
	// "getord" (canonical #4) ahead of "custlookup" (#5); "refundreject" (#26)
	// and "refund" (#34), in that order, both ahead of "discount" (#35) —
	// both anchors already exist by the time registerAdminOrders runs.
	// adminstock.go (📦 STOCK) registers last of all: "upstock" (#1) ahead of
	// "getord" (#4); "newprod"/"renprod"/"provpool" (#27-#29) and
	// "setnotice"/"setdelivery"/"editfile" (#31-#33), in that order, both
	// ahead of "refund" (#34); "hold" (#41) ahead of "tier" (#42) — every
	// anchor already exists in the chain by the time registerAdminStock runs.
	want := []string{
		"upstock",
		"getord",
		"custlookup", "addadmin", "addreseller", "addwl", "rmadmin",
		"setapiprice", "setapimin", "setapimax", "setkiroprice", "setkiromin", "setkiromax", "setapicap",
		"setkirourl", "setkirokey", "setkiroacct", "setbep20", "settron", "setmaxqty",
		"refundreject",
		"newprod", "renprod", "provpool",
		"setnotice", "setdelivery", "editfile",
		"refund",
		"discount", "reseller_discount", "promo", "code",
		"hold",
		"tier", "price", "deposit", "apikey", "apikeyrc",
		"ordercode", "purchase",
	}
	if len(got) != len(want) {
		t.Fatalf("capture chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capture chain = %v, want %v", got, want)
		}
	}
	if CanonicalCaptureOrder[len(CanonicalCaptureOrder)-1] != "purchase" {
		t.Errorf("purchase must be the last canonical capture")
	}
	// The canonical list itself must stay unique + the pinned length (44 — the
	// micro-USDT flip removed the USD→VND rate capture "setrate", 45 to 44;
	// earlier: adminstock.go's Go-only "provpool" entry brought it 49 to 50;
	// M3/M4 fix removed "addbal", which moved to the step-9 adminText chain;
	// AWS IDC/sell removals took it back to 46; hands-free-deposits removed the
	// customer tx-hash capture "txid", 46 to 45).
	if len(CanonicalCaptureOrder) != 44 {
		t.Errorf("canonical order length = %d, want 44", len(CanonicalCaptureOrder))
	}
	seen := map[string]bool{}
	for _, n := range CanonicalCaptureOrder {
		if seen[n] {
			t.Errorf("duplicate capture name %q in canonical order", n)
		}
		seen[n] = true
	}
	// File intercepts: adminsettings.go registers the broadcast photo intercept
	// (step 5 — photo messages carry no text and skip the admin-text stages).
	// adminstock.go appends two more: "upstockfile" (adm:upstock's document
	// upload, py_stock_spec.md §1.4) and "provpoolfile" (adm:provpool's
	// upload — Python's ONLY wired input path for that flow, §6.2).
	wantFI := []string{"broadcastphoto", "upstockfile", "provpoolfile"}
	gotFI := b.fileInterceptNames()
	if len(gotFI) != len(wantFI) {
		t.Fatalf("file intercept stages = %v, want %v", gotFI, wantFI)
	}
	for i := range wantFI {
		if gotFI[i] != wantFI[i] {
			t.Errorf("file intercept stages = %v, want %v", gotFI, wantFI)
		}
	}
	// Step 9 now carries the admin panel entry stages (admin.go). Same contract
	// as the capture chain: whatever is registered must be an in-order
	// subsequence of the canonical list, so later admin slices can append.
	if !b.adminTextOrderOK() {
		t.Errorf("admin text stages %v are not an in-order subsequence of %v",
			b.adminTextNames(), CanonicalAdminTextOrder)
	}
	if got := b.adminTextNames(); len(got) == 0 {
		t.Errorf("admin text stages must be registered (the Admin button needs them)")
	}
}

// TestKeyboardVersionRepush — handlers-go.md test contract #6: the version flag
// is set before send, its three skip rules hold, and a send failure rolls the
// flag back so the chat retries.
func TestKeyboardVersionRepush(t *testing.T) {
	ctx := context.Background()

	t.Run("repush once then suppressed", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		b.Process(ctx, textUpdate(1, "hello"))
		if f.sendCount() != 1 {
			t.Fatalf("first plain message should push keyboard once, sends=%d", f.sendCount())
		}
		if got := f.Sends[0].Text; got != i18n.C().KBRefreshNote.T(i18n.EN) {
			t.Errorf("refresh note = %q", got)
		}
		if st := b.states.Get(1); st.KBVersionSent != KeyboardVersion {
			t.Errorf("KBVersionSent = %q, want %q", st.KBVersionSent, KeyboardVersion)
		}
		// Second message: no additional refresh (version already current).
		b.Process(ctx, textUpdate(1, "again"))
		if f.sendCount() != 1 {
			t.Errorf("second message re-pushed keyboard, sends=%d", f.sendCount())
		}
	})

	t.Run("skip on /start", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		b.Process(ctx, textUpdate(1, "/start"))
		// /start sends its OWN welcome (1 send) but must not send a refresh
		// note; and its handler records the version so no re-push follows.
		if f.sendCount() != 1 {
			t.Fatalf("sends=%d", f.sendCount())
		}
		if f.Sends[0].Text == i18n.C().KBRefreshNote.T(i18n.EN) {
			t.Errorf("/start must not send the keyboard-refresh note")
		}
	})

	t.Run("skip while mid-capture (code pending)", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		st := b.states.Get(1)
		st.Code = &OTPCtx{} // chat is answering a login-code prompt
		b.Process(ctx, textUpdate(1, "someusername"))
		// The M2.6 login-code capture now CONSUMES this text (and replies to it —
		// here with the ownership refusal, since a test Bot owns no orders), so the
		// assertion is that no REFRESH NOTE wedged between the customer's answer and
		// its result — not that nothing was sent at all (@17116-17119).
		for _, s := range f.Sends {
			if s.Text == i18n.C().KBRefreshNote.T(i18n.EN) {
				t.Errorf("refresh must be skipped while code capture is armed")
			}
		}
		if st.KBVersionSent != "" {
			t.Errorf("KBVersionSent = %q, want untouched while mid-capture", st.KBVersionSent)
		}
	})

	t.Run("rollback on send error", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		f.sendErr = context.DeadlineExceeded
		b.ensureKeyboard(ctx, 1, i18n.EN)
		if st := b.states.Get(1); st.KBVersionSent != "" {
			t.Errorf("KBVersionSent = %q after failed push, want rolled back to \"\"", st.KBVersionSent)
		}
	})
}

// TestLabelRouting — handlers-go.md test contract: every keyboard label (all
// languages, fixed buttons, legacy) resolves to its command; unknown text does
// not.
func TestLabelRouting(t *testing.T) {
	c := i18n.C()
	cases := []struct{ label, want string }{
		{c.DescFor("buy").EN, "buy"},
		{c.DescFor("buy").VI, "buy"},
		{c.DescFor("buy").ZH, "buy"},
		{c.DescFor("deposit").VI, "deposit"},
		{c.DescFor("history").ZH, "history"},
		{c.DescFor("info").EN, "info"},
		{c.DescFor("otp").VI, "otp"},
		{c.DescFor("language").ZH, "language"},
		{i18n.LangButton, "language"},
		{i18n.SupportButton, "support"},
		{i18n.CodeBtn, "getcode"},
		{c.SelfServiceBtn.EN, "selfservice"},
		{c.SelfServiceBtn.VI, "selfservice"},
		{c.SelfServiceBtn.ZH, "selfservice"},
		{i18n.SelfServiceBtnLegacy, "selfservice"},
		{"  " + i18n.LangButton + "  ", "language"}, // trimmed
		{"not a button", ""},
		{i18n.AdminButton, ""}, // admin button routes via step 9, NOT labelToCmd
	}
	for _, tc := range cases {
		if got := labelToCmd(tc.label); got != tc.want {
			t.Errorf("labelToCmd(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// TestBanGate — handlers-go.md test contract: a banned non-admin's message is
// silently dropped; a banned admin still proceeds.
func TestBanGate(t *testing.T) {
	ctx := context.Background()

	t.Run("banned customer dropped", func(t *testing.T) {
		b, f := newTestBot("999", map[string]bool{"1": true})
		b.Process(ctx, textUpdate(1, "/start"))
		if f.sendCount() != 0 {
			t.Errorf("banned user must get no reply, sends=%d", f.sendCount())
		}
	})

	t.Run("banned admin proceeds", func(t *testing.T) {
		// Admin id == chat id, and that same id is in the banned set.
		b, f := newTestBot("1", map[string]bool{"1": true})
		b.Process(ctx, textUpdate(1, "/start"))
		if f.sendCount() != 1 {
			t.Errorf("admin is exempt from the ban gate, sends=%d", f.sendCount())
		}
		if f.Sends[0].Text != i18n.C().Welcome.T(i18n.EN) {
			t.Errorf("expected welcome, got %q", f.Sends[0].Text)
		}
	})
}

// TestPrivateOnlyGate — handlers-go.md §2 step 3: a non-private chat is ignored.
func TestPrivateOnlyGate(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("999", nil)
	b.Process(ctx, textUpdateChatType(1, "/start", "group"))
	if f.sendCount() != 0 {
		t.Errorf("group message must be ignored, sends=%d", f.sendCount())
	}
	// A private message (explicit type) still works.
	b.Process(ctx, textUpdateChatType(2, "/start", "private"))
	if f.sendCount() != 1 {
		t.Errorf("private message should be handled, sends=%d", f.sendCount())
	}
}

// TestCoreCommands pins /start, /language and /support behavior end-to-end
// through Process (label + slash), including the admin keyboard row.
func TestCoreCommands(t *testing.T) {
	ctx := context.Background()

	t.Run("start shows welcome + keyboard", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		b.Process(ctx, textUpdate(1, "/start"))
		if f.sendCount() != 1 || f.Sends[0].Text != i18n.C().Welcome.T(i18n.EN) {
			t.Fatalf("unexpected /start sends: %+v", f.Sends)
		}
		if f.Sends[0].ReplyMarkup == nil {
			t.Errorf("/start must attach the main keyboard")
		}
	})

	t.Run("language shows picker", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		// The chat already has the keyboard version, so no refresh precedes the
		// language reply — pre-seed it to isolate the /language send.
		b.states.Get(1).KBVersionSent = KeyboardVersion
		b.Process(ctx, textUpdate(1, "/language"))
		if f.sendCount() != 1 || f.Sends[0].Text != i18n.C().LangPrompt.T(i18n.EN) {
			t.Fatalf("unexpected /language sends: %+v", f.Sends)
		}
	})

	t.Run("support via tapped button", func(t *testing.T) {
		b, f := newTestBot("999", nil)
		b.states.Get(1).KBVersionSent = KeyboardVersion
		b.Process(ctx, textUpdate(1, i18n.SupportButton))
		if f.sendCount() != 1 || !strings.Contains(f.Sends[0].Text, i18n.DefaultSupportHandle) {
			t.Fatalf("unexpected support sends: %+v", f.Sends)
		}
	})

	t.Run("admin gets admin row in keyboard", func(t *testing.T) {
		b, f := newTestBot("1", nil) // chat 1 is the admin
		b.Process(ctx, textUpdate(1, "/start"))
		mk := mainKeyboard(i18n.EN, true)
		last := mk.Keyboard[len(mk.Keyboard)-1]
		if last[len(last)-1].Text != i18n.AdminButton {
			t.Errorf("admin keyboard missing admin button: %+v", last)
		}
		_ = f
	})
}

// TestNameCapture pins step-7 display-name capture (@17098-17107): first+last
// with the @handle appended, or the bare handle when there's no name.
func TestNameCapture(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBot("999", nil)

	u := textUpdate(1, "hi")
	u.Message.From = &tg.User{ID: 1, FirstName: "Ann", LastName: "Lee", Username: "annlee"}
	b.Process(ctx, u)
	if got := b.nameFor(1); got != "Ann Lee (@annlee)" {
		t.Errorf("nameFor = %q, want %q", got, "Ann Lee (@annlee)")
	}

	// No name, only a username -> bare handle.
	u2 := textUpdate(2, "hi")
	u2.Message.From = &tg.User{ID: 2, Username: "solo"}
	b.Process(ctx, u2)
	if got := b.nameFor(2); got != "@solo" {
		t.Errorf("nameFor(bare handle) = %q, want %q", got, "@solo")
	}
}

// TestSetLangCallback — the setlang: language switch: language recorded, per-
// chat menu swapped, confirmation sent in the new language.
func TestSetLangCallback(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("999", nil)
	b.Process(ctx, callbackUpdate(1, "setlang:vi"))

	if len(f.Answers) == 0 {
		t.Fatalf("callback must be acked")
	}
	if got := b.langFor(1, "en"); got != i18n.VI {
		t.Errorf("langFor after setlang = %q, want vi", got)
	}
	if len(f.SetCommands) != 1 {
		t.Fatalf("setlang must swap the slash menu, setCommands=%d", len(f.SetCommands))
	}
	if sc := f.SetCommands[0].Scope; sc == nil || sc.Type != "chat" || sc.ChatID != 1 {
		t.Errorf("menu must be per-chat scoped, got %+v", f.SetCommands[0].Scope)
	}
	if f.sendCount() != 1 || f.Sends[0].Text != i18n.C().LangConfirm.T(i18n.VI) {
		t.Fatalf("expected VI confirm, sends=%+v", f.Sends)
	}

	// Ignore an unsupported language code (@15026 `lang in LANGS`).
	b2, f2 := newTestBot("999", nil)
	b2.Process(ctx, callbackUpdate(1, "setlang:fr"))
	if f2.sendCount() != 0 {
		t.Errorf("unsupported lang must send nothing, sends=%d", f2.sendCount())
	}
}

// envDurationTestBot builds a Bot from a .env file written into a fresh temp
// dir, mirroring auditprune_test.go's below-floor/dot-env fixtures (Store and
// Stock are nil — envDuration and the four New()-wired call sites under test
// here never touch either).
func envDurationTestBot(t *testing.T, envBody string) *Bot {
	t.Helper()
	dir := t.TempDir()
	if envBody != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	return New(Deps{TG: &fakeTG{}, Config: cfg, AdminID: "1"})
}

// TestEnvDurationClampsConfiguredZeroAndLogs is the pin for task #30's
// sharpest edge: an explicit 0 parses fine under envInt's own contract
// (dispatch.go), so without envDuration's clamp a configured
// STOCK_SWEEP_INTERVAL_S=0 would feed time.After(0) and fire immediately,
// spinning the folder-stock sweep against the buy-path per-product mutex on
// every loop iteration. Also pins the log line: an operator who typed 0 and
// silently got the floor back cannot tell that apart from "it is working as
// configured."
//
// Falsification: deleting the `if d < min` clamp in envDuration makes the
// returned duration 0 instead of the floor, and the first assertion fails;
// deleting only the log.Printf call leaves the duration correct but the
// second assertion fails.
func TestEnvDurationClampsConfiguredZeroAndLogs(t *testing.T) {
	b := envDurationTestBot(t, "MY_TEST_INTERVAL_S=0\n")

	var got time.Duration
	out := captureLog(t, func() {
		got = b.envDuration("MY_TEST_INTERVAL_S", 300*time.Second, 5*time.Second)
	})

	if got != 5*time.Second {
		t.Errorf("envDuration(configured 0) = %s, want the 5s floor, not 0", got)
	}
	if !strings.Contains(out, "MY_TEST_INTERVAL_S=0s") || !strings.Contains(out, "5s") {
		t.Errorf("envDuration logged %q, want it to name the below-floor configured "+
			"value AND the floor it was raised to — the only signal an operator has "+
			"that their 0 was silently overridden", out)
	}
}

// TestEnvDurationClampsConfiguredNegativeAndLogs is the negative-value twin
// of the zero case: strconv.Atoi parses "-10" without error, so envInt
// returns it as-is — time.After(negative) also fires immediately, same hot-
// loop risk as 0.
//
// Falsification: same as the zero test — removing the clamp or its log line
// reddens the matching assertion.
func TestEnvDurationClampsConfiguredNegativeAndLogs(t *testing.T) {
	b := envDurationTestBot(t, "MY_TEST_INTERVAL_S=-10\n")

	var got time.Duration
	out := captureLog(t, func() {
		got = b.envDuration("MY_TEST_INTERVAL_S", 300*time.Second, 5*time.Second)
	})

	if got != 5*time.Second {
		t.Errorf("envDuration(configured -10) = %s, want the 5s floor, not negative", got)
	}
	if !strings.Contains(out, "MY_TEST_INTERVAL_S=-10s") || !strings.Contains(out, "5s") {
		t.Errorf("envDuration logged %q, want it to name the below-floor configured "+
			"value AND the floor it was raised to", out)
	}
}

// TestEnvDurationResolvesThroughConfigSnapshotNotAmbientEnv is the same class
// of fix task #23 closed for TRON_API_KEY and auditprune_test.go's
// TestPruneAuditOnceResolvesRetentionFromDotEnvSnapshot closed for
// AUDIT_RETENTION_DAYS, applied to envDuration: config.Load merges .env into
// a frozen snapshot that is NEVER copied back into the real process
// environment, so a raw os.Getenv read would be blind to a value supplied
// only via .env (production's systemd unit exports nothing else). The
// fixture asserts its own premise first — the key really is absent from the
// ambient environment — so this test cannot pass "by accident" if the
// ambient shell happens to already export it.
//
// MY_TEST_INTERVAL_S=45 via .env, well above the 1s floor so the clamp never
// masks the read. Under correct (b.envInt / b.cfg) resolution the result is
// 45s; under a raw-os.Getenv mutation the ambient-blind read falls back to
// the 300s default instead.
//
// Falsification: replacing b.envInt(key, ...) inside envDuration with a raw
// os.Getenv-based read makes this resolve to the 300s default instead of the
// .env-configured 45s, and the assertion fails.
func TestEnvDurationResolvesThroughConfigSnapshotNotAmbientEnv(t *testing.T) {
	if _, ok := os.LookupEnv("MY_TEST_INTERVAL_S"); ok {
		t.Fatal("fixture broken: MY_TEST_INTERVAL_S is set in the ambient " +
			"environment, so this test would pass even if envDuration read raw env")
	}

	b := envDurationTestBot(t, "MY_TEST_INTERVAL_S=45\n")

	got := b.envDuration("MY_TEST_INTERVAL_S", 300*time.Second, 1*time.Second)
	if got != 45*time.Second {
		t.Errorf("envDuration = %s, want 45s (the .env-configured value) — resolving "+
			"to the 300s default means the read fell back to the ambient-blind path", got)
	}
}

// TestNewClampsStockSweepIntervalConfiguredZero is the wiring pin: proves
// New() actually routes STOCK_SWEEP_INTERVAL_S through envDuration (not a
// bare envInt*time.Second) for the sharpest-edge key named in task #30 — a
// configured 0 must come out of New() as stockSweepMinInterval, not 0, so
// StartStockSweep's ticker (stocksweep.go) never fires immediately against
// the buy-path per-product mutex.
//
// Falsification: reverting dispatch.go's New() to
// `time.Duration(b.envInt("STOCK_SWEEP_INTERVAL_S", 300)) * time.Second`
// makes b.stockSweepInterval come out 0 instead of the floor, and the
// assertion fails.
func TestNewClampsStockSweepIntervalConfiguredZero(t *testing.T) {
	b := envDurationTestBot(t, "STOCK_SWEEP_INTERVAL_S=0\n")
	if b.stockSweepInterval != stockSweepMinInterval {
		t.Errorf("b.stockSweepInterval = %s, want the %s floor — a configured 0 "+
			"reached the ticker unclamped", b.stockSweepInterval, stockSweepMinInterval)
	}
}
