package stock

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// mkTree builds a small accounts-shaped tree with deliberately loose modes,
// standing in for an install created by a pre-fix build.
func mkTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	active := filepath.Join(root, "p1", "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	// MkdirAll is umask-subtracted, so assert the loose mode explicitly —
	// otherwise this test would pass on a host whose umask already did the job.
	for _, d := range []string{root, filepath.Join(root, "p1"), active} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"alice.txt", "bob.txt"} {
		p := filepath.Join(active, name)
		if err := os.WriteFile(p, []byte("u|p|url|seed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestTightenModesFixesLegacyTree — the migration must actually re-mode files
// written by an older build, because the new constants only govern new writes.
func TestTightenModesFixesLegacyTree(t *testing.T) {
	root := mkTree(t)
	n, err := TightenModes(root)
	if err != nil {
		t.Fatalf("TightenModes: %v", err)
	}
	// root + p1 + active + 2 files.
	if n != 5 {
		t.Errorf("tightened %d entries, want 5", n)
	}
	assertOwnerOnlyTree(t, root)
}

// TestTightenModesIdempotent — it runs on every boot, so a second pass must
// find nothing to do. A non-zero count here would mean the pass is churning
// inodes (and logging) forever.
func TestTightenModesIdempotent(t *testing.T) {
	root := mkTree(t)
	if _, err := TightenModes(root); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	n, err := TightenModes(root)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Errorf("second pass tightened %d entries, want 0 — the pass is not idempotent", n)
	}
}

// TestTightenModesNeverWidens — an operator (or a paranoid deploy) may have
// hand-set a file to 0400 or a directory to 0500. Subtracting bits rather than
// assigning a fixed mode is what keeps those untouched; assigning 0600/0700
// would silently hand back write permission.
func TestTightenModesNeverWidens(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "locked")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	readonly := filepath.Join(sub, "cred.txt")
	if err := os.WriteFile(readonly, []byte("secret"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0o400); err != nil {
		t.Fatal(err)
	}
	// 0500 last: the directory must still be traversable for the walk, but not
	// writable.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's cleanup cannot unlink out of a 0500 directory; restore write
	// permission after the assertions so the test does not fail on teardown.
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	if n, err := TightenModes(root); err != nil || n != 0 {
		t.Fatalf("TightenModes on an already-tight tree: n=%d err=%v, want 0/nil", n, err)
	}
	if fi, _ := os.Stat(readonly); fi.Mode().Perm() != 0o400 {
		t.Errorf("file mode = %v, want 0400 unchanged — the pass widened a tighter mode", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(sub); fi.Mode().Perm() != 0o500 {
		t.Errorf("dir mode = %v, want 0500 unchanged — the pass widened a tighter mode", fi.Mode().Perm())
	}
}

// TestTightenModesMissingRoot — a fresh install has no ./accounts yet, and
// that must not be reported as a failure at every boot.
func TestTightenModesMissingRoot(t *testing.T) {
	n, err := TightenModes(filepath.Join(t.TempDir(), "nope"))
	if err != nil || n != 0 {
		t.Fatalf("missing root: n=%d err=%v, want 0/nil", n, err)
	}
	if n, err := TightenModes(""); err != nil || n != 0 {
		t.Fatalf("empty root: n=%d err=%v, want 0/nil", n, err)
	}
}

// TestTightenModesDoesNotFollowSymlinks — os.Chmod follows symlinks, so a link
// planted inside the data root would otherwise let the migration re-mode an
// arbitrary file elsewhere on the host that the bot's uid happens to own.
func TestTightenModesDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; the bot targets unix")
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "victim.txt")
	if err := os.WriteFile(outside, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := TightenModes(root); err != nil {
		t.Fatalf("TightenModes: %v", err)
	}
	fi, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("symlink target mode = %v, want 0644 untouched — the pass chmod'd through a symlink",
			fi.Mode().Perm())
	}
}
