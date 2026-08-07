package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"codezdev-bot/internal/subproc"
)

// ── browser-login subprocess boundary ────────────────────────────────────────
//
// architecture-go.md §6 fixes the decision this file implements: the Go bot
// keeps ALL browser / Playwright / CloakBrowser work in child processes, under
// the same venv contract as Python ($KIRO_LOGIN_PYTHON,
// kiro_provision.py:99-105). Nothing here reimplements a login; it only
// launches one, waits for it, and reads what it left on disk.

const (
	// M365CallbackPort mirrors kiro-login-helper.SOCIAL_REDIRECT_PORT, which
	// is baked into the redirect_uri registered with the IdP and so CANNOT be
	// moved (kiro_provision.py:79-87). Two consequences worth knowing: only
	// ONE m365 login can run at a time host-wide, and any other service on
	// this port (3128 is squid's default) breaks the flow outright.
	M365CallbackPort = 3128

	// LoginTimeout is the per-account wall-clock budget for a browser login,
	// passed to the script as its OWN --timeout (kiro_provision.py:93).
	// Generous: a cold headless Chromium start plus the multi-page AWS IdC
	// sign-in and key creation can take a while.
	LoginTimeout = 300 * time.Second

	// SubprocGrace is extra slack added to the subprocess timeout over the
	// script's own --timeout, so the script's internal deadline fires (clean
	// per-account error) BEFORE our hard process-group kill does
	// (kiro_provision.py:94-97).
	SubprocGrace = 60 * time.Second

	// Script filenames, vendored alongside the bot (kiro_provision.py:75-77).
	// kiro-go-login.py imports kiro-login-helper.py as a SIBLING via importlib
	// (its filename has dashes, so a plain import is impossible), hence all
	// three must live in the same directory.
	webLoginScriptName    = "kiro-web-login.py"
	goLoginScriptName     = "kiro-go-login.py"
	loginHelperScriptName = "kiro-login-helper.py"
)

// LoginRunner is the seam between the orchestrator and the actual child
// processes. An interface so the batch tests can exercise every path — a
// login that succeeds, one that enrolls a TOTP seed, one that hangs, one that
// produces nothing — without a browser. This is the Go equivalent of Python's
// monkeypatching of kp._run_login / kp._run_m365_login, which is how all 38
// tests in tests/test_provision.py stay hermetic.
type LoginRunner interface {
	// RunIDCLogin drives kiro-web-login.py for a single 1-based account index
	// of an `username|password|start_url[|mfa_secret]` file.
	RunIDCLogin(ctx context.Context, accountsFile string, index int, keyOutDir string, headless bool, stepDelay float64) (subproc.Result, error)
	// RunM365Login drives kiro-go-login.py for a single 1-based account index
	// of an `email|password[|mfa_secret]` file.
	RunM365Login(ctx context.Context, m365File string, index int, outDir, email string, headless bool, stepDelay float64) (subproc.Result, error)
}

// LoginPythonFromEnv resolves the Python interpreter that has Playwright +
// CloakBrowser installed. Port of login_python (kiro_provision.py:99-105).
//
// This reads the RAW ambient environment only — it does NOT see a
// KIRO_LOGIN_PYTHON set via .env, because internal/config's snapshot is
// never copied back into os.Environ (config.Load's doc comment) and this
// package deliberately does not import internal/config to read it directly.
// Production code with a *config.Config available (internal/handlers) must
// resolve the interpreter through config.Config.KiroLoginPython() instead
// (bridged via Bot.loginPythonStr()) and pass the result to NewScriptRunner/
// entra.SoftDeleteByUPN as a parameter, the same way tron.Address's doc
// comment describes for that package's own raw-env primitive. This function
// survives for callers with no Config to resolve from (e.g. tests) and as
// the low-level primitive that seam builds on — do not call it directly
// from a code path that has cfg.
//
// KIRO_LOGIN_PYTHON is read LAZILY, at call time, rather than captured once
// at init, so a change to the ambient environment before this runs is not
// silently missed. Python falls back to sys.executable (correct when the
// bot itself has the browser stack); a Go binary has no such notion, so the
// fallback is a plain "python3" resolved through PATH.
func LoginPythonFromEnv() string {
	if v := os.Getenv("KIRO_LOGIN_PYTHON"); v != "" {
		return v
	}
	return "python3"
}

// ScriptRunner is the production LoginRunner: it shells out to the vendored
// browser-login scripts through internal/subproc.
type ScriptRunner struct {
	// Python is the interpreter with the browser stack (see
	// LoginPythonFromEnv).
	Python string
	// WebLoginScript / GoLoginScript / LoginHelperScript are absolute paths.
	// LoginHelperScript is never executed by us — kiro-go-login.py imports it
	// as a sibling — but its presence is part of BrowserStackError's check.
	WebLoginScript    string
	GoLoginScript     string
	LoginHelperScript string
	// Timeout is the script's own --timeout budget; Grace is the extra slack
	// before the hard kill. Zero means the LoginTimeout / SubprocGrace
	// defaults.
	Timeout time.Duration
	Grace   time.Duration
}

// NewScriptRunner builds a ScriptRunner for the three scripts vendored in
// scriptDir, with the default timeout budget.
func NewScriptRunner(python, scriptDir string) *ScriptRunner {
	return &ScriptRunner{
		Python:            python,
		WebLoginScript:    filepath.Join(scriptDir, webLoginScriptName),
		GoLoginScript:     filepath.Join(scriptDir, goLoginScriptName),
		LoginHelperScript: filepath.Join(scriptDir, loginHelperScriptName),
		Timeout:           LoginTimeout,
		Grace:             SubprocGrace,
	}
}

func (r *ScriptRunner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return LoginTimeout
}

func (r *ScriptRunner) grace() time.Duration {
	if r.Grace > 0 {
		return r.Grace
	}
	return SubprocGrace
}

// idcLoginArgs builds kiro-web-login.py's argv. Port of _run_login's command
// (kiro_provision.py:478-486). Kept as a pure function so the exact flag set
// and ORDER (the accounts file is positional and comes last, before the
// optional --headless) can be asserted in a test without launching anything.
//
// kiro-web-login.py defaults to HEADED and enables headless via the PRESENCE
// of --headless (a store_true flag with no headed counterpart), so headed mode
// is expressed by simply omitting the flag.
func (r *ScriptRunner) idcLoginArgs(accountsFile string, index int, keyOutDir string, headless bool, stepDelay float64) []string {
	args := []string{
		r.WebLoginScript,
		"--step-delay", formatStepDelay(stepDelay),
		"--timeout", strconv.Itoa(int(r.timeout().Seconds())),
		"--index", strconv.Itoa(index),
		"--api-key-name", "Kiro-Go",
		"--api-key-out-dir", keyOutDir,
		accountsFile,
	}
	if headless {
		args = append(args, "--headless")
	}
	return args
}

// m365LoginArgs builds kiro-go-login.py's argv. Port of _run_m365_login's
// command (kiro_provision.py:509-517).
//
// --email is passed EXPLICITLY and is NOT optional for us
// (kiro_provision.py:501-508, pinned by test_m365_login_pins_email_argument):
// without it the script derives the address from the access token's JWT
// claims, falling back through preferred_username/upn/unique_name all the way
// to `name` (a display name like "Bob Smith"), or to the account UUID when no
// claim exists at all. That address names the output file AND becomes its
// `email` field, so a tenant whose UPN differs from the sign-in alias would
// leave us unable to match the file back to the account we logged in — a
// successful login discarded as a failure, and if that login just enrolled a
// TOTP seed, the account is left MFA-changed for nothing.
//
// --step-delay defaults to 1.0s of per-step visibility pauses in the script;
// 0 is passed explicitly for an unattended production run.
func (r *ScriptRunner) m365LoginArgs(m365File string, index int, outDir, email string, headless bool, stepDelay float64) []string {
	args := []string{
		r.GoLoginScript,
		"--m365-accounts-file", m365File,
		"--index", strconv.Itoa(index),
		"--email", email,
		"--step-delay", formatStepDelay(stepDelay),
		"--timeout", strconv.Itoa(int(r.timeout().Seconds())),
		"--out-dir", outDir,
	}
	if headless {
		args = append(args, "--headless")
	}
	return args
}

// formatStepDelay renders the --step-delay value the way argparse type=float
// expects. Trailing zeros are dropped ("0" rather than Python's "0.0"); both
// parse identically, and no test or script behaviour keys on the spelling.
func formatStepDelay(d float64) string {
	return strconv.FormatFloat(d, 'f', -1, 64)
}

// RunIDCLogin implements LoginRunner. Port of _run_login
// (kiro_provision.py:471-488).
//
// The subprocess budget is the script's own --timeout PLUS the grace, so the
// script's internal deadline produces a clean per-account error before our
// hard process-group kill lands.
func (r *ScriptRunner) RunIDCLogin(ctx context.Context, accountsFile string, index int, keyOutDir string, headless bool, stepDelay float64) (subproc.Result, error) {
	return subproc.Run(ctx, subproc.Request{
		Path:    r.Python,
		Args:    r.idcLoginArgs(accountsFile, index, keyOutDir, headless, stepDelay),
		Timeout: r.timeout() + r.grace(),
	})
}

// RunM365Login implements LoginRunner. Port of _run_m365_login
// (kiro_provision.py:491-519).
func (r *ScriptRunner) RunM365Login(ctx context.Context, m365File string, index int, outDir, email string, headless bool, stepDelay float64) (subproc.Result, error) {
	return subproc.Run(ctx, subproc.Request{
		Path:    r.Python,
		Args:    r.m365LoginArgs(m365File, index, outDir, email, headless, stepDelay),
		Timeout: r.timeout() + r.grace(),
	})
}

// BrowserStackError returns "" if a browser login can run, else a human-facing
// reason. Port of _provpool_browser_stack_error (telegram_bot.py:5600-5634);
// the handler layer owns WHEN to call it, this package owns HOW.
//
// Call it ONLY when a batch actually contains browser-login lines (idc /
// m365). A keys-only paste skips it, so the pool can still be topped up from
// pre-minted `ksk_` keys on a host without the browser stack installed.
//
// All three scripts are required TOGETHER rather than per-flow: a browser
// batch may mix idc and m365 lines, and kiro-go-login.py additionally imports
// kiro-login-helper.py as a sibling, so a missing one would only surface
// mid-batch after minutes of browser time.
func (r *ScriptRunner) BrowserStackError(ctx context.Context) string {
	for _, script := range []string{r.WebLoginScript, r.GoLoginScript, r.LoginHelperScript} {
		if fi, err := os.Stat(script); err != nil || fi.IsDir() {
			return "login script missing: " + script
		}
	}
	// Probe the login interpreter for the browser stack in a SHORT
	// subprocess, so a missing dependency can never crash-import into the
	// stdlib-only bot process.
	res, err := subproc.Run(ctx, subproc.Request{
		Path:    r.Python,
		Args:    []string{"-c", "import playwright, cloakbrowser"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return fmt.Sprintf("couldn't probe the browser stack: %v", err)
	}
	if res.ExitCode != 0 {
		tail := "import failed"
		for _, ln := range strings.Split(strings.TrimSpace(res.Stderr), "\n") {
			if strings.TrimSpace(ln) != "" {
				tail = ln
			}
		}
		return fmt.Sprintf("browser stack not installed for the login interpreter (%s): %s", r.Python, tail)
	}
	return ""
}

// CallbackPortFree reports whether the M365 OAuth callback port can be bound
// right now. Port of m365_callback_port_free (kiro_provision.py:590-616).
//
// ADVISORY ONLY — the port is bound later, by the login subprocess, so this is
// a point-in-time check that can race. It is still worth doing: it turns the
// two realistic failures (another service squatting the port, or the other bot
// instance on this host mid-batch) into one clear up-front message instead of
// every m365 account failing individually after minutes of browser time.
//
// It mirrors the listener's bind exactly: 127.0.0.1 over IPv4 with
// SO_REUSEADDR, because kiro-login-helper's _V4Server sets
// allow_reuse_address = True. Go's net package sets SO_REUSEADDR on every
// TCP listener by default, so "tcp4" is all that is needed here. Matching
// matters in BOTH directions — without SO_REUSEADDR this probe would report
// "busy" for a port merely left in TIME_WAIT by the previous account's
// callback connection, which the real listener rebinds happily, and we would
// refuse a batch that would have worked. SO_REUSEADDR still does NOT let a
// bind succeed against a live listener, which is exactly the case to catch.
//
// The port is a parameter (not read from the constant) so the check is
// testable without asserting anything about whatever holds 3128 on the host
// running the tests.
func CallbackPortFree(port int) bool {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	// The probe runs before every m365 batch — it must NOT keep the port, or
	// it would block the very login it is clearing the way for.
	ln.Close()
	return true
}

// M365CallbackPortFree is CallbackPortFree bound to the real constant.
func M365CallbackPortFree() bool { return CallbackPortFree(M365CallbackPort) }

// ── credential discovery ─────────────────────────────────────────────────────

// keyJSON is the shape kiro-web-login.save_api_key writes.
type keyJSON struct {
	Username string `json:"username"`
	APIKey   string `json:"api_key"`
}

// accountJSON is the subset of the Kiro-Go account JSON kiro-go-login.py
// writes that this package needs to IDENTIFY and describe the credential. The
// full document is forwarded to the pool verbatim as raw bytes — see
// AdminClient.AddKiroAccount.
type accountJSON struct {
	AccessToken string `json:"accessToken"`
	Email       string `json:"email"`
	AuthMethod  string `json:"authMethod"`
	Region      string `json:"region"`
}

// findKeyJSON locates the JSON kiro-web-login.py wrote for `username`. Port of
// _find_key_json (kiro_provision.py:558-587).
//
// Tries the exact safe-user filename first, then any file in the dir whose
// payload username matches (covers the disambiguation-suffix case). Falls back
// to the exact file even if its username field didn't match (older
// single-account layout), as long as it holds a key.
func findKeyJSON(keyOutDir, username string) (string, keyJSON, bool) {
	exact := filepath.Join(keyOutDir, "kiro-api-key_"+safeUser(username)+".json")
	candidates := []string{}
	if isFile(exact) {
		candidates = append(candidates, exact)
	}
	for _, p := range listPrefixed(keyOutDir, "kiro-api-key_", ".json") {
		if p != exact {
			candidates = append(candidates, p)
		}
	}
	for _, p := range candidates {
		var payload keyJSON
		if !readJSONFile(p, &payload) {
			continue
		}
		if payload.Username == username && payload.APIKey != "" {
			return p, payload, true
		}
	}
	if isFile(exact) {
		var payload keyJSON
		if readJSONFile(exact, &payload) && payload.APIKey != "" {
			return exact, payload, true
		}
	}
	return "", keyJSON{}, false
}

// findAccountJSON locates the Kiro-Go account JSON kiro-go-login.py wrote for
// `email`, returning its path, its RAW bytes (for verbatim forwarding) and the
// parsed subset. Port of _find_account_json (kiro_provision.py:522-555).
//
// Both the filename and the payload's `email` are pinned to the address passed
// as --email, so the exact path normally hits. The dir scan is the safety net
// for the one case that renames the file: an address whose sanitized form is
// empty, which the script replaces with a timestamp.
//
// The match is deliberately IDENTITY-BASED, never positional: out_dir is
// SHARED across the batch and a failed account's JSON is left behind on
// purpose (so it stays retryable), so returning "some account file" could hand
// this account ANOTHER account's live credential — adding that one twice and
// silently dropping this one (pinned by
// test_m365_never_attributes_another_accounts_credential). Matching is
// case-insensitive only because an address's case is not meaningful.
func findAccountJSON(outDir, email string) (string, json.RawMessage, accountJSON, bool) {
	want := strings.ToLower(strings.TrimSpace(email))
	exact := filepath.Join(outDir, "kiro-go-account_"+safeComponent(email)+".json")
	candidates := []string{}
	if isFile(exact) {
		candidates = append(candidates, exact)
	}
	for _, p := range listPrefixed(outDir, "kiro-go-account_", ".json") {
		if p != exact {
			candidates = append(candidates, p)
		}
	}
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var payload accountJSON
		if json.Unmarshal(raw, &payload) != nil {
			continue
		}
		if payload.AccessToken == "" {
			continue
		}
		if strings.ToLower(strings.TrimSpace(payload.Email)) == want {
			return p, json.RawMessage(raw), payload, true
		}
	}
	return "", nil, accountJSON{}, false
}

// listPrefixed returns the sorted absolute paths of the entries in dir whose
// names have the given prefix and suffix. Mirrors Python's
// `sorted(os.listdir(dir))` guarded by os.path.isdir, so a missing dir is
// simply "no candidates" rather than an error.
func listPrefixed(dir, prefix, suffix string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := []string{}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(dir, n))
	}
	return out
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// readJSONFile decodes path into out, reporting false on any read or parse
// failure — a half-written or corrupt candidate is skipped, never fatal
// (Python's `except (OSError, ValueError): continue`).
func readJSONFile(path string, out any) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}
