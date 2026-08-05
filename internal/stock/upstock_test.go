package stock

// Tests for upstock.go: the per-type parsers, the dedup rules, and the
// accumulator/preview data model that back the "adm:upstock" admin flow
// (.omc/specs/py_stock_spec.md §1.6-1.7). Table-driven per parser, plus
// integration tests running the accumulator against a real Store so the
// dedup-against-known-stock path is exercised end to end.

import (
	"strings"
	"testing"
)

// TestParseUpstockText covers the password-type parser: '|' required,
// decoration lines skipped, first-'|' split only (password may itself
// contain '|'), and username-shape rejection.
func TestParseUpstockText(t *testing.T) {
	cases := []struct {
		name      string
		blob      string
		wantPairs []CredPair
		wantBad   int
	}{
		{
			name:      "plain pairs",
			blob:      "alice|s3cret\nbob|hunter2",
			wantPairs: []CredPair{{Username: "alice", Password: "s3cret"}, {Username: "bob", Password: "hunter2"}},
		},
		{
			name:      "email username allowed",
			blob:      "alice+tag@example.com|pw",
			wantPairs: []CredPair{{Username: "alice+tag@example.com", Password: "pw"}},
		},
		{
			name:      "password may itself contain a pipe, split on FIRST only",
			blob:      "alice|pw|with|pipes",
			wantPairs: []CredPair{{Username: "alice", Password: "pw|with|pipes"}},
		},
		{
			name: "decoration lines ignored even though some contain no pipe",
			blob: "🔐 Tài khoản (10)\n" +
				"📝 Định dạng: username|password\n" +
				"━━━ TK 1 ━━━\n" +
				"alice|pw",
			wantPairs: []CredPair{{Username: "alice", Password: "pw"}},
		},
		{
			name:    "bad username shape rejected, not silently dropped",
			blob:    "bad user name|pw",
			wantBad: 1,
		},
		{
			name:      "lines with no pipe at all are skipped, not counted bad",
			blob:      "just some text\nalice|pw",
			wantPairs: []CredPair{{Username: "alice", Password: "pw"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pairs, bad := ParseUpstockText(tc.blob)
			assertPairsEqual(t, pairs, tc.wantPairs)
			if bad != tc.wantBad {
				t.Errorf("bad = %d, want %d", bad, tc.wantBad)
			}
		})
	}
}

// TestParseOrderFileText covers the "resell" parser: block format, OTP
// accounts (no password: line), bad usernames, and header noise.
func TestParseOrderFileText(t *testing.T) {
	cases := []struct {
		name      string
		blob      string
		wantPairs []CredPair
		wantBad   int
	}{
		{
			name:      "one block with password",
			blob:      "Order ID: X1\nAccount list:\n---\n[account 1]\nusername: alice\npassword: pw1\n---",
			wantPairs: []CredPair{{Username: "alice", Password: "pw1"}},
		},
		{
			name:      "OTP account keeps blank password",
			blob:      "[account 1]\nusername: bob\n---",
			wantPairs: []CredPair{{Username: "bob", Password: ""}},
		},
		{
			name:      "multiple blocks",
			blob:      "[account 1]\nusername: alice\npassword: pw1\n---\n[account 2]\nusername: bob\npassword: pw2\n---",
			wantPairs: []CredPair{{Username: "alice", Password: "pw1"}, {Username: "bob", Password: "pw2"}},
		},
		{
			name:    "bad username shape counted, not dropped silently",
			blob:    "username: bad user\npassword: pw",
			wantBad: 1,
		},
		{
			name:      "case-insensitive label match",
			blob:      "USERNAME: alice\nPASSWORD: pw1",
			wantPairs: []CredPair{{Username: "alice", Password: "pw1"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pairs, bad := ParseOrderFileText(tc.blob)
			assertPairsEqual(t, pairs, tc.wantPairs)
			if bad != tc.wantBad {
				t.Errorf("bad = %d, want %d", bad, tc.wantBad)
			}
		})
	}
}

// TestParseKiroAPIText is the exact-grammar suite the task calls out
// specifically: valid ksk_ keys, the approved 'ksk_xxx|region' vendor
// suffix (stripped, decoration), the mangled-two-keys-joined case (whole
// line rejected), order-file meta lines (skipped, NOT bad), and malformed
// non-key lines (rejected).
func TestParseKiroAPIText(t *testing.T) {
	cases := []struct {
		name     string
		blob     string
		wantKeys []string
		wantBad  int
	}{
		{
			name:     "plain key list",
			blob:     "ksk_11111\nksk_22222\nksk_AbCdEf123",
			wantKeys: []string{"ksk_11111", "ksk_22222", "ksk_AbCdEf123"},
		},
		{
			name:     "header ignored",
			blob:     "API Keys:\nksk_11111",
			wantKeys: []string{"ksk_11111"},
		},
		{
			name:     "vendor region suffix stripped as decoration",
			blob:     "ksk_11111|eu-central-1",
			wantKeys: []string{"ksk_11111"},
		},
		{
			name:    "mangled two-keys-joined line rejected whole, not half-imported",
			blob:    "ksk_aaa|ksk_bbb",
			wantBad: 1,
		},
		{
			name:     "delivered order file meta lines skipped, NOT counted bad",
			blob:     "Order ID: X1\nDate: 2024-01-01\nWarranty: 7 days\nLogin portal: https://x\nRegion: us-east-1\nksk_11111\n---",
			wantKeys: []string{"ksk_11111"},
		},
		{
			name:    "malformed non-key line rejected",
			blob:    "not_a_key_at_all",
			wantBad: 1,
		},
		{
			name:     "blank lines skipped silently",
			blob:     "\n\nksk_11111\n\n",
			wantKeys: []string{"ksk_11111"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, bad := ParseKiroAPIText(tc.blob)
			if len(keys) != len(tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", keys, tc.wantKeys)
			}
			for i := range keys {
				if keys[i] != tc.wantKeys[i] {
					t.Errorf("keys[%d] = %q, want %q", i, keys[i], tc.wantKeys[i])
				}
			}
			if bad != tc.wantBad {
				t.Errorf("bad = %d, want %d", bad, tc.wantBad)
			}
		})
	}
}

// TestParsersRejectBadLinesWithoutErrorLeak guards the "never log/forward a
// credential" rule at the API-surface level: every parser in this file
// reports a rejected line via a plain int counter, never via a returned
// `error` value that could stringify (and thus leak) the offending
// credential-bearing line. None of these functions returns an error at all
// (see their signatures) — this test exercises each with a line embedding an
// obvious secret and confirms rejection is always visible only as a count.
func TestParsersRejectBadLinesWithoutErrorLeak(t *testing.T) {
	const secret = "Tr0ub4dor&3-hunter2-swordfish"

	if pairs, bad := ParseUpstockText("bad user name|" + secret); bad != 1 || len(pairs) != 0 {
		t.Fatalf("ParseUpstockText: pairs=%v bad=%d, want 0 pairs / 1 bad", pairs, bad)
	}
	if pairs, bad := ParseOrderFileText("username: bad user\npassword: " + secret); bad != 1 || len(pairs) != 0 {
		t.Fatalf("ParseOrderFileText: pairs=%v bad=%d, want 0 pairs / 1 bad", pairs, bad)
	}
	if keys, bad := ParseKiroAPIText("not_a_key_" + secret); bad != 1 || len(keys) != 0 {
		t.Fatalf("ParseKiroAPIText: keys=%v bad=%d, want 0 keys / 1 bad", keys, bad)
	}
	if objs, bad := ParseJSONAccounts(`{"email": "` + secret + `" not valid json`); bad == 0 || len(objs) != 0 {
		t.Fatalf("ParseJSONAccounts: objs=%v bad=%d, want 0 objs / >=1 bad", objs, bad)
	}
}

// TestDedupNewPairs covers within-batch dedup and dedup against already-known
// stock, matching _dedup_new_pairs exactly (dup counts both cases the same
// way, no distinction surfaced).
func TestDedupNewPairs(t *testing.T) {
	known := map[string]bool{"alice": true}
	pairs := []CredPair{
		{Username: "alice", Password: "x"}, // already known -> dup
		{Username: "bob", Password: "y"},   // new
		{Username: "bob", Password: "z"},   // repeated within batch -> dup
		{Username: "", Password: "w"},      // empty username -> dup (never filed)
		{Username: "carol", Password: "q"}, // new
	}
	newPairs, dup := dedupNewPairs(known, pairs)
	if dup != 3 {
		t.Fatalf("dup = %d, want 3", dup)
	}
	assertPairsEqual(t, newPairs, []CredPair{{Username: "bob", Password: "y"}, {Username: "carol", Password: "q"}})
}

// TestDedupNewObjs covers the JSON-account dedup path: no-email rejection,
// within-batch dedup by stem, and dedup against known stock.
func TestDedupNewObjs(t *testing.T) {
	known := map[string]bool{"alice@example.com": true}
	objs := []JSONAccount{
		{Obj: map[string]any{"email": "alice@example.com"}}, // known -> dup
		{Obj: map[string]any{"email": "bob@example.com"}},   // new
		{Obj: map[string]any{"email": "bob@example.com"}},   // repeated -> dup
		{Obj: map[string]any{}},                             // no email -> noEmail
	}
	newObjs, dup, noEmail := dedupNewObjs(known, objs)
	if dup != 2 {
		t.Fatalf("dup = %d, want 2", dup)
	}
	if noEmail != 1 {
		t.Fatalf("noEmail = %d, want 1", noEmail)
	}
	if len(newObjs) != 1 || AccountIdentity(newObjs[0].Obj) != "bob@example.com" {
		t.Fatalf("newObjs = %v, want just bob@example.com", newObjs)
	}
}

// TestUpstockBatchAccumulatesAcrossMultiplePastes proves the accumulator's
// whole point: several Add() calls (simulating separate pasted messages or
// uploaded files) all land in the same running batch, and Preview re-dedups
// the FULL accumulated set (not just the newest delta) against a real
// Store's known stock every time.
func TestUpstockBatchAccumulatesAcrossMultiplePastes(t *testing.T) {
	s := newTestStore(t, "p1", "alice") // "alice" already in stock

	b := NewUpstockBatch("p1", UpstockPassword)

	// First paste: one dup (alice, already known), one new (bob), one bad line.
	parsed, bad := b.Add("alice|pw\nbob|pw\nbad user|pw")
	if parsed != 2 || bad != 1 {
		t.Fatalf("first Add: parsed=%d bad=%d, want 2,1", parsed, bad)
	}
	p := b.Preview(s)
	if p.New != 1 || p.Dup != 1 || p.Bad != 1 {
		t.Fatalf("preview after 1st paste: New=%d Dup=%d Bad=%d, want 1,1,1", p.New, p.Dup, p.Bad)
	}

	// Second paste: "bob" again (now a within-batch dup relative to the
	// FIRST paste's bob, since neither has been committed yet), plus a
	// genuinely new "carol".
	parsed, bad = b.Add("bob|pw2\ncarol|pw3")
	if parsed != 2 || bad != 0 {
		t.Fatalf("second Add: parsed=%d bad=%d, want 2,0", parsed, bad)
	}
	p = b.Preview(s)
	if p.New != 2 { // bob (first occurrence wins) + carol
		t.Fatalf("preview after 2nd paste: New=%d, want 2 (bob+carol)", p.New)
	}
	if p.Dup != 2 { // alice (known) + bob's second occurrence
		t.Fatalf("preview after 2nd paste: Dup=%d, want 2", p.Dup)
	}
	if p.Bad != 1 { // running total across both Add calls
		t.Fatalf("preview after 2nd paste: Bad=%d, want 1 (running total)", p.Bad)
	}
	// bob's FIRST parsed password should win (dedup keeps first occurrence).
	for _, cp := range p.NewPairs {
		if cp.Username == "bob" && cp.Password != "pw" {
			t.Fatalf("bob password = %q, want first occurrence \"pw\"", cp.Password)
		}
	}

	text := p.RenderText()
	if !strings.Contains(text, "📋 Upstock preview — p1") {
		t.Errorf("RenderText missing title: %s", text)
	}
	if !strings.Contains(text, "🆕 New to add: 2") {
		t.Errorf("RenderText missing New count: %s", text)
	}
	if !strings.Contains(text, "↩️ Skip (already in stock/sold): 2") {
		t.Errorf("RenderText missing Dup line: %s", text)
	}
	if !strings.Contains(text, "⚠️ Rejected (bad format): 1") {
		t.Errorf("RenderText missing Bad line: %s", text)
	}
	if strings.Contains(text, "Upload more files") {
		t.Errorf("RenderText should not show the empty-batch trailer when New>0: %s", text)
	}
}

// TestUpstockBatchTokenKind exercises the "token" (JSON) path end to end:
// parsing, dedup-by-email-stem against known stock, and preview labels.
func TestUpstockBatchTokenKind(t *testing.T) {
	s := newTestStore(t, "p1") // no seed accounts

	b := NewUpstockBatch("p1", UpstockToken)
	parsed, bad := b.Add(`{"email":"a@example.com","token":"x"} {"email":"b@example.com","token":"y"}`)
	if parsed != 2 || bad != 0 {
		t.Fatalf("Add: parsed=%d bad=%d, want 2,0", parsed, bad)
	}
	p := b.Preview(s)
	if p.New != 2 || p.Dup != 0 || p.NoEmail != 0 {
		t.Fatalf("preview: New=%d Dup=%d NoEmail=%d, want 2,0,0", p.New, p.Dup, p.NoEmail)
	}
	text := p.RenderText()
	if !strings.Contains(text, "📋 JSON upstock preview — p1") {
		t.Errorf("RenderText missing JSON title: %s", text)
	}
	if !strings.Contains(text, "a@example.com") || !strings.Contains(text, "b@example.com") {
		t.Errorf("RenderText missing account labels: %s", text)
	}
}

// TestUpstockBatchKiroAPIKind proves kiroapi keys flow through the pairs
// dedup path (wrapped as (key, "") pairs) and render with a bare-key label
// (no trailing "|") and the kiroapi-specific title.
func TestUpstockBatchKiroAPIKind(t *testing.T) {
	s := newTestStore(t, "p1")
	b := NewUpstockBatch("p1", UpstockKiroAPI)
	parsed, bad := b.Add("ksk_11111\nksk_22222|eu-central-1")
	if parsed != 2 || bad != 0 {
		t.Fatalf("Add: parsed=%d bad=%d, want 2,0", parsed, bad)
	}
	p := b.Preview(s)
	if p.New != 2 {
		t.Fatalf("New = %d, want 2", p.New)
	}
	text := p.RenderText()
	if !strings.Contains(text, "📋 Kiro API Key upstock preview — p1") {
		t.Errorf("RenderText missing kiroapi title: %s", text)
	}
	if !strings.Contains(text, "🆕 ksk_11111") || strings.Contains(text, "ksk_11111|") {
		t.Errorf("RenderText should show a bare key label, not key|password: %s", text)
	}
}

// TestUpstockPreviewEmptyBatchTrailer proves the "(Upload more files or
// paste accounts.)" trailer only appears when there is nothing new to
// confirm yet — Python's payload else-branch (telegram_bot.py:8663-8664).
func TestUpstockPreviewEmptyBatchTrailer(t *testing.T) {
	s := newTestStore(t, "p1", "alice")
	b := NewUpstockBatch("p1", UpstockPassword)
	b.Add("alice|pw") // the only line is a dup -> New == 0
	p := b.Preview(s)
	if p.New != 0 {
		t.Fatalf("New = %d, want 0", p.New)
	}
	text := p.RenderText()
	if !strings.Contains(text, "(Upload more files or paste accounts.)") {
		t.Errorf("RenderText missing empty-batch trailer: %s", text)
	}
}

// assertPairsEqual is the shared CredPair-slice comparison helper for the
// parser table tests above.
func assertPairsEqual(t *testing.T, got, want []CredPair) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("pairs[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
