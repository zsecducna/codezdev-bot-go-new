package provision

import (
	"strings"
	"testing"
)

// Fixtures mirror tests/test_provision.py so the pins are comparable
// line-for-line with the Python acceptance contract.
const (
	// TEXT (test_provision.py:47) — a header, a Format line, one idc credential.
	idcText  = "Accounts: 1\nFormat: portal|region|user|pass|label\nuser.one|pw123|https://x.portal.aws/"
	m365Text = "m365.user@corp.example|pw456"
)

// TestClassifyAccountLine is the table form of test_classify_and_mixed_parse,
// test_idc_four_field_line_classifies_and_carries_seed (test_provision.py:227,
// 402) plus the rawkey pins.
func TestClassifyAccountLine(t *testing.T) {
	longKey := "ksk_" + strings.Repeat("z", 32)
	cases := []struct {
		name string
		line string
		want *Account
		why  string
	}{
		{
			name: "idc three fields",
			line: "user.one|pw123|https://x.portal.aws/",
			want: &Account{Flow: FlowIDC, Username: "user.one", Password: "pw123", StartURL: "https://x.portal.aws/"},
		},
		{
			name: "email-looking idc username stays idc",
			line: "bob@corp.com|pw|https://d-1.awsapps.com/start",
			want: &Account{Flow: FlowIDC, Username: "bob@corp.com", Password: "pw", StartURL: "https://d-1.awsapps.com/start"},
			why:  "the URL test decides FIRST; misrouting would sign in to Microsoft with an IdC password",
		},
		{
			name: "idc four fields carries the seed",
			line: "user.one|pw123|https://x.portal.aws/|SEED32SEED32SEED32SEED32SEED32AB",
			want: &Account{Flow: FlowIDC, Username: "user.one", Password: "pw123",
				StartURL: "https://x.portal.aws/", MFASecret: "SEED32SEED32SEED32SEED32SEED32AB"},
			why: "adding a 4th field must not tip an idc line into the m365 shape",
		},
		{
			name: "email-shaped idc username with a seed stays idc",
			line: "a@b.com|pw|https://x.portal.aws/|SEED32SEED32SEED32SEED32SEED32AB",
			want: &Account{Flow: FlowIDC, Username: "a@b.com", Password: "pw",
				StartURL: "https://x.portal.aws/", MFASecret: "SEED32SEED32SEED32SEED32SEED32AB"},
		},
		{
			name: "http scheme also counts as idc",
			line: "u|p|http://portal.local/start",
			want: &Account{Flow: FlowIDC, Username: "u", Password: "p", StartURL: "http://portal.local/start"},
		},
		{
			name: "m365 two fields",
			line: "m365.user@corp.example|pw",
			want: &Account{Flow: FlowM365, Username: "m365.user@corp.example", Password: "pw"},
		},
		{
			name: "m365 with seed",
			line: "m365.user@corp.example|pw|JBSWY3DPEH",
			want: &Account{Flow: FlowM365, Username: "m365.user@corp.example", Password: "pw", MFASecret: "JBSWY3DPEH"},
		},
		{
			name: "rawkey",
			line: longKey,
			want: &Account{Flow: FlowRawKey, Username: longKey},
		},
		{
			name: "rawkey tolerates surrounding whitespace",
			line: "  " + longKey + "  ",
			want: &Account{Flow: FlowRawKey, Username: longKey},
		},
		// ── refusals ────────────────────────────────────────────────────────
		{name: "blank", line: "", want: nil},
		{name: "whitespace only", line: "   \t ", want: nil},
		{name: "Accounts header", line: "Accounts: 3", want: nil},
		{name: "Format header", line: "Format: portal|region|user", want: nil},
		{name: "no pipes", line: "no-pipes-here", want: nil},
		{name: "not an email", line: "not an email|pw", want: nil},
		{
			name: "scheme-less start_url refused, never mis-routed to m365",
			line: "u@corp.com|pw|d-1234.awsapps.com/start",
			want: nil,
			why:  "a dropped https:// is a malformed idc line, not a TOTP seed (base32 has no '/')",
		},
		{
			name: "truncated ksk_ token is not a rawkey",
			line: "ksk_short",
			want: nil,
			why:  "the {13,} floor guarantees mask() always truncates an accepted key",
		},
		{
			name: "ksk_ token just under the length floor",
			line: "ksk_" + strings.Repeat("z", 12),
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClassifyAccountLine(tc.line)
			if tc.want == nil {
				if ok {
					t.Fatalf("classified %q as %+v, want refusal (%s)", tc.line, got, tc.why)
				}
				return
			}
			if !ok {
				t.Fatalf("refused %q, want %+v (%s)", tc.line, *tc.want, tc.why)
			}
			if got != *tc.want {
				t.Errorf("got %+v, want %+v (%s)", got, *tc.want, tc.why)
			}
		})
	}
}

func TestClassifyAccountLineKeepsPasswordRaw(t *testing.T) {
	// kiro-web-login.py reads field 2 raw; a delivered password may legitimately
	// contain spaces and must reach the login byte-for-byte.
	got, ok := ClassifyAccountLine("u| pw with spaces |https://x/")
	if !ok {
		t.Fatal("line refused")
	}
	if got.Password != " pw with spaces " {
		t.Errorf("password = %q, want it untrimmed", got.Password)
	}
}

// TestParseAllAccountsMixed ports test_classify_and_mixed_parse's mixed-file
// half (test_provision.py:249).
func TestParseAllAccountsMixed(t *testing.T) {
	mixed := "Format: a|b|c\n" +
		"u1|p1|https://a.aws/\n" +
		"a@corp.example|p2\n" +
		"u3|p3|https://c.aws/\n" +
		"b@corp.example|p4|SEED\n"
	accts := ParseAllAccounts(mixed)
	if got := flowsOf(accts); !equalFlows(got, []Flow{FlowIDC, FlowM365, FlowIDC, FlowM365}) {
		t.Errorf("flows = %v, want idc/m365/idc/m365 in file order", got)
	}
	// ParseIDCAccounts stays idc-only for its original callers.
	idc := ParseIDCAccounts(mixed)
	if len(idc) != 2 || idc[0].Username != "u1" || idc[1].Username != "u3" {
		t.Errorf("ParseIDCAccounts = %v, want u1/u3", usernamesOf(idc))
	}
}

func TestParseAllAccountsEmpty(t *testing.T) {
	if got := ParseAllAccounts(""); len(got) != 0 {
		t.Errorf("empty text yielded %d accounts", len(got))
	}
	if got := ParseAllAccounts("Accounts: 0\n\n# nothing here\n"); len(got) != 0 {
		t.Errorf("header-only text yielded %d accounts", len(got))
	}
}

// TestOneAccountText ports test_parse + the _one_account_text pins at
// test_provision.py:100, 260.
func TestOneAccountText(t *testing.T) {
	multi := "Format: a|b|c\nu1|p1|https://a.aws/\nu2|p2|https://b.aws/\nu3|p3|https://c.aws/"
	one := OneAccountText(multi, 2)
	got := ParseIDCAccounts(one)
	if len(got) != 1 || got[0].Username != "u2" {
		t.Errorf("extract(2) = %v, want exactly u2", usernamesOf(got))
	}

	mixed := "Format: a|b|c\n" +
		"u1|p1|https://a.aws/\n" +
		"a@corp.example|p2\n" +
		"u3|p3|https://c.aws/\n" +
		"b@corp.example|p4|SEED\n"
	// The index spans BOTH flows, matching the order the batch driver reports.
	if a := ParseAllAccounts(OneAccountText(mixed, 4)); len(a) != 1 || a[0].MFASecret != "SEED" {
		t.Errorf("extract(4) = %+v, want the m365 line with its seed", a)
	}
	if a := ParseAllAccounts(OneAccountText(mixed, 3)); len(a) != 1 || a[0].Username != "u3" {
		t.Errorf("extract(3) = %+v, want u3 (the 3rd line spans flows)", a)
	}

	// Out of range returns the text unchanged, exactly as Python does.
	if OneAccountText(multi, 99) != multi || OneAccountText(multi, 0) != multi {
		t.Error("out-of-range index must return the input unchanged")
	}

	// DELIBERATE DEVIATION (documented on OneAccountText): a rawkey round-trips
	// as the bare key. Python emitted "ksk_…|", which re-classifies as nothing.
	key := "ksk_" + strings.Repeat("q", 24)
	if a := ParseAllAccounts(OneAccountText(key, 1)); len(a) != 1 || a[0].Flow != FlowRawKey {
		t.Errorf("rawkey extract = %+v, want a single rawkey account", a)
	}
}

// ── delivered order-file parsing ─────────────────────────────────────────────

// orderText mirrors ORDER_TEXT (test_provision.py:758): a realistic delivered
// order file with two accounts under one Login portal.
const orderText = "Order ID: fb7367ad58a843f2b5952e66a019b156\n" +
	"Date: 2026-07-15 18:03:31 CEST\n" +
	"Warranty: No warranty\n" +
	"Login portal: https://d-90667733ae.awsapps.com/start\n" +
	"Region: us-east-1\n" +
	"Account list:\n" +
	"---\n" +
	"[account 1]\n" +
	"username: camilo.radelic@duke-kr.win\n" +
	"password: H9-f2h^kEP_a#IQ1\n" +
	"---\n" +
	"[account 2]\n" +
	"username: dana.two@duke-kr.win\n" +
	"password: pw:with spaces#2\n" +
	"---\n"

func TestOrderFileDetectedAndExpandedToIDC(t *testing.T) {
	accts := ParseAllAccounts(orderText)
	if len(accts) != 2 {
		t.Fatalf("parsed %d accounts, want 2", len(accts))
	}
	for _, a := range accts {
		if a.Flow != FlowIDC {
			t.Errorf("%s: flow = %s, want idc", a.Username, a.Flow)
		}
		// The shared Login portal is stamped onto EVERY account.
		if a.StartURL != "https://d-90667733ae.awsapps.com/start" {
			t.Errorf("%s: start_url = %q", a.Username, a.StartURL)
		}
	}
	if accts[0].Username != "camilo.radelic@duke-kr.win" || accts[0].Password != "H9-f2h^kEP_a#IQ1" {
		t.Errorf("first account = %+v", accts[0])
	}
	// A password with a ":" and spaces must round-trip byte-for-byte — the
	// block parser must not split or strip past the label.
	if accts[1].Password != "pw:with spaces#2" {
		t.Errorf("password = %q, want %q", accts[1].Password, "pw:with spaces#2")
	}
}

func TestOrderFileSkipsUnprovisionableBlocks(t *testing.T) {
	// OTP-login stock has no `password:` (nothing to log in with); a
	// `refresh_token:` block is a token account; a `file:` block's credential
	// lives in a separate .json. None can be provisioned from the .txt.
	txt := "Login portal: https://d-1.awsapps.com/start\n" +
		"Account list:\n" +
		"---\n[account 1]\nusername: has.pw@x.win\npassword: secret1\n" +
		"---\n[account 2]\nusername: otp.only@x.win\n" +
		"---\n[account 3]\nusername: tok@x.win\nrefresh_token: rt-abc\n" +
		"---\n[account 4]\nusername: json@x.win\nfile: json.json\n" +
		"---\n"
	got := usernamesOf(ParseAllAccounts(txt))
	if len(got) != 1 || got[0] != "has.pw@x.win" {
		t.Errorf("got %v, want only the password-bearing portal login", got)
	}
}

func TestOrderFileWithoutPortalYieldsNothing(t *testing.T) {
	// With no valid portal there is no start_url to sign in through, so every
	// block is skipped rather than provisioned against a garbage URL.
	txt := "Order ID: x\nAccount list:\n---\n[account 1]\nusername: a@x.win\npassword: p1\n---\n"
	if got := ParseAllAccounts(txt); len(got) != 0 {
		t.Errorf("got %v, want no provisionable accounts", usernamesOf(got))
	}
	// A portal missing its scheme is equally unusable.
	txt2 := "Login portal: d-1.awsapps.com/start\n---\n[account 1]\nusername: a@x.win\npassword: p1\n---\n"
	if got := ParseAllAccounts(txt2); len(got) != 0 {
		t.Errorf("scheme-less portal accepted: %v", usernamesOf(got))
	}
}

func TestOrderFileCarriesOptionalMFASeed(t *testing.T) {
	// Delivered files don't emit an MFA line, but an operator may add one for
	// an already-enrolled account, whose per-sign-in challenge is answered from
	// field 4.
	for _, label := range []string{"mfa_secret", "totp", "mfa"} {
		txt := "Login portal: https://d-1.awsapps.com/start\n" +
			"---\n[account 1]\nusername: a@x.win\npassword: p1\n" +
			label + ": JBSWY3DPEHPK3PXP\n---\n"
		accts := ParseAllAccounts(txt)
		if len(accts) != 1 || accts[0].MFASecret != "JBSWY3DPEHPK3PXP" {
			t.Errorf("%s: got %+v, want the seed carried to field 4", label, accts)
		}
	}
}

func TestOrderFileTwoOrdersEachUseOwnPortal(t *testing.T) {
	// An operator pasting two deliveries at once: the portal is tracked as the
	// walk proceeds, so each order's accounts get THEIR order's portal.
	txt := "Login portal: https://d-AAA.awsapps.com/start\n" +
		"---\n[account 1]\nusername: a@x.win\npassword: p1\n---\n" +
		"Login portal: https://d-BBB.awsapps.com/start\n" +
		"---\n[account 1]\nusername: b@x.win\npassword: p2\n---\n"
	accts := ParseAllAccounts(txt)
	if len(accts) != 2 {
		t.Fatalf("parsed %d accounts, want 2", len(accts))
	}
	if accts[0].StartURL != "https://d-AAA.awsapps.com/start" ||
		accts[1].StartURL != "https://d-BBB.awsapps.com/start" {
		t.Errorf("portals = %q / %q, want each order to stamp its own",
			accts[0].StartURL, accts[1].StartURL)
	}
}

func TestOrderFileCRLFTolerated(t *testing.T) {
	// Telegram/Windows uploads carry CRLF; a "password:\r" must not smuggle a
	// trailing CR into the credential.
	accts := ParseAllAccounts(strings.ReplaceAll(orderText, "\n", "\r\n"))
	if len(accts) != 2 {
		t.Fatalf("CRLF order file parsed %d accounts, want 2", len(accts))
	}
	if accts[0].Password != "H9-f2h^kEP_a#IQ1" {
		t.Errorf("password = %q, CR leaked in", accts[0].Password)
	}
}

func TestOrderFilePipeInCredentialSkipped(t *testing.T) {
	// A "|" can't survive the idc round-trip: the temp idc file is rebuilt
	// pipe-delimited and kiro-web-login.py splits it back on "|", so a "|"
	// password would field-shift. Drop the block rather than emit a record that
	// silently corrupts.
	txt := "Login portal: https://d-1.awsapps.com/start\n" +
		"---\n[account 1]\nusername: clean@x.win\npassword: goodpw\n" +
		"---\n[account 2]\nusername: piped@x.win\npassword: pw|with|pipes\n" +
		"---\n[account 3]\nusername: pi|pe@x.win\npassword: alsobad\n" +
		"---\n"
	got := usernamesOf(ParseAllAccounts(txt))
	if len(got) != 1 || got[0] != "clean@x.win" {
		t.Errorf("got %v, want only the pipe-free account", got)
	}
}

func TestOneLinePastesNeverMisreadAsOrderFile(t *testing.T) {
	// A one-credential-per-line paste carries no colon-label block body, so it
	// must ALWAYS take the classify path.
	paste := "u1|p1|https://a.aws/\na@corp.example|p2\nksk_" + strings.Repeat("z", 20)
	if looksLikeOrderFile(paste) {
		t.Error("a pipe/ksk paste was detected as an order file")
	}
	if got := flowsOf(ParseAllAccounts(paste)); !equalFlows(got, []Flow{FlowIDC, FlowM365, FlowRawKey}) {
		t.Errorf("flows = %v, want idc/m365/rawkey", got)
	}
	if looksLikeOrderFile("u1|p1|https://a.aws/") {
		t.Error("a single pipe line was detected as an order file")
	}
}

func TestStrayLabelLineDoesNotRerouteOneLinePaste(t *testing.T) {
	// Detection demands BOTH a body label AND a block opener. A paste carrying a
	// stray `file:` line has no `---`/`[account]` opener, so it must stay on the
	// classify path — rerouting it would drop every pipe/ksk line to zero, a
	// silent total loss.
	paste := "u1|p1|https://a.aws/\nfile: notes.txt\nksk_" + strings.Repeat("z", 20)
	if looksLikeOrderFile(paste) {
		t.Error("a paste with a stray label line was treated as an order file")
	}
	if got := flowsOf(ParseAllAccounts(paste)); !equalFlows(got, []Flow{FlowIDC, FlowRawKey}) {
		t.Errorf("flows = %v, want idc/rawkey", got)
	}
	// And the converse: a real order file (body label AND block opener) is
	// detected.
	if !looksLikeOrderFile(orderText) {
		t.Error("a real order file was not detected")
	}
	// A block opener with no body label is likewise not an order file.
	if looksLikeOrderFile("---\nu1|p1|https://a.aws/\n---\n") {
		t.Error("a block opener alone was treated as an order file")
	}
}

// ── filename sanitizers & masking ────────────────────────────────────────────

func TestSafeUser(t *testing.T) {
	// Matches kiro-web-login.save_api_key: every unsafe rune becomes ONE "_".
	cases := map[string]string{
		"user.one":      "user.one",
		"a.b@c-d.win":   "a.b_c-d.win",
		"with space":    "with_space",
		"a/b\\c":        "a_b_c",
		"":              "unknown",
		"@@@":           "___",
		"keep_-.chars":  "keep_-.chars",
		"tab\tand|pipe": "tab_and_pipe",
		"unicodé.user":  "unicodé.user", // Python's isalnum() is Unicode-aware
	}
	for in, want := range cases {
		if got := safeUser(in); got != want {
			t.Errorf("safeUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeComponent(t *testing.T) {
	// Matches kiro-login-helper.sanitize_file_component: ASCII-alnum only, runs
	// of unsafe chars collapse to a SINGLE dash, leading/trailing dashes are
	// stripped. Differs from safeUser on purpose — getting it wrong means
	// looking for the wrong filename and reporting a success as a failure.
	cases := map[string]string{
		"a.b@c-d.win":            "a.b-c-d.win",
		"m365.user@corp.example": "m365.user-corp.example",
		"a@@@b":                  "a-b",
		"@lead.and.trail@":       "lead.and.trail",
		"":                       "",
		"   ":                    "",
		"  padded@x.io  ":        "padded-x.io",
		"unicodé@x.io":           "unicod-x.io", // non-ASCII is NOT kept here
	}
	for in, want := range cases {
		if got := safeComponent(in); got != want {
			t.Errorf("safeComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMask(t *testing.T) {
	// The >16 threshold pairs with rawKeyRe's {13,} floor: every ACCEPTED
	// rawkey is long enough to actually be truncated, so a key can never be
	// echoed in full into a progress line.
	long := "ksk_" + strings.Repeat("z", 32)
	got := mask(long)
	if !strings.HasPrefix(got, "ksk_") || !strings.Contains(got, "…") || strings.Contains(got, strings.Repeat("z", 32)) {
		t.Errorf("mask(long) = %q, want a truncated prefix…tail form", got)
	}
	if len([]rune(got)) != 13 {
		t.Errorf("mask(long) = %q (%d runes), want 8+1+4", got, len([]rune(got)))
	}
	// A shortest accepted rawkey (4 + 13 = 17 chars) must still truncate.
	shortest := "ksk_" + strings.Repeat("z", 13)
	if mask(shortest) == shortest {
		t.Errorf("shortest accepted rawkey (%d chars) was returned verbatim by mask", len(shortest))
	}
	// Below the threshold the value is returned as-is (Python parity), which is
	// exactly why rawKeyRe refuses such tokens in the first place.
	if mask("short") != "short" {
		t.Error("mask must return a <=16-char value unchanged")
	}
	if mask("") != "" {
		t.Error("mask(\"\") must be \"\"")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func flowsOf(accts []Account) []Flow {
	out := make([]Flow, 0, len(accts))
	for _, a := range accts {
		out = append(out, a.Flow)
	}
	return out
}

func usernamesOf(accts []Account) []string {
	out := make([]string, 0, len(accts))
	for _, a := range accts {
		out = append(out, a.Username)
	}
	return out
}

func equalFlows(a, b []Flow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
