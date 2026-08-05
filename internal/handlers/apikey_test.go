package handlers

// apikey_test.go — the M2.5 acceptance pins, ported from the Python
// tests/test_apikey.py handler-facing cases. Every test drives the REAL
// handlers through the fake-Telegram harness plus an httptest Kiro-Go double;
// no live Telegram or Kiro-Go call is ever made.
//
// The money invariants under test (repo CLAUDE.md):
//   - mint-once: the api_keys row is written BEFORE the paid->delivered flip, so
//     a replayed settle re-delivers the STORED key and never double-mints;
//   - upstreamStatus "pending" (bedrock) / "skipped" (kiro) IS SUCCESS;
//   - a failed mint/recharge refunds to the wallet, never drops the money;
//   - recharge sends credits ONLY (never tokens/tokenLimit) and stamps
//     applied_ts only AFTER Kiro-Go confirms;
//   - a legacy ('' sale_class) key's recharge OMITS saleClass entirely;
//   - the product HIDES when Kiro-Go is not fully configured;
//   - a refunded key is never re-served (reveal / list / recharge picker).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codezdev-bot/internal/config"
	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/stock"
	"codezdev-bot/internal/store"
	"codezdev-bot/internal/tg"
)

// ---- fake Kiro-Go -----------------------------------------------------------

// fakeKeyState is one pool key as the fake server sees it. Remaining is derived
// (limit − used) unless Unlimited pins it to the -1 wire sentinel.
type fakeKeyState struct {
	ID, Name, Key, Status string
	CreditLimit, Used     int64
	Unlimited             bool
	Deleted               bool
}

// fakeKiro is an httptest stand-in for the Kiro-Go admin API. It records every
// request body verbatim (so the payload pins — no tokens, no accountId for
// kiro, omitted saleClass for legacy — can assert on the exact JSON that went
// out) and lets a test force a status code per endpoint.
type fakeKiro struct {
	mu  sync.Mutex
	srv *httptest.Server

	Provision []map[string]any
	Recharge  []map[string]any
	Delete    []map[string]any

	keys map[string]*fakeKeyState // by pool key id
	seq  int

	// Forced non-2xx status per endpoint (0 = respond normally). 4xx only:
	// internal/kirogo never retries a 4xx, so failure tests stay instant.
	ProvisionStatus int
	RechargeStatus  int
	DeleteStatus    int
	StatsStatus     int

	// UpstreamStatus / EchoSaleClass shape the mint response. EchoSaleClass
	// false models a PRE-saleClass server: it echoes no class, so the bot must
	// store '' and leave the recharge guard disarmed.
	UpstreamStatus string
	EchoSaleClass  bool

	// StatsSaleClass is echoed on every /admin/stats row — the class the SERVER
	// recorded, which is what a legacy ('' stored) row's display falls back to.
	StatsSaleClass string

	// OnRecharge fires INSIDE the recharge handler, i.e. after Kiro-Go has
	// committed the top-up but before the bot's post-call re-read. It is how a
	// test injects the concurrent whole-key refund the TOCTOU guard exists for.
	OnRecharge func()
}

func newFakeKiro(t *testing.T) *fakeKiro {
	t.Helper()
	fk := &fakeKiro{
		keys:           map[string]*fakeKeyState{},
		UpstreamStatus: "bound",
		EchoSaleClass:  true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/provision_customer", fk.handleProvision)
	mux.HandleFunc("/admin/recharge_api_key", fk.handleRecharge)
	mux.HandleFunc("/admin/delete_api_key", fk.handleDelete)
	mux.HandleFunc("/admin/stats", fk.handleStats)
	fk.srv = httptest.NewServer(mux)
	t.Cleanup(fk.srv.Close)
	return fk
}

func (fk *fakeKiro) decode(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	return body
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

func (fk *fakeKiro) handleProvision(w http.ResponseWriter, r *http.Request) {
	body := fk.decode(r)
	fk.mu.Lock()
	fk.Provision = append(fk.Provision, body)
	status := fk.ProvisionStatus
	if status != 0 {
		fk.mu.Unlock()
		fail(w, status, "provision rejected")
		return
	}
	fk.seq++
	id := "poolkey-" + itoa(fk.seq)
	key := "sk-test-" + id + "-0123456789abcdef"
	credits := int64(0)
	if v, ok := body["credits"].(float64); ok {
		credits = int64(v)
	}
	name, _ := body["name"].(string)
	fk.keys[id] = &fakeKeyState{ID: id, Name: name, Key: key,
		Status: "active", CreditLimit: credits}
	resp := map[string]any{
		"success": true, "id": id, "key": key, "name": name,
		"credits": credits, "upstreamKeyId": 4242, "upstreamBound": fk.UpstreamStatus == "bound",
		"upstreamStatus": fk.UpstreamStatus,
	}
	if fk.EchoSaleClass {
		if sc, ok := body["saleClass"].(string); ok && sc != "" {
			resp["saleClass"] = sc
		}
	}
	fk.mu.Unlock()
	_ = json.NewEncoder(w).Encode(resp)
}

func (fk *fakeKiro) handleRecharge(w http.ResponseWriter, r *http.Request) {
	body := fk.decode(r)
	fk.mu.Lock()
	fk.Recharge = append(fk.Recharge, body)
	if fk.RechargeStatus != 0 {
		status := fk.RechargeStatus
		fk.mu.Unlock()
		fail(w, status, "recharge rejected")
		return
	}
	st := fk.lookupLocked(body)
	if st == nil {
		fk.mu.Unlock()
		fail(w, 404, "API key not found")
		return
	}
	if v, ok := body["credits"].(float64); ok {
		st.CreditLimit += int64(v)
	}
	st.Status = "active"
	resp := map[string]any{"success": true, "id": st.ID, "enabled": true,
		"creditLimit": st.CreditLimit, "creditsUsed": st.Used,
		"creditsRemaining": st.CreditLimit - st.Used,
		"tokenLimit":       0, "tokensUsed": 0, "tokensRemaining": -1}
	hook := fk.OnRecharge
	fk.mu.Unlock()
	if hook != nil {
		hook() // the top-up is committed server-side from here on
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (fk *fakeKiro) handleDelete(w http.ResponseWriter, r *http.Request) {
	body := fk.decode(r)
	fk.mu.Lock()
	fk.Delete = append(fk.Delete, body)
	if fk.DeleteStatus != 0 {
		status := fk.DeleteStatus
		fk.mu.Unlock()
		fail(w, status, "API key not found")
		return
	}
	st := fk.lookupLocked(body)
	if st != nil {
		st.Deleted = true
	}
	id := ""
	if st != nil {
		id = st.ID
	}
	fk.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "id": id, "upstreamRevoked": true})
}

func (fk *fakeKiro) handleStats(w http.ResponseWriter, r *http.Request) {
	body := fk.decode(r)
	fk.mu.Lock()
	if fk.StatsStatus != 0 {
		status := fk.StatsStatus
		fk.mu.Unlock()
		fail(w, status, "stats rejected")
		return
	}
	var out []map[string]any
	for _, st := range fk.keys {
		if st.Deleted {
			continue
		}
		if id, ok := body["id"].(string); ok && id != "" && id != st.ID {
			continue
		}
		if k, ok := body["apiKey"].(string); ok && k != "" && k != st.Key {
			continue
		}
		remaining := st.CreditLimit - st.Used
		if st.Unlimited {
			remaining = -1
		}
		row := map[string]any{
			"id": st.ID, "name": st.Name, "keyMasked": maskAPIKey(st.Key),
			"enabled": st.Status == "active", "status": st.Status,
			"creditLimit": st.CreditLimit, "creditsUsed": st.Used,
			"creditsRemaining": remaining, "tokenLimit": 0,
			"tokensUsed": 0, "tokensRemaining": -1,
		}
		if fk.StatsSaleClass != "" {
			row["saleClass"] = fk.StatsSaleClass
		}
		out = append(out, row)
	}
	fk.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
}

// lookupLocked resolves the key a request addresses (by id, else by cleartext).
// Caller holds fk.mu.
func (fk *fakeKiro) lookupLocked(body map[string]any) *fakeKeyState {
	if id, ok := body["id"].(string); ok && id != "" {
		return fk.keys[id]
	}
	if k, ok := body["apiKey"].(string); ok && k != "" {
		for _, st := range fk.keys {
			if st.Key == k {
				return st
			}
		}
	}
	return nil
}

// register injects a key the bot did not mint in this process (used to build
// legacy / pre-seeded rows).
func (fk *fakeKiro) register(id, key string, limit, used int64) *fakeKeyState {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	st := &fakeKeyState{ID: id, Key: key, Status: "active", CreditLimit: limit, Used: used}
	fk.keys[id] = st
	return st
}

func (fk *fakeKiro) provisionCount() int {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return len(fk.Provision)
}

func (fk *fakeKiro) rechargeCount() int {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return len(fk.Recharge)
}

func (fk *fakeKiro) lastProvision() map[string]any {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return fk.Provision[len(fk.Provision)-1]
}

func (fk *fakeKiro) lastRecharge() map[string]any {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return fk.Recharge[len(fk.Recharge)-1]
}

// ---- harness ----------------------------------------------------------------

// writeSettings replaces settings.json under dir. Config re-reads on mtime
// change, so a mid-test rewrite takes effect on the very next gate check.
func writeSettings(t *testing.T, dir string, s map[string]any) {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// apikeySettings is the baseline fully-configured shop: bedrock priced at
// 4 USDT / 100 credits against the fake Kiro-Go.
func apikeySettings(fk *fakeKiro) map[string]any {
	return map[string]any{
		"kiro_go_base_url":        fk.srv.URL,
		"kiro_go_admin_key":       "admin-secret",
		"kiro_go_account_id":      "acct-1",
		"apikey_usdt_per_100":     4.0,
		"apikey_max_per_customer": 0,
	}
}

// newAPIKeyBot wires a Bot to a temp store/stock/config plus the fake Kiro-Go,
// with the baseline settings already written.
func newAPIKeyBot(t *testing.T) (*Bot, *fakeTG, *store.Store, *fakeKiro, string) {
	t.Helper()
	dir := t.TempDir()
	fk := newFakeKiro(t)
	writeSettings(t, dir, apikeySettings(fk))
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	stk := stock.New(filepath.Join(dir, "accounts"), filepath.Join(dir, "orders"))
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	f := &fakeTG{}
	b := New(Deps{TG: f, Store: st, Stock: stk, Config: cfg, AdminID: "999"})
	t.Cleanup(func() { b.Shutdown(); st.Close() })
	return b, f, st, fk, dir
}

// seedPaidOrder creates an order already in the given status.
func seedOrder(t *testing.T, st *store.Store, des string, chat int64, product string, qty, total int64, status string) *store.Order {
	t.Helper()
	if err := st.CreateOrder(des, fmtInt(chat), "en", product, "API", qty, total); err != nil {
		t.Fatal(err)
	}
	if status != "reserved" {
		if _, err := st.ClaimOrder(des, "reserved", status); err != nil {
			t.Fatal(err)
		}
	}
	o, err := st.GetOrder(des)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func lastMsg(t *testing.T, f *fakeTG) tg.SendMessageParams {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sends) == 0 {
		t.Fatal("no messages sent")
	}
	return f.Sends[len(f.Sends)-1]
}

// ---- mint --------------------------------------------------------------------

// TestApikeyMintIdempotent is THE invariant: the api_keys row is persisted
// before the paid->delivered flip, so a replayed settle re-delivers the STORED
// key and provision_customer is called exactly ONCE for one sale.
func TestApikeyMintIdempotent(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat, des = 100, "aaaa0000aaaa0000aaaa0000aaaa0000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("first delivery must succeed")
	}
	row, _ := st.APIKeyForOrder(des)
	if row == nil || row.APIKey == "" {
		t.Fatal("api_keys row must exist after the mint")
	}
	if got, _ := st.GetOrder(des); got.Status != "delivered" {
		t.Errorf("order status = %q, want delivered", got.Status)
	}

	// Replay the settle (crash-between-mint-and-flip / re-driven payment).
	o2, _ := st.GetOrder(des)
	if !b.deliverApikey(ctx, o2, des) {
		t.Fatal("replay must re-deliver")
	}
	if n := fk.provisionCount(); n != 1 {
		t.Fatalf("provision_customer called %d times, want exactly 1 (double-mint!)", n)
	}
	row2, _ := st.APIKeyForOrder(des)
	if row2.APIKey != row.APIKey {
		t.Errorf("replay delivered a DIFFERENT key: %q vs %q", row2.APIKey, row.APIKey)
	}
	if n := countContains(f, row.APIKey); n != 2 {
		t.Errorf("stored key delivered %d times, want 2 (mint + replay re-send)", n)
	}
}

// TestApikeyMintBedrockPayload pins the bedrock wire payload: explicit
// accountId, tokenLimit 0, saleClass "bedrock", skipUpstream false.
func TestApikeyMintBedrockPayload(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat, des = 100, "bbbb0000bbbb0000bbbb0000bbbb0000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")
	b.deliverApikey(ctx, o, des)

	p := fk.lastProvision()
	if p["accountId"] != "acct-1" {
		t.Errorf("accountId = %v, want acct-1 (must always be explicit)", p["accountId"])
	}
	if p["saleClass"] != "bedrock" {
		t.Errorf("saleClass = %v, want bedrock", p["saleClass"])
	}
	if p["tokenLimit"] != float64(0) {
		t.Errorf("tokenLimit = %v, want 0", p["tokenLimit"])
	}
	if p["skipUpstream"] != false {
		t.Errorf("skipUpstream = %v, want false for bedrock", p["skipUpstream"])
	}
	if p["credits"] != float64(500) {
		t.Errorf("credits = %v, want 500", p["credits"])
	}
	if name, _ := p["name"].(string); name != "tg-100-order-"+des {
		t.Errorf("name = %q, want the deterministic mint name", name)
	}
}

// TestApikeyMintKiroPayload pins the kiro wire payload: NO accountId field at
// all, skipUpstream true, saleClass "kiro".
func TestApikeyMintKiroPayload(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["kiro_apikey_enabled"] = true
	s["kiro_apikey_usdt_per_100"] = 1.0
	writeSettings(t, dir, s)

	const chat, des = 100, "cccc0000cccc0000cccc0000cccc0000"
	fk.UpstreamStatus = "skipped" // the NORMAL terminal status for a kiro mint
	o := seedOrder(t, st, des, chat, kiroApikeyProduct, 2000, 500000, "paid")
	if !b.deliverApikey(ctx, o, des) {
		t.Fatal("kiro mint must deliver")
	}
	p := fk.lastProvision()
	if _, present := p["accountId"]; present {
		t.Errorf("kiro mint must send NO accountId, got %v", p["accountId"])
	}
	if p["skipUpstream"] != true {
		t.Errorf("skipUpstream = %v, want true for kiro", p["skipUpstream"])
	}
	if p["saleClass"] != "kiro" {
		t.Errorf("saleClass = %v, want kiro", p["saleClass"])
	}
	row, _ := st.APIKeyForOrder(des)
	if row == nil || row.SaleClass != "kiro" {
		t.Fatalf("stored sale_class = %v, want kiro", row)
	}
}

// TestApikeyMintPendingIsSuccess: upstreamStatus "pending" (bedrock) and
// "skipped" (kiro) are BOTH terminal successes — the key is delivered, the
// order closes, and nothing is retried or refunded.
func TestApikeyMintPendingIsSuccess(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, product, upstream string
	}{
		{"bedrock/pending", apikeyProduct, "pending"},
		{"kiro/skipped", kiroApikeyProduct, "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, st, fk, dir := newAPIKeyBot(t)
			s := apikeySettings(fk)
			s["kiro_apikey_enabled"] = true
			s["kiro_apikey_usdt_per_100"] = 1.0
			writeSettings(t, dir, s)
			fk.UpstreamStatus = tc.upstream
			const chat, des = 100, "dddd0000dddd0000dddd0000dddd0000"
			o := seedOrder(t, st, des, chat, tc.product, 1000, 500000, "paid")

			if !b.deliverApikey(ctx, o, des) {
				t.Fatal("a 2xx mint is ALWAYS a completed sale, whatever upstreamStatus says")
			}
			if got, _ := st.GetOrder(des); got.Status != "delivered" {
				t.Errorf("order = %q, want delivered (never refunded on a 2xx)", got.Status)
			}
			if n := fk.provisionCount(); n != 1 {
				t.Errorf("provision called %d times, want 1 (never retry a 2xx)", n)
			}
			if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
				t.Errorf("balance = %d, want 0 (no refund on a successful mint)", bal)
			}
			row, _ := st.APIKeyForOrder(des)
			if row == nil || countContains(f, row.APIKey) != 1 {
				t.Error("the key must be delivered exactly once")
			}
		})
	}
}

// TestApikeyMintFailureRefunds: a non-2xx mint is the ONLY real failure — the
// money goes back to the wallet and the order lands 'refunded'.
func TestApikeyMintFailureRefunds(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	fk.ProvisionStatus = 400
	const chat, des = 100, "eeee0000eeee0000eeee0000eeee0000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")

	if b.deliverApikey(ctx, o, des) {
		t.Fatal("a rejected mint must not report success")
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
	// A 4xx is a real rejection: never retried (retrying can't succeed).
	if n := fk.provisionCount(); n != 1 {
		t.Errorf("provision called %d times, want 1 (never retry a 4xx)", n)
	}
}

// TestApikeyMintLegacyEchoStoresEmptyClass: a PRE-saleClass Kiro-Go echoes no
// class, so the bot must store '' — storing the order's "bedrock" would 409 the
// key's first-ever recharge.
func TestApikeyMintLegacyEchoStoresEmptyClass(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	fk.EchoSaleClass = false
	const chat, des = 100, "ffff0000ffff0000ffff0000ffff0000"
	o := seedOrder(t, st, des, chat, apikeyProduct, 500, 500000, "paid")
	b.deliverApikey(ctx, o, des)

	row, _ := st.APIKeyForOrder(des)
	if row == nil || row.SaleClass != "" {
		t.Fatalf("stored sale_class = %q, want '' (the server's absent echo)", row.SaleClass)
	}
}

// ---- recharge ----------------------------------------------------------------

// seedKey stores a delivered key order + its api_keys row and registers the key
// on the fake server.
func seedKey(t *testing.T, st *store.Store, fk *fakeKiro, des string, chat int64,
	keyID, key, saleClass string, bought, limit, used int64) {
	t.Helper()
	seedOrder(t, st, des, chat, apikeyProduct, bought, bought*1000, "delivered")
	if _, err := st.AddAPIKey(des, chat, keyID, key, float64(bought), saleClass); err != nil {
		t.Fatal(err)
	}
	fk.register(keyID, key, limit, used)
}

// TestApikeyRechargeNeverSendsTokens is test-pinned in the Python suite: a
// `tokens` (or tokenLimit) field on a recharge 400s the whole top-up and used to
// SET the token cap to N, instantly disabling any key already past N.
func TestApikeyRechargeNeverSendsTokens(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "11110000111100001111000011110000"
	const rcDes = "22220000222200002222000022220000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-x", "sk-test-poolkey-x-0123456789ab", "bedrock", 1000, 1000, 200)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if !b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("recharge must succeed")
	}
	p := fk.lastRecharge()
	if _, present := p["tokens"]; present {
		t.Error("recharge sent a `tokens` field — this 400s the top-up / disables the key")
	}
	if _, present := p["tokenLimit"]; present {
		t.Error("recharge sent a `tokenLimit` field — never allowed on a top-up")
	}
	if p["credits"] != float64(500) {
		t.Errorf("credits = %v, want 500", p["credits"])
	}
	if p["saleClass"] != "bedrock" {
		t.Errorf("saleClass guard = %v, want bedrock", p["saleClass"])
	}
	if p["id"] != "poolkey-x" {
		t.Errorf("id = %v, want the stored key id", p["id"])
	}
}

// TestApikeyRechargeLegacyOmitsSaleClass: a legacy ('' sale_class) row must send
// NO saleClass at all — the server's own record wins, and an explicit ""
// could be rejected as an unrecognized class.
func TestApikeyRechargeLegacyOmitsSaleClass(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "33330000333300003333000033330000"
	const rcDes = "44440000444400004444000044440000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-legacy", "sk-test-legacy-0123456789abcd", "", 1000, 1000, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")
	b.deliverApikeyRecharge(ctx, o, rcDes)

	if _, present := fk.lastRecharge()["saleClass"]; present {
		t.Error("a legacy key's recharge must OMIT saleClass entirely")
	}
}

// TestApikeyRechargeAppliedAfterConfirm: applied_ts is stamped only AFTER
// Kiro-Go confirms, and a replayed settle re-sends the receipt without topping
// the key up a second time.
func TestApikeyRechargeAppliedAfterConfirm(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "55550000555500005555000055550000"
	const rcDes = "66660000666600006666000066660000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-r", "sk-test-poolkey-r-0123456789ab", "bedrock", 1000, 1000, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	// 1) Kiro-Go rejects: applied_ts stays NULL and the money comes back.
	fk.RechargeStatus = 400
	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("a rejected recharge must not report success")
	}
	rec, _ := st.APIKeyRechargeForOrder(rcDes)
	if rec.AppliedTS != nil {
		t.Fatal("applied_ts must NOT be stamped when Kiro-Go rejected the top-up")
	}
	if got, _ := st.GetOrder(rcDes); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	if countContains(f, "recharge failed") != 1 {
		t.Error("the buyer must be told the recharge failed and the money came back")
	}

	// 2) A fresh order that succeeds stamps applied_ts and bumps stored credits.
	const rcDes2 = "77770000777700007777000077770000"
	fk.RechargeStatus = 0
	if _, err := st.AddAPIKeyRecharge(rcDes2, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o2 := seedOrder(t, st, rcDes2, chat, apikeyRechargeProduct, 500, 500000, "paid")
	if !b.deliverApikeyRecharge(ctx, o2, rcDes2) {
		t.Fatal("recharge must succeed")
	}
	rec2, _ := st.APIKeyRechargeForOrder(rcDes2)
	if rec2.AppliedTS == nil {
		t.Fatal("applied_ts must be stamped after Kiro-Go confirms")
	}
	if row, _ := st.APIKeyForOrder(keyDes); row.Credits != 1500 {
		t.Errorf("stored credits = %v, want 1500 (1000 + 500 top-up)", row.Credits)
	}

	// 3) Replay: the stamp is the recharge-once guard.
	o3, _ := st.GetOrder(rcDes2)
	b.deliverApikeyRecharge(ctx, o3, rcDes2)
	if n := fk.rechargeCount(); n != 2 {
		t.Errorf("recharge_api_key called %d times, want 2 (the rejected one + one success)", n)
	}
	if row, _ := st.APIKeyForOrder(keyDes); row.Credits != 1500 {
		t.Errorf("replay bumped stored credits to %v — must stay 1500", row.Credits)
	}
}

// TestApikeyRechargeRefundedTargetRefunds: a key refunded (and revoked) while
// the top-up sat unpaid can't be recharged — the money goes back instead.
func TestApikeyRechargeRefundedTargetRefunds(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "88880000888800008888000088880000"
	const rcDes = "99990000999900009999000099990000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-gone", "sk-test-gone-0123456789abcdef", "bedrock", 1000, 1000, 0)
	if err := st.MarkAPIKeyRefunded(keyDes); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("must not recharge a refunded key")
	}
	if fk.rechargeCount() != 0 {
		t.Error("no recharge call may reach Kiro-Go for a refunded key")
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
}

// ---- gates -------------------------------------------------------------------

// TestApikeyHiddenWhenUnconfigured: the product HIDES rather than selling and
// auto-refunding (repo CLAUDE.md). A bedrock sale needs the account id too.
func TestApikeyHiddenWhenUnconfigured(t *testing.T) {
	b, f, _, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	delete(s, "kiro_go_account_id") // bedrock gate needs URL + key + ACCOUNT ID
	writeSettings(t, dir, s)

	if b.apikeyForSale(saleClassBedrock) {
		t.Error("bedrock must NOT be for sale without an account id")
	}
	rows, any := b.apikeyBuyRows(100, i18n.EN)
	if any || len(rows) != 0 {
		t.Errorf("buy menu must show no API-key row when unconfigured, got %v", rows)
	}
	// A stale/forged tap is refused with APIKEY_OFF, not a sale.
	b.Process(context.Background(), callbackUpdate(100, "apikey:start"))
	if countContains(f, "not for sale") != 1 {
		t.Errorf("stale apikey:start must answer APIKEY_OFF, got %q", lastMsg(t, f).Text)
	}
}

// TestApikeyRateUnsetHidesProduct: a zero/unset rate disables the SKU even with
// Kiro-Go fully configured.
func TestApikeyRateUnsetHidesProduct(t *testing.T) {
	b, _, _, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	delete(s, "apikey_usdt_per_100")
	writeSettings(t, dir, s)
	if b.apikeyForSale(saleClassBedrock) {
		t.Error("a shop with no rate must not sell keys")
	}
	// Legacy per-1000 pricing migrates WITHOUT a price change: 40/1k == 4/100.
	s["apikey_usdt_per_1k"] = 40.0
	writeSettings(t, dir, s)
	if got := b.apikeyRateUSDT(saleClassBedrock); got != 4.0 {
		t.Errorf("legacy per-1k rate migrated to %v per 100, want 4", got)
	}
	if got := b.apikeyPriceVND(100, saleClassBedrock); got != 100000 {
		t.Errorf("100 credits = %d VND, want 100000 (4 USDT @ 25000)", got)
	}
}

// ---- Opus-5 token-value estimate (Kiro commit 3069329) ---------------------

// TestApikeyOpusTokenEstimate pins apikeyOpusEst's pure arithmetic and its
// failure paths — port of test_opus_token_estimate. Two numbers a buyer shops
// on (tokens per 1 USDT, USDT per 1M tokens) both derive from the price of
// 100 credits; they must move linearly with that price and vanish — never
// print a bogus zero — when the price or the config USD_TO_VND rate is
// unusable. The rate comes from usdtStrCfg (b.cfg.USDToVND()), the SAME
// config-layered source the "Price:" line uses, NOT the raw ambient env —
// see TestApikeyOpusEstimateUsesConfigRateNotAmbientEnv for the regression
// this matters for.
func TestApikeyOpusTokenEstimate(t *testing.T) {
	b, _, _, _, _ := newAPIKeyBot(t)
	t.Setenv("USD_TO_VND", "25000") // 1 USDT = 25,000 VND

	// 4.00 USDT / 100 credits, 80M tokens per 100 credits ->
	// 80M / 4 = 20.0M tokens per USDT, 4 / 80 = 0.05 USDT per 1M.
	if got := b.apikeyOpusEst(100000, i18n.EN); !strings.Contains(got, "20.0M") || !strings.Contains(got, "0.05 USDT per 1M") {
		t.Errorf("apikeyOpusEst(4.00 USDT/100) = %q, want 20.0M tokens and 0.05 USDT per 1M", got)
	}
	// Halving the price must double the tokens per USDT (linear, no drift).
	if got := b.apikeyOpusEst(50000, i18n.EN); !strings.Contains(got, "40.0M") {
		t.Errorf("half the price -> twice the tokens per USDT, got %q", got)
	}
	// A sub-cent per-1M price (0.70 USDT/100 credits -> 0.00875 USDT/1M) keeps
	// significant digits instead of rounding away to "0.01".
	if got := b.apikeyOpusEst(17500, i18n.EN); !strings.Contains(got, "0.00875") {
		t.Errorf("sub-cent per-1M price rounded away: %q", got)
	}
	// Every language renders and none of them crash.
	for _, lang := range i18n.LANGS {
		if got := b.apikeyOpusEst(100000, lang); !strings.Contains(got, "20.0M") {
			t.Errorf("estimate did not render for %v: %q", lang, got)
		}
	}
	// Unknown language falls back to English rather than raising/blanking.
	if got, want := b.apikeyOpusEst(100000, i18n.Lang("xx")), b.apikeyOpusEst(100000, i18n.EN); got != want {
		t.Errorf("unknown lang = %q, want English fallback %q", got, want)
	}
	// Unusable inputs -> EMPTY block (the screen simply drops the lines).
	if got := b.apikeyOpusEst(0, i18n.EN); got != "" {
		t.Errorf("no price -> no estimate, got %q", got)
	}
	if got := b.apikeyOpusEst(-5, i18n.EN); got != "" {
		t.Errorf("negative price -> no estimate, got %q", got)
	}
	// An absurd (fat-fingered) rate — 5000 USDT/100 credits — would floor the
	// M-rendering to a misleading "0.0M tokens"; the block must vanish instead.
	if got := b.apikeyOpusEst(125000000, i18n.EN); got != "" {
		t.Errorf("absurd rate -> no estimate, not '0.0M', got %q", got)
	}
	// Unsetting the AMBIENT env must NOT blank the estimate: usdtStrCfg reads
	// through b.cfg.USDToVND(), which falls back to the compiled default
	// ("25000") when neither settings.json nor the env set a rate — exactly
	// like usdtDisp/the Price line above it, and UNLIKE the pre-fix code
	// (usdtStr -> raw os.LookupEnv), which would have gone blank here. No
	// USD_TO_VND at all is a normal boot state (config.DefaultUSDToVND
	// exists precisely for it), not an unusable-rate state.
	os.Unsetenv("USD_TO_VND")
	if got := b.apikeyOpusEst(100000, i18n.EN); !strings.Contains(got, "20.0M") {
		t.Errorf("unset ambient USD_TO_VND should still render via the compiled default, got %q", got)
	}
}

// TestApikeyOpusEstimateFollowsResellerRate: a reseller's estimate must use
// their DISCOUNTED rate — port of test_opus_estimate_follows_reseller_rate.
// The estimate sits directly under the "Price: X / 100 credits" line, which
// is reseller-specific; if the two were computed from different rates the one
// screen would contradict itself.
func TestApikeyOpusEstimateFollowsResellerRate(t *testing.T) {
	b, _, st, fk, dir := newAPIKeyBot(t)
	t.Setenv("USD_TO_VND", "25000")
	const chat = 200

	full := b.apikeyOpusEstFor(chat, i18n.EN) // no @handle -> no reseller -> advertised 4.00 USDT rate
	if !strings.Contains(full, "20.0M") {
		t.Errorf("non-reseller sees the advertised-rate estimate, got %q", full)
	}

	if err := st.SetHandle(fmtInt(chat), "@myreseller"); err != nil {
		t.Fatal(err)
	}
	s := apikeySettings(fk)
	s["resellers"] = []any{map[string]any{"u": "myreseller", "note": "", "disc": 50}} // 4.00 -> 2.00 USDT/100
	writeSettings(t, dir, s)
	discounted := b.apikeyOpusEstFor(chat, i18n.EN)
	if !strings.Contains(discounted, "40.0M") {
		t.Errorf("50%%-off reseller sees twice the tokens per USDT, got %q", discounted)
	}

	// The kiro SKU never gets this block (its credit is a ~5.7x smaller unit);
	// KiroPrompt carries no {opus} placeholder for the shared i18n.F call to fill.
	if strings.Contains(i18n.C().APIKey.KiroPrompt.T(i18n.EN), "{opus}") {
		t.Error("KiroPrompt must not carry an {opus} placeholder")
	}
}

// TestApikeyPromptShowsOpusEstimate: cbApikeyStart renders the Opus-5 estimate
// on the bedrock amount prompt and never on the kiro one (a ~5.7x smaller
// credit unit, for which the same token count would be a giveaway).
func TestApikeyPromptShowsOpusEstimate(t *testing.T) {
	ctx := context.Background()
	b, f, _, fk, dir := newAPIKeyBot(t)
	t.Setenv("USD_TO_VND", "25000")
	s := apikeySettings(fk)
	s["kiro_apikey_enabled"] = true
	s["kiro_apikey_usdt_per_100"] = 1.0
	writeSettings(t, dir, s)
	const chat = 300

	b.Process(ctx, callbackUpdate(chat, "apikey:start"))
	if got := lastMsg(t, f).Text; !strings.Contains(got, "Opus 5 estimate") || !strings.Contains(got, "20.0M") {
		t.Errorf("bedrock prompt missing the opus estimate: %q", got)
	}

	b.Process(ctx, callbackUpdate(chat, "kiroapi:start"))
	if got := lastMsg(t, f).Text; strings.Contains(got, "Opus 5 estimate") || strings.Contains(got, "{opus}") {
		t.Errorf("kiro prompt must not carry the opus estimate or a stray placeholder: %q", got)
	}
}

// TestApikeyOpusEstimateUsesConfigRateNotAmbientEnv is a regression pin for a
// review-caught HIGH bug: apikeyOpusEst originally read USD_TO_VND through
// usdtStr (paypal.VndToUSD -> raw os.LookupEnv("USD_TO_VND")), while the
// "Price: X / 100 credits" line directly above it on the SAME screen reads
// the rate through usdtDisp/apikeyPriceVND (b.cfg.USDToVND() ->
// settings.json's admin-set "usd_to_vnd" overriding .env, else default).
// t.Setenv-only tests can't catch that divergence — they set the ONE value
// both paths would see. This test deliberately makes the two disagree: an
// admin-set settings.json rate (26000, as adm:setrate would save) against a
// stale ambient/.env rate (25000) that no longer matches. On the buggy code
// this reproduces the exact self-contradiction the review flagged — Price:
// 4.00 USDT, estimate silently computed off 4.16 USDT ("≈19.2M tokens per 1
// USDT"); on the fix both lines are computed off the SAME 26000 rate
// ("20.0M").
func TestApikeyOpusEstimateUsesConfigRateNotAmbientEnv(t *testing.T) {
	b, _, _, fk, dir := newAPIKeyBot(t)
	t.Setenv("USD_TO_VND", "25000") // stale ambient/.env rate
	s := apikeySettings(fk)
	s["usd_to_vnd"] = 26000 // admin-set rate (adm:setrate), must win over ambient env
	writeSettings(t, dir, s)

	// apikey_usdt_per_100=4.0 (apikeySettings baseline) priced through the
	// CONFIG rate is exactly what both apikeyPriceVND and the Price: line use.
	vndPer100 := b.apikeyPriceVND(100, saleClassBedrock)
	if vndPer100 != 104000 {
		t.Fatalf("precondition: apikeyPriceVND = %d, want 104000 (4.00 USDT @ 26000)", vndPer100)
	}

	got := b.apikeyOpusEst(vndPer100, i18n.EN)
	if strings.Contains(got, "19.2M") {
		t.Errorf("apikeyOpusEst used the stale ambient USD_TO_VND (25000) instead of the config rate (26000): %q", got)
	}
	if !strings.Contains(got, "20.0M") {
		t.Errorf("apikeyOpusEst = %q, want 20.0M tokens (same 26000 config rate as the Price line)", got)
	}
}

// TestApikeyKiroSKUNeedsToggle: the kiro SKU stays hidden until its master
// toggle flips, even when priced — enabling against a pre-saleClass server would
// mint UNCLASSED keys billed at the bedrock rate (~5.7x giveaway).
func TestApikeyKiroSKUNeedsToggle(t *testing.T) {
	b, _, _, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["kiro_apikey_usdt_per_100"] = 1.0
	writeSettings(t, dir, s)
	if b.apikeyForSale(saleClassKiro) {
		t.Error("kiro must stay hidden while its toggle is OFF")
	}
	s["kiro_apikey_enabled"] = true
	writeSettings(t, dir, s)
	if !b.apikeyForSale(saleClassKiro) {
		t.Error("kiro must sell once priced + toggled on")
	}
	// The kiro gate needs only URL + admin key (its mints send no accountId).
	delete(s, "kiro_go_account_id")
	writeSettings(t, dir, s)
	if !b.apikeyForSale(saleClassKiro) {
		t.Error("kiro must not be gated on the account id its mints never send")
	}
}

// TestApikeyPerCustomerCap: a customer at the cap can't even open the amount
// prompt, and a forged amount can't create an order either.
func TestApikeyPerCustomerCap(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["apikey_max_per_customer"] = 1
	writeSettings(t, dir, s)
	const chat = 100
	seedKey(t, st, fk, "cap00000cap00000cap00000cap00000", chat, "poolkey-cap", "sk-test-cap-0123456789abcdef", "bedrock", 1000, 1000, 0)

	b.Process(ctx, callbackUpdate(chat, "apikey:start"))
	if countContains(f, "limit of 1 API key") != 1 {
		t.Errorf("capped customer must see APIKEY_CUST_CAP, got %q", lastMsg(t, f).Text)
	}
	// Order creation re-checks the cap (a stale prompt can't slip past it).
	b.startApikeyPurchase(ctx, chat, i18n.EN, 500, saleClassBedrock)
	if countContains(f, "limit of 1 API key") != 2 {
		t.Error("startApikeyPurchase must re-check the per-customer cap")
	}
	if o, _ := st.ReservedOrderForChat(fmtInt(chat)); o != nil {
		t.Error("a capped customer must not get a reserved order")
	}
}

// TestApikeyRCGateHeadroomSubtractsInflight: the per-key headroom subtracts
// credits locked in RESERVED/PAID-but-unapplied recharge orders, so two
// overlapping checkouts can't jointly breach the cap (live creditsRemaining
// can't see them yet).
func TestApikeyRCGateHeadroomSubtractsInflight(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["apikey_max_credits"] = 1000
	writeSettings(t, dir, s)
	const chat = 100
	const keyDes = "aa110000aa110000aa110000aa110000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-h", "sk-test-poolkey-h-0123456789ab", "bedrock", 1000, 1000, 200)

	// remaining 800, cap 1000, nothing in flight -> headroom 200.
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "" || g.Headroom != 200 {
		t.Fatalf("gate = %+v, want headroom 200", g)
	}
	// A reserved 100-credit top-up is IN FLIGHT -> headroom drops to 100.
	const rcDes = "bb110000bb110000bb110000bb110000"
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 100); err != nil {
		t.Fatal(err)
	}
	seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 100, 100000, "reserved")
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "" || g.Headroom != 100 {
		t.Fatalf("gate = %+v, want headroom 100 after the in-flight subtraction", g)
	}
	// Ordering more than the headroom is refused.
	b.startApikeyRecharge(ctx, chat, i18n.EN, keyDes, 200)
	rows, _ := st.OrdersForChat(fmtInt(chat), 50, []string{"reserved"}, nil)
	if len(rows) != 1 {
		t.Errorf("an over-headroom top-up must not create an order, got %d reserved", len(rows))
	}
}

// TestApikeyRCGateRefusals pins the fail-closed / permanent-refusal verdicts.
func TestApikeyRCGateRefusals(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "cc110000cc110000cc110000cc110000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-g", "sk-test-poolkey-g-0123456789ab", "bedrock", 1000, 1000, 0)

	// Not this chat's key -> gone.
	if g := b.apikeyRCGate(ctx, 200, keyDes); g.Err != "gone" {
		t.Errorf("foreign key gate = %q, want gone", g.Err)
	}
	// Live stats unreadable -> FAIL CLOSED (never sell a top-up past an
	// unverifiable cap).
	fk.StatsStatus = 400
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "stats" {
		t.Errorf("unreadable stats gate = %q, want stats (fail closed)", g.Err)
	}
	fk.StatsStatus = 0
	// A MANUALLY disabled key is refused (a recharge would re-enable it), while
	// an exhausted one stays rechargeable — that is the primary use case.
	fk.mu.Lock()
	fk.keys["poolkey-g"].Status = "disabled"
	fk.mu.Unlock()
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "disabled" {
		t.Errorf("disabled gate = %q, want disabled", g.Err)
	}
	fk.mu.Lock()
	fk.keys["poolkey-g"].Status = "exhausted"
	fk.keys["poolkey-g"].Used = 1000
	fk.mu.Unlock()
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "" {
		t.Errorf("exhausted gate = %q, want rechargeable", g.Err)
	}
	// -1 remaining (unlimited) is rejected by Kiro-Go with a 400, so refuse here.
	fk.mu.Lock()
	fk.keys["poolkey-g"].Status = "active"
	fk.keys["poolkey-g"].Unlimited = true
	fk.mu.Unlock()
	if g := b.apikeyRCGate(ctx, chat, keyDes); g.Err != "unlimited" {
		t.Errorf("unlimited gate = %q, want unlimited", g.Err)
	}
}

// ---- my keys / reveal --------------------------------------------------------

// TestApikeyMyKeysAndReveal: the list renders live stats (with -1 rendering as
// ∞) and a refunded key is never re-served — no reveal button, and a forged
// apikeyrev: tap is a dead button.
func TestApikeyMyKeysAndReveal(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const liveDes = "dd110000dd110000dd110000dd110000"
	const refDes = "ee110000ee110000ee110000ee110000"
	seedKey(t, st, fk, liveDes, chat, "poolkey-live", "sk-test-live-0123456789abcdef", "bedrock", 1000, 1000, 250)
	seedKey(t, st, fk, refDes, chat, "poolkey-ref", "sk-test-ref-0123456789abcdefg", "bedrock", 500, 500, 0)
	if err := st.MarkAPIKeyRefunded(refDes); err != nil {
		t.Fatal(err)
	}

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	msg := lastMsg(t, f)
	if !strings.Contains(msg.Text, "Used 250/1000") {
		t.Errorf("live stats line missing from %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "refunded") {
		t.Error("a refunded key must still be listed, marked refunded")
	}
	rows := inlineRows(msg)
	if _, ok := findCB(rows, "apikeyrev:"+refDes); ok {
		t.Error("a refunded key must NOT get a reveal button")
	}
	if _, ok := findCB(rows, "apikeyrev:"+liveDes); !ok {
		t.Error("a live key must get a reveal button")
	}

	// Reveal serves the live key's cleartext...
	b.Process(ctx, callbackUpdate(chat, "apikeyrev:"+liveDes))
	if countContains(f, "sk-test-live-0123456789abcdef") != 1 {
		t.Error("reveal must re-send the buyer's own cleartext key")
	}
	// ...but never a refunded one, nor another chat's.
	before := f.sendCount()
	b.Process(ctx, callbackUpdate(chat, "apikeyrev:"+refDes))
	b.Process(ctx, callbackUpdate(200, "apikeyrev:"+liveDes))
	if f.sendCount() != before {
		t.Error("refunded / foreign reveals must be dead buttons")
	}
}

// TestApikeyMyKeysUnlimitedRender: creditsRemaining -1 means UNLIMITED, not
// "minus one left".
func TestApikeyMyKeysUnlimitedRender(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "ff110000ff110000ff110000ff110000"
	seedKey(t, st, fk, des, chat, "poolkey-inf", "sk-test-inf-0123456789abcdef", "bedrock", 1000, 0, 0)
	fk.mu.Lock()
	fk.keys["poolkey-inf"].Unlimited = true
	fk.mu.Unlock()

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	if !strings.Contains(lastMsg(t, f).Text, "∞ left") {
		t.Errorf("-1 remaining must render as ∞, got %q", lastMsg(t, f).Text)
	}
}

// TestApikeyRechargePickerSkipsRefunded: the picker lists LIVE keys only; with
// none left it says so instead of offering a revoked key.
func TestApikeyRechargePickerSkipsRefunded(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "11220000112200001122000011220000"
	seedKey(t, st, fk, des, chat, "poolkey-p", "sk-test-poolkey-p-0123456789ab", "bedrock", 1000, 1000, 0)
	if err := st.MarkAPIKeyRefunded(des); err != nil {
		t.Fatal(err)
	}
	b.Process(ctx, callbackUpdate(chat, "apikeyrc:start"))
	if countContains(f, "no API key to recharge") != 1 {
		t.Errorf("a refunded-only customer must see APIKEY_RC_NONE, got %q", lastMsg(t, f).Text)
	}
}

// ---- group refund ------------------------------------------------------------

// TestApikeyGroupRefundProration: the key + its APPLIED recharges refund as ONE
// unit, pro-rated by credits REMAINING, and the key is revoked once.
func TestApikeyGroupRefundProration(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "22330000223300002233000022330000"
	const rcDes = "33440000334400003344000033440000"
	// 1000 credits bought for 1,000,000 VND...
	seedOrder(t, st, keyDes, chat, apikeyProduct, 1000, 1000000, "delivered")
	if _, err := st.AddAPIKey(keyDes, chat, "poolkey-grp", "sk-test-grp-0123456789abcdef", 1000, "bedrock"); err != nil {
		t.Fatal(err)
	}
	// ...plus an APPLIED 500-credit top-up for 500,000 VND.
	seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "delivered")
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAPIKeyRechargeApplied(rcDes); err != nil {
		t.Fatal(err)
	}
	// Live: 1500 limit, 900 used -> 600 remaining of 1500 bought.
	fk.register("poolkey-grp", "sk-test-grp-0123456789abcdef", 1500, 900)

	o, _ := st.GetOrder(keyDes)
	amount, bought, used := b.apikeyRefundAmount(ctx, o)
	if bought != 1500 || used != 900 {
		t.Fatalf("group credits = %d bought / %d used, want 1500/900", bought, used)
	}
	if want := int64(600000); amount != want { // 1,500,000 * 600/1500
		t.Fatalf("pro-rated amount = %d, want %d", amount, want)
	}

	// Confirm through the customer path: one payout, both orders refunded, key
	// revoked, group note shown.
	b.Process(ctx, callbackUpdate(chat, "apikeyrefok:"+keyDes))
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 600000 {
		t.Errorf("wallet = %d, want 600000", bal)
	}
	for _, d := range []string{keyDes, rcDes} {
		if got, _ := st.GetOrder(d); got.Status != "refunded" {
			t.Errorf("order %s = %q, want refunded (the whole key refunds together)", d, got.Status)
		}
	}
	if row, _ := st.APIKeyForOrder(keyDes); row.RefundedTS == nil {
		t.Error("the api_keys row must be stamped refunded")
	}
	fk.mu.Lock()
	nDel := len(fk.Delete)
	fk.mu.Unlock()
	if nDel != 1 {
		t.Errorf("delete_api_key called %d times, want exactly 1", nDel)
	}
	if countContains(f, "recharge(s)") != 1 {
		t.Error("the receipt must state the whole-key/group semantics")
	}

	// A second tap is a no-op: every member claim is already lost.
	balBefore, _ := st.GetBalance(fmtInt(chat))
	b.Process(ctx, callbackUpdate(chat, "apikeyrefok:"+keyDes))
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != balBefore {
		t.Errorf("double-tap paid out twice: %d -> %d", balBefore, bal)
	}
}

// TestDoRefundAdminForceFullApikeyGroup proves the shared admin executor,
// doRefund, correctly routes an api-key-family order into the group refund
// path even under force-full (adm:refund's escalation, and frefundm:'s
// executor) — paying the FULL group total rather than the pro-rated amount,
// and that refundGroupTotal (the admin preview's "whole-key group" line)
// agrees with what actually gets paid.
func TestDoRefundAdminForceFullApikeyGroup(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "aabb0000aabb0000aabb0000aabb0000"
	const rcDes = "ccdd0000ccdd0000ccdd0000ccdd0000"
	seedOrder(t, st, keyDes, chat, apikeyProduct, 1000, 1000000, "delivered")
	if _, err := st.AddAPIKey(keyDes, chat, "poolkey-adm", "sk-test-adm-0123456789abcdef", 1000, "bedrock"); err != nil {
		t.Fatal(err)
	}
	seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "delivered")
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAPIKeyRechargeApplied(rcDes); err != nil {
		t.Fatal(err)
	}
	fk.register("poolkey-adm", "sk-test-adm-0123456789abcdef", 1500, 900)

	o, _ := st.GetOrder(keyDes)
	groupN, paid := b.refundGroupTotal(o)
	if groupN != 2 || paid != 1500000 {
		t.Fatalf("refundGroupTotal = (%d,%d), want (2,1500000)", groupN, paid)
	}

	res := b.doRefund(ctx, o, "balance", true)
	if res == nil {
		t.Fatal("doRefund(forceFull) on an apikey-family order returned nil")
	}
	if res.Amount != 1500000 {
		t.Errorf("force-full amount = %d, want 1500000 (full group paid total)", res.Amount)
	}
	if res.Unit != "Credit" {
		t.Errorf("unit = %q, want Credit", res.Unit)
	}
	for _, d := range []string{keyDes, rcDes} {
		if got, _ := st.GetOrder(d); got.Status != "refunded" {
			t.Errorf("order %s = %q, want refunded", d, got.Status)
		}
	}
	fk.mu.Lock()
	nDel := len(fk.Delete)
	fk.mu.Unlock()
	if nDel != 1 {
		t.Errorf("delete_api_key called %d times, want exactly 1 (force-full still revokes once)", nDel)
	}
}

// TestDoRefundApikeyGroupCreditBalanceFailure mirrors the plain-order R1
// proof for the api-key group path: a CreditBalance failure AFTER the group
// claims already committed must be flagged (CreditFailed), and the customer
// DM must be skipped entirely rather than falsely confirming a refund whose
// money never moved.
func TestDoRefundApikeyGroupCreditBalanceFailure(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "eeff0000eeff0000eeff0000eeff0000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-cbf", "sk-test-cbf-0123456789abcdef", "bedrock", 1000, 1000, 400)

	if _, err := st.DB().Exec("DROP TABLE balances"); err != nil {
		t.Fatal(err)
	}

	o, _ := st.GetOrder(keyDes)
	res := b.doRefundApikeyGroup(ctx, o, "balance", false)
	if res == nil {
		t.Fatal("doRefundApikeyGroup returned nil — the claim itself must still win even though the credit fails")
	}
	if !res.CreditFailed {
		t.Error("res.CreditFailed = false, want true")
	}
	if res.Notified {
		t.Error("res.Notified = true — the customer DM must be SKIPPED when the credit failed")
	}
	if got, _ := st.GetOrder(keyDes); got.Status != "refunded" {
		t.Errorf("order status = %q, want refunded (claim committed despite credit failure)", got.Status)
	}
	if lastSendMsgTo(f, chat) != nil {
		t.Error("buyer received a DM despite the credit failing")
	}
}

// TestApikeyRefundAbortsOnUnreadableStats: never guess a payout from stale data
// — an unreadable live balance aborts BEFORE any claim, leaving everything
// untouched, and the customer sees the retry notice.
func TestApikeyRefundAbortsOnUnreadableStats(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "44550000445500004455000044550000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-abort", "sk-test-abort-0123456789abcd", "bedrock", 1000, 1000, 100)
	fk.StatsStatus = 400

	o, _ := st.GetOrder(keyDes)
	b.apikeyRefundPreview(ctx, chat, i18n.EN, o)
	if countContains(f, "Couldn't check this key's remaining credits") != 1 {
		t.Errorf("preview must show the UNAVAIL notice, got %q", lastMsg(t, f).Text)
	}
	if res := b.doRefundApikeyGroup(ctx, o, "balance", false); res != nil {
		t.Fatal("the executor must abort when live credits are unreadable")
	}
	if got, _ := st.GetOrder(keyDes); got.Status != "delivered" {
		t.Errorf("order = %q, want delivered (an abort touches nothing)", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 0 {
		t.Errorf("wallet = %d, want 0 (nothing paid out)", bal)
	}
	fk.mu.Lock()
	nDel := len(fk.Delete)
	fk.mu.Unlock()
	if nDel != 0 {
		t.Error("an aborted refund must not revoke the key")
	}
}

// TestApikeyRefundFullyUsedKey: a burnt key pro-rates to nothing, so no Confirm
// button is offered.
func TestApikeyRefundFullyUsedKey(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "55660000556600005566000055660000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-burnt", "sk-test-burnt-0123456789abcd", "bedrock", 1000, 1000, 1000)

	o, _ := st.GetOrder(keyDes)
	b.apikeyRefundPreview(ctx, chat, i18n.EN, o)
	msg := lastMsg(t, f)
	if !strings.Contains(msg.Text, "fully used") {
		t.Errorf("preview = %q, want the NONE notice", msg.Text)
	}
	if _, ok := findCB(inlineRows(msg), "apikeyrefok:"); ok {
		t.Error("a fully-used key must offer NO Confirm button")
	}
}

// TestApikeyRevokeTreats404AsRevoked: "already gone" is the desired end state,
// so a 404 delete counts as a successful revoke and never blocks a refund.
func TestApikeyRevokeTreats404AsRevoked(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "66770000667700006677000066770000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-404", "sk-test-404-0123456789abcdef", "bedrock", 1000, 1000, 0)
	fk.DeleteStatus = 404

	row, _ := st.APIKeyForOrder(keyDes)
	ok, detail := b.revokeAPIKey(ctx, row)
	if !ok {
		t.Errorf("a 404 delete must count as revoked, got %q", detail)
	}
}

// ---- end-to-end --------------------------------------------------------------

// TestApikeyPurchaseE2E walks buy menu -> amount prompt -> quick-pick -> pay
// screen -> pay by balance -> mint -> delivery, all through Process.
func TestApikeyPurchaseE2E(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newAPIKeyBot(t)
	const chat = 100
	primeKB(b, chat)
	if _, err := st.CreditBalance(fmtInt(chat), 2000000); err != nil {
		t.Fatal(err)
	}

	// /buy shows the virtual API-key row (no folder products exist at all).
	b.Process(ctx, textUpdate(chat, "/buy"))
	buyMsg := lastMsg(t, f)
	if _, ok := findCB(inlineRows(buyMsg), "apikey:start"); !ok {
		t.Fatalf("buy menu missing the API-key row: %q", buyMsg.Text)
	}

	// Tap it -> amount prompt with quick-pick buttons in the SKU's bounds.
	b.Process(ctx, callbackUpdate(chat, "apikey:start"))
	prompt := lastMsg(t, f)
	if !strings.Contains(prompt.Text, "BUY BEDROCK API CREDITS") {
		t.Fatalf("amount prompt = %q", prompt.Text)
	}
	if _, ok := findCB(inlineRows(prompt), "apikeyqty:500"); !ok {
		t.Fatal("quick-pick buttons missing from the amount prompt")
	}

	// Quick-pick 500 credits -> reserved order + pay screen.
	b.Process(ctx, callbackUpdate(chat, "apikeyqty:500"))
	pay := lastMsg(t, f)
	if !strings.Contains(pay.Text, "500 credits") {
		t.Fatalf("pay screen = %q", pay.Text)
	}
	payBtn, ok := findCB(inlineRows(pay), "paybal:")
	if !ok {
		t.Fatal("pay screen missing the pay-by-balance button")
	}
	des := strings.TrimPrefix(payBtn.CallbackData, "paybal:")
	o, _ := st.GetOrder(des)
	if o == nil || o.Product != apikeyProduct || o.Qty != 500 || o.Tier != "API" {
		t.Fatalf("order = %+v, want a 500-credit __apikey__ order with tier API", o)
	}
	if o.Total != 500000 { // 500 credits @ 4 USDT/100 @ 25000 VND/USD
		t.Errorf("total = %d, want 500000", o.Total)
	}

	// Pay by balance -> settle -> mint -> delivery.
	b.Process(ctx, callbackUpdate(chat, "paybal:"+des))
	row, _ := st.APIKeyForOrder(des)
	if row == nil {
		t.Fatal("the key must be minted and stored")
	}
	if got, _ := st.GetOrder(des); got.Status != "delivered" {
		t.Errorf("order = %q, want delivered", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 1500000 {
		t.Errorf("balance = %d, want 1500000 after the 500000 debit", bal)
	}
	if countContains(f, row.APIKey) != 1 {
		t.Error("the cleartext key must be delivered exactly once")
	}
	if countContains(f, "<code>") != 1 {
		t.Error("the delivery message must use the HTML tap-to-copy shape")
	}
}

// TestApikeyTypedAmountCapture pins the pending_apikey capture (#45): PEEK
// escape semantics, thousands-separator tolerance, and the bad-amount retry.
func TestApikeyTypedAmountCapture(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newAPIKeyBot(t)
	const chat = 100
	primeKB(b, chat)

	b.Process(ctx, callbackUpdate(chat, "apikey:start"))
	// A slash command escapes WITHOUT consuming the slot (peek).
	b.Process(ctx, textUpdate(chat, "/support"))
	if b.states.Get(chat).APIKey == nil {
		t.Fatal("a /command must leave the apikey capture ARMED (peek semantics)")
	}
	// An out-of-range amount answers and keeps the slot armed for a retry.
	b.Process(ctx, textUpdate(chat, "5"))
	if countContains(f, "Invalid credit amount") != 1 {
		t.Errorf("below-min amount must be refused, got %q", lastMsg(t, f).Text)
	}
	if b.states.Get(chat).APIKey == nil {
		t.Fatal("an invalid amount must keep the capture armed")
	}
	// "7.500" / "7,500" both mean 7500.
	b.Process(ctx, textUpdate(chat, "7.500"))
	if b.states.Get(chat).APIKey != nil {
		t.Fatal("a valid amount must CONSUME the capture (double-submit guard)")
	}
	o, _ := st.ReservedOrderForChat(fmtInt(chat))
	if o == nil || o.Qty != 7500 {
		t.Fatalf("order = %+v, want 7500 credits", o)
	}
}

// TestApikeyHistoryRedlSlots: an API-key row consumes a re-download SLOT (its
// button becomes a reveal), and a recharge row consumes one while rendering no
// button at all — Python parity, so the buttons never drift older than the text.
func TestApikeyHistoryRedlSlots(t *testing.T) {
	ctx := context.Background()
	b, f, st, _, _ := newAPIKeyBot(t)
	const chat = 100
	// Six delivered orders, newest last-created: the detail view offers at most
	// HISTORY_DEFAULT_N (5) buttons.
	seedOrder(t, st, "h1000000h1000000h1000000h1000000", chat, apikeyProduct, 100, 100000, "delivered")
	seedOrder(t, st, "h2000000h2000000h2000000h2000000", chat, apikeyRechargeProduct, 100, 100000, "delivered")
	seedOrder(t, st, "h3000000h3000000h3000000h3000000", chat, kiroApikeyProduct, 500, 100000, "delivered")
	seedOrder(t, st, "h4000000h4000000h4000000h4000000", chat, store.DepositProduct, 1, 100000, "delivered")

	b.sendHistory(ctx, chat, i18n.EN, "5")
	msg := lastMsg(t, f)
	rows := inlineRows(msg)
	if _, ok := findCB(rows, "apikeyrev:h1000000h1000000h1000000h1000000"); !ok {
		t.Error("a bedrock key order must offer a reveal button, not a dead redl:")
	}
	if _, ok := findCB(rows, "apikeyrev:h3000000h3000000h3000000h3000000"); !ok {
		t.Error("a kiro key order must offer a reveal button")
	}
	if _, ok := findCB(rows, "apikeyrev:h2000000h2000000h2000000h2000000"); ok {
		t.Error("a RECHARGE order has no own key — it must offer no button")
	}
	if _, ok := findCB(rows, "redl:h4000000h4000000h4000000h4000000"); ok {
		t.Error("a deposit buys no file — no re-download button")
	}
	// The detail text uses the friendly credit labels, never the raw sentinels.
	if strings.Contains(msg.Text, "__apikey__") || strings.Contains(msg.Text, "__apikeyrc__") {
		t.Errorf("history text leaked a raw product sentinel: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "API key recharge — 100 credits") {
		t.Errorf("history text missing the recharge label: %q", msg.Text)
	}
}

// ---- review-driven branch coverage -------------------------------------------

// TestApikeyStuckRechargeRefundsFaceValue: a PAID-but-never-APPLIED recharge is
// NOT a group member (its credits never reached the key), so it refunds at FULL
// face value on top of the group proration — and the preview must quote the
// same figure the executor pays (telegram_bot.py:4791-4803 / 4607-4612).
func TestApikeyStuckRechargeRefundsFaceValue(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "77880000778800007788000077880000"
	const stuckDes = "88990000889900008899000088990000"
	// 1000 credits for 1,000,000 VND, 400 already burned -> 600 remaining.
	seedOrder(t, st, keyDes, chat, apikeyProduct, 1000, 1000000, "delivered")
	if _, err := st.AddAPIKey(keyDes, chat, "poolkey-stuck", "sk-test-stuck-0123456789abc", 1000, "bedrock"); err != nil {
		t.Fatal(err)
	}
	fk.register("poolkey-stuck", "sk-test-stuck-0123456789abc", 1000, 400)
	// A 500-credit top-up whose money landed but whose applied_ts was never
	// stamped (Kiro-Go was down between payment and fulfillment).
	seedOrder(t, st, stuckDes, chat, apikeyRechargeProduct, 500, 500000, "paid")
	if _, err := st.AddAPIKeyRecharge(stuckDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}

	stuck, _ := st.GetOrder(stuckDes)
	amount, bought, used := b.apikeyRefundAmount(ctx, stuck)
	// Group = the key order alone (1000 bought, 600 left) -> 600,000 pro-rated,
	// PLUS the stuck order's untouched 500,000 face value.
	if bought != 1000 || used != 400 {
		t.Fatalf("group credits = %d/%d, want 1000 bought / 400 used", bought, used)
	}
	if want := int64(1100000); amount != want {
		t.Fatalf("preview amount = %d, want %d (600000 pro-rated + 500000 face value)", amount, want)
	}
	res := b.doRefundApikeyGroup(ctx, stuck, "balance", false)
	if res == nil {
		t.Fatal("the executor must refund the stuck order alongside the group")
	}
	if res.Amount != 1100000 {
		t.Errorf("payout = %d, want 1100000 (preview must equal payout)", res.Amount)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 1100000 {
		t.Errorf("wallet = %d, want 1100000", bal)
	}
	for _, d := range []string{keyDes, stuckDes} {
		if got, _ := st.GetOrder(d); got.Status != "refunded" {
			t.Errorf("order %s = %q, want refunded", d, got.Status)
		}
	}
}

// TestApikeyStuckOnlyRefundDoesNotRevoke: when the rest of the group was already
// refunded, a stuck-only refund must pay the face value but leave the key alone
// — revoking it would kill a key the customer still rightfully holds
// (telegram_bot.py:4853-4856).
func TestApikeyStuckOnlyRefundDoesNotRevoke(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "99aa000099aa000099aa000099aa0000"
	const stuckDes = "aabb0000aabb0000aabb0000aabb0000"
	seedOrder(t, st, keyDes, chat, apikeyProduct, 1000, 1000000, "delivered")
	if _, err := st.AddAPIKey(keyDes, chat, "poolkey-so", "sk-test-so-0123456789abcdef", 1000, "bedrock"); err != nil {
		t.Fatal(err)
	}
	fk.register("poolkey-so", "sk-test-so-0123456789abcdef", 1000, 0)
	// The key order is ALREADY refunded -> not a refundable group member.
	if _, err := st.ClaimOrder(keyDes, "delivered", "refunded"); err != nil {
		t.Fatal(err)
	}
	seedOrder(t, st, stuckDes, chat, apikeyRechargeProduct, 500, 500000, "paid")
	if _, err := st.AddAPIKeyRecharge(stuckDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}

	stuck, _ := st.GetOrder(stuckDes)
	res := b.doRefundApikeyGroup(ctx, stuck, "balance", false)
	if res == nil || res.Amount != 500000 {
		t.Fatalf("stuck-only payout = %+v, want its 500000 face value", res)
	}
	fk.mu.Lock()
	nDel := len(fk.Delete)
	fk.mu.Unlock()
	if nDel != 0 {
		t.Error("a stuck-only refund must NOT revoke the customer's live key")
	}
	if row, _ := st.APIKeyForOrder(keyDes); row.RefundedTS != nil {
		t.Error("a stuck-only refund must not stamp the key row refunded")
	}
}

// TestApikeyRechargeTOCTOURefundsInstead: if a whole-key refund lands between
// the disabled-check and the recharge commit, this order's credits went onto a
// key whose money was just paid back — and it was excluded from that payout. The
// key is re-revoked and THIS order refunds instead of delivering
// (telegram_bot.py:7758-7772).
func TestApikeyRechargeTOCTOURefundsInstead(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "bbcc0000bbcc0000bbcc0000bbcc0000"
	const rcDes = "ccdd0000ccdd0000ccdd0000ccdd0000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-toctou", "sk-test-toctou-0123456789ab", "bedrock", 1000, 1000, 0)
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")
	// A concurrent whole-key refund commits while Kiro-Go is applying the top-up.
	fk.OnRecharge = func() { _ = st.MarkAPIKeyRefunded(keyDes) }

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("a top-up onto a just-refunded key must not be delivered")
	}
	if rec, _ := st.APIKeyRechargeForOrder(rcDes); rec.AppliedTS != nil {
		t.Error("applied_ts must NOT be stamped when the target was refunded mid-recharge")
	}
	if got, _ := st.GetOrder(rcDes); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("wallet = %d, want the 500000 back", bal)
	}
	fk.mu.Lock()
	nDel := len(fk.Delete)
	fk.mu.Unlock()
	if nDel != 1 {
		t.Errorf("delete_api_key called %d times, want 1 (kill the resurrected key)", nDel)
	}
}

// TestApikeyRCTypedAmountCapture pins the pending_apikeyrc capture (#46):
// PEEK escape, bad-amount retry, and the atomic claim on a valid amount.
func TestApikeyRCTypedAmountCapture(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "ddee0000ddee0000ddee0000ddee0000"
	primeKB(b, chat)
	seedKey(t, st, fk, keyDes, chat, "poolkey-cap46", "sk-test-cap46-0123456789ab", "bedrock", 1000, 1000, 500)

	b.Process(ctx, callbackUpdate(chat, "apikeyrcpick:"+keyDes))
	if b.states.Get(chat).APIKeyRC == nil {
		t.Fatal("picking a key must ARM the recharge capture")
	}
	// A tapped keyboard label escapes WITHOUT consuming the slot (peek).
	b.Process(ctx, textUpdate(chat, i18n.C().DescFor("buy").EN))
	if b.states.Get(chat).APIKeyRC == nil {
		t.Fatal("a keyboard label must leave the recharge capture ARMED (peek)")
	}
	// Below the SKU minimum -> refused, slot stays armed.
	b.Process(ctx, textUpdate(chat, "5"))
	if countContains(f, "Invalid credit amount") != 1 {
		t.Errorf("below-min top-up must be refused, got %q", lastMsg(t, f).Text)
	}
	if b.states.Get(chat).APIKeyRC == nil {
		t.Fatal("an invalid amount must keep the recharge capture armed")
	}
	// A valid amount consumes the slot and creates the bound recharge order.
	b.Process(ctx, textUpdate(chat, "300"))
	if b.states.Get(chat).APIKeyRC != nil {
		t.Fatal("a valid amount must CONSUME the capture (double-submit guard)")
	}
	o, _ := st.ReservedOrderForChat(fmtInt(chat))
	if o == nil || o.Product != apikeyRechargeProduct || o.Qty != 300 {
		t.Fatalf("order = %+v, want a 300-credit __apikeyrc__ order", o)
	}
	rec, _ := st.APIKeyRechargeForOrder(o.Des)
	if rec == nil || rec.TargetOrderID != keyDes || rec.AppliedTS != nil {
		t.Fatalf("binding = %+v, want an unapplied binding to %s written at ORDER CREATION", rec, keyDes)
	}
}

// TestApikeyQtyCrossSKUButtonRefused: a stale bedrock quick-pick after the kiro
// prompt was armed (or vice versa) must NOT buy the wrong product at the wrong
// rate — the two credit units are ~5.7x apart.
func TestApikeyQtyCrossSKUButtonRefused(t *testing.T) {
	ctx := context.Background()
	b, _, st, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["kiro_apikey_enabled"] = true
	s["kiro_apikey_usdt_per_100"] = 1.0
	writeSettings(t, dir, s)
	const chat = 100

	// Arm the KIRO prompt, then tap a stale BEDROCK quick-pick.
	b.Process(ctx, callbackUpdate(chat, "kiroapi:start"))
	b.Process(ctx, callbackUpdate(chat, "apikeyqty:500"))
	if o, _ := st.ReservedOrderForChat(fmtInt(chat)); o != nil {
		t.Fatalf("a cross-SKU stale button created an order: %+v", o)
	}
	if b.states.Get(chat).APIKey == nil {
		t.Error("a refused cross-SKU tap must leave the armed prompt intact")
	}
	// The MATCHING button still works and buys the kiro SKU.
	b.Process(ctx, callbackUpdate(chat, "kiroapiqty:500"))
	o, _ := st.ReservedOrderForChat(fmtInt(chat))
	if o == nil || o.Product != kiroApikeyProduct {
		t.Fatalf("order = %+v, want a __kiroapi__ order", o)
	}
}

// TestApikeyMyKeysLegacyClassFallsBackToServer: a legacy row the bot stored with
// an empty sale_class takes its DISPLAY class from the live stats row — the
// server knows classes the bot predates (telegram_bot.py:4340-4342).
func TestApikeyMyKeysLegacyClassFallsBackToServer(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const des = "eeff0000eeff0000eeff0000eeff0000"
	seedKey(t, st, fk, des, chat, "poolkey-leg", "sk-test-leg-0123456789abcdef", "", 1000, 1000, 0)
	fk.StatsSaleClass = "kiro"

	b.showMyAPIKeys(ctx, chat, i18n.EN)
	if !strings.Contains(lastMsg(t, f).Text, "🟣 Kiro") {
		t.Errorf("a legacy row classed kiro server-side must render the Kiro tag, got %q", lastMsg(t, f).Text)
	}
}

// TestApikeySelfServiceEntryPoints: the two buyer-facing views this slice owns
// are actually REACHABLE — ss:mykeys lists the keys, refreq:<des> shows the
// group-refund preview for an api-key order (and ignores anything else, which
// M2.6 owns).
func TestApikeySelfServiceEntryPoints(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "0a0a00000a0a00000a0a00000a0a0000"
	seedKey(t, st, fk, keyDes, chat, "poolkey-ss", "sk-test-ss-0123456789abcdef", "bedrock", 1000, 1000, 400)

	b.Process(ctx, callbackUpdate(chat, "ss:mykeys"))
	if !strings.Contains(lastMsg(t, f).Text, "YOUR API KEYS") {
		t.Errorf("ss:mykeys must render the My-Keys view, got %q", lastMsg(t, f).Text)
	}
	b.Process(ctx, callbackUpdate(chat, "refreq:"+keyDes))
	msg := lastMsg(t, f)
	if !strings.Contains(msg.Text, "API KEY REFUND") {
		t.Errorf("refreq: on an api-key order must show the preview, got %q", msg.Text)
	}
	if _, ok := findCB(inlineRows(msg), "apikeyrefok:"+keyDes); !ok {
		t.Error("the preview must carry a Confirm button for the acted-on order")
	}
	// Another chat's order reveals nothing.
	before := f.sendCount()
	b.Process(ctx, callbackUpdate(200, "refreq:"+keyDes))
	if f.sendCount() != before {
		t.Error("refreq: on a foreign order must be a dead button")
	}
}

// TestApikeyRateInvalidPer100DisablesSKU pins the Python float()-error path
// (telegram_bot.py:6180-6191): the legacy per-1k fallback applies ONLY when
// `apikey_usdt_per_100` is absent / nil / "" — a PRESENT-but-garbage value
// disables the SKU (rate 0), it must NOT silently re-price the shop off the
// write-only legacy key.
func TestApikeyRateInvalidPer100DisablesSKU(t *testing.T) {
	b, _, _, fk, dir := newAPIKeyBot(t)
	s := apikeySettings(fk)
	s["apikey_usdt_per_100"] = "garbage"
	s["apikey_usdt_per_1k"] = 40.0 // legacy key present and valid
	writeSettings(t, dir, s)
	if got := b.apikeyRateUSDT(saleClassBedrock); got != 0 {
		t.Errorf("corrupt per-100 rate must disable the SKU, got rate %v (legacy leaked through)", got)
	}
	if b.apikeyForSale(saleClassBedrock) {
		t.Error("a shop with a corrupt rate must not sell keys")
	}
	// An EMPTY per-100 value (unset via the admin panel) is the legacy case and
	// DOES fall back — Python's `if v is None or v == ""`.
	s["apikey_usdt_per_100"] = ""
	writeSettings(t, dir, s)
	if got := b.apikeyRateUSDT(saleClassBedrock); got != 4.0 {
		t.Errorf("empty per-100 must fall back to legacy 40/1k = 4/100, got %v", got)
	}
}

// TestApikeyRechargeDisabledAtDeliveryRefunds pins the delivery-time re-check
// (telegram_bot.py:7731-7740): a key the admin manually DISABLED during the
// payment window must be refused at fulfillment too — a recharge would
// re-enable it server-side — and the money must come back. No recharge call may
// reach Kiro-Go.
func TestApikeyRechargeDisabledAtDeliveryRefunds(t *testing.T) {
	ctx := context.Background()
	b, f, st, fk, _ := newAPIKeyBot(t)
	const chat = 100
	const keyDes = "d15ab1edd15ab1edd15ab1edd15ab1ed"
	const rcDes = "d15ab1e2d15ab1e2d15ab1e2d15ab1e2"
	seedKey(t, st, fk, keyDes, chat, "poolkey-dis", "sk-test-dis-0123456789abcdef", "bedrock", 1000, 1000, 0)
	// The admin disabled the key on Kiro-Go while the order sat unpaid.
	fk.mu.Lock()
	fk.keys["poolkey-dis"].Status = "disabled"
	fk.mu.Unlock()
	if _, err := st.AddAPIKeyRecharge(rcDes, keyDes, chat, 500); err != nil {
		t.Fatal(err)
	}
	o := seedOrder(t, st, rcDes, chat, apikeyRechargeProduct, 500, 500000, "paid")

	if b.deliverApikeyRecharge(ctx, o, rcDes) {
		t.Fatal("must not recharge a manually disabled key")
	}
	if fk.rechargeCount() != 0 {
		t.Error("no recharge call may reach Kiro-Go for a disabled key")
	}
	if got, _ := st.GetOrder(rcDes); got.Status != "refunded" {
		t.Errorf("order = %q, want refunded", got.Status)
	}
	if bal, _ := st.GetBalance(fmtInt(chat)); bal != 500000 {
		t.Errorf("balance = %d, want the full 500000 back", bal)
	}
	if countContains(f, "recharge failed") != 1 {
		t.Error("the buyer must be told the recharge failed and the money came back")
	}
	rec, _ := st.APIKeyRechargeForOrder(rcDes)
	if rec.AppliedTS != nil {
		t.Error("applied_ts must stay NULL for a refused recharge")
	}
}

// TestMaskAPIKey pins _mask_api_key (telegram_bot.py:4220): prefix(7) + "…" +
// tail(4) for anything longer than 14 chars, verbatim otherwise. Lists, button
// labels and owner alerts only ever show this masked form.
func TestMaskAPIKey(t *testing.T) {
	if got := maskAPIKey("sk-test-0123456789abcdef"); got != "sk-test…cdef" {
		t.Errorf("mask = %q, want %q", got, "sk-test…cdef")
	}
	if got := maskAPIKey("sk-short-14ch"); got != "sk-short-14ch" {
		t.Errorf("a <=14-char key renders verbatim, got %q", got)
	}
	if got := maskAPIKey(""); got != "" {
		t.Errorf("empty key masks to empty, got %q", got)
	}
}
