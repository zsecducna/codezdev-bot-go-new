package kirogo

import "context"

// Remaining models any Kiro-Go "*Remaining" field (creditsRemaining,
// tokensRemaining). -1 means unlimited (the corresponding limit is 0) per
// KIRO_GO_API.md:15 — invariant 4. Plain arithmetic on a Remaining value
// without checking IsUnlimited first is a bug (treating -1 as "minus one
// unit left" instead of "unlimited").
type Remaining int64

// IsUnlimited reports whether this remaining-count field represents an
// unlimited quota (wire value -1).
func (r Remaining) IsUnlimited() bool {
	return int64(r) == -1
}

// Upstream binding status returned by provision_customer
// (KIRO_GO_API.md:44). "pending" IS SUCCESS for a bedrock mint — see
// ProvisionCustomerResponse.Deliverable. "skipped" is the EXPECTED terminal
// status for a kiro-class mint (which always requests skipUpstream:true) but
// anomalous for bedrock — see ProvisionCustomerResponse.UpstreamStatusAnomalous.
const (
	UpstreamStatusBound   = "bound"
	UpstreamStatusPending = "pending"
	UpstreamStatusSkipped = "skipped"
)

// Sale classes provision_customer / recharge_api_key route on
// (architecture-go.md §6 invariant 3b; telegram_bot.py:7489-7516).
//
//   - SaleClassBedrock ("__apikey__" product, rate-card credits): the
//     original/default mint path — explicit accountId, upstream binding
//     attempted (upstreamStatus bound/pending).
//   - SaleClassKiro ("__kiroapi__" product, Kiro-credit keys): routes to
//     native Kiro accounts instead of an llmgate upstream — no accountId,
//     skipUpstream:true, upstreamStatus is always "skipped".
//
// The empty string "" means "legacy / unspecified": ProvisionCustomer treats
// it exactly like SaleClassBedrock (accountId required, no saleClass field
// sent on the wire — a pre-split server predates the field entirely).
// RechargeKey treats "" specially in the OTHER direction: it must OMIT the
// saleClass field entirely rather than send an empty string, because a
// legacy key's own stored sale_class (never recorded for pre-split rows) is
// the server-side source of truth there — see RechargeKeyRequest.
const (
	SaleClassBedrock = "bedrock"
	SaleClassKiro    = "kiro"
)

// ── POST /admin/provision_customer (KIRO_GO_API.md:21-46) ───────────────────
// The bot's primary mint path: sells a metered key AND (for bedrock) binds a
// per-customer upstream (llmgate) key to it (telegram_bot.py provision_customer
// call sites: 5516 (verify probe, bedrock), 7489-7516 (real sale, both classes)).

// ProvisionCustomerRequest is the request body for /admin/provision_customer.
// TokenLimit is deliberately NOT a field here: invariant 3 pins it to 0 in
// the wire payload unconditionally (see Client.ProvisionCustomer), so it is
// not something a caller can accidentally set non-zero through this type.
type ProvisionCustomerRequest struct {
	// Name labels the key, e.g. the Telegram order id ("tg-<chat>-order-<des>").
	Name string
	// Credits is the credit quota to mint. MUST be > 0 (checked client-side
	// before any HTTP call — KIRO_GO_API.md:32).
	Credits int64
	// SaleClass selects the mint class: "" or SaleClassBedrock for the
	// original rate-card path (accountId required), SaleClassKiro for the
	// Kiro-credit path (accountId forbidden, skipUpstream forced true). See
	// the package-level SaleClass* docs above.
	SaleClass string
	// AccountID overrides the client's configured account id for a
	// bedrock/legacy call. Leave empty to use Config.AccountID (the common
	// case). MUST be empty when SaleClass == SaleClassKiro — invariant 3b.
	AccountID string
	// SkipUpstream, when true, skips llmgate upstream binding entirely
	// (KIRO_GO_API.md:29). Only meaningful for the bedrock/legacy class —
	// ignored (and hardcoded true on the wire) for SaleClassKiro, which
	// always skips upstream binding by definition.
	SkipUpstream bool
}

// ProvisionCustomerResponse is the success (2xx) body of
// /admin/provision_customer (KIRO_GO_API.md:36-42).
//
// SECURITY (invariant 2): this struct has NO field for the upstream llmgate
// key — only the non-secret UpstreamKeyID. Kiro-Go deliberately never sends
// the llmgate key value back to the bot (KIRO_GO_API.md:46): any llmgate key
// can read the WHOLE account's quota and spend, not just this customer's, so
// there is no field here to decode one into even if a future server response
// carried it by mistake — encoding/json silently drops unknown JSON keys that
// have no matching Go field.
type ProvisionCustomerResponse struct {
	Success bool `json:"success"`
	// ID is the pool key's id (safe to log/store).
	ID string `json:"id"`
	// Key is the cleartext customer key (sk-...), shown ONCE. Deliver it to
	// the buyer and never store it in cleartext again (mirrors the Python
	// docstring at telegram_bot.py:6091 "never stored in cleartext again").
	Key           string `json:"key"`
	Name          string `json:"name"`
	Credits       int64  `json:"credits"`
	UpstreamKeyID int64  `json:"upstreamKeyId"`
	UpstreamBound bool   `json:"upstreamBound"`
	// UpstreamStatus is one of UpstreamStatusBound/Pending/Skipped.
	UpstreamStatus string `json:"upstreamStatus"`
	// SaleClass is the SERVER'S ECHO of the class it actually recorded for the
	// new key — NOT a copy of the request's. Callers persist THIS value (not the
	// order's SKU) because it is what arms the recharge 409 guard: a
	// pre-saleClass Kiro-Go ignores the request's saleClass and echoes none, and
	// the server 409s any guard sent against a key it recorded as unclassed
	// (only an OMITTED guard matches). Storing the order's "bedrock" for such a
	// mint would 409 — and thus auto-refund + page an owner on — that key's
	// first-ever recharge (telegram_bot.py:7603-7614).
	SaleClass string `json:"saleClass"`
}

// Deliverable reports whether this response represents a completed, billable
// sale that must be delivered to the buyer.
//
// INVARIANT 1 (the single most important rule in this package): a 2xx
// response is ALWAYS success, regardless of UpstreamStatus. For a bedrock
// mint, UpstreamStatus=="pending" means the upstream key could not be
// created yet, but the pool key was created and is fully working/billable —
// it forwards on the shared account credential until a background retry
// binds the upstream key later. For a kiro mint, UpstreamStatus=="skipped"
// is the NORMAL, expected terminal state (kiro mints always skip upstream
// binding). Either way the caller MUST deliver the key and complete the
// sale: no retry (would mint a SECOND paid key for one sale) and no refund.
// Only a non-2xx HTTP response (which never reaches this method — send
// returns an *APIError instead) means the sale actually failed. Deliverable
// simply mirrors Success; it exists so call sites read as a decision ("if
// !resp.Deliverable() { ... }") instead of accidentally branching on
// UpstreamStatus, which is exactly the mistake this invariant guards against.
func (r ProvisionCustomerResponse) Deliverable() bool {
	return r.Success
}

// IsUpstreamPending reports whether upstream binding is still in flight
// (bedrock class only — a kiro mint never returns "pending"). Informational
// only — per Deliverable's docstring, this must NEVER gate whether the key
// is delivered or the sale is completed.
func (r ProvisionCustomerResponse) IsUpstreamPending() bool {
	return r.UpstreamStatus == UpstreamStatusPending
}

// IsUpstreamSkipped reports whether upstream binding was skipped. This is
// the NORMAL terminal state for a kiro-class mint. Informational only — see
// Deliverable's docstring; a skipped mint is still a delivered, billable sale.
func (r ProvisionCustomerResponse) IsUpstreamSkipped() bool {
	return r.UpstreamStatus == UpstreamStatusSkipped
}

// UpstreamStatusAnomalous reports whether UpstreamStatus is unexpected for
// the given sale class — an operator-alert signal only, NEVER a failure
// signal (Deliverable() already covers that). For SaleClassKiro, anything
// other than "skipped" is anomalous (a kiro mint should always skip upstream
// binding by construction). For bedrock/legacy ("" or SaleClassBedrock),
// "skipped" is anomalous — a bedrock mint should bind or defer-bind, never
// skip; a caller seeing this should alert the owners the same way the
// Python bot does (telegram_bot.py:7572-7587) even though the sale itself
// still completes normally.
func (r ProvisionCustomerResponse) UpstreamStatusAnomalous(saleClass string) bool {
	if saleClass == SaleClassKiro {
		return r.UpstreamStatus != UpstreamStatusSkipped
	}
	return r.UpstreamStatus == UpstreamStatusSkipped
}

// ProvisionCustomer mints a metered key (POST /admin/provision_customer,
// KIRO_GO_API.md:21-46), routing on req.SaleClass per invariant 3b:
//
//   - "" / SaleClassBedrock: accountId is REQUIRED (from req.AccountID or the
//     client's configured Config.AccountID) and sent explicitly; skipUpstream
//     is req.SkipUpstream (default false — attempt upstream binding).
//   - SaleClassKiro: accountId must NOT be set (ErrAccountIDForbiddenForKiro
//     if req.AccountID is non-empty) and is never sent; skipUpstream is
//     hardcoded true on the wire regardless of req.SkipUpstream, exactly like
//     tokenLimit is hardcoded 0 — these are invariants of the class, not
//     caller-tunable knobs.
//
// tokenLimit is hardcoded 0 in BOTH branches (invariant 3).
func (c *Client) ProvisionCustomer(ctx context.Context, req ProvisionCustomerRequest) (*ProvisionCustomerResponse, error) {
	// Client-side validation BEFORE any HTTP call (invariant 3): a credits<=0
	// or misconfigured-account-id request must fail cheaply and locally, not
	// burn a round trip only to get the same 400 back.
	if req.Credits <= 0 {
		return nil, ErrCreditsMustBePositive
	}

	var payload any

	switch req.SaleClass {
	case SaleClassKiro:
		if req.AccountID != "" {
			return nil, ErrAccountIDForbiddenForKiro
		}
		// No AccountID field on this struct at all — guarantees "accountId"
		// is structurally absent from the marshaled JSON, not merely empty.
		payload = struct {
			Name         string `json:"name"`
			Credits      int64  `json:"credits"`
			TokenLimit   int    `json:"tokenLimit"`
			SaleClass    string `json:"saleClass"`
			SkipUpstream bool   `json:"skipUpstream"`
		}{
			Name:       req.Name,
			Credits:    req.Credits,
			TokenLimit: 0,
			SaleClass:  SaleClassKiro,
			// Hardcoded true — a kiro mint always skips upstream binding;
			// this is not something req.SkipUpstream can override.
			SkipUpstream: true,
		}

	case SaleClassBedrock, "":
		accountID := req.AccountID
		if accountID == "" {
			accountID = c.cfg.AccountID
		}
		if accountID == "" {
			return nil, ErrAccountIDRequired
		}
		payload = struct {
			Name         string `json:"name"`
			Credits      int64  `json:"credits"`
			AccountID    string `json:"accountId"`
			TokenLimit   int    `json:"tokenLimit"`
			SkipUpstream bool   `json:"skipUpstream"`
			SaleClass    string `json:"saleClass,omitempty"`
		}{
			Name:      req.Name,
			Credits:   req.Credits,
			AccountID: accountID,
			// Hardcoded 0, never a caller-controlled value — invariant 3
			// ("tokenLimit always 0"). Credits are the meter for minted keys.
			TokenLimit:   0,
			SkipUpstream: req.SkipUpstream,
			// "" (legacy) omits the field entirely (omitempty) — a
			// pre-split Kiro-Go server predates it. SaleClassBedrock sends
			// it explicitly, pinning routing+billing on a server that knows
			// about the field (harmless on one that doesn't).
			SaleClass: req.SaleClass,
		}

	default:
		return nil, ErrUnknownSaleClass
	}

	var out ProvisionCustomerResponse
	if err := c.adminSend(ctx, "POST", "/admin/provision_customer", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── POST /admin/new_api_key (KIRO_GO_API.md:50-65) — legacy, no upstream ────
// Superseded by ProvisionCustomer; kept because the documented endpoint still
// works and some callers (e.g. a future admin tool) may need the no-upstream
// path. The bot itself no longer calls it (KIRO_GO_API.md:51).

// NewAPIKeyRequest is the request body for /admin/new_api_key.
type NewAPIKeyRequest struct {
	// Name is optional (order id label).
	Name string
	// Credits MUST be > 0 (checked client-side — KIRO_GO_API.md:57).
	Credits int64
	// TokenLimit is optional, >= 0; 0 means no extra hard token cap
	// (KIRO_GO_API.md:59). Unlike ProvisionCustomer, this legacy endpoint's
	// documented contract allows a caller-supplied cap, so it IS a field here.
	TokenLimit int64
}

// NewAPIKeyResponse is the success body of /admin/new_api_key
// (KIRO_GO_API.md:63) — no upstream fields at all, since this endpoint never
// binds an upstream key.
type NewAPIKeyResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
	// Key is the cleartext key, shown once — same handling rule as
	// ProvisionCustomerResponse.Key.
	Key     string `json:"key"`
	Name    string `json:"name"`
	Credits int64  `json:"credits"`
}

// NewAPIKey mints a metered key with no upstream binding
// (POST /admin/new_api_key, KIRO_GO_API.md:50-65).
func (c *Client) NewAPIKey(ctx context.Context, req NewAPIKeyRequest) (*NewAPIKeyResponse, error) {
	if req.Credits <= 0 {
		return nil, ErrCreditsMustBePositive
	}
	if req.TokenLimit < 0 {
		return nil, ErrTokenLimitNegative
	}

	payload := struct {
		Name       string `json:"name,omitempty"`
		Credits    int64  `json:"credits"`
		TokenLimit int64  `json:"tokenLimit"`
	}{
		Name:       req.Name,
		Credits:    req.Credits,
		TokenLimit: req.TokenLimit,
	}

	var out NewAPIKeyResponse
	if err := c.adminSend(ctx, "POST", "/admin/new_api_key", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── POST /admin/recharge_api_key (KIRO_GO_API.md:69-90) ──────────────────────

// RechargeKeyRequest identifies the key to top up (ID or APIKey, at least
// one) plus the additive credit amount.
//
// There is deliberately NO Tokens field on this type. KIRO_GO_API.md:76 is
// explicit: "Send credits ONLY — never tokens." Every key this bot mints
// carries tokenLimit:0 (unlimited tokens; credits are the meter), so a
// `tokens` field on the wire 400s the WHOLE recharge, and used to (before that
// validation existed) silently SET the token cap to N — instantly disabling
// any key whose usage already exceeded N. Omitting the field from the type
// entirely makes that mistake unrepresentable in Go, not just discouraged by
// a comment.
type RechargeKeyRequest struct {
	// ID identifies the key by its pool id. Provide ID or APIKey (at least
	// one) — KIRO_GO_API.md:72.
	ID string
	// APIKey identifies the key by its cleartext value, as a fallback when ID
	// is unknown.
	APIKey string
	// Credits is ADDED to the key's current credit limit (100 -> +50 -> 150),
	// not set. MUST be > 0 (checked client-side — KIRO_GO_API.md:77).
	Credits int64
	// SaleClass is the server-side GUARD ("bedrock"|"kiro") that stops a
	// recharge from landing on the wrong class of key (a ~5.7x value
	// mismatch between a KIRO credit and a rate-card credit) — a class that
	// doesn't match the key's own is refused with a 409 and nothing changes.
	//
	// Leave EMPTY for a legacy key whose class the bot never recorded
	// (pre-split rows, store.py:170-177): the field is then OMITTED from
	// the wire request entirely (via the json "omitempty" tag below), not
	// sent as an empty string, so the server's own stored record decides —
	// sending "" would not merely be a no-op, Kiro-Go would see an explicit
	// (if empty) saleClass key and could reject it as an unrecognized class.
	SaleClass string
}

// RechargeKeyResponse is the success body of /admin/recharge_api_key
// (KIRO_GO_API.md:80-87).
type RechargeKeyResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
	// Enabled is true after the recharge — a top-up RE-ENABLES a key that was
	// disabled for exhaustion (KIRO_GO_API.md:78, 90).
	Enabled          bool      `json:"enabled"`
	CreditLimit      int64     `json:"creditLimit"`
	CreditsUsed      int64     `json:"creditsUsed"`
	CreditsRemaining Remaining `json:"creditsRemaining"`
	TokenLimit       int64     `json:"tokenLimit"`
	TokensUsed       int64     `json:"tokensUsed"`
	TokensRemaining  Remaining `json:"tokensRemaining"`
}

// RechargeKey tops up an existing key's credit quota
// (POST /admin/recharge_api_key, KIRO_GO_API.md:69-90). Sends credits ONLY —
// see RechargeKeyRequest's docstring for why a tokens field must never exist,
// and why an empty SaleClass omits the saleClass field entirely rather than
// sending "".
func (c *Client) RechargeKey(ctx context.Context, req RechargeKeyRequest) (*RechargeKeyResponse, error) {
	if req.ID == "" && req.APIKey == "" {
		return nil, ErrKeyIdentifierRequired
	}
	if req.Credits <= 0 {
		return nil, ErrCreditsMustBePositive
	}
	if req.SaleClass != "" && req.SaleClass != SaleClassBedrock && req.SaleClass != SaleClassKiro {
		return nil, ErrUnknownSaleClass
	}

	payload := struct {
		ID        string `json:"id,omitempty"`
		APIKey    string `json:"apiKey,omitempty"`
		Credits   int64  `json:"credits"`
		SaleClass string `json:"saleClass,omitempty"`
	}{
		ID:        req.ID,
		APIKey:    req.APIKey,
		Credits:   req.Credits,
		SaleClass: req.SaleClass,
	}

	var out RechargeKeyResponse
	if err := c.adminSend(ctx, "POST", "/admin/recharge_api_key", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── POST /admin/delete_api_key (KIRO_GO_API.md:94-101) ───────────────────────

// DeleteAPIKeyRequest identifies the key to revoke (ID or APIKey, at least
// one).
type DeleteAPIKeyRequest struct {
	ID     string
	APIKey string
}

// DeleteAPIKeyResponse is the success body of /admin/delete_api_key
// (KIRO_GO_API.md:99-100).
//
// UpstreamRevoked is a POINTER — this is the tri-state invariant 5 hinges on.
// nil means the field was ABSENT from the JSON body (an older Kiro-Go server
// that predates upstream binding); that is NOT the same as an explicit
// false, which means the bound llmgate key is still live and spending
// against our account. A plain `bool` field would collapse "absent" and
// "false" into the same zero value and silently miss the alert condition —
// exactly the bug KIRO_GO_API.md:100 warns about ("the bot checks is False,
// not falsiness").
type DeleteAPIKeyResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
	// UpstreamRevoked: nil = field absent (old server, unknown); non-nil true
	// = upstream key revoked; non-nil false = upstream key STILL LIVE.
	UpstreamRevoked *bool `json:"upstreamRevoked"`
}

// UpstreamStillLive reports the loud-alert condition of invariant 5: the pool
// key is gone but the bound upstream (llmgate) key is confirmed STILL LIVE
// and needs manual removal in the llmgate portal. This is deliberately
// FALSE both when UpstreamRevoked is nil (absent — old server, unknown, not
// an alert) and when it is true (actually revoked) — only an explicit false
// trips it. Callers must still report the delete itself as successful when
// this is true: the customer's pool key access is dead either way, and a
// refund must never block on upstream cleanup (KIRO_GO_API.md:100).
func (r DeleteAPIKeyResponse) UpstreamStillLive() bool {
	return r.UpstreamRevoked != nil && !*r.UpstreamRevoked
}

// DeleteAPIKey revokes a key (POST /admin/delete_api_key,
// KIRO_GO_API.md:94-101). The pool key is always deleted on a 2xx response
// regardless of upstream cleanup outcome — see DeleteAPIKeyResponse.UpstreamStillLive.
func (c *Client) DeleteAPIKey(ctx context.Context, req DeleteAPIKeyRequest) (*DeleteAPIKeyResponse, error) {
	if req.ID == "" && req.APIKey == "" {
		return nil, ErrKeyIdentifierRequired
	}

	payload := struct {
		ID     string `json:"id,omitempty"`
		APIKey string `json:"apiKey,omitempty"`
	}{ID: req.ID, APIKey: req.APIKey}

	var out DeleteAPIKeyResponse
	if err := c.adminSend(ctx, "POST", "/admin/delete_api_key", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── POST /admin/stats (KIRO_GO_API.md:105-125) — key info/list ──────────────

// StatsRequest holds optional AND-ed filters; the zero value (empty body)
// returns every key, masked (KIRO_GO_API.md:106).
type StatsRequest struct {
	APIKey string
	ID     string
	Name   string
}

// KeyStat is one entry of StatsResponse.Keys (KIRO_GO_API.md:114-120).
// Status is one of "active" | "exhausted" | "disabled" (KIRO_GO_API.md:125).
type KeyStat struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	KeyMasked        string    `json:"keyMasked"`
	Enabled          bool      `json:"enabled"`
	Status           string    `json:"status"`
	CreditLimit      int64     `json:"creditLimit"`
	CreditsUsed      int64     `json:"creditsUsed"`
	CreditsRemaining Remaining `json:"creditsRemaining"`
	TokenLimit       int64     `json:"tokenLimit"`
	TokensUsed       int64     `json:"tokensUsed"`
	TokensRemaining  Remaining `json:"tokensRemaining"`
	RequestsCount    int64     `json:"requestsCount"`
	CreatedAt        int64     `json:"createdAt"`
	LastUsedAt       int64     `json:"lastUsedAt"`
	// SaleClass is the class the SERVER has recorded for this key ("bedrock" |
	// "kiro" | absent on a pre-split server). It is the authority for a LEGACY
	// row the bot stored with an empty sale_class: the server may know a class
	// the bot predates, so display code falls back to this before defaulting to
	// bedrock (telegram_bot.py:4340-4342). Never use it to build a recharge
	// guard — that must come from the bot's own stored value, or a legacy row
	// would send a guess the server 409s.
	SaleClass string `json:"saleClass"`
}

// StatsTotals is the aggregate block of StatsResponse (KIRO_GO_API.md:122).
type StatsTotals struct {
	Keys     int64 `json:"keys"`
	Tokens   int64 `json:"tokens"`
	Credits  int64 `json:"credits"`
	Requests int64 `json:"requests"`
}

// StatsResponse is the success body of /admin/stats (KIRO_GO_API.md:111-124).
type StatsResponse struct {
	Keys   []KeyStat   `json:"keys"`
	Totals StatsTotals `json:"totals"`
}

// Stats fetches usage info for keys matching the (optional, AND-ed) filters
// (POST /admin/stats, KIRO_GO_API.md:105-125). An empty StatsRequest returns
// every key, masked.
func (c *Client) Stats(ctx context.Context, req StatsRequest) (*StatsResponse, error) {
	payload := struct {
		APIKey string `json:"apiKey,omitempty"`
		ID     string `json:"id,omitempty"`
		Name   string `json:"name,omitempty"`
	}{APIKey: req.APIKey, ID: req.ID, Name: req.Name}

	var out StatsResponse
	if err := c.adminSend(ctx, "POST", "/admin/stats", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── GET /api/me (KIRO_GO_API.md:203-215) — customer-realm quota lookup ──────
// Auth is the CUSTOMER's own key (X-Api-Key), not the admin password — used
// for buyer-facing "check my balance" flows.

// CustomerMeResponse is the body of /api/me (KIRO_GO_API.md:204-214).
// Status is one of "active" | "exhausted" | "disabled". Never carries the
// cleartext key — only KeyMasked.
type CustomerMeResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	KeyMasked  string `json:"keyMasked"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt"`

	TokenLimit  int64 `json:"tokenLimit"`
	CreditLimit int64 `json:"creditLimit"`
	TokensUsed  int64 `json:"tokensUsed"`
	CreditsUsed int64 `json:"creditsUsed"`

	TokensRemaining  Remaining `json:"tokensRemaining"`
	CreditsRemaining Remaining `json:"creditsRemaining"`

	RequestsCount int64 `json:"requestsCount"`
}

// CustomerMe fetches the identity + live quota for the buyer's own key
// (GET /api/me, KIRO_GO_API.md:203-215). apiKey is the CUSTOMER's key
// (sk-...), sent as X-Api-Key — never the admin password.
func (c *Client) CustomerMe(ctx context.Context, apiKey string) (*CustomerMeResponse, error) {
	var out CustomerMeResponse
	if err := c.customerGet(ctx, "/api/me", apiKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
