package handlers

import (
	"context"
	"strings"
	"testing"

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
	// capture ahead of it (deposit #44 < purchase #48); M2.5 splices the two
	// API-key captures between them (#45/#46); the USDT slice puts pending_txid
	// (#43) ahead of pending_deposit, so a pasted 64-hex tx hash is never misread
	// as a deposit amount (telegram_bot.py:18976); the M2.6 login-code slice puts
	// pending_code (#36) FIRST — it is the highest-priority customer capture in the
	// Python priority comment @3632 (`code > sell > deposit > apikey > …`).
	// Together they match the Python priority comment @3632, including the
	// M2.6 promo/referral ordercode capture (#47) between apikeyrc and purchase.
	// `code > … > txid > deposit > apikey > apikeyrc > ordercode > purchase`.
	want := []string{"code", "txid", "deposit", "apikey", "apikeyrc", "ordercode", "purchase"}
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
	// The canonical list itself must stay unique + the pinned length (48).
	if len(CanonicalCaptureOrder) != 48 {
		t.Errorf("canonical order length = %d, want 48", len(CanonicalCaptureOrder))
	}
	seen := map[string]bool{}
	for _, n := range CanonicalCaptureOrder {
		if seen[n] {
			t.Errorf("duplicate capture name %q in canonical order", n)
		}
		seen[n] = true
	}
	// The other two ordered stages are also empty in M2.1.
	if len(b.fileInterceptNames()) != 0 || len(b.adminTextNames()) != 0 {
		t.Errorf("file/admin stages should be empty in M2.1")
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
		{c.SellBtn.EN, "sellaws"},
		{c.SellBtn.ZH, "sellaws"},
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
