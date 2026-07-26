//go:build unix

package subproc

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child into its OWN process group (pgid == child
// pid) so killProcessGroup can signal the entire subtree at once.
//
// Package contract 4: the login scripts (kiro_provision.py:75-77) launch a
// headless Chromium via Playwright/CloakBrowser and, in some paths, node
// helpers. Those are grandchildren of this process. Killing only the direct
// Python child on a timeout leaves them running — holding memory, a browser
// profile lock, and (for the M365 flow) the fixed loopback callback port
// 3128 that kiro_provision.py:79-87 documents as unmovable, which wedges
// every subsequent m365 batch on the host.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the child's whole process group.
//
// A negative pid addresses the process GROUP in kill(2); because
// setProcessGroup made pgid == child pid, -pid is exactly the group we
// created. SIGKILL rather than SIGTERM: this only ever runs after the
// child's own, more generous internal deadline has already been given a
// chance to expire cleanly (kiro_provision.py:94-97 sizes SUBPROC_GRACE_S
// for precisely that), so at this point the child is unresponsive by
// definition and a catchable signal would just add another wait.
//
// The single-process fallback covers the narrow race where the child has
// exited but its group is gone while a grandchild survived reparenting.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
