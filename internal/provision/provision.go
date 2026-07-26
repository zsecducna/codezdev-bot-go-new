// Package provision is the Go port of the Kiro-Go pool provisioning bridge —
// the SUPPLY side of the API-key business.
//
// Python original: kiro_provision.py (1172 lines). It drives a browser login
// for each supplied account, captures the credential the login produces, adds
// it to the Kiro-Go pool via the admin API, and ARCHIVES the local credential
// JSON once it is safely in the pool — so an on-disk copy of a live credential
// never lingers next to the working set after it has been handed to Kiro-Go.
//
// Three supply flows, routed PER LINE off the accounts text, so one upload may
// freely mix all three (kiro_provision.py:10-32; see Flow and
// ClassifyAccountLine):
//
//	idc    username|password|start_url  -> AWS IdC browser login -> ksk_ key
//	                                    -> POST /admin/add_kiro_api_key
//	m365   email|password[|mfa_secret]  -> M365/Entra SSO login   -> OAuth acct
//	                                    -> POST /admin/add_kiro_account
//	rawkey ksk_… (bare, no "|")         -> added DIRECTLY, no login at all
//	                                    -> POST /admin/add_kiro_api_key
//
// SCOPE — what stays out of Go (architecture-go.md §6): the browser logins
// themselves. Playwright, CloakBrowser and a headless Chromium live ENTIRELY
// in the child processes this package shells out to, under the same
// $KIRO_LOGIN_PYTHON venv contract Python used. Ported natively are the
// pure-HTTP and orchestration pieces only: account-line parsing/classification
// (parse.go), the pool admin client (pool.go), archive/rewrite-capture
// (archive.go), and the batch driver here.
//
// DEPENDENCY DIRECTION (architecture-go.md §1): this package must never import
// internal/handlers or internal/tg. Progress is streamed out through the
// ProgressFunc callback instead, so the handler layer can relay it to Telegram
// without this package knowing Telegram exists.
//
// IDEMPOTENCY & SAFETY (kiro_provision.py:42-55):
//   - Each account runs in its own fresh, disposable browser profile (handled
//     by the login scripts), so nothing is cached between accounts.
//   - Both add endpoints de-duplicate on the UNDERLYING Kiro account
//     server-side, so re-adding an already-pooled credential returns
//     duplicate=true rather than a second pool slot.
//   - The local credential JSON is archived (MOVED, not copied) only AFTER a
//     successful pool add, and only that account's file — a failed add leaves
//     the JSON in place so the operator can retry without re-running the
//     browser.
//   - Either login may REWRITE its accounts file: kiro-web-login.py on a
//     forced password reset, kiro-go-login.py when it registers a new TOTP
//     seed. This bridge reads the file back afterward and returns any changed
//     password / new MFA secret so the caller can persist it — the working
//     file is wiped on exit, so this is the ONLY place those values can still
//     be recovered.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codezdev-bot/internal/subproc"
)

// Status is a per-account outcome. Port of the `status` values documented at
// kiro_provision.py:936-939.
type Status string

const (
	// StatusAdded — a fresh pool slot was created.
	StatusAdded Status = "added"
	// StatusDuplicate — every returned slot had duplicate=true, i.e. the
	// credential was already in the pool (an idempotent retry).
	StatusDuplicate Status = "duplicate"
	// StatusRepaired — the account was already in the pool but DISABLED
	// (e.g. an expired OAuth refresh token) and this run's fresh credential
	// revived it. A SUCCESS, not a no-op: rendering it as a plain "already in
	// pool" would tell the admin their repair did nothing
	// (KIRO_GO_API.md:179; test_provision.py:507).
	StatusRepaired Status = "repaired"
	// StatusLoginFailed — the browser login produced no usable credential.
	StatusLoginFailed Status = "login_failed"
	// StatusNoKey — a credential was produced but is not a ksk_ key, so it
	// was never sent to the pool.
	StatusNoKey Status = "no_key"
	// StatusPoolRejected — the pool add failed. The credential JSON is
	// deliberately LEFT on disk so the operator can retry.
	StatusPoolRejected Status = "pool_rejected"
	// StatusDryRun — login + archive ran, the add request was only described.
	StatusDryRun Status = "dry_run"
	// StatusError — anything unexpected. Isolated to this one account.
	StatusError Status = "error"
)

// InPool reports whether this status means the credential is serving in the
// pool. Mirrors the CLI's success accounting (kiro_provision.py:1127).
func (s Status) InPool() bool {
	return s == StatusAdded || s == StatusDuplicate || s == StatusRepaired
}

// Result is one account's outcome, in file order. Port of the result dict at
// kiro_provision.py:1008-1011.
type Result struct {
	// Username is the reported identity. SECURITY: for a rawkey account this
	// is the MASKED key, never the raw one — the batch driver reports this
	// field precisely so the raw key can never reach a progress line, the
	// summary, or a log (kiro_provision.py:869-876, 1018-1019).
	Username string
	Flow     Flow
	Status   Status
	Detail   string
	// Added holds the pool slots the add endpoint returned.
	Added []AddedSlot
	// KeyMasked is the short display form of the captured credential.
	KeyMasked string
	// Archived is the path the credential JSON was moved to, or "" if it was
	// not archived (a failed add leaves it in place, retryable).
	Archived string
	// NewPassword is a password the login REWROTE (forced reset), or "".
	NewPassword string
	// NewMFA is a TOTP seed the login just ENROLLED, or "". Unlike a
	// password, a lost seed cannot be reset — it leaves the account MFA-locked
	// with no recovery path — so this is the highest-value field here.
	NewMFA string
	// RewriteSaved is the archive-dir file a rewritten credential was
	// persisted to, or "". Its existence is what makes a failed Telegram
	// report recoverable rather than fatal.
	RewriteSaved string
}

// ProgressFunc is called after each account so a caller can stream live
// updates. Best-effort: a panicking or slow callback must never break the
// batch, so the driver isolates it (kiro_provision.py:994-999).
//
// This callback IS the dependency inversion that keeps architecture-go.md §1's
// arrow pointing one way: the handler layer supplies a func that talks to
// Telegram, and this package never imports internal/tg or internal/handlers.
//
// SECURITY: `username` is Result.Username, i.e. already masked for rawkey
// accounts. Implementations must not try to recover the raw value.
type ProgressFunc func(idx, total int, username string, status Status, detail string)

// ErrBusy is returned by Provision when a batch is already running on this
// Provisioner.
//
// Single-flight is a hard requirement, not a nicety: the m365 flow's OAuth
// callback listener binds a FIXED loopback port (M365CallbackPort) that cannot
// be moved, so only ONE m365 login can run at a time host-wide
// (kiro_provision.py:79-87). A second concurrent batch would also mean a
// second browser fleet on the same host. telegram_bot.py enforces this with a
// module-level `_provpool_running` flag under a lock (telegram_bot.py:5668);
// this port owns the discipline in the Provisioner itself, so it cannot be
// forgotten by a caller.
var ErrBusy = errors.New("provision: a batch is already running")

// defaultNicknamePrefix matches provision_accounts' nickname_prefix default
// (kiro_provision.py:923), which the tests pin as "tg-pool-<username>".
const defaultNicknamePrefix = "tg-pool"

// Options configures a Provisioner. Every collaborator is injected — there are
// no package-level mutable globals, so two Provisioners (or a test and the
// bot) can never interfere.
type Options struct {
	// Login drives the browser-login subprocesses. Required for idc/m365
	// lines; a rawkey-only batch never touches it.
	Login LoginRunner
	// Pool is the Kiro-Go admin client. Required unless every run is a dry
	// run.
	Pool PoolClient
	// ArchiveRoot is where archived credential JSONs and rewrite records land
	// (see ArchiveRootFromEnv). Required.
	ArchiveRoot string
	// Logger receives NON-SECRET operational notices only (currently just the
	// timed-out-but-salvaged notice). Never used to log a credential, an
	// accounts line, or a request/response body. nil means slog.Default().
	Logger *slog.Logger
	// Now supplies the clock for the per-run archive tag and rewrite record
	// timestamps. nil means time.Now.
	Now func() time.Time
}

// RunOptions are the per-batch knobs, matching provision_accounts' keyword
// arguments (kiro_provision.py:922-924).
type RunOptions struct {
	// Headless runs the browsers without a visible window. NOTE the polarity:
	// the login scripts default to HEADED and opt into headless via a bare
	// --headless flag, so the production default is Headless: true and headed
	// mode is expressed by omitting the flag.
	Headless bool
	// StepDelay is a per-step visibility pause in seconds (0 for unattended
	// runs).
	StepDelay float64
	// NicknamePrefix prefixes each pool nickname; "" means "tg-pool".
	NicknamePrefix string
	// Region forces a region for the idc flow ("" = let the pool auto-probe).
	Region string
	// DryRun runs the login and the archive but only DESCRIBES the add
	// request instead of sending it.
	DryRun bool
	// Progress, if set, is called after each account.
	Progress ProgressFunc
}

// Provisioner drives batches. Safe for concurrent use; concurrent Provision
// calls are rejected with ErrBusy rather than serialized (see ErrBusy).
type Provisioner struct {
	opts Options

	mu      sync.Mutex
	running bool
}

// New builds a Provisioner.
func New(opts Options) *Provisioner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Provisioner{opts: opts}
}

// runCtx is the per-batch working state, the Go twin of Python's `ctx` dict
// (kiro_provision.py:958-968).
type runCtx struct {
	idcFile    string
	m365File   string
	keyOutDir  string
	acctOutDir string
	runTag     string
	ro         RunOptions
}

// Provision provisions every account in accountsText into the Kiro-Go pool and
// returns a Result per account, in file order. Port of provision_accounts
// (kiro_provision.py:922-1026).
func (p *Provisioner) Provision(ctx context.Context, accountsText string, ro RunOptions) ([]Result, error) {
	accounts := ParseAllAccounts(accountsText)
	total := len(accounts)
	results := []Result{}
	if total == 0 {
		// Nothing to do — and deliberately NOT an error, nor a reason to take
		// the single-flight lock or create a working dir.
		return results, nil
	}

	// Validated BEFORE any work starts, and before the single-flight lock is
	// taken. An empty ArchiveRoot is not a harmless default: filepath.Join("",
	// runTag) is the RELATIVE path "<runTag>", so every archived ksk_ key,
	// OAuth account JSON and plaintext TOTP-seed record would be written under
	// whatever the process CWD happens to be — plausibly a source checkout.
	// Python cannot reach this state (archive_root() always falls back to the
	// module directory), so the check has no Python twin; it exists because the
	// Go field is caller-supplied.
	if p.opts.ArchiveRoot == "" {
		return nil, errors.New("provision: ArchiveRoot is required (live credentials must not land in the process CWD)")
	}

	if err := p.acquire(); err != nil {
		return nil, err
	}
	defer p.release()

	if ro.NicknamePrefix == "" {
		ro.NicknamePrefix = defaultNicknamePrefix
	}

	// Per-run working dir, 0700. One accounts file PER FLOW plus a credential
	// output dir per flow. Separate files matter: each login script parses its
	// own format and numbers --index over only the lines IT recognizes, so a
	// shared file would desynchronize the two index spaces
	// (kiro_provision.py:951-955).
	work, err := os.MkdirTemp("", "kiro-provision-")
	if err != nil {
		return nil, fmt.Errorf("provision: create working dir: %w", err)
	}
	if err := os.Chmod(work, archiveDirMode); err != nil {
		os.RemoveAll(work)
		return nil, fmt.Errorf("provision: secure working dir: %w", err)
	}
	// Deferred, NOT placed at the end of the happy path: `work` holds the
	// plaintext accounts files (usernames + passwords + TOTP seeds) and any
	// not-yet-archived credential, so it MUST be wiped even if an unexpected
	// failure escapes the loop below — otherwise live credentials would linger
	// on disk (kiro_provision.py:1001-1005, 1021-1025). Anything still here at
	// that point is a login that never produced a usable credential.
	defer os.RemoveAll(work)

	rc := &runCtx{
		idcFile:    filepath.Join(work, "accounts-idc.txt"),
		m365File:   filepath.Join(work, "accounts-m365.txt"),
		keyOutDir:  filepath.Join(work, "api-keys"),
		acctOutDir: filepath.Join(work, "accounts"),
		// runTag groups this batch's archived JSONs under one directory.
		runTag: p.opts.Now().Format("20060102-150405"),
		ro:     ro,
	}
	for _, dir := range []string{rc.keyOutDir, rc.acctOutDir} {
		if err := os.MkdirAll(dir, archiveDirMode); err != nil {
			return nil, fmt.Errorf("provision: create output dir: %w", err)
		}
	}

	// Bucket per flow and assign each account its 1-based index WITHIN its own
	// flow's file. rawkey lines write no file and index nothing, but they are
	// still bucketed so the counting stays uniform.
	perFlow := map[Flow][]*Account{FlowIDC: nil, FlowM365: nil, FlowRawKey: nil}
	for i := range accounts {
		a := &accounts[i]
		perFlow[a.Flow] = append(perFlow[a.Flow], a)
		a.flowIndex = len(perFlow[a.Flow])
	}

	// Rebuild each flow's file from the CLASSIFIED fields rather than copying
	// the source lines: this drops headers/comments so line numbering matches
	// flowIndex exactly, which is what kiro-go-login.py's in-place MFA rewrite
	// indexes against.
	//
	// The idc line MUST carry field 4 when the account already has a TOTP
	// seed: kiro-web-login.py answers the MFA challenge from it, and an
	// enrolled account is challenged on EVERY sign-in. Dropping it here would
	// strand a re-provision at manual_action_required with no way to finish
	// (kiro_provision.py:983-986; test_idc_temp_file_carries_stored_seed...).
	// Conversely a seedless account must stay 3-field: an empty 4th field
	// would be read back as a seed of "" and is just noise.
	var idcBody strings.Builder
	for _, a := range perFlow[FlowIDC] {
		idcBody.WriteString(a.Username + "|" + a.Password + "|" + a.StartURL)
		if a.MFASecret != "" {
			idcBody.WriteString("|" + a.MFASecret)
		}
		idcBody.WriteString("\n")
	}
	var m365Body strings.Builder
	for _, a := range perFlow[FlowM365] {
		m365Body.WriteString(a.Username + "|" + a.Password)
		if a.MFASecret != "" {
			m365Body.WriteString("|" + a.MFASecret)
		}
		m365Body.WriteString("\n")
	}
	if err := writePrivate(rc.idcFile, idcBody.String()); err != nil {
		return nil, fmt.Errorf("provision: write idc accounts file: %w", err)
	}
	if err := writePrivate(rc.m365File, m365Body.String()); err != nil {
		return nil, fmt.Errorf("provision: write m365 accounts file: %w", err)
	}

	// The loop deliberately does NOT break on ctx cancellation. Every account
	// must appear in the returned slice, in file order — that one-Result-per-
	// account contract is what the handler layer renders its summary from, and
	// a short slice would silently under-report the batch. On a dead context
	// each remaining login fails immediately and is recorded as StatusError,
	// which is both honest and fast.
	for i := range accounts {
		acct := &accounts[i]
		res := Result{Username: acct.Username, Flow: acct.Flow, Status: StatusError}
		p.runOne(ctx, acct, rc, &res)
		results = append(results, res)
		// Report res.Username, NOT acct.Username: the rawkey runner masks the
		// key into res.Username, so the raw key never reaches progress
		// (kiro_provision.py:1018-1019).
		emitProgress(ro.Progress, i+1, total, res)
	}
	return results, nil
}

// acquire takes the single-flight lock (see ErrBusy).
func (p *Provisioner) acquire() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return ErrBusy
	}
	p.running = true
	return nil
}

// release drops the single-flight lock. Always deferred, so the feature can
// never wedge permanently "already running" the way telegram_bot.py:5673-5676
// warns about.
func (p *Provisioner) release() {
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
}

// runOne dispatches one account to its flow runner, isolating any panic so a
// per-account bug can never abort the remaining accounts. Port of the
// per-iteration try/except at kiro_provision.py:1012-1016.
func (p *Provisioner) runOne(ctx context.Context, acct *Account, rc *runCtx, res *Result) {
	defer func() {
		if r := recover(); r != nil {
			res.Status = StatusError
			res.Detail = fmt.Sprintf("unexpected error: %v", r)
		}
	}()
	switch acct.Flow {
	case FlowIDC:
		p.provisionIDC(ctx, acct, rc, res)
	case FlowM365:
		p.provisionM365(ctx, acct, rc, res)
	case FlowRawKey:
		p.provisionRawKey(ctx, acct, rc, res)
	default:
		res.Status = StatusError
		res.Detail = fmt.Sprintf("unknown flow %q", acct.Flow)
	}
}

// emitProgress calls the progress callback best-effort: a panicking callback
// must not take the batch down with it (Python's bare `except: pass`,
// kiro_provision.py:996-999).
func emitProgress(fn ProgressFunc, idx, total int, res Result) {
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(idx, total, res.Username, res.Status, res.Detail)
}

// ── idc flow ─────────────────────────────────────────────────────────────────

// provisionIDC provisions ONE idc account: browser-login on kiro.dev via AWS
// IAM Identity Center, generate a ksk_ key, add it to the pool, archive the key
// JSON. Port of _provision_idc (kiro_provision.py:728-801).
func (p *Provisioner) provisionIDC(ctx context.Context, acct *Account, rc *runCtx, res *Result) {
	username := acct.Username

	// Snapshot this account's password line BEFORE login so a forced-reset
	// rewrite can be detected afterward and the new password surfaced.
	pwBefore, _ := passwordInFile(rc.idcFile, username)
	// Same for the TOTP seed: a tenant that mandates MFA makes
	// kiro-web-login.py enrol a device on first sign-in and write the seed
	// into field 4 of the temp accounts file — which is wiped when the run
	// ends, so this diff is the only chance to capture it.
	mfaBefore, _ := idcMFAInFile(rc.idcFile, username)

	// DEFERRED, and registered BEFORE the login call, on purpose — this is
	// Python's `finally` (kiro_provision.py:756-764) and it is load-bearing:
	// kiro-web-login.py writes a freshly minted TOTP seed to the accounts file
	// BEFORE it submits the enrollment code, so a login that binds the device
	// and then HANGS (the enrollment steps can straddle the hard subprocess
	// deadline) has already put the seed on disk. Skipping the read on the
	// timeout/error paths would let the working dir be wiped and leave the
	// account MFA-locked with no recovery path (pinned by
	// test_idc_seed_captured_even_when_login_times_out).
	defer p.captureIDCRewrites(rc, username, pwBefore, mfaBefore, res)

	proc, err := p.opts.Login.RunIDCLogin(ctx, rc.idcFile, acct.flowIndex, rc.keyOutDir,
		rc.ro.Headless, rc.ro.StepDelay)

	// A FAILED LOGIN IS NOT A RETURN. The login script writes the key JSON the
	// moment it reads the raw value back, so a kill landing after that point
	// leaves a perfectly good key on disk. Bailing out on the error would skip
	// the lookup and let the working dir be wiped — destroying our only copy of
	// a live ksk_ that Kiro gives us no way to revoke (kiro_provision.py:744-752).
	//
	// The lookup therefore runs on EVERY path, not just the timeout one Python
	// singles out. Python only had to handle TimeoutExpired because it has no
	// notion of a parent context; here a caller-imposed batch deadline or a
	// shutdown cancellation surfaces as ctx.Err() rather than ErrTimeout
	// (subproc contract), and it strands exactly the same live key.
	//
	// Note: a NON-ZERO EXIT is not an error from subproc.Run (that package's
	// contract 5), matching Python's subprocess.run(check=False) — so err here
	// is only ever a genuine failure to run, a timeout, or a dead context.
	timedOut := errors.Is(err, subproc.ErrTimeout)

	path, payload, ok := findKeyJSON(rc.keyOutDir, username)
	if !ok {
		// No key to salvage, so the login's failure mode decides the report.
		if err != nil && !timedOut {
			res.Status = StatusError
			res.Detail = fmt.Sprintf("login subprocess error: %v", err)
			return
		}
		res.Status = StatusLoginFailed
		reason := "no API key produced"
		if timedOut {
			reason = "browser login timed out"
		}
		res.Detail = reason + tailDetail(proc)
		return
	}
	if err != nil {
		// Killed mid-run, but the key had already been captured — salvage it
		// rather than stranding a live credential on the account. Only the
		// non-secret username is logged.
		p.opts.Logger.Warn("provision: idc login failed after the key was captured; salvaging it into the pool",
			"username", username, "timed_out", timedOut)
	}

	ksk := strings.TrimSpace(payload.APIKey)
	res.KeyMasked = mask(ksk)
	if !strings.HasPrefix(ksk, "ksk_") {
		res.Status = StatusNoKey
		res.Detail = "generated key is not a ksk_ key"
		return
	}

	if rc.ro.DryRun {
		res.Status = StatusDryRun
		res.Detail = fmt.Sprintf("would add %s to the pool", res.KeyMasked)
		// Still archive, so the working dir doesn't retain the live key.
		res.Archived = archiveKeyJSON(path, p.opts.ArchiveRoot, rc.runTag)
		return
	}

	resp, err := p.opts.Pool.AddKiroAPIKey(ctx, ksk, rc.ro.NicknamePrefix+"-"+username, rc.ro.Region)
	if err != nil {
		res.Status = StatusPoolRejected
		res.Detail = err.Error()
		return
	}
	// Safely in the pool -> archive the local key JSON (move it out of the
	// working dir). NEVER touched on a failed add above, so the operator can
	// retry without re-running the browser.
	if summarizeAdd(res, resp) {
		res.Archived = archiveKeyJSON(path, p.opts.ArchiveRoot, rc.runTag)
	}
}

// captureIDCRewrites folds any credential kiro-web-login.py rewrote in the temp
// accounts file into res, by diffing against the pre-login snapshot. Port of
// _capture_idc_rewrites (kiro_provision.py:702-725).
//
// Both values have exactly one copy — the temp accounts file, which is wiped
// when the run ends — so this is the only chance to keep them. A lost password
// can be reset again; a lost TOTP seed for an already-enrolled device cannot,
// and locks the account out of every future sign-in. Hence the caller runs
// this on EVERY exit path, not just the successful one.
//
// No stderr fallback is needed here (unlike the m365 twin): kiro-web-login.py
// persists the seed to this file BEFORE submitting the enrollment code and
// aborts outright if that write fails, so a bound device always has a seed on
// disk for this diff to find.
func (p *Provisioner) captureIDCRewrites(rc *runCtx, username, pwBefore, mfaBefore string, res *Result) {
	if pwAfter, ok := passwordInFile(rc.idcFile, username); ok && pwAfter != "" && pwAfter != pwBefore {
		res.NewPassword = pwAfter
		res.RewriteSaved = recordRewrite(p.opts.ArchiveRoot, rc.runTag, username, "password", pwAfter, p.opts.Now())
	}
	if mfaAfter, ok := idcMFAInFile(rc.idcFile, username); ok && mfaAfter != "" && mfaAfter != mfaBefore {
		res.NewMFA = mfaAfter
		res.RewriteSaved = recordRewrite(p.opts.ArchiveRoot, rc.runTag, username, "mfa_secret", mfaAfter, p.opts.Now())
	}
}

// ── m365 flow ────────────────────────────────────────────────────────────────

// provisionM365 provisions ONE m365 account: drive the Microsoft 365 / Entra ID
// SSO login, then add the resulting OAuth account JSON to the pool and archive
// it. Port of _provision_m365 (kiro_provision.py:804-861).
func (p *Provisioner) provisionM365(ctx context.Context, acct *Account, rc *runCtx, res *Result) {
	email := acct.Username

	// Snapshot the MFA field BEFORE login: kiro-go-login.py rewrites the
	// accounts file when it registers a NEW TOTP seed, and the working copy is
	// wiped on exit, so this diff is the only way that seed survives the run.
	mfaBefore, _ := m365MFAInFile(rc.m365File, email)

	proc, err := p.opts.Login.RunM365Login(ctx, rc.m365File, acct.flowIndex, rc.acctOutDir,
		email, rc.ro.Headless, rc.ro.StepDelay)
	if err != nil {
		// Unlike the idc twin, a timeout here IS terminal and returns
		// immediately (kiro_provision.py:815-820). The asymmetry is real, not
		// an oversight: kiro-go-login.py writes its account JSON only at the
		// very end of a completed OAuth exchange, so a kill mid-run cannot
		// have left a usable credential behind for us to salvage.
		if errors.Is(err, subproc.ErrTimeout) {
			res.Status = StatusLoginFailed
			res.Detail = "browser login timed out"
			return
		}
		res.Status = StatusError
		res.Detail = fmt.Sprintf("login subprocess error: %v", err)
		return
	}

	if mfaAfter, ok := m365MFAInFile(rc.m365File, email); ok && mfaAfter != "" && mfaAfter != mfaBefore {
		res.NewMFA = mfaAfter
		res.RewriteSaved = recordRewrite(p.opts.ArchiveRoot, rc.runTag, email, "mfa_secret", mfaAfter, p.opts.Now())
	} else {
		// The file diff is the normal capture path, but it only works if the
		// helper SUCCEEDED in writing the seed back. When that write fails the
		// helper has already enrolled the seed upstream (the account's MFA is
		// changed) and can only print it to stderr — which a successful login
		// otherwise discards. Missing it would leave the account MFA-locked
		// with no recovery, so scrape it as a last resort
		// (kiro_provision.py:826-837).
		if m := mfaStderrRe.FindStringSubmatch(proc.Stderr); m != nil {
			// IDENTITY-CHECKED: attributing another account's seed would
			// report the wrong credential for this one
			// (test_m365_stderr_seed_for_another_account_ignored).
			if strings.EqualFold(strings.TrimSpace(m[1]), strings.TrimSpace(email)) {
				res.NewMFA = strings.TrimSpace(m[2])
				res.RewriteSaved = recordRewrite(p.opts.ArchiveRoot, rc.runTag, email, "mfa_secret", res.NewMFA, p.opts.Now())
			}
		}
	}

	path, raw, payload, ok := findAccountJSON(rc.acctOutDir, email)
	if !ok {
		res.Status = StatusLoginFailed
		res.Detail = "no account credential produced" + tailDetail(proc)
		return
	}

	res.KeyMasked = mask(payload.AccessToken)
	if rc.ro.DryRun {
		res.Status = StatusDryRun
		res.Detail = fmt.Sprintf("would add %s account (%s) to the pool",
			orQuestionMark(payload.AuthMethod), orQuestionMark(payload.Region))
		// Still archive, so the working dir doesn't retain the live refresh
		// token.
		res.Archived = archiveKeyJSON(path, p.opts.ArchiveRoot, rc.runTag)
		return
	}

	resp, err := p.opts.Pool.AddKiroAccount(ctx, raw, rc.ro.NicknamePrefix+"-"+email)
	if err != nil {
		res.Status = StatusPoolRejected
		res.Detail = err.Error()
		return
	}
	if summarizeAdd(res, resp) {
		res.Archived = archiveKeyJSON(path, p.opts.ArchiveRoot, rc.runTag)
	}
}

// ── rawkey flow ──────────────────────────────────────────────────────────────

// provisionRawKey provisions ONE pre-minted `ksk_` key: no browser login, no
// file — the key is already in hand, so it goes straight to the pool through
// the same endpoint the idc flow ends at. Port of _provision_rawkey
// (kiro_provision.py:864-897).
//
// SECURITY: the raw key IS the account identity here, so it must never reach a
// progress line, the summary, or a log. res.Username is overwritten with the
// MASKED form (the batch driver reports res.Username, not the account value)
// and the full key is used ONLY for the add-to-pool request itself — even the
// per-run nickname carries the masked form, not the key.
func (p *Provisioner) provisionRawKey(ctx context.Context, acct *Account, rc *runCtx, res *Result) {
	ksk := strings.TrimSpace(acct.Username)
	masked := mask(ksk)
	res.Username = masked
	res.KeyMasked = masked

	if !strings.HasPrefix(ksk, "ksk_") {
		// rawKeyRe already guarantees this; guard defensively, because a value
		// reaching here without the prefix means classification changed, not
		// that we found a key worth pushing to the pool.
		res.Status = StatusNoKey
		res.Detail = "not a ksk_ key"
		return
	}

	if rc.ro.DryRun {
		res.Status = StatusDryRun
		res.Detail = fmt.Sprintf("would add %s to the pool", masked)
		return
	}

	resp, err := p.opts.Pool.AddKiroAPIKey(ctx, ksk, rc.ro.NicknamePrefix+"-"+masked, rc.ro.Region)
	if err != nil {
		res.Status = StatusPoolRejected
		res.Detail = err.Error()
		return
	}
	// No local JSON to archive — nothing was written to disk for a pasted key.
	summarizeAdd(res, resp)
}

// ── shared result folding ────────────────────────────────────────────────────

// summarizeAdd folds a pool add-response into res (status + detail) and
// reports whether the credential is safely in the pool (and so may be
// archived). Shared by both flows: /admin/add_kiro_api_key and
// /admin/add_kiro_account return the same {added, skipped} shape. Port of
// _summarize_add (kiro_provision.py:670-699).
func summarizeAdd(res *Result, resp *AddResponse) bool {
	// The contract says a 2xx body is a JSON object; guard a malformed shape
	// so a nil/garbage response can't take the batch down mid-run.
	if resp == nil {
		res.Status = StatusPoolRejected
		res.Detail = "unexpected add response shape"
		return false
	}
	res.Added = resp.Added
	if len(resp.Added) == 0 {
		res.Status = StatusPoolRejected
		res.Detail = "pool accepted nothing"
		return false
	}

	// duplicate=true on EVERY returned slot means the credential was already
	// in the pool (an idempotent retry) — reported distinctly from a fresh add.
	allDup := true
	// repaired=true means the slot was BANNED (typically an expired OAuth
	// refresh token) and this upload's fresh credential revived it. That is
	// the whole point of re-uploading a dead account, so it must not read as a
	// no-op "duplicate".
	repaired := false
	regions := make([]string, 0, len(resp.Added))
	for _, a := range resp.Added {
		if !a.Duplicate {
			allDup = false
		}
		if a.Repaired {
			repaired = true
		}
		regions = append(regions, orQuestionMark(a.Region))
	}

	what := "added"
	switch {
	case repaired:
		res.Status = StatusRepaired
		what = "revived (was banned)"
	case allDup:
		res.Status = StatusDuplicate
		what = "already in pool"
	default:
		res.Status = StatusAdded
	}

	joined := strings.Join(regions, ", ")
	if joined == "" {
		joined = "—"
	}
	res.Detail = fmt.Sprintf("%s (%s)", what, joined)
	if n := len(resp.Skipped); n > 0 {
		res.Detail += fmt.Sprintf("; skipped %d region(s)", n)
	}
	return true
}

// tailDetail returns the tail of a failed login's output for triage — never
// the whole thing, which can be noisy enough to blow the Telegram message
// limit. Port of _tail_detail (kiro_provision.py:900-916).
//
// Chatter that carries no diagnostic value is dropped FIRST, before the tail
// is taken: cloakbrowser prints an "Update available" nag to stderr on every
// run, and since the tail is only 3 lines deep the nag EVICTED the actual
// error from the report — several accounts once failed with nothing but the
// nag to show for it, which is what an operator has to debug from.
//
// SECURITY: every credential-report line the login scripts emit is REDACTED
// here — the m365 seed (mfaStderrRe) and its idc siblings, the reset-password
// and enrolled-seed reports (secretReportRe; see its doc for the full list and
// the source lines). Unlike the Python original, this detail string is handed
// to ProgressFunc and relayed onward to a Telegram chat log, so an unredacted
// copy would persist a live password or TOTP seed in a place we do not control.
// Nothing is LOST by redacting: the operator's real copy comes from the
// accounts-file diff -> recordRewrite -> Result.NewPassword / Result.NewMFA and
// the 0600 archive record, all of which run before this function does.
func tailDetail(proc subproc.Result) string {
	src := proc.Stderr
	if src == "" {
		src = proc.Stdout
	}
	// Normalize CRLF/CR the way Python's .splitlines() does, so a child
	// relayed through a pty doesn't leave a stray CR in every reported line.
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	raw := []string{}
	for _, ln := range strings.Split(strings.TrimSpace(src), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		// Two passes on purpose. The first keeps the m365 seed line's redaction
		// byte-identical to what the capture path pins; the second covers its
		// SIBLINGS — kiro-web-login.py's "NEW PASSWORD" / "attempted NEW
		// PASSWORD" (stderr, which this function PREFERS over stdout) and its
		// "MFA SECRET" line (stdout). Redacting only the m365 seed left a live
		// plaintext password reaching Telegram on the most ordinary idc failure
		// there is: a portal-forced reset that the password policy rejects
		// (kiro-web-login.py:575-578) prints the attempted password and then
		// returns an error, so no key JSON exists and this tail IS the report.
		// Both regexps are idempotent, so the order and the overlap are safe.
		ln = mfaStderrRe.ReplaceAllString(ln, "!! MFA secret for ${1}: <redacted>")
		raw = append(raw, secretReportRe.ReplaceAllString(ln, "${1}<redacted>"))
	}
	noise := []string{"update available:", "run: pip install"}
	lines := []string{}
	for _, ln := range raw {
		lower := strings.ToLower(ln)
		keep := true
		for _, n := range noise {
			if strings.Contains(lower, n) {
				keep = false
				break
			}
		}
		if keep {
			lines = append(lines, ln)
		}
	}
	// Fall back to the unfiltered tail when filtering leaves nothing: a bare
	// "no API key produced" with no context at all would be strictly worse
	// than showing the nag.
	if len(lines) == 0 {
		lines = raw
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return " — " + strings.Join(lines, " / ")
}

// orQuestionMark renders an absent optional string as "?", matching Python's
// `x or "?"` in the detail messages.
func orQuestionMark(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// compile-time assertion that the concrete client satisfies the seam the
// orchestrator depends on.
var _ PoolClient = (*AdminClient)(nil)

// compile-time assertion for the login seam.
var _ LoginRunner = (*ScriptRunner)(nil)
