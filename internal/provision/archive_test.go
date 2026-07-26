package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testClock = time.Date(2026, 7, 15, 18, 3, 31, 0, time.UTC)

func TestArchiveRootFromEnv(t *testing.T) {
	// Read LAZILY: the bot loads .env AFTER this code is linked in, so an
	// init-time capture would silently ignore a .env override.
	t.Setenv("KIRO_ARCHIVE_DIR", "/var/lib/kiro-archive")
	if got := ArchiveRootFromEnv("/fallback"); got != "/var/lib/kiro-archive" {
		t.Errorf("got %q, want the env value", got)
	}
	t.Setenv("KIRO_ARCHIVE_DIR", "")
	if got := ArchiveRootFromEnv("/fallback"); got != "/fallback" {
		t.Errorf("got %q, want the fallback", got)
	}
}

func TestWritePrivateIsCreated0600(t *testing.T) {
	// These files hold plaintext passwords and TOTP seeds, so the mode must
	// never be widened — not even for the instant a chmod-after-write leaves
	// open.
	path := filepath.Join(t.TempDir(), "accounts-idc.txt")
	if err := writePrivate(path, "u|p|https://x/\n"); err != nil {
		t.Fatalf("writePrivate: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	body, _ := os.ReadFile(path)
	if string(body) != "u|p|https://x/\n" {
		t.Errorf("body = %q", body)
	}
	// A rewrite truncates rather than appending.
	if err := writePrivate(path, "short\n"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	body, _ = os.ReadFile(path)
	if string(body) != "short\n" {
		t.Errorf("rewrite left %q, want a truncating write", body)
	}
}

func TestRecordRewriteAppendsUnder0700Dir(t *testing.T) {
	root := t.TempDir()
	p1 := recordRewrite(root, "20260715-180331", "user.one", "mfa_secret", "SEED1", testClock)
	if p1 == "" {
		t.Fatal("recordRewrite returned no path")
	}
	// Several accounts in one run may each rewrite — the record APPENDS.
	p2 := recordRewrite(root, "20260715-180331", "user.two", "password", "pw2", testClock)
	if p1 != p2 {
		t.Errorf("second record went to %q, want the same run file %q", p2, p1)
	}
	body, err := os.ReadFile(p1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("record has %d lines, want 2 (appended, never truncated)", len(lines))
	}
	wantFirst := "2026-07-15 18:03:31\tuser.one\tmfa_secret\tSEED1"
	if lines[0] != wantFirst {
		t.Errorf("line 1 = %q, want %q", lines[0], wantFirst)
	}
	if !strings.HasSuffix(lines[1], "user.two\tpassword\tpw2") {
		t.Errorf("line 2 = %q", lines[1])
	}
	// The record holds a LIVE secret: 0600 file inside a 0700 dir.
	fi, _ := os.Stat(p1)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p1))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("run dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestRecordRewriteFailureIsNotFatal(t *testing.T) {
	// A rewrite that cannot be persisted must degrade to "" so the caller can
	// still REPORT the value, rather than taking the account down.
	root := filepath.Join(t.TempDir(), "file-not-a-dir")
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := recordRewrite(root, "tag", "u", "mfa_secret", "S", testClock); got != "" {
		t.Errorf("got %q, want \"\" when the archive dir cannot be created", got)
	}
}

func TestArchiveKeyJSONMovesTheCredential(t *testing.T) {
	work := t.TempDir()
	root := t.TempDir()
	src := filepath.Join(work, "kiro-api-key_user.one.json")
	if err := os.WriteFile(src, []byte(`{"api_key":"`+testKSK+`"}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	dest := archiveKeyJSON(src, root, "20260715-180331")
	if dest == "" {
		t.Fatal("archiveKeyJSON returned no path")
	}
	// MOVED, not copied: the live credential must no longer sit in the working
	// key dir.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after archive (err=%v)", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil || !strings.Contains(string(body), testKSK) {
		t.Errorf("archived body = %q (err %v)", body, err)
	}
	if di, _ := os.Stat(filepath.Dir(dest)); di.Mode().Perm() != 0o700 {
		t.Errorf("archive dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestArchiveKeyJSONSuffixesOnCollision(t *testing.T) {
	// A same-named archive means a retry of the same account in the same run
	// tag; suffix rather than clobber, because the earlier credential may still
	// be the live one.
	root := t.TempDir()
	var dests []string
	for i := 0; i < 3; i++ {
		work := t.TempDir()
		src := filepath.Join(work, "kiro-api-key_user.one.json")
		os.WriteFile(src, []byte(`{"n":`+string(rune('0'+i))+`}`), 0o600)
		d := archiveKeyJSON(src, root, "tag")
		if d == "" {
			t.Fatalf("archive %d failed", i)
		}
		dests = append(dests, d)
	}
	seen := map[string]bool{}
	for _, d := range dests {
		if seen[d] {
			t.Errorf("archive path %q reused — an earlier credential was clobbered", d)
		}
		seen[d] = true
	}
	if !strings.HasSuffix(dests[1], ".1.json") || !strings.HasSuffix(dests[2], ".2.json") {
		t.Errorf("collision suffixes = %v, want .1.json / .2.json", dests[1:])
	}
}

func TestArchiveKeyJSONFailureIsNotFatal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file-not-a-dir")
	os.WriteFile(root, []byte("x"), 0o600)
	work := t.TempDir()
	src := filepath.Join(work, "kiro-api-key_user.one.json")
	os.WriteFile(src, []byte("{}"), 0o600)

	// Non-fatal: by the time this runs the credential is already in the pool.
	if got := archiveKeyJSON(src, root, "tag"); got != "" {
		t.Errorf("got %q, want \"\" when the archive dir cannot be created", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("a failed archive destroyed the source: %v", err)
	}
}

func TestCopyFileFallbackKeeps0600(t *testing.T) {
	// moveFile's cross-filesystem branch. EXDEV can't be provoked portably and
	// a same-directory os.Rename always wins first, so copyFile is called
	// DIRECTLY — calling moveFile here would exercise the rename path and
	// assert nothing about the branch this test is named for.
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	dst := filepath.Join(dir, "dst.json")
	// A deliberately world-readable source: the copy must not carry that mode
	// across, because the file holds a live credential.
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if body, _ := os.ReadFile(dst); string(body) != "payload" {
		t.Errorf("body = %q", body)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("dst mode = %v, want 0600 — it holds a live credential", fi.Mode().Perm())
	}
	// The source is removed LAST, but it must be gone once the copy succeeded.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source survived the copy")
	}
	// moveFile still routes through the rename path for a same-dir move.
	src2, dst2 := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	os.WriteFile(src2, []byte("x"), 0o600)
	if err := moveFile(src2, dst2); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	if _, err := os.Stat(dst2); err != nil {
		t.Errorf("moveFile did not produce the destination: %v", err)
	}
}

func TestArchiveKeyJSONNormalizesTheMode(t *testing.T) {
	// os.Rename PRESERVES the source mode, so a login script that wrote its key
	// JSON 0644 would leave a world-readable live ksk_ key inside the archive.
	work, root := t.TempDir(), t.TempDir()
	src := filepath.Join(work, "kiro-api-key_user.one.json")
	if err := os.WriteFile(src, []byte(`{"api_key":"`+testKSK+`"}`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	dest := archiveKeyJSON(src, root, "tag")
	if dest == "" {
		t.Fatal("archive failed")
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("archived mode = %v, want 0600 (a live key must not stay world-readable)", fi.Mode().Perm())
	}
}

func TestArchiveKeyJSONCollisionLoopTerminatesOnStatError(t *testing.T) {
	// REGRESSION: the collision loop once broke only on os.IsNotExist, so any
	// other stat error (EACCES on the run dir, ENAMETOOLONG, EIO) made the
	// condition permanently false and spun forever — hanging Provision, which
	// in turn skipped the deferred working-dir wipe and left plaintext
	// credentials on disk with the single-flight lock held.
	//
	// EACCES is provoked by making the run dir untraversable: MkdirAll still
	// succeeds (the dir exists), but os.Stat on a path INSIDE it fails with a
	// permission error rather than ENOENT.
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the directory permission this test relies on")
	}
	work, root := t.TempDir(), t.TempDir()
	src := filepath.Join(work, "kiro-api-key_user.one.json")
	if err := os.WriteFile(src, []byte("{}"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	runDir := filepath.Join(root, "tag")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("setup run dir: %v", err)
	}
	if err := os.Chmod(runDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore the mode so t.TempDir's cleanup can remove the tree.
	t.Cleanup(func() { os.Chmod(runDir, 0o700) })

	// Confirm the premise: stat inside the dir must fail with something OTHER
	// than "not exists", which is precisely what the old loop condition
	// treated as "keep going".
	if _, err := os.Stat(filepath.Join(runDir, "probe.json")); err == nil || os.IsNotExist(err) {
		t.Skipf("this filesystem does not enforce directory traversal permissions (err=%v)", err)
	}

	done := make(chan string, 1)
	go func() { done <- archiveKeyJSON(src, root, "tag") }()
	select {
	case got := <-done:
		// Termination is the assertion; the archive itself cannot succeed
		// against an untraversable dir, and that failure must stay non-fatal.
		if got != "" {
			t.Errorf("archiveKeyJSON returned %q, want \"\" when the run dir is unusable", got)
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("a failed archive destroyed the source: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("archiveKeyJSON did not terminate — the collision loop is spinning")
	}
}

func TestFieldInFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts-idc.txt")
	body := "# a comment with no pipe\n" +
		"user.one|  pw with spaces  |https://x/| SEEDY \n" +
		"user.two|pw2|https://y/\n" +
		"  user.three  |pw3|https://z/\n"
	os.WriteFile(path, []byte(body), 0o600)

	// A password is read RAW (never stripped): it is handed back to the
	// operator as a live credential, so it must survive verbatim. Note the
	// whole LINE is trimmed first (Python's line.strip()), so only the
	// internal spaces remain.
	if got, ok := passwordInFile(path, "user.one"); !ok || got != "  pw with spaces  " {
		t.Errorf("password = %q (%v), want it untrimmed", got, ok)
	}
	// An MFA seed IS stripped, mirroring how the login script reads it.
	if got, ok := idcMFAInFile(path, "user.one"); !ok || got != "SEEDY" {
		t.Errorf("idc mfa = %q (%v), want the stripped seed", got, ok)
	}
	// Field 4 absent on a 3-field line.
	if got, ok := idcMFAInFile(path, "user.two"); ok {
		t.Errorf("idc mfa for a 3-field line = %q (%v), want absent", got, ok)
	}
	// The username column is compared trimmed.
	if got, ok := passwordInFile(path, "user.three"); !ok || got != "pw3" {
		t.Errorf("password for a padded username = %q (%v)", got, ok)
	}
	// Unknown user / missing file.
	if _, ok := passwordInFile(path, "nobody"); ok {
		t.Error("an unknown username matched")
	}
	if _, ok := passwordInFile(filepath.Join(dir, "nope.txt"), "user.one"); ok {
		t.Error("a missing file matched")
	}

	// The m365 twin reads a DIFFERENT index (field 3 of email|password|seed).
	m365Path := filepath.Join(dir, "accounts-m365.txt")
	os.WriteFile(m365Path, []byte("a@corp.example|pw| M365SEED \nb@corp.example|pw2\n"), 0o600)
	if got, ok := m365MFAInFile(m365Path, "a@corp.example"); !ok || got != "M365SEED" {
		t.Errorf("m365 mfa = %q (%v)", got, ok)
	}
	if _, ok := m365MFAInFile(m365Path, "b@corp.example"); ok {
		t.Error("a seedless m365 line reported a seed")
	}
}

func TestFieldInFileHandlesHugeLines(t *testing.T) {
	// REGRESSION: a bufio.Scanner silently stops at its token cap and its
	// Scan()==false is indistinguishable from "no such user" — which would
	// report "no rewrite" for an account whose TOTP seed was just enrolled,
	// losing it with the working dir. Python's line iterator has no cap.
	path := filepath.Join(t.TempDir(), "accounts.txt")
	huge := "noise|" + strings.Repeat("x", 8*1024*1024) + "\n"
	body := huge + "user.one|pw|https://x/|REALSEED\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got, ok := idcMFAInFile(path, "user.one"); !ok || got != "REALSEED" {
		t.Errorf("got %q (%v), want the seed found past an 8MiB line", got, ok)
	}
}

func TestFieldInFileFindsLastLineWithoutTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.txt")
	os.WriteFile(path, []byte("a|1|2|3\nuser.one|pw|https://x/|SEED"), 0o600)
	if got, ok := idcMFAInFile(path, "user.one"); !ok || got != "SEED" {
		t.Errorf("got %q (%v), want the unterminated last line read", got, ok)
	}
}

func TestFieldInFileSkipsShortMatchingLines(t *testing.T) {
	// Matching Python exactly: a matching line that is too short does NOT stop
	// the scan — the next matching line with enough fields wins.
	path := filepath.Join(t.TempDir(), "accounts.txt")
	os.WriteFile(path, []byte("user.one|pw\nuser.one|pw|https://x/|LATER\n"), 0o600)
	if got, ok := idcMFAInFile(path, "user.one"); !ok || got != "LATER" {
		t.Errorf("got %q (%v), want the scan to continue past the short line", got, ok)
	}
}
