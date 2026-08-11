// orders_test.go — tests for orders.go's refunded_amount write path (task
// #20, stage S2): SetRefundedAmount is the ONLY writer of that column, and it
// must both persist the value it's given and refuse to succeed silently
// against an order id that doesn't exist.
package store

import (
	"database/sql"
	"strings"
	"testing"
)

// refundedAmountFor reads the raw refunded_amount column back — there is no
// Order.RefundedAmount field yet (stage S3, the stats read side, hasn't
// landed), so tests reach straight for the column the way the eventual stats
// query will.
func refundedAmountFor(t *testing.T, s *Store, des string) (int64, bool) {
	t.Helper()
	var v sql.NullInt64
	if err := s.db.QueryRow("SELECT refunded_amount FROM orders WHERE des=?", des).Scan(&v); err != nil {
		t.Fatalf("read refunded_amount(%s): %v", des, err)
	}
	return v.Int64, v.Valid
}

// TestSetRefundedAmountWrites proves the happy path: the column starts NULL
// (stage S1's invariant — nothing has ever written it) and SetRefundedAmount
// stamps the given value. Deleting the call turns this test red: the column
// stays NULL/unwritten.
func TestSetRefundedAmountWrites(t *testing.T) {
	s := newTestStore(t)
	des := NewDes()
	if err := s.CreateOrder(des, "1", "en", "prod", "BH7D", 1, 7000); err != nil {
		t.Fatal(err)
	}
	if _, ok := refundedAmountFor(t, s, des); ok {
		t.Fatal("refunded_amount is non-NULL before any write — S1's invariant broke")
	}
	if err := s.SetRefundedAmount(des, 4200); err != nil {
		t.Fatalf("SetRefundedAmount: %v", err)
	}
	got, ok := refundedAmountFor(t, s, des)
	if !ok || got != 4200 {
		t.Errorf("refunded_amount = (valid=%v, %d), want (true, 4200)", ok, got)
	}
	// A second write (e.g. a bookkeeping correction) must simply overwrite —
	// SetRefundedAmount is a plain setter, not an accumulator.
	if err := s.SetRefundedAmount(des, 0); err != nil {
		t.Fatalf("SetRefundedAmount(0): %v", err)
	}
	got, ok = refundedAmountFor(t, s, des)
	if !ok || got != 0 {
		t.Errorf("refunded_amount after overwrite = (valid=%v, %d), want (true, 0)", ok, got)
	}
}

// TestSetRefundedAmountUnknownDesErrors proves the RowsAffected guard: a
// zero-match UPDATE against an order id that doesn't exist must ERROR, never
// silently return nil. Reverting to a bare `_, err := s.db.Exec(...); return
// err` (no RowsAffected check) turns this test red — that version reports
// success for an UPDATE that touched nothing, which is exactly the "money
// recorded nowhere, ledger still says OK" defect this guard exists to catch
// (the same class of bug the migrate-usdt tool had).
func TestSetRefundedAmountUnknownDesErrors(t *testing.T) {
	s := newTestStore(t)
	err := s.SetRefundedAmount("does-not-exist", 100)
	if err == nil {
		t.Fatal("SetRefundedAmount on an unknown des returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error = %q, want it to name the offending des", err.Error())
	}
}
