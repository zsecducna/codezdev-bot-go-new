//go:build !unix

package main

// hardenUmask is a no-op on non-Unix platforms: syscall.Umask does not exist
// there (Windows has no umask, and permissions are ACL-based). The bot deploys
// to Linux and is developed on macOS, so the real implementation lives in
// umask_unix.go; this file exists only so a cross-platform `go build ./...`
// still compiles, matching the split in internal/subproc (pgroup_other.go).
//
// The explicit 0600/0700 modes in internal/stock and internal/dbfile are
// unaffected by this build — they are the actual protection, and they are
// portable.
func hardenUmask() {}
