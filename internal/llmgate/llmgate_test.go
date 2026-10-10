package llmgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// asAPIError wraps errors.As so test call sites read as a boolean check
// instead of repeating the boilerplate everywhere.
func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

// newTestClient wires a fully-configured Client at the given fake server
// (all three of BaseURL/Username/Password set — the user-realm shape).
func newTestClient(srv *httptest.Server) *Client {
	return New(Config{BaseURL: srv.URL, Username: "tester", Password: "secret-pw"}, nil)
}

// writeData emits the success wrapper WITH a data payload; data == nil
// marshals as an explicit JSON null (the "data":null tolerance shape).
func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"code": status, "message": "success", "data": data,
	})
}

// writeNoData emits the success wrapper with NO data field at all —
// delete's verified response shape.
func writeNoData(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%d,"message":"success"}`, status)
}

// writeErr emits the error wrapper llmgate returns on non-2xx
// ({"code":401,"message":"Authentication failed"} shape).
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"code": status, "message": msg})
}

// loginData builds the login payload with a token expiring ttl from now —
// RFC3339 with a +07:00-style offset would be equally decodable; local
// offsets keep the freshness math relative to this process's clock.
func loginData(token string, ttl time.Duration) map[string]any {
	return map[string]any{
		"id":        22,
		"token":     token,
		"expire_at": time.Now().Add(ttl).Format(time.RFC3339),
		"username":  "tester",
	}
}

// shrinkBackoff collapses retryBackoffBase so 5xx-retry tests run in
// milliseconds; restored on cleanup so tests stay order-independent.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	old := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = old })
}

// ── configuration ────────────────────────────────────────────────────────────

// TestConfig_Configured — the user realm needs all three fields; a missing
// one must gate the product, not surface as a mid-sale login failure.
func TestConfig_Configured(t *testing.T) {
	full := Config{BaseURL: "https://x", Username: "u", Password: "p"}
	if !full.Configured() {
		t.Error("full config not Configured")
	}
	for name, cfg := range map[string]Config{
		"no base":     {Username: "u", Password: "p"},
		"no username": {BaseURL: "https://x", Password: "p"},
		"no password": {BaseURL: "https://x", Username: "u"},
		"empty":       {},
	} {
		if cfg.Configured() {
			t.Errorf("%s: Configured = true, want false", name)
		}
	}
}

// TestNew_TrimsTrailingSlash — New canonicalizes the base URL so base+path
// can never produce a doubled slash on the wire.
func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New(Config{BaseURL: "https://x.example//"}, nil)
	if c.cfg.BaseURL != "https://x.example" {
		t.Errorf("BaseURL = %q, want trailing slashes trimmed", c.cfg.BaseURL)
	}
}

// TestNotConfigured_FailsWithoutNetwork — user-realm calls on an
// unconfigured client must fail locally with ErrNotConfigured; the server
// (if one were even reachable) must not be asked anything.
func TestNotConfigured_FailsWithoutNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeErr(w, http.StatusUnauthorized, "Authentication failed")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil) // no username/password
	if _, err := c.ListKeys(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("ListKeys error = %v, want ErrNotConfigured", err)
	}
	if called {
		t.Error("unconfigured client still hit the network")
	}
}

// ── token cache (invariant 1) ────────────────────────────────────────────────

// TestLoginCachedAcrossConcurrentCalls — 16 concurrent first-calls must
// produce exactly ONE login: login runs under tokenMu, so the fifteen
// losers block and then reuse the winner's token instead of racing their
// own logins.
func TestLoginCachedAcrossConcurrentCalls(t *testing.T) {
	var logins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer jwt-1" {
			t.Errorf("Authorization = %q, want Bearer jwt-1", got)
		}
		writeData(w, http.StatusOK, []any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			_, err := c.ListKeys(context.Background())
			errs <- err
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent ListKeys: %v", err)
		}
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want exactly 1 for 16 concurrent callers", n)
	}
}

// TestTokenProactiveRefreshNearExpiry — a token inside its final 60s margin
// (here: expiring in 50s) is treated as already stale and re-logged-in
// BEFORE the server can 401 it; a token with a healthy margin is reused.
func TestTokenProactiveRefreshNearExpiry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ttl      time.Duration
		wantLogn int32
	}{
		{name: "healthy margin reused", ttl: 24 * time.Hour, wantLogn: 1},
		{name: "inside 60s margin refreshed", ttl: 50 * time.Second, wantLogn: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logins atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
				logins.Add(1)
				writeData(w, http.StatusOK, loginData("jwt-1", tc.ttl))
			})
			mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
				writeData(w, http.StatusOK, []any{})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := newTestClient(srv)
			for i := 0; i < 2; i++ {
				if _, err := c.ListKeys(context.Background()); err != nil {
					t.Fatalf("call %d: %v", i+1, err)
				}
			}
			if n := logins.Load(); n != tc.wantLogn {
				t.Errorf("logins = %d after 2 calls, want %d", n, tc.wantLogn)
			}
		})
	}
}

// ── 401 handling in the user realm (invariant 1) ─────────────────────────────

// TestUserSend_401ReloginRetrySucceeds — a 401 on a user-realm call means
// the cached JWT went stale: exactly one forced re-login, the request
// replayed once with the new token, and then success. The first token
// (jwt-1) is rejected by the list endpoint on purpose.
func TestUserSend_401ReloginRetrySucceeds(t *testing.T) {
	var logins, listCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		n := logins.Add(1)
		writeData(w, http.StatusOK, loginData(fmt.Sprintf("jwt-%d", n), 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		if listCalls.Add(1) == 1 || r.Header.Get("Authorization") != "Bearer jwt-2" {
			writeErr(w, http.StatusUnauthorized, "Authentication failed")
			return
		}
		writeData(w, http.StatusOK, []any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	if _, err := c.ListKeys(context.Background()); err != nil {
		t.Fatalf("ListKeys after 401+relogin: %v", err)
	}
	if n := logins.Load(); n != 2 {
		t.Errorf("logins = %d, want 2 (initial + forced)", n)
	}
	if n := listCalls.Load(); n != 2 {
		t.Errorf("list calls = %d, want 2 (rejected + retried)", n)
	}
}

// TestUserSend_401TwiceErrAuthFailed — a SECOND 401 after the forced
// re-login is terminal: ErrAuthFailed, no further retries (the newest
// possible token already failed; more attempts just hammer a credentials
// problem).
func TestUserSend_401TwiceErrAuthFailed(t *testing.T) {
	var logins, listCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		writeData(w, http.StatusOK, loginData("jwt-fresh", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		listCalls.Add(1)
		writeErr(w, http.StatusUnauthorized, "Authentication failed")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListKeys(context.Background())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("error = %v, want ErrAuthFailed after second 401", err)
	}
	if n := logins.Load(); n != 2 {
		t.Errorf("logins = %d, want 2", n)
	}
	if n := listCalls.Load(); n != 2 {
		t.Errorf("list calls = %d, want exactly 2 — a third attempt would violate the retry-once rule", n)
	}
}

// ── POST /api/v1/apikey/create (invariant 3) ─────────────────────────────────

// TestCreateKey_WireBodyNoExpiry — the create payload must STRUCTURALLY
// omit expires_at (no key at all in the JSON), because an omitted field
// means "never expires" while a zero-valued one would mean "already
// expired". name/is_active/credit_limit must all be present.
func TestCreateKey_WireBodyNoExpiry(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode create body: %v", err)
		}
		if _, exists := body["expires_at"]; exists {
			t.Errorf("expires_at present in create body %v — must be structurally omitted (never expires)", body)
		}
		if body["name"] != "order-9" {
			t.Errorf("name = %v, want order-9", body["name"])
		}
		if body["is_active"] != true {
			t.Errorf("is_active = %v, want true", body["is_active"])
		}
		if body["credit_limit"] != float64(5) {
			t.Errorf("credit_limit = %v, want 5", body["credit_limit"])
		}
		writeData(w, http.StatusOK, map[string]any{
			"id": 1883, "api_key": "sk-llmgate-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"name": "order-9", "is_active": true, "credit_limit": 5,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	key, err := c.CreateKey(context.Background(), "order-9", 5)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if key.ID != 1883 || key.APIKey == "" || key.Name != "order-9" || !key.IsActive || key.CreditLimit != 5 {
		t.Errorf("decoded key = %+v, want the minted values incl. cleartext APIKey", key)
	}
}

// TestCreateKey_Guards — a negative credit limit is a caller bug: rejected
// client-side with ErrCreditLimitNegative, never reaching the network.
// credit_limit 0 (UNLIMITED) is legal and DOES go through.
func TestCreateKey_Guards(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/create", func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeData(w, http.StatusOK, map[string]any{"id": 1, "name": "x", "is_active": true, "credit_limit": 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	if _, err := c.CreateKey(context.Background(), "x", -0.5); !errors.Is(err, ErrCreditLimitNegative) {
		t.Errorf("negative limit error = %v, want ErrCreditLimitNegative", err)
	}
	if called {
		t.Error("negative limit still reached the server")
	}
	if _, err := c.CreateKey(context.Background(), "x", 0); err != nil {
		t.Errorf("unlimited (0) limit rejected: %v", err)
	}
	if !called {
		t.Error("unlimited (0) limit never reached the server")
	}
}

// ── POST /api/v1/apikey/update (invariant 4) ─────────────────────────────────

// TestUpdateKey_WireBody — update always carries id AND name (omitting
// name CLEARS it server-side, a verified footgun), plus is_active and the
// absolute credit_limit. The response has no api_key value (only a
// length) and a zeroed created_at; Key must decode that without error and
// leave APIKey empty.
func TestUpdateKey_WireBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/update", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode update body: %v", err)
		}
		if body["id"] != float64(1882) {
			t.Errorf("id = %v, want 1882", body["id"])
		}
		if body["name"] != "current-name" {
			t.Errorf("name = %v, want current-name (omitting it would CLEAR it)", body["name"])
		}
		if body["is_active"] != true {
			t.Errorf("is_active = %v, want true", body["is_active"])
		}
		if body["credit_limit"] != float64(12) {
			t.Errorf("credit_limit = %v, want 12 (absolute, not additive)", body["credit_limit"])
		}
		// The verified update response: api_key_length instead of api_key,
		// and a zeroed created_at that must not be trusted.
		writeData(w, http.StatusOK, map[string]any{
			"id": 1882, "api_key_length": 59, "name": "current-name",
			"is_active": true, "created_at": "0001-01-01T00:00:00Z", "credit_limit": 12,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	key, err := c.UpdateKey(context.Background(), 1882, "current-name", true, 12)
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if key.APIKey != "" {
		t.Errorf("APIKey = %q, want empty — update never returns the cleartext key", key.APIKey)
	}
	if key.ID != 1882 || key.CreditLimit != 12 {
		t.Errorf("decoded key = %+v, want id 1882 and the new absolute limit", key)
	}
}

// TestUpdateKey_Guards — id <= 0 and a negative credit limit are rejected
// before any HTTP call.
func TestUpdateKey_Guards(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeErr(w, http.StatusBadRequest, "bad")
	}))
	defer srv.Close()

	c := newTestClient(srv)
	for _, id := range []int64{0, -7} {
		if _, err := c.UpdateKey(context.Background(), id, "n", true, 1); !errors.Is(err, ErrIDRequired) {
			t.Errorf("id %d: error = %v, want ErrIDRequired", id, err)
		}
	}
	if _, err := c.UpdateKey(context.Background(), 5, "n", true, -1); !errors.Is(err, ErrCreditLimitNegative) {
		t.Errorf("negative limit: error = %v, want ErrCreditLimitNegative", err)
	}
	if called {
		t.Error("a guarded call still reached the server")
	}
}

// ── DELETE /api/v1/apikey/delete/{id} (invariant 6) ──────────────────────────

// TestDeleteKey_NoDataSuccess — delete's 2xx has NO data field at all;
// that must be a plain nil error, not a decode failure.
func TestDeleteKey_NoDataSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/delete/1882", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		writeNoData(w, http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.DeleteKey(context.Background(), 1882); err != nil {
		t.Errorf("DeleteKey on a data-less 200: %v, want nil", err)
	}
}

// TestDeleteKey_NotFound — a 404 maps to ErrNotFound so callers can
// errors.Is it instead of string-matching.
func TestDeleteKey_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/delete/404", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.DeleteKey(context.Background(), 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 delete error = %v, want ErrNotFound", err)
	}
}

// TestDeleteKey_IDRequired — id <= 0 can never name a key; reject locally.
func TestDeleteKey_IDRequired(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeErr(w, http.StatusNotFound, "not found")
	}))
	defer srv.Close()

	c := newTestClient(srv)
	for _, id := range []int64{0, -1} {
		if err := c.DeleteKey(context.Background(), id); !errors.Is(err, ErrIDRequired) {
			t.Errorf("id %d: error = %v, want ErrIDRequired", id, err)
		}
	}
	if called {
		t.Error("guarded delete still reached the server")
	}
}

// ── GET /api/v1/apikey/list ──────────────────────────────────────────────────

// TestListKeys_Decode — list returns metadata-only records (api_key_length,
// no key value) with created_at present server-side; Key must decode the
// useful fields and ignore the rest.
func TestListKeys_Decode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, []map[string]any{
			{"id": 1342, "api_key_length": 59, "name": "Pool", "is_active": true,
				"created_at": "2026-08-12T15:20:02.754123+07:00", "credit_limit": 0},
			{"id": 1883, "api_key_length": 59, "name": "order-9", "is_active": false,
				"created_at": "2026-09-01T09:00:00+07:00", "credit_limit": 2.5},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	keys, err := c.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}
	if keys[0].ID != 1342 || keys[0].Name != "Pool" || !keys[0].IsActive || keys[0].CreditLimit != 0 {
		t.Errorf("keys[0] = %+v, want Pool/active/unlimited", keys[0])
	}
	if keys[0].APIKey != "" {
		t.Errorf("keys[0].APIKey = %q, want empty — list never returns key values", keys[0].APIKey)
	}
	if keys[1].IsActive || keys[1].CreditLimit != 2.5 {
		t.Errorf("keys[1] = %+v, want inactive with limit 2.5", keys[1])
	}
}

// ── POST /api/v1/apikey/{id}/reveal ──────────────────────────────────────────

// TestRevealKey_PostMethod — reveal is POST-only on the live API (GET
// 405s); the client must send POST.
func TestRevealKey_PostMethod(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/1342/reveal", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST (GET is 405 on the live API)", r.Method)
		}
		writeData(w, http.StatusOK, map[string]any{
			"id": 1342, "api_key": "sk-llmgate-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"name": "Pool", "is_active": true, "credit_limit": 0,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	key, err := c.RevealKey(context.Background(), 1342)
	if err != nil {
		t.Fatalf("RevealKey: %v", err)
	}
	if key.APIKey == "" {
		t.Error("revealed key is empty; reveal exists to return the cleartext")
	}
}

// TestRevealKey_NullDataTolerance — a "data":null on a 2xx is success with
// a zero Key, never a decode error (invariant 6's null-tolerant decode).
func TestRevealKey_NullDataTolerance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/7/reveal", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, nil)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	key, err := c.RevealKey(context.Background(), 7)
	if err != nil {
		t.Fatalf("null-data 2xx must be success, got %v", err)
	}
	if key == nil || key.ID != 0 {
		t.Errorf("key = %+v, want non-nil zero Key", key)
	}
}

// TestRevealKey_IDRequired — same id guard as every id-addressed endpoint.
func TestRevealKey_IDRequired(t *testing.T) {
	c := New(Config{BaseURL: "http://127.0.0.1:0", Username: "u", Password: "p"}, nil)
	if _, err := c.RevealKey(context.Background(), 0); !errors.Is(err, ErrIDRequired) {
		t.Errorf("error = %v, want ErrIDRequired", err)
	}
}

// ── GET /api/v1/apikey/stats — KEY realm (invariant 1) ───────────────────────

// statsBody is the verified live stats payload (fractional costs, token
// counts, unlimited credit_limit 0) — the decode + arithmetic fixture.
const statsBody = `{"info":{"id":1342,"api_key_length":59,"name":"Pool","is_active":true,` +
	`"created_at":"2026-08-12T15:20:02.754123+07:00","credit_limit":0},` +
	`"stats":{"user_id":22,"api_key_id":1342,"name":"","input_token":156592761,` +
	`"output_token":779179,"cache_token":152646462,"cache_write_token":47394030,` +
	`"input_cost":21.000417709350586,"output_cost":18.97,"cache_cost":76.24,` +
	`"cache_write_cost":440.35,"wait_time":32037154,"request_success":978,"request_failed":9}}`

// TestKeyStats_BearerCustomerKeyNotUserToken — the key realm authenticates
// with the CUSTOMER's key as the Bearer, never with a login JWT: the test
// server 401s any user token on sight, and the client is configured with
// NO username/password at all to prove stats never needs login.
func TestKeyStats_BearerCustomerKeyNotUserToken(t *testing.T) {
	const customerKey = "sk-llmgate-cccccccccccccccccccccccccccccccccccccccccccccccc"
	loginSeen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/user/login" {
			loginSeen = true
			writeErr(w, http.StatusUnauthorized, "Authentication failed")
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+customerKey {
			t.Errorf("Authorization = %q, want the customer key as Bearer", got)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"code":200,"message":"success","data":%s}`, statsBody)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil) // key realm: no credentials at all
	usage, err := c.KeyStats(context.Background(), customerKey)
	if err != nil {
		t.Fatalf("KeyStats: %v", err)
	}
	if loginSeen {
		t.Error("KeyStats logged in — the key realm must never use the user token")
	}
	if want := 21.000417709350586 + 18.97 + 76.24 + 440.35; usage.Used() > want+1e-9 || usage.Used() < want-1e-9 {
		t.Errorf("Used() = %v, want %v (sum of the four cost buckets)", usage.Used(), want)
	}
	if usage.Stats.InputTokens != 156592761 || usage.Stats.RequestsOK != 978 || usage.Stats.RequestsFailed != 9 {
		t.Errorf("stats decode = %+v", usage.Stats)
	}
	if rem, limited := usage.Remaining(); limited || rem != 0 {
		t.Errorf("unlimited key: Remaining = (%v, %v), want (0, false)", rem, limited)
	}
	if got := usage.Status(); got != "active" {
		t.Errorf("Status() = %q, want active (unlimited limit despite heavy usage)", got)
	}
}

// TestKeyStats_401SurfacesNoRelogin — a 401 in the KEY realm means the
// customer key itself is bad: no re-login-retry (nothing a fresh JWT could
// fix), the *APIError surfaces, and it is NOT ErrAuthFailed.
func TestKeyStats_401SurfacesNoRelogin(t *testing.T) {
	var statsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/user/login" {
			t.Error("key-realm 401 triggered a login — forbidden")
			return
		}
		statsCalls.Add(1)
		writeErr(w, http.StatusUnauthorized, "Authentication failed")
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.KeyStats(context.Background(), "sk-llmgate-badbadbadbadbadbadbadbad")
	if err == nil {
		t.Fatal("expected a 401 error")
	}
	if errors.Is(err, ErrAuthFailed) {
		t.Errorf("key-realm 401 became ErrAuthFailed: %v — the key is bad, re-login cannot apply", err)
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("error = %v, want *APIError with 401", err)
	}
	if n := statsCalls.Load(); n != 1 {
		t.Errorf("stats calls = %d, want exactly 1 (no retry after key-realm 401)", n)
	}
}

// TestKeyStats_EmptyCustomerKey — an empty key cannot authenticate the key
// realm at all; reject before the network.
func TestKeyStats_EmptyCustomerKey(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeErr(w, http.StatusUnauthorized, "Authentication failed")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, nil)
	if _, err := c.KeyStats(context.Background(), ""); err == nil {
		t.Error("empty customer key accepted")
	}
	if called {
		t.Error("empty customer key still hit the server")
	}
}

// ── KeyUsage semantics (invariant 5 helpers) ─────────────────────────────────

// TestKeyUsage_StatusMatrix — the handlers' verdict table: disabled wins
// over everything; exhausted means limited AND remaining <= 0 (including
// overspent negatives); unlimited keys stay active with (0, false).
func TestKeyUsage_StatusMatrix(t *testing.T) {
	usage := func(active bool, limit float64, costs ...float64) *KeyUsage {
		u := &KeyUsage{Info: Key{IsActive: active, CreditLimit: limit}}
		u.Stats.InputCost, u.Stats.OutputCost, u.Stats.CacheCost, u.Stats.CacheWriteCost = costs[0], costs[1], costs[2], costs[3]
		return u
	}
	cases := []struct {
		name          string
		u             *KeyUsage
		wantStatus    string
		wantRemaining float64
		wantLimited   bool
	}{
		{"disabled wins over exhausted", usage(false, 10, 11, 0, 0, 0), "disabled", -1, true},
		{"exhausted at the line", usage(true, 10, 10, 0, 0, 0), "exhausted", 0, true},
		{"exhausted overspent keeps negative", usage(true, 10, 6, 5, 0, 0), "exhausted", -1, true},
		{"active with headroom", usage(true, 10, 9, 0, 0.5, 0), "active", 0.5, true},
		{"unlimited is never exhausted", usage(true, 0, 999, 999, 999, 999), "active", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.Status(); got != tc.wantStatus {
				t.Errorf("Status() = %q, want %q", got, tc.wantStatus)
			}
			rem, limited := tc.u.Remaining()
			if limited != tc.wantLimited {
				t.Errorf("limited = %v, want %v", limited, tc.wantLimited)
			}
			if limited && (rem > tc.wantRemaining+1e-9 || rem < tc.wantRemaining-1e-9) {
				t.Errorf("Remaining() = %v, want %v (unclamped)", rem, tc.wantRemaining)
			}
		})
	}
}

// ── send retry policy (invariant 6) ──────────────────────────────────────────

// TestSend_Retries5xxThenSucceeds — a 5xx is transient: one linear-backoff
// retry (backoff shrunk for speed) and the replayed request succeeds.
func TestSend_Retries5xxThenSucceeds(t *testing.T) {
	shrinkBackoff(t)
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeErr(w, http.StatusBadGateway, "upstream exploded")
			return
		}
		writeData(w, http.StatusOK, []any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	if _, err := c.ListKeys(context.Background()); err != nil {
		t.Fatalf("ListKeys after one 502: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("list calls = %d, want 2 (failed + retried)", n)
	}
}

// TestSend_5xxExhaustsRetries — three 5xx responses exhaust the
// maxAttempts=3 budget and surface the last *APIError.
func TestSend_5xxExhaustsRetries(t *testing.T) {
	shrinkBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeErr(w, http.StatusInternalServerError, "boom")
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListKeys(context.Background())
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("error = %v, want *APIError 500 after retries", err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("calls = %d, want exactly maxAttempts (3)", n)
	}
}

// TestSend_4xxNoRetry — a 4xx is a real rejection; retrying cannot fix it
// (and on a mint would double-spend), so exactly one request is made.
func TestSend_4xxNoRetry(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeErr(w, http.StatusBadRequest, "bad payload")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListKeys(context.Background())
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Message != "bad payload" {
		t.Errorf("apiErr = %+v, want 400 with server message", apiErr)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1 — a 4xx must never be retried", n)
	}
}

// TestSend_BodyCodeNon200In2xx — a 2xx transport whose wrapper code says
// something else is a business failure: *APIError carrying the BODY code
// and message, not a silent success.
func TestSend_BodyCodeNon200In2xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"code":400,"message":"boom"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListKeys(context.Background())
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError from body code", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Message != "boom" {
		t.Errorf("apiErr = %+v, want body code 400 and its message", apiErr)
	}
}

// TestLogin_WireBodyAndFallbackTTL — login carries username/password and
// expire:-1 (server-default ~30-day token). An unparseable expire_at must
// fall back to the conservative short TTL, not break the call or cache a
// forever-token: two calls, one login.
func TestLogin_WireBodyAndFallbackTTL(t *testing.T) {
	var logins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode login body: %v", err)
		}
		if body["username"] != "tester" || body["password"] != "secret-pw" {
			t.Errorf("credentials = %v/%v, want tester/secret-pw", body["username"], body["password"])
		}
		if body["expire"] != float64(-1) {
			t.Errorf("expire = %v, want -1 (server-default token lifetime)", body["expire"])
		}
		// Deliberately garbage expire_at to force the fallback path.
		writeData(w, http.StatusOK, map[string]any{"id": 22, "token": "jwt-1", "expire_at": "not-a-time", "username": "tester"})
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, []any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	for i := 0; i < 2; i++ {
		if _, err := c.ListKeys(context.Background()); err != nil {
			t.Fatalf("call %d with garbage expire_at: %v", i+1, err)
		}
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want 1 — fallback TTL must still cache within its window", n)
	}
}

// ── create's single-attempt money guard ──────────────────────────────────────

// TestCreateKey_SingleAttemptOn500 — create is money-minting: even on a 5xx
// (which every OTHER endpoint retries), CreateKey must send exactly ONE
// attempt. A blind retry after a timeout-that-was-really-a-commit would
// mint a second live key for one paid sale while the first becomes an
// unrecorded billable orphan. No backoff shrinking needed — the whole point
// is that the second attempt never happens.
func TestCreateKey_SingleAttemptOn500(t *testing.T) {
	var creates atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, loginData("jwt-1", 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/create", func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		writeErr(w, http.StatusInternalServerError, "boom")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateKey(context.Background(), "order-x", 5)
	if err == nil {
		t.Fatal("expected the 500 to surface as an error")
	}
	if n := creates.Load(); n != 1 {
		t.Errorf("create attempts = %d, want EXACTLY 1 — a create retry can double-mint a paid key", n)
	}
	if !IsAmbiguous(err) {
		t.Errorf("IsAmbiguous(500 create error) = false, want true — the server may have committed before the 5xx")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("error = %v, want *APIError 500", err)
	}
}

// TestIsAmbiguous — the mint path's branch table: transport-shaped errors
// and 5xx *APIErrors are ambiguous (the request MAY have committed), every
// definitive outcome (4xx, 401/404 sentinels, guard errors, auth failure)
// is not.
func TestIsAmbiguous(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"500 APIError", &APIError{Path: "/p", StatusCode: 500, Message: "boom"}, true},
		{"503 APIError", &APIError{Path: "/p", StatusCode: 503, Message: "unavailable"}, true},
		{"400 APIError", &APIError{Path: "/p", StatusCode: 400, Message: "bad"}, false},
		{"401 APIError", &APIError{Path: "/p", StatusCode: 401, Message: "auth"}, false},
		{"404-mapped ErrNotFound", fmt.Errorf("%w: /p", ErrNotFound), false},
		{"ErrNotConfigured", ErrNotConfigured, false},
		{"ErrIDRequired", ErrIDRequired, false},
		{"ErrCreditLimitNegative", ErrCreditLimitNegative, false},
		{"ErrAuthFailed", fmt.Errorf("%w: /p still rejected", ErrAuthFailed), false},
		{"transport wrapper", fmt.Errorf("llmgate: /api/v1/apikey/create: %w", context.DeadlineExceeded), true},
		{"undecodable 2xx body", fmt.Errorf("llmgate: decode response for /p: unexpected EOF"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAmbiguous(tc.err); got != tc.want {
				t.Errorf("IsAmbiguous(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
