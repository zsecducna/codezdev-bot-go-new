package provision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pool-client tests run against an httptest fake — no network, no real
// Kiro-Go. They pin the wire contract from KIRO_GO_API.md:134-191 and the
// Python poster's behaviour (kiro_provision.py:421-467).

// capture records what the fake server received, so the request side can be
// asserted as precisely as the response side.
type capture struct {
	method string
	path   string
	auth   string
	body   []byte
}

func fakePoolServer(t *testing.T, status int, respBody string, got *capture) *AdminClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = capture{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return NewAdminClient(srv.URL, "adminpw", srv.Client())
}

func TestAddKiroAPIKey(t *testing.T) {
	var got capture
	c := fakePoolServer(t, 200, `{"success":true,"enabled":true,
		"added":[{"id":"uuid","region":"us-east-1","email":"a@b.c","duplicate":false}],
		"skipped":[{"region":"eu-central-1","error":"403 ..."}]}`, &got)

	resp, err := c.AddKiroAPIKey(context.Background(), testKSK, "tg-pool-u1", "us-east-1")
	if err != nil {
		t.Fatalf("AddKiroAPIKey: %v", err)
	}
	if got.method != http.MethodPost || got.path != "/admin/add_kiro_api_key" {
		t.Errorf("request = %s %s, want POST /admin/add_kiro_api_key", got.method, got.path)
	}
	// Auth: Bearer, byte-identical to kiro_provision.py:432 and to
	// internal/kirogo (architecture-go.md §6 pins the port to Bearer).
	if got.auth != "Bearer adminpw" {
		t.Errorf("Authorization = %q, want a Bearer admin key", got.auth)
	}
	var sent map[string]string
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("request body: %v", err)
	}
	want := map[string]string{"kiroApiKey": testKSK, "nickname": "tg-pool-u1", "region": "us-east-1"}
	for k, v := range want {
		if sent[k] != v {
			t.Errorf("sent %s = %q, want %q", k, sent[k], v)
		}
	}
	if len(resp.Added) != 1 || resp.Added[0].Region != "us-east-1" || resp.Added[0].ID != "uuid" {
		t.Errorf("added = %+v", resp.Added)
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0].Region != "eu-central-1" {
		t.Errorf("skipped = %+v", resp.Skipped)
	}
}

func TestAddKiroAPIKeyOmitsEmptyOptionals(t *testing.T) {
	// Matching Python's `if nickname: payload[...] = ...` — an empty optional
	// is OMITTED, not sent as "".
	var got capture
	c := fakePoolServer(t, 200, `{"added":[{"id":"x","region":"us-east-1"}]}`, &got)
	if _, err := c.AddKiroAPIKey(context.Background(), testKSK, "", ""); err != nil {
		t.Fatalf("AddKiroAPIKey: %v", err)
	}
	var sent map[string]any
	json.Unmarshal(got.body, &sent)
	if _, ok := sent["nickname"]; ok {
		t.Error("empty nickname was sent")
	}
	if _, ok := sent["region"]; ok {
		t.Error("empty region was sent")
	}
}

func TestAddKiroAccountForwardsPayloadVerbatim(t *testing.T) {
	// The account JSON must reach the pool BYTE-FOR-BYTE: it carries the
	// refresh material (tokenEndpoint/clientId/scopes) Kiro-Go needs to keep
	// the ~1h access token alive and the profileArn that pins its data-plane
	// region (KIRO_GO_API.md:168-175). A decode/re-encode round trip would drop
	// any field this port doesn't model.
	var got capture
	c := fakePoolServer(t, 200, `{"added":[{"id":"a1","region":"eu-central-1","repaired":true,"duplicate":true}]}`, &got)

	raw := json.RawMessage(`{"accessToken":"eyJx","refreshToken":"rt-1","authMethod":"external_idp",` +
		`"profileArn":"arn:x","clientId":"cid","tokenEndpoint":"https://idp/token",` +
		`"scopes":"s offline_access","aFieldThisPortDoesNotModel":{"nested":[1,2,3]}}`)
	resp, err := c.AddKiroAccount(context.Background(), raw, "tg-pool-a@corp.example")
	if err != nil {
		t.Fatalf("AddKiroAccount: %v", err)
	}
	if got.path != "/admin/add_kiro_account" {
		t.Errorf("path = %s", got.path)
	}
	var sent struct {
		Account  json.RawMessage `json:"account"`
		Nickname string          `json:"nickname"`
	}
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if string(sent.Account) != string(raw) {
		t.Errorf("account forwarded as\n  %s\nwant verbatim\n  %s", sent.Account, raw)
	}
	if sent.Nickname != "tg-pool-a@corp.example" {
		t.Errorf("nickname = %q", sent.Nickname)
	}
	if !resp.Added[0].Repaired {
		t.Error("repaired flag not decoded")
	}
}

func TestAddKiroAccountEscapesNickname(t *testing.T) {
	var got capture
	c := fakePoolServer(t, 200, `{"added":[{"id":"a1"}]}`, &got)
	nick := `tg-pool-quote"and\backslash`
	if _, err := c.AddKiroAccount(context.Background(), json.RawMessage(`{"a":1}`), nick); err != nil {
		t.Fatalf("AddKiroAccount: %v", err)
	}
	var sent struct {
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("request body was not valid JSON: %v", err)
	}
	if sent.Nickname != nick {
		t.Errorf("nickname = %q, want %q", sent.Nickname, nick)
	}
}

func TestPoolErrorSurfacesServerText(t *testing.T) {
	// Python's message shape — "<endpoint> rejected: <server error>" — is
	// surfaced to the operator as a per-account result detail, so the wording
	// is part of the contract.
	var got capture
	c := fakePoolServer(t, 400, `{"error":"kiroApiKey must start with ksk_"}`, &got)

	_, err := c.AddKiroAPIKey(context.Background(), "nope", "", "")
	var pe *PoolError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *PoolError", err)
	}
	if pe.StatusCode != 400 || pe.Endpoint != "add_kiro_api_key" {
		t.Errorf("PoolError = %+v", pe)
	}
	if pe.Error() != "add_kiro_api_key rejected: kiroApiKey must start with ksk_" {
		t.Errorf("Error() = %q", pe.Error())
	}
}

func TestPoolErrorFallsBackToStatusWithoutLeakingBody(t *testing.T) {
	// A non-JSON body (an upstream gateway's HTML page) must not be echoed —
	// these endpoints' bodies can carry pool ids and emails, and the repo's
	// secret-hygiene rule forbids surfacing them.
	var got capture
	c := fakePoolServer(t, 502, `<html><body>Bad Gateway ksk_leakedsecretvalue</body></html>`, &got)

	_, err := c.AddKiroAPIKey(context.Background(), testKSK, "", "")
	if err == nil {
		t.Fatal("expected an error for a 502")
	}
	if strings.Contains(err.Error(), "ksk_leakedsecretvalue") || strings.Contains(err.Error(), "html") {
		t.Errorf("error leaked the response body: %q", err)
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error = %q, want an HTTP 502 fallback", err)
	}
}

func TestPoolErrorNeverLeaksTheAdminKey(t *testing.T) {
	var got capture
	c := fakePoolServer(t, 401, `{"error":"unauthorized"}`, &got)
	_, err := c.AddKiroAPIKey(context.Background(), testKSK, "", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "adminpw") || strings.Contains(err.Error(), testKSK) {
		t.Errorf("error leaked a credential: %q", err)
	}
}

func TestPoolNotConfigured(t *testing.T) {
	// The restocking gate is base URL + admin key ONLY — deliberately not an
	// account id, which restocking has nothing to do with
	// (telegram_bot.py:5586-5600).
	cases := []struct {
		name, base, key string
		wantConfigured  bool
	}{
		{"both set", "http://kg", "pw", true},
		{"no base url", "", "pw", false},
		{"no admin key", "http://kg", "", false},
		{"neither", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewAdminClient(tc.base, tc.key, nil)
			if c.Configured() != tc.wantConfigured {
				t.Fatalf("Configured() = %v, want %v", c.Configured(), tc.wantConfigured)
			}
			if tc.wantConfigured {
				return
			}
			if _, err := c.AddKiroAPIKey(context.Background(), testKSK, "", ""); !errors.Is(err, ErrPoolNotConfigured) {
				t.Errorf("err = %v, want ErrPoolNotConfigured", err)
			}
		})
	}
}

func TestAdminClientTrimsTrailingSlash(t *testing.T) {
	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		io.WriteString(w, `{"added":[{"id":"x"}]}`)
	}))
	defer srv.Close()
	c := NewAdminClient(srv.URL+"///", "pw", srv.Client())
	if _, err := c.AddKiroAPIKey(context.Background(), testKSK, "", ""); err != nil {
		t.Fatalf("AddKiroAPIKey: %v", err)
	}
	if got.path != "/admin/add_kiro_api_key" {
		t.Errorf("path = %q, want no doubled slash", got.path)
	}
}

func TestPoolHealth(t *testing.T) {
	// GET /admin/pool's body shape is documented nowhere and the Python bot
	// never calls it, so the client hands back raw JSON rather than inventing
	// a struct the server may not match.
	var got capture
	c := fakePoolServer(t, 200, `{"accounts":[{"id":"a","region":"us-east-1","banned":false}],"total":1}`, &got)

	raw, err := c.Pool(context.Background())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if got.method != http.MethodGet || got.path != "/admin/pool" {
		t.Errorf("request = %s %s, want GET /admin/pool", got.method, got.path)
	}
	if got.auth != "Bearer adminpw" {
		t.Errorf("Authorization = %q", got.auth)
	}
	var decoded struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Total != 1 {
		t.Errorf("raw body = %s (err %v)", raw, err)
	}
}

func TestPoolRequestHonoursContextCancellation(t *testing.T) {
	var got capture
	c := fakePoolServer(t, 200, `{"added":[]}`, &got)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.AddKiroAPIKey(ctx, testKSK, "", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestPoolNoRetryOn5xx(t *testing.T) {
	// Deliberately NO retries, matching Python: the caller's per-account loop
	// already makes a failure retryable by leaving the credential JSON on disk,
	// and an automatic retry would hide the failure from the operator's report.
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()
	c := NewAdminClient(srv.URL, "pw", srv.Client())
	if _, err := c.AddKiroAPIKey(context.Background(), testKSK, "", ""); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("server saw %d attempts, want exactly 1 (no retries)", attempts)
	}
}
