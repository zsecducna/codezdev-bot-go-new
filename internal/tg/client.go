// Client, the generic call() helper, typed method wrappers, and the
// long-poll iterator. Ports telegram_bot.py's inlined API base + call()
// (telegram_bot.py:67-69, 6725-6761), the hand-rolled multipart uploads
// (send_document telegram_bot.py:6795-6869, send_photo_file
// telegram_bot.py:10015-10047), and the getUpdates poll loop shape
// (telegram_bot.py:19019-19039). No third-party HTTP/Bot-API library — the
// architecture doc (§1 internal/tg row) calls this out explicitly so the
// port stays "same behavior, file-for-file auditable" instead of adopting a
// framework's own dispatch model.
package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// API base URL template (telegram_bot.py:67: API = "https://api.telegram.org/bot{token}/{method}").
const apiBase = "https://api.telegram.org/bot"

const (
	// defaultCallTimeout bounds ordinary (non-long-poll) JSON calls when the
	// caller's context carries no deadline of its own. Python's call() uses a
	// flat urlopen(timeout=60) (telegram_bot.py:6736); we trim this a bit
	// since regular Bot API calls (sendMessage, editMessageText, ...) are
	// normally sub-second and a long block should fail fast instead of
	// tying up a handler goroutine.
	defaultCallTimeout = 30 * time.Second

	// uploadTimeout mirrors Python's urlopen(timeout=120) for sendDocument
	// (telegram_bot.py:6836) — generous because account .zip uploads are slow.
	uploadTimeout = 120 * time.Second

	// pollReadBuffer is added on top of the caller's requested getUpdates
	// `timeout` so the HTTP round trip has room to actually receive Telegram's
	// response after the long-poll wait ends server-side. Per the task spec:
	// "long-poll needs read timeout > 50s poll timeout."
	pollReadBuffer = 10 * time.Second

	// maxAttempts mirrors Python's `for attempt in range(4)` retry budget,
	// used identically here for both the JSON call() path (telegram_bot.py:
	// 6733) and the multipart upload path (telegram_bot.py:6839).
	maxAttempts = 4

	// dialTimeout bounds the TCP handshake itself (separate from the overall
	// request timeout above), a sane default the Python original didn't need
	// since urllib's timeout covers connect+read as one figure.
	dialTimeout = 10 * time.Second
)

// allowedUpdateTypes restricts getUpdates to what the bot actually routes on
// (process(), telegram_bot.py:16916-16919 only branches on "callback_query"
// vs "message"), matching telegram_bot.py:19030 exactly so Telegram doesn't
// waste bandwidth pushing update kinds nothing here reads.
var allowedUpdateTypes = []string{"message", "callback_query"}

// Client is a minimal hand-rolled Telegram Bot API client: one token, one
// http.Client. Safe for concurrent use (http.Client is; Client holds no
// mutable state besides that).
type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

// NewClient builds a Client whose transport dials IPv4 only.
//
// Rationale ported verbatim from telegram_bot.py:54-64: this host has an
// IPv6 default route to api.telegram.org whose handshake intermittently
// sticks in SYN-SENT; single-connect (non-Happy-Eyeballs) callers then stall
// until timeout. Python patched socket.getaddrinfo process-wide to filter to
// AF_INET; Go's equivalent is forcing the dial network to "tcp4" in the
// Transport's DialContext, which has the same effect scoped to this client
// (cleaner than a process-wide monkeypatch — nothing else in the Go binary
// needs the override).
func NewClient(token string) *Client {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr) // force IPv4 (telegram_bot.py:54-67)
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		token:   token,
		baseURL: apiBase + token + "/",
		// Deliberately no blanket http.Client.Timeout: the long-poll
		// getUpdates call legitimately holds the connection open ~50s+,
		// while other calls want a much shorter fuse. Each call path below
		// applies its own context deadline instead (call()/sendMultipart
		// default it when the caller's ctx has none; GetUpdates always sets
		// one sized to its poll timeout).
		http: &http.Client{Transport: transport},
	}
}

// ---- token redaction ------------------------------------------------------

// redactedError replaces an error's TEXT while keeping its chain intact, so
// errors.Is / errors.As still see the original cause but no formatting verb
// (%v, %s, %+v, Error()) can print the secret the original text carried.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// tokenPlaceholder is what a redacted bot token is replaced with. Deliberately
// not the token's length or a prefix of it: a partial token is still a partial
// credential leak (and its length alone identifies the bot id).
const tokenPlaceholder = "<redacted-token>"

// redact strips the bot token from err's text before err is returned to any
// caller.
//
// Why this must live HERE and not at the log sites: http.Client.Do returns a
// *url.Error whose Error() embeds the full request URL, and every request URL
// in this package is https://api.telegram.org/bot<TOKEN>/<method>. Wrapping
// such an error with %w preserves that text, so any caller that formats the
// error — every fire-and-forget `log.Printf("send failed: %v", err)` in the
// handlers layer — writes the bot's credential to the log. A bot token is full
// account takeover: read the chats, impersonate the shop, drain the flows.
//
// Redacting inside the client makes the guarantee unconditional: no caller can
// leak the token regardless of which verb it picks, and no future call site has
// to remember a rule. The error chain survives via Unwrap so errors.Is checks
// (context.Canceled, net timeouts) keep working.
func (c *Client) redact(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	msg := err.Error()
	clean := strings.ReplaceAll(msg, c.token, tokenPlaceholder)
	if clean == msg {
		return err // nothing to hide — keep the original error exactly as-is
	}
	return &redactedError{msg: clean, cause: err}
}

// APIError is returned whenever Telegram answers with {"ok": false, ...}
// (telegram_bot.py:6754-6755's `raise RuntimeError(f"{method} failed: ...")`,
// typed instead of stringly so callers can branch on Code/RetryAfter).
type APIError struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is the seconds Telegram asked us to wait before retrying,
	// taken from the "parameters" object on a 429 response (mirrors
	// body.get("parameters", {}).get("retry_after", ...) in Python's
	// send_document, telegram_bot.py:6853). Zero when absent/not applicable.
	RetryAfter int
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("tg: %s failed: %d %s (retry_after=%ds)", e.Method, e.Code, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("tg: %s failed: %d %s", e.Method, e.Code, e.Description)
}

// IsConflict reports whether this is Telegram's 409 response — issued when a
// second getUpdates long-poller starts against the same token while another
// is already connected. bot_service.sh:13-15 encodes the operational rule
// this exists to police: exactly one poller per token, ever.
func (e *APIError) IsConflict() bool { return e.Code == http.StatusConflict }

// IsConflict unwraps err looking for an *APIError representing Telegram's
// 409 "terminated by other getUpdates request" conflict.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsConflict()
}

// call posts JSON to a Bot API method and returns the raw "result" payload
// for the caller to unmarshal into its typed shape. Ports telegram_bot.py's
// call() (6725-6761):
//   - retries transient network errors and non-JSON error bodies (Cloudflare
//     502/504 HTML during a Telegram incident, telegram_bot.py:6738-6746)
//     with the same 4-attempt / 1.5*(attempt+1)s backoff;
//   - raises immediately on an {"ok": false} response — a real API mistake
//     must surface right away, not get retried into a false success.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var body []byte
	if params == nil {
		body = []byte("{}")
	} else {
		var err error
		body, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("tg: marshal %s params: %w", method, err)
		}
	}

	// Per-ATTEMPT deadline, not a shared budget: Python's urlopen(timeout=60)
	// re-arms on every retry (telegram_bot.py:6736), so a hung first attempt
	// still leaves each remaining attempt its full window. A single shared
	// WithTimeout across the loop would let one stalled response read consume
	// the entire budget and silently cancel the very retries this loop exists
	// to provide (hung connects are this bot's documented failure mode,
	// telegram_bot.py:54-58). Callers that DO bring their own deadline
	// (GetUpdates sizes one to the long-poll wait) keep it untouched.
	_, callerHasDeadline := ctx.Deadline()

	url := c.baseURL + method
	var lastErr error
	for attempt := range maxAttempts {
		// Fresh defaultCallTimeout fuse per attempt when the caller brought
		// no deadline (see the parity comment above the loop). cancel is
		// invoked on every exit path of this iteration; the no-op default
		// covers the caller-deadline case.
		attemptCtx := ctx
		var cancel context.CancelFunc = func() {}
		if !callerHasDeadline {
			attemptCtx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
		}

		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			// url.Parse failures echo the offending URL — which carries the token.
			return nil, fmt.Errorf("tg: build %s request: %w", method, c.redact(err))
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			// Network/timeout error — same bucket as Python's
			// urllib.error.URLError / ConnectionError / TimeoutError
			// (telegram_bot.py:6749-6752): transient, back off and retry.
			cancel()
			// http.Client.Do hands back a *url.Error carrying the FULL request
			// URL (https://api.telegram.org/bot<TOKEN>/<method>); redact before
			// it is stored or wrapped, so neither the retry-exhausted wrap below
			// nor any caller's log line can print the token.
			lastErr = c.redact(err)
			// Give up only when the CALLER's context is done — an expired
			// per-attempt fuse is exactly the timeout case Python retries.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("tg: %s: %w", method, lastErr)
			}
			sleepBackoff(attempt)
			continue
		}

		var apiResp apiResponse
		decErr := json.NewDecoder(resp.Body).Decode(&apiResp)
		resp.Body.Close()
		cancel() // body fully consumed; safe to release the attempt fuse
		if decErr != nil {
			// Non-JSON body (e.g. a gateway HTML error page) — treat as
			// transient exactly like Python's json.load(e) ValueError
			// fallback (telegram_bot.py:6738-6746).
			// Redacted too: a mid-body transport failure surfaces here, and that
			// error can also be a *url.Error carrying the tokenized URL.
			lastErr = fmt.Errorf("tg: %s: decode response: %w", method, c.redact(decErr))
			if ctx.Err() != nil {
				return nil, lastErr
			}
			sleepBackoff(attempt)
			continue
		}
		if !apiResp.OK {
			return nil, &APIError{
				Method:      method,
				Code:        apiResp.ErrorCode,
				Description: apiResp.Description,
				RetryAfter:  apiResp.Parameters.RetryAfter,
			}
		}
		return apiResp.Result, nil
	}
	// lastErr is redacted at both assignment sites; redact again so a future
	// assignment that forgets cannot reopen the leak.
	return nil, fmt.Errorf("tg: %s failed after %d attempts: %w", method, maxAttempts, c.redact(lastErr))
}

// sleepBackoff mirrors Python's time.sleep(1.5 * (attempt + 1)) retry pacing
// (telegram_bot.py:6750, 6743).
func sleepBackoff(attempt int) {
	time.Sleep(time.Duration(1.5*float64(attempt+1)*1000) * time.Millisecond)
}

// ---- typed methods -------------------------------------------------------

// GetMe calls getMe and returns the bot's own User record.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	raw, err := c.call(ctx, "getMe", nil)
	if err != nil {
		return nil, err
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("tg: getMe: decode result: %w", err)
	}
	return &u, nil
}

// getUpdatesParams mirrors the payload built at telegram_bot.py:19028-19030:
// {"timeout": 50, "offset": offset, "allowed_updates": [...]}.
type getUpdatesParams struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// GetUpdates long-polls for new updates. offset<=0 omits the field (fetch
// from whatever Telegram still has pending — Python's initial `offset =
// None`, telegram_bot.py:19018); otherwise it's the next-update_id cursor
// (telegram_bot.py:19033: `offset = u["update_id"] + 1`). timeoutSec is the
// Bot API long-poll wait; the HTTP request deadline is set to
// timeoutSec+10s so the read has room to complete after Telegram's wait
// ends server-side (this is why GetUpdates always sets its own context
// deadline rather than relying on call()'s default).
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second+pollReadBuffer)
	defer cancel()

	params := getUpdatesParams{Timeout: timeoutSec, AllowedUpdates: allowedUpdateTypes}
	if offset > 0 {
		params.Offset = offset
	}
	raw, err := c.call(ctx, "getUpdates", params)
	if err != nil {
		return nil, err
	}
	var updates []Update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("tg: getUpdates: decode result: %w", err)
	}
	return updates, nil
}

// SendMessageParams is the sendMessage payload. Fields mirror the dict
// literals built ad hoc throughout telegram_bot.py (e.g. chat_id/text/
// parse_mode/reply_markup at 5665-5667; disable_web_page_preview is part of
// the same Bot API method though not separately grepped, included per the
// task's explicit field list).
type SendMessageParams struct {
	ChatID                int64  `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	ReplyMarkup           any    `json:"reply_markup,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview,omitempty"`
}

// SendMessage sends a text message and returns the sent Message.
func (c *Client) SendMessage(ctx context.Context, p SendMessageParams) (*Message, error) {
	raw, err := c.call(ctx, "sendMessage", p)
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("tg: sendMessage: decode result: %w", err)
	}
	return &m, nil
}

// EditMessageTextParams mirrors the editMessageText payloads built at
// telegram_bot.py:2220-2221, 6059-6060, 8083-8084, 8847-8848.
type EditMessageTextParams struct {
	ChatID      int64  `json:"chat_id"`
	MessageID   int64  `json:"message_id"`
	Text        string `json:"text"`
	ParseMode   string `json:"parse_mode,omitempty"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

// EditMessageText edits a previously sent message's text/keyboard in place.
func (c *Client) EditMessageText(ctx context.Context, p EditMessageTextParams) (*Message, error) {
	raw, err := c.call(ctx, "editMessageText", p)
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("tg: editMessageText: decode result: %w", err)
	}
	return &m, nil
}

// AnswerCallbackQueryParams mirrors the answerCallbackQuery payloads at
// telegram_bot.py:12229-12258 (ack-only, or with a toast/alert text).
type AnswerCallbackQueryParams struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

// AnswerCallbackQuery acks an inline-button tap, clearing its loading
// spinner (and optionally showing a toast/alert). The Bot API result is
// just `true`, so callers only need the error.
func (c *Client) AnswerCallbackQuery(ctx context.Context, p AnswerCallbackQueryParams) error {
	_, err := c.call(ctx, "answerCallbackQuery", p)
	return err
}

// DeleteMessage deletes chatID's messageID (telegram_bot.py:4243, 6696,
// 8709, 10826, 12283).
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	_, err := c.call(ctx, "deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	return err
}

// SetMyCommands sets the "/" command menu, optionally scoped to one chat
// (telegram_bot.py:7375-7383: per-chat scope is how /language swaps only
// the tapping user's menu). scope may be nil for the default (global) scope.
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand, scope *BotCommandScope) error {
	params := map[string]any{"commands": commands}
	if scope != nil {
		params["scope"] = scope
	}
	_, err := c.call(ctx, "setMyCommands", params)
	return err
}

// GetChat fetches a Chat by id (telegram_bot.py:7960, used to resolve the
// owner's live @username for SUPPORT_HANDLE auto-resolve).
func (c *Client) GetChat(ctx context.Context, chatID int64) (*Chat, error) {
	raw, err := c.call(ctx, "getChat", map[string]any{"chat_id": chatID})
	if err != nil {
		return nil, err
	}
	var chat Chat
	if err := json.Unmarshal(raw, &chat); err != nil {
		return nil, fmt.Errorf("tg: getChat: decode result: %w", err)
	}
	return &chat, nil
}

// getFileResult is the getFile response's "result" object — only file_path
// matters (telegram_bot.py never had an equivalent; every prior upload path
// in this port only ever re-sends a file_id back to Telegram, which resolves
// it server-side — see GetFileBytes's own doc comment for why this is new).
type getFileResult struct {
	FilePath string `json:"file_path"`
}

// maxDownloadedFileBytes bounds a single incoming-file download. Telegram's
// Bot API already refuses to serve a getFile above ~20MB, so this is a
// second, independent ceiling (not a workaround for a known limit) — a
// defensive cap against ever buffering an unbounded response into memory,
// mirroring the same "never trust the far end's size claim" posture
// sendMultipart's retry loop takes toward Telegram's own responses.
const maxDownloadedFileBytes = 25 << 20 // 25MB

// GetFileBytes downloads the raw content of a previously uploaded file by its
// file_id — the getFile-then-fetch two-step every Bot API client needs for
// reading an upload's actual bytes, which nothing in this package needed
// until now: SendDocument/SendPhoto only ever UPLOAD, and every existing
// incoming-file path (adminsettings.go's broadcastphoto) only ever stores the
// file_id and re-sends it, since Telegram accepts a file_id anywhere a fresh
// upload is accepted. The 📦 Stock admin category is the first feature that
// must actually READ what an admin uploaded (upstock/provpool account
// lists), so this is genuinely new capability, not a refactor.
func (c *Client) GetFileBytes(ctx context.Context, fileID string) ([]byte, error) {
	raw, err := c.call(ctx, "getFile", map[string]any{"file_id": fileID})
	if err != nil {
		return nil, err
	}
	var f getFileResult
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("tg: getFile: decode result: %w", err)
	}
	if f.FilePath == "" {
		return nil, fmt.Errorf("tg: getFile: empty file_path for file_id")
	}

	// The file CDN lives on a sibling path (api.telegram.org/file/bot<token>/…),
	// not under the JSON-RPC "call" base this package otherwise always posts
	// to, so this builds its own URL rather than reusing c.call.
	url := "https://api.telegram.org/file/bot" + c.token + "/" + f.FilePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// url carries the token; redact before it can reach a caller's log line.
		return nil, fmt.Errorf("tg: downloadFile: build request: %w", c.redact(err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tg: downloadFile: %w", c.redact(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tg: downloadFile: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadedFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("tg: downloadFile: read body: %w", err)
	}
	if len(body) > maxDownloadedFileBytes {
		return nil, fmt.Errorf("tg: downloadFile: file exceeds %d bytes", maxDownloadedFileBytes)
	}
	return body, nil
}

// ---- multipart uploads ----------------------------------------------------

// documentContentType picks a part Content-Type from the filename extension,
// mirroring send_document's mapping (telegram_bot.py:6820-6827) so the order
// zip/JSON/text file is offered as a proper download rather than previewed.
func documentContentType(filename string) string {
	switch low := strings.ToLower(filename); {
	case strings.HasSuffix(low, ".zip"):
		return "application/zip"
	case strings.HasSuffix(low, ".json"):
		return "application/json"
	default:
		return "text/plain"
	}
}

// SendDocument uploads a local file via multipart/form-data (sendDocument).
// Ports send_document (telegram_bot.py:6795-6869): retries transient
// failures (network errors, 429 honoring retry_after, 5xx) up to 4 attempts
// total — the order file is the only thing the customer receives, so a
// single Telegram hiccup must not silently drop it.
func (c *Client) SendDocument(ctx context.Context, chatID int64, filePath, caption string) (*Message, error) {
	filename := filepath.Base(filePath)
	fields := map[string]string{"chat_id": fmt.Sprintf("%d", chatID)}
	if caption != "" {
		fields["caption"] = caption
	}
	raw, err := c.sendMultipart(ctx, "sendDocument", fields, "document", filePath, filename, documentContentType(filename))
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("tg: sendDocument: decode result: %w", err)
	}
	return &m, nil
}

// SendPhoto uploads a local image file via multipart/form-data (sendPhoto).
// Ports send_photo_file (telegram_bot.py:10015-10047): local images (e.g. a
// seller's on-disk QR code) have no URL, so this builds the multipart body
// the same way SendDocument does, always as image/png (matching Python,
// which hardcodes the same content type there).
func (c *Client) SendPhoto(ctx context.Context, chatID int64, filePath, caption string) (*Message, error) {
	fields := map[string]string{"chat_id": fmt.Sprintf("%d", chatID)}
	if caption != "" {
		fields["caption"] = caption
	}
	raw, err := c.sendMultipart(ctx, "sendPhoto", fields, "photo", filePath, filepath.Base(filePath), "image/png")
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("tg: sendPhoto: decode result: %w", err)
	}
	return &m, nil
}

// sendPhotoURLParams is the JSON sendPhoto payload used when `photo` is a URL
// (Telegram fetches the image itself). Distinct from SendPhoto, which uploads a
// local file via multipart.
// SendPhotoURLParams is the full JSON sendPhoto payload for a URL photo. The
// BEP20/TRON pay screens send the address as a Markdown code block AND attach
// the "I have made the payment" inline button to the photo itself
// (telegram_bot.py:9461-9466, 9911-9916), so parse_mode + reply_markup are
// part of the payload there.
type SendPhotoURLParams struct {
	ChatID      int64  `json:"chat_id"`
	Photo       string `json:"photo"`
	Caption     string `json:"caption,omitempty"`
	ParseMode   string `json:"parse_mode,omitempty"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

// SendPhotoURLOpts sends a photo BY URL with the full parameter set (parse_mode
// / reply_markup). SendPhotoURL is the 3-argument convenience wrapper over it.
func (c *Client) SendPhotoURLOpts(ctx context.Context, p SendPhotoURLParams) (*Message, error) {
	raw, err := c.call(ctx, "sendPhoto", p)
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("tg: sendPhoto(url): decode result: %w", err)
	}
	return &m, nil
}

// SendPhotoURL sends a photo BY URL (no local upload) — the 3-argument JSON
// sendPhoto variant (telegram_bot.py:10621, 15642). Telegram downloads the
// image from photoURL and posts it with the caption. Currently unused by
// production handlers (its last caller, Binance Pay's QR sender, was removed
// by the USDT-only migration, S3) — kept as generic client surface; SendPhoto
// requests still use SendPhotoURLOpts directly.
func (c *Client) SendPhotoURL(ctx context.Context, chatID int64, photoURL, caption string) (*Message, error) {
	return c.SendPhotoURLOpts(ctx, SendPhotoURLParams{ChatID: chatID, Photo: photoURL, Caption: caption})
}

// sendMultipart is the shared multipart POST + retry loop behind
// SendDocument/SendPhoto. Built on stdlib mime/multipart (idiomatic Go)
// rather than Python's by-hand boundary construction (telegram_bot.py:
// 6808-6833) — same wire shape, cleaner implementation. Retry policy mirrors
// send_document (telegram_bot.py:6839-6867): up to 4 attempts, transient
// network errors and non-JSON bodies back off 2*(attempt+1)s, a 429 honors
// the server's retry_after (falling back to the same backoff), a 5xx also
// retries, and anything else raises immediately.
func (c *Client) sendMultipart(ctx context.Context, method string, fields map[string]string, fileField, filePath, filename, fileContentType string) (json.RawMessage, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("tg: %s: read %s: %w", method, filePath, err)
	}

	// Same per-ATTEMPT deadline rationale as call(): Python re-arms
	// urlopen(timeout=120) on every retry (telegram_bot.py:6846), and the 429
	// path may legitimately sleep retry_after seconds between attempts — a
	// shared 120s budget would starve later attempts (or be eaten whole by
	// one hung upload). Each attempt below gets its own uploadTimeout fuse
	// when the caller brought no deadline.
	_, callerHasDeadline := ctx.Deadline()

	url := c.baseURL + method
	var lastErr error
	for attempt := range maxAttempts {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			if v == "" {
				continue
			}
			if err := mw.WriteField(k, v); err != nil {
				return nil, fmt.Errorf("tg: %s: write field %s: %w", method, k, err)
			}
		}
		// A plain mw.CreateFormFile would default to
		// application/octet-stream; Telegram (and the customer's client)
		// benefit from the explicit type Python picks per extension
		// (telegram_bot.py:6828-6831), so build the part header by hand.
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, filename))
		h.Set("Content-Type", fileContentType)
		part, err := mw.CreatePart(h)
		if err != nil {
			return nil, fmt.Errorf("tg: %s: create file part: %w", method, err)
		}
		if _, err := part.Write(content); err != nil {
			return nil, fmt.Errorf("tg: %s: write file part: %w", method, err)
		}
		if err := mw.Close(); err != nil {
			return nil, fmt.Errorf("tg: %s: close multipart writer: %w", method, err)
		}

		// Fresh uploadTimeout fuse per attempt (see comment above the loop);
		// cancel runs on every exit path of this iteration.
		attemptCtx := ctx
		var cancel context.CancelFunc = func() {}
		if !callerHasDeadline {
			attemptCtx, cancel = context.WithTimeout(ctx, uploadTimeout)
		}

		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(buf.Bytes()))
		if err != nil {
			cancel()
			// url.Parse failures echo the offending (tokenized) URL.
			return nil, fmt.Errorf("tg: %s: build request: %w", method, c.redact(err))
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())

		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			// Same *url.Error token leak as in call(): redact before storing.
			lastErr = c.redact(err)
			// Give up only when the CALLER's context is done — an expired
			// per-attempt fuse is the retryable-timeout case (Python's
			// `except OSError` bucket, telegram_bot.py:6849-6852).
			if ctx.Err() != nil {
				return nil, fmt.Errorf("tg: %s: %w", method, lastErr)
			}
			sleepUploadBackoff(attempt)
			continue
		}
		var apiResp apiResponse
		decErr := json.NewDecoder(resp.Body).Decode(&apiResp)
		resp.Body.Close()
		cancel() // body fully consumed; safe to release the attempt fuse
		if decErr != nil {
			// A mid-body transport failure lands here carrying the tokenized URL.
			lastErr = fmt.Errorf("tg: %s: decode response: %w", method, c.redact(decErr))
			if ctx.Err() != nil {
				return nil, lastErr
			}
			sleepUploadBackoff(attempt)
			continue
		}
		if !apiResp.OK {
			apiErr := &APIError{
				Method:      method,
				Code:        apiResp.ErrorCode,
				Description: apiResp.Description,
				RetryAfter:  apiResp.Parameters.RetryAfter,
			}
			retryable := attempt < maxAttempts-1
			switch {
			case apiResp.ErrorCode == http.StatusTooManyRequests && retryable:
				// Honor Telegram's retry_after (telegram_bot.py:6857-6859).
				wait := apiResp.Parameters.RetryAfter
				if wait <= 0 {
					wait = 2 * (attempt + 1)
				}
				lastErr = apiErr
				sleepCtx(ctx, time.Duration(wait+1)*time.Second)
				continue
			case isRetryableServerError(apiResp.ErrorCode) && retryable:
				lastErr = apiErr
				sleepUploadBackoff(attempt)
				continue
			default:
				return nil, apiErr
			}
		}
		return apiResp.Result, nil
	}
	// Belt-and-braces redaction of the retry-exhausted wrap (see call()).
	return nil, fmt.Errorf("tg: %s failed after %d attempts: %w", method, maxAttempts, c.redact(lastErr))
}

// isRetryableServerError mirrors telegram_bot.py:6860's transient 5xx set.
func isRetryableServerError(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// sleepUploadBackoff mirrors Python's time.sleep(2 * (attempt + 1))
// (telegram_bot.py:6851, 6862) used throughout send_document's retry loop.
func sleepUploadBackoff(attempt int) { time.Sleep(2 * time.Duration(attempt+1) * time.Second) }

// sleepCtx sleeps for d, waking early if ctx is done first — used for
// retry_after / conflict backoffs so a canceled poll loop doesn't sit
// through a multi-second sleep before noticing shutdown.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ---- long-poll iterator ----------------------------------------------------

const (
	// pollTimeoutSec matches telegram_bot.py:19029's {"timeout": 50}.
	pollTimeoutSec = 50
	// pollErrorBackoff matches telegram_bot.py:19023-19024's
	// `except RuntimeError as e: print(...); time.sleep(3)`.
	pollErrorBackoff = 3 * time.Second
	// conflictBackoff is used instead of pollErrorBackoff specifically for a
	// 409 (another poller already connected) — a little longer than the
	// generic transient-error backoff since a 409 won't self-resolve in 3s;
	// whoever's misconfigured needs a human to notice and fix it.
	conflictBackoff = 5 * time.Second
)

// Poll returns an iterator (Go 1.23+ range-over-func, iter.Seq2) that
// long-polls getUpdates in a loop, tracking the offset cursor itself so
// callers just range over updates:
//
//	for upd, err := range client.Poll(ctx) {
//		if err != nil { log.Println("poll error:", err); continue }
//		... handle upd ...
//	}
//
// Ports the shape of the loop at telegram_bot.py:19019-19039: fixed
// timeout=50 long-poll, offset advanced from the last update_id+1, and
// transient errors logged + retried after a backoff rather than killing the
// loop. A yielded (zero Update, non-nil error) lets the caller observe and
// log failures (including surfacing them for alerting) without stopping
// iteration — mirroring Python's `except RuntimeError: print(...); continue`.
// A 409 conflict (another poller running against this token — the
// bot_service.sh:13-15 exactly-one-poller rule) is logged loudly via the
// standard logger in addition to being yielded, since it signals a
// deployment misconfiguration an operator needs to see, not routine network
// flakiness. The loop exits only when ctx is done.
func (c *Client) Poll(ctx context.Context) iter.Seq2[Update, error] {
	return func(yield func(Update, error) bool) {
		var offset int64
		for {
			if ctx.Err() != nil {
				return
			}
			updates, err := c.GetUpdates(ctx, offset, pollTimeoutSec)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				backoff := pollErrorBackoff
				if IsConflict(err) {
					log.Printf("tg: getUpdates 409 Conflict — another long-poller is already running for this token (bot_service.sh:13-15: exactly one poller per token, ever)")
					backoff = conflictBackoff
				}
				if !yield(Update{}, err) {
					return
				}
				sleepCtx(ctx, backoff)
				continue
			}
			for _, u := range updates {
				offset = u.UpdateID + 1
				if !yield(u, nil) {
					return
				}
			}
		}
	}
}
