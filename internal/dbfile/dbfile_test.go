package dbfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestPrecreateNewFileIsOwnerOnly — the whole point of pre-creating is that
// the database never exists at a looser mode, not even briefly, so the file
// must come out 0600 on a fresh install.
func TestPrecreateNewFileIsOwnerOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	if err := Precreate(p); err != nil {
		t.Fatalf("Precreate: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if fi.Mode().Perm() != Mode {
		t.Errorf("mode = %v, want %v", fi.Mode().Perm(), Mode)
	}
}

// TestPrecreateTightensExistingFile — every real deployment already has a
// store.db written by an older build at 0644. O_CREATE's perm argument does
// nothing for an existing file, so Precreate must chmod it as well or the
// installs that actually hold customer data stay exposed.
func TestPrecreateTightensExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(p, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil { // defeat any ambient umask
		t.Fatal(err)
	}
	if err := Precreate(p); err != nil {
		t.Fatalf("Precreate: %v", err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != Mode {
		t.Errorf("mode = %v, want %v", fi.Mode().Perm(), Mode)
	}
	// Pre-creating must never truncate a real database.
	if b, _ := os.ReadFile(p); string(b) != "sqlite" {
		t.Errorf("content = %q, want the existing database preserved", string(b))
	}
}

// TestPrecreateSkipsNonFileDSN — a ":memory:" or already-assembled "file:"
// DSN has no path to create, and turning that into an error (or into a stray
// file literally named ":memory:") would break the caller.
func TestPrecreateSkipsNonFileDSN(t *testing.T) {
	for _, dsn := range []string{"", ":memory:", "file:x?_pragma=busy_timeout(1)"} {
		if err := Precreate(dsn); err != nil {
			t.Errorf("Precreate(%q) = %v, want nil", dsn, err)
		}
		if dsn != "" {
			if _, err := os.Stat(dsn); err == nil {
				t.Errorf("Precreate(%q) created a file", dsn)
				os.Remove(dsn)
			}
		}
	}
}

// TestTightenCoversSidecars — the WAL and SHM files are created by the driver
// on the first write, AFTER Precreate has run, and they contain committed rows
// that have not been checkpointed yet: the same cleartext keys as the database.
// Tightening only the main file would leave the newest secrets readable.
func TestTightenCoversSidecars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "store.db")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.WriteFile(p+suffix, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Tighten(p)
	if err != nil {
		t.Fatalf("Tighten: %v", err)
	}
	if n != 4 {
		t.Errorf("tightened %d files, want 4 (db + wal + shm + journal)", n)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		fi, err := os.Stat(p + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s%s: mode %v still readable by group/other", "store.db", suffix, fi.Mode().Perm())
		}
	}
}

// TestTightenIdempotentAndMissingOK — Tighten runs on every Open, so a second
// call must report no work, and absent sidecars (a brand-new database) must
// not be an error.
func TestTightenIdempotentAndMissingOK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := Tighten(p); err != nil || n != 1 {
		t.Fatalf("first Tighten: n=%d err=%v, want 1/nil (sidecars absent is fine)", n, err)
	}
	if n, err := Tighten(p); err != nil || n != 0 {
		t.Fatalf("second Tighten: n=%d err=%v, want 0/nil", n, err)
	}
}

// TestTightenNeverWidens — subtracting the group/other bits rather than
// assigning 0600 is what keeps a hand-hardened 0400 database file at 0400.
func TestTightenNeverWidens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(p, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if n, err := Tighten(p); err != nil || n != 0 {
		t.Fatalf("Tighten: n=%d err=%v, want 0/nil", n, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o400 {
		t.Errorf("mode = %v, want 0400 unchanged", fi.Mode().Perm())
	}
}

// TestTightenDoesNotFollowSymlinks — os.Chmod follows symlinks, so a sidecar
// path replaced by a link would otherwise redirect the chmod at an arbitrary
// file the bot's uid owns.
func TestTightenDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; the bot targets unix")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "store.db")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, p+"-wal"); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := Tighten(p); err != nil {
		t.Fatalf("Tighten: %v", err)
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o644 {
		t.Errorf("symlink target mode = %v, want 0644 untouched", fi.Mode().Perm())
	}
}
