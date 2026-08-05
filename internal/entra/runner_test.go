package entra

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codezdev-bot/internal/subproc"
)

// runner_test.go exercises the SHARED plumbing (runScript/decodeResult) that
// every operation in enterpriseapp.go and users.go is built on, directly and
// with short timeouts — the public per-operation functions each hardcode
// (or default to) multi-minute timeouts matching the Python original
// (py_entra_spec.md), which would make a "child that times out" test
// impossibly slow if driven through them. Since every operation shares this
// exact code path, these tests cover the cross-cutting behaviors ONCE:
// non-zero exit with a JSON line, no parseable line at all, a timeout with
// no output, a crash whose PARTIAL output (the JSON line, printed just
// before a kill) must still be usable, and secret redaction. Each
// operation's own happy path is still covered per-function in
// enterpriseapp_test.go / users_test.go.

func writeStubScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunScriptHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	script := writeStubScript(t, dir, "op.sh", `echo '{"ok":true,"x":1}'`)
	raw, err := runScript(context.Background(), "sh", script, nil, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("runScript: %v", err)
	}
	var out struct {
		OK bool `json:"ok"`
		X  int  `json:"x"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || out.X != 1 {
		t.Errorf("got %+v, want ok=true x=1", out)
	}
}

func TestRunScriptNonZeroExitWithJSONLineIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	// Mirrors main()'s `return 0 if out.get("ok") else 1` — an "ok": false
	// line still exits non-zero, and subproc.go contract 5 says that must
	// NOT surface as a Go error from runScript; decodeResult is what turns
	// "ok": false into an error, one layer up.
	script := writeStubScript(t, dir, "op.sh", `echo '{"ok":false,"error":"boom","step":"x"}'; exit 1`)
	raw, err := runScript(context.Background(), "sh", script, nil, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("runScript: %v, want nil (non-zero exit is data, not an error)", err)
	}
	if _, err := decodeResult[struct{}](raw, "op.sh"); err == nil {
		t.Fatal("decodeResult: got nil error, want the ok:false envelope surfaced as an error")
	} else if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "step=x") {
		t.Errorf("decodeResult error = %q, want it to contain the message and step", err.Error())
	}
}

func TestRunScriptNoParseableLineFallsBackToStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	script := writeStubScript(t, dir, "op.sh", `echo "not json at all" >&2; exit 1`)
	_, err := runScript(context.Background(), "sh", script, nil, nil, 5*time.Second, nil)
	if err == nil {
		t.Fatal("runScript: got nil error, want the stderr-derived error")
	}
	if !strings.Contains(err.Error(), "not json at all") {
		t.Errorf("runScript error = %q, want it to surface the child's stderr", err.Error())
	}
}

func TestRunScriptTimeoutWithNoOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	script := writeStubScript(t, dir, "op.sh", `sleep 5`)
	_, err := runScript(context.Background(), "sh", script, nil, nil, 100*time.Millisecond, nil)
	if err == nil {
		t.Fatal("runScript: got nil error, want a timeout error")
	}
	if !errors.Is(err, subproc.ErrTimeout) {
		t.Errorf("runScript error = %v, want errors.Is(err, subproc.ErrTimeout)", err)
	}
}

// TestRunScriptCrashedChildPartialOutputStillUsable pins that a child killed
// by OUR OWN timeout, but which had already flushed its one required JSON
// line beforehand, still yields a usable result — subproc retains captured
// output even on a timeout kill (subproc.go contracts 2/3), and runScript
// checks for a parseable line BEFORE looking at the run error, so a "crash"
// (here: killed for over-running) that nonetheless finished writing its
// answer is not thrown away.
func TestRunScriptCrashedChildPartialOutputStillUsable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	script := writeStubScript(t, dir, "op.sh", `echo '{"ok":true,"x":42}'; sleep 5`)
	raw, err := runScript(context.Background(), "sh", script, nil, nil, 200*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("runScript: %v, want the pre-kill JSON line to still be usable", err)
	}
	var out struct {
		X int `json:"x"`
	}
	if json.Unmarshal(raw, &out) != nil || out.X != 42 {
		t.Errorf("decoded %+v (raw=%s), want x=42", out, raw)
	}
}

// TestRunScriptRedactsPlantedSecretFromStderrError pins that a secret
// planted in the child's stderr never survives into the error runScript
// synthesizes when no JSON line was printed — this repo's fourth review
// round on exactly this leak class (task brief); it must not regress here.
func TestRunScriptRedactsPlantedSecretFromStderrError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	const secret = "ENTRA-SCIM-SUPER-SECRET-SHOULD-NOT-LEAK"
	dir := t.TempDir()
	script := writeStubScript(t, dir, "op.sh", `echo "traceback: token `+secret+` rejected" >&2; exit 1`)
	_, err := runScript(context.Background(), "sh", script, nil, nil, 5*time.Second, []string{secret})
	if err == nil {
		t.Fatal("runScript: got nil error, want the stderr-derived error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("runScript error = %q, still contains the planted secret", err.Error())
	}
}
