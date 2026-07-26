package handlers

// admin_test.go — regression tests for the bug this slice fixes: an admin
// tapping 🔑 Admin got NOTHING back, because step 9 of Process iterated an empty
// adminText slice and labelMap never mapped the Admin label. The first test here
// fails against the pre-fix tree.

import (
	"context"
	"strings"
	"testing"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// lastSendMsg returns the most recent recorded sendMessage, or nil.
func lastSendMsg(f *fakeTG) *tg.SendMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sends) == 0 {
		return nil
	}
	return &f.Sends[len(f.Sends)-1]
}

// buttonData flattens an inline keyboard's callback_data values, or nil when the
// markup is not an inline keyboard.
func buttonData(markup any) []string {
	kb, ok := markup.(*tg.InlineKeyboardMarkup)
	if !ok {
		return nil
	}
	var out []string
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			out = append(out, btn.CallbackData)
		}
	}
	return out
}

// TestAdminButtonResponds is the regression test for the reported bug. Tapping
// the Admin reply button must open the panel; a non-admin tapping the same label
// (it is never drawn for them, but the text is trivially typed) gets no panel.
func TestAdminButtonResponds(t *testing.T) {
	ctx := context.Background()

	t.Run("admin gets the panel", func(t *testing.T) {
		b, f := newTestBot("42", nil)
		// First message pushes the keyboard refresh note (KeyboardVersion), so the
		// panel is the LAST send, not the only one.
		b.Process(ctx, textUpdate(42, i18n.AdminButton))
		last := lastSendMsg(f)
		if last == nil {
			t.Fatal("admin button produced no reply at all (the original bug)")
		}
		if !strings.Contains(last.Text, "ADMIN DASHBOARD") {
			t.Fatalf("last send is not the admin panel: %q", last.Text)
		}
		data := buttonData(last.ReplyMarkup)
		for _, want := range []string{"admcat:stock", "admcat:orders", "adm:revenue"} {
			if !containsStr(data, want) {
				t.Errorf("panel missing %q button; got %v", want, data)
			}
		}
	})

	t.Run("super admin also gets Admin roles", func(t *testing.T) {
		b, f := newTestBot("42", nil)
		b.Process(ctx, textUpdate(42, i18n.AdminButton))
		if !containsStr(buttonData(lastSendMsg(f).ReplyMarkup), "admcat:roles") {
			t.Error("super admin panel should offer admcat:roles")
		}
	})

	t.Run("non-admin gets no panel", func(t *testing.T) {
		b, f := newTestBot("42", nil)
		b.Process(ctx, textUpdate(7, i18n.AdminButton)) // chat 7 is not the admin
		if last := lastSendMsg(f); last != nil && strings.Contains(last.Text, "ADMIN DASHBOARD") {
			t.Fatal("non-admin reached the admin panel")
		}
	})

	t.Run("banned admin label from a banned non-admin is dropped", func(t *testing.T) {
		b, f := newTestBot("42", map[string]bool{"7": true})
		b.Process(ctx, textUpdate(7, i18n.AdminButton))
		if lastSendMsg(f) != nil {
			t.Fatal("banned chat should get no reply at all")
		}
	})
}

// TestAdminSlashCommandsRespond pins that every panel slash command answers.
// Pre-fix all of these were silent no-ops in handleCommand's default branch.
func TestAdminSlashCommandsRespond(t *testing.T) {
	ctx := context.Background()
	for _, cmd := range []string{"/admin", "/stock", "/orders", "/addbal", "/broadcast", "/refund", "/getord", "/pool"} {
		b, f := newTestBot("42", nil)
		b.Process(ctx, textUpdate(42, cmd))
		last := lastSendMsg(f)
		if last == nil {
			t.Errorf("%s produced no reply", cmd)
			continue
		}
		// The keyboard refresh note is not an answer to the command.
		if last.Text == i18n.C().KBRefreshNote.T(i18n.EN) {
			t.Errorf("%s only pushed the keyboard note, no answer", cmd)
		}
	}
}

// TestAdminPanelNavigation walks the panel callbacks: every category button
// opens a submenu, and every submenu offers a way back — an admin must never
// dead-end, which is the same class of bug as the unresponsive button.
func TestAdminPanelNavigation(t *testing.T) {
	ctx := context.Background()
	cats := []string{"stock", "orders", "customers", "pricing", "settings", "payments", "entra", "roles", "stdperms", "resellers", "whitelist"}
	for _, cat := range cats {
		b, f := newTestBot("42", nil)
		b.Process(ctx, callbackUpdate(42, "admcat:"+cat))
		last := lastSendMsg(f)
		if last == nil {
			t.Errorf("admcat:%s rendered nothing", cat)
			continue
		}
		data := buttonData(last.ReplyMarkup)
		if len(data) == 0 {
			t.Errorf("admcat:%s rendered no buttons", cat)
			continue
		}
		hasBack := false
		for _, d := range data {
			if d == "admroot" || strings.HasPrefix(d, "admcat:") {
				hasBack = true
			}
		}
		if !hasBack {
			t.Errorf("admcat:%s has no Back route; buttons=%v", cat, data)
		}
	}
}

// TestAdminCategoryGates: roles/stdperms are super-admin only, and an unknown
// category (a stale button) renders nothing rather than an empty menu.
func TestAdminCategoryGates(t *testing.T) {
	ctx := context.Background()

	// A granted "standard" admin is not a super admin.
	b, f := newTestBot("42", nil)
	b.adminsMu.Lock()
	b.admins["77"] = true
	b.adminsMu.Unlock()
	b.Process(ctx, callbackUpdate(77, "admcat:roles"))
	if last := lastSendMsg(f); last != nil && strings.Contains(last.Text, "ADMIN ROLES") {
		t.Error("standard admin reached the super-admin-only roles menu")
	}

	b2, f2 := newTestBot("42", nil)
	b2.Process(ctx, callbackUpdate(42, "admcat:nosuchcategory"))
	if lastSendMsg(f2) != nil {
		t.Error("unknown category should render nothing")
	}
}

// TestAdminRootBackReturnsPanel: the Back button from any submenu re-renders the
// root panel (Python's admroot branch).
func TestAdminRootBackReturnsPanel(t *testing.T) {
	b, f := newTestBot("42", nil)
	b.Process(context.Background(), callbackUpdate(42, "admroot"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "ADMIN DASHBOARD") {
		t.Fatalf("admroot did not re-render the panel: %+v", last)
	}
}

// TestAdminActionsAlwaysAnswer: every adm:* / panel-adjacent callback the
// rendered menus can emit produces a reply. A rendered button that answers with
// nothing is exactly the failure mode being fixed.
func TestAdminActionsAlwaysAnswer(t *testing.T) {
	ctx := context.Background()
	// Collect every callback_data the root panel and all submenus can emit.
	probe, _ := newTestBot("42", nil)
	seen := map[string]bool{}
	collect := func(rows [][]tg.InlineKeyboardButton) {
		for _, row := range rows {
			for _, btn := range row {
				if btn.CallbackData != "" {
					seen[btn.CallbackData] = true
				}
			}
		}
	}
	for _, cat := range []string{"stock", "orders", "customers", "pricing", "settings", "payments", "entra", "roles", "stdperms", "resellers", "whitelist"} {
		if _, rows, ok := probe.adminCategoryMenu(42, cat); ok {
			collect(rows)
		}
	}
	// Root panel buttons too.
	pb, pf := newTestBot("42", nil)
	pb.Process(ctx, textUpdate(42, i18n.AdminButton))
	for _, d := range buttonData(lastSendMsg(pf).ReplyMarkup) {
		seen[d] = true
	}

	for data := range seen {
		b, f := newTestBot("42", nil)
		b.Process(ctx, callbackUpdate(42, data))
		if lastSendMsg(f) == nil {
			t.Errorf("callback %q produced no reply (dead button)", data)
		}
	}
}

// TestAdminTextStageOrder pins the step-9 registration against the canonical
// Python order, the same contract TestCaptureChainOrder enforces for step 10.
func TestAdminTextStageOrder(t *testing.T) {
	b, _ := newTestBot("42", nil)
	got := b.adminTextNames()
	if len(got) == 0 {
		t.Fatal("no admin text stages registered — the Admin button cannot route")
	}
	if !b.adminTextOrderOK() {
		t.Errorf("admin text stages %v are not an in-order subsequence of %v", got, CanonicalAdminTextOrder)
	}
	seen := map[string]bool{}
	for _, n := range CanonicalAdminTextOrder {
		if seen[n] {
			t.Errorf("duplicate name %q in CanonicalAdminTextOrder", n)
		}
		seen[n] = true
	}
}

// TestAdminButtonClearsPendingInput: tapping Admin abandons a half-typed admin
// prompt (_clear_admin_pending @17798), so a stale flag can't consume the next
// plain text an admin sends.
func TestAdminButtonClearsPendingInput(t *testing.T) {
	b, _ := newTestBot("42", nil)
	st := b.states.Get(42)
	st.mu.Lock()
	st.AddAdmin = true
	st.Refund = true
	st.mu.Unlock()

	b.Process(context.Background(), textUpdate(42, i18n.AdminButton))

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.AddAdmin || st.Refund {
		t.Error("tapping Admin must clear pending admin input")
	}
}

// containsStr is a tiny helper (the tests predate slices.Contains use here).
func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
