package kirogo

import (
	"context"
	"encoding/json"
	"strings"
)

// Remaining models any Kiro-Go "*Remaining" field (creditsRemaining,
// tokensRemaining). -1 means unlimited (the corresponding limit is 0) per
// KIRO_GO_API.md:15 — invariant 4. Plain arithmetic on a Remaining value
// without checking IsUnlimited first is a bug (treating -1 as "minus one
// unit left" instead of "unlimited").
//
// Backed by float64, not int64: creditsRemaining is a metered, server-computed
// balance and the live deploy sends fractional values (observed verbatim:
// creditsRemaining=1999.95 for a creditLimit=2000/creditsUsed=0.05 key) —
// decoding it into an integer field crashes json.Unmarshal outright ("cannot
// unmarshal number 0.05 into ... of type int64"), which is exactly the live
// bug this type exists to fix. -1 (the unlimited sentinel) is exactly
// representable in float64, so IsUnlimited below is unaffected.
type Remaining float64

// IsUnlimited reports whether this remaining-count field represents an
// unlimited quota (wire value -1).
func (r Remaining) IsUnlimited() bool {
	return float64(r) == -1
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
// SaleClassFixed is Fixed Pricing: capacity the operator bought as prepaid
// vendor API keys in bulk, billed to the customer at a flat credit price per
// REQUEST (set per model on the pool, not here). Like kiro and unlike bedrock it
// binds no per-customer upstream key: the pool account fronts its own rotating
// vendor key pool, so there is nothing per-customer to provision.
const (
	SaleClassBedrock = "bedrock"
	SaleClassKiro    = "kiro"
	SaleClassFixed   = "fixed"
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

	case SaleClassFixed:
		if req.AccountID != "" {
			return nil, ErrAccountIDForbiddenForFixed
		}
		// Same structural guarantee as the kiro case: no AccountID field at
		// all, so "accountId" cannot appear in the marshaled JSON. Fixed
		// Pricing capacity is a pooled account that rotates its own vendor
		// keys, so there is no per-customer upstream key to bind.
		payload = struct {
			Name         string `json:"name"`
			Credits      int64  `json:"credits"`
			TokenLimit   int    `json:"tokenLimit"`
			SaleClass    string `json:"saleClass"`
			SkipUpstream bool   `json:"skipUpstream"`
		}{
			Name:         req.Name,
			Credits:      req.Credits,
			TokenLimit:   0,
			SaleClass:    SaleClassFixed,
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
	Enabled bool `json:"enabled"`
	// CreditLimit / CreditsUsed are RESPONSE-side (server-computed running
	// totals after this top-up), same fractional-credit exposure as
	// CreditsRemaining — see the Remaining doc comment. Unlike the
	// int64 CreditLimit line, these deal with the raw creditsUsed meter, so
	// they carry the same float requirement independently.
	CreditLimit      float64   `json:"creditLimit"`
	CreditsUsed      float64   `json:"creditsUsed"`
	CreditsRemaining Remaining `json:"creditsRemaining"`
	// TokenLimit/TokensUsed/TokensRemaining stay integral: Kiro-Go meters
	// tokens as whole units (every observed body, live and documented, carries
	// a whole-number tokensUsed/tokensRemaining) — only credits accrue
	// fractionally.
	TokenLimit      int64     `json:"tokenLimit"`
	TokensUsed      int64     `json:"tokensUsed"`
	TokensRemaining Remaining `json:"tokensRemaining"`
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
	if req.SaleClass != "" && req.SaleClass != SaleClassBedrock &&
		req.SaleClass != SaleClassKiro && req.SaleClass != SaleClassFixed {
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
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyMasked string `json:"keyMasked"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
	// CreditLimit / CreditsUsed: RESPONSE-side, same live-deploy fractional
	// exposure that caused this package's headline bug — the verbatim body
	// that crashed decoding was creditLimit=2000, creditsUsed=0.05 off THIS
	// struct. See the Remaining doc comment for the wire evidence.
	CreditLimit      float64   `json:"creditLimit"`
	CreditsUsed      float64   `json:"creditsUsed"`
	CreditsRemaining Remaining `json:"creditsRemaining"`
	// TokenLimit/TokensUsed/TokensRemaining stay integral — tokens meter in
	// whole units on every observed body; only credits accrue fractionally.
	TokenLimit      int64     `json:"tokenLimit"`
	TokensUsed      int64     `json:"tokensUsed"`
	TokensRemaining Remaining `json:"tokensRemaining"`
	RequestsCount   int64     `json:"requestsCount"`
	CreatedAt       int64     `json:"createdAt"`
	LastUsedAt      int64     `json:"lastUsedAt"`
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
//
// Field tags are the live deploy's spelling, NOT the doc's. KIRO_GO_API.md:122
// documents `{"keys":1,"tokens":12345,"credits":40,"requests":20}`, but a real
// curled body (same investigation as the KeyStat fractional-credits bug) is
// `{"creditsUsed":48752.967...,"keys":65,"requestsCount":72959,
// "tokensUsed":9608720736}` — three of four field names differ from the doc.
// Under the old tags ("tokens"/"credits"/"requests") those three fields
// silently decoded as their zero value on every real response: no error, no
// log line, just permanently-0 totals. This is the SAME failure shape as the
// "keys" vs "apiKeys" mismatch on StatsResponse.Keys — a doc example that
// isn't the schema. Only "keys" is confirmed correct in both doc and deploy.
type StatsTotals struct {
	Keys int64 `json:"keys"`
	// Tokens stays integral (see KeyStat's TokenLimit/TokensUsed doc comment —
	// tokens meter in whole units on every observed body).
	Tokens int64 `json:"tokensUsed"`
	// Credits: RESPONSE-side aggregate over every key's fractional
	// creditsUsed — must be float for the same reason as KeyStat.CreditsUsed.
	Credits  float64 `json:"creditsUsed"`
	Requests int64   `json:"requestsCount"`
}

// StatsResponse is the success body of /admin/stats (KIRO_GO_API.md:111-124).
//
// The Keys field carries a custom UnmarshalJSON (below) because the doc and
// the live deploy disagree on the wire name for this array: KIRO_GO_API.md
// spells it "keys", but the actually-deployed Kiro-Go server sends "apiKeys"
// (confirmed by curling POST /admin/stats against production with Bearer
// auth — the response has no "keys" key at all, only "apiKeys" and
// "totals.keys", the latter being an unrelated COUNT that made the doc's
// spelling look right at a glance). The Python original never trusted the
// doc alone: telegram_bot.py:4349 and :4544 both do
// `res.get("keys") or res.get("apiKeys") or []`, deliberately accepting
// either spelling. That dual-read is not incidental — CLAUDE.md pins the
// Python source, not KIRO_GO_API.md, as the behavioral reference for this
// port, and the Python comments at telegram_bot.py:4346-4347 and :4535-4536
// spell out exactly this doc-vs-deploy split. If this fallback is ever
// removed, decoding a real response silently succeeds (Stats() returns a
// nil error) while Keys stays permanently empty — every caller then takes
// its zero-keys path with NO error and NO log line: apikeyLiveStats
// (handlers/apikey.go:639) returns nil on len(res.Keys)==0 without logging
// anything, so the failure is invisible in the server log. The customer-
// visible symptom is Self-Service → refund an API key →
// "⚠️ Couldn't check this key's remaining credits right now."
type StatsResponse struct {
	Keys   []KeyStat   `json:"-"`
	Totals StatsTotals `json:"totals"`
}

// statsResponseWire is the raw decode target for StatsResponse: it captures
// both possible spellings of the key list so UnmarshalJSON can pick the
// right one after the fact, mirroring the Python original's
// `res.get("keys") or res.get("apiKeys") or []`.
type statsResponseWire struct {
	Keys    []KeyStat   `json:"keys"`
	APIKeys []KeyStat   `json:"apiKeys"`
	Totals  StatsTotals `json:"totals"`
}

// UnmarshalJSON decodes a /admin/stats body accepting either the documented
// "keys" spelling or the deployed server's "apiKeys" spelling for the key
// list, preferring "keys" when both are present and non-empty — the same
// precedence as Python's `res.get("keys") or res.get("apiKeys") or []` (an
// empty/absent "keys" falls through to "apiKeys" rather than winning by mere
// presence). See the StatsResponse doc comment for why this exists.
func (r *StatsResponse) UnmarshalJSON(data []byte) error {
	var wire statsResponseWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	r.Totals = wire.Totals
	if len(wire.Keys) > 0 {
		r.Keys = wire.Keys
	} else {
		r.Keys = wire.APIKeys
	}
	return nil
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

	TokenLimit int64 `json:"tokenLimit"`
	TokensUsed int64 `json:"tokensUsed"`
	// CreditLimit / CreditsUsed: RESPONSE-side, same fractional-credit
	// exposure as KeyStat/RechargeKeyResponse — this is the buyer-facing
	// balance-check endpoint, so a fractional creditsUsed here would crash
	// json.Unmarshal exactly like the /admin/stats bug this package now fixes.
	// Not independently confirmed against a live /api/me body (only
	// /admin/stats was curled) — flagged as the same doc-example risk
	// pattern, fixed proactively rather than left to fail the same way later.
	CreditLimit float64 `json:"creditLimit"`
	CreditsUsed float64 `json:"creditsUsed"`

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

// ── POST /admin/set_model_price, GET /admin/model_prices ────────────────────
// Flat per-request pricing for the "fixed" sale class. The pool's rate card is
// otherwise editable only from its admin panel, which the bot cannot reach: the
// panel routes sit behind panel auth while the bot holds the admin key.

// SetModelPriceRequest sets the flat credit charge for one request to one model.
type SetModelPriceRequest struct {
	Model string
	// PerRequest is credits per request. A pointer so an omitted value is
	// distinguishable from a deliberate 0 — sending nothing would price the
	// model free, which is the one mistake here that costs money on every
	// request afterwards rather than failing loudly once.
	PerRequest *float64
}

// SetModelPriceResponse is the pool's echo of the stored price.
type SetModelPriceResponse struct {
	Success    bool    `json:"success"`
	Model      string  `json:"model"`
	PerRequest float64 `json:"perRequest"`
}

// ModelPrice is one row of the flat-price card.
type ModelPrice struct {
	Model      string  `json:"model"`
	PerRequest float64 `json:"perRequest"`
}

// ModelPricesResponse lists every model carrying a flat per-request price.
type ModelPricesResponse struct {
	Success bool         `json:"success"`
	Count   int          `json:"count"`
	Prices  []ModelPrice `json:"prices"`
}

// SetModelPrice sets a model's flat per-request price on the pool.
func (c *Client) SetModelPrice(ctx context.Context, req SetModelPriceRequest) (*SetModelPriceResponse, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, ErrModelRequired
	}
	// Validated here rather than left to the server so an admin typo costs a
	// message, not a round trip and a raw upstream error string in a chat.
	if req.PerRequest == nil {
		return nil, ErrPerRequestRequired
	}
	if *req.PerRequest < 0 {
		return nil, ErrPerRequestNegative
	}

	payload := struct {
		Model      string  `json:"model"`
		PerRequest float64 `json:"perRequest"`
	}{Model: strings.TrimSpace(req.Model), PerRequest: *req.PerRequest}

	var out SetModelPriceResponse
	if err := c.adminSend(ctx, "POST", "/admin/set_model_price", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ModelPrices lists the models that carry a flat per-request price.
func (c *Client) ModelPrices(ctx context.Context) (*ModelPricesResponse, error) {
	var out ModelPricesResponse
	if err := c.adminSend(ctx, "GET", "/admin/model_prices", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
