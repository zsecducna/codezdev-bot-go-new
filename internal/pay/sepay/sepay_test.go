package sepay

import (
	"encoding/json"
	"testing"
)

// TestNormalize mirrors sepay.py:44-46 — uppercase then strip everything but
// [A-Z0-9], so bank-mangled content still matches.
func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already clean", "ABC123", "ABC123"},
		{"lowercase", "abc123", "ABC123"},
		{"punctuation and spaces stripped", "CT DEN:ABC-123.XYZ", "CTDENABC123XYZ"},
		{"vietnamese-style statement noise", "chuyen tien MBVCB.123456.ABC123.CT tu 001", "CHUYENTIENMBVCB123456ABC123CTTU001"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize(tc.in); got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildQRURL checks the VietQR URL is built with des and an optional
// fixed amount, mirroring sepay.py:32-41.
func TestBuildQRURL(t *testing.T) {
	des := "abc123def456"
	url := BuildQRURL(des, nil)
	if want := QRBase + "?acc=" + accountNumber() + "&bank=" + bank() + "&des=" + des; url != want {
		t.Errorf("BuildQRURL(no amount) = %q, want %q", url, want)
	}
	amt := int64(50000)
	url = BuildQRURL(des, &amt)
	if want := QRBase + "?acc=" + accountNumber() + "&amount=50000&bank=" + bank() + "&des=" + des; url != want {
		t.Errorf("BuildQRURL(amount) = %q, want %q", url, want)
	}
}

// TestFindMatchingTxn_Match exercises the substring-after-normalize matcher
// (sepay.py:60-81) against a canned transaction list — no network.
func TestFindMatchingTxn_Match(t *testing.T) {
	des := "abc123def456"
	// Simulate list_transactions by directly testing the matching loop logic
	// via a hand-rolled slice (avoids hitting the network in ListTransactions).
	txns := []Transaction{
		{ID: json.Number("1"), TransactionContent: "khong lien quan", AmountIn: "10000"},
		{ID: json.Number("2"), TransactionContent: "CT DEN:ABC123DEF456.XYZ", AmountIn: "150000"},
	}
	got := matchInList(txns, des, nil)
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
	if got.ID != "2" || got.Amount != 150000 {
		t.Errorf("match = %+v, want id=2 amount=150000", got)
	}
}

// TestFindMatchingTxn_SkipsUsed verifies the isUsed callback lets the caller
// skip an already-consumed txn and keep scanning for a later one
// (sepay.py:66-68, 73-75).
func TestFindMatchingTxn_SkipsUsed(t *testing.T) {
	des := "abc123def456"
	txns := []Transaction{
		{ID: json.Number("1"), TransactionContent: "CT DEN:ABC123DEF456 (partial)", AmountIn: "50000"},
		{ID: json.Number("2"), TransactionContent: "CT DEN:ABC123DEF456 (full)", AmountIn: "150000"},
	}
	used := map[string]bool{"1": true}
	got := matchInList(txns, des, func(id string) bool { return used[id] })
	if got == nil || got.ID != "2" {
		t.Fatalf("expected match id=2, got %+v", got)
	}
}

// TestFindMatchingTxn_NoIncoming ensures a non-positive amount_in never
// matches (sepay.py:76-77).
func TestFindMatchingTxn_NoIncoming(t *testing.T) {
	des := "abc123def456"
	txns := []Transaction{
		{ID: json.Number("1"), TransactionContent: "CT DEN:ABC123DEF456", AmountIn: "0"},
	}
	if got := matchInList(txns, des, nil); got != nil {
		t.Errorf("expected no match for zero amount_in, got %+v", got)
	}
}
