package provision

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── credential archival & login-rewrite capture ──────────────────────────────
//
// Everything in this file exists to keep a live credential from either
// (a) lingering in the working directory after it is safely in the pool, or
// (b) being destroyed when the working directory is wiped.
//
// The working dir holds the plaintext accounts files (usernames, passwords,
// TOTP seeds) and every not-yet-archived credential JSON, and it is removed
// unconditionally when a batch ends (kiro_provision.py:1021-1025). So for the
// two values a login can REWRITE — a reset password and a newly enrolled TOTP
// seed — the diff performed here is the only chance to keep them. A lost
// password can be reset again; a lost TOTP seed for an already-enrolled device
// CANNOT, and locks the account out of every future sign-in with no recovery
// path (kiro_provision.py:619-631).

// archiveDirMode / credentialFileMode are the permissions every artifact in
// this file is created with. 0700/0600 from CREATION, never widened even
// briefly (kiro_provision.py:635, 638, 1029-1031) — these paths hold plaintext
// passwords, TOTP seeds and live API keys, and the tests pin the 0600 mode
// explicitly (test_provision.py:313, 435).
const (
	archiveDirMode     os.FileMode = 0o700
	credentialFileMode os.FileMode = 0o600
	rewriteRecordName              = "credential-rewrites.txt"
	rewriteTimeLayout              = "2006-01-02 15:04:05"
)

// ArchiveRootFromEnv resolves where archived key JSONs and rewrite records
// land. Port of archive_root (kiro_provision.py:107-113).
//
// KIRO_ARCHIVE_DIR is read LAZILY, at call time — never captured at package
// init — because the bot loads .env into the environment AFTER importing this
// code, so an init-time capture would silently ignore a .env override.
// `fallback` is the compiled-in default (a gitignored dir next to the binary).
func ArchiveRootFromEnv(fallback string) string {
	if v := os.Getenv("KIRO_ARCHIVE_DIR"); v != "" {
		return v
	}
	return fallback
}

// writePrivate writes `text` to `path` with 0600 FROM CREATION. Port of
// _write_private (kiro_provision.py:1029-1033). O_CREATE|O_TRUNC|O_WRONLY with
// the mode passed to Open is what makes the file never exist world-readable,
// not even for the instant a chmod-after-write would leave open.
func writePrivate(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, credentialFileMode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// recordRewrite persists a login-rewritten credential to the run's archive dir
// the MOMENT it is observed, returning the file path (or "" if the write
// failed). Port of _record_rewrite (kiro_provision.py:619-645).
//
// The caller DOES also report the value (Telegram / stdout), but that report
// can fail — and a newly enrolled TOTP seed that is never captured leaves the
// account MFA-locked with no recovery path.
//
// It lands in the archive dir (0700, gitignored) rather than in a log on
// purpose: this is the established home for live credentials, whereas the
// system journal is persistent, root-readable and often shipped off-box.
//
// The record is APPENDED, never truncated: several accounts in one run may
// each rewrite.
func recordRewrite(archiveRoot, runTag, username, field, value string, now time.Time) string {
	destDir := filepath.Join(archiveRoot, runTag)
	if err := os.MkdirAll(destDir, archiveDirMode); err != nil {
		return ""
	}
	path := filepath.Join(destDir, rewriteRecordName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, credentialFileMode)
	if err != nil {
		return ""
	}
	line := fmt.Sprintf("%s\t%s\t%s\t%s\n", now.Format(rewriteTimeLayout), username, field, value)
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return ""
	}
	if err := f.Close(); err != nil {
		return ""
	}
	return path
}

// archiveKeyJSON MOVES a delivered credential JSON into the per-run archive
// dir (0700), so the live credential no longer sits in the working key dir.
// Returns the archived path, or "" if the move failed — non-fatal, because by
// the time this runs the credential is already in the pool. Port of
// _archive_key_json (kiro_provision.py:648-666).
//
// Called ONLY after a successful pool add (or a dry run). A FAILED add
// deliberately leaves the JSON in place so the operator can retry without
// re-running the multi-minute browser login (kiro_provision.py:48-50).
func archiveKeyJSON(path, archiveRoot, runTag string) string {
	destDir := filepath.Join(archiveRoot, runTag)
	if err := os.MkdirAll(destDir, archiveDirMode); err != nil {
		return ""
	}
	dest := filepath.Join(destDir, filepath.Base(path))
	// A same-named archive already there means a retry of the same account in
	// the same run tag; suffix with a counter rather than clobbering the
	// earlier credential, which may still be the live one.
	base, ext := strings.TrimSuffix(dest, filepath.Ext(dest)), filepath.Ext(dest)
	for n := 1; ; n++ {
		// Break on ANY stat error, not just IsNotExist. Python's
		// `while os.path.exists(dest)` returns False for every error, so
		// stopping only on ENOENT would spin forever on EACCES /
		// ENAMETOOLONG / EIO — and because this runs inside Provision, that
		// hang would also skip the deferred working-dir wipe, leaving the
		// plaintext accounts file and every unarchived credential on disk
		// while the single-flight lock stays held until restart.
		if _, err := os.Stat(dest); err != nil {
			break
		}
		dest = fmt.Sprintf("%s.%d%s", base, n, ext)
	}
	if err := moveFile(path, dest); err != nil {
		return ""
	}
	// Normalize the mode: os.Rename PRESERVES whatever the login script gave
	// the JSON, which may be 0644 — a world-readable live ksk_ key or OAuth
	// refresh token, protected only by the 0700 archive dir. Python's
	// shutil.move has the same gap; closing it here costs one syscall.
	// Non-fatal: the credential is already safely in the pool by this point.
	_ = os.Chmod(dest, credentialFileMode)
	return dest
}

// moveFile renames src to dst, falling back to copy+remove when the two are on
// different filesystems (os.Rename returns EXDEV). Python's shutil.move
// handles that transparently; KIRO_ARCHIVE_DIR pointing at another mount than
// the temp working dir is a perfectly ordinary deployment, so the fallback is
// load-bearing rather than defensive.
//
// The destination is created 0600 so the credential is never briefly readable
// during the copy.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// copyFile is moveFile's cross-filesystem branch, split out so it can be
// exercised directly: EXDEV cannot be provoked portably in a test, and a
// same-directory rename would always win before reaching this code.
//
// The destination is created 0600 so the credential is never briefly readable
// during the copy.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, credentialFileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	// Remove the source LAST: until it is gone the credential exists twice,
	// which is safe; removing it first and then failing would lose it.
	return os.Remove(src)
}

// fieldInFile returns field `index` of `username`'s line in a pipe-delimited
// accounts file, reporting ok=false when the file, the line, or the field is
// absent. Port of _field_in_file (kiro_provision.py:1036-1055).
//
// Used to diff a login's IN-PLACE rewrite of the accounts file against a
// pre-login snapshot — the login subprocess is the only writer, and the file
// is about to be wiped.
//
// `strip` mirrors how the owning login script reads that specific field, so a
// value round-trips byte-for-byte: passwords are taken RAW (a password may
// legitimately contain spaces), MFA seeds are stripped.
//
// Matching Python exactly: the scan returns on the FIRST line whose field 0
// equals `username` AND which actually has enough fields; a matching line that
// is too short does NOT stop the scan.
func fieldInFile(path, username string, index int, strip bool) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	// bufio.Reader, NOT bufio.Scanner: a Scanner silently stops at its token
	// cap, and its Scan() == false is indistinguishable from "no such user".
	// That failure mode is unacceptable here — it would report "no rewrite"
	// for an account whose password was reset or whose TOTP seed was just
	// enrolled, losing the value with the working dir. Python's line iterator
	// has no cap, and neither does this.
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			s := strings.TrimSpace(line)
			if strings.Contains(s, "|") {
				parts := strings.Split(s, "|")
				// Matching Python: a matching line that is too SHORT does not
				// stop the scan — the loop continues to the next line.
				if strings.TrimSpace(parts[0]) == username && len(parts) > index {
					if strip {
						return strings.TrimSpace(parts[index]), true
					}
					return parts[index], true
				}
			}
		}
		if err != nil {
			// io.EOF (or a read error) ends the scan; either way the field was
			// not found.
			return "", false
		}
	}
}

// passwordInFile returns the current password field for `username`. Detects a
// forced-reset rewrite by kiro-web-login.py. Read RAW (never stripped): this
// value is handed back to the operator as a live credential, so it must
// survive verbatim. Port of _password_in_file (kiro_provision.py:1058-1062).
func passwordInFile(path, username string) (string, bool) {
	return fieldInFile(path, username, 1, false)
}

// m365MFAInFile returns the current mfa_secret field for an m365 `email`
// (field 3 of `email|password|mfa_secret`). Detects a newly registered TOTP
// seed written back by kiro-go-login.py's update_m365_secret, which reads this
// field stripped. Port of _mfa_in_file (kiro_provision.py:1065-1069).
func m365MFAInFile(path, email string) (string, bool) {
	return fieldInFile(path, email, 2, true)
}

// idcMFAInFile returns the current mfa_secret field for an idc `username` —
// field 4 of `username|password|start_url|mfa_secret`, so a DIFFERENT index
// than the m365 twin above. Detects a TOTP seed written back by
// kiro-web-login.py's update_account_mfa_secret, which reads this field
// stripped. Port of _idc_mfa_in_file (kiro_provision.py:1072-1077).
func idcMFAInFile(path, username string) (string, bool) {
	return fieldInFile(path, username, 3, true)
}
