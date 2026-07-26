package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"codezdev-bot/internal/dbfile"
)

// TestHardenUmaskMakesNewFilesOwnerOnly — the umask is the backstop for code
// that does not exist yet: a future credential-writing path that forgets an
// explicit mode must still fail closed. This asserts the backstop actually
// works, using a 0666 create (what a careless writer would do) and checking
// the result is owner-only.
//
// Deliberately does NOT restore the previous umask: it applies to the whole
// process by design, and the assertions below only ever get stricter under it.
func TestHardenUmaskMakesNewFilesOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no umask on Windows; hardenUmask is a documented no-op there")
	}
	hardenUmask()

	p := filepath.Join(t.TempDir(), "careless.txt")
	if err := os.WriteFile(p, []byte("password"), 0o666); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("mode = %v after a 0666 create; the umask backstop is not in effect", fi.Mode().Perm())
	}
}

// TestTightenDataModesIsIdempotentAndSilent exercises the startup migration
// against a real, deliberately world-readable data layout at the exact
// relative paths main() uses.
//
// The count is asserted rather than just "no error" because the failure this
// guards against is the pass silently doing nothing — which is
// indistinguishable from success if you only check err.
func TestTightenDataModesIsIdempotentAndSilent(t *testing.T) {
	t.Chdir(t.TempDir())

	// A pre-fix install: loose dirs, loose credential file, loose database.
	active := filepath.Join(accountsDir, "p1", "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(active, "alice.txt")
	if err := os.WriteFile(cred, []byte("alice|pw|url|seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storeDBPath, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	// MkdirAll and WriteFile are umask-subtracted, and the umask test above may
	// already have tightened this process, so force the loose modes explicitly
	// — otherwise this test could pass without the migration doing anything.
	for _, p := range []string{accountsDir, filepath.Join(accountsDir, "p1"), active, ordersDir} {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{cred, storeDBPath} {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tightenDataModes()

	for _, p := range []string{accountsDir, filepath.Join(accountsDir, "p1"), active, cred, ordersDir, storeDBPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: mode %v still readable by group/other after the migration", p, fi.Mode().Perm())
		}
	}

	// Second run must find nothing: it runs on every boot.
	if n, err := dbfile.Tighten(storeDBPath); err != nil || n != 0 {
		t.Errorf("re-tightening the db: n=%d err=%v, want 0/nil", n, err)
	}
}
