package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"testing"
)

// TestSignedQuery verifies HMAC-SHA256 signature generation against a known
// key/expected-signature pair, independent of this package's implementation,
// mirroring binance_pay.py:92-97 (urlencode once, sign the exact wire
// string, append &signature=<hex>).
func TestSignedQuery(t *testing.T) {
	secret := "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j"
	params := url.Values{}
	params.Set("symbol", "LTCBTC")
	params.Set("side", "BUY")
	params.Set("type", "LIMIT")
	params.Set("timeInForce", "GTC")
	params.Set("quantity", "1")
	params.Set("price", "0.1")
	params.Set("recvWindow", "5000")
	params.Set("timestamp", "1499827319559")

	query := params.Encode()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	wantSig := hex.EncodeToString(mac.Sum(nil))

	got := signedQuery(secret, params)
	want := query + "&signature=" + wantSig
	if got != want {
		t.Errorf("signedQuery() = %q, want %q", got, want)
	}
}

// TestAmountToCents mirrors binance_pay.py:151-152's Decimal cent conversion.
func TestAmountToCents(t *testing.T) {
	cases := []struct {
		amount string
		want   int64
	}{
		{"12.35", 1235},
		{"0", 0},
		{"100", 10000},
		{"12.3549", 1235}, // rounds to nearest cent
	}
	for _, tc := range cases {
		got, err := amountToCents(tc.amount)
		if err != nil {
			t.Fatalf("amountToCents(%q) error: %v", tc.amount, err)
		}
		if got != tc.want {
			t.Errorf("amountToCents(%q) = %d, want %d", tc.amount, got, tc.want)
		}
	}
}

// TestMatchPayment_Basic exercises the matcher (binance_pay.py:126-159): only
// USDT, only incoming (positive), within tolerance, at/after since-grace.
func TestMatchPayment_Basic(t *testing.T) {
	sinceMS := int64(1_700_000_000_000)
	txns := []Transaction{
		{TransactionID: num("1"), TransactionTime: sinceMS - 10000, Amount: "12.35", Currency: "USDT"}, // too early (before grace)
		{TransactionID: num("2"), TransactionTime: sinceMS + 1000, Amount: "-12.35", Currency: "USDT"}, // outgoing, skip
		{TransactionID: num("3"), TransactionTime: sinceMS + 1000, Amount: "5.00", Currency: "BUSD"},   // wrong currency
		{TransactionID: num("4"), TransactionTime: sinceMS + 2000, Amount: "12.36", Currency: "USDT"},  // within tol of 12.35 (1 cent)
	}
	got, err := MatchPayment(txns, "12.35", sinceMS, nil)
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "4" {
		t.Fatalf("MatchPayment = %+v, want id=4", got)
	}
}

// TestMatchPayment_ClockGrace confirms a credit landing just before sinceMS
// but within ClockGraceMS still matches (binance_pay.py:139, 149).
func TestMatchPayment_ClockGrace(t *testing.T) {
	sinceMS := int64(1_700_000_000_000)
	txns := []Transaction{
		{TransactionID: num("1"), TransactionTime: sinceMS - ClockGraceMS + 1, Amount: "10.00", Currency: "USDT"},
	}
	got, err := MatchPayment(txns, "10.00", sinceMS, nil)
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil {
		t.Fatal("expected a match within clock grace, got nil")
	}
}

// TestMatchPayment_IsUsedSkips verifies the isUsed callback dedupe hook
// (binance_pay.py:156-157), with the "bn:" id prefix.
func TestMatchPayment_IsUsedSkips(t *testing.T) {
	sinceMS := int64(1_700_000_000_000)
	txns := []Transaction{
		{TransactionID: num("1"), TransactionTime: sinceMS + 1000, Amount: "10.00", Currency: "USDT"},
		{TransactionID: num("2"), TransactionTime: sinceMS + 2000, Amount: "10.00", Currency: "USDT"},
	}
	got, err := MatchPayment(txns, "10.00", sinceMS, func(id string) bool { return id == "bn:1" })
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "2" {
		t.Fatalf("MatchPayment = %+v, want id=2 (id=1 skipped)", got)
	}
}

// TestMatchPayment_OutOfTolerance ensures an amount outside AmountMatchTol
// never matches.
func TestMatchPayment_OutOfTolerance(t *testing.T) {
	sinceMS := int64(1_700_000_000_000)
	txns := []Transaction{
		{TransactionID: num("1"), TransactionTime: sinceMS + 1000, Amount: "10.10", Currency: "USDT"}, // 10 cents off, tol default 4
	}
	got, err := MatchPayment(txns, "10.00", sinceMS, nil)
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got != nil {
		t.Errorf("expected no match outside tolerance, got %+v", got)
	}
}

func num(s string) json.Number { return json.Number(s) }
