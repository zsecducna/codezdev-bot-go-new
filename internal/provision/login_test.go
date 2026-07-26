package provision

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── argv contracts ───────────────────────────────────────────────────────────

// indexOf returns the position of `flag` in args, or -1.
func indexOf(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}

// valueAfter returns the argument following `flag`, or "".
func valueAfter(args []string, flag string) string {
	if i := indexOf(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestIDCLoginArgs(t *testing.T) {
	r := NewScriptRunner("/venv/bin/python", "/opt/bot")
	args := r.idcLoginArgs("/tmp/accts.txt", 3, "/tmp/keys", true, 0)

	if args[0] != "/opt/bot/"+webLoginScriptName {
		t.Errorf("script = %q, want kiro-web-login.py", args[0])
	}
	if valueAfter(args, "--index") != "3" {
		t.Errorf("--index = %q, want 3", valueAfter(args, "--index"))
	}
	if valueAfter(args, "--api-key-name") != "Kiro-Go" {
		t.Errorf("--api-key-name = %q", valueAfter(args, "--api-key-name"))
	}
	if valueAfter(args, "--api-key-out-dir") != "/tmp/keys" {
		t.Errorf("--api-key-out-dir = %q", valueAfter(args, "--api-key-out-dir"))
	}
	if valueAfter(args, "--timeout") != strconv.Itoa(int(LoginTimeout.Seconds())) {
		t.Errorf("--timeout = %q, want the script's own budget", valueAfter(args, "--timeout"))
	}
	// The accounts file is POSITIONAL and must sit after every flag that takes
	// a value, immediately before the optional --headless.
	if indexOf(args, "/tmp/accts.txt") != len(args)-2 {
		t.Errorf("args = %v, want the positional accounts file just before --headless", args)
	}
	// kiro-web-login.py defaults to HEADED; headless is the PRESENCE of the
	// flag, so headed mode is expressed by omitting it.
	if indexOf(args, "--headless") < 0 {
		t.Error("--headless missing for a headless run")
	}
	if headed := r.idcLoginArgs("/tmp/accts.txt", 1, "/tmp/keys", false, 0); indexOf(headed, "--headless") >= 0 {
		t.Errorf("headed run still passed --headless: %v", headed)
	}
}

func TestM365LoginArgsPinsEmail(t *testing.T) {
	// The bridge MUST pass --email (kiro_provision.py:501-508). Without it
	// kiro-go-login.py names the output file from the access token's JWT
	// claims, falling back through preferred_username/upn/unique_name to
	// `name` (a display name), or to the account UUID. Any of those makes the
	// file unmatchable to the account we signed in as — so a successful ~2min
	// login is reported as login_failed and its live credential is wiped with
	// the temp dir; and if that login just enrolled a TOTP seed, the account is
	// left MFA-changed for nothing.
	r := NewScriptRunner("/venv/bin/python", "/opt/bot")
	args := r.m365LoginArgs("/tmp/accts.txt", 3, "/tmp/out", "bob@corp.example", true, 0)

	if args[0] != "/opt/bot/"+goLoginScriptName {
		t.Errorf("script = %q, want kiro-go-login.py", args[0])
	}
	if valueAfter(args, "--email") != "bob@corp.example" {
		t.Errorf("--email = %q, want it pinned to the signed-in address", valueAfter(args, "--email"))
	}
	if valueAfter(args, "--index") != "3" {
		t.Errorf("--index = %q, want 3", valueAfter(args, "--index"))
	}
	if valueAfter(args, "--m365-accounts-file") != "/tmp/accts.txt" {
		t.Errorf("--m365-accounts-file = %q", valueAfter(args, "--m365-accounts-file"))
	}
	if valueAfter(args, "--out-dir") != "/tmp/out" {
		t.Errorf("--out-dir = %q", valueAfter(args, "--out-dir"))
	}
	if indexOf(args, "--headless") < 0 {
		t.Error("headless is the production default")
	}
	// --step-delay is passed explicitly as 0 for an unattended run (the script
	// itself defaults to 1.0s of visibility pauses).
	if valueAfter(args, "--step-delay") != "0" {
		t.Errorf("--step-delay = %q, want an explicit 0", valueAfter(args, "--step-delay"))
	}
	if got := r.m365LoginArgs("/tmp/a", 1, "/tmp/o", "e@x.io", true, 1.5); valueAfter(got, "--step-delay") != "1.5" {
		t.Errorf("--step-delay = %q, want 1.5", valueAfter(got, "--step-delay"))
	}
}

// TestScriptRunnerLaunchesTheChild drives the REAL subprocess path with a
// /bin/sh script standing in for the Python interpreter, so the argv actually
// reaches a child and the key-JSON hand-off is exercised end to end.
func TestScriptRunnerLaunchesTheChild(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	// The fake "interpreter" records its argv, then writes the key JSON where
	// --api-key-out-dir says, exactly as kiro-web-login.save_api_key would.
	fakePython := filepath.Join(dir, "fake-python.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--api-key-out-dir\" ]; then out=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"mkdir -p \"$out\"\n" +
		"printf '{\"username\":\"user.one\",\"api_key\":\"" + testKSK + "\"}' > \"$out/kiro-api-key_user.one.json\"\n"
	if err := os.WriteFile(fakePython, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake python: %v", err)
	}

	r := &ScriptRunner{
		Python:         "/bin/sh",
		WebLoginScript: fakePython,
		Timeout:        10 * time.Second,
		Grace:          5 * time.Second,
	}
	keyOut := filepath.Join(dir, "keys")
	res, err := r.RunIDCLogin(context.Background(), filepath.Join(dir, "accts.txt"), 1, keyOut, true, 0)
	if err != nil {
		t.Fatalf("RunIDCLogin: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("child never ran: %v", err)
	}
	if !strings.Contains(string(argv), "--headless") || !strings.Contains(string(argv), "Kiro-Go") {
		t.Errorf("child argv = %q", argv)
	}
	// And the discovery half finds what it wrote.
	path, payload, ok := findKeyJSON(keyOut, "user.one")
	if !ok || payload.APIKey != testKSK {
		t.Errorf("findKeyJSON(%q) = (%q, %+v, %v)", keyOut, path, payload, ok)
	}
}

// ── credential discovery ─────────────────────────────────────────────────────

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestFindKeyJSON(t *testing.T) {
	dir := t.TempDir()

	// Missing dir / no candidates.
	if _, _, ok := findKeyJSON(filepath.Join(dir, "nope"), "user.one"); ok {
		t.Error("found a key in a nonexistent dir")
	}

	// Exact safe-user filename.
	writeJSON(t, filepath.Join(dir, "kiro-api-key_user.one.json"),
		map[string]any{"username": "user.one", "api_key": testKSK})
	path, payload, ok := findKeyJSON(dir, "user.one")
	if !ok || payload.APIKey != testKSK || !strings.HasSuffix(path, "kiro-api-key_user.one.json") {
		t.Errorf("exact hit failed: (%q, %+v, %v)", path, payload, ok)
	}

	// Disambiguation-suffix case: the file is named differently but its payload
	// username matches.
	dir2 := t.TempDir()
	writeJSON(t, filepath.Join(dir2, "kiro-api-key_other.2.json"),
		map[string]any{"username": "user.one", "api_key": "ksk_from-suffixed-file"})
	_, payload, ok = findKeyJSON(dir2, "user.one")
	if !ok || payload.APIKey != "ksk_from-suffixed-file" {
		t.Errorf("suffixed-file scan failed: (%+v, %v)", payload, ok)
	}

	// Older single-account layout: the exact file has no matching username
	// field but does hold a key.
	dir3 := t.TempDir()
	writeJSON(t, filepath.Join(dir3, "kiro-api-key_user.one.json"),
		map[string]any{"api_key": "ksk_legacy-layout"})
	_, payload, ok = findKeyJSON(dir3, "user.one")
	if !ok || payload.APIKey != "ksk_legacy-layout" {
		t.Errorf("legacy-layout fallback failed: (%+v, %v)", payload, ok)
	}

	// A key-less or corrupt candidate is skipped, never fatal.
	dir4 := t.TempDir()
	os.WriteFile(filepath.Join(dir4, "kiro-api-key_corrupt.json"), []byte("{not json"), 0o600)
	writeJSON(t, filepath.Join(dir4, "kiro-api-key_empty.json"), map[string]any{"username": "user.one"})
	if _, _, ok := findKeyJSON(dir4, "user.one"); ok {
		t.Error("a corrupt/key-less candidate was accepted")
	}
}

func TestFindAccountJSONIsIdentityBased(t *testing.T) {
	// out_dir is SHARED across the batch and a failed add deliberately leaves
	// its JSON behind, so returning "some account file" could hand this account
	// ANOTHER account's live credential.
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "kiro-go-account_"+safeComponent("a@corp.example")+".json"),
		map[string]any{"accessToken": "tokA", "email": "a@corp.example", "authMethod": "external_idp", "region": "eu-central-1"})

	path, raw, payload, ok := findAccountJSON(dir, "a@corp.example")
	if !ok || payload.AccessToken != "tokA" || payload.Region != "eu-central-1" {
		t.Fatalf("exact hit failed: (%q, %+v, %v)", path, payload, ok)
	}
	// Raw bytes are the file's bytes, for verbatim forwarding.
	onDisk, _ := os.ReadFile(path)
	if string(raw) != string(onDisk) {
		t.Errorf("raw = %s, want the file bytes verbatim", raw)
	}

	// A different account must NOT inherit the leftover file.
	if _, _, _, ok := findAccountJSON(dir, "b@corp.example"); ok {
		t.Error("another account's credential was attributed to b@corp.example")
	}

	// Case-insensitive: an address's case is not meaningful.
	if _, _, _, ok := findAccountJSON(dir, "A@Corp.Example"); !ok {
		t.Error("case-differing address did not match")
	}

	// A payload with no accessToken is not a usable credential.
	dir2 := t.TempDir()
	writeJSON(t, filepath.Join(dir2, "kiro-go-account_x.json"), map[string]any{"email": "a@corp.example"})
	if _, _, _, ok := findAccountJSON(dir2, "a@corp.example"); ok {
		t.Error("a token-less payload was accepted")
	}
}

func TestFindAccountJSONScansWhenTheFilenameWasRenamed(t *testing.T) {
	// The one case that renames the file: an address whose sanitized form is
	// empty, which the script replaces with a timestamp. The dir scan is the
	// safety net, and it stays identity-matched.
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "kiro-go-account_20260715-180331.json"),
		map[string]any{"accessToken": "tokA", "email": "a@corp.example"})
	_, _, payload, ok := findAccountJSON(dir, "a@corp.example")
	if !ok || payload.AccessToken != "tokA" {
		t.Errorf("dir scan failed: (%+v, %v)", payload, ok)
	}
}

// ── preflight checks ─────────────────────────────────────────────────────────

func TestCallbackPortFree(t *testing.T) {
	// Runs against a port chosen at random rather than the real constant:
	// asserting anything about 3128 on the host running the tests would couple
	// them to whatever happens to be listening there.
	picker, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	port := picker.Addr().(*net.TCPAddr).Port
	picker.Close()

	if !CallbackPortFree(port) {
		t.Fatal("a free port was reported busy")
	}

	// A live listener, bound the way kiro-login-helper's _V4Server does
	// (allow_reuse_address = True, which Go sets by default), must read as busy.
	blocker, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("bind blocker: %v", err)
	}
	if CallbackPortFree(port) {
		blocker.Close()
		t.Fatal("a live listener was not detected as busy")
	}
	blocker.Close()

	// The probe runs before every m365 batch — it must not KEEP the port, or it
	// would block the very login it is clearing the way for.
	if !CallbackPortFree(port) {
		t.Error("the probe kept the port it tested")
	}
	// The bound-to-the-constant wrapper must at least be callable without
	// asserting anything about the host's port 3128.
	_ = M365CallbackPortFree()
}

func TestM365CallbackPortMatchesVendoredHelper(t *testing.T) {
	// Port of test_m365_callback_port_matches_vendored_helper
	// (test_provision.py:735). M365CallbackPort mirrors kiro-login-helper's
	// SOCIAL_REDIRECT_PORT, which is baked into the IdP-registered
	// redirect_uri. The helper is VENDORED, so a future re-vendor could move
	// the port and leave CallbackPortFree guarding the wrong one — silently
	// protecting nothing.
	//
	// The Go repo does not vendor the three login scripts yet, so this skips
	// until they land next to the binary; it then activates automatically.
	candidates := []string{
		filepath.Join("..", "..", loginHelperScriptName),
		filepath.Join("..", "..", "scripts", loginHelperScriptName),
	}
	var src []byte
	for _, c := range candidates {
		if body, err := os.ReadFile(c); err == nil {
			src = body
			break
		}
	}
	if src == nil {
		t.Skipf("%s is not vendored in this repo yet; this pin activates when it is", loginHelperScriptName)
	}
	m := regexp.MustCompile(`(?m)^SOCIAL_REDIRECT_PORT\s*=\s*(\d+)`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer defines SOCIAL_REDIRECT_PORT", loginHelperScriptName)
	}
	got, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("SOCIAL_REDIRECT_PORT is not a number: %v", err)
	}
	if got != M365CallbackPort {
		t.Errorf("helper port = %d, M365CallbackPort = %d — the probe is guarding the wrong port",
			got, M365CallbackPort)
	}
}

func TestBrowserStackErrorReportsMissingScripts(t *testing.T) {
	// All three scripts are required TOGETHER: a browser batch may mix idc and
	// m365 lines, and kiro-go-login.py imports kiro-login-helper.py as a
	// sibling, so a missing one would only surface mid-batch.
	dir := t.TempDir()
	r := NewScriptRunner("/bin/sh", dir)
	msg := r.BrowserStackError(context.Background())
	if !strings.Contains(msg, "login script missing") {
		t.Fatalf("msg = %q, want a missing-script report", msg)
	}

	// Present two of three: still refused, and it names the missing one.
	os.WriteFile(r.WebLoginScript, []byte("#\n"), 0o600)
	os.WriteFile(r.GoLoginScript, []byte("#\n"), 0o600)
	msg = r.BrowserStackError(context.Background())
	if !strings.Contains(msg, loginHelperScriptName) {
		t.Errorf("msg = %q, want it to name the missing helper", msg)
	}
}

func TestBrowserStackErrorProbesTheInterpreter(t *testing.T) {
	dir := t.TempDir()
	r := NewScriptRunner("", dir)
	for _, p := range []string{r.WebLoginScript, r.GoLoginScript, r.LoginHelperScript} {
		if err := os.WriteFile(p, []byte("#\n"), 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
	}

	// A "python" whose import probe fails: the last stderr line is surfaced.
	failing := filepath.Join(dir, "no-stack.sh")
	os.WriteFile(failing, []byte("#!/bin/sh\necho \"ModuleNotFoundError: No module named 'cloakbrowser'\" >&2\nexit 1\n"), 0o700)
	r.Python = failing
	msg := r.BrowserStackError(context.Background())
	if !strings.Contains(msg, "browser stack not installed") || !strings.Contains(msg, "cloakbrowser") {
		t.Errorf("msg = %q, want the failing import surfaced", msg)
	}

	// A "python" whose probe succeeds clears the check.
	ok := filepath.Join(dir, "has-stack.sh")
	os.WriteFile(ok, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	r.Python = ok
	if msg := r.BrowserStackError(context.Background()); msg != "" {
		t.Errorf("msg = %q, want the check to pass", msg)
	}

	// An interpreter that cannot be launched at all is reported, not panicked.
	r.Python = "/definitely/not/a/real/interpreter"
	if msg := r.BrowserStackError(context.Background()); !strings.Contains(msg, "couldn't probe the browser stack") {
		t.Errorf("msg = %q, want a probe-failure report", msg)
	}
}

func TestLoginPythonFromEnv(t *testing.T) {
	// Read LAZILY: the bot loads .env into the environment AFTER this code is
	// linked in, so an init-time capture would silently ignore a .env override.
	t.Setenv("KIRO_LOGIN_PYTHON", "/venv/bin/python3")
	if got := LoginPythonFromEnv(); got != "/venv/bin/python3" {
		t.Errorf("got %q, want the env value", got)
	}
	t.Setenv("KIRO_LOGIN_PYTHON", "")
	if got := LoginPythonFromEnv(); got != "python3" {
		t.Errorf("got %q, want the python3 fallback", got)
	}
}
