package stock

import (
	"io/fs"
	"os"
	"path/filepath"
)

// TightenModes is the one-shot migration for installs created by a build that
// wrote this tree at 0644/0755. New credentialFileMode / credentialDirMode
// constants only govern files created FROM NOW ON; every account file already
// on disk keeps the world-readable mode it was written with, which on a
// long-running shop is the entire inventory. This walks `root` and removes the
// group and other permission bits from everything under it, returning how many
// entries were actually changed.
//
// Safe to call on every boot, and it is: the walk is idempotent (an entry that
// is already owner-only is skipped, so a second run reports 0), it NEVER
// widens a mode — it subtracts bits from whatever the entry already has rather
// than assigning a fixed value, so a hand-tightened 0400 file stays 0400 — and
// it never follows a symlink, so a link planted inside ./accounts cannot be
// used to re-mode a file elsewhere on the host.
//
// A missing root is not an error: a fresh install has no ./accounts yet.
// Per-entry errors are counted but do not abort the walk; one unreadable
// subdirectory must not leave the rest of the tree exposed. The FIRST such
// error is returned so the caller can log that something went wrong, without
// naming the entry — a path under ./accounts is an account username.
func TightenModes(root string) (int, error) {
	if root == "" {
		return 0, nil
	}
	if _, err := os.Lstat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	// filepath.WalkDir reads directory entries with Lstat semantics and does
	// not descend into symlinked directories, which is exactly the containment
	// this pass needs.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Returning nil (not the error) keeps the walk going past an
			// unreadable entry; SkipDir would abandon a whole product folder.
			return nil
		}
		// os.Chmod FOLLOWS symlinks, so chmod-ing a link would re-mode its
		// target — possibly a file outside the data root. Skip links entirely.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if firstErr == nil {
				firstErr = ierr
			}
			return nil
		}
		perm := info.Mode().Perm()
		if perm&0o077 == 0 {
			return nil // already owner-only
		}
		if cerr := os.Chmod(path, perm&^0o077); cerr != nil {
			if firstErr == nil {
				firstErr = cerr
			}
			return nil
		}
		n++
		return nil
	})
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return n, firstErr
}
