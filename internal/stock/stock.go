// Package stock is the Go port of the Python folder-state stock machine
// (/Users/z/Desktop/Kiro/accounts.py). Each product folder under ./accounts
// holds one credential file per account (`username: ...\npassword: ...`),
// arranged in subfolders that ARE the source of truth for stock
// (accounts.py:1-26):
//
//	active/<username>.txt              -> AVAILABLE for sale
//	reserved/<order_id>/<username>.txt -> held for an in-flight order
//	reserved/<order_id>/.expiry        -> epoch seconds; past it the whole
//	                                      order is swept back to active/
//	sold/<username>.txt                -> SOLD (paid + delivered)
//	held/<username>.txt                -> temporarily off sale (hold/restore)
//	store/<subname>/<username>.txt     -> staging area, "email|password"
//
// Selling = moving files between these folders, so state is self-describing
// and survives a restart. All mutations run under a process-wide sync.Mutex
// AND a cross-process flock on accounts/<product>/.order.lock, so concurrent
// buyers (goroutines OR processes, including the legacy Python tools on the
// same tree) never grab the same file (accounts.py:23-25, 96-118).
package stock

import (
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// EXPIRY_MARKER filename inside a reserved/<order_id>/ folder — holds epoch
// seconds after which the reservation is reclaimed (accounts.py:93).
const expiryMarker = ".expiry"

// credentialDirMode / credentialFileMode are the permissions EVERY directory
// and file this package creates is given. 0700/0600 from CREATION, never
// widened — the same rule internal/provision already states for the archive
// tree (provision/archive.go:29-31), applied here because this package holds
// the same data class.
//
// Security warning, normal prose: the files under ./accounts and ./orders hold
// the plaintext password, portal start URL and TOTP seed for every account the
// shop has in stock or has already sold ("username|password|start_url|
// mfa_secret" in active/ and sold/, and the same material rendered as
// "username:"/"password:"/"refresh_token:" blocks plus OAuth refresh-token
// JSON in the delivered-order files). At the previous 0644/0755 any local user
// or process on the host could read the shop's entire inventory: every sold
// account could be taken over and every unsold one drained. There is no
// legitimate reader of this tree other than the bot's own uid, so the modes
// are the narrowest the data allows.
//
// These are deliberately applied to NON-credential artifacts too (the "type"
// marker, .expiry, .order.lock, product config files): a single mode for the
// whole tree is what makes it auditable — any 0644 under ./accounts is then a
// bug by inspection, with no per-file exception to reason about. Tightening a
// lock file costs nothing.
//
// Files already on disk from an older build keep their old mode; TightenModes
// below is the one-shot migration that fixes them.
const (
	credentialDirMode  os.FileMode = 0o700
	credentialFileMode os.FileMode = 0o600
)

// mu is the process-wide lock, the Go equivalent of Python's module-level
// threading.Lock `_LOCK` (accounts.py:45). It is package-level (not per-Store)
// so even two Store values pointed at the same tree in one process serialize;
// cross-process safety comes from the flock below.
var mu sync.Mutex

// Store binds the two on-disk roots the stock machine operates on. The Python
// original hardcodes ACCOUNTS_DIR / ORDERS_DIR next to the script
// (accounts.py:89-91); Go takes them as fields so tests can run on t.TempDir().
type Store struct {
	AccountsDir string // e.g. ./accounts — product folders live here
	OrdersDir   string // e.g. ./orders  — delivery records live here
}

// New builds a Store for the given accounts/ and orders/ roots.
func New(accountsDir, ordersDir string) *Store {
	return &Store{AccountsDir: accountsDir, OrdersDir: ordersDir}
}

// productDir is accounts/<product> (accounts.py:124).
func (s *Store) productDir(product string) string {
	return filepath.Join(s.AccountsDir, product)
}

// dirs returns the (active, reserved, sold) absolute paths for a product —
// the Go twin of Python `_dirs` (accounts.py:122-127).
func (s *Store) dirs(product string) (active, reserved, sold string) {
	folder := s.productDir(product)
	return filepath.Join(folder, "active"),
		filepath.Join(folder, "reserved"),
		filepath.Join(folder, "sold")
}

// lockProduct takes the process mutex, then a BLOCKING exclusive flock on
// accounts/<product>/.order.lock — the exact lock file + ordering the Python
// bot uses (`with _LOCK, _FolderLock(folder)`, accounts.py:96-118), which is
// what lets this Go bot coexist with stray Python tools on one tree. The
// returned func releases flock then mutex; call it via defer so the lock is
// dropped on every exit path. Held only for the brief move critical section
// of each operation, never across a payment wait. The OS drops the flock if
// the process dies, so it can never get stuck.
func (s *Store) lockProduct(product string) (unlock func(), err error) {
	mu.Lock()
	folder := s.productDir(product)
	// _FolderLock.__init__ creates the folder before opening the lock file
	// (accounts.py:106).
	if err := os.MkdirAll(folder, credentialDirMode); err != nil {
		mu.Unlock()
		return nil, err
	}
	// open(self.path, "w") — create/truncate the lock file (accounts.py:111).
	f, err := os.OpenFile(filepath.Join(folder, ".order.lock"),
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, credentialFileMode)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	// fcntl.flock(fh, LOCK_EX) BLOCKS until free (accounts.py:112). Retry on
	// EINTR — a signal must not make us proceed unlocked.
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() {
		// Release order mirrors _FolderLock.__exit__ then `with` unwinding:
		// flock LOCK_UN, close, then the process mutex (accounts.py:115-119).
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		mu.Unlock()
	}, nil
}

// listTxt returns the sorted *.txt filenames in directory d, empty if the
// directory is absent — Python `_list_txt` (accounts.py:130-135). The sort
// keeps behavior deterministic and skips dot-markers like .expiry.
func listTxt(d string) []string {
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// readWholeFile reads a credential file's whole body, stripped, or "" if
// missing/unreadable — Python `_read_token_file` (accounts.py:247-261).
// Finalize sniffs the returned text to tell JSON / username:password /
// legacy-token files apart, so this is a generic full-body reader. Any read
// error degrades to "" (which Finalize SKIPS) rather than aborting a
// mid-delivery.
func readWholeFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeFileAtomic writes content to <path>.tmp then renames over <path> —
// the write-then-os.replace pattern every Python config/credential write uses
// (e.g. accounts.py:176-179, 368-373). Rename is atomic on POSIX so readers
// never observe a torn file.
//
// The tmp file is removed before it is (re)created, and its mode is asserted
// with an explicit Chmod afterwards. Both steps matter for credential
// material: os.WriteFile only applies `perm` when it CREATES the file, so a
// stale 0644 .tmp left by an earlier crash would silently keep its old,
// world-readable mode and the rename would carry that mode onto the live
// credential file. The Chmod also undoes any umask subtraction, so 0600 means
// 0600 regardless of how the process was started.
func writeFileAtomic(path, content string, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(tmp, []byte(content), perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// sweepExpiredLocked moves any expired reservation's files back to active/
// (lazy reclaim) — Python `_sweep_expired` (accounts.py:264-301). Caller MUST
// already hold the locks. An order folder whose .expiry is in the past (or
// missing/garbled) is fully returned to active/ and removed, so abandoned
// checkouts free their stock automatically and the state survives a restart.
func (s *Store) sweepExpiredLocked(product string) {
	active, reserved, _ := s.dirs(product)
	// time.time() is a float in Python; use float seconds for the comparison
	// so a fractional .expiry written by another tool still parses.
	now := float64(time.Now().UnixNano()) / 1e9
	entries, err := os.ReadDir(reserved)
	if err != nil {
		return // no reserved/ yet -> nothing to sweep (accounts.py:274-276)
	}
	os.MkdirAll(active, credentialDirMode)
	for _, e := range entries {
		odir := filepath.Join(reserved, e.Name())
		if !e.IsDir() {
			continue // stray file at reserved/ top level (accounts.py:280-281)
		}
		// Parse the .expiry marker; no/garbled marker -> exp=0 -> treat as
		// expired and reclaim (accounts.py:282-285).
		exp := 0.0
		if raw := readWholeFile(filepath.Join(odir, expiryMarker)); raw != "" {
			if v, perr := strconv.ParseFloat(raw, 64); perr == nil {
				exp = v
			}
		}
		if now <= exp {
			continue // still valid (accounts.py:286-287)
		}
		// Move every credential file back to active/; individual failures are
		// swallowed so one bad file can't wedge the sweep (accounts.py:288-292).
		for _, f := range listTxt(odir) {
			_ = os.Rename(filepath.Join(odir, f), filepath.Join(active, f))
		}
		// Remove marker + (now empty) order dir; both best-effort
		// (accounts.py:293-301).
		_ = os.Remove(filepath.Join(odir, expiryMarker))
		_ = os.Remove(odir)
	}
}

// SweepExpired runs the expiry sweep for a product under the full lock pair.
// The sweep also runs lazily inside CountAvailable/Reserve/Hold, matching the
// Python callers of `_sweep_expired`; this exported entry point lets a
// periodic janitor force a pass.
func (s *Store) SweepExpired(product string) error {
	unlock, err := s.lockProduct(product)
	if err != nil {
		return err
	}
	defer unlock()
	s.sweepExpiredLocked(product)
	return nil
}

// CountAvailable — how many accounts are AVAILABLE (files in active/, after
// sweeping expired reservations back in) — Python `count_available`
// (accounts.py:304-310).
func (s *Store) CountAvailable(product string) (int, error) {
	active, _, _ := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, err
	}
	defer unlock()
	s.sweepExpiredLocked(product)
	return len(listTxt(active)), nil
}

// CountHeld — how many accounts are HELD (in held/, off-sale until restored).
// Lock-free like the Python original (accounts.py:313-316).
func (s *Store) CountHeld(product string) int {
	return len(listTxt(filepath.Join(s.productDir(product), "held")))
}

// reservedCountLocked tallies reserved/*/*.txt WITHOUT sweeping — caller must
// already hold the locks and have swept (or deliberately not swept) first.
// Split out of countReservedLocked so CountsSnapshot can share it without
// paying for a second sweep pass (see CountsSnapshot).
func (s *Store) reservedCountLocked(reserved string) int {
	n := 0
	entries, err := os.ReadDir(reserved)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		n += len(listTxt(filepath.Join(reserved, e.Name())))
	}
	return n
}

// countReservedLocked — reserved-account count; caller MUST already hold the
// locks. Sweeps expired reservations first so only live in-flight holds count
// — Python `_count_reserved_locked` (accounts.py:1454-1468).
func (s *Store) countReservedLocked(product string) int {
	_, reserved, _ := s.dirs(product)
	s.sweepExpiredLocked(product)
	return s.reservedCountLocked(reserved)
}

// CountReserved — how many accounts are RESERVED right now (in-flight
// orders), after sweep — Python `count_reserved` (accounts.py:1471-1481).
// Best-effort preview for admin warnings; the authoritative check is redone
// atomically inside RenameProduct/RemoveProduct under the lock.
func (s *Store) CountReserved(product string) (int, error) {
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return s.countReservedLocked(product), nil
}

// ProductCounts is the paired Available/Reserved result of CountsSnapshot —
// see that function for why the pair must come from ONE read, not two.
type ProductCounts struct {
	Available int
	Reserved  int
}

// CountsSnapshot returns a product's Available and Reserved counts together,
// from a SINGLE lock acquisition and a SINGLE expiry sweep — task #19.
//
// Two defects in calling CountAvailable then CountReserved separately (the
// shape every render path used before this): first, each call takes its own
// lock and runs its own sweep, so building one rendered line that shows both
// numbers pays the lock/sweep cost TWICE per product — on the
// most-frequently-opened admin screen (the dashboard), that is now visible
// cost, not just a theoretical one. Second, and subtler: the two calls judge
// the SAME reservation's expiry against two DIFFERENT clock reads. If a
// reservation ticks from live to expired in the gap between them,
// CountAvailable's (earlier) sweep does not yet reclaim it — it is still
// live at that instant, so it is not counted as available — and
// CountReserved's (later) sweep DOES reclaim it back to active before
// counting reserved/ — so it is not counted as reserved either. The
// reservation is real, but the one render line it lands on shows it in
// NEITHER bucket: the product under-reports its total stock by one,
// display-only, and self-corrects on the very next render (nothing about
// the stock itself is lost — only that single rendered line's numbers).
// This is not a concurrency hazard requiring another goroutine — a single
// caller making two sequential calls can hit it purely from wall-clock time
// passing between them.
//
// CountsSnapshot closes both gaps: one lockProduct call, one
// sweepExpiredLocked call, then both directory reads while still holding the
// lock — so no reservation's expiry can be judged twice against two
// different instants, and the SAME sweep result backs both numbers.
//
// This still sweeps (it is not a read-only path) deliberately: nothing else
// in this bot sweeps a product's expired reservations periodically — Reserve
// and Hold sweep lazily at their own call sites, and the exported
// SweepExpired has no periodic caller — so for a product nobody is actively
// buying or holding, a render calling this IS how an abandoned reservation
// ever gets reclaimed. Trading that reclaim path away for a purely
// read-only render would trade a display quirk for a real stock leak on any
// product between purchases; CountsSnapshot keeps the reclaim but pays its
// lock/sweep cost once instead of twice.
func (s *Store) CountsSnapshot(product string) (ProductCounts, error) {
	active, reserved, _ := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return ProductCounts{}, err
	}
	defer unlock()
	s.sweepExpiredLocked(product)
	return ProductCounts{
		Available: len(listTxt(active)),
		Reserved:  s.reservedCountLocked(reserved),
	}, nil
}

// CountSold — how many accounts have been SOLD (files in sold/), lock-free
// like Python `count_sold` (accounts.py:1484-1487).
func (s *Store) CountSold(product string) int {
	_, _, sold := s.dirs(product)
	return len(listTxt(sold))
}

// knownUsernamesLocked — set of every username this product has seen in ANY
// state — Python `_known_usernames` (accounts.py:319-337). Spans active/,
// held/, sold/ and every reserved/<order>/ so re-adding a username already
// sold (or in-flight) is rejected: never resurrect stock. Caller holds locks
// (or accepts a racy snapshot, same as the Python public wrapper).
func (s *Store) knownUsernamesLocked(product string) map[string]bool {
	folder := s.productDir(product)
	known := make(map[string]bool)
	for _, sub := range []string{"active", "held", "sold"} {
		for _, f := range listTxt(filepath.Join(folder, sub)) {
			known[strings.TrimSuffix(f, ".txt")] = true
		}
	}
	reserved := filepath.Join(folder, "reserved")
	if entries, err := os.ReadDir(reserved); err == nil {
		for _, e := range entries {
			for _, f := range listTxt(filepath.Join(reserved, e.Name())) {
				known[strings.TrimSuffix(f, ".txt")] = true
			}
		}
	}
	return known
}

// KnownIdentities — public snapshot of knownUsernamesLocked: every account
// stem this product has in ANY state — Python `known_identities`
// (accounts.py:340-344). Used to skip duplicates before spending a token
// refresh on an account that's already in stock or sold.
func (s *Store) KnownIdentities(product string) map[string]bool {
	return s.knownUsernamesLocked(product)
}

// randomSample picks n distinct random elements from files — the Go stand-in
// for Python's random.sample used by reserve()/hold() (accounts.py:1029,1123)
// so buyers get RANDOM accounts, not always the alphabetically first ones.
func randomSample(files []string, n int) []string {
	idx := rand.Perm(len(files))
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = files[idx[i]]
	}
	return out
}

// Reserve moves `qty` RANDOM active accounts into reserved/<orderID> for
// `ttl` — Python `reserve` (accounts.py:1109-1132). Returns the list of
// reserved usernames, or nil if fewer than `qty` are available
// (ALL-OR-NOTHING). Writes a .expiry marker so an unpaid order is
// auto-reclaimed by the sweep. Goroutine- and process-safe.
func (s *Store) Reserve(product string, qty int, orderID string, ttl time.Duration) ([]string, error) {
	active, reserved, _ := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.sweepExpiredLocked(product)
	files := listTxt(active)
	if len(files) < qty || qty <= 0 {
		return nil, nil // all-or-nothing: not enough stock (accounts.py:1121-1122)
	}
	chosen := randomSample(files, qty)
	odir := filepath.Join(reserved, orderID)
	if err := os.MkdirAll(odir, credentialDirMode); err != nil {
		return nil, err
	}
	// .expiry holds str(int(time.time() + ttl)) — integer epoch seconds
	// (accounts.py:1126-1127). Written atomically (tmp+rename) so a crash
	// can't leave a torn marker; a missing marker would just mean "expired".
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	if err := writeFileAtomic(filepath.Join(odir, expiryMarker), exp, credentialFileMode); err != nil {
		return nil, err
	}
	usernames := make([]string, 0, qty)
	for _, f := range chosen {
		// A failed move here is a real error (Python lets the exception
		// propagate, accounts.py:1130) — the sweeper will reclaim whatever
		// landed in the order folder once the .expiry passes.
		if err := os.Rename(filepath.Join(active, f), filepath.Join(odir, f)); err != nil {
			return nil, err
		}
		usernames = append(usernames, strings.TrimSuffix(f, ".txt")) // strip .txt
	}
	return usernames, nil
}

// Release moves this order's reserved files back to active/ and returns the
// count — Python `release` (accounts.py:1135-1163). No-op (0) if the order
// folder is gone (already finalized or swept). A SOLD account is never
// touched (it lives in sold/, not reserved/<order_id>).
func (s *Store) Release(product, orderID string) (int, error) {
	active, reserved, _ := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, err
	}
	defer unlock()
	odir := filepath.Join(reserved, orderID)
	if fi, err := os.Stat(odir); err != nil || !fi.IsDir() {
		return 0, nil
	}
	os.MkdirAll(active, credentialDirMode)
	n := 0
	for _, f := range listTxt(odir) {
		// Individual move failures are swallowed (accounts.py:1149-1154).
		if os.Rename(filepath.Join(odir, f), filepath.Join(active, f)) == nil {
			n++
		}
	}
	// Best-effort marker + dir cleanup (accounts.py:1155-1162).
	_ = os.Remove(filepath.Join(odir, expiryMarker))
	_ = os.Remove(odir)
	return n, nil
}

// Hold moves `qty` RANDOM active accounts into held/ (temporarily off sale)
// and returns the count actually moved — Python `hold` (accounts.py:1012-1034).
// Held accounts are not in active/, so they won't be sold/reserved until
// restored. Goroutine- and process-safe.
func (s *Store) Hold(product string, qty int) (int, error) {
	active, _, _ := s.dirs(product)
	held := filepath.Join(s.productDir(product), "held")
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, err
	}
	defer unlock()
	s.sweepExpiredLocked(product)
	files := listTxt(active)
	n := min(qty, len(files))
	if n <= 0 {
		return 0, nil
	}
	if err := os.MkdirAll(held, credentialDirMode); err != nil {
		return 0, err
	}
	for _, f := range randomSample(files, n) {
		// Individual move failures swallowed (accounts.py:1029-1033); note
		// the Python original returns n regardless — keep that behavior.
		_ = os.Rename(filepath.Join(active, f), filepath.Join(held, f))
	}
	return n, nil
}

// Restore moves up to `qty` held accounts back into active/ (return to sale)
// and returns the count — Python `restore` (accounts.py:1037-1056). Unlike
// Hold this takes the FIRST files in sorted order (Python files[:n]).
func (s *Store) Restore(product string, qty int) (int, error) {
	active, _, _ := s.dirs(product)
	held := filepath.Join(s.productDir(product), "held")
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, err
	}
	defer unlock()
	files := listTxt(held)
	n := min(qty, len(files))
	if n <= 0 {
		return 0, nil
	}
	if err := os.MkdirAll(active, credentialDirMode); err != nil {
		return 0, err
	}
	for _, f := range files[:n] {
		_ = os.Rename(filepath.Join(held, f), filepath.Join(active, f))
	}
	return n, nil
}

// RestoreSold moves specific SOLD accounts back into active/ (admin
// force-refund undo) — Python `restore_sold` (accounts.py:1059-1106).
//
// Unlike Restore/Release (which return RESERVED, never-delivered stock), this
// pulls already-DELIVERED credentials out of sold/ and re-lists them. The
// caller passes the exact identifiers from OrderUsernames; for password and
// JSON accounts that stem IS the sold/<stem>.txt filename. Legacy token stock
// records the raw token (not the filename) and can't match — it lands in
// `unmatched`.
//
// DANGER: the buyer already holds these credentials; re-listing means a
// future buyer gets an account a previous buyer can still access — only an
// admin doing a deliberate full undo should decide this is OK.
//
// Returns (restored, unmatched): count moved back to active, and the
// identifiers whose sold/<id>.txt was absent.
func (s *Store) RestoreSold(product string, usernames []string) (int, []string, error) {
	active, _, sold := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return 0, nil, err
	}
	defer unlock()
	os.MkdirAll(active, credentialDirMode)
	restored := 0
	var unmatched []string
	for _, u := range usernames {
		// The order file stores bare identifiers; reject anything with a path
		// separator so a crafted/legacy value can never escape sold/
		// (accounts.py:1085-1090).
		if u == "" || strings.ContainsAny(u, "/\\") || u == "." || u == ".." {
			unmatched = append(unmatched, u)
			continue
		}
		src := filepath.Join(sold, u+".txt")
		dst := filepath.Join(active, u+".txt")
		// Already back in active (double tap / partial prior restore): treat
		// as done, don't clobber the live active copy (accounts.py:1093-1097).
		if _, err := os.Stat(dst); err == nil {
			restored++
			continue
		}
		if _, err := os.Stat(src); err != nil {
			unmatched = append(unmatched, u)
			continue
		}
		if os.Rename(src, dst) == nil {
			restored++
		} else {
			unmatched = append(unmatched, u)
		}
	}
	return restored, unmatched, nil
}

// DeliveredKind discriminates the three credential formats Finalize can ship
// (Python signals them via dict keys, accounts.py:1360-1362).
type DeliveredKind int

const (
	// KindPassword — `username: ...\npassword: ...` credential file.
	KindPassword DeliveredKind = iota
	// KindJSON — whole-file JSON object account (email is the identity).
	KindJSON
	// KindToken — legacy single-line refresh-token stock.
	KindToken
)

// Delivered is one delivered account — the Go shape of the per-format dicts
// Python finalize() returns (accounts.py:1356-1362).
type Delivered struct {
	Kind     DeliveredKind
	Username string // KindPassword: file stem (the username)
	Password string // KindPassword: may be "" for OTP-login accounts
	Email    string // KindJSON: file stem (the account email)
	Raw      []byte // KindJSON: the file's raw JSON body (key order preserved)
	Token    string // KindToken: the raw single-line refresh token
}

// Finalize moves this order's reserved files to sold/, reads the creds,
// writes the order delivery file, and returns the delivered list — Python
// `finalize` (accounts.py:1356-1429). Returns nil if the reservation folder
// is gone (reservation lost/swept). `warranty` "" omits the line; `lang`
// picks the buyer's timezone for the Date stamp.
func (s *Store) Finalize(product, orderID, warranty, lang string) ([]Delivered, error) {
	_, reserved, sold := s.dirs(product)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return nil, err
	}
	defer unlock()
	odir := filepath.Join(reserved, orderID)
	files := listTxt(odir)
	if len(files) == 0 {
		return nil, nil // reservation lost (accounts.py:1369-1370)
	}
	if err := os.MkdirAll(sold, credentialDirMode); err != nil {
		return nil, err
	}
	var delivered []Delivered
	for _, f := range files {
		src := filepath.Join(odir, f)
		// Decide the credential format PER FILE from its actual contents, not
		// from the product's `type` flag: the flag can be flipped by
		// upstocking the other format, and a product can hold mixed/legacy
		// stock. Read the body BEFORE the move to sold/ (accounts.py:1375-1380).
		raw := readWholeFile(src)
		// An unreadable/empty credential file must NOT be delivered: skip it
		// — it stays in reserved/, the sweeper returns it to active/ for
		// inspection, and the shortfall is visible via len(delivered)
		// (accounts.py:1381-1388).
		if raw == "" {
			log.Printf("WARN finalize: unreadable/empty credential file skipped: %s", src)
			continue
		}
		// Guard the move: a transient FS error on ONE file must not abort the
		// whole delivery half-way; a failed move skips just that account
		// (accounts.py:1389-1397).
		if err := os.Rename(src, filepath.Join(sold, f)); err != nil {
			log.Printf("WARN finalize: move to sold/ failed, account skipped: %s: %v", src, err)
			continue
		}
		stem := strings.TrimSuffix(f, ".txt")
		if obj := tryParseJSONObject(raw); obj != nil {
			// JSON account: ships as its own file at order-write time; email
			// (stem) is the identity for ownership checks (accounts.py:1399-1407).
			delivered = append(delivered, Delivered{Kind: KindJSON, Email: stem, Raw: []byte(raw)})
		} else if strings.HasPrefix(raw, "username:") {
			// username:/password: credential file (password blank for OTP)
			// (accounts.py:1408-1415).
			pw := ""
			for _, line := range strings.Split(raw, "\n") {
				if strings.HasPrefix(line, "password:") {
					pw = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
					break
				}
			}
			delivered = append(delivered, Delivered{Kind: KindPassword, Username: stem, Password: pw})
		} else {
			// Legacy single-line refresh-token stock (accounts.py:1416-1418).
			delivered = append(delivered, Delivered{Kind: KindToken, Token: raw})
		}
	}
	// Clean up the now-empty order folder, best-effort (accounts.py:1419-1427).
	_ = os.Remove(filepath.Join(odir, expiryMarker))
	_ = os.Remove(odir)
	// Build the ./orders/<order_id>.txt delivery record (accounts.py:1428).
	if err := s.writeOrderFile(orderID, delivered, product, warranty, lang); err != nil {
		return delivered, err
	}
	return delivered, nil
}
