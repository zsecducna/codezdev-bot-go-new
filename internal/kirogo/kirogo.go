// Package kirogo is the Go port of the Kiro-Go admin/customer HTTP client.
//
// Python original: kiro_provision.py:421-470 (_kiro_go_admin_post /
// kiro_go_add_key / kiro_go_add_account) plus the richer client that grew up
// inside telegram_bot.py (kiro_go_post:5847, kiro_go_configured:5816,
// kiro_go_configured_base:5829, kiro_go_delete_key:6070,
// kiro_go_recharge_key:6119, the provision_customer call sites at
// telegram_bot.py:5516 (bedrock/verify), 7489-7516 (both mint classes)).
// Full wire contract: /Users/z/Desktop/Kiro/KIRO_GO_API.md (line numbers
// cited per-endpoint below).
//
// This package intentionally covers ONLY the pure-HTTP admin/customer surface
// documented in KIRO_GO_API.md — supply-side pool provisioning
// (/admin/add_kiro_api_key, /admin/add_kiro_account, /admin/pool) belongs to
// internal/provision per architecture-go.md §1/§6 and is out of scope here.
//
// Hard domain invariants encoded below (repo CLAUDE.md "Domain invariants" +
// architecture-go.md §6 "internal/kirogo invariants" — every one is load-
// bearing, not a style preference):
//
//  1. provision_customer upstreamStatus=="pending" on a 2xx response IS
//     SUCCESS for the bedrock class: the pool key was created and is
//     billable, deliver it, no retry (would mint a second paid key for one
//     sale), no refund. Only a non-2xx response is a real failure. See
//     ProvisionCustomerResponse.Deliverable.
//  2. The response types below carry NO field for an upstream llmgate key
//     (only the non-secret UpstreamKeyID) — surfacing one would expose the
//     whole account's aggregate quota, not just one customer's. This client
//     also never logs request/response bodies (see send): only path, status
//     code, and non-secret ids ever reach the logger.
//  3. accountId is always sent explicitly on a bedrock/legacy
//     ProvisionCustomer call (falls back to the client's configured account
//     id, never silently omitted); Credits must be > 0 (checked client-side,
//     before any HTTP call); tokenLimit is hardcoded to 0 in the wire
//     payload and is not a caller-settable field.
//
// Invariant 3b, TWO MINT CLASSES through provision_customer
// (architecture-go.md §6), is a named sub-rule of #3 above, deliberately
// kept as one prose paragraph rather than its own numbered or bulleted
// item: gofmt's doc-comment formatter only recognizes plain sequential
// digits as list markers (not a letter suffix like "3b"), and even a
// "-"-bulleted sub-list here got its own "+"-prefixed wrapped line read
// back as a THIRD bullet — either shape silently re-mangles on the next
// gofmt -w. Bedrock (SaleClassBedrock, or "" legacy) REQUIRES accountId
// (rule 3 above) and is gated on the sell side by Configured() (base URL,
// admin key, and account id all set). Kiro (SaleClassKiro) sends
// saleClass:"kiro", skipUpstream:true (hardcoded — not caller-controlled,
// same treatment as tokenLimit) and NO accountId field on the wire at all
// — an explicit AccountID on a kiro request is rejected client-side
// (ErrAccountIDForbiddenForKiro) rather than silently dropped, since
// silently dropping a caller's explicit value is its own bug class. Kiro
// is gated on the sell side by ConfiguredBase() (base URL and admin key
// only) — requiring an account id here would hide a fully sellable
// product behind a setting its mints never read (telegram_bot.py:5829-5836).
//
//  4. -1 in any *Remaining field means unlimited, with the corresponding
//     limit at zero — modeled as the Remaining int64 type with IsUnlimited().
//  5. DeleteAPIKey's upstreamRevoked is tri-state: absent (old server) is NOT
//     the same as an explicit false. An explicit false means the upstream
//     (llmgate) key is still live and spending — DeleteAPIKeyResponse reports
//     that via UpstreamStillLive() (a loud alert condition) while the delete
//     itself still counts as successful (the pool key, and the customer's
//     access, is dead either way).
//  6. Configured() is true only when BaseURL, AdminKey AND AccountID are all
//     set — the bedrock product must hide rather than sell-then-auto-refund
//     (KIRO_GO_API.md:33; architecture-go.md §6). ConfiguredBase() (BaseURL +
//     AdminKey only) is the weaker gate the kiro-class product uses instead.
//  7. Retry policy (architecture-go.md §6, telegram_bot.py:7461-7468): retry
//     ONLY on 5xx responses or network/timeout errors, up to maxAttempts,
//     with linear backoff. NEVER retry a 4xx (it is a real rejection, not a
//     transient failure, and retrying it can't succeed). NEVER retry a 2xx —
//     "pending"/"skipped" upstream statuses are terminal successes, not a
//     reason to retry the mint (that would double-mint a paid key). A
//     request that exhausts all retries on 5xx/network errors surfaces the
//     last error to the caller, who accepts the same committed-but-
//     unanswered double-mint window Python does rather than re-minting on
//     ambiguity.
//
// Auth (KIRO_GO_API.md:6-13): admin calls carry Authorization: Bearer
// <adminKey> — Kiro-Go also accepts an X-Admin-Password header, but the
// Python bot sends Bearer (telegram_bot.py:5873, kiro_provision.py:432) and
// this port matches it exactly (architecture-go.md §6). Customer calls carry
// X-Api-Key. Admin auth fails closed with 401 on any mismatch, no cookie
// fallback.
package kirogo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

// Config is the Kiro-Go connection configuration the bot pins from env /
// settings.json (telegram_bot.py: KIRO_GO_BASE_URL / KIRO_GO_ADMIN_KEY /
// KIRO_GO_ACCOUNT_ID, see kiro_go_account_id:5356, kiro_go_configured:5816).
// internal/config owns loading these; this package only consumes them.
type Config struct {
	// BaseURL is the Kiro-Go proxy root, e.g. https://codezdev-shop.uk:8080.
	// No trailing slash required — trimmed defensively in send.
	BaseURL string
	// AdminKey is the admin password sent as "Authorization: Bearer
	// <AdminKey>" (KIRO_GO_API.md:8-11; telegram_bot.py:5873). Never logged,
	// never included in error text.
	AdminKey string
	// AccountID is the llmgate-holding account pinned into every bedrock/
	// legacy ProvisionCustomer call's accountId field (KIRO_GO_API.md:28-33).
	// NOT used, and must not be sent, for kiro-class mints (invariant 3b).
	AccountID string
}

// Configured reports whether ALL three of BaseURL, AdminKey and AccountID are
// set — invariant 6, the gate for the BEDROCK product. Callers must use
// this, not a partial check, so a bedrock mint never fails AFTER the
// customer has already paid (architecture-go.md §6; mirrors Python
// kiro_go_configured:5816).
func (c Config) Configured() bool {
	return c.BaseURL != "" && c.AdminKey != "" && c.AccountID != ""
}

// ConfiguredBase reports whether BaseURL and AdminKey are set, WITHOUT
// requiring AccountID — the gate for the KIRO-class product (invariant 3b)
// and the minimum needed to make any admin call at all. Kiro mints send no
// accountId, so gating them on Configured() would hide a fully sellable
// product behind a setting their mints never read (mirrors Python
// kiro_go_configured_base:5829-5836).
func (c Config) ConfiguredBase() bool {
	return c.BaseURL != "" && c.AdminKey != ""
}

// Client is a Kiro-Go admin/customer HTTP client. Safe for concurrent use
// (holds no mutable state beyond an *http.Client, which is itself safe for
// concurrent use).
type Client struct {
	cfg        Config
	httpClient *http.Client
	logger     *slog.Logger
}

// perAttemptTimeout bounds each individual HTTP attempt (mirrors the
// Python client's mint-path timeout, kiro_go_post timeout=20 default,
// telegram_bot.py:5847 docstring).
const perAttemptTimeout = 20 * time.Second

// maxAttempts is the retry ceiling for 5xx/network failures (invariant 7;
// mirrors kiro_go_post's default attempts=3, telegram_bot.py:5847).
const maxAttempts = 3

// retryBackoffBase is the base backoff between retry attempts, multiplied by
// the attempt number — mirrors the Python client's time.sleep(1.0*(attempt+1))
// (telegram_bot.py:5883). A package-level var (not const) purely so tests can
// shrink it and keep the retry-path tests fast; production code never
// reassigns it.
var retryBackoffBase = 1 * time.Second

// New builds a Client. Pass nil for logger to use slog.Default().
func New(cfg Config, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: perAttemptTimeout},
		logger:     logger,
	}
}

// Configured reports whether this client is fully configured to sell
// BEDROCK keys (invariant 6). Thin passthrough to Config.Configured so
// callers holding a *Client don't need to reach into its private cfg.
func (c *Client) Configured() bool {
	return c.cfg.Configured()
}

// ConfiguredBase reports whether this client can sell KIRO-class keys
// (invariant 3b/6 — base URL + admin key only, no account id required).
func (c *Client) ConfiguredBase() bool {
	return c.cfg.ConfiguredBase()
}

// Sentinel errors returned BEFORE any HTTP call — client-side validation per
// invariant 3 and the "fail at the menu, not at the mint" architecture goal.
var (
	// ErrNotConfigured is returned when BaseURL or AdminKey is missing — no
	// admin call can be attempted at all.
	ErrNotConfigured = errors.New("kirogo: not configured (base URL / admin key missing)")
	// ErrAccountIDRequired is returned by a bedrock/legacy ProvisionCustomer
	// call when no account id is available from either the request or the
	// client's Config — invariant 3 ("accountId always sent explicitly").
	ErrAccountIDRequired = errors.New("kirogo: accountId is required (configure Config.AccountID or pass one explicitly)")
	// ErrAccountIDForbiddenForKiro is returned when a kiro-class
	// ProvisionCustomer request carries an explicit AccountID. Kiro mints
	// never send accountId (invariant 3b) — rejecting it client-side beats
	// silently dropping a caller's explicit value, which would be its own
	// class of bug (the caller would reasonably assume it was honored).
	ErrAccountIDForbiddenForKiro = errors.New("kirogo: accountId must not be set for a kiro-class mint")
	// ErrCreditsMustBePositive guards every endpoint that mints or recharges
	// credits (KIRO_GO_API.md: "credits must be > 0" on provision_customer/
	// new_api_key, "amounts ... > 0" on recharge_api_key).
	ErrCreditsMustBePositive = errors.New("kirogo: credits must be > 0")
	// ErrTokenLimitNegative guards new_api_key's optional tokenLimit
	// (KIRO_GO_API.md:59, "tokenLimit (optional, >= 0)").
	ErrTokenLimitNegative = errors.New("kirogo: tokenLimit must be >= 0")
	// ErrKeyIdentifierRequired guards RechargeKey/DeleteAPIKey, both of which
	// require an id OR an apiKey (KIRO_GO_API.md:72, 95).
	ErrKeyIdentifierRequired = errors.New("kirogo: id or apiKey is required")
	// ErrUnknownSaleClass guards the SaleClass field on ProvisionCustomer and
	// RechargeKey requests — only "" (legacy/omit), SaleClassBedrock and
	// SaleClassKiro are meaningful.
	ErrUnknownSaleClass = errors.New("kirogo: unknown sale class (want \"\", \"bedrock\" or \"kiro\")")
)

// APIError wraps a non-2xx JSON error response from Kiro-Go
// ({"error": "..."} per KIRO_GO_API.md throughout). StatusCode lets callers
// distinguish 401 (Unauthorized) / 404 (not found) / 400 (bad request) /
// 5xx without string-matching Message. A 4xx is surfaced immediately
// (never retried — invariant 7); a 5xx is only surfaced this way after
// maxAttempts have been exhausted.
type APIError struct {
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kirogo: %s rejected (HTTP %d): %s", e.Path, e.StatusCode, e.Message)
}

// apiErrorBody is the shape of Kiro-Go's JSON error body on non-2xx
// responses: {"error": "..."} (KIRO_GO_API.md, every Errors: line).
type apiErrorBody struct {
	Error string `json:"error"`
}

// trimTrailingSlashes strips any trailing '/' from a configured base URL so
// concatenating it with a leading-slash path never produces a doubled slash.
func trimTrailingSlashes(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// parseErrorMessage extracts Kiro-Go's {"error": "..."} text from a non-2xx
// body, falling back to a generic "HTTP <code>" when the body isn't the
// expected shape (e.g. an upstream gateway's HTML error page).
// The extracted text is run through redactKeyShapes before it is returned.
// This is the one place a SERVER-controlled string enters APIError.Message,
// which Error() renders into any %v — and customerGet authenticates with the
// customer's cleartext API key, so a 401 body shaped
// {"error":"invalid api key: sk-…"} would put that key into a log line
// (handlers/apikey.go's kiro-go stats failure path). Whether Kiro-Go actually
// echoes a rejected key is not knowable from this repo; the redaction costs
// one regexp pass and makes the answer irrelevant.
func parseErrorMessage(body []byte, statusCode int) string {
	var eb apiErrorBody
	if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
		return redactKeyShapes(eb.Error)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// keyShapeRe matches the two API-key shapes this system mints: the customer
// key "sk-<base62>" and the upstream pool key "ksk_<base62>".
//
// The {8,} floor exists so the redaction can never fire on a short unrelated
// token that merely starts with those characters, and — more importantly — so
// a partial match can never leave a usable prefix behind. Matching runs to the
// end of the key's character class rather than to end-of-line, because unlike
// a password (provision/parse.go:107-108) a key has no spaces, so the class
// boundary IS the key boundary and the surrounding message stays readable.
var keyShapeRe = regexp.MustCompile(`(sk-|ksk_)[A-Za-z0-9_-]{8,}`)

// redactKeyShapes replaces any API key embedded in server-supplied text with
// its prefix and a placeholder. Idempotent: the replacement contains no
// character sequence the pattern matches.
func redactKeyShapes(s string) string {
	return keyShapeRe.ReplaceAllString(s, "${1}<redacted>")
}

// sleepOrDone waits d, or returns ctx's error early if ctx is cancelled
// first — so a caller-cancelled context aborts a retry wait immediately
// instead of blocking the caller for the full backoff.
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

// send issues one HTTP request, retrying per invariant 7, and decodes the
// 2xx JSON body into out (out may be nil to discard the body). setHeaders
// attaches realm-specific auth (Bearer admin key, or X-Api-Key) to each
// attempt's request.
//
// SECURITY (invariant 2): this method NEVER logs the request or response
// body — only the path and, on failure, the status code. Every mint endpoint
// (ProvisionCustomer, NewAPIKey) returns a cleartext key in its body, and even
// non-mint bodies may carry customer-identifying data; logging them here
// would be a standing leak surface regardless of which specific endpoint is
// called. Callers that need to log a successful mint must log ONLY the
// non-secret id/name fields off the parsed struct, never the raw body.
//
// RETRY (invariant 7): a network error or a 5xx response is retried up to
// maxAttempts times with linear backoff (mirrors kiro_go_post,
// telegram_bot.py:5847-5886). A 4xx is returned immediately — it is a real
// rejection (bad payload / unauthorized / not found) that a retry can never
// fix. A 2xx is likewise returned immediately on the first attempt: this
// method does not, and must not, inspect a successful body's contents (e.g.
// upstreamStatus) to decide whether to retry — that decision belongs
// entirely to the caller via the response type's own methods (see
// ProvisionCustomerResponse.Deliverable).
func (c *Client) send(ctx context.Context, method, path string, setHeaders func(*http.Request), payload any, out any) error {
	base := trimTrailingSlashes(c.cfg.BaseURL)

	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("kirogo: encode request for %s: %w", path, err)
		}
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var bodyReader io.Reader
		if encoded != nil {
			bodyReader = bytes.NewReader(encoded)
		}

		req, err := http.NewRequestWithContext(ctx, method, base+path, bodyReader)
		if err != nil {
			return fmt.Errorf("kirogo: build request for %s: %w", path, err)
		}
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		setHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network error / timeout: retryable (invariant 7).
			lastErr = fmt.Errorf("kirogo: %s: %w", path, err)
			c.logger.Warn("kirogo: request failed", "path", path, "attempt", attempt+1)
			if attempt < maxAttempts-1 {
				if waitErr := sleepOrDone(ctx, retryBackoffBase*time.Duration(attempt+1)); waitErr != nil {
					return waitErr
				}
				continue
			}
			return fmt.Errorf("kirogo: %s failed after %d attempts: %w", path, maxAttempts, lastErr)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("kirogo: read response for %s: %w", path, readErr)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("kirogo: decode response for %s: %w", path, err)
				}
			}
			return nil
		}

		apiErr := &APIError{Path: path, StatusCode: resp.StatusCode, Message: parseErrorMessage(respBody, resp.StatusCode)}

		if resp.StatusCode >= 500 {
			// 5xx: retryable (invariant 7).
			lastErr = apiErr
			c.logger.Warn("kirogo: server error, retrying", "path", path, "status", resp.StatusCode, "attempt", attempt+1)
			if attempt < maxAttempts-1 {
				if waitErr := sleepOrDone(ctx, retryBackoffBase*time.Duration(attempt+1)); waitErr != nil {
					return waitErr
				}
				continue
			}
			return apiErr
		}

		// 4xx: a real rejection. NEVER retried (invariant 7) — return at once.
		c.logger.Warn("kirogo: request rejected", "path", path, "status", resp.StatusCode)
		return apiErr
	}
	return lastErr
}

// adminSend issues an admin-realm request (Authorization: Bearer AdminKey).
func (c *Client) adminSend(ctx context.Context, method, path string, payload any, out any) error {
	if !c.cfg.ConfiguredBase() {
		return ErrNotConfigured
	}
	return c.send(ctx, method, path, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AdminKey)
	}, payload, out)
}

// customerGet issues a GET to a customer-realm endpoint (X-Api-Key auth).
// The customer endpoints accept GET and POST with the POST body ignored
// (KIRO_GO_API.md:200), so GET-with-no-body is the simplest faithful choice.
func (c *Client) customerGet(ctx context.Context, path, apiKey string, out any) error {
	if c.cfg.BaseURL == "" {
		return ErrNotConfigured
	}
	if apiKey == "" {
		return errors.New("kirogo: apiKey is required")
	}
	return c.send(ctx, http.MethodGet, path, func(req *http.Request) {
		req.Header.Set("X-Api-Key", apiKey)
	}, nil, out)
}
