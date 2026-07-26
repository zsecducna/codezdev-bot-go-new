// Delivery-record writing + order file readers — ports the ./orders/ surface
// of accounts.py: localize/tz_label (accounts.py:47-88), _read_config
// (accounts.py:1166-1184), _write_order_file (accounts.py:1187-1295),
// order_files (accounts.py:1298-1316) and order_usernames
// (accounts.py:1319-1353).
package stock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// langTZ — customer language -> the fixed UTC offset (seconds) its buyers
// expect order times in (accounts.py:47-51). Vietnam (+07) and China (+08)
// observe no DST, so fixed offsets are exact year-round; English (and any
// unlisted language) keeps the server's own local timezone. Fixed zones also
// sidestep the missing-tzdata failure mode the Python code guards against
// (accounts.py:64-68) — delivery must never break on a slim image.
var langTZ = map[string]int{"vi": 7 * 3600, "zh": 8 * 3600}

// localize converts epoch ts to an aware time in the customer's timezone (by
// language) — Python `localize` (accounts.py:54-69). vi -> +07, zh -> +08;
// anything else falls back to server-local (unchanged behavior).
func localize(ts int64, lang string) time.Time {
	if off, ok := langTZ[lang]; ok {
		return time.Unix(ts, 0).In(time.FixedZone("", off))
	}
	return time.Unix(ts, 0).Local()
}

// tzLabel — short timezone label to show next to a localized time — Python
// `tz_label` (accounts.py:72-88). Remapped customer zones (vi/zh) render as
// "(GMT +7)" (numeric offset; their tz abbreviation is ambiguous/absent);
// English/server shows the zone abbreviation (e.g. CEST). Empty string if the
// platform reports neither.
func tzLabel(dt time.Time, lang string) string {
	if _, ok := langTZ[lang]; ok {
		_, off := dt.Zone() // offset east of UTC in seconds
		sign := "+"
		if off < 0 {
			sign = "-"
			off = -off
		}
		hh, mm := off/3600, (off%3600)/60
		// Drop the leading zero on the hour; show :MM only for half-hour
		// zones — "(GMT +7)" is clearer than a bare "+07" (accounts.py:83-87).
		body := fmt.Sprintf("%s%d", sign, hh)
		if mm != 0 {
			body += fmt.Sprintf(":%02d", mm)
		}
		return "(GMT " + body + ")"
	}
	return dt.Format("MST") // server zone abbrev, e.g. CEST (accounts.py:88)
}

// readConfig reads a product's accounts/<product>/config (KEY="value" lines)
// into a map — Python `_read_config` (accounts.py:1166-1184). Delivered to
// the customer in the order file so they know where/how to log in.
func (s *Store) readConfig(product string) map[string]string {
	cfg := make(map[string]string)
	b, err := os.ReadFile(filepath.Join(s.productDir(product), "config"))
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		// Skip blanks, comments, and lines with no '=' (accounts.py:1178-1179).
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		// Python: v.strip().strip('"').strip("'") — trim whitespace, then all
		// surrounding double quotes, then all surrounding single quotes.
		v = strings.Trim(strings.Trim(strings.TrimSpace(v), `"`), "'")
		cfg[strings.TrimSpace(k)] = v
	}
	return cfg
}

// writeOrderFile writes ./orders/<order_id>.txt with the delivered accounts +
// login info — Python `_write_order_file` (accounts.py:1187-1295). Layout:
//
//	Order ID: <id>
//	Date: <YYYY-MM-DD HH:MM:SS> <tz label>
//	Warranty: <policy>            (omitted if not provided)
//	Login portal: <START_URL>
//	Region: <REGION>
//	Account list:
//	---
//	[account 1]
//	username: ...
//	password: ...           (omitted if blank — OTP-login accounts)
//	---
//
// JSON accounts ship each credential as its OWN ./orders/<id>/<email>.json
// file; the detail .txt lists emails + a `file:` reference. Kiro API-key
// products (persisted type "kiroapi") replace the Account list with a bare
// key list under an "API Keys:" header — that header text is load-bearing
// for round-trips (the paste/resell upstock parser skips it) and for
// OrderUsernames. Returns nil on success; the written-paths list the Python
// version returns is reconstructable via OrderFiles.
func (s *Store) writeOrderFile(orderID string, accounts []Delivered, product, warranty, lang string) error {
	if err := os.MkdirAll(s.OrdersDir, credentialDirMode); err != nil {
		return err
	}
	// A pure-JSON order (every account is a JSON object) is a token/credential
	// account, not a portal login — its product config's START_URL/REGION are
	// irrelevant and omitted (accounts.py:1219-1222).
	isPureJSON := len(accounts) > 0
	for _, a := range accounts {
		if a.Kind != KindJSON {
			isPureJSON = false
			break
		}
	}
	// Stamp the delivery time in the BUYER's timezone (by order language),
	// with the tz label appended; strip a trailing space if the platform
	// reports no label (accounts.py:1223-1229).
	dt := localize(time.Now().Unix(), lang)
	label := tzLabel(dt, lang)
	lines := []string{
		"Order ID: " + orderID,
		strings.TrimRight("Date: "+dt.Format("2006-01-02 15:04:05")+" "+label, " "),
	}
	if warranty != "" {
		lines = append(lines, "Warranty: "+warranty)
	}
	cfg := map[string]string{}
	if !isPureJSON && product != "" {
		cfg = s.readConfig(product)
	}
	if cfg["START_URL"] != "" {
		lines = append(lines, "Login portal: "+cfg["START_URL"])
	}
	if cfg["REGION"] != "" {
		lines = append(lines, "Region: "+cfg["REGION"])
	}
	// Kiro API-key products deliver a bare key list under "API Keys:" instead
	// of [account N] blocks — keys have no password or portal login. Keys off
	// the product's persisted type, BUT only when every entry is a plain
	// username-style credential: a stray JSON/legacy-token entry (type flag
	// flipped mid-stock) falls back to the generic per-entry format so nothing
	// is ever delivered mis-labeled (accounts.py:1237-1249).
	allPassword := true
	for _, a := range accounts {
		if a.Kind != KindPassword {
			allPassword = false
			break
		}
	}
	// NOTE: GetAccountType here reads the type file while we already hold the
	// lock pair (Finalize calls us inside its critical section) — the read is
	// lock-free by design (accounts.py:148-158), so no deadlock.
	isKiroAPI := product != "" && !isPureJSON && allPassword &&
		s.GetAccountType(product) == "kiroapi"
	if isKiroAPI {
		lines = append(lines, "API Keys:")
		for _, a := range accounts {
			lines = append(lines, a.Username) // the key IS the identifier (accounts.py:1253)
		}
		lines = append(lines, "---")
		return writeFileAtomic(filepath.Join(s.OrdersDir, orderID+".txt"),
			strings.Join(lines, "\n")+"\n", credentialFileMode)
	}
	lines = append(lines, "Account list:")
	// Format decided PER ENTRY (not per order), so an order mixing JSON,
	// password and legacy-token stock delivers each account correctly
	// (accounts.py:1262-1288).
	jdir := filepath.Join(s.OrdersDir, orderID)
	for i, a := range accounts {
		lines = append(lines, "---", fmt.Sprintf("[account %d]", i+1))
		switch a.Kind {
		case KindJSON:
			// JSON account -> its own .json file under ./orders/<id>/. The
			// stem falls back to "accountN" if the email sanitizes to nothing
			// (accounts.py:1271-1282).
			email := a.Email
			stem := safeAccountStem(email)
			if stem == "" {
				stem = fmt.Sprintf("account%d", i+1)
			}
			name := email
			if name == "" {
				name = stem
			}
			lines = append(lines, "username: "+name) // picked up by OrderUsernames
			lines = append(lines, "file: "+stem+".json")
			if err := os.MkdirAll(jdir, credentialDirMode); err != nil { // lazily on first JSON acct
				return err
			}
			// Python writes json.dumps(obj, ensure_ascii=False, indent=2).
			// json.Indent on the RAW body reformats whitespace while keeping
			// the original key order — closest stdlib match to Python's
			// insertion-order dump.
			var buf bytes.Buffer
			if err := json.Indent(&buf, a.Raw, "", "  "); err != nil {
				buf.Reset()
				buf.Write(a.Raw) // unindentable (shouldn't happen: validated at parse)
			}
			if err := writeFileAtomic(filepath.Join(jdir, stem+".json"),
				buf.String()+"\n", credentialFileMode); err != nil {
				return err
			}
		case KindToken:
			// Legacy single-line token account (accounts.py:1283-1284).
			lines = append(lines, "refresh_token: "+a.Token)
		default:
			// username/password account; password omitted for OTP logins
			// (accounts.py:1285-1288).
			lines = append(lines, "username: "+a.Username)
			if a.Password != "" {
				lines = append(lines, "password: "+a.Password)
			}
		}
	}
	lines = append(lines, "---")
	return writeFileAtomic(filepath.Join(s.OrdersDir, orderID+".txt"),
		strings.Join(lines, "\n")+"\n", credentialFileMode)
}

// OrderFiles returns all delivery file paths for an order, in send order —
// Python `order_files` (accounts.py:1298-1316). The detail
// ./orders/<order_id>.txt first, then any per-account
// ./orders/<order_id>/<email>.json files. Missing pieces are skipped, so this
// is safe for password orders (just the txt) or undelivered orders (empty).
func (s *Store) OrderFiles(orderID string) []string {
	var paths []string
	detail := filepath.Join(s.OrdersDir, orderID+".txt")
	if _, err := os.Stat(detail); err == nil {
		paths = append(paths, detail)
	}
	jdir := filepath.Join(s.OrdersDir, orderID)
	if entries, err := os.ReadDir(jdir); err == nil {
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			paths = append(paths, filepath.Join(jdir, n))
		}
	}
	return paths
}

// kskKeyRe matches exactly one Kiro API key line inside the "API Keys:"
// section (accounts.py:1349) — strict so a future footer line can never be
// mistaken for a delivered key by the ownership gate.
var kskKeyRe = regexp.MustCompile(`^ksk_[A-Za-z0-9]+$`)

// OrderUsernames returns the account identifiers delivered in
// orders/<order_id>.txt — Python `order_usernames` (accounts.py:1319-1353).
// For password/OTP accounts that's each `username:`; for token accounts each
// `refresh_token:` value; for Kiro API-key orders each bare key line under
// the "API Keys:" header. Used for ownership checks and the admin
// customer-lookup view. Empty if the file is absent.
func (s *Store) OrderUsernames(orderID string) []string {
	b, err := os.ReadFile(filepath.Join(s.OrdersDir, orderID+".txt"))
	if err != nil {
		return nil
	}
	var out []string
	inKeys := false // inside the "API Keys:" section
	for _, line := range strings.Split(string(b), "\n") {
		st := strings.TrimSpace(line)
		switch {
		// Prefix checks run on the RAW line like the Python original
		// (accounts.py:1338-1341) — indented lines don't count.
		case strings.HasPrefix(line, "username:"):
			out = append(out, strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
		case strings.HasPrefix(line, "refresh_token:"):
			out = append(out, strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
		case st == "API Keys:":
			inKeys = true // bare keys follow until the trailer
		case inKeys:
			// Only exact key-shaped lines count (accounts.py:1344-1350).
			if kskKeyRe.MatchString(st) {
				out = append(out, st)
			}
		}
	}
	return out
}
