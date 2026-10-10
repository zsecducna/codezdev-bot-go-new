package llmgate

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Key is one llmgate api-key record. It decodes every key-shaped payload in
// this API (create/reveal/list/update) even though those payloads differ:
//
//   - APIKey (cleartext "sk-llmgate-...") is present ONLY on create and
//     reveal — one-time values the caller must deliver or store before the
//     struct reference is dropped. list and update return no key value
//     (update echoes only api_key_length), so APIKey decodes empty there;
//     an empty APIKey is NOT an error (invariant 2).
//   - CreditLimit is 0 = UNLIMITED (1 credit = $1 of usage). Modeled as
//     float64, never int: limits and the costs metered against them are
//     fractional dollars in live responses (see UsageStats).
//   - ExpiresAt is the RAW ISO-8601 string off the wire (kept unparsed on
//     the struct so encoding round-trips are lossless); ABSENT means the
//     key never expires — the stats endpoint (verified live) includes
//     "expires_at" ONLY when the key has one. Parse it through Expiry(),
//     never time.Parse at the call site.
//   - list reports created_at per key, but update responses zero it
//     ("0001-01-01T00:00:00Z" — invariant 4); this struct therefore has NO
//     created_at field at all, so no caller can branch on a field this
//     client cannot guarantee is real.
type Key struct {
	ID          int64   `json:"id"`
	APIKey      string  `json:"api_key"`
	Name        string  `json:"name"`
	IsActive    bool    `json:"is_active"`
	CreditLimit float64 `json:"credit_limit"`
	// ExpiresAt is the raw expires_at string; omitempty mirrors the wire
	// (absent = never expires) so a Key re-marshals to the same shape.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Expiry parses Key.ExpiresAt into a time.Time. The bool is false when the
// key HAS NO expiry (field absent — never expires) or the value is unparsable
// garbage: both mean "this client cannot prove the key is expired", and every
// caller treats expiry as OPT-IN (an unparsable timestamp must never
// render a live sold key dead — the same fail-shape invariant 3 gives the
// create side).
//
// One layout suffices despite the server's two observed spellings: Go's
// time.Parse with time.RFC3339 accepts fractional seconds in the INPUT even
// though the layout does not mention them (documented time.Parse behavior),
// so "2026-10-11T01:35:00.000Z" and "2026-10-11T01:35:00Z" both parse, as do
// non-UTC offsets (+07:00, observed on login's expire_at).
func (k *Key) Expiry() (time.Time, bool) {
	if k.ExpiresAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, k.ExpiresAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// UsageStats is the metered-spend half of GET /api/v1/apikey/stats. Costs
// are in DOLLARS (input_cost 21.0 == $21), tokens are counts; the four
// cost buckets together ARE the complete metered spend (KeyUsage.Used sums
// exactly those four). RequestsOK/RequestsFailed count relayed requests,
// not billable units. wait_time is deliberately absent: nothing in the bot
// branches on it and the exported contract is fixed.
type UsageStats struct {
	InputTokens      int64   `json:"input_token"`
	OutputTokens     int64   `json:"output_token"`
	CacheTokens      int64   `json:"cache_token"`
	CacheWriteTokens int64   `json:"cache_write_token"`
	InputCost        float64 `json:"input_cost"`
	OutputCost       float64 `json:"output_cost"`
	CacheCost        float64 `json:"cache_cost"`
	CacheWriteCost   float64 `json:"cache_write_cost"`
	RequestsOK       int64   `json:"request_success"`
	RequestsFailed   int64   `json:"request_failed"`
}

// KeyUsage is the payload of GET /api/v1/apikey/stats (KEY realm,
// invariant 1): the key's own record plus its lifetime usage. IMPORTANT:
// these numbers LAG reality by ~30-60 seconds — the server aggregates
// usage asynchronously (package-doc invariant 5), so a top-up done a
// moment ago may not be reflected yet. Never treat a fresh KeyUsage as a
// realtime balance.
type KeyUsage struct {
	Info  Key        `json:"info"`
	Stats UsageStats `json:"stats"`
}

// Used returns total metered spend in dollars: the sum of the four cost
// buckets (input + output + cache + cache-write). Token counts are NOT
// part of "used" — spend is priced in cost dollars, and mixing a token
// count into a dollar sum would silently inflate it by six orders of
// magnitude.
func (u *KeyUsage) Used() float64 {
	return u.Stats.InputCost + u.Stats.OutputCost + u.Stats.CacheCost + u.Stats.CacheWriteCost
}

// Remaining returns (CreditLimit - Used, true) for a limited key.
//
// The bool is the unlimited marker: CreditLimit <= 0 means UNLIMITED — the
// one shape handlers must never do arithmetic on — and Remaining reports
// (0, false) for it. Skipping the bool check is a bug: (0, false) reads as
// "nothing left" to code that only looks at the float, exactly backwards
// for an unlimited key.
//
// A limited result may be NEGATIVE (spend past the limit); it is returned
// UNCLAMPED because the sign and magnitude past the line are the only
// signals for how far a key overspent — clamping to 0 would erase them.
func (u *KeyUsage) Remaining() (float64, bool) {
	if u.Info.CreditLimit <= 0 {
		return 0, false
	}
	return u.Info.CreditLimit - u.Used(), true
}

// Status is the one-word verdict handlers render for a key:
//
//	"disabled"  Info.IsActive == false — the switch is off, spend state is
//	            irrelevant (checked FIRST: a disabled+exhausted key is
//	            disabled, not exhausted — the operator action differs).
//	"expired"   an Expiry is set and lies in the past — the key is dead by
//	            calendar, not by spend (business rule 2026-10-10: minted keys
//	            live 30 days; recharges RENEW, expired keys cannot be
//	            recharged). outranks "exhausted" and unlimited-ness: an
//	            expired UNLIMITED key is still just expired.
//	"exhausted" limited AND remaining <= 0 — cut off now, or overspent.
//	"active"    everything else, including UNLIMITED keys (a credit-limit-0
//	            key can never be exhausted by definition, so it stays
//	            active no matter how large Used grows).
//
// Deliberately plain string literals (not typed consts): handlers compare
// the literals in display/branch code and the strings ARE the product
// vocabulary of the fixed contract.
func (u *KeyUsage) Status() string {
	if !u.Info.IsActive {
		return "disabled"
	}
	if exp, has := u.Info.Expiry(); has && exp.Before(time.Now()) {
		return "expired"
	}
	if remaining, limited := u.Remaining(); limited && remaining <= 0 {
		return "exhausted"
	}
	return "active"
}

// ── POST /api/v1/apikey/create ─────────────────────────────────────────────
// The mint path: one sold key = one CreateKey call.

// CreateKey mints a new llmgate key (user realm). creditLimit is the
// absolute credit cap in dollars (1 credit = $1); 0 means UNLIMITED and is
// allowed, a negative value is a caller bug rejected before any HTTP call
// (ErrCreditLimitNegative) so a typo costs a local error instead of a
// server 400 and a dead round trip.
//
// WIRE SHAPE (invariant 3): expires encodes the key's expiry. A ZERO time
// omits the field from the JSON entirely (omitempty on an empty string —
// the same structural-omission trick as the no-accountId payload), and the
// verified behavior of this API is that an omitted expires_at yields a key
// that NEVER expires. A NON-zero time is sent as RFC3339 (millis optional
// server-side; this client sends the plain spelling). The bot's mint path
// always passes time.Now().Add(DefaultValidity) — every sold key lives
// 30 DAYS (business rule 2026-10-10) — while the zero time remains
// the caller's explicit "never expires" escape hatch (a zero time in the
// JSON body itself would mean "expired at the epoch": a silently dead
// key, which is exactly why omission is structural). is_active is
// hardcoded true — a key minted disabled cannot serve its buyer, and no
// caller of this client has a reason to mint one off.
//
// The returned Key.APIKey is the cleartext key, delivered by the server
// exactly ONCE (invariant 2): hand it to the buyer / store it hashed
// before the reference is dropped — it is not retrievable later except
// via RevealKey.
//
// SINGLE ATTEMPT, NO RETRY (the money guard): create is money-minting, so
// a blind retry after a timeout/5xx could mint a SECOND live key for one
// paid sale while the first becomes an unrecorded billable orphan. This
// method therefore sends exactly ONE create attempt (userSendOnce — no
// 5xx/network retry; the 401 re-login replay is kept, since a 401
// definitively rejected the first attempt and nothing was minted). The
// ambiguity a timeout leaves behind is handled by the CALLER's
// reconciliation (mintLlmgateKey): on an IsAmbiguous error it lists the
// account's keys, finds this order's deterministic name, and reveals THAT
// key — recovering a committed mint without ever minting twice.
func (c *Client) CreateKey(ctx context.Context, name string, creditLimit float64, expires time.Time) (*Key, error) {
	if creditLimit < 0 {
		return nil, ErrCreditLimitNegative
	}
	payload := struct {
		Name        string  `json:"name"`
		IsActive    bool    `json:"is_active"`
		CreditLimit float64 `json:"credit_limit"`
		// ExpiresAt is "" for a zero `expires` → the field is OMITTED from
		// the body (never-expire); otherwise the RFC3339 spelling above.
		ExpiresAt string `json:"expires_at,omitempty"`
	}{Name: name, IsActive: true, CreditLimit: creditLimit}
	if !expires.IsZero() {
		payload.ExpiresAt = expires.Format(time.RFC3339)
	}

	var key Key
	if err := c.userSendOnce(ctx, http.MethodPost, "/api/v1/apikey/create", payload, &key); err != nil {
		return nil, err
	}
	return &key, nil
}

// ── POST /api/v1/apikey/update ─────────────────────────────────────────────

// UpdateKey mutates an existing key (user realm): name, is_active,
// credit_limit and expires_at in one absolute write (invariant 4).
//
// WIRE SHAPE (invariant 4, verified live):
//   - name is ALWAYS sent. Omitting it on this endpoint CLEARS the stored
//     name — so this client cannot express "leave the name alone"; callers
//     must pass the current name (the handlers own it). name == "" is
//     therefore an explicit "blank the label" only when the caller really
//     asked for it, never an accident of a sparse struct.
//   - credit_limit is SET to the given value, NOT added to the old one: a
//     top-up call re-reads current usage and sends the new absolute cap.
//   - expires_at IS settable on update (verified live), encoded exactly
//     like CreateKey's: a zero time OMITS the field. The omission
//     semantics of THIS endpoint are UNVERIFIED (does an absent
//     expires_at clear the stored expiry, or leave it alone?); every
//     caller in this repo passes a CONCRETE time — the recharge path
//     renews to now+DefaultValidity (business rule 2026-10-10) — so a
//     zero time here is always the caller's deliberate choice, never a
//     default.
//
// The returned Key carries the post-update record with APIKey EMPTY (the
// server returns api_key_length only) and no trustworthy created_at (the
// field decodes as zeroed garbage — invariant 4); branch on neither.
func (c *Client) UpdateKey(ctx context.Context, id int64, name string, isActive bool, creditLimit float64, expires time.Time) (*Key, error) {
	if id <= 0 {
		return nil, ErrIDRequired
	}
	if creditLimit < 0 {
		return nil, ErrCreditLimitNegative
	}
	payload := struct {
		ID          int64   `json:"id"`
		Name        string  `json:"name"`
		IsActive    bool    `json:"is_active"`
		CreditLimit float64 `json:"credit_limit"`
		// ExpiresAt is "" for a zero `expires` → OMITTED (see the doc above:
		// unverified server-side, so callers must mean it).
		ExpiresAt string `json:"expires_at,omitempty"`
	}{ID: id, Name: name, IsActive: isActive, CreditLimit: creditLimit}
	if !expires.IsZero() {
		payload.ExpiresAt = expires.Format(time.RFC3339)
	}

	var key Key
	if err := c.userSend(ctx, http.MethodPost, "/api/v1/apikey/update", payload, &key); err != nil {
		return nil, err
	}
	return &key, nil
}

// ── DELETE /api/v1/apikey/delete/{id} ──────────────────────────────────────

// DeleteKey removes a key permanently (user realm). This is the one
// endpoint whose 2xx carries NO data field at all — send's null-tolerant
// wrapper decode (out == nil here) turns that into a plain nil error
// (invariant 6). A 404 maps to ErrNotFound: the caller acted on an id the
// server no longer knows (already deleted — the delete still "succeeded"
// in effect, but only the caller can decide whether that is success for
// THEM, so the distinct sentinel travels up instead of a silent nil).
func (c *Client) DeleteKey(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrIDRequired
	}
	return c.userSend(ctx, http.MethodDelete, "/api/v1/apikey/delete/"+fmt.Sprint(id), nil, nil)
}

// ── GET /api/v1/apikey/list ────────────────────────────────────────────────

// ListKeys returns every key on the account (user realm), newest first as
// the server orders it. The records are metadata ONLY: no cleartext key
// values (invariant 2 — list reports api_key_length instead) and no usage
// numbers; a caller needing usage for one of them follows up with
// KeyStats. An empty account is an empty slice + nil error, not an error:
// "no keys" is a normal, representable state.
func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	var keys []Key
	if err := c.userSend(ctx, http.MethodGet, "/api/v1/apikey/list", nil, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// ── POST /api/v1/apikey/{id}/reveal ────────────────────────────────────────

// RevealKey fetches the cleartext key for an existing record (user realm).
// The method is POST, not GET — the live API answers a GET with 405
// (package doc); this client sends POST and never falls back. The
// returned Key.APIKey is the full cleartext key (invariant 2): treat it
// with the same one-time care as a create response.
func (c *Client) RevealKey(ctx context.Context, id int64) (*Key, error) {
	if id <= 0 {
		return nil, ErrIDRequired
	}
	var key Key
	if err := c.userSend(ctx, http.MethodPost, "/api/v1/apikey/"+fmt.Sprint(id)+"/reveal", nil, &key); err != nil {
		return nil, err
	}
	return &key, nil
}

// ── GET /api/v1/apikey/stats (KEY realm) ───────────────────────────────────

// KeyStats reads a key's record + lifetime usage authenticated AS that key
// (invariant 1: Bearer = the customer's own sk-llmgate-... key, NOT the
// login JWT). This is why an empty customerKey is rejected before any
// HTTP call — there is no token to fall back on, and sending an empty
// Bearer would only buy a confusing 401.
//
// A 401 here is the KEY being bad (revoked, mistyped, retired pool key):
// this client NEVER re-login-retries it (a fresh login cannot fix the
// customer's credential) — the *APIError surfaces to the caller as-is.
// Because the key realm never logs in, KeyStats works on a client whose
// Username/Password are unset; only BaseURL is required.
//
// The returned numbers lag reality by ~30-60s (invariant 5): async
// aggregation server-side. See KeyUsage for the operator-facing caveat.
func (c *Client) KeyStats(ctx context.Context, customerKey string) (*KeyUsage, error) {
	if c.cfg.BaseURL == "" {
		return nil, ErrNotConfigured
	}
	if customerKey == "" {
		return nil, errCustomerKeyRequired
	}
	var usage KeyUsage
	if err := c.send(ctx, http.MethodGet, "/api/v1/apikey/stats", bearer(customerKey), nil, &usage); err != nil {
		return nil, err
	}
	return &usage, nil
}
