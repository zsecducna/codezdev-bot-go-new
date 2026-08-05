// Tests for the store.py port — the areas the architecture doc calls out
// (architecture-go.md §8): deposit idempotency, ClaimOrder race, migration on
// an OLD-schema DB, promo reserve bounds, api_keys double-insert guard.
package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestStore opens a fresh store.db in a temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestCreditIdempotent proves the deposit-idempotency invariant: a duplicate
// txn_id is rejected by the processed PRIMARY KEY (store.py:674-697) and the
// balance is credited exactly once.
func TestCreditIdempotent(t *testing.T) {
	s := newTestStore(t)
	ok, bal, err := s.Credit("123", 50000, "TXN-1")
	if err != nil || !ok || bal != 50000 {
		t.Fatalf("first credit: ok=%v bal=%d err=%v", ok, bal, err)
	}
	// Same txn replayed — must be rejected, balance unchanged.
	ok, bal, err = s.Credit("123", 50000, "TXN-1")
	if err != nil {
		t.Fatalf("dup credit err: %v", err)
	}
	if ok {
		t.Fatal("duplicate txn was credited — idempotency broken")
	}
	if bal != 50000 {
		t.Fatalf("balance changed on duplicate: %d", bal)
	}
	// Different txn credits again.
	ok, bal, err = s.Credit("123", 1000, "TXN-2")
	if err != nil || !ok || bal != 51000 {
		t.Fatalf("second txn: ok=%v bal=%d err=%v", ok, bal, err)
	}
	// MarkTxn shares the same PK gate: TXN-1 already consumed.
	if fresh, err := s.MarkTxn("TXN-1", "123", 1); err != nil || fresh {
		t.Fatalf("MarkTxn on consumed txn: fresh=%v err=%v", fresh, err)
	}
	// UnmarkTxn releases the claim so the id becomes usable again.
	if err := s.UnmarkTxn("TXN-2"); err != nil {
		t.Fatalf("UnmarkTxn: %v", err)
	}
	if exists, _ := s.TxnExists("TXN-2"); exists {
		t.Fatal("TXN-2 still marked after UnmarkTxn")
	}
}

// TestClaimOrderRace proves the double-delivery gate: two concurrent
// reserved->paid claims on one order, exactly ONE may win
// (store.py:255-270, architecture-go.md §5).
func TestClaimOrderRace(t *testing.T) {
	s := newTestStore(t)
	des := NewDes()
	if err := s.CreateOrder(des, "42", "en", "gpt", "t1", 1, 10000); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	var wg sync.WaitGroup
	wins := make(chan *Order, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := s.ClaimOrder(des, "reserved", "paid")
			if err != nil {
				t.Errorf("ClaimOrder err: %v", err)
				return
			}
			if o != nil {
				wins <- o
			}
		}()
	}
	wg.Wait()
	close(wins)
	var winners []*Order
	for o := range wins {
		winners = append(winners, o)
	}
	if len(winners) != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", len(winners))
	}
	if winners[0].Status != "paid" {
		t.Fatalf("winner status=%q", winners[0].Status)
	}
	// A stale re-claim from reserved must now fail.
	if o, _ := s.ClaimOrder(des, "reserved", "paid"); o != nil {
		t.Fatal("stale claim succeeded after transition")
	}
}

// TestMigrationOldSchema builds a store.db BY HAND with the OLD minimal schema
// (orders without the promo/entra columns, api_keys without
// refunded_ts/sale_class — the pre-migration shapes from store.py:68-71,
// 159-162 minus the later ALTERs), then opens it via Open and asserts the
// PRAGMA table_info column-add migrations ran (store.py:106-117, 164-177).
func TestMigrationOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	// OLD orders: no code/code_kind/referrer_chat/entra_deleted_ts.
	if _, err := raw.Exec("CREATE TABLE orders (" +
		"des TEXT PRIMARY KEY, chat_id TEXT, lang TEXT, product TEXT, " +
		"tier TEXT, qty INTEGER, total INTEGER, status TEXT, " +
		"paypal_order_id TEXT, ts REAL)"); err != nil {
		t.Fatalf("create old orders: %v", err)
	}
	// OLD api_keys: no refunded_ts, no sale_class.
	if _, err := raw.Exec("CREATE TABLE api_keys (" +
		"order_id TEXT PRIMARY KEY, chat_id INTEGER, key_id TEXT, " +
		"api_key TEXT, credits REAL, created_at INTEGER)"); err != nil {
		t.Fatalf("create old api_keys: %v", err)
	}
	// Seed a legacy row in each so migrations must preserve data.
	if _, err := raw.Exec("INSERT INTO orders(des, chat_id, status, qty, total, ts) " +
		"VALUES ('old1','7','delivered',1,5000,1.0)"); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := raw.Exec("INSERT INTO api_keys(order_id, chat_id, key_id, api_key, credits, created_at) " +
		"VALUES ('old1',7,'kid','sk-x',100,1)"); err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// Boot the port on the old DB — must not error and must add columns.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on old schema: %v", err)
	}
	defer s.Close()

	cols := func(table string) map[string]bool {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		have, err := tableColumns(tx, table)
		if err != nil {
			t.Fatalf("tableColumns(%s): %v", table, err)
		}
		return have
	}
	ordersCols := cols("orders")
	for _, c := range []string{"code", "code_kind", "referrer_chat", "entra_deleted_ts"} {
		if !ordersCols[c] {
			t.Errorf("orders missing migrated column %q", c)
		}
	}
	akCols := cols("api_keys")
	for _, c := range []string{"refunded_ts", "sale_class"} {
		if !akCols[c] {
			t.Errorf("api_keys missing migrated column %q", c)
		}
	}
	// Legacy rows survive and scan through the new column set.
	o, err := s.GetOrder("old1")
	if err != nil || o == nil {
		t.Fatalf("GetOrder legacy: %v %v", o, err)
	}
	if o.Total != 5000 || o.Code != nil || o.EntraDeletedTS != nil {
		t.Fatalf("legacy order mangled: %+v", o)
	}
	k, err := s.APIKeyForOrder("old1")
	if err != nil || k == nil {
		t.Fatalf("APIKeyForOrder legacy: %v %v", k, err)
	}
	// Migrated sale_class default is '' (legacy class, store.py:170-177).
	if k.SaleClass != "" || k.RefundedTS != nil {
		t.Fatalf("legacy api_key mangled: %+v", k)
	}
}

// TestPromoReserveBounds proves the atomic used_count<max_uses gate
// (store.py:1027-1050): a 2-use code yields exactly 2 reservations, release
// restores a slot, and used_count never goes negative.
func TestPromoReserveBounds(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreatePromo("save10", 10, 2, false); err != nil {
		t.Fatalf("CreatePromo: %v", err)
	}
	// Case-insensitive reserve (codes stored upper-case).
	p1, err := s.PromoReserveUse("SAVE10")
	if err != nil || p1 == nil || p1.UsedCount != 1 {
		t.Fatalf("reserve 1: %+v err=%v", p1, err)
	}
	p2, err := s.PromoReserveUse("save10")
	if err != nil || p2 == nil || p2.UsedCount != 2 {
		t.Fatalf("reserve 2: %+v err=%v", p2, err)
	}
	// Third reserve must fail — exhausted.
	if p3, err := s.PromoReserveUse("SAVE10"); err != nil || p3 != nil {
		t.Fatalf("reserve 3 should fail: %+v err=%v", p3, err)
	}
	// Release a slot -> reservable again.
	if err := s.PromoReleaseUse("SAVE10"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if p4, err := s.PromoReserveUse("SAVE10"); err != nil || p4 == nil {
		t.Fatalf("reserve after release: %+v err=%v", p4, err)
	}
	// Deactivated code must not reserve even with uses left.
	if err := s.PromoReleaseUse("SAVE10"); err != nil {
		t.Fatalf("release2: %v", err)
	}
	if err := s.SetPromoActive("SAVE10", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if p, err := s.PromoReserveUse("SAVE10"); err != nil || p != nil {
		t.Fatalf("inactive reserve should fail: %+v err=%v", p, err)
	}
	// MAX(0,...) floor: over-release never drives used_count negative.
	for i := 0; i < 5; i++ {
		if err := s.PromoReleaseUse("SAVE10"); err != nil {
			t.Fatalf("over-release: %v", err)
		}
	}
	p, err := s.GetPromo("save10")
	if err != nil || p == nil || p.UsedCount != 0 {
		t.Fatalf("used_count floor: %+v err=%v", p, err)
	}
}

// TestAPIKeyDoubleInsert proves the no-double-mint anchor: INSERT OR IGNORE on
// the api_keys order_id PK (store.py:314-332) — a settle replay re-delivers
// the STORED key instead of minting a second one.
func TestAPIKeyDoubleInsert(t *testing.T) {
	s := newTestStore(t)
	ins, err := s.AddAPIKey("ord1", 99, "kid-1", "sk-first", 500, "bedrock")
	if err != nil || !ins {
		t.Fatalf("first mint: ins=%v err=%v", ins, err)
	}
	// Replayed settle tries to mint a different key for the same order —
	// must be IGNOREd, first key stays.
	ins, err = s.AddAPIKey("ord1", 99, "kid-2", "sk-second", 500, "bedrock")
	if err != nil {
		t.Fatalf("replay err: %v", err)
	}
	if ins {
		t.Fatal("replayed mint inserted a second key")
	}
	k, err := s.APIKeyForOrder("ord1")
	if err != nil || k == nil {
		t.Fatalf("APIKeyForOrder: %v %v", k, err)
	}
	if k.APIKey != "sk-first" || k.KeyID != "kid-1" {
		t.Fatalf("stored key clobbered: %+v", k)
	}
	// Refund mark excludes the key from the live count.
	if n, _ := s.CountAPIKeysForChat(99); n != 1 {
		t.Fatalf("live count=%d", n)
	}
	if err := s.MarkAPIKeyRefunded("ord1"); err != nil {
		t.Fatalf("MarkAPIKeyRefunded: %v", err)
	}
	if n, _ := s.CountAPIKeysForChat(99); n != 0 {
		t.Fatalf("live count after refund=%d", n)
	}
}

// TestNewDesFormat checks the order-id format contract: 32 lowercase hex
// chars, no hyphens, uuid4 version/variant bits (architecture-go.md §4.1).
func TestNewDesFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		des := NewDes()
		if len(des) != 32 {
			t.Fatalf("len=%d des=%q", len(des), des)
		}
		for _, ch := range des {
			if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
				t.Fatalf("non-hex char %q in %q", ch, des)
			}
		}
		if des[12] != '4' { // uuid4 version nibble at hex position 12
			t.Fatalf("version nibble=%c in %q", des[12], des)
		}
		if seen[des] {
			t.Fatalf("duplicate des %q", des)
		}
		seen[des] = true
	}
}

// TestDebitGuard: the WHERE amount>=? guard rejects overdrafts atomically
// (store.py:854-869).
func TestDebitGuard(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreditBalance("5", 100); err != nil {
		t.Fatalf("CreditBalance: %v", err)
	}
	ok, bal, err := s.Debit("5", 150)
	if err != nil || ok || bal != 100 {
		t.Fatalf("overdraft: ok=%v bal=%d err=%v", ok, bal, err)
	}
	ok, bal, err = s.Debit("5", 60)
	if err != nil || !ok || bal != 40 {
		t.Fatalf("debit: ok=%v bal=%d err=%v", ok, bal, err)
	}
}

// TestDebitConcurrent is M8's fix: Debit's overdraw refusal was never
// exercised concurrently before this — store.go:79's SetMaxOpenConns(1) on a
// SINGLE *Store already serializes every call through its own connection
// pool, so hammering ONE Store proves only the boundary (won == wantWon
// exactly), not atomicity: a naive SELECT-then-UPDATE would pass identically
// under that serialization, since nothing ever runs concurrently against it.
// TWO independent *Store handles (two real *sql.DB pools, both WAL +
// busy_timeout) opened on the SAME store.db file give genuine cross-
// connection contention on the same row — mirroring promo_test.go's
// TestPromoReserveUseConcurrent pattern — which is what Debit's atomic
// `UPDATE ... WHERE amount >= ?` guard (balance.go:141-166) actually has to
// survive. 30 concurrent 40-unit debits against a 1000-unit balance can let
// AT MOST 25 succeed; a broken (non-atomic) guard would let more through and
// drive the balance negative.
func TestDebitConcurrent(t *testing.T) {
	const startBalance = 1000
	const attempts = 30
	const debitEach = 40 // 30*40 = 1200 > 1000: not every attempt can win

	path := filepath.Join(t.TempDir(), "store.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	t.Cleanup(func() { s1.Close() })
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	if _, err := s1.CreditBalance("race-debit", startBalance); err != nil {
		t.Fatalf("CreditBalance: %v", err)
	}

	var wg sync.WaitGroup
	oks := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		s := s1
		if i%2 == 1 { // alternate attempts across the two independent pools
			s = s2
		}
		go func(s *Store) {
			defer wg.Done()
			ok, _, err := s.Debit("race-debit", debitEach)
			if err != nil {
				t.Errorf("Debit err: %v", err)
				return
			}
			oks <- ok
		}(s)
	}
	wg.Wait()
	close(oks)

	won := 0
	for ok := range oks {
		if ok {
			won++
		}
	}
	const wantWon = startBalance / debitEach // 25
	if won != wantWon {
		t.Fatalf("won %d debits, want exactly %d (overdraw guard failed)", won, wantWon)
	}

	final, err := s1.GetBalance("race-debit")
	if err != nil {
		t.Fatalf("GetBalance after race: %v", err)
	}
	if final < 0 {
		t.Fatalf("balance went NEGATIVE (%d) — overdraw guard failed", final)
	}
	if want := int64(startBalance - wantWon*debitEach); final != want {
		t.Errorf("final balance = %d, want %d", final, want)
	}
}

// TestClaimOrderRefundedConcurrent is the money-critical double-refund guard
// (internal/handlers' adminorders.go task brief: "a defect here pays a
// customer twice or not at all") under GENUINE cross-connection concurrency.
// TestClaimOrderRace above already proves the boundary (exactly 1 winner) but
// does so against a SINGLE *Store, whose SetMaxOpenConns(1) already
// serializes every call through its own connection pool — that test would
// pass identically even with a naive, non-atomic SELECT-then-UPDATE, since
// nothing ever runs concurrently against one pool. TWO independent *Store
// handles (two real *sql.DB pools, both WAL + busy_timeout=30000) opened on
// the SAME store.db file give genuine cross-connection contention on the
// same row — mirroring promo_test.go's TestPromoReserveUseConcurrent and this
// file's own TestDebitConcurrent — which is what ClaimOrder's atomic
// `UPDATE ... WHERE des=? AND status=?` guard (orders.go) actually has to
// survive. This exercises the delivered->refunded transition specifically
// (every refund entry point's shared chokepoint), not the reserved->paid one
// TestClaimOrderRace already covers.
func TestClaimOrderRefundedConcurrent(t *testing.T) {
	const attempts = 30

	path := filepath.Join(t.TempDir(), "store.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	t.Cleanup(func() { s1.Close() })
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	des := NewDes()
	if err := s1.CreateOrder(des, "42", "en", "gpt", "BH7D", 1, 10000); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if _, err := s1.ClaimOrder(des, "reserved", "delivered"); err != nil {
		t.Fatalf("seed delivered: %v", err)
	}

	var wg sync.WaitGroup
	wins := make(chan *Order, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		s := s1
		if i%2 == 1 { // alternate attempts across the two independent pools
			s = s2
		}
		go func(s *Store) {
			defer wg.Done()
			o, err := s.ClaimOrder(des, "delivered", "refunded")
			if err != nil {
				t.Errorf("ClaimOrder err: %v", err)
				return
			}
			if o != nil {
				wins <- o
			}
		}(s)
	}
	wg.Wait()
	close(wins)

	var winners []*Order
	for o := range wins {
		winners = append(winners, o)
	}
	if len(winners) != 1 {
		t.Fatalf("won %d refund claims, want exactly 1 (double-refund guard failed)", len(winners))
	}
	if winners[0].Status != "refunded" {
		t.Fatalf("winner status=%q, want refunded", winners[0].Status)
	}

	final, err := s1.GetOrder(des)
	if err != nil || final == nil {
		t.Fatalf("GetOrder after race: %+v err=%v", final, err)
	}
	if final.Status != "refunded" {
		t.Errorf("final status=%q, want refunded", final.Status)
	}
	// A stale re-claim from 'delivered' must now fail — the order already
	// moved on, so nothing may credit a second payout against it.
	if o, _ := s1.ClaimOrder(des, "delivered", "refunded"); o != nil {
		t.Fatal("stale claim from 'delivered' succeeded after the order already transitioned to 'refunded'")
	}
}
