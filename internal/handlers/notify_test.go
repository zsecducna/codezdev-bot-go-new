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
func TestNotifyAdminSaleGatedByToggle(t *testing.T) {
	ctx := context.Background()
	b, f, _, _, _ := newBuyBot(t, "999")
	const buyer = 4242

	b.notifyAdminSale(ctx, buyer, "Bedrock API Credits", "API", 500, 500000, "des-on")
	if n := countContains(f, "NEW SALE"); n != 1 {
		t.Fatalf("default (unset) admin_sale_notify: got %d NEW SALE alert(s), want 1", n)
	}

	s := b.cfg.Settings()
	s["admin_sale_notify"] = false
	if err := b.cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	f.Sends = nil
	b.notifyAdminSale(ctx, buyer, "Bedrock API Credits", "API", 500, 500000, "des-off")
	if n := countContains(f, "NEW SALE"); n != 0 {
		t.Fatalf("admin_sale_notify=false: got %d NEW SALE alert(s), want 0 (toggle OFF must be silent)", n)
	}
}

// TestNotifyAdminDepositGatedByToggle is admin_deposit_notify's twin of the
// test above (telegram_bot.py:8125-8126).
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
}

// TestBuyerMentionFallsBackToChatID: with no cached handle/name and a
// fakeTG.GetChat that returns no username, buyerMention must still render a
// usable (non-empty) tappable mention keyed on the bare chat id — never an
// empty display segment — mirroring _buyer_mention's own
// `str(buyer_chat_id)` fallback (telegram_bot.py:8076).
func TestBuyerMentionFallsBackToChatID(t *testing.T) {
	b, _ := newTestBot("999", nil)
	got := b.buyerMention(context.Background(), 12345)
	if !strings.Contains(got, "12345") {
		t.Errorf("buyerMention(12345) = %q, want it to contain the bare chat id as the fallback display", got)
	}
	if strings.Contains(got, "<a") && !strings.Contains(got, `tg://user?id=12345`) {
		t.Errorf("buyerMention(12345) = %q, tappable link must target the same chat id", got)
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
func TestReconcileAlertResetsOnRecovery(t *testing.T) {
	ctx := context.Background()
	b, _, _, _, _ := newBuyBot(t, "999")

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
	// would wrongly suppress it.
	if _, err := b.store.ClaimOrder("stuck1", "delivered", "paid"); err != nil {
		t.Fatalf("re-claim paid: %v", err)
	}
	if !b.reconcileShouldAlert(1, 3000) {
		t.Error("a recurrence of the identical (count, oldestTS) AFTER a full recovery " +
			"must alert again — the fingerprint was not reset on recovery")
	}
}
