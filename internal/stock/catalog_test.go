package stock

// Tests for catalog.go's admin price-write path (SetProductPrice /
// DeleteProductTier) — the money-safety surface adminpricing.go's
// tier/price captures drive. ProductTiers/ProductPrice's READ side already
// has implicit coverage via stock_test.go's other tests; this file is
// specifically the WRITE side: create, update, delete, atomicity, and the
// discovery-order the admin UI depends on.
//
// Price UNIT is micro-USDT (1 USDT = 1_000_000 micro) since the VND->micro-USDT
// flip. The micro values below are the converted equivalents of the old VND
// prices at rate 25000 (old_VND × 40): e.g. 50_000 VND -> 2_000_000 micro
// (2.00 USDT). The read/write MECHANICS are unchanged (a bare int); only the
// unit the int denotes + the _USDT filename changed.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSetProductPriceCreateUpdate proves the add-vs-update distinction the
// admin confirmation message ("Added"/"Updated") depends on, and that
// ProductTiers/ProductPrice see the write immediately.
func TestSetProductPriceCreateUpdate(t *testing.T) {
	s := newTestStore(t, "p1")

	existed, err := s.SetProductPrice("p1", "kbh", 2_000_000) // 50_000 VND x 40 = 2.00 USDT
	if err != nil {
		t.Fatalf("SetProductPrice (create): %v", err)
	}
	if existed {
		t.Error("first write should report existed=false")
	}
	if got, ok := s.ProductPrice("p1", "KBH"); !ok || got != 2_000_000 {
		t.Fatalf("ProductPrice after create = (%d,%v), want (2000000,true)", got, ok)
	}

	existed, err = s.SetProductPrice("p1", "KBH", 4_800_000) // 120_000 VND x 40 = 4.80 USDT
	if err != nil {
		t.Fatalf("SetProductPrice (update): %v", err)
	}
	if !existed {
		t.Error("second write to the same tier should report existed=true")
	}
	if got, ok := s.ProductPrice("p1", "kbh"); !ok || got != 4_800_000 {
		t.Fatalf("ProductPrice after update = (%d,%v), want (4800000,true)", got, ok)
	}

	// The write is always canonical-case ("price_KBH_USDT"), no matter what
	// case the caller passed in.
	if _, err := os.Stat(filepath.Join(s.AccountsDir, "p1", "price_KBH_USDT")); err != nil {
		t.Errorf("expected canonical-case price file: %v", err)
	}
}

// TestSetProductPriceNoHalfWrite proves the atomic tmp+rename contract: the
// live price file's content is always a complete integer, never a partial
// write, and no stray .tmp file is left behind after a successful write.
func TestSetProductPriceNoHalfWrite(t *testing.T) {
	s := newTestStore(t, "p1")
	if _, err := s.SetProductPrice("p1", "BH30D", 2_200_000); err != nil { // 55_000 VND x 40 = 2.20 USDT
		t.Fatalf("SetProductPrice: %v", err)
	}
	path := filepath.Join(s.AccountsDir, "p1", "price_BH30D_USDT")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read price file: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "2200000" {
		t.Errorf("price file content = %q, want \"2200000\"", string(raw))
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("stray .tmp file left behind: err=%v", err)
	}
}

// TestProductTiersDiscoveryAndSort proves tiers are DISCOVERED (not
// declared) off the price_<CODE>_USDT files SetProductPrice writes, sorted
// KBH-first then by warranty duration, and that price_API_USDT (the virtual
// API-key product's reserved marker) never leaks in as a folder tier.
func TestProductTiersDiscoveryAndSort(t *testing.T) {
	s := newTestStore(t, "p1")
	for _, tc := range []struct {
		code  string
		micro int
	}{
		{"BH30D", 3_600_000}, {"KBH", 2_000_000}, {"BH12H", 2_800_000}, {"BH1D", 2_400_000},
	} {
		if _, err := s.SetProductPrice("p1", tc.code, tc.micro); err != nil {
			t.Fatalf("SetProductPrice(%s): %v", tc.code, err)
		}
	}
	// SetProductPrice itself now rejects the reserved "API" code (it would
	// otherwise create a folder tier invisible to, and colliding with, the
	// virtual API-key product's own reserved marker) — write the file
	// directly here so ProductTiers' own API-skip below is still exercised
	// against a real price_API_USDT on disk, the way a stray legacy file
	// would appear.
	if _, err := s.SetProductPrice("p1", "API", 1); err == nil {
		t.Error("SetProductPrice(\"API\", ...) should be rejected as a reserved tier code")
	}
	if err := s.DeleteProductTier("p1", "API"); err == nil {
		t.Error("DeleteProductTier(\"API\") should be rejected as a reserved tier code")
	}
	if err := os.WriteFile(filepath.Join(s.AccountsDir, "p1", "price_API_USDT"), []byte("1"), 0o600); err != nil {
		t.Fatalf("seed reserved-code price file directly: %v", err)
	}
	tiers := s.ProductTiers("p1")
	var codes []string
	for _, ti := range tiers {
		codes = append(codes, ti.Code)
	}
	// KBH first, then ascending by warranty seconds (12h < 1d < 30d); API is
	// reserved and must never appear.
	want := []string{"KBH", "BH12H", "BH1D", "BH30D"}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Fatalf("ProductTiers order = %v, want %v", codes, want)
	}
}

// TestDeleteProductTier proves the remove path: it deletes the exact
// on-disk file CASE-INSENSITIVELY (so a tier discovered via ProductTiers,
// which always reports upper-case, still deletes a lower-case legacy file),
// and a repeat delete of an already-gone tier reports an error rather than
// silently no-op-succeeding.
func TestDeleteProductTier(t *testing.T) {
	s := newTestStore(t, "p1")
	if _, err := s.SetProductPrice("p1", "BH7D", 1_600_000); err != nil { // 40_000 VND x 40 = 1.60 USDT
		t.Fatalf("seed price: %v", err)
	}
	// Simulate a legacy lower-case file alongside — DeleteProductTier must
	// still find and remove it via the case-insensitive scan.
	lower := filepath.Join(s.AccountsDir, "p1", "price_kbh_usdt")
	if err := os.WriteFile(lower, []byte(strconv.Itoa(1000)), 0o600); err != nil {
		t.Fatalf("seed legacy lower-case file: %v", err)
	}

	if err := s.DeleteProductTier("p1", "KBH"); err != nil {
		t.Fatalf("DeleteProductTier(KBH) on legacy lower-case file: %v", err)
	}
	if _, err := os.Stat(lower); !os.IsNotExist(err) {
		t.Errorf("legacy lower-case file should be gone: err=%v", err)
	}

	if err := s.DeleteProductTier("p1", "BH7D"); err != nil {
		t.Fatalf("DeleteProductTier(BH7D): %v", err)
	}
	if _, ok := s.ProductPrice("p1", "BH7D"); ok {
		t.Error("BH7D should be gone after delete")
	}

	// Deleting a tier that no longer exists is an error, not a silent no-op.
	if err := s.DeleteProductTier("p1", "BH7D"); err == nil {
		t.Error("deleting an already-removed tier should return an error")
	}
}
