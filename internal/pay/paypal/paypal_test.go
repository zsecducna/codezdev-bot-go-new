package paypal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestVndToUSD mirrors paypal.py:46-55 — vnd_to_usd rounds UP to the cent so
// we never undercharge, and raises (returns an error here) if USD_TO_VND is
// missing/invalid.
func TestVndToUSD(t *testing.T) {
	t.Setenv("USD_TO_VND", "25000")

	cases := []struct {
		name string
		vnd  int64
		want string
	}{
		{"exact division", 25000, "1.00"},
		{"rounds up a fraction of a cent", 25001, "1.01"}, // 1.00004 -> ceil to 1.01
		{"zero", 0, "0.00"},
		{"large amount", 1234567, "49.39"}, // 49.38268 -> ceil to 49.39
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VndToUSD(tc.vnd)
			if err != nil {
				t.Fatalf("VndToUSD(%d) error: %v", tc.vnd, err)
			}
			if got != tc.want {
				t.Errorf("VndToUSD(%d) = %q, want %q", tc.vnd, got, tc.want)
			}
		})
	}
}

// TestVndToUSD_MissingRate ensures a missing USD_TO_VND fails loudly rather
// than silently defaulting (paypal.py:52 raises KeyError -> we return an
// error).
func TestVndToUSD_MissingRate(t *testing.T) {
	os.Unsetenv("USD_TO_VND")
	if _, err := VndToUSD(10000); err == nil {
		t.Error("expected an error when USD_TO_VND is unset, got nil")
	}
}

// TestConfigured mirrors paypal.py:32-38 — both client id and secret must be
// set to report configured.
func TestConfigured(t *testing.T) {
	t.Setenv("PAYPAL_CLIENT_ID", "")
	t.Setenv("PAYPAL_SECRET_KEY", "")
	if Configured() {
		t.Error("expected not configured with both env vars empty")
	}

	t.Setenv("PAYPAL_CLIENT_ID", "cid")
	t.Setenv("PAYPAL_SECRET_KEY", "")
	if Configured() {
		t.Error("expected not configured with only client id set")
	}

	t.Setenv("PAYPAL_CLIENT_ID", "cid")
	t.Setenv("PAYPAL_SECRET_KEY", "secret")
	if !Configured() {
		t.Error("expected configured with both env vars set")
	}
}

// TestBase mirrors paypal.py:41-43 — defaults to sandbox on unset/unknown.
func TestBase(t *testing.T) {
	cases := map[string]string{
		"":            apiBases["sandbox"],
		"sandbox":     apiBases["sandbox"],
		"SANDBOX":     apiBases["sandbox"],
		"production":  apiBases["production"],
		"PRODUCTION":  apiBases["production"],
		"garbage-val": apiBases["sandbox"],
	}
	for env, want := range cases {
		t.Run(env, func(t *testing.T) {
			t.Setenv("PAYPAL_ENVIRONMENT", env)
			if got := base(); got != want {
				t.Errorf("base() with PAYPAL_ENVIRONMENT=%q = %q, want %q", env, got, want)
			}
		})
	}
}

// ---- end-to-end client tests against a fake PayPal REST API -----------------

// fakePayPalAPI stands up an httptest server that mimics the PayPal Orders v2 +
// webhook-verification endpoints and repoints the sandbox base URL at it for the
// duration of the test, so CreateOrder / CaptureOrder / VerifyWebhook exercise
// the REAL HTTP path (auth header, JSON bodies, error handling) without ever
// touching live PayPal. Returns the recorder holding each decoded request body.
//
// NOT parallel-safe (mutates the package-level apiBases) — callers must not
// t.Parallel().
func fakePayPalAPI(t *testing.T, verification string) *apiRecorder {
	t.Helper()
	rec := &apiRecorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.auths = append(rec.auths, r.Header.Get("Authorization"))
		rec.bodies = append(rec.bodies, string(body))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"TOKEN123"}`))
		case r.URL.Path == "/v2/checkout/orders":
			_, _ = w.Write([]byte(`{"id":"ORD1","links":[` +
				`{"rel":"self","href":"https://x/self"},` +
				`{"rel":"approve","href":"https://x/approve/ORD1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/capture"):
			_, _ = w.Write([]byte(`{"status":"COMPLETED","purchase_units":[{"custom_id":"DES1",` +
				`"payments":{"captures":[{"amount":{"value":"10.00","currency_code":"USD"}}]}}]}`))
		case r.URL.Path == "/v1/notifications/verify-webhook-signature":
			_, _ = w.Write([]byte(`{"verification_status":"` + verification + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	old := apiBases["sandbox"]
	apiBases["sandbox"] = ts.URL
	t.Cleanup(func() { apiBases["sandbox"] = old; ts.Close() })
	t.Setenv("PAYPAL_ENVIRONMENT", "sandbox")
	t.Setenv("PAYPAL_CLIENT_ID", "cid")
	t.Setenv("PAYPAL_SECRET_KEY", "secret")
	return rec
}

// apiRecorder captures what the client actually sent to the fake API.
type apiRecorder struct {
	mu     sync.Mutex
	paths  []string
	auths  []string
	bodies []string
}

func (r *apiRecorder) snapshot() ([]string, []string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...), append([]string(nil), r.auths...), append([]string(nil), r.bodies...)
}

// TestCreateOrderAgainstFakeAPI: CreateOrder authenticates, posts an
// intent=CAPTURE order carrying custom_id = des and the ceil-to-cent USD value,
// and returns the "approve" link (paypal.py:86-108).
func TestCreateOrderAgainstFakeAPI(t *testing.T) {
	rec := fakePayPalAPI(t, "SUCCESS")
	t.Setenv("USD_TO_VND", "23000")

	id, approve, err := CreateOrder(230000, "DES1", "1x prodA (KBH)")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if id != "ORD1" || approve != "https://x/approve/ORD1" {
		t.Fatalf("CreateOrder = (%q,%q), want (ORD1, the approve link)", id, approve)
	}
	paths, auths, bodies := rec.snapshot()
	if len(paths) != 2 || paths[0] != "/v1/oauth2/token" || paths[1] != "/v2/checkout/orders" {
		t.Fatalf("request paths = %v, want token then orders", paths)
	}
	if auths[1] != "Bearer TOKEN123" {
		t.Errorf("order call Authorization = %q, want the bearer token", auths[1])
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[1]), &sent); err != nil {
		t.Fatalf("order body not JSON: %v", err)
	}
	if sent["intent"] != "CAPTURE" {
		t.Errorf("intent = %v, want CAPTURE", sent["intent"])
	}
	pu := sent["purchase_units"].([]any)[0].(map[string]any)
	if pu["custom_id"] != "DES1" {
		t.Errorf("custom_id = %v, want DES1 (the webhook's link back to our order)", pu["custom_id"])
	}
	amt := pu["amount"].(map[string]any)
	if amt["currency_code"] != "USD" || amt["value"] != "10.00" {
		t.Errorf("amount = %v, want 10.00 USD", amt)
	}
}

// TestCaptureOrderAgainstFakeAPI: CaptureOrder digs status, custom_id and the
// captured amount out of the nested response (paypal.py:111-124).
func TestCaptureOrderAgainstFakeAPI(t *testing.T) {
	fakePayPalAPI(t, "SUCCESS")

	res, err := CaptureOrder("ORD1")
	if err != nil {
		t.Fatalf("CaptureOrder: %v", err)
	}
	if res.Status != "COMPLETED" || res.CustomID != "DES1" || res.AmountUSD != "10.00" {
		t.Errorf("CaptureResult = %+v, want {COMPLETED DES1 10.00}", res)
	}
}

// TestVerifyWebhookAgainstFakeAPI: the transmission headers + PAYPAL_WEBHOOK_ID
// are forwarded verbatim and only verification_status == SUCCESS verifies
// (paypal.py:127-159).
func TestVerifyWebhookAgainstFakeAPI(t *testing.T) {
	rec := fakePayPalAPI(t, "SUCCESS")
	t.Setenv("PAYPAL_WEBHOOK_ID", "8SC4544JF5824441C")

	h := http.Header{}
	h.Set("Paypal-Auth-Algo", "SHA256withRSA")
	h.Set("Paypal-Cert-Url", "https://api.paypal.com/cert.pem")
	h.Set("Paypal-Transmission-Id", "TID")
	h.Set("Paypal-Transmission-Sig", "SIG")
	h.Set("Paypal-Transmission-Time", "2026-07-25T00:00:00Z")

	ok, err := VerifyWebhook(h, []byte(`{"event_type":"CHECKOUT.ORDER.APPROVED"}`))
	if err != nil || !ok {
		t.Fatalf("VerifyWebhook = (%t,%v), want (true,nil)", ok, err)
	}
	_, _, bodies := rec.snapshot()
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[1]), &sent); err != nil {
		t.Fatalf("verify body not JSON: %v", err)
	}
	for k, want := range map[string]string{
		"auth_algo": "SHA256withRSA", "cert_url": "https://api.paypal.com/cert.pem",
		"transmission_id": "TID", "transmission_sig": "SIG",
		"transmission_time": "2026-07-25T00:00:00Z", "webhook_id": "8SC4544JF5824441C",
	} {
		if sent[k] != want {
			t.Errorf("verify payload %s = %v, want %q", k, sent[k], want)
		}
	}
	if _, ok := sent["webhook_event"].(map[string]any); !ok {
		t.Errorf("verify payload webhook_event = %v, want the decoded event object", sent["webhook_event"])
	}
}

// TestVerifyWebhookFailsClosed pins the security rule: a non-SUCCESS verdict
// rejects, and an UNSET PAYPAL_WEBHOOK_ID verifies nothing at all — without
// even calling PayPal — so a spoofed POST can never trigger delivery
// (paypal.py:145-147).
func TestVerifyWebhookFailsClosed(t *testing.T) {
	t.Run("non-success verdict", func(t *testing.T) {
		fakePayPalAPI(t, "FAILURE")
		t.Setenv("PAYPAL_WEBHOOK_ID", "WHID")
		ok, err := VerifyWebhook(http.Header{}, []byte(`{}`))
		if err != nil || ok {
			t.Errorf("VerifyWebhook = (%t,%v), want (false,nil)", ok, err)
		}
	})
	t.Run("webhook id unset", func(t *testing.T) {
		rec := fakePayPalAPI(t, "SUCCESS")
		t.Setenv("PAYPAL_WEBHOOK_ID", "")
		ok, err := VerifyWebhook(http.Header{}, []byte(`{}`))
		if err != nil || ok {
			t.Errorf("VerifyWebhook = (%t,%v), want (false,nil)", ok, err)
		}
		if paths, _, _ := rec.snapshot(); len(paths) != 0 {
			t.Errorf("no PAYPAL_WEBHOOK_ID must short-circuit before any API call, got %v", paths)
		}
	})
}

// TestAPIErrorSurfacesStatus: a non-2xx from PayPal becomes an error carrying
// the status + a truncated body, never a silent success (paypal.py:82-83).
func TestAPIErrorSurfacesStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			_, _ = w.Write([]byte(`{"access_token":"T"}`))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"name":"UNPROCESSABLE_ENTITY"}`))
	}))
	old := apiBases["sandbox"]
	apiBases["sandbox"] = ts.URL
	t.Cleanup(func() { apiBases["sandbox"] = old; ts.Close() })
	t.Setenv("PAYPAL_ENVIRONMENT", "sandbox")
	t.Setenv("PAYPAL_CLIENT_ID", "cid")
	t.Setenv("PAYPAL_SECRET_KEY", "secret")
	t.Setenv("USD_TO_VND", "23000")

	if _, _, err := CreateOrder(230000, "DES1", "x"); err == nil {
		t.Fatal("expected an error on a 422 from PayPal")
	} else if !strings.Contains(err.Error(), "422") {
		t.Errorf("error = %v, want it to carry the HTTP status", err)
	}
}
