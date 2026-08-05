// Package refund is the Go port of /Users/z/Desktop/Kiro/refund_store.py — the
// SQLite store for CUSTOMER refund REQUESTS.
//
// A customer asks for a refund on one of their recent orders. The bot records
// the request here for an admin to review (approve/reject). Approval routes
// into the bot's existing admin refund UI — this package never moves money, it
// only tracks the request lifecycle (refund_store.py:2-8).
//
// This package owns the `refund_requests` table inside the SAME store.db the
// rest of the bot uses (one DB file, separate domain table,
// refund_store.py:9-10). Like the Python module it opens its OWN handle on the
// shared file: single-writer via SetMaxOpenConns(1), WAL + busy_timeout=30000
// so it coexists with internal/store's writer exactly like the two Python
// modules' independent connections did (refund_store.py:52-55).
//
// Status state machine (single column `status`, refund_store.py:14-18):
//
//	pending ──► approved   (admin approved; routed into the refund UI)
//	    └─────► rejected   (admin rejected with a reason)
package refund

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"codezdev-bot/internal/dbfile"
)

// Status values (refund_store.py:35-37).
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// Store wraps the single-writer handle for the refund_requests table.
type Store struct {
	db *sql.DB
}

// Open opens the SAME store.db file the main store uses (path is the caller's
// choice — Python resolved it as the file next to the module,
// refund_store.py:32-33) with identical PRAGMAs: WAL + busy_timeout=30000
// (refund_store.py:54-55). `_txlock=immediate` mirrors internal/store so
// read-modify-write flows take the writer lock up front.
func Open(path string) (*Store, error) {
	// This is the SAME store.db that holds cleartext customer API keys, and
	// this package can be the first to open it on a fresh install, so it must
	// apply the identical 0600 pre-create rather than rely on internal/store
	// having gone first.
	if err := dbfile.Precreate(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("refund: open %s: %w", path, err)
	}
	// Single writer: the 1-connection pool replaces refund_store.py's module
	// _LOCK (refund_store.py:31).
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("refund: init schema: %w", err)
	}
	// First write done -> tighten the WAL sidecars the driver just created.
	// Non-fatal, same reasoning as internal/store.
	if _, err := dbfile.Tighten(path); err != nil {
		log.Println("refund: could not tighten db file permissions:", err)
	}
	return s, nil
}

// Close releases the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// initSchema creates the refund_requests table if absent (idempotent) —
// verbatim port of refund_store.py _init() (refund_store.py:62-80).
func (s *Store) initSchema() error {
	_, err := s.db.Exec(
		"CREATE TABLE IF NOT EXISTS refund_requests (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, " +
			"des TEXT, " + //                   order id this request is about
			"chat_id TEXT, " + //               requesting customer
			"product TEXT, " + //               snapshot of order product
			"tier TEXT, " + //                  snapshot of order tier code
			"total INTEGER, " + //              snapshot of paid total (VND)
			"order_ts REAL, " + //              snapshot of order timestamp
			"status TEXT, " +
			"predated_suspend INTEGER, " + //   1 if product was NOT suspended at request time
			"product_suspend_ts REAL, " + //    product suspend ts at request time, or NULL
			"admin_id INTEGER, " + //           admin who decided
			"reject_reason TEXT, " +
			"ts REAL, " + //                    created
			"updated_ts REAL)") //              last status change
	return err
}

// Request mirrors one `refund_requests` row (refund_store.py:66-80).
// PredatedSuspend snapshots whether the product was NOT yet suspended when the
// customer filed — i.e. the customer detected the problem before the admins
// did (refund_store.py:19-23). Nullable columns are pointers.
type Request struct {
	ID               int64
	Des              string
	ChatID           string
	Product          string
	Tier             string
	Total            int64
	OrderTS          float64
	Status           string
	PredatedSuspend  bool
	ProductSuspendTS *float64 // NULL when the product was active at filing time
	AdminID          *int64   // NULL until an admin decides
	RejectReason     *string
	TS               float64
	UpdatedTS        float64
}

// requestCols is the explicit SELECT list matching scanRequest.
const requestCols = "id, des, chat_id, product, tier, total, order_ts, status, " +
	"predated_suspend, product_suspend_ts, admin_id, reject_reason, ts, updated_ts"

// rowScanner abstracts *sql.Row / *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanRequest converts one row into a Request, mapping SQL NULLs to nil
// pointers / zero values the same way sqlite3.Row -> dict carried None.
func scanRequest(r rowScanner) (*Request, error) {
	var (
		req                        Request
		des, chatID, product, tier sql.NullString
		total                      sql.NullInt64
		orderTS                    sql.NullFloat64
		status                     sql.NullString
		predated                   sql.NullInt64
		suspendTS                  sql.NullFloat64
		adminID                    sql.NullInt64
		rejectReason               sql.NullString
		ts, updatedTS              sql.NullFloat64
	)
	if err := r.Scan(&req.ID, &des, &chatID, &product, &tier, &total, &orderTS,
		&status, &predated, &suspendTS, &adminID, &rejectReason, &ts, &updatedTS); err != nil {
		return nil, err
	}
	req.Des, req.ChatID, req.Product, req.Tier = des.String, chatID.String, product.String, tier.String
	req.Total, req.OrderTS, req.Status = total.Int64, orderTS.Float64, status.String
	req.PredatedSuspend = predated.Int64 != 0
	if suspendTS.Valid && suspendTS.Float64 > 0 {
		// A stored suspend ts of exactly 0 is a falsy/absent value in
		// Python (`if susp.get("ts")`) — treat it the same as NULL rather
		// than handing callers a "suspended at the Unix epoch" timestamp
		// that would collapse their warranty-clock maths to 0.
		v := suspendTS.Float64
		req.ProductSuspendTS = &v
	}
	if adminID.Valid {
		v := adminID.Int64
		req.AdminID = &v
	}
	if rejectReason.Valid {
		v := rejectReason.String
		req.RejectReason = &v
	}
	req.TS, req.UpdatedTS = ts.Float64, updatedTS.Float64
	return &req, nil
}

// now returns epoch seconds as float64 (Python time.time()).
func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// CreateRequest inserts a freshly-filed refund request (status pending) and
// returns its row id (refund_store.py:86-97). productSuspendTS is nil when the
// product was still active at filing time.
func (s *Store) CreateRequest(des, chatID, product, tier string, total int64,
	orderTS float64, predatedSuspend bool, productSuspendTS *float64) (int64, error) {
	nowTS := now()
	pred := 0
	if predatedSuspend {
		pred = 1
	}
	var suspend any
	if productSuspendTS != nil {
		suspend = *productSuspendTS
	}
	res, err := s.db.Exec(
		"INSERT INTO refund_requests(des, chat_id, product, tier, total, "+
			"order_ts, status, predated_suspend, product_suspend_ts, ts, updated_ts) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		des, chatID, product, tier, total, orderTS,
		StatusPending, pred, suspend, nowTS, nowTS)
	if err != nil {
		return 0, err
	}
	// cur.lastrowid equivalent (refund_store.py:97).
	return res.LastInsertId()
}

// GetRequest returns the request row, or nil if absent
// (refund_store.py:100-106).
func (s *Store) GetRequest(reqID int64) (*Request, error) {
	req, err := scanRequest(s.db.QueryRow(
		"SELECT "+requestCols+" FROM refund_requests WHERE id=?", reqID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return req, err
}

// RequestsByStatus returns requests whose status is in `statuses`, newest
// first (refund_store.py:109-117). Empty statuses -> empty result, like
// Python's early return.
func (s *Store) RequestsByStatus(statuses []string, limit int) ([]*Request, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	// Build "?,?,..." for the IN list (mirror of ",".join("?"*n)).
	ph := ""
	for i := range statuses {
		if i > 0 {
			ph += ","
		}
		ph += "?"
	}
	args := make([]any, 0, len(statuses)+1)
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, limit)
	rows, err := s.db.Query(
		"SELECT "+requestCols+" FROM refund_requests WHERE status IN ("+ph+") "+
			"ORDER BY ts DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

// CountPending returns the number of requests still awaiting an admin decision
// — for the admin-menu badge (refund_store.py:120-124).
func (s *Store) CountPending() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM refund_requests WHERE status=?",
		StatusPending).Scan(&n)
	return n, err
}

// OpenRequestForOrder returns the PENDING request for order `des`, or nil.
// Used as the duplicate guard so a customer can't stack multiple open requests
// on one order (refund_store.py:127-134).
func (s *Store) OpenRequestForOrder(des string) (*Request, error) {
	req, err := scanRequest(s.db.QueryRow(
		"SELECT "+requestCols+" FROM refund_requests WHERE des=? AND status=? "+
			"ORDER BY ts DESC LIMIT 1", des, StatusPending))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return req, err
}

// AdminDecide is the admin approve/reject: atomic `pending` ->
// approved|rejected (refund_store.py:137-155). Only a `pending` request can
// transition (UPDATE ... WHERE status='pending', RowsAffected()==1 gate), so a
// double-tap or a stale button can't decide twice. Returns the updated request
// if THIS call won the transition, else nil. reason is nil for approvals.
func (s *Store) AdminDecide(reqID, adminID int64, approve bool, reason *string) (*Request, error) {
	newStatus := StatusRejected
	if approve {
		newStatus = StatusApproved
	}
	var reasonVal any
	if reason != nil {
		reasonVal = *reason
	}
	// One BEGIN IMMEDIATE transaction covers guard + read-back (mirror of the
	// single Python connection block).
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		"UPDATE refund_requests SET status=?, admin_id=?, reject_reason=?, "+
			"updated_ts=? WHERE id=? AND status=?",
		newStatus, adminID, reasonVal, now(), reqID, StatusPending)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		// Lost the transition race (or unknown id) — caller must not act.
		return nil, tx.Commit()
	}
	req, err := scanRequest(tx.QueryRow(
		"SELECT "+requestCols+" FROM refund_requests WHERE id=?", reqID))
	if err != nil {
		return nil, err
	}
	return req, tx.Commit()
}
