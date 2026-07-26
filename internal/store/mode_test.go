package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenCreatesOwnerOnlyDB pins the file mode of store.db and its WAL
// sidecars.
//
// Security warning, normal prose: this database holds every customer's
// cleartext API key (apikeys.go:18). At the SQLite driver's default mode, any
// local user on the host can read the whole table, and the -wal sidecar leaks
// the most recent rows the same way. This test fails the build if a future
// change drops the pre-create / tighten calls in Open.
func TestOpenCreatesOwnerOnlyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// The main file must be owner-only.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("store.db mode = %v, readable by group/other", fi.Mode().Perm())
	}
	// Any sidecar that exists must be too. They are checked conditionally
	// because whether the driver has materialised them by now is its business,
	// not this package's contract.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		si, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		if si.Mode().Perm()&0o077 != 0 {
			t.Errorf("store.db%s mode = %v, readable by group/other", suffix, si.Mode().Perm())
		}
	}
}

// TestOpenTightensPreExistingDB — every real deployment already has a
// world-readable store.db from an older build, and that is the case this fix
// actually has to cover.
func TestOpenTightensPreExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")

	// Create + close a real database, then loosen it the way an older build
	// would have left it.
	s0, err := Open(path)
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	s0.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("store.db mode = %v after reopen, want owner-only", fi.Mode().Perm())
	}
}
