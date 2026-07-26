// Catalog reads — the purchasable-product surface of the folder-state machine:
// list_products (telegram_bot.py:4369-4378), product_price
// (telegram_bot.py:4381-4398), product_tiers (telegram_bot.py:5043-5072) and
// the warranty-code duration primitives (_warranty_seconds @4439,
// _tier_sort_key @4413). Kept in `stock` because they read the accounts/<product>
// folder + its price_<CODE>_vnd files, the same on-disk surface accounts.py owns;
// the LOCALIZED tier label lives in the handlers layer (tier_label @4659).
package stock

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ListProducts returns the sorted subfolder names under AccountsDir (the
// available products) — Python list_products (telegram_bot.py:4369-4378).
// Hidden folders (leading dot, e.g. .git/.codegraph) are excluded so only real
// product folders show up. Missing AccountsDir -> empty (fresh install).
func (s *Store) ListProducts() []string {
	entries, err := os.ReadDir(s.AccountsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// priceFileRe matches a price_<CODE>_vnd file name (case-insensitive), capturing
// the tier CODE — Python's `price_(.+)_vnd` regex (telegram_bot.py:5056).
var priceFileRe = regexp.MustCompile(`(?i)^price_(.+)_vnd$`)

// ProductPrice reads a product's unit price (int VND) for a warranty TIER code —
// Python product_price (telegram_bot.py:4381-4398). Each product folder holds
// one price file per tier, named price_<CODE>_VND, matched case-insensitively.
// ok is false if the folder or a valid price file for that code is absent, or
// the file's contents don't parse to an int.
func (s *Store) ProductPrice(name, tier string) (int, bool) {
	target := strings.ToLower("price_" + tier + "_vnd")
	entries, err := os.ReadDir(s.productDir(name))
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(e.Name()) != target {
			continue
		}
		raw := strings.TrimSpace(readWholeFile(filepath.Join(s.productDir(name), e.Name())))
		v, perr := strconv.Atoi(raw)
		if perr != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// Tier is one discovered warranty option: its CODE (upper-cased) and unit price
// in VND — the Go shape of product_tiers' {"code","price"} dicts.
type Tier struct {
	Code  string
	Price int
}

// ProductTiers discovers a product's warranty tiers from its price_<CODE>_vnd
// files — Python product_tiers (telegram_bot.py:5043-5072). Sorted KBH-first
// then by warranty duration (in SECONDS, so hour and day tiers interleave
// correctly) then alphabetically. The reserved "API" code (the virtual API-key
// product's tier marker) is skipped so a stray price_API_VND can never mint a
// folder tier that collides with it. Empty when the product has no price files.
func (s *Store) ProductTiers(name string) []Tier {
	entries, err := os.ReadDir(s.productDir(name))
	if err != nil {
		return nil
	}
	var tiers []Tier
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := priceFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		code := strings.ToUpper(m[1])
		if code == "API" { // reserved for the virtual API-key product (telegram_bot.py:5064)
			continue
		}
		raw := strings.TrimSpace(readWholeFile(filepath.Join(s.productDir(name), e.Name())))
		v, perr := strconv.Atoi(raw)
		if perr != nil {
			continue
		}
		tiers = append(tiers, Tier{Code: code, Price: v})
	}
	sort.Slice(tiers, func(i, j int) bool {
		ki, kj := tierSortKey(tiers[i].Code), tierSortKey(tiers[j].Code)
		if ki != kj {
			return ki < kj
		}
		return tiers[i].Code < tiers[j].Code
	})
	return tiers
}

// warrantyDayRe / warrantyHourRe match the two duration-bearing tier codes,
// BH<N>D (days) and BH<N>H (hours) — telegram_bot.py:4447-4452.
var (
	warrantyDayRe  = regexp.MustCompile(`^BH(\d+)D$`)
	warrantyHourRe = regexp.MustCompile(`^BH(\d+)H$`)
)

// WarrantySeconds returns the total warranty duration in SECONDS for any tier
// code — Python _warranty_seconds (telegram_bot.py:4439-4453). KBH/unknown -> 0,
// BH<N>D -> N*86400, BH<N>H -> N*3600. The unit-agnostic primitive the tier sort
// keys off; the localized label lives in the handlers layer (tier_label).
func WarrantySeconds(code string) int {
	cu := strings.ToUpper(code)
	if cu == "KBH" {
		return 0
	}
	if m := warrantyDayRe.FindStringSubmatch(cu); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n * 86400
	}
	if m := warrantyHourRe.FindStringSubmatch(cu); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n * 3600
	}
	return 0
}

// tierSortKey is the SECONDS-based sort key — Python _tier_sort_key
// (telegram_bot.py:4413-4419): KBH=-1 (first), a real duration by its seconds,
// unknown=1e9 (last). Only relative order matters.
func tierSortKey(code string) int {
	if strings.ToUpper(code) == "KBH" {
		return -1
	}
	if sec := WarrantySeconds(code); sec > 0 {
		return sec
	}
	return 1_000_000_000
}
