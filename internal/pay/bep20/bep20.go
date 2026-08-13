// Package bep20 ports bep20_pay.py (Kiro Python original, 258 lines) — USDT
// BEP20 (BNB Smart Chain) payments via a free public BSC RPC.
//
// The customer sends USDT (BEP20) to the seller's on-chain address. On-chain
// transfers carry NO memo/order id, so — exactly like the binance package —
// each pending order is assigned a UNIQUE USDT amount and a credit is
// recognised by matching an incoming transfer of that exact amount to the
// seller's address (bep20_pay.py:2-7).
//
// Confirmation source: eth_getLogs for the USDT contract's Transfer events
// with the indexed `to` topic = seller address. No Etherscan (its free tier
// excludes BSC); free public RPCs (publicnode, bloXroute) serve getLogs over
// a wide block range. Stdlib-only HTTP, mirroring binance.go / sepay.go /
// paypal.go (bep20_pay.py:9-12).
//
// Token: Binance-Peg BSC-USD ("USDT") 0x55d3...7955, 18 decimals (NOT 6 like
// ETH/Tron USDT) — verified on-chain via decimals() (bep20_pay.py:14-16).
// All on-chain amounts are handled as big.Int wei — no float arithmetic for
// money, per the port's money-safety rule.
package bep20

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// USDTContract is the USDT BEP20 contract address (bep20_pay.py:27).
const USDTContract = "0x55d398326f99059fF775485246999027B3197955"

// TransferTopic is the ERC-20 Transfer(address,address,uint256) topic0
// (bep20_pay.py:28).
const TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// TokenDecimals is USDT's on-chain decimals on BSC (bep20_pay.py:29).
const TokenDecimals = 18

// RPCEndpoints are free public BSC RPCs that serve eth_getLogs over a wide
// block range (measured: both accept >=1000-block ranges; publicnode up to
// 50000). Rotated in order (bep20_pay.py:33-36).
var RPCEndpoints = []string{
	"https://bsc-rpc.publicnode.com",
	"https://bsc.rpc.blxrbdn.com",
}

const httpTimeout = 20 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

// DefaultAmountDecimals / DefaultMinConfirmations / DefaultAmountMatchTol /
// DefaultAmountOffsetStep are the compiled-in fallbacks for the four knobs
// below — named so a caller resolving one of these through config.Config
// (rather than through this package's own raw env read; see
// MinConfirmations's doc comment) has the same default to fall back to
// without hardcoding a second copy of the literal.
const (
	DefaultAmountDecimals   = 5
	DefaultMinConfirmations = 12
	DefaultAmountMatchTol   = 50
	DefaultAmountOffsetStep = 101
)

// AmountDecimals is the precision of the per-order UNIQUE amount. 5 = 1e-5
// USDT, raised from the Python original's 2 (cents) by the hands-free-deposits
// ruling: the picker's identifying jitter is only 0.01000-0.01999 USDT wide, so
// the slots inside it are 0.00101 apart and cents cannot express them. BSC
// Binance-Peg USDT carries 18 decimals on-chain, so 5 is exactly representable
// and weiToUnits' 18->5 floor discards at most 0.00001 USDT — a fiftieth of the
// match tolerance (bep20_pay.py:37-40).
func AmountDecimals() int {
	return envInt("BEP20_AMOUNT_DECIMALS", DefaultAmountDecimals)
}

// MinConfirmations reads the RAW .env/ambient BEP20_MIN_CONF — nothing more.
// Like Address() (see that function's doc comment for the full reasoning),
// it does NOT see a value set only via .env under systemd (no
// EnvironmentFile in production, config.Load's doc comment): a production
// caller with a *config.Config available must resolve BEP20_MIN_CONF through
// it and pass the result into MatchPaymentConf instead of letting this package
// read it. This function — and the unparameterized MatchPayment below, which
// delegates to MatchPaymentConf using this value — survive only as the
// low-level env primitive and its default-argument callers; do not call
// them directly for anything customer-facing.
//
// reorg safety: don't settle until the transfer is this many blocks deep
// (BSC ~3s/block, so 12 ~ ~36s, bep20_pay.py:41-43).
func MinConfirmations() int {
	return envInt("BEP20_MIN_CONF", DefaultMinConfirmations)
}

// AmountMatchTol / AmountOffsetStep: a payment within AmountMatchTol units of
// the expected amount matches; the unique-amount picker spaces concurrent
// same-price orders by AmountOffsetStep units (>= 2*tol+1) so the ±tol bands
// never overlap -> a payment maps to exactly one order. Units are at
// AmountDecimals precision.
//
// Default tol = 50 (0.00050 USDT), step = 101 (0.00101 USDT) — exactly
// 2*50+1, the tightest step the disjointness rule permits, chosen so ten slots
// fit the 0.01000-0.01999 jitter window. EXACT matching is therefore far
// tighter than the Python original's forgiving 10-cent fuzz: customers pay from
// personal wallets, so the exact amount normally arrives and the network fee is
// charged separately rather than skimmed off the transfer. The pay screen tells
// them so in every language (i18n catalog_pay.go).
//
// A sender whose platform DOES skim is handled by a second, wider rule that
// does not live here — bands/match.go's fee-shortfall path, which forgives up
// to min(0.5 USDT, 5% of the quote) but only when exactly one live band could
// claim the payment. Tol below governs the EXACT path; do not widen it in the
// belief that it is what forgives a fee (bep20_pay.py:44-58).
func AmountMatchTol() int {
	return envInt("BEP20_MATCH_TOL", DefaultAmountMatchTol)
}

func AmountOffsetStep() int {
	return envInt("BEP20_OFFSET_STEP", DefaultAmountOffsetStep)
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

// addrRE is a BSC/EVM address shape check: 0x + 40 hex (bep20_pay.py:60).
var addrRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Address reads the RAW .env/ambient BEP20_ADDRESS ("" if unset) — nothing
// more. It does NOT see an admin's settings.json address change (task #17):
// unlike the Python original, which mutated os.environ in-process on every
// /setbep20 (apply_saved_bep20, telegram_bot.py:5294-5306) so a bare getenv
// stayed live, Go's config.Config deliberately never touches the real
// process environment (config.Load's doc comment) — the admin-set value only
// ever lives in settings.json. The customer-facing address, deposit
// matching, and Configured() gate must all resolve through
// config.Config.BEP20Address() (settings.json overriding this env snapshot),
// not this function; internal/handlers passes that resolved value into the
// functions in this package that need it (GetIncomingTransfers) instead of
// letting them call Address() themselves. This function survives only as the
// low-level env primitive those call sites build on — do not call it
// directly for anything customer-facing.
func Address() string {
	return strings.TrimSpace(os.Getenv("BEP20_ADDRESS"))
}

// ValidAddress reports whether a looks like a BSC/EVM address (0x + 40 hex,
// bep20_pay.py:69-71).
func ValidAddress(a string) bool {
	a = strings.TrimSpace(a)
	return a != "" && addrRE.MatchString(a)
}

// Configured reports whether Address() (the raw env snapshot, NOT
// settings.json — see Address's doc comment) looks like a valid receiving
// address. Kept for parity with bep20_pay.py:74-76; internal/handlers gates
// the pay button on ValidAddress(config.Config.BEP20Address()) instead, so
// an admin-set address is honoured (task #17).
func Configured() bool {
	return ValidAddress(Address())
}

// QRURL is the optional hosted image URL of the seller's address QR ("" if
// unset, bep20_pay.py:79-81).
func QRURL() string {
	return strings.TrimSpace(os.Getenv("BEP20_QR_URL"))
}

// QRPath is the optional local file path of the seller's address QR ("" if
// unset, bep20_pay.py:84-86).
func QRPath() string {
	return strings.TrimSpace(os.Getenv("BEP20_QR_PATH"))
}

// UsdtToVND converts a received USDT amount to VND (USDT ~ USD) at the
// VND-per-USD rate the CALLER supplies. Used to credit the wallet by the
// value ACTUALLY received on a second payment (bep20_pay.py:89-94).
//
// rateStr is passed in rather than read from the process environment, for the
// same reason the seller address is (see Address() above): this package
// deliberately does not import internal/config, and the admin rate wizard
// (adm:setrate, internal/handlers/adminsettings.go) only ever writes
// settings.json. internal/handlers resolves the effective rate — settings.json's
// usd_to_vnd over the .env/ambient USD_TO_VND snapshot, else
// config.DefaultUSDToVND — and hands the result here, so the VND a customer is
// CREDITED comes from the same resolution as the quote they were charged. The
// previous version read os.LookupEnv("USD_TO_VND") directly, which saw neither
// a settings.json write nor config's compiled fallback.
//
// An empty, unparseable, or non-positive rate returns an error rather than
// converting, so the hands-free watcher holds the credit for the next tick
// instead of acting on a value it cannot trust. Every other consumer of this
// rate in internal/handlers (usdtDisp, deposit's usdtToVND, vndToUSD) already
// refuses a non-positive rate the same way; this converter was the one that
// did not.
//
// What the guard actually protects, stated precisely because the obvious
// reading is too broad:
//
//   - A deposit paid WITHIN the band tolerance was never at risk either way:
//     bands.DepositCredit returns the order's desiredVND inside tolerance
//     regardless of the converted recvVND. Only an OFF-BAND payment credits
//     the converted value, so that is the whole blast radius on this path.
//   - For an off-band deposit, a rate of 0 credits 0. A NEGATIVE rate is
//     worse than nothing: the deposit path's store.Credit upserts with
//     "amount = amount + excluded.amount" (internal/store/balance.go), so a
//     real payment would REDUCE the customer's balance. Money moving the
//     wrong direction is why this is an error and not a clamp to zero.
//   - On the PAY (non-deposit) path the damage is accounting, not the
//     customer: recvVND feeds only MarkTxn, while the delivery decision comes
//     from `matched`, a band comparison on USDT units. The customer still
//     receives their accounts and the shop still received the USDT — what
//     breaks is a processed-ledger row recording 0 for a payment that was
//     really made.
//
// NOTE: like the Python original (float(usdt) * rate), this conversion is
// float-based, not big.Int — VND has no sub-unit so the drift is at most a
// rounding of the final integer VND amount, matching Python's behavior
// exactly rather than "improving" on it and changing observable amounts.
func UsdtToVND(usdt *big.Rat, rateStr string) (int64, error) {
	rateStr = strings.TrimSpace(rateStr)
	if rateStr == "" {
		return 0, fmt.Errorf("bep20: no USD_TO_VND rate supplied")
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return 0, fmt.Errorf("bep20: invalid USD_TO_VND %q: %w", rateStr, err)
	}
	if rate <= 0 {
		return 0, fmt.Errorf("bep20: non-positive USD_TO_VND %q", rateStr)
	}
	f, _ := usdt.Float64()
	return int64(math.Round(f * rate)), nil
}

// AmountUnits returns the integer amount-unit value of a USDT amount string
// at AmountDecimals precision (e.g. "5.23" -> 523 at 2 decimals). Uses
// big.Rat for exact decimal-string parsing, avoiding float drift, mirroring
// Python's Decimal math (bep20_pay.py:97-100).
func AmountUnits(amountStr string) (int64, error) {
	r, ok := new(big.Rat).SetString(amountStr)
	if !ok {
		return 0, fmt.Errorf("bep20: invalid amount %q", amountStr)
	}
	scale := new(big.Rat).SetInt(pow10(AmountDecimals()))
	r.Mul(r, scale)
	return ratToIntegral(r), nil
}

// weiToUnits floors an on-chain 18-dec wei value to AmountDecimals units.
// Floor (not round) so a dust OVERPAY still matches the order and a dust
// UNDERPAY never credits a higher amount (bep20_pay.py:103-107).
func weiToUnits(wei *big.Int) int64 {
	divisor := pow10(TokenDecimals - AmountDecimals())
	q := new(big.Int).Div(wei, divisor) // big.Int.Div floors for positive operands
	return q.Int64()
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// ratToIntegral rounds a big.Rat to the nearest integer, half-away-from-zero.
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

// rpcRequest / rpcResponse mirror the JSON-RPC 2.0 envelope used against the
// BSC RPC pool (bep20_pay.py:110-128).
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// rpc calls a JSON-RPC method against the endpoint pool; first success wins.
// Returns an error only if every endpoint fails (the caller's poll cycle
// retries next round, bep20_pay.py:110-128).
func rpc(method string, params []any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, endpoint := range RPCEndpoints {
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "kiro-bot")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var d rpcResponse
		if err := json.Unmarshal(raw, &d); err != nil {
			lastErr = err
			continue
		}
		if len(d.Error) > 0 && string(d.Error) != "null" {
			lastErr = fmt.Errorf("%s: %s", endpoint, string(d.Error)) // endpoint-specific (e.g. range cap) -> try next
			continue
		}
		return d.Result, nil
	}
	return nil, fmt.Errorf("all BSC RPCs failed for %s: %w", method, lastErr)
}

// LatestBlock returns the current BSC head block number (bep20_pay.py:131-133).
func LatestBlock() (int64, error) {
	raw, err := rpc("eth_blockNumber", []any{})
	if err != nil {
		return 0, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return 0, fmt.Errorf("bep20: decode eth_blockNumber: %w", err)
	}
	return parseHexInt64(hexStr)
}

// BlockTimestamp returns the epoch-MILLISECOND timestamp of a BSC block, via
// eth_getBlockByNumber. The hands-free watcher uses it for the freshness anchor
// (settleWatchedTransfer compares a transfer's time against its band's start):
// an eth_getLogs row carries no timestamp, and estimating one from the block
// number is unsafe under variable block times, so the real timestamp is fetched
// for each transfer that reaches the settle path.
func BlockTimestamp(block int64) (int64, error) {
	raw, err := rpc("eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", block), false})
	if err != nil {
		return 0, err
	}
	var hdr struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return 0, fmt.Errorf("bep20: decode eth_getBlockByNumber: %w", err)
	}
	ts, err := parseHexInt64(hdr.Timestamp)
	if err != nil {
		return 0, err
	}
	return ts * 1000, nil // epoch seconds -> ms, the domain the freshness check uses
}

func parseHexInt64(s string) (int64, error) {
	s = strings.TrimPrefix(s, "0x")
	n, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func parseHexBigInt(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("bep20: invalid hex integer %q", s)
	}
	return n, nil
}

// Transfer is one incoming USDT transfer found via eth_getLogs
// (bep20_pay.py:136-139): {id: txhash, frm, wei, units (floored
// AmountDecimals value), block}.
type Transfer struct {
	ID    string
	Frm   string
	Wei   *big.Int
	Units int64
	Block int64
}

// logEntry mirrors one eth_getLogs result row.
type logEntry struct {
	TransactionHash string   `json:"transactionHash"`
	Data            string   `json:"data"`
	BlockNumber     string   `json:"blockNumber"`
	Topics          []string `json:"topics"`
}

// GetIncomingTransfers returns incoming USDT transfers to addr in
// [fromBlock, toBlock] (toBlock=nil -> "latest"). Confirmations are computed
// by the caller from a single LatestBlock() so all matches in a cycle share
// one head (bep20_pay.py:136-160).
func GetIncomingTransfers(addr string, fromBlock int64, toBlock *int64) ([]Transfer, error) {
	topicTo := "0x" + strings.Repeat("0", 24) + strings.ToLower(strings.TrimPrefix(addr, "0x"))
	toBlockHex := "latest"
	if toBlock != nil {
		toBlockHex = fmt.Sprintf("0x%x", *toBlock)
	}
	params := []any{map[string]any{
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   toBlockHex,
		"address":   USDTContract,
		"topics":    []any{TransferTopic, nil, topicTo},
	}}
	raw, err := rpc("eth_getLogs", params)
	if err != nil {
		return nil, err
	}
	var logs []logEntry
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &logs); err != nil {
			return nil, fmt.Errorf("bep20: decode eth_getLogs: %w", err)
		}
	}
	var out []Transfer
	for _, lg := range logs {
		if lg.TransactionHash == "" {
			continue // no dedupe key -> skip (real RPCs always send it)
		}
		wei, err := parseHexBigInt(lg.Data)
		if err != nil {
			continue // one malformed log must not void the whole cycle
		}
		block, err := parseHexInt64(lg.BlockNumber)
		if err != nil {
			continue
		}
		if len(lg.Topics) < 2 || len(lg.Topics[1]) < 40 {
			continue
		}
		frm := "0x" + lg.Topics[1][len(lg.Topics[1])-40:]
		out = append(out, Transfer{
			ID:    lg.TransactionHash,
			Frm:   frm,
			Wei:   wei,
			Units: weiToUnits(wei),
			Block: block,
		})
	}
	return out, nil
}

// MatchedTransfer is the result of a successful match (bep20_pay.py:172-173):
// {id, amount}; the caller dedupes by id (prefixed "bsc:").
type MatchedTransfer struct {
	ID     string
	Amount *big.Rat
}

// MatchPayment is MatchPaymentConf using the raw-env MinConfirmations() —
// kept for callers with no *config.Config to resolve BEP20_MIN_CONF through
// (see MinConfirmations's doc comment). Has no production caller today; BEP20
// deposits are credited hands-free by the watcher (usdtwatch.go), which
// matches through bands.MatchIncoming, not this helper.
func MatchPayment(transfers []Transfer, expectedAmount string, minBlock, latest int64, isUsed func(txnID string) bool) (*MatchedTransfer, error) {
	return MatchPaymentConf(transfers, expectedAmount, minBlock, latest, MinConfirmations(), isUsed)
}

// MatchPaymentConf returns the first incoming transfer matching
// expectedAmount (a string like "5.23"), else (nil, nil).
//
// Matches when: amount (floored to AmountDecimals) is within AmountMatchTol
// of expected — 0.00050 USDT at the shipped geometry, i.e. essentially the
// exact figure, NOT the "cent or two" of slack this comment claimed while the
// rail still ran at 2 decimals. A genuinely short payment is handled by
// bands/match.go's fee-shortfall rule instead. Also requires that the
// tx is at or after minBlock (the order's own creation block — so a
// recycled amount's older payment can't settle a newer order), it has >=
// minConf confirmations (latest - block), and the tx isn't already
// consumed. minConf is the caller's resolved BEP20_MIN_CONF (see
// MinConfirmations's doc comment) rather than read from raw env here. The
// picker keeps live amounts >= 2*tol+1 apart, so a payment is within tol of
// at most ONE order (bep20_pay.py:163-186).
func MatchPaymentConf(transfers []Transfer, expectedAmount string, minBlock, latest int64, minConf int, isUsed func(txnID string) bool) (*MatchedTransfer, error) {
	want, err := AmountUnits(expectedAmount)
	if err != nil {
		return nil, err
	}
	tol := int64(AmountMatchTol())
	minConf64 := int64(minConf)
	for _, t := range transfers {
		if absInt64(t.Units-want) > tol { // fuzzy: forgot-a-cent still matches
			continue
		}
		if t.Block < minBlock {
			continue
		}
		if (latest - t.Block) < minConf64 {
			continue // not deep enough yet -> next cycle
		}
		if isUsed != nil && isUsed(fmt.Sprintf("bsc:%s", t.ID)) {
			continue
		}
		amount := new(big.Rat).SetFrac(t.Wei, pow10(TokenDecimals))
		return &MatchedTransfer{ID: t.ID, Amount: amount}, nil
	}
	return nil, nil
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
