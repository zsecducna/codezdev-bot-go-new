package handlers

// adminstock_test.go — tests for the 📦 STOCK category (adminstock.go).

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// TestProdRmRefusesWithLiveReservation is the sharper framing task #19's
// follow-up review gave cbProdRm: the Reserved count CountsSnapshot returns
// here is not a cosmetic number the way it is on a plain render — it is the
// ONLY guard standing between an admin tapping "Remove product" and a LIVE
// in-flight order's stock being deleted out from under it. Seeds a real
// reservation via stock.Reserve (the exact path a customer's checkout
// takes), drives the REAL cbProdRm callback, and asserts the removal is
// refused with the reserved count named — never reaching the
// delete-confirm screen, which would let a follow-up tap actually erase the
// product's folder while an order is still holding stock in it.
func TestProdRmRefusesWithLiveReservation(t *testing.T) {
	ctx := context.Background()
	b, f, _, stk, dir := newBuyBot(t, "1")
	seedProduct(t, dir, "prodA", 5000, 3)

	if _, err := stk.Reserve("prodA", 1, "ord1", time.Hour); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// showManageProducts assigns callback indices in listProducts() order;
	// with exactly one product seeded, index 0 is unambiguous.
	b.Process(ctx, callbackUpdate(1, "prodrm:0"))
	last := lastSendMsg(f)
	if last == nil {
		t.Fatal("prodrm produced no reply")
	}
	if !strings.Contains(last.Text, "1 reserved (in-flight)") {
		t.Errorf("prodrm with a live reservation = %q, want the reserved-count refusal naming it", last.Text)
	}
	if strings.Contains(last.Text, "DELETE") {
		t.Errorf("prodrm reached the delete-confirm screen despite a live reservation: %q", last.Text)
	}
}

// TestRunPoolProvisionUsesEnvOnlyLoginPython pins task #24's second
// b.loginPythonStr() migration (commit b3fa3a1) at ITS call site:
// runPoolProvision (adminstock.go:1929, provision.NewScriptRunner(
// b.loginPythonStr(), scriptDir())). The sibling call site
// (entraRunRefundDelete, adminorders.go) already has a pinning test —
// TestEntraRunRefundDeleteUsesEnvOnlyLoginPython in adminorders_test.go, whose
// shape this test copies verbatim, INCLUDING its self-check that
// KIRO_LOGIN_PYTHON is genuinely absent from the ambient process
// environment before proceeding, so this test can't rot into a tautology if
// a later CI config happens to export it. Without a test here, one of two
// identical migrations landed by the same commit is unpinned — exactly the
// gap that lets a later edit revert THIS site back to
// provision.LoginPythonFromEnv's raw os.Getenv (which never sees .env)
// unnoticed, while the entra test keeps passing.
//
// Ranking, kept honest (not overstated as a money/correctness bug): the
// failure mode here is LOUD, not silent — a python without the browser
// stack surfaces as runner.BrowserStackError's own refusal message
// (adminstock.go's "⛔ Browser-login stack unavailable" send), never a wrong
// action taken quietly. This test matters because it is the difference
// between "one of two identical migrations is untested" and "both are", not
// because the untested state was ever exploitable.
func TestRunPoolProvisionUsesEnvOnlyLoginPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	if _, ok := os.LookupEnv("KIRO_LOGIN_PYTHON"); ok {
		t.Fatal("fixture broken: KIRO_LOGIN_PYTHON is set in the ambient environment, so this test would pass even if the fix read raw env")
	}

	dir := t.TempDir()
	scriptDir := t.TempDir()
	// BrowserStackError (internal/provision) requires all three vendored
	// scripts to exist on disk before it ever probes the interpreter —
	// content is irrelevant, only presence is checked.
	for _, name := range []string{"kiro-web-login.py", "kiro-go-login.py", "kiro-login-helper.py"} {
		if err := os.WriteFile(filepath.Join(scriptDir, name), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The stub interpreter: a POSIX shell script standing in for the
	// browser-stack Python, at a path unique to this test run. Its exit
	// code (7, chosen to be unambiguous) drives BrowserStackError into its
	// non-zero-exit branch, which echoes the RESOLVED interpreter path
	// straight into the refusal text — that's the proof the .env value
	// reached provision.NewScriptRunner rather than a hardcoded "python3"
	// fallback.
	stubPython := filepath.Join(scriptDir, "stub-python")
	if err := os.WriteFile(stubPython, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KIRO_LOGIN_PYTHON="+stubPython+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// KIRO_SCRIPT_DIR is a separate, still-live os.Getenv read (scriptDir(),
	// otp.go) untouched by this fix — set it ambient, matching its own
	// resolution rule, not through .env (same split as
	// TestEntraRunRefundDeleteUsesEnvOnlyLoginPython).
	t.Setenv("KIRO_SCRIPT_DIR", scriptDir)

	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })

	// One AWS-IdC line (username|password|start_url, field 3 a URL) is
	// enough to make runPoolProvision see nIDC>0 and reach the
	// NewScriptRunner/BrowserStackError call this test pins — a keys-only
	// batch would skip that call entirely (provision.ScriptRunner.
	// BrowserStackError's own doc comment).
	b.runPoolProvision(context.Background(), 1, "user1|pass1|https://example.com/start")

	last := lastSendMsg(f)
	if last == nil || !strings.Contains(last.Text, stubPython) {
		t.Fatalf("expected the browser-stack refusal to name the .env-resolved interpreter %q; got %+v", stubPython, last)
	}
}

// TestCapRenProdWarnsWhenSettingsCarryFails pins task #28 (HIGH): renaming a
// SUSPENDED product must not silently put it back on sale. carryProductSettings
// (adminstock.go) moves the "suspended"/"private_products" settings.json
// entries from the old product name to the new one, but productSuspended and
// productPrivateAllowed (catalog.go:77-90) key off PRESENCE of the entry
// under the CURRENT product name — and by the time carryProductSettings
// runs, RenameProduct has already moved the real folder on disk to the new
// name. If the settings carry fails, the suspended entry is stranded under a
// name that no longer exists, and the freshly-renamed folder reads as
// unsuspended immediately (no restart, no delay). Before the fix, capRenProd
// swallowed that failure into a log line and told the admin the rename
// simply worked; this test proves the reply now warns instead.
//
// Fault injection reuses newReadOnlyCfgAuditBot's shape (a settings dir made
// read-only so every SaveSettings call fails) rather than that helper
// itself, because this path also needs a REAL writable stock folder for
// RenameProduct to succeed against — newReadOnlyCfgAuditBot's Deps carries
// no Stock at all, so RenameProduct would fail first and the settings-carry
// path this test targets would never run.
func TestCapRenProdWarnsWhenSettingsCarryFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.Mkdir(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	cfg, err := config.Load(cfgDir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })

	seedProduct(t, dir, "prodA", 5000, 1)
	if err := b.setProductSuspended("prodA", true, 1); err != nil {
		t.Fatalf("seed suspend: %v", err)
	}

	// Make the settings dir read-only AFTER the seed write above, so the
	// seed itself lands and only the carry triggered by the rename below
	// fails.
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o700) })

	// showManageProducts assigns callback indices in listProducts() order;
	// with exactly one product seeded, index 0 is unambiguous.
	b.Process(ctx, callbackUpdate(1, "prodren:0"))
	b.Process(ctx, textUpdate(1, "prodB"))

	// capRenProd's own reply is followed by an unconditional
	// showManageProducts send (adminstock.go), so lastSendMsg would grab the
	// product list instead — the rename reply is the second-to-last send.
	f.mu.Lock()
	sends := append([]tg.SendMessageParams(nil), f.Sends...)
	f.mu.Unlock()
	if len(sends) < 2 {
		t.Fatalf("expected at least 2 sends (rename reply + product list), got %d", len(sends))
	}
	renameReply := sends[len(sends)-2].Text
	if !strings.Contains(renameReply, "Renamed prodA → prodB") {
		t.Fatalf("expected the rename itself to still report success (RenameProduct did not fail), got %q", renameReply)
	}
	if !strings.Contains(renameReply, "did NOT carry over") {
		t.Errorf("rename reply = %q, want it to warn that suspend/private flags did not carry over to the new name", renameReply)
	}
}

// TestSendAuditLogCollapsesRepeatedActionTargetWithCount is the display-side
// regression pin for the audit-flood fix: sendAuditLog now reads via
// RecentAuditCollapsed instead of RecentAudit, so a stuck hands-free-deposit
// payment that logs the same (action, target) 50 times must render as ONE
// entry carrying a "×50" count — not 50 separate lines burying everything
// else on the admin's screen. An ordinary, never-repeated row seeded
// alongside it must render exactly as before, with no count suffix at all,
// proving the common case is untouched.
func TestSendAuditLogCollapsesRepeatedActionTargetWithCount(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newBuyBot(t, "1")

	for range 50 {
		st.LogAction("1", "admin", "watch_ambiguous", "tx-stuck", "still unresolved", "")
	}
	st.LogAction("1", "admin", "delete_account", "user123", "ok", "")

	b.sendAuditLog(ctx, 1, "")

	if got := f.sendCount(); got != 1 {
		t.Fatalf("sendCount = %d, want 1", got)
	}
	text := f.Sends[0].Text
	if !strings.Contains(text, "watch_ambiguous → tx-stuck") {
		t.Fatalf("audit text missing the collapsed entry, got %q", text)
	}
	if !strings.Contains(text, "watch_ambiguous → tx-stuck (×50)") {
		t.Errorf("audit text = %q, want the repeated entry to carry a (×50) count suffix", text)
	}
	if !strings.Contains(text, "delete_account → user123") {
		t.Fatalf("audit text missing the ordinary single-occurrence entry, got %q", text)
	}
	if strings.Contains(text, "delete_account → user123 (×") {
		t.Errorf("audit text = %q, an ordinary single-occurrence row must not get a count suffix", text)
	}
	// Exactly one "(×" in the whole message: the 50-times entry must not
	// have been split back into raw rows, each somehow tagged.
	if n := strings.Count(text, "(×"); n != 1 {
		t.Errorf("count-suffix occurrences = %d, want exactly 1 (one collapsed entry, not 50 raw rows)", n)
	}
}
