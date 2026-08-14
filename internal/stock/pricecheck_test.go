package stock

// Tests for CheckVndPriceFiles — the on-disk price-file boot guard (catalog.go).
// Mirrors how store.CheckMicroLedger is tested: a pure function returning error,
// so cmd/bot/main.go turns the error into a fatal exit and these tests assert
// the decision directly without spawning the bot.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckVndPriceFilesClean proves a tree with ONLY _USDT price files (or no
// product folders at all) passes the guard — the normal post-migration and
// fresh-install states.
func TestCheckVndPriceFilesClean(t *testing.T) {
	dir := t.TempDir()

	// Empty / missing product folders -> clean (fresh install).
	if err := CheckVndPriceFiles(dir); err != nil {
		t.Fatalf("clean empty dir: got %v, want nil", err)
	}

	// A product folder whose only price file is the NEW _USDT name -> clean.
	prod := filepath.Join(dir, "kbh")
	if err := os.MkdirAll(prod, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prod, "price_KBH_USDT"), []byte("2000000"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A non-price file in the same tree must not trip the guard.
	if err := os.WriteFile(filepath.Join(prod, "type"), []byte("folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckVndPriceFiles(dir); err != nil {
		t.Fatalf("clean _USDT tree: got %v, want nil", err)
	}

	// A non-existent dir is also clean (fresh deploy with no accounts/ yet).
	if err := CheckVndPriceFiles(filepath.Join(dir, "does-not-exist")); err != nil {
		t.Fatalf("missing dir should be clean: got %v, want nil", err)
	}
}

// TestCheckVndPriceFilesFindsVND proves the guard errors when even ONE
// price_<CODE>_VND file remains, names that file in the message, and points at
// the migrate-prices command. This is the cutover-forgotten case the guard
// exists for: without it every folder product undercharges ~40x.
func TestCheckVndPriceFilesFindsVND(t *testing.T) {
	dir := t.TempDir()
	prod := filepath.Join(dir, "kbh")
	if err := os.MkdirAll(prod, 0o700); err != nil {
		t.Fatal(err)
	}
	// A leftover VND file (the pre-flip name) alongside an already-migrated
	// _USDT one — the guard must catch the _VND regardless of siblings.
	vndPath := filepath.Join(prod, "price_BH30D_VND")
	if err := os.WriteFile(vndPath, []byte("55000"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prod, "price_KBH_USDT"), []byte("2000000"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := CheckVndPriceFiles(dir)
	if err == nil {
		t.Fatal("expected error for leftover _VND file, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "price_BH30D_VND") {
		t.Errorf("error should name the offending file %q, got: %s", "price_BH30D_VND", msg)
	}
	if !strings.Contains(msg, "migrate-prices") {
		t.Errorf("error should point at the migrate-prices command, got: %s", msg)
	}
	if !strings.Contains(msg, dir) {
		t.Errorf("error should name the accounts dir %q, got: %s", dir, msg)
	}
}

// TestCheckVndPriceFilesCaseInsensitive proves a lower-case legacy filename
// (which the pre-flip catalog also matched case-insensitively) is not missed —
// the guard and the migrator must agree on what "a VND price file" is.
func TestCheckVndPriceFilesCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	prod := filepath.Join(dir, "kbh")
	if err := os.MkdirAll(prod, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prod, "price_kbh_vnd"), []byte("50000"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckVndPriceFiles(dir); err == nil {
		t.Fatal("expected error for lower-case _vnd file, got nil")
	}
}
