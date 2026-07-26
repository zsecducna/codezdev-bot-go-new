//go:build !unix

package subproc

import "os/exec"

// setProcessGroup is a no-op on non-Unix platforms: SysProcAttr.Setpgid does
// not exist there. The bot deploys to Linux (systemd) and is developed on
// macOS (launchd) — see architecture-go.md §7 "Runtime shape" — so the real
// implementation lives in pgroup_unix.go. This file exists only so the
// package still compiles for a cross-platform `go build ./...`.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing the direct child only. Grandchildren
// (a headless Chromium) may survive; that degradation is accepted because no
// supported deployment target uses this build.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
