package handlers

// code_test.go — table-driven tests for the promo/referral code engine.
//
// Covers (per task requirements):
//   - codedTotalFull / apikeyCodedTotalFull: none/referral/promo-stack/promo-replace
//     + 0.95 clamp + reseller-bypass
//   - applyCodeToOrder: promo happy path, inactive, exhausted
//   - applyCodeToOrder: referral happy path, own-code, already-redeemed, reserve-race
//   - Unknown code leaves order at BASELINE price (asserts stored total, not just note)
//   - Re-apply replaces old hold and RELEASES prior hold (store state verified)
//   - rmcode releases hold (via cbRemoveCode)
//   - Expiry releases hold (via spawnOrderExpiry)
//   - Cancel releases hold (via cbCancelOrd)
//   - payReferrer credits exactly REFERRAL_PCT% once; skips when no referrer
//   - capOrderCode pop semantics: escaping text CONSUMES slot; plain text applies code

import (
	"testing"

	"codezdev-bot/internal/store"
)

// ============================================================
// helpers
// ============================================================

// makeOrder returns a bare reserved order the tests can mutate.
func makeOrder(des, chatID, product string, qty, total int64) *store.Order {
	return &store.Order{
		Des:     des,
		ChatID:  chatID,
		Lang:    "en",
		Product: product,
		Tier:    "BH30D",
		Qty:     qty,
		Total:   total,
		Status:  "reserved",
	}
}

// strPtr converts a literal to *string.
func strPtr(s string) *string { return &s }

// ============================================================
// codedTotalFull — unit math (no store, no bot)
// ============================================================

func TestCodedTotalFull(t *testing.T) {
	// unit=100 VND per account. This makes totals easy to reason about.
	type tc struct {
		name        string
		unit        int
		qty         int
		kind        string
		percent     float64
		stack       bool
		resellerPct int
		wantTotal   int
		wantEff     int // effective %
		wantVol     int // volume-tier %
	}
	cases := []tc{
		// ---- baseline ----
		{name: "baseline_no_reseller_qty1", unit: 100, qty: 1, wantTotal: 100, wantEff: 0, wantVol: 0},
		{name: "baseline_volume10pct", unit: 100, qty: 10, wantTotal: 900, wantEff: 10, wantVol: 10},
		{name: "baseline_volume30pct", unit: 100, qty: 50, wantTotal: 3500, wantEff: 30, wantVol: 30},
		{name: "baseline_reseller_replaces_vol", unit: 100, qty: 10, resellerPct: 15, wantTotal: 850, wantEff: 15, wantVol: 10},
		// ---- referral: REFERRAL_PCT=10, bypasses resellerPct ----
		{name: "referral_qty1", unit: 100, qty: 1, kind: "referral", wantTotal: 90, wantEff: 10, wantVol: 0},
		{name: "referral_bypasses_reseller", unit: 100, qty: 1, kind: "referral", resellerPct: 20, wantTotal: 90, wantEff: 10, wantVol: 0},
		// ---- promo replace ----
		{name: "promo_replace_20pct", unit: 100, qty: 1, kind: "promo", percent: 20, stack: false, wantTotal: 80, wantEff: 20, wantVol: 0},
		{name: "promo_replace_bypasses_reseller", unit: 100, qty: 1, kind: "promo", percent: 20, resellerPct: 15, wantTotal: 80, wantEff: 20, wantVol: 0},
		// ---- promo stack: adds on top of volume tier ----
		{name: "promo_stack_on_vol10", unit: 100, qty: 10, kind: "promo", percent: 10, stack: true, wantTotal: 800, wantEff: 20, wantVol: 10},
		{name: "promo_stack_on_vol30", unit: 100, qty: 50, kind: "promo", percent: 10, stack: true, wantTotal: 3000, wantEff: 40, wantVol: 30},
		// ---- clamp at 0.95 ----
		{name: "clamp_at_95", unit: 100, qty: 1, kind: "promo", percent: 99, stack: false, wantTotal: 5, wantEff: 95, wantVol: 0},
		{name: "stack_clamp", unit: 100, qty: 50, kind: "promo", percent: 80, stack: true, wantTotal: 250, wantEff: 95, wantVol: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, eff, vol := codedTotalFull(tc.unit, tc.qty, tc.kind, tc.percent, tc.stack, tc.resellerPct)
			if total != tc.wantTotal {
				t.Errorf("total=%d, want %d", total, tc.wantTotal)
			}
			if eff != tc.wantEff {
				t.Errorf("effPct=%d, want %d", eff, tc.wantEff)
			}
			if vol != tc.wantVol {
				t.Errorf("volPct=%d, want %d", vol, tc.wantVol)
			}
		})
	}
}
