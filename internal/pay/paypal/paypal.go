// Package paypal ports paypal.py (Kiro Python original, 159 lines) — PayPal
// integration for international (USD) payments.
//
// Prices are stored in VND; PayPal cannot charge VND, so the VND total is
// converted to USD with the fixed USD_TO_VND rate from .env. Flow
// (paypal.py:4-18):
//
//  1. CreateOrder(vndTotal, des) -> (orderID, approveURL)
//     Creates a PayPal Orders v2 order (intent=CAPTURE) whose purchase unit
//     carries custom_id = des, so the webhook can map a completed payment
//     back to our internal order. Returns the approveURL the customer taps.
//  2. Customer approves on paypal.com.
//  3. PayPal POSTs a CHECKOUT.ORDER.APPROVED webhook to our listener, which
//     verifies the signature (VerifyWebhook) and calls CaptureOrder.
//  4. CaptureOrder(orderID) -> result with status + custom_id, so the caller
//     can deliver the matching internal order exactly once.
//
// All HTTP uses stdlib net/http (no extra deps), mirroring sepay.go.
package paypal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// apiBases mirrors Python's _API dict (paypal.py:26-29): sandbox vs
// production PayPal REST hosts, selected by PAYPAL_ENVIRONMENT.
var apiBases = map[string]string{
	"sandbox":    "https://api-m.sandbox.paypal.com",
	"production": "https://api-m.paypal.com",
}

const httpTimeout = 30 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

// Configured reports whether PayPal API credentials are present (client id +
// secret). Used to skip the webhook listener + hide PayPal on instances that
// don't use it (e.g. a reseller), so no port is bound and no dead button is
// shown (paypal.py:32-38).
func Configured() bool {
	return os.Getenv("PAYPAL_CLIENT_ID") != "" && os.Getenv("PAYPAL_SECRET_KEY") != ""
}

// base resolves the active PayPal REST host from PAYPAL_ENVIRONMENT,
// defaulting to sandbox on unset/unknown values (paypal.py:41-43).
func base() string {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("PAYPAL_ENVIRONMENT")))
	if b, ok := apiBases[env]; ok {
		return b
	}
	return apiBases["sandbox"]
}

// VndToUSD converts a VND amount to a USD string ("12.34") using USD_TO_VND.
//
// Rounds UP to the cent so we never undercharge. Returns an error if the rate
// env var is missing/invalid — better to fail loudly than charge a wrong
// amount (paypal.py:46-55).
func VndToUSD(vnd int64) (string, error) {
	rateStr, ok := os.LookupEnv("USD_TO_VND")
	if !ok {
		return "", fmt.Errorf("paypal: USD_TO_VND is not set")
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return "", fmt.Errorf("paypal: invalid USD_TO_VND %q: %w", rateStr, err)
	}
	usd := float64(vnd) / rate
	// Python: cents = int(usd*100) + (1 if (usd*100) % 1 else 0)  -> ceil to cent.
	cents := math.Ceil(usd*100 - 1e-9)
	if cents == 0 {
		cents = 0 // avoid printing "-0.00" when usd rounds to exactly zero
	}
	return fmt.Sprintf("%.2f", cents/100), nil
}

// tokenResponse is the OAuth2 client_credentials grant response.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

// token fetches an OAuth2 access token (client_credentials grant, paypal.py:58-69).
func token() (string, error) {
	cid := os.Getenv("PAYPAL_CLIENT_ID")
	secret := os.Getenv("PAYPAL_SECRET_KEY")
	form := url.Values{"grant_type": {"client_credentials"}}
	auth := base64.StdEncoding.EncodeToString([]byte(cid + ":" + secret))

	req, err := http.NewRequest(http.MethodPost, base()+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Python's urlopen raises on a non-2xx here (bad credentials → immediate,
	// clear failure). Mirror that: without this check a 401 body would decode
	// to an empty token and only fail later, downstream, with a confusing
	// unauthorized error on the actual API call.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return "", fmt.Errorf("PayPal POST /v1/oauth2/token failed: %d %s", resp.StatusCode, snippet)
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("paypal: decode token response: %w", err)
	}
	// A 2xx with no access_token is still unusable — fail here, not downstream
	// (Python's json.load(resp)["access_token"] KeyErrors for the same reason).
	if tr.AccessToken == "" {
		return "", fmt.Errorf("paypal: token response carried no access_token")
	}
	return tr.AccessToken, nil
}

// apiCall calls a PayPal REST endpoint and returns the parsed JSON body as a
// map (mirrors Python's _api returning the raw decoded JSON, paypal.py:72-83).
// Raises (returns an error) on a non-2xx HTTP response, including the first
// 300 bytes of the error body like Python does.
func apiCall(method, path, tok string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("PayPal %s %s failed: %d %s", method, path, resp.StatusCode, snippet)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("paypal: decode response: %w", err)
		}
	}
	return out, nil
}

// CreateOrder creates a CAPTURE order for the USD equivalent of vndTotal.
//
// custom_id = des links the PayPal order back to our internal order. Returns
// (orderID, approveURL). approveURL is the link the customer taps to pay.
// Mirrors paypal.py:86-108.
func CreateOrder(vndTotal int64, des, description string) (orderID string, approveURL string, err error) {
	tok, err := token()
	if err != nil {
		return "", "", err
	}
	usd, err := VndToUSD(vndTotal)
	if err != nil {
		return "", "", err
	}
	if description == "" {
		description = "Account purchase"
	}
	// PayPal caps description at 127 CHARACTERS; Python slices characters
	// ((description)[:127]). Truncate by runes, not bytes — a byte slice could
	// split a multi-byte character (Vietnamese product names) mid-sequence and
	// ship a mangled U+FFFD into the order.
	if r := []rune(description); len(r) > 127 {
		description = string(r[:127])
	}
	payload := map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{{
			"custom_id":   des,
			"description": description,
			"amount": map[string]any{
				"currency_code": "USD",
				"value":         usd,
			},
		}},
		"application_context": map[string]any{
			"shipping_preference": "NO_SHIPPING",
			"user_action":         "PAY_NOW",
		},
	}
	order, err := apiCall(http.MethodPost, "/v2/checkout/orders", tok, payload)
	if err != nil {
		return "", "", err
	}
	orderID, _ = order["id"].(string)
	if links, ok := order["links"].([]any); ok {
		for _, l := range links {
			link, ok := l.(map[string]any)
			if !ok {
				continue
			}
			if rel, _ := link["rel"].(string); rel == "approve" {
				approveURL, _ = link["href"].(string)
				break
			}
		}
	}
	return orderID, approveURL, nil
}

// CaptureResult is the outcome of capturing an approved order.
type CaptureResult struct {
	Status    string
	CustomID  string
	AmountUSD string
}

// CaptureOrder captures an approved order.
//
// Idempotent on PayPal's side: capturing an already-captured order returns an
// error we surface; the caller dedupes delivery by our internal order id.
// Mirrors paypal.py:111-124.
func CaptureOrder(orderID string) (CaptureResult, error) {
	tok, err := token()
	if err != nil {
		return CaptureResult{}, err
	}
	res, err := apiCall(http.MethodPost, fmt.Sprintf("/v2/checkout/orders/%s/capture", orderID), tok, map[string]any{})
	if err != nil {
		return CaptureResult{}, err
	}
	out := CaptureResult{}
	out.Status, _ = res["status"].(string)

	purchaseUnits, _ := res["purchase_units"].([]any)
	var pu map[string]any
	if len(purchaseUnits) > 0 {
		pu, _ = purchaseUnits[0].(map[string]any)
	}
	if pu != nil {
		out.CustomID, _ = pu["custom_id"].(string)
		if payments, ok := pu["payments"].(map[string]any); ok {
			if captures, ok := payments["captures"].([]any); ok && len(captures) > 0 {
				if cap0, ok := captures[0].(map[string]any); ok {
					if amount, ok := cap0["amount"].(map[string]any); ok {
						out.AmountUSD, _ = amount["value"].(string)
					}
				}
			}
		}
	}
	return out, nil
}

// VerifyWebhook verifies a webhook notification's signature with PayPal.
//
// Uses the verify-webhook-signature API + PAYPAL_WEBHOOK_ID. Returns true
// only if PayPal reports verification_status == SUCCESS. Header lookups are
// case-insensitive (http.Header is already canonicalized by net/http, but we
// use Get() to mirror Python's manual case-insensitive scan).
//
// PAYPAL_WEBHOOK_ID is the ID PayPal assigns when you register the webhook
// URL in the dashboard (looks like '8SC4544JF5824441C') — NOT the app client
// id. Until it is set, verification fails closed (rejects every event), so a
// spoofed POST can never trigger delivery. Mirrors paypal.py:127-159.
func VerifyWebhook(headers http.Header, rawBody []byte) (bool, error) {
	webhookID := os.Getenv("PAYPAL_WEBHOOK_ID")
	if webhookID == "" {
		return false, nil
	}
	tok, err := token()
	if err != nil {
		return false, err
	}
	var webhookEvent any
	if err := json.Unmarshal(rawBody, &webhookEvent); err != nil {
		return false, fmt.Errorf("paypal: invalid webhook body JSON: %w", err)
	}
	payload := map[string]any{
		"auth_algo":         headers.Get("paypal-auth-algo"),
		"cert_url":          headers.Get("paypal-cert-url"),
		"transmission_id":   headers.Get("paypal-transmission-id"),
		"transmission_sig":  headers.Get("paypal-transmission-sig"),
		"transmission_time": headers.Get("paypal-transmission-time"),
		"webhook_id":        webhookID,
		"webhook_event":     webhookEvent,
	}
	res, err := apiCall(http.MethodPost, "/v1/notifications/verify-webhook-signature", tok, payload)
	if err != nil {
		return false, err
	}
	status, _ := res["verification_status"].(string)
	return status == "SUCCESS", nil
}
