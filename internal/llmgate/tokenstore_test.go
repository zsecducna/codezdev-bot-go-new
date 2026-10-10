package llmgate

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// tokenstore_test.go — pins for NewWithTokenStore's on-disk login-token
// persistence (the restart-survivability feature): a login writes the file,
// a fresh client over the same file skips that login, anything bogus on
// disk is ignored, and a broken store never fails the login itself.

// newTokenStoreTestServer serves the two endpoints the store tests touch
// (login + list) with a login counter and a record of the last Bearer the
// list endpoint saw — the assertions are "how many logins" and "which
// token was actually used", both observable without touching client
// internals.
func newTokenStoreTestServer(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var logins atomic.Int32
	var lastListAuth atomic.Value // string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", func(w http.ResponseWriter, r *http.Request) {
		n := logins.Add(1)
		writeData(w, http.StatusOK, loginData("jwt-"+string(rune('A'+n-1)), 24*time.Hour))
	})
	mux.HandleFunc("/api/v1/apikey/list", func(w http.ResponseWriter, r *http.Request) {
		lastListAuth.Store(r.Header.Get("Authorization"))
		writeData(w, http.StatusOK, []any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &logins, &lastListAuth
}

// newStoreClient builds a fully-configured client whose token store is path
// — newTestClient's NewWithTokenStore twin.
func newStoreClient(srv *httptest.Server, path string) *Client {
	return NewWithTokenStore(Config{BaseURL: srv.URL, Username: "tester", Password: "secret-pw"}, nil, path)
}

// TestTokenStore_RoundTripSkipsSecondClientsLogin — the restart pin: client
// one's login writes the file, so client two (a "restarted process") built
// with the same path seeds its cache from disk and serves its first call
// WITHOUT a second login, reusing the very token client one obtained.
func TestTokenStore_RoundTripSkipsSecondClientsLogin(t *testing.T) {
	srv, logins, lastListAuth := newTokenStoreTestServer(t)
	path := filepath.Join(t.TempDir(), "token.json")

	c1 := newStoreClient(srv, path)
	if _, err := c1.ListKeys(context.Background()); err != nil {
		t.Fatalf("first client ListKeys: %v", err)
	}
	if n := logins.Load(); n != 1 {
		t.Fatalf("logins = %d after first client, want 1", n)
	}
	// The file must exist, be owner-only, and carry the token + expiry.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token store not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token store mode = %o, want 0600", perm)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token store: %v", err)
	}
	var st storedToken
	if err := json.Unmarshal(blob, &st); err != nil || st.Token != "jwt-A" {
		t.Errorf("stored token = %+v (err %v), want jwt-A", st, err)
	}
	if _, err := time.Parse(time.RFC3339, st.ExpireAt); err != nil {
		t.Errorf("stored expire_at %q unparseable: %v", st.ExpireAt, err)
	}

	// The "restart": a brand-new client over the same file.
	c2 := newStoreClient(srv, path)
	if _, err := c2.ListKeys(context.Background()); err != nil {
		t.Fatalf("second client ListKeys: %v", err)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d after restarted client, want 1 (stored token must skip the login)", n)
	}
	if got, _ := lastListAuth.Load().(string); got != "Bearer jwt-A" {
		t.Errorf("restarted client used %q, want the stored Bearer jwt-A", got)
	}
}

// TestTokenStore_ExpiredOrStaleEntryNotSeeded — a stored entry at/below the
// freshness bar must NOT seed the cache: seeding it would send a token the
// server may already reject (past expiry) or inside the final-minute skew
// window (tokenRefreshMargin), trading one predictable login for a 401
// round trip. Both cases here must produce a fresh login instead.
func TestTokenStore_ExpiredOrStaleEntryNotSeeded(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration // stored expire_at offset from now
	}{
		{name: "expired yesterday", ttl: -24 * time.Hour},
		{name: "expired one second ago", ttl: -1 * time.Second},
		{name: "inside the 60s refresh margin", ttl: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, logins, lastListAuth := newTokenStoreTestServer(t)
			path := filepath.Join(t.TempDir(), "token.json")
			blob, err := json.Marshal(storedToken{
				Token:    "jwt-stale",
				ExpireAt: time.Now().Add(tc.ttl).Format(time.RFC3339),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, blob, 0o600); err != nil {
				t.Fatal(err)
			}

			c := newStoreClient(srv, path)
			if _, err := c.ListKeys(context.Background()); err != nil {
				t.Fatalf("ListKeys: %v", err)
			}
			if n := logins.Load(); n != 1 {
				t.Errorf("logins = %d, want 1 (stale stored token must not seed — a login must happen)", n)
			}
			if got, _ := lastListAuth.Load().(string); got != "Bearer jwt-A" {
				t.Errorf("client used %q, want fresh Bearer jwt-A (not the stale entry)", got)
			}
		})
	}
}

// TestTokenStore_CorruptOrAbsentFileIgnored — the store is an optimization,
// not a dependency: garbage bytes, a missing file, or valid JSON with no
// token must each degrade silently to New's login-on-first-use behavior,
// never wedge construction or the first call.
func TestTokenStore_CorruptOrAbsentFileIgnored(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write string // file content; "" = leave the path absent
	}{
		{name: "garbage bytes", write: "\x00not json{{{"},
		{name: "wrong-shape json", write: `{"unexpected":true}`},
		{name: "empty token", write: `{"token":"","expire_at":"2099-01-01T00:00:00Z"}`},
		{name: "absent file", write: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, logins, _ := newTokenStoreTestServer(t)
			path := filepath.Join(t.TempDir(), "token.json")
			if tc.write != "" {
				if err := os.WriteFile(path, []byte(tc.write), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			c := newStoreClient(srv, path)
			if _, err := c.ListKeys(context.Background()); err != nil {
				t.Fatalf("ListKeys: %v", err)
			}
			if n := logins.Load(); n != 1 {
				t.Errorf("logins = %d, want 1 (bad store must fall back to a normal login)", n)
			}
		})
	}
}

// TestTokenStore_WriteFailureLogsAndStillLogsIn — an unwritable store path
// (here: the parent "directory" is a regular file) must never fail the
// login: the Warn lands in the log, the token stays usable in memory, and
// — security pin — the token VALUE never appears in that log output.
func TestTokenStore_WriteFailureLogsAndStillLogsIn(t *testing.T) {
	srv, logins, lastListAuth := newTokenStoreTestServer(t)
	// blocker is a FILE; the store path "inside" it cannot be created, so
	// the tmp write fails deterministically regardless of privileges (no
	// chmod tricks that a root-run test would silently defeat).
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "token.json")

	var buf bytes.Buffer
	c := NewWithTokenStore(
		Config{BaseURL: srv.URL, Username: "tester", Password: "secret-pw"},
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		path)
	if _, err := c.ListKeys(context.Background()); err != nil {
		t.Fatalf("ListKeys despite broken store: %v", err)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want 1 (write failure must not fail the login)", n)
	}
	if got, _ := lastListAuth.Load().(string); got != "Bearer jwt-A" {
		t.Errorf("client used %q, want Bearer jwt-A (memory token must work)", got)
	}
	if out := buf.String(); !bytes.Contains([]byte(out), []byte("token store")) {
		t.Errorf("log output missing the token-store Warn, got: %q", out)
	}
	if bytes.Contains([]byte(buf.String()), []byte("jwt-A")) {
		t.Errorf("token value leaked into the log output: %q", buf.String())
	}
}
