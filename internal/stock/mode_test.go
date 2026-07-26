// Tests pinning the on-disk permissions of the stock tree.
//
// Security warning, normal prose: these files hold plaintext account
// passwords, portal start URLs, TOTP seeds and OAuth refresh tokens for the
// shop's entire inventory, sold and unsold. A regression that puts any of them
// back at 0644 hands every local user on the host the whole inventory. These
// tests exist so such a regression is a red build rather than a quiet one —
// the same role archive_test.go plays for internal/provision.
package stock

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// assertOwnerOnlyTree walks `root` and fails on ANY entry readable by group or
// other. Written as a whole-tree sweep rather than a list of named paths on
// purpose: a per-path assertion only covers the writers someone remembered to
// enumerate, and the finding this guards against was precisely a writer nobody
// re-audited. Anything a future code path creates under these roots is checked
// automatically.
func assertOwnerOnlyTree(t *testing.T, root string) {
	t.Helper()
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		seen++
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s: mode %v is readable by group/other; credential material must be owner-only", rel, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if seen == 0 {
		t.Fatalf("walk %s visited nothing — the test asserted on an empty tree", root)
	}
}

// TestCredentialModesFullLifecycle drives a product through every state
// transition that writes to disk — intake (password + JSON), product metadata,
// the staging store area, hold, reserve and finalize — and then asserts that
// nothing anywhere under accounts/ or orders/ is group- or world-readable.
func TestCredentialModesFullLifecycle(t *testing.T) {
	root := t.TempDir()
	s := New(filepath.Join(root, "accounts"), filepath.Join(root, "orders"))

	// Password intake -> active/<user>.txt (the "username|password|start_url|
	// mfa_secret" file class).
	if res, err := s.AddAccounts("p1", []CredPair{
		{Username: "alice", Password: "pw-alice"},
		{Username: "bob", Password: "pw-bob"},
		{Username: "carol", Password: "pw-carol"},
	}); err != nil || res.Added != 3 {
		t.Fatalf("AddAccounts: added=%d err=%v", res.Added, err)
	}

	// JSON intake -> active/<email>.txt holding an OAuth refresh token.
	raw := json.RawMessage(`{"email":"dave@example.com","refresh_token":"rt-secret"}`)
	if res, err := s.AddJSONAccounts("p2", []JSONAccount{{
		Obj: map[string]any{"email": "dave@example.com", "refresh_token": "rt-secret"},
		Raw: raw,
	}}); err != nil || res.Added != 1 {
		t.Fatalf("AddJSONAccounts: added=%d err=%v", res.Added, err)
	}

	// Product metadata writers (type marker, notice, delivery note, config
	// file) — these share the tree, so they share the mode.
	if err := s.SetAccountType("p1", "password"); err != nil {
		t.Fatalf("SetAccountType: %v", err)
	}
	if err := s.SetProductNotice("p1", "notice"); err != nil {
		t.Fatalf("SetProductNotice: %v", err)
	}
	if err := s.SetProductDelivery("p1", "delivery"); err != nil {
		t.Fatalf("SetProductDelivery: %v", err)
	}
	if err := s.WriteTextFile("p1", "config.txt", "START_URL=https://example.test"); err != nil {
		t.Fatalf("WriteTextFile: %v", err)
	}

	// Staging area (store/<sub>/<user>.txt = "email|password").
	if n, err := s.AddStoreAccounts("p1", "sub1", []StoreEntry{
		{Username: "erin", Email: "erin@example.com", Password: "pw-erin"},
	}); err != nil || n != 1 {
		t.Fatalf("AddStoreAccounts: n=%d err=%v", n, err)
	}

	// held/ (off-sale) — its own MkdirAll site.
	if n, err := s.Hold("p1", 1); err != nil || n != 1 {
		t.Fatalf("Hold: n=%d err=%v", n, err)
	}

	// reserved/<oid>/ + .expiry, then sold/ + the orders/<oid>.txt delivery
	// record (which renders the passwords in cleartext).
	if users, err := s.Reserve("p1", 2, "ORD1", 15*time.Minute); err != nil || len(users) != 2 {
		t.Fatalf("Reserve: users=%v err=%v", users, err)
	}
	// Assert the reserved state's modes BEFORE finalize consumes the folder.
	assertOwnerOnlyTree(t, s.AccountsDir)
	if d, err := s.Finalize("p1", "ORD1", "7 days", "vi"); err != nil || len(d) != 2 {
		t.Fatalf("Finalize: delivered=%d err=%v", len(d), err)
	}

	assertOwnerOnlyTree(t, s.AccountsDir)
	assertOwnerOnlyTree(t, s.OrdersDir)
}

// TestOrderJSONFilesOwnerOnly covers the per-account JSON delivery files,
// which live in their own orders/<oid>/ subdirectory created by a separate
// MkdirAll and hold a raw OAuth refresh token.
func TestOrderJSONFilesOwnerOnly(t *testing.T) {
	root := t.TempDir()
	s := New(filepath.Join(root, "accounts"), filepath.Join(root, "orders"))

	obj := map[string]any{"email": "frank@example.com", "refresh_token": "rt-secret"}
	if res, err := s.AddJSONAccounts("pj", []JSONAccount{{
		Obj: obj,
		Raw: json.RawMessage(`{"email":"frank@example.com","refresh_token":"rt-secret"}`),
	}}); err != nil || res.Added != 1 {
		t.Fatalf("AddJSONAccounts: added=%d err=%v", res.Added, err)
	}
	if _, err := s.Reserve("pj", 1, "ORDJ", time.Minute); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := s.Finalize("pj", "ORDJ", "7 days", "vi"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// The JSON order writes a subdirectory of per-account files; there must be
	// more than just the detail .txt, or this test is asserting nothing.
	if files := s.OrderFiles("ORDJ"); len(files) < 2 {
		t.Fatalf("OrderFiles = %v, want the detail txt plus at least one JSON", files)
	}
	assertOwnerOnlyTree(t, s.OrdersDir)
}

// TestWriteFileAtomicIgnoresStaleTmpMode pins the reason writeFileAtomic
// removes and re-chmods its temp file: os.WriteFile applies its perm argument
// only when it CREATES the file, so a 0644 .tmp left behind by an earlier
// crash would otherwise keep that mode and the rename would carry it straight
// onto the live credential file.
func TestWriteFileAtomicIgnoresStaleTmpMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred.txt")
	// Plant the stale, world-readable temp file the crash would have left.
	if err := os.WriteFile(path+".tmp", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".tmp", 0o644); err != nil { // defeat any umask
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, "user|password\n", credentialFileMode); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != credentialFileMode {
		t.Errorf("mode = %v, want %v — a stale tmp must not leak its mode onto the credential",
			fi.Mode().Perm(), credentialFileMode)
	}
	if b, _ := os.ReadFile(path); string(b) != "user|password\n" {
		t.Errorf("content = %q, want the freshly written body", string(b))
	}
}

// TestModeConstants states the intended values outright, so a change to them
// is a deliberate edit to a test rather than a silent widening.
func TestModeConstants(t *testing.T) {
	if credentialFileMode != 0o600 {
		t.Errorf("credentialFileMode = %v, want 0600", credentialFileMode)
	}
	if credentialDirMode != 0o700 {
		t.Errorf("credentialDirMode = %v, want 0700", credentialDirMode)
	}
}
