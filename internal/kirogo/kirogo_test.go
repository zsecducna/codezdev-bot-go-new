package kirogo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// asAPIError is a small errors.As wrapper so test call sites read as a plain
// boolean check instead of repeating the errors.As boilerplate everywhere.
func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

// newTestClient wires a Client at the given fake server with a full Config
// (all three of BaseURL/AdminKey/AccountID set) unless the caller overrides
// fields on the returned Config before constructing their own Client.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(Config{BaseURL: srv.URL, AdminKey: "test-admin-pw", AccountID: "acct-123"}, nil)
}

// ── invariant 1: upstreamStatus=="pending" on 2xx IS success ─────────────────

func TestProvisionCustomer_PendingUpstreamIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-admin-pw" {
			t.Errorf("Authorization header = %q, want Bearer test-admin-pw", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["accountId"] != "acct-123" {
			t.Errorf("accountId = %v, want acct-123 (must always be sent explicitly)", body["accountId"])
		}
		if body["tokenLimit"] != float64(0) {
			t.Errorf("tokenLimit = %v, want 0", body["tokenLimit"])
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "pool-1", "key": "sk-abc123", "name": "order-1",
			"credits": 100, "upstreamKeyId": 1346, "upstreamBound": false,
			"upstreamStatus": "pending",
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "order-1", Credits: 100,
	})
	if err != nil {
		t.Fatalf("ProvisionCustomer returned error on pending upstream: %v", err)
	}
	if !resp.Deliverable() {
		t.Error("Deliverable() = false for a 2xx/success=true response with upstreamStatus=pending; must be true (invariant 1)")
	}
	if !resp.IsUpstreamPending() {
		t.Error("IsUpstreamPending() = false, want true")
	}
	if resp.Key != "sk-abc123" {
		t.Errorf("Key = %q, want sk-abc123", resp.Key)
	}
}

func TestProvisionCustomer_BoundUpstreamIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "pool-2", "key": "sk-def456", "name": "order-2",
			"credits": 100, "upstreamKeyId": 77, "upstreamBound": true,
			"upstreamStatus": "bound",
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "order-2", Credits: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Deliverable() || resp.IsUpstreamPending() {
		t.Errorf("bound response: Deliverable=%v IsUpstreamPending=%v, want true/false", resp.Deliverable(), resp.IsUpstreamPending())
	}
}

// ── 401 unauthorized ──────────────────────────────────────────────────────────

func TestProvisionCustomer_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": "Unauthorized"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error is not *APIError: %v (%T)", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

// ── client-side validation: credits <= 0 must never reach the network ───────

func TestProvisionCustomer_CreditsMustBePositive_NoNetworkCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "credits must be > 0"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	for _, credits := range []int64{0, -5} {
		_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: credits})
		if err != ErrCreditsMustBePositive {
			t.Errorf("credits=%d: err = %v, want ErrCreditsMustBePositive", credits, err)
		}
	}
	if called {
		t.Error("server was called despite credits <= 0 — validation must happen client-side, before any HTTP request")
	}
}

func TestProvisionCustomer_AccountIDRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called when no account id is configured")
	}))
	defer srv.Close()

	// No AccountID in Config and none passed on the request.
	c := New(Config{BaseURL: srv.URL, AdminKey: "pw"}, nil)
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10})
	if err != ErrAccountIDRequired {
		t.Errorf("err = %v, want ErrAccountIDRequired", err)
	}
}

func TestProvisionCustomer_AccountIDOverride(t *testing.T) {
	var gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotAccountID, _ = body["accountId"].(string)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "key": "sk-x", "upstreamStatus": "bound"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv) // Config.AccountID = "acct-123"
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "x", Credits: 10, AccountID: "override-acct",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccountID != "override-acct" {
		t.Errorf("accountId sent = %q, want override-acct", gotAccountID)
	}
}

// ── invariant 2: no logged bodies, no llmgate-key field ──────────────────────

func TestProvisionCustomerResponse_NoUpstreamKeyField(t *testing.T) {
	// Regression guard for invariant 2: fail the build if a field whose name
	// suggests it carries the raw upstream (llmgate) key is ever added to this
	// struct. UpstreamKeyID (the non-secret id) is explicitly allowed.
	typ := reflect.TypeOf(ProvisionCustomerResponse{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if name == "UpstreamKeyID" {
			continue
		}
		lower := strings.ToLower(name)
		if strings.Contains(lower, "upstreamkey") || strings.Contains(lower, "llmgate") {
			t.Errorf("ProvisionCustomerResponse has field %q — response types must never carry the raw upstream/llmgate key, only the non-secret UpstreamKeyID", name)
		}
	}
}

func TestDoRequest_NeverLogsResponseBody(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "pool-9", "key": "sk-SUPER-SECRET-VALUE", "upstreamStatus": "bound",
		})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, AdminKey: "test-admin-pw", AccountID: "acct-123"}, logger)
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(logged.String(), "sk-SUPER-SECRET-VALUE") {
		t.Errorf("logger output contains the cleartext key: %q", logged.String())
	}
	if strings.Contains(logged.String(), "test-admin-pw") {
		t.Errorf("logger output contains the admin key: %q", logged.String())
	}
}

// ── invariant 3: tokenLimit hardcoded to 0 on ProvisionCustomer ──────────────

func TestProvisionCustomer_TokenLimitAlwaysZero(t *testing.T) {
	var gotTokenLimit float64 = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotTokenLimit, _ = body["tokenLimit"].(float64)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "key": "sk-x", "upstreamStatus": "bound"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTokenLimit != 0 {
		t.Errorf("tokenLimit sent = %v, want 0", gotTokenLimit)
	}
}

// ── invariant 4: -1 means unlimited ───────────────────────────────────────────

func TestRemaining_IsUnlimited(t *testing.T) {
	cases := []struct {
		v    Remaining
		want bool
	}{{-1, true}, {0, false}, {50, false}, {1, false}}
	for _, tc := range cases {
		if got := tc.v.IsUnlimited(); got != tc.want {
			t.Errorf("Remaining(%d).IsUnlimited() = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestRechargeKey_UnlimitedTokensRemaining(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, hasTokens := body["tokens"]; hasTokens {
			t.Error("request body contains a 'tokens' field — recharge must send credits ONLY, never tokens")
		}
		if body["credits"] != float64(50) {
			t.Errorf("credits = %v, want 50", body["credits"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "uuid", "enabled": true,
			"creditLimit": 150, "creditsUsed": 100, "creditsRemaining": 50,
			"tokenLimit": 0, "tokensUsed": 0, "tokensRemaining": -1,
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "uuid", Credits: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.TokensRemaining.IsUnlimited() {
		t.Errorf("TokensRemaining = %d, want unlimited (-1)", resp.TokensRemaining)
	}
	if resp.CreditsRemaining != 50 {
		t.Errorf("CreditsRemaining = %d, want 50", resp.CreditsRemaining)
	}
}

func TestRechargeKey_RequiresPositiveCredits(t *testing.T) {
	c := New(Config{BaseURL: "http://unused.invalid", AdminKey: "pw"}, nil)
	if _, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "x", Credits: 0}); err != ErrCreditsMustBePositive {
		t.Errorf("err = %v, want ErrCreditsMustBePositive", err)
	}
}

func TestRechargeKey_RequiresIdentifier(t *testing.T) {
	c := New(Config{BaseURL: "http://unused.invalid", AdminKey: "pw"}, nil)
	if _, err := c.RechargeKey(context.Background(), RechargeKeyRequest{Credits: 10}); err != ErrKeyIdentifierRequired {
		t.Errorf("err = %v, want ErrKeyIdentifierRequired", err)
	}
}

// ── invariant 5: upstreamRevoked tri-state (absent vs explicit false) ───────

func TestDeleteAPIKey_UpstreamRevokedAbsent_OldServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Old server: no upstreamRevoked field at all.
		json.NewEncoder(w).Encode(map[string]any{"success": true, "id": "uuid"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DeleteAPIKey(context.Background(), DeleteAPIKeyRequest{ID: "uuid"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.UpstreamRevoked != nil {
		t.Errorf("UpstreamRevoked = %v, want nil (field absent)", *resp.UpstreamRevoked)
	}
	if resp.UpstreamStillLive() {
		t.Error("UpstreamStillLive() = true when the field was merely absent; want false (absent != false)")
	}
}

func TestDeleteAPIKey_UpstreamRevokedExplicitFalse_LoudAlert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "id": "uuid", "upstreamRevoked": false})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DeleteAPIKey(context.Background(), DeleteAPIKeyRequest{ID: "uuid"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.UpstreamRevoked == nil || *resp.UpstreamRevoked != false {
		t.Fatalf("UpstreamRevoked = %v, want explicit false", resp.UpstreamRevoked)
	}
	if !resp.UpstreamStillLive() {
		t.Error("UpstreamStillLive() = false for explicit upstreamRevoked:false; want true (loud alert)")
	}
	if !resp.Success {
		t.Error("Success = false; the delete must still report success even when upstream cleanup failed")
	}
}

func TestDeleteAPIKey_UpstreamRevokedTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "id": "uuid", "upstreamRevoked": true})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DeleteAPIKey(context.Background(), DeleteAPIKeyRequest{APIKey: "sk-abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.UpstreamStillLive() {
		t.Error("UpstreamStillLive() = true when upstreamRevoked:true; want false")
	}
}

func TestDeleteAPIKey_RequiresIdentifier(t *testing.T) {
	c := New(Config{BaseURL: "http://unused.invalid", AdminKey: "pw"}, nil)
	if _, err := c.DeleteAPIKey(context.Background(), DeleteAPIKeyRequest{}); err != ErrKeyIdentifierRequired {
		t.Errorf("err = %v, want ErrKeyIdentifierRequired", err)
	}
}

func TestDeleteAPIKey_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "API key not found"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.DeleteAPIKey(context.Background(), DeleteAPIKeyRequest{ID: "gone"})
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, want *APIError{StatusCode: 404}", err)
	}
}

// ── invariant 6: Configured() requires base URL + admin key + account id ────

func TestConfig_Configured(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"all set", Config{BaseURL: "u", AdminKey: "k", AccountID: "a"}, true},
		{"missing base url", Config{AdminKey: "k", AccountID: "a"}, false},
		{"missing admin key", Config{BaseURL: "u", AccountID: "a"}, false},
		{"missing account id", Config{BaseURL: "u", AdminKey: "k"}, false},
		{"zero value", Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Configured(); got != tc.want {
				t.Errorf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClient_NotConfigured_NoAccountID_StillAllowsRecharge(t *testing.T) {
	// RechargeKey/DeleteAPIKey/Stats don't send accountId at all, so a client
	// missing only AccountID (Configured()==false) must still be able to make
	// those calls — only ProvisionCustomer needs the account id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "uuid", "enabled": true,
			"creditLimit": 60, "creditsUsed": 0, "creditsRemaining": 60,
			"tokenLimit": 0, "tokensUsed": 0, "tokensRemaining": -1,
		})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, AdminKey: "pw"}, nil) // no AccountID
	if c.Configured() {
		t.Fatal("Configured() should be false without an account id")
	}
	if _, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "uuid", Credits: 10}); err != nil {
		t.Errorf("RechargeKey failed on a client with no AccountID: %v", err)
	}
}

func TestDoRequest_NotConfigured(t *testing.T) {
	c := New(Config{}, nil)
	_, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "x", Credits: 1})
	if err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

// ── NewAPIKey (legacy) ────────────────────────────────────────────────────────

func TestNewAPIKey_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if _, hasAccountID := body["accountId"]; hasAccountID {
			t.Error("new_api_key request must not send accountId — that's provision_customer's field")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "uuid", "key": "sk-legacy", "name": "order-12345", "credits": 100,
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.NewAPIKey(context.Background(), NewAPIKeyRequest{Name: "order-12345", Credits: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Key != "sk-legacy" {
		t.Errorf("Key = %q, want sk-legacy", resp.Key)
	}
}

func TestNewAPIKey_CreditsMustBePositive(t *testing.T) {
	c := New(Config{BaseURL: "http://unused.invalid", AdminKey: "pw"}, nil)
	if _, err := c.NewAPIKey(context.Background(), NewAPIKeyRequest{Credits: 0}); err != ErrCreditsMustBePositive {
		t.Errorf("err = %v, want ErrCreditsMustBePositive", err)
	}
}

func TestNewAPIKey_TokenLimitNegative(t *testing.T) {
	c := New(Config{BaseURL: "http://unused.invalid", AdminKey: "pw"}, nil)
	if _, err := c.NewAPIKey(context.Background(), NewAPIKeyRequest{Credits: 10, TokenLimit: -1}); err != ErrTokenLimitNegative {
		t.Errorf("err = %v, want ErrTokenLimitNegative", err)
	}
}

func TestNewAPIKey_BadRequest_CreditsServerSide(t *testing.T) {
	// Exercises the documented 400 "credits must be > 0" path via a server
	// that (hypothetically) disagrees with client-side validation, proving
	// APIError surfaces server rejections correctly end-to-end.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "credits must be > 0"})
	}))
	defer srv.Close()

	// Bypass client-side validation isn't possible via NewAPIKey's exported
	// path (Credits<=0 short-circuits locally) — call adminSend directly to
	// prove the server-side 400 decodes into a proper APIError.
	c := newTestClient(t, srv)
	err := c.adminSend(context.Background(), "POST", "/admin/new_api_key", map[string]any{"credits": 0}, &NewAPIKeyResponse{})
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want *APIError{StatusCode: 400}", err)
	}
	if !strings.Contains(apiErr.Message, "credits must be > 0") {
		t.Errorf("Message = %q, want it to contain the server's error text", apiErr.Message)
	}
}

// ── Stats ──────────────────────────────────────────────────────────────────

func TestStats_ListAndFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"id": "uuid", "name": "order-12345", "keyMasked": "sk-abc****wxyz",
				"enabled": true, "status": "active",
				"creditLimit": 100, "creditsUsed": 40, "creditsRemaining": 60,
				"tokenLimit": 0, "tokensUsed": 12345, "tokensRemaining": -1,
				"requestsCount": 20, "createdAt": 0, "lastUsedAt": 0,
			}},
			"totals": map[string]any{"keys": 1, "tokens": 12345, "credits": 40, "requests": 20},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.Stats(context.Background(), StatsRequest{Name: "order-12345"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].ID != "uuid" {
		t.Fatalf("Keys = %+v, want one entry with id uuid", resp.Keys)
	}
	if !resp.Keys[0].TokensRemaining.IsUnlimited() {
		t.Error("Keys[0].TokensRemaining should be unlimited")
	}
	if resp.Totals.Keys != 1 {
		t.Errorf("Totals.Keys = %d, want 1", resp.Totals.Keys)
	}
}

func TestStats_InvalidJSON400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "Invalid JSON"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Stats(context.Background(), StatsRequest{})
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want *APIError{StatusCode: 400}", err)
	}
}

// ── StatsResponse "keys" vs "apiKeys" wire-spelling fallback ────────────────
//
// KIRO_GO_API.md:111-124 documents the key list as "keys", but the actually
// deployed Kiro-Go server sends "apiKeys" (verified by curling POST
// /admin/stats against production). Python's original never trusted the doc
// alone (telegram_bot.py:4349, :4544: `res.get("keys") or res.get("apiKeys")
// or []`), so StatsResponse.UnmarshalJSON must carry the same fallback. These
// three tests pin: decoding the real "apiKeys" body, decoding a "keys" body,
// and precedence when both are present.

// realApiKeysStatsBody is the VERBATIM response body observed curling the
// live, deployed Kiro-Go server's POST /admin/stats with Bearer auth — not a
// simplified stand-in. It has no "keys" field at all, only "apiKeys" (plus
// the unrelated "totals.keys" COUNT, which is what made the doc's "keys"
// spelling look plausible at a glance).
const realApiKeysStatsBody = `{"apiKeys":[{"id":"e51dd91c-...","name":"tg-...-order-...","keyMasked":"sk-406****16ea",
  "saleClass":"bedrock","enabled":true,"createdAt":1786349280,"creditLimit":100,
  "tokensUsed":0,"creditsUsed":0,"requestsCount":0,"status":"active",
  "creditsRemaining":100,"tokensRemaining":-1}],
 "totals":{"creditsUsed":0,"keys":1,"requestsCount":0,"tokensUsed":0}}`

// TestStatsResponse_DecodesRealApiKeysSpelling pins the fix for the live bug:
// the deployed server's actual response body must decode into a populated
// Keys slice, not silently empty out while Stats() still reports success.
func TestStatsResponse_DecodesRealApiKeysSpelling(t *testing.T) {
	var resp StatsResponse
	if err := json.Unmarshal([]byte(realApiKeysStatsBody), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Keys) != 1 {
		t.Fatalf("Keys = %+v, want exactly one entry", resp.Keys)
	}
	if resp.Keys[0].ID != "e51dd91c-..." {
		t.Errorf("Keys[0].ID = %q, want %q", resp.Keys[0].ID, "e51dd91c-...")
	}
	if resp.Keys[0].CreditsRemaining != 100 {
		t.Errorf("Keys[0].CreditsRemaining = %v, want 100", resp.Keys[0].CreditsRemaining)
	}
	if resp.Keys[0].Status != "active" {
		t.Errorf("Keys[0].Status = %q, want %q", resp.Keys[0].Status, "active")
	}
}

// TestStatsResponse_DecodesKeysSpelling pins that the documented "keys"
// spelling still works (a future/other server may send it correctly).
func TestStatsResponse_DecodesKeysSpelling(t *testing.T) {
	body := `{"keys":[{"id":"doc-spelled","status":"active","creditsRemaining":60}],"totals":{"keys":1}}`
	var resp StatsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].ID != "doc-spelled" {
		t.Fatalf("Keys = %+v, want one entry with id doc-spelled", resp.Keys)
	}
}

// TestStatsResponse_BothSpellingsPrefersKeys pins Python's precedence
// (`res.get("keys") or res.get("apiKeys") or []`): when a response somehow
// carries both, the documented "keys" spelling wins.
func TestStatsResponse_BothSpellingsPrefersKeys(t *testing.T) {
	body := `{"keys":[{"id":"from-keys"}],"apiKeys":[{"id":"from-apiKeys"}],"totals":{"keys":1}}`
	var resp StatsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].ID != "from-keys" {
		t.Fatalf("Keys = %+v, want one entry with id from-keys", resp.Keys)
	}
}

// ── CustomerMe (customer-realm quota) ────────────────────────────────────────

func TestCustomerMe_UsesXApiKeyNotAdminHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "sk-buyer-key" {
			t.Errorf("X-Api-Key = %q, want sk-buyer-key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty on a customer-realm call", got)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "uuid", "name": "order-12345", "keyMasked": "sk-abc****wxyz",
			"status": "active", "createdAt": 0, "lastUsedAt": 0,
			"tokenLimit": 0, "creditLimit": 100, "tokensUsed": 12345, "creditsUsed": 40,
			"tokensRemaining": -1, "creditsRemaining": 60, "requestsCount": 20,
		})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil) // no admin credentials needed for a customer call
	resp, err := c.CustomerMe(context.Background(), "sk-buyer-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CreditsRemaining != 60 {
		t.Errorf("CreditsRemaining = %d, want 60", resp.CreditsRemaining)
	}
	if !resp.TokensRemaining.IsUnlimited() {
		t.Error("TokensRemaining should be unlimited")
	}
}

func TestCustomerMe_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": "Unauthorized"})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil)
	_, err := c.CustomerMe(context.Background(), "sk-bad-key")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want *APIError{StatusCode: 401}", err)
	}
}

// ── invariant 3b: two mint classes through provision_customer ───────────────

func TestProvisionCustomer_KiroClass_SkippedUpstreamIsExpectedSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, hasAccountID := body["accountId"]; hasAccountID {
			t.Errorf("kiro-class request must not send accountId, body = %v", body)
		}
		if body["saleClass"] != "kiro" {
			t.Errorf("saleClass = %v, want kiro", body["saleClass"])
		}
		if body["skipUpstream"] != true {
			t.Errorf("skipUpstream = %v, want true (hardcoded for kiro class)", body["skipUpstream"])
		}
		if body["tokenLimit"] != float64(0) {
			t.Errorf("tokenLimit = %v, want 0", body["tokenLimit"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "pool-kiro-1", "key": "sk-kiro1", "name": "order-kiro-1",
			"credits": 500, "upstreamKeyId": 0, "upstreamBound": false,
			"upstreamStatus": "skipped",
		})
	}))
	defer srv.Close()

	// No AccountID configured at all — must not matter for a kiro mint
	// (invariant 3b: ConfiguredBase(), not Configured(), gates this class).
	c := New(Config{BaseURL: srv.URL, AdminKey: "test-admin-pw"}, nil)
	resp, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "order-kiro-1", Credits: 500, SaleClass: SaleClassKiro,
	})
	if err != nil {
		t.Fatalf("ProvisionCustomer (kiro) returned error: %v", err)
	}
	if !resp.Deliverable() {
		t.Error("Deliverable() = false for a 2xx kiro mint with upstreamStatus=skipped; want true")
	}
	if !resp.IsUpstreamSkipped() {
		t.Error("IsUpstreamSkipped() = false, want true")
	}
	if resp.UpstreamStatusAnomalous(SaleClassKiro) {
		t.Error("UpstreamStatusAnomalous(kiro) = true for a skipped kiro mint; skipped is the NORMAL terminal state")
	}
}

func TestProvisionCustomer_KiroClass_AccountIDForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called: accountId on a kiro request must be rejected client-side")
	}))
	defer srv.Close()

	c := newTestClient(t, srv) // Config.AccountID = "acct-123"
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "x", Credits: 10, SaleClass: SaleClassKiro, AccountID: "should-not-be-allowed",
	})
	if err != ErrAccountIDForbiddenForKiro {
		t.Errorf("err = %v, want ErrAccountIDForbiddenForKiro", err)
	}
}

func TestProvisionCustomer_BedrockClass_SkippedUpstreamIsAnomalous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "pool-x", "key": "sk-x", "upstreamStatus": "skipped",
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "x", Credits: 10, SaleClass: SaleClassBedrock,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Still a completed, billable sale — anomalous is an alert signal, not a
	// failure signal.
	if !resp.Deliverable() {
		t.Error("Deliverable() = false; a 2xx bedrock response is always a completed sale, even if upstreamStatus is unexpected")
	}
	if !resp.UpstreamStatusAnomalous(SaleClassBedrock) {
		t.Error("UpstreamStatusAnomalous(bedrock) = false for upstreamStatus=skipped; want true (bedrock should bind or defer-bind, never skip)")
	}
}

func TestProvisionCustomer_BedrockClass_SendsExplicitSaleClass(t *testing.T) {
	var gotSaleClass any = "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotSaleClass = body["saleClass"]
		json.NewEncoder(w).Encode(map[string]any{"success": true, "key": "sk-x", "upstreamStatus": "bound"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{
		Name: "x", Credits: 10, SaleClass: SaleClassBedrock,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotSaleClass != "bedrock" {
		t.Errorf("saleClass sent = %v, want bedrock", gotSaleClass)
	}
}

func TestProvisionCustomer_LegacyClass_OmitsSaleClassField(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "key": "sk-x", "upstreamStatus": "bound"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	// SaleClass left as the zero value "" — legacy/unspecified.
	if _, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := body["saleClass"]; present {
		t.Errorf("body has a saleClass field (%v) for the legacy \"\" class; want it entirely absent", body["saleClass"])
	}
	// accountId is still required and sent for the legacy class.
	if body["accountId"] != "acct-123" {
		t.Errorf("accountId = %v, want acct-123 (legacy class still requires it)", body["accountId"])
	}
}

func TestProvisionCustomer_UnknownSaleClass(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for an unknown sale class")
	})))
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10, SaleClass: "made-up-class"})
	if err != ErrUnknownSaleClass {
		t.Errorf("err = %v, want ErrUnknownSaleClass", err)
	}
}

// ── RechargeKey sale-class guard: '' omits saleClass, bedrock/kiro send it ──

func TestRechargeKey_LegacyClass_OmitsSaleClassField(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "uuid", "enabled": true,
			"creditLimit": 60, "creditsUsed": 0, "creditsRemaining": 60,
			"tokenLimit": 0, "tokensUsed": 0, "tokensRemaining": -1,
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	// SaleClass left "" — a legacy key whose class the bot never recorded.
	if _, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "uuid", Credits: 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := body["saleClass"]; present {
		t.Errorf("body has a saleClass field (%v) for an empty SaleClass; want it entirely absent, not sent as \"\"", body["saleClass"])
	}
}

func TestRechargeKey_BedrockClass_SendsSaleClass(t *testing.T) {
	var gotSaleClass any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotSaleClass = body["saleClass"]
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "id": "uuid", "enabled": true,
			"creditLimit": 60, "creditsUsed": 0, "creditsRemaining": 60,
			"tokenLimit": 0, "tokensUsed": 0, "tokensRemaining": -1,
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "uuid", Credits: 10, SaleClass: SaleClassBedrock}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotSaleClass != "bedrock" {
		t.Errorf("saleClass sent = %v, want bedrock", gotSaleClass)
	}
}

func TestRechargeKey_UnknownSaleClass(t *testing.T) {
	c := New(Config{BaseURL: "http://unused.invalid", AdminKey: "pw"}, nil)
	_, err := c.RechargeKey(context.Background(), RechargeKeyRequest{ID: "x", Credits: 10, SaleClass: "made-up-class"})
	if err != ErrUnknownSaleClass {
		t.Errorf("err = %v, want ErrUnknownSaleClass", err)
	}
}

// ── invariant 6b: ConfiguredBase() — the kiro-class gate ─────────────────────

func TestConfig_ConfiguredBase(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"all set", Config{BaseURL: "u", AdminKey: "k", AccountID: "a"}, true},
		{"no account id, still configured-base", Config{BaseURL: "u", AdminKey: "k"}, true},
		{"missing base url", Config{AdminKey: "k"}, false},
		{"missing admin key", Config{BaseURL: "u"}, false},
		{"zero value", Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ConfiguredBase(); got != tc.want {
				t.Errorf("ConfiguredBase() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── invariant 7: retry policy — 5xx/network retried, 4xx and 2xx are not ────

func TestSend_RetriesOn5xxThenSucceeds(t *testing.T) {
	orig := retryBackoffBase
	retryBackoffBase = time.Millisecond // keep the test fast
	defer func() { retryBackoffBase = orig }()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"error": "temporarily unavailable"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "key": "sk-x", "upstreamStatus": "bound"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10})
	if err != nil {
		t.Fatalf("unexpected error after retrying 5xx: %v", err)
	}
	if !resp.Deliverable() {
		t.Error("Deliverable() = false after a successful retry")
	}
	if calls != 3 {
		t.Errorf("server called %d times, want exactly 3 (2 failures + 1 success)", calls)
	}
}

func TestSend_5xxExhaustsRetriesAndFails(t *testing.T) {
	orig := retryBackoffBase
	retryBackoffBase = time.Millisecond
	defer func() { retryBackoffBase = orig }()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": "boom"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10})
	if err == nil {
		t.Fatal("expected an error after exhausting retries on persistent 5xx")
	}
	if calls != maxAttempts {
		t.Errorf("server called %d times, want exactly maxAttempts=%d", calls, maxAttempts)
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want *APIError{StatusCode: 500}", err)
	}
}

func TestSend_4xxNeverRetried(t *testing.T) {
	orig := retryBackoffBase
	retryBackoffBase = time.Millisecond
	defer func() { retryBackoffBase = orig }()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "credits must be > 0"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.adminSend(context.Background(), "POST", "/admin/provision_customer", map[string]any{"credits": 0}, &ProvisionCustomerResponse{})
	if err == nil {
		t.Fatal("expected an error on 400")
	}
	if calls != 1 {
		t.Errorf("server called %d times on a 4xx, want exactly 1 (never retried)", calls)
	}
}

func TestSend_2xxNeverRetriedRegardlessOfBody(t *testing.T) {
	// A 2xx must be treated as final on the FIRST attempt, no matter what
	// upstreamStatus the body carries — retry decisions belong to the
	// caller's interpretation of the parsed response, never to the
	// transport layer re-inspecting a successful body.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"success": true, "key": "sk-x", "upstreamStatus": "pending"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.ProvisionCustomer(context.Background(), ProvisionCustomerRequest{Name: "x", Credits: 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("server called %d times for a 2xx/pending response, want exactly 1", calls)
	}
}
