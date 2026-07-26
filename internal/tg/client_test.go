package tg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient builds a Client pointed at a fake Bot API httptest.Server.
// White-box (package tg) so the test can set the unexported baseURL/http
// fields directly instead of needing a token that resolves to a real host.
func newTestClient(server *httptest.Server) *Client {
	return &Client{
		token:   "TESTTOKEN",
		baseURL: server.URL + "/",
		http:    server.Client(),
	}
}

// decodeBody reads and JSON-decodes a request body into v, failing the test
// on any error — a small helper shared by several handlers below.
func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// TestCallHappyPath ports the "call() happy path" case: a well-formed
// {"ok": true, "result": ...} response decodes into the typed method's
// return value (telegram_bot.py:6754-6759, GetMe -> getMe).
func TestCallHappyPath(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasSuffix(r.URL.Path, "/getMe") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"ok":     true,
			"result": map[string]any{"id": 42, "is_bot": true, "first_name": "TestBot", "username": "test_bot"},
		})
	}))
	defer server.Close()

	c := newTestClient(server)
	u, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: unexpected error: %v", err)
	}
	if u.ID != 42 || !u.IsBot || u.Username != "test_bot" {
		t.Fatalf("GetMe: unexpected result: %+v", u)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}
}

// TestCallAPIErrorNoRetry ports the "ok:false raises immediately, no retry"
// behavior (telegram_bot.py:6754-6755: a real API mistake — bad chat id,
// bad params — must surface right away, never get silently retried into a
// stale success).
func TestCallAPIErrorNoRetry(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(t, w, map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: chat not found",
		})
	}))
	defer server.Close()

	c := newTestClient(server)
	_, err := c.SendMessage(context.Background(), SendMessageParams{ChatID: 1, Text: "hi"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 400 || apiErr.Description != "Bad Request: chat not found" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
	if apiErr.IsConflict() {
		t.Fatal("400 must not be reported as a conflict")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("API-level error must not be retried, got %d requests", got)
	}
}

// TestCall429RetryAfterParsed ports the 429 case: Telegram's rate-limit
// response carries retry_after inside "parameters" (telegram_bot.py:6853's
// `body.get("parameters") or {}).get("retry_after", ...)`); the typed
// APIError must expose it, and IsConflict/errors reporting must correctly
// distinguish 429 from 409.
func TestCall429RetryAfterParsed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests: retry after 7",
			"parameters":  map[string]any{"retry_after": 7},
		})
	}))
	defer server.Close()

	c := newTestClient(server)
	_, err := c.SendMessage(context.Background(), SendMessageParams{ChatID: 1, Text: "hi"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 429 || apiErr.RetryAfter != 7 {
		t.Fatalf("expected code=429 retry_after=7, got %+v", apiErr)
	}
	if IsConflict(err) {
		t.Fatal("429 must not be classified as a 409 conflict")
	}
}

// TestConflictDetected ports the 409 "another poller running" case
// (bot_service.sh:13-15's exactly-one-poller rule): IsConflict must
// recognize it distinctly from an ordinary API error.
func TestConflictDetected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"ok":          false,
			"error_code":  409,
			"description": "Conflict: terminated by other getUpdates request",
		})
	}))
	defer server.Close()

	c := newTestClient(server)
	_, err := c.GetUpdates(context.Background(), 0, 1)
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict(err) == true, got err=%v", err)
	}
}

// TestGetUpdatesOffsetAdvance ports the getUpdates loop's offset bookkeeping
// (telegram_bot.py:19033: `offset = u["update_id"] + 1`). Drives the actual
// Poll() iterator (not just a single GetUpdates call): the first response
// hands back two updates, and every request after that must carry
// offset = last update_id + 1 (102), proving Poll tracks the cursor itself
// rather than the caller having to.
func TestGetUpdatesOffsetAdvance(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []int64 // offset field of each request, in arrival order
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p getUpdatesParams
		decodeBody(t, r, &p)
		mu.Lock()
		requests = append(requests, p.Offset)
		n := len(requests)
		mu.Unlock()
		if n == 1 {
			writeJSON(t, w, map[string]any{
				"ok": true,
				"result": []map[string]any{
					{"update_id": 100, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 5}, "text": "a"}},
					{"update_id": 101, "message": map[string]any{"message_id": 2, "chat": map[string]any{"id": 5}, "text": "b"}},
				},
			})
			return
		}
		// Every request from here on: nothing new. Poll keeps re-polling
		// with offset=102 until the test's context deadline stops it.
		writeJSON(t, w, map[string]any{"ok": true, "result": []any{}})
	}))
	defer server.Close()

	c := newTestClient(server)
	// Short deadline: once the first (2-update) response is drained, every
	// further round trip is an instant empty reply against a local
	// httptest.Server, so this is enough time for several more requests to
	// land before Poll's GetUpdates call finally observes ctx.Done().
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var got []Update
	for upd, err := range c.Poll(ctx) {
		if err != nil {
			break // expected: context deadline exceeded once the empty-result loop drains it
		}
		got = append(got, upd)
	}
	if len(got) != 2 || got[0].UpdateID != 100 || got[1].UpdateID != 101 {
		t.Fatalf("unexpected updates: %+v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d: %v", len(requests), requests)
	}
	if requests[0] != 0 {
		t.Fatalf("expected first request's offset to be omitted (0), got %d", requests[0])
	}
	if requests[1] != 102 {
		t.Fatalf("expected second request offset=102 (last update_id 101 + 1), got %d", requests[1])
	}
}

// TestSendMessageKeyboardMarshalGolden pins the exact JSON body SendMessage
// puts on the wire for a message with an inline keyboard, guarding against
// accidental field renames/omission (parse_mode, reply_markup shape) since
// these are the literals telegram_bot.py builds by hand throughout (e.g.
// 1729-1732, 8083-8084).
func TestSendMessageKeyboardMarshalGolden(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		writeJSON(t, w, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 7}},
		})
	}))
	defer server.Close()

	c := newTestClient(server)
	params := SendMessageParams{
		ChatID:    7,
		Text:      "Choose a language:",
		ParseMode: "HTML",
		ReplyMarkup: InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "🇻🇳 Tiếng Việt", CallbackData: "lang:vi"}},
			{{Text: "🇬🇧 English", CallbackData: "lang:en"}},
		}},
		DisableWebPagePreview: true,
	}
	if _, err := c.SendMessage(context.Background(), params); err != nil {
		t.Fatalf("SendMessage: unexpected error: %v", err)
	}

	const golden = `{"chat_id":7,"text":"Choose a language:","parse_mode":"HTML","reply_markup":{"inline_keyboard":[[{"text":"🇻🇳 Tiếng Việt","callback_data":"lang:vi"}],[{"text":"🇬🇧 English","callback_data":"lang:en"}]]},"disable_web_page_preview":true}`
	if strings.TrimSpace(string(gotBody)) != golden {
		t.Fatalf("request body mismatch:\n got:  %s\n want: %s", gotBody, golden)
	}
}

// TestSendDocumentMultipartRoundtrip ports the "multipart SendDocument
// round-trip" case: a real file on disk goes out as multipart/form-data
// with the right field names/content-type (documentContentType, mirroring
// telegram_bot.py:6820-6827) and the fake server's parsed result comes back
// through to the typed Message return.
func TestSendDocumentMultipartRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "order-abc123.json")
	const fileContent = `{"email":"user@example.com","password":"hunter2"}`
	if err := os.WriteFile(path, []byte(fileContent), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	var (
		gotChatID  string
		gotCaption string
		gotFile    []byte
		gotCType   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("expected multipart content-type, got %q (err=%v)", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read multipart part: %v", err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("read part body: %v", err)
			}
			switch part.FormName() {
			case "chat_id":
				gotChatID = string(data)
			case "caption":
				gotCaption = string(data)
			case "document":
				gotFile = data
				gotCType = part.Header.Get("Content-Type")
			default:
				t.Fatalf("unexpected form field: %s", part.FormName())
			}
		}
		writeJSON(t, w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 99,
				"chat":       map[string]any{"id": 7},
				"document":   map[string]any{"file_id": "FILE123", "file_name": "order-abc123.json"},
			},
		})
	}))
	defer server.Close()

	c := newTestClient(server)
	msg, err := c.SendDocument(context.Background(), 7, path, "Your order")
	if err != nil {
		t.Fatalf("SendDocument: unexpected error: %v", err)
	}
	if msg.MessageID != 99 || msg.Chat.ID != 7 {
		t.Fatalf("unexpected returned message: %+v", msg)
	}
	if msg.Document == nil || msg.Document.FileID != "FILE123" {
		t.Fatalf("unexpected returned document: %+v", msg.Document)
	}
	if gotChatID != "7" {
		t.Fatalf("expected chat_id field %q, got %q", "7", gotChatID)
	}
	if gotCaption != "Your order" {
		t.Fatalf("expected caption field %q, got %q", "Your order", gotCaption)
	}
	if string(gotFile) != fileContent {
		t.Fatalf("uploaded file content mismatch:\n got:  %s\n want: %s", gotFile, fileContent)
	}
	if gotCType != "application/json" {
		t.Fatalf("expected document part content-type application/json (per .json extension), got %q", gotCType)
	}
}

// ---- bot-token redaction --------------------------------------------------

// leakTestToken is a realistic-shaped bot token (<bot_id>:<secret>) used by the
// redaction tests below. It is fake — the point is that it must never appear in
// an error string, not that it is valid.
const leakTestToken = "7654321:AAH-FakeSecretForTests_0123456789abcd"

// errTransport is a RoundTripper that always fails, forcing http.Client.Do to
// return the *url.Error that carries the full (tokenized) request URL — the
// exact shape that used to leak the bot token into the handlers' log lines.
type errTransport struct{}

// errForcedTransport is the sentinel errTransport fails with, so a test can
// assert the error CHAIN survived redaction (not just the text).
var errForcedTransport = errors.New("forced transport failure")

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errForcedTransport
}

// newLeakTestClient builds a Client whose baseURL is the REAL api.telegram.org
// shape (apiBase + token + "/") so the URL embedded in a transport error
// actually contains the token, while the transport itself never touches the
// network.
func newLeakTestClient() *Client {
	return &Client{
		token:   leakTestToken,
		baseURL: apiBase + leakTestToken + "/",
		http:    &http.Client{Transport: errTransport{}},
	}
}

// canceledCtx returns an already-canceled context so the retry loop bails out
// after the first failed attempt instead of sleeping through four backoffs.
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestCallErrorNeverLeaksBotToken pins the security invariant behind
// Client.redact: a transport failure on the JSON call() path must not put the
// bot token anywhere in the returned error's text. The bot token is full
// account takeover, and every fire-and-forget send in the handlers layer logs
// its error with %v (e.g. handlers/otp.go's delivery-failure line), so any
// token left in the text lands in the log file verbatim.
func TestCallErrorNeverLeaksBotToken(t *testing.T) {
	c := newLeakTestClient()

	_, err := c.GetMe(canceledCtx())
	if err == nil {
		t.Fatal("expected a transport error from the always-failing RoundTripper")
	}
	assertNoToken(t, err)
}

// TestSendMessageErrorNeverLeaksBotToken covers the exact call the OTP delivery
// path makes (handlers/otp.go SendMessage -> log.Printf("...: %v", err)).
func TestSendMessageErrorNeverLeaksBotToken(t *testing.T) {
	c := newLeakTestClient()

	_, err := c.SendMessage(canceledCtx(), SendMessageParams{ChatID: 5, Text: "hi"})
	if err == nil {
		t.Fatal("expected a transport error from the always-failing RoundTripper")
	}
	assertNoToken(t, err)
}

// TestSendMultipartErrorNeverLeaksBotToken covers the upload path, which builds
// its own request/retry loop and therefore has its own set of wrap sites.
func TestSendMultipartErrorNeverLeaksBotToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	c := newLeakTestClient()

	_, err := c.SendDocument(canceledCtx(), 7, path, "Your order")
	if err == nil {
		t.Fatal("expected a transport error from the always-failing RoundTripper")
	}
	assertNoToken(t, err)
}

// TestGetUpdatesErrorNeverLeaksBotToken covers the HIGHEST-VOLUME leak path.
//
// cmd/bot/main.go logs every long-poll failure with `log.Println("poll error:",
// perr)`, and perr is whatever Poll yields. Poll does not re-wrap: it forwards
// the error from GetUpdates verbatim (`yield(Update{}, err)`), so GetUpdates is
// the producer to pin. Every transient network blip — of which there are many
// over a long-lived poller — used to write the bot token to the log there, which
// made this the dominant sink by volume even though the OTP path is the one that
// advertises secret hygiene.
//
// Driven through GetUpdates rather than Poll because Poll only yields after the
// full 4-attempt retry budget (~9s of real backoff); the error value under test
// is byte-identical either way.
func TestGetUpdatesErrorNeverLeaksBotToken(t *testing.T) {
	c := newLeakTestClient()

	_, err := c.GetUpdates(canceledCtx(), 0, pollTimeoutSec)
	if err == nil {
		t.Fatal("expected a transport error from the always-failing RoundTripper")
	}
	assertNoToken(t, err)
}

// TestBuildRequestErrorNeverLeaksBotToken covers the OTHER wrap site in each
// retry loop: http.NewRequestWithContext fails before any transport call when
// the URL will not parse, and its *url.Error also embeds the raw (tokenized)
// URL. A control character in the path forces that branch without touching the
// network.
//
// Not covered here, redacted by inspection only: the retry-exhausted wraps
// ("failed after N attempts"), which are reachable solely by letting all four
// attempts run — ~9s of real backoff, too slow to be worth a unit test.
func TestBuildRequestErrorNeverLeaksBotToken(t *testing.T) {
	c := newLeakTestClient()
	c.baseURL = apiBase + leakTestToken + "/\x7f" // DEL is illegal in a URL

	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected a URL parse error")
	}
	assertNoToken(t, err)
}

// TestRedactPreservesErrorChain asserts redaction is not a lobotomy: callers
// that branch on the cause (errors.Is on a transport sentinel, context
// cancellation, net timeouts) must keep working even though the printed text
// changed. redactedError therefore replaces the TEXT and keeps Unwrap intact.
func TestRedactPreservesErrorChain(t *testing.T) {
	c := newLeakTestClient()

	_, err := c.GetMe(canceledCtx())
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if !errors.Is(err, errForcedTransport) {
		t.Fatalf("redaction broke the error chain: errors.Is(err, errForcedTransport) is false for %v", err)
	}
}

// TestRedactLeavesTokenlessErrorsUntouched pins that redact is a no-op (same
// error value, not a copy) when there is nothing to hide, so unrelated errors
// keep their concrete type for errors.As.
func TestRedactLeavesTokenlessErrorsUntouched(t *testing.T) {
	c := newLeakTestClient()
	orig := &APIError{Method: "sendMessage", Code: 400, Description: "Bad Request"}

	if got := c.redact(orig); got != error(orig) {
		t.Fatalf("expected the identical error value back, got %#v", got)
	}
	if c.redact(nil) != nil {
		t.Fatal("redact(nil) must stay nil")
	}
}

// assertNoToken fails the test if the token appears anywhere in err's rendered
// text — checked through both Error() and the %v verb, since callers use both.
func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), leakTestToken) {
		t.Fatalf("bot token leaked via Error(): %s", err.Error())
	}
	if formatted := fmt.Sprintf("%v", err); strings.Contains(formatted, leakTestToken) {
		t.Fatalf("bot token leaked via %%v: %s", formatted)
	}
	if !strings.Contains(err.Error(), tokenPlaceholder) {
		t.Fatalf("expected the redaction placeholder %q in the error text, got: %s",
			tokenPlaceholder, err.Error())
	}
}
