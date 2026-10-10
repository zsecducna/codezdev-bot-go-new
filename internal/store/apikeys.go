// API keys + recharges — port of store.py:312-482 ("api keys (Kiro-Go metered
// keys sold as __apikey__ orders)").
//
// ⚠ Per-table type quirk preserved from Python: chat_id in api_keys and
// apikey_recharges is INTEGER, while every other table stores chat_id as TEXT
// (store.py:159-162, 187-189; architecture-go.md §4.1). Callers pass int64
// here and string everywhere else.
package store

import "database/sql"

// Backend markers for api_keys.backend — WHICH gateway a row's key lives on.
// The llmgate cutover moved minting from the Kiro-Go/Pool gateway to direct
// llmgate.app keys, but every pool key already sold stays valid on its own
// gateway, so the row has to say which one it is. The zero value IS
// BackendPool BY DESIGN (and is the column's SQL-side DEFAULT ”): every row
// minted before the cutover is a pool key, so ” classifies the entire
// pre-existing table correctly with no backfill — the same trick SaleClass's
// ” legacy value plays.
const (
	// BackendPool marks a key minted on the Kiro-Go/Pool gateway — the only
	// backend that existed before the cutover, and still the backend for the
	// kiro/fixed SKUs, which are NOT migrating.
	BackendPool = ""
	// BackendLlmgate marks a key the bot minted directly on llmgate.app.
	BackendLlmgate = "llmgate"
)

// APIKey mirrors one `api_keys` row (store.py:159-162): one row per delivered
// "__apikey__" order; order_id PK is the settle-idempotency anchor.
type APIKey struct {
	OrderID    string
	ChatID     int64
	KeyID      string   // gateway's uuid for the key (used to query /admin/stats)
	APIKey     string   // cleartext key — the buyer's own secret, resent via "reveal"
	Credits    float64  // total credits ever bought (mint + confirmed recharges)
	CreatedAt  int64    // epoch seconds (INTEGER column)
	RefundedTS *float64 // non-NULL once refunded (store.py:164-169)
	SaleClass  string   // "bedrock" | "kiro" | "" legacy (store.py:170-177)
	Backend    string   // BackendPool ("") | BackendLlmgate — which gateway holds this key
	PoolKeyID  string   // ORIGINAL pool uuid, preserved by MigrateAPIKeyToLlmgate ("" = none displaced)
	PoolAPIKey string   // ORIGINAL pool cleartext key, preserved for a later pool-side revoke
}

// scanAPIKey scans one api_keys row (explicit column order below). The three
// llmgate-cutover columns are APPENDED AT THE END — every earlier column's
// position is load-bearing (scan order must match apiKeyCols verbatim), so a
// new column may only ever be added at the tail of both.
const apiKeyCols = "order_id, chat_id, key_id, api_key, credits, created_at, refunded_ts, sale_class, backend, pool_key_id, pool_api_key"

func scanAPIKey(r rowScanner) (*APIKey, error) {
	var (
		k                                                        APIKey
		keyID, apiKey, saleClass, backend, poolKeyID, poolAPIKey sql.NullString
		credits                                                  sql.NullFloat64
		createdAt                                                sql.NullInt64
		refunded                                                 sql.NullFloat64
	)
	if err := r.Scan(&k.OrderID, &k.ChatID, &keyID, &apiKey, &credits,
		&createdAt, &refunded, &saleClass, &backend, &poolKeyID, &poolAPIKey); err != nil {
		return nil, err
	}
	k.KeyID, k.APIKey = keyID.String, apiKey.String
	k.Credits, k.CreatedAt = credits.Float64, createdAt.Int64
	k.SaleClass, k.Backend = saleClass.String, backend.String
	k.PoolKeyID, k.PoolAPIKey = poolKeyID.String, poolAPIKey.String
	if refunded.Valid {
		v := refunded.Float64
		k.RefundedTS = &v
	}
	return &k, nil
}

// AddAPIKey persists a freshly minted key for an order
// (store.py:314-332). Returns true if the row was inserted, false if one
// already existed for this order (PRIMARY KEY conflict) — the caller treats
// false as "already minted, deliver the STORED key" so a settle replay can
// never double-mint. INSERT OR IGNORE (not plain INSERT) so the
// no-double-mint guard is enforced by the DB itself, with no app-side
// read-then-write race.
//
// saleClass is the key's SKU ("bedrock" | "kiro"), fixed forever at mint; ""
// only exists for legacy callers/rows.
//
// backend records WHICH gateway minted the key — BackendPool ("") for the
// Kiro-Go/Pool gateway, BackendLlmgate for a direct llmgate.app mint. Like
// saleClass it is stamped once at insert and must match where the key REALLY
// lives (only MigrateAPIKeyToLlmgate ever changes it), or the recharge/stats
// paths will later talk to the wrong gateway for this key.
func (s *Store) AddAPIKey(orderID string, chatID int64, keyID, apiKey string, credits float64, saleClass, backend string) (bool, error) {
	res, err := s.db.Exec(
		"INSERT OR IGNORE INTO api_keys "+
			"(order_id, chat_id, key_id, api_key, credits, created_at, sale_class, backend) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		orderID, chatID, keyID, apiKey, credits, nowUnix(), saleClass, backend)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// APIKeyForOrder returns the minted-key row for an order, or nil if nothing
// was minted (store.py:335-342). This is the settle-idempotency read:
// fulfillment mints only when this is nil.
func (s *Store) APIKeyForOrder(orderID string) (*APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRow(
		"SELECT "+apiKeyCols+" FROM api_keys WHERE order_id=?", orderID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// BumpAPIKeyCredits adds `delta` to a minted key's locally stored credit quota
// after a CONFIRMED recharge (store.py:345-353), so every stored-credits
// consumer keeps matching the key's real server-side limit. Kiro-Go's live
// stats stay the source of truth for remaining credits; this only tracks the
// total ever bought.
func (s *Store) BumpAPIKeyCredits(orderID string, delta float64) error {
	_, err := s.db.Exec("UPDATE api_keys SET credits = COALESCE(credits,0) + ? "+
		"WHERE order_id=?", delta, orderID)
	return err
}

// MarkAPIKeyRefunded stamps a minted key's row as REFUNDED (idempotent — first
// stamp wins, store.py:356-364). Stops the reveal button / My-Keys view from
// re-serving a refunded key; belt-and-suspenders next to the Kiro-Go revoke.
func (s *Store) MarkAPIKeyRefunded(orderID string) error {
	_, err := s.db.Exec("UPDATE api_keys SET refunded_ts=? "+
		"WHERE order_id=? AND refunded_ts IS NULL", now(), orderID)
	return err
}

// CountAPIKeysForChat returns the number of LIVE API keys this chat currently
// holds (rows with refunded_ts NULL) — powers the per-customer key cap
// (store.py:441-451). Refunded keys are excluded: the row is kept for
// history/idempotency, but a refunded key must not consume a slot.
func (s *Store) CountAPIKeysForChat(chatID int64) (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM api_keys "+
		"WHERE chat_id=? AND refunded_ts IS NULL", chatID).Scan(&n)
	return n, err
}

// APIKeysForChat returns this chat's minted keys, newest first — powers the
// Self-Service "My API Keys" view (store.py:454-467). liveOnly=true filters
// refunded keys IN SQL so the limit counts only live rows — a Go-side
// post-filter over a capped page would wrongly hide an older live key behind
// `limit` newer refunded ones (the recharge-picker bug the Python comment
// records).
func (s *Store) APIKeysForChat(chatID int64, limit int, liveOnly bool) ([]*APIKey, error) {
	q := "SELECT " + apiKeyCols + " FROM api_keys WHERE chat_id=? "
	if liveOnly {
		q += "AND refunded_ts IS NULL "
	}
	q += "ORDER BY created_at DESC LIMIT ?"
	rows, err := s.db.Query(q, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---- llmgate migration (Kiro-Go/Pool → llmgate.app cutover) -----------------

// ListAPIKeysToMigrate returns the pool-backend keys still eligible for the
// one-shot migration to llmgate.app keys, oldest first (a deterministic
// replay order, so a re-run after a partial pass resumes where it stopped).
// Eligible = live (refunded_ts IS NULL), backend=” (pool — this is what
// excludes already-migrated rows and makes re-running the tool safe), and
// sale_class IN (”,'bedrock') — the metered pool classes. kiro/fixed rows
// stay pool-backed BY DECISION: those SKUs are not moving in this cutover,
// so handing them to the migrator would repoint keys whose gateway must not
// change. Refunded rows are excluded because there is nothing to hand the
// customer and no live quota to carry over.
func (s *Store) ListAPIKeysToMigrate(limit int) ([]*APIKey, error) {
	rows, err := s.db.Query(
		"SELECT "+apiKeyCols+" FROM api_keys "+
			"WHERE refunded_ts IS NULL AND backend='' "+
			"AND sale_class IN ('','bedrock') "+
			"ORDER BY created_at ASC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// MigrateAPIKeyToLlmgate repoints one api_keys row from its pool key to a
// freshly minted llmgate.app key — the COMMIT POINT of the per-key migration.
// The caller must have ALREADY created the llmgate key (and topped it up to
// lgCredits) before calling: this single UPDATE only swaps the row's
// key_id/api_key/credits to the llmgate values and stamps backend='llmgate'.
//
// poolKeyID/poolAPIKey must be the row's ORIGINAL pool uuid + cleartext
// customer key (i.e. what key_id/api_key held BEFORE this call): they are
// preserved into pool_key_id/pool_api_key because revoking the pool key
// later NEEDS both — the pool admin API addresses keys by uuid, and an
// operator revoking by hand needs the cleartext to identify the row in the
// pool's own console. That preservation is what makes the migration
// reversible by hand; do not "clean up" the columns.
//
// The WHERE backend=” AND refunded_ts IS NULL guard makes the move
// IDEMPOTENT and race-safe in exactly the spirit of AddAPIKey's INSERT OR
// IGNORE: a replayed or concurrent call matches zero rows, returns
// (false, nil), and leaves the stored values untouched (the losing caller's
// llmgate key is orphaned, never double-applied). refunded_ts IS NULL is
// load-bearing alongside it: NEVER repoint an already-refunded row — its
// revoke already killed (or tried to kill) the key the row pointed at, and
// swapping the corpse for a fresh llmgate key would arm a SECOND live key
// under a row whose money was already paid back. false therefore means
// "already migrated, refunded, or no such row", which callers must treat as
// success, not failure.
func (s *Store) MigrateAPIKeyToLlmgate(orderID, poolKeyID, poolAPIKey, lgKeyID, lgAPIKey string, lgCredits float64) (bool, error) {
	res, err := s.db.Exec(
		"UPDATE api_keys SET pool_key_id=?, pool_api_key=?, key_id=?, "+
			"api_key=?, credits=?, backend='llmgate' "+
			"WHERE order_id=? AND backend='' AND refunded_ts IS NULL",
		poolKeyID, poolAPIKey, lgKeyID, lgAPIKey, lgCredits, orderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountAPIKeysToMigrate is the COUNT(*) twin of ListAPIKeysToMigrate over the
// exact same WHERE — the migration tool's progress meter (done vs remaining).
// Kept as a separate method (not len(ListAPIKeysToMigrate(huge))) so the
// count never depends on paging through every row, and so the two queries
// drift only if someone edits one WHERE and not the other — which the store
// tests pin.
func (s *Store) CountAPIKeysToMigrate() (int64, error) {
	var n int64
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM api_keys " +
			"WHERE refunded_ts IS NULL AND backend='' " +
			"AND sale_class IN ('','bedrock')").Scan(&n)
	return n, err
}

// ---- apikey recharges (credit top-ups for already-sold keys) --------------

// Recharge mirrors one `apikey_recharges` row (store.py:187-189): one row per
// recharge ORDER, written at ORDER-CREATION time so the order→key binding
// survives a restart between payment and fulfillment.
type Recharge struct {
	OrderID       string
	TargetOrderID string // des of the ORIGINAL "__apikey__" order (joins api_keys.order_id)
	ChatID        int64
	Credits       float64
	CreatedAt     int64
	AppliedTS     *float64 // non-NULL once Kiro-Go confirmed — the replay-settle anchor
}

// AddAPIKeyRecharge persists the recharge-order → target-key binding at
// ORDER-CREATION time, before payment (store.py:386-399). INSERT OR IGNORE +
// PRIMARY KEY so a duplicate create (shouldn't happen — des is a fresh uuid)
// can't clobber an existing binding. Returns true when the row was inserted.
func (s *Store) AddAPIKeyRecharge(orderID, targetOrderID string, chatID int64, credits float64) (bool, error) {
	res, err := s.db.Exec(
		"INSERT OR IGNORE INTO apikey_recharges "+
			"(order_id, target_order_id, chat_id, credits, created_at) "+
			"VALUES (?,?,?,?,?)",
		orderID, targetOrderID, chatID, credits, nowUnix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// APIKeyRechargeForOrder returns the recharge-binding row for an order, or nil
// (store.py:402-410). Fulfillment reads this to find the target key;
// AppliedTS non-nil means Kiro-Go already confirmed the top-up (replayed
// settles must NOT recharge again).
func (s *Store) APIKeyRechargeForOrder(orderID string) (*Recharge, error) {
	var (
		r       Recharge
		target  sql.NullString
		credits sql.NullFloat64
		created sql.NullInt64
		applied sql.NullFloat64
	)
	err := s.db.QueryRow(
		"SELECT order_id, target_order_id, chat_id, credits, created_at, applied_ts "+
			"FROM apikey_recharges WHERE order_id=?", orderID).
		Scan(&r.OrderID, &target, &r.ChatID, &credits, &created, &applied)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.TargetOrderID, r.Credits, r.CreatedAt = target.String, credits.Float64, created.Int64
	if applied.Valid {
		v := applied.Float64
		r.AppliedTS = &v
	}
	return &r, nil
}

// MarkAPIKeyRechargeApplied stamps a recharge as APPLIED on Kiro-Go
// (idempotent — first stamp wins, store.py:413-420). Set immediately after the
// /admin/recharge_api_key call succeeds; the stamp is what stops a replayed
// settle from topping the key up twice.
func (s *Store) MarkAPIKeyRechargeApplied(orderID string) error {
	_, err := s.db.Exec("UPDATE apikey_recharges SET applied_ts=? "+
		"WHERE order_id=? AND applied_ts IS NULL", now(), orderID)
	return err
}

// RechargeOrder is one row of the applied-recharge × order join
// (store.py:423-438) — powers the whole-key GROUP refund maths.
type RechargeOrder struct {
	Des           string // the recharge order's des (r.order_id)
	TargetOrderID string
	Credits       float64
	AppliedTS     float64
	Status        string
	Total         int64
	Qty           int64
	ChatID        string // TEXT here — comes from the orders table side of the join
	TS            float64
}

// RechargeOrdersForTarget returns all APPLIED recharge orders for one sold key
// (oldest first), each joined with its order row's status/total/qty
// (store.py:423-438). Un-applied rows (unpaid / mint pending /
// refunded-before-apply) are excluded — their credits never reached the key.
func (s *Store) RechargeOrdersForTarget(targetOrderID string) ([]RechargeOrder, error) {
	rows, err := s.db.Query(
		"SELECT r.order_id AS des, r.target_order_id, r.credits, "+
			"r.applied_ts, o.status, o.total, o.qty, o.chat_id, o.ts "+
			"FROM apikey_recharges r JOIN orders o ON o.des = r.order_id "+
			"WHERE r.target_order_id=? AND r.applied_ts IS NOT NULL "+
			"ORDER BY r.created_at ASC", targetOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RechargeOrder
	for rows.Next() {
		var (
			ro                 RechargeOrder
			target, st, chatID sql.NullString
			credits, appl, ts  sql.NullFloat64
			total, qty         sql.NullInt64
		)
		if err := rows.Scan(&ro.Des, &target, &credits, &appl, &st, &total,
			&qty, &chatID, &ts); err != nil {
			return nil, err
		}
		ro.TargetOrderID, ro.Status, ro.ChatID = target.String, st.String, chatID.String
		ro.Credits, ro.AppliedTS, ro.TS = credits.Float64, appl.Float64, ts.Float64
		ro.Total, ro.Qty = total.Int64, qty.Int64
		out = append(out, ro)
	}
	return out, rows.Err()
}

// PendingRechargeCreditsForTarget returns Σ credits of IN-FLIGHT (reserved or
// paid, not yet applied) recharge orders for one key (store.py:470-482). The
// per-key cap gate subtracts this from the live headroom so two overlapping
// checkouts can't jointly push the key past the cap — live creditsRemaining
// alone only reflects recharges already APPLIED.
func (s *Store) PendingRechargeCreditsForTarget(targetOrderID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(
		"SELECT COALESCE(SUM(o.qty),0) FROM apikey_recharges r "+
			"JOIN orders o ON o.des = r.order_id "+
			"WHERE r.target_order_id=? AND r.applied_ts IS NULL "+
			"AND o.status IN ('reserved','paid')", targetOrderID).Scan(&n)
	return n, err
}
