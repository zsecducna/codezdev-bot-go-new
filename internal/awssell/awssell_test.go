// Tests for the aws_sell.py port: create-sale round trip, invoice update,
// status transitions + counts_by_status badge, and drop-in boot on a
// hand-created Python-shaped store.db (aws_sell.py:59-85, 205-209).
package awssell

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestCreateSaleRoundTrip: create -> get -> invoice update -> get again
// (aws_sell.py:88-121).
func TestCreateSaleRoundTrip(t *testing.T) {
	s := newTestStore(t)

	id, err := s.CreateSale("42", "01/2020", "111122223333", "admin",
		"Passw0rd!", "AKIAEXAMPLE", "secretvalue", "bank", "1234567890@Bank", "en")
	if err != nil || id == 0 {
		t.Fatalf("CreateSale: id=%d err=%v", id, err)
	}

	sale, err := s.GetSale(id)
	if err != nil || sale == nil {
		t.Fatalf("GetSale: %+v err=%v", sale, err)
	}
	if sale.Status != StatusValidating {
		t.Fatalf("fresh sale status = %q, want %q", sale.Status, StatusValidating)
	}
	if sale.ChatID != "42" || sale.AWSAccountID != "111122223333" || sale.PayMethod != "bank" {
		t.Fatalf("fresh sale wrong: %+v", sale)
	}
	if sale.InvoicePath != nil || sale.Price != nil {
		t.Fatalf("fresh sale should have nil invoice/price: %+v", sale)
	}

	// set_invoice (aws_sell.py:117-121).
	if err := s.SetInvoice(id, "/data/invoices/42.pdf", "FILEID123"); err != nil {
		t.Fatalf("SetInvoice: %v", err)
	}
	sale, err = s.GetSale(id)
	if err != nil || sale == nil {
		t.Fatalf("GetSale after invoice: %+v err=%v", sale, err)
	}
	if sale.InvoicePath == nil || *sale.InvoicePath != "/data/invoices/42.pdf" {
		t.Fatalf("invoice path not set: %+v", sale)
	}
	if sale.InvoiceFileID == nil || *sale.InvoiceFileID != "FILEID123" {
		t.Fatalf("invoice file id not set: %+v", sale)
	}

	// Unknown id -> nil, no error (aws_sell.py:109-114).
	missing, err := s.GetSale(id + 999)
	if err != nil || missing != nil {
		t.Fatalf("GetSale missing: %+v err=%v", missing, err)
	}
}

// TestStatusTransitionsAndCounts drives the full state machine
// (validating -> reviewing -> priced -> approved/rejected) and checks the
// atomic guards plus counts_by_status (aws_sell.py:124-209).
func TestStatusTransitionsAndCounts(t *testing.T) {
	s := newTestStore(t)

	id, err := s.CreateSale("100", "05/2019", "999988887777", "root",
		"pw", "AKIAOTHER", "secret2", "paypal", "buyer@example.com", "vi")
	if err != nil {
		t.Fatalf("CreateSale: %v", err)
	}

	// Validation worker promotes validating -> reviewing.
	if err := s.SetValidation(id, StatusReviewing, "keys OK, account matches"); err != nil {
		t.Fatalf("SetValidation: %v", err)
	}
	sale, _ := s.GetSale(id)
	if sale.Status != StatusReviewing || sale.Validation == nil {
		t.Fatalf("after validation: %+v", sale)
	}

	// A stale set_price against a NON-reviewing sale must lose.
	id2, err := s.CreateSale("101", "06/2019", "1", "u", "p", "AK", "SK", "bank", "d", "en")
	if err != nil {
		t.Fatalf("CreateSale id2: %v", err)
	}
	if won, err := s.SetPrice(id2, 5_000_000); err != nil || won != nil {
		t.Fatalf("SetPrice on validating sale should lose: %+v err=%v", won, err)
	}

	// Admin prices the reviewing sale -> priced.
	priced, err := s.SetPrice(id, 5_000_000)
	if err != nil || priced == nil {
		t.Fatalf("SetPrice: %+v err=%v", priced, err)
	}
	if priced.Status != StatusPriced || priced.Price == nil || *priced.Price != 5_000_000 {
		t.Fatalf("priced wrong: %+v", priced)
	}
	// Double-tap: re-pricing an already-priced sale must lose (rowcount 0).
	if again, err := s.SetPrice(id, 6_000_000); err != nil || again != nil {
		t.Fatalf("stale SetPrice should lose: %+v err=%v", again, err)
	}

	// IDOR guard: a different chat_id cannot decide this sale.
	if wrong, err := s.CustomerDecide(id, "999", true); err != nil || wrong != nil {
		t.Fatalf("CustomerDecide wrong owner should lose: %+v err=%v", wrong, err)
	}
	// Owning chat accepts -> approved.
	approved, err := s.CustomerDecide(id, "100", true)
	if err != nil || approved == nil {
		t.Fatalf("CustomerDecide: %+v err=%v", approved, err)
	}
	if approved.Status != StatusApproved {
		t.Fatalf("approved wrong: %+v", approved)
	}
	// Double-tap after decision must lose.
	if again, err := s.CustomerDecide(id, "100", false); err != nil || again != nil {
		t.Fatalf("stale CustomerDecide should lose: %+v err=%v", again, err)
	}

	// AdminReject path on a separate reviewing sale.
	id3, err := s.CreateSale("200", "07/2019", "2", "u2", "p2", "AK2", "SK2", "usdt", "wallet", "en")
	if err != nil {
		t.Fatalf("CreateSale id3: %v", err)
	}
	if err := s.SetValidation(id3, StatusReviewing, "ok"); err != nil {
		t.Fatalf("SetValidation id3: %v", err)
	}
	rejected, err := s.AdminReject(id3, "suspicious account age")
	if err != nil || rejected == nil {
		t.Fatalf("AdminReject: %+v err=%v", rejected, err)
	}
	if rejected.Status != StatusRejectedAdmin || rejected.RejectReason == nil ||
		*rejected.RejectReason != "suspicious account age" {
		t.Fatalf("rejected wrong: %+v", rejected)
	}
	// Stale reject on an already-decided sale must lose.
	if again, err := s.AdminReject(id3, "again"); err != nil || again != nil {
		t.Fatalf("stale AdminReject should lose: %+v err=%v", again, err)
	}

	// SalesByStatus filters + newest first + empty-input short-circuit.
	reviewing, err := s.SalesByStatus([]string{StatusReviewing}, 50)
	if err != nil || len(reviewing) != 0 {
		t.Fatalf("SalesByStatus reviewing: n=%d err=%v", len(reviewing), err)
	}
	if empty, err := s.SalesByStatus(nil, 50); err != nil || len(empty) != 0 {
		t.Fatalf("empty statuses: n=%d err=%v", len(empty), err)
	}

	// counts_by_status: id2 still validating, id approved, id3 rejected_admin.
	counts, err := s.CountsByStatus()
	if err != nil {
		t.Fatalf("CountsByStatus: %v", err)
	}
	if counts[StatusValidating] != 1 || counts[StatusApproved] != 1 || counts[StatusRejectedAdmin] != 1 {
		t.Fatalf("counts wrong: %+v", counts)
	}
}

// TestOpenOnPythonCreatedDB hand-creates aws_sales with the EXACT CREATE
// statement aws_sell.py _init() runs (aws_sell.py:62-82) and a row inserted
// the way create_sale inserts (aws_sell.py:98-105, later-lifecycle columns
// left SQL NULL) — simulating the prod store.db the Go binary drops onto
// with zero migration (architecture-go.md §4). Open() must no-op on the
// existing table, read the Python-written row (NULLs -> nil pointers), and
// win a status transition against it.
func TestOpenOnPythonCreatedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")

	// Hand-create the table in Python's exact shape, bypassing initSchema.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS aws_sales (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id TEXT, " +
		"creation_period TEXT, invoice_path TEXT, invoice_file_id TEXT, " +
		"aws_account_id TEXT, iam_username TEXT, iam_password TEXT, " +
		"access_key TEXT, secret_key TEXT, pay_method TEXT, pay_detail TEXT, " +
		"status TEXT, validation TEXT, price INTEGER, reject_reason TEXT, " +
		"lang TEXT, ts REAL, updated_ts REAL)"); err != nil {
		t.Fatalf("create python-shaped schema: %v", err)
	}
	// Seed exactly like aws_sell.py create_sale: invoice_path/invoice_file_id/
	// validation/price/reject_reason are never in the INSERT list, so they
	// stay NULL until later lifecycle steps.
	if _, err := db.Exec("INSERT INTO aws_sales(chat_id, creation_period, "+
		"aws_account_id, iam_username, iam_password, access_key, secret_key, "+
		"pay_method, pay_detail, status, lang, ts, updated_ts) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"7", "01/2020", "111122223333", "admin", "pw", "AK", "SK",
		"bank", "acct", StatusReviewing, "en", 1.0, 1.0); err != nil {
		t.Fatalf("seed python row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Open() runs CREATE TABLE IF NOT EXISTS, which must no-op cleanly.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on python-created schema: %v", err)
	}
	defer s.Close()

	// The Python-written row must read through the normal API, with the
	// never-set columns surfacing as nil pointers.
	legacy, err := s.GetSale(1)
	if err != nil || legacy == nil {
		t.Fatalf("GetSale on python row: %+v err=%v", legacy, err)
	}
	if legacy.ChatID != "7" || legacy.Status != StatusReviewing ||
		legacy.CreationPeriod != "01/2020" || legacy.AWSAccountID != "111122223333" {
		t.Fatalf("python row wrong: %+v", legacy)
	}
	if legacy.InvoicePath != nil || legacy.Validation != nil ||
		legacy.Price != nil || legacy.RejectReason != nil {
		t.Fatalf("NULL columns should be nil pointers: %+v", legacy)
	}

	// Write path against the Python-created table: admin prices the
	// reviewing sale (atomic transition must win, aws_sell.py:134-150).
	priced, err := s.SetPrice(1, 5_000_000)
	if err != nil || priced == nil || priced.Status != StatusPriced ||
		priced.Price == nil || *priced.Price != 5_000_000 {
		t.Fatalf("SetPrice on python row: %+v err=%v", priced, err)
	}

	// Re-opening (idempotent CREATE no-op) must still work.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
}
