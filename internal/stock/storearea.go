// "store" staging area (Entra / SSO accounts, pre-upstock) — ports
// accounts.py:502-643:
//
//	accounts/<product>/store/<subscription>/<username>.txt -> "email|password"
//
// A holding area for freshly generated / password-reset Entra (SSO) accounts
// that may or may not yet be subscribed to a Kiro plan. Deliberately kept OUT
// of active/held/sold/reserved, so knownUsernamesLocked never sees them and
// they can never be sold by accident. "Upstock from store" promotes a whole
// subscription folder into active/ once the admin confirms those accounts are
// subscribed.
package stock

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// slugSub — lowercase alnum runs joined by single hyphens: a filesystem-safe,
// stable subscription folder name — Python `_slug_sub` (accounts.py:512-523).
// Matches entra_create_users._slug_subname so the folder a username was
// generated under ('z-kiropro-0001' -> 'kiropro') is the same folder a later
// reset / upstock resolves to. Capped at 40 chars, no edge hyphens.
func slugSub(sub string) string {
	var b strings.Builder
	prev := true // suppress a leading hyphen, like Python's prev=True start
	for _, ch := range sub {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
			b.WriteString(strings.ToLower(string(ch)))
			prev = false
		} else if !prev {
			b.WriteByte('-')
			prev = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40] // Python slices to 40 THEN rstrips "-" (accounts.py:523)
	}
	return strings.TrimRight(out, "-")
}

// storeRoot — accounts/<product>/store (accounts.py:526-527).
func (s *Store) storeRoot(product string) string {
	return filepath.Join(s.productDir(product), "store")
}

// storeSubDir — accounts/<product>/store/<slug(subname)>; subname is slugged
// so callers may pass a raw subscription name ('KiroPro') or an already-
// slugged one (accounts.py:530-533).
func (s *Store) storeSubDir(product, subname string) string {
	slug := slugSub(subname)
	if slug == "" {
		slug = "default"
	}
	return filepath.Join(s.storeRoot(product), slug)
}

// StoreEntry is one (username, email, password) row for AddStoreAccounts.
type StoreEntry struct {
	Username string
	Email    string
	Password string
}

// AddStoreAccounts writes store/<subname>/<username>.txt = "email|password"
// for each entry — Python `add_store_accounts` (accounts.py:536-566).
// Overwrites an existing file (a re-reset replaces the saved password). Files
// are 0600 and the folder 0700 (plaintext passwords). Returns the number
// written. Does NOT touch active/sold/etc., and these accounts are invisible
// to knownUsernamesLocked, so they are never for sale until promoted.
func (s *Store) AddStoreAccounts(product, subname string, entries []StoreEntry) (int, error) {
	d := s.storeSubDir(product, subname)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := os.MkdirAll(d, credentialDirMode); err != nil {
		return 0, err
	}
	_ = os.Chmod(d, credentialDirMode) // tighten a pre-existing dir too (accounts.py:549-552)
	n := 0
	for _, e := range entries {
		// Stem from username, else email; both must be filename-safe
		// (accounts.py:555-557).
		stem := safeAccountStem(e.Username)
		if stem == "" {
			stem = safeAccountStem(e.Email)
		}
		if stem == "" || e.Email == "" {
			continue
		}
		// 0600 tmp + rename — the Python original os.open(...0o600)+fchmod
		// then os.replace (accounts.py:558-564).
		if err := writeFileAtomic(filepath.Join(d, stem+".txt"),
			fmt.Sprintf("%s|%s\n", e.Email, e.Password), credentialFileMode); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// StoreSub is one subscription folder summary (accounts.py:571).
type StoreSub struct {
	Name  string
	Count int
}

// ListStoreSubs — subscription folders under store/ with their account
// counts, name-sorted — Python `list_store_subs` (accounts.py:569-582).
// Empty if no store yet. Lock-free read, same as the original.
func (s *Store) ListStoreSubs(product string) []StoreSub {
	root := s.storeRoot(product)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []StoreSub
	for _, name := range names {
		d := filepath.Join(root, name)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, StoreSub{Name: name, Count: len(listTxt(d))})
		}
	}
	return out
}

// CountStore — total accounts staged across every store/ subscription folder
// — Python `count_store` (accounts.py:585-587).
func (s *Store) CountStore(product string) int {
	total := 0
	for _, sub := range s.ListStoreSubs(product) {
		total += sub.Count
	}
	return total
}

// UpstockResult is the summary dict upstock_from_store returns
// (accounts.py:641-643).
type UpstockResult struct {
	Added          int      // promoted into active/
	Skipped        int      // SkippedKnown + SkippedNoEmail
	SkippedKnown   int      // already in stock/sold -> file CONSUMED (removed)
	SkippedNoEmail int      // no email in file -> KEPT for inspection
	Total          int      // store files examined
	AddedUsers     []string // the promoted emails
}

// UpstockFromStore promotes store/<subname> accounts into active/ as password
// accounts — Python `upstock_from_store` (accounts.py:590-643,
// the §591-631 promotion logic). Each store file ("email|password") becomes
// active/<email>.txt holding:
//
//	username: <email>\n
//	password: <pw>\n      (line omitted when pw is blank)
//
// (buyers sign in to Kiro SSO with the email). Skips any email already
// present in ANY state. Promoted AND already-known files are removed from
// store (consumed); files with no email are left behind for inspection. An
// emptied sub-folder is removed. Goroutine- and process-safe.
func (s *Store) UpstockFromStore(product, subname string) (UpstockResult, error) {
	active, _, _ := s.dirs(product)
	src := s.storeSubDir(product, subname)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return UpstockResult{}, err
	}
	defer unlock()
	known := s.knownUsernamesLocked(product)
	if err := os.MkdirAll(active, credentialDirMode); err != nil {
		return UpstockResult{}, err
	}
	res := UpstockResult{}
	for _, f := range listTxt(src) {
		res.Total++
		spath := filepath.Join(src, f)
		body := readWholeFile(spath)
		// "email|password" split; no "|" -> whole body is the email, pw ""
		// (Python partition semantics, accounts.py:613).
		email, pw, _ := strings.Cut(body, "|")
		email = strings.TrimSpace(email)
		stem := safeAccountStem(email)
		if stem == "" {
			res.SkippedNoEmail++
			continue // no email -> KEPT for inspection (accounts.py:616-618)
		}
		if known[stem] {
			res.SkippedKnown++
			_ = os.Remove(spath) // already in stock/sold -> consume (accounts.py:619-622)
			continue
		}
		known[stem] = true
		// Path uses the sanitized stem; the body carries the RAW email
		// (accounts.py:625-631).
		bodyOut := fmt.Sprintf("username: %s\n", email)
		if pw != "" {
			bodyOut += fmt.Sprintf("password: %s\n", pw)
		}
		if err := writeFileAtomic(filepath.Join(active, stem+".txt"), bodyOut, credentialFileMode); err != nil {
			res.Skipped = res.SkippedKnown + res.SkippedNoEmail
			res.Added = len(res.AddedUsers)
			return res, err
		}
		_ = os.Remove(spath) // promoted -> remove from store (accounts.py:632-633)
		res.AddedUsers = append(res.AddedUsers, email)
	}
	// Remove the sub-folder once emptied, best-effort (accounts.py:635-639).
	if entries, err := os.ReadDir(src); err == nil && len(entries) == 0 {
		_ = os.Remove(src)
	}
	res.Added = len(res.AddedUsers)
	res.Skipped = res.SkippedKnown + res.SkippedNoEmail
	return res, nil
}
