// Package sepay ports sepay.py (Kiro Python original, 81 lines) — SePay bank
// transfer integration: build a VietQR image URL and match incoming bank
// transfers against a pending deposit's `des` (description) code.
//
// Flow (per the deposit feature, sepay.py:4-8):
//   - BuildQRURL(des): VietQR image for our TPBank account, no fixed amount,
//     with `des` as the transfer content used to identify the payment.
//   - FindMatchingTxn(des): GET the last 50 transactions, return the first
//     whose normalized transaction_content contains the normalized `des`.
//
// Bank statements uppercase content and strip punctuation, so matching
// normalizes BOTH sides to [A-Z0-9] before substring-checking (sepay.py:10-12).
// `des` is a uuid4 hex (32 chars, no hyphens) chosen precisely so it survives
// that normalization intact.
package sepay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Beneficiary account for the VietQR (holder NGO ANH DUC, TPBank by default).
// Overridable per deployment via SEPAY_BANK / SEPAY_ACCOUNT so a second
// (reseller) instance collects into ITS OWN bank account. Bank is SePay's
// VietQR short name. Mirrors sepay.py:22-26.
func bank() string {
	if v := os.Getenv("SEPAY_BANK"); v != "" {
		return v
	}
	return "TPBank"
}

func accountNumber() string {
	if v := os.Getenv("SEPAY_ACCOUNT"); v != "" {
		return v
	}
	return "00194358802"
}

// AccountNumber exposes the beneficiary account this instance is bound to —
// ambient SEPAY_ACCOUNT or the compiled default. It is the "bound" value the
// multi-tenant money-safety boot guard compares the .env against
// (telegram_bot.py:19186 `sepay.ACCOUNT_NUMBER`), and the value printed in the
// instance-config line (@19198). Reading os.Getenv mirrors Python freezing
// ACCOUNT_NUMBER at import from the ambient env (before load_env runs).
func AccountNumber() string { return accountNumber() }

// Bank exposes the VietQR bank short-name for the instance-config log line
// (telegram_bot.py:19198 `sepay.BANK`).
func Bank() string { return bank() }

// QRBase is SePay's public VietQR image endpoint (sepay.py:28).
const (
	QRBase      = "https://qr.sepay.vn/img"
	httpTimeout = 30 * time.Second
)

// TxnListURL is SePay's transactions/list endpoint (sepay.py:29). Kept a var
// (not const) so tests can redirect it to an httptest server without hitting
// the live API — the same seam handler-level SePay tests rely on.
var TxnListURL = "https://my.sepay.vn/userapi/transactions/list"

// httpClient is shared across calls; timeout matches Python's urlopen(timeout=30).
var httpClient = &http.Client{Timeout: httpTimeout}

// normalizeRe strips everything but [A-Z0-9] after uppercasing (sepay.py:44-46).
var normalizeRe = regexp.MustCompile(`[^A-Z0-9]`)

// BuildQRURL returns the VietQR image URL for our account (sepay.py:32-41).
//
// amount: fixed transfer amount in VND. Pass nil to let the payer enter any
// amount (free top-up), mirroring Python's amount=None default.
func BuildQRURL(des string, amount *int64) string {
	q := url.Values{}
	q.Set("acc", accountNumber())
	q.Set("bank", bank())
	q.Set("des", des)
	if amount != nil {
		q.Set("amount", strconv.FormatInt(*amount, 10))
	}
	return QRBase + "?" + q.Encode()
}

// normalize uppercases and strips to [A-Z0-9] so bank-mangled content still
// matches (sepay.py:44-46).
func normalize(s string) string {
	return normalizeRe.ReplaceAllString(strings.ToUpper(s), "")
}

// Transaction mirrors one element of SePay's transactions/list response body.
type Transaction struct {
	ID                 json.Number `json:"id"`
	TransactionContent string      `json:"transaction_content"`
	AmountIn           string      `json:"amount_in"`
}

// listResponse mirrors the SePay transactions/list JSON envelope
// (sepay.py:54-57): {"messages":{"success":bool},"transactions":[...]}.
type listResponse struct {
	Messages struct {
		Success bool `json:"success"`
	} `json:"messages"`
	Error        string        `json:"error"`
	Transactions []Transaction `json:"transactions"`
}

// ListTransactions GETs the most recent `limit` transactions (sepay.py:49-57).
// Returns an error on HTTP/transport/JSON failure or when the API reports
// messages.success == false.
func ListTransactions(apiKey string, limit int) ([]Transaction, error) {
	reqURL := fmt.Sprintf("%s?limit=%d", TxnListURL, limit)
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("sepay: decode response: %w", err)
	}
	if !body.Messages.Success {
		return nil, fmt.Errorf("SePay list failed: %s", body.Error)
	}
	return body.Transactions, nil
}

// MatchedTxn is the result of a successful match (sepay.py:78-80): id, amount
// (int VND, floored) and raw content so the caller can credit + dedupe by id.
type MatchedTxn struct {
	ID      string
	Amount  int64
	Content string
}

// FindMatchingTxn returns the first incoming txn whose content contains
// `des`, else (nil, nil) (sepay.py:60-81).
//
// Only counts credits (amount_in > 0). isUsed(txnID) -> bool (optional): skip
// txns already consumed (credited or marked), so after an underpaid credit is
// banked to the wallet the worker can still find a LATER, sufficient transfer
// instead of re-matching the partial.
func FindMatchingTxn(apiKey, des string, limit int, isUsed func(txnID string) bool) (*MatchedTxn, error) {
	txns, err := ListTransactions(apiKey, limit)
	if err != nil {
		return nil, err
	}
	return matchInList(txns, des, isUsed), nil
}

// matchInList is the pure matching loop factored out of FindMatchingTxn so it
// can be table-tested against canned Transaction slices with no network
// (sepay.py:70-81).
func matchInList(txns []Transaction, des string, isUsed func(txnID string) bool) *MatchedTxn {
	needle := normalize(des)
	for _, txn := range txns {
		if needle == "" || !strings.Contains(normalize(txn.TransactionContent), needle) {
			continue
		}
		tid := txn.ID.String()
		if isUsed != nil && isUsed(tid) {
			continue
		}
		// float(txn.get("amount_in", "0") or 0) in Python — amount_in comes
		// back as a numeric string; ParseFloat mirrors that coercion, then we
		// floor to int64 same as Python's int(amount_in).
		amountIn, err := strconv.ParseFloat(strings.TrimSpace(txn.AmountIn), 64)
		if err != nil {
			amountIn = 0
		}
		if amountIn > 0 {
			return &MatchedTxn{ID: tid, Amount: int64(amountIn), Content: txn.TransactionContent}
		}
	}
	return nil
}
