package bep20

import (
	"encoding/json"
	"math/big"
	"testing"
)

// TestValidAddress mirrors bep20_pay.py:69-71's 0x+40-hex shape check.
func TestValidAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"0x55d398326f99059fF775485246999027B3197955", true}, // USDT contract itself
		{"", false},
		{"55d398326f99059fF775485246999027B3197955", false},   // missing 0x
		{"0x55d398326f99059fF775485246999027B319795", false},  // 39 hex chars, too short
		{"0xZZd398326f99059fF775485246999027B3197955", false}, // non-hex chars
	}
	for _, tc := range cases {
		if got := ValidAddress(tc.addr); got != tc.want {
			t.Errorf("ValidAddress(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestAmountUnits mirrors bep20_pay.py:97-100 — Decimal(str(amount)) *
// 10^AMOUNT_DECIMALS, exact-decimal (no float drift).
func TestAmountUnits(t *testing.T) {
	t.Setenv("BEP20_AMOUNT_DECIMALS", "2")
	cases := []struct {
		amount string
		want   int64
	}{
		{"5.23", 523},
		{"0", 0},
		{"100", 10000},
	}
	for _, tc := range cases {
		got, err := AmountUnits(tc.amount)
		if err != nil {
			t.Fatalf("AmountUnits(%q) error: %v", tc.amount, err)
		}
		if got != tc.want {
			t.Errorf("AmountUnits(%q) = %d, want %d", tc.amount, got, tc.want)
		}
	}
}

// TestWeiToUnits mirrors bep20_pay.py:103-107 — floor an 18-decimal wei
// value to AMOUNT_DECIMALS (2) units. big.Int math throughout, per the
// "no float for BEP20 amounts" rule (token has 18 decimals).
func TestWeiToUnits(t *testing.T) {
	t.Setenv("BEP20_AMOUNT_DECIMALS", "2")
	// 105 USDT = 105 * 10^18 wei = 10500 units at 2 decimals.
	wei, ok := new(big.Int).SetString("105000000000000000000", 10)
	if !ok {
		t.Fatal("bad literal")
	}
	if got := weiToUnits(wei); got != 10500 {
		t.Errorf("weiToUnits(105 USDT) = %d, want 10500", got)
	}
	// Dust overpay floors down, never rounds up to a different match band.
	weiDust, _ := new(big.Int).SetString("105009999000000000000", 10)
	if got := weiToUnits(weiDust); got != 10500 {
		t.Errorf("weiToUnits(dust overpay) = %d, want 10500 (floored)", got)
	}
}

// TestGetIncomingTransfers_LogParsing exercises eth_getLogs row parsing
// against a canned JSON-RPC response (no network): transactionHash, data
// (wei, hex), blockNumber (hex), and topics[1] (sender, right-padded to 32
// bytes) must decode correctly (bep20_pay.py:136-160).
func TestGetIncomingTransfers_LogParsing(t *testing.T) {
	raw := []byte(`[
		{
			"transactionHash": "0xaaaa000000000000000000000000000000000000000000000000000000000001",
			"data": "0x000000000000000000000000000000000000000000000005b12aefafa8040000",
			"blockNumber": "0x2a",
			"topics": [
				"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
				"0x000000000000000000000000cccccccccccccccccccccccccccccccccccccccc",
				"0x00000000000000000000000055d398326f99059ff775485246999027b3197955"
			]
		},
		{
			"transactionHash": "",
			"data": "0x00",
			"blockNumber": "0x1",
			"topics": ["a", "b", "c"]
		}
	]`)
	var logs []logEntry
	if err := json.Unmarshal(raw, &logs); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	var out []Transfer
	for _, lg := range logs {
		if lg.TransactionHash == "" {
			continue // mirrors the "no dedupe key -> skip" rule
		}
		wei, err := parseHexBigInt(lg.Data)
		if err != nil {
			t.Fatalf("parseHexBigInt: %v", err)
		}
		block, err := parseHexInt64(lg.BlockNumber)
		if err != nil {
			t.Fatalf("parseHexInt64: %v", err)
		}
		frm := "0x" + lg.Topics[1][len(lg.Topics[1])-40:]
		out = append(out, Transfer{ID: lg.TransactionHash, Frm: frm, Wei: wei, Units: weiToUnits(wei), Block: block})
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 parsed transfer (empty hash skipped), got %d", len(out))
	}
	if out[0].Block != 42 {
		t.Errorf("block = %d, want 42", out[0].Block)
	}
	if out[0].Frm != "0xcccccccccccccccccccccccccccccccccccccccc" {
		t.Errorf("frm = %q, want sender address", out[0].Frm)
	}
	wantWei, _ := new(big.Int).SetString("105000000000000000000", 10)
	if out[0].Wei.Cmp(wantWei) != 0 {
		t.Errorf("wei = %s, want %s", out[0].Wei, wantWei)
	}
}

// TestMatchPayment exercises the matcher (bep20_pay.py:163-186): fuzzy
// amount tolerance, min_block floor, MIN_CONFIRMATIONS depth, isUsed dedupe.
func TestMatchPayment(t *testing.T) {
	t.Setenv("BEP20_AMOUNT_DECIMALS", "2")
	t.Setenv("BEP20_MATCH_TOL", "10")
	t.Setenv("BEP20_MIN_CONF", "12")

	minBlock := int64(1000)
	latest := int64(1020) // 20 blocks deep
	dummyWei := big.NewInt(1)

	transfers := []Transfer{
		{ID: "shallow", Units: 10000, Block: 1015, Wei: dummyWei},  // only 5 confs, not enough
		{ID: "too-early", Units: 10000, Block: 900, Wei: dummyWei}, // before min_block
		{ID: "good", Units: 10005, Block: 1005, Wei: dummyWei},     // 5 cents off (within tol), 15 confs
	}
	got, err := MatchPayment(transfers, "100.00", minBlock, latest, nil)
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "good" {
		t.Fatalf("MatchPayment = %+v, want id=good", got)
	}
}

// TestMatchPayment_IsUsedSkips verifies the "bsc:" prefixed dedupe hook.
func TestMatchPayment_IsUsedSkips(t *testing.T) {
	t.Setenv("BEP20_AMOUNT_DECIMALS", "2")
	t.Setenv("BEP20_MIN_CONF", "12")
	minBlock := int64(1000)
	latest := int64(1020)
	dummyWei := big.NewInt(1)

	transfers := []Transfer{
		{ID: "first", Units: 10000, Block: 1001, Wei: dummyWei},
		{ID: "second", Units: 10000, Block: 1002, Wei: dummyWei},
	}
	got, err := MatchPayment(transfers, "100.00", minBlock, latest, func(id string) bool { return id == "bsc:first" })
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "second" {
		t.Fatalf("MatchPayment = %+v, want id=second (first skipped)", got)
	}
}

// TestMatchPaymentConf_HonoursInjectedMinConf pins the parameter-threading
// added for task #42 item 2: MatchPaymentConf must consult the minConf it is
// PASSED, not the raw-env BEP20_MIN_CONF — because in production
// BEP20_MIN_CONF is set only via .env, which config.Load never copies back
// into os.Environ (no systemd EnvironmentFile), so a package-level envInt
// read is always the compiled default there. Set BEP20_MIN_CONF to a value
// that would WRONGLY accept a shallow transfer if the raw env were still
// consulted, then prove the injected minConf is what actually gates it.
func TestMatchPaymentConf_HonoursInjectedMinConf(t *testing.T) {
	t.Setenv("BEP20_AMOUNT_DECIMALS", "2")
	t.Setenv("BEP20_MATCH_TOL", "10")
	t.Setenv("BEP20_MIN_CONF", "1") // if this leaked through, 15 confs would pass

	minBlock := int64(1000)
	latest := int64(1015) // 15 confs deep
	transfers := []Transfer{{ID: "shallow", Units: 10000, Block: 1000, Wei: big.NewInt(1)}}

	// Injected minConf=20 is stricter than both the env value (1) and the
	// depth (15) -> must be rejected. Old code reading raw MinConfirmations()
	// would have accepted it (env says 1 <= 15).
	got, err := MatchPaymentConf(transfers, "100.00", minBlock, latest, 20, nil)
	if err != nil {
		t.Fatalf("MatchPaymentConf error: %v", err)
	}
	if got != nil {
		t.Fatalf("MatchPaymentConf = %+v, want nil (15 confs < injected minConf=20)", got)
	}

	// Injected minConf=10 is looser than the depth (15) -> must match, even
	// though the env value (1) is irrelevant either way here — the point is
	// the injected value decides, not the env.
	got, err = MatchPaymentConf(transfers, "100.00", minBlock, latest, 10, nil)
	if err != nil {
		t.Fatalf("MatchPaymentConf error: %v", err)
	}
	if got == nil || got.ID != "shallow" {
		t.Fatalf("MatchPaymentConf = %+v, want id=shallow (15 confs >= injected minConf=10)", got)
	}
}
