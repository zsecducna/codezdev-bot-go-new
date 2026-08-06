// Crypto *_pending tables — port of store.py:739-814 ("binance pending
// (restart-safe payment matching)" + bep20 + tron).
//
// One row per awaiting order; `amount` is the 2-dp unique USDT STRING to match
// on (store.py:126-133). Persisted so a restart can rehydrate the in-memory
// poller AND an out-of-process reconcile can settle a credit that arrived
// while the bot was down.
//
// USDT migration note (2026-08-06): the Binance Pay rail was removed (S3),
// taking its only callers with it — BinancePendingAdd/Del/All and the
// BinancePending struct were deleted as dead code (defect class: a
// feature-cut leaving a handler function with zero remaining callers). The
// `binance_pending` TABLE itself is left in the schema (internal/store/store.go)
// per the migration plan — any straggler rows stay inspectable until the
// later live-data migration slice drops it.
package store

import "database/sql"

// Bep20Pending mirrors one `bep20_pending` row (store.py:137-139). FromBlock
// was the eth_getLogs floor (unused/0 now — no poller, store.py:769-770).
type Bep20Pending struct {
	Des       string
	ChatID    string
	Amount    string
	Total     int64
	FromBlock int64
	Deadline  float64
	TS        float64
}

// TronPending mirrors one `tron_pending` row (store.py:143-145). MinTS is the
// epoch-ms verify-query floor.
type TronPending struct {
	Des      string
	ChatID   string
	Amount   string
	Total    int64
	MinTS    int64
	Deadline float64
	TS       float64
}

// Bep20PendingAdd persists a pending USDT BEP20 order so its unique-amount
// reservation + the customer-submitted verify-by-hash survive a restart
// (store.py:767-776). INSERT OR REPLACE so a re-tap just refreshes the row.
func (s *Store) Bep20PendingAdd(des, chatID, amount string, total, fromBlock int64, deadline float64) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO bep20_pending"+
		"(des, chat_id, amount, total, from_block, deadline, ts) "+
		"VALUES (?,?,?,?,?,?,?)",
		des, chatID, amount, total, fromBlock, deadline, now())
	return err
}

// Bep20PendingDel drops a pending USDT BEP20 order. Idempotent
// (store.py:779-782).
func (s *Store) Bep20PendingDel(des string) error {
	_, err := s.db.Exec("DELETE FROM bep20_pending WHERE des=?", des)
	return err
}

// Bep20PendingAll returns all pending BEP20 orders (for startup rehydrate /
// grace prune, store.py:785-789).
func (s *Store) Bep20PendingAll() ([]Bep20Pending, error) {
	rows, err := s.db.Query(
		"SELECT des, chat_id, amount, total, from_block, deadline, ts FROM bep20_pending")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bep20Pending
	for rows.Next() {
		var (
			p              Bep20Pending
			chatID, amount sql.NullString
			total, from    sql.NullInt64
			deadline, ts   sql.NullFloat64
		)
		if err := rows.Scan(&p.Des, &chatID, &amount, &total, &from, &deadline, &ts); err != nil {
			return nil, err
		}
		p.ChatID, p.Amount = chatID.String, amount.String
		p.Total, p.FromBlock = total.Int64, from.Int64
		p.Deadline, p.TS = deadline.Float64, ts.Float64
		out = append(out, p)
	}
	return out, rows.Err()
}

// TronPendingAdd persists a pending USDT TRC20 (TRON) order (store.py:792-801).
// INSERT OR REPLACE so a re-tap just refreshes the row.
func (s *Store) TronPendingAdd(des, chatID, amount string, total, minTS int64, deadline float64) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO tron_pending"+
		"(des, chat_id, amount, total, min_ts, deadline, ts) "+
		"VALUES (?,?,?,?,?,?,?)",
		des, chatID, amount, total, minTS, deadline, now())
	return err
}

// TronPendingDel drops a pending USDT TRON order. Idempotent
// (store.py:804-807).
func (s *Store) TronPendingDel(des string) error {
	_, err := s.db.Exec("DELETE FROM tron_pending WHERE des=?", des)
	return err
}

// TronPendingAll returns all pending TRON orders (for startup rehydrate /
// grace prune, store.py:810-814).
func (s *Store) TronPendingAll() ([]TronPending, error) {
	rows, err := s.db.Query(
		"SELECT des, chat_id, amount, total, min_ts, deadline, ts FROM tron_pending")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TronPending
	for rows.Next() {
		var (
			p              TronPending
			chatID, amount sql.NullString
			total, minTS   sql.NullInt64
			deadline, ts   sql.NullFloat64
		)
		if err := rows.Scan(&p.Des, &chatID, &amount, &total, &minTS, &deadline, &ts); err != nil {
			return nil, err
		}
		p.ChatID, p.Amount = chatID.String, amount.String
		p.Total, p.MinTS = total.Int64, minTS.Int64
		p.Deadline, p.TS = deadline.Float64, ts.Float64
		out = append(out, p)
	}
	return out, rows.Err()
}
