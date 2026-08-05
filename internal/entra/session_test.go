package entra

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// session_test.go drives StartDeviceCode/CompleteDeviceCode/SessionStatus/
// GetAccessToken/SignOut/SetTenant/GetTenant against a local httptest.Server
// standing in for login.microsoftonline.com + graph.microsoft.com — no real
// network call ever leaves this test process.

// fakeIDToken builds an unsigned JWT-shaped string carrying claims — good
// enough for decodeIDTokenClaims, which never verifies a signature (see its
// doc: the token arrives directly from the token endpoint over TLS).
func fakeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// idpServer is a configurable stand-in for the Microsoft identity platform +
// Graph /organization endpoint. tokenResponses is consumed IN ORDER, one
// per /oauth2/v2.0/token call (device_code poll OR refresh); the LAST entry
// repeats once exhausted. orgResponse backs GET /organization; a nil
// orgResponse makes that endpoint 500, to prove a caller tolerates a failed
// (best-effort) org lookup.
type idpServer struct {
	*httptest.Server
	tokenCalls int64
	orgCalls   int64
}

func newIDPServer(t *testing.T, tokenResponses []map[string]any, orgResponse map[string]any) *idpServer {
	t.Helper()
	srv := &idpServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/devicecode"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "DC1", "user_code": "ABC-DEF",
				"verification_uri": "https://microsoft.com/devicelogin",
				"expires_in":       900, "interval": 5,
				"message": "go sign in",
			})
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			idx := atomic.AddInt64(&srv.tokenCalls, 1) - 1
			var resp map[string]any
			switch {
			case int(idx) < len(tokenResponses):
				resp = tokenResponses[idx]
			case len(tokenResponses) > 0:
				resp = tokenResponses[len(tokenResponses)-1]
			default:
				resp = map[string]any{"error": "authorization_pending"}
			}
			if _, isErr := resp["error"]; isErr {
				w.WriteHeader(http.StatusBadRequest)
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/organization":
			atomic.AddInt64(&srv.orgCalls, 1)
			if orgResponse == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(orgResponse)
		default:
			t.Errorf("idpServer: unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	srv.Server = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testClient(srv *idpServer) Client {
	return Client{AuthorityBase: srv.URL, GraphBase: srv.URL}
}

func TestStartDeviceCodeHappyPath(t *testing.T) {
	srv := newIDPServer(t, nil, nil)
	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	if flow.UserCode != "ABC-DEF" || flow.VerificationURI != "https://microsoft.com/devicelogin" {
		t.Errorf("flow = %+v", flow)
	}
	if flow.ExpiresIn != 900*time.Second || flow.Interval != 5*time.Second {
		t.Errorf("flow timing = expires_in=%v interval=%v, want 900s/5s", flow.ExpiresIn, flow.Interval)
	}
}

func TestStartDeviceCodeErrorResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request", "error_description": "bad tenant"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, err := (Client{AuthorityBase: srv.URL}).StartDeviceCode(context.Background(), "bad-tenant")
	if err == nil || !strings.Contains(err.Error(), "bad tenant") {
		t.Errorf("StartDeviceCode error = %v, want it to contain the server's error_description", err)
	}
}

func TestCompleteDeviceCodeSuccessAfterPending(t *testing.T) {
	idToken := fakeIDToken(t, map[string]any{"tid": "tid-from-claims", "preferred_username": "admin@kiro.uk"})
	srv := newIDPServer(t, []map[string]any{
		{"error": "authorization_pending"},
		{"access_token": "AT1", "refresh_token": "RT1", "id_token": idToken, "expires_in": 3600},
	}, map[string]any{"value": []map[string]any{{"id": "org-tid", "verifiedDomains": []map[string]any{{"name": "kiro.uk", "isDefault": true}}}}})

	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	flow.Interval = 5 * time.Millisecond // keep the test fast

	cacheDir := t.TempDir()
	var progressCalls int
	identity, err := testClient(srv).CompleteDeviceCode(context.Background(), flow, cacheDir, func(elapsed, remaining time.Duration) {
		progressCalls++
	})
	if err != nil {
		t.Fatalf("CompleteDeviceCode: %v", err)
	}
	if identity.Username != "admin@kiro.uk" || identity.TenantID != "org-tid" || identity.TenantDomain != "kiro.uk" {
		t.Errorf("identity = %+v", identity)
	}
	if progressCalls == 0 {
		t.Error("onProgress was never called")
	}
	cache, ok, err := loadSessionCache(cacheDir)
	if err != nil || !ok {
		t.Fatalf("loadSessionCache: ok=%v err=%v", ok, err)
	}
	if cache.AccessToken != "AT1" || cache.RefreshToken != "RT1" {
		t.Errorf("persisted cache = %+v", cache)
	}
}

func TestCompleteDeviceCodeSlowDownThenSuccess(t *testing.T) {
	idToken := fakeIDToken(t, map[string]any{"preferred_username": "admin@kiro.uk"})
	srv := newIDPServer(t, []map[string]any{
		{"error": "slow_down"},
		{"access_token": "AT2", "refresh_token": "RT2", "id_token": idToken, "expires_in": 3600},
	}, nil) // org lookup failing is fine — best-effort, falls back to id_token claims

	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	flow.Interval = 5 * time.Millisecond

	identity, err := testClient(srv).CompleteDeviceCode(context.Background(), flow, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("CompleteDeviceCode: %v", err)
	}
	if identity.Username != "admin@kiro.uk" {
		t.Errorf("identity = %+v, want username from id_token claims fallback", identity)
	}
}

func TestCompleteDeviceCodeDeclinedIsTerminal(t *testing.T) {
	srv := newIDPServer(t, []map[string]any{
		{"error": "authorization_declined", "error_description": "the admin declined the sign-in"},
	}, nil)
	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	flow.Interval = 5 * time.Millisecond
	cacheDir := t.TempDir()
	_, err = testClient(srv).CompleteDeviceCode(context.Background(), flow, cacheDir, nil)
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("CompleteDeviceCode error = %v, want it to surface the decline", err)
	}
	if _, ok, _ := loadSessionCache(cacheDir); ok {
		t.Error("a declined login must not persist a session cache")
	}
}

func TestCompleteDeviceCodeExpires(t *testing.T) {
	srv := newIDPServer(t, nil, nil) // always authorization_pending
	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	flow.Interval = 5 * time.Millisecond
	flow.ExpiresIn = 20 * time.Millisecond
	_, err = testClient(srv).CompleteDeviceCode(context.Background(), flow, t.TempDir(), nil)
	if !errors.Is(err, ErrDeviceCodeExpired) {
		t.Errorf("CompleteDeviceCode error = %v, want errors.Is(err, ErrDeviceCodeExpired)", err)
	}
}

func TestCompleteDeviceCodeContextCancellation(t *testing.T) {
	srv := newIDPServer(t, nil, nil) // always authorization_pending
	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	flow.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = testClient(srv).CompleteDeviceCode(ctx, flow, t.TempDir(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CompleteDeviceCode error = %v, want errors.Is(err, context.Canceled) — a bot Shutdown must drain, not hang", err)
	}
}

// TestCompleteDeviceCodePersistFailureNeverLeaksTokens pins that when
// persisting the session fails (here: cacheDir is a FILE, so MkdirAll
// errors), the returned error string never contains the raw access/refresh
// token this call just obtained.
func TestCompleteDeviceCodePersistFailureNeverLeaksTokens(t *testing.T) {
	const accessToken = "SECRET-ACCESS-TOKEN-VALUE"
	const refreshToken = "SECRET-REFRESH-TOKEN-VALUE"
	idToken := fakeIDToken(t, map[string]any{"preferred_username": "admin@kiro.uk"})
	srv := newIDPServer(t, []map[string]any{
		{"access_token": accessToken, "refresh_token": refreshToken, "id_token": idToken, "expires_in": 3600},
	}, nil)
	flow, err := testClient(srv).StartDeviceCode(context.Background(), "")
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	flow.Interval = 5 * time.Millisecond

	notADir := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := notADir + "/.entra-auth" // MkdirAll under a FILE must fail

	_, err = testClient(srv).CompleteDeviceCode(context.Background(), flow, cacheDir, nil)
	if err == nil {
		t.Fatal("CompleteDeviceCode: got nil error, want the persist failure to surface")
	}
	if strings.Contains(err.Error(), accessToken) || strings.Contains(err.Error(), refreshToken) {
		t.Errorf("CompleteDeviceCode error = %q, leaks a raw token", err.Error())
	}
}

func TestSessionStatusNotLoggedIn(t *testing.T) {
	got, err := (Client{}).SessionStatus(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("SessionStatus: %v", err)
	}
	if got.State != SessionNotLoggedIn {
		t.Errorf("SessionStatus() = %+v, want not_logged_in", got)
	}
}

// TestSessionStatusValidMakesNoNetworkCall pins that a cached, unexpired
// access token is reported valid WITHOUT any HTTP call — the server below
// fails the test if it ever receives a request.
func TestSessionStatusValidMakesNoNetworkCall(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network call to %s for an unexpired cached session", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cacheDir := t.TempDir()
	if err := saveSessionCache(cacheDir, sessionCache{
		Schema: sessionCacheSchema, Username: "admin@kiro.uk", TenantID: "t1", TenantDomain: "kiro.uk",
		AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := (Client{AuthorityBase: srv.URL, GraphBase: srv.URL}).SessionStatus(context.Background(), cacheDir)
	if err != nil {
		t.Fatalf("SessionStatus: %v", err)
	}
	if got.State != SessionValid || got.Username != "admin@kiro.uk" {
		t.Errorf("SessionStatus() = %+v", got)
	}
}

func TestSessionStatusExpiredRefreshSuccess(t *testing.T) {
	srv := newIDPServer(t, []map[string]any{
		{"access_token": "AT-NEW", "refresh_token": "RT-NEW", "expires_in": 3600},
	}, map[string]any{"value": []map[string]any{{"id": "t1", "verifiedDomains": []map[string]any{{"name": "kiro.uk", "isDefault": true}}}}})

	cacheDir := t.TempDir()
	if err := SetTenant(cacheDir, "kiro.uk"); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionCache(cacheDir, sessionCache{
		Schema: sessionCacheSchema, Username: "admin@kiro.uk",
		AccessToken: "AT-OLD", RefreshToken: "RT-OLD", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := testClient(srv).SessionStatus(context.Background(), cacheDir)
	if err != nil {
		t.Fatalf("SessionStatus: %v", err)
	}
	if got.State != SessionValid || got.Username != "admin@kiro.uk" {
		t.Errorf("SessionStatus() = %+v, want valid with the ORIGINAL username preserved across refresh", got)
	}
	cache, ok, _ := loadSessionCache(cacheDir)
	if !ok || cache.AccessToken != "AT-NEW" {
		t.Errorf("persisted cache = %+v, want the refreshed access token saved", cache)
	}
}

func TestSessionStatusExpiredRefreshFailure(t *testing.T) {
	srv := newIDPServer(t, []map[string]any{
		{"error": "invalid_grant", "error_description": "refresh token revoked"},
	}, nil)
	cacheDir := t.TempDir()
	if err := saveSessionCache(cacheDir, sessionCache{
		Schema: sessionCacheSchema, Username: "admin@kiro.uk",
		AccessToken: "AT-OLD", RefreshToken: "RT-OLD", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := testClient(srv).SessionStatus(context.Background(), cacheDir)
	if err != nil {
		t.Fatalf("SessionStatus: %v", err)
	}
	if got.State != SessionExpired || got.Username != "admin@kiro.uk" {
		t.Errorf("SessionStatus() = %+v, want expired with the username still reported", got)
	}
}

func TestGetAccessTokenNotLoggedIn(t *testing.T) {
	got, err := (Client{}).GetAccessToken(context.Background(), t.TempDir())
	if err != nil || got != "" {
		t.Errorf("GetAccessToken() = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestGetAccessTokenValidNoNetworkCall(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network call to %s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cacheDir := t.TempDir()
	if err := saveSessionCache(cacheDir, sessionCache{
		Schema: sessionCacheSchema, AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := (Client{AuthorityBase: srv.URL, GraphBase: srv.URL}).GetAccessToken(context.Background(), cacheDir)
	if err != nil || got != "AT" {
		t.Errorf("GetAccessToken() = (%q, %v), want (\"AT\", nil)", got, err)
	}
}

func TestGetAccessTokenRefreshSuccess(t *testing.T) {
	srv := newIDPServer(t, []map[string]any{
		{"access_token": "AT-NEW", "expires_in": 3600},
	}, nil)
	cacheDir := t.TempDir()
	if err := saveSessionCache(cacheDir, sessionCache{
		Schema: sessionCacheSchema, AccessToken: "AT-OLD", RefreshToken: "RT-OLD", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := testClient(srv).GetAccessToken(context.Background(), cacheDir)
	if err != nil || got != "AT-NEW" {
		t.Errorf("GetAccessToken() = (%q, %v), want (\"AT-NEW\", nil)", got, err)
	}
}

func TestSignOutRemovesCacheFileNotTenantFile(t *testing.T) {
	cacheDir := t.TempDir()
	if err := SetTenant(cacheDir, "kiro.uk"); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionCache(cacheDir, sessionCache{Schema: sessionCacheSchema, AccessToken: "AT"}); err != nil {
		t.Fatal(err)
	}
	if err := SignOut(cacheDir); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if _, ok, _ := loadSessionCache(cacheDir); ok {
		t.Error("SignOut left a readable session cache behind")
	}
	tenant, err := GetTenant(cacheDir)
	if err != nil || tenant != "kiro.uk" {
		t.Errorf("GetTenant() = (%q, %v), want (\"kiro.uk\", nil) — SignOut must not touch tenant.txt", tenant, err)
	}
}

func TestSignOutIsNoopWhenAbsent(t *testing.T) {
	if err := SignOut(t.TempDir()); err != nil {
		t.Errorf("SignOut on an absent cache = %v, want nil", err)
	}
}

func TestSetTenantChangeWipesExistingCache(t *testing.T) {
	cacheDir := t.TempDir()
	if err := SetTenant(cacheDir, "tenant-a.example"); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionCache(cacheDir, sessionCache{Schema: sessionCacheSchema, AccessToken: "AT"}); err != nil {
		t.Fatal(err)
	}
	if err := SetTenant(cacheDir, "tenant-b.example"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := loadSessionCache(cacheDir); ok {
		t.Error("changing tenant must wipe the OLD tenant's cached session — a token for a different tenant would just 403")
	}
	tenant, _ := GetTenant(cacheDir)
	if tenant != "tenant-b.example" {
		t.Errorf("GetTenant() = %q, want tenant-b.example", tenant)
	}
}

func TestSetTenantSameValueKeepsCache(t *testing.T) {
	cacheDir := t.TempDir()
	if err := SetTenant(cacheDir, "tenant-a.example"); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionCache(cacheDir, sessionCache{Schema: sessionCacheSchema, AccessToken: "AT"}); err != nil {
		t.Fatal(err)
	}
	if err := SetTenant(cacheDir, "tenant-a.example"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := loadSessionCache(cacheDir); !ok {
		t.Error("re-setting the SAME tenant must not wipe the existing cached session")
	}
}

func TestGetTenantEmptyWhenNeverSet(t *testing.T) {
	got, err := GetTenant(t.TempDir())
	if err != nil || got != "" {
		t.Errorf("GetTenant() = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestLoadSessionCacheCorruptFileDegradesToNotLoggedIn(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheDir+"/"+cacheFileName, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := loadSessionCache(cacheDir)
	if err != nil || ok {
		t.Errorf("loadSessionCache(corrupt) = (ok=%v, err=%v), want (false, nil) — a damaged cache must degrade to not-logged-in, never crash a caller", ok, err)
	}
}
