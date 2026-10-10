// Tests for the store.py port — the areas the architecture doc calls out
// (architecture-go.md §8): deposit idempotency, ClaimOrder race, migration on
// an OLD-schema DB, promo reserve bounds, api_keys double-insert guard.
package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
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

// ---- micro-USDT ledger-unit boot guard -------------------------------------

// TestCheckMicroLedgerFreshDBSelfMarksMicro: a brand-new DB (no meta table, no
// money rows) is the safe-to-mark case — CheckMicroLedger writes the
// ledger_unit='micro_usdt' marker itself and boots (returns nil), so the bot
// never starts against a ledger whose unit it cannot name.
func TestCheckMicroLedgerFreshDBSelfMarksMicro(t *testing.T) {
	s := newTestStore(t)
	// initSchema created the money tables but NOT meta (only migrate-usdt or
	// EnsureMicroLedger writes meta), so this is a genuinely fresh ledger.
	if hasMeta, err := s.metaTableExists(); err != nil || hasMeta {
		t.Fatalf("fresh DB must have no meta table yet, got hasMeta=%v err=%v", hasMeta, err)
	}
	if err := s.CheckMicroLedger(); err != nil {
		t.Fatalf("CheckMicroLedger on a fresh empty DB = %v, want nil (self-mark + boot)", err)
	}
	if unit, ok := s.LedgerUnit(); !ok || unit != "micro_usdt" {
		t.Errorf("LedgerUnit after self-mark = (%q,%t), want (micro_usdt,true)", unit, ok)
	}
	// Idempotent: a second check must still pass (the marker is INSERT OR IGNORE).
	if err := s.CheckMicroLedger(); err != nil {
		t.Errorf("CheckMicroLedger re-run = %v, want nil (idempotent)", err)
	}
}

// TestCheckMicroLedgerRefusesUnmigratedVND: a DB that predates the flip holds
// money rows but no meta marker — booting against it would read VND figures as
// micro and mis-price everything ~40x. CheckMicroLedger must refuse with
// ErrUnmigratedVNDLedger rather than boot.
func TestCheckMicroLedgerRefusesUnmigratedVND(t *testing.T) {
	s := newTestStore(t)
	// Seed a money row the way an old VND build would have left one, with NO
	// migration marker written.
	if _, _, err := s.Credit("999", 2_500_000, "TXN-legacy"); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	if hasMoney, err := s.hasMoneyRows(); err != nil || !hasMoney {
		t.Fatalf("fixture: expected money rows present, got hasMoney=%v err=%v", hasMoney, err)
	}
	err := s.CheckMicroLedger()
	if !errors.Is(err, ErrUnmigratedVNDLedger) {
		t.Fatalf("CheckMicroLedger on an un-migrated DB = %v, want ErrUnmigratedVNDLedger (refuse to boot)", err)
	}
	// The marker must NOT have been written on the refusal path.
	if unit, ok := s.LedgerUnit(); ok {
		t.Errorf("LedgerUnit after refusal = (%q,true), want no marker written (ok=false)", unit)
	}
}

// TestCheckMicroLedgerWrongUnitRefuses: a meta marker that names a unit OTHER
// than micro_usdt must refuse, rather than silently booting against a ledger
// denominated in something else.
func TestCheckMicroLedgerWrongUnitRefuses(t *testing.T) {
	s := newTestStore(t)
	// Simulate a DB a different build marked with a different unit.
	if err := s.EnsureMicroLedger(); err != nil {
		t.Fatalf("EnsureMicroLedger: %v", err)
	}
	if _, err := s.db.Exec(
		"UPDATE meta SET value='vnd' WHERE key='ledger_unit'"); err != nil {
		t.Fatalf("rewrite ledger_unit: %v", err)
	}
	err := s.CheckMicroLedger()
	if err == nil {
		t.Fatal("CheckMicroLedger on a vnd-marked DB = nil, want a unit-mismatch error (refuse to boot)")
	}
}

// TestCheckMicroLedgerMetaPresentRowMissingRefusesOnMoney: the M1
// partial-restore window — meta table present but the ledger_unit ROW absent
// (a partial restore kept the table + dropped the row), AND money rows present.
// This must NOT self-mark micro (that would read the leftover VND rows as micro
// ~40x); it refuses like the no-meta un-migrated case. Locks the !ok branch so
// a future "simplification" can't drop the money-row check there.
func TestCheckMicroLedgerMetaPresentRowMissingRefusesOnMoney(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnsureMicroLedger(); err != nil {
		t.Fatalf("EnsureMicroLedger: %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM meta WHERE key='ledger_unit'"); err != nil {
		t.Fatalf("delete ledger_unit row: %v", err)
	}
	if _, _, err := s.Credit("999", 2_500_000, "TXN-legacy"); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	if err := s.CheckMicroLedger(); !errors.Is(err, ErrUnmigratedVNDLedger) {
		t.Fatalf("CheckMicroLedger (meta present, ledger_unit row missing, money rows) = %v, want ErrUnmigratedVNDLedger", err)
	}
	if _, ok := s.LedgerUnit(); ok {
		t.Errorf("LedgerUnit after refusal = ok=true, want no marker written")
	}
}

// TestEnsureMicroLedgerIsIdempotent: re-running EnsureMicroLedger must not
// clobber an existing marker (INSERT OR IGNORE), so a bot that boots, then a
// second boot, never flips the recorded unit.
func TestEnsureMicroLedgerIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnsureMicroLedger(); err != nil {
		t.Fatalf("first EnsureMicroLedger: %v", err)
	}
	// Sabotage attempt: pre-write a different value, then EnsureMicroLedger
	// must NOT overwrite it (the row's PK already exists).
	if _, err := s.db.Exec(
		"UPDATE meta SET value='vnd' WHERE key='ledger_unit'"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := s.EnsureMicroLedger(); err != nil {
		t.Fatalf("second EnsureMicroLedger: %v", err)
	}
	if unit, _ := s.LedgerUnit(); unit != "vnd" {
		t.Errorf("EnsureMicroLedger overwrote an existing marker to %q; INSERT OR IGNORE must leave 'vnd' in place", unit)
	}
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

// TestAdminCreditBalanceIdempotent mirrors TestCreditIdempotent above, but
// for the id-based (not txn-hash-based) dedupe gate AdminCreditBalance adds
// for admin-typed actions (the execAddBal money-safety-hazard fix).
func TestAdminCreditBalanceIdempotent(t *testing.T) {
	s := newTestStore(t)
	applied, bal, err := s.AdminCreditBalance("ADM-1", "1", "555", 2_500_000)
	if err != nil || !applied || bal != 2_500_000 {
		t.Fatalf("first admin credit: applied=%v bal=%d err=%v", applied, bal, err)
	}
	// Same id replayed — must be rejected, balance unchanged.
	applied, bal, err = s.AdminCreditBalance("ADM-1", "1", "555", 2_500_000)
	if err != nil {
		t.Fatalf("dup admin credit err: %v", err)
	}
	if applied {
		t.Fatal("duplicate admin_credits id was applied — idempotency broken")
	}
	if bal != 2_500_000 {
		t.Fatalf("balance changed on duplicate: %d", bal)
	}
	// A genuinely new admin action (a different id) credits again, even for
	// the SAME target/amount — a manual repeat is a real, intentional action,
	// not a replay (see execAddBal's doc comment for why that's correct).
	applied, bal, err = s.AdminCreditBalance("ADM-2", "1", "555", 2_500_000)
	if err != nil || !applied || bal != 5_000_000 {
		t.Fatalf("second admin action: applied=%v bal=%d err=%v", applied, bal, err)
	}
}

// TestAdminCreditBalanceRecordsLedgerRow proves the admin_credits row lands
// with the right actor/target/amount, and that resulting_balance gets
// backfilled correctly (it's written NULL-then-UPDATE within the same
// transaction, since the balance isn't known until after the upsert).
func TestAdminCreditBalanceRecordsLedgerRow(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.AdminCreditBalance("ADM-3", "99", "555", 1_000_000); err != nil {
		t.Fatalf("AdminCreditBalance: %v", err)
	}
	var actor, target string
	var amount, resultBal int64
	err := s.db.QueryRow(
		"SELECT actor_chat, target_chat, amount, resulting_balance FROM admin_credits WHERE id=?",
		"ADM-3").Scan(&actor, &target, &amount, &resultBal)
	if err != nil {
		t.Fatalf("query admin_credits: %v", err)
	}
	if actor != "99" || target != "555" || amount != 1_000_000 || resultBal != 1_000_000 {
		t.Errorf("row = actor=%q target=%q amount=%d resultBal=%d, want 99/555/1000000/1000000",
			actor, target, amount, resultBal)
	}
}

// TestAdminCreditBalanceConcurrent proves the id-based dedupe gate is
// genuinely atomic across independent connection pools, not just within one
// — the same two-*Store*-handle pattern as TestDebitConcurrent, because a
// single-pool test proves nothing here (this repo has been burned by that
// exact false-confidence shape before). Every attempt races the SAME id:
// exactly one must apply, no matter which handle wins.
func TestAdminCreditBalanceConcurrent(t *testing.T) {
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

	const attempts = 20
	var wg sync.WaitGroup
	appliedCh := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		s := s1
		if i%2 == 1 { // alternate attempts across the two independent pools
			s = s2
		}
		go func(s *Store) {
			defer wg.Done()
			applied, _, err := s.AdminCreditBalance("ADM-RACE", "1", "race-target", 1000)
			if err != nil {
				t.Errorf("AdminCreditBalance err: %v", err)
				return
			}
			appliedCh <- applied
		}(s)
	}
	wg.Wait()
	close(appliedCh)

	wins := 0
	for a := range appliedCh {
		if a {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("applied count = %d, want exactly 1 (idempotency gate must serialize the race)", wins)
	}
	bal, err := s1.GetBalance("race-target")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if bal != 1000 {
		t.Errorf("final balance = %d, want 1000 (credited exactly once, not %d times)", bal, attempts)
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
	// The llmgate-cutover columns (backend + pool preservation pair) must be
	// ALTERed on by the same boot, with no data touched.
	for _, c := range []string{"backend", "pool_key_id", "pool_api_key"} {
		if !akCols[c] {
			t.Errorf("api_keys missing cutover column %q", c)
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
	// Same for the cutover columns: a pre-cutover row must read as POOL with
	// no displaced pool originals — that DEFAULT '' classification is the
	// whole reason the backend marker's zero value is pool.
	if k.Backend != BackendPool || k.PoolKeyID != "" || k.PoolAPIKey != "" {
		t.Fatalf("legacy api_key cutover columns = %q/%q/%q, want pool/\"\"/\"\"",
			k.Backend, k.PoolKeyID, k.PoolAPIKey)
	}
	// And a legacy row is exactly what the migration tool must pick up.
	if n, err := s.CountAPIKeysToMigrate(0); err != nil || n != 1 {
		t.Fatalf("CountAPIKeysToMigrate on legacy DB = %d err=%v, want 1 (the legacy row is pool+bedrock-class)", n, err)
	}
	// The notify stamp column must land on old DBs by the same boot, so a
	// pre-notify-store db migrates in one Open (column-gated ALTER).
	if !akCols["migrated_notified_ts"] {
		t.Errorf("api_keys missing notify column migrated_notified_ts")
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
	ins, err := s.AddAPIKey("ord1", 99, "kid-1", "sk-first", 500, "bedrock", BackendPool)
	if err != nil || !ins {
		t.Fatalf("first mint: ins=%v err=%v", ins, err)
	}
	// Replayed settle tries to mint a different key for the same order —
	// must be IGNOREd, first key stays.
	ins, err = s.AddAPIKey("ord1", 99, "kid-2", "sk-second", 500, "bedrock", BackendPool)
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

// TestAPIKeyBackendColumnsRoundTrip pins the llmgate-cutover columns through
// the AddAPIKey → APIKeyForOrder round trip: the backend stamped at mint
// reads back verbatim (both markers), and the pool_* preservation columns
// start EMPTY (SQL NULL → "") — they are written only by
// MigrateAPIKeyToLlmgate. This is also the cheapest guard that apiKeyCols and
// scanAPIKey still agree: the SELECT lists columns explicitly and the scan
// reads them positionally, so a column added to one but not the other would
// silently shift every later field — this test would catch the backend value
// landing in the wrong slot.
func TestAPIKeyBackendColumnsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if ins, err := s.AddAPIKey("ord-pool", 1, "kid-pool", "sk-pool", 100, "bedrock", BackendPool); err != nil || !ins {
		t.Fatalf("pool mint: ins=%v err=%v", ins, err)
	}
	k, err := s.APIKeyForOrder("ord-pool")
	if err != nil || k == nil {
		t.Fatalf("APIKeyForOrder(ord-pool): %v %v", k, err)
	}
	if k.Backend != BackendPool || k.PoolKeyID != "" || k.PoolAPIKey != "" {
		t.Fatalf("pool row cutover columns = backend:%q poolKeyID:%q poolAPIKey:%q, want %q/\"\"/\"\"",
			k.Backend, k.PoolKeyID, k.PoolAPIKey, BackendPool)
	}
	if ins, err := s.AddAPIKey("ord-lg", 2, "kid-lg", "sk-lg", 200, "bedrock", BackendLlmgate); err != nil || !ins {
		t.Fatalf("llmgate mint: ins=%v err=%v", ins, err)
	}
	k, err = s.APIKeyForOrder("ord-lg")
	if err != nil || k == nil {
		t.Fatalf("APIKeyForOrder(ord-lg): %v %v", k, err)
	}
	if k.Backend != BackendLlmgate {
		t.Fatalf("llmgate row backend = %q, want %q", k.Backend, BackendLlmgate)
	}
	// Sanity on the marker values themselves: '' is pool BY DESIGN (it is
	// the column DEFAULT, classifying every pre-cutover row correctly with
	// no backfill), 'llmgate' is the only other backend today.
	if BackendPool != "" || BackendLlmgate != "llmgate" {
		t.Fatalf("backend consts drifted: pool=%q llmgate=%q", BackendPool, BackendLlmgate)
	}
}

// TestMigrateAPIKeyToLlmgate proves the per-key commit point: one UPDATE
// repoints the row at the llmgate key while PRESERVING the pool originals in
// pool_key_id/pool_api_key (what makes a later pool-side revoke possible),
// and the backend=” WHERE guard makes a replayed call a no-op (false, values
// untouched) instead of a double-apply.
func TestMigrateAPIKeyToLlmgate(t *testing.T) {
	s := newTestStore(t)
	// Seed a row exactly as the pre-cutover world wrote it: pool key, pool
	// backend, originals in key_id/api_key.
	if ins, err := s.AddAPIKey("ord-m", 7, "pool-kid-1", "sk-pool-1", 500, "bedrock", BackendPool); err != nil || !ins {
		t.Fatalf("seed pool row: ins=%v err=%v", ins, err)
	}
	moved, err := s.MigrateAPIKeyToLlmgate("ord-m", "pool-kid-1", "sk-pool-1", "lg-kid-9", "sk-lg-9", 650)
	if err != nil || !moved {
		t.Fatalf("first migrate: moved=%v err=%v", moved, err)
	}
	k, err := s.APIKeyForOrder("ord-m")
	if err != nil || k == nil {
		t.Fatalf("APIKeyForOrder after migrate: %v %v", k, err)
	}
	if k.Backend != BackendLlmgate || k.KeyID != "lg-kid-9" || k.APIKey != "sk-lg-9" || k.Credits != 650 {
		t.Fatalf("row not repointed at llmgate key: %+v", k)
	}
	if k.PoolKeyID != "pool-kid-1" || k.PoolAPIKey != "sk-pool-1" {
		t.Fatalf("pool originals not preserved: poolKeyID:%q poolAPIKey:%q", k.PoolKeyID, k.PoolAPIKey)
	}
	// Replay (tool re-run / concurrent worker): must report false AND leave
	// every stored value exactly as the first call left it — the losing
	// caller's llmgate key is orphaned server-side, never double-applied.
	moved, err = s.MigrateAPIKeyToLlmgate("ord-m", "pool-kid-1", "sk-pool-1", "lg-kid-OTHER", "sk-lg-OTHER", 1)
	if err != nil {
		t.Fatalf("replayed migrate err: %v", err)
	}
	if moved {
		t.Fatal("replayed migrate moved a row — backend='' guard is broken, second llmgate key applied over the first")
	}
	k, err = s.APIKeyForOrder("ord-m")
	if err != nil || k == nil {
		t.Fatalf("APIKeyForOrder after replay: %v %v", k, err)
	}
	if k.KeyID != "lg-kid-9" || k.APIKey != "sk-lg-9" || k.Credits != 650 ||
		k.Backend != BackendLlmgate || k.PoolKeyID != "pool-kid-1" || k.PoolAPIKey != "sk-pool-1" {
		t.Fatalf("replay mutated stored values: %+v", k)
	}
	// Unknown order: idempotent false, not an error (the tool may race a
	// hand-deleted row).
	if moved, err = s.MigrateAPIKeyToLlmgate("ord-nope", "a", "b", "c", "d", 1); err != nil || moved {
		t.Fatalf("migrate unknown order: moved=%v err=%v, want false/nil", moved, err)
	}
	// A REFUNDED row must NEVER be repointed (the refunded_ts IS NULL guard
	// in the WHERE): its revoke already killed — or tried to kill — the key
	// the row points at, and swapping the corpse for a fresh live llmgate
	// key would arm a SECOND spendable key under a row whose money was
	// already paid back. Same idempotent-false contract as a replay.
	if ins, err := s.AddAPIKey("ord-ref", 8, "pool-kid-r", "sk-pool-r", 90, "bedrock", BackendPool); err != nil || !ins {
		t.Fatalf("seed refunded-row fixture: ins=%v err=%v", ins, err)
	}
	if err := s.MarkAPIKeyRefunded("ord-ref"); err != nil {
		t.Fatalf("mark ord-ref refunded: %v", err)
	}
	if moved, err = s.MigrateAPIKeyToLlmgate("ord-ref", "pool-kid-r", "sk-pool-r", "lg-kid-r", "sk-lg-r", 90); err != nil || moved {
		t.Fatalf("migrate refunded row: moved=%v err=%v, want false/nil (never repoint a refunded row)", moved, err)
	}
	if k, err = s.APIKeyForOrder("ord-ref"); err != nil || k == nil {
		t.Fatalf("APIKeyForOrder(ord-ref): %v %v", k, err)
	}
	if k.Backend != BackendPool || k.KeyID != "pool-kid-r" || k.APIKey != "sk-pool-r" || k.PoolKeyID != "" || k.PoolAPIKey != "" {
		t.Fatalf("refunded row mutated by refused migrate: %+v", k)
	}
}

// TestListAndCountAPIKeysToMigrate pins the eligibility WHERE shared by
// ListAPIKeysToMigrate and CountAPIKeysToMigrate: live + backend=” +
// sale_class IN (”,'bedrock'). Refunded, kiro, fixed and already-migrated
// rows must all be excluded, oldest-first order must hold, LIMIT must page,
// and the COUNT(*) twin must agree with the full list.
func TestListAndCountAPIKeysToMigrate(t *testing.T) {
	s := newTestStore(t)
	seed := func(ins bool, err error) {
		t.Helper()
		if err != nil || !ins {
			t.Fatalf("seed: ins=%v err=%v", ins, err)
		}
	}
	seed(s.AddAPIKey("ord-kiro", 1, "kid-k", "sk-k", 100, "kiro", BackendPool))       // stays pool by decision
	seed(s.AddAPIKey("ord-bedrock", 1, "kid-b", "sk-b", 100, "bedrock", BackendPool)) // eligible
	seed(s.AddAPIKey("ord-legacy", 1, "kid-l", "sk-l", 100, "", BackendPool))         // eligible (legacy '')
	seed(s.AddAPIKey("ord-fixed", 1, "kid-f", "sk-f", 100, "fixed", BackendPool))     // stays pool by decision
	seed(s.AddAPIKey("ord-refunded", 1, "kid-r", "sk-r", 100, "bedrock", BackendPool))
	// Refund mark comes after the seed helper (it returns only error).
	if err := s.MarkAPIKeyRefunded("ord-refunded"); err != nil { // refunded -> excluded
		t.Fatalf("seed refund mark: %v", err)
	}
	// Already migrated -> excluded by backend='llmgate'.
	seed(s.AddAPIKey("ord-migrated", 1, "kid-m", "sk-m", 100, "bedrock", BackendPool))
	if moved, err := s.MigrateAPIKeyToLlmgate("ord-migrated", "kid-m", "sk-m", "lg-1", "sk-lg-1", 100); err != nil || !moved {
		t.Fatalf("pre-migrate fixture: moved=%v err=%v", moved, err)
	}
	// AddAPIKey stamps whole-second created_at values, so same-instant seeds
	// would tie and ORDER BY created_at ASC would be non-deterministic;
	// stamp explicit distinct values instead.
	for i, id := range []string{"ord-kiro", "ord-bedrock", "ord-legacy", "ord-fixed", "ord-refunded", "ord-migrated"} {
		if _, err := s.db.Exec("UPDATE api_keys SET created_at=? WHERE order_id=?", int64(1000+i), id); err != nil {
			t.Fatalf("stamp created_at %s: %v", id, err)
		}
	}

	got, err := s.ListAPIKeysToMigrate(100, 0)
	if err != nil {
		t.Fatalf("ListAPIKeysToMigrate: %v", err)
	}
	var ids []string
	for _, k := range got {
		ids = append(ids, k.OrderID)
	}
	if len(ids) != 2 || ids[0] != "ord-bedrock" || ids[1] != "ord-legacy" {
		t.Fatalf("eligible set = %v, want [ord-bedrock ord-legacy] (refunded/kiro/fixed/migrated excluded, oldest first)", ids)
	}
	// LIMIT pages the tool through batches.
	got, err = s.ListAPIKeysToMigrate(1, 0)
	if err != nil || len(got) != 1 || got[0].OrderID != "ord-bedrock" {
		t.Fatalf("ListAPIKeysToMigrate(1) = %d rows err=%v, want exactly [ord-bedrock]", len(got), err)
	}
	// COUNT(*) twin agrees with the unlimited list.
	if n, err := s.CountAPIKeysToMigrate(0); err != nil || n != 2 {
		t.Fatalf("CountAPIKeysToMigrate = %d err=%v, want 2", n, err)
	}
}

// TestListAndCountAPIKeysToMigrateChatScope pins the -chat rehearsal filter:
// a non-zero chatID narrows BOTH the list and the count to that chat's
// eligible rows only, while chatID=0 keeps the unscoped behavior (proven
// directly above). This is the store half of the operator's dress-rehearsal
// mode in cmd/migrate-llmgate.
func TestListAndCountAPIKeysToMigrateChatScope(t *testing.T) {
	s := newTestStore(t)
	seed := func(ins bool, err error) {
		t.Helper()
		if err != nil || !ins {
			t.Fatalf("seed: ins=%v err=%v", ins, err)
		}
	}
	// Two eligible rows in chat 7, one in chat 8, plus a chat-7 row that is
	// NOT eligible (kiro class) — the scope must filter, never widen.
	seed(s.AddAPIKey("ord-7a", 7, "kid-7a", "sk-7a", 100, "bedrock", BackendPool))
	seed(s.AddAPIKey("ord-7k", 7, "kid-7k", "sk-7k", 100, "kiro", BackendPool))
	seed(s.AddAPIKey("ord-8a", 8, "kid-8a", "sk-8a", 100, "", BackendPool))
	for i, id := range []string{"ord-7a", "ord-7k", "ord-8a"} {
		if _, err := s.db.Exec("UPDATE api_keys SET created_at=? WHERE order_id=?", int64(2000+i), id); err != nil {
			t.Fatalf("stamp created_at %s: %v", id, err)
		}
	}

	// Scoped list: only chat 7's ELIGIBLE row (the kiro row stays excluded by
	// the sale_class term — chat scope composes with, never replaces, the
	// eligibility WHERE).
	got, err := s.ListAPIKeysToMigrate(100, 7)
	if err != nil || len(got) != 1 || got[0].OrderID != "ord-7a" {
		t.Fatalf("scoped list = %v err=%v, want exactly [ord-7a]", got, err)
	}
	if n, err := s.CountAPIKeysToMigrate(7); err != nil || n != 1 {
		t.Fatalf("scoped count = %d err=%v, want 1", n, err)
	}
	// A chat with no rows at all: empty list, zero count — not an error.
	if got, err = s.ListAPIKeysToMigrate(100, 999); err != nil || len(got) != 0 {
		t.Fatalf("no-such-chat list = %v err=%v, want empty", got, err)
	}
	if n, err := s.CountAPIKeysToMigrate(999); err != nil || n != 0 {
		t.Fatalf("no-such-chat count = %d err=%v, want 0", n, err)
	}
	// chatID=0: unscoped — both chats' eligible rows, exactly as before the
	// flag existed.
	if n, err := s.CountAPIKeysToMigrate(0); err != nil || n != 2 {
		t.Fatalf("unscoped count = %d err=%v, want 2 (0 must mean no filter)", n, err)
	}
	if got, err = s.ListAPIKeysToMigrate(100, 0); err != nil || len(got) != 2 {
		t.Fatalf("unscoped list = %d rows err=%v, want 2", len(got), err)
	}
}

// TestListUnnotifiedMigratedKeysAndMarkNotified is the cmd/migrate-notify
// store round trip: the listing admits ONLY migrated rows (backend='llmgate'
// + non-empty pool_key_id) whose migrated_notified_ts is still NULL, joins
// the order's lang (COALESCE'd to 'en' when the order is missing or its lang
// is NULL/empty), pages by LIMIT oldest-first, and MarkMigratedNotified
// stamps the row so a re-list skips it — idempotently (a replayed stamp
// never re-opens the row).
func TestListUnnotifiedMigratedKeysAndMarkNotified(t *testing.T) {
	s := newTestStore(t)
	seed := func(ins bool, err error) {
		t.Helper()
		if err != nil || !ins {
			t.Fatalf("seed: ins=%v err=%v", ins, err)
		}
	}
	// Three real migrated rows (AddAPIKey pool-side, then the migration
	// commit), each with an orders row in a different lang.
	mk := func(des, lang string, chat int64, credits float64, createdAt int64) {
		seed(s.AddAPIKey(des, chat, "pool-"+des, "sk-pool-"+des, 1, "bedrock", BackendPool))
		if lang != "" {
			// A real order row carrying the lang the DM must be keyed off.
			if err := s.CreateOrder(des, strconv.FormatInt(chat, 10), lang, "__apikey__", "", 1, 100); err != nil {
				t.Fatalf("CreateOrder %s: %v", des, err)
			}
		}
		if moved, err := s.MigrateAPIKeyToLlmgate(des, "pool-"+des, "sk-pool-"+des, "lg-"+des, "sk-lg-"+des, credits); err != nil || !moved {
			t.Fatalf("migrate %s: moved=%v err=%v", des, moved, err)
		}
		if _, err := s.db.Exec("UPDATE api_keys SET created_at=? WHERE order_id=?", createdAt, des); err != nil {
			t.Fatalf("stamp created_at %s: %v", des, err)
		}
	}
	mk("ord-en", "en", 11, 100, 3000)
	mk("ord-vi", "vi", 22, 200.5, 3001)
	mk("ord-zh", "zh", 33, 300, 3002)
	// The shapes that must be EXCLUDED:
	seed(s.AddAPIKey("ord-fresh-lg", 44, "lg-fresh", "sk-lg-fresh", 50, "bedrock", BackendLlmgate)) // fresh llmgate mint: pool_key_id NULL
	seed(s.AddAPIKey("ord-pool", 55, "kid-p", "sk-p", 100, "bedrock", BackendPool))                 // not migrated at all
	// Already notified: a migrated row with the stamp set.
	mk("ord-done", "", 66, 400, 2999)
	if err := s.MarkMigratedNotified("ord-done"); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	got, err := s.ListUnnotifiedMigratedKeys(100)
	if err != nil {
		t.Fatalf("ListUnnotifiedMigratedKeys: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unnotified set = %d rows, want 3 (fresh lg / pool / notified excluded): %+v", len(got), got)
	}
	// Oldest first, with the migrated key values, carried credits, and the
	// joined langs (ord-done at 2999 is older but already stamped — excluded).
	want := []MigratedKey{
		{OrderID: "ord-en", ChatID: 11, APIKey: "sk-lg-ord-en", Credits: 100, Lang: "en"},
		{OrderID: "ord-vi", ChatID: 22, APIKey: "sk-lg-ord-vi", Credits: 200.5, Lang: "vi"},
		{OrderID: "ord-zh", ChatID: 33, APIKey: "sk-lg-ord-zh", Credits: 300, Lang: "zh"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, got[i], w)
		}
	}
	// LIMIT pages the tool through batches.
	if got, err = s.ListUnnotifiedMigratedKeys(1); err != nil || len(got) != 1 || got[0].OrderID != "ord-en" {
		t.Fatalf("LIMIT 1 = %v err=%v, want exactly [ord-en]", got, err)
	}

	// Stamp one row: it leaves the queue immediately, others stay.
	if err := s.MarkMigratedNotified("ord-vi"); err != nil {
		t.Fatalf("mark ord-vi: %v", err)
	}
	got, err = s.ListUnnotifiedMigratedKeys(100)
	if err != nil || len(got) != 2 {
		t.Fatalf("after stamp = %d rows err=%v, want 2 (ord-en, ord-zh)", len(got), err)
	}
	// Replaying the stamp is a no-op (first stamp wins — the DM must never
	// be re-sendable by a re-run), and stamping an unknown order is silent.
	if err := s.MarkMigratedNotified("ord-vi"); err != nil {
		t.Fatalf("replayed mark: %v", err)
	}
	if err := s.MarkMigratedNotified("ord-nope"); err != nil {
		t.Fatalf("unknown-order mark: %v", err)
	}
	if got, err = s.ListUnnotifiedMigratedKeys(100); err != nil || len(got) != 2 {
		t.Fatalf("after replayed stamps = %d rows err=%v, want still 2", len(got), err)
	}

	// Lang defaulting: a migrated row with NO order row at all must COALESCE
	// to 'en' — the LEFT JOIN half of the contract.
	mk("ord-noorder", "", 77, 10, 3003)
	if got, err = s.ListUnnotifiedMigratedKeys(100); err != nil || len(got) != 3 {
		t.Fatalf("after no-order row = %d rows err=%v, want 3", len(got), err)
	}
	last := got[len(got)-1]
	if last.OrderID != "ord-noorder" || last.Lang != "en" {
		t.Errorf("orderless row = %+v, want lang 'en' via COALESCE", last)
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
