package tron

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestValidAddress mirrors tron_pay.py:71-73's base58 shape check.
func TestValidAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", true}, // USDT contract itself, valid shape
		{"", false},
		{"T123", false}, // too short
		{"1R7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6tX", false}, // doesn't start with T
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t0", false}, // '0' not in base58 alphabet, wrong length too
	}
	for _, tc := range cases {
		if got := ValidAddress(tc.addr); got != tc.want {
			t.Errorf("ValidAddress(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestAmountUnits mirrors tron_pay.py:98-101 — Decimal(str(amount)) *
// 10^AMOUNT_DECIMALS, exact-decimal (no float drift).
func TestAmountUnits(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	cases := []struct {
		amount string
		want   int64
	}{
		{"5.23", 523},
		{"0", 0},
		{"100", 10000},
		{"5.2", 520},
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

// TestValToUnits mirrors tron_pay.py:104-108 — floor a 6-decimal base-unit
// value to AMOUNT_DECIMALS (2) units; floor, not round, so dust never
// rounds up to a higher-matching amount.
func TestValToUnits(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	cases := []struct {
		value string
		want  int64
	}{
		{"105000000", 10500}, // 105 USDT exactly -> 10500 cents
		{"105009999", 10500}, // dust overpay floors down, still matches 105.00
		{"105004999", 10500},
	}
	for _, tc := range cases {
		v, ok := new(big.Int).SetString(tc.value, 10)
		if !ok {
			t.Fatalf("bad test value %q", tc.value)
		}
		if got := valToUnits(v); got != tc.want {
			t.Errorf("valToUnits(%s) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestMatchPayment exercises the matcher (tron_pay.py:165-185): fuzzy amount
// tolerance, at/after min_ts_ms, isUsed dedupe with "trx:" prefix.
func TestMatchPayment(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	t.Setenv("TRON_MATCH_TOL", "10")
	minTS := int64(1_700_000_000_000)

	v1, _ := new(big.Int).SetString("105000000", 10) // 105.00 USDT
	v2, _ := new(big.Int).SetString("105050000", 10) // 105.05 USDT (5 cents off, within tol 10)

	transfers := []Transfer{
		{ID: "old", Value: v1, Units: 10500, TS: minTS - 1000},  // too early
		{ID: "good", Value: v2, Units: 10505, TS: minTS + 1000}, // matches
	}
	got, err := MatchPayment(transfers, "105.00", minTS, nil)
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "good" {
		t.Fatalf("MatchPayment = %+v, want id=good", got)
	}
}

// TestMatchPayment_IsUsedSkips verifies the "trx:" prefixed dedupe hook.
func TestMatchPayment_IsUsedSkips(t *testing.T) {
	t.Setenv("TRON_AMOUNT_DECIMALS", "2")
	minTS := int64(1_700_000_000_000)
	v, _ := new(big.Int).SetString("105000000", 10)

	transfers := []Transfer{
		{ID: "first", Value: v, Units: 10500, TS: minTS + 1000},
		{ID: "second", Value: v, Units: 10500, TS: minTS + 2000},
	}
	got, err := MatchPayment(transfers, "105.00", minTS, func(id string) bool { return id == "trx:first" })
	if err != nil {
		t.Fatalf("MatchPayment error: %v", err)
	}
	if got == nil || got.ID != "second" {
		t.Fatalf("MatchPayment = %+v, want id=second (first skipped)", got)
	}
}

// TestUsdtToVNDRejectsUnusableRate pins the guard added with task #22. Deleting
// the `rate <= 0` check in UsdtToVND must fail here, so the two cases that
// matter most carry their own consequence in the failure message:
//
//   - "0"  -> converts to 0, crediting nothing for a real off-band payment.
//   - "-5" -> converts NEGATIVE, and the deposit path's store.Credit upserts
//     with "amount = amount + excluded.amount", so a paying customer's balance
//     goes DOWN. Money moving the wrong direction, not merely money not moving.
//
// The positive case is not filler: without it a mutation that made this
// function reject EVERY rate would still pass the four error cases, and the
// guard would look tested while the converter was dead.
func TestUsdtToVNDRejectsUnusableRate(t *testing.T) {
	amt := big.NewRat(8, 1) // 8 USDT, the off-band overpay shape
	cases := []struct {
		name    string
		rate    string
		wantErr bool
		want    int64
	}{
		{"zero credits nothing", "0", true, 0},
		{"negative would REDUCE the balance", "-5", true, 0},
		{"empty is not a rate", "", true, 0},
		{"non-numeric is not a rate", "abc", true, 0},
		{"a usable rate still converts", "30000", false, 240000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UsdtToVND(amt, tc.rate)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UsdtToVND(8, %q) = %d, nil — want an error; a bad rate must re-arm, never value a payment", tc.rate, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("UsdtToVND(8, %q) errored: %v", tc.rate, err)
			}
			if got != tc.want {
				t.Errorf("UsdtToVND(8, %q) = %d, want %d", tc.rate, got, tc.want)
			}
		})
	}
}

// TestGetIncomingTransfersConf_HonoursInjectedPageLimitAndBase pins the
// parameter-threading added for task #42 item 5: GetIncomingTransfersConf
// must hit the injected base URL and send the injected page limit, not the
// raw-env TRON_API_BASE/TRON_PAGE_LIMIT — because in production those are
// set only via .env, which config.Load never copies back into os.Environ (no
// systemd EnvironmentFile), so a package-level envInt/os.Getenv read is
// always the compiled default there. Env is set to values that would prove
// the OLD (raw-env) code path was used if they leaked through.
func TestGetIncomingTransfersConf_HonoursInjectedPageLimitAndBase(t *testing.T) {
	t.Setenv("TRON_PAGE_LIMIT", "7")                // would leak through as limit=7 if not injected
	t.Setenv("TRON_API_BASE", "http://127.0.0.1:1") // would leak through as an unreachable host if not injected

	var gotLimit string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer srv.Close()

	// Injected pageLimit=99 and base=srv.URL must be what's actually used —
	// hitting the test server at all proves apiBase() (which would dial the
	// unreachable 127.0.0.1:1 from the env) was NOT consulted.
	_, err := GetIncomingTransfersConf("Tseller", 0, "", 99, srv.URL)
	if err != nil {
		t.Fatalf("GetIncomingTransfersConf error: %v (want it to hit the injected base, not the env TRON_API_BASE)", err)
	}
	if gotPath == "" {
		t.Fatal("test server never received a request — injected base was not used")
	}
	if gotLimit != "99" {
		t.Errorf("limit sent = %q, want \"99\" (the injected pageLimit, not env TRON_PAGE_LIMIT=7)", gotLimit)
	}
}

// trc20Page renders one API page. `fingerprint` empty means LAST page — that is
// TronGrid's real contract, verified against the live endpoint on 2026-08-12:
// meta.fingerprint is present only while more pages exist and is omitted from
// the final response.
func trc20Page(fingerprint string, ids ...string) string {
	rows := make([]string, 0, len(ids))
	for i, id := range ids {
		rows = append(rows, fmt.Sprintf(
			`{"transaction_id":%q,"type":"Transfer","from":"TFrom","value":"1000000","block_timestamp":%d,`+
				`"token_info":{"address":%q,"decimals":6}}`, id, 1700000000000+int64(i)*1000, USDTContract))
	}
	meta := `"meta":{"at":1,"page_size":` + strconv.Itoa(len(ids))
	if fingerprint != "" {
		meta += `,"fingerprint":"` + fingerprint + `"`
	}
	meta += `}`
	return `{"success":true,"data":[` + strings.Join(rows, ",") + `],` + meta + `}`
}

// TestGetIncomingTransfersConfFollowsTheFingerprint pins the pagination
// contract. A single fixed page silently drops the NEWEST transfers in an
// ascending window — exactly the deposits a customer is waiting on — so
// "returned a full page" is not the same as "read the window".
func TestGetIncomingTransfersConfFollowsTheFingerprint(t *testing.T) {
	var fingerprints []string
	var otherParamsPerCall []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fp := q.Get("fingerprint")
		fingerprints = append(fingerprints, fp)
		// The API's contract: every parameter EXCEPT fingerprint must be
		// identical between pages. Record them so the test can prove it.
		q.Del("fingerprint")
		otherParamsPerCall = append(otherParamsPerCall, q.Encode())
		w.Header().Set("Content-Type", "application/json")
		switch fp {
		case "":
			w.Write([]byte(trc20Page("fp-1", "a1", "a2")))
		case "fp-1":
			w.Write([]byte(trc20Page("fp-2", "b1")))
		default:
			w.Write([]byte(trc20Page("", "c1"))) // no fingerprint -> last page
		}
	}))
	defer srv.Close()

	got, err := GetIncomingTransfersConf("Tseller", 0, "", 2, srv.URL)
	if err != nil {
		t.Fatalf("GetIncomingTransfersConf: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("got %d transfers, want 4 — the tail of the window was dropped", len(got))
	}
	if len(fingerprints) != 3 {
		t.Fatalf("made %d requests (%v), want 3 — pagination stopped early or did not stop", len(fingerprints), fingerprints)
	}
	if fingerprints[1] != "fp-1" || fingerprints[2] != "fp-2" {
		t.Errorf("fingerprints sent = %v, want [\"\", \"fp-1\", \"fp-2\"]", fingerprints)
	}
	for i, p := range otherParamsPerCall {
		if p != otherParamsPerCall[0] {
			t.Errorf("request %d changed a non-fingerprint parameter (%q vs %q) — the API requires "+
				"every other parameter to stay identical across pages", i, p, otherParamsPerCall[0])
		}
	}
}

// TestGetIncomingTransfersConfFailsRatherThanReturnHalfAWindow: a page-2 error
// must not come back as a short-but-successful read. The caller would treat the
// missing tail as "that payment never arrived" and leave a real deposit
// uncredited; failing means the next tick simply tries again.
func TestGetIncomingTransfersConfFailsRatherThanReturnHalfAWindow(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Write([]byte(trc20Page("fp-1", "a1")))
			return
		}
		w.Write([]byte(`{"success":false}`))
	}))
	defer srv.Close()

	got, err := GetIncomingTransfersConf("Tseller", 0, "", 1, srv.URL)
	if err == nil {
		t.Errorf("got (%d transfers, nil error), want an error — a half-read window must never look complete", len(got))
	}
	if got != nil {
		t.Errorf("got %d transfers alongside the failure, want none", len(got))
	}
}

// TestGetIncomingTransfersConfStopsAtThePageGuard: the runaway guard must bound
// one call, keep what it read, and not loop forever against a server that hands
// back a continuation indefinitely.
func TestGetIncomingTransfersConfStopsAtThePageGuard(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(trc20Page("more", "id"+strconv.Itoa(calls)))) // never the last page
	}))
	defer srv.Close()

	got, err := GetIncomingTransfersConf("Tseller", 0, "", 1, srv.URL)
	if !errors.Is(err, ErrWindowTruncated) {
		t.Errorf("error = %v, want ErrWindowTruncated — a caller that cannot tell a truncated window "+
			"from a complete one reads a missing transfer as one that never arrived", err)
	}
	if calls != maxTransferPages {
		t.Errorf("made %d requests, want %d (the guard) — an unbounded pager inside a periodic tick", calls, maxTransferPages)
	}
	if len(got) != maxTransferPages {
		t.Errorf("got %d transfers, want %d — what WAS read is real and must still be returned alongside the error", len(got), maxTransferPages)
	}
}

// TestFindConfirmedTransferByHashRefusesToAnswerFromATruncatedWindow: the
// by-hash lookup's (nil, nil) is a TERMINAL "you did not pay" shown to a
// customer. It must never be concluded from a window known to be incomplete.
func TestFindConfirmedTransferByHashRefusesToAnswerFromATruncatedWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(trc20Page("more", "deadbeef"))) // never the last page
	}))
	defer srv.Close()

	tx, err := FindConfirmedTransferByHashConf("T"+strings.Repeat("A", 33), strings.Repeat("a", 64),
		0, "", 1, srv.URL)
	if tx != nil {
		t.Errorf("found a transfer = %v, want none", tx)
	}
	if !errors.Is(err, ErrWindowTruncated) {
		t.Errorf("error = %v, want ErrWindowTruncated — answering \"not found\" here tells a customer "+
			"who really paid that they did not", err)
	}
}

// TestFindConfirmedTransferByHashAnswersFromATruncatedWindowWhenItHasTheHash is
// the other half of the rule above, and the half that costs a customer money if
// it is missing.
//
// A truncated window makes ABSENCE unprovable. It does not make a row that IS
// in the window less real. Refusing a hash the read already returned would be
// refusing on evidence in hand — and for one of this path's two callers there
// is no second chance. verifyUSDTTxid serves both a deposit hash-paste (where
// the hands-free watcher would eventually credit the same transfer anyway) and
// a PRODUCT ORDER payment via cbPayOnchain, which the watcher deliberately
// never touches because its only action is a wallet credit. So a wrongly
// refused order payment has nothing else to rescue it.
//
// The server here never sends a last page, so the guard fires and the window is
// genuinely truncated; the target sits on page 1, inside what was read.
func TestFindConfirmedTransferByHashAnswersFromATruncatedWindowWhenItHasTheHash(t *testing.T) {
	want := strings.Repeat("deadbeef", 8) // 64 hex chars, what hashRE demands
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Write([]byte(trc20Page("more", want))) // the payment, on a page that WAS read
			return
		}
		w.Write([]byte(trc20Page("more", "id"+strconv.Itoa(calls)))) // never the last page
	}))
	defer srv.Close()

	tx, err := FindConfirmedTransferByHashConf("T"+strings.Repeat("A", 33), want, 0, "", 1, srv.URL)
	if err != nil {
		t.Fatalf("error = %v, want nil — the hash was in the rows that came back, so truncation "+
			"further along the window says nothing about it", err)
	}
	if tx == nil || tx.ID != want {
		t.Fatalf("transfer = %v, want the one with id %s — a real payment refused because the read "+
			"stopped AFTER it", tx, want)
	}
	if calls != maxTransferPages {
		t.Errorf("made %d requests, want %d — the window really must be truncated, or this test "+
			"passes without exercising the case", calls, maxTransferPages)
	}
}
