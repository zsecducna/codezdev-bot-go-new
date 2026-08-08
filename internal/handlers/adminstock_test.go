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
	"codezdev-bot/internal/store"
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
