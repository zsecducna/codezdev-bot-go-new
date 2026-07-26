// Tests for the refund_store.py port: request lifecycle, pending badge count,
// duplicate-request guard, and the atomic pending->decided transition
// (refund_store.py:137-155).
package refund

import (
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

// TestRequestLifecycle: create -> pending badge -> duplicate guard -> decide
// once -> stale decide loses.
func TestRequestLifecycle(t *testing.T) {
	s := newTestStore(t)

	// File a request; product was NOT yet suspended (predated=true, NULL ts).
	id, err := s.CreateRequest("des1", "42", "gpt", "t1", 10000, 123.0, true, nil)
	if err != nil || id == 0 {
		t.Fatalf("CreateRequest: id=%d err=%v", id, err)
	}
	req, err := s.GetRequest(id)
	if err != nil || req == nil {
		t.Fatalf("GetRequest: %v %v", req, err)
	}
	if req.Status != StatusPending || !req.PredatedSuspend || req.ProductSuspendTS != nil {
		t.Fatalf("fresh request wrong: %+v", req)
	}
	if n, _ := s.CountPending(); n != 1 {
		t.Fatalf("pending count=%d", n)
	}
	// Duplicate guard: the open PENDING request for the order is visible.
	open, err := s.OpenRequestForOrder("des1")
	if err != nil || open == nil || open.ID != id {
		t.Fatalf("OpenRequestForOrder: %+v err=%v", open, err)
	}

	// Admin approves — wins the pending->approved transition.
	decided, err := s.AdminDecide(id, 777, true, nil)
	if err != nil || decided == nil {
		t.Fatalf("AdminDecide: %+v err=%v", decided, err)
	}
	if decided.Status != StatusApproved || decided.AdminID == nil || *decided.AdminID != 777 {
		t.Fatalf("decided wrong: %+v", decided)
	}
	// Double-tap / stale button: second decide must LOSE (rowcount 0).
	reason := "nope"
	if again, err := s.AdminDecide(id, 888, false, &reason); err != nil || again != nil {
		t.Fatalf("stale decide should lose: %+v err=%v", again, err)
	}
	// No longer pending anywhere.
	if n, _ := s.CountPending(); n != 0 {
		t.Fatalf("pending after decide=%d", n)
	}
	if open, _ := s.OpenRequestForOrder("des1"); open != nil {
		t.Fatalf("open request survived decision: %+v", open)
	}

	// Rejection path stores the reason.
	id2, err := s.CreateRequest("des2", "42", "gpt", "t1", 5000, 124.0, false, ptr(99.5))
	if err != nil {
		t.Fatalf("CreateRequest 2: %v", err)
	}
	rej, err := s.AdminDecide(id2, 777, false, &reason)
	if err != nil || rej == nil {
		t.Fatalf("reject: %+v err=%v", rej, err)
	}
	if rej.Status != StatusRejected || rej.RejectReason == nil || *rej.RejectReason != "nope" {
		t.Fatalf("reject wrong: %+v", rej)
	}
	if rej.ProductSuspendTS == nil || *rej.ProductSuspendTS != 99.5 {
		t.Fatalf("suspend ts lost: %+v", rej)
	}

	// RequestsByStatus filters + newest first.
	list, err := s.RequestsByStatus([]string{StatusApproved, StatusRejected}, 50)
	if err != nil || len(list) != 2 {
		t.Fatalf("RequestsByStatus: n=%d err=%v", len(list), err)
	}
	if empty, err := s.RequestsByStatus(nil, 50); err != nil || len(empty) != 0 {
		t.Fatalf("empty statuses: n=%d err=%v", len(empty), err)
	}
}

func ptr(f float64) *float64 { return &f }
