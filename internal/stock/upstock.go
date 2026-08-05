// Upstock pipeline — the DATA half of accounts.py/telegram_bot.py's
// "adm:upstock" admin flow: per-account-type parsers, the dedup rules the
// running preview and the final commit both rely on, and the accumulator
// that lets an admin paste/upload several messages and see ONE running
// preview grow across them. This file owns exactly the parts described as
// pure/data in the porting spec (.omc/specs/py_stock_spec.md §1.6-1.7):
//
//	ParseUpstockText / ParseOrderFileText / ParseKiroAPIText -> per-type parsers
//	    (telegram_bot.py:8410-8547; ParseJSONAccounts, the "token" type's
//	    parser, already lives in intake.go)
//	dedupNewPairs / dedupNewObjs -> the dedup rules
//	    (telegram_bot.py:8550-8585, _dedup_new_pairs / _dedup_new_objs)
//	UpstockBatch / UpstockPreview -> the accumulator + preview data model
//	    (telegram_bot.py:8588-8665, accumulate_upstock / _upstock_preview_payload,
//	    minus the Telegram network calls and the per-chat rendering-flag
//	    coalescing loop — telegram_bot.py:8668-8723's _render_upstock_loop is a
//	    handler-layer concern: throttling, sendMessage/deleteMessage, and the
//	    "one renderer per chat" guard belong to whatever owns pending_upstock*).
//
// None of these functions perform I/O, take the product lock, or log their
// input: stock files ARE credentials (username/password/API-key lines), so a
// parser must never write what it was handed to a log line, and a rejection
// is always reported as a plain int count, never as an error string that
// could embed the offending line.
package stock

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// upstockUsernameRe validates a plain username/password-account username —
// Python's `[A-Za-z0-9._@+-]+` fullmatch (telegram_bot.py:8434), shared by
// ParseUpstockText and ParseOrderFileText. '@'/'+' allow email usernames
// (Kiro SSO accounts sign in by email).
var upstockUsernameRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)

// upstockDecorationPrefixes are the shop's own decorative/header lines a
// stock listing wraps credentials in — Python's startswith tuple
// (telegram_bot.py:8430): a 10-account block header, emoji rule, or
// Vietnamese label line, never a real credential even when it happens to
// contain no '|'.
var upstockDecorationPrefixes = []string{"📝", "🔐", "━", "Định", "Tài"}

// ParseUpstockText parses pasted/uploaded "username|password" stock text into
// credential pairs — Python `parse_upstock_text` (telegram_bot.py:8410-8438).
// Accepts any number of concatenated 10-account blocks; a credential line is
// any line containing '|' that isn't a decoration line. Splits on the FIRST
// '|' only (a password may itself contain '|') and the password is kept
// verbatim. A username that doesn't fullmatch upstockUsernameRe is counted in
// `bad` rather than silently dropped, so the admin preview's "Rejected (bad
// format)" count is exact.
func ParseUpstockText(blob string) (pairs []CredPair, bad int) {
	for _, raw := range strings.Split(blob, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.Contains(line, "|") {
			continue
		}
		decoration := false
		for _, p := range upstockDecorationPrefixes {
			if strings.HasPrefix(line, p) {
				decoration = true
				break
			}
		}
		if decoration {
			continue
		}
		user, pw, _ := strings.Cut(line, "|")
		user = strings.TrimSpace(user)
		if !upstockUsernameRe.MatchString(user) {
			bad++
			continue
		}
		pairs = append(pairs, CredPair{Username: user, Password: pw})
	}
	return pairs, bad
}

// ParseOrderFileText parses a delivered order .txt (the layout writeOrderFile
// produces) back into credential pairs — Python `parse_order_file_text`
// (telegram_bot.py:8441-8481). This is the "resell" account type: a reseller
// buys from the main shop, receives the order file, and re-stocks it into
// their own shop. Each `username:` line opens a block; the following
// `password:` line (if any) pairs with it — an absent password is kept as ""
// so OTP-login accounts still upstock. A username failing upstockUsernameRe
// counts as `bad`; every other (header/meta) line is silently ignored.
func ParseOrderFileText(blob string) (pairs []CredPair, bad int) {
	var curUser, curPW string
	have := false
	// flush closes out the block currently being accumulated, validating the
	// username exactly once per block (matching Python's nested _flush,
	// telegram_bot.py:8463-8470).
	flush := func() {
		if !have {
			return
		}
		if upstockUsernameRe.MatchString(curUser) {
			pairs = append(pairs, CredPair{Username: curUser, Password: curPW})
		} else {
			bad++
		}
	}
	for _, raw := range strings.Split(blob, "\n") {
		line := strings.TrimSpace(raw)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "username:"):
			flush()
			_, v, _ := strings.Cut(line, ":")
			curUser, curPW, have = strings.TrimSpace(v), "", true
		case strings.HasPrefix(low, "password:") && have:
			_, v, _ := strings.Cut(line, ":")
			curPW = strings.TrimSpace(v)
		}
	}
	flush()
	return pairs, bad
}

// kiroAPIKeyRe matches exactly one Kiro API key — the approved shape
// `ksk_[A-Za-z0-9]+`, taken from the CURRENT parse_kiroapi_text
// (telegram_bot.py:8543; git history: 5568282 first added the parser,
// 003063c "fix: align kiroapi key regex with the approved spec" corrected it
// to this exact pattern, and 47d14cc later layered the '|region' stripping
// below around it without touching the key shape itself).
var kiroAPIKeyRe = regexp.MustCompile(`^ksk_[A-Za-z0-9]+$`)

// kiroAPIMetaPrefixes are the label lines writeOrderFile emits on a kiroapi
// order's "API Keys:" delivery (order.go's writeOrderFile / accounts.py
// _write_order_file's kiroapi branch, stock.go/order.go equivalents) — when a
// reseller re-upstocks a delivered order file, these are decoration, NOT
// counted bad (telegram_bot.py:8524).
var kiroAPIMetaPrefixes = []string{"order id:", "date:", "warranty:", "login portal:", "region:"}

// ParseKiroAPIText parses a pasted/uploaded Kiro API key list into bare keys
// — Python `parse_kiroapi_text` (telegram_bot.py:8492-8547). One `ksk_...`
// key per line; blank lines, the "API Keys:" header, a "---" trailer, and
// order-file meta lines (Order id:/Date:/Warranty:/Login portal:/Region:,
// case-insensitive prefix) are skipped WITHOUT counting bad, so a reseller
// re-upstocking a real order .txt sees a clean "New: N" rather than bad-line
// noise. A vendor 'ksk_xxx|region' suffix has everything after the FIRST '|'
// dropped as decoration (keys aren't region-scoped here) UNLESS a dropped
// field is ITSELF ksk_-shaped (a mangled two-keys-joined paste,
// 'ksk_aaa|ksk_bbb') — then the WHOLE line counts bad, because silently
// keeping only the first half would lose inventory with no warning.
func ParseKiroAPIText(blob string) (keys []string, bad int) {
	for _, raw := range strings.Split(blob, "\n") {
		line := strings.TrimSpace(raw)
		low := strings.ToLower(line)
		if line == "" || low == "api keys:" || strings.HasPrefix(line, "---") {
			continue
		}
		meta := false
		for _, p := range kiroAPIMetaPrefixes {
			if strings.HasPrefix(low, p) {
				meta = true
				break
			}
		}
		if meta {
			continue
		}
		keyPart, rest, _ := strings.Cut(line, "|")
		keyPart = strings.TrimSpace(keyPart)
		// Guard against a mangled paste that joined two keys on one line: if
		// ANY dropped '|'-field is itself key-shaped, reject the whole line
		// rather than silently keeping only keyPart.
		mangled := false
		if rest != "" {
			for _, f := range strings.Split(rest, "|") {
				f = strings.TrimSpace(f)
				if f != "" && kiroAPIKeyRe.MatchString(f) {
					mangled = true
					break
				}
			}
		}
		switch {
		case mangled:
			bad++
		case kiroAPIKeyRe.MatchString(keyPart):
			keys = append(keys, keyPart)
		default:
			bad++
		}
	}
	return keys, bad
}

// dedupNewPairs returns the (user, pw) pairs not already known in ANY product
// state and not repeated within pairs itself — Python `_dedup_new_pairs`
// (telegram_bot.py:8550-8564). `known` is a caller-supplied snapshot (e.g.
// Store.KnownIdentities) rather than a Store+product pair, so this stays a
// pure, directly-testable function with no locking/I-O of its own.
func dedupNewPairs(known map[string]bool, pairs []CredPair) (newPairs []CredPair, dup int) {
	seen := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		if p.Username == "" || known[p.Username] || seen[p.Username] {
			dup++
			continue
		}
		seen[p.Username] = true
		newPairs = append(newPairs, p)
	}
	return newPairs, dup
}

// dedupNewObjs is the JSON-account twin of dedupNewPairs — Python
// `_dedup_new_objs` (telegram_bot.py:8567-8585). The dedup key is the same
// filename-safe email stem AddJSONAccounts files under, so the preview count
// always matches what a subsequent AddJSONAccounts commit will actually add.
func dedupNewObjs(known map[string]bool, objs []JSONAccount) (newObjs []JSONAccount, dup, noEmail int) {
	seen := make(map[string]bool, len(objs))
	for _, o := range objs {
		stem := safeAccountStem(AccountIdentity(o.Obj))
		if stem == "" {
			noEmail++
			continue
		}
		if known[stem] || seen[stem] {
			dup++
			continue
		}
		seen[stem] = true
		newObjs = append(newObjs, o)
	}
	return newObjs, dup, noEmail
}

// UpstockKind selects which parser and dedup path an UpstockBatch uses —
// the Go twin of the "actype" string Python threads through
// pending_upstock/accumulate_upstock. A distinct type (not a bare string) so
// a typo'd literal is caught by the compiler rather than silently falling
// through to the password parser at runtime.
type UpstockKind string

const (
	UpstockPassword UpstockKind = "password" // username|password lines
	UpstockResell   UpstockKind = "resell"   // a delivered order .txt, re-stocked
	UpstockToken    UpstockKind = "token"    // one JSON object per account
	UpstockKiroAPI  UpstockKind = "kiroapi"  // bare ksk_ keys, one per line
)

// UpstockBatch is the DATA side of the running "paste more, see the preview
// grow" upstock accumulator — Python's pending_upstock_acc dict entry
// (telegram_bot.py:8614-8619). One instance per admin chat; the handler layer
// owns the map[chatID]*UpstockBatch, the per-chat renderer/throttle loop, and
// every Telegram call — this type only tracks what has been parsed so far.
//
// Not goroutine-safe by itself, matching the Python dict entry (which is only
// ever mutated under _upstock_lock by the single active renderer for that
// chat); a caller feeding it from multiple goroutines must supply its own
// mutex, same as the handler layer's other pending_* maps already need.
type UpstockBatch struct {
	Product string
	Kind    UpstockKind
	Bad     int // rejected lines across every Add call so far

	pairs []CredPair    // raw (pre-dedup) parses for password/resell/kiroapi
	objs  []JSONAccount // raw (pre-dedup) parses for token
}

// NewUpstockBatch starts a fresh accumulator for one product/type pair —
// the Go twin of pending_upstock_acc.setdefault(...) (telegram_bot.py:8614-8616).
func NewUpstockBatch(product string, kind UpstockKind) *UpstockBatch {
	return &UpstockBatch{Product: product, Kind: kind}
}

// Add parses one pasted/uploaded blob with the parser matching b.Kind and
// appends the results to the running batch — the parsing half of Python's
// `accumulate_upstock` (telegram_bot.py:8600-8619), minus the
// _upstock_lock/renderer bookkeeping the handler layer owns. Any UpstockKind
// other than the four constants above (should never happen — the handler
// layer clamps actype before it reaches here, telegram_bot.py:8399/§1.3 in
// the spec) falls back to the password parser, mirroring Python's own bare
// `else` branch. Returns the number of items newly parsed and lines newly
// rejected by THIS call.
func (b *UpstockBatch) Add(blob string) (parsed, bad int) {
	switch b.Kind {
	case UpstockToken:
		objs, n := ParseJSONAccounts(blob)
		b.objs = append(b.objs, objs...)
		b.Bad += n
		return len(objs), n
	case UpstockResell:
		pairs, n := ParseOrderFileText(blob)
		b.pairs = append(b.pairs, pairs...)
		b.Bad += n
		return len(pairs), n
	case UpstockKiroAPI:
		// Kiro API keys are bare tokens, not username/password pairs; wrap
		// each key as a (key, "") pair so it flows through the same
		// dedup/storage path as password accounts (telegram_bot.py:8605-8610).
		keys, n := ParseKiroAPIText(blob)
		for _, k := range keys {
			b.pairs = append(b.pairs, CredPair{Username: k})
		}
		b.Bad += n
		return len(keys), n
	default: // UpstockPassword, and any unrecognized kind
		pairs, n := ParseUpstockText(blob)
		b.pairs = append(b.pairs, pairs...)
		b.Bad += n
		return len(pairs), n
	}
}

// UpstockPreview is the de-duplicated, render-ready snapshot of a batch at
// one point in time — the Go analogue of what _render_upstock_loop computes
// under the lock each pass (telegram_bot.py:8686-8701) before handing off to
// _upstock_preview_payload. NewPairs/NewObjs are exactly what the handler
// layer should hand to AddAccounts/AddJSONAccounts (after, for token, doing
// its own network refresh per object) when the admin confirms.
type UpstockPreview struct {
	Product string
	Kind    UpstockKind

	NewPairs []CredPair    // password/resell/kiroapi: de-duped new items
	NewObjs  []JSONAccount // token: de-duped new objects
	New      int           // len(NewPairs) or len(NewObjs) — the "Confirm N" count

	Dup     int // already known (any state) or duplicate within the batch
	NoEmail int // token only: object had no usable "email" field
	Bad     int // rejected lines across the whole batch (running Add total)
}

// Preview re-dedups the WHOLE accumulated batch against current stock and
// returns a render-ready snapshot — Python's per-render dedup step
// (telegram_bot.py:8686-8701: _dedup_new_pairs/_dedup_new_objs are called
// fresh against the FULL batch every pass, not just the newest delta, so a
// duplicate introduced earlier in the SAME batch is still caught). Takes its
// own snapshot of s's known stock via Store.KnownIdentities, which — like the
// Python original's _known_usernames call from this same path — is
// deliberately lock-free: a race here can only ever make a PREVIEW stale, the
// real duplicate check happens again under the lock inside
// AddAccounts/AddJSONAccounts at confirm time.
func (b *UpstockBatch) Preview(s *Store) UpstockPreview {
	known := s.KnownIdentities(b.Product)
	p := UpstockPreview{Product: b.Product, Kind: b.Kind, Bad: b.Bad}
	switch b.Kind {
	case UpstockToken:
		objs, dup, noEmail := dedupNewObjs(known, b.objs)
		p.NewObjs, p.Dup, p.NoEmail = objs, dup, noEmail
		p.New = len(objs)
	default: // password, resell, kiroapi all dedup as (user, pw) pairs
		pairs, dup := dedupNewPairs(known, b.pairs)
		p.NewPairs, p.Dup = pairs, dup
		p.New = len(pairs)
	}
	return p
}

// upstockPreviewCharCap is the soft render-size budget the preview body is
// truncated to — Python's literal 3500 (telegram_bot.py:8643), well inside
// Telegram's 4096-char hard message limit even after the summary lines below
// the listing are appended.
const upstockPreviewCharCap = 3500

// runeLen counts Unicode code points, matching Python's len(str) semantics —
// the char budget above must be measured the same way Python measured it
// (code points, not UTF-8 bytes) or an emoji-heavy preview would truncate at
// a different point than the original.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// RenderText builds the preview message BODY — the data half of
// _upstock_preview_payload (telegram_bot.py:8626-8665): title, one "🆕 label"
// row per new item (capped at upstockPreviewCharCap, "… (+N more new)" when
// truncated), then the New/Dup/NoEmail/Bad summary lines, and — when there is
// nothing new yet — the "(Upload more files or paste accounts.)" trailer.
// This is exactly Python's payload["text"]; the confirm/cancel INLINE
// KEYBOARD (shown only when p.New > 0) is presentation, not data, and is the
// handler layer's responsibility to attach.
//
// The 🆕 rows deliberately show the raw username|password / key / email — an
// admin upstock preview exists specifically so the admin can eyeball what is
// about to be added before confirming; that is a feature of this API, not a
// credential leak (contrast with error strings and logs, which must never
// embed a parsed credential).
func (p UpstockPreview) RenderText() string {
	var title string
	var labels []string
	switch p.Kind {
	case UpstockToken:
		title = "📋 JSON upstock preview — " + p.Product
		for _, o := range p.NewObjs {
			labels = append(labels, AccountIdentity(o.Obj))
		}
	case UpstockKiroAPI:
		title = "📋 Kiro API Key upstock preview — " + p.Product
		for _, cp := range p.NewPairs {
			labels = append(labels, cp.Username)
		}
	default:
		title = "📋 Upstock preview — " + p.Product
		for _, cp := range p.NewPairs {
			labels = append(labels, cp.Username+"|"+cp.Password)
		}
	}

	lines := []string{title, ""}
	shown := 0
	for _, label := range labels {
		row := "🆕 " + label
		total := 0
		for _, x := range lines {
			total += runeLen(x) + 1
		}
		if total+runeLen(row) > upstockPreviewCharCap {
			lines = append(lines, fmt.Sprintf("… (+%d more new)", p.New-shown))
			break
		}
		lines = append(lines, row)
		shown++
	}
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("🆕 New to add: %d", p.New))
	if p.Dup > 0 {
		lines = append(lines, fmt.Sprintf("↩️ Skip (already in stock/sold): %d", p.Dup))
	}
	if p.NoEmail > 0 {
		lines = append(lines, fmt.Sprintf("⚠️ Skipped (no \"email\" field): %d", p.NoEmail))
	}
	if p.Bad > 0 {
		lines = append(lines, fmt.Sprintf("⚠️ Rejected (bad format): %d", p.Bad))
	}

	text := strings.Join(lines, "\n")
	if p.New == 0 {
		text += "\n\n(Upload more files or paste accounts.)"
	}
	return text
}
