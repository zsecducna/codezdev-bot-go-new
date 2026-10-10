// Tests for migrate-llmgate: end-to-end through the REAL kirogo and llmgate
// clients against httptest fakes speaking each upstream's wire shape, plus
// the real store on a temp store.db — no interface mocks, so the tests pin
// the whole stack from flag-level Counts down to the SQL commit.
//
// Fake wire shapes:
//   - kirogo: POST /admin/stats with "Authorization: Bearer <admin key>",
//     body {"id":...}|{"apiKey":...}, 2xx body
//     {"success":true,"keys":[{...KeyStat...}],"totals":{...}}
//     (StatsResponse.UnmarshalJSON accepts the "keys" spelling).
//   - llmgate: the {code,message,data} wrapper (package doc). USER realm:
//     POST /api/v1/user/login {"username","password","expire":-1} →
//     data {"token","expire_at"}; POST /api/v1/apikey (create) → data = the
//     Key wire shape (snake_case per llmgate.Key's tags). KEY realm:
//     GET /api/v1/apikey/stats with "Authorization: Bearer <customer key>"
//     → data carrying the key's id (see fakeGate.statsData for the exact
//     superset encoding).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codezdev-bot/internal/kirogo"
	"codezdev-bot/internal/llmgate"
	"codezdev-bot/internal/store"
)

// ---- fake Kiro-Go pool ------------------------------------------------------

// poolKeyState is one fake pool key's live stats: what /admin/stats should
// report for it. CreditLimit/CreditsUsed are derived so the figures are
// internally consistent with CreditsRemaining.
type poolKeyState struct {
	remaining float64 // -1 = unlimited sentinel, exactly as the pool sends it
}

// fakePool is a kirogo-shaped /admin/stats + /admin/delete_api_key server.
// Every stats response is a 2xx {success, keys:[...]} body; unknown ids match
// ZERO keys (the fail-closed input for the migrator), and failAll 404s every
// stats request (the transport-error input). 4xx only on the delete path
// unless failDeleteStatus says otherwise — the real client never retries a
// 4xx, so the tests stay fast.
type fakePool struct {
	mu      sync.Mutex
	calls   int
	byID    map[string]poolKeyState
	byKey   map[string]poolKeyState
	failAll bool
	// deleteCalls / deletedIDs record what the exhausted-delete rule
	// (2026-10-10) actually sent to the pool; failDeleteStatus (0 = succeed)
	// is the pool's answer for the failure/404 legs.
	deleteCalls      int
	deletedIDs       []string
	failDeleteStatus int
}

// newFakePool builds an empty pool.
func newFakePool() *fakePool {
	return &fakePool{byID: map[string]poolKeyState{}, byKey: map[string]poolKeyState{}}
}

// ServeHTTP implements the two admin endpoints: POST /admin/stats (Bearer
// auth check, id/apiKey filter lookup, the {success, keys, totals} answer)
// and POST /admin/delete_api_key (the exhausted-key delete of the 2026-10-10
// rule — same Bearer check, records the identifier it was asked to kill).
func (f *fakePool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	if r.Method != http.MethodPost {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer pool-admin-key" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	// The delete endpoint: uuid (id) primary, cleartext apiKey fallback —
	// exactly the identifier discipline deleteExhaustedPoolKey implements.
	// failDeleteStatus simulates the pool's answer (409 = a "real" failure,
	// 404 = already gone); anything else records a successful delete.
	if r.URL.Path == "/admin/delete_api_key" {
		var req struct {
			ID     string `json:"id"`
			APIKey string `json:"apiKey"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.deleteCalls++
		if f.failDeleteStatus != 0 {
			writeJSON(w, f.failDeleteStatus, map[string]any{"error": "delete failed for test"})
			return
		}
		id := req.ID
		if id == "" {
			id = req.APIKey
		}
		f.deletedIDs = append(f.deletedIDs, id)
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "upstreamRevoked": true})
		return
	}

	if r.URL.Path != "/admin/stats" {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}
	var req struct {
		ID     string `json:"id"`
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if f.failAll {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "pool down for test"})
		return
	}

	// The filter the migrator actually sends: id first, apiKey as fallback.
	state, ok := poolKeyState{}, false
	if req.ID != "" {
		state, ok = f.byID[req.ID]
	} else if req.APIKey != "" {
		state, ok = f.byKey[req.APIKey]
	}

	keys := []map[string]any{}
	if ok {
		// Derive limit/used so the trio is consistent; for the unlimited
		// sentinel the pool reports limit 0 (invariant: -1 remaining ↔
		// limit 0) and zero usage.
		limit, used := state.remaining, 0.0
		if state.remaining == -1 {
			limit, used = 0, 0
		}
		keys = append(keys, map[string]any{
			"id": "stat-id", "name": "key", "keyMasked": "sk-***",
			"enabled": true, "status": "active",
			"creditLimit": limit, "creditsUsed": used,
			"creditsRemaining": state.remaining,
			"tokenLimit":       0, "tokensUsed": 0, "tokensRemaining": -1,
			"requestsCount": 0, "createdAt": 0, "lastUsedAt": 0, "saleClass": "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"keys":    keys,
		"totals":  map[string]any{"keys": len(keys), "tokensUsed": 0, "creditsUsed": 0, "requestsCount": 0},
	})
}

// ---- fake llmgate -----------------------------------------------------------

// fakeGate is an llmgate-shaped server covering the three endpoints this
// migration exercises: login (USER realm auth), key create (USER realm),
// and key stats (KEY realm — the customer key itself rides the Bearer
// header). Counts every login/create so tests can assert "zero llmgate
// creates" for dry runs.
type fakeGate struct {
	mu sync.Mutex

	logins  int
	creates int
	// lastCreateName/lastCreateLimit/lastCreateExpires capture what CreateKey
	// actually sent — how the tests pin the name convention, the credit
	// figure, and the 30-day expiry every migrated key adopts.
	lastCreateName    string
	lastCreateLimit   float64
	lastCreateExpires string

	nextID int64
	// keysByValue: created keys, keyed by cleartext value (what KeyStats
	// authenticates with).
	keysByValue map[string]llmgate.Key
	// statsIDOverride: apiKey value → the id KeyStats should REPORT. Used
	// by the verify-mismatch test (reports an id that is not the created
	// one); absent = report the created id (the honest answer).
	statsIDOverride map[string]int64

	// fail switches, all 4xx so the real client never retries.
	createFail bool // 400 every create
	statsFail  bool // 404 every stats call
}

// newFakeGate builds an empty llmgate; the first minted key gets id 1,
// matching the tests' "llmgate id=1" assertions.
func newFakeGate() *fakeGate {
	return &fakeGate{nextID: 1, keysByValue: map[string]llmgate.Key{}, statsIDOverride: map[string]int64{}}
}

// ServeHTTP routes the three llmgate endpoints.
func (f *fakeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/user/login":
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username != "llm-user" || body.Password != "llm-pass" {
			writeJSON(w, http.StatusUnauthorized, llmgateWrap(401, "bad credentials", nil))
			return
		}
		f.logins++
		writeJSON(w, http.StatusOK, llmgateWrap(200, "success", map[string]any{
			"token":     "test-token-abc",
			"expire_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}))

	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/apikey/create":
		if !f.authed(w, r) {
			return
		}
		if f.createFail {
			writeJSON(w, http.StatusBadRequest, llmgateWrap(400, "create refused for test", nil))
			return
		}
		var body struct {
			Name        string  `json:"name"`
			IsActive    bool    `json:"is_active"`
			CreditLimit float64 `json:"credit_limit"`
			ExpiresAt   string  `json:"expires_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.creates++
		f.lastCreateName, f.lastCreateLimit, f.lastCreateExpires = body.Name, body.CreditLimit, body.ExpiresAt
		id := f.nextID
		f.nextID++
		key := llmgate.Key{
			ID: id, APIKey: fakeLlmgateKey(id), Name: body.Name,
			IsActive: body.IsActive, CreditLimit: body.CreditLimit,
			ExpiresAt: body.ExpiresAt,
		}
		f.keysByValue[key.APIKey] = key
		writeJSON(w, http.StatusOK, llmgateWrap(200, "success", fakeGateKeyWire(key)))

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apikey/stats":
		// KEY realm: the customer key itself is the Bearer token.
		if f.statsFail {
			writeJSON(w, http.StatusNotFound, llmgateWrap(404, "no such key", nil))
			return
		}
		keyVal := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		key, ok := f.keysByValue[keyVal]
		if !ok {
			writeJSON(w, http.StatusNotFound, llmgateWrap(404, "unknown key", nil))
			return
		}
		id := key.ID
		if over, ok := f.statsIDOverride[keyVal]; ok {
			id = over
		}
		writeJSON(w, http.StatusOK, llmgateWrap(200, "success", f.statsData(key, id)))

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// authed checks the USER realm's Bearer JWT (the token login issued).
func (f *fakeGate) authed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer test-token-abc" {
		writeJSON(w, http.StatusUnauthorized, llmgateWrap(401, "no token", nil))
		return false
	}
	return true
}

// fakeLlmgateKey mints a cleartext key value in llmgate's sk-llmgate-...
// shape for a numeric id — long enough to look real, unique per id.
func fakeLlmgateKey(id int64) string {
	return "sk-llmgate-test-" + strings.Repeat("x", 20) + "-" + jsonNumber(id)
}

// jsonNumber renders an int64 without importing strconv in yet another
// spot — trivial helper keeping the fake's key minter one-liner readable.
func jsonNumber(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// fakeGateKeyWire is the Key wire shape (snake_case per llmgate.Key tags).
// expires_at rides only when the key has one — the verified wire fact that
// absent means never-expire.
func fakeGateKeyWire(k llmgate.Key) map[string]any {
	out := map[string]any{
		"id": k.ID, "api_key": k.APIKey, "name": k.Name,
		"is_active": k.IsActive, "credit_limit": k.CreditLimit,
	}
	if k.ExpiresAt != "" {
		out["expires_at"] = k.ExpiresAt
	}
	return out
}

// statsData builds the KeyStats data payload in the exact llmgate.KeyUsage
// wire shape: {"info":<Key wire>,"stats":<UsageStats wire>}. reportID is
// what the answer CLAIMS the key's id is — the honest value in happy paths,
// the wrong value in the verify-mismatch test.
func (f *fakeGate) statsData(k llmgate.Key, reportID int64) map[string]any {
	info := fakeGateKeyWire(k)
	info["id"] = reportID // the mismatches live here, on purpose
	return map[string]any{
		"info": info,
		"stats": map[string]any{
			"input_token": 0, "output_token": 0, "cache_token": 0, "cache_write_token": 0,
			"input_cost": 0, "output_cost": 0, "cache_cost": 0, "cache_write_cost": 0,
			"request_success": 0, "request_failed": 0,
		},
	}
}

// llmgateWrap builds the {code,message,data} envelope every llmgate
// response rides in.
func llmgateWrap(code int, message string, data any) map[string]any {
	return map[string]any{"code": code, "message": message, "data": data}
}

// writeJSON emits v as a JSON HTTP response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- test environment -------------------------------------------------------

// testEnv wires the migrator under test to the two fakes and a real store,
// with everything torn down via t.Cleanup.
type testEnv struct {
	pool *fakePool
	gate *fakeGate
	st   *store.Store
	out  *bytes.Buffer
	m    *Migrator
}

// newEnv starts both fake upstreams, opens a fresh store.db, and builds the
// Migrator with a zero abort delay and a discarding logger (per-row verdicts
// are asserted off the Out transcript, not off log lines).
func newEnv(t *testing.T) *testEnv {
	t.Helper()

	pool := newFakePool()
	poolSrv := httptest.NewServer(pool)
	t.Cleanup(poolSrv.Close)

	gate := newFakeGate()
	gateSrv := httptest.NewServer(gate)
	t.Cleanup(gateSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	out := &bytes.Buffer{}
	m := &Migrator{
		Store: st,
		Pool:  kirogo.New(kirogo.Config{BaseURL: poolSrv.URL, AdminKey: "pool-admin-key"}, logger),
		Gate: llmgate.New(llmgate.Config{
			BaseURL: gateSrv.URL, Username: "llm-user", Password: "llm-pass",
		}, logger),
		Log:        logger,
		Out:        out,
		AbortDelay: 0,
	}
	return &testEnv{pool: pool, gate: gate, st: st, out: out, m: m}
}

// seedRow inserts one live pool-backend api_keys row (the state every
// pre-cutover row is in: backend=”, sale_class per the SKU) and registers
// its pool stats answer. credits is the row's stored total-bought figure;
// remaining is what the POOL reports live (the only figure the migration
// carries).
func (e *testEnv) seedRow(t *testing.T, des string, chat int64, keyID, apiKey string, credits float64, saleClass string, remaining float64) {
	t.Helper()
	ok, err := e.st.AddAPIKey(des, chat, keyID, apiKey, credits, saleClass, store.BackendPool)
	if err != nil || !ok {
		t.Fatalf("seed %s: ok=%v err=%v", des, ok, err)
	}
	e.pool.mu.Lock()
	e.pool.byID[keyID] = poolKeyState{remaining: remaining}
	e.pool.byKey[apiKey] = poolKeyState{remaining: remaining}
	e.pool.mu.Unlock()
}

// row fetches one api_keys row for assertions.
func (e *testEnv) row(t *testing.T, des string) *store.APIKey {
	t.Helper()
	k, err := e.st.APIKeyForOrder(des)
	if err != nil {
		t.Fatalf("fetch %s: %v", des, err)
	}
	if k == nil {
		t.Fatalf("row %s missing", des)
	}
	return k
}

// run is Run with the boilerplate (background ctx, no chat scope) folded in.
func (e *testEnv) run(t *testing.T, apply bool) (Counts, error) {
	t.Helper()
	return e.m.Run(context.Background(), apply, 0, 0)
}

// ---- the eight required cases ------------------------------------------------

// TestDryRunPerformsZeroCreatesAndNoRowChanges: a dry run may read pool
// stats and print the plan, but must not mint (zero llmgate traffic at all
// — not even a login) and must not move a single row.
func TestDryRunPerformsZeroCreatesAndNoRowChanges(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 500, "", 100.5)

	counts, err := e.run(t, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if counts.Migrated != 0 || counts.Planned != 1 {
		t.Errorf("counts: migrated=%d planned=%d, want 0/1", counts.Migrated, counts.Planned)
	}
	e.gate.mu.Lock()
	creates, logins := e.gate.creates, e.gate.logins
	e.gate.mu.Unlock()
	if creates != 0 || logins != 0 {
		t.Errorf("dry run touched llmgate: creates=%d logins=%d, want 0/0", creates, logins)
	}
	k := e.row(t, "ord1")
	if k.Backend != store.BackendPool || k.KeyID != "pool-uuid-1" || k.APIKey != "sk-pool-1" ||
		k.PoolKeyID != "" || k.PoolAPIKey != "" || k.Credits != 500 {
		t.Errorf("row changed by dry run: %+v", k)
	}
	if out := e.out.String(); !strings.Contains(out, "DRY RUN — nothing was written; 1 keys would be migrated, Σ credits 100.50") {
		t.Errorf("dry-run summary line missing/wrong:\n%s", out)
	}
}

// TestApplyMigratesRowHappyPath is the happy path, end to end: an ”-class
// AND a 'bedrock'-class row both migrate (the two classes the store query
// admits), while a kiro row and a refunded row — the two shapes it must
// exclude — are never listed, never minted, never touched.
func TestApplyMigratesRowHappyPath(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 2000, "", 1999.95)
	e.seedRow(t, "ord2", 222, "pool-uuid-2", "sk-pool-2", 100, "bedrock", 10.001)
	// Excluded shapes: kiro-class stays pool-backed BY DECISION; a refunded
	// row has nothing to hand the customer.
	e.seedRow(t, "ordkiro", 333, "pool-uuid-k", "sk-pool-k", 50, "kiro", 50)
	e.seedRow(t, "ordref", 444, "pool-uuid-r", "sk-pool-r", 70, "", 70)
	if err := e.st.MarkAPIKeyRefunded("ordref"); err != nil {
		t.Fatalf("mark refunded: %v", err)
	}

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Migrated != 2 || counts.Planned != 2 {
		t.Errorf("counts: migrated=%d planned=%d, want 2/2 (full tally: %+v)", counts.Migrated, counts.Planned, counts)
	}
	if counts.FailureCount() != 0 {
		t.Errorf("failure count = %d, want 0 (tally: %+v)", counts.FailureCount(), counts)
	}

	e.gate.mu.Lock()
	creates, name1, limit1 := e.gate.creates, e.gate.lastCreateName, e.gate.lastCreateLimit
	e.gate.mu.Unlock()
	if creates != 2 {
		t.Errorf("llmgate creates = %d, want 2 (excluded rows must not mint)", creates)
	}
	// lastCreate* captured the SECOND create (ord2): name convention and
	// the customer-favorable round-UP (10.001 → 10.01).
	if name1 != "tg-222-order-ord2" {
		t.Errorf("create name = %q, want tg-222-order-ord2", name1)
	}
	if limit1 != 10.01 {
		t.Errorf("create credit_limit = %v, want 10.01 (ceil to 2dp)", limit1)
	}

	// ord1: backend flips, key_id becomes the llmgate id STRING, pool values
	// preserved into pool_key_id/pool_api_key, credits = carried remaining.
	k := e.row(t, "ord1")
	if k.Backend != store.BackendLlmgate {
		t.Errorf("ord1 backend = %q, want llmgate", k.Backend)
	}
	if k.KeyID != "1" || k.APIKey != fakeLlmgateKey(1) {
		t.Errorf("ord1 repointed wrong: key_id=%q api_key=%q", k.KeyID, k.APIKey)
	}
	if k.PoolKeyID != "pool-uuid-1" || k.PoolAPIKey != "sk-pool-1" {
		t.Errorf("ord1 pool provenance lost: pool_key_id=%q pool_api_key=%q", k.PoolKeyID, k.PoolAPIKey)
	}
	if k.Credits != 1999.95 {
		t.Errorf("ord1 credits = %v, want 1999.95 (carried remaining)", k.Credits)
	}
	// The success line, verbatim vocabulary.
	if out := e.out.String(); !strings.Contains(out, "migrated des=ord1 chat=111 pool=pool-uuid-1 → llmgate id=1 credits=1999.95") {
		t.Errorf("success line missing/wrong:\n%s", out)
	}

	// The excluded rows: untouched, still pool-backend, originals in place.
	for des, uuid := range map[string]string{"ordkiro": "pool-uuid-k", "ordref": "pool-uuid-r"} {
		k := e.row(t, des)
		if k.Backend != store.BackendPool || k.KeyID != uuid || k.PoolKeyID != "" {
			t.Errorf("%s was listed/touched by the migrator: %+v", des, k)
		}
	}
	if out := e.out.String(); strings.Contains(out, "ordkiro") || strings.Contains(out, "ordref") {
		t.Errorf("excluded rows appear in the plan transcript:\n%s", out)
	}
}

// TestApplyMints30DayExpiry: every migrated key is minted with the SAME
// 30-day validity as a fresh sale (user rule 2026-10-10) — migrated owners
// recharge-to-renew exactly like everyone else, instead of holding a
// privileged never-expiring class. Pinned on the exact create body: an
// RFC3339 expires_at ≈ now+30 days (llmgate.DefaultValidity).
func TestApplyMints30DayExpiry(t *testing.T) {
	e := newEnv(t)
	applyStart := time.Now()
	e.seedRow(t, "ordexp", 111, "pool-uuid-exp", "sk-pool-exp", 100, "", 42)

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1 (tally: %+v)", counts.Migrated, counts)
	}
	e.gate.mu.Lock()
	raw := e.gate.lastCreateExpires
	e.gate.mu.Unlock()
	if raw == "" {
		t.Fatal("create body omitted expires_at — migrated keys must adopt the 30-day validity, not the legacy never-expire shape")
	}
	exp, perr := time.Parse(time.RFC3339, raw)
	if perr != nil {
		t.Fatalf("create expires_at %q is not RFC3339: %v", raw, perr)
	}
	if d := exp.Sub(applyStart.Add(llmgate.DefaultValidity)); d < -time.Hour || d > time.Hour {
		t.Errorf("create expires_at = %v, want ≈ %v (apply-time + 30 days)", exp, applyStart.Add(llmgate.DefaultValidity))
	}
}

// TestExhaustedRowSkipped: remaining <= 0 carries nothing — skipped from
// migration, no mint, row untouched — and, per the 2026-10-10 business
// rule, its pool key IS deleted under -apply.
func TestExhaustedRowSkipped(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 0)

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.SkippedExhausted != 1 || counts.Migrated != 0 || counts.FailedExhaustedDelete != 0 {
		t.Errorf("counts: %+v, want skipped-exhausted=1 migrated=0 failed-exhausted-delete=0", counts)
	}
	e.gate.mu.Lock()
	creates := e.gate.creates
	e.gate.mu.Unlock()
	if creates != 0 {
		t.Errorf("llmgate creates = %d, want 0", creates)
	}
	if k := e.row(t, "ord1"); k.Backend != store.BackendPool || k.KeyID != "pool-uuid-1" {
		t.Errorf("exhausted row changed: %+v", k)
	}
	// The delete: exactly one, for the row's pool uuid, with one transcript
	// line — the stdout record the rule asks for.
	e.pool.mu.Lock()
	deletes, ids := e.pool.deleteCalls, append([]string(nil), e.pool.deletedIDs...)
	e.pool.mu.Unlock()
	if deletes != 1 || len(ids) != 1 || ids[0] != "pool-uuid-1" {
		t.Errorf("pool deletes = %d %v, want exactly [pool-uuid-1]", deletes, ids)
	}
	if out := e.out.String(); !strings.Contains(out, "deleted-exhausted des=ord1 chat=111 pool=pool-uuid-1\n") {
		t.Errorf("transcript missing the deleted-exhausted line:\n%s", out)
	}
}

// TestExhaustedDeleteDryRunDeletesNothing: the DELETE-EXHAUSTED plan line is
// a PREVIEW only — a dry run reaches none of the act-phase deletes (and, as
// ever, mints and writes nothing).
func TestExhaustedDeleteDryRunDeletesNothing(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 0)

	counts, err := e.run(t, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if counts.SkippedExhausted != 1 || counts.Migrated != 0 {
		t.Errorf("counts: %+v, want skipped-exhausted=1 migrated=0", counts)
	}
	if out := e.out.String(); !strings.Contains(out, "DELETE-EXHAUSTED pool=pool-uuid-1") {
		t.Errorf("dry-run plan missing the DELETE-EXHAUSTED line:\n%s", out)
	}
	e.pool.mu.Lock()
	deletes := e.pool.deleteCalls
	e.pool.mu.Unlock()
	if deletes != 0 {
		t.Errorf("pool delete calls = %d, want 0 (a dry run deletes nothing)", deletes)
	}
}

// TestExhaustedDeleteFailureCountedNotFatal: a delete failing with anything
// but 404 bumps failed-exhausted-delete — and the run CONTINUES: the healthy
// row after the failing one still migrates. (409, not 5xx, so the kirogo
// client's 5xx retry discipline does not slow the test; the bucket logic
// only cares that it is not a 404.)
func TestExhaustedDeleteFailureCountedNotFatal(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ordx", 111, "pool-uuid-x", "sk-pool-x", 100, "", 0)
	e.seedRow(t, "ordok", 222, "pool-uuid-ok", "sk-pool-ok", 100, "", 42)
	e.pool.mu.Lock()
	e.pool.failDeleteStatus = http.StatusConflict
	e.pool.mu.Unlock()

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v (a failed delete must not abort the run)", err)
	}
	if counts.FailedExhaustedDelete != 1 || counts.Migrated != 1 || counts.SkippedExhausted != 1 {
		t.Errorf("counts: %+v, want failed-exhausted-delete=1 migrated=1 skipped-exhausted=1", counts)
	}
	if out := e.out.String(); !strings.Contains(out, "failed-exhausted-delete des=ordx") ||
		!strings.Contains(out, "failed-exhausted-delete=1") {
		t.Errorf("transcript missing the failure line and/or the summary bucket:\n%s", out)
	}
}

// TestExhaustedDelete404IsFine: 404 means the key is already gone — the
// rule's desired end state — so it is NOT a failure bucket, and the
// transcript still carries one accounting line for the row.
func TestExhaustedDelete404IsFine(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 0)
	e.pool.mu.Lock()
	e.pool.failDeleteStatus = http.StatusNotFound
	e.pool.mu.Unlock()

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.FailedExhaustedDelete != 0 || counts.SkippedExhausted != 1 || counts.Migrated != 0 {
		t.Errorf("counts: %+v, want failed-exhausted-delete=0 skipped-exhausted=1 migrated=0", counts)
	}
	if out := e.out.String(); !strings.Contains(out, "deleted-exhausted des=ord1 chat=111 pool=pool-uuid-1 (already gone, 404)") {
		t.Errorf("transcript missing the 404 deleted-exhausted line:\n%s", out)
	}
}

// TestExhaustedDeleteNoUuidRowUsesCleartextSilently: a legacy row with NO
// stored uuid is deleted by its cleartext pool key (the request-side
// fallback), but the transcript must never print that key — pool= stays the
// uuid slot (empty here), same secret hygiene as every other line.
func TestExhaustedDeleteNoUuidRowUsesCleartextSilently(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ordnv", 111, "", "sk-pool-nv", 100, "", 0)

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.SkippedExhausted != 1 || counts.FailedExhaustedDelete != 0 {
		t.Errorf("counts: %+v, want skipped-exhausted=1 failed-exhausted-delete=0", counts)
	}
	e.pool.mu.Lock()
	ids := append([]string(nil), e.pool.deletedIDs...)
	e.pool.mu.Unlock()
	if len(ids) != 1 || ids[0] != "sk-pool-nv" {
		t.Errorf("pool deletes = %v, want the cleartext fallback [sk-pool-nv]", ids)
	}
	out := e.out.String()
	if strings.Contains(out, "sk-pool-nv") {
		t.Errorf("transcript leaked the cleartext pool key:\n%s", out)
	}
	if !strings.Contains(out, "deleted-exhausted des=ordnv chat=111 pool=\n") {
		t.Errorf("transcript missing the deleted-exhausted line (empty uuid slot):\n%s", out)
	}
}

// TestUnlimitedRowSkipped: -1 remaining is the unlimited sentinel — a loud
// skip for hand-migration, never an automated unlimited mint.
func TestUnlimitedRowSkipped(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", -1)

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.SkippedUnlimited != 1 || counts.Migrated != 0 {
		t.Errorf("counts: %+v, want skipped-unlimited=1 migrated=0", counts)
	}
	e.gate.mu.Lock()
	creates := e.gate.creates
	e.gate.mu.Unlock()
	if creates != 0 {
		t.Errorf("llmgate creates = %d, want 0 (never mint unlimited)", creates)
	}
	if k := e.row(t, "ord1"); k.Backend != store.BackendPool {
		t.Errorf("unlimited row changed: %+v", k)
	}
	if out := e.out.String(); !strings.Contains(out, "SKIP unlimited") {
		t.Errorf("plan transcript missing the loud unlimited skip:\n%s", out)
	}
}

// TestPoolStatsErrorSkippedFailClosed: an unreadable pool key (id lookup
// AND apiKey fallback both fail) must skip the row and mint nothing — the
// tool never guesses a quota. Two rows prove the run CONTINUES past the
// failure.
func TestPoolStatsErrorSkippedFailClosed(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)
	e.seedRow(t, "ord2", 222, "pool-uuid-2", "sk-pool-2", 100, "", 42)
	e.pool.mu.Lock()
	e.pool.failAll = true
	e.pool.mu.Unlock()

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v (stats failures must not abort the run)", err)
	}
	if counts.SkippedStats != 2 || counts.Migrated != 0 {
		t.Errorf("counts: %+v, want skipped-stats=2 migrated=0", counts)
	}
	e.gate.mu.Lock()
	creates := e.gate.creates
	e.gate.mu.Unlock()
	if creates != 0 {
		t.Errorf("llmgate creates = %d, want 0 (fail-closed)", creates)
	}
	for _, des := range []string{"ord1", "ord2"} {
		if k := e.row(t, des); k.Backend != store.BackendPool {
			t.Errorf("%s changed despite stats failure: %+v", des, k)
		}
	}
}

// TestVerifyMismatchCountedFailedVerifyRowNotMigrated: KeyStats answering
// with a DIFFERENT id than CreateKey returned is a failed verify — the row
// must NOT be committed, the count must say failed-verify, and the
// transcript must carry the CRITICAL line naming the unrecorded key's id
// and credit limit for manual cleanup.
func TestVerifyMismatchCountedFailedVerifyRowNotMigrated(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42.5)
	e.gate.mu.Lock()
	e.gate.statsIDOverride[fakeLlmgateKey(1)] = 999 // stats will claim id 999
	e.gate.mu.Unlock()

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.FailedVerify != 1 || counts.Migrated != 0 || counts.SkippedRace != 0 {
		t.Errorf("counts: %+v, want failed-verify=1 migrated=0", counts)
	}
	if k := e.row(t, "ord1"); k.Backend != store.BackendPool || k.KeyID != "pool-uuid-1" {
		t.Errorf("row committed despite verify mismatch: %+v", k)
	}
	out := e.out.String()
	if !strings.Contains(out, "CRITICAL failed-verify") || !strings.Contains(out, "id=1") || !strings.Contains(out, "credit_limit=42.50") {
		t.Errorf("CRITICAL unrecorded-key line missing id/credit_limit:\n%s", out)
	}
	if counts.FailureCount() != 1 {
		t.Errorf("failure count = %d, want 1 (non-zero exit contract)", counts.FailureCount())
	}
}

// TestChatRehearsalScopeFiltersRowsAndCount: -chat is the operator's dress
// rehearsal — with chat=222 set, ONLY that chat's rows are counted, listed,
// and migrated (rows of other chats and even other eligible chats stay
// pool-backed, untouched); the plan headline names the scope. chat=0 keeps
// the unscoped behavior (both chats migrate), which the other tests already
// pin — this test contrasts the two directly.
func TestChatRehearsalScopeFiltersRowsAndCount(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)
	e.seedRow(t, "ord2", 222, "pool-uuid-2", "sk-pool-2", 100, "bedrock", 43)

	// Scoped apply: only chat 222's row is in play.
	counts, err := e.m.Run(context.Background(), true, 0, 222)
	if err != nil {
		t.Fatalf("scoped apply: %v", err)
	}
	if counts.Eligible != 1 || counts.Planned != 1 || counts.Migrated != 1 {
		t.Errorf("scoped counts: %+v, want eligible/planned/migrated = 1/1/1", counts)
	}
	if k := e.row(t, "ord1"); k.Backend != store.BackendPool || k.KeyID != "pool-uuid-1" {
		t.Errorf("out-of-scope row migrated by -chat run: %+v", k)
	}
	if k := e.row(t, "ord2"); k.Backend != store.BackendLlmgate || k.KeyID != "1" {
		t.Errorf("in-scope row not migrated: %+v", k)
	}
	if out := e.out.String(); !strings.Contains(out, "plan: 1 eligible key(s) to migrate (rehearsal chat=222)") {
		t.Errorf("plan headline must name the rehearsal scope:\n%s", out)
	}
	if out := e.out.String(); strings.Contains(out, "des=ord1") {
		t.Errorf("out-of-scope row leaked into the scoped transcript:\n%s", out)
	}

	// Unscoped follow-up: chat 0 behaves as before — the remaining row is
	// eligible and migrates normally.
	e.out.Reset()
	counts, err = e.m.Run(context.Background(), true, 0, 0)
	if err != nil {
		t.Fatalf("unscoped apply: %v", err)
	}
	if counts.Eligible != 1 || counts.Migrated != 1 {
		t.Errorf("unscoped counts: %+v, want eligible/migrated = 1/1 (chat=0 must keep the old behavior)", counts)
	}
	if k := e.row(t, "ord1"); k.Backend != store.BackendLlmgate {
		t.Errorf("chat=0 run left the row unmigrated: %+v", k)
	}
}

// TestSecondRunOverSameDBMigratesNothing: idempotence via backend=” —
// after a successful apply, a re-run lists 0 rows, mints nothing, and
// still exits clean.
func TestSecondRunOverSameDBMigratesNothing(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)

	if _, err := e.run(t, true); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	e.gate.mu.Lock()
	createsAfterFirst := e.gate.creates
	e.gate.mu.Unlock()

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if counts.Eligible != 0 || counts.Planned != 0 || counts.Migrated != 0 {
		t.Errorf("second run counts: %+v, want all zero", counts)
	}
	e.gate.mu.Lock()
	createsAfterSecond := e.gate.creates
	e.gate.mu.Unlock()
	if createsAfterSecond != createsAfterFirst {
		t.Errorf("second run minted %d new key(s)", createsAfterSecond-createsAfterFirst)
	}
	if counts.FailureCount() != 0 {
		t.Errorf("failure count = %d, want 0", counts.FailureCount())
	}
	if out := e.out.String(); !strings.Contains(out, "plan: 0 eligible key(s) to migrate") {
		t.Errorf("second run plan headline missing:\n%s", out)
	}
	// The first run's row must be INTACT after the second — no double
	// migration, no clobbering of the preserved pool columns.
	k := e.row(t, "ord1")
	if k.Backend != store.BackendLlmgate || k.PoolKeyID != "pool-uuid-1" || k.KeyID != "1" {
		t.Errorf("migrated row disturbed by second run: %+v", k)
	}
}

// ---- the failure branches the summary's other buckets exist for --------------

// TestLostRaceOrphansKey: if the commit loses to a concurrent migration
// (the store's WHERE backend=” guard returns false), the row is safe but
// the just-minted key is ORPHANED — skipped-race counted, CRITICAL line
// naming the orphan's id, and the row keeps the WINNER's values.
func TestLostRaceOrphansKey(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)

	// The seam fires between verify and commit: a "someone else" migrates
	// the row first, through the REAL store method, so the false return the
	// migrator sees is the genuine guard, not a mock.
	e.m.PreCommitHook = func(row *store.APIKey) {
		moved, err := e.st.MigrateAPIKeyToLlmgate(
			row.OrderID, row.KeyID, row.APIKey, "999", "sk-elsewhere", 1)
		if err != nil || !moved {
			t.Errorf("hook's competing migration failed: moved=%v err=%v", moved, err)
		}
	}

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v (a lost race must not abort the run)", err)
	}
	if counts.SkippedRace != 1 || counts.Migrated != 0 || counts.FailedVerify != 0 {
		t.Errorf("counts: %+v, want skipped-race=1 migrated=0", counts)
	}
	k := e.row(t, "ord1")
	if k.KeyID != "999" || k.APIKey != "sk-elsewhere" {
		t.Errorf("race winner's values clobbered: %+v", k)
	}
	out := e.out.String()
	if !strings.Contains(out, "CRITICAL skipped-race") || !strings.Contains(out, "id=1") {
		t.Errorf("CRITICAL orphan line missing the orphaned key's id:\n%s", out)
	}
	if counts.FailureCount() != 1 {
		t.Errorf("failure count = %d, want 1 (a lost race is a non-zero exit)", counts.FailureCount())
	}
}

// TestFailedMintContinuesToNextRow: a create refusal is counted failed-mint
// and the run CONTINUES — the second row still migrates.
func TestFailedMintContinuesToNextRow(t *testing.T) {
	e := newEnv(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)
	e.seedRow(t, "ord2", 222, "pool-uuid-2", "sk-pool-2", 100, "", 43)
	e.gate.mu.Lock()
	e.gate.createFail = true
	e.gate.mu.Unlock()

	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v (mint failures must not abort the run)", err)
	}
	if counts.FailedMint != 2 || counts.Migrated != 0 {
		t.Errorf("counts: %+v, want failed-mint=2 migrated=0", counts)
	}
	for _, des := range []string{"ord1", "ord2"} {
		if k := e.row(t, des); k.Backend != store.BackendPool {
			t.Errorf("%s committed despite failed mint: %+v", des, k)
		}
	}
	if counts.FailureCount() != 2 {
		t.Errorf("failure count = %d, want 2", counts.FailureCount())
	}
}

// ---- pure pieces -------------------------------------------------------------

// TestResolveSettingsPrecedence: flag beats env beats default, and only the
// llmgate base has a default.
func TestResolveSettingsPrecedence(t *testing.T) {
	env := map[string]string{
		"KIRO_GO_BASE_URL": "https://pool.env", "KIRO_GO_ADMIN_KEY": "env-pool-key",
		"LLMGATE_BASE_URL": "https://gate.env", "LLMGATE_USERNAME": "env-user", "LLMGATE_PASSWORD": "env-pass",
	}
	getenv := func(k string) string { return env[k] }

	// No flags: everything falls to env, llmgate base to env's value.
	s := resolveSettings(settingsInput{}, getenv)
	if s.poolBase != "https://pool.env" || s.poolKey != "env-pool-key" ||
		s.lgBase != "https://gate.env" || s.lgUser != "env-user" || s.lgPass != "env-pass" {
		t.Errorf("env fallback: %+v", s)
	}

	// Flags win over env.
	s = resolveSettings(settingsInput{
		poolBase: "https://pool.flag", poolKey: "flag-pool-key",
		lgBase: "https://gate.flag", lgUser: "flag-user", lgPass: "flag-pass",
	}, getenv)
	if s.poolBase != "https://pool.flag" || s.poolKey != "flag-pool-key" ||
		s.lgBase != "https://gate.flag" || s.lgUser != "flag-user" || s.lgPass != "flag-pass" {
		t.Errorf("flag precedence: %+v", s)
	}

	// No flag, no env for the llmgate base → the shop's own deployment; the
	// other knobs have NO default (they must fail the Configured gate).
	s = resolveSettings(settingsInput{}, func(string) string { return "" })
	if s.lgBase != defaultLlmgateBase {
		t.Errorf("llmgate base default = %q, want %q", s.lgBase, defaultLlmgateBase)
	}
	if s.poolBase != "" || s.poolKey != "" || s.lgUser != "" || s.lgPass != "" {
		t.Errorf("unexpected defaults for credential knobs: %+v", s)
	}
}

// TestRoundUpTwoDecimalsCustomerFavorable pins the rounding rule directly:
// always UP at the cent boundary (the customer's residue is never shaved),
// with float artifacts (1999.95*100 = 199994.999...) landing back on the
// exact figure.
func TestRoundUpTwoDecimalsCustomerFavorable(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{10.001, 10.01},
		{1999.95, 1999.95},
		{0.03, 0.03},
		{1.999999, 2},
	}
	for _, c := range cases {
		if got := roundUp2(c.in); got != c.want {
			t.Errorf("roundUp2(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ---- the durable journal ------------------------------------------------------

// journalPath builds a journal path inside a fresh temp dir.
func journalPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "store.db.migrate-journal")
}

// TestJournalRecordsMintingAndCommitted: the happy path appends the MINTING
// intent BEFORE the create and the COMMITTED terminal line after the
// commit — two lines per row, in that order, carrying the des/name/credits
// an operator needs to hunt an orphan in the llmgate console.
func TestJournalRecordsMintingAndCommitted(t *testing.T) {
	e := newEnv(t)
	e.m.JournalPath = journalPath(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)

	if _, err := e.run(t, true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := os.ReadFile(e.m.JournalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d, want exactly 2 (MINTING + COMMITTED):\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], " MINTING des=ord1 name=tg-111-order-ord1 credits=42.00") {
		t.Errorf("MINTING line wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "COMMITTED des=ord1 llmkey=1") {
		t.Errorf("COMMITTED line wrong: %q", lines[1])
	}
	// The unix-nano timestamp prefix must be present and numeric so entries
	// order across runs.
	ts := strings.SplitN(lines[0], " ", 2)[0]
	for _, ch := range ts {
		if ch < '0' || ch > '9' {
			t.Errorf("journal line does not start with a unix-nano timestamp: %q", lines[0])
			break
		}
	}
	// A clean journal warns about nothing: re-running apply over it must not
	// emit the interrupted-MINTING warning.
	e.out.Reset()
	if _, err := e.run(t, true); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if strings.Contains(e.out.String(), "possible orphaned llmgate key") {
		t.Errorf("clean journal still warned:\n%s", e.out.String())
	}
}

// TestJournalInterruptedMintingWarnsOnNextApply: a journal left with a
// MINTING and no terminal line (hard crash between create and commit) must
// produce a LOUD warning at the next apply-mode start, naming the mint
// (des + the llmgate key name to check). Once a terminal line arrives for
// that des, the warning stops — the entry is resolved.
func TestJournalInterruptedMintingWarnsOnNextApply(t *testing.T) {
	e := newEnv(t)
	jp := journalPath(t)
	// Simulate the crashed run: it wrote the mint intent, then died.
	if err := os.WriteFile(jp, []byte(
		"1780000000000000000 MINTING des=ordX name=tg-111-order-ordX credits=42.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.m.JournalPath = jp

	// The DB is empty (the commit never landed), so this run migrates
	// nothing — but the STARTUP SCAN must still fire the warning.
	counts, err := e.run(t, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Migrated != 0 {
		t.Fatalf("migrated = %d, want 0 (nothing to migrate)", counts.Migrated)
	}
	out := e.out.String()
	if !strings.Contains(out, "possible orphaned llmgate key from an interrupted run") {
		t.Errorf("warning line missing:\n%s", out)
	}
	if !strings.Contains(out, "name=tg-111-order-ordX") {
		t.Errorf("warning must NAME the orphan candidate key, got:\n%s", out)
	}

	// Resolve the entry by hand (operator deleted the orphan): the terminal
	// line closes it, and the next start stays quiet.
	f, err := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("1780000001000000000 FAILED-MINT des=ordX\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	e.out.Reset()
	if _, err := e.run(t, true); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if strings.Contains(e.out.String(), "possible orphaned llmgate key") {
		t.Errorf("resolved entry still warns:\n%s", e.out.String())
	}
}

// TestJournalDryRunWritesNothing: a dry run mints nothing, so it must also
// leave NO journal trace — the file is not even created.
func TestJournalDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.m.JournalPath = journalPath(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)

	if _, err := e.run(t, false); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, statErr := os.Stat(e.m.JournalPath); !os.IsNotExist(statErr) {
		t.Errorf("dry run wrote the journal (stat err = %v) — mint-less runs must journal nothing", statErr)
	}
}

// TestJournalFailurePathsGetTerminalLines: FAILED-MINT and RACE (and by the
// same write sites FAILED-VERIFY) each close their MINTING so an
// interrupted-run warning is reserved for genuinely unresolved mints.
func TestJournalFailurePathsGetTerminalLines(t *testing.T) {
	// failed-mint closes its entry
	e := newEnv(t)
	e.m.JournalPath = journalPath(t)
	e.seedRow(t, "ord1", 111, "pool-uuid-1", "sk-pool-1", 100, "", 42)
	e.gate.mu.Lock()
	e.gate.createFail = true
	e.gate.mu.Unlock()
	if _, err := e.run(t, true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := os.ReadFile(e.m.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "MINTING des=ord1") || !strings.Contains(string(data), "FAILED-MINT des=ord1") {
		t.Errorf("failed-mint journal missing MINTING/FAILED-MINT pair:\n%s", data)
	}
	e.out.Reset()
	if _, err := e.run(t, true); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if strings.Contains(e.out.String(), "possible orphaned llmgate key") {
		t.Errorf("a FAILED-MINT-terminal entry must not warn as interrupted:\n%s", e.out.String())
	}

	// RACE closes its entry with the orphaned key id
	e2 := newEnv(t)
	e2.m.JournalPath = journalPath(t)
	e2.seedRow(t, "ord2", 222, "pool-uuid-2", "sk-pool-2", 100, "", 42)
	e2.m.PreCommitHook = func(row *store.APIKey) {
		if _, err := e2.st.MigrateAPIKeyToLlmgate(row.OrderID, row.KeyID, row.APIKey, "999", "sk-elsewhere", 1); err != nil {
			t.Errorf("hook migration: %v", err)
		}
	}
	if _, err := e2.run(t, true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data2, err := os.ReadFile(e2.m.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data2), "RACE des=ord2 llmkey=1") {
		t.Errorf("race terminal line missing:\n%s", data2)
	}
	e2.out.Reset()
	if _, err := e2.run(t, true); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if strings.Contains(e2.out.String(), "possible orphaned llmgate key") {
		t.Errorf("a RACE-terminal entry must not warn as interrupted:\n%s", e2.out.String())
	}
}
