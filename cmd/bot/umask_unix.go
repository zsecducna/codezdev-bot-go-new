//go:build unix

package main

import "syscall"

// hardenUmask sets the process umask to 0077, so every file and directory this
// process creates is owner-only unless code explicitly asks for more.
//
// This is defense in depth, NOT the mechanism. The real fix is the explicit
// 0600/0700 modes in internal/stock and internal/dbfile; a umask is
// process-global, invisible at the call site, and trivially lost in a
// refactor, so nothing may depend on it. What it buys is a safe default for
// code that does not exist yet: the next credential-writing path added to this
// bot fails CLOSED (owner-only) instead of silently shipping a world-readable
// password file, which is exactly how the ./accounts tree became exposed.
//
// Called first thing in main, before any file is opened. syscall.Umask is
// process-wide and inherited by children — the login subprocesses in
// internal/subproc write credential JSONs of their own, so they benefit too.
func hardenUmask() {
	syscall.Umask(0o077)
}
