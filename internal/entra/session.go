// Device-code login, session status and silent refresh — the Go twin of
// entra_auth.py (/Users/z/Desktop/Kiro/entra_auth.py). Unlike every other
// file in this package, nothing here shells out to a child process: the
// device-code flow is a plain OAuth2 device-authorization grant (RFC 8628)
// against the Microsoft identity platform's public v2.0 endpoints, and the
// admin completes sign-in on THEIR OWN device by visiting a URL — there is
// no browser automation happening in this process at all, so
// architecture-go.md §6 ("browser/Playwright/CloakBrowser/boto3 work stays
// in child processes") does not apply to this piece the way it does to the
// three Graph-touching scripts in enterpriseapp.go/users.go.
//
// Client id ("04b07795-8ddb-461a-bbee-02f9e1bf7b46"), authority
// ("/organizations" or the pinned target tenant), and scope
// ("https://graph.microsoft.com/.default") are copied verbatim from
// entra_auth.py:32-47 — this is the well-known Azure CLI public client,
// already pre-authorized for Graph via ".default" with no app registration
// needed, same identity the Python original authenticates as.
//
// # KNOWN GAP — session-cache format is NOT MSAL-compatible
//
// entra_auth.py persists an msal.SerializableTokenCache to disk
// (entra_auth.py:90-121), and the three Python child scripts this package's
// sibling files (enterpriseapp.go/users.go) run — entra_enterprise_app.py,
// entra_create_users.py, and aws_idc.py — each call entra_auth.get_token()
// THEMSELVES to read that SAME cache file and obtain a Graph bearer token;
// none of them receive a token from the bot via argv/env
// (py_entra_spec.md "Subprocess contract (general)": "None of the three
// scripts receive a Graph token via argv/env from the bot").
//
// This package does NOT attempt to reproduce MSAL's internal unified cache
// schema (its exact cache-key composition/hashing rules are undocumented
// enough, and unverifiable in this environment without a live Azure AD
// tenant and the real msal-python library to round-trip against, that a
// best-effort reimplementation risks being SILENTLY wrong — the three child
// scripts would then fail to find a token at all despite the Go bot
// reporting a successful login). Instead this package writes its OWN
// documented JSON schema (sessionCache, cache.go) to a DELIBERATELY
// different filename (entra_session.json, not msal_cache.bin) in the SAME
// directory (.entra-auth, matching entra_auth.py's CACHE_DIR convention) so
// there is no false implication of wire compatibility.
//
// CONSEQUENCE: as of this package, a login performed here is NOT yet
// readable by a vendored (unmodified) entra_enterprise_app.py /
// entra_create_users.py / aws_idc.py. None of those three scripts exist in
// this Go repo yet (only aws_idc_provision_cli.py, an unrelated CLI, is
// vendored so far) — bridging this gap is an explicit OPEN DECISION for
// whoever wires the full feature end-to-end. Three options, in order of
// how much they change: (a) have those child scripts accept a Graph token
// via an env var this Go bot supplies directly (GetAccessToken below),
// bypassing entra_auth.get_token() entirely — cleanest, no duplicated OAuth
// logic across two languages, but changes the Python scripts' own contract;
// (b) implement genuine MSAL wire-format compatibility here, verified
// against a live tenant + the real msal-python library before shipping;
// (c) keep login/status/logout as a small Python subprocess too (reusing
// entra_auth.py unmodified), so the SAME library that reads the cache is
// the one that wrote it. This file intentionally does not pick one — that
// choice belongs to whoever owns the end-to-end wiring.
package entra

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wellKnownClientID is the Azure CLI public client (entra_auth.py:34) — see
// package doc.
const wellKnownClientID = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"

// graphScope is the only scope requested (entra_auth.py:47) — see package
// doc for why ".default" is required rather than explicit delegated scopes.
const graphScope = "https://graph.microsoft.com/.default"

// defaultDeviceCodeExpiry is used only if a devicecode response omits
// expires_in (defensive; Microsoft's endpoint always sends one) — MSAL's own
// typical default, matching the Python bot's own comment
// (py_entra_spec.md §1 step 4: "MSAL default expires_in is typically 900s").
const defaultDeviceCodeExpiry = 900 * time.Second

// defaultPollInterval mirrors the same defensive default for a devicecode
// response that omits "interval".
const defaultPollInterval = 5 * time.Second

// ErrDeviceCodeExpired is returned by CompleteDeviceCode when the flow's own
// expiry elapsed before the admin finished sign-in — the Go twin of MSAL
// raising RuntimeError("expired_token...") inside acquire_token_by_device_flow
// (entra_auth.py:244-254 / py_entra_spec.md §1 step 4).
var ErrDeviceCodeExpired = errors.New("entra: device code expired before sign-in completed")

// errAuthPending / errSlowDown are internal poll-loop signals, never
// returned to a caller — RFC 8628's "authorization_pending"/"slow_down"
// error codes, both meaning "keep polling", not a failure.
var errAuthPending = errors.New("entra: authorization pending")
var errSlowDown = errors.New("entra: slow down")

// Client is the seam for the Microsoft identity platform HTTP calls this
// file makes. The zero value talks to the real endpoints; tests set
// AuthorityBase/GraphBase to an httptest.Server so no network call ever
// reaches login.microsoftonline.com or graph.microsoft.com during `go test`.
type Client struct {
	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client
	// AuthorityBase defaults to "https://login.microsoftonline.com".
	AuthorityBase string
	// GraphBase defaults to "https://graph.microsoft.com/v1.0".
	GraphBase string
}

func (c Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Client) graphBase() string {
	if c.GraphBase != "" {
		return c.GraphBase
	}
	return "https://graph.microsoft.com/v1.0"
}

// authorityFor mirrors entra_auth._authority (entra_auth.py:84-87): the
// pinned target tenant if given, else the generic "/organizations" authority
// (work/school accounts, any tenant).
func (c Client) authorityFor(tenant string) string {
	base := c.AuthorityBase
	if base == "" {
		base = "https://login.microsoftonline.com"
	}
	t := strings.TrimSpace(tenant)
	if t == "" {
		t = "organizations"
	}
	return base + "/" + t
}

// postForm POSTs a application/x-www-form-urlencoded body and decodes the
// JSON response body into out (when the body is non-empty). Returns the
// HTTP status code alongside any transport/decode error so callers can
// branch on Microsoft's error-code-carrying 4xx responses (device-flow
// polling depends on reading the BODY of a 400, not just the status).
func (c Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if len(body) > 0 {
		if uerr := json.Unmarshal(body, out); uerr != nil {
			return resp.StatusCode, fmt.Errorf("entra: malformed response from %s: %w", endpoint, uerr)
		}
	}
	return resp.StatusCode, nil
}

// deviceCodeResponse is the Microsoft identity platform's /devicecode
// response shape (RFC 8628 §3.2), matching the fields MSAL's flow dict
// surfaces (entra_auth.py:229-241: user_code, verification_uri, expires_in,
// message).
type deviceCodeResponse struct {
	DeviceCode       string `json:"device_code"`
	UserCode         string `json:"user_code"`
	VerificationURI  string `json:"verification_uri"`
	ExpiresIn        int    `json:"expires_in"`
	Interval         int    `json:"interval"`
	Message          string `json:"message"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// tokenResponse is the /token endpoint's response shape for BOTH the
// device_code and refresh_token grants — a successful body carries
// access_token/refresh_token/id_token; a pending/failed body carries
// error/error_description instead (RFC 8628 §3.5).
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// DeviceCodeFlow is the opaque in-memory state between StartDeviceCode and
// CompleteDeviceCode — the Go twin of MSAL's flow dict (entra_auth.py:236,
// "flow['_app'] = app # reuse this app/cache instance"). deviceCode and
// authority are unexported: a caller (the handler layer) holds this value
// across the two calls without ever needing to inspect it, exactly like the
// Python bot passes the flow dict opaquely into a thread.
type DeviceCodeFlow struct {
	UserCode        string
	VerificationURI string
	Message         string
	ExpiresIn       time.Duration
	Interval        time.Duration
	deviceCode      string
	authority       string
}

// StartDeviceCode begins a device-code login against the given tenant (its
// id or primary domain — "" means the generic /organizations authority,
// matching a fresh, never-configured session). Port of
// entra_auth.start_device_flow (entra_auth.py:229-241): the returned flow
// carries the user_code + verification_uri to show the admin, and expires_in
// (~15 min typically) as the flow's own budget. Failure (e.g. no user_code
// in the response) is the Go twin of MSAL's RuntimeError.
func (c Client) StartDeviceCode(ctx context.Context, tenant string) (DeviceCodeFlow, error) {
	authority := c.authorityFor(tenant)
	form := url.Values{"client_id": {wellKnownClientID}, "scope": {graphScope}}
	var resp deviceCodeResponse
	status, err := c.postForm(ctxOrBackground(ctx), authority+"/oauth2/v2.0/devicecode", form, &resp)
	if err != nil {
		return DeviceCodeFlow{}, err
	}
	if status != http.StatusOK || resp.UserCode == "" {
		desc := resp.ErrorDescription
		if desc == "" {
			desc = resp.Error
		}
		if desc == "" {
			desc = fmt.Sprintf("device flow init failed (http %d)", status)
		}
		return DeviceCodeFlow{}, fmt.Errorf("entra: %s", desc)
	}
	expiresIn := time.Duration(resp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = defaultDeviceCodeExpiry
	}
	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return DeviceCodeFlow{
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		Message:         resp.Message,
		ExpiresIn:       expiresIn,
		Interval:        interval,
		deviceCode:      resp.DeviceCode,
		authority:       authority,
	}, nil
}

// LoginIdentity is what a completed login reveals about the signed-in admin
// and tenant — the Go twin of complete_device_flow's return dict
// (entra_auth.py:262-266).
type LoginIdentity struct {
	TenantID     string
	TenantDomain string
	Username     string
}

// DeviceCodeProgressFunc is called once per poll tick while
// CompleteDeviceCode waits, so the handler can edit a spinner message
// ("still waiting… Xm left") without this package importing internal/tg —
// the same dependency-inversion shape as provision.ProgressFunc and
// internal/awsidc's onCreated/onActivated callbacks.
type DeviceCodeProgressFunc func(elapsed, remaining time.Duration)

// CompleteDeviceCode blocks, polling the token endpoint at flow's own
// interval, until the admin finishes sign-in, declines, or the flow expires
// — the Go twin of entra_auth.complete_device_flow
// (entra_auth.py:244-266), EXCEPT this version is context-cancellable
// (py_entra_spec.md §1 step 4 notes the Python original has "NO separate
// bot-side timeout/cancel — the whole wait is inside MSAL"; an earlier Go
// port slice was found hanging past Shutdown for exactly this class of bug,
// per the task brief, so ctx cancellation here is load-bearing, not
// decorative). On success the session is persisted via saveSessionCache
// (cache.go) before returning, mirroring entra_auth.py:255's _save_cache
// call.
func (c Client) CompleteDeviceCode(ctx context.Context, flow DeviceCodeFlow, cacheDir string, onProgress DeviceCodeProgressFunc) (LoginIdentity, error) {
	ctx = ctxOrBackground(ctx)
	deadline := time.Now().Add(flow.ExpiresIn)
	interval := flow.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return LoginIdentity{}, ctx.Err()
		case now := <-ticker.C:
			if now.After(deadline) {
				return LoginIdentity{}, ErrDeviceCodeExpired
			}
			if onProgress != nil {
				onProgress(flow.ExpiresIn-time.Until(deadline), time.Until(deadline))
			}
			tok, perr := c.pollToken(ctx, flow.authority, flow.deviceCode)
			switch {
			case perr == nil:
				cache, serr := c.persistToken(ctx, cacheDir, tok, "", "")
				if serr != nil {
					return LoginIdentity{}, serr
				}
				return LoginIdentity{TenantID: cache.TenantID, TenantDomain: cache.TenantDomain, Username: cache.Username}, nil
			case errors.Is(perr, errAuthPending):
				continue
			case errors.Is(perr, errSlowDown):
				interval += 5 * time.Second
				ticker.Reset(interval)
				continue
			default:
				return LoginIdentity{}, perr
			}
		}
	}
}

// pollToken makes ONE device_code-grant poll. A pending/slow_down response
// is reported via the internal sentinel errors above (NOT a real failure —
// the caller's poll loop keeps going); any other error response, or a
// transport failure, is terminal.
func (c Client) pollToken(ctx context.Context, authority, deviceCode string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {wellKnownClientID},
		"device_code": {deviceCode},
	}
	var tok tokenResponse
	status, err := c.postForm(ctx, authority+"/oauth2/v2.0/token", form, &tok)
	if err != nil {
		return tokenResponse{}, err
	}
	if status == http.StatusOK && tok.AccessToken != "" {
		return tok, nil
	}
	switch tok.Error {
	case "authorization_pending":
		return tokenResponse{}, errAuthPending
	case "slow_down":
		return tokenResponse{}, errSlowDown
	default:
		desc := tok.ErrorDescription
		if desc == "" {
			desc = tok.Error
		}
		if desc == "" {
			desc = fmt.Sprintf("device flow failed (http %d)", status)
		}
		return tokenResponse{}, fmt.Errorf("entra: %s", desc)
	}
}

// refreshAccessToken exchanges a refresh token for a fresh access token —
// the HTTP call underneath MSAL's acquire_token_silent
// (entra_auth.py:199-216 get_token / entra_auth.py:163-196 session_status).
func (c Client) refreshAccessToken(ctx context.Context, tenant, refreshToken string) (tokenResponse, error) {
	authority := c.authorityFor(tenant)
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {wellKnownClientID},
		"refresh_token": {refreshToken},
		"scope":         {graphScope},
	}
	var tok tokenResponse
	status, err := c.postForm(ctx, authority+"/oauth2/v2.0/token", form, &tok)
	if err != nil {
		return tokenResponse{}, err
	}
	if status != http.StatusOK || tok.AccessToken == "" {
		desc := tok.ErrorDescription
		if desc == "" {
			desc = tok.Error
		}
		if desc == "" {
			desc = fmt.Sprintf("refresh failed (http %d)", status)
		}
		return tokenResponse{}, fmt.Errorf("entra: %s", desc)
	}
	return tok, nil
}

// graphOrgResponse is the subset of Graph GET /organization this package
// needs — the AUTHORITATIVE tenant id + primary domain source, mirroring
// entra_auth._graph_org's own doc (entra_auth.py:132-149): "the token's UPN
// domain and the id_token tid claim are unreliable here".
type graphOrgResponse struct {
	Value []struct {
		ID              string `json:"id"`
		VerifiedDomains []struct {
			Name      string `json:"name"`
			IsDefault bool   `json:"isDefault"`
		} `json:"verifiedDomains"`
	} `json:"value"`
}

// graphOrg calls Graph /organization with accessToken, returning the
// tenant id and its default verified domain. Best-effort by design (the
// Python original wraps this in try/except and falls back to id_token
// claims, entra_auth.py:186-189/258-261) — callers here likewise treat a
// graphOrg failure as non-fatal and fall back to whatever claims-derived
// tenant id they already have.
func (c Client) graphOrg(ctx context.Context, accessToken string) (tenantID, primaryDomain string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.graphBase()+"/organization", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("entra: graph /organization returned http %d", resp.StatusCode)
	}
	var out graphOrgResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if len(out.Value) == 0 {
		return "", "", errors.New("entra: no organization returned")
	}
	org := out.Value[0]
	for _, d := range org.VerifiedDomains {
		if d.IsDefault {
			return org.ID, d.Name, nil
		}
	}
	return org.ID, "", nil
}

// decodeIDTokenClaims reads the tid/preferred_username/upn claims out of a
// JWT id_token WITHOUT verifying its signature — the token arrived directly
// from Microsoft's token endpoint over TLS, so there is nothing to verify
// against locally; MSAL itself does not verify id_token signatures on this
// path either. A malformed/missing token yields all-empty results rather
// than an error, since id_token is only ever used as a fallback here (Graph
// /organization is authoritative — see graphOrg's doc).
func decodeIDTokenClaims(idToken string) (tid, preferredUsername, upn string) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}
	var claims struct {
		TID               string `json:"tid"`
		PreferredUsername string `json:"preferred_username"`
		UPN               string `json:"upn"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", ""
	}
	return claims.TID, claims.PreferredUsername, claims.UPN
}

// persistToken builds a sessionCache from a token response and saves it.
// fallbackUsername/fallbackRefreshToken carry values from an EXISTING
// session on a silent-refresh call (Python's session_status keeps the
// account's stored username rather than re-deriving it every refresh,
// entra_auth.py:179-196) — pass "" for both on a fresh login. A token
// response's own refresh_token is preferred when present; some refresh
// calls omit a new one, in which case the existing one is kept.
func (c Client) persistToken(ctx context.Context, cacheDir string, tok tokenResponse, fallbackUsername, fallbackRefreshToken string) (sessionCache, error) {
	tid, preferredUsername, upn := decodeIDTokenClaims(tok.IDToken)
	username := fallbackUsername
	if username == "" {
		username = preferredUsername
		if username == "" {
			username = upn
		}
	}
	tenantID, tenantDomain := tid, ""
	if gotTID, gotDomain, gerr := c.graphOrg(ctx, tok.AccessToken); gerr == nil {
		tenantID, tenantDomain = gotTID, gotDomain
	}
	refreshToken := tok.RefreshToken
	if refreshToken == "" {
		refreshToken = fallbackRefreshToken
	}
	expiresIn := time.Duration(tok.ExpiresIn) * time.Second
	cache := sessionCache{
		Schema:       sessionCacheSchema,
		Username:     username,
		TenantID:     tenantID,
		TenantDomain: tenantDomain,
		AccessToken:  tok.AccessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(expiresIn),
	}
	if err := saveSessionCache(cacheDir, cache); err != nil {
		return sessionCache{}, err
	}
	return cache, nil
}

// SessionState is session_status()'s three-way result (entra_auth.py:
// 163-196).
type SessionState string

const (
	SessionNotLoggedIn SessionState = "not_logged_in"
	SessionExpired     SessionState = "expired"
	SessionValid       SessionState = "valid"
)

// SessionStatusResult mirrors session_status()'s return dict shape
// (entra_auth.py:167-196).
type SessionStatusResult struct {
	State        SessionState
	Username     string
	TenantID     string
	TenantDomain string
	ExpiresIn    time.Duration
}

// SessionStatus describes the cached session WITHOUT prompting a sign-in —
// port of entra_auth.session_status (entra_auth.py:163-196). Network-free
// on the not-logged-in path (no cache file at all) AND when the cached
// access token has not yet expired (the Python original still touches the
// network in that case, per entra_auth.py:155-160's own comment about
// building an MSAL app; this Go port's cache-hit-first check is a
// deliberate, flagged improvement — see session.go's package doc — not a
// behavioral regression). A refresh_token present but rejected (revoked
// session, or the refresh call itself fails) reports "expired", matching
// "'expired' in practice only means the admin was signed out or the refresh
// token was revoked" (py_entra_spec.md §1's session-expiry note).
func (c Client) SessionStatus(ctx context.Context, cacheDir string) (SessionStatusResult, error) {
	ctx = ctxOrBackground(ctx)
	cache, ok, err := loadSessionCache(cacheDir)
	if err != nil {
		return SessionStatusResult{}, err
	}
	if !ok {
		return SessionStatusResult{State: SessionNotLoggedIn}, nil
	}
	if time.Now().Before(cache.ExpiresAt) {
		return SessionStatusResult{
			State: SessionValid, Username: cache.Username,
			TenantID: cache.TenantID, TenantDomain: cache.TenantDomain,
			ExpiresIn: time.Until(cache.ExpiresAt),
		}, nil
	}
	if cache.RefreshToken == "" {
		return SessionStatusResult{State: SessionExpired, Username: cache.Username}, nil
	}
	tenant, terr := GetTenant(cacheDir)
	if terr != nil {
		return SessionStatusResult{}, terr
	}
	tok, rerr := c.refreshAccessToken(ctx, tenant, cache.RefreshToken)
	if rerr != nil || tok.AccessToken == "" {
		return SessionStatusResult{State: SessionExpired, Username: cache.Username}, nil
	}
	newCache, perr := c.persistToken(ctx, cacheDir, tok, cache.Username, cache.RefreshToken)
	if perr != nil {
		return SessionStatusResult{}, perr
	}
	return SessionStatusResult{
		State: SessionValid, Username: newCache.Username,
		TenantID: newCache.TenantID, TenantDomain: newCache.TenantDomain,
		ExpiresIn: time.Until(newCache.ExpiresAt),
	}, nil
}

// GetAccessToken returns a valid Graph access token via silent refresh, or
// "" when there is no usable session — port of entra_auth.get_token
// (entra_auth.py:199-216). Network-free when not logged in. A refresh
// failure is reported as ("", nil) — NOT an error — matching get_token's own
// "return None" on any silent-refresh failure; callers should treat an
// empty string as "not logged in", same as the Python original's None.
func (c Client) GetAccessToken(ctx context.Context, cacheDir string) (string, error) {
	ctx = ctxOrBackground(ctx)
	cache, ok, err := loadSessionCache(cacheDir)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	if time.Now().Before(cache.ExpiresAt) {
		return cache.AccessToken, nil
	}
	if cache.RefreshToken == "" {
		return "", nil
	}
	tenant, terr := GetTenant(cacheDir)
	if terr != nil {
		return "", terr
	}
	tok, rerr := c.refreshAccessToken(ctx, tenant, cache.RefreshToken)
	if rerr != nil || tok.AccessToken == "" {
		return "", nil
	}
	newCache, perr := c.persistToken(ctx, cacheDir, tok, cache.Username, cache.RefreshToken)
	if perr != nil {
		return "", perr
	}
	return newCache.AccessToken, nil
}
