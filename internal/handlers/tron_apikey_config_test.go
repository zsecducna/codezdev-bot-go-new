package handlers

// tron_apikey_config_test.go — task #16: TRON_API_KEY must reach TronGrid,
// the deposit-CREDIT path (money in), from dir/.env alone.
//
// Before this fix, internal/pay/tron.apiGet read os.Getenv("TRON_API_KEY")
// itself. Go's config.Load never copies its .env-merged snapshot back into
// the real process environment (config.Load's own doc comment; unlike the
// Python original, which mutates os.environ so a bare getenv stays live) —
// so a TRON_API_KEY supplied only via .env was invisible to that raw read,
// and every TronGrid call went out unauthenticated, hitting TronGrid's
// unauthenticated per-IP rate limit on exactly the path that credits a
// customer's on-chain deposit.
//
// This test proves the fix end to end: it builds *config.Config via
// config.Load(dir) against a dir/.env file that carries TRON_API_KEY —
// never os.Setenv/t.Setenv, so a raw os.Getenv("TRON_API_KEY") in this same
// process finds nothing — wires a real Bot from it (New(), the exact
// production closure assignment dispatch.go performs, not a hand-rolled
// substitute), and asserts the header a fake TronGrid actually receives
// when the production b.tronTransfersSince seam (the watcher's one chain
// read) is called.
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/pay/tron"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
)

// TestTronWatcherSendsEnvOnlyAPIKeyHeader is the money-path half of task #16.
func TestTronWatcherSendsEnvOnlyAPIKeyHeader(t *testing.T) {
	if _, ok := os.LookupEnv("TRON_API_KEY"); ok {
		t.Fatal("fixture broken: TRON_API_KEY is set in the ambient environment, so this test would pass even if apiGet read raw env")
	}

	// A fake TronGrid that records the TRON-PRO-API-KEY header it received
	// on every request and returns one confirmed transfer, so the watcher
	// fetch's happy path is exercised, not just its error path (an
	// empty/missing header could otherwise pass by never being asked for).
	const wantKey = "env-only-tron-key"
	trxHash := hash64("9")
	var gotHeader string
	trx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("TRON-PRO-API-KEY")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []any{map[string]any{
				"transaction_id":  trxHash,
				"type":            "Transfer",
				"from":            "TSender",
				"value":           "6000000",
				"block_timestamp": float64(time.Now().UnixMilli()),
				"token_info":      map[string]any{"address": tron.USDTContract},
			}},
		})
	}))
	defer trx.Close()
	// TRON_API_BASE is a separate, still-live os.Getenv read (tron.go's
	// apiBase()) untouched by this fix — override it ambient, matching its
	// own resolution rule, not through .env.
	t.Setenv("TRON_API_BASE", trx.URL)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"TRON_ADDRESS="+testSellerTRX+"\n"+
			"TRON_API_KEY="+wantKey+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "1"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })

	// b.tronTransfersSince is the exact closure dispatch.go's New() installs
	// for the hands-free watcher — not overridden here — so this calls
	// production code all the way down to apiGet.
	transfers, err := b.tronTransfersSince(0)
	if err != nil {
		t.Fatalf("tronTransfersSince error = %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("tronTransfersSince returned %d transfers, want 1", len(transfers))
	}
	if gotHeader != wantKey {
		t.Errorf("TronGrid saw TRON-PRO-API-KEY = %q, want the .env-only value %q", gotHeader, wantKey)
	}
}
