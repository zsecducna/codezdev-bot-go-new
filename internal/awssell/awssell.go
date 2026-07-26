// Package awssell is the Go port of /Users/z/Desktop/Kiro/aws_sell.py — the
// SQLite store for customer AWS-account SELL requests.
//
// A customer can sell their AWS account to us: they submit the account
// details (creation period, latest invoice, account id, admin IAM
// user/pass, admin access key + secret) and a preferred payout method. The
// bot validates the AWS keys, then an admin reviews, sets a price, and the
// customer accepts/rejects the offer (aws_sell.py:4-7).
//
// This package owns the `aws_sales` table inside the SAME store.db the rest
// of the bot uses (one DB file, separate domain table, aws_sell.py:9-10).
// Like internal/refund it opens its OWN handle on the shared file:
// single-writer via SetMaxOpenConns(1), WAL + busy_timeout=30000
// (aws_sell.py:51-56) so it coexists with internal/store's writer.
//
// Status state machine (single column `status`, aws_sell.py:14-24):
//
//	validating ──► reviewing ──► priced ──► approved          (happy path)
//	    │              │            │
//	    │              │            └─► rejected_customer      (customer declined price)
//	    │              └─► rejected_admin                      (admin rejected w/ reason)
//	    └─► invalid                                            (key validation failed)
//
// SECURITY NOTE (aws_sell.py:26-29): the IAM password and secret access key
// are the *product being sold to us*, so they are intentionally stored so
// the admin can receive them. They live in plaintext in store.db (same as
// the shop's own account passwords in accounts/*/active/*.txt). Keep
// store.db file permissions tight on the host.
package awssell

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"codezdev-bot/internal/dbfile"
)

// Status values (aws_sell.py:42-48).
const (
	StatusValidating       = "validating"        // submitted; AWS key validation in progress
	StatusInvalid          = "invalid"           // validation failed (bad keys / account mismatch)
	StatusReviewing        = "reviewing"         // keys valid -> awaiting admin review
	StatusPriced           = "priced"            // admin set a price -> awaiting customer accept/reject
	StatusApproved         = "approved"          // approved by BOTH admin and customer
	StatusRejectedAdmin    = "rejected_admin"    // admin rejected with a reason
	StatusRejectedCustomer = "rejected_customer" // customer declined the offered price
)

// Store wraps the single-writer handle for the aws_sales table.
type Store struct {
	db *sql.DB
}

// Open opens the SAME store.db file the main store uses, with identical
// PRAGMAs: WAL + busy_timeout=30000 (aws_sell.py:53-56). `_txlock=immediate`
// mirrors internal/store and internal/refund so read-modify-write flows
// (set_price, admin_reject, customer_decide) take the writer lock up front.
func Open(path string) (*Store, error) {
	// Make good on the SECURITY NOTE in the package doc above: the IAM password
	// and AWS secret access key stored in this file are live credentials, so the
	// file is created 0600 rather than at the driver's umask-derived default.
	if err := dbfile.Precreate(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("awssell: open %s: %w", path, err)
	}
	// Single writer: the 1-connection pool replaces aws_sell.py's module
	// _LOCK (aws_sell.py:37).
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("awssell: init schema: %w", err)
	}
	// First write done -> the WAL sidecars now exist at the driver's default
	// mode and carry the same secrets. Non-fatal, same reasoning as
	// internal/store.
	if _, err := dbfile.Tighten(path); err != nil {
		log.Println("awssell: could not tighten db file permissions:", err)
	}
	return s, nil
}

// Close releases the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// initSchema creates the aws_sales table if absent — a verbatim, idempotent
// port of aws_sell.py _init() (aws_sell.py:59-85): one full CREATE TABLE IF
// NOT EXISTS with every column, in Python's exact order and types, id as
// AUTOINCREMENT PK (aws_sell.py:64).
//
// Deliberately NO PRAGMA table_info column-add migrations here: unlike
// store.py's orders/api_keys tables, aws_sell.py never evolved this schema,
// so every real DB (Python-created prod store.db included) already carries
// the full column set and CREATE IF NOT EXISTS no-ops on it. This matches
// architecture-go.md §4.1's "exactly as Python does" rule and mirrors
// internal/refund, whose refund_store.py original is migration-free in the
// same way. Column ORDER is not load-bearing anywhere — Go reads via the
// explicit saleCols list, Python via SELECT * -> sqlite3.Row -> dict (both
// by NAME) — so matching Python's order is purely for byte-faithfulness on
// fresh DBs.
func (s *Store) initSchema() error {
	_, err := s.db.Exec(
		"CREATE TABLE IF NOT EXISTS aws_sales (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, " +
			"chat_id TEXT, " +
			"creation_period TEXT, " + //  MM/YYYY entered by the customer (aws_sell.py:66)
			"invoice_path TEXT, " + //     local saved copy of the uploaded invoice
			"invoice_file_id TEXT, " + //  Telegram file_id fallback for re-sending
			"aws_account_id TEXT, " +
			"iam_username TEXT, " +
			"iam_password TEXT, " +
			"access_key TEXT, " +
			"secret_key TEXT, " +
			"pay_method TEXT, " + //       bank / paypal / usdt
			"pay_detail TEXT, " + //       payout destination (acct no / email / address)
			"status TEXT, " +
			"validation TEXT, " + //       human-readable validation result
			"price INTEGER, " + //         VND the admin will pay (set on approval)
			"reject_reason TEXT, " +
			"lang TEXT, " +
			"ts REAL, " + //               created
			"updated_ts REAL)") //         last status change
	return err
}

// Sale mirrors one `aws_sales` row (aws_sell.py:65-82). Nullable columns
// (populated only later in the lifecycle) are pointers, mapping SQL NULL to
// nil the same way sqlite3.Row -> dict carried None/absent.
type Sale struct {
	ID             int64
	ChatID         string
	CreationPeriod string
	InvoicePath    *string
	InvoiceFileID  *string
	AWSAccountID   string
	IAMUsername    string
	IAMPassword    string
	AccessKey      string
	SecretKey      string
	PayMethod      string
	PayDetail      string
	Status         string
	Validation     *string
	Price          *int64
	RejectReason   *string
	Lang           string
	TS             float64
	UpdatedTS      *float64
}

// saleCols is the explicit SELECT list matching scanSale.
const saleCols = "id, chat_id, creation_period, invoice_path, invoice_file_id, " +
	"aws_account_id, iam_username, iam_password, access_key, secret_key, " +
	"pay_method, pay_detail, status, validation, price, reject_reason, " +
	"lang, ts, updated_ts"

// rowScanner abstracts *sql.Row / *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanSale converts one row into a Sale, mapping SQL NULLs to nil
// pointers / zero values the same way sqlite3.Row -> dict carried None.
func scanSale(r rowScanner) (*Sale, error) {
	// Every column is scanned through a Null* wrapper: the table declares no
	// NOT NULL constraints (aws_sell.py:63-82), and the invoice/validation/
	// price/reject/updated_ts fields are NULL by design until later lifecycle
	// steps fill them — plain string/float64 destinations would fail to Scan
	// a SQL NULL, so nothing here is scanned directly.
	var (
		sale                                                         Sale
		chatID, creationPeriod                                       sql.NullString
		invoicePath, invoiceFileID                                   sql.NullString
		awsAccountID, iamUsername, iamPassword, accessKey, secretKey sql.NullString
		payMethod, payDetail, status, validation, rejectReason, lang sql.NullString
		price                                                        sql.NullInt64
		ts, updatedTS                                                sql.NullFloat64
	)
	if err := r.Scan(&sale.ID, &chatID, &creationPeriod, &invoicePath,
		&invoiceFileID, &awsAccountID, &iamUsername, &iamPassword,
		&accessKey, &secretKey, &payMethod, &payDetail,
		&status, &validation, &price, &rejectReason, &lang, &ts,
		&updatedTS); err != nil {
		return nil, err
	}
	sale.ChatID, sale.CreationPeriod = chatID.String, creationPeriod.String
	sale.AWSAccountID, sale.IAMUsername, sale.IAMPassword = awsAccountID.String, iamUsername.String, iamPassword.String
	sale.AccessKey, sale.SecretKey = accessKey.String, secretKey.String
	sale.PayMethod, sale.PayDetail, sale.Status, sale.Lang = payMethod.String, payDetail.String, status.String, lang.String
	sale.TS = ts.Float64
	if invoicePath.Valid {
		v := invoicePath.String
		sale.InvoicePath = &v
	}
	if invoiceFileID.Valid {
		v := invoiceFileID.String
		sale.InvoiceFileID = &v
	}
	if validation.Valid {
		v := validation.String
		sale.Validation = &v
	}
	if price.Valid {
		v := price.Int64
		sale.Price = &v
	}
	if rejectReason.Valid {
		v := rejectReason.String
		sale.RejectReason = &v
	}
	if updatedTS.Valid {
		v := updatedTS.Float64
		sale.UpdatedTS = &v
	}
	return &sale, nil
}

// now returns epoch seconds as float64 (Python time.time()).
func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// CreateSale inserts a freshly-submitted sell request; returns its new row
// id (aws_sell.py:88-106). The row always starts in `validating` — every
// call site in telegram_bot.py uses the default status, so the key-
// validation worker is the only path that promotes it to `reviewing` or
// `invalid` (aws_sell.py:93-94).
func (s *Store) CreateSale(chatID, creationPeriod, awsAccountID, iamUsername,
	iamPassword, accessKey, secretKey, payMethod, payDetail, lang string) (int64, error) {
	nowTS := now()
	res, err := s.db.Exec(
		"INSERT INTO aws_sales(chat_id, creation_period, aws_account_id, "+
			"iam_username, iam_password, access_key, secret_key, pay_method, "+
			"pay_detail, status, lang, ts, updated_ts) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		chatID, creationPeriod, awsAccountID, iamUsername, iamPassword,
		accessKey, secretKey, payMethod, payDetail, StatusValidating, lang,
		nowTS, nowTS)
	if err != nil {
		return 0, err
	}
	// cur.lastrowid equivalent (aws_sell.py:106).
	return res.LastInsertId()
}

// GetSale returns the sale row, or nil if absent (aws_sell.py:109-114).
func (s *Store) GetSale(saleID int64) (*Sale, error) {
	sale, err := scanSale(s.db.QueryRow(
		"SELECT "+saleCols+" FROM aws_sales WHERE id=?", saleID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sale, err
}

// SetInvoice records the saved invoice path + Telegram file_id for a sale
// (aws_sell.py:117-121).
func (s *Store) SetInvoice(saleID int64, invoicePath, invoiceFileID string) error {
	_, err := s.db.Exec(
		"UPDATE aws_sales SET invoice_path=?, invoice_file_id=? WHERE id=?",
		invoicePath, invoiceFileID, saleID)
	return err
}

// SetValidation stores the validation outcome text and moves the sale to
// `status` (aws_sell.py:124-131). Used by the key-validation worker to flip
// validating -> reviewing | invalid.
func (s *Store) SetValidation(saleID int64, status, validation string) error {
	_, err := s.db.Exec(
		"UPDATE aws_sales SET status=?, validation=?, updated_ts=? WHERE id=?",
		status, validation, now(), saleID)
	return err
}

// SetPrice is the admin-approval step: set the offered price (VND) and move
// to `priced` (aws_sell.py:134-150). Atomic guard: only a `reviewing` sale
// can be priced, so a double-tap or a stale button can't re-price an
// already-decided sale. Returns the updated sale if THIS call won the
// transition, else nil.
func (s *Store) SetPrice(saleID, price int64) (*Sale, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op after Commit
	res, err := tx.Exec(
		"UPDATE aws_sales SET price=?, status=?, updated_ts=? "+
			"WHERE id=? AND status=?",
		price, StatusPriced, now(), saleID, StatusReviewing)
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
	sale, err := scanSale(tx.QueryRow(
		"SELECT "+saleCols+" FROM aws_sales WHERE id=?", saleID))
	if err != nil {
		return nil, err
	}
	return sale, tx.Commit()
}

// AdminReject is the admin rejection path: record the reason and move
// `reviewing` -> `rejected_admin` (aws_sell.py:153-168). Atomic: only a
// `reviewing` sale can be rejected this way. Returns the updated sale if
// this call won the transition, else nil.
func (s *Store) AdminReject(saleID int64, reason string) (*Sale, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op after Commit
	res, err := tx.Exec(
		"UPDATE aws_sales SET reject_reason=?, status=?, updated_ts=? "+
			"WHERE id=? AND status=?",
		reason, StatusRejectedAdmin, now(), saleID, StatusReviewing)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, tx.Commit()
	}
	sale, err := scanSale(tx.QueryRow(
		"SELECT "+saleCols+" FROM aws_sales WHERE id=?", saleID))
	if err != nil {
		return nil, err
	}
	return sale, tx.Commit()
}

// CustomerDecide is the customer accept/reject of a priced offer (atomic,
// ownership-checked, aws_sell.py:171-191). Only the OWNING chat may decide,
// and only a `priced` sale can transition. accept=true -> approved (agreed
// by both); accept=false -> rejected_customer. Returns the updated sale if
// THIS call won, else nil (not owner, not priced, or already decided). The
// chat_id check here is the IDOR guard: a different customer can't
// accept/reject someone else's offer (aws_sell.py:178-179).
func (s *Store) CustomerDecide(saleID int64, chatID string, accept bool) (*Sale, error) {
	newStatus := StatusRejectedCustomer
	if accept {
		newStatus = StatusApproved
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op after Commit
	res, err := tx.Exec(
		"UPDATE aws_sales SET status=?, updated_ts=? "+
			"WHERE id=? AND chat_id=? AND status=?",
		newStatus, now(), saleID, chatID, StatusPriced)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, tx.Commit()
	}
	sale, err := scanSale(tx.QueryRow(
		"SELECT "+saleCols+" FROM aws_sales WHERE id=?", saleID))
	if err != nil {
		return nil, err
	}
	return sale, tx.Commit()
}

// SalesByStatus returns sales whose status is in `statuses`, newest first
// (aws_sell.py:194-202). Empty statuses -> empty result, like Python's
// early return.
func (s *Store) SalesByStatus(statuses []string, limit int) ([]*Sale, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	// Build "?,?,..." for the IN list (mirror of ",".join("?"*n),
	// aws_sell.py:200-201) — Repeat + TrimSuffix instead of += in a loop.
	ph := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args := make([]any, 0, len(statuses)+1)
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, limit)
	rows, err := s.db.Query(
		"SELECT "+saleCols+" FROM aws_sales WHERE status IN ("+ph+") "+
			"ORDER BY ts DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sale
	for rows.Next() {
		sale, err := scanSale(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sale)
	}
	return out, rows.Err()
}

// CountsByStatus returns {status: count} across all sales — for the admin
// menu badges (aws_sell.py:205-209).
func (s *Store) CountsByStatus() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT status, COUNT(*) FROM aws_sales GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
