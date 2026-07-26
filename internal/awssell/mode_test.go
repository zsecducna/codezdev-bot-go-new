package awssell

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenCreatesOwnerOnlyDB makes the package doc's SECURITY NOTE
// (awssell.go:24-29) enforceable instead of advisory.
//
// Security warning, normal prose: the rows in this database are
// customer-supplied AWS IAM passwords and secret access keys — live
// credentials for accounts the shop is buying. Left at the driver's default
// mode, any local user on the host can read them, and so can anything that
// gets a foothold as another service account.
func TestOpenCreatesOwnerOnlyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "awssell.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("awssell.db mode = %v, readable by group/other", fi.Mode().Perm())
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		si, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		if si.Mode().Perm()&0o077 != 0 {
			t.Errorf("awssell.db%s mode = %v, readable by group/other", suffix, si.Mode().Perm())
		}
	}
}

// TestOpenTightensPreExistingDB covers the upgrade case: a database already on
// disk from a build that set no mode.
func TestOpenTightensPreExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "awssell.db")
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
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("awssell.db mode = %v after reopen, want owner-only", fi.Mode().Perm())
	}
}
