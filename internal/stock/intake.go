// Stock intake — ports accounts.py's add paths: add_accounts
// (accounts.py:347-376), parse_json_accounts (accounts.py:380-426),
// account_identity / _safe_account_stem / account_stem (accounts.py:429-463)
// and add_json_accounts (accounts.py:466-499).
package stock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AddResult is the summary Python's add_accounts / add_json_accounts return
// as a dict (accounts.py:354, 474).
type AddResult struct {
	Added      int      // credential files written into active/
	Skipped    int      // duplicates (already known in ANY state or in-batch)
	Bad        int      // JSON path only: objects with no usable email
	AddedUsers []string // the stems actually filed
}

// CredPair is one (username, password) input row for AddAccounts.
type CredPair struct {
	Username string
	Password string // may be "" for OTP-login accounts
}

// AddAccounts adds new credential files to a product's active/ from
// (user, pass) pairs — Python `add_accounts` (accounts.py:347-376). Writes
// active/<username>.txt with the standard body:
//
//	username: <user>\n
//	password: <pw>\n      (line omitted when pw is blank)
//
// Skips any username already present in ANY state (active/held/sold/reserved)
// and any duplicate within pairs. Goroutine- and process-safe.
func (s *Store) AddAccounts(product string, pairs []CredPair) (AddResult, error) {
	active, _, _ := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return AddResult{}, err
	}
	defer unlock()
	known := s.knownUsernamesLocked(product)
	if err := os.MkdirAll(active, credentialDirMode); err != nil {
		return AddResult{}, err
	}
	res := AddResult{}
	for _, p := range pairs {
		if p.Username == "" || known[p.Username] {
			res.Skipped++
			continue
		}
		known[p.Username] = true // dedupe within this batch (accounts.py:366)
		// The exact credential body format the whole pipeline round-trips on
		// (accounts.py:369-372): password line only when non-blank.
		body := fmt.Sprintf("username: %s\n", p.Username)
		if p.Password != "" {
			body += fmt.Sprintf("password: %s\n", p.Password)
		}
		if err := writeFileAtomic(filepath.Join(active, p.Username+".txt"), body, credentialFileMode); err != nil {
			return res, err
		}
		res.AddedUsers = append(res.AddedUsers, p.Username)
	}
	res.Added = len(res.AddedUsers)
	return res, nil
}

// JSONAccount is one parsed admin-supplied JSON account: the decoded object
// plus its raw bytes. Raw is kept so downstream writes (active/ file,
// delivery .json) preserve the source's key order — Python dicts preserve
// insertion order on json.dumps; Go maps don't, so the raw bytes are the
// fidelity anchor.
type JSONAccount struct {
	Obj map[string]any  // decoded object (identity lookup)
	Raw json.RawMessage // the object's original bytes
}

// ParseJSONAccounts parses admin-supplied text into JSON account objects —
// Python `parse_json_accounts` (accounts.py:380-426). One JSON object = one
// account. Accepts a JSON array of objects, a single object, or several
// objects run together / separated by whitespace, commas or newlines.
// Returns (accounts, bad) where bad counts chunks that weren't a JSON object.
func ParseJSONAccounts(text string) ([]JSONAccount, int) {
	var accounts []JSONAccount
	bad := 0
	src := strings.TrimSpace(text)
	i, n := 0, len(src)
	for i < n {
		// Skip separators between top-level values: whitespace, commas, and
		// stray array brackets left by concatenated arrays (accounts.py:394-399).
		for i < n && strings.ContainsRune(" \t\r\n,", rune(src[i])) {
			i++
		}
		if i >= n {
			break
		}
		// raw_decode equivalent: decode ONE value from src[i:] and learn how
		// many bytes it consumed via InputOffset.
		dec := json.NewDecoder(strings.NewReader(src[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			// Malformed chunk. Count it and resync to the next value start
			// ({ or [) so one bad object doesn't drop every later valid
			// account (accounts.py:402-414).
			bad++
			nxt := -1
			for _, ch := range []string{"{", "["} {
				if p := strings.Index(src[i+1:], ch); p != -1 {
					if nxt == -1 || i+1+p < nxt {
						nxt = i + 1 + p
					}
				}
			}
			if nxt == -1 {
				break // nothing parseable left
			}
			i = nxt
			continue
		}
		i += int(dec.InputOffset())
		switch raw[0] {
		case '{':
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				accounts = append(accounts, JSONAccount{Obj: obj, Raw: raw})
			} else {
				bad++
			}
		case '[':
			// A top-level array of accounts: each dict element is one account
			// (accounts.py:418-423).
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) == nil {
				for _, it := range items {
					var obj map[string]any
					if len(it) > 0 && it[0] == '{' && json.Unmarshal(it, &obj) == nil {
						accounts = append(accounts, JSONAccount{Obj: obj, Raw: it})
					} else {
						bad++
					}
				}
			} else {
				bad++
			}
		default:
			bad++ // bare string/number/etc. (accounts.py:425)
		}
	}
	return accounts, bad
}

// AccountIdentity — the username/identity of a JSON account = its `email`
// field, stringified + trimmed — Python `account_identity`
// (accounts.py:429-438). Empty string if absent (such an object can't be
// filed). A JSON null email is treated as "" (the Python str(None)="None"
// path is a pathological input we deliberately normalize away).
func AccountIdentity(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	v, ok := obj["email"]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

// safeAccountStem — filename-safe stem derived from an account's
// email/identity — Python `_safe_account_stem` (accounts.py:441-453). Strips
// path separators, NULs and leading dots so a crafted `email` can't escape
// the product folder or create a hidden file. Returns "" if nothing usable
// remains (the account is then rejected, never filed under a bad name).
func safeAccountStem(name string) string {
	name = strings.TrimSpace(name)
	name = strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name)
	name = strings.TrimLeft(name, ".")
	if name == "." || name == ".." { // unreachable after TrimLeft; kept for parity
		return ""
	}
	return name
}

// AccountStem — filename-safe dedupe key for a JSON account (from its email)
// — Python `account_stem` (accounts.py:456-463). Same stem AddJSONAccounts
// files the account under, so callers can test against KnownIdentities before
// upstocking. "" = no usable email.
func AccountStem(obj map[string]any) string {
	return safeAccountStem(AccountIdentity(obj))
}

// AddJSONAccounts adds JSON-object accounts to a product's active/ — Python
// `add_json_accounts` (accounts.py:466-499). Each object is one account,
// filed as active/<email>.txt holding the compact JSON. The `email` field is
// the identity, so dedup uses the same stem set as password accounts — an
// email already in ANY state or earlier in this batch is skipped; objects
// with no usable email count in Bad.
func (s *Store) AddJSONAccounts(product string, objs []JSONAccount) (AddResult, error) {
	active, _, _ := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return AddResult{}, err
	}
	defer unlock()
	known := s.knownUsernamesLocked(product)
	if err := os.MkdirAll(active, credentialDirMode); err != nil {
		return AddResult{}, err
	}
	res := AddResult{}
	seen := make(map[string]bool)
	for _, a := range objs {
		stem := safeAccountStem(AccountIdentity(a.Obj))
		if stem == "" {
			res.Bad++ // no usable email -> can't file (accounts.py:485-487)
			continue
		}
		if known[stem] || seen[stem] {
			res.Skipped++
			continue
		}
		seen[stem] = true // dedupe within this batch too (accounts.py:491)
		// Python writes json.dumps(obj, ensure_ascii=False) — a compact dump
		// preserving insertion order. json.Compact on the RAW bytes is the
		// key-order-preserving Go equivalent.
		var buf bytes.Buffer
		if err := json.Compact(&buf, a.Raw); err != nil {
			buf.Reset()
			buf.Write(a.Raw)
		}
		if err := writeFileAtomic(filepath.Join(active, stem+".txt"), buf.String(), credentialFileMode); err != nil {
			return res, err
		}
		res.AddedUsers = append(res.AddedUsers, stem)
	}
	res.Added = len(res.AddedUsers)
	return res, nil
}

// tryParseJSONObject returns the decoded object if raw is a JSON object, else
// nil — the sniff Finalize uses (`raw.startswith("{")` + json.loads +
// isinstance dict, accounts.py:1398-1404).
func tryParseJSONObject(raw string) map[string]any {
	if !strings.HasPrefix(raw, "{") {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return nil
	}
	return obj
}
