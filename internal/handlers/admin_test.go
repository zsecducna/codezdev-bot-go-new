package handlers

// admin_test.go — regression tests for the bug this slice fixes: an admin
// tapping 🔑 Admin got NOTHING back, because step 9 of Process iterated an empty
// adminText slice and labelMap never mapped the Admin label. The first test here
// fails against the pre-fix tree.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/store"
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

// TestFilterAdminRowsParity is H1's own audit item: "Verify the Go menu
// [strips buttons a standard admin cannot use] the same [as Python's
// _filter_admin_rows], or report the gap." This proves admincap.go's
// filterAdminRows, wired into showAdminPanel and adminCategoryMenu, actually
// hides disallowed buttons rather than merely denying the tap after the
// fact — a standard admin should never even SEE a button their capability
// set doesn't unlock.
func TestFilterAdminRowsParity(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil) // super admin is chat 1
	b.adminsMu.Lock()
	b.admins["7"] = true // chat 7: standard admin, holds only defaultStdCaps
	b.adminsMu.Unlock()

	t.Run("root panel", func(t *testing.T) {
		b.Process(ctx, callbackUpdate(7, "admroot"))
		data := buttonData(lastSendMsg(f).ReplyMarkup)
		for _, want := range []string{"admcat:stock", "admcat:settings", "adm:audit"} {
			if !containsStr(data, want) {
				t.Errorf("standard admin's root panel missing allowed button %q; got %v", want, data)
			}
		}
		for _, deny := range []string{
			"admcat:orders", "admcat:customers", "admcat:pricing", "admcat:entra",
			"adm:pool", "adm:revenue", "admcat:roles",
		} {
			if containsStr(data, deny) {
				t.Errorf("standard admin's root panel shows disallowed button %q; got %v", deny, data)
			}
		}
	})

	t.Run("settings category", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, callbackUpdate(7, "admcat:settings"))
		data := buttonData(lastSendMsg(f).ReplyMarkup)
		for _, want := range []string{"adm:togglesales", "adm:toggleprivate", "admcat:whitelist"} {
			if !containsStr(data, want) {
				t.Errorf("standard admin's settings menu missing allowed button %q; got %v", want, data)
			}
		}
		for _, deny := range []string{
			"adm:togglebcast", "adm:setrate", "adm:setkirourl", "admcat:payments",
			"admcat:resellers", "admcat:stdperms", "adm:setmaxqty",
		} {
			if containsStr(data, deny) {
				t.Errorf("standard admin's settings menu shows disallowed button %q; got %v", deny, data)
			}
		}
	})

	t.Run("stock category", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, callbackUpdate(7, "admcat:stock"))
		data := buttonData(lastSendMsg(f).ReplyMarkup)
		for _, want := range []string{"adm:stock", "adm:holdstock"} {
			if !containsStr(data, want) {
				t.Errorf("standard admin's stock menu missing allowed button %q; got %v", want, data)
			}
		}
		for _, deny := range []string{"adm:upstock", "adm:upstockstore", "adm:mngprod", "adm:stockbroadcast", "adm:provpool"} {
			if containsStr(data, deny) {
				t.Errorf("standard admin's stock menu shows disallowed button %q; got %v", deny, data)
			}
		}
	})

	t.Run("full admin (super) is never filtered", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, callbackUpdate(1, "admroot"))
		data := buttonData(lastSendMsg(f).ReplyMarkup)
		for _, want := range []string{"admcat:orders", "admcat:customers", "adm:revenue", "admcat:roles"} {
			if !containsStr(data, want) {
				t.Errorf("super admin's root panel missing %q; got %v", want, data)
			}
		}
	})
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
		// adm:stockbroadcast (adminstock.go, py_stock_spec.md §5) is a "no
		// input, no confirmation" action whose only reply comes from a
		// background goroutine (workers.Go) — matching the same pattern
		// execBroadcast/refund-delete sweeps already use
		// elsewhere in this admin surface. Poll briefly so a scheduling race
		// doesn't misreport a genuinely-answered button as dead; a TRULY dead
		// button still fails after the deadline.
		deadline := time.Now().Add(300 * time.Millisecond)
		var last *tg.SendMessageParams
		for {
			last = lastSendMsg(f)
			if last != nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if last == nil {
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

// TestSlashCommandCapabilityGate is the regression test for the Opus-review
// H1 finding on admincap.go: `/orders 50` used to bypass the capability gate
// entirely (step 9 only checked isAdmin), leaking the full orders_view
// report — customer chat ids, product, tier, total, des — to a standard
// admin whose hand-crafted `adm:orders`/`admcat:orders` callbacks were
// already correctly denied. capAdminCmd (admin.go) now consults stdCmdCap
// the same way the callback path consults stdAdminCbCap.
func TestSlashCommandCapabilityGate(t *testing.T) {
	ctx := context.Background()
	b, f := newTestBot("1", nil) // super admin is chat 1
	b.adminsMu.Lock()
	b.admins["7"] = true // chat 7: standard admin, default caps only (no orders_view)
	b.adminsMu.Unlock()

	t.Run("standard admin /orders is denied, not leaked", func(t *testing.T) {
		b.Process(ctx, textUpdate(7, "/orders 50"))
		last := lastSendMsg(f)
		if last == nil || last.Text != settingsPermissionDenied {
			t.Fatalf("/orders for a standard admin = %+v, want a plain %q message", last, settingsPermissionDenied)
		}
		if strings.Contains(last.Text, "chat=") {
			t.Fatal("orders_view data leaked to a standard admin via the slash-command path")
		}
	})

	t.Run("standard admin /stock (stock_read, a default cap) still works", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, textUpdate(7, "/stock"))
		last := lastSendMsg(f)
		if last == nil || last.Text == settingsPermissionDenied {
			t.Fatalf("/stock for a standard admin was denied: %+v, want it to pass (stock_read is a default cap)", last)
		}
	})

	t.Run("standard admin /admin (nav, cap \"\") still works", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, textUpdate(7, "/admin"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "ADMIN DASHBOARD") {
			t.Fatalf("/admin for a standard admin = %+v, want the panel to open", last)
		}
	})

	t.Run("full admin /orders still works", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, textUpdate(1, "/orders 50"))
		last := lastSendMsg(f)
		if last == nil || last.Text == settingsPermissionDenied {
			t.Fatalf("/orders for the super admin was denied: %+v", last)
		}
	})
}

// newDashboardTestBot wires a Bot to a real Store rooted at a fresh temp dir
// — the A4 dashboard split (below) reads GetRevenueSummary, which needs real
// SQL aggregation, not the nil-store no-op path every other admin_test.go
// case uses.
func newDashboardTestBot(t *testing.T, adminID string) (*Bot, *fakeTG, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, AdminID: adminID})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	return b, f, st
}

// seedPaid creates an order and immediately claims it to 'paid' — the A4
// "money is in, goods are not out" state. seedReserved leaves it at the
// CreateOrder default. Both use distinct `des` per call so multiple rows
// coexist.
func seedPaid(t *testing.T, st *store.Store, des, product string) {
	t.Helper()
	if err := st.CreateOrder(des, "9", "en", product, "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(%s): %v", des, err)
	}
	if _, err := st.ClaimOrder(des, "reserved", "paid"); err != nil {
		t.Fatalf("ClaimOrder(%s -> paid): %v", des, err)
	}
}

func seedReserved(t *testing.T, st *store.Store, des, product string) {
	t.Helper()
	if err := st.CreateOrder(des, "9", "en", product, "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(%s): %v", des, err)
	}
}

// TestAdminDashboardPaidUndeliveredAlarm is the HANDLER half of A4
// (stats-fixes-spec.md §5.3). The store half (Store.PaidUndelivered,
// Store.GetRevenueSummary's split fields) is proven separately in
// internal/store/stats_test.go; this test proves the DASHBOARD RENDER: the
// old combined "⏳ Pending: N" line (reserved+paid, deposits included) is
// gone, 'reserved' renders as ordinary "Awaiting payment", and 'paid' gets
// its own alarming, deposit-excluding, zero-suppressed line.
//
// Falsification: reverting adminDashboardText's ordersBlock to the
// pre-fix single "⏳ Pending: "+itoa64(r.PendingCount) line makes every
// sub-test below fail — verified by mutation (see the executor report).
func TestAdminDashboardPaidUndeliveredAlarm(t *testing.T) {
	ctx := context.Background()

	t.Run("zero paid-undelivered suppresses the alarm entirely", func(t *testing.T) {
		b, f, st := newDashboardTestBot(t, "1")
		seedReserved(t, st, "res1", "kiro-pro")
		b.Process(ctx, callbackUpdate(1, "admroot"))
		last := lastSendMsg(f)
		if last == nil {
			t.Fatal("admroot produced no reply")
		}
		if strings.Contains(last.Text, "PAID, NOT DELIVERED") {
			t.Errorf("alarm line present at PaidUndeliveredCount==0:\n%s", last.Text)
		}
		if !strings.Contains(last.Text, "⏳ Awaiting payment: 1") {
			t.Errorf("expected the reserved order under Awaiting payment, got:\n%s", last.Text)
		}
		if strings.Contains(last.Text, "⏳ Pending:") {
			t.Errorf("the old combined Pending line must be gone, got:\n%s", last.Text)
		}
	})

	t.Run("paid product orders trigger the alarm, deposits are excluded", func(t *testing.T) {
		b, f, st := newDashboardTestBot(t, "1")
		// The deposit trap: a paid __deposit__ row must not inflate the alarm —
		// A1's exact bug (a missing product!=? filter) reappearing inside A4.
		seedPaid(t, st, "dep1", depositProduct)
		seedPaid(t, st, "prod1", "kiro-pro")
		seedReserved(t, st, "res1", "kiro-pro")

		b.Process(ctx, callbackUpdate(1, "admroot"))
		last := lastSendMsg(f)
		if last == nil {
			t.Fatal("admroot produced no reply")
		}
		if !strings.Contains(last.Text, "🚨 PAID, NOT DELIVERED: 1") {
			t.Errorf("want exactly 1 paid-undelivered (deposit excluded), got:\n%s", last.Text)
		}
		if !strings.Contains(last.Text, "(oldest ") {
			t.Errorf("alarm line missing the oldest-age annotation, got:\n%s", last.Text)
		}
		if !strings.Contains(last.Text, "⏳ Awaiting payment: 1") {
			t.Errorf("want 1 awaiting payment (the reserved order only), got:\n%s", last.Text)
		}
	})
}

// TestAdminDashboardPaidUndeliveredNullTSOmitsAge is the edge-case hardening
// found in review: orders.ts is a nullable column (store.go's schema has no
// NOT NULL on it), so a 'paid' non-deposit row can in principle carry a NULL
// ts. SQL's MIN() over a non-empty set whose ts values are ALL NULL still
// returns NULL, which Store.PaidUndelivered surfaces as
// PaidUndeliveredOldestTS==0 — the SAME zero value used for "no such order".
// Without a guard, ageString(0) computes an age off the Unix epoch and prints
// something like "(oldest 20000+d ago)" on a genuine, present-day alarm —
// false precision that undermines the whole point of showing an age at all.
// The count alone must still fire the alarm; only the age clause is dropped.
func TestAdminDashboardPaidUndeliveredNullTSOmitsAge(t *testing.T) {
	ctx := context.Background()
	b, f, st := newDashboardTestBot(t, "1")
	// CreateOrder always stamps ts=now(); reach past the public API with a raw
	// insert to construct the one state Store.PaidUndelivered's own doc
	// comment calls out as a real possibility: a paid, non-deposit row whose
	// ts is NULL.
	if _, err := st.DB().Exec(
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, total, status, ts) "+
			"VALUES ('nullts','9','en','kiro-pro','T1',1,5000,'paid',NULL)"); err != nil {
		t.Fatalf("seed NULL-ts paid order: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "admroot"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("admroot produced no reply")
	}
	if !strings.Contains(last.Text, "🚨 PAID, NOT DELIVERED: 1") {
		t.Errorf("alarm must still fire on the count alone, got:\n%s", last.Text)
	}
	if strings.Contains(last.Text, "(oldest") {
		t.Errorf("age clause must be omitted when the oldest ts is unknown, got:\n%s", last.Text)
	}
}

// wantAdminCapDenied asserts that a standard (granted, non-super) admin was
// denied `data` by H1's central capability gate (admincap.go, wired into
// command.go's handleCallback BEFORE the unconditional ack) rather than by
// any downstream per-handler chat-message refusal (fullAdminOnly,
// settingsSuperAdminOnly). The gate fires pre-ack, so its alert IS that
// update's one and only callback answer — never a chat message, and never a
// SECOND answerCallbackQuery (which is exactly the shape
// adminsettings.go's settingsSuperAdminOnly doc comment explains real
// Telegram would silently drop — that gate runs deep in dispatch, AFTER the
// ack; this one runs before it, so there is no "second" to drop). f.Sends
// must be untouched by this callback specifically — callers that already
// primed f.Sends with an earlier arm step should reset it (f.Sends = nil)
// right before the call under test.
func wantAdminCapDenied(t *testing.T, f *fakeTG, data string) {
	t.Helper()
	if last := lastSendMsg(f); last != nil {
		t.Errorf("%s: standard admin should get no chat message (denied pre-ack), got %+v", data, last)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// H2 fix (Opus review): assert EXACTLY one answer, not merely "at least
	// one, check the last". Telegram allows exactly one answerCallbackQuery
	// per callback_query_id; if the gate ever moved below command.go's
	// unconditional pre-dispatch ack, f.Answers would become [blank, alert]
	// — the last entry still matches this alert, so the old `len == 0` check
	// passed while a real Telegram server would silently drop the SECOND
	// answer and the denied admin would see nothing at all. buy_test.go's
	// rate-limit gate assertion already uses this stricter form.
	if len(f.Answers) != 1 {
		t.Errorf("%s: want exactly 1 answer (pre-ack denial), got %d", data, len(f.Answers))
		return
	}
	last := f.Answers[len(f.Answers)-1]
	if !last.ShowAlert || last.Text != settingsPermissionDenied {
		t.Errorf("%s: denial alert = %+v, want ShowAlert=true Text=%q", data, last, settingsPermissionDenied)
	}
}
