// Balances + processed-txn dedupe — port of store.py:666-736 and 838-869.
//
// This file owns the MONEY invariants:
//   - Deposit crediting is idempotent BY TRANSACTION ID, enforced at the
//     storage layer via the processed.txn_id PRIMARY KEY — never in
//     application code (repo CLAUDE.md invariant; store.py:12-13).
//   - Debit is a single guarded UPDATE so concurrent spends can never
//     overdraw (store.py:854-869).
package store

import "database/sql"

// GetBalance returns the current balance (int VND) for a chat; 0 if the chat
// has no balance row (store.py:666-671).
func (s *Store) GetBalance(chatID string) (int64, error) {
	var amount int64
	err := s.db.QueryRow("SELECT amount FROM balances WHERE chat_id=?",
		chatID).Scan(&amount)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return amount, err
}

// Credit credits `amount` to a chat for a deposit txn, EXACTLY ONCE
// (store.py:674-697). Returns (credited, newBalance). credited is false when
// this txnID was already processed — the INSERT into processed is the dedupe
// gate: a duplicate txn_id hits SQLITE_CONSTRAINT_PRIMARYKEY (detected by
// driver error CODE, architecture-go.md §5) BEFORE any balance change, so the
// dedupe is atomic with no app-side race window.
func (s *Store) Credit(chatID string, amount int64, txnID string) (bool, int64, error) {
	// One BEGIN IMMEDIATE transaction covers gate + upsert + read-back, the
	// Go equivalent of Python's single `with _LOCK, _conn() as c` block.
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	// This INSERT is the dedupe gate: duplicate txn_id -> PK constraint
	// (store.py:685-687).
	_, err = tx.Exec("INSERT INTO processed(txn_id, chat_id, amount, ts) VALUES (?,?,?,?)",
		txnID, chatID, amount, now())
	if isPKConflict(err) {
		// Already processed: report the CURRENT balance, credit nothing
		// (store.py:688-690).
		var bal int64
		berr := tx.QueryRow("SELECT amount FROM balances WHERE chat_id=?", chatID).Scan(&bal)
		if berr == sql.ErrNoRows {
			bal, berr = 0, nil
		}
		if berr != nil {
			return false, 0, berr
		}
		return false, bal, tx.Commit()
	}
	if err != nil {
		return false, 0, err
	}

	// New txn -> bump balance (upsert, store.py:691-695).
	if _, err := tx.Exec(
		"INSERT INTO balances(chat_id, amount) VALUES (?, ?) "+
			"ON CONFLICT(chat_id) DO UPDATE SET amount = amount + excluded.amount",
		chatID, amount); err != nil {
		return false, 0, err
	}
	var newBal int64
	if err := tx.QueryRow("SELECT amount FROM balances WHERE chat_id=?", chatID).Scan(&newBal); err != nil {
		return false, 0, err
	}
	return true, newBal, tx.Commit()
}

// TxnExists reports whether this txn id was already processed (credited or
// marked). Read-only — does NOT consume the id — so a poller can SKIP an
// already-consumed credit when scanning history (store.py:700-709).
func (s *Store) TxnExists(txnID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM processed WHERE txn_id=?", txnID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// AmbiguousTxnExists reports whether this transfer was already quarantined as
// unattributable by the hands-free watcher (see the ambiguous_txns table in
// store.go for what that means and why it is durable).
//
// Read-only, and keyed on the PK, so the watcher can ask it about every
// transfer on every tick. Deliberately separate from TxnExists: that one means
// "consumed — already settled, never act on it again", while this one means
// "arrived, deliberately NOT settled, and must never be settled automatically".
// A transfer here is still waiting for a human.
func (s *Store) AmbiguousTxnExists(txnID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM ambiguous_txns WHERE txn_id=?", txnID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// MarkAmbiguousTxn quarantines a transfer the watcher refused to attribute.
//
// Idempotent by the txn_id PK: the watcher re-sees the same transfer on every
// tick for as long as it stays unattributable, and the FIRST sighting is the
// one worth keeping (first_seen is how long the money has been waiting on a
// human). A repeat insert is therefore ignored, not an error.
func (s *Store) MarkAmbiguousTxn(txnID, provider, amount string, ts float64) error {
	_, err := s.db.Exec(
		"INSERT INTO ambiguous_txns(txn_id, provider, amount, first_seen) VALUES (?,?,?,?) "+
			"ON CONFLICT(txn_id) DO NOTHING",
		txnID, provider, amount, ts)
	return err
}

// MarkTxn records a txn as processed WITHOUT crediting balance
// (store.py:712-727). Used for QR PURCHASES: the money pays for accounts
// directly, so it must NOT also top up the wallet (free credit). Returns true
// if this txn is new (caller delivers), false if already seen (dedupe — caller
// must not deliver twice). Atomic via the processed PK, detected by driver
// error code exactly like Credit.
func (s *Store) MarkTxn(txnID, chatID string, amount int64) (bool, error) {
	_, err := s.db.Exec("INSERT INTO processed(txn_id, chat_id, amount, ts) VALUES (?,?,?,?)",
		txnID, chatID, amount, now())
	if isPKConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UnmarkTxn releases a processed-txn claim — the compensating action when a
// settle FAILS before any money moved, so the credit isn't permanently burned
// and a later poll/reconcile can retry it. Idempotent (no-op if absent). Only
// safe to call when the caller has confirmed nothing was delivered/credited
// for this txn (store.py:730-736).
func (s *Store) UnmarkTxn(txnID string) error {
	_, err := s.db.Exec("DELETE FROM processed WHERE txn_id=?", txnID)
	return err
}

// CreditBalance adds `amount` to a chat's balance directly, with NO txn dedupe
// (store.py:838-851). Used for REFUNDS (paid but undeliverable) — the money
// becomes spendable wallet credit. Returns the new balance.
func (s *Store) CreditBalance(chatID string, amount int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"INSERT INTO balances(chat_id, amount) VALUES (?, ?) "+
			"ON CONFLICT(chat_id) DO UPDATE SET amount = amount + excluded.amount",
		chatID, amount); err != nil {
		return 0, err
	}
	var newBal int64
	if err := tx.QueryRow("SELECT amount FROM balances WHERE chat_id=?", chatID).Scan(&newBal); err != nil {
		return 0, err
	}
	return newBal, tx.Commit()
}

// Debit atomically spends `amount` from balance (store.py:854-869). Returns
// (ok, newBalance). ok is false (no change) when the balance is insufficient —
// the UPDATE's WHERE guard makes the check-and-deduct a single atomic
// statement, so two concurrent debits can never overdraw.
func (s *Store) Debit(chatID string, amount int64) (bool, int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		"UPDATE balances SET amount = amount - ? WHERE chat_id = ? AND amount >= ?",
		amount, chatID, amount)
	if err != nil {
		return false, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	// Read the (possibly unchanged) balance back, 0 when no row — mirrors
	// store.py:867-869.
	var bal int64
	berr := tx.QueryRow("SELECT amount FROM balances WHERE chat_id=?", chatID).Scan(&bal)
	if berr == sql.ErrNoRows {
		bal, berr = 0, nil
	}
	if berr != nil {
		return false, 0, berr
	}
	return n == 1, bal, tx.Commit()
}
