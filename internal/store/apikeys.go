// API keys + recharges — port of store.py:312-482 ("api keys (Kiro-Go metered
// keys sold as __apikey__ orders)").
//
// ⚠ Per-table type quirk preserved from Python: chat_id in api_keys and
// apikey_recharges is INTEGER, while every other table stores chat_id as TEXT
// (store.py:159-162, 187-189; architecture-go.md §4.1). Callers pass int64
// here and string everywhere else.
package store

import "database/sql"

// APIKey mirrors one `api_keys` row (store.py:159-162): one row per delivered
// "__apikey__" order; order_id PK is the settle-idempotency anchor.
type APIKey struct {
	OrderID    string
	ChatID     int64
	KeyID      string   // Kiro-Go's uuid for the key (used to query /admin/stats)
	APIKey     string   // cleartext key — the buyer's own secret, resent via "reveal"
	Credits    float64  // total credits ever bought (mint + confirmed recharges)
	CreatedAt  int64    // epoch seconds (INTEGER column)
	RefundedTS *float64 // non-NULL once refunded (store.py:164-169)
	SaleClass  string   // "bedrock" | "kiro" | "" legacy (store.py:170-177)
}

// scanAPIKey scans one api_keys row (explicit column order below).
const apiKeyCols = "order_id, chat_id, key_id, api_key, credits, created_at, refunded_ts, sale_class"

func scanAPIKey(r rowScanner) (*APIKey, error) {
	var (
		k             APIKey
		keyID, apiKey sql.NullString
		credits       sql.NullFloat64
		createdAt     sql.NullInt64
		refunded      sql.NullFloat64
		saleClass     sql.NullString
	)
	if err := r.Scan(&k.OrderID, &k.ChatID, &keyID, &apiKey, &credits,
		&createdAt, &refunded, &saleClass); err != nil {
		return nil, err
	}
	k.KeyID, k.APIKey = keyID.String, apiKey.String
	k.Credits, k.CreatedAt = credits.Float64, createdAt.Int64
	k.SaleClass = saleClass.String
	if refunded.Valid {
		v := refunded.Float64
		k.RefundedTS = &v
	}
	return &k, nil
}

// AddAPIKey persists a freshly minted Kiro-Go key for an order
// (store.py:314-332). Returns true if the row was inserted, false if one
// already existed for this order (PRIMARY KEY conflict) — the caller treats
// false as "already minted, deliver the STORED key" so a settle replay can
// never double-mint. INSERT OR IGNORE (not plain INSERT) so the
// no-double-mint guard is enforced by the DB itself, with no app-side
// read-then-write race.
//
// saleClass is the key's SKU ("bedrock" | "kiro"), fixed forever at mint; ""
// only exists for legacy callers/rows.
func (s *Store) AddAPIKey(orderID string, chatID int64, keyID, apiKey string, credits float64, saleClass string) (bool, error) {
	res, err := s.db.Exec(
		"INSERT OR IGNORE INTO api_keys "+
			"(order_id, chat_id, key_id, api_key, credits, created_at, sale_class) "+
			"VALUES (?,?,?,?,?,?,?)",
		orderID, chatID, keyID, apiKey, credits, nowUnix(), saleClass)
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
