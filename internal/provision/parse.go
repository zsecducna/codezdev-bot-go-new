package provision

import (
	"regexp"
	"strings"
	"unicode"
)

// Flow is the supply route one credential line takes into the Kiro-Go pool.
// Python original: the "flow" key of classify_account_line
// (kiro_provision.py:151-203) and the _FLOW_RUNNERS dispatch table
// (kiro_provision.py:919).
type Flow string

const (
	// FlowIDC is `username|password|start_url[|mfa_secret]` — an AWS IAM
	// Identity Center browser sign-in driven by kiro-web-login.py, which
	// mints a ksk_ Kiro API key. Ends at POST /admin/add_kiro_api_key.
	// (kiro_provision.py:13-16)
	FlowIDC Flow = "idc"
	// FlowM365 is `email|password[|mfa_secret]` — a Microsoft 365 / Entra ID
	// SSO sign-in driven by kiro-go-login.py, which writes a full Kiro-Go
	// account JSON (OAuth access+refresh tokens, authMethod "external_idp").
	// Ends at POST /admin/add_kiro_account. (kiro_provision.py:17-21)
	FlowM365 Flow = "m365"
	// FlowRawKey is a bare, already-minted `ksk_…` key on its own line. No
	// browser login at all — the key goes straight to the pool through the
	// same endpoint the idc flow ends at. (kiro_provision.py:23-26)
	FlowRawKey Flow = "rawkey"
)

// Account is one classified credential line.
//
// The three forms cannot collide (kiro_provision.py:28-32): the idc form
// REQUIRES an http(s) URL in field 3, which an m365 line never has; a rawkey
// line is a single `ksk_` token with no `|` at all, which neither credential
// form can be. A field 3 that looks like a URL path but lacks a scheme is
// refused outright rather than guessed at.
type Account struct {
	Flow Flow
	// Username is the sign-in identity — or, for FlowRawKey, the raw ksk_
	// key itself (that is the identity we report and add,
	// kiro_provision.py:160-161). SECURITY: for a rawkey account this field
	// therefore holds a LIVE credential and must never be logged or shown;
	// the orchestrator replaces it with a masked form in every report (see
	// provisionRawKey).
	Username string
	// Password is taken RAW (never trimmed) — a delivered password may
	// legitimately contain spaces, and it must reach the login byte-for-byte
	// (kiro_provision.py:1058-1062).
	Password string
	// StartURL is the AWS IdC portal for an idc line; empty otherwise.
	StartURL string
	// MFASecret is the TOTP seed of an ALREADY-enrolled account (idc field 4
	// / m365 field 3). Optional: an account is enrolled by us on its first
	// run and only carries a seed from then on (kiro_provision.py:180-183).
	MFASecret string

	// flowIndex is this account's 1-based position within its OWN flow's
	// rebuilt accounts file. Each login script numbers --index over only the
	// lines IT recognizes, so the two flows keep independent index spaces —
	// a shared file would desynchronize them and log in the WRONG account
	// (kiro_provision.py:951-955). Set by the orchestrator, not by parsing.
	flowIndex int
}

// _EMAIL_RE (kiro_provision.py:126), copied verbatim from kiro-go-login.py. It
// forbids whitespace and requires a dotted domain, so decorative/header lines
// that happen to contain "|" and "@" are not mistaken for credentials.
var emailRe = regexp.MustCompile(`^[^@\s|]+@[^@\s|]+\.[^@\s|]+$`)

// _MFA_STDERR_RE (kiro_provision.py:131) matches kiro-go-login.py's last-ditch
// stderr report of a freshly enrolled TOTP seed, printed ONLY when it could
// not write the seed back to the accounts file.
var mfaStderrRe = regexp.MustCompile(`!! MFA secret for (\S+): (\S+)`)

// secretReportRe matches the WHOLE FAMILY of "here is the credential I just
// minted" lines the vendored login scripts emit, so tailDetail can redact the
// value while keeping the line for triage. mfaStderrRe above covers exactly one
// of them (kiro-go-login.py:886) and must stay byte-exact because provisionM365
// also CAPTURES through it; this one is redaction-only and deliberately wider.
//
// The siblings it exists for — every one of these carries a LIVE credential and
// every one can be among the last lines of a login that then fails, which is
// precisely when tailDetail runs:
//
//	kiro-web-login.py:577,1010  stderr  "  !! attempted NEW PASSWORD for <user>: <pw>"
//	kiro-web-login.py:586,1017  stderr  "  !! NEW PASSWORD for <user>: <pw>"
//	kiro-web-login.py:704       stdout  "  -> MFA SECRET for <user>: <seed>"
//	kiro-go-login.py:886        stderr  "  !! MFA secret for <email>: <seed>"
//
// The password pair is the worst of them: it is printed on STDERR, which
// tailDetail PREFERS over stdout, and the 577/1010 variant is followed
// immediately by an error return — so a policy-rejected password reset would
// otherwise put a live plaintext password into a Telegram chat log every time.
// kiro-web-login.py:697-703 reasons about this exposure and concludes the seed
// is safe on stdout "because neither stream reaches journald"; that reasoning
// was sound for the Python bot, which never forwarded _tail_detail anywhere,
// and does NOT hold here — Result.Detail is streamed through ProgressFunc.
//
// Case-insensitive because the scripts disagree on capitalisation ("NEW
// PASSWORD", "MFA SECRET", "MFA secret"). The value is matched to end-of-line
// rather than as one \S+ token, because a delivered password may contain
// spaces and a partial redaction is no redaction.
var secretReportRe = regexp.MustCompile(
	`(?i)((?:!!|->)\s+(?:attempted )?(?:new password|mfa secret) for (\S+): ).+`)

// _RAWKEY_RE (kiro_provision.py:140). The {13,} floor is TWO things at once:
// it rejects a stray/truncated "ksk_" token, AND it guarantees the token is
// >16 chars so mask() always truncates it — a shorter key would be returned
// verbatim by mask and leak IN FULL into the progress line / summary /
// nickname, the exact exposure the rawkey masking exists to prevent.
var rawKeyRe = regexp.MustCompile(`^ksk_[^\s|]{13,}$`)

// _ORDER_BODY_RE (kiro_provision.py:148) matches the account-block body labels
// a bot-delivered order file carries (accounts.py:_write_order_file). A pipe or
// ksk line never carries a colon-label, so this can never false-positive on
// them.
var orderBodyRe = regexp.MustCompile(`(?i)^\s*(?:username|password|file|refresh_token):`)

// _ORDER_HEADER_RE (kiro_provision.py:213) pulls the shared IdC portal out of a
// delivered order file's `Login portal: <url>` header. That one URL is the
// start_url for EVERY account in the file.
var orderHeaderRe = regexp.MustCompile(`(?i)^\s*Login portal:\s*(\S+)`)

// _ORDER_KV_RE (kiro_provision.py:219) splits an account-block "key: value"
// line into its label + value. Only the single post-colon separator space is
// consumed, so a password value is otherwise taken RAW (a delivered password
// may legitimately contain a ":" or spaces).
var orderKVRe = regexp.MustCompile(`^\s*([A-Za-z_]+):[ \t]?(.*)$`)

// ClassifyAccountLine classifies one accounts-file line, reporting ok=false
// when it isn't a credential. Port of kiro_provision.py:151-203.
//
// The URL test runs FIRST and decides: an idc line requires an http(s) URL in
// field 3, which an m365 line never has. That ordering also means an idc
// account whose username happens to be an email is still correctly read as
// idc — misrouting it would sign in to Microsoft with an IdC password.
func ClassifyAccountLine(line string) (Account, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "Accounts:") || strings.HasPrefix(s, "Format:") {
		return Account{}, false
	}

	// Flow 3 (rawkey): a single pre-minted `ksk_` token, no pipe. Tested
	// BEFORE the "|" guard below because it is the one credential form that
	// carries no "|" (kiro_provision.py:167-171).
	if rawKeyRe.MatchString(s) {
		return Account{Flow: FlowRawKey, Username: s}, true
	}

	if !strings.Contains(s, "|") {
		return Account{}, false
	}
	parts := strings.Split(s, "|")
	user := strings.TrimSpace(parts[0])
	field3 := ""
	if len(parts) > 2 {
		field3 = strings.TrimSpace(parts[2])
	}

	// Flow 1 (idc): username|password|start_url[|mfa_secret]. Classification
	// keys on field 3 being a URL, so a 4-field idc line can never be
	// mistaken for the 3-field m365 shape (kiro_provision.py:179-187).
	if strings.HasPrefix(field3, "http://") || strings.HasPrefix(field3, "https://") {
		mfa := ""
		if len(parts) > 3 {
			mfa = strings.TrimSpace(parts[3])
		}
		return Account{
			Flow:      FlowIDC,
			Username:  user,
			Password:  parts[1], // raw, never trimmed
			StartURL:  field3,
			MFASecret: mfa,
		}, true
	}

	// Flow 2 (m365): email|password[|mfa_secret] (kiro_provision.py:189-202).
	if emailRe.MatchString(user) {
		// A field-3 that looks like a URL PATH but failed the scheme test
		// above is a malformed start_url (a dropped "https://"), NOT a TOTP
		// seed — which is base32 and so never contains "/". Refusing to
		// classify surfaces the line as unparsed in the pre-batch count;
		// guessing "m365" would instead sign in to Microsoft with an IdC
		// password and possibly re-enroll MFA against a garbage seed.
		if strings.Contains(field3, "/") {
			return Account{}, false
		}
		password := ""
		if len(parts) > 1 {
			password = parts[1] // raw, never trimmed
		}
		return Account{Flow: FlowM365, Username: user, Password: password, MFASecret: field3}, true
	}
	return Account{}, false
}

// looksLikeOrderFile reports whether `text` is a bot-delivered order file
// rather than a one-per-line paste. Port of kiro_provision.py:222-241.
//
// Requires BOTH signatures of the delivered format together: a block body
// label (`username:` / `password:` / `file:` / `refresh_token:`) AND a block
// opener (a `---` separator or an `[account N]` header). A delivered order
// always has both; a one-per-line pipe/ksk paste has NEITHER. Demanding both
// (not just a body label) stops a one-line paste that happens to carry a stray
// `username:`-style line from being wholly rerouted to the order parser —
// which would silently drop its every pipe/ksk line, since that parser only
// reads accounts inside `---`/`[account]` blocks.
func looksLikeOrderFile(text string) bool {
	hasBody, hasBlock := false, false
	for _, line := range strings.Split(text, "\n") {
		if orderBodyRe.MatchString(line) {
			hasBody = true
		}
		// TrimSpace also eats a trailing \r, so a CRLF order file is
		// detected without normalizing first (matching Python's .strip()).
		s := strings.TrimSpace(line)
		if s == "---" || strings.HasPrefix(s, "[account") {
			hasBlock = true
		}
		if hasBody && hasBlock {
			return true
		}
	}
	return false
}

// ParseOrderFileAccounts expands a bot-delivered order .txt into classified
// IdC records. Port of kiro_provision.py:244-362.
//
// Every provisionable block becomes the idc record `username|password|
// start_url` — the SAME shape ClassifyAccountLine emits for a one-line idc
// paste — so the whole downstream pipeline (browser login -> ksk_ key -> pool
// add -> MFA-seed capture) is reused with no new flow. This is the RIGHT flow
// for these accounts: the start_url opens an AWS IdC sign-in that federates
// onward to M365/Entra SSO, forces a password change on first login, and MAY
// enroll MFA — all of which kiro-web-login.py already handles.
//
// Blocks that are NOT a portal login are skipped on purpose, because they
// cannot be provisioned from the .txt: token accounts (`refresh_token:`),
// JSON accounts (`file:` — the credential lives in a separate per-account
// .json), password-less accounts (OTP-login stock), and every account when the
// file carries no valid `Login portal:` URL.
func ParseOrderFileAccounts(text string) []Account {
	out := []Account{}
	startURL := "" // current shared portal; empty until a valid one is seen

	// cur accumulates the block being read; curOpen distinguishes "no block
	// opened yet" (header lines before the first `---` must not accumulate)
	// from "an empty block".
	var cur Account
	curSkip := false
	curOpen := false

	flush := func() {
		// Emit the block under construction as an idc record, if it is a
		// provisionable portal login. Silently drops anything that isn't.
		if !curOpen {
			return
		}
		if curSkip || startURL == "" {
			return
		}
		user := strings.TrimSpace(cur.Username)
		if user == "" || cur.Password == "" {
			return
		}
		// A "|" in the username or password can't be provisioned through the
		// idc flow: the orchestrator rebuilds each idc line pipe-delimited
		// (`user|pw|start_url`) and kiro-web-login.py reads it back by
		// splitting on "|", so a pipe inside a field would shift every field
		// after it. Drop the block rather than emit a record that silently
		// corrupts on the round-trip (kiro_provision.py:300-309).
		if strings.Contains(user, "|") || strings.Contains(cur.Password, "|") {
			return
		}
		out = append(out, Account{
			Flow:      FlowIDC,
			Username:  user,
			Password:  cur.Password,
			StartURL:  startURL,
			MFASecret: strings.TrimSpace(cur.MFASecret),
		})
	}

	// Normalize CRLF/CR up front: Telegram/Windows uploads carry CRLF, and a
	// "password:\r" would otherwise smuggle a trailing CR into the credential.
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		// A new shared portal: update it for every block that follows. Order
		// files never repeat it per account, but a concatenation of two
		// orders will — so it is tracked as the walk proceeds, not read once
		// up front, and each order stamps its own accounts.
		if m := orderHeaderRe.FindStringSubmatch(line); m != nil {
			url := strings.TrimSpace(m[1])
			// Only accept an http(s) portal — a malformed/absent one leaves
			// startURL as-is, and blocks under no valid portal are dropped by
			// flush rather than provisioned against a garbage start_url.
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				startURL = url
			}
			continue
		}

		// A "---" separator or an "[account N]" header opens a new block.
		stripped := strings.TrimSpace(line)
		if stripped == "---" || strings.HasPrefix(stripped, "[account") {
			flush()
			cur, curSkip, curOpen = Account{}, false, true
			continue
		}

		// Inside a block, collect the fields we care about. Anything that
		// isn't a recognized "key: value" (the "Account list:" header,
		// "Region:", blanks) is ignored.
		if !curOpen {
			continue
		}
		kv := orderKVRe.FindStringSubmatch(line)
		if kv == nil {
			continue
		}
		switch strings.ToLower(kv[1]) {
		case "username":
			cur.Username = kv[2]
		case "password":
			// RAW value (only the single post-colon separator space is
			// consumed by the regex): a password may contain ":" or spaces,
			// and it must reach the login byte-for-byte.
			cur.Password = kv[2]
		case "mfa_secret", "totp", "mfa":
			cur.MFASecret = kv[2]
		case "refresh_token", "file":
			// Not a portal login: a token account, or a JSON account whose
			// credential is a separate file. Mark the block so flush drops it
			// even if it also carries a username line (JSON blocks emit
			// `username:` for display).
			curSkip = true
		}
	}

	flush() // the last block has no trailing "---" opener to flush it
	return out
}

// ParseAllAccounts returns a classified Account per credential line in `text`,
// in file order. Unrecognized lines are skipped (headers, comments, blanks).
// Port of kiro_provision.py:365-384.
//
// Two input shapes are accepted and auto-detected: one credential per line, or
// a whole bot-delivered order .txt expanded to idc records. The order-file
// shape is recognized by its colon-label block body plus a block opener, which
// a one-per-line paste never has, so the two can't be confused.
func ParseAllAccounts(text string) []Account {
	if looksLikeOrderFile(text) {
		return ParseOrderFileAccounts(text)
	}
	out := []Account{}
	for _, line := range strings.Split(text, "\n") {
		if rec, ok := ClassifyAccountLine(line); ok {
			out = append(out, rec)
		}
	}
	return out
}

// ParseIDCAccounts returns only the FlowIDC accounts in `text`, in file order.
// Port of parse_accounts (kiro_provision.py:387-391), kept for the original
// single-flow callers; new code wants ParseAllAccounts.
func ParseIDCAccounts(text string) []Account {
	out := []Account{}
	for _, a := range ParseAllAccounts(text) {
		if a.Flow == FlowIDC {
			out = append(out, a)
		}
	}
	return out
}

// OneAccountText returns an accounts-file text containing only the `index`-th
// credential line (1-based over ALL flows, matching the order the orchestrator
// reports — not each flow's private index space). Port of _one_account_text
// (kiro_provision.py:1155-1167). An out-of-range index returns `text`
// unchanged, exactly as Python does.
//
// The line is rebuilt from the classified fields, so it comes back in
// whichever flow's canonical format that account belongs to.
//
// DELIBERATE DEVIATION from the Python original, in the rawkey case only:
// Python falls through to `f"{username}|{password}"`, which for a rawkey
// account (password "") emits `ksk_…|` — a string that carries a pipe and so
// re-classifies as NOTHING, silently yielding zero accounts. Emitting the bare
// key instead is a strict fix: the Python output was unparseable, so nothing
// can depend on it.
func OneAccountText(text string, index int) string {
	accts := ParseAllAccounts(text)
	if index < 1 || index > len(accts) {
		return text
	}
	a := accts[index-1]
	switch a.Flow {
	case FlowIDC:
		// Note: matches Python in dropping field 4. The seed is re-attached
		// by the orchestrator when it rebuilds the temp idc file from the
		// classified record, so nothing is lost on the provisioning path.
		return a.Username + "|" + a.Password + "|" + a.StartURL
	case FlowRawKey:
		return a.Username
	default:
		if a.MFASecret != "" {
			return a.Username + "|" + a.Password + "|" + a.MFASecret
		}
		return a.Username + "|" + a.Password
	}
}

// safeUser returns the filename-safe form of a username, matching
// kiro-web-login.save_api_key so we can locate the JSON that login just wrote.
// Port of _safe_user (kiro_provision.py:394-397).
//
// Python's str.isalnum() is Unicode-aware, so a non-ASCII letter is KEPT; the
// unicode.IsLetter/IsDigit/IsNumber triple is the Go equivalent of that
// category test. Every other rune becomes a single "_" (one per rune, not one
// per run — that is the difference from safeComponent below).
func safeUser(username string) string {
	var b strings.Builder
	for _, r := range username {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsNumber(r) ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// safeComponent returns the filename-safe form matching
// kiro-login-helper.sanitize_file_component, so we can locate the account JSON
// kiro-go-login.py just wrote. Port of _safe_component
// (kiro_provision.py:400-417).
//
// This deliberately DIFFERS from safeUser: it accepts only ASCII alphanumerics
// (Python's `ch.isalnum() and ch.isascii()`), collapses runs of unsafe chars
// to a SINGLE dash, and strips leading/trailing dashes — e.g.
// "a.b@c-d.win" -> "a.b-c-d.win". Getting this wrong means looking for the
// wrong filename and reporting a successful login as a failure.
func safeComponent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		isASCIIAlnum := r < unicode.MaxASCII &&
			(unicode.IsLetter(r) || unicode.IsDigit(r))
		if isASCIIAlnum || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// mask returns the short display form of a captured credential (a ksk_ key or
// an OAuth token) — prefix + tail only, never enough to use. Port of _mask
// (kiro_provision.py:1080-1084).
//
// The >16 threshold pairs with rawKeyRe's {13,} floor: together they guarantee
// every accepted rawkey is long enough to actually be truncated here, so a
// short/garbage key can never be echoed in full into a progress line.
func mask(k string) string {
	r := []rune(k)
	if len(r) > 16 {
		return string(r[:8]) + "…" + string(r[len(r)-4:])
	}
	return k
}
