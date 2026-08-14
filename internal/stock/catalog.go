// Catalog reads — the purchasable-product surface of the folder-state machine:
// list_products (telegram_bot.py:4369-4378), product_price
// (telegram_bot.py:4381-4398), product_tiers (telegram_bot.py:5043-5072) and
// the warranty-code duration primitives (_warranty_seconds @4439,
// _tier_sort_key @4413). Kept in `stock` because they read the accounts/<product>
// folder + its price_<CODE>_USDT files, the same on-disk surface accounts.py owns;
// the LOCALIZED tier label lives in the handlers layer (tier_label @4659).
package stock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// errReservedTierCode is returned by SetProductPrice/DeleteProductTier when
// the caller passes the reserved "API" tier code (case-insensitive) — see
// each function's doc comment.
var errReservedTierCode = errors.New("stock: \"API\" is a reserved tier code")

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

// priceFileRe matches a price_<CODE>_USDT file name (case-insensitive), capturing
// the tier CODE — Python's `price_(.+)_vnd` regex (telegram_bot.py:5056), renamed
// to _USDT by the VND->micro-USDT price-file flip (store.db money columns are
// micro now; these on-disk product prices had to follow, or every folder product
// would undercharge ~40x at cutover — see cmd/migrate-prices).
var priceFileRe = regexp.MustCompile(`(?i)^price_(.+)_usdt$`)

// legacyVndPriceFileRe matches a pre-flip price_<CODE>_VND file (the name the
// catalog used before the VND->micro-USDT flip). Used ONLY by CheckVndPriceFiles
// — the boot guard that refuses to start against a tree still holding VND-scale
// price files. Kept case-insensitive so a lower-case legacy file is not missed.
var legacyVndPriceFileRe = regexp.MustCompile(`(?i)^price_(.+)_vnd$`)

// CheckVndPriceFiles is the on-disk price-file boot guard — the filesystem twin
// of store.CheckMicroLedger. After the catalog flip, ProductPrice reads its int
// as MICRO-USDT; a price file still holding a VND-scale value (still named
// price_<CODE>_VND, never converted by migrate-prices) would be charged verbatim
// and undercharge every folder product by ~40x at cutover (rate 25000). This
// walks dir ONCE for any price_*_VND file and returns an error naming the first
// one found + the exact migrate-prices command to run, so cmd/bot/main.go can
// log.Fatal on it. Returns nil when the tree is clean (all files _USDT, or no
// product folders / missing dir — a fresh install is clean).
//
// Pure decision (one WalkDir, no logging/log.Fatal) so it is directly
// unit-testable; cmd/bot/main.go calls it and turns the error into a fatal exit,
// exactly the split store.CheckMicroLedger uses.
func CheckVndPriceFiles(dir string) error {
	var first string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A missing/unreadable accounts dir is treated as clean: a fresh
			// deploy has no product folders, and the stock machine itself
			// creates accounts/ lazily — refusing to boot because a brand-new
			// tree does not exist yet would be wrong. The DB guard already
			// owns the "does the data exist" question for its own store.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if legacyVndPriceFileRe.MatchString(d.Name()) {
			if rel, rerr := filepath.Rel(dir, path); rerr == nil {
				first = rel
			} else {
				first = d.Name()
			}
			return filepath.SkipDir // one hit is enough; stop walking this branch
		}
		return nil
	})
	if first != "" {
		return fmt.Errorf("unconverted VND price file %q under %s — ProductPrice now reads micro-USDT, "+
			"so this would undercharge ~40x at cutover; run: migrate-prices -accounts %s -rate <R> -apply "+
			"(use the SAME rate migrate-usdt used)", first, dir, dir)
	}
	return nil
}

// ProductPrice reads a product's unit price (int micro-USDT) for a warranty TIER
// code — Python product_price (telegram_bot.py:4381-4398). Each product folder
// holds one price file per tier, named price_<CODE>_USDT, matched
// case-insensitively. ok is false if the folder or a valid price file for that
// code is absent, or the file's contents don't parse to an int. The unit is
// MICRO-USDT (1 USDT = 1,000,000 micro); a value written as VND by an older
// build must be converted by migrate-prices before this build boots, or the
// boot guard (stock.CheckVndPriceFiles) refuses to start.
func (s *Store) ProductPrice(name, tier string) (int, bool) {
	target := strings.ToLower("price_" + tier + "_usdt")
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
// in micro-USDT — the Go shape of product_tiers' {"code","price"} dicts.
type Tier struct {
	Code  string
	Price int
}

// ProductTiers discovers a product's warranty tiers from its price_<CODE>_USDT
// files — Python product_tiers (telegram_bot.py:5043-5072). Sorted KBH-first
// then by warranty duration (in SECONDS, so hour and day tiers interleave
// correctly) then alphabetically. The reserved "API" code (the virtual API-key
// product's tier marker) is skipped so a stray price_API_USDT can never mint a
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

// SetProductPrice creates or overwrites a warranty tier's price file for a
// product — the admin editor's write path (telegram_bot.py:19193-19213 add a
// new tier, :19215-19239 edit an existing tier's price). micro is the unit
// price in MICRO-USDT (1 USDT = 1,000,000 micro); the handlers layer already
// converts the admin's typed USDT amount via adminUsdtToMicro before calling
// here. A price write is MONEY: a crash mid-write must never leave
// price_<CODE>_USDT half-written (a torn read would serve a garbage price on
// the very next buy-menu render), so this goes through writeFileAtomic (tmp +
// os.replace, stock.go:192) under the SAME product lock every stock mutation
// takes (stock.go:110), so a price change can never race a concurrent
// reservation/sale of this product. existed reports whether this exact tier
// code already had a price file (the "Updated"/"Added" verb the admin's
// confirmation message picks between).
//
// "API" is rejected outright: it is the reserved marker ProductTiers already
// skips (telegram_bot.py:5064) for the virtual API-key product, so writing a
// real price_API_USDT file here would create a folder tier that silently
// collides with — and is invisible to — that reserved code. Unreachable
// from the admin UI today (product names/tier codes never resolve to
// "API"), but the guard costs nothing and closes the hole outright.
func (s *Store) SetProductPrice(product, code string, micro int) (existed bool, err error) {
	if strings.ToUpper(code) == "API" {
		return false, errReservedTierCode
	}
	unlock, err := s.lockProduct(product)
	if err != nil {
		return false, err
	}
	defer unlock()
	dir := s.productDir(product)
	if err := os.MkdirAll(dir, credentialDirMode); err != nil {
		return false, err
	}
	path := filepath.Join(dir, "price_"+strings.ToUpper(code)+"_USDT")
	if _, statErr := os.Stat(path); statErr == nil {
		existed = true
	}
	return existed, writeFileAtomic(path, strconv.Itoa(micro), credentialFileMode)
}

// DeleteProductTier removes a warranty tier's price file — telegram_bot.py:
// 14854-14877 (tierdo:), a direct removal with NO confirmation step (the
// confirm-by-tapping-the-list already happened one screen up, at tierdel:).
// The match is CASE-INSENSITIVE, the same rule ProductTiers/ProductPrice
// already read under: the code discovered off a tier button was itself
// upper-cased by ProductTiers, so deleting by that exact case would silently
// no-op against an on-disk file some other tool (or an older build) created
// lower-case. Runs under the product lock so this can never race a concurrent
// buy that just reserved the last account at this tier's price.
func (s *Store) DeleteProductTier(product, code string) error {
	if strings.ToUpper(code) == "API" { // reserved marker, same guard as SetProductPrice
		return errReservedTierCode
	}
	unlock, err := s.lockProduct(product)
	if err != nil {
		return err
	}
	defer unlock()
	target := strings.ToLower("price_" + code + "_usdt")
	dir := s.productDir(product)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(e.Name()) != target {
			continue
		}
		return os.Remove(filepath.Join(dir, e.Name()))
	}
	return os.ErrNotExist
}
