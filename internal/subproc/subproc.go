// Package subproc is the shared child-process helper for the Go port.
//
// Python original: the scattered `subprocess.run(..., capture_output=True,
// text=True, timeout=N)` conventions across the bot — kiro_provision.py:487
// (_run_login), kiro_provision.py:518 (_run_m365_login), telegram_bot.py:5625
// (the browser-stack import probe) — plus the "single JSON object on the LAST
// stdout line" output contract that aws_idc.py:1-13 and
// validate_aws_account.py document and every boto3/MSAL helper obeys.
//
// architecture-go.md §6 fixes the design decision this package exists to
// serve: ALL browser / MSAL / boto3 work stays in child processes. Go ports
// only the pure-HTTP and orchestration pieces, so every one of those children
// is driven through here.
//
// Contracts this package encodes (each one load-bearing, not style):
//
//  1. Secrets travel via the ENVIRONMENT, never argv (aws_idc.py:8-13:
//     "Credentials — two modes, secrets NEVER on argv"). Request.Env is the
//     only channel for them; a process listing (`ps`) is world-readable on
//     the deploy hosts, so an argv-passed key leaks to every local user.
//     This package never logs anything at all — not argv, not env, not the
//     captured streams — because all three routinely carry live credentials.
//
//  2. stdout and stderr are captured SEPARATELY and both are retained even
//     on a failed or timed-out run. kiro_provision.py:833 scrapes a freshly
//     enrolled TOTP seed out of the child's stderr as the last-resort
//     recovery path ("!! MFA secret for <email>: <secret>"); merging the
//     streams, or dropping them on a non-zero exit, would destroy the only
//     surviving copy of a credential that leaves the account MFA-locked with
//     no recovery if lost.
//
//  3. Neither stream is truncated. The tail-trimming for human display is a
//     presentation concern of the caller (kiro_provision.py:900 _tail_detail);
//     doing it here would silently drop a seed line that scrolled past.
//
//  4. On timeout or context cancellation the whole PROCESS GROUP is killed,
//     not just the direct child. The login scripts spawn a headless Chromium
//     (and node helpers); killing only the Python parent would leave orphaned
//     browsers holding memory, profile locks and — for the M365 flow — the
//     fixed loopback callback port 3128 (kiro_provision.py:79-87), wedging
//     every later batch on the host.
//
//  5. A NON-ZERO EXIT IS NOT AN ERROR from Run's point of view. This mirrors
//     Python's subprocess.run(check=False) exactly, and the callers depend on
//     it: kiro_provision.py:766 looks for the key JSON on disk regardless of
//     the child's return code (a login can produce a perfectly good key and
//     still exit non-zero on a later cleanup step), and aws_idc.py:23 states
//     the helpers "always exit 0 so the caller parses JSON rather than a
//     crash" — meaning the exit code carries no information the JSON does not.
//     Callers that do care read Result.ExitCode / Result.Success(). Run
//     returns a non-nil error ONLY for: a failure to start, a timeout
//     (errors.Is(err, ErrTimeout)), a cancelled parent context, or an I/O
//     error while capturing the streams.
package subproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrTimeout is wrapped into the error Run returns when Request.Timeout
// elapsed and the child's process group had to be killed. It is the Go twin
// of Python's subprocess.TimeoutExpired, which kiro_provision.py:744 and
// :815 both branch on explicitly — and, critically, the idc path treats as
// SALVAGEABLE rather than fatal (the key JSON may already be on disk), so
// callers must be able to tell a timeout apart from every other failure.
var ErrTimeout = errors.New("subproc: timed out")

// ErrNoJSONLine is returned (possibly wrapped) by ParseLastJSONLine when the
// child's stdout does not end in a single JSON object — the violated half of
// the aws_idc.py:5-7 / validate_aws_account.py output contract.
var ErrNoJSONLine = errors.New("subproc: no JSON object on the last stdout line")

// Request describes one child process to run.
type Request struct {
	// Path is the executable to run (an interpreter path such as
	// $KIRO_LOGIN_PYTHON, or a plain "python3"/"node"). Required.
	Path string
	// Args are the arguments after argv[0]. NEVER put a secret here — see
	// contract 1 in the package doc; use Env instead.
	Args []string
	// Env holds EXTRA "KEY=VALUE" entries layered on top of the parent
	// process environment. This is the sanctioned channel for credentials
	// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY per aws_idc.py:12-13).
	// When empty the child simply inherits the parent environment.
	Env []string
	// Dir is the child's working directory ("" = inherit the parent's).
	Dir string
	// Stdin, when non-empty, is fed to the child on standard input and the
	// pipe is then closed. When empty the child gets an immediately-EOF
	// stdin, never the parent's terminal (an interactive prompt inside an
	// unattended batch would otherwise hang until the timeout).
	Stdin []byte
	// Timeout is the wall-clock budget for the whole run. Zero means "no
	// timeout" and is only appropriate for a child that cannot hang. The
	// browser logins pass LOGIN_TIMEOUT_S + SUBPROC_GRACE_S so the script's
	// OWN internal deadline fires first and produces a clean per-account
	// error before this hard kill lands (kiro_provision.py:93-97).
	Timeout time.Duration
}

// Result is everything observable about a finished (or killed) child.
type Result struct {
	// Stdout is the child's complete standard output, untruncated.
	Stdout string
	// Stderr is the child's complete standard error, untruncated and kept
	// separate from Stdout — see package contract 2 (MFA-seed recovery).
	Stderr string
	// ExitCode is the child's exit status, or -1 when it never exited
	// normally (killed by a signal, or never started).
	ExitCode int
	// TimedOut reports that Request.Timeout elapsed and the process group
	// was killed. Stdout/Stderr still hold whatever the child managed to
	// write before the kill — the timed-out idc login path depends on that
	// partial output (kiro_provision.py:744-752).
	TimedOut bool
}

// Success reports a clean exit-0 run. Provided because Run deliberately does
// NOT turn a non-zero exit into an error (package contract 5), so callers
// that do care about the status have an obvious way to ask.
func (r Result) Success() bool { return r.ExitCode == 0 && !r.TimedOut }

// Run executes one child process to completion, capturing both streams.
//
// See package contract 5 for the error policy: a non-zero exit is reported
// through Result.ExitCode, NOT through err. err is non-nil only for a start
// failure, a timeout (errors.Is(err, ErrTimeout)), a cancelled ctx, or a
// stream-capture I/O error. Result is always populated as far as the run got,
// including on every error path, because partial output is the only remaining
// copy of some credentials.
func Run(ctx context.Context, req Request) (Result, error) {
	res := Result{ExitCode: -1}
	if req.Path == "" {
		return res, errors.New("subproc: Path is required")
	}

	// A derived context carries the timeout, so a parent cancellation and a
	// timeout both funnel through the same single kill path below.
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	// exec.Command, NOT exec.CommandContext: CommandContext kills only the
	// direct child (and, since Go 1.20, cannot be combined with our own
	// process-group teardown cleanly). Contract 4 requires the whole group
	// to die, so the kill is driven by hand below.
	cmd := exec.Command(req.Path, req.Args...)
	cmd.Dir = req.Dir
	if len(req.Env) > 0 {
		// Layer the caller's entries ON TOP of the parent environment: later
		// duplicates win in every Unix exec implementation, so this is how a
		// caller overrides an inherited value (and how secrets are injected
		// without ever touching argv).
		cmd.Env = append(os.Environ(), req.Env...)
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	// Separate unbounded buffers: no interleaving, no truncation
	// (contracts 2 and 3).
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	// Put the child in its own process group so the kill below can reach
	// every descendant (headless Chromium, node helpers) — contract 4.
	setProcessGroup(cmd)

	// WaitDelay bounds the ONE way cmd.Wait can block forever here.
	//
	// Because Stdout/Stderr are io.Writers (not *os.File), os/exec hands the
	// child a PIPE and drains it in a helper goroutine; Wait does not return
	// until every write end of that pipe is closed. A grandchild that inherited
	// the descriptors and then escaped our process group — a browser helper
	// that calls setsid(2), which killProcessGroup's -pid signal cannot reach —
	// holds a write end open after the child itself is dead, and Wait blocks on
	// it with no deadline of any kind. That is not a slow batch: Provision holds
	// the single-flight lock for the whole run, so the feature would answer
	// ErrBusy until the process is restarted, and the deferred wipe of the
	// working dir (plaintext passwords, TOTP seeds, unarchived credentials)
	// would never run either.
	//
	// The timer starts only once Wait observes that the child has EXITED, so a
	// healthy run — where the pipes close with the child — never reaches it and
	// nothing here changes. When it does fire, Cmd closes the pipes itself and
	// Wait returns exec.ErrWaitDelay; Result still carries everything captured
	// up to that moment, which is what the salvage paths read. Ten seconds is
	// far longer than any legitimate post-exit flush and far shorter than
	// "forever".
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("subproc: start %s: %w", req.Path, err)
	}

	// cmd.Wait both reaps the child AND waits for the goroutines os/exec
	// spawned to drain our two buffers. Every read of outBuf/errBuf below
	// therefore happens strictly after Wait returned — that ordering is what
	// makes the buffers race-free.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	killed := false
	// Completion is checked FIRST, non-blockingly. When a child exits inside
	// the same scheduling window as the deadline both channels are ready, and
	// a plain two-case select picks at random — which would report a clean
	// exit-0 run as timed out. That misreport is not cosmetic: the m365 caller
	// treats a timeout as terminal, so a SUCCESSFUL login would be recorded as
	// login_failed and its account JSON wiped with the working directory.
	select {
	case waitErr = <-done:
	default:
		select {
		case <-runCtx.Done():
			killed = true
			killProcessGroup(cmd)
			// Still wait: killing the group closes every write end of the
			// capture pipes, so Wait returns promptly and the buffers settle.
			waitErr = <-done
		case waitErr = <-done:
		}
	}

	res.Stdout = outBuf.String()
	res.Stderr = errBuf.String()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	// The non-blocking check above narrows the deadline-vs-exit race but cannot
	// close it: cmd.Wait has to run to completion before `done` is ready, so a
	// child that exited microseconds before the deadline can still lose the
	// inner select's coin toss and be reported as killed. ProcessState settles
	// that authoritatively — Exited() is true ONLY for a process that ran to its
	// own exit(2), and false for one SIGKILLed by killProcessGroup (Signaled()).
	// So if the child exited on its own, it was never actually killed by us,
	// whatever the select decided.
	//
	// Reporting that misroute as a timeout is the exact data-loss the ordering
	// above exists to prevent: provisionM365 returns on ErrTimeout WITHOUT
	// looking for the account JSON, so a SUCCESSFUL login would be recorded
	// login_failed and its OAuth credential — plus any TOTP seed it just
	// enrolled — destroyed with the working dir. A parent-cancel misroute
	// (ctx.Err()) loses the same credential down the StatusError branch, so
	// both are suppressed. This also matches Python more closely: a script that
	// exits on its OWN --timeout yields a CompletedProcess with a non-zero
	// returncode there, never a TimeoutExpired.
	if killed && cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		killed = false
	}

	if killed {
		// A cancelled PARENT context outranks our own deadline: the caller
		// asked to stop, and reporting that as a timeout would send an idc
		// login down the "salvage the key" branch on a deliberate shutdown.
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.TimedOut = true
		return res, fmt.Errorf("subproc: %s: %w after %s", req.Path, ErrTimeout, req.Timeout)
	}

	// A non-zero exit surfaces as *exec.ExitError; per contract 5 that is
	// NOT an error here — it is data in Result.ExitCode. Anything else (an
	// I/O failure draining the pipes) is a genuine error the caller cannot
	// see any other way.
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return res, fmt.Errorf("subproc: %s: %w", req.Path, waitErr)
	}
	return res, nil
}

// ParseLastJSONLine decodes the single JSON object the helper scripts print
// on their LAST stdout line into out (a pointer, as for json.Unmarshal).
//
// This is the aws_idc.py:5-7 contract verbatim: "it runs this script in a
// subprocess and reads the single JSON object printed on the last stdout
// line". Everything before that line is human/diagnostic chatter (boto3
// warnings, progress prints) and is ignored by design, which is exactly why
// the helpers can stay chatty without breaking their callers.
//
// Trailing blank lines are skipped (a script's final print adds one). The
// last non-blank line must then BE the object: it is required to start with
// '{' and to parse. A line that merely CONTAINS JSON, or a run whose last
// line is a stray log message, is rejected with ErrNoJSONLine rather than
// searched backwards for something that parses — silently accepting an older
// line would let a helper that crashed after printing an early result be read
// as a success.
func ParseLastJSONLine(stdout string, out any) error {
	// Normalize CRLF: a helper run through a Windows-ish toolchain, or output
	// relayed through a pty, can carry \r\n, and a trailing \r makes the JSON
	// parse fail for a reason that has nothing to do with the payload.
	normalized := strings.ReplaceAll(stdout, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			last = s
			break
		}
	}
	if last == "" || !strings.HasPrefix(last, "{") {
		return ErrNoJSONLine
	}
	if err := json.Unmarshal([]byte(last), out); err != nil {
		return fmt.Errorf("%w: %v", ErrNoJSONLine, err)
	}
	return nil
}
