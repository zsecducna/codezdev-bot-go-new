// Tests for the folder-state stock machine port. Every test runs on its own
// t.TempDir() tree, mirroring the on-disk layout the Python bot leaves behind
// (accounts.py:2-23), so byte/layout compatibility is exercised directly.
package stock

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore builds a Store over a fresh temp tree and seeds one product
// with `users` password accounts via AddAccounts (the canonical writer).
func newTestStore(t *testing.T, product string, users ...string) *Store {
	t.Helper()
	root := t.TempDir()
	s := New(filepath.Join(root, "accounts"), filepath.Join(root, "orders"))
	if len(users) > 0 {
		pairs := make([]CredPair, 0, len(users))
		for _, u := range users {
			pairs = append(pairs, CredPair{Username: u, Password: "pw-" + u})
		}
		res, err := s.AddAccounts(product, pairs)
		if err != nil || res.Added != len(users) {
			t.Fatalf("seed AddAccounts: added=%d err=%v", res.Added, err)
		}
	}
	return s
}

// TestReserveFinalize — reserve moves files active/ -> reserved/<oid> with a
// .expiry marker; finalize moves them to sold/, returns the parsed creds, and
// writes the orders/<oid>.txt delivery record.
func TestReserveFinalize(t *testing.T) {
	s := newTestStore(t, "p1", "alice", "bob", "carol")
	users, err := s.Reserve("p1", 2, "ORD1", 15*time.Minute)
	if err != nil || len(users) != 2 {
		t.Fatalf("Reserve: users=%v err=%v", users, err)
	}
	// Files must be physically in reserved/ORD1 with the expiry marker.
	odir := filepath.Join(s.AccountsDir, "p1", "reserved", "ORD1")
	if got := len(listTxt(odir)); got != 2 {
		t.Fatalf("reserved dir txt count = %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(odir, ".expiry")); err != nil {
		t.Fatalf(".expiry marker missing: %v", err)
	}
	if n, _ := s.CountAvailable("p1"); n != 1 {
		t.Fatalf("CountAvailable after reserve = %d, want 1", n)
	}
	// Finalize: reserved -> sold, creds parsed from the file bodies.
	delivered, err := s.Finalize("p1", "ORD1", "7 days", "vi")
	if err != nil || len(delivered) != 2 {
		t.Fatalf("Finalize: delivered=%v err=%v", delivered, err)
	}
	for _, d := range delivered {
		if d.Kind != KindPassword || d.Password != "pw-"+d.Username {
			t.Fatalf("bad delivered cred: %+v", d)
		}
	}
	if s.CountSold("p1") != 2 {
		t.Fatalf("CountSold = %d, want 2", s.CountSold("p1"))
	}
	if _, err := os.Stat(odir); !os.IsNotExist(err) {
		t.Fatalf("reserved order dir should be removed after finalize")
	}
	// Delivery record: header lines + per-account blocks + warranty.
	body, err := os.ReadFile(filepath.Join(s.OrdersDir, "ORD1.txt"))
	if err != nil {
		t.Fatalf("order file missing: %v", err)
	}
	txt := string(body)
	for _, want := range []string{"Order ID: ORD1", "Warranty: 7 days",
		"Account list:", "[account 1]", "(GMT +7)"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("order file missing %q in:\n%s", want, txt)
		}
	}
	// OrderUsernames reads the identifiers back for the ownership gate.
	got := s.OrderUsernames("ORD1")
	if len(got) != 2 {
		t.Fatalf("OrderUsernames = %v, want 2 entries", got)
	}
	for _, u := range got {
		if u != users[0] && u != users[1] {
			t.Fatalf("OrderUsernames unexpected %q (reserved %v)", u, users)
		}
	}
	// OrderFiles: detail txt only for a password order.
	if files := s.OrderFiles("ORD1"); len(files) != 1 {
		t.Fatalf("OrderFiles = %v, want 1 path", files)
	}
}

// TestReserveExpirySweep — a reservation past its .expiry is swept back to
// active/ (lazy reclaim), returning the stock and deleting the order folder.
func TestReserveExpirySweep(t *testing.T) {
	s := newTestStore(t, "p1", "u1", "u2")
	if users, err := s.Reserve("p1", 2, "ORDX", time.Minute); err != nil || len(users) != 2 {
		t.Fatalf("Reserve: %v %v", users, err)
	}
	// Backdate the marker so the reservation is already expired
	// (accounts.py:282-287: past .expiry -> reclaim).
	odir := filepath.Join(s.AccountsDir, "p1", "reserved", "ORDX")
	if err := os.WriteFile(filepath.Join(odir, ".expiry"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CountAvailable sweeps lazily and must see both accounts again.
	if n, _ := s.CountAvailable("p1"); n != 2 {
		t.Fatalf("CountAvailable after sweep = %d, want 2", n)
	}
	if _, err := os.Stat(odir); !os.IsNotExist(err) {
		t.Fatalf("expired order dir should be removed")
	}
	// A finalize on the swept order finds nothing (reservation lost).
	if d, err := s.Finalize("p1", "ORDX", "", "en"); err != nil || d != nil {
		t.Fatalf("Finalize after sweep = %v %v, want nil,nil", d, err)
	}
}

// TestConcurrentReserveLastItem — N goroutines race to reserve the LAST
// account; exactly one may win (all-or-nothing under the lock pair).
func TestConcurrentReserveLastItem(t *testing.T) {
	s := newTestStore(t, "p1", "only")
	const racers = 16
	var wg sync.WaitGroup
	wins := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(oid string) {
			defer wg.Done()
			users, err := s.Reserve("p1", 1, oid, time.Minute)
			if err != nil {
				t.Errorf("Reserve error: %v", err)
			}
			if len(users) == 1 {
				wins <- oid
			}
		}(filepath.Join("O" + string(rune('A'+i))))
	}
	wg.Wait()
	close(wins)
	var winners []string
	for w := range wins {
		winners = append(winners, w)
	}
	if len(winners) != 1 {
		t.Fatalf("want exactly 1 winner, got %d: %v", len(winners), winners)
	}
	if n, _ := s.CountAvailable("p1"); n != 0 {
		t.Fatalf("CountAvailable = %d, want 0", n)
	}
}

// TestHoldRestoreRoundTrip — hold pulls stock off sale into held/, restore
// puts it back; counts must round-trip exactly.
func TestHoldRestoreRoundTrip(t *testing.T) {
	s := newTestStore(t, "p1", "a", "b", "c")
	if n, err := s.Hold("p1", 2); err != nil || n != 2 {
		t.Fatalf("Hold = %d %v, want 2", n, err)
	}
	if s.CountHeld("p1") != 2 {
		t.Fatalf("CountHeld = %d, want 2", s.CountHeld("p1"))
	}
	if n, _ := s.CountAvailable("p1"); n != 1 {
		t.Fatalf("CountAvailable = %d, want 1", n)
	}
	// Restore more than held: capped at what's actually there.
	if n, err := s.Restore("p1", 5); err != nil || n != 2 {
		t.Fatalf("Restore = %d %v, want 2", n, err)
	}
	if n, _ := s.CountAvailable("p1"); n != 3 || s.CountHeld("p1") != 0 {
		t.Fatalf("after restore: avail=%d held=%d, want 3/0", n, s.CountHeld("p1"))
	}
}

// TestRestoreSold — admin force-refund undo: sold stems move back to active/;
// unknown / path-crafted identifiers land in unmatched.
func TestRestoreSold(t *testing.T) {
	s := newTestStore(t, "p1", "x1", "x2")
	if _, err := s.Reserve("p1", 2, "OS", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finalize("p1", "OS", "", "en"); err != nil {
		t.Fatal(err)
	}
	restored, unmatched, err := s.RestoreSold("p1", []string{"x1", "ghost", "../evil"})
	if err != nil || restored != 1 || len(unmatched) != 2 {
		t.Fatalf("RestoreSold = %d %v %v", restored, unmatched, err)
	}
	if n, _ := s.CountAvailable("p1"); n != 1 || s.CountSold("p1") != 1 {
		t.Fatalf("after restore_sold: avail=%d sold=%d, want 1/1", n, s.CountSold("p1"))
	}
}

// TestMetaFiles — type / notice / delivery / read-write text round-trips,
// including the clear-removes-file behavior.
func TestMetaFiles(t *testing.T) {
	s := newTestStore(t, "p1", "seed")
	// Default with no type file is "password" (accounts.py:156-158).
	if got := s.GetAccountType("p1"); got != "password" {
		t.Fatalf("default type = %q", got)
	}
	if err := s.SetAccountType("p1", "kiroapi"); err != nil {
		t.Fatal(err)
	}
	if got := s.GetAccountType("p1"); got != "kiroapi" {
		t.Fatalf("type = %q, want kiroapi", got)
	}
	// Garbage normalizes to password on write.
	if err := s.SetAccountType("p1", "weird"); err != nil {
		t.Fatal(err)
	}
	if got := s.GetAccountType("p1"); got != "password" {
		t.Fatalf("type = %q, want password", got)
	}
	// Notice set/get/clear.
	if err := s.SetProductNotice("p1", "  hello buyer  "); err != nil {
		t.Fatal(err)
	}
	if got := s.ProductNotice("p1"); got != "hello buyer" {
		t.Fatalf("notice = %q", got)
	}
	if err := s.SetProductNotice("p1", "   "); err != nil {
		t.Fatal(err)
	}
	if got := s.ProductNotice("p1"); got != "" {
		t.Fatalf("cleared notice = %q", got)
	}
	// Delivery note mirror.
	if err := s.SetProductDelivery("p1", "read the manual"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProductDelivery("p1"); got != "read the manual" {
		t.Fatalf("delivery = %q", got)
	}
	// Raw text file editor surface (config, .env).
	if err := s.WriteTextFile("p1", "config", "START_URL=\"https://sso\"\nREGION=us-east-1\n"); err != nil {
		t.Fatal(err)
	}
	if got := s.ReadTextFile("p1", "config"); !strings.Contains(got, "START_URL") {
		t.Fatalf("ReadTextFile = %q", got)
	}
	// readConfig strips the quotes.
	if cfg := s.readConfig("p1"); cfg["START_URL"] != "https://sso" || cfg["REGION"] != "us-east-1" {
		t.Fatalf("readConfig = %v", cfg)
	}
}

// TestUpstockFromStore — the store/ 'email|password' promotion logic
// (accounts.py:591-631): promote into active/ as username:/password: files,
// consume promoted + known files, keep no-email files, remove emptied folder.
func TestUpstockFromStore(t *testing.T) {
	s := newTestStore(t, "p1", "taken@x.com") // taken@x.com already in active/
	n, err := s.AddStoreAccounts("p1", "KiroPro", []StoreEntry{
		{Username: "u1", Email: "new@x.com", Password: "s3cret"},
		{Username: "u2", Email: "taken@x.com", Password: "dup"},
	})
	if err != nil || n != 2 {
		t.Fatalf("AddStoreAccounts = %d %v", n, err)
	}
	// Raw subname must slug to the same folder ('KiroPro' -> 'kiropro').
	if subs := s.ListStoreSubs("p1"); len(subs) != 1 || subs[0].Name != "kiropro" || subs[0].Count != 2 {
		t.Fatalf("ListStoreSubs = %v", subs)
	}
	res, err := s.UpstockFromStore("p1", "KiroPro")
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.SkippedKnown != 1 || res.Total != 2 {
		t.Fatalf("UpstockFromStore = %+v", res)
	}
	// Promoted file carries the exact credential body format.
	body, err := os.ReadFile(filepath.Join(s.AccountsDir, "p1", "active", "new@x.com.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "username: new@x.com\npassword: s3cret\n" {
		t.Fatalf("promoted body = %q", body)
	}
	// Store folder consumed entirely (promoted + known both removed).
	if s.CountStore("p1") != 0 {
		t.Fatalf("CountStore = %d, want 0", s.CountStore("p1"))
	}
	if _, err := os.Stat(s.storeSubDir("p1", "KiroPro")); !os.IsNotExist(err) {
		t.Fatalf("emptied store sub-folder should be removed")
	}
}

// TestJSONAccountsFinalize — JSON stock: parse pasted objects, upstock, then
// finalize delivers the object as its own orders/<id>/<email>.json file with
// key order preserved from the source bytes.
func TestJSONAccountsFinalize(t *testing.T) {
	s := newTestStore(t, "pj")
	objs, bad := ParseJSONAccounts(`{"email":"j@x.com","zeta":1,"alpha":2} {"nope":true}`)
	if bad != 0 || len(objs) != 2 {
		t.Fatalf("ParseJSONAccounts = %d objs, bad=%d", len(objs), bad)
	}
	res, err := s.AddJSONAccounts("pj", objs)
	if err != nil || res.Added != 1 || res.Bad != 1 { // second obj has no email
		t.Fatalf("AddJSONAccounts = %+v err=%v", res, err)
	}
	if users, err := s.Reserve("pj", 1, "OJ", time.Minute); err != nil || len(users) != 1 {
		t.Fatalf("Reserve json = %v %v", users, err)
	}
	d, err := s.Finalize("pj", "OJ", "", "en")
	if err != nil || len(d) != 1 || d[0].Kind != KindJSON || d[0].Email != "j@x.com" {
		t.Fatalf("Finalize json = %+v %v", d, err)
	}
	// Per-account .json file exists and preserves the source key order.
	jbody, err := os.ReadFile(filepath.Join(s.OrdersDir, "OJ", "j@x.com.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jbody), "\"zeta\"") ||
		strings.Index(string(jbody), "zeta") > strings.Index(string(jbody), "alpha") {
		t.Fatalf("json delivery lost key order:\n%s", jbody)
	}
	// OrderFiles: detail txt + one json, in send order.
	files := s.OrderFiles("OJ")
	if len(files) != 2 || !strings.HasSuffix(files[0], "OJ.txt") || !strings.HasSuffix(files[1], ".json") {
		t.Fatalf("OrderFiles = %v", files)
	}
}

// TestProductCRUD — create / rename / remove with the reserved-order guard.
func TestProductCRUD(t *testing.T) {
	s := newTestStore(t, "unused")
	if err := s.CreateProduct("Shop A"); err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	if err := s.CreateProduct("Shop A"); err == nil {
		t.Fatal("duplicate CreateProduct should fail")
	}
	if err := s.CreateProduct("../evil"); err == nil {
		t.Fatal("traversal name should be rejected")
	}
	// Seed + reserve to trip the in-flight guard.
	if _, err := s.AddAccounts("Shop A", []CredPair{{Username: "u", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("Shop A", 1, "OR", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameProduct("Shop A", "Shop B"); err == nil {
		t.Fatal("rename with reserved order should be refused")
	}
	if err := s.RemoveProduct("Shop A"); err == nil {
		t.Fatal("remove with reserved order should be refused")
	}
	// Release the order, then both ops succeed.
	if _, err := s.Release("Shop A", "OR"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameProduct("Shop A", "Shop B"); err != nil {
		t.Fatalf("RenameProduct: %v", err)
	}
	if !s.ProductExists("Shop B") || s.ProductExists("Shop A") {
		t.Fatal("rename did not move the folder")
	}
	if err := s.RemoveProduct("Shop B"); err != nil {
		t.Fatalf("RemoveProduct: %v", err)
	}
	if s.ProductExists("Shop B") {
		t.Fatal("remove left the folder behind")
	}
}
