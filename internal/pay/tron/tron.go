// Package tron ports tron_pay.py (Kiro Python original, 212 lines) — USDT
// TRC20 (TRON) payments via the free TronGrid HTTP API.
//
// Twin of the bep20 package, but for the TRON network. The customer sends
// USDT (TRC20) to the seller's TRON address. On-chain transfers carry NO
// memo/order id, so — exactly like bep20 / binance — each pending order is
// assigned a UNIQUE USDT amount and a credit is recognised by matching an
// incoming transfer of that exact amount to the seller's address
// (tron_pay.py:2-8).
//
// Confirmation source: TronGrid's TRC20 transactions endpoint
// GET /v1/accounts/{address}/transactions/trc20 with only_to=true and
// only_confirmed=true (so only finalised transfers are returned — TRON
// finality is ~19 blocks / ~1 min, handled server-side, so we need no manual
// block-depth counting like BSC's eth_getLogs path). Stdlib-only HTTP,
// mirroring bep20.go (tron_pay.py:10-14).
//
// Token: Tether USD TRC20 TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t, 6 decimals (NOT
// 18 like BSC-USD). `value` in the API response is the integer token amount
// in base units ("sun"), e.g. 105 USDT -> "105000000" (tron_pay.py:16-18).
//
// A free TronGrid tier works without a key; set TRON_API_KEY for a higher
// rate limit (sent as the TRON-PRO-API-KEY header, tron_pay.py:20-21).
package tron

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// USDTContract is the USDT TRC20 contract address (Tether USD on TRON,
// tron_pay.py:33).
const USDTContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// TokenDecimals is USDT's on-chain decimals on TRON (tron_pay.py:34).
const TokenDecimals = 6

const httpTimeout = 20 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

// DefaultAPIBase / DefaultAmountDecimals / DefaultMatchTol / DefaultOffsetStep
// / DefaultPageLimit are the compiled-in fallbacks for the knobs below —
// named so a caller resolving one of these through config.Config (rather
// than through this package's own raw env read; see Address's doc comment)
// has the same default to fall back to without hardcoding a second copy of
// the literal.
const (
	DefaultAPIBase        = "https://api.trongrid.io"
	DefaultAmountDecimals = 5
	DefaultMatchTol       = 50
	DefaultOffsetStep     = 101
	DefaultPageLimit      = 200
)

// apiBase is the TronGrid REST base (override for a private node via
// TRON_API_BASE, tron_pay.py:37). Raw .env/ambient read only — same caveat
// as Address()/PageLimit(): a production caller with a *config.Config
// available should resolve TRON_API_BASE through it and pass the result into
// apiGetConf/GetIncomingTransfersConf instead. apiGet delegates here so it
// keeps working for callers with no cfg to resolve from.
func apiBase() string {
	if v := os.Getenv("TRON_API_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultAPIBase
}

// AmountDecimals is the precision of the per-order UNIQUE amount. 5 = 1e-5
// USDT, raised from the Python original's 2 (cents) by the hands-free-deposits
// ruling — see bep20.AmountDecimals for the reasoning; this mirrors it so the
// SHARED unique-amount picker sees one precision across both rails and its
// cross-provider collision set needs no rescaling. TRC20 USDT carries exactly 6
// decimals on-chain, so 5 is representable and valToUnits' 6->5 floor discards
// at most 0.000009 USDT — under a fifth of one amount unit, and a fiftieth of
// the match tolerance (tron_pay.py:38-40).
func AmountDecimals() int {
	return envInt("TRON_AMOUNT_DECIMALS", DefaultAmountDecimals)
}

// AmountMatchTol / AmountOffsetStep: a payment within AmountMatchTol units of
// the expected amount matches; the unique-amount picker spaces concurrent
// same-price orders by AmountOffsetStep units (>= 2*tol+1) so the ±tol bands
// never overlap. Units are at AmountDecimals precision. Default tol = 50
// (0.00050 USDT); step = 101 (0.00101 USDT) = exactly 2*50+1, which keeps
// same-price orders unambiguous while fitting ten slots in the
// 0.01000-0.01999 jitter window. This is the EXACT path only — fee forgiveness
// is a separate, ambiguity-checked rule in bands/match.go; see
// bep20.AmountMatchTol for the full picture (tron_pay.py:41-54).
func AmountMatchTol() int {
	return envInt("TRON_MATCH_TOL", DefaultMatchTol)
}

func AmountOffsetStep() int {
	return envInt("TRON_OFFSET_STEP", DefaultOffsetStep)
}

// PageLimit reads the RAW .env/ambient TRON_PAGE_LIMIT — nothing more, same
// caveat as Address()'s doc comment: it never sees a .env-only value under
// systemd (no EnvironmentFile in production, config.Load's doc comment). A
// production caller with a *config.Config available should resolve
// TRON_PAGE_LIMIT through it and pass the result into
// GetIncomingTransfersConf instead of letting this package read it.
// GetIncomingTransfers below delegates to GetIncomingTransfersConf using this
// value, and survives only as the low-level primitive and its default-argument
// caller.
//
// The max transfers to pull per poll cycle (TronGrid caps at 200,
// tron_pay.py:55-57).
func PageLimit() int {
	return envInt("TRON_PAGE_LIMIT", DefaultPageLimit)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// addrRE is a TRON base58 address shape check: starts 'T', 34 chars total,
// Bitcoin base58 alphabet (no 0 O I l). Not a base58check checksum verify —
// enough to reject a fat-fingered admin entry; TronGrid rejects a truly
// invalid address (tron_pay.py:59-62).
var addrRE = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)

// Address reads the RAW .env/ambient TRON_ADDRESS ("" if unset) — nothing
// more. It does NOT see an admin's settings.json address change (task #17):
// unlike the Python original, which mutated os.environ in-process on every
// /settron (apply_saved_tron, telegram_bot.py:5309-5321) so a bare getenv
// stayed live, Go's config.Config deliberately never touches the real
// process environment (config.Load's doc comment) — the admin-set value only
// ever lives in settings.json. The customer-facing address, deposit
// matching, and Configured() gate must all resolve through
// config.Config.TRONAddress() (settings.json overriding this env snapshot),
// not this function; internal/handlers passes that resolved value into the
// functions in this package that need it (GetIncomingTransfers) instead of
// letting them call Address() themselves. This function survives only as the
// low-level env primitive those call sites build on — do not call it
// directly for anything customer-facing.
func Address() string {
	return strings.TrimSpace(os.Getenv("TRON_ADDRESS"))
}

// ValidAddress reports whether a looks like a TRON (base58) address (T + 33
// base58 chars, tron_pay.py:71-73).
func ValidAddress(a string) bool {
	a = strings.TrimSpace(a)
	return a != "" && addrRE.MatchString(a)
}

// Configured reports whether Address() (the raw env snapshot, NOT
// settings.json — see Address's doc comment) looks like a valid receiving
// address. Kept for parity with tron_pay.py:76-78; internal/handlers gates
// the pay button on ValidAddress(config.Config.TRONAddress()) instead, so an
// admin-set address is honoured (task #17).
func Configured() bool {
	return ValidAddress(Address())
}

// QRURL is the optional hosted image URL of the seller's address QR ("" if
// unset, tron_pay.py:81-83).
func QRURL() string {
	return strings.TrimSpace(os.Getenv("TRON_QR_URL"))
}

// QRPath is the optional local file path of the seller's address QR ("" if
// unset, tron_pay.py:86-88).
func QRPath() string {
	return strings.TrimSpace(os.Getenv("TRON_QR_PATH"))
}

// AmountUnits returns the integer amount-unit value of a USDT amount string
// at AmountDecimals precision (e.g. "5.23" -> 523 at 2 decimals). Uses
// big.Rat for exact decimal-string parsing, avoiding float drift, mirroring
// Python's Decimal math (tron_pay.py:98-101).
func AmountUnits(amountStr string) (int64, error) {
	r, ok := new(big.Rat).SetString(amountStr)
	if !ok {
		return 0, fmt.Errorf("tron: invalid amount %q", amountStr)
	}
	scale := new(big.Rat).SetInt(pow10(AmountDecimals()))
	r.Mul(r, scale)
	return ratToIntegral(r), nil
}

// valToUnits floors an on-chain 6-dec `value` (base units) to AmountDecimals
// units. Floor (not round) so a dust OVERPAY still matches the order and a
// dust UNDERPAY never credits a higher amount (tron_pay.py:104-108).
func valToUnits(value *big.Int) int64 {
	divisor := pow10(TokenDecimals - AmountDecimals())
	q := new(big.Int).Div(value, divisor) // big.Int.Div floors for positive operands
	return q.Int64()
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// ratToIntegral rounds a big.Rat to the nearest integer, half-away-from-zero
// (mirrors Decimal.to_integral_value()'s default ROUND_HALF_EVEN closely
// enough for the cent-scale amounts this handles; ties are not a realistic
// input here since amounts are user-entered decimal strings, not computed).
func ratToIntegral(r *big.Rat) int64 {
	num, den := r.Num(), r.Denom() // big.Rat guarantees Denom() > 0
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem) // truncated division; rem's sign matches num's
	rem.Abs(rem)
	if new(big.Int).Lsh(rem, 1).Cmp(den) >= 0 { // 2*|rem| >= den -> round away from zero
		if num.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// apiGet is apiGetConf using the raw-env apiBase() — kept for callers with
// no *config.Config to resolve TRON_API_BASE through (see apiBase's doc
// comment).
//
// apiKey is the caller-resolved TronGrid API key (sent as the
// TRON-PRO-API-KEY header, raising TronGrid's rate limit; "" sends no
// header, using the free keyless tier). This package deliberately does not
// read TRON_API_KEY itself — see Address's doc comment for why — so the
// caller must resolve it (config.Config.TRONAPIKey()) and pass it in.
func apiGet(path string, params url.Values, apiKey string) (map[string]any, error) {
	return apiGetConf(path, params, apiKey, apiBase())
}

// apiGetConf is apiGet with the TronGrid REST base injected explicitly
// rather than read from apiBase()'s raw env.
func apiGetConf(path string, params url.Values, apiKey, base string) (map[string]any, error) {
	key := strings.TrimSpace(apiKey)
	reqURL := fmt.Sprintf("%s%s?%s", base, path, params.Encode())
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kiro-bot")
	if key != "" {
		req.Header.Set("TRON-PRO-API-KEY", key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("TronGrid GET %s failed: %d %s", path, resp.StatusCode, snippet)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tron: decode response: %w", err)
	}
	return out, nil
}

// Transfer is one confirmed incoming USDT (TRC20) transfer to our address
// (tron_pay.py:130-131): {id: txhash, frm, value (base-units), units
// (floored AmountDecimals value), ts (block ms)}.
type Transfer struct {
	ID    string
	Frm   string
	Value *big.Int
	Units int64
	TS    int64
}

// GetIncomingTransfers is GetIncomingTransfersConf using the raw-env
// PageLimit()/apiBase() — kept for callers with no *config.Config to
// resolve TRON_PAGE_LIMIT/TRON_API_BASE through (see their doc comments).
func GetIncomingTransfers(addr string, minTSMS int64, apiKey string) ([]Transfer, error) {
	return GetIncomingTransfersConf(addr, minTSMS, apiKey, PageLimit(), apiBase())
}

// GetIncomingTransfersConf returns confirmed incoming USDT (TRC20) transfers
// to addr since minTSMS (epoch ms). Only `transfer`-type, only_to,
// only_confirmed — so every row is a finalised credit to us
// (tron_pay.py:128-162). apiKey is forwarded to apiGetConf — see its doc
// comment. pageLimit/base are the caller-resolved TRON_PAGE_LIMIT/
// TRON_API_BASE (see PageLimit's/apiBase's doc comments) rather than read
// from raw env here.
//
// THE WHOLE WINDOW, not one page. pageLimit sizes a page; this follows
// TronGrid's fingerprint continuation until the window is exhausted (or the
// maxTransferPages guard fires, which is logged). That matters because the sort
// is ASCENDING: with a single fixed page the transfers dropped are the newest
// ones, which are precisely the payments a customer is waiting to be credited
// for. Flipping the sort would not have fixed it — it would only have moved the
// loss to the oldest end of the window.
func GetIncomingTransfersConf(addr string, minTSMS int64, apiKey string, pageLimit int, base string) ([]Transfer, error) {
	params := url.Values{}
	params.Set("only_to", "true")
	params.Set("only_confirmed", "true")
	params.Set("contract_address", USDTContract)
	params.Set("min_timestamp", strconv.FormatInt(minTSMS, 10))
	params.Set("limit", strconv.Itoa(pageLimit))
	params.Set("order_by", "block_timestamp,asc")

	path := fmt.Sprintf("/v1/accounts/%s/transactions/trc20", addr)
	var out []Transfer
	for page := 1; ; page++ {
		d, err := apiGetConf(path, params, apiKey, base)
		if err != nil {
			return nil, err
		}
		success, _ := d["success"].(bool)
		if !success {
			if page > 1 {
				// A window half-read must NEVER be handed back as if it were
				// complete: the caller would treat the missing tail as "no such
				// payment arrived". Fail the whole fetch and let the next tick
				// start over.
				return nil, fmt.Errorf("tron: page %d of the transfer window failed (success=false)", page)
			}
			return nil, nil // page 1: pre-existing "nothing to report" behaviour
		}
		data, _ := d["data"].([]any)
		out = appendTransfers(out, data)
		// PAGINATION. TronGrid returns meta.fingerprint ONLY while more pages
		// exist and omits it on the last one — verified against the live API on
		// 2026-08-12, both directions. So its absence is the terminator; an
		// empty page is the belt-and-braces one.
		//
		// The continuation is passed back as the `fingerprint` query param, and
		// the API's own contract is that every OTHER parameter must stay
		// identical between pages — which is why params is built once, above,
		// and only this key is rewritten.
		meta, _ := d["meta"].(map[string]any)
		fp, _ := meta["fingerprint"].(string)
		if fp == "" || len(data) == 0 {
			return out, nil
		}
		if page >= maxTransferPages {
			// One tick must not turn into unbounded HTTP against TronGrid. Keep
			// what was read — those transfers are real and settleable — but hand
			// back ErrWindowTruncated ALONGSIDE them, because the dropped tail is
			// the NEWEST end of an ascending window and a caller that cannot tell
			// truncated from complete would read "absent" as "never arrived".
			log.Printf("tron: transfer window TRUNCATED after %d pages (%d rows/page, %d transfers read) — "+
				"the newest transfers in this window were not read; raise TRON_PAGE_LIMIT or "+
				"shorten the deposit window", page, pageLimit, len(out))
			return out, ErrWindowTruncated
		}
		params.Set("fingerprint", fp)
	}
}

// ErrWindowTruncated accompanies a PARTIAL transfer window: the page guard
// fired before the window was exhausted, so the rows returned are real but the
// newest ones are missing.
//
// It is a sentinel rather than a plain error so the watcher can react
// correctly: it settles what it CAN see and tries again next tick, so losing
// the tail costs a delay, not a deposit.
//
// Returned WITH the partial slice, so a caller that wants the rows can take
// them (errors.Is) and one that needs certainty cannot miss the caveat.
var ErrWindowTruncated = errors.New("tron: transfer window truncated at the page guard")

// maxTransferPages bounds one GetIncomingTransfersConf call. At the shipped
// TRON_PAGE_LIMIT of 200 that is 2,000 transfers in a single window — orders of
// magnitude past this shop's real traffic, so it is a runaway guard rather than
// a tuning knob: the watcher runs this inside a periodic tick, and a paging
// loop with no ceiling could spend an unbounded number of HTTP calls there if
// the API ever kept handing back a continuation.
const maxTransferPages = 10

// appendTransfers parses one page of the TRC20 response into Transfers. Split
// out of GetIncomingTransfersConf only so the paging loop above stays readable;
// the filtering rules are unchanged and are documented inline.
func appendTransfers(out []Transfer, data []any) []Transfer {
	for _, item := range data {
		tx, ok := item.(map[string]any)
		if !ok {
			continue
		}
		txh, _ := tx["transaction_id"].(string)
		if txh == "" {
			continue
		}
		// Python (tron_pay.py:147): keep the row only when `type` is absent/
		// null or exactly "Transfer"/"transfer"; anything else — including an
		// empty string or a non-string value — is skipped. Mirror that exactly:
		// a money-path filter must not be more permissive than the reference.
		if typRaw, exists := tx["type"]; exists && typRaw != nil {
			typ, isStr := typRaw.(string)
			if !isStr || (typ != "Transfer" && typ != "transfer") {
				continue
			}
		}
		// Defend against an unexpected token slipping in (we filter by
		// contract_address, but verify) and against malformed rows.
		if ti, ok := tx["token_info"].(map[string]any); ok {
			if addrField, ok := ti["address"].(string); ok && addrField != "" && addrField != USDTContract {
				continue
			}
		}
		valueStr, ok1 := tx["value"].(string)
		tsF, ok2 := tx["block_timestamp"].(float64)
		frm, _ := tx["from"].(string)
		if !ok1 || !ok2 {
			continue // one malformed row must not void the cycle
		}
		value, ok := new(big.Int).SetString(valueStr, 10)
		if !ok {
			continue
		}
		out = append(out, Transfer{
			ID:    txh,
			Frm:   frm,
			Value: value,
			Units: valToUnits(value),
			TS:    int64(tsF),
		})
	}
	return out
}

// MatchedTransfer is the result of a successful match (tron_pay.py:184):
// {id, amount}; the caller dedupes by id (prefixed "trx:").
type MatchedTransfer struct {
	ID     string
	Amount *big.Rat
}

// NOTE: tron's standalone MatchPayment + absInt64 helper (the by-hash fuzzy
// matcher, tron_pay.py:165-185) were removed here — zero production callers
// left after the hands-free watcher (usdtwatch.go) took over via
// bands.MatchIncoming; only their own dedicated tests referenced them.
