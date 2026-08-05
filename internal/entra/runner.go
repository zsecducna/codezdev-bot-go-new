package entra

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"codezdev-bot/internal/subproc"
)

// opError is the generic {"ok": false, "error": "...", "step": "..."}
// envelope every entra_enterprise_app.py / entra_create_users.py mode can
// print — both from operation-specific early-return checks (e.g.
// do_generate's subname validation, entra_create_users.py:414-417) and from
// main()'s catch-all except clauses (GraphError / generic Exception)
// wrapping ANY unhandled failure (entra_enterprise_app.py:739-744,
// entra_create_users.py:1310-1315). "step" is present only for
// GraphError-derived failures; absent otherwise.
type opError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Step  string `json:"step"`
}

// decodeResult decodes one child's last-JSON-line payload into T when its
// "ok" key is true, or synthesizes a Go error from the opError envelope when
// it is false. A single script prints WILDLY different field sets depending
// on success vs. failure — e.g. CreateUsersNowResult's "error" key is an INT
// (an error COUNT) on success but is entirely absent from the failure
// envelope, which instead carries a STRING "error" (a message) — so
// decoding twice, gated on "ok", is what lets both shapes decode cleanly
// without a hand-rolled discriminated union.
func decodeResult[T any](raw json.RawMessage, scriptName string) (T, error) {
	var zero T
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return zero, fmt.Errorf("%s: malformed JSON line: %w", scriptName, err)
	}
	if !envelope.OK {
		var oe opError
		_ = json.Unmarshal(raw, &oe)
		msg := oe.Error
		if msg == "" {
			msg = "operation failed"
		}
		if oe.Step != "" {
			msg = fmt.Sprintf("%s (step=%s)", msg, oe.Step)
		}
		return zero, fmt.Errorf("%s: %s", scriptName, msg)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("%s: %w", scriptName, err)
	}
	return out, nil
}

// runScript executes `python scriptPath args...`, with env layered on top
// of the parent process environment (subproc's ONLY secret channel — see
// subproc.go contract 1; a secret must NEVER be placed in args), and returns
// the raw bytes of the single JSON object printed on the child's LAST
// stdout line.
//
// This reuses subproc.ParseLastJSONLine rather than reimplementing its
// contract (this package's brief) — the spec confirms all three
// Graph-touching Python scripts print exactly one JSON object as their only
// meaningful stdout (py_entra_spec.md "Subprocess contract (general)").
//
// When no such line exists AND the run itself reported no error (a non-zero
// exit is DATA, not an error — subproc.go contract 5), the child's stderr is
// folded into a synthesized error, redacted of every value in `secrets` and
// tail-trimmed to 300 chars — mirroring internal/awsidc's Discover/Provision
// exactly (discover.go:90-106, provision.go:71-87), since that raw text can
// otherwise reach an admin's chat verbatim (this repo's fourth review round
// on exactly this class of leak, per the task brief).
func runScript(ctx context.Context, python, scriptPath string, args []string, env []string, timeout time.Duration, secrets []string) (json.RawMessage, error) {
	res, runErr := subproc.Run(ctxOrBackground(ctx), subproc.Request{
		Path:    python,
		Args:    append([]string{scriptPath}, args...),
		Env:     env,
		Timeout: timeout,
	})
	var raw json.RawMessage
	if perr := subproc.ParseLastJSONLine(res.Stdout, &raw); perr == nil {
		return raw, nil
	}
	if runErr != nil {
		return nil, runErr
	}
	scriptName := filepath.Base(scriptPath)
	if stderr := strings.TrimSpace(res.Stderr); stderr != "" {
		return nil, fmt.Errorf("%s: %s", scriptName, tail(redactChildOutput(stderr, secrets...), 300))
	}
	return nil, fmt.Errorf("%s: %w", scriptName, subproc.ErrNoJSONLine)
}
