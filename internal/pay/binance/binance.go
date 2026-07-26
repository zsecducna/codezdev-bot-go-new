// Package binance ports binance_pay.py (Kiro Python original, 159 lines) —
// Binance USDT payments for an INDIVIDUAL Binance account (no merchant
// needed).
//
// A merchant account can't be used here (it requires business KYB the seller
// does not have), so payments are received as ordinary Binance Pay C2C
// transfers to the seller's personal Pay ID and verified by reading the
// account's pay history (binance_pay.py:2-15).
//
// Flow ("Pay with Binance" purchase option):
//  1. The bot shows the seller's Pay ID + personal QR and a UNIQUE USDT
//     amount.
//  2. The customer sends exactly that amount via Binance Pay
//     (Binance-to-Binance).
//  3. MatchPayment scans GET /sapi/v1/pay/transactions for the incoming
//     credit.
//
// C2C transfers carry NO memo/order id, so the only discriminator is the
// amount — the caller therefore assigns each pending order a unique amount
// (see the bot's unique-amount picker) so a credit can only ever match the
// order it was sent for.
//
// Endpoint (verified against the official Binance SDK + docs,
// binance_pay.py:17-24):
//
//	GET https://api.binance.com/sapi/v1/pay/transactions (weight 3000, USER_DATA)
//	HMAC-SHA256 signature over the query string; header X-MBX-APIKEY: <api key>.
//	Response: {"code":"000000","success":true,"data":[{transactionId,
//	           transactionTime(ms), amount(str, +income/-expense), currency, ...}]}
//
// The API key only needs "Enable Reading" (NO withdrawal permission).
// Stdlib-only HTTP, mirroring sepay.go / paypal.go.
package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	Base       = "https://api.binance.com"
	PayTxnPath = "/sapi/v1/pay/transactions"

	// RecvWindowMS is the max tolerated clock skew vs Binance server time.
	RecvWindowMS = 60000

	// ClockGraceMS: our order clock and Binance's transaction clock can
	// differ slightly; allow a small grace when deciding a credit landed
	// "after" the order so a genuine payment near the order's start isn't
	// missed. Kept TIGHT (real clock skew, not minutes): because unique
	// amounts are RECYCLED once an order finishes/expires, a wide grace would
	// let a credit that predates a new order — i.e. one sent for an earlier
	// order that happened to reuse the same amount — settle that new order.
	// A few seconds covers true skew while closing that replay window.
	// (binance_pay.py:40-48)
	ClockGraceMS = 5000

	httpTimeout = 30 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

// AmountMatchTol / AmountOffsetStep: fuzzy amount matching (cents) so a
// customer who drops a cent or two still confirms. A credit within
// AmountMatchTol cents of the expected amount matches; the bot's
// unique-amount picker spaces concurrent same-price orders by
// AmountOffsetStep cents (>= 2*tol+1) so the ±tol bands never overlap and a
// credit maps to exactly one order. Underpay up to AmountMatchTol is
// absorbed. Env-tunable exactly like binance_pay.py:49-58.
func AmountMatchTol() int {
	return envInt("BINANCE_MATCH_TOL", 4)
}

func AmountOffsetStep() int {
	return envInt("BINANCE_OFFSET_STEP", 10)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// UsdtToVND converts a received USDT amount to VND via USD_TO_VND (USDT ~
// USD).
//
// Used to credit the wallet by the value ACTUALLY received on a duplicate /
// second Binance payment, instead of an order's cached total (which can
// drift from what was really sent). Returns an error if the rate is
// missing/invalid (binance_pay.py:61-69).
func UsdtToVND(usdt float64) (int64, error) {
	rateStr, ok := os.LookupEnv("USD_TO_VND")
	if !ok {
		return 0, fmt.Errorf("binance: USD_TO_VND is not set")
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return 0, fmt.Errorf("binance: invalid USD_TO_VND %q: %w", rateStr, err)
	}
	// Python: int(round(float(usdt) * rate)) — round-half-to-even via
	// Python's round(); math.Round (half-away-from-zero) is the stdlib
	// equivalent and matches for all realistic (non-tie) VND amounts.
	return int64(math.Round(usdt * rate)), nil
}

// PayID is the seller's Binance Pay ID to show the customer ("" if unset,
// binance_pay.py:72-74).
func PayID() string {
	// TrimSpace mirrors Python's .strip() (binance_pay.py:74) — a padded env
	// value must not reach the customer-facing message or QR path verbatim.
	return strings.TrimSpace(os.Getenv("BINANCE_PAY_ID"))
}

// QRURL is the optional hosted image URL of the seller's personal Binance
// Pay QR ("" if unset, binance_pay.py:77-79).
func QRURL() string {
	// .strip() parity with binance_pay.py:79.
	return strings.TrimSpace(os.Getenv("BINANCE_QR_URL"))
}

// QRPath is the optional local file path of the seller's personal Binance
// Pay QR ("" if unset, binance_pay.py:82-84).
func QRPath() string {
	// .strip() parity with binance_pay.py:84 — a stray trailing space would
	// break the local-file QR lookup.
	return strings.TrimSpace(os.Getenv("BINANCE_QR_PATH"))
}

// Configured reports whether the read keys are set (gate the pay button,
// binance_pay.py:87-89).
func Configured() bool {
	return os.Getenv("BINANCE_API_KEY") != "" && os.Getenv("BINANCE_SECRET_KEY") != ""
}

// signedQuery returns "k=v&...&signature=<hmac>" — the exact query string
// sent on the wire (urlencoded once, then signed) so the signature always
// matches (binance_pay.py:92-97).
func signedQuery(secret string, params url.Values) string {
	query := params.Encode()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	sig := hex.EncodeToString(mac.Sum(nil))
	return query + "&signature=" + sig
}

// signatureRe matches the signed-query parameter in any error text: the
// literal "signature=" followed by its hex value, up to the next separator.
//
// Anchored on the parameter NAME rather than on the secret's value, because
// the value is derived per request — there is no fixed string to search for,
// and by the time an error exists the caller no longer has the signature to
// compare against. Written to match whether the parameter is first ("?sig...")
// or later ("&sig..."), and to stop at the next '&' or whitespace so a URL
// with trailing parameters is only partially rewritten, not truncated.
// The `+` (not `*`) is what makes redactSignature idempotent: the placeholder
// it writes is "signature=<redacted>", and '<' is not a hex digit, so a second
// pass finds nothing to replace.
var signatureRe = regexp.MustCompile(`signature=[0-9a-fA-F]+`)

// redactSignature replaces the HMAC in an error's text with a placeholder,
// returning a plain error rather than a wrapper.
//
// It deliberately DISCARDS the original error's type: keeping it via a custom
// Error() would leave the real *url.Error reachable through errors.As/Unwrap,
// and anything that then formatted the unwrapped value would print the
// signature again — which is precisely the accident this function exists to
// prevent. Nothing in this package's callers type-asserts on the transport
// error, so the text is all that is load-bearing.
//
// Idempotent: running it over already-redacted text is a no-op, because the
// placeholder does not match the pattern.
func redactSignature(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(signatureRe.ReplaceAllString(err.Error(), "signature=<redacted>"))
}

// Transaction mirrors one element of Binance's pay/transactions response.
type Transaction struct {
	TransactionID   json.Number `json:"transactionId"`
	TransactionTime int64       `json:"transactionTime"`
	Amount          string      `json:"amount"`
	Currency        string      `json:"currency"`
}

// payTxnResponse mirrors the /sapi/v1/pay/transactions envelope
// (binance_pay.py:20-21).
type payTxnResponse struct {
	Code         string        `json:"code"`
	Success      *bool         `json:"success"`
	Msg          string        `json:"msg"`
	ErrorMessage string        `json:"errorMessage"`
	Data         []Transaction `json:"data"`
}

// GetPayTransactions GETs the signed Pay trade history (list of txns).
// Returns an error on transport/HTTP/API failure.
//
// startMS (optional, pass nil to omit): only return txns at/after this
// epoch-ms (shrinks the payload). limit: max 100 (Binance cap). Mirrors
// binance_pay.py:100-123.
func GetPayTransactions(apiKey, secret string, startMS *int64, limit int) ([]Transaction, error) {
	params := url.Values{}
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	params.Set("recvWindow", strconv.Itoa(RecvWindowMS))
	params.Set("limit", strconv.Itoa(limit))
	if startMS != nil {
		params.Set("startTime", strconv.FormatInt(*startMS, 10))
	}
	reqURL := fmt.Sprintf("%s%s?%s", Base, PayTxnPath, signedQuery(secret, params))

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		// NEVER return the transport error raw. net/http returns a *url.Error
		// whose Error() embeds the full request URL, and reqURL ends in
		// "&signature=<hmac>". The caller is a 45-second poller whose sink logs
		// the error on every cycle (handlers/usdt.go binancePollerCycle), so one
		// network outage writes the signature to the journal repeatedly.
		//
		// Security warning, normal prose: the leaked value is the derived HMAC,
		// not the API secret — it authenticates only this exact query string and
		// only within RecvWindowMS, so the exposure is a replayable read of pay
		// transactions inside that window rather than key recovery. Bounded, but
		// there is no reason to emit it at all. Redaction happens HERE, at the
		// single point that produces the error, rather than at the log sites:
		// the same reasoning as the Telegram-token fix, and it covers every
		// future caller for free.
		return nil, fmt.Errorf("binance: pay/transactions request failed: %w", redactSignature(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// Signed-endpoint errors (bad signature -1022, timestamp -1021, rate
	// limit 429) come back as HTTP 4xx with a JSON {code,msg} body
	// (binance_pay.py:116-119).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("Binance pay/transactions HTTP %d: %s", resp.StatusCode, snippet)
	}
	var body payTxnResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("binance: decode response: %w", err)
	}
	success := body.Success == nil || *body.Success // default True per Python's body.get("success", True)
	if body.Code != "000000" || !success {
		msg := body.Msg
		if msg == "" {
			msg = body.ErrorMessage
		}
		return nil, fmt.Errorf("Binance pay/transactions error: %s %s", body.Code, msg)
	}
	return body.Data, nil
}

// MatchedTxn is the result of a successful match (binance_pay.py:158): id,
// amount; the caller dedupes by id.
type MatchedTxn struct {
	ID     string
	Amount float64
}

// MatchPayment returns the first INCOMING USDT credit in txns matching
// expectedAmount (a 2-decimal string, e.g. "12.35"), else (nil, nil).
//
// Matches when: currency is USDT, amount > 0 (Binance signs outgoing
// negative), the amount is within AmountMatchTol cents of expectedAmount,
// and it landed at/after the order time (minus a small clock-skew grace).
// Because the caller makes each pending order's amount unique, a credit only
// ever matches its own order.
//
// isUsed(txnID) -> bool (optional): skip credits already consumed, so a
// stale credit of a (recycled) amount can never shadow a fresh one of the
// same amount. Mirrors binance_pay.py:126-159.
func MatchPayment(txns []Transaction, expectedAmount string, sinceMS int64, isUsed func(txnID string) bool) (*MatchedTxn, error) {
	floorMS := sinceMS - ClockGraceMS
	wantCents, err := amountToCents(expectedAmount)
	if err != nil {
		return nil, err
	}
	tol := int64(AmountMatchTol())
	for _, txn := range txns {
		if !strings.EqualFold(txn.Currency, "USDT") {
			continue
		}
		amount, err := strconv.ParseFloat(txn.Amount, 64)
		if err != nil {
			continue
		}
		if amount <= 0 { // incoming only
			continue
		}
		if txn.TransactionTime < floorMS {
			continue
		}
		recvCents, err := amountToCents(txn.Amount)
		if err != nil {
			continue
		}
		if abs(recvCents-wantCents) > tol { // fuzzy: forgot-a-cent still matches
			continue
		}
		txnID := txn.TransactionID.String()
		if isUsed != nil && isUsed(fmt.Sprintf("bn:%s", txnID)) { // already consumed by another order
			continue
		}
		return &MatchedTxn{ID: txnID, Amount: amount}, nil
	}
	return nil, nil
}

// amountToCents mirrors Python's int((Decimal(str(amount)) * 100).to_integral_value())
// — parse the decimal-string amount and round to the nearest cent.
// NOTE: like the Python original (which builds amount via `float(txn.get(...))`
// before ever reaching Decimal), this still goes through a float64 parse; for
// realistic 2-6 decimal USDT amounts this introduces no observable drift at
// cent precision, but it is float-based, not exact-decimal, arithmetic
// (binance_pay.py:151-152).
func amountToCents(amount string) (int64, error) {
	f, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(f * 100)), nil
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
