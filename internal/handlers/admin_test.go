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
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, total, status, ts) " +
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

// TestAdminDashboardRefundBucketsRender is the HANDLER half of A2
// (stats-fixes-spec.md §5.2), closing a coverage gap a review round found:
// no test previously read the new ORDERS block at all — reverting the
// refund-rate denominator to the pre-fix bare DeliveredCount (a >100% bug),
// reverting deposit.go's kind selection to always late_settled, or renaming
// every new dashboard label to garbage each passed the full suite silently.
//
// Seeds ONE order of each new bucket (customer refund, mint_failed,
// out_of_stock, an unclassified NULL-kind refund, and an expired checkout)
// plus one delivered order, then asserts the exact rendered line for each —
// including the refund-rate PERCENTAGE, which is the number a mutated
// denominator changes.
//
// Falsification (verified by mutation, then reverted): reverting the
// denominator from `DeliveredCount+RefundedCount` back to bare
// `DeliveredCount` changes the seeded 1-delivered/1-refunded scenario's rate
// from 1/2=50.0% to 1/1=100.0%, so the "(50.0%)" assertion below fails.
func TestAdminDashboardRefundBucketsRender(t *testing.T) {
	ctx := context.Background()
	b, f, st := newDashboardTestBot(t, "1")

	// One delivered (non-deposit) sale — DeliveredCount=1, and the refund-rate
	// denominator (DeliveredCount+RefundedCount).
	seedPaid(t, st, "delivered1", "kiro-pro")
	if _, err := st.ClaimOrder("delivered1", "paid", "delivered"); err != nil {
		t.Fatalf("claim delivered1 -> delivered: %v", err)
	}

	// One genuine customer refund of a completed sale — RefundedCount=1.
	seedPaid(t, st, "refcust1", "kiro-pro")
	if _, err := st.ClaimOrder("refcust1", "paid", "delivered"); err != nil {
		t.Fatalf("claim refcust1 -> delivered: %v", err)
	}
	if _, err := st.ClaimOrderKind("refcust1", "delivered", "refunded", store.RefundKindCustomer); err != nil {
		t.Fatalf("claim refcust1 -> refunded/customer: %v", err)
	}

	// One of our own mint failures — MintFailedCount=1.
	seedPaid(t, st, "mint1", "kiro-pro")
	if _, err := st.ClaimOrderKind("mint1", "paid", "refunded", store.RefundKindMintFailed); err != nil {
		t.Fatalf("claim mint1 -> refunded/mint_failed: %v", err)
	}

	// One stockout refund — OutOfStockCount=1.
	seedPaid(t, st, "oos1", "kiro-pro")
	if _, err := st.ClaimOrderKind("oos1", "paid", "refunded", store.RefundKindOutOfStock); err != nil {
		t.Fatalf("claim oos1 -> refunded/out_of_stock: %v", err)
	}

	// One refund with NO kind (the plain wrapper's shape) — UnclassifiedCount=1.
	seedPaid(t, st, "unclass1", "kiro-pro")
	if _, err := st.ClaimOrder("unclass1", "paid", "refunded"); err != nil {
		t.Fatalf("claim unclass1 -> refunded (no kind): %v", err)
	}

	// One abandoned checkout — ExpiredCount=1.
	seedReserved(t, st, "exp1", "kiro-pro")
	if _, err := st.ClaimOrder("exp1", "reserved", "expired"); err != nil {
		t.Fatalf("claim exp1 -> expired: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "admroot"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("admroot produced no reply")
	}

	// Gross = DeliveredCount(1) + RefundedCount(1) = 2; rate = 1/2 = 50.0%.
	// This is the assertion a reverted (bare-DeliveredCount) denominator
	// breaks: it would read "(100.0%)" instead.
	if !strings.Contains(last.Text, "↩️ Refunded: 1 (50.0%)") {
		t.Errorf("want the real (non-inflated) refund rate, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "⌛ Expired checkouts: 1") {
		t.Errorf("want 1 expired checkout, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "📭 Paid, no stock: 1") {
		t.Errorf("want 1 out-of-stock refund, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "🛠 Failed mints: 1") {
		t.Errorf("want 1 mint-failed refund, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "❔ Unclassified: 1") {
		t.Errorf("want 1 unclassified refund, got:\n%s", last.Text)
	}
}

// TestAdminDashboardGrossRefundedNetRender is the HANDLER half of A7
// (stats-fixes-spec.md §5.5) AND task #20 stage S3 (misc.go's
// GetRevenueSummary doc comment on the refund-split query). The old
// dashboard printed a single "Total" line that already excluded refunds, so
// a refund on an order from an already-closed period silently shrank that
// report with nothing on screen explaining why — A7 replaced it with
// Gross/Refunded/Net. Stage S3 then fixed WHAT those lines measure: this
// test seeds a PARTIAL refund (1250 credited back out of a 2000 total,
// recorded via SetRefundedAmount — the stage-S2 write path) specifically so
// a regression back to the pre-S3 behaviour (Refunded = the 2000 order
// total, Net = a bare relabelled DeliveredRevenue that ignores the refund
// entirely) is caught: Refunded must read the CREDITED 1250, not the 2000
// total, and Net must read Gross minus that credited amount (5750/0.23 USDT),
// not the old 5000/0.20 USDT DeliveredRevenue figure. No legacy (unrecorded)
// refund exists in this scenario, so Net renders as a single point — the
// range-collapse case is pinned separately by
// TestAdminDashboardRefundedRangeForLegacyUnknownRefunds.
//
// Falsification: reverting the REVENUE block back to the single
// "Total: 0.20 USDT (1 orders)" line, OR reverting Refunded/Net to read
// RefundedRevenue/DeliveredRevenue (the pre-S3 order-total-based figures)
// instead of the RefundedAmountKnown split, makes every assertion below
// fail — verified by mutation (see the executor report).
func TestAdminDashboardGrossRefundedNetRender(t *testing.T) {
	ctx := context.Background()
	b, f, st := newDashboardTestBot(t, "1")

	if err := st.CreateOrder("delivered1", "9", "en", "kiro-pro", "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(delivered1): %v", err)
	}
	if _, err := st.ClaimOrder("delivered1", "reserved", "delivered"); err != nil {
		t.Fatalf("claim delivered1 -> delivered: %v", err)
	}

	if err := st.CreateOrder("refcust1", "9", "en", "kiro-pro", "T1", 1, 2000); err != nil {
		t.Fatalf("CreateOrder(refcust1): %v", err)
	}
	if _, err := st.ClaimOrder("refcust1", "reserved", "delivered"); err != nil {
		t.Fatalf("claim refcust1 -> delivered: %v", err)
	}
	if _, err := st.ClaimOrderKind("refcust1", "delivered", "refunded", store.RefundKindCustomer); err != nil {
		t.Fatalf("claim refcust1 -> refunded/customer: %v", err)
	}
	// The stage-S2 write: only 1250 of the 2000 total was actually credited
	// back (a pro-rated warranty refund), recorded strictly after the status
	// flip above, per SetRefundedAmount's own doc comment (orders.go).
	if err := st.SetRefundedAmount("refcust1", 1250); err != nil {
		t.Fatalf("SetRefundedAmount(refcust1, 1250): %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "admroot"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("admroot produced no reply")
	}
	if !strings.Contains(last.Text, "Gross: 0.28 USDT  (2 orders)") {
		t.Errorf("want Gross = Σ original charges = 0.28 USDT over 2 orders "+
			"(unaffected by the partial refund), got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "Refunded: -0.05 USDT exact  (1 orders)") {
		t.Errorf("want Refunded = the CREDITED 1250 (0.05 USDT), not refcust1's "+
			"2000 total, got:\n%s", last.Text)
	}
	if strings.Contains(last.Text, "unrecorded") {
		t.Errorf("no legacy/unknown refund exists in this scenario, so the "+
			"'unrecorded' continuation line must not render, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "Net: 0.23 USDT  (1 orders)") {
		t.Errorf("want Net = Gross(0.28) - Refunded(0.05) = 0.23 USDT as a single "+
			"point (no legacy unknown to widen the range), got:\n%s", last.Text)
	}
	if strings.Contains(last.Text, "Total: ") {
		t.Errorf("the old single Total line must be gone, replaced by Gross/Refunded/Net, got:\n%s", last.Text)
	}
}

// TestAdminDashboardRefundedRangeForLegacyUnknownRefunds is the HANDLER half
// of task #20 stage S3's core new behaviour: a refund with no recorded
// refunded_amount (the shape every refund had before stage S2 started
// writing the column — ClaimOrderKind alone, no SetRefundedAmount call)
// must render as a SEPARATE "+ up to X unrecorded" line and widen Net into a
// RANGE, never get silently folded into the exact known figure.
//
// This is the regression pin for the trap misc.go's GetRevenueSummary doc
// comment calls out by name: SQLite's SUM() silently skips NULL rows, so a
// naive SUM(refunded_amount) (or, equivalently at the render layer, a Net
// computed as a single `Gross - RefundedAmountKnown` point instead of a
// range) would treat every legacy unknown refund as if it credited back
// $0 — UNDERSTATING refunds and OVERSTATING Net, the mirror image of the A7
// bug this whole column exists to fix. A "fix" that reverts Net to a single
// number cannot pass this test: the exact range string asserted below only
// exists if RefundedUnknownUpperBound is actually subtracted at the low end.
//
// Seeds ONE known refund (1250 credited of a 2000 total, same as the sibling
// test above) alongside ONE legacy/unknown refund (3000 total, refunded_amount
// left NULL) so known and unknown are visibly distinct populations on the
// same screen.
//
// Falsification: reverting Net to `b.usdtDisp(grossRevenue - r.RefundedAmountKnown)`
// with no range logic renders "Net: 0.35 USDT  (1 orders)" instead of the
// "0.23 USDT – 0.35 USDT" range asserted below, and dropping the
// RefundedUnknownCount>0 branch entirely removes the "unrecorded" line —
// both make this test fail (verified by mutation, see the executor report).
func TestAdminDashboardRefundedRangeForLegacyUnknownRefunds(t *testing.T) {
	ctx := context.Background()
	b, f, st := newDashboardTestBot(t, "1")

	if err := st.CreateOrder("delivered1", "9", "en", "kiro-pro", "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(delivered1): %v", err)
	}
	if _, err := st.ClaimOrder("delivered1", "reserved", "delivered"); err != nil {
		t.Fatalf("claim delivered1 -> delivered: %v", err)
	}

	// Known: recorded via SetRefundedAmount, the stage-S2 write path.
	if err := st.CreateOrder("refcust1", "9", "en", "kiro-pro", "T1", 1, 2000); err != nil {
		t.Fatalf("CreateOrder(refcust1): %v", err)
	}
	if _, err := st.ClaimOrder("refcust1", "reserved", "delivered"); err != nil {
		t.Fatalf("claim refcust1 -> delivered: %v", err)
	}
	if _, err := st.ClaimOrderKind("refcust1", "delivered", "refunded", store.RefundKindCustomer); err != nil {
		t.Fatalf("claim refcust1 -> refunded/customer: %v", err)
	}
	if err := st.SetRefundedAmount("refcust1", 1250); err != nil {
		t.Fatalf("SetRefundedAmount(refcust1, 1250): %v", err)
	}

	// Unknown: the pre-S2 shape — status flip only, refunded_amount left NULL.
	if err := st.CreateOrder("reflegacy1", "9", "en", "kiro-pro", "T1", 1, 3000); err != nil {
		t.Fatalf("CreateOrder(reflegacy1): %v", err)
	}
	if _, err := st.ClaimOrder("reflegacy1", "reserved", "paid"); err != nil {
		t.Fatalf("claim reflegacy1 -> paid: %v", err)
	}
	if _, err := st.ClaimOrderKind("reflegacy1", "paid", "refunded", store.RefundKindAdminCancel); err != nil {
		t.Fatalf("claim reflegacy1 -> refunded/admin_cancel: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "admroot"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("admroot produced no reply")
	}
	if !strings.Contains(last.Text, "Gross: 0.40 USDT  (3 orders)") {
		t.Errorf("want Gross = Σ original charges = 0.40 USDT over 3 orders, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "Refunded: -0.05 USDT exact  (1 orders)") {
		t.Errorf("want the exact known refund on its own line, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "+ up to 0.12 USDT unrecorded  (1 legacy orders — actual refund not recorded)") {
		t.Errorf("want the legacy refund's total (0.12 USDT) shown as a "+
			"separate bounded ceiling, not folded into the exact figure, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "Net: 0.23 USDT – 0.35 USDT") {
		t.Errorf("want Net as a RANGE — high end assumes the legacy refund "+
			"credited $0 (0.35), low end assumes it credited its full 3000 "+
			"total (0.23) — got:\n%s", last.Text)
	}
}

// TestAdminDashboardZeroRefundsOmitsNegativeSign guards the zero-refund
// formatting edge the A7 render must not get wrong: "-0.00 USDT" on a shop
// with no refunds at all reads as a bug, not a healthy zero.
//
// Review-round update: the negative-zero fix moved from a LOCAL sign-strip
// on just this Refunded line (admin.go) to the SOURCE, money.go's usdtDisp
// itself — reverting either the money.go `cents==0` guard OR the surviving
// `RefundedRevenue>0` sign-add in admin.go would surface this. The Wallet
// balances line is now asserted too: it shares the SAME usdtDisp call, so it
// is no longer a separate out-of-scope bug — fixing the source fixed both,
// which is the whole reason the fix was moved there instead of staying a
// one-line-only workaround.
//
// Falsification: reverting money.go's `if cents == 0 { cents = 0 }` guard
// makes BOTH assertions below fail (verified by mutation, see the executor
// report — TestUsdtDispZeroHasNoNegativeSign in money_test.go pins the same
// fact at the unit level). Separately, unconditionally prefixing "-" to the
// Refunded line in admin.go (instead of only when RefundedRevenue>0) also
// makes the Refunded assertion fail on its own.
func TestAdminDashboardZeroRefundsOmitsNegativeSign(t *testing.T) {
	ctx := context.Background()
	b, f, st := newDashboardTestBot(t, "1")
	if err := st.CreateOrder("delivered1", "9", "en", "kiro-pro", "T1", 1, 5000); err != nil {
		t.Fatalf("CreateOrder(delivered1): %v", err)
	}
	if _, err := st.ClaimOrder("delivered1", "reserved", "delivered"); err != nil {
		t.Fatalf("claim delivered1 -> delivered: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "admroot"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("admroot produced no reply")
	}
	if strings.Contains(last.Text, "Refunded: -0.00 USDT") {
		t.Errorf("a healthy zero-refund shop must not render a negative sign on the "+
			"Refunded line, got:\n%s", last.Text)
	}
	if strings.Contains(last.Text, "-0.00 USDT") {
		t.Errorf("the source-level usdtDisp(0) fix must clear the negative sign "+
			"EVERYWHERE it renders, not just on the Refunded line, got:\n%s", last.Text)
	}
	if !strings.Contains(last.Text, "Refunded: 0.00 USDT exact  (0 orders)") {
		t.Errorf("want an unsigned zero Refunded line, got:\n%s", last.Text)
	}
}

// TestAdminDashboardAndStockShowDistinctHeldReservedIcons is the HANDLER half
// of A5 (stats-fixes-spec.md §5.4, audit A5). Before this fix the dashboard's
// stock block printed 🔒 for CountHeld (admin-quarantined, off-sale) while
// /stock printed the SAME 🔒 for CountReserved (a live in-flight checkout) —
// two different folders, same icon, on two different screens, and neither
// screen showed all three states (dashboard omitted Reserved, /stock omitted
// Held). Seeds one product with both a held account and a live reserved
// checkout and asserts both surfaces now show ⏳ reserved / 🔒 held distinctly
// and completely.
//
// Falsification: reverting adminStockText's "⏳ Reserved:"/"🔒 Held:" pair
// back to the pre-fix single "🔒 Reserved:" line (and dropping Held) makes
// the /stock assertions below fail; reverting the dashboard loop to omit the
// CountReserved line makes the dashboard assertion fail — verified by
// mutation (see the executor report).
func TestAdminDashboardAndStockShowDistinctHeldReservedIcons(t *testing.T) {
	ctx := context.Background()
	b, f, _, stk, dir := newBuyBot(t, "1")
	seedProduct(t, dir, "kiro-pro", 5000, 5)

	if n, err := stk.Hold("kiro-pro", 2); err != nil || n != 2 {
		t.Fatalf("Hold(kiro-pro, 2): n=%d err=%v", n, err)
	}
	if usernames, err := stk.Reserve("kiro-pro", 1, "ord1", time.Hour); err != nil || len(usernames) != 1 {
		t.Fatalf("Reserve(kiro-pro, 1): usernames=%v err=%v", usernames, err)
	}

	b.Process(ctx, textUpdate(1, "/admin"))
	dash := lastSendMsg(f)
	if dash == nil {
		t.Fatal("/admin produced no reply")
	}
	if !strings.Contains(dash.Text, "⏳ 1 reserved") {
		t.Errorf("dashboard missing the reserved count (previously omitted entirely), got:\n%s", dash.Text)
	}
	if !strings.Contains(dash.Text, "🔒 2 held") {
		t.Errorf("dashboard missing the held count, got:\n%s", dash.Text)
	}

	f.Sends = nil
	b.Process(ctx, textUpdate(1, "/stock"))
	stockMsg := lastSendMsg(f)
	if stockMsg == nil {
		t.Fatal("/stock produced no reply")
	}
	if !strings.Contains(stockMsg.Text, "⏳ Reserved: 1") {
		t.Errorf("/stock reserved count must use ⏳, not the held icon 🔒, got:\n%s", stockMsg.Text)
	}
	if !strings.Contains(stockMsg.Text, "🔒 Held: 2") {
		t.Errorf("/stock missing the Held field (previously omitted entirely), got:\n%s", stockMsg.Text)
	}
	if strings.Contains(stockMsg.Text, "🔒 Reserved:") {
		t.Errorf("/stock still labels Reserved with the held icon 🔒, got:\n%s", stockMsg.Text)
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
