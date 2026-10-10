// Command migrate-notify's notification engine. main.go is the CLI wrapper
// (flags, env fallbacks, exit codes); this file holds the per-recipient loop
// (list unnotified migrated keys → DM the new key → stamp the row), the
// Telegram sendMessage client with its documented 429/5xx retry discipline,
// and the three-language message bodies.
//
// SAFETY INVARIANT, same class as the bot's own delivery path: the customer
// key and the bot token are SECRETS. They are placed into the HTTPS request
// body and the Telegram URL respectively, and appear NOWHERE else — never in
// the dry-run transcript (which prints chat/lang/credits/order only), never
// in a log line, and never in an error string (transport errors from net/http
// embed the request URL, so every error that can carry one is sanitized
// through safeErr before it is returned or logged).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"codezdev-bot/internal/store"
)

// telegramAPIBase is the real Bot API root; a Notifier field (not a const at
// the use site) purely so tests can point it at an httptest fake — the URL
// template is api.telegram.org's own documented "<base>/bot<token>/<method>".
const telegramAPIBase = "https://api.telegram.org"

// maxSendRetries is how many TIMES a 429/5xx/transport-failed sendMessage is
// retried after its first attempt (so up to 4 attempts total): Telegram's
// flood limits resolve in seconds, and one bad moment must not cost a
// customer their DM for the day.
const maxSendRetries = 3

// Notifier bundles the collaborators the notify loop needs — same explicit
// wiring discipline as migrate-llmgate's Migrator: every field is set by main
// (tests wire fakes and zero delays instead), so the algorithm's inputs are
// all visible at the call site.
type Notifier struct {
	// Store is the read-write handle to store.db; only
	// ListUnnotifiedMigratedKeys / MarkMigratedNotified are called on it.
	Store *store.Store
	// Log receives operational warnings/errors (stderr via slog); the
	// operator-facing per-recipient/summary lines go to Out instead.
	Log *slog.Logger
	// Out receives the dry-run recipient list and the apply transcript.
	Out io.Writer
	// HTTP is the client used for the sendMessage calls (its Timeout, not
	// any retry logic, bounds a single attempt).
	HTTP *http.Client
	// TgBase is the Telegram Bot API root (telegramAPIBase in production;
	// an httptest URL in tests).
	TgBase string
	// Token is the bot's own Telegram token — the DMs go out under the SAME
	// bot identity the customers already talk to. SECRET: never logged.
	Token string
	// APIBase is the shop's API endpoint the DM tells customers to call
	// (env LLMGATE_API_BASE; the shop's llmgate deployment by default).
	APIBase string
	// PubBase is the human-facing site the DM links for quota/docs
	// (env LLMGATE_PUBLIC_URL; falls back to APIBase).
	PubBase string
	// SendPause is the idle time BETWEEN two sends (1s in production) —
	// pacing the whole fan-out under Telegram's flood limits instead of
	// discovering them mid-run. Tests pass 0.
	SendPause time.Duration
	// RetryWait is the backoff used when a 429 does not document a
	// retry_after (and for every 5xx/transport retry) — 3s in production,
	// near-zero in tests.
	RetryWait time.Duration
}

// NotifyCounts is the run's tally: every listed recipient ends up Sent or
// Failed, and the exit code derives from Failed alone.
type NotifyCounts struct {
	// Recipients is how many unnotified migrated keys the run listed.
	Recipients int
	// Sent: sendMessage accepted AND the row was stamped — the only
	// success bucket.
	Sent int
	// Failed: the DM never landed (send exhausted its retries) or landed
	// but the stamp errored. The row stays unnotified either way (except
	// the stamp-error corner: the customer WAS served, the row just says
	// otherwise — loudly logged for the operator to fix by hand).
	Failed int
}

// Run executes the whole notification pass. Dry run: print one line per
// recipient (chat, lang, credits, order — NEVER the key) and touch nothing.
// Apply: DM each recipient their new key, stamp the row on success, and keep
// going past per-recipient failures — one unreachable chat must not strand
// the other hundreds. A non-nil error means the run could not complete (the
// listing query failed, or the context was cancelled between sends); every
// per-recipient outcome — send failure, even a stamp error after a
// successful DM — lands in the Counts instead, and main turns Failed>0 or
// the error into exit 1.
func (n *Notifier) Run(ctx context.Context, apply bool, limit int) (NotifyCounts, error) {
	var c NotifyCounts

	// Same limit translation as migrate-llmgate: limit<=0 means "all", but
	// SQL "LIMIT 0" returns NOTHING, so 0 becomes SQLite's no-limit -1.
	sqlLimit := limit
	if sqlLimit <= 0 {
		sqlLimit = -1
	}
	keys, err := n.Store.ListUnnotifiedMigratedKeys(sqlLimit)
	if err != nil {
		return c, fmt.Errorf("list unnotified migrated keys: %w", err)
	}
	c.Recipients = len(keys)

	// DRY RUN: the recipient audit only — chat/lang/credits/order. The key
	// is deliberately absent: a piped dry-run transcript is exactly the kind
	// of artifact that ends up in a ticket, and it must carry no secrets.
	if !apply {
		for _, k := range keys {
			fmt.Fprintf(n.Out, "would-notify chat=%d lang=%s credits=%.2f order=%s\n",
				k.ChatID, k.Lang, k.Credits, k.OrderID)
		}
		fmt.Fprintf(n.Out, "DRY RUN — nothing was sent; %d recipient(s) would be DM'd their new key\n",
			c.Recipients)
		return c, nil
	}

	for i, k := range keys {
		// The DM body, keyed off the ORDER's language (vi/zh, en default —
		// the store's COALESCE already applied the default; the switch here
		// only picks the template).
		text := messageFor(k.Lang, k.APIKey, n.APIBase, n.PubBase)

		switch sendErr := n.send(ctx, k.ChatID, text); {
		case sendErr != nil:
			// Per-recipient failure: count, log (chat + order + reason —
			// never key/token), CONTINUE. The row keeps its NULL stamp, so
			// the next run picks this customer up again by itself.
			c.Failed++
			n.Log.Error("migrate-notify: DM failed — row stays unnotified for the next run",
				"chat", k.ChatID, "order", k.OrderID, "error", sendErr.Error())
			fmt.Fprintf(n.Out, "failed chat=%d order=%s\n", k.ChatID, k.OrderID)
		default:
			// Sent. Stamp the row BEFORE counting success: an unstamped
			// sent-DM is the one state worse than a clean failure (the
			// customer has the key, the database will re-DM them), so a
			// stamp error is loud and reads as a failed recipient the
			// operator must reconcile by hand.
			if err := n.Store.MarkMigratedNotified(k.OrderID); err != nil {
				c.Failed++
				n.Log.Error("migrate-notify: DM sent but stamp FAILED — customer was served, row will re-DM on next run; stamp by hand",
					"chat", k.ChatID, "order", k.OrderID, "error", err.Error())
				fmt.Fprintf(n.Out, "stamp-failed chat=%d order=%s\n", k.ChatID, k.OrderID)
				continue
			}
			c.Sent++
			fmt.Fprintf(n.Out, "sent chat=%d lang=%s order=%s\n", k.ChatID, k.Lang, k.OrderID)
		}

		// Pace BETWEEN sends (not after the last): 1s idle keeps the whole
		// fan-out under Telegram's per-bot flood limits, and skipping the
		// trailing sleep keeps a one-recipient run instant.
		if i < len(keys)-1 {
			if err := sleepCtx(ctx, n.SendPause); err != nil {
				return c, fmt.Errorf("aborted between sends: %w", err)
			}
		}
	}
	fmt.Fprintf(n.Out, "summary: recipients=%d sent=%d failed=%d\n",
		c.Recipients, c.Sent, c.Failed)
	return c, nil
}

// ---- Telegram sendMessage -----------------------------------------------------

// sendMessageRequest is the exact sendMessage body this tool sends: chat_id,
// text, parse_mode=HTML (the message templates carry <code> markup and
// pre-escaped entities), and disable_web_page_preview — the preview card
// would push the tap-to-copy key down the screen on every DM.
type sendMessageRequest struct {
	ChatID                int64  `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// telegramResponse is the slice of Bot API response shape this tool decides
// on: the top-level ok, and parameters.retry_after — the documented flood
// wait Telegram attaches to a 429.
type telegramResponse struct {
	OK         bool `json:"ok"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
	Description string `json:"description"`
}

// send delivers one HTML DM, retrying per the documented discipline: HTTP 200
// with ok:true is the ONLY success; 429 waits parameters.retry_after seconds
// (falling back to RetryWait when undocumented) and retries; 5xx and
// transport errors wait RetryWait and retry; every other 4xx is permanent
// (retrying a 400/403 cannot change the answer). Up to maxSendRetries
// retries, then the error is returned sanitized. The token rides the URL and
// net/http embeds URLs in transport errors, so EVERY error path funnels
// through safeErr — the token must never reach a log line.
func (n *Notifier) send(ctx context.Context, chatID int64, text string) error {
	// The URL is built once; the token in it is the reason this whole method
	// is as defensive about error text as it is.
	url := n.TgBase + "/bot" + n.Token + "/sendMessage"

	var lastErr error
	for attempt := 0; attempt <= maxSendRetries; attempt++ {
		if attempt > 0 {
			// Backoff before a retry: the wait was chosen where the failure
			// was classified below (documented retry_after wins over the
			// default). Cancellation-aware, same as the between-sends pause.
			if err := sleepCtx(ctx, n.retryDelay(lastErr)); err != nil {
				return n.safeErr(err)
			}
		}

		body, err := json.Marshal(sendMessageRequest{
			ChatID: chatID, Text: text,
			ParseMode: "HTML", DisableWebPagePreview: true,
		})
		if err != nil {
			// A marshal failure of our own structs is a bug, not a
			// Telegram condition — permanent, and carries no secret anyway.
			return fmt.Errorf("encode sendMessage request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return n.safeErr(fmt.Errorf("build sendMessage request: %w", err))
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := n.HTTP.Do(req)
		if err != nil {
			// Transport-level failure (DNS, TLS, timeout): retryable. The
			// *url.Error text names the URL — sanitize before anything else
			// touches it.
			lastErr = n.safeErr(fmt.Errorf("transport: %w", err))
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		closeErr := resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response (status %d): %w", resp.StatusCode, err)
			continue
		}
		if closeErr != nil {
			lastErr = fmt.Errorf("close response body: %w", closeErr)
			continue
		}

		var tr telegramResponse
		// A non-JSON body (proxy page, HTML error) decodes as ok:false —
		// which lands in the permanent-failure branch below. Good enough:
		// the status code already said what happened.
		_ = json.Unmarshal(data, &tr)

		switch {
		case resp.StatusCode == http.StatusOK && tr.OK:
			// The only documented success shape.
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			// 429: retry after the DOCUMENTED wait when present, else the
			// default backoff. retryDelay reads it off lastErr's typed
			// payload below.
			lastErr = &retryableError{msg: fmt.Sprintf("429 flood-limited (retry_after=%ds)", tr.Parameters.RetryAfter), retryAfter: tr.Parameters.RetryAfter}
		case resp.StatusCode >= 500:
			// 5xx: Telegram-side hiccup — retry on the default backoff.
			lastErr = &retryableError{msg: fmt.Sprintf("server error %d", resp.StatusCode)}
		default:
			// Permanent (400 bad request, 403 bot blocked by the chat, 404
			// wrong token...): retrying cannot change the answer. The
			// description is Telegram's own text — it contains no secret.
			return fmt.Errorf("sendMessage rejected: %d %s", resp.StatusCode, tr.Description)
		}
	}
	// Retries exhausted. lastErr is already sanitized on every path that
	// could carry the URL; wrap with the attempt count for the operator.
	return fmt.Errorf("gave up after %d retries: %w", maxSendRetries, lastErr)
}

// retryableError is a failure worth retrying, carrying the documented
// retry_after seconds when Telegram sent one (0 = not documented → use the
// Notifier's default RetryWait).
type retryableError struct {
	msg        string
	retryAfter int
}

func (e *retryableError) Error() string { return e.msg }

// retryDelay picks the wait before the next attempt: the documented
// retry_after when the failure carries one, else the default RetryWait.
func (n *Notifier) retryDelay(err error) time.Duration {
	var re *retryableError
	if errors.As(err, &re) && re.retryAfter > 0 {
		return time.Duration(re.retryAfter) * time.Second
	}
	return n.RetryWait
}

// safeErr strips the bot token from any error text before it is returned or
// logged — net/http's *url.Error embeds the full request URL (which contains
// "/bot<token>"), so an unsanitized transport error would leak the credential
// the whole tool is careful to keep out of logs. Belt and suspenders: the
// key never rides an error at all, but the same scrub is applied uniformly.
func (n *Notifier) safeErr(err error) error {
	if err == nil || n.Token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), n.Token, "***TOKEN***"))
}

// sleepCtx waits d, returning early with ctx's error if cancelled first —
// so Ctrl-C stays responsive during the pacing sleeps and flood waits.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- the three message bodies --------------------------------------------------

// messageTemplates maps order-lang → DM body. Placeholders {key} {apibase}
// {pubbase} are substituted with html-escape values at send time (the same
// belt-and-suspenders the bot's sendHTML paths apply: llmgate keys are
// [A-Za-z0-9_-] so escaping changes nothing today, but a future key alphabet
// must not become an HTML-injection vector into a parse_mode=HTML message).
// The literal &lt;/&amp; entities inside the bodies are INTENTIONAL template
// text (they must reach Telegram pre-escaped).
var messageTemplates = map[string]string{
	"en": `🔄 Your API key was upgraded — codezdev shop

The shop now serves API traffic through a faster gateway. Your remaining credits were carried over to this NEW key:

🔑 Your new API key (tap to copy):
<code>{key}</code>

📍 Base URL:
<code>{apibase}</code>
🔐 Header: <code>Authorization: Bearer &lt;your key&gt;</code>

⏳ Valid 30 days — recharging adds credits AND extends validity by another 30 days.
📊 Check quota &amp; usage anytime: {pubbase}/customer
📖 Client setup: {pubbase}/docs

Your OLD key still works for now — switch when convenient. View the new key anytime: Self-Service → 🔑 My API Keys.`,
	"vi": `🔄 API key của bạn đã được nâng cấp — codezdev shop

Shop hiện phục vụ lưu lượng API qua một cổng nhanh hơn. Số tín dụng còn lại của bạn đã được chuyển sang key MỚI này:

🔑 API key mới của bạn (chạm để sao chép):
<code>{key}</code>

📍 Base URL:
<code>{apibase}</code>
🔐 Header: <code>Authorization: Bearer &lt;key của bạn&gt;</code>

⏳ Hiệu lực 30 ngày — nạp thêm sẽ cộng tín dụng VÀ gia hạn thêm 30 ngày nữa.
📊 Kiểm tra hạn mức &amp; mức dùng bất cứ lúc nào: {pubbase}/customer
📖 Hướng dẫn kết nối: {pubbase}/docs

Key CŨ của bạn vẫn còn dùng được tạm thời — hãy chuyển khi tiện. Xem key mới bất cứ lúc nào: Tự phục vụ → 🔑 My API Keys.`,
	"zh": `🔄 您的 API key 已升级 — codezdev shop

本店现在通过更快的网关提供 API 服务。您剩余的额度已转入这把新 key：

🔑 您的新 API key（点击复制）：
<code>{key}</code>

📍 Base URL：
<code>{apibase}</code>
🔐 请求头：<code>Authorization: Bearer &lt;您的 key&gt;</code>

⏳ 有效期 30 天 — 充值会增加额度并再延长 30 天有效期。
📊 随时查询额度与用量：{pubbase}/customer
📖 客户端配置：{pubbase}/docs

您的旧 key 目前仍可使用 — 方便时再切换即可。随时可在 自助服务 → 🔑 My API Keys 查看新 key。`,
}

// messageFor renders one recipient's DM: pick the template by order lang
// (anything but vi/zh is English — the store already COALESCE'd NULL/missing
// langs to 'en', this switch only guards unknown values), then substitute
// the three placeholders with their HTML-escaped values.
func messageFor(lang, key, apiBase, pubBase string) string {
	tmpl, ok := messageTemplates[lang]
	if !ok {
		tmpl = messageTemplates["en"]
	}
	return strings.NewReplacer(
		"{key}", html.EscapeString(key),
		"{apibase}", html.EscapeString(apiBase),
		"{pubbase}", html.EscapeString(pubBase),
	).Replace(tmpl)
}
