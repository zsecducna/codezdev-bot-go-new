package handlers

// adminusers_test.go — tests for the 👥 CUSTOMERS, 🛡️ ADMIN ROLES, private-sale
// WHITELIST, and RESELLERS callbacks (adminusers.go). Drives every validator
// through b.Process (never reimplements a validator in the test body — a
// previous slice was caught doing that) and uses newTestBot/callbackUpdate/
// textUpdate/lastSendMsg/buttonData from fake_test.go and admin_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// lastSendMsgTo returns the most recent recorded sendMessage TO a specific
// chatID, or nil — several flows in this file send the admin's own
// confirmation AND a customer DM (or a re-rendered menu) in the same
// b.Process call, so plain lastSendMsg (which only sees the very last of
// EITHER) is the wrong tool for asserting on the admin-side message.
func lastSendMsgTo(f *fakeTG, chatID int64) *tg.SendMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.Sends) - 1; i >= 0; i-- {
		if f.Sends[i].ChatID == chatID {
			return &f.Sends[i]
		}
	}
	return nil
}

// anySendContains (usdt_test.go) is reused below for flows (rmwl:/
// rmreseller:) that send a confirmation THEN re-render a menu in the same
// b.Process call, so the confirmation is not necessarily the last send.

// withAdminsFile isolates the package-level adminsFilePath var for one test —
// New() reads admins.json at construction time, so a test must redirect the
// path BEFORE building a Bot (mirrors adminpricing_test.go's
// withDiscountsFile), or every test in this package would race on (and could
// write into) the real package directory's admins.json.
func withAdminsFile(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := adminsFilePath
	adminsFilePath = filepath.Join(dir, "admins.json")
	t.Cleanup(func() { adminsFilePath = old })
}

// newUsersTestBot builds a Bot wired to a real *config.Config (whitelist/
// resellers persistence) AND a real *store.Store (balances/orders/handles) —
// newTestBot's Store/Config are always nil, which would panic/no-op every
// write this file's handlers make.
func newUsersTestBot(t *testing.T, adminID string) (*Bot, *fakeTG, *config.Config, *store.Store) {
	t.Helper()
	withAdminsFile(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeTG{}
	b := New(Deps{TG: f, Config: cfg, Store: st, AdminID: adminID})
	t.Cleanup(b.Shutdown)
	return b, f, cfg, st
}

// ============================================================================
// 👥 CUSTOMERS
// ============================================================================

func TestCustLookup(t *testing.T) {
	ctx := context.Background()

	t.Run("found by numeric chat_id", func(t *testing.T) {
		b, f, _, st := newUsersTestBot(t, "1")
		if _, _, err := st.Credit("555", 10_000_000, "TXN-1"); err != nil { // 10.00 USDT in micro
			t.Fatalf("seed credit: %v", err)
		}
		b.Process(ctx, callbackUpdate(1, "adm:custlookup"))
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Send the customer's chat_id") {
			t.Fatalf("arm prompt = %+v", last)
		}
		b.Process(ctx, textUpdate(1, "555"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "👤 CUSTOMER 555") {
			t.Fatalf("profile = %+v", last)
		}
		if !strings.Contains(last.Text, "10.00 USDT") {
			t.Errorf("profile missing balance: %q", last.Text)
		}
	})

	t.Run("found by handle", func(t *testing.T) {
		b, f, _, st := newUsersTestBot(t, "1")
		if err := st.SetHandle("777", "@bob"); err != nil {
			t.Fatalf("seed handle: %v", err)
		}
		b.Process(ctx, callbackUpdate(1, "adm:custlookup"))
		b.Process(ctx, textUpdate(1, "@bob"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "👤 CUSTOMER 777") {
			t.Fatalf("profile = %+v", last)
		}
	})

	t.Run("not found — abort, no re-arm on next line", func(t *testing.T) {
		b, f, _, _ := newUsersTestBot(t, "1")
		b.Process(ctx, callbackUpdate(1, "adm:custlookup"))
		b.Process(ctx, textUpdate(1, "@nosuchuser"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "No customer found for “@nosuchuser”") {
			t.Fatalf("rejection = %+v", last)
		}
		if !containsStr(buttonData(last.ReplyMarkup), "adm:custlookup") {
			t.Errorf("rejection missing re-arm button: %v", buttonData(last.ReplyMarkup))
		}
		// Abort-on-attempt: the slot was already popped, so a second line is
		// NOT treated as a second lookup attempt.
		f.Sends = nil
		b.Process(ctx, textUpdate(1, "555"))
		if last := lastSendMsg(f); last != nil {
			t.Errorf("slot should be popped after one attempt; got %+v", last)
		}
	})
}

// ============================================================================
// 💰 adm:addbal
// ============================================================================

func TestAddBalFlow(t *testing.T) {
	ctx := context.Background()

	t.Run("credit path", func(t *testing.T) {
		b, f, _, st := newUsersTestBot(t, "1")
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "amount in USDT") {
			t.Fatalf("field2 prompt = %+v", last)
		}
		b.Process(ctx, textUpdate(1, "2.5"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "confirm") || !containsStr(buttonData(last.ReplyMarkup), "admdo:confirm") {
			t.Fatalf("confirm summary = %+v", last)
		}
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		last = lastSendMsgTo(f, 1)
		want := "✅ Credited 2.50 USDT to 555. New balance: 2.50 USDT"
		if last == nil || last.Text != want {
			t.Fatalf("admin confirmation = %+v, want %q", last, want)
		}
		bal, err := st.GetBalance("555")
		if err != nil || bal != 2_500_000 {
			t.Errorf("GetBalance(555) = %d, %v, want 2_500_000 (2.50 USDT in micro)", bal, err)
		}
		// Customer DM.
		found := false
		for _, s := range f.Sends {
			if s.ChatID == 555 && strings.Contains(s.Text, "added to your balance") {
				found = true
			}
		}
		if !found {
			t.Error("customer DM not sent")
		}
	})

	t.Run("debit path succeeds within balance", func(t *testing.T) {
		b, f, _, st := newUsersTestBot(t, "1")
		if _, err := st.CreditBalance("555", 4_000_000); err != nil { // 4.00 USDT
			t.Fatal(err)
		}
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		b.Process(ctx, textUpdate(1, "-2"))
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		last := lastSendMsgTo(f, 1)
		want := "✅ Deducted 2.00 USDT from 555. New balance: 2.00 USDT"
		if last == nil || last.Text != want {
			t.Fatalf("admin confirmation = %+v, want %q", last, want)
		}
		if bal, _ := st.GetBalance("555"); bal != 2_000_000 {
			t.Errorf("GetBalance(555) = %d, want 2_000_000 (2.00 USDT in micro)", bal)
		}
	})

	t.Run("debit-overdraw refusal persists nothing", func(t *testing.T) {
		b, f, _, st := newUsersTestBot(t, "1")
		if _, err := st.CreditBalance("555", 1_000_000); err != nil { // 1.00 USDT
			t.Fatal(err)
		}
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		b.Process(ctx, textUpdate(1, "-5")) // trying to deduct 5 USDT from a 1 USDT balance
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		last := lastSendMsg(f)
		want := "❌ Can't deduct 5.00 USDT from 555 — balance is only 1.00 USDT."
		if last == nil || last.Text != want {
			t.Fatalf("rejection = %+v, want %q", last, want)
		}
		if bal, _ := st.GetBalance("555"); bal != 1_000_000 {
			t.Errorf("balance mutated on refused debit: got %d, want 1_000_000 (1.00 USDT in micro)", bal)
		}
	})

	t.Run("invalid amount rejects and persists nothing", func(t *testing.T) {
		b, f, _, st := newUsersTestBot(t, "1")
		if _, err := st.CreditBalance("555", 1_000_000); err != nil {
			t.Fatal(err)
		}
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		b.Process(ctx, textUpdate(1, "not-a-number"))
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		if last := lastSendMsg(f); last == nil || last.Text != "❌ Amount must be a number, e.g. 2.5 or -1." {
			t.Fatalf("rejection = %+v", last)
		}
		if bal, _ := st.GetBalance("555"); bal != 1_000_000 {
			t.Errorf("balance mutated on invalid amount: got %d, want 1_000_000", bal)
		}
	})

	t.Run("zero amount rejected", func(t *testing.T) {
		b, f, _, _ := newUsersTestBot(t, "1")
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		b.Process(ctx, textUpdate(1, "0"))
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		if last := lastSendMsg(f); last == nil || last.Text != "❌ Amount can't be 0." {
			t.Fatalf("rejection = %+v", last)
		}
	})

	t.Run("customer not found — resolved only at confirm time", func(t *testing.T) {
		b, f, _, _ := newUsersTestBot(t, "1")
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "@nosuchuser")) // accepted RAW, unvalidated at this stage
		b.Process(ctx, textUpdate(1, "2.5"))
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		last := lastSendMsg(f)
		if last == nil || !strings.Contains(last.Text, "No customer found for “@nosuchuser”") {
			t.Fatalf("rejection = %+v", last)
		}
	})

	t.Run("cancel discards the wizard", func(t *testing.T) {
		b, f, _, _ := newUsersTestBot(t, "1")
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		b.Process(ctx, textUpdate(1, "2.5"))
		b.Process(ctx, callbackUpdate(1, "admdo:cancel"))
		if last := lastSendMsg(f); last == nil || last.Text != "❌ Cancelled." {
			t.Fatalf("cancel reply = %+v", last)
		}
		st := b.states.Get(1)
		st.mu.Lock()
		armed := st.Admin
		st.mu.Unlock()
		if armed != nil {
			t.Error("Admin (addbal) must be cleared after cancel")
		}
	})
}

// ---- task #31: execAddBal audit rows ---------------------------------------
//
// execAddBal's own doc comment (adminusers.go ~394-400) flags the hazard
// these tests pin against: store.CreditBalance writes NO ledger/txn row, so
// before this task the audit row added here was the ONLY possible record
// that an admin wallet adjustment ever happened — there is no second table
// to cross-check against, unlike every other admin money mutation in this
// codebase. Row-count DELTAS are asserted throughout (not a filtered
// lookup) per this repo's own lesson (task #14) that a filtered query can
// miss a row that got split or renamed.

// TestAddBalCreditAuditsRow proves the credit branch writes exactly one row
// with the signed (+) amount and the resulting balance.
func TestAddBalCreditAuditsRow(t *testing.T) {
	ctx := context.Background()
	b, f, _, st := newUsersTestBot(t, "1")

	before, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit before: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:addbal"))
	b.Process(ctx, textUpdate(1, "555"))
	b.Process(ctx, textUpdate(1, "2.5"))
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
	if last := lastSendMsgTo(f, 1); last == nil || !strings.Contains(last.Text, "✅ Credited") {
		t.Fatalf("expected a credit confirmation, got %+v", last)
	}

	after, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("audit row count = %d, want exactly %d (before+1): %+v", len(after), len(before)+1, after)
	}
	row := after[0] // RecentAudit is newest-first
	if row.Action != "admin_credit_balance" {
		t.Errorf("Action = %q, want admin_credit_balance", row.Action)
	}
	if row.ActorChat != "1" || row.ActorKind != "admin" {
		t.Errorf("actor = %q/%q, want 1/admin", row.ActorChat, row.ActorKind)
	}
	if row.Target != "555" {
		t.Errorf("Target = %q, want 555", row.Target)
	}
	if want := "+2.50 USDT → new balance 2.50 USDT"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}
	if want := "amount_micro=2500000 new_balance_micro=2500000"; row.Detail != want {
		t.Errorf("Detail = %q, want %q", row.Detail, want)
	}
}

// TestAddBalCreditCollisionDoesNotDoubleCredit is a white-box test of
// execAddBal's OWN behavior on an admin_credits id collision — calls it
// directly (bypassing the wizard, which by construction never produces two
// calls with the same id; that's arm-time minting's whole point, see
// AdminAction.ID's doc comment) to reach the branch a genuine collision
// would take. Proves the review fix: before it, a collision fell through
// into the LogAction call and the "✅ Credited" send, telling the admin AND
// customer a credit happened that didn't — exactly the kind of lie this
// whole feature exists to prevent. Now it must short-circuit with none of
// that.
func TestAddBalCreditCollisionDoesNotDoubleCredit(t *testing.T) {
	ctx := context.Background()
	b, f, _, st := newUsersTestBot(t, "1")

	before, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit before: %v", err)
	}

	id := store.NewDes()
	b.execAddBal(ctx, 1, id, "555", "2.5")
	if last := lastSendMsgTo(f, 1); last == nil || !strings.Contains(last.Text, "✅ Credited") {
		t.Fatalf("first call: expected a credit confirmation, got %+v", last)
	}
	if bal, err := st.GetBalance("555"); err != nil || bal != 2_500_000 {
		t.Fatalf("balance after first call = %d, err=%v, want 2500000", bal, err)
	}

	// Same id again: must be rejected, not silently re-applied or reported
	// as a fresh credit.
	b.execAddBal(ctx, 1, id, "555", "2.5")
	last := lastSendMsgTo(f, 1)
	if last == nil || strings.Contains(last.Text, "✅ Credited") {
		t.Fatalf("second call (collision) must NOT claim a new credit, got %+v", last)
	}
	if !strings.Contains(last.Text, "Already applied") {
		t.Errorf("second call message = %q, want it to say already applied", last.Text)
	}
	if bal, err := st.GetBalance("555"); err != nil || bal != 2_500_000 {
		t.Fatalf("balance after collision = %d, err=%v, want STILL 2500000 (no double-credit)", bal, err)
	}

	after, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("audit row count = %d, want exactly %d (before+1 -- a collision must NOT audit a second row)",
			len(after), len(before)+1)
	}
}

// TestAddBalRearmMintsFreshID proves the OTHER half of the arm-time-id
// design: a manual re-arm (adm:addbal run twice, same target/amount) is a
// genuinely new admin action, not a replay — it must credit BOTH times, not
// collide. Distinguishing this from a real collision (previous test) is the
// whole reason ID is minted at arm time instead of e.g. from
// target+amount.
func TestAddBalRearmMintsFreshID(t *testing.T) {
	ctx := context.Background()
	b, f, _, st := newUsersTestBot(t, "1")

	for i := range 2 {
		b.Process(ctx, callbackUpdate(1, "adm:addbal"))
		b.Process(ctx, textUpdate(1, "555"))
		b.Process(ctx, textUpdate(1, "2.5"))
		b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
		if last := lastSendMsgTo(f, 1); last == nil || !strings.Contains(last.Text, "✅ Credited") {
			t.Fatalf("arm #%d: expected a credit confirmation, got %+v", i+1, last)
		}
	}
	if bal, err := st.GetBalance("555"); err != nil || bal != 5_000_000 {
		t.Fatalf("balance after two re-arms = %d, err=%v, want 5000000 (both must apply)", bal, err)
	}
}

// TestAddBalDebitAuditsRowWithCorrectSign proves the debit branch writes
// exactly one row, and — the property that actually matters here — the
// SIGN is "-", not "+". A debit that recorded with a "+" sign would be
// indistinguishable from a credit to anyone scanning /audit for where a
// customer's balance came from, which is exactly the forensic use case this
// row exists for.
func TestAddBalDebitAuditsRowWithCorrectSign(t *testing.T) {
	ctx := context.Background()
	b, f, _, st := newUsersTestBot(t, "1")
	if _, err := st.CreditBalance("555", 4_000_000); err != nil {
		t.Fatal(err)
	}

	before, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit before: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:addbal"))
	b.Process(ctx, textUpdate(1, "555"))
	b.Process(ctx, textUpdate(1, "-2"))
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
	if last := lastSendMsgTo(f, 1); last == nil || !strings.Contains(last.Text, "✅ Deducted") {
		t.Fatalf("expected a debit confirmation, got %+v", last)
	}

	after, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("audit row count = %d, want exactly %d (before+1): %+v", len(after), len(before)+1, after)
	}
	row := after[0]
	if row.Action != "admin_debit_balance" {
		t.Errorf("Action = %q, want admin_debit_balance", row.Action)
	}
	if strings.HasPrefix(row.Outcome, "+") {
		t.Fatalf("Outcome = %q — a debit must NEVER carry a '+' sign", row.Outcome)
	}
	if want := "-2.00 USDT → new balance 2.00 USDT"; row.Outcome != want {
		t.Errorf("Outcome = %q, want %q", row.Outcome, want)
	}
	if want := "amount_micro=-2000000 new_balance_micro=2000000"; row.Detail != want {
		t.Errorf("Detail = %q, want %q", row.Detail, want)
	}
}

// TestAddBalNoAuditOnFailedMutation proves NO row is written when the
// underlying store write itself fails (as opposed to the "not enough
// balance" refusal, which is a deliberate no-op already covered by
// TestAddBalFlow's "debit-overdraw refusal persists nothing" subtest and
// never reaches the audit call in the first place). Fault injection reuses
// newQueryOnlyStoreBot from adminpricing_test.go (read-only reference, same
// package, not an edit to that file) rather than re-deriving the same
// table-scoped trigger technique a third time — CreditBalance/Debit are
// plain store writes with no config.SaveSettings step, the same shape
// capPromo/cbPromoDel needed that helper for. Faults "balances" only
// (CreditBalance's write), so — per that helper's doc comment on the trap a
// blanket query_only fell into — the audit table stays healthy and the
// count comparison below actually tests the guard, not the fault
// injection.
func TestAddBalNoAuditOnFailedMutation(t *testing.T) {
	ctx := context.Background()
	b, f, st := newQueryOnlyStoreBot(t, nil, "balances")

	before, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit before: %v", err)
	}

	b.Process(ctx, callbackUpdate(1, "adm:addbal"))
	b.Process(ctx, textUpdate(1, "555"))
	b.Process(ctx, textUpdate(1, "2.5"))
	b.Process(ctx, callbackUpdate(1, "admdo:confirm"))
	if last := lastSendMsg(f); last == nil || last.Text != settingsSaveFailed {
		t.Fatalf("reply on failed CreditBalance = %+v, want %q", last, settingsSaveFailed)
	}

	after, err := st.RecentAudit(50, "")
	if err != nil {
		t.Fatalf("RecentAudit after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("audit row count = %d, want unchanged %d (no row on a failed mutation): %+v", len(after), len(before), after)
	}
}

// ============================================================================
// 🛡️ ADMIN ROLES
// ============================================================================

func TestAdminRolesRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, f, _, _ := newUsersTestBot(t, "1")

	// Grant.
	b.Process(ctx, callbackUpdate(1, "adm:addadmin"))
	b.Process(ctx, textUpdate(1, "555"))
	last := lastSendMsgTo(f, 1)
	if last == nil || last.Text != "✅ Granted admin access to 555." {
		t.Fatalf("grant reply = %+v", last)
	}
	dmFound := false
	for _, s := range f.Sends {
		if s.ChatID == 555 && strings.Contains(s.Text, "granted admin access") {
			dmFound = true
		}
	}
	if !dmFound {
		t.Error("grant DM not sent to 555")
	}
	if !b.isAdmin(555) {
		t.Error("555 should now be a standard admin")
	}

	// listadmins shows it.
	b.Process(ctx, callbackUpdate(1, "adm:listadmins"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "555") {
		t.Fatalf("roster = %+v", last)
	}

	// Dup grant.
	b.Process(ctx, callbackUpdate(1, "adm:addadmin"))
	b.Process(ctx, textUpdate(1, "555"))
	if last := lastSendMsg(f); last == nil || last.Text != "ℹ️ 555 is already an admin (or is the super admin)." {
		t.Fatalf("dup grant reply = %+v", last)
	}

	// Super admin can never be granted (already implicit).
	b.Process(ctx, callbackUpdate(1, "adm:addadmin"))
	b.Process(ctx, textUpdate(1, "1"))
	if last := lastSendMsg(f); last == nil || last.Text != "ℹ️ 1 is already an admin (or is the super admin)." {
		t.Fatalf("super-admin grant reply = %+v", last)
	}

	// Invalid (non-numeric) grant target.
	b.Process(ctx, callbackUpdate(1, "adm:addadmin"))
	b.Process(ctx, textUpdate(1, "@notanumber"))
	if last := lastSendMsg(f); last == nil || last.Text != "⚠️ Enter a numeric chat_id, e.g. 7302701741." {
		t.Fatalf("invalid grant reply = %+v", last)
	}

	// Revoke.
	b.Process(ctx, callbackUpdate(1, "adm:rmadmin"))
	b.Process(ctx, textUpdate(1, "555"))
	if last := lastSendMsgTo(f, 1); last == nil || last.Text != "✅ Revoked admin access from 555." {
		t.Fatalf("revoke reply = %+v", last)
	}
	if b.isAdmin(555) {
		t.Error("555 should no longer be an admin")
	}
	revokeDM := false
	for _, s := range f.Sends {
		if s.ChatID == 555 && strings.Contains(s.Text, "revoked") {
			revokeDM = true
		}
	}
	if !revokeDM {
		t.Error("revoke DM not sent to 555")
	}

	// Revoke again — not a granted admin.
	b.Process(ctx, callbackUpdate(1, "adm:rmadmin"))
	b.Process(ctx, textUpdate(1, "555"))
	if last := lastSendMsg(f); last == nil || last.Text != "ℹ️ 555 is not a granted admin (the super admin can't be removed)." {
		t.Fatalf("re-revoke reply = %+v", last)
	}

	// The super admin can NEVER be removed (was never stored).
	b.Process(ctx, callbackUpdate(1, "adm:rmadmin"))
	b.Process(ctx, textUpdate(1, "1"))
	if last := lastSendMsg(f); last == nil || last.Text != "ℹ️ 1 is not a granted admin (the super admin can't be removed)." {
		t.Fatalf("remove-super-admin reply = %+v", last)
	}
}

// ============================================================================
// Private-sale whitelist
// ============================================================================

func TestWhitelistAddRemove(t *testing.T) {
	ctx := context.Background()
	b, f, cfg, _ := newUsersTestBot(t, "1")

	// Invalid.
	b.Process(ctx, callbackUpdate(1, "adm:addwl"))
	b.Process(ctx, textUpdate(1, "bad!!"))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "⚠️ Not a valid Telegram numeric ID") {
		t.Fatalf("invalid reply = %+v", last)
	}

	// Valid numeric id.
	b.Process(ctx, callbackUpdate(1, "adm:addwl"))
	b.Process(ctx, textUpdate(1, "7302701741"))
	want := "✅ Added 7302701741 to the private-sale whitelist. They can buy while sale is paused + Private Sale is ON."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("add reply = %+v, want %q", last, want)
	}

	// Dup (case doesn't matter for a bare digit id, but exercise it as-is).
	b.Process(ctx, callbackUpdate(1, "adm:addwl"))
	b.Process(ctx, textUpdate(1, "7302701741"))
	if last := lastSendMsg(f); last == nil || last.Text != "ℹ️ That customer is already whitelisted." {
		t.Fatalf("dup reply = %+v", last)
	}

	// Username + note.
	b.Process(ctx, callbackUpdate(1, "adm:addwl"))
	b.Process(ctx, textUpdate(1, "@vipuser | VIP"))
	want = "✅ Added @vipuser to the private-sale whitelist — VIP. They can buy while sale is paused + Private Sale is ON."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("username add reply = %+v, want %q", last, want)
	}

	// Full.
	s := cfg.Settings()
	full := make([]any, whitelistMax)
	for i := range full {
		full[i] = map[string]any{"v": strconv.Itoa(10000000 + i), "note": ""}
	}
	s["whitelist"] = full
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	b.Process(ctx, callbackUpdate(1, "adm:addwl"))
	b.Process(ctx, textUpdate(1, "9999999999"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Whitelist is full (max 100)") {
		t.Fatalf("full reply = %+v", last)
	}

	// Removal by value: reset to just the earlier two entries and remove one.
	s = cfg.Settings()
	s["whitelist"] = []any{
		map[string]any{"v": "7302701741", "note": ""},
		map[string]any{"v": "vipuser", "note": "VIP"},
	}
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	b.Process(ctx, callbackUpdate(1, "rmwl:7302701741"))
	// cbRmWL sends the "➖ Removed" confirmation FIRST, then re-renders the
	// whitelist menu — so it is not necessarily the LAST send.
	if !anySendContains(f, "➖ Removed 7302701741.") {
		t.Fatalf("removal confirmation not sent; sends=%+v", f.Sends)
	}
	if b.isWhitelisted(1234567890) { // sanity: unrelated chat still not whitelisted
		t.Error("unrelated chat should not be whitelisted")
	}
	for _, e := range b.whitelist() {
		if e.V == "7302701741" {
			t.Error("7302701741 should have been removed from the whitelist")
		}
	}
}

// ============================================================================
// Authorized resellers
// ============================================================================

func TestResellerAddRemoveDiscount(t *testing.T) {
	ctx := context.Background()
	b, f, cfg, _ := newUsersTestBot(t, "1")

	// Invalid (all-digit is not a valid reseller handle).
	b.Process(ctx, callbackUpdate(1, "adm:addreseller"))
	b.Process(ctx, textUpdate(1, "1234"))
	if last := lastSendMsg(f); last == nil || !strings.HasPrefix(last.Text, "⚠️ That's not a valid Telegram @username") {
		t.Fatalf("invalid reply = %+v", last)
	}

	// Valid add.
	b.Process(ctx, callbackUpdate(1, "adm:addreseller"))
	b.Process(ctx, textUpdate(1, "@myreseller"))
	want := "✅ Added reseller @myreseller. Customers now see it on the Buy screen."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("add reply = %+v, want %q", last, want)
	}

	// Dup.
	b.Process(ctx, callbackUpdate(1, "adm:addreseller"))
	b.Process(ctx, textUpdate(1, "@myreseller"))
	if last := lastSendMsg(f); last == nil || last.Text != "ℹ️ That reseller is already in the list." {
		t.Fatalf("dup reply = %+v", last)
	}

	// Full.
	s := cfg.Settings()
	full := make([]any, resellerMax)
	for i := range full {
		full[i] = map[string]any{"u": "reseller" + strconv.Itoa(i), "note": "", "disc": 0}
	}
	s["resellers"] = full
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	b.Process(ctx, callbackUpdate(1, "adm:addreseller"))
	b.Process(ctx, textUpdate(1, "@onemoreone"))
	if last := lastSendMsg(f); last == nil || !strings.Contains(last.Text, "Reseller list is full (max 20)") {
		t.Fatalf("full reply = %+v", last)
	}

	// Reset to a single known reseller for the manage/discount/removal flow.
	s = cfg.Settings()
	s["resellers"] = []any{map[string]any{"u": "myreseller", "note": "", "disc": 0}}
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	// resmng: submenu.
	b.Process(ctx, callbackUpdate(1, "resmng:myreseller"))
	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, "@myreseller") {
		t.Fatalf("resmng submenu = %+v", last)
	}
	if !containsStr(buttonData(last.ReplyMarkup), "resdisc:myreseller") ||
		!containsStr(buttonData(last.ReplyMarkup), "rmreseller:myreseller") {
		t.Errorf("resmng submenu missing expected buttons: %v", buttonData(last.ReplyMarkup))
	}

	// Discount: invalid (>95).
	b.Process(ctx, callbackUpdate(1, "resdisc:myreseller"))
	b.Process(ctx, textUpdate(1, "96"))
	if last := lastSendMsg(f); last == nil || last.Text != "⚠️ Invalid. Send a whole number 0–95 (0 = off)." {
		t.Fatalf("invalid discount reply = %+v", last)
	}

	// Discount: set to 50.
	b.Process(ctx, callbackUpdate(1, "resdisc:myreseller"))
	b.Process(ctx, textUpdate(1, "50"))
	want = "✅ Reselling discount for @myreseller set to 50% (ignored when they use a promo/referral code)."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("set discount reply = %+v, want %q", last, want)
	}
	if pct := b.resellerPctFor(0); pct != 0 { // unrelated chat, no handle -> 0, sanity only
		t.Errorf("resellerPctFor(unrelated) = %d, want 0", pct)
	}

	// Discount: 0 turns it off.
	b.Process(ctx, callbackUpdate(1, "resdisc:myreseller"))
	b.Process(ctx, textUpdate(1, "0"))
	want = "✅ Reselling discount for @myreseller turned OFF (they get the normal volume discount)."
	if last := lastSendMsg(f); last == nil || last.Text != want {
		t.Fatalf("turn-off discount reply = %+v, want %q", last, want)
	}

	// Vanished-reseller case: arm the discount prompt, then remove the
	// reseller before replying.
	b.Process(ctx, callbackUpdate(1, "resdisc:myreseller"))
	b.Process(ctx, callbackUpdate(1, "rmreseller:myreseller"))
	// cbRmReseller sends the "➖ Removed" confirmation FIRST, then re-renders
	// the resellers menu — so it is not necessarily the last send.
	if !anySendContains(f, "➖ Removed @myreseller.") {
		t.Fatalf("removal confirmation not sent; sends=%+v", f.Sends)
	}
	b.Process(ctx, textUpdate(1, "10"))
	if last := lastSendMsg(f); last == nil || last.Text != "⚠️ @myreseller is no longer in the reseller list." {
		t.Fatalf("vanished-reseller reply = %+v", last)
	}
}

// TestCapResellerDiscountDemotedAdminClearsArm proves capResellerDiscount's
// G2 isSuperAdmin re-check (adminusers.go) sits AFTER the pop, not before —
// the only ordering that can be correct here. c.st.ResellerDiscount carries
// the target reseller's USERNAME, not a plain bool, so a guard placed above
// the pop would consume the demoted admin's reply (the nav/slash carve-out
// already passed) without ever clearing the slot: the wizard stays armed
// forever, wedging every later message from that chat behind a capture that
// can now never again return false.
//
// The wizard can only be armed for real through cbResDisc, which is
// fullAdminOnly-gated, so there is no live path for a chat that was NEVER
// super-admin to arm it itself. This test sets the pending state directly to
// stand in for the scenario the sibling captures' own comments describe as
// real: a super admin arms it, then loses super-admin status before typing
// the reply (mid-flow revocation).
func TestCapResellerDiscountDemotedAdminClearsArm(t *testing.T) {
	ctx := context.Background()
	b, _, cfg, _ := newUsersTestBot(t, "1") // super admin is chat 1
	s := cfg.Settings()
	s["resellers"] = []any{map[string]any{"u": "myreseller", "note": "", "disc": 0}}
	if err := cfg.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	const demoted = int64(7) // never super-admin — adminID is "1"
	st := b.states.Get(demoted)
	st.mu.Lock()
	st.ResellerDiscount = "myreseller"
	st.mu.Unlock()

	b.Process(ctx, textUpdate(demoted, "42"))

	st.mu.Lock()
	armed := st.ResellerDiscount
	st.mu.Unlock()
	if armed != "" {
		t.Errorf("ResellerDiscount = %q after a demoted admin's reply, want \"\" — arm left wedged, every later message from this chat is now dead on arrival", armed)
	}
	if r := b.findReseller("myreseller"); r == nil || r.Disc != 0 {
		t.Errorf("reseller discount mutated by a demoted admin's reply: %+v, want unchanged disc=0", r)
	}
}

// ============================================================================
// Permission split — customers_view/toggle_sales (grantable) vs full-admin-only
// ============================================================================

func TestAdminUsersPermissionSplit(t *testing.T) {
	ctx := context.Background()
	b, f, _, _ := newUsersTestBot(t, "1")
	b.adminsMu.Lock()
	b.admins["77"] = true // a granted "standard" admin — NOT the super admin
	b.adminsMu.Unlock()

	// M7 fix: this subtest used to ALSO assert "adm:custlookup" reaches a
	// standard admin — that ENSHRINED the H1 privilege-escalation bug
	// (customers_view is NOT one of admincap.go's defaultStdCaps, so a
	// standard admin must NOT reach it). custlookup now lives in the
	// "CANNOT reach" subtest below instead; only toggle_sales-capability
	// actions (default-ON) stay here.
	t.Run("standard admin CAN reach toggle_sales actions", func(t *testing.T) {
		f.Sends, f.Answers = nil, nil
		b.Process(ctx, callbackUpdate(77, "adm:addwl"))
		if last := lastSendMsg(f); last == nil || last.Text == settingsPermissionDenied {
			t.Errorf("addwl denied to standard admin: %+v", last)
		}
		f.Sends, f.Answers = nil, nil
		b.Process(ctx, callbackUpdate(77, "rmwl:nosuchvalue"))
		if last := lastSendMsg(f); last != nil && last.Text == settingsPermissionDenied {
			t.Errorf("rmwl denied to standard admin: %+v", last)
		}
	})

	// H1 fix: adm:custlookup (customers_view) is now gated by admincap.go's
	// central chokepoint (command.go's handleCallback, BEFORE the ack) —
	// this is the exact live privilege-escalation path the independent
	// review found (a granted-but-not-super admin could read any customer's
	// wallet balance, full order history, and owned-account usernames).
	// Denial is a pre-ack ALERT, not a chat message (wantAdminCapDenied's own
	// doc explains why), so this can no longer share the "CAN reach"
	// subtest's lastSendMsg-based assertion style.
	t.Run("standard admin CANNOT reach customers_view actions", func(t *testing.T) {
		f.Sends, f.Answers = nil, nil
		b.Process(ctx, callbackUpdate(77, "adm:custlookup"))
		wantAdminCapDenied(t, f, "adm:custlookup")
		// The lookup capture must never have armed either — belt-and-braces
		// against the gate being bypassed by a stray text message.
		st := b.states.Get(77)
		st.mu.Lock()
		armed := st.CustLookup
		st.mu.Unlock()
		if armed {
			t.Error("CustLookup should never arm for a standard admin lacking customers_view")
		}
	})

	// H1 fix: these were ALREADY denied before this task (fullAdminOnly's
	// chat-message refusal), but the central chokepoint now intercepts them
	// even earlier — pre-ack, via alert — so the assertion style changes
	// from lastSendMsg to wantAdminCapDenied even though the SECURITY
	// outcome (denied) is unchanged. This is Python's real, single-chokepoint
	// behavior — handle_callback's gate (telegram_bot.py
	// `_is_admin_cb`/`_cb_allowed_for` call site, ≈:12496-12497 @3069329 —
	// cite by symbol, not just line; see admincap.go's port-shape doc for
	// why): every one of these callbacks is absent from _std_admin_cb_cap,
	// so the alert fires before Python's own per-handler is_super_admin
	// checks would ever run either.
	t.Run("standard admin CANNOT reach full-admin-only actions", func(t *testing.T) {
		full := []string{"adm:addbal", "adm:listadmins", "adm:addadmin", "adm:rmadmin", "adm:addreseller"}
		for _, cb := range full {
			f.Sends, f.Answers = nil, nil
			b.Process(ctx, callbackUpdate(77, cb))
			wantAdminCapDenied(t, f, cb)
		}
		// Standalone routes outside the adm: prefix.
		standalone := []string{"resmng:x", "resdisc:x", "rmreseller:x"}
		for _, cb := range standalone {
			f.Sends, f.Answers = nil, nil
			b.Process(ctx, callbackUpdate(77, cb))
			wantAdminCapDenied(t, f, cb)
		}
		// Admin (the shared addbal/broadcast wizard slot) must never have
		// gotten armed.
		st := b.states.Get(77)
		st.mu.Lock()
		armed := st.Admin
		st.mu.Unlock()
		if armed != nil {
			t.Error("Admin (addbal) should never arm for a standard admin")
		}
	})

	t.Run("a plain non-admin gets silence on the standalone routes", func(t *testing.T) {
		for _, cb := range []string{"resmng:x", "resdisc:x", "rmreseller:x", "rmwl:x", "admdo:confirm", "admdo:cancel"} {
			f.Sends = nil
			b.Process(ctx, callbackUpdate(999, cb))
			if last := lastSendMsg(f); last != nil {
				t.Errorf("%s: non-admin got a reply: %+v", cb, last)
			}
		}
	})

	t.Run("resellers category is fully blocked for a standard admin", func(t *testing.T) {
		f.Sends = nil
		b.Process(ctx, callbackUpdate(77, "admcat:resellers"))
		if last := lastSendMsg(f); last != nil {
			t.Errorf("admcat:resellers should be silent for a standard admin, got %+v", last)
		}
	})
}

// ============================================================================
// admins.json boot-load (M6 fix)
// ============================================================================
//
// This is the entire point of dispatch.go's `admins: loadAdminsFile()` wiring
// (H1's own trigger: until that line landed, b.admins was ALWAYS an empty
// map, so the whole standard-admin tier — and every gate this task audits —
// was unreachable). It was COMPLETELY UNTESTED: a stub loadAdminsFile
// returning an empty map would have passed every test that existed before
// this fix, since every other test builds its "standard admin" fixtures by
// poking b.admins directly (b.adminsMu.Lock(); b.admins["77"] = true; ...),
// never by writing a real admins.json and calling New().

// withRedirectedAdminsFile points the package-level adminsFilePath at `path`
// for the duration of one test — like withAdminsFile, but for a CALLER-
// CHOSEN path (a pre-written file, or a path inside an unwritable directory)
// rather than a fresh empty temp dir.
func withRedirectedAdminsFile(t *testing.T, path string) {
	t.Helper()
	old := adminsFilePath
	adminsFilePath = path
	t.Cleanup(func() { adminsFilePath = old })
}

func TestAdminsFileBootLoad(t *testing.T) {
	t.Run("a valid grant loads and isAdmin sees it after a restart", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admins.json")
		if err := os.WriteFile(path, []byte(`["555"]`), 0o600); err != nil {
			t.Fatal(err)
		}
		withRedirectedAdminsFile(t, path)

		b := New(Deps{TG: &fakeTG{}, AdminID: "1"})
		t.Cleanup(b.Shutdown)
		if !b.isAdmin(555) {
			t.Error("New() should have loaded 555 as a standard admin from admins.json")
		}
		if b.isSuperAdmin(555) {
			t.Error("555 should be a STANDARD admin (granted), not super")
		}
	})

	t.Run("numeric entries load too (L12 fix — Python's str(x) parity)", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admins.json")
		if err := os.WriteFile(path, []byte(`[111, 222]`), 0o600); err != nil {
			t.Fatal(err)
		}
		withRedirectedAdminsFile(t, path)

		b := New(Deps{TG: &fakeTG{}, AdminID: "1"})
		t.Cleanup(b.Shutdown)
		if !b.isAdmin(111) || !b.isAdmin(222) {
			t.Error("bare-number admins.json entries should grant admin, same as Python's str(x)")
		}
	})

	t.Run("malformed JSON grants nothing and does not crash New()", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admins.json")
		if err := os.WriteFile(path, []byte(`not json at all`), 0o600); err != nil {
			t.Fatal(err)
		}
		withRedirectedAdminsFile(t, path)

		b := New(Deps{TG: &fakeTG{}, AdminID: "1"})
		t.Cleanup(b.Shutdown)
		if b.isAdmin(555) {
			t.Error("malformed admins.json must grant nothing")
		}
	})

	t.Run("empty array grants nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admins.json")
		if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
			t.Fatal(err)
		}
		withRedirectedAdminsFile(t, path)

		b := New(Deps{TG: &fakeTG{}, AdminID: "1"})
		t.Cleanup(b.Shutdown)
		if b.isAdmin(555) {
			t.Error("empty admins.json must grant nothing")
		}
	})

	t.Run("missing file grants nothing", func(t *testing.T) {
		withRedirectedAdminsFile(t, filepath.Join(t.TempDir(), "does-not-exist.json"))

		b := New(Deps{TG: &fakeTG{}, AdminID: "1"})
		t.Cleanup(b.Shutdown)
		if b.isAdmin(555) {
			t.Error("a missing admins.json must grant nothing")
		}
	})

	// M6(c): a failed persist must never report success (the recurring
	// defect this task calls out across three consecutive slices). Pointing
	// adminsFilePath at an unwritable directory forces saveAdminsFile's
	// os.WriteFile(tmp, ...) to fail, so addAdmin must roll the in-memory
	// grant back and capAddAdmin must tell the admin the write didn't land.
	t.Run("unwritable admins dir: grant reports save failure and isAdmin stays false", func(t *testing.T) {
		roDir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(roDir, 0o500); err != nil { // read+execute, no write
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) }) // let TempDir cleanup remove it
		withRedirectedAdminsFile(t, filepath.Join(roDir, "admins.json"))

		f := &fakeTG{}
		b := New(Deps{TG: f, AdminID: "1"})
		t.Cleanup(b.Shutdown)

		ctx := context.Background()
		b.Process(ctx, callbackUpdate(1, "adm:addadmin"))
		b.Process(ctx, textUpdate(1, "555"))
		last := lastSendMsg(f)
		if last == nil || last.Text != settingsSaveFailed {
			t.Fatalf("grant reply = %+v, want %q", last, settingsSaveFailed)
		}
		if b.isAdmin(555) {
			t.Error("a failed persist must never leave the in-memory grant live")
		}
	})
}
