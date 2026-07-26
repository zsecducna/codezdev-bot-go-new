package subproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shFixture writes `script` into a temp file and returns a Request that runs
// it with /bin/sh. Every test child in this package is a tiny shell script:
// hermetic (no network, no browser, no `go run`), instant, and available on
// every deploy target (architecture-go.md §7).
func shFixture(t *testing.T, script string) Request {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return Request{Path: "/bin/sh", Args: []string{path}}
}

func TestRunHappyPath(t *testing.T) {
	req := shFixture(t, "echo out-line; echo err-line >&2; exit 0")
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "out-line" {
		t.Errorf("stdout = %q, want %q", got, "out-line")
	}
	// Streams stay SEPARATE (package contract 2) — the MFA-seed recovery in
	// kiro_provision.py:833 scrapes stderr specifically.
	if got := strings.TrimSpace(res.Stderr); got != "err-line" {
		t.Errorf("stderr = %q, want %q", got, "err-line")
	}
	if res.ExitCode != 0 || !res.Success() {
		t.Errorf("ExitCode = %d, Success = %v; want 0, true", res.ExitCode, res.Success())
	}
	if res.TimedOut {
		t.Error("TimedOut set on a clean run")
	}
}

func TestRunNonZeroExitIsNotAnError(t *testing.T) {
	// Package contract 5 / Python subprocess.run(check=False): a non-zero
	// exit must NOT be an error, because kiro_provision.py:766 goes on to
	// look for the key JSON on disk regardless of the child's return code.
	req := shFixture(t, "echo partial; echo boom >&2; exit 7")
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("non-zero exit must not surface as an error, got %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Success() {
		t.Error("Success() true for exit 7")
	}
	// Output on a FAILED run is exactly what _tail_detail (kiro_provision.py:900)
	// reports for triage, so it must survive.
	if !strings.Contains(res.Stdout, "partial") || !strings.Contains(res.Stderr, "boom") {
		t.Errorf("output dropped on non-zero exit: out=%q err=%q", res.Stdout, res.Stderr)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	// The child writes a marker, backgrounds a long-lived GRANDCHILD that
	// would keep writing to the same marker file, then sleeps past the
	// deadline. If only the direct child were killed the grandchild would
	// survive and keep appending — package contract 4 (orphaned Chromium /
	// the wedged 3128 callback port, kiro_provision.py:79-87).
	dir := t.TempDir()
	marker := filepath.Join(dir, "alive.txt")
	script := "echo started\n" +
		"( while true; do echo tick >> " + marker + "; sleep 0.05; done ) &\n" +
		"sleep 30\n"
	req := shFixture(t, script)
	req.Timeout = 300 * time.Millisecond

	start := time.Now()
	res, err := Run(context.Background(), req)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !res.TimedOut {
		t.Error("Result.TimedOut not set")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run blocked %s past the 300ms deadline", elapsed)
	}
	// Partial output written BEFORE the kill must survive: the timed-out idc
	// login path (kiro_provision.py:744-752) reads it to decide whether a key
	// was salvaged.
	if !strings.Contains(res.Stdout, "started") {
		t.Errorf("partial stdout lost on timeout: %q", res.Stdout)
	}

	// The grandchild must be dead: sample the marker twice and require it to
	// have stopped growing.
	size := func() int64 {
		fi, statErr := os.Stat(marker)
		if statErr != nil {
			return -1
		}
		return fi.Size()
	}
	first := size()
	time.Sleep(400 * time.Millisecond)
	if second := size(); second != first {
		t.Errorf("grandchild survived the group kill: marker grew %d -> %d", first, second)
	}
}

func TestRunParentCancelIsNotReportedAsTimeout(t *testing.T) {
	// A deliberate shutdown must NOT look like a timeout: the idc path treats
	// a timeout as "salvage the key that may already be on disk"
	// (kiro_provision.py:744-752), which is the wrong branch for a cancel.
	req := shFixture(t, "sleep 30")
	req.Timeout = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	res, err := Run(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrTimeout) || res.TimedOut {
		t.Error("a cancelled parent context was reported as a timeout")
	}
}

func TestRunSecretsTravelInEnvNotArgv(t *testing.T) {
	// aws_idc.py:8-13 — "secrets NEVER on argv". The child prints its own
	// argv and the injected secret's env value; the assertion is that the
	// secret is reachable through the environment and absent from argv.
	const secret = "SECRET-AKIA-VALUE"
	req := shFixture(t, `printf 'argv:%s\n' "$*"; printf 'env:%s\n' "$AWS_SECRET_ACCESS_KEY"`)
	req.Args = append(req.Args, "--profile", "kiro-idc")
	req.Env = []string{"AWS_SECRET_ACCESS_KEY=" + secret}

	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "env:"+secret) {
		t.Errorf("secret did not reach the child env: %q", res.Stdout)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "argv:") && strings.Contains(line, secret) {
			t.Errorf("secret leaked onto argv: %q", line)
		}
	}
	// Env must LAYER on the parent environment, not replace it: the login
	// scripts still need PATH/HOME to find their interpreter and browser
	// profile dir.
	req2 := shFixture(t, `printf 'path:%s\n' "$PATH"`)
	req2.Env = []string{"EXTRA=1"}
	res2, err := Run(context.Background(), req2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(res2.Stdout), "path:")) == "" {
		t.Error("Request.Env replaced the parent environment instead of layering onto it")
	}
}

func TestRunLargeOutputNotTruncated(t *testing.T) {
	// Package contract 3. A chatty login can far exceed a pipe buffer; any
	// cap here would silently drop the stderr line carrying a freshly
	// enrolled TOTP seed (kiro_provision.py:128-131).
	const lines = 20000
	script := "i=0; while [ $i -lt " + itoa(lines) + " ]; do " +
		"echo 0123456789012345678901234567890123456789; " +
		"echo 0123456789012345678901234567890123456789 >&2; " +
		"i=$((i+1)); done"
	req := shFixture(t, script)
	req.Timeout = 60 * time.Second

	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Count(res.Stdout, "\n"); got != lines {
		t.Errorf("stdout lines = %d, want %d (truncated)", got, lines)
	}
	if got := strings.Count(res.Stderr, "\n"); got != lines {
		t.Errorf("stderr lines = %d, want %d (truncated)", got, lines)
	}
}

func TestRunStartFailure(t *testing.T) {
	res, err := Run(context.Background(), Request{Path: "/definitely/not/a/real/binary"})
	if err == nil {
		t.Fatal("expected an error for a missing executable")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a process that never started", res.ExitCode)
	}
}

func TestRunRejectsEmptyPath(t *testing.T) {
	if _, err := Run(context.Background(), Request{}); err == nil {
		t.Fatal("expected an error for an empty Path")
	}
}

func TestRunStdin(t *testing.T) {
	req := shFixture(t, "cat")
	req.Stdin = []byte("piped-payload\n")
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "piped-payload" {
		t.Errorf("stdout = %q, want the piped payload", res.Stdout)
	}
}

// ── ParseLastJSONLine (the aws_idc.py:5-7 output contract) ───────────────────

type idcVerify struct {
	OK    bool   `json:"ok"`
	Users int    `json:"users"`
	Error string `json:"error"`
}

func TestParseLastJSONLineIgnoresLeadingChatter(t *testing.T) {
	// Realistic aws_idc.py shape: boto3/diagnostic chatter, then the single
	// JSON object as the final line, then the trailing newline print adds.
	stdout := "scanning us-east-1...\n" +
		"WARNING: region eu-west-1 disabled\n" +
		`{"ok": true, "account": "123456789012", "users": 42, "error": null}` + "\n"
	var got idcVerify
	if err := ParseLastJSONLine(stdout, &got); err != nil {
		t.Fatalf("ParseLastJSONLine: %v", err)
	}
	if !got.OK || got.Users != 42 {
		t.Errorf("parsed %+v, want ok=true users=42", got)
	}
}

func TestParseLastJSONLineSkipsTrailingBlankLines(t *testing.T) {
	stdout := `{"ok": true, "users": 3}` + "\n\n   \n\n"
	var got idcVerify
	if err := ParseLastJSONLine(stdout, &got); err != nil {
		t.Fatalf("ParseLastJSONLine: %v", err)
	}
	if got.Users != 3 {
		t.Errorf("users = %d, want 3", got.Users)
	}
}

func TestParseLastJSONLineCRLF(t *testing.T) {
	stdout := "chatter\r\n" + `{"ok": true, "users": 1}` + "\r\n"
	var got idcVerify
	if err := ParseLastJSONLine(stdout, &got); err != nil {
		t.Fatalf("ParseLastJSONLine: %v", err)
	}
	if !got.OK {
		t.Error("CRLF-terminated JSON line failed to parse")
	}
}

func TestParseLastJSONLineRejections(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
	}{
		{"empty output", ""},
		{"only whitespace", "  \n\n \t\n"},
		{"no JSON at all", "boto3 unavailable\ntraceback...\n"},
		// A crash AFTER the result line must not read as a success — the last
		// line is what counts, and this parser never searches backwards.
		{"JSON followed by a log line", `{"ok": true}` + "\nERROR: cleanup failed\n"},
		{"malformed JSON object", `{"ok": true,` + "\n"},
		// A bare array/string is not the documented "single JSON OBJECT".
		{"JSON array not object", `[{"ok": true}]` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got idcVerify
			err := ParseLastJSONLine(tc.stdout, &got)
			if !errors.Is(err, ErrNoJSONLine) {
				t.Errorf("err = %v, want ErrNoJSONLine", err)
			}
		})
	}
}

func TestRunThenParseLastJSONLine(t *testing.T) {
	// End-to-end over a real child, the way _run_aws_idc drives aws_idc.py:
	// chatter to stdout, the JSON object last, exit 0 always (aws_idc.py:23).
	req := shFixture(t, `echo "probing..."; echo '{"ok": true, "users": 9}'; exit 0`)
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got idcVerify
	if err := ParseLastJSONLine(res.Stdout, &got); err != nil {
		t.Fatalf("ParseLastJSONLine: %v", err)
	}
	if !got.OK || got.Users != 9 {
		t.Errorf("parsed %+v, want ok=true users=9", got)
	}
}

// itoa avoids a strconv import for the one place a test builds shell source.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
