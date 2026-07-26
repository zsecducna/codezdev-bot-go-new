// Package dbfile owns the on-disk permissions of the bot's SQLite files.
//
// Security warning, normal prose: store.db holds every customer's CLEARTEXT
// API key (store/apikeys.go:18 — "cleartext key — the buyer's own secret") and
// awssell.db holds customer-supplied IAM passwords and AWS secret access keys
// (awssell.go:24-29 already flags this and asks that the file's permissions be
// kept tight on the host). The SQLite driver creates a database file with
// whatever the process umask allows, which under a standard umask is 0644 —
// world-readable. The WAL and SHM sidecars are created by the driver later, on
// first write, and inherit the same loose mode, so tightening only the main
// file is not enough.
//
// This package exists because THREE packages open a credential-bearing SQLite
// file the same way (internal/store, internal/awssell, internal/refund) and a
// per-package copy of the rule is exactly how one of them ends up skipped.
package dbfile

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Mode is the permission every SQLite file this bot owns is created with, and
// the ceiling its sidecars are held to: owner-only read/write.
const Mode os.FileMode = 0o600

// sidecarSuffixes are the files SQLite creates NEXT TO the database, each of
// which can contain committed-but-not-yet-checkpointed rows — i.e. the same
// cleartext secrets as the database itself. `-wal`/`-shm` come from
// journal_mode=WAL (the mode every Open in this repo sets); `-journal` is the
// rollback journal a non-WAL fallback would leave behind.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// Precreate creates `path` with 0600 BEFORE sql.Open is called, so the
// database file never exists at a looser mode — not even for the instant a
// chmod-after-open would leave open. An existing file is not truncated, only
// re-moded; SQLite is then free to open it normally.
//
// A non-file DSN (":memory:", or an already-assembled "file:..." URI) has no
// path to pre-create and is skipped rather than treated as an error.
func Precreate(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, Mode)
	if err != nil {
		return fmt.Errorf("dbfile: pre-create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("dbfile: pre-create %s: %w", path, err)
	}
	// An existing file kept its old, possibly world-readable mode above
	// (O_CREATE's perm applies only on creation), so assert the mode rather
	// than assume it.
	if _, err := tightenPath(path); err != nil {
		return fmt.Errorf("dbfile: pre-create %s: %w", path, err)
	}
	return nil
}

// Tighten strips every group and other permission bit from the database file
// and its sidecars, returning how many files were actually changed.
//
// Called AFTER schema init, because that is the first write and therefore the
// moment the -wal / -shm sidecars come into existence at the driver's default
// mode. Missing files are not an error: a read-only or freshly-created DB may
// have no sidecars yet.
func Tighten(path string) (int, error) {
	if path == "" || path == ":memory:" {
		return 0, nil
	}
	n := 0
	var firstErr error
	for _, p := range append([]string{path}, sidecarPaths(path)...) {
		changed, err := tightenPath(p)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if changed {
			n++
		}
	}
	return n, firstErr
}

// sidecarPaths lists the companion files SQLite may have created for `path`.
func sidecarPaths(path string) []string {
	out := make([]string, 0, len(sidecarSuffixes))
	for _, suf := range sidecarSuffixes {
		out = append(out, path+suf)
	}
	return out
}

// tightenPath chmods one file to owner-only, reporting whether it changed. A
// file that does not exist is a no-op, not a failure.
func tightenPath(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// Never chmod through a symlink: os.Chmod follows it, so a symlink planted
	// here would let an attacker re-mode an arbitrary file the bot's uid owns.
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return false, nil // already owner-only; keeps this idempotent
	}
	return true, tightenOne(p, fi)
}

// tightenOne clears the group/other bits of one path, PRESERVING the owner
// bits. Subtracting bits rather than assigning a fixed 0600 is what guarantees
// this can never widen a file that is already tighter than the target.
func tightenOne(p string, fi fs.FileInfo) error {
	if fi == nil {
		var err error
		fi, err = os.Lstat(p)
		if err != nil {
			return err
		}
	}
	return os.Chmod(p, fi.Mode().Perm()&^0o077)
}
