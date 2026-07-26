package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── Kiro-Go pool admin client (supply side) ──────────────────────────────────
//
// Python original: _kiro_go_admin_post / kiro_go_add_key / kiro_go_add_account
// (kiro_provision.py:421-467). Wire contract: KIRO_GO_API.md:129-193.
//
// WHY THESE THREE ENDPOINTS LIVE HERE AND NOT IN internal/kirogo:
// architecture-go.md §1 splits the Kiro-Go surface by BUSINESS SIDE, not by
// hostname. internal/kirogo owns the DEMAND side (provision_customer,
// new_api_key, recharge, delete — everything that mints or bills a customer
// key). The three routes below are the SUPPLY side: they push operator-owned
// upstream credentials INTO the serving pool. They share no request/response
// types, no retry policy and no configuration gate with the demand side, so
// folding them into that client would couple two independent contracts.
//
// AUTH: Authorization: Bearer <adminKey>, byte-identical to
// kiro_provision.py:432 and to internal/kirogo's adminSend. KIRO_GO_API.md:8-11
// documents that the server also accepts an `X-Admin-Password` header, but the
// Python bot has always sent Bearer and architecture-go.md §6 pins the port to
// "the same" — so Bearer it is. Sending the other header would be a silent
// behavioural divergence from the module this is a port of.
//
// SECRET HYGIENE (repo CLAUDE.md; KIRO_GO_API.md:46): this client never logs —
// it holds no logger at all. Every request body here carries a live credential
// (a ksk_ key, or an OAuth refresh token), and every response body carries the
// pool ids they were bound to. Errors surface the server's own error TEXT
// only, never the payload.
//
// NO RETRIES, deliberately, matching Python. Both add endpoints de-duplicate
// on the UNDERLYING Kiro account server-side (KIRO_GO_API.md:135, 178), so a
// retry is safe in principle — but the caller's per-account loop already
// treats a failure as retryable by LEAVING THE CREDENTIAL JSON ON DISK
// (kiro_provision.py:48-50), which lets the operator retry without re-running
// a multi-minute browser login. An automatic retry here would only duplicate
// that mechanism while hiding the failure from the operator's report.

// poolTimeout is the per-request budget, matching the Python posters'
// `timeout=30` default (kiro_provision.py:421, 446, 457).
const poolTimeout = 30 * time.Second

// ErrPoolNotConfigured mirrors kiro_provision.py:427 — no base URL or no admin
// key means no admin call can be attempted at all.
var ErrPoolNotConfigured = errors.New("provision: kiro-go not configured (base URL / admin key missing)")

// AddedSlot is one pool slot returned by add_kiro_api_key / add_kiro_account
// (KIRO_GO_API.md:143, 187).
type AddedSlot struct {
	ID     string `json:"id"`
	Region string `json:"region"`
	Email  string `json:"email"`
	// Duplicate reports that the credential was already in the pool — an
	// idempotent retry, distinct from a fresh add (KIRO_GO_API.md:178).
	Duplicate bool `json:"duplicate"`
	// Repaired reports that the slot was BANNED (typically an expired OAuth
	// refresh token) and this upload's fresh credential revived it
	// (KIRO_GO_API.md:179). That is the whole point of re-uploading a dead
	// account, so it must NOT read as a no-op "duplicate" — see summarizeAdd.
	Repaired bool `json:"repaired"`
}

// SkippedSlot is one region the credential did not serve
// (KIRO_GO_API.md:144).
type SkippedSlot struct {
	Region string `json:"region"`
	Error  string `json:"error"`
}

// AddResponse is the shared 200 body of both add endpoints
// (KIRO_GO_API.md:139-146, 183-190).
type AddResponse struct {
	Success bool          `json:"success"`
	Enabled bool          `json:"enabled"`
	Added   []AddedSlot   `json:"added"`
	Skipped []SkippedSlot `json:"skipped"`
}

// PoolError is a non-2xx response from a /admin pool endpoint.
//
// Error() reproduces the Python message verbatim —
// `"<endpoint> rejected: <server error text>"` (kiro_provision.py:443) —
// because that string is surfaced to the operator as a per-account result
// detail (kiro_provision.py:796) and the Telegram-side wording depends on it.
type PoolError struct {
	// Endpoint is the last path segment, e.g. "add_kiro_api_key".
	Endpoint   string
	StatusCode int
	// Message is the server's own {"error": ...} text, or "HTTP <code>" when
	// the body isn't that shape. NEVER the request payload.
	Message string
}

func (e *PoolError) Error() string {
	return fmt.Sprintf("%s rejected: %s", e.Endpoint, e.Message)
}

// PoolClient is the seam the orchestrator talks to. An interface (rather than
// the concrete *AdminClient) so the batch tests can drive every add/duplicate/
// repaired/rejected path without a network — the Go equivalent of Python's
// monkeypatching of kp.kiro_go_add_key / kp.kiro_go_add_account.
type PoolClient interface {
	// AddKiroAPIKey adds a ksk_ key to the serving pool
	// (POST /admin/add_kiro_api_key, KIRO_GO_API.md:134-147).
	AddKiroAPIKey(ctx context.Context, kiroAPIKey, nickname, region string) (*AddResponse, error)
	// AddKiroAccount adds a full OAuth account to the pool
	// (POST /admin/add_kiro_account, KIRO_GO_API.md:151-191). `account` is
	// forwarded VERBATIM as raw JSON — see the note on the concrete method.
	AddKiroAccount(ctx context.Context, account json.RawMessage, nickname string) (*AddResponse, error)
}

// AdminClient is the concrete PoolClient over HTTP. Safe for concurrent use.
type AdminClient struct {
	baseURL    string
	adminKey   string
	httpClient *http.Client
}

// NewAdminClient builds a pool admin client. Pass nil for httpClient to get
// one bounded by poolTimeout.
func NewAdminClient(baseURL, adminKey string, httpClient *http.Client) *AdminClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: poolTimeout}
	}
	return &AdminClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		adminKey:   adminKey,
		httpClient: httpClient,
	}
}

// Configured reports whether a pool call can be attempted at all: base URL +
// admin key, and deliberately NOT an account id.
//
// This is the restocking gate _provpool_preflight uses (telegram_bot.py:5586,
// with its comment spelled out): the sell-side kiro_go_configured() gate also
// requires KIRO_GO_ACCOUNT_ID, which restocking has nothing to do with — it
// only hits /admin/add_kiro_api_key and /admin/add_kiro_account. Reusing the
// sell-side gate would block restocking during a stockout and point the
// operator at a setting that is already correct.
func (c *AdminClient) Configured() bool {
	return c.baseURL != "" && c.adminKey != ""
}

// AddKiroAPIKey implements PoolClient. Port of kiro_go_add_key
// (kiro_provision.py:446-454). Optional fields are omitted when empty,
// matching Python's `if nickname: payload[...] = ...`.
func (c *AdminClient) AddKiroAPIKey(ctx context.Context, kiroAPIKey, nickname, region string) (*AddResponse, error) {
	payload := map[string]string{"kiroApiKey": kiroAPIKey}
	if nickname != "" {
		payload["nickname"] = nickname
	}
	if region != "" {
		payload["region"] = region
	}
	return c.post(ctx, "/admin/add_kiro_api_key", payload)
}

// AddKiroAccount implements PoolClient. Port of kiro_go_add_account
// (kiro_provision.py:457-467).
//
// `account` is json.RawMessage, not a decoded map, ON PURPOSE: the account
// JSON kiro-go-login.py emitted must reach the pool BYTE-FOR-BYTE. It carries
// the refresh material (tokenEndpoint / clientId / scopes) Kiro-Go needs to
// keep the ~1h access token alive and the profileArn that pins its data-plane
// region (KIRO_GO_API.md:168-175). Decoding and re-encoding through a Go map
// would silently drop any field the port doesn't know about and could
// renormalize numbers — and a missing field here is a 400 at best, or an
// account that dies silently an hour later at worst.
func (c *AdminClient) AddKiroAccount(ctx context.Context, account json.RawMessage, nickname string) (*AddResponse, error) {
	payload := map[string]json.RawMessage{"account": account}
	if nickname != "" {
		// Marshal the nickname through encoding/json so quoting/escaping is
		// correct for an address containing any JSON-special character.
		encoded, err := json.Marshal(nickname)
		if err != nil {
			return nil, fmt.Errorf("provision: encode nickname: %w", err)
		}
		payload["nickname"] = encoded
	}
	return c.post(ctx, "/admin/add_kiro_account", payload)
}

// Pool fetches operator-side pool health (GET /admin/pool,
// KIRO_GO_API.md:129-131). Assigned to this package by architecture-go.md §1
// alongside the two add routes.
//
// The body is returned as raw JSON rather than a typed struct because
// KIRO_GO_API.md documents this route's SHAPE nowhere ("Returns the
// operator-side account pool ... Use for operator dashboards"), and the Python
// bot never calls it — so there is no reference implementation to port a
// struct from. Inventing one would pin fields the server may not have.
// Callers decode the subset they need.
func (c *AdminClient) Pool(ctx context.Context) (json.RawMessage, error) {
	body, err := c.do(ctx, http.MethodGet, "/admin/pool", nil)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

// post sends a JSON body to an /admin endpoint and decodes the 2xx response
// into an AddResponse. Port of _kiro_go_admin_post (kiro_provision.py:421-443).
func (c *AdminClient) post(ctx context.Context, path string, payload any) (*AddResponse, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Never include the payload in the message — it holds the credential.
		return nil, fmt.Errorf("provision: encode request for %s: %w", path, err)
	}
	body, err := c.do(ctx, http.MethodPost, path, encoded)
	if err != nil {
		return nil, err
	}
	var out AddResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("provision: decode response for %s: %w", path, err)
	}
	return &out, nil
}

// do performs one request with admin auth and returns the 2xx body.
func (c *AdminClient) do(ctx context.Context, method, path string, encoded []byte) ([]byte, error) {
	if !c.Configured() {
		return nil, ErrPoolNotConfigured
	}

	var reader io.Reader
	if encoded != nil {
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("provision: build request for %s: %w", path, err)
	}
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// KIRO_GO_API.md:8-11 / kiro_provision.py:432. The admin key is NEVER
	// logged and never reaches an error string.
	req.Header.Set("Authorization", "Bearer "+c.adminKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provision: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("provision: read response for %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &PoolError{
			Endpoint:   path[strings.LastIndex(path, "/")+1:],
			StatusCode: resp.StatusCode,
			Message:    poolErrorMessage(body, resp.StatusCode),
		}
	}
	return body, nil
}

// poolErrorMessage extracts Kiro-Go's {"error": "..."} text from a non-2xx
// body, falling back to "HTTP <code>" when the body isn't that shape (an
// upstream gateway's HTML page, say). Port of kiro_provision.py:437-442.
func poolErrorMessage(body []byte, statusCode int) string {
	var eb struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
		return eb.Error
	}
	// Python falls back to json.dumps(body) for a 2xx-shaped JSON object with
	// no "error" key; that would echo the response body, which for these
	// endpoints can carry pool ids and emails. "HTTP <code>" is strictly
	// safer and just as actionable.
	return fmt.Sprintf("HTTP %d", statusCode)
}
