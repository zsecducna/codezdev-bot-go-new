package handlers

// notify_test.go — pins for the M3.1 owner/admin alert channel (notify.go):
// recipient-set derivation, the admin_sale_notify/admin_deposit_notify
// toggle gates (#36), the buyer-mention fallback, and the stuck-order
// reconcile alert's de-duplication/escalation/reset policy.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestOwnerRecipientsUnionsDevAndAdmin pins ownerRecipients — DEV_IDS ∪
// {ADMIN_ID} (telegram_bot.py:10849), de-duped when they overlap.
func TestOwnerRecipientsUnionsDevAndAdmin(t *testing.T) {
	b, _ := newTestBot("555", nil)
	b.devIDs = map[string]bool{"111": true, "222": true, "555": true} // 555 overlaps ADMIN_ID
	ids := b.ownerRecipients()
	want := map[int64]bool{111: true, 222: true, 555: true}
	if len(ids) != len(want) {
		t.Fatalf("ownerRecipients = %v, want exactly %v (de-duped)", ids, want)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected owner recipient %d", id)
		}
	}
}

// TestAdminRecipientsExcludesDevIDs pins adminRecipients — {ADMIN_ID} ∪
// admins.json (telegram_bot.py:10827-10829), which deliberately does NOT
// union devIDs in (only _notify_owners does that).
func TestAdminRecipientsExcludesDevIDs(t *testing.T) {
	b, _ := newTestBot("555", nil)
	b.devIDs = map[string]bool{"999": true} // must NOT leak into adminRecipients
	b.adminsMu.Lock()
	b.admins = map[string]bool{"333": true, "444": false} // 444 revoked (false) -> excluded
	b.adminsMu.Unlock()
	ids := b.adminRecipients()
	want := map[int64]bool{555: true, 333: true}
	if len(ids) != len(want) {
		t.Fatalf("adminRecipients = %v, want exactly %v", ids, want)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected admin recipient %d (devIDs or a revoked grant leaked in)", id)
		}
	}
}

// TestNotifyAdminSaleGatedByToggle is a #36 pin: admin_sale_notify defaults
// true (a sale alerts unless explicitly turned off) and OFF means silent
// sales, exactly like Python's `if not load_settings().get("admin_sale_notify",
// True): return` (telegram_bot.py:8087-8088).
//
// Task #38 (U1) fix: the FIRST leg below used to be the only "default"
// coverage, but newBuyBot's config.Load seeds admin_sale_notify=TRUE in
// defaultSettings() (config/settings.go:22-23) — so that leg only ever
// exercised the key PRESENT-AND-true, never genuinely ABSENT. Flipping
// settingBool's default arg at notify.go:185 from true to false left every
// test in the suite green. The third leg now deletes the key outright
// (not merely sets it — b.cfg.Settings() returns a deep copy, so a bare
// delete on it does nothing until SaveSettings writes it back) and asserts
// the alert still fires, which is the only way to observe settingBool's
// actual default argument. Falsification: reverting notify.go's
// `settingBool(b.settings(), "admin_sale_notify", true)` to `..., false)`
// turns this leg red — verified by mutation (see the executor report).
func TestNotifyAdminSaleGatedByToggle(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999")
	const buyer = 4242

	b.notifyAdminSale(ctx, buyer, "API Credits", "API", 500, 500000, "des-on")
	if n := countContains(f, "NEW SALE"); n != 1 {
		t.Fatalf("default (unset) admin_sale_notify: got %d NEW SALE alert(s), want 1", n)
	}

	s := b.cfg.Settings()
	s["admin_sale_notify"] = false
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	f.Sends = nil
	b.notifyAdminSale(ctx, buyer, "API Credits", "API", 500, 500000, "des-off")
	if n := countContains(f, "NEW SALE"); n != 0 {
		t.Fatalf("admin_sale_notify=false: got %d NEW SALE alert(s), want 0 (toggle OFF must be silent)", n)
	}

	// Third leg: the key GENUINELY ABSENT (not merely true), the only state
	// that actually exercises settingBool's default argument.
	s = b.cfg.Settings()
	delete(s, "admin_sale_notify")
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	f.Sends = nil
	b.notifyAdminSale(ctx, buyer, "API Credits", "API", 500, 500000, "des-absent")
	if n := countContains(f, "NEW SALE"); n != 1 {
		t.Fatalf("absent admin_sale_notify key: got %d NEW SALE alert(s), want 1 (an absent key must default to ON, not silently OFF)", n)
	}
}

// TestNotifyAdminDepositGatedByToggle is admin_deposit_notify's twin of the
// test above (telegram_bot.py:8125-8126). See that test's task #38 (U1)
// comment for why the third leg below is the only one that actually
// exercises settingBool's default argument.
func TestNotifyAdminDepositGatedByToggle(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999")
	const buyer = 4343

	b.notifyAdminDeposit(ctx, buyer, 200000, 200000, "USDT BEP20")
	if n := countContains(f, "NEW DEPOSIT"); n != 1 {
		t.Fatalf("default (unset) admin_deposit_notify: got %d NEW DEPOSIT alert(s), want 1", n)
	}

	s := b.cfg.Settings()
	s["admin_deposit_notify"] = false
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	f.Sends = nil
	b.notifyAdminDeposit(ctx, buyer, 200000, 400000, "USDT BEP20")
	if n := countContains(f, "NEW DEPOSIT"); n != 0 {
		t.Fatalf("admin_deposit_notify=false: got %d NEW DEPOSIT alert(s), want 0 (toggle OFF must be silent)", n)
	}

	// Third leg: the key GENUINELY ABSENT (not merely true) — the deep-copy
	// trap is the same as the sale test above: b.cfg.Settings() must be
	// saved back or the delete is a no-op.
	s = b.cfg.Settings()
	delete(s, "admin_deposit_notify")
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	f.Sends = nil
	b.notifyAdminDeposit(ctx, buyer, 200000, 600000, "USDT BEP20")
	if n := countContains(f, "NEW DEPOSIT"); n != 1 {
		t.Fatalf("absent admin_deposit_notify key: got %d NEW DEPOSIT alert(s), want 1 (an absent key must default to ON, not silently OFF)", n)
	}
}

// TestBuyerMentionFallsBackToChatID: with no cached handle/name and a
// fakeTG.GetChat that returns no username, buyerMention must still render a
// usable (non-empty) tappable mention keyed on the bare chat id — never an
// empty display segment — mirroring _buyer_mention's own
// `str(buyer_chat_id)` fallback (telegram_bot.py:8076).
//
// Task #38 (U2) fix: the tappable-anchor assertion used to be guarded by
// `strings.Contains(got, "<a") && ...`, which makes the whole check OPTIONAL
// — a buyerMention that dropped the <a href="tg://user?id=..."> wrapper
// entirely (rendering a plain, non-tappable "12345 (12345)") would satisfy
// the guard's false left-hand side and the test would stay green. A
// tappable mention is the entire reason notifyAdminSale/notifyAdminDeposit
// use parse_mode=HTML (notify.go's own doc comment), so this now asserts
// the anchor UNCONDITIONALLY. Falsification: stripping the `<a href="tg://
// user?id=%d">...</a>` wrapper from buyerMention (returning just
// disp+" ("+id+")") turns this red — verified by mutation (see the
// executor report).
func TestBuyerMentionFallsBackToChatID(t *testing.T) {
	b, _ := newTestBot("999", nil)
	got := b.buyerMention(context.Background(), 12345)
	if !strings.Contains(got, "12345") {
		t.Errorf("buyerMention(12345) = %q, want it to contain the bare chat id as the fallback display", got)
	}
	if !strings.Contains(got, `tg://user?id=12345`) {
		t.Errorf("buyerMention(12345) = %q, want a tappable tg://user?id=12345 link unconditionally — a bare-text mention defeats the whole reason these alerts use parse_mode=HTML", got)
	}
}

// ---- reconcile alert de-dup / escalation / reset ----------------------------

// TestReconcileAlertDedupSuppressesIdenticalRepeat is the de-dup policy pin:
// the SAME (count, oldestTS) must alert once, then stay silent on repeats —
// the failure mode CARE named explicitly (an identical DM every tick trains
// the operator to ignore the channel).
func TestReconcileAlertDedupSuppressesIdenticalRepeat(t *testing.T) {
	b, _ := newTestBot("999", nil)
	if !b.reconcileShouldAlert(3, 1000) {
		t.Fatal("first-ever pass with a nonzero backlog must alert")
	}
	if b.reconcileShouldAlert(3, 1000) {
		t.Fatal("an UNCHANGED (count, oldestTS) repeat must NOT re-alert")
	}
	if !b.reconcileShouldAlert(4, 1000) {
		t.Fatal("count changing (a new order got stuck) must re-alert")
	}
	if b.reconcileShouldAlert(4, 1000) {
		t.Fatal("repeat of the NEW state must again stay silent")
	}
	if !b.reconcileShouldAlert(4, 2000) {
		t.Fatal("oldestTS changing (the oldest stuck order changed) must re-alert even with count unchanged")
	}
}

// TestReconcileAlertEscalatesAfterTimeout pins the escalation backstop: an
// UNCHANGED backlog still re-alerts after reconcileAlertEscalate, so a human
// who missed the first DM is not left with permanent silence on a problem
// that never got fixed.
func TestReconcileAlertEscalatesAfterTimeout(t *testing.T) {
	orig := reconcileAlertEscalate
	reconcileAlertEscalate = time.Hour
	t.Cleanup(func() { reconcileAlertEscalate = orig })

	b, _ := newTestBot("999", nil)
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }

	if !b.reconcileShouldAlert(1, 500) {
		t.Fatal("first alert must fire")
	}
	if b.reconcileShouldAlert(1, 500) {
		t.Fatal("immediate identical repeat must be suppressed")
	}

	now = now.Add(59 * time.Minute)
	if b.reconcileShouldAlert(1, 500) {
		t.Fatal("still within the escalation window: an unchanged backlog must stay suppressed")
	}

	now = now.Add(2 * time.Minute) // now 61 minutes after the first alert
	if !b.reconcileShouldAlert(1, 500) {
		t.Fatal("past the escalation window: an unchanged backlog must re-alert")
	}
}

// TestReconcileAlertResetsOnRecovery pins the fix for the reset gap: once the
// backlog clears (count==0, via reconcileStuckOrdersOnce's early return) the
// fingerprint must be cleared, so a LATER recurrence of the identical
// (count, oldestTS) is treated as a brand-new incident, not "unchanged since
// last time" — without the reset, the exact same order getting stuck again
// after a full recovery would stay silent forever.
//
// Task #38 (U3+U4) fix: this test used to end with a DIRECT
// `b.reconcileShouldAlert(1, 3000)` call — hardcoded literals that read no
// store and check no send. That made the "re-claim paid" ClaimOrder call
// just above it DEAD: reconcileShouldAlert never looks at the store, so the
// test passed identically whether or not "stuck1" was actually re-claimed.
// It also meant the reconcile owner DM's actual CONTENT (recipient, text,
// stuck count) was pinned nowhere in the suite — the only other coverage,
// reconcile_test.go's TestReconcileReportMutatesNothing, only asserts a
// send-COUNT delta (+1), which cannot detect a wrong recipient or wrong
// text.
//
// Both close with the SAME edit: replace the direct reconcileShouldAlert
// call with a real b.reconcileStuckOrdersOnce(ctx) pass (so the recreated
// "stuck1" row is what actually drives the alert, making the ClaimOrder
// call load-bearing) and assert on the DM itself — that the owner
// specifically receives a message naming PAID-BUT-UNDELIVERED and the
// stuck count — plus the de-dup half: a second, immediately-following pass
// at the SAME (count, oldestTS) must send nothing.
func TestReconcileAlertResetsOnRecovery(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999") // AdminID "999", no devIDs -> the sole owner recipient is 999

	seedOrderAtBot(t, b, "stuck1", "kiro-pro", "paid", 3000)
	b.reconcileStuckOrdersOnce(ctx) // first alert fires, fingerprint = (1, 3000)

	// Recovery: the order gets delivered, so the next pass sees count==0 and
	// must reset the fingerprint (reconcile.go's count==0 branch).
	if _, err := b.store.ClaimOrder("stuck1", "paid", "delivered"); err != nil {
		t.Fatalf("claim delivered: %v", err)
	}
	b.reconcileStuckOrdersOnce(ctx) // count==0 -> reset, no alert

	// The SAME order gets stuck again with the SAME ts (adversarial but
	// possible: a replayed settle re-lands it 'paid'). Without the reset this
	// looks identical to the pre-recovery state and reconcileShouldAlert
	// would wrongly suppress it. This ClaimOrder call is what the fix above
	// makes load-bearing: reconcileStuckOrdersOnce below reads the store, so
	// deleting this line drops the count back to 0 and the alert never fires.
	if _, err := b.store.ClaimOrder("stuck1", "delivered", "paid"); err != nil {
		t.Fatalf("re-claim paid: %v", err)
	}

	f.Sends = nil // isolate what THIS recurrence sends
	b.reconcileStuckOrdersOnce(ctx)
	last := lastSendMsgTo(f, 999)
	if last == nil {
		t.Fatal("a recurrence of the identical (count, oldestTS) AFTER a full recovery " +
			"must alert the owner again — the fingerprint was not reset on recovery")
	}
	if !strings.Contains(last.Text, "PAID-BUT-UNDELIVERED") {
		t.Errorf("owner alert text = %q, want it to contain PAID-BUT-UNDELIVERED", last.Text)
	}
	if !strings.Contains(last.Text, "1 PAID-BUT-UNDELIVERED") {
		t.Errorf("owner alert text = %q, want the stuck count (1) in the alert", last.Text)
	}

	// De-dup half: an immediately-following pass at the SAME (count,
	// oldestTS) must send NOTHING.
	before := len(f.Sends)
	b.reconcileStuckOrdersOnce(ctx)
	if len(f.Sends) != before {
		t.Errorf("a second immediate pass sent %d more message(s), want 0 (unchanged backlog must stay de-duped)", len(f.Sends)-before)
	}
}
