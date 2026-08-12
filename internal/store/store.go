// Package store is the Go port of /Users/z/Desktop/Kiro/store.py — the SQLite
// store for user balances, processed deposits, orders, api keys, promo codes,
// referrals, handles, audit trail, bans and the crypto *_pending tables.
//
// Balances are real money (bank top-ups, VND), so they persist in a SQLite DB
// (store.py:2-16). Idempotency is enforced by the DB: processed.txn_id is a
// PRIMARY KEY, so a duplicate credit hits a constraint error and is rejected —
// no app-side race window (store.py:12-13).
//
// Concurrency model (architecture-go.md §5): Python serialized all writers with
// a module-level threading.Lock + a short-lived connection per call
// (store.py:25, 35-49). In Go the single *sql.DB with SetMaxOpenConns(1) IS the
// global lock — the pool hands out at most one connection, so every call is
// serialized exactly like Python's _LOCK, and busy_timeout=30000 keeps
// coexistence with external Python tools hitting the same file safe.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"codezdev-bot/internal/dbfile"
)

// DepositProduct is the sentinel product for wallet top-ups (store.py:28-31).
// Deposit orders are real money but are NOT account purchases, so customer-
// and admin-facing order views exclude them; settle credits balance directly.
const DepositProduct = "__deposit__"

// Store wraps the single-writer SQLite handle for store.db.
type Store struct {
	db *sql.DB
}

// DB exposes the underlying handle so sibling packages that share the same
// store.db file (none today; internal/refund opens its own handle exactly like
// refund_store.py does) or tests can reach it. Read-mostly escape hatch.
func (s *Store) DB() *sql.DB { return s.db }

// Open opens (or creates) store.db at `path` with the exact PRAGMAs the Python
// module set per-connection (store.py:46-49): journal_mode=WAL for better
// concurrent read/write and busy_timeout=30000 so a competing writer (e.g. a
// stray Python maintenance tool) blocks instead of erroring. `_txlock=immediate`
// makes every transaction BEGIN IMMEDIATE — the writer lock is taken up front,
// which is what the read-modify-write flows (promo reserve, credit, claim)
// require (architecture-go.md §5).
func Open(path string) (*Store, error) {
	// modernc.org/sqlite DSN: pragmas are applied on every new connection —
	// with MaxOpenConns(1) that is exactly one connection, mirroring Python's
	// per-call `PRAGMA` executes (store.py:48-49).
	// store.db holds cleartext customer API keys (apikeys.go:18), so the file
	// must never exist at the driver's default (umask-derived, typically
	// world-readable) mode — not even between sql.Open and the first chmod.
	// Pre-creating it 0600 closes that window; Tighten below then covers the
	// -wal/-shm sidecars, which the driver creates on the first write.
	if err := dbfile.Precreate(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Single writer: the 1-connection pool is the Go replacement for Python's
	// module _LOCK (store.py:25) — simpler and deadlock-free vs a separate
	// mutex (architecture-go.md §5).
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init schema: %w", err)
	}
	// initSchema is the first write, so the WAL sidecars exist by now — and
	// they were created by the driver at its own loose default. Non-fatal: a
	// chmod failure must not stop the bot from booting, and the main DB file is
	// already tight.
	if _, err := dbfile.Tighten(path); err != nil {
		log.Println("store: could not tighten db file permissions:", err)
	}
	return s, nil
}

// Close releases the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// initSchema is the port of store.py _init() (store.py:56-191): the same
// idempotent CREATE TABLE IF NOT EXISTS statements plus the same
// PRAGMA table_info column-add migrations, so the Go binary boots on BOTH an
// old prod store.db and a current one, exactly as Python does
// (architecture-go.md §4.1 rules).
func (s *Store) initSchema() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	// Every CREATE below mirrors store.py verbatim (column names, types,
	// defaults) — table-by-table source line cited inline.
	creates := []string{
		// balances(chat_id TEXT PK, amount INTEGER) — current balance, VND
		// (store.py:59-60).
		"CREATE TABLE IF NOT EXISTS balances (" +
			"chat_id TEXT PRIMARY KEY, amount INTEGER NOT NULL DEFAULT 0)",
		// processed(txn_id TEXT PK, ...) — credited SePay txns; the PK is the
		// deposit-idempotency anchor (store.py:61-62).
		"CREATE TABLE IF NOT EXISTS processed (" +
			"txn_id TEXT PRIMARY KEY, chat_id TEXT, amount INTEGER, ts REAL)",
		// Persisted orders; status state machine reserved -> paid ->
		// delivered/refunded, reserved -> expired (store.py:63-71).
		"CREATE TABLE IF NOT EXISTS orders (" +
			"des TEXT PRIMARY KEY, chat_id TEXT, lang TEXT, product TEXT, " +
			"tier TEXT, qty INTEGER, total INTEGER, status TEXT, " +
			"paypal_order_id TEXT, ts REAL)",
		// chat_id <-> @handle directory for admin @username lookup
		// (store.py:72-76).
		"CREATE TABLE IF NOT EXISTS handles (" +
			"chat_id TEXT PRIMARY KEY, handle TEXT, ts REAL)",
		// Append-only audit trail of destructive actions (store.py:77-83).
		"CREATE TABLE IF NOT EXISTS audit (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, ts REAL, actor_chat TEXT, " +
			"actor_kind TEXT, action TEXT, target TEXT, outcome TEXT, detail TEXT)",
		"CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts)", // store.py:84
		// Admin-issued promo codes; used_count is the atomic running tally
		// (store.py:85-94).
		"CREATE TABLE IF NOT EXISTS promo_codes (" +
			"code TEXT PRIMARY KEY, percent REAL NOT NULL, " +
			"max_uses INTEGER NOT NULL, used_count INTEGER NOT NULL DEFAULT 0, " +
			"stack INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1, " +
			"ts REAL)",
		// One persistent referral code per customer; code UNIQUE across the
		// table (store.py:95-99).
		"CREATE TABLE IF NOT EXISTS referrals (" +
			"chat_id TEXT PRIMARY KEY, code TEXT UNIQUE NOT NULL, ts REAL)",
		// One referral redemption per customer FOR LIFE — PK is the REDEEMER's
		// chat_id (store.py:100-105).
		"CREATE TABLE IF NOT EXISTS referral_redemptions (" +
			"chat_id TEXT PRIMARY KEY, code TEXT, referrer_chat TEXT, " +
			"order_des TEXT, ts REAL)",
	}
	for _, q := range creates {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("create: %w", err)
		}
	}

	// Migration: add promo/referral columns to an older `orders` table
	// (store.py:106-112). `have` is collected ONCE and reused for the
	// entra_deleted_ts check, exactly like Python (store.py:109, 116).
	have, err := tableColumns(tx, "orders")
	if err != nil {
		return err
	}
	for _, col := range []string{"code", "code_kind", "referrer_chat"} {
		if !have[col] {
			if _, err := tx.Exec("ALTER TABLE orders ADD COLUMN " + col + " TEXT"); err != nil {
				return fmt.Errorf("migrate orders.%s: %w", col, err)
			}
		}
	}
	// Migration: entra_deleted_ts — set when the Entra account(s) tied to a
	// refunded order have been soft-deleted (store.py:113-117).
	if !have["entra_deleted_ts"] {
		if _, err := tx.Exec("ALTER TABLE orders ADD COLUMN entra_deleted_ts REAL"); err != nil {
			return fmt.Errorf("migrate orders.entra_deleted_ts: %w", err)
		}
	}
	// Migration: statistics columns (.omc/artifacts/stats-fixes-spec.md §1.1).
	// A Go-side addition with no store.py counterpart — the Python original
	// never recorded any of this, which is precisely why its reports could not
	// tell a customer refund from a stockout, or measure what a promo cost.
	//
	// All three are NULLABLE ON PURPOSE. SQLite cannot ADD COLUMN ... NOT NULL
	// without a constant default, and any constant here would be a LIE ABOUT
	// HISTORY: an existing 'refunded' row cannot be retroactively classified
	// (the distinguishing information was never recorded), and an existing
	// order's pre-discount price is gone for good because `total` was
	// overwritten in place by UpdateOrderCode. So NULL carries its own meaning
	// — UNCLASSIFIED — which is a DISTINCT state from any real value, and
	// reports MUST bucket it separately rather than defaulting it into a kind
	// or counting it as a zero discount (spec §1.2).
	//
	// NOTE for whoever writes those reports: NULL still does not mean
	// "pre-migration" for refund_kind, even though the step-3 handler
	// migration has landed (all seven refund sites now call ClaimOrderKind
	// with an explicit kind, guarded by
	// internal/handlers.TestNoBareClaimOrderToRefunded) — that test covers the
	// shapes it can see, not a correctness proof, so a genuinely NULL row
	// should still read as UNCLASSIFIED rather than assumed historical.
	// list_total/code_discount have NO writer at all until spec §3, so THEIR
	// NULL bucket is still growing daily by construction — do not label
	// either bucket "pre-migration orders" while that remains true, or the
	// count will read as a failed migration instead of a pending one.
	// `have` is reused, not re-read, matching the two migrations above.
	if !have["refund_kind"] {
		if _, err := tx.Exec("ALTER TABLE orders ADD COLUMN refund_kind TEXT"); err != nil {
			return fmt.Errorf("migrate orders.refund_kind: %w", err)
		}
	}
	// list_total / code_discount are written by a later slice (spec §3); the
	// columns land now so the schema migrates in one step rather than asking a
	// live prod DB to migrate twice.
	for _, col := range []string{"list_total", "code_discount"} {
		if !have[col] {
			if _, err := tx.Exec("ALTER TABLE orders ADD COLUMN " + col + " INTEGER"); err != nil {
				return fmt.Errorf("migrate orders.%s: %w", col, err)
			}
		}
	}
	// Migration: refunded_amount (task #20, stage S1 — misc.go's
	// RefundedRevenue caveat). Stats today sum orders.total for every refunded
	// row, but warranty refunds are frequently PARTIAL (adminorders.go
	// pro-rates day/hour tiers down from o.Total unless forceFull is set), so
	// the refunded figure is overstated and net revenue is understated by the
	// same residue. This column will hold what was actually credited back, so
	// a later slice (S3) can report the real number instead of the order
	// total. THIS commit only adds the column — no displayed figure changes
	// (S1 is deliberately inert); S2 wires the two refund call sites to
	// populate it, S3 rewires the stats query.
	//
	// NULLABLE, not DEFAULT 0 — and this is the load-bearing decision, not a
	// stylistic one. SQLite ADD COLUMN with a constant default would stamp
	// every pre-existing refunded row with 0, which reads as "nothing was
	// credited back" — i.e. the shop KEPT the money. That is a strictly worse
	// lie than the bug this column fixes: today's stats overstate the refund
	// (bad), but a defaulted 0 would claim the refund never happened (worse —
	// it inverts who has the money). The amount actually credited on every
	// refund before this column existed was never persisted anywhere
	// queryable (misc.go:520-532), so it is permanently unknowable — do NOT
	// attempt to backfill it, and specifically do not reach for the `audit`
	// table: those rows are free-text and denominated in VND forever, so a
	// reconstruction from them would silently reintroduce a unit bug on top
	// of an already-approximate number. NULL is the honest state — "not
	// recorded" — for every row that predates this column, exactly like
	// list_total/code_discount above.
	//
	// NOTE for S2 (do not get this wrong when wiring the writer): this column
	// means CREDITED, not INTENDED. ClaimOrderKind commits the order's
	// status='refunded' BEFORE CreditBalance runs, and both refund call sites
	// have a CreditFailed branch where the order already reads 'refunded' but
	// ZERO money moved (that branch is exactly what fires the stuck-refund
	// owner alert). Writing this column alongside the ClaimOrderKind call —
	// the naturally-reached-for implementation — would make it LIE on
	// precisely the path that already needs an operator's attention. Set it
	// only once CreditBalance has actually succeeded.
	//
	// Coupling: this column is money-shaped, so it must migrate in the same
	// commit as its cmd/migrate-usdt/migrate.go baseSpecs entry — a money
	// column that appears on a DB after the one-shot VND->micro-USDT ledger
	// migration has run there would stay permanently unconverted with nothing
	// in the data flagging the mixed unit.
	if !have["refunded_amount"] {
		if _, err := tx.Exec("ALTER TABLE orders ADD COLUMN refunded_amount INTEGER"); err != nil {
			return fmt.Errorf("migrate orders.refunded_amount: %w", err)
		}
	}

	creates2 := []string{
		// Secondary indexes for the hot non-PK lookups (store.py:118-125).
		"CREATE INDEX IF NOT EXISTS idx_orders_paypal ON orders(paypal_order_id)",
		"CREATE INDEX IF NOT EXISTS idx_orders_chat ON orders(chat_id)",
		"CREATE INDEX IF NOT EXISTS idx_orders_status_ts ON orders(status, ts)",
		// Statistics indexes (stats-fixes-spec.md §1.1) — the refund-kind
		// counters and the promo-cost report both group on these columns, and
		// without an index each dashboard render is a full table scan of every
		// order the shop has ever taken. Declared here (not beside the column
		// ALTERs above) because CREATE INDEX IF NOT EXISTS is idempotent and
		// this is where every other orders index lives.
		"CREATE INDEX IF NOT EXISTS idx_orders_refund_kind ON orders(refund_kind)",
		"CREATE INDEX IF NOT EXISTS idx_orders_code_kind ON orders(code_kind)",
		// Pending Binance USDT payments — restart-safe poller state
		// (store.py:126-133).
		"CREATE TABLE IF NOT EXISTS binance_pending (" +
			"des TEXT PRIMARY KEY, chat_id TEXT, amount TEXT, total INTEGER, " +
			"since_ms INTEGER, deadline REAL, ts REAL)",
		// Same for USDT BEP20 on-chain orders (store.py:134-139).
		"CREATE TABLE IF NOT EXISTS bep20_pending (" +
			"des TEXT PRIMARY KEY, chat_id TEXT, amount TEXT, total INTEGER, " +
			"from_block INTEGER, deadline REAL, ts REAL)",
		// Same for USDT TRC20 (TRON) orders (store.py:140-145).
		"CREATE TABLE IF NOT EXISTS tron_pending (" +
			"des TEXT PRIMARY KEY, chat_id TEXT, amount TEXT, total INTEGER, " +
			"min_ts INTEGER, deadline REAL, ts REAL)",
		// Incoming on-chain transfers the hands-free deposit watcher could NOT
		// attribute, because two or more live deposits could each claim the
		// amount (internal/handlers/usdtwatch.go, bands.MatchAmbiguous). No
		// Python ancestor — the watcher is new in the Go port.
		//
		// WHY THIS IS A SEPARATE TABLE AND NOT A FLAG ON `processed`: a row in
		// `processed` means CREDITED, and TxnExists is read as exactly that by
		// the settle paths. A quarantined transfer is the opposite — money that
		// arrived and was deliberately NOT credited — so recording it there
		// would corrupt a meaning other code already depends on.
		//
		// WHY DURABLE rather than an in-memory set: without it the refusal only
		// holds until the competing band settles or sweeps, at which point the
		// same transfer looks unambiguous and is auto-credited to whichever
		// deposit happened to survive — attribution by attrition. A process-local
		// set would additionally forget everything on each deploy.
		//
		// Columns are the minimum the skip and a human's investigation need:
		// txn_id is both the lookup key and (as PK) the write's own dedupe;
		// provider says which chain to go looking on; amount is what a human
		// compares against the candidate bands; first_seen is when it arrived.
		// Nothing here is pre-built for the resolution UI — that is a later
		// task's to design.
		//
		// ROWS ARE NEVER DELETED by this code, and that is safe: if the customer
		// later pastes the hash, the verify path credits it (it does not consult
		// this table — the human-driven route must stay open) and the watcher's
		// own processed-ledger pre-filter skips it from then on, so a stale row
		// is inert. Clearing them belongs to the human-resolution workflow.
		"CREATE TABLE IF NOT EXISTS ambiguous_txns (" +
			"txn_id TEXT PRIMARY KEY, provider TEXT, amount TEXT, first_seen REAL)",
		// Banned chats — persistent backing for the in-memory ban gate
		// (store.py:146-150).
		"CREATE TABLE IF NOT EXISTS banned (" +
			"chat_id TEXT PRIMARY KEY, reason TEXT, by_chat TEXT, ts REAL)",
		// Kiro-Go API keys minted for delivered "__apikey__" orders. order_id
		// PK = the settle-idempotency anchor. ⚠ chat_id is INTEGER here,
		// TEXT everywhere else — preserve per-table types
		// (store.py:151-162, architecture-go.md §4.1).
		"CREATE TABLE IF NOT EXISTS api_keys (" +
			"order_id TEXT PRIMARY KEY, chat_id INTEGER, key_id TEXT, " +
			"api_key TEXT, credits REAL, created_at INTEGER, refunded_ts REAL, " +
			"sale_class TEXT NOT NULL DEFAULT '')",
		"CREATE INDEX IF NOT EXISTS idx_api_keys_chat ON api_keys(chat_id)", // store.py:163
	}
	for _, q := range creates2 {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("create: %w", err)
		}
	}

	// Migration: refunded_ts on an api_keys table created before the
	// refund-marking column existed (store.py:164-169).
	haveAK, err := tableColumns(tx, "api_keys")
	if err != nil {
		return err
	}
	if !haveAK["refunded_ts"] {
		if _, err := tx.Exec("ALTER TABLE api_keys ADD COLUMN refunded_ts REAL"); err != nil {
			return fmt.Errorf("migrate api_keys.refunded_ts: %w", err)
		}
	}
	// Migration: sale_class for the two-SKU split; '' = legacy pre-split rows
	// (store.py:170-177).
	if !haveAK["sale_class"] {
		if _, err := tx.Exec("ALTER TABLE api_keys ADD COLUMN sale_class TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migrate api_keys.sale_class: %w", err)
		}
	}

	// Credit top-ups for already-sold Kiro-Go keys; applied_ts is the
	// fulfillment idempotency anchor (store.py:178-191).
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS apikey_recharges (" +
		"order_id TEXT PRIMARY KEY, target_order_id TEXT, chat_id INTEGER, " +
		"credits REAL, created_at INTEGER, applied_ts REAL)"); err != nil {
		return fmt.Errorf("create apikey_recharges: %w", err)
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_apikey_recharges_target " +
		"ON apikey_recharges(target_order_id)"); err != nil {
		return fmt.Errorf("index apikey_recharges: %w", err)
	}

	return tx.Commit()
}

// tableColumns returns the set of column names in `table` via
// PRAGMA table_info — the same probe store.py uses for its column-add
// migrations (store.py:109, 167).
func tableColumns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		// table_info columns: cid, name, type, notnull, dflt_value, pk —
		// we only need `name` (index 1, mirrors r[1] in store.py:109).
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// ---- shared helpers -------------------------------------------------------

// now returns epoch seconds as float64 — the Go equivalent of Python's
// time.time(), matching the REAL `ts` columns store.py writes.
func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// nowUnix returns whole epoch seconds — Python's int(time.time()), used for
// the INTEGER created_at columns in api_keys / apikey_recharges
// (store.py:331, 398).
func nowUnix() int64 { return time.Now().Unix() }

// NewDes generates an order id in the exact Python format: uuid4 hex, 32
// lowercase chars, NO hyphens (architecture-go.md §4.1: "des = uuid4 hex —
// survives bank-statement normalization, sepay.py:11-12"). Uses crypto/rand
// and sets the RFC 4122 version-4/variant bits so the value is a genuine
// uuid4, byte-identical in shape to Python's uuid.uuid4().hex.
func NewDes() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure means the OS entropy source is broken — there
		// is no safe fallback for order ids, so fail loudly.
		panic("store: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return hex.EncodeToString(b)
}

// isPKConflict reports whether err is SQLITE_CONSTRAINT_PRIMARYKEY (code
// 1555) from the modernc driver. Detection is by driver error CODE — never by
// string match, never by pre-SELECT (that would reintroduce the race Python
// deliberately avoids, store.py:11-13; architecture-go.md §5).
func isPKConflict(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// placeholders builds "?,?,?" for n params — the Go version of Python's
// ",".join("?" * len(statuses)) used all over store.py (e.g. store.py:302).
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// truncRunes clips s to at most n runes — the Go version of Python's
// str(x)[:n] clipping in log_action / ban_user (store.py:531, 632), which
// slices characters, not bytes.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// strOrNil converts a possibly-empty *string into a driver value: nil pointer
// -> SQL NULL (mirrors Python passing None).
func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
