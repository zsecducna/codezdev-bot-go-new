// Tests for migrate-notify: end-to-end through the REAL notifier against an
// httptest fake speaking Telegram's Bot API wire shape, plus the real store
// on a temp store.db — no interface mocks, so the tests pin the whole stack
// from the Notifier's counts down to the SQL stamp.
//
// Fake wire shape: POST /bot<token>/sendMessage with a JSON body
// {chat_id, text, parse_mode, disable_web_page_preview}; 200 {ok:true} is
// the documented success; 429 carries {ok:false, parameters:{retry_after}};
// per-chat fail switches produce the permanent (403) and transient (500/429)
// failure inputs. The fake also RECORDS every request's body so the tests
// can assert the HTML message actually carried the key — and that the
// transcript never did.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codezdev-bot/internal/store"
)

// ---- fake Telegram ------------------------------------------------------------

// fakeTelegram is a sendMessage-only Bot API. fail429/fail500 map a chat_id
// to how many of its NEXT sends should fail with that status (then succeed);
// failForever marks a chat as permanently refused (403, the "bot blocked"
// shape a retry can never fix).
type fakeTelegram struct {
	mu sync.Mutex

	token  string
	calls  int // total sendMessage requests, retries included
	bodies []sentBody

	fail429     map[int64]int
	fail500     map[int64]int
	failForever map[int64]bool
}

// sentBody is the decoded request payload of one sendMessage call.
type sentBody struct {
	ChatID                int64  `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// newFakeTelegram builds a fake serving the given bot token.
func newFakeTelegram(token string) *fakeTelegram {
	return &fakeTelegram{
		token:       token,
		fail429:     map[int64]int{},
		fail500:     map[int64]int{},
		failForever: map[int64]bool{},
	}
}

// ServeHTTP implements POST /bot<token>/sendMessage with the verdict ladder
// above; every non-2xx body carries Telegram's documented shape so the
// notifier's retry_after parsing exercises the real wire format.
func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodPost || r.URL.Path != "/bot"+f.token+"/sendMessage" {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}
	var b sentBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	f.calls++
	f.bodies = append(f.bodies, b)

	switch {
	case f.failForever[b.ChatID]:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"ok": false, "description": "Forbidden: bot was blocked by the user",
		})
	case f.fail429[b.ChatID] > 0:
		f.fail429[b.ChatID]--
		// retry_after deliberately UNDOCUMENTED (absent) — the notifier must
		// fall back to its default backoff instead of relying on the field.
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok": false, "description": "Too Many Requests: retry later",
		})
	case f.fail500[b.ChatID] > 0:
		f.fail500[b.ChatID]--
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "description": "Internal Server Error",
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// writeJSON emits v as a JSON HTTP response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- test environment -----------------------------------------------------------

// testEnv wires the notifier under test to the fake Telegram and a real store.
type testEnv struct {
	tg  *fakeTelegram
	st  *store.Store
	out *bytes.Buffer
	n   *Notifier
}

// notifierToken is the fake bot token the tests serve — also the string the
// sanitize assertions look for in error text.
const notifierToken = "12345:TEST-TOKEN"

// newEnv starts the fake Bot API, opens a fresh store.db, and builds the
// Notifier with zero pacing (the sleeps exist for Telegram's flood limits,
// not for correctness — the tests must not pay for them).
func newEnv(t *testing.T) *testEnv {
	t.Helper()

	tg := newFakeTelegram(notifierToken)
	tgSrv := httptest.NewServer(tg)
	t.Cleanup(tgSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	out := &bytes.Buffer{}
	n := &Notifier{
		Store:     st,
		Log:       logger,
		Out:       out,
		HTTP:      &http.Client{Timeout: 5 * time.Second},
		TgBase:    tgSrv.URL,
		Token:     notifierToken,
		APIBase:   "https://api.test-shop.uk",
		PubBase:   "https://pub.test-shop.uk",
		SendPause: 0,
		RetryWait: time.Millisecond,
	}
	return &testEnv{tg: tg, st: st, out: out, n: n}
}

// seedMigrated inserts one MIGRATED llmgate row (the state
// migrate-llmgate -apply leaves behind) plus its order row for the language,
// and returns nothing — the des is the handle assertions use.
func (e *testEnv) seedMigrated(t *testing.T, des string, chat int64, lang, newKey string, credits float64) {
	t.Helper()
	if ins, err := e.st.AddAPIKey(des, chat, "pool-"+des, "sk-pool-"+des, 1, "bedrock", store.BackendPool); err != nil || !ins {
		t.Fatalf("seed pool row %s: ins=%v err=%v", des, ins, err)
	}
	if err := e.st.CreateOrder(des, strconv.FormatInt(chat, 10), lang, "__apikey__", "", 1, 100); err != nil {
		t.Fatalf("seed order %s: %v", des, err)
	}
	if moved, err := e.st.MigrateAPIKeyToLlmgate(des, "pool-"+des, "sk-pool-"+des, "lg-"+des, newKey, credits); err != nil || !moved {
		t.Fatalf("seed migrate %s: moved=%v err=%v", des, moved, err)
	}
}

// stampCreatedAt forces a row's created_at — AddAPIKey stamps whole-second
// values, so same-instant seeds would tie and ORDER BY created_at ASC (the
// queue's oldest-first contract) would be non-deterministic in the tests
// that assert order. Uses the Store's public DB() escape hatch; test-only.
func (e *testEnv) stampCreatedAt(t *testing.T, des string, createdAt int64) {
	t.Helper()
	if _, err := e.st.DB().Exec("UPDATE api_keys SET created_at=? WHERE order_id=?", createdAt, des); err != nil {
		t.Fatalf("stamp created_at %s: %v", des, err)
	}
}

// run is Run with the background ctx folded in.
func (e *testEnv) run(t *testing.T, apply bool, limit int) (NotifyCounts, error) {
	t.Helper()
	return e.n.Run(context.Background(), apply, limit)
}

// ---- the required cases ---------------------------------------------------------

// TestDryRunPrintsRecipientsAndSendsNothing: a dry run prints one line per
// recipient (chat, lang, credits, order) plus the DRY RUN summary, sends
// ZERO Telegram requests, stamps nothing — and the printed transcript never
// contains a key value.
func TestDryRunPrintsRecipientsAndSendsNothing(t *testing.T) {
	e := newEnv(t)
	e.seedMigrated(t, "ord-en", 11, "en", "sk-lg-ord-en", 100)
	e.seedMigrated(t, "ord-vi", 22, "vi", "sk-lg-ord-vi", 200.5)

	counts, err := e.run(t, false, 0)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if counts.Recipients != 2 || counts.Sent != 0 || counts.Failed != 0 {
		t.Errorf("counts: %+v, want recipients=2 sent=0 failed=0", counts)
	}
	e.tg.mu.Lock()
	calls := e.tg.calls
	e.tg.mu.Unlock()
	if calls != 0 {
		t.Errorf("dry run sent %d Telegram request(s), want 0", calls)
	}
	out := e.out.String()
	for _, want := range []string{
		"would-notify chat=11 lang=en credits=100.00 order=ord-en",
		"would-notify chat=22 lang=vi credits=200.50 order=ord-vi",
		"DRY RUN — nothing was sent; 2 recipient(s) would be DM'd their new key",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run transcript missing %q:\n%s", want, out)
		}
	}
	// NO key values in the transcript — it must be safe to paste into a ticket.
	if strings.Contains(out, "sk-lg-") {
		t.Errorf("dry-run transcript leaks a key value:\n%s", out)
	}
	// Stamps untouched: a subsequent apply must still see both rows.
	keys, err := e.st.ListUnnotifiedMigratedKeys(100)
	if err != nil || len(keys) != 2 {
		t.Fatalf("dry run stamped rows: %d rows err=%v, want 2 unnotified", len(keys), err)
	}
}

// TestApplySendsHTMLWithKeyAndStampsRows is the apply happy path: each
// recipient gets a parse_mode=HTML, no-preview message carrying the NEW key
// in a <code> block plus the base URLs; the row is stamped, so a re-run
// sends nothing.
func TestApplySendsHTMLWithKeyAndStampsRows(t *testing.T) {
	e := newEnv(t)
	e.seedMigrated(t, "ord-en", 11, "en", "sk-lg-ord-en", 100)
	e.seedMigrated(t, "ord-zh", 33, "zh", "sk-lg-ord-zh", 300)
	// Deterministic queue order: en (11) first, zh (33) second.
	e.stampCreatedAt(t, "ord-en", 5000)
	e.stampCreatedAt(t, "ord-zh", 5001)

	counts, err := e.run(t, true, 0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Recipients != 2 || counts.Sent != 2 || counts.Failed != 0 {
		t.Errorf("counts: %+v, want recipients/sent/failed = 2/2/0", counts)
	}
	e.tg.mu.Lock()
	bodies := append([]sentBody(nil), e.tg.bodies...)
	e.tg.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(bodies))
	}

	en := bodies[0]
	if en.ChatID != 11 || en.ParseMode != "HTML" || !en.DisableWebPagePreview {
		t.Errorf("en send params wrong: %+v", en)
	}
	for _, want := range []string{
		"<code>sk-lg-ord-en</code>",
		"<code>https://api.test-shop.uk</code>",
		"https://pub.test-shop.uk/customer",
		"https://pub.test-shop.uk/docs",
		"Authorization: Bearer &lt;your key&gt;",
		"Valid 30 days",
	} {
		if !strings.Contains(en.Text, want) {
			t.Errorf("en message missing %q:\n%s", want, en.Text)
		}
	}
	zh := bodies[1]
	if zh.ChatID != 33 || !strings.Contains(zh.Text, "<code>sk-lg-ord-zh</code>") ||
		!strings.Contains(zh.Text, "有效期 30 天") {
		t.Errorf("zh message wrong (must be the zh template with the key):\n%s", zh.Text)
	}

	// The stamp is the delivery record: a re-run finds an empty queue, sends
	// nothing, and still exits clean.
	e.out.Reset()
	counts, err = e.run(t, true, 0)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if counts.Recipients != 0 || counts.Sent != 0 || counts.Failed != 0 {
		t.Errorf("re-run counts: %+v, want all zero (stamped rows must be skipped)", counts)
	}
	e.tg.mu.Lock()
	calls := e.tg.calls
	e.tg.mu.Unlock()
	if calls != 2 {
		t.Errorf("re-run sent %d more request(s), want 0", calls-2)
	}
}

// TestMessageForLanguagesAndEscaping pins the template picker (unknown lang
// → English) and the HTML-escaping of the substituted values — the
// belt-and-suspenders the bot's sendHTML paths apply, so a future key or URL
// alphabet containing &<> can never inject markup into a parse_mode=HTML DM.
func TestMessageForLanguagesAndEscaping(t *testing.T) {
	// vi template identifiable, en fallback for junk langs.
	if m := messageFor("vi", "k", "https://a", "https://p"); !strings.Contains(m, "API key của bạn") {
		t.Errorf("vi message did not use the vi template:\n%s", m)
	}
	if m := messageFor("fr", "k", "https://a", "https://p"); !strings.Contains(m, "Your API key was upgraded") {
		t.Errorf("unknown lang must fall back to English:\n%s", m)
	}
	// Escaping: a hostile key value must arrive entity-encoded, and both
	// URL placeholders must survive verbatim.
	m := messageFor("en", `sk-x"<a>&'`, "https://a&b", "https://p")
	if strings.Contains(m, `sk-x"<a>`) {
		t.Errorf("key value not HTML-escaped:\n%s", m)
	}
	if !strings.Contains(m, "https://a&amp;b") {
		t.Errorf("apibase not HTML-escaped:\n%s", m)
	}
}

// TestFailingChatSkippedOthersStillSent: one permanently-refused chat (403,
// unretryable by contract) is counted failed, logged for the next run, and
// does NOT stop the fan-out — the other customers still get their DMs and
// stamps; the failed row stays unnotified.
func TestFailingChatSkippedOthersStillSent(t *testing.T) {
	e := newEnv(t)
	e.seedMigrated(t, "ord-bad", 11, "en", "sk-lg-ord-bad", 100)
	e.seedMigrated(t, "ord-good", 22, "en", "sk-lg-ord-good", 200)
	e.tg.mu.Lock()
	e.tg.failForever[11] = true
	e.tg.mu.Unlock()
	// Deterministic queue order: the blocked chat first, the good one second.
	e.stampCreatedAt(t, "ord-bad", 5100)
	e.stampCreatedAt(t, "ord-good", 5101)

	counts, err := e.run(t, true, 0)
	if err != nil {
		t.Fatalf("apply: %v (a failing chat must not abort the run)", err)
	}
	if counts.Recipients != 2 || counts.Sent != 1 || counts.Failed != 1 {
		t.Errorf("counts: %+v, want recipients/sent/failed = 2/1/1", counts)
	}
	out := e.out.String()
	if !strings.Contains(out, "failed chat=11 order=ord-bad") || !strings.Contains(out, "sent chat=22 lang=en order=ord-good") {
		t.Errorf("transcript missing per-recipient verdicts:\n%s", out)
	}
	if !strings.Contains(out, "summary: recipients=2 sent=1 failed=1") {
		t.Errorf("summary line wrong:\n%s", out)
	}
	// The failed row keeps its NULL stamp (retried next run); the sent one
	// is stamped. Verified through the store's own queue.
	keys, err := e.st.ListUnnotifiedMigratedKeys(100)
	if err != nil || len(keys) != 1 || keys[0].OrderID != "ord-bad" {
		t.Fatalf("after run, unnotified queue = %+v err=%v, want exactly [ord-bad]", keys, err)
	}
}

// TestRetryThenSuccessHonors429And5xx: a 429 (undocumented retry_after →
// default backoff) and a 500 both retry and then succeed — one retryable
// hiccup must not cost the customer their DM. The request COUNT proves the
// retries actually happened (3 requests per recipient, not 1).
func TestRetryThenSuccessHonors429And5xx(t *testing.T) {
	e := newEnv(t)
	e.seedMigrated(t, "ord-429", 11, "en", "sk-lg-ord-429", 100)
	e.seedMigrated(t, "ord-500", 22, "en", "sk-lg-ord-500", 100)
	e.tg.mu.Lock()
	e.tg.fail429[11] = 2 // two flood-limits, then success — within maxSendRetries
	e.tg.fail500[22] = 1
	e.tg.mu.Unlock()

	counts, err := e.run(t, true, 0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Sent != 2 || counts.Failed != 0 {
		t.Errorf("counts: %+v, want sent=2 failed=0 (retryable statuses must recover)", counts)
	}
	e.tg.mu.Lock()
	calls := e.tg.calls
	e.tg.mu.Unlock()
	if calls != 5 { // 3 attempts for ord-429 (429, 429, ok), 2 for ord-500 (500, ok)
		t.Errorf("sendMessage calls = %d, want 5 (retries must actually re-request)", calls)
	}
}

// TestRetryExhaustionCountsFailed: 429s that NEVER clear exhaust the retry
// budget (1 + maxSendRetries attempts), the recipient is counted failed, the
// run continues, and the row stays queued.
func TestRetryExhaustionCountsFailed(t *testing.T) {
	e := newEnv(t)
	e.seedMigrated(t, "ord-flood", 11, "en", "sk-lg-ord-flood", 100)
	e.tg.mu.Lock()
	e.tg.fail429[11] = 1000 // never clears
	e.tg.mu.Unlock()

	counts, err := e.run(t, true, 0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Failed != 1 || counts.Sent != 0 {
		t.Errorf("counts: %+v, want failed=1 sent=0", counts)
	}
	e.tg.mu.Lock()
	calls := e.tg.calls
	e.tg.mu.Unlock()
	if calls != 1+maxSendRetries {
		t.Errorf("attempts = %d, want %d (1 + maxSendRetries)", calls, 1+maxSendRetries)
	}
	if keys, err := e.st.ListUnnotifiedMigratedKeys(100); err != nil || len(keys) != 1 {
		t.Errorf("flooded row must stay queued for the next run: %+v err=%v", keys, err)
	}
}

// TestRetryDelayHonorsDocumentedRetryAfter pins the wait picker directly:
// a failure carrying Telegram's retry_after waits exactly that many seconds;
// anything else falls back to the Notifier's default backoff.
func TestRetryDelayHonorsDocumentedRetryAfter(t *testing.T) {
	n := &Notifier{RetryWait: 3 * time.Second}
	if got := n.retryDelay(&retryableError{msg: "429", retryAfter: 7}); got != 7*time.Second {
		t.Errorf("retryDelay(retry_after=7) = %v, want 7s", got)
	}
	if got := n.retryDelay(&retryableError{msg: "429", retryAfter: 0}); got != 3*time.Second {
		t.Errorf("retryDelay(undocumented) = %v, want the 3s default", got)
	}
	if got := n.retryDelay(nil); got != 3*time.Second {
		t.Errorf("retryDelay(nil) = %v, want the 3s default", got)
	}
}

// TestSafeErrScrubsToken: transport errors from net/http embed the request
// URL — which contains /bot<token> — so safeErr must replace the token
// everywhere it appears. The customer key never rides an error, but the
// transcript assertion in the dry-run test covers that side.
func TestSafeErrScrubsToken(t *testing.T) {
	e := newEnv(t)
	secret := "70" + notifierToken + "XY" // embed the token mid-string
	err := e.n.safeErr(io.ErrUnexpectedEOF)
	if err == nil || err.Error() != io.ErrUnexpectedEOF.Error() {
		t.Fatalf("safeErr(nil-safe passthrough) = %v", err)
	}
	scrubbed := e.n.safeErr(errString("boom " + secret + " boom"))
	if strings.Contains(scrubbed.Error(), notifierToken) {
		t.Errorf("safeErr leaked the token: %q", scrubbed.Error())
	}
	if !strings.Contains(scrubbed.Error(), "***TOKEN***") {
		t.Errorf("safeErr did not mark the scrub site: %q", scrubbed.Error())
	}
}

// errString is a trivial error value for the scrub test.
type errString string

func (e errString) Error() string { return string(e) }

// TestLimitPagesRecipients: -limit slices the queue oldest-first, leaving
// the rest stamped-free for the next run.
func TestLimitPagesRecipients(t *testing.T) {
	e := newEnv(t)
	e.seedMigrated(t, "ord-1", 11, "en", "sk-lg-1", 10)
	e.seedMigrated(t, "ord-2", 22, "en", "sk-lg-2", 20)
	e.seedMigrated(t, "ord-3", 33, "en", "sk-lg-3", 30)
	e.stampCreatedAt(t, "ord-1", 6000)
	e.stampCreatedAt(t, "ord-2", 6001)
	e.stampCreatedAt(t, "ord-3", 6002)

	counts, err := e.run(t, true, 1)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if counts.Recipients != 1 || counts.Sent != 1 {
		t.Errorf("counts: %+v, want recipients/sent = 1/1", counts)
	}
	e.tg.mu.Lock()
	first := sentBody{}
	if len(e.tg.bodies) > 0 {
		first = e.tg.bodies[0]
	}
	e.tg.mu.Unlock()
	// Oldest first: ord-1 was created earliest.
	if first.ChatID != 11 {
		t.Errorf("-limit picked chat %d, want 11 (oldest row first)", first.ChatID)
	}
	if keys, err := e.st.ListUnnotifiedMigratedKeys(100); err != nil || len(keys) != 2 {
		t.Errorf("remaining queue = %d rows err=%v, want 2", len(keys), err)
	}
}

// TestResolveConfigFallbacksAndGates pins the env fallback table and the
// exit-2 gates: -db required always; TELEGRAM_BOT_TOKEN required only for
// -apply (dry run works tokenless); LLMGATE_API_BASE defaults to the shop
// gateway; LLMGATE_PUBLIC_URL falls back to the resolved api base.
func TestResolveConfigFallbacksAndGates(t *testing.T) {
	env := map[string]string{
		"TELEGRAM_BOT_TOKEN": "tg-token",
		"LLMGATE_API_BASE":   "https://api.env.uk",
		"LLMGATE_PUBLIC_URL": "https://pub.env.uk",
	}
	getenv := func(k string) string { return env[k] }

	// Happy path: everything from env.
	cfg, err := resolveConfig("store.db", true, 0, getenv)
	if err != nil || cfg.token != "tg-token" || cfg.apiBase != "https://api.env.uk" || cfg.pubBase != "https://pub.env.uk" {
		t.Errorf("full-env config = %+v err=%v", cfg, err)
	}

	// Defaults: no env at all — api base is the shop gateway, pub base
	// follows it; a DRY run tolerates the missing token.
	empty := func(string) string { return "" }
	cfg, err = resolveConfig("store.db", false, 0, empty)
	if err != nil {
		t.Fatalf("tokenless dry run must resolve: %v", err)
	}
	if cfg.token != "" || cfg.apiBase != defaultShopAPIBase || cfg.pubBase != defaultShopAPIBase {
		t.Errorf("defaults = %+v, want empty token + both bases on %q", cfg, defaultShopAPIBase)
	}

	// Public URL alone missing → falls back to the api base (not the default).
	cfg, err = resolveConfig("store.db", false, 0, func(k string) string {
		if k == "LLMGATE_API_BASE" {
			return "https://custom.api.uk"
		}
		return ""
	})
	if err != nil || cfg.pubBase != "https://custom.api.uk" {
		t.Errorf("pub fallback = %+v err=%v, want pubBase to follow apiBase", cfg, err)
	}

	// The two exit-2 gates.
	if _, err = resolveConfig("", true, 0, getenv); !errors.Is(err, errConfig) {
		t.Errorf("missing -db must be a config error, got %v", err)
	}
	if _, err = resolveConfig("store.db", true, 0, empty); !errors.Is(err, errConfig) {
		t.Errorf("-apply without token must be a config error, got %v", err)
	}
}
