// Package llmgate is the HTTP client for the llmgate user/key-management
// API served behind https://api.codezdev-shop.uk (a Cloudflare-proxied alias
// of llmgate.app — every call in this package goes to the configured base
// URL, never to llmgate.app directly).
//
// Consumers: internal/handlers (selling __apikey__ product, metering,
// top-ups) and the standalone key-migration tool. The exported surface of
// this package is a FIXED CONTRACT for those callers — change it only with
// the same coordination as a wire-format change.
//
// Every response from this API is wrapped:
//
//	{"code":<int>,"message":"success","data":<payload>}
//
// with the HTTP status matching the body code (405 means wrong method —
// notably /api/v1/apikey/{id}/reveal is POST-only and 405s a GET). "data"
// may be absent or null even on a 2xx (delete returns no data field at
// all) — that is SUCCESS, not an error.
//
// Hard invariants encoded below (every one verified against the live API;
// all are load-bearing, not style preferences):
//
//  1. TWO AUTH REALMS with different failure semantics. The USER realm
//     (login, create/update/delete/list/reveal) authenticates with a Bearer
//     JWT obtained from POST /api/v1/user/login (~30-day token; independent
//     logins do NOT invalidate each other, so re-logging in is always safe).
//     A 401 there means the token went stale: re-login once and retry the
//     request once; a second 401 is ErrAuthFailed. The KEY realm
//     (GET /api/v1/apikey/stats) authenticates with the customer's own
//     sk-llmgate-... key. A 401 there means the KEY itself is bad: NEVER
//     re-login-retry (no user token would fix it), surface the *APIError.
//  2. The cleartext upstream key appears on the wire exactly twice —
//     create and reveal — and is returned to the caller ONCE (Key.APIKey,
//     empty on list/update). This client NEVER logs request or response
//     bodies, tokens, or key values: only path, status code and attempt
//     number reach the logger. Server-supplied text that must become error
//     text is passed through redactSecrets first (invariant 7).
//  3. Expiry is CALLER-controlled on the wire. A ZERO time structurally
//     OMITS expires_at from the create/update body (omitempty over an
//     empty string), and the verified behavior of this API is that an
//     omitted expires_at on CREATE yields a key that NEVER expires.
//     Sending a zero/empty expires_at VALUE instead would mean "expired
//     at epoch", a silently dead key — which is why the omission is
//     structural, never a marshaled zero. The bot's mint path passes
//     now+DefaultValidity (business rule 2026-10-10: sold keys live 30
//     days; recharging an ACTIVE key RENEWS that window). The stats
//     endpoint reports expires_at ONLY when the key has one (verified).
//  4. UpdateKey is ABSOLUTE, not additive: credit_limit is SET to the
//     request value, and OMITTING name CLEARS the stored name (verified) —
//     so UpdateKey always sends the current name and never lets a caller
//     accidentally blank it. expires_at is settable on update (verified);
//     its omission semantics there are UNVERIFIED, so the recharge path
//     always sends a concrete renewal time. The update response's
//     created_at is zeroed garbage ("0001-01-01T00:00:00Z", verified) and
//     its api_key comes back absent (only api_key_length): Key decodes
//     both fine and callers must not trust those fields off an update.
//  5. Usage numbers from /stats LAG reality by ~30-60 seconds (the server
//     aggregates asynchronously). A "just topped up" key can still report
//     exhausted for a minute; handlers must not treat a fresh stats read as
//     a realtime balance. 1 credit = $1 of usage; credit_limit 0 =
//     unlimited.
//  6. Retry policy (mirrors internal/kirogo invariant 7): retry ONLY on
//     5xx responses or network/timeout errors, maxAttempts total, linear
//     backoff. NEVER retry a 4xx — it is a real rejection a retry cannot
//     fix (and retrying a create could mint a second paid key). A 2xx goes
//     to wrapper decode: body code != 200 is an *APIError, absent/null
//     "data" is success. HTTP 404 maps to ErrNotFound. ONE EXCEPTION:
//     CreateKey sends a SINGLE attempt (no 5xx/network retry either — see
//     sendOnce): a create retried after a timeout can mint a second live
//     key for one paid sale, so its committed-but-unanswered ambiguity is
//     resolved by the caller's reconciliation, never by a resend.
//  7. Redaction: any server-supplied string that becomes error text is
//     scrubbed of sk-llmgate-... key shapes and JWT shapes before it can
//     reach a log line. Whether the server ever echoes one is not
//     knowable from this repo; the one-regexp pass makes the answer
//     irrelevant.
package llmgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"
)

// DefaultBaseURL is the production llmgate front door. internal/config pins
// Config.BaseURL to this when the env knob is unset; this package never
// injects it itself (Configured stays a pure reflection of what was pinned,
// so a missing override is visible instead of silently papered over).
const DefaultBaseURL = "https://api.codezdev-shop.uk"

// DefaultValidity is how long a freshly minted key lives: 30 DAYS exactly
// (business rule 2026-10-10, revised same day: 30 days, NOT a calendar
// month — AddDate(0,1,0) would hand buyers 28-to-31-day "months" and drift
// by up to a day either way). ONE constant because three call sites must
// agree on it: the mint path (fresh keys), the recharge renewal (topping up
// an ACTIVE key extends its window) and cmd/migrate-llmgate (migrated keys
// adopt the same validity). It lives HERE, at the llmgate wire boundary
// every caller computes it against.
const DefaultValidity = 30 * 24 * time.Hour

// Config is the llmgate connection configuration the bot pins from env
// (internal/config owns loading; this package only consumes). All three
// fields are required for USER-realm calls; the KEY realm (KeyStats) needs
// only BaseURL because it authenticates with the customer's key, not a
// login.
type Config struct {
	// BaseURL is the API root, e.g. DefaultBaseURL. Trailing slashes are
	// trimmed defensively in New so path concatenation can't double-slash.
	BaseURL string
	// Username / Password are the llmgate panel credentials exchanged for
	// a Bearer JWT by lazy/proactive login (invariant 1). The password is
	// never logged and never included in error text.
	Username string
	Password string
}

// Configured reports whether ALL of BaseURL, Username and Password are set —
// the gate for every USER-realm call (login itself needs the credentials;
// without them no user-realm request can even be attempted). Callers use
// this to hide the product rather than sell-then-refund, the same fail-closed
// shape as kirogo.Config.Configured.
func (c Config) Configured() bool {
	return c.BaseURL != "" && c.Username != "" && c.Password != ""
}

// Client is an llmgate user/key-management HTTP client. Safe for concurrent
// use by the bot's 16-worker pool: the only mutable state is the cached
// login token, guarded by tokenMu (login itself runs under that mutex so a
// burst of callers produces ONE login, not sixteen — invariant 1).
type Client struct {
	cfg        Config
	httpClient *http.Client
	logger     *slog.Logger

	// tokenMu guards userToken/userTokenExpiry AND serializes logins (the
	// login HTTP call runs while holding it — that is the mechanism, not
	// an accident: the second caller to arrive blocks until the first has
	// installed a fresh token, then reuses it).
	tokenMu         sync.Mutex
	userToken       string
	userTokenExpiry time.Time

	// tokenPath is the OPTIONAL on-disk persistence file for the login
	// token (NewWithTokenStore; "" = the memory-only behavior of New).
	// Set, it lets the ~30-day token survive a process restart: this
	// client seeds its cache from the file at construction and rewrites
	// it on every successful login. All access to the FILE itself happens
	// under tokenMu too (writes come only from loginLocked, the load runs
	// before the client is shared), so two goroutines can never interleave
	// a half-written read with a rename. See NewWithTokenStore for the
	// full contract; the token VALUE never appears in any log line
	// (invariant 2).
	tokenPath string
}

// perAttemptTimeout bounds each individual HTTP attempt, matching the 20s
// the bot uses for its other upstream clients (kirogo.perAttemptTimeout).
const perAttemptTimeout = 20 * time.Second

// maxAttempts is the retry ceiling for 5xx/network failures (invariant 6).
const maxAttempts = 3

// retryBackoffBase is the linear backoff unit between attempts (attempt n
// waits n*base — mirrors kirogo/telegram_bot.py). A package-level var, not
// a const, purely so tests can shrink it and keep retry-path tests fast;
// production code never reassigns it.
var retryBackoffBase = 1 * time.Second

// tokenRefreshMargin is how early, before userTokenExpiry, a cached token is
// considered stale and proactively re-logged-in. 60s comfortably covers
// clock skew between this host and the llmgate origin, so a token is never
// sent in its final minute where the server might already reject it (the
// reactive 401 path of invariant 1 remains as the backstop).
const tokenRefreshMargin = 60 * time.Second

// loginExpireDefault is the "expire" field sent on every login: -1 tells
// llmgate to issue its default long-lived token (~30 days, per the verified
// live behavior). The bot never wants short-lived user tokens — it logs in
// lazily and caches, so anything shorter just multiplies logins.
const loginExpireDefault = -1

// fallbackTokenTTL is assumed when a login response's expire_at is missing
// or unparseable. Deliberately SHORT (the server's real tokens live ~30
// days): an under-guess only costs an extra login (independent logins do
// not invalidate each other — invariant 1), while an over-guess would ride
// a dead token until a reactive 401. The reactive path bounds either
// direction, so this only tunes login frequency.
const fallbackTokenTTL = 1 * time.Hour

// New builds a Client. Pass nil for logger to use slog.Default(). The
// configured BaseURL's trailing slashes are trimmed HERE (not per-request)
// so the whole client holds one canonical base; path constants below all
// start with '/' and concatenation can never produce a doubled slash.
func New(cfg Config, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.BaseURL = trimTrailingSlashes(cfg.BaseURL)
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: perAttemptTimeout},
		logger:     logger,
	}
}

// NewWithTokenStore builds the SAME client as New plus an optional on-disk
// login-token store at path. MOTIVATION: New caches the ~30-day login token
// in memory only, so the FIRST mint after every bot restart must login — and
// one transient failure on that single login (a live Cloudflare 522 was
// observed) fails the mint and auto-refunds a paid order. A persisted token
// removes the restart-login window entirely: the client comes up already
// authenticated.
//
// Contract:
//   - path == "" is EXACTLY New's behavior (pure memory, no file I/O) — the
//     knob is opt-in so nothing changes for callers that don't ask for it.
//   - On construction the file is read BEST-EFFORT: a well-formed entry whose
//     expire_at is still more than tokenRefreshMargin (1min) away seeds the
//     in-memory cache, skipping the first login entirely. An absent, corrupt,
//     unparseable, or within-margin-stale file is silently ignored — the
//     client just starts fresh and logins on first use like New would.
//   - On every successful login the file is rewritten ({token, expire_at},
//     JSON, mode 0600, tmp-file + rename so a reader never sees a torn
//     write). A write FAILURE only logs a Warn: the in-memory token this
//     process just obtained is unaffected, so a read-only disk degrades to
//     New's memory-only behavior instead of failing the login.
//
// SECURITY (invariant 2 extended to disk): the file holds the raw JWT, so it
// is written 0600 and its path is operator-provided (handlers:
// LLMGATE_TOKEN_STORE); the token VALUE itself is never logged — error text
// from the os package carries the PATH, not the contents.
func NewWithTokenStore(cfg Config, logger *slog.Logger, path string) *Client {
	c := New(cfg, logger)
	c.tokenPath = path
	c.loadStoredToken()
	return c
}

// Configured reports whether this client can make USER-realm calls — thin
// passthrough to Config.Configured so callers holding a *Client don't need
// to reach into its private cfg.
func (c *Client) Configured() bool {
	return c.cfg.Configured()
}

// Sentinel errors. ErrNotConfigured and the argument guards are returned
// BEFORE any HTTP call — a misconfigured or malformed request must fail
// cheaply and locally, never burn a round trip (same fail-shape as kirogo).
var (
	// ErrNotConfigured is returned when a call needs credentials or a base
	// URL the client doesn't have.
	ErrNotConfigured = errors.New("llmgate: not configured (base URL / credentials missing)")
	// ErrNotFound maps HTTP 404 (invariant 6): the id the caller acted on
	// does not exist server-side (already deleted, wrong account, ...).
	ErrNotFound = errors.New("llmgate: not found")
	// ErrAuthFailed is the terminal user-realm auth failure: the request
	// was 401'd, one forced re-login + retry was made, and the retry was
	// 401'd again (invariant 1) — or the re-login itself was rejected.
	// Retrying further cannot succeed; an operator must look at the
	// credentials.
	ErrAuthFailed = errors.New("llmgate: authentication failed")
	// ErrIDRequired guards every endpoint whose target is a numeric key
	// id (update/delete/reveal): id <= 0 can never name a real key.
	ErrIDRequired = errors.New("llmgate: key id is required (> 0)")
	// ErrCreditLimitNegative guards create/update: a negative credit
	// limit is meaningless on the wire (0 means unlimited); rejecting it
	// client-side turns a typo into a local error instead of a server 400.
	ErrCreditLimitNegative = errors.New("llmgate: credit limit must be >= 0")
	// errCustomerKeyRequired guards KeyStats: the key realm authenticates
	// WITH the customer key (invariant 1), so an empty one means the call
	// cannot be attempted at all.
	errCustomerKeyRequired = errors.New("llmgate: customer key is required")
)

// APIError wraps a non-2xx response, or a 2xx whose body code != 200
// (invariant 6). StatusCode lets callers branch on 401/404/400/5xx without
// string-matching Message. Message is server-supplied text and has already
// been through redactSecrets (invariant 7) — it is safe to log.
type APIError struct {
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llmgate: %s rejected (HTTP %d): %s", e.Path, e.StatusCode, e.Message)
}

// wireResponse is the envelope on EVERY llmgate response (package doc).
// Data stays RawMessage so send can distinguish "absent/null data on 2xx =
// success" (invariant 6) from "decode the payload" — and so a delete's
// missing data field never trips a decode.
type wireResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// storedToken is the JSON shape of the on-disk token store (NewWithTokenStore)
// — the login response's two load-bearing fields, nothing else: the JWT
// itself and its expiry. expire_at is RFC3339 for symmetry with the login
// wire format (the value round-trips time.Parse(time.RFC3339) unchanged).
type storedToken struct {
	Token    string `json:"token"`
	ExpireAt string `json:"expire_at"`
}

// secretShapeRe matches the two secret shapes this API traffic in: the
// upstream key "sk-llmgate-<48 chars>" and the login JWT
// "eyJ<hdr>.<payload>.<sig>". The {8,}/+ floors keep the patterns from ever
// firing on a short unrelated token that merely starts with those
// characters, and matching runs the full key character class because a key
// (unlike a password) contains no spaces — the class boundary IS the secret
// boundary and the surrounding message stays readable.
var (
	llmgateKeyShapeRe = regexp.MustCompile(`(sk-llmgate-)[A-Za-z0-9_-]{8,}`)
	jwtShapeRe        = regexp.MustCompile(`(eyJ)[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
)

// redactSecrets replaces any upstream-key or JWT shape embedded in
// server-supplied text with its prefix and a placeholder (invariant 7).
// Idempotent: neither replacement contains a sequence its own (or the
// other) pattern can match — "<" is outside both character classes — so
// repeated passes are stable.
func redactSecrets(s string) string {
	s = llmgateKeyShapeRe.ReplaceAllString(s, "${1}<redacted>")
	return jwtShapeRe.ReplaceAllString(s, "${1}<redacted>")
}

// trimTrailingSlashes strips trailing '/' from a configured base URL so
// base+path concatenation never produces a doubled slash.
func trimTrailingSlashes(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// parseErrorMessage extracts the wrapper's "message" from a non-2xx body,
// falling back to a generic "HTTP <code>" when the body isn't the expected
// shape (e.g. a Cloudflare HTML error page). The text is redacted before it
// is returned: this is the one place a SERVER-controlled string enters
// APIError.Message, which Error() renders into any %v — and both realms
// authenticate with secrets a 401 body could plausibly echo (invariant 7).
func parseErrorMessage(body []byte, statusCode int) string {
	var wrap wireResponse
	if json.Unmarshal(body, &wrap) == nil && wrap.Message != "" {
		return redactSecrets(wrap.Message)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// translateAPIError converts an *APIError into the sentinel-shaped error
// callers branch on (invariant 6): 404 becomes a wrapped ErrNotFound so
// handlers can errors.Is it without caring whether the wrapper code came
// from the HTTP status or the body. Everything else passes through as the
// plain *APIError.
func translateAPIError(path string, apiErr *APIError) error {
	if apiErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return apiErr
}

// IsAmbiguous reports whether err leaves the server's action UNKNOWN —
// true for transport/timeout failures and 5xx-exhausted *APIErrors
// (outcomes where the request may have been COMMITTED before the
// connection died), false for definitive outcomes (any 4xx including 401/
// 404, ErrAuthFailed, and the pre-flight guard errors where no request was
// ever sent). The mint path branches on exactly this: only an AMBIGUOUS
// create failure is worth a list+reveal reconciliation round trip, because
// only there can a committed-but-unanswered mint exist to recover (and the
// reconciliation itself never re-sends the create, so it can never
// double-mint).
func IsAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		// 5xx-exhausted (or a single-attempt 5xx): the server's fate for the
		// request is unknowable. Every 4xx — including 401 (rejected) and 404
		// (nothing was there) — is a definitive answer.
		return apiErr.StatusCode >= 500
	}
	// Definitive sentinels: ErrNotConfigured/ErrIDRequired/
	// ErrCreditLimitNegative/errCustomerKeyRequired mean NO request was
	// sent; ErrAuthFailed means the server definitively 401'd the attempt
	// (twice); ErrNotFound is a definitive 404. None can have minted
	// anything. Everything else reaching here — network/timeout wrappers,
	// response-decode failures — leaves the outcome unknown: a 2xx whose
	// body we could not decode may well have COMMITTED the action.
	if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrIDRequired) ||
		errors.Is(err, ErrCreditLimitNegative) || errors.Is(err, ErrAuthFailed) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, errCustomerKeyRequired) {
		return false
	}
	return true
}

// sleepOrDone waits d, or returns ctx's error early if ctx is cancelled
// first — a caller-cancelled context must abort a retry wait immediately
// instead of blocking the worker for the full backoff.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// send issues one HTTP request, retrying per invariant 6, and decodes the
// 2xx wrapper into out — the standard multi-attempt path (maxAttempts).
// Everything else about the request lives on sendAttempts below; see there
// for the security and retry contracts.
func (c *Client) send(ctx context.Context, method, path string, setHeaders func(*http.Request), payload any, out any) error {
	return c.sendAttempts(ctx, method, path, setHeaders, payload, out, maxAttempts)
}

// sendOnce is sendAttempts with attempts=1 — the MONEY-MINTING path. Create
// (and only create) goes through here: a create retried after a timeout can
// mint a SECOND live key for one paid sale while the first becomes an
// unrecorded billable orphan, so create must never blind-retry. The
// ambiguity this leaves behind (committed but unanswered) is resolved by the
// CALLER's list+reveal reconciliation, never by re-sending the create.
func (c *Client) sendOnce(ctx context.Context, method, path string, setHeaders func(*http.Request), payload any, out any) error {
	return c.sendAttempts(ctx, method, path, setHeaders, payload, out, 1)
}

// sendAttempts is the request engine every llmgate call funnels through:
// one HTTP request per attempt, up to `attempts` total, retrying per
// invariant 6, and decoding the 2xx wrapper into out (out may be nil to
// discard — delete; a present null/absent data with out != nil is also
// success, leaving out zero). setHeaders attaches realm auth to EACH
// attempt's request (the token can differ between userSend's initial call
// and its post-relogin retry).
//
// SECURITY (invariant 2): this method NEVER logs the request or response
// body, the Authorization header, or any decoded value — only path, status
// code and attempt number. Create/reveal bodies carry the cleartext key and
// login bodies carry the JWT; logging them here would be a standing leak
// surface no matter which endpoint called in. Callers that must log a
// result log the non-secret fields off the parsed struct (id, name).
//
// RETRY (invariant 6): network error or 5xx → retry up to `attempts` with
// linear backoff; 4xx → *APIError immediately (no retry can fix a
// rejection); 2xx → decode the wrapper, where code != 200 becomes an
// *APIError and absent/null data is success. 404 (either layer) maps to
// ErrNotFound via translateAPIError. attempts=1 disables retrying entirely
// (see sendOnce for why create needs that).
func (c *Client) sendAttempts(ctx context.Context, method, path string, setHeaders func(*http.Request), payload any, out any, attempts int) error {
	base := c.cfg.BaseURL

	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("llmgate: encode request for %s: %w", path, err)
		}
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		var bodyReader io.Reader
		if encoded != nil {
			bodyReader = bytes.NewReader(encoded)
		}

		req, err := http.NewRequestWithContext(ctx, method, base+path, bodyReader)
		if err != nil {
			return fmt.Errorf("llmgate: build request for %s: %w", path, err)
		}
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		setHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network error / timeout: retryable (invariant 6).
			lastErr = fmt.Errorf("llmgate: %s: %w", path, err)
			c.logger.Warn("llmgate: request failed", "path", path, "attempt", attempt+1)
			if attempt < attempts-1 {
				if waitErr := sleepOrDone(ctx, retryBackoffBase*time.Duration(attempt+1)); waitErr != nil {
					return waitErr
				}
				continue
			}
			if attempts > 1 {
				return fmt.Errorf("llmgate: %s failed after %d attempts: %w", path, attempts, lastErr)
			}
			return lastErr
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("llmgate: read response for %s: %w", path, readErr)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// An utterly empty body carries no wrapper to check and no
			// data to decode — treat as success with nothing decoded
			// (the null-tolerant reading of invariant 6; a body-less 204
			// from a proxy must not fail a call the server accepted).
			if len(bytes.TrimSpace(respBody)) == 0 {
				return nil
			}
			var wrap wireResponse
			if err := json.Unmarshal(respBody, &wrap); err != nil {
				return fmt.Errorf("llmgate: decode response for %s: %w", path, err)
			}
			if wrap.Code != 200 {
				// 2xx transport but the business code says otherwise —
				// the server guarantees HTTP status matches code, so
				// prefer the body code when present (HTTP status as the
				// fallback keeps the field non-zero either way).
				status := wrap.Code
				if status == 0 {
					status = resp.StatusCode
				}
				return translateAPIError(path, &APIError{
					Path:       path,
					StatusCode: status,
					Message:    redactSecrets(wrap.Message),
				})
			}
			if out != nil && len(wrap.Data) > 0 && !bytes.Equal(bytes.TrimSpace(wrap.Data), []byte("null")) {
				if err := json.Unmarshal(wrap.Data, out); err != nil {
					return fmt.Errorf("llmgate: decode data for %s: %w", path, err)
				}
			}
			return nil
		}

		apiErr := &APIError{Path: path, StatusCode: resp.StatusCode, Message: parseErrorMessage(respBody, resp.StatusCode)}

		if resp.StatusCode >= 500 {
			// 5xx: retryable (invariant 6).
			lastErr = apiErr
			c.logger.Warn("llmgate: server error, retrying", "path", path, "status", resp.StatusCode, "attempt", attempt+1)
			if attempt < attempts-1 {
				if waitErr := sleepOrDone(ctx, retryBackoffBase*time.Duration(attempt+1)); waitErr != nil {
					return waitErr
				}
				continue
			}
			return apiErr
		}

		// 4xx: a real rejection. NEVER retried (invariant 6) — 404 maps
		// to ErrNotFound, everything else surfaces as the *APIError.
		c.logger.Warn("llmgate: request rejected", "path", path, "status", resp.StatusCode)
		return translateAPIError(path, apiErr)
	}
	return lastErr
}

// bearer returns the header setter for "Authorization: Bearer <token>",
// used by both realms (user JWT, customer key) — the token VALUE differs,
// the header shape does not.
func bearer(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// isUnauthorized reports whether err is an *APIError with a 401 status —
// the one condition userSend reacts to with a re-login (invariant 1).
func isUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized
}

// userSend issues a USER-realm request (Bearer JWT, invariant 1): obtains
// a cached-or-fresh token, sends, and on a 401 does exactly ONE forced
// re-login + retry. A second 401 returns ErrAuthFailed — retrying again
// would just hammer a credentials problem (the re-login already proved the
// newest possible token doesn't help). Standard multi-attempt retrying
// (maxAttempts); the single-attempt variant is userSendOnce.
func (c *Client) userSend(ctx context.Context, method, path string, payload any, out any) error {
	return c.userSendAttempts(ctx, method, path, payload, out, maxAttempts)
}

// userSendOnce is userSend with the underlying send capped at ONE attempt —
// the create path's money guard (see sendOnce): the 401 re-login retry is
// KEPT (a 401 is a definitive rejection, so replaying after re-login can
// never double-mint), but no 5xx/network retry of the create itself.
func (c *Client) userSendOnce(ctx context.Context, method, path string, payload any, out any) error {
	return c.userSendAttempts(ctx, method, path, payload, out, 1)
}

// userSendAttempts is userSend's body parameterized by the per-send attempt
// ceiling (maxAttempts everywhere except create's 1 — see sendOnce).
func (c *Client) userSendAttempts(ctx context.Context, method, path string, payload any, out any, attempts int) error {
	if !c.cfg.Configured() {
		return ErrNotConfigured
	}

	tok, err := c.authToken(ctx)
	if err != nil {
		return err
	}

	err = c.sendAttempts(ctx, method, path, bearer(tok), payload, out, attempts)
	if !isUnauthorized(err) {
		return err
	}

	// 401 in the user realm = the token went stale (invariant 1). Force a
	// re-login — authToken might have returned a cached token that was
	// revoked server-side early, so the cache is invalidated outright, not
	// consulted.
	c.tokenMu.Lock()
	c.userToken, c.userTokenExpiry = "", time.Time{}
	tok, loginErr := c.loginLocked(ctx)
	c.tokenMu.Unlock()
	if loginErr != nil {
		// A 401 from LOGIN itself is bad credentials, not a stale token —
		// the definitive ErrAuthFailed. Anything else (network, 5xx after
		// retries) surfaces as-is; it is not an auth verdict.
		if isUnauthorized(loginErr) {
			return fmt.Errorf("%w: re-login rejected: %w", ErrAuthFailed, loginErr)
		}
		return loginErr
	}

	err = c.sendAttempts(ctx, method, path, bearer(tok), payload, out, attempts)
	if isUnauthorized(err) {
		return fmt.Errorf("%w: %s still rejected after one re-login", ErrAuthFailed, path)
	}
	return err
}

// authToken returns a usable user-realm token, logging in if needed. The
// mutex is held across the whole call INCLUDING any login HTTP round trip:
// that is what makes a burst of concurrent callers share one login (the
// second caller blocks until the first has installed the token, then finds
// it cached) instead of racing sixteen logins — invariant 1.
func (c *Client) authToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Proactive freshness: a token inside its final tokenRefreshMargin is
	// treated as already expired (invariant 1's margin) so it is never sent
	// in the window where server clock skew could reject it.
	if c.userToken != "" && time.Now().Before(c.userTokenExpiry.Add(-tokenRefreshMargin)) {
		return c.userToken, nil
	}
	return c.loginLocked(ctx)
}

// loginLocked performs one login round trip and installs the new token into
// the cache. Caller must hold tokenMu (authToken and userSend's 401 path
// both serialize through it).
func (c *Client) loginLocked(ctx context.Context) (string, error) {
	var data struct {
		Token    string `json:"token"`
		ExpireAt string `json:"expire_at"`
	}
	// login is the ONE request that carries no Authorization header — it
	// is how the token is obtained, so there is nothing to present yet
	// (the credentials ride the JSON body instead).
	err := c.send(ctx, http.MethodPost, "/api/v1/user/login", func(*http.Request) {}, struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Expire   int    `json:"expire"`
	}{c.cfg.Username, c.cfg.Password, loginExpireDefault}, &data)
	if err != nil {
		return "", err
	}
	if data.Token == "" {
		// A 200/code-200 wrapper without a token is a server contract
		// break; treating it as success would cache an empty token and
		// 401 every subsequent call.
		return "", fmt.Errorf("llmgate: login for %s returned no token", "/api/v1/user/login")
	}

	// expire_at is RFC3339 with non-UTC offsets (+07:00 observed). Go's
	// RFC3339 parse accepts those and any fractional seconds natively; a
	// parse failure falls back to the conservative short TTL (see
	// fallbackTokenTTL for why short is the safe direction).
	expiry, parseErr := time.Parse(time.RFC3339, data.ExpireAt)
	if parseErr != nil {
		c.logger.Warn("llmgate: unparseable expire_at, using fallback token TTL", "error", parseErr.Error())
		expiry = time.Now().Add(fallbackTokenTTL)
	}

	c.userToken, c.userTokenExpiry = data.Token, expiry

	// Persist FIRST, before returning success: this is the only install
	// point for the in-memory cache too (both authToken's lazy login and
	// userSend's 401 re-login funnel here), so hooking the store here keeps
	// disk and memory from ever disagreeing about which token is current.
	// Best-effort by contract — a write failure is logged inside and never
	// fails the login (see saveTokenLocked).
	c.saveTokenLocked()
	return c.userToken, nil
}

// saveTokenLocked writes the current token cache to tokenPath (tmp file +
// rename, 0600). Caller must hold tokenMu (loginLocked is the only caller,
// which is what serializes writers). BEST-EFFORT: any failure — unwritable
// directory, full disk, a path that names a directory — logs ONE Warn and
// returns; the in-memory token the caller just installed keeps working for
// this process, so a broken store degrades to New's memory-only behavior
// rather than breaking logins. SECURITY: the error attr is the os error,
// which carries the PATH and errno but never the token bytes; the token
// itself is only ever written into the 0600 file (invariant 2).
func (c *Client) saveTokenLocked() {
	if c.tokenPath == "" {
		return
	}
	blob, err := json.Marshal(storedToken{
		Token:    c.userToken,
		ExpireAt: c.userTokenExpiry.UTC().Format(time.RFC3339),
	})
	if err != nil {
		// Unreachable in practice (two strings marshal); kept for the same
		// best-effort shape as the I/O failures below.
		c.logger.Warn("llmgate: token store encode failed", "error", err.Error())
		return
	}
	// tmp + rename = atomic publish. The tmp sits BESIDE the final path
	// (same directory) so rename(2) never crosses a filesystem boundary
	// and a concurrent reader sees either the old file or the new one,
	// never a torn write. Mode is set on the tmp and survives the rename.
	tmp := c.tokenPath + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		c.logger.Warn("llmgate: token store write failed", "error", err.Error())
		return
	}
	if err := os.Rename(tmp, c.tokenPath); err != nil {
		c.logger.Warn("llmgate: token store publish failed", "error", err.Error())
	}
}

// loadStoredToken seeds the in-memory token cache from tokenPath. Called
// only from NewWithTokenStore, before the client is shared with any
// goroutine, so no lock is needed. EVERY failure mode — absent file,
// unreadable, corrupt JSON, missing token, unparseable expiry — returns
// silently: the client simply starts fresh and logins on first use, exactly
// like New, so a damaged store can never wedge startup (invariant: the
// store is an optimization, not a dependency). A structurally valid entry
// is used ONLY when its expiry is still more than tokenRefreshMargin away —
// the same freshness bar authToken applies to the in-memory cache — so a
// restored token is never sent inside the final-minute window where server
// clock skew could 401 it.
func (c *Client) loadStoredToken() {
	if c.tokenPath == "" {
		return
	}
	blob, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return // absent/unreadable: start fresh
	}
	var st storedToken
	if json.Unmarshal(blob, &st) != nil || st.Token == "" {
		return // corrupt or tokenless: start fresh
	}
	expiry, err := time.Parse(time.RFC3339, st.ExpireAt)
	if err != nil {
		return // unparseable expiry: start fresh
	}
	if !time.Now().Before(expiry.Add(-tokenRefreshMargin)) {
		return // expired or inside the final margin: start fresh
	}
	c.userToken, c.userTokenExpiry = st.Token, expiry
}
