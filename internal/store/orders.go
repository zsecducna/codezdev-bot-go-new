// Orders — port of the "---- orders ----" section of store.py (store.py:197-309)
// plus the admin/sweeper order queries (store.py:503-519, 817-835, 874-889) and
// the delivered-history aggregates (store.py:485-500, 1150-1157).
package store

import (
	"database/sql"
	"fmt"
)

// Order mirrors one `orders` row (schema store.py:68-71 + migrated columns
// store.py:106-117). Nullable columns are pointers so a NULL round-trips
// exactly like Python's None-carrying dicts.
type Order struct {
	Des            string
	ChatID         string
	Lang           string
	Product        string
	Tier           string
	Qty            int64
	Total          int64
	Status         string
	PaypalOrderID  *string  // NULL until set_paypal_order (store.py:216-220)
	TS             float64  // creation epoch seconds
	Code           *string  // applied promo/referral code (store.py:106-112)
	CodeKind       *string  // 'promo' | 'referral'
	ReferrerChat   *string  // who to pay on delivery (referral only)
	EntraDeletedTS *float64 // refund-delete sweep stamp (store.py:113-117)
}

// orderCols is the explicit column list every order SELECT uses — explicit
// (never SELECT *) so scanning is immune to physical column order differences
// between a freshly-created DB and an ALTER-migrated old one.
const orderCols = "des, chat_id, lang, product, tier, qty, total, status, " +
	"paypal_order_id, ts, code, code_kind, referrer_chat, entra_deleted_ts"

// rowScanner abstracts *sql.Row / *sql.Rows for the shared scan helper.
type rowScanner interface{ Scan(dest ...any) error }

// scanOrder scans one row selected with orderCols into an Order, converting
// SQL NULLs the same way sqlite3.Row -> dict did (None -> nil pointer;
// NULL text/int in non-null-by-convention columns -> zero value).
func scanOrder(r rowScanner) (*Order, error) {
	var (
		o                            Order
		lang, product, tier, status  sql.NullString
		qty, total                   sql.NullInt64
		paypal, code, kind, referrer sql.NullString
		ts, entraTS                  sql.NullFloat64
		chatID                       sql.NullString
	)
	if err := r.Scan(&o.Des, &chatID, &lang, &product, &tier, &qty, &total,
		&status, &paypal, &ts, &code, &kind, &referrer, &entraTS); err != nil {
		return nil, err
	}
	o.ChatID = chatID.String
	o.Lang, o.Product, o.Tier, o.Status = lang.String, product.String, tier.String, status.String
	o.Qty, o.Total, o.TS = qty.Int64, total.Int64, ts.Float64
	if paypal.Valid {
		v := paypal.String
		o.PaypalOrderID = &v
	}
	if code.Valid {
		v := code.String
		o.Code = &v
	}
	if kind.Valid {
		v := kind.String
		o.CodeKind = &v
	}
	if referrer.Valid {
		v := referrer.String
		o.ReferrerChat = &v
	}
	if entraTS.Valid {
		v := entraTS.Float64
		o.EntraDeletedTS = &v
	}
	return &o, nil
}

// queryOrders runs a SELECT built on orderCols and scans all rows.
func (s *Store) queryOrders(q string, args ...any) ([]*Order, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CreateOrder persists a freshly reserved order with status='reserved'
// (store.py:199-205).
func (s *Store) CreateOrder(des, chatID, lang, product, tier string, qty, total int64) error {
	_, err := s.db.Exec(
		"INSERT INTO orders(des, chat_id, lang, product, tier, qty, "+
			"total, status, paypal_order_id, ts) "+
			"VALUES (?,?,?,?,?,?,?, 'reserved', NULL, ?)",
		des, chatID, lang, product, tier, qty, total, now())
	return err
}

// GetOrder returns the order row, or nil if absent (store.py:208-213).
func (s *Store) GetOrder(des string) (*Order, error) {
	o, err := scanOrder(s.db.QueryRow(
		"SELECT "+orderCols+" FROM orders WHERE des=?", des))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// SetPaypalOrder records the PayPal order id so a webhook can map it back to
// `des` (store.py:216-220).
func (s *Store) SetPaypalOrder(des, paypalOrderID string) error {
	_, err := s.db.Exec("UPDATE orders SET paypal_order_id=? WHERE des=?",
		paypalOrderID, des)
	return err
}

// MarkEntraDeleted records that the Entra account(s) for order `des` have been
// soft-deleted, so the refund-delete admin sweep and per-order button both
// skip it from then on (store.py:223-228).
func (s *Store) MarkEntraDeleted(des string, ts float64) error {
	_, err := s.db.Exec("UPDATE orders SET entra_deleted_ts=? WHERE des=?", ts, des)
	return err
}

// UpdateOrderCode attaches (or clears) an applied promo/referral code on a
// RESERVED order and overwrites its total to the recomputed, code-adjusted
// price (store.py:231-244). code=nil clears the code. Only a reserved order is
// mutated so a paid/delivered order's recorded total can never change
// underneath delivery.
func (s *Store) UpdateOrderCode(des string, code, codeKind, referrerChat *string, total int64) error {
	_, err := s.db.Exec(
		"UPDATE orders SET code=?, code_kind=?, referrer_chat=?, total=? "+
			"WHERE des=? AND status='reserved'",
		strOrNil(code), strOrNil(codeKind), strOrNil(referrerChat), total, des)
	return err
}

// OrderDesForPaypal reverse-looks-up our `des` from a PayPal order id
// (survives restart, store.py:247-252). Returns "" if unknown.
func (s *Store) OrderDesForPaypal(paypalOrderID string) (string, error) {
	var des string
	err := s.db.QueryRow("SELECT des FROM orders WHERE paypal_order_id=?",
		paypalOrderID).Scan(&des)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return des, err
}

// ClaimOrder atomically moves an order from `fromStatus` to `toStatus`
// (store.py:255-270). Returns the order if THIS call won the transition
// (caller proceeds), or nil if the order was missing or already past
// `fromStatus` — another path already claimed it. The
// UPDATE ... WHERE status=? with a RowsAffected()==1 gate is the single atomic
// guard against double-delivery across goroutines/events
// (architecture-go.md §5).
func (s *Store) ClaimOrder(des, fromStatus, toStatus string) (*Order, error) {
	// One BEGIN IMMEDIATE transaction so the guarded UPDATE and the read-back
	// SELECT see a consistent row (mirror of Python's single `with c` conn).
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE orders SET status=? WHERE des=? AND status=?",
		toStatus, des, fromStatus)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		// Lost the race (or unknown des) — caller must not act (store.py:267-268).
		return nil, tx.Commit()
	}
	o, err := scanOrder(tx.QueryRow("SELECT "+orderCols+" FROM orders WHERE des=?" /**/, des))
	if err != nil {
		return nil, err
	}
	return o, tx.Commit()
}

// ReservedOrderForChat returns this chat's currently-reserved (unpaid) order,
// or nil (store.py:273-285). Used to detect a checkout still awaiting payment
// when the customer starts a new action. Wallet top-ups excluded.
func (s *Store) ReservedOrderForChat(chatID string) (*Order, error) {
	o, err := scanOrder(s.db.QueryRow(
		"SELECT "+orderCols+" FROM orders WHERE chat_id=? AND status='reserved' "+
			"AND product!=? ORDER BY ts DESC LIMIT 1", chatID, DepositProduct))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// OrdersForChat returns this chat's orders, newest first (store.py:288-309).
// Powers the customer /history view. statuses: optional filter (nil/empty =
// all); sinceTS: optional epoch lower bound on ts (nil = no bound).
func (s *Store) OrdersForChat(chatID string, limit int, statuses []string, sinceTS *float64) ([]*Order, error) {
	q := "SELECT " + orderCols + " FROM orders WHERE chat_id=?"
	args := []any{chatID}
	if len(statuses) > 0 {
		q += " AND status IN (" + placeholders(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	if sinceTS != nil {
		q += " AND ts>=?"
		args = append(args, *sinceTS)
	}
	q += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	return s.queryOrders(q, args...)
}

// AllOrders returns every order across ALL customers, newest first
// (store.py:503-519). statuses: optional filter; nil = all. Used by admin
// maintenance such as the Entra 'Clean Up Orders' tool.
func (s *Store) AllOrders(statuses []string) ([]*Order, error) {
	q := "SELECT " + orderCols + " FROM orders"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (" + placeholders(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += " ORDER BY ts DESC"
	return s.queryOrders(q, args...)
}

// RecentOrders returns the most recent orders, newest first (store.py:874-889).
// statuses: optional filter; nil = all statuses.
func (s *Store) RecentOrders(limit int, statuses []string) ([]*Order, error) {
	q := "SELECT " + orderCols + " FROM orders"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (" + placeholders(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	return s.queryOrders(q, args...)
}

// ReservedOrdersBefore returns reserved orders created before `tsCutoff`
// (store.py:817-824). Powers the restart-safe expiry backstop that expires
// checkouts whose per-order timer goroutine died.
func (s *Store) ReservedOrdersBefore(tsCutoff float64) ([]*Order, error) {
	return s.queryOrders(
		"SELECT "+orderCols+" FROM orders WHERE status='reserved' AND ts<?",
		tsCutoff)
}

// PaidOrders returns orders stuck in 'paid' (money in, delivery never
// completed), newest first (store.py:827-835). Surfaced by the cron reconcile
// for manual review — re-driving them moves real funds, so they are
// intentionally never auto-handled.
func (s *Store) PaidOrders() ([]*Order, error) {
	return s.queryOrders(
		"SELECT " + orderCols + " FROM orders WHERE status='paid' ORDER BY ts DESC")
}

// HasDeliveredOrder reports whether this chat has any DELIVERED account order
// (i.e. is not a first-time buyer). Wallet top-ups are excluded
// (store.py:1150-1157).
func (s *Store) HasDeliveredOrder(chatID string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		"SELECT 1 FROM orders WHERE chat_id=? AND status='delivered' "+
			"AND product!=? LIMIT 1", chatID, DepositProduct).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// ProductSummary is one row of the delivered-per-product aggregate
// (store.py:485-500).
type ProductSummary struct {
	Product  string
	Accounts int64 // Σ qty
	Paid     int64 // Σ total, VND
	Orders   int64
}

// DeliveredProductSummary aggregates a chat's DELIVERED orders per product for
// the /history summary (store.py:485-500). Aggregates in SQL over ALL
// delivered orders (no row cap), excluding wallet top-ups; ordered by amount
// paid desc.
func (s *Store) DeliveredProductSummary(chatID string) ([]ProductSummary, error) {
	rows, err := s.db.Query(
		"SELECT product, COALESCE(SUM(qty),0) AS accounts, "+
			"COALESCE(SUM(total),0) AS paid, COUNT(*) AS orders "+
			"FROM orders WHERE chat_id=? AND status='delivered' AND product!=? "+
			"GROUP BY product ORDER BY paid DESC", chatID, DepositProduct)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductSummary
	for rows.Next() {
		var (
			p  sql.NullString
			ps ProductSummary
		)
		if err := rows.Scan(&p, &ps.Accounts, &ps.Paid, &ps.Orders); err != nil {
			return nil, err
		}
		ps.Product = p.String
		out = append(out, ps)
	}
	return out, rows.Err()
}

// ReservedApikeyCredits returns the total CREDITS locked in currently-RESERVED
// (unpaid, not-yet-minted) API-key orders across ALL customers
// (store.py:367-383). For an API-key order the `qty` column holds the credit
// amount, so this is SUM(qty) over reserved apikey orders. Counts BOTH new-key
// sales and recharge top-ups. nil products = the Python default
// ("__apikey__", "__apikeyrc__").
func (s *Store) ReservedApikeyCredits(products []string) (int64, error) {
	if len(products) == 0 {
		products = []string{"__apikey__", "__apikeyrc__"}
	}
	q := fmt.Sprintf("SELECT COALESCE(SUM(qty),0) FROM orders "+
		"WHERE product IN (%s) AND status='reserved'", placeholders(len(products)))
	args := make([]any, len(products))
	for i, p := range products {
		args[i] = p
	}
	var total int64
	err := s.db.QueryRow(q, args...).Scan(&total)
	return total, err
}
