package handlers

// apikey_llmgate_test.go — the llmgate-cutover acceptance pins. Mirrors the
// fakeKiro pattern: an httptest stand-in for the llmgate user/key-management
// API (login wrapper {code,message,data}, create/update/delete/stats), driving
// the REAL handlers so every wire fact the cutover leans on is pinned against
// the exact JSON that goes out:
//
//   - bedrock mints CreateKey with the deterministic name + credit_limit and
//     a 30-day expires_at on the wire (business rule 2026-10-10);
//   - the row is stored backend=llmgate with the DECIMAL key id, and the pool
//     fake receives NOTHING for a bedrock sale;
//   - recharge is a read-modify-write: update carries the CURRENT name and
//     is_active true, sets the ABSOLUTE new limit (10 + 50 = 60) and RENEWS
//     expires_at by ≈30 more days; an EXPIRED key is refused at the gate and
//     refunded at delivery, never renewed;
//   - unlimited keys refuse top-ups; unreadable stats fail CLOSED at the gate;
//   - revoke treats 404 as "already gone";
//   - My-Keys renders llmgate rows with live stats (∞ when unlimited) and the
//     reveal re-sends the DeliveredLlmgate wording.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/llmgate"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// fakeLlmgateKey is one llmgate key as the fake server sees it. Used is the
// SUM of the four cost buckets the /stats endpoint reports. ExpiresAt is the
// RAW ISO string ("" = never expires, the pre-2026-10-10 mint shape) so the
// fake mirrors the verified wire fact that stats includes expires_at ONLY
// when the key has one.
type fakeLlmgateKey struct {
	ID        int64
	Name      string
	Key       string // full cleartext, "sk-llmgate-…" (59 chars)
	Limit     float64
	Used      float64
	Alive     bool // is_active
	Gone      bool // deleted
	ExpiresAt string
}

// fakeLlmgate is an httptest stand-in for the llmgate API behind
// api.codezdev-shop.uk. It records every request body verbatim (so the
// payload pins — no expires_at on create, current name on update — can
// assert on the exact JSON that went out) and lets a test force a 4xx per
// endpoint (internal/llmgate never retries a 4xx, so failure tests stay
// instant).
type fakeLlmgate struct {
	mu  sync.Mutex
	srv *httptest.Server

	Logins  int // POST /api/v1/user/login serves (the client caches ~30d tokens, so this stays tiny)
	Creates []map[string]any
	Updates []map[string]any
	Deletes []string // the id path segment of every delete call
	Lists   int      // GET /api/v1/apikey/list serves (the reconciliation round trip)
	Reveals []string // the id path segment of every reveal call

	// Forced non-2xx status per endpoint (0 = respond normally). 4xx only.
	CreateStatus int
	UpdateStatus int
	DeleteStatus int
	StatsStatus  int

	// CreateAmbiguous models the money-danger window CreateKey's
	// single-attempt policy exists for: the server PROCESSES the create
	// (the key exists and is billable from now on) but the response is a
	// 500 — what a timeout-after-commit looks like from the client side.
	// The client must give up after ONE attempt and reconcile via
	// list+reveal instead of re-sending the create.
	CreateAmbiguous bool

	keys map[int64]*fakeLlmgateKey
	seq  int
}

// newFakeLlmgate builds and serves the double; the server closes with the test.
func newFakeLlmgate(t *testing.T) *fakeLlmgate {
	t.Helper()
	fl := &fakeLlmgate{keys: map[int64]*fakeLlmgateKey{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/login", fl.handleLogin)
	mux.HandleFunc("/api/v1/apikey/create", fl.handleCreate)
	mux.HandleFunc("/api/v1/apikey/update", fl.handleUpdate)
	mux.HandleFunc("/api/v1/apikey/stats", fl.handleStats)
	mux.HandleFunc("/api/v1/apikey/delete/", fl.handleDelete)
	mux.HandleFunc("/api/v1/apikey/list", fl.handleList)
	// Catch-all prefix so /api/v1/apikey/{id}/reveal (id varies) reaches its
	// handler; every other /apikey route above is a longer exact/prefix
	// pattern, so ServeMux's longest-match rule keeps them winning.
	mux.HandleFunc("/api/v1/apikey/", fl.handleReveal)
	fl.srv = httptest.NewServer(mux)
	t.Cleanup(fl.srv.Close)
	return fl
}

// wrap writes one llmgate envelope: {"code":200,"message":"success","data":…}
// with the HTTP status matching the body code, the shape every response from
// this API carries (internal/llmgate package doc).
func wrap(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := map[string]any{"code": code, "message": "success"}
	if data != nil {
		enc["data"] = data
	}
	_ = json.NewEncoder(w).Encode(enc)
}

// reject answers a forced/real 4xx in the envelope shape.
func reject(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

func (fl *fakeLlmgate) decode(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	return body
}

// handleLogin exchanges the panel credentials for a Bearer JWT. The expire_at
// is comfortably in the future so the client caches the token for the whole
// test (one login per Bot, the load shape tokenMu exists to produce).
func (fl *fakeLlmgate) handleLogin(w http.ResponseWriter, r *http.Request) {
	fl.mu.Lock()
	fl.Logins++
	fl.mu.Unlock()
	wrap(w, 200, map[string]any{
		"token":     "eyJhbGciOiJIUzI1NiJ9.test.test",
		"expire_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
}

// handleCreate mints a key: id = sequence, cleartext = "sk-llmgate-" + a
// 48-char zero-padded decimal tail (the real shape is 59 chars). The response
// carries the cleartext exactly ONCE, like the real create. CreateAmbiguous
// mints the key AND answers 500 — committed-but-unanswered, the exact input
// the reconciliation path exists to recover from.
func (fl *fakeLlmgate) handleCreate(w http.ResponseWriter, r *http.Request) {
	body := fl.decode(r)
	fl.mu.Lock()
	fl.Creates = append(fl.Creates, body)
	status := fl.CreateStatus
	ambiguous := fl.CreateAmbiguous
	if status == 0 || ambiguous {
		fl.seq++
		id := int64(fl.seq)
		name, _ := body["name"].(string)
		limit, _ := body["credit_limit"].(float64)
		expires, _ := body["expires_at"].(string)
		key := fmt.Sprintf("sk-llmgate-%048d", id)
		fl.keys[id] = &fakeLlmgateKey{ID: id, Name: name, Key: key, Limit: limit, Alive: true, ExpiresAt: expires}
		fl.mu.Unlock()
		if ambiguous {
			reject(w, 500, "create committed but the answer was lost")
			return
		}
		created := map[string]any{
			"id": id, "api_key": key, "name": name,
			"is_active": true, "credit_limit": limit,
		}
		if expires != "" {
			created["expires_at"] = expires
		}
		wrap(w, 200, created)
		return
	}
	fl.mu.Unlock()
	reject(w, status, "create rejected")
}

// handleUpdate applies an ABSOLUTE write (name / is_active / credit_limit) and
// answers with NO api_key — only api_key_length, mirroring the verified real
// update response the handlers must not trust a cleartext off.
func (fl *fakeLlmgate) handleUpdate(w http.ResponseWriter, r *http.Request) {
	body := fl.decode(r)
	fl.mu.Lock()
	fl.Updates = append(fl.Updates, body)
	status := fl.UpdateStatus
	id, _ := body["id"].(float64)
	k := fl.keys[int64(id)]
	if status == 0 && k != nil && !k.Gone {
		if name, ok := body["name"].(string); ok {
			k.Name = name
		}
		if act, ok := body["is_active"].(bool); ok {
			k.Alive = act
		}
		if lim, ok := body["credit_limit"].(float64); ok {
			k.Limit = lim
		}
		// expires_at: the bot always sends a concrete renewal time on this
		// path, so apply it when present; an ABSENT field leaves the stored
		// expiry alone (the omission semantics of the real update endpoint
		// are unverified, and no test exercises that shape).
		if exp, ok := body["expires_at"].(string); ok {
			k.ExpiresAt = exp
		}
		updated := map[string]any{
			"id": k.ID, "name": k.Name, "is_active": k.Alive,
			"credit_limit": k.Limit, "api_key_length": 59,
		}
		if k.ExpiresAt != "" {
			updated["expires_at"] = k.ExpiresAt
		}
		fl.mu.Unlock()
		wrap(w, 200, updated)
		return
	}
	fl.mu.Unlock()
	if status != 0 {
		reject(w, status, "update rejected")
		return
	}
	reject(w, 404, "key not found")
}

// handleStats serves the KEY realm: the Authorization Bearer IS the customer's
// own key. An unknown bearer is a 401 (the key itself is bad — the client must
// NOT re-login-retry it). Used rides the input_cost bucket alone (KeyUsage
// sums the four buckets).
func (fl *fakeLlmgate) handleStats(w http.ResponseWriter, r *http.Request) {
	fl.mu.Lock()
	status := fl.StatsStatus
	var k *fakeLlmgateKey
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	for _, cand := range fl.keys {
		if cand.Key == bearer {
			k = cand
			break
		}
	}
	if k != nil && k.Gone {
		k = nil // a deleted key no longer authenticates
	}
	var limit, used float64
	var alive bool
	var name, expires string
	var id int64
	if k != nil {
		limit, used, alive, name, id, expires = k.Limit, k.Used, k.Alive, k.Name, k.ID, k.ExpiresAt
	}
	fl.mu.Unlock()
	if status != 0 {
		reject(w, status, "stats rejected")
		return
	}
	if k == nil {
		reject(w, 401, "invalid api key")
		return
	}
	info := map[string]any{
		"id": id, "name": name, "is_active": alive, "credit_limit": limit,
	}
	// Verified wire fact: expires_at rides the stats info ONLY when the key
	// has one — never-expiring keys omit the field entirely.
	if expires != "" {
		info["expires_at"] = expires
	}
	wrap(w, 200, map[string]any{
		"info": info,
		"stats": map[string]any{
			"input_cost": used, "output_cost": 0.0, "cache_cost": 0.0,
			"cache_write_cost": 0.0, "input_token": 0, "output_token": 0,
			"cache_token": 0, "cache_write_token": 0,
			"request_success": 0, "request_failed": 0,
		},
	})
}

// handleDelete removes a key by NUMERIC id from the path. A 404 for an unknown
// or already-deleted id mirrors the real API (llmgate.ErrNotFound upstream).
func (fl *fakeLlmgate) handleDelete(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/v1/apikey/delete/")
	fl.mu.Lock()
	fl.Deletes = append(fl.Deletes, idStr)
	status := fl.DeleteStatus
	id, perr := strconv.ParseInt(idStr, 10, 64)
	var k *fakeLlmgateKey
	if perr == nil {
		k = fl.keys[id]
	}
	if status == 0 && k != nil && !k.Gone {
		k.Gone = true
		fl.mu.Unlock()
		wrap(w, 200, nil) // NO data field at all — success per the envelope rule
		return
	}
	fl.mu.Unlock()
	if status != 0 {
		reject(w, status, "delete rejected")
		return
	}
	reject(w, 404, "key not found")
}

// handleList serves GET /api/v1/apikey/list: every non-deleted key as
// METADATA ONLY (api_key_length, never the cleartext — invariant 2) — the
// reconciliation path's lookup half.
func (fl *fakeLlmgate) handleList(w http.ResponseWriter, r *http.Request) {
	fl.mu.Lock()
	fl.Lists++
	out := []map[string]any{}
	for _, k := range fl.keys {
		if k.Gone {
			continue
		}
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "is_active": k.Alive,
			"credit_limit": k.Limit, "api_key_length": len(k.Key),
		})
	}
	fl.mu.Unlock()
	wrap(w, 200, out)
}

// handleReveal serves POST /api/v1/apikey/{id}/reveal: the full record
// INCLUDING the cleartext key — the reconciliation path's recovery half,
// and the only other place (besides create) the cleartext ever appears.
func (fl *fakeLlmgate) handleReveal(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/reveal") || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/apikey/"), "/reveal")
	id, err := strconv.ParseInt(idStr, 10, 64)
	// Snapshot the record under mu; the response is written after unlock.
	var found bool
	var snap fakeLlmgateKey
	fl.mu.Lock()
	fl.Reveals = append(fl.Reveals, idStr)
	if err == nil {
		if k := fl.keys[id]; k != nil && !k.Gone {
			found, snap = true, *k
		}
	}
	fl.mu.Unlock()
	if !found {
		reject(w, 404, "key not found")
		return
	}
	revealed := map[string]any{
		"id": snap.ID, "api_key": snap.Key, "name": snap.Name,
		"is_active": snap.Alive, "credit_limit": snap.Limit,
	}
	if snap.ExpiresAt != "" {
		revealed["expires_at"] = snap.ExpiresAt
	}
	wrap(w, 200, revealed)
}

// register injects a key the bot did not mint in this process (pre-seeded /
// migration-shaped rows).
func (fl *fakeLlmgate) register(id int64, key string, limit, used float64) *fakeLlmgateKey {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	k := &fakeLlmgateKey{ID: id, Name: "tg-100-order-seeded", Key: key, Limit: limit, Used: used, Alive: true}
	fl.keys[id] = k
	if int(id) > fl.seq {
		fl.seq = int(id)
	}
	return k
}

func (fl *fakeLlmgate) createCount() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return len(fl.Creates)
}

func (fl *fakeLlmgate) listCount() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.Lists
}

func (fl *fakeLlmgate) revealCount() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return len(fl.Reveals)
}

func (fl *fakeLlmgate) deleteIDs() []string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return append([]string{}, fl.Deletes...)
}

func (fl *fakeLlmgate) lastCreate() map[string]any {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.Creates[len(fl.Creates)-1]
}

func (fl *fakeLlmgate) lastUpdate() map[string]any {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.Updates[len(fl.Updates)-1]
}

// seedLlmgateKey stores a delivered key order + its llmgate-backend row and
// registers the key on the fake server — the llmgate twin of seedKey.
func seedLlmgateKey(t *testing.T, st *store.Store, fl *fakeLlmgate, des string, chat int64,
	lgID int64, key, saleClass string, bought, limit, used float64) {
	t.Helper()
	seedOrder(t, st, des, chat, apikeyProduct, int64(bought), int64(bought)*1000, "delivered")
	if _, err := st.AddAPIKey(des, chat, strconv.FormatInt(lgID, 10), key, bought, saleClass, store.BackendLlmgate); err != nil {
		t.Fatal(err)
	}
	fl.register(lgID, key, limit, used)
}

// ---- mint ---------------------------------------------------------------------

// TestApikeyLlmgateMintPayloadAndRow is the cutover's headline pin: a bedrock
// sale mints DIRECTLY on llmgate — create carries the deterministic name,
// credit_limit and the 30-DAY expires_at (business rule 2026-10-10: sold
// keys live 30 days; the field must be PRESENT on the wire, an RFC3339
// instant ≈ now+30d, since the never-expire omission is no longer the
// mint shape), the stored row is backend=llmgate with the DECIMAL
// llmgate id, the delivery message carries {apibase} + {pubbase} and the
// cleartext key, and the POOL fake receives NOTHING (no provision, no
// anything) for the sale.
func TestApikeyLlmgateMintPayloadAndRow(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, _ := newAPIKeyBot(t)
	const chat, des = 100, "1a2b00001a2b00001a2b00001a2b0000"
	minted := time.Now()
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("bedrock mint must deliver via llmgate")
	}
	p := fl.lastCreate()
	if name, _ := p["name"].(string); name != "tg-100-order-"+des {
		t.Errorf("create name = %q, want the deterministic mint name", name)
	}
	if p["credit_limit"] != float64(500) {
		t.Errorf("credit_limit = %v, want 500", p["credit_limit"])
	}
	raw, ok := p["expires_at"].(string)
	if !ok || raw == "" {
		t.Fatalf("create expires_at = %v, want the 30-day RFC3339 validity (business rule 2026-10-10)", p["expires_at"])
	}
	exp, perr := time.Parse(time.RFC3339, raw)
	if perr != nil {
		t.Fatalf("create expires_at %q is not RFC3339: %v", raw, perr)
	}
	// ≈ mint-time + 30 days (llmgate.DefaultValidity — a flat window, not a
	// calendar month). Tolerance of 1h absorbs clock jitter; anything
	// wildly off is a wrong rule.
	if d := exp.Sub(minted.Add(llmgate.DefaultValidity)); d < -time.Hour || d > time.Hour {
		t.Errorf("create expires_at = %v, want ≈ %v (minted+30 days)", exp, minted.Add(llmgate.DefaultValidity))
	}
	if p["is_active"] != true {
		t.Errorf("is_active = %v, want true (a key minted disabled cannot serve its buyer)", p["is_active"])
	}
	row, _ := st.APIKeyForOrder(des)
	if row == nil {
		t.Fatal("api_keys row must exist after the llmgate mint")
	}
	if row.Backend != store.BackendLlmgate {
		t.Errorf("stored backend = %q, want %q", row.Backend, store.BackendLlmgate)
	}
	if row.SaleClass != saleClassBedrock {
		t.Errorf("stored sale_class = %q, want bedrock (llmgate has no class echo)", row.SaleClass)
	}
	fl.mu.Lock()
	created := fl.keys[1]
	fl.mu.Unlock()
	if created == nil || strconv.FormatInt(created.ID, 10) != row.KeyID {
		t.Fatalf("stored key_id = %q, want the decimal llmgate id", row.KeyID)
	}
	if row.APIKey != created.Key {
		t.Fatalf("stored key = %q, want the created cleartext %q", row.APIKey, created.Key)
	}
	// The delivered message shows the llmgate wording: the {apibase}
	// completions URL, the {pubbase} web-pages links, the 30-day validity
	// note, and the key in a tap-to-copy code block — and NO legacy endpoint
	// line (removed with the 2026-10-10 rules).
	msg := lastSendMsgTo(f, chat)
	if msg == nil {
		t.Fatal("no delivery message")
	}
	if !strings.Contains(msg.Text, "https://api.example.test") {
		t.Errorf("delivery missing the llmgate {apibase} URL: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "https://shop.example.test") {
		t.Errorf("delivery missing the shop's {pubbase} URL: %q", msg.Text)
	}
	if strings.Contains(msg.Text, fk.srv.URL) {
		t.Errorf("delivery must NOT carry the pool/legacy URL anymore: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "valid for 30 days") {
		t.Errorf("delivery missing the 30-day validity note: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "<code>"+row.APIKey+"</code>") {
		t.Errorf("delivery missing the cleartext key in a code block: %q", msg.Text)
	}
	if got, _ := st.GetOrder(des); got.Status != "delivered" {
		t.Errorf("order = %q, want delivered", got.Status)
	}
	// The pool fake receives NOTHING for a bedrock sale — the cutover's whole
	// point. provisionCount covers provision_customer; the stats/delete slices
	// start empty too.
	if n := fk.provisionCount(); n != 0 {
		t.Errorf("pool provision called %d times, want 0 (bedrock mints on llmgate)", n)
	}
}

// TestApikeyLlmgateMintFailureRefunds: a rejected llmgate create is the ONLY
// real failure — the money goes back to the wallet and the order lands
// 'refunded'.
func TestApikeyLlmgateMintFailureRefunds(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, _ := newAPIKeyBot(t)
	fl.CreateStatus = 400
	const chat, des = 100, "1c2d00001c2d00001c2d00001c2d0000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if b.deliverApikey(ctx, o, des) {
		t.Fatal("a rejected llmgate mint must not report success")
	}
	if got, _ := st.GetOrder(des); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	if row, _ := st.APIKeyForOrder(des); row != nil {
		t.Error("no api_keys row may exist after a failed mint")
	}
	if countContains(f, "could not be created") != 1 {
		t.Error("the buyer must be told the mint failed and the money came back")
	}
	if n := fk.provisionCount(); n != 0 {
		t.Errorf("pool provision called %d times, want 0", n)
	}
}

// TestApikeyLlmgateMintIdempotentReplay: the MINT-ONCE invariant on the new
// backend — a replayed settle re-delivers the STORED llmgate key and create is
// called exactly ONCE for one sale.
func TestApikeyLlmgateMintIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const chat, des = 100, "1e2f00001e2f00001e2f00001e2f0000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("first delivery must succeed")
	}
	row, _ := st.APIKeyForOrder(des)
	if row == nil || row.APIKey == "" {
		t.Fatal("api_keys row must exist after the mint")
	}
	// Replay the settle (crash-between-mint-and-flip / re-driven payment).
	o2, _ := st.GetOrder(des)
	if !b.deliverApikey(ctx, o2, des) {
		t.Fatal("replay must re-deliver")
	}
	if n := fl.createCount(); n != 1 {
		t.Fatalf("llmgate create called %d times, want exactly 1 (double-mint!)", n)
	}
	row2, _ := st.APIKeyForOrder(des)
	if row2.APIKey != row.APIKey {
		t.Errorf("replay delivered a DIFFERENT key: %q vs %q", row2.APIKey, row.APIKey)
	}
	if n := countContains(f, row.APIKey); n != 2 {
		t.Errorf("stored key delivered %d times, want 2 (mint + replay re-send)", n)
	}
}

// ---- sale gate ------------------------------------------------------------------

// TestApikeyBedrockGateOnLlmgate pins the cutover's gate: bedrock sells iff
// the LLMGATE client is configured — the pool's account id (or the whole pool)
// is irrelevant to a mint that never touches it.
func TestApikeyBedrockGateOnLlmgate(t *testing.T) {
	b, _, _, fk, fl, dir := newAPIKeyBot(t)

	// llmgate unconfigured, pool FULLY configured -> hidden.
	s := apikeySettings(fk, fl)
	delete(s, "llmgate_password")
	writeSettings(t, dir, s)
	if b.apikeyForSale(saleClassBedrock) {
		t.Error("bedrock must NOT be for sale when llmgate is unconfigured, even with the pool fully configured")
	}

	// llmgate configured, pool NOT configured at all -> sells.
	s = apikeySettings(fk, fl)
	delete(s, "kiro_go_base_url")
	delete(s, "kiro_go_admin_key")
	delete(s, "kiro_go_account_id")
	writeSettings(t, dir, s)
	if !b.apikeyForSale(saleClassBedrock) {
		t.Error("bedrock must sell on llmgate alone — the pool is irrelevant to its mint")
	}
	// Kiro/fixed still gate on the pool they mint through.
	if b.apikeyForSale(saleClassKiro) || b.apikeyForSale(saleClassFixed) {
		t.Error("kiro/fixed must stay hidden without the pool they mint on")
	}
}

// ---- recharge --------------------------------------------------------------------

// TestApikeyLlmgateRechargeRMW pins the read-modify-write: limit 10 + a
// 50-credit top-up writes the ABSOLUTE new limit 60, the update body carries
// the CURRENT name (omitting it would CLEAR it server-side — verified),
// is_active true, and the RENEWAL expires_at ≈ now+30 days (business rule
// 2026-10-10: recharging an ACTIVE key extends its validity), the receipt
// quotes 60−used rounded, the row's stored credits are bumped and the
// applied stamp is set.
func TestApikeyLlmgateRechargeRMW(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "2a2b00002a2b00002a2b00002a2b0000"
	const rcDes = "2c2d00002c2d00002c2d00002c2d0000"
	// Live key: limit 10, 2.4 used (fractional — the receipt must round), and
	// a live (future) expiry like every key minted under the 30-day rule.
	seedLlmgateKey(t, st, fl, keyDes, chat, 77, "sk-llmgate-"+strings.Repeat("7", 48), "bedrock", 10, 10, 2.4)
	fl.mu.Lock()
	fl.keys[77].Name = "tg-100-order-" + keyDes
	fl.keys[77].ExpiresAt = time.Now().Add(10 * 24 * time.Hour).Format(time.RFC3339)
	fl.mu.Unlock()
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 50); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 50, 500000, "paid")
	charged := time.Now()

	if !b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("llmgate recharge must succeed")
	}
	u := fl.lastUpdate()
	if u["id"] != float64(77) {
		t.Errorf("update id = %v, want 77 (the stored decimal key id)", u["id"])
	}
	if u["name"] != "tg-100-order-"+keyDes {
		t.Errorf("update name = %v, want the CURRENT name from stats (omitting it CLEARS it)", u["name"])
	}
	if u["is_active"] != true {
		t.Errorf("is_active = %v, want true", u["is_active"])
	}
	if u["credit_limit"] != float64(60) {
		t.Errorf("credit_limit = %v, want 60 (10 + 50, ABSOLUTE write)", u["credit_limit"])
	}
	// RENEWAL: the update carries expires_at ≈ now+30 days — the recharge
	// EXTENDS the key's validity (the old 10-days-left expiry is replaced,
	// not kept). Tolerance 1h for clock jitter; update-side omission is
	// unverified, so the field must ALWAYS be explicit here.
	raw, ok := u["expires_at"].(string)
	if !ok || raw == "" {
		t.Fatalf("update expires_at = %v, want the explicit renewal (never omitted on this path)", u["expires_at"])
	}
	renew, rerr := time.Parse(time.RFC3339, raw)
	if rerr != nil {
		t.Fatalf("update expires_at %q is not RFC3339: %v", raw, rerr)
	}
	if d := renew.Sub(charged.Add(llmgate.DefaultValidity)); d < -time.Hour || d > time.Hour {
		t.Errorf("update expires_at = %v, want ≈ %v (recharge renews by another 30 days)", renew, charged.Add(llmgate.DefaultValidity))
	}
	// The fake's stored record moved too (the renewal actually landed).
	fl.mu.Lock()
	stored := fl.keys[77].ExpiresAt
	fl.mu.Unlock()
	if stored != raw {
		t.Errorf("fake's stored expiry = %q, want the renewal %q applied", stored, raw)
	}
	// Receipt: 60 − 2.4 = 57.6 -> rounds to 58 (never floors to 57). The
	// buyer's receipt is followed by the owner sale alert, so read the last
	// message TO THE BUYER, not the global last send.
	receipt := lastSendMsgTo(f, chat)
	if receipt == nil || !strings.Contains(receipt.Text, "58") {
		t.Fatalf("receipt missing the rounded remaining 58: %v", receipt)
	}
	row, _ := st.APIKeyForOrder(keyDes)
	if row.Credits != 60 {
		t.Errorf("stored credits = %v, want 60 (BumpAPIKeyCredits 10 + 50)", row.Credits)
	}
	rec, _ := st.APIKeyRechargeForOrder(rcDes)
	if rec.AppliedTS == nil {
		t.Fatal("applied_ts must be stamped after the llmgate update confirms")
	}
}

// TestApikeyLlmgateRechargeUnlimitedRefunds: an unlimited llmgate key
// (credit_limit 0) cannot take a credit top-up — and a naive 0+credits write
// would CONVERT it to a limited key. The money comes back instead.
func TestApikeyLlmgateRechargeUnlimitedRefunds(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "2e2f00002e2f00002e2f00002e2f0000"
	const rcDes = "30310000303100003031000030310000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 88, "sk-llmgate-"+strings.Repeat("8", 48), "bedrock", 1000, 0, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("an unlimited llmgate key must not take a credit top-up")
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	fl.mu.Lock()
	nUpd := len(fl.Updates)
	fl.mu.Unlock()
	if nUpd != 0 {
		t.Errorf("update called %d times, want 0 (never write a limit onto an unlimited key)", nUpd)
	}
	rec, _ := st.APIKeyRechargeForOrder(rcDes)
	if rec.AppliedTS != nil {
		t.Error("applied_ts must stay NULL for a refused top-up")
	}
}

// TestApikeyLlmgateRCGate pins the gate verdicts for llmgate rows at the same
// layer the pool tests use (apikeyRCGate): unreadable stats FAIL CLOSED, a
// disabled key is refused, and an exhausted key stays rechargeable with the
// correct headroom (cap − remaining − inflight; a negative remaining means
// overspend and still counts honestly).
func TestApikeyLlmgateRCGate(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "32330000323300003233000032330000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 99, "sk-llmgate-"+strings.Repeat("9", 48), "bedrock", 1000, 1000, 0)

	// Readable stats, nothing in flight: headroom = cap(1,000,000) − 1000.
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "" || g.Headroom != 999000 {
		t.Fatalf("gate = %+v, want headroom 999000", g)
	}
	// Stats unreadable -> FAIL CLOSED (never sell a top-up past an
	// unverifiable cap) — the same verdict the pool path gives.
	fl.StatsStatus = 400
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "stats" {
		t.Errorf("unreadable stats gate = %q, want stats (fail closed)", g.Err)
	}
	fl.StatsStatus = 0
	// A MANUALLY disabled key is refused (a recharge would re-enable it);
	// an exhausted one stays rechargeable — that is the primary use case.
	fl.mu.Lock()
	fl.keys[99].Alive = false
	fl.mu.Unlock()
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "disabled" {
		t.Errorf("disabled gate = %q, want disabled", g.Err)
	}
	fl.mu.Lock()
	fl.keys[99].Alive = true
	fl.keys[99].Used = 1000 // exhausted: remaining 0
	fl.mu.Unlock()
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "" || g.Headroom != 1000000 {
		t.Fatalf("exhausted gate = %+v, want rechargeable with full headroom", g)
	}
	// Overspend (remaining negative) is NOT clamped: headroom grows past the
	// cap by exactly the overspend.
	fl.mu.Lock()
	fl.keys[99].Used = 1002.5
	fl.mu.Unlock()
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "" || g.Headroom != 1000002 {
		t.Fatalf("overspent gate = %+v, want headroom 1000002 (cap − (−2.5) rounded down by the int64 cast)", g)
	}
}

// TestApikeyLlmgateRechargeStatsUnreadableRefunds pins the DELIVERY-time layer
// for unreadable llmgate stats (the gate layer is pinned in
// TestApikeyLlmgateRCGate, same split as the pool tests): the disabled
// pre-check fails OPEN, but the read-modify-write MUST have a real limit or
// the update would write a garbage cap — so an unreadable stats read inside
// the lock fails CLOSED into the refund path.
func TestApikeyLlmgateRechargeStatsUnreadableRefunds(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "40410000404100004041000040410000"
	const rcDes = "42430000424300004243000042430000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 123, "sk-llmgate-"+strings.Repeat("1", 48), "bedrock", 1000, 1000, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")
	fl.StatsStatus = 400 // every KeyStats read fails

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("a top-up whose limit cannot be read must not be delivered")
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	fl.mu.Lock()
	nUpd := len(fl.Updates)
	fl.mu.Unlock()
	if nUpd != 0 {
		t.Errorf("update called %d times, want 0 (never write a limit computed from nothing)", nUpd)
	}
	rec, _ := st.APIKeyRechargeForOrder(rcDes)
	if rec.AppliedTS != nil {
		t.Error("applied_ts must stay NULL when the top-up never happened")
	}
}

// TestApikeyLlmgateRechargeDisabledAtDeliveryRefunds: the delivery-time
// disabled re-check routes llmgate rows too — a key the admin disabled during
// the payment window must be refused (a recharge would re-enable it
// server-side), with no update call reaching llmgate.
func TestApikeyLlmgateRechargeDisabledAtDeliveryRefunds(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "44450000444500004445000044450000"
	const rcDes = "46470000464700004647000046470000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 234, "sk-llmgate-"+strings.Repeat("2", 48), "bedrock", 1000, 1000, 0)
	// The admin disabled the key on llmgate while the order sat unpaid.
	fl.mu.Lock()
	fl.keys[234].Alive = false
	fl.mu.Unlock()
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("must not recharge a manually disabled llmgate key")
	}
	fl.mu.Lock()
	nUpd := len(fl.Updates)
	fl.mu.Unlock()
	if nUpd != 0 {
		t.Errorf("update called %d times, want 0 (no re-enable via top-up)", nUpd)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
}

// ---- revoke ----------------------------------------------------------------------

// TestApikeyLlmgateRevoke: a 200 delete is "revoked"; a 404 is "already gone"
// — the desired end state — and both count as ok so a refund is never blocked.
func TestApikeyLlmgateRevoke(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "34350000343500003435000034350000"
	const goneDes = "36370000363700003637000036370000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 111, "sk-llmgate-"+strings.Repeat("a", 48), "bedrock", 1000, 1000, 0)
	seedLlmgateKey(t, st, fl, goneDes, chat, 222, "sk-llmgate-"+strings.Repeat("b", 48), "bedrock", 1000, 1000, 0)

	row, _ := st.APIKeyForOrder(keyDes)
	if ok, detail := b.revokeAPIKey(ctx, row); !ok || detail != "revoked" {
		t.Errorf("revoke = (%v,%q), want (true,\"revoked\")", ok, detail)
	}
	fl.mu.Lock()
	if !fl.keys[111].Gone {
		t.Error("the key must be deleted server-side")
	}
	fl.mu.Unlock()

	gone, _ := st.APIKeyForOrder(goneDes)
	fl.DeleteStatus = 404
	if ok, detail := b.revokeAPIKey(ctx, gone); !ok || detail != "already gone" {
		t.Errorf("404 revoke = (%v,%q), want (true,\"already gone\")", ok, detail)
	}
}

// TestApikeyLlmgateRefundOverspentKeyProRatesToNothing: an overspent llmgate
// key reports a NEGATIVE remaining (CreditLimit − Used, unclamped upstream).
// The refund adapter must clamp it to 0 — passed through raw it would ride the
// unlimited (< 0) branch of the shared refund maths and pay a FULL refund for
// a fully-used key. THE money guard of the cutover's stats union.
func TestApikeyLlmgateRefundOverspentKeyProRatesToNothing(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "48490000484900004849000048490000"
	// 1000 bought for 1,000,000, 1002.5 spent live -> remaining -2.5.
	seedLlmgateKey(t, st, fl, keyDes, chat, 345, "sk-llmgate-"+strings.Repeat("3", 48), "bedrock", 1000, 1000, 1002.5)

	o, _ := st.GetOrder(keyDes)
	amount, bought, used := b.apikeyRefundAmount(ctx, o)
	if bought != 1000 || used != 1000 {
		t.Fatalf("bought/used = %d/%d, want 1000/1000 (overspend counts as fully used)", bought, used)
	}
	if amount != 0 {
		t.Fatalf("overspent key must pro-rate to 0, got %d (a negative remaining leaked into the unlimited branch!)", amount)
	}
}

// ---- My-Keys / reveal --------------------------------------------------------------

// TestApikeyLlmgateMyKeysAndReveal: an llmgate row renders live stats from the
// per-key KeyStats read (∞ when unlimited), carries the bedrock SKU tag and a
// reveal button, and the reveal re-sends the DeliveredLlmgate wording (both
// base URLs) — the same template the first delivery used.
func TestApikeyLlmgateMyKeysAndReveal(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const limDes = "38390000383900003839000038390000"
	const infDes = "3a3b00003a3b00003a3b00003a3b0000"
	seedLlmgateKey(t, st, fl, limDes, chat, 333, "sk-llmgate-"+strings.Repeat("c", 48), "bedrock", 1000, 1000, 250)
	seedLlmgateKey(t, st, fl, infDes, chat, 444, "sk-llmgate-"+strings.Repeat("d", 48), "bedrock", 1000, 0, 0)

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	msg := lastMsg(t, f)
	if !strings.Contains(msg.Text, "Used 250/1000") {
		t.Errorf("llmgate live stats line missing from %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "∞ left") {
		t.Errorf("an unlimited llmgate key must render ∞, got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "🔷 AI Credits") {
		t.Errorf("llmgate rows carry the bedrock SKU tag, got %q", msg.Text)
	}
	if _, ok := findCB(inlineRows(msg), "apikeyrev:"+limDes); !ok {
		t.Error("a live llmgate key must get a reveal button")
	}

	// Reveal: the DeliveredLlmgate wording — {apibase} + the {pubbase} links
	// and the 30-day validity note — never the pool-only single-URL template.
	f.Sends = nil
	b.Process(ctx, callbackUpdate(chat, "apikeyrev:"+limDes))
	reveal := lastSendMsgTo(f, chat)
	if reveal == nil {
		t.Fatal("no reveal message")
	}
	if !strings.Contains(reveal.Text, "https://api.example.test") ||
		!strings.Contains(reveal.Text, "https://shop.example.test") {
		t.Errorf("reveal must use the llmgate template with both hosts, got %q", reveal.Text)
	}
	if strings.Contains(reveal.Text, "Base URL (recommended)") {
		t.Errorf("reveal still shows the removed legacy-endpoint block: %q", reveal.Text)
	}
}

// TestApikeyLlmgateMyKeysExpiredLabel: DEAD keys (EXPIRED past the 30-day
// window, and EXHAUSTED balance-spent) are OMITTED from My-Keys entirely
// (operator 2026-10-10: dead keys on this screen are noise). Seeded expired +
// exhausted + LIVE rows in one page: only the live one renders, with exactly
// its one reveal button.
func TestApikeyLlmgateMyKeysExpiredLabel(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const deadDes = "3f4100003f4100003f4100003f410000"
	const liveDes = "3f4200003f4200003f4200003f420000"
	seedLlmgateKey(t, st, fl, deadDes, chat, 888, "sk-llmgate-"+strings.Repeat("a", 48), "bedrock", 1000, 1000, 250)
	seedLlmgateKey(t, st, fl, liveDes, chat, 889, "sk-llmgate-"+strings.Repeat("c", 48), "bedrock", 1000, 1000, 100)
	fl.mu.Lock()
	fl.keys[888].ExpiresAt = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	fl.mu.Unlock()

	// …and an EXHAUSTED third key hides too (operator: dead is dead).
	seedLlmgateKey(t, st, fl, "3f4300003f4300003f4300003f430000", chat, 890,
		"sk-llmgate-"+strings.Repeat("e", 48), "bedrock", 10, 10, 10)

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	msg := lastMsg(t, f)
	if strings.Contains(msg.Text, maskAPIKey("sk-llmgate-"+strings.Repeat("a", 48))) {
		t.Errorf("expired key must be hidden, got %q", msg.Text)
	}
	if strings.Contains(msg.Text, maskAPIKey("sk-llmgate-"+strings.Repeat("e", 48))) {
		t.Errorf("exhausted key must be hidden, got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, maskAPIKey("sk-llmgate-"+strings.Repeat("c", 48))) {
		t.Errorf("live key must still render, got %q", msg.Text)
	}
	if mk, ok := msg.ReplyMarkup.(*tg.InlineKeyboardMarkup); !ok || len(mk.InlineKeyboard) != 1 {
		t.Errorf("reveal buttons: want exactly the live key's one row")
	}
}

// TestApikeyLlmgateRechargeExpiredRefusedAndRefunded: business rule
// 2026-10-10 rule 3, both layers — the ORDER-time gate refuses an expired
// key ("expired", the RCExpired notice), and the DELIVERY-time pre-check
// (for an order that slipped through while the key was still alive)
// auto-refunds WITHOUT any update call reaching llmgate: the renewal write
// is reserved for ACTIVE keys, never a resurrection.
func TestApikeyLlmgateRechargeExpiredRefusedAndRefunded(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "41430000414300004143000041430000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 999, "sk-llmgate-"+strings.Repeat("b", 48), "bedrock", 1000, 1000, 100)
	fl.mu.Lock()
	fl.keys[999].ExpiresAt = time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	fl.mu.Unlock()

	// ORDER-TIME: the gate refuses with "expired" (before the unlimited
	// check, after disabled — an expired unlimited key would be refused
	// here identically).
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "expired" {
		t.Fatalf("gate = %+v, want Err expired", g)
	}

	// DELIVERY-TIME: a paid recharge order on the now-expired key must be
	// refunded, with NO update call (no renewal of a dead key).
	const rcDes = "43450000434500004345000043450000"
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("must not recharge an expired key")
	}
	fl.mu.Lock()
	nUpd := len(fl.Updates)
	fl.mu.Unlock()
	if nUpd != 0 {
		t.Errorf("update called %d times, want 0 (never renew an expired key)", nUpd)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	if got, _ := st.GetOrder(rcDes); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
	rec, _ := st.APIKeyRechargeForOrder(rcDes)
	if rec.AppliedTS != nil {
		t.Error("applied_ts must stay NULL for a refused top-up")
	}
}

// TestApikeyLlmgateRechargeDisabledAtLockRefusedAndRefunded pins the
// in-lock DISABLED re-check: the pre-check passed while the key was alive,
// but the fresh read inside the lock sees the admin's disable — the update's
// is_active:true pin must never resurrect that key, so the order refunds
// with zero update calls.
func TestApikeyLlmgateRechargeDisabledAtLockRefusedAndRefunded(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "41460000414600004146000041460000"
	seedLlmgateKey(t, st, fl, keyDes, chat, 777, "sk-llmgate-"+strings.Repeat("d", 48), "bedrock", 1000, 1000, 50)

	const rcDes = "43460000434600004346000043460000"
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	// Disable AFTER order creation but BEFORE delivery — the pre-check at
	// delivery time uses the same stats read in this test harness, so flip it
	// here and let both the pre-check and the in-lock read see the disable.
	fl.mu.Lock()
	fl.keys[777].Alive = false
	fl.mu.Unlock()

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("must not recharge a disabled key")
	}
	fl.mu.Lock()
	nUpd := len(fl.Updates)
	fl.mu.Unlock()
	if nUpd != 0 {
		t.Errorf("update called %d times, want 0 (never re-enable a disabled key)", nUpd)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	if got, _ := st.GetOrder(rcDes); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
}

// TestApikeyLlmgateMyKeysDegradesSilently: when llmgate's stats are
// unreadable, the view degrades to the local row plus the stats-unavailable
// note — it never blocks and never fabricates numbers.
func TestApikeyLlmgateMyKeysDegradesSilently(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "3c3d00003c3d00003c3d00003c3d0000"
	seedLlmgateKey(t, st, fl, des, chat, 555, "sk-llmgate-"+strings.Repeat("e", 48), "bedrock", 1000, 1000, 0)
	fl.StatsStatus = 400

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	msg := lastMsg(t, f)
	if !strings.Contains(msg.Text, "live stats unavailable") {
		t.Errorf("a failed llmgate stats read must append the stats-unavailable note, got %q", msg.Text)
	}
	if strings.Contains(msg.Text, "Used ") {
		t.Errorf("no live numbers may be fabricated from a failed read: %q", msg.Text)
	}
	if _, ok := findCB(inlineRows(msg), "apikeyrev:"+des); !ok {
		t.Error("the reveal button must still be offered (the stored key is reachable without stats)")
	}
}

// TestApikeyLlmgateMyKeysNoPoolNote: an llmgate-only page with NO pool
// configured must not append the pool-shaped "stats unavailable" note just
// because kirogoClient() is nil — the note belongs to keys whose stats
// actually failed, not to a gateway the page never consults.
func TestApikeyLlmgateMyKeysNoPoolNote(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, dir := newAPIKeyBot(t)
	s := apikeySettings(fk, fl)
	delete(s, "kiro_go_base_url")
	delete(s, "kiro_go_admin_key")
	writeSettings(t, dir, s)
	const chat = 100
	const des = "3e3f00003e3f00003e3f00003e3f0000"
	seedLlmgateKey(t, st, fl, des, chat, 666, "sk-llmgate-"+strings.Repeat("f", 48), "bedrock", 1000, 1000, 100)

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	msg := lastMsg(t, f)
	if strings.Contains(msg.Text, "live stats unavailable") {
		t.Errorf("an llmgate-only page must not carry the pool's unavailable note, got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "Used 100/1000") {
		t.Errorf("the llmgate stats must still load, got %q", msg.Text)
	}
}

// ---- migrated-row refund: the pool twin must die with the llmgate key --------

// seedMigratedKey builds the exact row shape cmd/migrate-llmgate leaves
// behind: a pool-minted row committed through the REAL
// store.MigrateAPIKeyToLlmgate (pool originals preserved into
// pool_key_id/pool_api_key, key_id/api_key repointed at a registered
// llmgate key, backend=llmgate), with both keys live on their fakes.
func seedMigratedKey(t *testing.T, st *store.Store, fk *fakeKiro, fl *fakeLlmgate,
	des string, chat int64, poolID, poolKey string, bought, remaining float64) {
	t.Helper()
	seedOrder(t, st, des, chat, apikeyProduct, int64(bought), int64(bought)*1000, "delivered")
	if _, err := st.AddAPIKey(des, chat, poolID, poolKey, bought, "bedrock", store.BackendPool); err != nil {
		t.Fatal(err)
	}
	fk.register(poolID, poolKey, int64(bought), int64(bought-remaining))
	fl.mu.Lock()
	fl.seq++
	lgID := int64(fl.seq)
	fl.mu.Unlock()
	lgKey := fmt.Sprintf("sk-llmgate-%048d", lgID)
	if moved, err := st.MigrateAPIKeyToLlmgate(des, poolID, poolKey, strconv.FormatInt(lgID, 10), lgKey, remaining); err != nil || !moved {
		t.Fatalf("seed migration: moved=%v err=%v", moved, err)
	}
	fl.register(lgID, lgKey, remaining, 0)
	fl.mu.Lock()
	fl.keys[lgID].Name = "tg-" + strconv.FormatInt(chat, 10) + "-order-" + des
	fl.mu.Unlock()
}

// TestApikeyLlmgateRefundMigratedRevokesBothKeys is FIX 1's headline pin: a
// refund of a MIGRATED row must delete BOTH spendable halves — the llmgate
// key AND the preserved pool twin. The migration intentionally left the old
// pool key live; both carry the same remaining, and the refund pays that
// remaining back ONCE. A revoke that killed only the llmgate half would
// leave the customer's old pool key live and spendable after the payout —
// double value on a fully automated path.
func TestApikeyLlmgateRefundMigratedRevokesBothKeys(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "50510000505100005051000050510000"
	// 1000 bought for 1,000,000; migration carried 800 to the llmgate key
	// (pool twin still holds the same 800 remaining).
	seedMigratedKey(t, st, fk, fl, des, chat, "poolkey-mig", "sk-test-mig-0123456789abcdef", 1000, 800)

	o, _ := st.GetOrder(des)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil {
		t.Fatal("the group refund must proceed for a migrated row")
	}
	if got, _ := st.GetOrder(des); got.Status != "refunded" {
		t.Fatalf("order = %q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 800000 {
		t.Fatalf("wallet = %d, want the pro-rated 800000 (800 of 1000 remaining)", bal)
	}
	// BOTH fakes received a delete: the llmgate key by numeric id...
	fl.mu.Lock()
	lgGone := fl.keys[1] != nil && fl.keys[1].Gone
	fl.mu.Unlock()
	if !lgGone {
		t.Error("the llmgate key must be deleted server-side by the refund")
	}
	// ...and the preserved pool twin by its original uuid.
	fk.mu.Lock()
	twin := fk.keys["poolkey-mig"]
	twinDeleted := twin != nil && twin.Deleted
	nPoolDeletes := len(fk.Delete)
	fk.mu.Unlock()
	if twin == nil {
		t.Fatal("pool twin never registered on the pool fake")
	}
	if !twinDeleted || nPoolDeletes != 1 {
		t.Errorf("pool twin deleted=%v pool deletes=%d, want true/1 — a live twin after payout is double value", twinDeleted, nPoolDeletes)
	}
	// The combined outcome also travels in the revoke detail: with the
	// llmgate key already dead and the pool fake now forced to 404, a
	// re-revoke reads "already gone" on BOTH halves.
	row, _ := st.APIKeyForOrder(des)
	fk.DeleteStatus = 404
	if ok, detail := b.revokeAPIKey(ctx, row); !ok || detail != "already gone (llmgate + pool)" {
		t.Errorf("re-revoke = (%v,%q), want (true,\"already gone (llmgate + pool)\")", ok, detail)
	}
}

// TestApikeyLlmgateRevokeMigratedTwinFailureStillRefundsAndAlerts: when the
// pool-twin delete FAILS, the llmgate revoke still counts as ok and the
// refund proceeds — the payout must never block on a best-effort cleanup —
// while the owners are paged for the manual twin deletion (masked key only,
// never the cleartext).
func TestApikeyLlmgateRevokeMigratedTwinFailureStillRefundsAndAlerts(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "51520000515200005152000051520000"
	const poolKey = "sk-test-migtwin-0123456789abcdef"
	seedMigratedKey(t, st, fk, fl, des, chat, "poolkey-migtwin", poolKey, 1000, 800)
	fk.DeleteStatus = 400 // twin delete fails (4xx: instant, never retried)

	o, _ := st.GetOrder(des)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil {
		t.Fatal("the refund must proceed even when the twin revoke fails")
	}
	if got, _ := st.GetOrder(des); got.Status != "refunded" {
		t.Fatalf("order = %q, want refunded — the payout must not block on the twin", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 800000 {
		t.Fatalf("wallet = %d, want the full pro-rated 800000", bal)
	}
	// The llmgate half IS dead; the twin half is not — and that fact is
	// paged to the owners with BOTH ids, masked key only.
	fl.mu.Lock()
	lgGone := fl.keys[1] != nil && fl.keys[1].Gone
	fl.mu.Unlock()
	if !lgGone {
		t.Error("the llmgate key itself must be deleted even though the twin revoke failed")
	}
	if n := countContains(f, "POOL TWIN NOT REVOKED"); n != 1 {
		t.Fatalf("owner alert count = %d, want exactly 1 (twin needs manual cleanup)", n)
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "poolkey-migtwin") {
		t.Fatalf("twin alert must name the pool twin id, got %v", alert)
	}
	if strings.Contains(alert.Text, poolKey) {
		t.Error("twin alert leaked the CLEARTEXT pool key — masked form only")
	}
	if !strings.Contains(alert.Text, maskAPIKey(poolKey)) {
		t.Errorf("twin alert should carry the masked pool key for identification, got %q", alert.Text)
	}
}

// TestApikeyLlmgateRevokeMigratedTwinAlreadyGone: a 404 on the pool twin is
// the desired end state ("already gone") — the revoke stays ok, no alert.
func TestApikeyLlmgateRevokeMigratedTwinAlreadyGone(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, fl, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "52530000525300005253000052530000"
	seedMigratedKey(t, st, fk, fl, des, chat, "poolkey-twin404", "sk-test-twin404-0123456789ab", 1000, 800)
	fk.DeleteStatus = 404 // the twin was already deleted by hand

	row, _ := st.APIKeyForOrder(des)
	ok, detail := b.revokeAPIKey(ctx, row)
	if !ok || detail != "revoked llmgate; pool twin already gone" {
		t.Fatalf("revoke = (%v,%q), want (true,\"revoked llmgate; pool twin already gone\")", ok, detail)
	}
	if n := countContains(f, "POOL TWIN"); n != 0 {
		t.Errorf("a 404 twin needs no owner alert, got %d", n)
	}
}

// ---- ambiguous-create reconciliation (FIX 2b) ---------------------------------

// TestApikeyLlmgateMintAmbiguousRecoveredViaListReveal: the fake PROCESSES
// the create but answers 500 (committed-but-unanswered). The client must
// give up after exactly ONE create attempt, and the handler must recover
// the mint via list+reveal on the deterministic name — the order DELIVERS,
// the row persists the revealed cleartext and decimal id, and NO refund
// happens. This closes the orphan window without ever minting twice.
func TestApikeyLlmgateMintAmbiguousRecoveredViaListReveal(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	fl.CreateAmbiguous = true // create commits server-side, answers 500
	const chat, des = 100, "53540000535400005354000053540000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("an ambiguous create whose key exists must DELIVER via reconciliation, not refund")
	}
	if n := fl.createCount(); n != 1 {
		t.Fatalf("llmgate create attempts = %d, want exactly 1 (a create retry could mint a second paid key)", n)
	}
	if n := fl.listCount(); n != 1 {
		t.Errorf("list calls = %d, want 1 (the single reconciliation pass)", n)
	}
	if n := fl.revealCount(); n != 1 {
		t.Errorf("reveal calls = %d, want 1 (the recovered key's cleartext)", n)
	}
	row, _ := st.APIKeyForOrder(des)
	if row == nil {
		t.Fatal("the recovered mint must be persisted like any other")
	}
	fl.mu.Lock()
	minted := fl.keys[1]
	fl.mu.Unlock()
	if minted == nil || row.KeyID != "1" || row.APIKey != minted.Key {
		t.Fatalf("stored row = %+v, want the REVEALED key (id 1, the committed mint's cleartext)", row)
	}
	if got, _ := st.GetOrder(des); got.Status != "delivered" {
		t.Errorf("order = %q, want delivered (no refund on a recovered mint)", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("balance = %d, want 0 — the sale completed against the recovered key", bal)
	}
}

// TestApikeyLlmgateMint4xxSkipsReconciliation: a 400 is a DEFINITIVE
// rejection — nothing was minted, so no list+reveal round trip may happen;
// the sale fails straight to the refund.
func TestApikeyLlmgateMint4xxSkipsReconciliation(t *testing.T) {
	ctx := context.Background()
	b, _, st, _, fl, _ := newAPIKeyBot(t)
	fl.CreateStatus = 400
	const chat, des = 100, "54550000545500005455000054550000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if b.deliverApikey(ctx, o, des) {
		t.Fatal("a rejected create must not report success")
	}
	if n := fl.createCount(); n != 1 {
		t.Errorf("create attempts = %d, want 1", n)
	}
	if n := fl.listCount(); n != 0 {
		t.Errorf("list calls = %d, want 0 — a 4xx is definitive, reconciliation is wasted work", n)
	}
	if n := fl.revealCount(); n != 0 {
		t.Errorf("reveal calls = %d, want 0", n)
	}
	if got, _ := st.GetOrder(des); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
}

// ---- backend-aware revoke-failed operator text (FIX 4a) -----------------------

// TestApikeyRefundRevokeFailedAlertNamesBackend: the manual-cleanup
// instruction must name the console the key actually lives on — "on
// llmgate" for llmgate-backend rows, "on Kiro-Go" for pool rows. A wrong
// portal in that alert strands spendable credits while the operator hunts
// the wrong console.
func TestApikeyRefundRevokeFailedAlertNamesBackend(t *testing.T) {
	ctx := context.Background()
	const chat = 100

	// llmgate row: revoke fails on llmgate -> "DELETE this key on llmgate".
	b, f, st, _, fl, _ := newAPIKeyBot(t)
	const lgDes = "55560000555600005556000055560000"
	seedLlmgateKey(t, st, fl, lgDes, chat, 777, "sk-llmgate-"+strings.Repeat("7", 48), "bedrock", 1000, 1000, 0)
	fl.DeleteStatus = 400
	o, _ := st.GetOrder(lgDes)
	if res := b.doRefundApikeyGroup(ctx, o, "balance", false); res == nil {
		t.Fatal("llmgate refund must proceed despite the revoke failure")
	}
	alert := lastSendMsgTo(f, 999)
	if alert == nil || !strings.Contains(alert.Text, "DELETE this key on llmgate now") {
		t.Fatalf("llmgate alert = %v, want the llmgate console named", alert)
	}
	if strings.Contains(alert.Text, "on Kiro-Go now") {
		t.Errorf("llmgate alert must not send the operator to Kiro-Go: %q", alert.Text)
	}

	// pool row: revoke fails on the pool -> "DELETE this key on Kiro-Go".
	b2, f2, st2, fk2, _, _ := newAPIKeyBot(t)
	const poolDes = "56570000565700005657000056570000"
	seedKey(t, st2, fk2, poolDes, chat, "poolkey-where", "sk-test-where-0123456789abcd", "bedrock", 1000, 1000, 0)
	fk2.DeleteStatus = 400
	o2, _ := st2.GetOrder(poolDes)
	if res := b2.doRefundApikeyGroup(ctx, o2, "balance", false); res == nil {
		t.Fatal("pool refund must proceed despite the revoke failure")
	}
	alert2 := lastSendMsgTo(f2, 999)
	if alert2 == nil || !strings.Contains(alert2.Text, "DELETE this key on Kiro-Go now") {
		t.Fatalf("pool alert = %v, want the Kiro-Go console named", alert2)
	}
}
